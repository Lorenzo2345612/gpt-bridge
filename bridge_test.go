package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProtocolParity(t *testing.T) {
	raw, e := os.ReadFile("protocol-fixtures.json")
	if e != nil {
		t.Fatal(e)
	}
	var cases []struct {
		Body     M                 `json:"body"`
		Default  string            `json:"default_model"`
		Aliases  map[string]string `json:"aliases"`
		Expected M                 `json:"expected"`
		Error    string            `json:"error"`
	}
	if e = decode(raw, &cases); e != nil {
		t.Fatal(e)
	}
	for i, c := range cases {
		p, e := prepare(c.Body, c.Default, c.Aliases)
		if c.Error != "" {
			if e == nil {
				t.Errorf("case %d expected rejection: %s", i, c.Error)
			}
			continue
		}
		if e != nil {
			t.Errorf("case %d: %v", i, e)
			continue
		}
		var effort any
		if p.Effort != "" {
			effort = p.Effort
		}
		got := M{"system": p.System, "specs": p.Specs, "names": p.Names, "model": p.Model, "messages": p.Messages, "effort": effort}
		if string(encode(got)) != string(encode(c.Expected)) {
			t.Errorf("case %d mismatch\n got %s\nwant %s", i, encode(got), encode(c.Expected))
		}
	}
}

type rpcCall struct {
	Method string
	Params M
}
type fakeRPC struct {
	mu                     sync.Mutex
	events                 chan M
	done                   chan struct{}
	calls                  []rpcCall
	sent                   []M
	tool                   string
	toolsPerTurn, received int
	closed                 bool
	fail                   bool
}

func newFake() *fakeRPC {
	return &fakeRPC{events: make(chan M, 64), done: make(chan struct{}), toolsPerTurn: 1}
}
func (f *fakeRPC) Call(ctx context.Context, method string, p M) (M, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, rpcCall{method, p})
	switch method {
	case "thread/start":
		if specs := array(p["dynamicTools"]); len(specs) > 0 {
			f.tool = str(object(specs[0])["name"])
		}
		return M{"thread": M{"id": uid("thread_")}}, nil
	case "thread/resume":
		f.tool = dispatcherName
		return M{"thread": M{"id": p["threadId"]}}, nil
	case "turn/interrupt":
		f.events <- M{"method": "turn/completed", "params": M{"turn": M{"id": p["turnId"], "status": "interrupted"}}}
	case "thread/compact/start":
		f.events <- M{"method": "turn/completed", "params": M{"threadId": p["threadId"], "turn": M{"id": "compact-test", "status": "completed"}}}
	case "turn/start":
		var active []any
		if f.tool == dispatcherName {
			_ = decode([]byte(contextText(object(p["additionalContext"]), "tools")), &active)
		}
		if f.tool != "" && (f.tool != dispatcherName || len(active) > 0) {
			f.received = 0
			for i := 0; i < f.toolsPerTurn; i++ {
				f.events <- M{"id": i + 1, "method": "item/tool/call", "params": M{"tool": f.tool, "arguments": func() M {
					if f.tool == dispatcherName {
						return M{"name": object(active[0])["name"], "arguments": M{"skill": "my-skill"}}
					}
					return M{"skill": "my-skill"}
				}(), "turnId": "t1"}}
			}
		} else {
			f.finishLocked()
		}
	}
	return M{}, nil
}
func (f *fakeRPC) finishLocked() {
	if f.fail {
		f.events <- M{"method": "error", "params": M{"willRetry": false, "error": M{"message": "quota exceeded"}}}
		return
	}
	f.events <- M{"method": "item/agentMessage/delta", "params": M{"itemId": "answer", "delta": "¡Hecho!"}}
	f.events <- M{"method": "item/completed", "params": M{"item": M{"type": "agentMessage", "id": "answer", "text": "¡Hecho!"}}}
	f.events <- M{"method": "thread/tokenUsage/updated", "params": M{"tokenUsage": M{"modelContextWindow": 828400, "last": M{"inputTokens": 20, "cachedInputTokens": 5, "outputTokens": 4}}}}
	f.events <- M{"method": "turn/completed", "params": M{"turn": M{"status": "completed"}}}
}
func (f *fakeRPC) Send(p M) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, p)
	if p["result"] != nil {
		f.received++
		if f.received >= f.toolsPerTurn {
			f.finishLocked()
		}
	}
	return nil
}
func (f *fakeRPC) Events() <-chan M      { return f.events }
func (f *fakeRPC) Done() <-chan struct{} { return f.done }
func (f *fakeRPC) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.done)
	}
}
func (f *fakeRPC) snapshot() ([]rpcCall, []M) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rpcCall{}, f.calls...), append([]M{}, f.sent...)
}
func testCatalog() []M {
	r := []M{}
	for _, name := range []string{"gpt-test-sol", "gpt-test-astra", "gpt-test-terra", "gpt-test-luna"} {
		r = append(r, M{"model": name, "maxContextWindow": 872000, "effectiveContextWindowPercent": 95, "supportedReasoningEfforts": []any{M{"reasoningEffort": "low"}, M{"reasoningEffort": "high"}}})
	}
	return r
}
func testRequest(tools bool) M {
	ts := []any{}
	if tools {
		ts = append(ts, M{"name": "Skill", "description": "Load a skill", "input_schema": M{"type": "object", "properties": M{"skill": M{"type": "string"}}, "required": []any{"skill"}}})
	}
	return M{"model": "gpt-test-sol", "max_tokens": 100, "system": "Use my Skill tool", "messages": []any{M{"role": "user", "content": "Use the skill"}}, "tools": ts}
}

type httpFixture struct {
	b         *Bridge
	server    *httptest.Server
	mu        sync.Mutex
	backends  []*fakeRPC
	configure func(*fakeRPC)
}

func newHTTPFixture(t *testing.T) *httpFixture {
	t.Helper()
	catalog := testCatalog()
	aliases, _ := modelAliases(catalog, "gpt-test-sol", nil)
	b := newBridge(nil, t.TempDir(), "gpt-test-sol", catalog, aliases, 3*time.Second, 8, 0, "")
	f := &httpFixture{b: b}
	b.factory = func(context.Context, []string, string, time.Duration) (backend, error) {
		rpc := newFake()
		f.mu.Lock()
		if f.configure != nil {
			f.configure(rpc)
		}
		f.backends = append(f.backends, rpc)
		f.mu.Unlock()
		return rpc, nil
	}
	f.server = httptest.NewServer(&apiHandler{b, "test-local-secret-123456789"})
	t.Cleanup(func() { f.server.Close(); b.close() })
	return f
}
func (f *httpFixture) rpc() *fakeRPC {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.backends[len(f.backends)-1]
}
func (f *httpFixture) fetch(t *testing.T, method, path string, body M, token string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(encode(body))
	}
	req, e := http.NewRequest(method, f.server.URL+path, reader)
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("x-api-key", token)
	r, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Body.Close()
	raw, e := io.ReadAll(r.Body)
	if e != nil {
		t.Fatal(e)
	}
	return r.StatusCode, raw
}
func (f *httpFixture) post(t *testing.T, body M) (int, M) {
	t.Helper()
	status, raw := f.fetch(t, "POST", "/v1/messages", body, "test-local-secret-123456789")
	var result M
	if e := decode(raw, &result); e != nil {
		t.Fatalf("invalid response %s", raw)
	}
	return status, result
}
func TestAuthDiscoveryAndCount(t *testing.T) {
	f := newHTTPFixture(t)
	for _, path := range []string{"/health", "/v1/models"} {
		status, _ := f.fetch(t, "GET", path, nil, "wrong")
		if status != 401 {
			t.Fatal(status)
		}
	}
	status, raw := f.fetch(t, "GET", "/v1/models", nil, "test-local-secret-123456789")
	if status != 200 || !bytes.Contains(raw, []byte("gpt-test-terra")) {
		t.Fatalf("%d %s", status, raw)
	}
	status, raw = f.fetch(t, "POST", "/v1/messages/count_tokens", testRequest(false), "test-local-secret-123456789")
	if status != 200 || !bytes.Contains(raw, []byte("input_tokens")) {
		t.Fatal(status, string(raw))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.backends) != 0 {
		t.Fatal("Unexpected backend for discovery/count")
	}
}
func TestLiveToolRoundTripAndReuse(t *testing.T) {
	f := newHTTPFixture(t)
	body := testRequest(true)
	status, first := f.post(t, body)
	if status != 200 || first["stop_reason"] != "tool_use" {
		t.Fatal(status, first)
	}
	tool := object(array(first["content"])[0])
	if tool["name"] != "Skill" {
		t.Fatal(tool)
	}
	body["messages"] = append(array(body["messages"]), M{"role": "assistant", "content": first["content"]}, M{"role": "user", "content": []any{M{"type": "tool_result", "tool_use_id": tool["id"], "content": "My skill instructions"}}})
	status, last := f.post(t, body)
	if status != 200 || last["stop_reason"] != "end_turn" {
		t.Fatal(status, last)
	}
	if integer(object(last["usage"])["cache_read_input_tokens"]) != 5 {
		t.Fatal(last)
	}
	calls, sent := f.rpc().snapshot()
	if len(calls) != 2 || calls[0].Params["sandbox"] != "read-only" || object(calls[1].Params["sandboxPolicy"])["type"] != "externalSandbox" {
		t.Fatal(calls)
	}
	if len(sent) != 1 || !boolean(object(sent[0]["result"])["success"]) {
		t.Fatal(sent)
	}
	body["messages"] = append(array(body["messages"]), M{"role": "assistant", "content": last["content"]}, M{"role": "user", "content": "Next"})
	status, _ = f.post(t, body)
	if status != 200 {
		t.Fatal(status)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.backends) != 1 {
		t.Fatal("Conversation was not reused")
	}
}
func TestConcurrentToolsOutOfOrderAndRetryCache(t *testing.T) {
	f := newHTTPFixture(t)
	f.configure = func(r *fakeRPC) { r.toolsPerTurn = 2 }
	body := testRequest(true)
	_, first := f.post(t, body)
	_, retry := f.post(t, body)
	if !reflect.DeepEqual(first["content"], retry["content"]) {
		t.Fatal("Retry changed tool IDs")
	}
	tools := array(first["content"])
	if len(tools) != 2 {
		t.Fatal(first)
	}
	body["messages"] = append(array(body["messages"]), M{"role": "assistant", "content": first["content"]}, M{"role": "user", "content": []any{M{"type": "tool_result", "tool_use_id": object(tools[1])["id"], "content": "second"}, M{"type": "tool_result", "tool_use_id": object(tools[0])["id"], "content": "first", "is_error": true}}})
	status, last := f.post(t, body)
	if status != 200 || last["stop_reason"] != "end_turn" {
		t.Fatal(status, last)
	}
	_, sent := f.rpc().snapshot()
	if len(sent) != 2 || boolean(object(sent[0]["result"])["success"]) {
		t.Fatal(sent)
	}
}
func TestMismatchedToolResultsRejected(t *testing.T) {
	f := newHTTPFixture(t)
	body := testRequest(true)
	_, first := f.post(t, body)
	body["messages"] = append(array(body["messages"]), M{"role": "assistant", "content": first["content"]}, M{"role": "user", "content": []any{M{"type": "tool_result", "tool_use_id": "wrong", "content": "wrong"}}})
	status, _ := f.post(t, body)
	if status != 400 {
		t.Fatal(status)
	}
}

func TestHumanAnswerIsDeliveredAsDecisionBeforeToolResult(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(map[bool]string{false: "answer", true: "refusal"}[refused], func(t *testing.T) {
			f := newHTTPFixture(t)
			body := testRequest(true)
			toolDef := object(array(body["tools"])[0])
			toolDef["name"] = "AskUserQuestion"
			_, first := f.post(t, body)
			tool := object(array(first["content"])[0])
			answer := `Your questions have been answered: "Scope"="Complete initiative". You can now continue with these answers in mind.`
			if refused {
				answer = "The user cancelled the question; do not proceed."
			}
			body["messages"] = append(array(body["messages"]), M{"role": "assistant", "content": first["content"]}, M{"role": "user", "content": []any{M{"type": "tool_result", "tool_use_id": tool["id"], "content": answer, "is_error": refused}}})
			status, last := f.post(t, body)
			if status != 200 || last["stop_reason"] != "end_turn" {
				t.Fatal(status, last)
			}
			calls, sent := f.rpc().snapshot()
			if len(calls) != 3 || calls[2].Method != "turn/steer" || calls[2].Params["expectedTurnId"] != "t1" {
				t.Fatal(calls)
			}
			steering := str(object(array(calls[2].Params["input"])[0])["text"])
			var transcript []any
			if e := decode([]byte(strings.TrimPrefix(steering, transcriptHeader)), &transcript); e != nil {
				t.Fatal(e)
			}
			text := str(object(array(object(transcript[0])["content"])[0])["text"])
			if !strings.Contains(text, "Latest human reply") || !strings.Contains(text, string(encode(answer))) {
				t.Fatal("Human decision was dropped")
			}
			if len(sent) != 1 || boolean(object(sent[0]["result"])["success"]) == refused {
				t.Fatal(sent)
			}
			if str(object(array(object(sent[0]["result"])["contentItems"])[0])["text"]) != answer {
				t.Fatal("Changed original tool result")
			}
		})
	}
}
func TestModelSwitchRolesAndUnsupportedModels(t *testing.T) {
	f := newHTTPFixture(t)
	for _, c := range []struct{ role, want string }{{"opus", "gpt-test-sol"}, {"sonnet", "gpt-test-terra"}, {"haiku", "gpt-test-luna"}, {"fable", "gpt-test-astra"}} {
		body := testRequest(false)
		body["model"] = c.role
		status, r := f.post(t, body)
		if status != 200 || r["model"] != c.want {
			t.Fatal(c, status, r)
		}
	}
	body := testRequest(false)
	body["model"] = "missing"
	status, _ := f.post(t, body)
	if status != 404 {
		t.Fatal(status)
	}
}
func TestEffortValidationAndContextOverrides(t *testing.T) {
	f := newHTTPFixture(t)
	body := testRequest(false)
	body["output_config"] = M{"effort": "high"}
	status, _ := f.post(t, body)
	if status != 200 {
		t.Fatal(status)
	}
	calls, _ := f.rpc().snapshot()
	if calls[1].Params["effort"] != "high" || integer(object(calls[0].Params["config"])["model_context_window"]) != 872000 {
		t.Fatal(calls)
	}
	body["output_config"] = M{"effort": "max"}
	status, _ = f.post(t, body)
	if status != 400 {
		t.Fatal(status)
	}
}
func TestOversizedHistoryAndUnicodeBoundary(t *testing.T) {
	f := newHTTPFixture(t)
	body := testRequest(false)
	text := "BEGIN" + strings.Repeat("x", maxTurnChars) + "END"
	body["messages"] = []any{M{"role": "user", "content": text}}
	status, r := f.post(t, body)
	if status != 200 {
		t.Fatal(status, r)
	}
	calls, _ := f.rpc().snapshot()
	if len(calls) != 3 || calls[1].Method != "thread/inject_items" {
		t.Fatal("Missing oversized history injection")
	}
	injected := str(object(array(object(array(calls[1].Params["items"])[0])["content"])[0])["text"])
	var history []any
	if e := decode([]byte(strings.TrimPrefix(injected, transcriptHeader)), &history); e != nil {
		t.Fatal(e)
	}
	if str(object(array(object(history[0])["content"])[0])["text"]) != text {
		t.Fatal("History truncated")
	}
	if inputChars([]any{M{"type": "text", "text": strings.Repeat("😀", maxTurnChars/2)}, M{"type": "text", "text": strings.Repeat("a", maxTurnChars/2)}}) != maxTurnChars {
		t.Fatal("Character limit counted bytes")
	}
}
func TestStreamingSuccessAndError(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "error"}[fail], func(t *testing.T) {
			f := newHTTPFixture(t)
			f.configure = func(r *fakeRPC) { r.fail = fail }
			body := testRequest(false)
			body["stream"] = true
			status, raw := f.fetch(t, "POST", "/v1/messages", body, "test-local-secret-123456789")
			if status != 200 || !bytes.Contains(raw, []byte("event: message_start")) {
				t.Fatal(status, string(raw))
			}
			if fail {
				if !bytes.Contains(raw, []byte("event: error")) || !bytes.Contains(raw, []byte("quota exceeded")) {
					t.Fatal(string(raw))
				}
			} else if !bytes.Contains(raw, []byte("event: message_stop")) {
				t.Fatal(string(raw))
			}
		})
	}
}
func TestCatalogContextAndEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	raw := M{"models": []any{M{"slug": "gpt-test-sol", "context_window": 500000, "max_context_window": 872000, "effective_context_window_percent": 95}, M{"slug": "hidden", "max_context_window": 1000000}}}
	_ = os.WriteFile(path, encode(raw), 0600)
	catalog := []M{{"model": "gpt-test-sol"}}
	enrichCatalog(catalog, path)
	window, e := clientContext(catalog, 0)
	if e != nil || window != 828400 || len(catalog) != 1 {
		t.Fatal(window, e, catalog)
	}
	if _, e = clientContext(catalog, 1000000); e == nil {
		t.Fatal("Accepted oversized context")
	}
	env := claudeEnvironment(map[string]string{"ANTHROPIC_API_KEY": "old", "CLAUDE_CODE_USE_BEDROCK": "1", "MY_HOOK": "keep"}, "token", "url", "gpt-test-sol", nil, window)
	if _, ok := env["CLAUDE_CONFIG_DIR"]; ok {
		t.Fatal("Changed implicit profile")
	}
	if env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] != "828400" || env["MY_HOOK"] != "keep" || env["ANTHROPIC_API_KEY"] != "" {
		t.Fatal(env)
	}
}
func TestRestoreModelAndParseOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	_ = os.WriteFile(path, []byte(`{"model":"gpt-test-sol","theme":"new"}`), 0600)
	if e := restoreHostModel(path, M{"model": "opus", "theme": "old"}, map[string]M{"gpt-test-sol": {}}); e != nil {
		t.Fatal(e)
	}
	m := readSettings(path)
	if m["model"] != "opus" || m["theme"] != "new" {
		t.Fatal(m)
	}
	_ = os.WriteFile(path, []byte(`{"model":"sonnet"}`), 0600)
	_ = restoreHostModel(path, M{"model": "opus"}, map[string]M{"gpt-test-sol": {}})
	if readSettings(path)["model"] != "sonnet" {
		t.Fatal("Overwrote concurrent native selection")
	}
	o, e := parseOptions([]string{"--dangerously-skip-permissions", "--model", "gpt-test-sol", "--max-sessions=8", "--resume"})
	if e != nil || o.maxSessions != 8 || o.model != "gpt-test-sol" || !reflect.DeepEqual(o.claudeArgs, []string{"--dangerously-skip-permissions", "--resume"}) {
		t.Fatal(o, e)
	}
}
func TestImagesAndHistoryAreNotExecuted(t *testing.T) {
	messages := []any{M{"role": "assistant", "content": []any{M{"type": "tool_use", "id": "old", "name": "Read", "input": M{"file_path": "old"}}}}, M{"role": "user", "content": []any{M{"type": "tool_result", "tool_use_id": "old", "content": "historical result"}, M{"type": "image", "source": M{"type": "base64", "media_type": "image/png", "data": "YWJj"}}}}}
	inputs, e := transcriptInput(messages)
	if e != nil {
		t.Fatal(e)
	}
	if len(inputs) != 2 || !strings.Contains(str(object(inputs[0])["text"]), "historical result") {
		t.Fatal(inputs)
	}
	items := historyItems(inputs)
	content := array(object(items[0])["content"])
	if object(content[1])["image_url"] != "data:image/png;base64,YWJj" {
		t.Fatal(items)
	}
	if _, e = toolResultContent([]any{M{"type": "image", "source": M{"type": "url", "url": "http://unsafe"}}}); e == nil {
		t.Fatal("Accepted insecure image URL")
	}
}
func TestIdleReapingSkipsInFlightAndClose(t *testing.T) {
	f := newHTTPFixture(t)
	f.post(t, testRequest(false))
	b := f.b
	b.mu.Lock()
	c := b.sessions[0]
	c.lastUsed = time.Now().Add(-time.Hour)
	c.mu.Lock()
	b.reapLocked(time.Now())
	if len(b.sessions) != 1 {
		t.Fatal("Reaped active request")
	}
	c.mu.Unlock()
	b.reapLocked(time.Now())
	if len(b.sessions) != 0 {
		t.Fatal("Did not reap expired conversation")
	}
	b.mu.Unlock()
}
func BenchmarkPrepareLargeHistory(b *testing.B) {
	body := testRequest(false)
	messages := []any{}
	for i := 0; i < 100; i++ {
		messages = append(messages, M{"role": "user", "content": strings.Repeat("context ", 1250)}, M{"role": "assistant", "content": "ok"})
	}
	body["messages"] = append(messages, M{"role": "user", "content": "continue"})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, e := prepare(body, "gpt-test-sol", nil); e != nil {
			b.Fatal(e)
		}
	}
}
