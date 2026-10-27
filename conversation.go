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
	closeOnce             sync.Once
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
	auxiliary             bool
	allowed               map[string]bool
	currentContext        M
	contextHash           string
	contextChanged        bool
	model                 string
	contextWindow         int
	statePath             string
	lease                 *os.File
	primary               bool
	compactHash           string
	baseSystemHash        string
	resetHistory          bool
	recoveredPending      []savedPending
	requestHashes         []string
	inFlight              bool
	// See usage_estimate.go.
	unmeasured                    map[int]int
	samplesEnded, samplesMeasured int
	sentContext                   map[string]string
	toolOutputTokenLimit          int
	cwd                           string
	freshThread                   bool
}

func newConversation(ctx context.Context, rpc backend, p *prepared, cwd, key string) (*conversation, error) {
	return openConversation(ctx, rpc, p, cwd, key, nil)
}
func threadStartParams(p *prepared, cwd string, persistent bool) M {
	// Claude's system prompt already carries the environment, skills and
	// permissions. Codex's own copies are dead weight in every native thread
	// (and after every compaction); process-level features.* flags do not
	// remove them.
	config := M{"project_doc_max_bytes": 0, "web_search": "disabled", "include_permissions_instructions": false, "include_collaboration_mode_instructions": false, "include_apps_instructions": false, "include_environment_context": false, "skills": M{"include_instructions": false}}
	instructions := hostInstructions + dispatcherInstructions
	specs := dispatcherSpecs()
	if p.Search != nil {
		specs = []any{}
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
	return M{"model": p.Model, "modelProvider": "openai", "cwd": cwd, "baseInstructions": instructions, "developerInstructions": "", "approvalPolicy": "never", "sandbox": "read-only", "ephemeral": !persistent, "environments": []any{}, "dynamicTools": specs, "config": config}
}
func openConversation(ctx context.Context, rpc backend, p *prepared, cwd, key string, saved *sessionCheckpoint) (*conversation, error) {
	params := threadStartParams(p, cwd, p.Persistent)
	method := "thread/start"
	if saved != nil {
		method = "thread/resume"
		params["threadId"] = saved.ThreadID
		params["excludeTurns"] = true
		delete(params, "ephemeral")
		delete(params, "dynamicTools")
		delete(params, "environments")
	}
	result, e := rpc.Call(ctx, method, params)
	if e != nil {
		rpc.Close()
		return nil, e
	}
	id := str(object(result["thread"])["id"])
	if id == "" || (saved != nil && id != saved.ThreadID) {
		rpc.Close()
		return nil, upstream("Codex returned an invalid thread")
	}
	c := &conversation{auxiliary: p.Auxiliary, baseSystemHash: p.BaseSystemHash, compactHash: p.CompactHash, contextWindow: p.ContextWindow, model: p.Model, hostIdentity: p.HostIdentity, search: p.Search != nil, rpc: rpc, cwd: cwd, key: key, threadID: id, effort: p.Effort, names: p.Names, lastUsed: time.Now(), usage: M{"input_tokens": 0, "output_tokens": 0}}
	if saved != nil {
		c.expected = append([]string{}, saved.Expected...)
		c.primary = saved.Primary
		c.compactHash = saved.CompactHash
		for key, h := range saved.Context {
			if c.sentContext == nil {
				c.sentContext = map[string]string{}
			}
			c.sentContext[key] = h
		}
		c.recoveredPending = saved.Pending
		c.resetHistory = !prefixMatches(p.Hashes, c.expected)
	}
	if !c.search {
		c.updateHost(p)
	}
	return c, nil
}
func (c *conversation) advance(ctx context.Context, p *prepared) error {
	if len(c.recoveredPending) > 0 && !c.resetHistory && !pendingAnswered(p, c.recoveredPending) {
		return bad("Expected the pending tool_result; conversation changed")
	}
	if c.resetHistory {
		if err := c.interruptForCompaction(ctx); err != nil {
			return err
		}
		if p.CompactHash != "" && p.CompactHash != c.compactHash {
			if err := c.restartNative(ctx, p); err != nil {
				return err
			}
		}
	}
	c.requestHashes = append([]string{}, p.Hashes...)
	c.toolOutputTokenLimit = p.ToolOutputTokenLimit
	c.inFlight = true
	if err := c.saveState(); err != nil {
		return err
	}
	if !c.search {
		c.updateHost(p)
	}
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
		if c.model != p.Model {
			return bad("Finish the pending tool before changing models")
		}
		if len(extras) > 0 || c.contextChanged {
			if len(extras) == 0 {
				extras = append(extras, M{"type": "text", "text": "The host configuration has been updated. Continue with the pending tool result using the current instructions and tool catalog."})
			}
			inputs, e := transcriptInput([]any{M{"role": "user", "content": extras}})
			if e != nil {
				return e
			}
			if inputChars(inputs) > maxTurnChars {
				return bad("Host steering exceeds Codex's character limit; compact the conversation before continuing")
			}
			delta, sent := c.contextDelta()
			if _, e = c.rpc.Call(ctx, "turn/steer", M{"threadId": c.threadID, "expectedTurnId": c.pending[0].TurnID, "input": inputs, "additionalContext": delta}); e != nil {
				return e
			}
			c.addUnmeasured(estimateItems(inputs))
			sent()
		}
		for _, pending := range c.pending {
			if e := c.rpc.Send(M{"id": pending.RPCID, "result": outputs[pending.PublicID]}); e != nil {
				return e
			}
			c.addUnmeasured(c.estimateToolOutput(array(outputs[pending.PublicID]["contentItems"])))
		}
		c.pending = nil
		c.contextSent()
		return nil
	}
	messages := p.Messages
	if len(c.expected) > 0 && !c.resetHistory {
		messages = messages[len(c.expected):]
	}
	inputs, e := transcriptInput(messages)
	if c.freshThread {
		inputs = append([]any{M{"type": "text", "text": "Claude compacted its host transcript. This native thread continues the SAME conversation from Claude's summary and the messages below, which are the complete current view; it is not a new task. Respond only to the latest request; never re-execute historical tool calls."}}, inputs...)
	} else if c.resetHistory {
		inputs = append([]any{M{"type": "text", "text": "Claude compacted or recovered its host transcript. This is a summary/current view of the SAME conversation, not a new task or a request to repeat tools. Preserve your existing native context. Apply the summary and respond only to its latest request; never re-execute historical tool calls."}}, inputs...)
	}
	for _, pending := range c.recoveredPending {
		if pending.Name == "AskUserQuestion" {
			inputs = append(inputs, M{"type": "text", "text": "The host messages above contain the current human reply/cancellation from AskUserQuestion. Respect that decision; do not repeat the question or treat cancellation as approval."})
		}
	}
	if len(c.recoveredPending) > 0 {
		inputs = append(inputs, M{"type": "text", "text": "The host tool_result messages above are authoritative, including errors and cancellations. The previous native executor may have stopped while Claude continued executing the tool externally. Do not repeat a host tool merely because its old native RPC was interrupted; continue from the actual host results."})
	}
	if e != nil {
		return e
	}
	appended := estimateItems(inputs)
	if inputChars(inputs) > maxTurnChars {
		if _, e = c.rpc.Call(ctx, "thread/inject_items", M{"threadId": c.threadID, "items": historyItems(inputs)}); e != nil {
			return e
		}
		inputs = []any{M{"type": "text", "text": "Continue the Claude Code conversation provided immediately above. Respond to its latest user message. Do not repeat historical tool calls."}}
	}
	params := M{"threadId": c.threadID, "input": inputs, "model": p.Model, "sandboxPolicy": M{"type": "externalSandbox", "networkAccess": "enabled"}}
	sent := func() {}
	if !c.search {
		params["additionalContext"], sent = c.contextDelta()
	}
	if c.effort != "" {
		params["effort"] = c.effort
	}
	_, e = c.rpc.Call(ctx, "turn/start", params)
	if e == nil {
		c.addUnmeasured(appended)
		sent()
		c.model = p.Model
		c.resetHistory = false
		c.freshThread = false
		c.compactHash = p.CompactHash
		c.recoveredPending = nil
		c.contextSent()
	}
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
			c.sampleEnded()
			return emit("done", M{"reason": "tool_use", "usage": c.reportedUsage()})
		case msg := <-c.rpc.Events():
			method := str(msg["method"])
			p := object(msg["params"])
			if id, ok := msg["id"]; ok {
				if method != "item/tool/call" {
					_ = c.rpc.Send(M{"id": id, "error": M{"code": -32601, "message": "Only Claude Code dynamic tools are allowed"}})
					return upstream("Unexpected Codex request: %s", method)
				}
				name := c.names[str(p["tool"])]
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
				if str(p["tool"]) == dispatcherName {
					invocation := object(args)
					name = str(invocation["name"])
					args = invocation["arguments"]
					if !c.allowed[name] || object(args) == nil {
						return upstream("Codex requested an unavailable host tool or invalid arguments")
					}
				}
				if name == "" {
					return upstream("Codex requested an unknown host tool")
				}
				c.addUnmeasured(len(encode(args)) / estimatedCharsPerToken)
				publicID := uid("toolu_")
				c.pending = append(c.pending, pendingTool{RPCID: id, PublicID: publicID, TurnID: str(p["turnId"]), Name: name})
				if err := c.saveState(); err != nil {
					return err
				}
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
			case "thread/compacted":
				c.sentContext = nil
			case "item/completed":
				item := object(p["item"])
				if item["type"] == "contextCompaction" {
					c.sentContext = nil
				}
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
				c.sampleMeasured(M{"input_tokens": input, "output_tokens": integer(last["outputTokens"]), "cache_read_input_tokens": cached})
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
				c.turnEnded()
				return emit("done", M{"reason": "end_turn", "usage": c.reportedUsage()})
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
	key           string
	data          []byte
	pendingThread string
	pendingState  string
}
type Bridge struct {
	mu                         sync.Mutex
	command                    []string
	cwd, model, effort         string
	timeout                    time.Duration
	idleTimeout                time.Duration
	maxSessions, contextWindow int
	catalog                    []M
	aliases                    map[string]string
	known                      map[string]M
	sessions                   []*conversation
	cache                      []cachedResponse
	cacheBytes                 int
	factory                    backendFactory
	stateDir                   string
	retiring                   map[string]chan struct{}
	retireWG                   sync.WaitGroup
	closed                     bool
}

func newBridge(command []string, cwd, model string, catalog []M, aliases map[string]string, timeout time.Duration, maxSessions, window int, effort string) *Bridge {
	known := map[string]M{}
	for _, m := range catalog {
		known[str(m["model"])] = m
	}
	return &Bridge{command: command, cwd: cwd, model: model, catalog: catalog, aliases: aliases, known: known, timeout: timeout, idleTimeout: 2 * time.Minute, maxSessions: maxSessions, contextWindow: window, effort: effort, factory: newRPC, retiring: map[string]chan struct{}{}}
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
	p.ToolOutputTokenLimit = integer(info["toolOutputTokenLimit"])
	key := hash(M{"search": p.Search, "system": p.System, "specs": p.Specs, "model": p.Model, "effort": p.Effort, "context_window": p.ContextWindow, "host_identity": object(body["metadata"])["user_id"]})
	requestKey := hash(M{"key": key, "messages": p.Hashes})
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneStoppedLocked()
	for _, cached := range b.cache {
		if cached.key == requestKey {
			if cached.pendingThread != "" {
				valid := false
				for _, owner := range b.sessions {
					if owner.threadID != cached.pendingThread || !owner.mu.TryLock() {
						continue
					}
					valid = len(owner.pending) > 0 && len(owner.expected) == len(p.Hashes)+1 && prefixMatches(owner.expected, p.Hashes)
					owner.mu.Unlock()
					if valid {
						break
					}
				}
				if !valid {
					if !cachedPendingCheckpointMatches(cached, p) {
						continue
					}
				}
			}
			return nil, p, requestKey, cached.data, nil
		}
	}
	for _, c := range b.sessions {
		compatible := (c.key == key || (p.HostIdentity != "" && c.hostIdentity == p.HostIdentity && !c.search && p.Search == nil && c.contextWindow == p.ContextWindow)) && c.auxiliary == p.Auxiliary
		if compatible && prefixMatches(p.Hashes, c.expected) {
			if !c.mu.TryLock() {
				return nil, nil, "", nil, &bridgeError{409, "This conversation already has a request in flight"}
			}
			if len(c.pending) > 0 && c.key != key {
				results := map[string]bool{}
				for _, v := range array(object(p.Messages[len(p.Messages)-1])["content"]) {
					m := object(v)
					if m["type"] == "tool_result" {
						results[str(m["tool_use_id"])] = true
					}
				}
				complete := true
				for _, pending := range c.pending {
					if !results[pending.PublicID] {
						complete = false
						break
					}
				}
				if !complete {
					c.mu.Unlock()
					continue
				}
			}
			if len(c.pending) > 0 && c.model != p.Model {
				c.mu.Unlock()
				return nil, nil, "", nil, bad("Finish the pending tool before changing models")
			}
			b.trace("reuse", c, "history_matches")
			return c, p, requestKey, nil, nil
		}
	}
	// Background-agent completion can re-render earlier host messages. Native
	// pending tool IDs still identify the live owner unambiguously even when
	// the transcript prefix no longer matches. Deliver to that existing RPC,
	// never try to reacquire its own persistent lease or replay the tool.
	if p.HostIdentity != "" && p.Search == nil && !p.Auxiliary {
		for _, c := range b.sessions {
			if c.hostIdentity != p.HostIdentity || c.search || c.auxiliary || !c.mu.TryLock() {
				continue
			}
			pending := make([]savedPending, 0, len(c.pending))
			for _, v := range c.pending {
				pending = append(pending, savedPending{PublicID: v.PublicID, Name: v.Name})
			}
			if !pendingAnswered(p, pending) {
				c.mu.Unlock()
				continue
			}
			if c.model != p.Model {
				c.mu.Unlock()
				return nil, nil, "", nil, bad("Finish the pending tool before changing models")
			}
			b.trace("reuse", c, "pending_tool_identifies_live_owner")
			return c, p, requestKey, nil, nil
		}
	}
	if p.HostIdentity != "" && p.CompactHash != "" && p.Search == nil && !p.Auxiliary {
		candidates := []*conversation{}
		newCompaction := true
		identityCount := 0
		var only *conversation
		for _, c := range b.sessions {
			if c.hostIdentity == p.HostIdentity && !c.search && !c.auxiliary {
				identityCount++
				if c.compactHash == p.CompactHash {
					newCompaction = false
				}
				only = c
				if c.compactHash != p.CompactHash && c.baseSystemHash == p.BaseSystemHash {
					candidates = append(candidates, c)
				}
			}
		}
		if !newCompaction {
			candidates = nil
		}
		if newCompaction {
			id, err := b.identifyCompaction(p)
			if err != nil {
				return nil, nil, "", nil, err
			}
			p.CompactionThread = id
			if id != "" {
				candidates = nil
				for _, c := range b.sessions {
					if c.threadID == id {
						if !c.mu.TryLock() {
							return nil, nil, "", nil, &bridgeError{409, "This conversation already has a request in flight"}
						}
						c.resetHistory = true
						b.trace("compact", c, "host_compaction_lineage_matched")
						return c, p, requestKey, nil, nil
					}
				}
			}
		}
		if p.CompactionThread == "" && newCompaction && len(candidates) == 0 && identityCount == 1 && only.compactHash != p.CompactHash {
			candidates = append(candidates, only)
		}
		if len(candidates) > 1 {
			return nil, nil, "", nil, &bridgeError{409, "Compacted history matches multiple branches; refusing to mix their contexts"}
		}
		if len(candidates) == 1 {
			c := candidates[0]
			if !c.mu.TryLock() {
				return nil, nil, "", nil, &bridgeError{409, "This conversation already has a request in flight"}
			}
			c.resetHistory = true
			b.trace("compact", c, "host_history_compacted")
			return c, p, requestKey, nil, nil
		}
	}
	b.retireSupersededLocked(p, key)
	if b.idleTimeout > 0 {
		b.reapLocked(time.Now().Add(-b.idleTimeout))
	}
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
		oldest.close()
	}
	c, resumed, e := b.openPersistent(ctx, p, key)
	if e != nil {
		return nil, nil, "", nil, e
	}
	c.mu.Lock()
	b.sessions = append(b.sessions, c)
	if resumed {
		b.trace("resume", c, "persistent_thread_resumed")
	} else {
		b.trace("new", c, "no_reusable_conversation")
	}
	return c, p, requestKey, nil, nil
}

// A request that answers every pending tool of the same history/host branch
// supersedes that branch if its configuration changed. Never leave the old
// native turn waiting forever, and never retire another session or busy branch.
func (b *Bridge) retireSupersededLocked(p *prepared, key string) {
	if p.HostIdentity == "" || p.Auxiliary {
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
		c.close()
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
	role := "conversation"
	if c.primary {
		role = "primary"
	}
	if c.search {
		role = "web_search"
	}
	if c.auxiliary {
		role = "compact_summary"
	}
	fmt.Fprintf(os.Stderr, "gpt-bridge: event=%s thread=%s host=%s live=%d role=%s reason=%s\n", event, c.threadID, identity, len(b.sessions), role, reason)
}

func (b *Bridge) removeLocked(c *conversation) {
	for i, x := range b.sessions {
		if x == c {
			b.sessions = append(b.sessions[:i], b.sessions[i+1:]...)
			return
		}
	}
}
func (b *Bridge) commit(c *conversation, p *prepared, key string, response M) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	c.expected = append(append([]string{}, p.Hashes...), hash(M{"role": "assistant", "content": response["content"]}))
	c.lastUsed = time.Now()
	c.inFlight = false
	c.requestHashes = nil
	c.compactHash = p.CompactHash
	if err := c.saveState(); err != nil {
		return err
	}
	// Bound cached response memory as well as entry count. Cache only the response,
	// never another copy of a potentially multi-megabyte request transcript.
	raw := encode(response)
	const maxCacheBytes = 64 << 20
	if len(raw) > maxCacheBytes {
		return nil
	}
	pendingThread := ""
	if response["stop_reason"] == "tool_use" {
		pendingThread = c.threadID
	}
	b.cache = append(b.cache, cachedResponse{key: key, data: raw, pendingThread: pendingThread, pendingState: c.statePath})
	b.cacheBytes += len(raw)
	for len(b.cache) > 128 || b.cacheBytes > maxCacheBytes {
		b.cacheBytes -= len(b.cache[0].data)
		b.cache = b.cache[1:]
	}
	if c.auxiliary && len(c.pending) == 0 {
		b.removeLocked(c)
		c.close()
	}
	return nil
}
func (b *Bridge) discard(c *conversation) {
	b.mu.Lock()
	b.removeLocked(c)
	b.mu.Unlock()
	c.close()
}
func (b *Bridge) reapLocked(before time.Time) {
	if b.closed || b.idleTimeout <= 0 {
		return
	}
	for i := 0; i < len(b.sessions); {
		c := b.sessions[i]
		// A named ephemeral conversation has no durable recovery path. Keep it
		// resident; persistent named threads can release their executor safely.
		if c.hostIdentity != "" && !c.search && !c.auxiliary && c.statePath == "" {
			i++
			continue
		}
		if c.lastUsed.Before(before) && c.mu.TryLock() {
			if c.inFlight || (c.statePath == "" && (len(c.pending) > 0 || len(c.recoveredPending) > 0)) {
				c.mu.Unlock()
				i++
				continue
			}
			if c.statePath != "" {
				if err := c.saveState(); err != nil {
					b.trace("keep", c, "idle_checkpoint_write_failed")
					c.mu.Unlock()
					i++
					continue
				}
			}
			b.sessions = append(b.sessions[:i], b.sessions[i+1:]...)
			done := make(chan struct{})
			b.retiring[c.threadID] = done
			b.retireWG.Add(1)
			b.trace("unload", c, "idle_executor_checkpoint_preserved")
			// Shutdown can take seconds. Do not block unrelated requests while
			// waiting for a native process group; its lease stays held until exit.
			go func(c *conversation, done chan struct{}) {
				defer b.retireWG.Done()
				c.close()
				c.mu.Unlock()
				close(done)
				b.mu.Lock()
				if b.retiring[c.threadID] == done {
					delete(b.retiring, c.threadID)
				}
				b.mu.Unlock()
			}(c, done)
		} else {
			i++
		}
	}
}
func (b *Bridge) janitor(ctx context.Context) {
	interval := 30 * time.Second
	if b.idleTimeout > 0 && b.idleTimeout/2 < interval {
		interval = max(time.Second, b.idleTimeout/2)
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			b.mu.Lock()
			b.reapLocked(now.Add(-b.idleTimeout))
			b.mu.Unlock()
		}
	}
}
func (b *Bridge) close() {
	b.mu.Lock()
	b.closed = true
	sessions := b.sessions
	b.sessions = nil
	b.cache = nil
	b.cacheBytes = 0
	b.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range sessions {
		wg.Add(1)
		go func(c *conversation) { defer wg.Done(); c.mu.Lock(); defer c.mu.Unlock(); c.close() }(c)
	}
	wg.Wait()
	b.retireWG.Wait()
}
func errorStatus(err error) int {
	var e *bridgeError
	if errors.As(err, &e) {
		return e.Status
	}
	return 500
}
