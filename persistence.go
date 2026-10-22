package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var errStateLocked = errors.New("state file is locked")
var legacySessionID = regexp.MustCompile(`_session_([a-zA-Z0-9-]+)(?:_agent_([a-zA-Z0-9-]+))?`)

func hostIdentity(value string) string {
	if value == "" {
		return ""
	}
	var metadata M
	if decode([]byte(value), &metadata) == nil && metadata != nil {
		if session := str(metadata["session_id"]); session != "" {
			return hash(M{"session_id": session, "agent_id": str(metadata["agent_id"])})
		}
		delete(metadata, "tk")
		return hash(metadata)
	}
	if match := legacySessionID.FindStringSubmatch(value); len(match) > 1 {
		return hash(M{"session_id": match[1], "agent_id": match[2]})
	}
	return hash(value)
}

func namedClaudeSession(value string) bool {
	var metadata M
	if decode([]byte(value), &metadata) == nil && str(metadata["session_id"]) != "" {
		return true
	}
	return legacySessionID.MatchString(value)
}

type savedPending struct {
	PublicID string `json:"public_id"`
	Name     string `json:"name"`
}
type sessionCheckpoint struct {
	Version        int            `json:"version"`
	ThreadID       string         `json:"thread_id"`
	HostIdentity   string         `json:"host_identity"`
	Key            string         `json:"key"`
	Expected       []string       `json:"expected"`
	RequestHashes  []string       `json:"request_hashes,omitempty"`
	InFlight       bool           `json:"in_flight"`
	Pending        []savedPending `json:"pending,omitempty"`
	Primary        bool           `json:"primary"`
	CompactHash    string         `json:"compact_hash,omitempty"`
	BaseSystemHash string         `json:"base_system_hash"`
	Model          string         `json:"model"`
	ContextWindow  int            `json:"context_window"`
	UpdatedAt      time.Time      `json:"updated_at"`
	path           string
}

func compactionHash(messages []any) string {
	result := ""
	for _, v := range messages {
		m := object(v)
		if m["role"] != "user" {
			continue
		}
		bs, _ := blocks(m["content"])
		for _, v := range bs {
			b := object(v)
			if b["type"] == "text" && strings.HasPrefix(strings.TrimSpace(str(b["text"])), "This session is being continued from a previous conversation") {
				result = hash(b["text"])
			}
		}
	}
	return result
}

func pendingAnswered(p *prepared, pending []savedPending) bool {
	if len(pending) == 0 {
		return false
	}
	results := map[string]bool{}
	for _, v := range array(object(p.Messages[len(p.Messages)-1])["content"]) {
		b := object(v)
		if b["type"] == "tool_result" {
			results[str(b["tool_use_id"])] = true
		}
	}
	for _, pending := range pending {
		if !results[pending.PublicID] {
			return false
		}
	}
	return true
}

func checkpointScore(s sessionCheckpoint, p *prepared) int {
	if prefixMatches(p.Hashes, s.Expected) {
		return 1000000 + len(s.Expected)
	}
	if pendingAnswered(p, s.Pending) {
		return 900000
	}
	if s.InFlight && prefixMatches(p.Hashes, s.RequestHashes) {
		return 800000 + len(s.RequestHashes)
	}
	return 0
}

func readCheckpoints(dir, identity string) ([]sessionCheckpoint, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	result := []sessionCheckpoint{}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var state sessionCheckpoint
		if decode(raw, &state) != nil || state.Version != 1 || state.ThreadID == "" || state.HostIdentity != identity {
			return nil, fmt.Errorf("invalid bridge checkpoint: %s", path)
		}
		state.path = path
		result = append(result, state)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UpdatedAt.After(result[j].UpdatedAt) })
	return result, nil
}

func (c *conversation) saveState() error {
	if c.statePath == "" {
		return nil
	}
	pending := append([]savedPending{}, c.recoveredPending...)
	for _, p := range c.pending {
		pending = append(pending, savedPending{PublicID: p.PublicID, Name: p.Name})
	}
	state := sessionCheckpoint{Version: 1, ThreadID: c.threadID, HostIdentity: c.hostIdentity, Key: c.key, Expected: c.expected, RequestHashes: c.requestHashes, InFlight: c.inFlight, Pending: pending, Primary: c.primary, CompactHash: c.compactHash, BaseSystemHash: c.baseSystemHash, Model: c.model, ContextWindow: c.contextWindow, UpdatedAt: time.Now()}
	f, err := os.CreateTemp(filepath.Dir(c.statePath), ".checkpoint-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(encode(state))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return replaceStateFile(temp, c.statePath)
}

func (c *conversation) close() {
	c.closeOnce.Do(func() {
		c.rpc.Close()
		if c.lease != nil {
			_ = c.lease.Close()
		}
	})
}

func (b *Bridge) openPersistent(ctx context.Context, p *prepared, key string) (*conversation, bool, error) {
	if b.stateDir == "" || p.HostIdentity == "" || p.Search != nil || p.Auxiliary {
		rpc, err := b.factory(ctx, b.command, b.cwd, b.timeout)
		if err != nil {
			return nil, false, err
		}
		c, err := newConversation(ctx, rpc, p, b.cwd, key)
		return c, false, err
	}
	dir := filepath.Join(b.stateDir, p.HostIdentity)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, false, err
	}
	gate, err := tryStateLock(filepath.Join(dir, "index.lock"))
	if err != nil {
		if errors.Is(err, errStateLocked) {
			return nil, false, &bridgeError{409, "Another bridge is updating this Claude session; retry shortly"}
		}
		return nil, false, err
	}
	defer gate.Close()
	states, err := readCheckpoints(dir, p.HostIdentity)
	if err != nil {
		return nil, false, err
	}
	var selected *sessionCheckpoint
	score := 0
	// Live conversations already own their leases. Their pending results are
	// routed in acquire; a divergent background branch must open its own thread
	// rather than attempting to resume a live parent's checkpoint.
	owned := map[string]bool{}
	for _, live := range b.sessions {
		owned[live.threadID] = true
	}
	for i := range states {
		if owned[states[i].ThreadID] {
			continue
		}
		if value := checkpointScore(states[i], p); value > score {
			selected = &states[i]
			score = value
		}
	}
	// On a fresh bridge, Claude's explicit session UUID identifies its primary
	// thread even if replay changes old host-message wrappers. Live forks are
	// still matched by history and may never take over a parent's active thread.
	if selected == nil && p.NamedSession {
		livePrimary := false
		for _, live := range b.sessions {
			if live.hostIdentity == p.HostIdentity && live.primary {
				livePrimary = true
			}
		}
		if !livePrimary {
			for i := range states {
				if states[i].Primary && !owned[states[i].ThreadID] {
					selected = &states[i]
					break
				}
			}
		}
	}
	newCompaction := p.CompactHash != ""
	for _, state := range states {
		if state.CompactHash == p.CompactHash {
			newCompaction = false
		}
	}
	if selected == nil && newCompaction {
		candidates := []int{}
		for i, state := range states {
			if !owned[state.ThreadID] && state.BaseSystemHash == p.BaseSystemHash {
				candidates = append(candidates, i)
			}
		}
		if len(candidates) == 0 && len(states) == 1 && !owned[states[0].ThreadID] {
			candidates = append(candidates, 0)
		}
		if len(candidates) > 1 || (len(candidates) == 0 && len(states) > 0) {
			return nil, false, &bridgeError{409, "Cannot identify the compacted conversation unambiguously; saved threads were preserved"}
		}
		if len(candidates) == 1 {
			selected = &states[candidates[0]]
		}
	}
	var lease *os.File
	if selected != nil {
		lease, err = tryStateLock(selected.path + ".lock")
		if err != nil {
			if errors.Is(err, errStateLocked) {
				return nil, false, &bridgeError{409, "This Claude conversation is already open in another bridge"}
			}
			return nil, false, err
		}
	}
	rpc, err := b.factory(ctx, b.command, b.cwd, b.timeout)
	if err != nil {
		if lease != nil {
			lease.Close()
		}
		return nil, false, err
	}
	p.Persistent = true
	c, err := openConversation(ctx, rpc, p, b.cwd, key, selected)
	if err != nil {
		if lease != nil {
			lease.Close()
		}
		return nil, false, err
	}
	if selected != nil {
		c.statePath = selected.path
		c.lease = lease
		c.primary = selected.Primary
	} else {
		c.statePath = filepath.Join(dir, hash(c.threadID)+".json")
		c.primary = len(states) == 0
		c.lease, err = tryStateLock(c.statePath + ".lock")
		if err != nil {
			c.close()
			return nil, false, err
		}
	}
	if err = c.saveState(); err != nil {
		c.close()
		return nil, false, err
	}
	return c, selected != nil, nil
}

// A dead app-server must be resumed from the existing checkpoint, not reused as
// a dead connection or replaced by a new native thread.
func (b *Bridge) pruneStoppedLocked() {
	for i := 0; i < len(b.sessions); {
		c := b.sessions[i]
		select {
		case <-c.rpc.Done():
			if c.mu.TryLock() {
				b.removeLocked(c)
				c.close()
				c.mu.Unlock()
				continue
			}
		default:
		}
		i++
	}
}
