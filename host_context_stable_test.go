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

// ToolSearch answers with tool_reference blocks. Rejecting them failed the
// request, discarded the live executor and re-sent the whole host context to a
// resumed thread.
func TestToolSearchReferencesAreValidToolResults(t *testing.T) {
	items, err := toolResultContent([]any{M{"type": "tool_reference", "tool_name": "mcp__portal__list"}})
	if err != nil || len(items) != 1 || !strings.Contains(str(object(items[0])["text"]), "mcp__portal__list") {
		t.Fatal(items, err)
	}
	if _, err := toolResultContent([]any{M{"type": "tool_reference"}}); err == nil {
		t.Fatal("accepted a reference without a name")
	}
	history, err := transcriptInput([]any{M{"role": "user", "content": []any{M{"type": "tool_result", "tool_use_id": "toolu_1", "content": []any{M{"type": "tool_reference", "tool_name": "mcp__portal__list"}}}}}})
	if err != nil || !strings.Contains(str(object(history[0])["text"]), "mcp__portal__list") {
		t.Fatal(history, err)
	}
}

func TestToolSearchReferencesActivateDeferredTools(t *testing.T) {
	deferred := M{"name": "mcp__portal__list", "description": "List.", "input_schema": M{"type": "object"}, "defer_loading": true}
	other := M{"name": "mcp__portal__other", "description": "Other.", "input_schema": M{"type": "object"}, "defer_loading": true}
	messages := []any{
		M{"role": "user", "content": "load it"},
		M{"role": "assistant", "content": []any{M{"type": "tool_use", "id": "toolu_1", "name": "ToolSearch", "input": M{}}}},
		M{"role": "user", "content": []any{M{"type": "tool_result", "tool_use_id": "toolu_1", "content": []any{M{"type": "tool_reference", "tool_name": "mcp__portal__list"}}}}},
	}
	_, _, tools, err := hostContext(messages, []any{deferred, other})
	if err != nil || len(tools) != 1 || object(tools[0])["name"] != "mcp__portal__list" {
		t.Fatal(tools, err)
	}
}
