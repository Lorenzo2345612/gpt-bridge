package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func branchingCompactionFixture(t *testing.T) (*httpFixture, M, M, string) {
	t.Helper()
	f := persistentFixture(t, t.TempDir())
	body := testRequest(true)
	body["metadata"] = M{"user_id": `{"session_id":"lineage-test"}`}
	status, first := f.post(t, body)
	if status != 200 {
		t.Fatal(status, first)
	}
	branch := copyMap(body)
	branch["messages"] = append(append([]any{}, array(body["messages"])...), M{"role": "assistant", "content": first["content"]}, M{"role": "user", "content": "Background branch with its own unfinished tool"}, M{"role": "system", "content": "Updated host instruction for a parallel branch"})
	status, response := f.post(t, branch)
	if status != 200 {
		t.Fatal(status, response)
	}
	if len(f.b.sessions) != 2 || f.b.sessions[0].baseSystemHash != f.b.sessions[1].baseSystemHash {
		t.Fatal("Fixture did not create ambiguous branches")
	}
	return f, body, response, f.b.sessions[1].threadID
}

func writeCompactionJournal(t *testing.T, body, response M) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	p, err := prepare(body, "gpt-test-sol", nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "projects", "test-project", p.ClaudeSessionID+".jsonl")
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	rows := []M{
		{"type": "assistant", "sessionId": p.ClaudeSessionID, "message": M{"role": "assistant", "content": response["content"]}},
		{"type": "system", "subtype": "compact_boundary", "sessionId": p.ClaudeSessionID, "logicalParentUuid": "attachment-never-persisted"},
		{"type": "user", "isCompactSummary": true, "sessionId": p.ClaudeSessionID, "message": object(array(body["messages"])[0])},
	}
	var raw []byte
	for _, row := range rows {
		raw = append(raw, encode(row)...)
		raw = append(raw, '\n')
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCompactionUsesJournalLineageWithMultipleBranches(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "restart"}[restart], func(t *testing.T) {
			f, body, response, wanted := branchingCompactionFixture(t)
			// Host compaction continues the owner in a fresh native thread; its
			// checkpoint (and lease) identify the conversation.
			wantedPath := f.b.sessions[1].statePath
			compact := compactRequest(body)
			compact["tools"] = []any{}
			writeCompactionJournal(t, compact, response)
			if restart {
				dir := f.b.stateDir
				f.b.close()
				f = persistentFixture(t, dir)
			}
			status, result := f.post(t, compact)
			if status != 200 {
				t.Fatal("Compaction could not identify its actual owner", status, result)
			}
			var owner *conversation
			for _, c := range f.b.sessions {
				if c.statePath == wantedPath {
					owner = c
				}
			}
			if owner == nil || owner.compactHash == "" || owner.threadID == wanted {
				t.Fatal("Selected the primary or created a new native thread instead of the journal owner")
			}
			if !restart && (len(f.b.sessions[0].pending) != 1 || len(f.b.sessions) != 2) {
				t.Fatal("Disturbed a separate pending branch")
			}
			if restart {
				calls, _ := f.rpc().snapshot()
				if calls[0].Method != "thread/resume" || calls[0].Params["threadId"] != wanted {
					t.Fatal("Restart lost the non-primary thread", calls)
				}
			}
		})
	}
}

func TestCompactionUsesUniqueRetainedMessage(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	f, body, response, wanted := branchingCompactionFixture(t)
	wantedPath := f.b.sessions[1].statePath
	compact := compactRequest(body)
	compact["tools"] = []any{}
	compact["messages"] = append(array(compact["messages"]), M{"role": "assistant", "content": response["content"]}, M{"role": "user", "content": "Continue after the compacted summary"})
	status, result := f.post(t, compact)
	if status != 200 {
		t.Fatal(status, result)
	}
	if f.b.sessions[1].statePath != wantedPath || f.b.sessions[1].threadID == wanted || f.b.sessions[1].compactHash == "" || len(f.b.sessions[0].pending) != 1 {
		t.Fatal("Retained message selected the wrong branch")
	}
}

// An ambiguous compacted view must not take over a branch. Recover in a fresh
// thread and retain all old checkpoints, including pending tool invocations.
func TestAmbiguousCompactionRecoversWithoutChangingBranches(t *testing.T) {
	for _, mode := range []string{"live", "restart", "no-compatible-checkpoint"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			f, body, _, _ := branchingCompactionFixture(t)
			dir := f.b.stateDir
			original := map[string][]byte{}
			ids := map[string]bool{}
			for _, c := range f.b.sessions {
				raw, err := os.ReadFile(c.statePath)
				if err != nil {
					t.Fatal(err)
				}
				original[c.statePath] = raw
				ids[c.threadID] = true
			}
			if mode != "live" {
				f.b.close()
				f = persistentFixture(t, dir)
			}
			compact := compactRequest(body)
			compact["tools"] = []any{}
			if mode == "no-compatible-checkpoint" {
				compact["system"] = "Changed host instructions after compaction"
			}
			status, result := f.post(t, compact)
			if status != 200 || result["stop_reason"] != "end_turn" {
				t.Fatal("ambiguous compaction did not recover", status, result)
			}
			fresh := f.b.sessions[len(f.b.sessions)-1]
			if ids[fresh.threadID] || original[fresh.statePath] != nil {
				t.Fatal("recovery took over an ambiguous branch")
			}
			for path, before := range original {
				after, err := os.ReadFile(path)
				if err != nil || string(after) != string(before) {
					t.Fatal("recovery changed an existing checkpoint", path, err)
				}
			}
			calls, _ := f.rpc().snapshot()
			for _, call := range calls {
				if call.Method == "thread/resume" || call.Method == "turn/interrupt" {
					t.Fatal("recovery touched an ambiguous thread", call.Method)
				}
			}
			last := calls[len(calls)-1]
			if last.Method != "turn/start" || !strings.Contains(string(encode(last.Params["input"])), "complete current view") {
				t.Fatal("fresh thread did not receive the compacted host view")
			}
			if mode == "live" && (len(f.b.sessions) != 3 || len(f.b.sessions[0].pending) != 1 || len(f.b.sessions[1].pending) != 1) {
				t.Fatal("recovery disturbed pending branches")
			}
			count := len(f.backends)
			if retryStatus, retry := f.post(t, compact); retryStatus != 200 || string(encode(retry["content"])) != string(encode(result["content"])) || len(f.backends) != count {
				t.Fatal("identical retry created another thread", retryStatus, retry)
			}
			id := fresh.threadID
			// A continuation after executor unload must select the recovered
			// checkpoint, not create another recovery thread.
			f.b.close()
			reopened := persistentFixture(t, dir)
			status, result = reopened.post(t, continueRequest(compact, result, "Continue recovered conversation"))
			if status != 200 || reopened.b.sessions[0].threadID != id {
				t.Fatal("recovery did not survive restart", status, result)
			}
		})
	}
}

// Older bridges rejected compaction without updating version-1 checkpoints.
// The same host session must recover after upgrading, even with failed retries
// in its journal; proven lineage is reused, otherwise old branches stay intact.
func TestPreviouslyRejectedCompactionRecoversLegacySession(t *testing.T) {
	for _, proven := range []bool{false, true} {
		t.Run(map[bool]string{false: "ambiguous", true: "journal-owner"}[proven], func(t *testing.T) {
			f, body, response, _ := branchingCompactionFixture(t)
			dir := f.b.stateDir
			ownerPath := f.b.sessions[1].statePath
			f.b.close()
			original := map[string][]byte{}
			states, err := filepath.Glob(filepath.Join(dir, "*", "*.json"))
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range states {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var state M
				if err = decode(raw, &state); err != nil {
					t.Fatal(err)
				}
				delete(state, "context_tokens")
				raw = encode(state)
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				original[path] = raw
			}
			if len(original) != 2 {
				t.Fatal("missing legacy checkpoints")
			}
			compact := compactRequest(body)
			compact["tools"] = []any{}
			if !proven {
				response = M{"content": "Unmatched older response"}
			}
			writeCompactionJournal(t, compact, response)
			p, err := prepare(compact, "gpt-test-sol", nil)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(claudeConfig(), "projects", "test-project", p.ClaudeSessionID+".jsonl")
			journal, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			for retry := 0; retry < 3; retry++ {
				row := M{"type": "assistant", "isApiErrorMessage": true, "message": M{"role": "assistant", "model": "<synthetic>", "content": []any{M{"type": "text", "text": "409 Cannot identify the compacted conversation unambiguously; saved threads were preserved"}}}}
				if _, err = journal.Write(append(encode(row), '\n')); err != nil {
					journal.Close()
					t.Fatal(err)
				}
			}
			journal.Close()
			f = persistentFixture(t, dir)
			status, result := f.post(t, compact)
			if status != 200 || result["stop_reason"] != "end_turn" {
				t.Fatal("previously rejected session did not recover", status, result)
			}
			recovered := f.b.sessions[0]
			if (recovered.statePath == ownerPath) != proven {
				t.Fatal("recovery did not respect available lineage")
			}
			for path, before := range original {
				if proven && path == ownerPath {
					continue
				}
				after, err := os.ReadFile(path)
				if err != nil || string(after) != string(before) {
					t.Fatal("recovery changed an unrelated legacy checkpoint", path, err)
				}
			}
			id := recovered.threadID
			f.b.close()
			f = persistentFixture(t, dir)
			status, result = f.post(t, continueRequest(compact, result, "Continue previously rejected session"))
			if status != 200 || f.b.sessions[0].threadID != id {
				t.Fatal("legacy recovery did not survive restart", status, result)
			}
		})
	}
}

func TestCompactionAnchorsDoNotBreakSharedHistoryTie(t *testing.T) {
	candidates := []compactionCandidate{{"a", []string{"old-a", "shared"}}, {"b", []string{"shared"}}}
	if id, matched := compactionAnchorOwner([]string{"old-a", "shared"}, candidates); !matched || id != "" {
		t.Fatal("Used an older ancestor to guess ownership", id, matched)
	}
}

func TestJournalCompactionIgnoresSyntheticOwnershipAnchors(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		sharedReal  bool
		want        string
	}{
		{"synthetic-placeholder", "<synthetic>", false, "a"},
		{"real-shared-output", "gpt-test-sol", false, ""},
		{"synthetic-does-not-resolve-real-tie", "<synthetic>", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actual := M{"role": "assistant", "content": []any{M{"type": "text", "text": "Completed branch-specific work"}}}
			placeholder := M{"role": "assistant", "model": tc.model, "content": []any{M{"type": "text", "text": "No response requested."}}}
			canonical, _ := canonicalMessages([]any{actual, placeholder})
			actualHash, placeholderHash := hash(canonical[0]), hash(canonical[1])
			candidates := []compactionCandidate{{"a", []string{actualHash, placeholderHash}}, {"b", []string{placeholderHash}}}
			if tc.sharedReal {
				candidates[1].Hashes = append(candidates[1].Hashes, actualHash)
			}
			summary := M{"role": "user", "content": "This session is being continued from a previous conversation that ran out of context. Summary: completed work."}
			p := &prepared{ClaudeSessionID: "synthetic-lineage", CompactHash: compactionHash([]any{summary})}
			rows := []M{
				{"type": "assistant", "message": actual},
				{"type": "assistant", "message": placeholder},
				{"type": "system", "subtype": "compact_boundary"},
				{"type": "user", "isCompactSummary": true, "message": summary},
			}
			path := filepath.Join(t.TempDir(), "journal.jsonl")
			var data []byte
			for _, r := range rows {
				data = append(data, encode(r)...)
				data = append(data, '\n')
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			id, matched := journalCompactionOwner(path, p, candidates)
			if !matched || id != tc.want {
				t.Fatalf("owner=%q matched=%v, want %q", id, matched, tc.want)
			}
		})
	}
}

func TestJournalLineageSurvivesProviderSwitchAndRepeatedCompaction(t *testing.T) {
	real := M{"role": "assistant", "content": []any{M{"type": "text", "text": "Unique native branch reply"}}}
	canonical, _ := canonicalMessages([]any{real})
	candidates := []compactionCandidate{{"actual-thread", []string{hash(canonical[0])}}, {"older-thread", []string{"older-hash"}}}
	rows := []M{{"type": "assistant", "message": real}}
	var latest M
	for boundary := 0; boundary < 3; boundary++ {
		for i := 0; i < 90; i++ {
			rows = append(rows, M{"type": "assistant", "message": M{"role": "assistant", "model": "other-provider", "content": []any{M{"type": "text", "text": "Unmatched provider output"}}}})
		}
		latest = M{"role": "user", "content": "This session is being continued from a previous conversation. Summary: " + string(rune('a'+boundary))}
		rows = append(rows, M{"type": "system", "subtype": "compact_boundary"}, M{"type": "user", "isCompactSummary": true, "message": latest})
	}
	path := filepath.Join(t.TempDir(), "session.jsonl")
	var raw []byte
	for _, row := range rows {
		raw = append(raw, encode(row)...)
		raw = append(raw, '\n')
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	p := &prepared{CompactHash: compactionHash([]any{latest})}
	id, matched := journalCompactionOwner(path, p, candidates)
	if !matched || id != "actual-thread" {
		t.Fatal("lost proven lineage across compactions", id, matched)
	}
}

func TestCompactionSummaryWhitespaceKeepsJournalIdentity(t *testing.T) {
	summary := M{"role": "user", "content": "This session is being continued from a previous conversation. Summary: retained work."}
	request := M{"role": "user", "content": str(summary["content"]) + "\n"}
	if compactionHash([]any{summary}) != compactionHash([]any{request}) {
		t.Fatal("trailing newline changed compaction identity")
	}
	canonical, _ := canonicalMessages([]any{M{"role": "assistant", "content": "Unique response"}})
	path := filepath.Join(t.TempDir(), "session.jsonl")
	rows := []M{{"type": "assistant", "message": canonical[0]}, {"type": "system", "subtype": "compact_boundary"}, {"type": "user", "isCompactSummary": true, "message": summary}}
	var raw []byte
	for _, r := range rows {
		raw = append(raw, encode(r)...)
		raw = append(raw, '\n')
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	p := &prepared{CompactHash: compactionHash([]any{request})}
	id, matched := journalCompactionOwner(path, p, []compactionCandidate{{"correct-thread", []string{hash(canonical[0])}}})
	if !matched || id != "correct-thread" {
		t.Fatal("request newline lost journal owner", id, matched)
	}
}
