package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLineageRows(t *testing.T, path string, rows []M) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	for _, row := range rows {
		raw = append(raw, encode(row)...)
		raw = append(raw, '\n')
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitLineageBoundaryAncestry(t *testing.T) {
	summary := M{"role": "user", "content": "This session is being continued from a previous conversation. Summary: work."}
	p := &prepared{ClaudeSessionID: "session", CompactHash: compactionHash([]any{summary})}
	for _, mode := range []string{"notification", "missing-parent", "cycle", "duplicate-uuid", "conflicting-id", "unrecorded-response", "synthetic", "other-session", "sidechain"} {
		t.Run(mode, func(t *testing.T) {
			owners := map[string]map[string]bool{"msg_known": {"owner": true}}
			response := M{"type": "assistant", "uuid": "reply", "sessionId": "session", "message": M{"role": "assistant", "model": "gpt-test", "id": "msg_known", "content": "Re-rendered text"}}
			link := M{"type": "user", "uuid": "notification", "parentUuid": "reply"}
			rows := []M{response, link}
			parent := "notification"
			want, matched := "owner", true
			switch mode {
			case "missing-parent":
				parent, want, matched = "missing", "", false
			case "cycle":
				link["parentUuid"] = "notification"
				want, matched = "", false
			case "duplicate-uuid":
				rows = append(rows, M{"type": "user", "uuid": "reply"})
				want = ""
			case "conflicting-id":
				owners["msg_known"]["another-owner"] = true
				want = ""
			case "unrecorded-response":
				link["type"] = "assistant"
				link["message"] = M{"role": "assistant", "model": "gpt-test", "id": "msg_unknown"}
				want, matched = "", false
			case "synthetic":
				object(response["message"])["model"] = "<synthetic>"
				want, matched = "", false
			case "other-session":
				response["sessionId"] = "another-session"
				want, matched = "", false
			case "sidechain":
				response["isSidechain"] = true
				want, matched = "", false
			}
			rows = append(rows, M{"type": "system", "subtype": "compact_boundary", "logicalParentUuid": parent}, M{"type": "user", "isCompactSummary": true, "message": summary})
			path := filepath.Join(t.TempDir(), "journal.jsonl")
			writeLineageRows(t, path, rows)
			if id, ok := journalExplicitCompactionOwner(path, p, owners); id != want || ok != matched {
				t.Fatalf("owner=%q matched=%v, want %q %v", id, ok, want, matched)
			}
		})
	}
}

func TestExplicitLineageSurvivesRestartAndRepeatedCompaction(t *testing.T) {
	for _, agent := range []bool{false, true} {
		t.Run(map[bool]string{false: "main", true: "subagent"}[agent], func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			f := persistentFixture(t, t.TempDir())
			body := testRequest(true)
			identity := `{"session_id":"lineage-test"}`
			if agent {
				identity = `{"session_id":"lineage-test","agent_id":"worker"}`
			}
			body["metadata"] = M{"user_id": identity}
			status, first := f.post(t, body)
			if status != 200 {
				t.Fatal(status, first)
			}
			branch := copyMap(body)
			branch["messages"] = append(append([]any{}, array(body["messages"])...), M{"role": "assistant", "content": first["content"]}, M{"role": "user", "content": "Parallel branch"}, M{"role": "system", "content": "Updated branch instructions"})
			status, response := f.post(t, branch)
			if status != 200 || len(f.b.sessions) != 2 {
				t.Fatal("missing parallel branch", status, response)
			}
			owner := f.b.sessions[1]
			ownerPath, dir := owner.statePath, f.b.stateDir
			if len(owner.lineage) != 1 || owner.lineage[0].MessageID != response["id"] {
				t.Fatal("response identity was not committed")
			}
			for generation := 0; generation < 2; generation++ {
				compact := compactRequest(body)
				compact["tools"] = []any{}
				object(array(compact["messages"])[0])["content"] = "This session is being continued from a previous conversation. Summary: generation " + string(rune('a'+generation))
				p, err := prepare(compact, "gpt-test-sol", nil)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(claudeConfig(), "projects", "test", p.ClaudeSessionID+".jsonl")
				if agent {
					path = filepath.Join(claudeConfig(), "projects", "test", p.ClaudeSessionID, "subagents", "agent-"+p.ClaudeAgentID+".jsonl")
				}
				writeLineageRows(t, path, []M{
					{"type": "assistant", "uuid": "reply", "sessionId": p.ClaudeSessionID, "message": M{"id": response["id"], "role": "assistant", "model": "gpt-test-sol", "content": "Changed wrapper, no matching content hash"}},
					{"type": "user", "uuid": "notification", "parentUuid": "reply"},
					{"type": "system", "subtype": "compact_boundary", "logicalParentUuid": "notification"},
					{"type": "user", "isCompactSummary": true, "message": object(array(compact["messages"])[0])},
				})
				f.b.close()
				f = persistentFixture(t, dir)
				status, result := f.post(t, compact)
				if status != 200 || f.b.sessions[0].statePath != ownerPath {
					t.Fatal("explicit lineage did not select the checkpoint", status, result)
				}
				owner = f.b.sessions[0]
				if len(owner.lineage) != generation+2 || owner.lineage[len(owner.lineage)-1].ThreadID != owner.threadID {
					t.Fatal("lost response identities across native generations")
				}
				// Retry keeps the public ID and does not append another record.
				status, retry := f.post(t, compact)
				if status != 200 || retry["id"] != result["id"] || len(owner.lineage) != generation+2 {
					t.Fatal("retry changed response identity or lineage")
				}
				response = result
			}
		})
	}
}

func TestFailedLineageCommitLeavesCheckpointUnchanged(t *testing.T) {
	f, _, _, _ := branchingCompactionFixture(t)
	c := f.b.sessions[1]
	path := c.statePath
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err = os.WriteFile(blocked, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	c.statePath = filepath.Join(blocked, "checkpoint.json")
	err = f.b.commit(c, &prepared{}, "failed-request", M{"id": "msg_not_committed", "content": []any{}})
	if err == nil {
		t.Fatal("expected atomic checkpoint write to fail")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || string(after) != string(before) || strings.Contains(string(after), "msg_not_committed") {
		t.Fatal("failed response acquired durable lineage", readErr)
	}
	f.b.discard(c)
}

func TestLineageCheckpointContainsOnlyResponseIdentities(t *testing.T) {
	f, _, response, _ := branchingCompactionFixture(t)
	for _, c := range f.b.sessions {
		raw, err := os.ReadFile(c.statePath)
		if err != nil {
			t.Fatal(err)
		}
		var saved sessionCheckpoint
		if err = decode(raw, &saved); err != nil || len(saved.Lineage) != 1 {
			t.Fatal("missing committed lineage", err)
		}
		if strings.Contains(string(raw), "unfinished tool") {
			t.Fatal("checkpoint stored prompt text")
		}
	}
	// The actual response ID is retained, not a content-derived surrogate.
	last := f.b.sessions[1].lineage[0]
	if last.MessageID != response["id"] {
		t.Fatal(last)
	}
}
