package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func persistentFixture(t *testing.T, dir string) *httpFixture {
	f := newHTTPFixture(t)
	f.b.stateDir = dir
	return f
}
func continueRequest(body M, response M, text string) M {
	next := copyMap(body)
	next["messages"] = append(append([]any{}, array(body["messages"])...), M{"role": "assistant", "content": response["content"]}, M{"role": "user", "content": text})
	return next
}
func compactRequest(body M) M {
	next := copyMap(body)
	next["messages"] = []any{M{"role": "user", "content": "This session is being continued from a previous conversation that ran out of context. Summary: previous work completed. Continue without repeating tools."}}
	return next
}
func TestPersistentRestartAndCompactionKeepThread(t *testing.T) {
	dir := t.TempDir()
	f := persistentFixture(t, dir)
	body := testRequest(false)
	body["metadata"] = M{"user_id": "persistent-main"}
	status, response := f.post(t, body)
	if status != 200 {
		t.Fatal(status, response)
	}
	f.b.mu.Lock()
	id := f.b.sessions[0].threadID
	f.b.mu.Unlock()
	calls, _ := f.rpc().snapshot()
	if calls[0].Params["ephemeral"] != false {
		t.Fatal("Thread is not persistent")
	}
	f.b.close()
	second := persistentFixture(t, dir)
	body = continueRequest(body, response, "Continue")
	status, response = second.post(t, body)
	if status != 200 {
		t.Fatal(status, response)
	}
	calls, _ = second.rpc().snapshot()
	if calls[0].Method != "thread/resume" || calls[0].Params["threadId"] != id {
		t.Fatal("Created a replacement thread", calls)
	}
	body = compactRequest(body)
	status, response = second.post(t, body)
	if status != 200 {
		t.Fatal(status, response)
	}
	second.b.mu.Lock()
	if len(second.b.sessions) != 1 || second.b.sessions[0].threadID != id {
		t.Fatal("Compaction replaced the thread")
	}
	second.b.mu.Unlock()
	calls, _ = second.rpc().snapshot()
	if len(calls) != 3 || !strings.Contains(string(encode(calls[2].Params["input"])), "SAME conversation") {
		t.Fatal(calls)
	}
	second.b.close()
	third := persistentFixture(t, dir)
	body = continueRequest(body, response, "Continue after another restart")
	status, response = third.post(t, body)
	if status != 200 {
		t.Fatal(status, response)
	}
	calls, _ = third.rpc().snapshot()
	if calls[0].Method != "thread/resume" || calls[0].Params["threadId"] != id {
		t.Fatal(calls)
	}
}
func TestPersistentPendingReplyAndExclusiveLease(t *testing.T) {
	dir := t.TempDir()
	f := persistentFixture(t, dir)
	body := testRequest(true)
	body["metadata"] = M{"user_id": "persistent-tool"}
	_, response := f.post(t, body)
	tool := object(array(response["content"])[0])
	next := copyMap(body)
	next["messages"] = append(append([]any{}, array(body["messages"])...), M{"role": "assistant", "content": response["content"]}, M{"role": "user", "content": []any{M{"type": "tool_result", "tool_use_id": tool["id"], "content": "already done"}}})
	other := persistentFixture(t, dir)
	status, result := other.post(t, next)
	if status != 409 {
		t.Fatal(status, result)
	}
	f.b.mu.Lock()
	id := f.b.sessions[0].threadID
	f.b.mu.Unlock()
	f.b.close()
	// No tools are available on the resumed turn; it must not replay Skill.
	next["tools"] = []any{}
	status, result = other.post(t, next)
	if status != 200 || result["stop_reason"] != "end_turn" {
		t.Fatal(status, result)
	}
	calls, _ := other.rpc().snapshot()
	if calls[0].Method != "thread/resume" || calls[0].Params["threadId"] != id {
		t.Fatal(calls)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*.json"))
	if len(files) != 1 {
		t.Fatal(files)
	}
	raw, _ := os.ReadFile(files[0])
	if strings.Contains(string(raw), "already done") || strings.Contains(string(raw), "Use my Skill") {
		t.Fatal("Stored prompt or result text")
	}
	if info, _ := os.Stat(files[0]); runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		t.Fatal("Checkpoint permissions are not private")
	}
}
func TestCompactionInterruptsPendingNativeTurnWithoutNewThread(t *testing.T) {
	f := persistentFixture(t, t.TempDir())
	body := testRequest(true)
	body["metadata"] = M{"user_id": "compact-pending"}
	_, response := f.post(t, body)
	if response["stop_reason"] != "tool_use" {
		t.Fatal(response)
	}
	body = compactRequest(body)
	body["tools"] = []any{}
	status, response := f.post(t, body)
	if status != 200 || response["stop_reason"] != "end_turn" {
		t.Fatal(status, response)
	}
	calls, _ := f.rpc().snapshot()
	count := 0
	interrupted := false
	for _, call := range calls {
		if call.Method == "thread/start" {
			count++
		}
		if call.Method == "turn/interrupt" {
			interrupted = true
		}
	}
	if count != 1 || !interrupted {
		t.Fatal(calls)
	}
}
func TestCheckpointCorruptionFailsWithoutReplacingConversation(t *testing.T) {
	dir := t.TempDir()
	f := persistentFixture(t, dir)
	body := testRequest(false)
	body["metadata"] = M{"user_id": "corrupt-test"}
	_, response := f.post(t, body)
	f.b.close()
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*.json"))
	os.WriteFile(files[0], []byte("broken"), 0600)
	other := persistentFixture(t, dir)
	status, _ := other.post(t, continueRequest(body, response, "Continue"))
	if status != 500 {
		t.Fatal(status)
	}
	other.mu.Lock()
	count := len(other.backends)
	other.mu.Unlock()
	if count != 0 {
		t.Fatal("Created a new thread after corrupt checkpoint")
	}
}

func TestStableClaudeIdentityIgnoresMetadataFormatting(t *testing.T) {
	first := hostIdentity(`{"device_id":"one","account_uuid":"account","session_id":"session","tk":"first"}`)
	next := hostIdentity(`{"tk":"changed","session_id":"session","account_uuid":"account","device_id":"two"}`)
	if first != next {
		t.Fatal("Restart metadata changed the session identity")
	}
	if first == hostIdentity(`{"session_id":"different"}`) || first == hostIdentity(`{"session_id":"session","agent_id":"child"}`) {
		t.Fatal("Mixed independent sessions or agents")
	}
}

func TestDeadBackendIsResumedWithoutReplacingItsThread(t *testing.T) {
	f := persistentFixture(t, t.TempDir())
	body := testRequest(false)
	body["metadata"] = M{"user_id": "backend-crash"}
	_, response := f.post(t, body)
	f.b.mu.Lock()
	id := f.b.sessions[0].threadID
	f.b.mu.Unlock()
	f.rpc().Close()
	status, result := f.post(t, continueRequest(body, response, "Continue after the native backend died"))
	if status != 200 {
		t.Fatal(status, result)
	}
	calls, _ := f.rpc().snapshot()
	if calls[0].Method != "thread/resume" || calls[0].Params["threadId"] != id {
		t.Fatal("Replaced the crashed native thread", calls)
	}
}

func TestSummaryHelperNeverReplacesOrPersistsAsMainConversation(t *testing.T) {
	dir := t.TempDir()
	f := persistentFixture(t, dir)
	body := testRequest(false)
	body["metadata"] = M{"user_id": "summary-helper-test"}
	_, response := f.post(t, body)
	f.b.mu.Lock()
	id := f.b.sessions[0].threadID
	f.b.mu.Unlock()
	helper := continueRequest(body, response, "CRITICAL: Respond with TEXT ONLY. Your task is to create a detailed summary of the conversation so far. Return a <summary> block.")
	status, result := f.post(t, helper)
	if status != 200 {
		t.Fatal(status, result)
	}
	f.b.mu.Lock()
	count := len(f.b.sessions)
	remaining := f.b.sessions[0].threadID
	f.b.mu.Unlock()
	if count != 1 || remaining != id {
		t.Fatal("Helper changed the main thread")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*.json"))
	if len(files) != 1 {
		t.Fatal("Saved ephemeral helper as a conversation", files)
	}
	body = compactRequest(body)
	status, result = f.post(t, body)
	if status != 200 {
		t.Fatal(status, result)
	}
	f.b.mu.Lock()
	same := f.b.sessions[0].threadID == id
	f.b.mu.Unlock()
	if !same {
		t.Fatal("Compaction replaced main")
	}
}
func TestClaudeSessionUUIDRestoresPrimaryAfterReplayChanges(t *testing.T) {
	dir := t.TempDir()
	f := persistentFixture(t, dir)
	body := testRequest(false)
	body["metadata"] = M{"user_id": `{"session_id":"stable-session","device_id":"old","tk":"old"}`}
	status, result := f.post(t, body)
	if status != 200 {
		t.Fatal(status, result)
	}
	f.b.mu.Lock()
	id := f.b.sessions[0].threadID
	f.b.mu.Unlock()
	f.b.close()
	next := persistentFixture(t, dir)
	body["metadata"] = M{"user_id": `{"tk":"new","device_id":"changed","session_id":"stable-session"}`}
	body["messages"] = []any{M{"role": "user", "content": "Reopened the same Claude session with updated host wrappers; continue."}}
	status, result = next.post(t, body)
	if status != 200 {
		t.Fatal(status, result)
	}
	calls, _ := next.rpc().snapshot()
	if calls[0].Method != "thread/resume" || calls[0].Params["threadId"] != id {
		t.Fatal("Lost UUID association", calls)
	}
}
