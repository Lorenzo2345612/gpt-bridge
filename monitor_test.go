package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

const monitorTestThread = "01a160ab-3c9e-72a2-a2c8-8fc1e793a03e"
const monitorTestProcess = "pid:123:instance-one"

func monitorTestSpan(model string) string {
	return fmt.Sprintf("session_loop{thread_id=%s}:turn{otel.name=\"session_task.turn\" thread.id=%s turn.id=01a160ab-3cf0-7221-acfc-7965da2e12cd model=%s codex.turn.reasoning_effort=high}:session_task.run:run_turn: ", monitorTestThread, monitorTestThread, model)
}

func TestMonitorParsesStructuredNativeEvidenceOnly(t *testing.T) {
	body := monitorTestSpan("gpt-6-astra") + "post sampling token usage total_usage_tokens=27546"
	v := parseMonitorLog("/codex", 100, body, monitorTestProcess)
	if v == nil || v.Model != "gpt-6-astra" || v.PID != 123 || v.ContextTokens != 27546 || v.Evidence != "sample" || v.Effort != "high" {
		t.Fatalf("wrong evidence: %+v", v)
	}
	for _, fake := range []string{"app-server event: thread/tokenUsage/updated", `<- response: DynamicToolCall {text: "` + body + `"}`, "app_server.request{startup_prewarm model=gpt-6-astra}"} {
		if parseMonitorLog("", 100, fake, monitorTestProcess) != nil {
			t.Fatalf("accepted quoted content or warmup: %s", fake)
		}
	}
}

func TestMonitorSQLiteIncrementalModelChanges(t *testing.T) {
	home := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(home, "logs_2.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE logs(id INTEGER PRIMARY KEY,ts INTEGER,target TEXT,feedback_log_body TEXT,process_uuid TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(id int, target, body string) {
		t.Helper()
		_, e := db.Exec("INSERT INTO logs VALUES(?,?,?,?,?)", id, 100+id, target, body, monitorTestProcess)
		if e != nil {
			t.Fatal(e)
		}
	}
	insert(1, "codex_core::client", `app_server.request{app_server.client_name="claude_codex_bridge" app_server.client_version="0.5.0"}`)
	insert(2, "codex_core::session::turn", monitorTestSpan("gpt-5.6-terra")+"post sampling token usage total_usage_tokens=100")
	r, err := openCodexLog(home)
	if err != nil {
		t.Fatal(err)
	}
	defer r.db.Close()
	events := []monitorEvent{}
	add := func(e monitorEvent) { events = append(events, e) }
	if err = r.update(context.Background(), add); err != nil {
		t.Fatal(err)
	}
	key := monitorTestProcess + ":" + monitorTestThread
	if len(events) != 1 || r.threads[key].SamplingEvents != 1 || r.versions[monitorTestProcess] != "0.5.0" {
		t.Fatalf("wrong initial import: %+v", r)
	}
	if err = r.update(context.Background(), add); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || r.threads[key].SamplingEvents != 1 {
		t.Fatal("poll counted old samples again")
	}
	insert(3, "feedback_tags", monitorTestSpan("gpt-6-astra")+"model=\"gpt-6-astra\"")
	if err = r.update(context.Background(), add); err != nil {
		t.Fatal(err)
	}
	v := r.threads[key]
	if v.Model != "gpt-6-astra" || v.ModelChanges != 1 || v.ContextTokens != 0 || v.Evidence != "request" {
		t.Fatalf("stale context attributed to new model: %+v", v)
	}
	insert(4, "codex_core::session::turn", monitorTestSpan("gpt-6-astra")+"post sampling token usage total_usage_tokens=700")
	if err = r.update(context.Background(), add); err != nil {
		t.Fatal(err)
	}
	if v.ContextTokens != 700 || v.SamplingEvents != 2 || v.Evidence != "sample" || len(events) != 2 {
		t.Fatalf("wrong model transition: %+v", v)
	}
	if _, err = r.db.Exec("DELETE FROM logs"); err == nil {
		t.Fatal("Codex source was opened writable")
	}
}

func TestMonitorParentAssociationAndCycles(t *testing.T) {
	all := map[int]monitorProcess{1: {PID: 1, Bridge: true}, 2: {PID: 2, ParentPID: 1}, 3: {PID: 3, ParentPID: 2}, 4: {PID: 4, ParentPID: 5}, 5: {PID: 5, ParentPID: 4}}
	if bridgeAncestor(3, all) != 1 || bridgeAncestor(4, all) != 0 || bridgeAncestor(999, all) != 0 {
		t.Fatal("invalid ancestry")
	}
}

func TestMonitorHTTPReadOnlyAndSameOrigin(t *testing.T) {
	m := &bridgeMonitor{snapshot: []byte(`{"processes":[],"threads":[]}`)}
	for _, tc := range []struct {
		method, host, origin, path string
		status                     int
	}{
		{"GET", "127.0.0.1:8787", "", "/api/monitor", 200},
		{"GET", "127.0.0.1:8787", "http://127.0.0.1:8787", "/", 200},
		{"POST", "127.0.0.1:8787", "", "/api/monitor", 405},
		{"GET", "evil.example:8787", "", "/api/monitor", 403},
		{"GET", "127.0.0.1:8787", "https://evil.example", "/api/monitor", 403},
		{"GET", "127.0.0.1:8787", "", "/missing", 404},
	} {
		r := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		m.handler().ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%+v got %d", tc, w.Code)
		}
		if tc.status == 200 && !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Fatal("missing frame protection")
		}
	}
}
