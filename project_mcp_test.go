package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBridgeDisablesInheritedProjectMCPWithoutChangingFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	root := t.TempDir()
	cwd := filepath.Join(root, "nested", "work")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(root, ".codex", "config.toml"):           "[mcp_servers.plane]\ncommand='must-not-start'\n[mcp_servers.playwright]\ncommand='must-not-start'\n",
		filepath.Join(root, "nested", ".codex", "config.toml"): "[mcp_servers.\"portal.po\"]\ncommand='must-not-start'\n",
	}
	for p, v := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(v), 0600); err != nil {
			t.Fatal(err)
		}
	}
	args, err := codexCommandForCWD("codex", cwd)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\n")
	for _, name := range []string{"plane", "playwright", "portal.po"} {
		if !strings.Contains(joined, string(encode(name))+"={enabled=false,command=") {
			t.Fatalf("project MCP %s remains enabled", name)
		}
	}
	for p, want := range files {
		raw, err := os.ReadFile(p)
		if err != nil || string(raw) != want {
			t.Fatal("changed shared Codex configuration", p, err)
		}
	}
	if strings.Contains(joined, "code_mode_host=false") {
		t.Fatal("disabled dynamic tool dispatcher")
	}
}
