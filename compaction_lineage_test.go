package main

import (
	"os"
	"path/filepath"
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
				if c.threadID == wanted {
					owner = c
				}
			}
			if owner == nil || owner.compactHash == "" {
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
	compact := compactRequest(body)
	compact["tools"] = []any{}
	compact["messages"] = append(array(compact["messages"]), M{"role": "assistant", "content": response["content"]}, M{"role": "user", "content": "Continue after the compacted summary"})
	status, result := f.post(t, compact)
	if status != 200 {
		t.Fatal(status, result)
	}
	if f.b.sessions[1].threadID != wanted || f.b.sessions[1].compactHash == "" || len(f.b.sessions[0].pending) != 1 {
		t.Fatal("Retained message selected the wrong branch")
	}
}

func TestCompactionStillRejectsUnprovenBranch(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	f, body, _, _ := branchingCompactionFixture(t)
	status, result := f.post(t, compactRequest(body))
	if status != 409 {
		t.Fatal("Guessed a branch with no lineage evidence", status, result)
	}
	if len(f.b.sessions) != 2 || len(f.b.sessions[0].pending) != 1 || len(f.b.sessions[1].pending) != 1 {
		t.Fatal("Ambiguous compaction changed existing contexts")
	}
	dir := f.b.stateDir
	f.b.close()
	reopened := persistentFixture(t, dir)
	status, result = reopened.post(t, compactRequest(body))
	if status != 409 {
		t.Fatal("Restart guessed the primary without compaction evidence", status, result)
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
