package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

const hostInstructions = "You are the model driving Claude Code. Follow the host's instructions below. Use only the cc_ dynamic tools supplied by the host. They map to Claude Code's tools, including Skill and MCP. The host executes them with its own permissions and hooks. The external host controls filesystem, network, and approval access. Host tools can perform writes when the host permits them: request the tool rather than inferring that it is unavailable from Codex's internal execution environment. Respect host refusals and any explicit user restrictions. Never execute tools yourself or use Codex's built-in tools.\n\n"

type pendingTool struct {
	RPCID                  any
	PublicID, TurnID, Name string
}
type conversation struct {
	mu                    sync.Mutex
	rpc                   backend
	key, threadID, effort string
	hostIdentity          string
	names                 map[string]string
	expected              []string // hashes, not a second copy of the entire user transcript
	pending               []pendingTool
	lastUsed              time.Time
	usage                 M
	actualContextWindow   int
	search                bool
}

func newConversation(ctx context.Context, rpc backend, p *prepared, cwd, key string) (*conversation, error) {
	config := M{"project_doc_max_bytes": 0, "web_search": "disabled"}
	instructions := hostInstructions + p.System
	if p.Search != nil {
		config["web_search"] = "live"
		searchConfig := M{"context_size": "medium"}
		if domains := array(p.Search["allowed_domains"]); len(domains) > 0 {
			searchConfig["allowed_domains"] = domains
		}
		config["tools"] = M{"web_search": searchConfig}
		instructions = searchInstructions
		if domains := array(p.Search["allowed_domains"]); len(domains) > 0 {
			instructions += "Only return source URLs belonging to these allowed domains (or their subdomains); do not add other links from memory: " + string(encode(domains)) + "\n\n"
		}
		instructions += p.System
	}
	if p.ContextWindow > 0 {
		config["model_context_window"] = p.ContextWindow
		config["model_auto_compact_token_limit"] = p.EffectiveContextWindow
	}
	result, e := rpc.Call(ctx, "thread/start", M{"model": p.Model, "modelProvider": "openai", "cwd": cwd, "baseInstructions": instructions, "developerInstructions": "", "approvalPolicy": "never", "sandbox": "read-only", "ephemeral": true, "environments": []any{}, "dynamicTools": p.Specs, "config": config})
	if e != nil {
		rpc.Close()
		return nil, e
	}
	id := str(object(result["thread"])["id"])
	if id == "" {
		rpc.Close()
		return nil, upstream("Codex returned an invalid thread")
	}
	return &conversation{hostIdentity: p.HostIdentity, search: p.Search != nil, rpc: rpc, key: key, threadID: id, effort: p.Effort, names: p.Names, lastUsed: time.Now(), usage: M{"input_tokens": 0, "output_tokens": 0}}, nil
}
func (c *conversation) advance(ctx context.Context, p *prepared) error {
	if len(c.pending) > 0 {
		last := object(p.Messages[len(p.Messages)-1])
		results := map[string]M{}
		extras := []any{}
		count := 0
		for _, v := range array(last["content"]) {
			b := object(v)
			if b["type"] == "tool_result" {
				count++
				id := str(b["tool_use_id"])
				results[id] = b
			} else {
				extras = append(extras, b)
			}
		}
		if count != len(c.pending) || len(results) != len(c.pending) {
			return bad("Expected the pending tool_result; conversation changed")
		}
		outputs := map[string]M{}
		for _, pending := range c.pending {
			b, ok := results[pending.PublicID]
			if !ok {
				return bad("Expected the pending tool_result; conversation changed")
			}
			value := b["content"]
			if value == nil {
				value = ""
			}
			items, e := toolResultContent(value)
			if e != nil {
				return e
			}
			outputs[pending.PublicID] = M{"contentItems": items, "success": !boolean(b["is_error"])}
			if pending.Name == "AskUserQuestion" {
				// Human UI answers are also current user input, not merely opaque tool
				// data. Enqueue the decision before releasing the blocked tool call so
				// the active turn sees it before deciding whether/how to continue.
				extras = append(extras, M{"type": "text", "text": "Latest human reply from AskUserQuestion (including any refusal or cancellation):\n" + string(encode(value)) + "\nUse this as the current user decision. Continue from the answers given; do not repeat questions already answered. Respect refusals, cancellations and any limits on the authorization."})
			}
		}
		if len(extras) > 0 {
			inputs, e := transcriptInput([]any{M{"role": "user", "content": extras}})
			if e != nil {
				return e
			}
			if inputChars(inputs) > maxTurnChars {
				return bad("Host steering exceeds Codex's character limit; compact the conversation before continuing")
			}
			if _, e = c.rpc.Call(ctx, "turn/steer", M{"threadId": c.threadID, "expectedTurnId": c.pending[0].TurnID, "input": inputs}); e != nil {
				return e
			}
		}
		for _, pending := range c.pending {
			if e := c.rpc.Send(M{"id": pending.RPCID, "result": outputs[pending.PublicID]}); e != nil {
				return e
			}
		}
		c.pending = nil
		return nil
	}
	messages := p.Messages
	if len(c.expected) > 0 {
		messages = messages[len(c.expected):]
	}
	inputs, e := transcriptInput(messages)
	if e != nil {
		return e
	}
	if inputChars(inputs) > maxTurnChars {
		if _, e = c.rpc.Call(ctx, "thread/inject_items", M{"threadId": c.threadID, "items": historyItems(inputs)}); e != nil {
			return e
		}
		inputs = []any{M{"type": "text", "text": "Continue the Claude Code conversation provided immediately above. Respond to its latest user message. Do not repeat historical tool calls."}}
	}
	params := M{"threadId": c.threadID, "input": inputs, "sandboxPolicy": M{"type": "externalSandbox", "networkAccess": "enabled"}}
	if c.effort != "" {
		params["effort"] = c.effort
	}
	_, e = c.rpc.Call(ctx, "turn/start", params)
	return e
}

type emitFunc func(string, any) error

func (c *conversation) events(ctx context.Context, emit emitFunc) error {
	texts := map[string]string{}
	searched := false
	ping := time.NewTicker(5 * time.Second)
	defer ping.Stop()
	var batch <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return &bridgeError{504, "Codex response timed out or was cancelled"}
		case <-c.rpc.Done():
			return upstream("Codex app-server exited during inference")
		case <-ping.C:
			if e := emit("ping", nil); e != nil {
				return e
			}
		case <-batch:
			return emit("done", M{"reason": "tool_use", "usage": c.usage})
		case msg := <-c.rpc.Events():
			method := str(msg["method"])
			p := object(msg["params"])
			if id, ok := msg["id"]; ok {
				if method != "item/tool/call" {
					_ = c.rpc.Send(M{"id": id, "error": M{"code": -32601, "message": "Only Claude Code dynamic tools are allowed"}})
					return upstream("Unexpected Codex request: %s", method)
				}
				name := c.names[str(p["tool"])]
				if name == "" {
					return upstream("Codex requested an unknown host tool")
				}
				args := p["arguments"]
				if s, ok := args.(string); ok {
					var parsed any
					if decode([]byte(s), &parsed) != nil {
						return upstream("Invalid Codex tool arguments")
					}
					args = parsed
				}
				if object(args) == nil {
					return upstream("Codex tool arguments were not an object")
				}
				publicID := uid("toolu_")
				c.pending = append(c.pending, pendingTool{RPCID: id, PublicID: publicID, TurnID: str(p["turnId"]), Name: name})
				if e := emit("tool", M{"type": "tool_use", "id": publicID, "name": name, "input": args}); e != nil {
					return e
				}
				if timer != nil {
					timer.Stop()
				}
				timer = time.NewTimer(75 * time.Millisecond)
				batch = timer.C
				continue
			}
			switch method {
			case "item/agentMessage/delta":
				id := str(p["itemId"])
				delta := str(p["delta"])
				texts[id] += delta
				if delta != "" {
					if e := emit("text", delta); e != nil {
						return e
					}
				}
			case "item/completed":
				item := object(p["item"])
				if item["type"] == "webSearch" {
					searched = true
				}
				if item["type"] == "agentMessage" {
					id := str(item["id"])
					text := str(item["text"])
					already := texts[id]
					if strings.HasPrefix(text, already) && len(text) > len(already) {
						if e := emit("text", text[len(already):]); e != nil {
							return e
						}
					}
					texts[id] = text
				}
			case "thread/tokenUsage/updated":
				u := object(p["tokenUsage"])
				last := object(u["last"])
				cached := integer(last["cachedInputTokens"])
				input := integer(last["inputTokens"]) - cached
				if input < 0 {
					input = 0
				}
				c.actualContextWindow = integer(u["modelContextWindow"])
				c.usage = M{"input_tokens": input, "output_tokens": integer(last["outputTokens"]), "cache_read_input_tokens": cached}
			case "turn/completed":
				turn := object(p["turn"])
				if turn["status"] != "completed" {
					detail := turn["error"]
					if detail == nil {
						detail = turn["status"]
					}
					return upstream("Codex turn failed: %s", encode(detail))
				}
				if c.search && !searched {
					return upstream("Codex web search completed without executing a search; retry the request")
				}
				return emit("done", M{"reason": "end_turn", "usage": c.usage})
			case "error":
				if !boolean(p["willRetry"]) {
					return upstream("Codex: %s", fallback(str(object(p["error"])["message"]), "upstream error"))
				}
			}
		}
	}
}

type backendFactory func(context.Context, []string, string, time.Duration) (backend, error)
type cachedResponse struct {
	key  string
	data []byte
}
type Bridge struct {
	mu                         sync.Mutex
	command                    []string
	cwd, model, effort         string
	timeout                    time.Duration
	maxSessions, contextWindow int
	catalog                    []M
	aliases                    map[string]string
	known                      map[string]M
	sessions                   []*conversation
	cache                      []cachedResponse
	cacheBytes                 int
	factory                    backendFactory
}

func newBridge(command []string, cwd, model string, catalog []M, aliases map[string]string, timeout time.Duration, maxSessions, window int, effort string) *Bridge {
	known := map[string]M{}
	for _, m := range catalog {
		known[str(m["model"])] = m
	}
	return &Bridge{command: command, cwd: cwd, model: model, catalog: catalog, aliases: aliases, known: known, timeout: timeout, maxSessions: maxSessions, contextWindow: window, effort: effort, factory: newRPC}
}
func prefixMatches(hashes, expected []string) bool {
	if len(expected) == 0 || len(hashes) < len(expected) {
		return false
	}
	for i, h := range expected {
		if hashes[i] != h {
			return false
		}
	}
	return true
}
func (b *Bridge) acquire(ctx context.Context, body M) (*conversation, *prepared, string, []byte, error) {
	p, e := prepare(body, b.model, b.aliases)
	if e != nil {
		return nil, nil, "", nil, e
	}
	info, ok := b.known[p.Model]
	if !ok {
		return nil, nil, "", nil, &bridgeError{404, "Requested model is not in your Codex catalog; run claude-codex --models"}
	}
	if b.effort != "" {
		p.Effort = b.effort
	}
	supported := array(info["supportedReasoningEfforts"])
	if p.Effort != "" && len(supported) > 0 {
		found := false
		for _, v := range supported {
			if object(v)["reasoningEffort"] == p.Effort {
				found = true
			}
		}
		if !found {
			return nil, nil, "", nil, bad("Requested effort is not supported by the selected model")
		}
	}
	p.ContextWindow = b.contextWindow
	if p.ContextWindow == 0 {
		p.ContextWindow = integer(info["maxContextWindow"])
	}
	percent := integer(info["effectiveContextWindowPercent"])
	if percent == 0 {
		percent = 95
	}
	p.EffectiveContextWindow = p.ContextWindow * percent / 100
	key := hash(M{"search": p.Search, "system": p.System, "specs": p.Specs, "model": p.Model, "effort": p.Effort, "context_window": p.ContextWindow, "host_identity": object(body["metadata"])["user_id"]})
	requestKey := hash(M{"key": key, "messages": p.Hashes})
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, cached := range b.cache {
		if cached.key == requestKey {
			return nil, p, requestKey, cached.data, nil
		}
	}
	for _, c := range b.sessions {
		if c.key == key && prefixMatches(p.Hashes, c.expected) {
			if !c.mu.TryLock() {
				return nil, nil, "", nil, &bridgeError{409, "This conversation already has a request in flight"}
			}
			b.trace("reuse", c, "history_matches")
			return c, p, requestKey, nil, nil
		}
	}
	b.retireSupersededLocked(p, key)
	b.reapLocked(time.Now().Add(-30 * time.Minute))
	if b.maxSessions > 0 && len(b.sessions) >= b.maxSessions {
		var oldest *conversation
		for _, c := range b.sessions {
			if c.mu.TryLock() {
				idle := len(c.pending) == 0
				c.mu.Unlock()
				if idle && (oldest == nil || c.lastUsed.Before(oldest.lastUsed)) {
					oldest = c
				}
			}
		}
		if oldest == nil {
			return nil, nil, "", nil, &bridgeError{503, "Too many live conversations; finish a pending tool or restart"}
		}
		b.removeLocked(oldest)
		oldest.rpc.Close()
	}
	rpc, e := b.factory(ctx, b.command, b.cwd, b.timeout)
	if e != nil {
		return nil, nil, "", nil, e
	}
	c, e := newConversation(ctx, rpc, p, b.cwd, key)
	if e != nil {
		return nil, nil, "", nil, e
	}
	c.mu.Lock()
	b.sessions = append(b.sessions, c)
	b.trace("new", c, "no_reusable_conversation")
	return c, p, requestKey, nil, nil
}

// A request that answers every pending tool of the same history/host branch
// supersedes that branch if its configuration changed. Never leave the old
// native turn waiting forever, and never retire another session or busy branch.
func (b *Bridge) retireSupersededLocked(p *prepared, key string) {
	if p.HostIdentity == "" {
		return
	}
	for i := 0; i < len(b.sessions); {
		c := b.sessions[i]
		if c.key == key || c.hostIdentity != p.HostIdentity || !prefixMatches(p.Hashes, c.expected) || !c.mu.TryLock() {
			i++
			continue
		}
		answered := map[string]bool{}
		for _, v := range p.Messages[len(c.expected):] {
			m := object(v)
			if m["role"] != "user" {
				continue
			}
			for _, v := range array(m["content"]) {
				block := object(v)
				if block["type"] == "tool_result" {
					answered[str(block["tool_use_id"])] = true
				}
			}
		}
		complete := true
		for _, pending := range c.pending {
			if !answered[pending.PublicID] {
				complete = false
				break
			}
		}
		if !complete {
			c.mu.Unlock()
			i++
			continue
		}
		b.removeLocked(c)
		b.trace("retire", c, "configuration_changed_same_history")
		c.rpc.Close()
		c.mu.Unlock()
	}
}
func (b *Bridge) trace(event string, c *conversation, reason string) {
	if os.Getenv("CLAUDE_CODEX_TRACE") != "1" {
		return
	}
	identity := c.hostIdentity
	if len(identity) > 12 {
		identity = identity[:12]
	}
	fmt.Fprintf(os.Stderr, "gpt-bridge: event=%s thread=%s host=%s live=%d reason=%s\n", event, c.threadID, identity, len(b.sessions), reason)
}

func (b *Bridge) removeLocked(c *conversation) {
	for i, x := range b.sessions {
		if x == c {
			b.sessions = append(b.sessions[:i], b.sessions[i+1:]...)
			return
		}
	}
}
func (b *Bridge) commit(c *conversation, p *prepared, key string, response M) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c.expected = append(append([]string{}, p.Hashes...), hash(M{"role": "assistant", "content": response["content"]}))
	c.lastUsed = time.Now()
	// Bound cached response memory as well as entry count. Cache only the response,
	// never another copy of a potentially multi-megabyte request transcript.
	raw := encode(response)
	const maxCacheBytes = 64 << 20
	if len(raw) > maxCacheBytes {
		return
	}
	b.cache = append(b.cache, cachedResponse{key, raw})
	b.cacheBytes += len(raw)
	for len(b.cache) > 128 || b.cacheBytes > maxCacheBytes {
		b.cacheBytes -= len(b.cache[0].data)
		b.cache = b.cache[1:]
	}
}
func (b *Bridge) discard(c *conversation) {
	b.mu.Lock()
	b.removeLocked(c)
	b.mu.Unlock()
	c.rpc.Close()
}
func (b *Bridge) reapLocked(before time.Time) {
	for i := 0; i < len(b.sessions); {
		c := b.sessions[i]
		if c.lastUsed.Before(before) && c.mu.TryLock() {
			b.sessions = append(b.sessions[:i], b.sessions[i+1:]...)
			c.rpc.Close()
			c.mu.Unlock()
		} else {
			i++
		}
	}
}
func (b *Bridge) janitor(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			b.mu.Lock()
			b.reapLocked(now.Add(-30 * time.Minute))
			b.mu.Unlock()
		}
	}
}
func (b *Bridge) close() {
	b.mu.Lock()
	sessions := b.sessions
	b.sessions = nil
	b.cache = nil
	b.cacheBytes = 0
	b.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range sessions {
		wg.Add(1)
		go func(c *conversation) { defer wg.Done(); c.rpc.Close() }(c)
	}
	wg.Wait()
}
func errorStatus(err error) int {
	var e *bridgeError
	if errors.As(err, &e) {
		return e.Status
	}
	return 500
}
