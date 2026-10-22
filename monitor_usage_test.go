package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func usageLine(at string, total, last monitorTokens) []byte {
	v := M{"type": "event_msg", "timestamp": at, "payload": M{"type": "token_count", "info": M{"total_token_usage": total, "last_token_usage": last}}}
	raw, _ := json.Marshal(v)
	return append(raw, '\n')
}
func contextLine(model string) []byte {
	raw, _ := json.Marshal(M{"type": "turn_context", "payload": M{"model": model}})
	return append(raw, '\n')
}

func TestRolloutIncrementalUsageDeduplicatesAndAttributesModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	a := monitorTokens{Input: 100, Cached: 60, Output: 20, Reasoning: 5}
	b := monitorTokens{Input: 250, Cached: 160, Output: 50, Reasoning: 15}
	raw := append(contextLine("gpt-6.1-sol"), usageLine("2026-10-21T12:00:00Z", a, a)...)
	raw = append(raw, usageLine("2026-10-21T12:00:00Z", a, a)...)
	raw = append(raw, contextLine("gpt-6-astra")...)
	second := usageLine("2026-10-22T12:00:00Z", b, monitorTokens{Input: 150, Cached: 100, Output: 30, Reasoning: 10})
	raw = append(raw, second[:len(second)-1]...)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	r := &monitorRollout{path: path}
	if err := r.update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.total.Input != 100 || r.total.Calls != 1 || !r.backlog {
		t.Fatalf("Duplicate or partial append counted: %+v", r.total)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("\n"))
	file.Close()
	if err := r.update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.total.Input != 250 || r.total.Cached != 160 || r.total.Output != 50 || r.total.Calls != 2 {
		t.Fatalf("Wrong cumulative delta: %+v", r.total)
	}
	if r.models["gpt-6-astra"].Input != 150 || r.models["gpt-6.1-sol"].Input != 100 {
		t.Fatal("Usage attributed to the latest model instead of event model", r.models)
	}
	day := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC).In(time.Local).Format("2006-01-02")
	if u := r.snapshot(day); u.Today.Input != 150 || u.Today.Output != 30 || u.Today.Calls != 1 {
		t.Fatalf("Wrong daily usage: %+v", u)
	}
	if err := r.update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.total.Calls != 2 {
		t.Fatal("Polling counted an old event twice")
	}
}

func TestRolloutCounterResetAndFileTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	a := monitorTokens{Input: 100, Cached: 60, Output: 20}
	b := monitorTokens{Input: 10, Cached: 5, Output: 2}
	raw := append(contextLine("gpt-6-astra"), usageLine("2026-10-22T12:00:00Z", a, a)...)
	raw = append(raw, usageLine("2026-10-22T12:01:00Z", b, b)...)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	r := &monitorRollout{path: path}
	if err := r.update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.resets != 1 || r.total.Input != 110 || r.total.Output != 22 {
		t.Fatalf("Wrong reset recovery: %+v", r)
	}
	// Replacement/truncation must rebuild the file counters, not append them.
	raw = append(contextLine("gpt-6-luna"), usageLine("2026-10-22T12:02:00Z", b, b)...)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.total.Input != 10 || r.models["gpt-6-astra"].Input != 0 {
		t.Fatal("File truncation duplicated old usage")
	}
}

func TestMonitorUsageCountsResumedThreadOnlyOnce(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "rollout.jsonl")
	a := monitorTokens{Input: 100, Cached: 60, Output: 20, Reasoning: 5}
	raw := append(contextLine("gpt-6.1-sol"), usageLine(time.Now().UTC().Format(time.RFC3339), a, a)...)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("CREATE TABLE threads(id TEXT,rollout_path TEXT)")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("INSERT INTO threads VALUES(?,?)", monitorTestThread, path)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	s := monitorSnapshot{Threads: []monitorThread{{ID: monitorTestThread, PID: 1, CodexHome: home}, {ID: monitorTestThread, PID: 2, CodexHome: home}}}
	m := bridgeMonitor{}
	m.updateUsage(context.Background(), &s, map[int]*monitorRoot{})
	if s.Usage.Threads != 1 || s.Usage.Covered != 1 || s.Usage.Total.Input != 100 || s.Usage.Total.Output != 20 {
		t.Fatalf("Resumed thread counted twice: %+v", s.Usage)
	}
	if s.Threads[0].Usage == nil || s.Threads[1].Usage == nil {
		t.Fatal("Missing per-thread usage")
	}
}
