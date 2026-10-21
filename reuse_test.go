package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestConfigurationUpdatesRetireAnsweredPendingTurns(t *testing.T) {
	f := newHTTPFixture(t)
	f.b.maxSessions = 1
	body := testRequest(true)
	body["metadata"] = M{"user_id": "session-main"}
	for i := 0; i < 40; i++ {
		status, response := f.post(t, body)
		if status != 200 || response["stop_reason"] != "tool_use" {
			t.Fatal(i, status, response)
		}
		rpc := f.rpc()
		f.b.mu.Lock()
		live := len(f.b.sessions)
		f.b.mu.Unlock()
		if live != 1 {
			t.Fatal("Leaked replaced conversation", live)
		}
		tool := object(array(response["content"])[0])
		body["messages"] = append(array(body["messages"]), M{"role": "assistant", "content": response["content"]}, M{"role": "user", "content": []any{M{"type": "tool_result", "tool_use_id": tool["id"], "content": "done"}}}, M{"role": "system", "content": fmt.Sprintf("New host rule %d", i)})
		// The replacement must contain every new instruction, not just abandon
		// the configuration change to keep an old native thread alive.
		if i > 0 {
			calls, _ := rpc.snapshot()
			if !strings.Contains(str(calls[0].Params["baseInstructions"]), fmt.Sprintf("New host rule %d", i-1)) {
				t.Fatal("Lost current instructions")
			}
		}
		if i > 0 {
			f.mu.Lock()
			previous := f.backends[i-1]
			f.mu.Unlock()
			previous.mu.Lock()
			closed := previous.closed
			previous.mu.Unlock()
			if !closed {
				t.Fatal("Old native process still waiting")
			}
		}
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
