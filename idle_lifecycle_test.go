package main

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func expireFixtureConversation(f *httpFixture) {
	f.b.mu.Lock()
	f.b.sessions[0].lastUsed = time.Now().Add(-time.Hour)
	f.b.reapLocked(time.Now().Add(-2 * time.Minute))
	f.b.mu.Unlock()
}

func TestPersistentIdleExecutorUnloadsAndResumesSameThread(t *testing.T) {
	f := persistentFixture(t, t.TempDir())
	body := testRequest(false)
	body["metadata"] = M{"user_id": "idle-persistent-thread"}
	status, response := f.post(t, body)
	if status != 200 {
		t.Fatal(status, response)
	}
	c := f.b.sessions[0]
	id, path := c.threadID, c.statePath
	expireFixtureConversation(f)
	f.b.mu.Lock()
	remaining := len(f.b.sessions)
	f.b.mu.Unlock()
	if remaining != 0 {
		t.Fatal("idle identified executor remained resident", remaining)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("removed checkpoint", err)
	}
	status, response = f.post(t, continueRequest(body, response, "Continue after idle unload"))
	if status != 200 {
		t.Fatal(status, response)
	}
	calls, _ := f.rpc().snapshot()
	if calls[0].Method != "thread/resume" || calls[0].Params["threadId"] != id {
		t.Fatal("lost native identity", calls)
	}
}

func TestIdlePendingToolUnloadsWithoutReplayingHostInvocation(t *testing.T) {
	f := persistentFixture(t, t.TempDir())
	body := testRequest(true)
	body["metadata"] = M{"user_id": "idle-pending-thread"}
	_, response := f.post(t, body)
	id := f.b.sessions[0].threadID
	expireFixtureConversation(f)
	f.b.mu.Lock()
	remaining := len(f.b.sessions)
	f.b.mu.Unlock()
	if remaining != 0 {
		t.Fatal("pending host tool retained an idle native executor")
	}
	status, retry := f.post(t, body)
	if status != 200 || string(encode(retry["content"])) != string(encode(response["content"])) {
		t.Fatal("retry replaced the public tool invocation", status, retry)
	}
	f.mu.Lock()
	backends := len(f.backends)
	f.mu.Unlock()
	if backends != 1 {
		t.Fatal("retry unnecessarily reopened executor")
	}
	tool := object(array(response["content"])[0])
	next := continueRequest(body, response, "")
	messages := array(next["messages"])
	messages[len(messages)-1] = M{"role": "user", "content": []any{M{"type": "tool_result", "tool_use_id": tool["id"], "content": "executed once"}}}
	next["messages"] = messages
	next["tools"] = []any{}
	status, result := f.post(t, next)
	if status != 200 || result["stop_reason"] != "end_turn" {
		t.Fatal(status, result)
	}
	calls, sent := f.rpc().snapshot()
	if len(sent) != 0 || calls[0].Method != "thread/resume" || calls[0].Params["threadId"] != id {
		t.Fatal("pending result resumed the wrong thread", calls)
	}
	if !strings.Contains(string(encode(calls[len(calls)-1].Params["input"])), "executed once") {
		t.Fatal("lost host tool result")
	}
}

func TestEphemeralPendingToolKeepsItsOnlyExecutor(t *testing.T) {
	f := newHTTPFixture(t)
	f.post(t, testRequest(true))
	expireFixtureConversation(f)
	if len(f.b.sessions) != 1 {
		t.Fatal("unloaded nonpersistent pending RPC")
	}
}

type slowIdleClose struct {
	*fakeRPC
	started, release chan struct{}
}

func (r *slowIdleClose) Close() { close(r.started); <-r.release; r.fakeRPC.Close() }

func TestIdleShutdownDoesNotBlockOtherSessionsOrLoseLease(t *testing.T) {
	f := persistentFixture(t, t.TempDir())
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	made := 0
	f.b.factory = func(context.Context, []string, string, time.Duration) (backend, error) {
		made++
		rpc := newFake()
		if made == 1 {
			return &slowIdleClose{rpc, started, release}, nil
		}
		return rpc, nil
	}
	body := testRequest(false)
	body["metadata"] = M{"user_id": "slow-idle-close"}
	_, response := f.post(t, body)
	id := f.b.sessions[0].threadID
	expireFixtureConversation(f)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("executor close not started")
	}
	resumed := make(chan int, 1)
	go func() { s, _ := f.post(t, continueRequest(body, response, "Resume during shutdown")); resumed <- s }()
	other := testRequest(false)
	other["metadata"] = M{"user_id": "independent-host"}
	independent := make(chan int, 1)
	go func() { s, _ := f.post(t, other); independent <- s }()
	select {
	case s := <-independent:
		if s != 200 {
			t.Fatal(s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow executor shutdown blocked unrelated session")
	}
	select {
	case s := <-resumed:
		t.Fatal("resumed before old lease released", s)
	default:
	}
	once.Do(func() { close(release) })
	select {
	case s := <-resumed:
		if s != 200 {
			t.Fatal("resume failed while old executor retired", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resume did not finish")
	}
	found := false
	f.b.mu.Lock()
	for _, c := range f.b.sessions {
		if c.threadID == id {
			found = true
		}
	}
	f.b.mu.Unlock()
	if !found {
		t.Fatal("native thread replaced")
	}
}

func TestIdleTimeoutOptions(t *testing.T) {
	for _, seconds := range []int{0, 1, 120} {
		args := []string{"--idle-timeout", strconv.Itoa(seconds)}
		o, err := parseOptions(args)
		if err != nil || o.idleTimeout != seconds {
			t.Fatal(args, o, err)
		}
	}
	if _, err := parseOptions([]string{"--idle-timeout=-1"}); err == nil {
		t.Fatal("negative timeout accepted")
	}
	o, err := parseOptions(nil)
	if err != nil || o.idleTimeout != 120 {
		t.Fatal("unexpected default idle timeout", o, err)
	}
}

func TestIdleTimeoutDisabledKeepsPersistentExecutor(t *testing.T) {
	f := persistentFixture(t, t.TempDir())
	body := testRequest(false)
	body["metadata"] = M{"user_id": "idle-disabled"}
	f.post(t, body)
	f.b.idleTimeout = 0
	expireFixtureConversation(f)
	if len(f.b.sessions) != 1 {
		t.Fatal("disabled idle timeout unloaded executor")
	}
}

func TestIdleReapingProtectsInFlightFlag(t *testing.T) {
	f := persistentFixture(t, t.TempDir())
	body := testRequest(false)
	body["metadata"] = M{"user_id": "idle-inflight"}
	f.post(t, body)
	f.b.sessions[0].inFlight = true
	expireFixtureConversation(f)
	if len(f.b.sessions) != 1 {
		t.Fatal("unloaded an unfinished native turn")
	}
}

func TestIdleReapingKeepsExecutorIfCheckpointCannotBeSaved(t *testing.T) {
	f := persistentFixture(t, t.TempDir())
	body := testRequest(false)
	body["metadata"] = M{"user_id": "idle-checkpoint-failure"}
	f.post(t, body)
	c := f.b.sessions[0]
	c.statePath = t.TempDir() // a directory cannot be replaced by the checkpoint file
	expireFixtureConversation(f)
	if len(f.b.sessions) != 1 {
		t.Fatal("unloaded the only recoverable copy after checkpoint failure")
	}
}

func TestIdleAgentBranchesReleaseAllExecutorsAndKeepOwnIDs(t *testing.T) {
	f := persistentFixture(t, t.TempDir())
	type branch struct {
		body, response M
		id             string
	}
	branches := []branch{}
	for i := 0; i < 12; i++ {
		body := testRequest(false)
		body["metadata"] = M{"user_id": string(encode(M{"session_id": "many-idle-branches", "agent_id": strconv.Itoa(i)}))}
		status, response := f.post(t, body)
		if status != 200 {
			t.Fatal(status, response)
		}
		branches = append(branches, branch{body, response, f.b.sessions[len(f.b.sessions)-1].threadID})
	}
	f.b.mu.Lock()
	for _, c := range f.b.sessions {
		c.lastUsed = time.Now().Add(-time.Hour)
	}
	f.b.reapLocked(time.Now().Add(-2 * time.Minute))
	remaining := len(f.b.sessions)
	f.b.mu.Unlock()
	if remaining != 0 {
		t.Fatal("retained idle subagent executors", remaining)
	}
	for i, b := range branches {
		status, result := f.post(t, continueRequest(b.body, b.response, "Continue branch "+strconv.Itoa(i)))
		if status != 200 {
			t.Fatal(status, result)
		}
		calls, _ := f.rpc().snapshot()
		if calls[0].Method != "thread/resume" || calls[0].Params["threadId"] != b.id {
			t.Fatal("mixed or replaced subagent identity", i, calls)
		}
	}
}

// Codex skips unchanged additional context only within one app-server process.
// A resumed executor must not re-append the host instructions and tool catalog
// the thread already holds (~120k tokens per idle unload in real sessions).
func TestResumedExecutorSendsOnlyChangedHostContext(t *testing.T) {
	f := persistentFixture(t, t.TempDir())
	body := testRequest(false)
	body["system"] = hostPrompt("gitStatus: clean")
	body["metadata"] = M{"user_id": "idle-host-context"}
	status, response := f.post(t, body)
	if status != 200 {
		t.Fatal(status, response)
	}
	first, _ := f.rpc().snapshot()
	full := len(object(first[len(first)-1].Params["additionalContext"]))
	id := f.b.sessions[0].threadID
	expireFixtureConversation(f)
	next := continueRequest(body, response, "Continue after idle unload")
	next["system"] = hostPrompt("gitStatus: modified a.go")
	status, response = f.post(t, next)
	if status != 200 {
		t.Fatal(status, response)
	}
	calls, _ := f.rpc().snapshot()
	if calls[0].Method != "thread/resume" {
		t.Fatal("did not resume", calls[0].Method)
	}
	start := calls[len(calls)-1]
	sent := len(object(start.Params["additionalContext"]))
	if start.Method != "turn/start" || full < 20 || sent > 8 {
		t.Fatalf("resumed executor sent %d of %d host context keys", sent, full)
	}
	if !strings.Contains(contextText(threadContext(id), "system"), "gitStatus: modified a.go") {
		t.Fatal("thread lost the updated host instructions")
	}
}

func TestResumedExecutorReportsTheKnownContextSize(t *testing.T) {
	saved := &sessionCheckpoint{Version: 1, ThreadID: "thread-known", Expected: []string{}, ContextTokens: 91050}
	c, err := openConversation(context.Background(), newFake(), &prepared{Model: "gpt-test-sol", Persistent: true}, t.TempDir(), "key", saved)
	if err != nil {
		t.Fatal(err)
	}
	// Before Codex measures anything, the first tool_use must not report only
	// what was appended since the resume.
	c.addUnmeasured(200)
	c.sampleEnded()
	if got := integer(c.reportedUsage()["input_tokens"]); got != 91250 {
		t.Fatal("resumed executor forgot the native context size", got)
	}
	c.sampleMeasured(M{"input_tokens": 500, "output_tokens": 10, "cache_read_input_tokens": 91000})
	if got := c.contextTokens(); got != 91510 {
		t.Fatal("measurement must replace the saved estimate", got)
	}
}
