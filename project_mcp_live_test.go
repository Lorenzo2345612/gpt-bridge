package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLiveBridgeProjectMCPIsolation(t *testing.T) {
	cwd := os.Getenv("BRIDGE_MCP_TEST_CWD")
	if cwd == "" {
		t.Skip("opt-in real Codex app-server test")
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	command, err := codexCommandForCWD(binary, cwd)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	rpc, err := newRPC(ctx, command, cwd, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer rpc.Close()
	c, err := newConversation(ctx, rpc, &prepared{Model: "gpt-6.1-sol", Names: map[string]string{}, System: "Synthetic bridge MCP isolation check; do not execute tools."}, cwd, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	result, err := rpc.Call(ctx, "mcpServerStatus/list", M{"threadId": c.threadID, "limit": 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range array(result["data"]) {
		s := object(v)
		if len(object(s["tools"])) != 0 || len(array(s["resources"])) != 0 {
			t.Fatalf("native MCP server exposes tools/resources: %s", str(s["name"]))
		}
	}
	if runtime.GOOS == "linux" {
		pid := rpc.(*rpcClient).cmd.Process.Pid
		for i := 0; i < 3; i++ {
			time.Sleep(100 * time.Millisecond)
			todo := []string{fmt.Sprint(pid)}
			seen := map[string]bool{}
			for len(todo) > 0 {
				parent := todo[len(todo)-1]
				todo = todo[:len(todo)-1]
				if seen[parent] {
					continue
				}
				seen[parent] = true
				children, err := os.ReadFile("/proc/" + parent + "/task/" + parent + "/children")
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, child := range strings.Fields(string(children)) {
					exe, err := os.Readlink("/proc/" + child + "/exe")
					if os.IsNotExist(err) {
						continue
					}
					if err != nil {
						t.Fatal(err)
					}
					if filepath.Base(exe) != "codex-code-mode-host" && filepath.Base(exe) != "codex" {
						t.Fatalf("native app-server spawned unexpected process: %s", filepath.Base(exe))
					}
					todo = append(todo, child)
				}
			}
		}
	}
	t.Log("Real native thread started without MCP child processes or tools; project configuration preserved")
}
