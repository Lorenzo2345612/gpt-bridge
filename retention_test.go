package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaudeRetentionUsesExistingSetting(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	cwd := t.TempDir()
	check := func(args []string, want int, valid bool) {
		t.Helper()
		got, err := claudeRetentionDays(cwd, args)
		if (err == nil) != valid || (valid && got != want) {
			t.Fatalf("days=%d err=%v, want %d valid=%v", got, err, want, valid)
		}
	}
	check(nil, 30, true)
	if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte(`{"cleanupPeriodDays":90}`), 0600); err != nil {
		t.Fatal(err)
	}
	check(nil, 90, true)
	if err := os.MkdirAll(filepath.Join(cwd, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".claude", "settings.json"), []byte(`{"cleanupPeriodDays":45}`), 0600); err != nil {
		t.Fatal(err)
	}
	check(nil, 45, true)
	check([]string{"--settings", `{"cleanupPeriodDays":7}`}, 7, true)
	check([]string{"--setting-sources", "user"}, 90, true)
	check([]string{"--settings", `{"cleanupPeriodDays":0}`}, 0, false)
	check([]string{"--settings", `{"cleanupPeriodDays":1.5}`}, 0, false)
	check([]string{"--settings", `{"cleanupPeriodDays":"30"}`}, 0, false)
}

func TestRetentionExpiresOnlySafeInactiveGroups(t *testing.T) {
	for _, mode := range []string{"expired", "recent", "pending", "inflight", "leased", "index-locked", "legacy", "longer-policy", "recent-fork", "host-active", "replayed", "invalid-checkpoint"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			identity := hostIdentity("retention-test")
			dir := filepath.Join(root, identity)
			replies := filepath.Join(dir, "replies")
			if err := os.MkdirAll(replies, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "branch.json")
			now := time.Now()
			state := sessionCheckpoint{Version: 1, ThreadID: "thread-old", HostIdentity: identity, RetentionDays: 30, UpdatedAt: now.Add(-31 * 24 * time.Hour)}
			switch mode {
			case "recent":
				state.UpdatedAt = now.Add(-29 * 24 * time.Hour)
			case "pending":
				state.Pending = []savedPending{{PublicID: "toolu_pending", Name: "Skill"}}
			case "inflight":
				state.InFlight = true
			case "legacy":
				state.RetentionDays = 0
			case "longer-policy":
				state.RetentionDays = 90
			case "host-active":
				state.ClaudeSessionID = "host-session"
				journal := filepath.Join(claudeConfig(), "projects", "project", "host-session.jsonl")
				writeLineageRows(t, journal, []M{{"type": "user"}})
			}
			if err := writePrivateState(path, encode(state)); err != nil {
				t.Fatal(err)
			}
			if mode == "recent-fork" {
				recent := state
				recent.ThreadID, recent.UpdatedAt = "thread-new", now
				if err := writePrivateState(filepath.Join(dir, "fork.json"), encode(recent)); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "replayed" {
				if err := touchResponseOwner(path); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "invalid-checkpoint" {
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			receipt := filepath.Join(replies, "receipt.json")
			if err := os.WriteFile(receipt, []byte("private response"), 0600); err != nil {
				t.Fatal(err)
			}
			if mode == "leased" || mode == "index-locked" {
				lock := path + ".lock"
				if mode == "index-locked" {
					lock = filepath.Join(dir, "index.lock")
				}
				lease, err := tryStateLock(lock)
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Close()
			}
			b := &Bridge{stateDir: root}
			err := b.cleanupExpiredSessions(context.Background(), now)
			if err != nil && mode != "invalid-checkpoint" {
				t.Fatal(err)
			}
			_, checkpointErr := os.Stat(path)
			_, receiptErr := os.Stat(receipt)
			if mode == "expired" {
				if !os.IsNotExist(checkpointErr) || !os.IsNotExist(receiptErr) {
					t.Fatal("expired checkpoint or receipt survived")
				}
				// A second sweep is harmless, including the retained lock files.
				if err := b.cleanupExpiredSessions(context.Background(), now); err != nil {
					t.Fatal(err)
				}
			} else if checkpointErr != nil || receiptErr != nil {
				t.Fatal("cleanup deleted protected state", checkpointErr, receiptErr)
			}
		})
	}
}

func TestRetentionFinishesInterruptedSweep(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, hostIdentity("interrupted-sweep"))
	replies := filepath.Join(dir, "replies")
	if err := os.MkdirAll(replies, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateState(filepath.Join(dir, ".expired"), encode(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(replies, "orphan.json"), []byte("response"), 0600); err != nil {
		t.Fatal(err)
	}
	b := &Bridge{stateDir: root}
	if err := b.cleanupExpiredSessions(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(replies); !os.IsNotExist(err) {
		t.Fatal("interrupted sweep left response payloads", err)
	}
}

func TestNewCheckpointAdoptsRetentionPolicy(t *testing.T) {
	f := persistentFixture(t, t.TempDir())
	f.b.retentionDays = 60
	body := testRequest(false)
	body["metadata"] = M{"user_id": `{"session_id":"retention-session"}`}
	if status, result := f.post(t, body); status != 200 {
		t.Fatal(status, result)
	}
	c := f.b.sessions[0]
	raw, err := os.ReadFile(c.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state sessionCheckpoint
	if err := decode(raw, &state); err != nil || state.RetentionDays != 60 || state.ClaudeSessionID != "retention-session" {
		t.Fatal("policy or host identity did not persist", state, err)
	}
}
