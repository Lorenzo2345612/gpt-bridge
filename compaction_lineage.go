package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
)

type compactionCandidate struct {
	ID     string
	Hashes []string
}

var safeClaudeID = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)

func claudeSessionParts(value string) (string, string) {
	var m M
	if decode([]byte(value), &m) == nil {
		return str(m["session_id"]), str(m["agent_id"])
	}
	if parts := legacySessionID.FindStringSubmatch(value); len(parts) > 1 {
		return parts[1], parts[2]
	}
	return "", ""
}

// Return an owner only for a unique matching native-history hash. If the most
// recent matching message is shared by forks, older ancestors cannot break the
// tie safely: one fork may have already compacted its copy of those ancestors.
func compactionAnchorOwner(anchors []string, candidates []compactionCandidate) (string, bool) {
	for i := len(anchors) - 1; i >= 0; i-- {
		owners := map[string]bool{}
		for _, c := range candidates {
			for _, h := range c.Hashes {
				if h == anchors[i] {
					owners[c.ID] = true
					break
				}
			}
		}
		if len(owners) > 1 {
			return "", true
		}
		for id := range owners {
			return id, true
		}
	}
	return "", false
}

func retainedCompactionOwner(p *prepared, candidates []compactionCandidate) (string, bool) {
	// Only content following the current summary is retained context. Matching
	// the summary inherited from a parent would conflate a fork with its owner.
	start := -1
	for i, v := range p.Messages {
		if compactionHash([]any{v}) == p.CompactHash {
			start = i + 1
		}
	}
	if start < 0 {
		return "", false
	}
	return compactionAnchorOwner(p.Hashes[start:], candidates)
}

type journalLineageRecord struct {
	UUID              string          `json:"uuid"`
	ParentUUID        string          `json:"parentUuid"`
	LogicalParentUUID string          `json:"logicalParentUuid"`
	Type              string          `json:"type"`
	Subtype           string          `json:"subtype"`
	SessionID         string          `json:"sessionId"`
	Sidechain         bool            `json:"isSidechain"`
	Summary           bool            `json:"isCompactSummary"`
	Message           json.RawMessage `json:"message"`
}

func journalCompactionOwner(path string, p *prepared, candidates []compactionCandidate) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 64<<10), 64<<20)
	// Keep only hashes, never a second copy of Claude's conversation. Main and
	// subagent journals are separate files. Physical order remains usable when
	// logicalParentUuid refers to an attachment Claude did not persist.
	anchors := []string{}
	known := map[string]bool{}
	for _, candidate := range candidates {
		for _, h := range candidate.Hashes {
			known[h] = true
		}
	}
	var beforeBoundary []string
	owner := ""
	found := false
	for scan.Scan() {
		var record journalLineageRecord
		if json.Unmarshal(scan.Bytes(), &record) != nil {
			continue
		}
		if record.SessionID != "" && record.SessionID != p.ClaudeSessionID {
			continue
		}
		if p.ClaudeAgentID == "" && record.Sidechain {
			continue
		}
		if record.Type == "system" && record.Subtype == "compact_boundary" {
			beforeBoundary = append([]string{}, anchors...)
			// A session may compact repeatedly while using another provider or
			// while retries fail. Carry its last native anchors across those
			// boundaries until newer native output supplies different evidence.
			continue
		}
		if len(record.Message) == 0 {
			continue
		}
		var message M
		if decode(record.Message, &message) != nil {
			continue
		}
		if record.Summary && compactionHash([]any{message}) == p.CompactHash {
			owner, found = compactionAnchorOwner(beforeBoundary, candidates)
		}
		if record.Type != "assistant" || message["role"] != "assistant" {
			continue
		}
		// Claude inserts local placeholder replies (e.g. "No response
		// requested.") in unrelated branches. They are not model output and
		// cannot establish ownership of a native Codex conversation.
		if str(message["model"]) == "<synthetic>" {
			continue
		}
		canonical, err := canonicalMessages([]any{message})
		if err != nil {
			continue
		}
		h := hash(canonical[0])
		if !known[h] {
			continue
		}
		anchors = append(anchors, h)
		if len(anchors) > 64 {
			anchors = anchors[len(anchors)-64:]
		}
	}
	if scan.Err() != nil {
		return "", false
	}
	return owner, found
}

// Resolve only an exact boundary ancestry chain. Physical journal order and
// similar content cannot substitute for a missing UUID edge.
func journalExplicitCompactionOwner(path string, p *prepared, owners map[string]map[string]bool) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	type node struct{ parent, message string }
	nodes := map[string]node{}
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 64<<10), 64<<20)
	boundary := ""
	selected, matched := "", false
	for scan.Scan() {
		var record journalLineageRecord
		if json.Unmarshal(scan.Bytes(), &record) != nil ||
			(record.SessionID != "" && record.SessionID != p.ClaudeSessionID) ||
			(p.ClaudeAgentID == "" && record.Sidechain) {
			continue
		}
		if record.Type == "system" && record.Subtype == "compact_boundary" {
			boundary = fallback(record.LogicalParentUUID, record.ParentUUID)
		}
		var message M
		_ = decode(record.Message, &message)
		if record.Summary && compactionHash([]any{message}) == p.CompactHash {
			seen := map[string]bool{}
			for uuid := boundary; uuid != "" && !seen[uuid]; {
				seen[uuid] = true
				n, ok := nodes[uuid]
				if !ok {
					break
				}
				if ids := owners[n.message]; len(ids) > 0 {
					if len(ids) != 1 {
						return "", true
					}
					for id := range ids {
						if matched && selected != id {
							return "", true
						}
						selected, matched = id, true
					}
					break
				}
				// An unrecorded real response may belong to a newer fork. Do
				// not promote an older ancestor into explicit ownership proof.
				if n.message != "" {
					break
				}
				uuid = n.parent
			}
		}
		if record.UUID != "" {
			n := node{parent: record.ParentUUID}
			if record.Type == "assistant" && message["role"] == "assistant" && message["model"] != "<synthetic>" {
				n.message = str(message["id"])
			}
			if old, exists := nodes[record.UUID]; exists && old != n {
				return "", true
			}
			nodes[record.UUID] = n
		}
	}
	if scan.Err() != nil {
		return "", false
	}
	return selected, matched
}

func (b *Bridge) identifyCompaction(p *prepared) (string, error) {
	byID := map[string]compactionCandidate{}
	owners := map[string]map[string]bool{}
	addLineage := func(id string, records []responseLineage) {
		for _, record := range records {
			if record.MessageID == "" || record.ThreadID == "" {
				continue
			}
			if owners[record.MessageID] == nil {
				owners[record.MessageID] = map[string]bool{}
			}
			owners[record.MessageID][id] = true
		}
	}
	for _, c := range b.sessions {
		if c.hostIdentity == p.HostIdentity && !c.auxiliary && !c.search {
			byID[c.threadID] = compactionCandidate{c.threadID, c.expected}
			addLineage(c.threadID, c.lineage)
		}
	}
	if b.stateDir != "" {
		states, err := readCheckpoints(filepath.Join(b.stateDir, p.HostIdentity), p.HostIdentity)
		if err != nil {
			return "", err
		}
		for _, s := range states {
			if _, live := byID[s.ThreadID]; !live {
				byID[s.ThreadID] = compactionCandidate{s.ThreadID, s.Expected}
				addLineage(s.ThreadID, s.Lineage)
			}
		}
	}
	candidates := make([]compactionCandidate, 0, len(byID))
	for _, c := range byID {
		candidates = append(candidates, c)
	}
	if !safeClaudeID.MatchString(p.ClaudeSessionID) || (p.ClaudeAgentID != "" && !safeClaudeID.MatchString(p.ClaudeAgentID)) {
		id, _ := retainedCompactionOwner(p, candidates)
		return id, nil
	}
	pattern := filepath.Join(claudeConfig(), "projects", "*", p.ClaudeSessionID+".jsonl")
	if p.ClaudeAgentID != "" {
		pattern = filepath.Join(claudeConfig(), "projects", "*", p.ClaudeSessionID, "subagents", "agent-"+p.ClaudeAgentID+".jsonl")
	}
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	var selected string
	explicit := false
	for _, path := range paths {
		if id, matched := journalExplicitCompactionOwner(path, p, owners); matched {
			if id == "" || (explicit && selected != id) {
				return "", nil
			}
			selected, explicit = id, true
		}
	}
	if explicit {
		return selected, nil
	}
	if id, _ := retainedCompactionOwner(p, candidates); id != "" {
		return id, nil
	}
	for _, path := range paths {
		if id, matched := journalCompactionOwner(path, p, candidates); matched {
			if id == "" || (selected != "" && selected != id) {
				return "", nil
			}
			selected = id
		}
	}
	return selected, nil
}
