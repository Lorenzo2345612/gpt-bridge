package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type missingRolloutRPC struct {
	*fakeRPC
	message string
}

func (r *missingRolloutRPC) Call(ctx context.Context, method string, p M) (M, error) {
	if method == "thread/resume" {
		return nil, upstream("Codex thread/resume: %s", r.message)
	}
	return r.fakeRPC.Call(ctx, method, p)
}

func TestMissingRolloutRecoveryOnlyForUncommittedExactHistory(t *testing.T) {
	for _, kind := range []string{"uncommitted", "completed", "pending", "different-request", "other-error"} {
		t.Run(kind, func(t *testing.T) {
			f := persistentFixture(t, t.TempDir())
			body := testRequest(false)
			body["metadata"] = M{"user_id": "missing-rollout-test"}
			p, err := prepare(body, f.b.model, f.b.aliases)
			if err != nil {
				t.Fatal(err)
			}
			p.NamedSession = true
			dir := filepath.Join(f.b.stateDir, p.HostIdentity)
			if err = os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "orphan.json")
			s := sessionCheckpoint{Version: 1, ThreadID: "missing-thread", HostIdentity: p.HostIdentity, Key: "test", InFlight: true, RequestHashes: p.Hashes, Primary: true, UpdatedAt: time.Now()}
			switch kind {
			case "completed":
				s.Expected = p.Hashes
			case "pending":
				s.Pending = []savedPending{{PublicID: "tool1", Name: "Bash"}}
			case "different-request":
				s.RequestHashes = []string{"unrelated"}
			}
			if err = os.WriteFile(path, encode(s), 0600); err != nil {
				t.Fatal(err)
			}
			message := "no rollout found for thread id missing-thread"
			if kind == "other-error" {
				message = "connection failed"
			}
			created := 0
			f.b.factory = func(context.Context, []string, string, time.Duration) (backend, error) {
				created++
				if created == 1 {
					return &missingRolloutRPC{newFake(), message}, nil
				}
				return newFake(), nil
			}
			c, resumed, err := f.b.openPersistent(context.Background(), p, "test")
			if kind != "uncommitted" {
				if err == nil || created != 1 {
					t.Fatalf("unsafe recovery: %v, %d", err, created)
				}
				if _, err = os.Stat(path); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer c.close()
			if resumed || !c.primary || created != 2 || c.threadID == s.ThreadID {
				t.Fatal("invalid replacement")
			}
			if _, err = os.Stat(path + ".missing-rollout"); err != nil {
				t.Fatal(err)
			}
			if err = c.advance(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			calls, _ := c.rpc.(*fakeRPC).snapshot()
			if !strings.Contains(string(encode(calls[len(calls)-1].Params["input"])), "Use the skill") {
				t.Fatal("host history lost")
			}
		})
	}
}
