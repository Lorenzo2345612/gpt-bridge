package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type compactFailureRPC struct{ *fakeRPC }

func (r *compactFailureRPC) Call(ctx context.Context, method string, p M) (M, error) {
	if method == "thread/start" {
		return nil, upstream("test thread start failure")
	}
	return r.fakeRPC.Call(ctx, method, p)
}

func TestNativeCompactionFailureDoesNotStartInference(t *testing.T) {
	f := newFake()
	c := &conversation{rpc: &compactFailureRPC{f}, threadID: "original", resetHistory: true, compactHash: "old"}
	err := c.advance(context.Background(), &prepared{CompactHash: "new"})
	if err == nil || !strings.Contains(err.Error(), "thread start failure") {
		t.Fatal(err)
	}
	calls, _ := f.snapshot()
	for _, call := range calls {
		if call.Method == "turn/start" {
			t.Fatal("started inference after failed compaction")
		}
	}
	if c.threadID != "original" || c.compactHash != "old" {
		t.Fatal("failed compaction changed identity or checkpoint")
	}
}

// Codex compaction keeps every earlier user message (each Claude turn), so host
// compaction continues in a fresh native thread carrying Claude's summary only.
func TestHostCompactionContinuesInFreshNativeThreadOnce(t *testing.T) {
	f := persistentFixture(t, t.TempDir())
	body := testRequest(false)
	body["metadata"] = M{"user_id": "native-compact-once"}
	_, _ = f.post(t, body)
	id, path := f.b.sessions[0].threadID, f.b.sessions[0].statePath
	body = compactRequest(body)
	status, response := f.post(t, body)
	if status != 200 {
		t.Fatal(status, response)
	}
	body = continueRequest(body, response, "Continue after compact")
	status, response = f.post(t, body)
	if status != 200 {
		t.Fatal(status, response)
	}
	calls, _ := f.rpc().snapshot()
	starts := 0
	for i, call := range calls {
		if call.Method == "thread/compact/start" {
			t.Fatal("compacted the old native thread instead of replacing it")
		}
		if call.Method == "thread/start" && i > 0 {
			starts++
			if i+1 >= len(calls) || calls[i+1].Method != "turn/start" || calls[i+1].Params["threadId"] == id || !strings.Contains(string(encode(calls[i+1].Params["input"])), "SAME conversation") {
				t.Fatal("fresh thread order or identity", calls)
			}
			if call.Params["ephemeral"] != false {
				t.Fatal("fresh thread of a persistent conversation is ephemeral")
			}
		}
	}
	c := f.b.sessions[0]
	if starts != 1 || c.threadID == id || c.statePath != path {
		t.Fatal("compaction repeated or lost its checkpoint", calls)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), c.threadID) {
		t.Fatal("checkpoint does not point at the fresh thread", err)
	}
}

func TestAwayRecapIsAnEphemeralSideRequest(t *testing.T) {
	f := persistentFixture(t, t.TempDir())
	body := testRequest(false)
	body["metadata"] = M{"user_id": "away-recap"}
	status, response := f.post(t, body)
	if status != 200 {
		t.Fatal(status, response)
	}
	main := f.b.sessions[0].threadID
	for i := 0; i < 2; i++ {
		recap := continueRequest(body, response, fmt.Sprintf("The user stepped away and is coming back. Recap in under 40 words, 1-2 plain sentences, no markdown. (%d)", i))
		if status, result := f.post(t, recap); status != 200 {
			t.Fatal(status, result)
		}
	}
	files, _ := filepath.Glob(filepath.Join(f.b.stateDir, "*", "*.json"))
	if len(files) != 1 {
		t.Fatal("persisted a recap as a conversation branch", files)
	}
	for _, c := range f.b.sessions {
		if !c.auxiliary && c.threadID != main {
			t.Fatal("recap opened or replaced a main conversation thread")
		}
	}
	calls := 0
	for _, rpc := range f.backends {
		snapshot, _ := rpc.snapshot()
		for _, call := range snapshot {
			if call.Method == "thread/resume" {
				t.Fatal("recap resumed a persisted thread")
			}
			if call.Method == "thread/start" && call.Params["ephemeral"] == true {
				calls++
			}
		}
	}
	if calls != 2 {
		t.Fatal("each recap must run in its own ephemeral thread", calls)
	}
}
