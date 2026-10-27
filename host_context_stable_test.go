package main

import (
	"fmt"
	"strings"
	"testing"
)

func hostPrompt(lead string) string {
	paragraphs := []string{lead}
	for i := 0; i < 300; i++ {
		paragraphs = append(paragraphs, fmt.Sprintf("Rule %d: keep the host instructions stable across turns so native context is not duplicated.", i))
	}
	return strings.Join(paragraphs, "\n\n")
}

func TestHostContextKeysSurviveEarlySystemEdits(t *testing.T) {
	body := testRequest(true)
	body["system"] = hostPrompt("gitStatus: clean")
	before, err := prepare(body, "gpt-test-sol", nil)
	if err != nil {
		t.Fatal(err)
	}
	body["system"] = hostPrompt("gitStatus: modified files a.go b.go c.go and an extra reminder line")
	after, err := prepare(body, "gpt-test-sol", nil)
	if err != nil {
		t.Fatal(err)
	}
	old, current := currentHostContext(before), currentHostContext(after)
	if contextText(current, "system") != after.System {
		t.Fatal("Lost host instructions")
	}
	total := len(array(contextManifest(current)["system"]))
	if changed := changedHostKeys(old, current); total < 20 || changed > 4 {
		t.Fatalf("An early edit re-sent %d of %d host chunks", changed, total)
	}
}

func TestHostContextToolEditOnlyChangesThatTool(t *testing.T) {
	body := testRequest(true)
	tools := []any{}
	for i := 0; i < 40; i++ {
		tools = append(tools, M{"name": fmt.Sprintf("Tool%d", i), "description": strings.Repeat("describe ", 120), "input_schema": M{"type": "object"}})
	}
	body["tools"] = tools
	before, err := prepare(body, "gpt-test-sol", nil)
	if err != nil {
		t.Fatal(err)
	}
	object(tools[0])["description"] = "changed"
	body["tools"] = append([]any{M{"name": "NewTool", "description": "new", "input_schema": M{"type": "object"}}}, tools...)
	after, err := prepare(body, "gpt-test-sol", nil)
	if err != nil {
		t.Fatal(err)
	}
	old, current := currentHostContext(before), currentHostContext(after)
	var catalog []any
	if decode([]byte(contextText(current, "tools")), &catalog) != nil || len(catalog) != 41 {
		t.Fatal("Lost tool catalog")
	}
	// Tool0 shrank to one chunk and NewTool appeared; nothing else is re-sent.
	if changed := changedHostKeys(old, current); changed != 2 {
		t.Fatalf("Tool edit re-sent %d chunks", changed)
	}
}

func TestHostContextRepeatedParagraphsStayLossless(t *testing.T) {
	body := testRequest(true)
	body["system"] = strings.Repeat("same paragraph\n\n", 200) + "tail"
	p, err := prepare(body, "gpt-test-sol", nil)
	if err != nil {
		t.Fatal(err)
	}
	if contextText(currentHostContext(p), "system") != p.System {
		t.Fatal("Repeated host chunks were not reconstructed")
	}
}
