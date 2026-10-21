package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A protocol-speaking subprocess exercises the real stdio and process cleanup
// code without depending on an account or making any subscription requests.
func TestRPCProcessHelper(t *testing.T) {
	if os.Getenv("GO_BRIDGE_RPC_HELPER") != "1" {
		return
	}
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var msg M
		if decode(scan.Bytes(), &msg) != nil {
			os.Exit(2)
		}
		if msg["id"] == nil {
			continue
		}
		result := M{}
		var failure M
		switch str(msg["method"]) {
		case "initialize":
			if os.Getenv("OPENAI_API_KEY") != "" {
				os.Exit(3)
			}
		case "account/read":
			result = M{"account": M{"type": fallback(os.Getenv("GO_BRIDGE_RPC_AUTH"), "chatgpt")}}
		case "echo":
			result = object(msg["params"])
			result["own_process_group"] = helperProcessIsolated()
		case "event":
			fmt.Println(string(encode(M{"method": "test/event", "params": M{"value": "ok"}})))
		case "invalid":
			failure = M{"code": -32602, "message": "invalid params"}
		case "hang":
			continue
		}
		reply := M{"id": msg["id"], "result": result}
		if failure != nil {
			reply = M{"id": msg["id"], "error": failure}
		}
		fmt.Println(string(encode(reply)))
	}
	os.Exit(0)
}
func helperCommand(t *testing.T) []string {
	t.Helper()
	t.Setenv("GO_BRIDGE_RPC_HELPER", "1")
	binary, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	return []string{binary, "-test.run=TestRPCProcessHelper"}
}
func TestRPCSubprocessRoundTripCancellationAndCleanup(t *testing.T) {
	command := helperCommand(t)
	t.Setenv("OPENAI_API_KEY", "test-key-must-not-reach-subprocess")
	b, e := newRPC(context.Background(), command, t.TempDir(), time.Second)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	rpc := b.(*rpcClient)
	result, e := b.Call(context.Background(), "echo", M{"text": "hello"})
	if e != nil || result["text"] != "hello" || !boolean(result["own_process_group"]) {
		t.Fatal(result, e)
	}
	if _, e = b.Call(context.Background(), "invalid", M{}); e == nil || errorStatus(e) != 400 {
		t.Fatal(e)
	}
	if _, e = b.Call(context.Background(), "event", M{}); e != nil {
		t.Fatal(e)
	}
	select {
	case event := <-b.Events():
		if event["method"] != "test/event" {
			t.Fatal(event)
		}
	case <-time.After(time.Second):
		t.Fatal("Lost native event")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, e = b.Call(ctx, "hang", M{}); e == nil || errorStatus(e) != 504 {
		t.Fatal(e)
	}
	rpc.mu.Lock()
	count := len(rpc.replies)
	rpc.mu.Unlock()
	if count != 0 {
		t.Fatal("Leaked RPC waiter")
	}
	pid := rpc.cmd.Process.Pid
	b.Close()
	select {
	case <-rpc.waited:
	case <-time.After(time.Second):
		t.Fatal("Backend process not reaped")
	}
	if helperProcessExists(pid) {
		t.Fatal("Backend still alive", pid)
	}
}
func TestRPCRejectsAPIKeyAccount(t *testing.T) {
	command := helperCommand(t)
	t.Setenv("GO_BRIDGE_RPC_AUTH", "apiKey")
	_, e := newRPC(context.Background(), command, t.TempDir(), time.Second)
	if e == nil || !strings.Contains(e.Error(), "API-key auth refused") {
		t.Fatal(e)
	}
}
func TestCodexCommandParsesQuotedMCPNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	raw := "[mcp_servers.\"server.with spaces\"]\ncommand = \"ignored\"\n"
	if e := os.WriteFile(home+"/config.toml", []byte(raw), 0600); e != nil {
		t.Fatal(e)
	}
	args, e := codexCommand("codex")
	if e != nil {
		t.Fatal(e)
	}
	joined := strings.Join(args, "\n")
	if !strings.Contains(joined, `mcp_servers."server.with spaces".enabled=false`) {
		t.Fatal(args)
	}
	if strings.Contains(joined, "code_mode_host=false") {
		t.Fatal("Disabled dynamic tool dispatcher")
	}
}
func TestMalformedRequestJSON(t *testing.T) {
	for _, s := range []string{`{"x":1} {"y":2}`, `{"x":1} garbage`, `{"x":`} {
		var v M
		if decode([]byte(s), &v) == nil {
			t.Fatal("Accepted trailing or malformed input", s)
		}
	}
	var v M
	if e := decode([]byte(`{"number":1234567890123456789}`), &v); e != nil || fmt.Sprint(v["number"]) != strconv.FormatInt(1234567890123456789, 10) {
		t.Fatal(v, e)
	}
}
