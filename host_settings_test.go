package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHostHooksAndProviderModelPickerShareOneSettingsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host settings.json")
	if e := os.WriteFile(path, []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"cw _hook"}]}]},"env":{"HOST_SETTING":"yes","ANTHROPIC_MODEL":"opus"},"model":"opus"}`), 0600); e != nil {
		t.Fatal(e)
	}
	provider := M{"model": "gpt-test-sol", "modelPicker": M{"replaceBuiltInOptions": true}, "env": M{"ANTHROPIC_MODEL": "gpt-test-sol"}}
	settings, args, e := mergedHostSettings([]string{"--resume", "session", "--settings", path, `--settings={"hooks":{"Stop":[{"hooks":[{"command":"cw _hook"}]}]}}`}, provider)
	if e != nil {
		t.Fatal(e)
	}
	if len(args) != 2 || args[0] != "--resume" || args[1] != "session" {
		t.Fatal("Forwarded duplicate settings flags", args)
	}
	if settings["model"] != "gpt-test-sol" || settings["modelPicker"] == nil {
		t.Fatal("Lost bridge models", settings)
	}
	if object(settings["env"])["ANTHROPIC_MODEL"] != "gpt-test-sol" || object(settings["env"])["HOST_SETTING"] != "yes" {
		t.Fatal("Lost provider or host env")
	}
	hooks := object(settings["hooks"])
	if len(array(hooks["SessionStart"])) != 1 || len(array(hooks["Stop"])) != 1 {
		t.Fatal("Lost workspace hooks", hooks)
	}
}
func TestHostSettingsInvalidInputsFailAndArgumentsStayLiteral(t *testing.T) {
	for _, args := range [][]string{{"--settings"}, {"--settings="}, {"--settings", "{invalid}"}, {"--settings", "/does/not/exist"}} {
		if _, _, e := mergedHostSettings(args, M{}); e == nil {
			t.Fatal("Accepted invalid host settings", args)
		}
	}
	_, args, e := mergedHostSettings([]string{"--", "--settings", "literal prompt"}, M{})
	if e != nil || len(args) != 3 {
		t.Fatal("Parsed prompt text as settings", e, args)
	}
}
