package main

import (
	"context"
	"os"
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
