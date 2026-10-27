package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestConfigurationUpdatesReuseOneNativeThread(t *testing.T) {
	f := newHTTPFixture(t)
	f.b.maxSessions = 1
	body := testRequest(true)
	body["metadata"] = M{"user_id": "session-main"}
	for i := 0; i < 40; i++ {
		status, response := f.post(t, body)
		if status != 200 || response["stop_reason"] != "tool_use" {
			t.Fatal(i, status, response)
		}
		tool := object(array(response["content"])[0])
		body["messages"] = append(array(body["messages"]), M{"role": "assistant", "content": response["content"]}, M{"role": "user", "content": []any{M{"type": "tool_result", "tool_use_id": tool["id"], "content": "done"}}}, M{"role": "system", "content": fmt.Sprintf("New host rule %d", i)})
		// Change the tool catalog while a tool is pending. Its reply must still go
		// to the original native invocation; the next turn sees the new catalog.
		object(array(body["tools"])[0])["description"] = fmt.Sprintf("Updated tool description %d", i)
		status, final := f.post(t, body)
		if status != 200 || final["stop_reason"] != "end_turn" {
			t.Fatal(status, final)
		}
		calls, _ := f.rpc().snapshot()
		last := calls[len(calls)-1]
		if last.Method != "turn/steer" || !strings.Contains(contextText(threadContext(str(last.Params["threadId"])), "system"), fmt.Sprintf("New host rule %d", i)) {
			t.Fatal("Lost current instructions", last.Method)
		}
		body["messages"] = append(array(body["messages"]), M{"role": "assistant", "content": final["content"]}, M{"role": "user", "content": "Call the tool again"})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.backends) != 1 {
		t.Fatal("Created a new native conversation", len(f.backends))
	}
	calls, _ := f.backends[0].snapshot()
	starts := 0
	for _, call := range calls {
		if call.Method == "thread/start" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatal(starts)
	}
}

func TestReplacementProtectsOtherHostsAndUnansweredBranches(t *testing.T) {
	for _, differentHost := range []bool{false, true} {
		f := newHTTPFixture(t)
		body := testRequest(true)
		body["metadata"] = M{"user_id": "session-one"}
		_, response := f.post(t, body)
		rpc := f.rpc()
		tool := object(array(response["content"])[0])
		body["messages"] = append(array(body["messages"]), M{"role": "assistant", "content": response["content"]})
		if differentHost {
			body["metadata"] = M{"user_id": "session-two"}
			body["messages"] = append(array(body["messages"]), M{"role": "user", "content": []any{M{"type": "tool_result", "tool_use_id": tool["id"], "content": "done"}}})
		} else {
			body["messages"] = append(array(body["messages"]), M{"role": "user", "content": "Independent branch; no result for the pending tool."})
		}
		body["system"] = "Different host instructions"
		status, response := f.post(t, body)
		if status != 200 {
			t.Fatal(status, response)
		}
		rpc.mu.Lock()
		closed := rpc.closed
		rpc.mu.Unlock()
		if closed {
			t.Fatal("Closed another host or unanswered branch")
		}
	}
}

func TestHostContextIsLosslessAndChunkedBelowNativeLimit(t *testing.T) {
	body := testRequest(true)
	body["system"] = strings.Repeat("🚀regla-日本語-", 1200) + "END_SYSTEM"
	object(array(body["tools"])[0])["description"] = strings.Repeat("schema description ", 2000) + "END_TOOL"
	p, err := prepare(body, "gpt-test-sol", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := currentHostContext(p)
	if contextText(ctx, "system") != p.System {
		t.Fatal("Truncated host instructions")
	}
	if !strings.Contains(contextText(ctx, "tools"), "END_TOOL") {
		t.Fatal("Truncated tool schema")
	}
	for key, v := range ctx {
		if len(str(object(v)["value"])) > 900 {
			t.Fatal("Context value exceeds safe native size", key)
		}
	}
}

func TestModelChangesKeepTheNativeThread(t *testing.T) {
	f := newHTTPFixture(t)
	body := testRequest(false)
	body["metadata"] = M{"user_id": "main-model-change"}
	_, response := f.post(t, body)
	body["model"] = "gpt-test-luna"
	body["messages"] = append(array(body["messages"]), M{"role": "assistant", "content": response["content"]}, M{"role": "user", "content": "Continue on the new model"})
	status, response := f.post(t, body)
	if status != 200 || response["model"] != "gpt-test-luna" {
		t.Fatal(status, response)
	}
	f.mu.Lock()
	count := len(f.backends)
	f.mu.Unlock()
	calls, _ := f.rpc().snapshot()
	if count != 1 || calls[len(calls)-1].Params["model"] != "gpt-test-luna" {
		t.Fatal("Rebuilt instead of switching model")
	}
}

func TestIdentifiedChatsKeepTheirThreadAcrossIdleTime(t *testing.T) {
	for _, identified := range []bool{false, true} {
		f := newHTTPFixture(t)
		body := testRequest(false)
		if identified {
			body["metadata"] = M{"user_id": "main-idle"}
		}
		status, response := f.post(t, body)
		if status != 200 {
			t.Fatal(status, response)
		}
		f.b.mu.Lock()
		f.b.sessions[0].lastUsed = time.Now().Add(-time.Hour)
		f.b.reapLocked(time.Now().Add(-30 * time.Minute))
		count := len(f.b.sessions)
		f.b.mu.Unlock()
		if identified && count != 1 {
			t.Fatal("Idle timer discarded identified chat")
		}
		if !identified && count != 0 {
			t.Fatal("Anonymous conversation did not expire")
		}
	}
}

func TestCompletedToolResponsesAreNotReplayedAsPending(t *testing.T) {
	f := newHTTPFixture(t)
	body := testRequest(true)
	_, first := f.post(t, body)
	tool := object(array(first["content"])[0])
	continuation := copyMap(body)
	continuation["messages"] = append(append([]any{}, array(body["messages"])...), M{"role": "assistant", "content": first["content"]}, M{"role": "user", "content": []any{M{"type": "tool_result", "tool_use_id": tool["id"], "content": "done"}}})
	status, final := f.post(t, continuation)
	if status != 200 || final["stop_reason"] != "end_turn" {
		t.Fatal(status, final)
	}
	status, next := f.post(t, body)
	if status != 200 || next["stop_reason"] != "tool_use" {
		t.Fatal(status, next)
	}
	if object(array(next["content"])[0])["id"] == tool["id"] {
		t.Fatal("Replayed an already-completed tool invocation")
	}
}
