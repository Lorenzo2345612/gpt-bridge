package main

import (
	"context"
	"strings"
	"testing"
)

type compactFailureRPC struct{ *fakeRPC }

func (r *compactFailureRPC) Call(ctx context.Context, method string, p M) (M, error) {
	if method == "thread/compact/start" {
		r.events <- M{"method": "turn/completed", "params": M{"threadId": p["threadId"], "turn": M{"status": "failed", "error": M{"message": "test compaction failure"}}}}
		return M{}, nil
	}
	return r.fakeRPC.Call(ctx, method, p)
}

func TestNativeCompactionFailureDoesNotStartInference(t *testing.T) {
	f := newFake()
	c := &conversation{rpc: &compactFailureRPC{f}, threadID: "original", resetHistory: true, compactHash: "old"}
	err := c.advance(context.Background(), &prepared{CompactHash: "new"})
	if err == nil || !strings.Contains(err.Error(), "compaction failed") {
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

func TestHostCompactionRunsNativeCompactionOnceBeforeInference(t *testing.T) {
	f := persistentFixture(t, t.TempDir())
	body := testRequest(false)
	body["metadata"] = M{"user_id": "native-compact-once"}
	_, _ = f.post(t, body)
	id := f.b.sessions[0].threadID
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
	count := 0
	for i, call := range calls {
		if call.Method == "thread/compact/start" {
			count++
			if call.Params["threadId"] != id || i+1 >= len(calls) || calls[i+1].Method != "turn/start" {
				t.Fatal("compaction order or identity", calls)
			}
		}
	}
	if count != 1 || f.b.sessions[0].threadID != id {
		t.Fatal("compaction repeated or replaced thread", calls)
	}
}
