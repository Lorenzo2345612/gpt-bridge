package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDurableExactRetryAfterProcessRestart(t *testing.T) {
	for _, tools := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "pending-tool"}[tools], func(t *testing.T) {
			dir := t.TempDir()
			f := persistentFixture(t, dir)
			body := testRequest(tools)
			body["metadata"] = M{"user_id": `{"session_id":"durable-retry"}`}
			status, first := f.post(t, body)
			if status != 200 {
				t.Fatal(status, first)
			}
			f.b.close()
			f = persistentFixture(t, dir)
			status, retry := f.post(t, body)
			if status != 200 || string(encode(first)) != string(encode(retry)) || len(f.backends) != 0 {
				t.Fatal("retry did not replay the committed response without a backend", status, retry)
			}
			// A receipt remains replayable across more than one bridge restart.
			f.b.close()
			f = persistentFixture(t, dir)
			status, retry = f.post(t, body)
			if status != 200 || retry["id"] != first["id"] || len(f.backends) != 0 {
				t.Fatal("second restart lost the receipt")
			}
			files, err := filepath.Glob(filepath.Join(dir, "*", "replies", "*.json"))
			if err != nil || len(files) != 1 {
				t.Fatal("missing private response receipt", files, err)
			}
			if info, err := os.Stat(files[0]); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
				t.Fatal("response receipt permissions are not private", err)
			}
		})
	}
}

func TestAcquirePreservesInFlightRecoveryMarker(t *testing.T) {
	dir := t.TempDir()
	f := persistentFixture(t, dir)
	body := testRequest(false)
	body["metadata"] = M{"user_id": `{"session_id":"durable-inflight"}`}
	c, p, _, _, err := f.b.acquire(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	c.inFlight, c.requestHashes = true, p.Hashes
	if err = c.saveState(); err != nil {
		t.Fatal(err)
	}
	path := c.statePath
	c.mu.Unlock()
	f.b.close()
	f = persistentFixture(t, dir)
	c, _, _, _, err = f.b.acquire(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	defer c.mu.Unlock()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved sessionCheckpoint
	if err = decode(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.InFlight || len(saved.RequestHashes) != len(p.Hashes) {
		t.Fatal("acquire erased the durable in-flight marker before advancing")
	}
}

func TestDurableReceiptCorruptionFailsClosed(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			f := persistentFixture(t, dir)
			body := testRequest(false)
			body["metadata"] = M{"user_id": "receipt-integrity"}
			f.post(t, body)
			f.b.close()
			files, _ := filepath.Glob(filepath.Join(dir, "*", "replies", "*.json"))
			if len(files) != 1 {
				t.Fatal(files)
			}
			var err error
			if mode == "missing" {
				err = os.Remove(files[0])
			} else {
				err = os.WriteFile(files[0], []byte("{}"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			f = persistentFixture(t, dir)
			status, _ := f.post(t, body)
			if status != 500 || len(f.backends) != 0 {
				t.Fatal("receipt failure silently restarted inference", status)
			}
		})
	}
}

func TestUncommittedReceiptCannotAuthorizeReplay(t *testing.T) {
	f := persistentFixture(t, t.TempDir())
	body := testRequest(false)
	body["metadata"] = M{"user_id": "uncommitted-receipt"}
	c, p, key, _, err := f.b.acquire(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	// Store a valid response file without committing its reference.
	response := encode(M{"id": "msg_orphan", "stop_reason": "end_turn", "content": []any{}})
	dir := filepath.Join(filepath.Dir(c.statePath), "replies")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = writePrivateState(filepath.Join(dir, hash(string(response))+".json"), response); err != nil {
		t.Fatal(err)
	}
	c.mu.Unlock()
	if raw, err := f.b.replayResponse(p, key); err != nil || raw != nil {
		t.Fatal("uncommitted receipt was replayed", err)
	}
}

func TestAnsweredToolReceiptIsNotReplayed(t *testing.T) {
	f := persistentFixture(t, t.TempDir())
	body := testRequest(true)
	body["metadata"] = M{"user_id": "answered-receipt"}
	_, first := f.post(t, body)
	tool := object(array(first["content"])[0])
	next := continueRequest(body, first, "")
	messages := array(next["messages"])
	messages[len(messages)-1] = M{"role": "user", "content": []any{M{"type": "tool_result", "tool_use_id": tool["id"], "content": "done"}}}
	next["tools"] = []any{}
	if status, result := f.post(t, next); status != 200 || result["stop_reason"] != "end_turn" {
		t.Fatal(status, result)
	}
	for _, cached := range f.b.cache {
		if cached.pendingThread == "" {
			continue
		}
		p, err := prepare(body, f.b.model, f.b.aliases)
		if err != nil {
			t.Fatal(err)
		}
		if raw, err := f.b.replayResponse(p, cached.key); err != nil || raw != nil {
			t.Fatal("replayed a tool that already received its result", err)
		}
	}
}
