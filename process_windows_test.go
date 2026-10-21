//go:build windows

package main

import (
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"unsafe"
)

func helperProcessIsolated() bool {
	var inJob int32
	result, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob").Call(uintptr(windows.CurrentProcess()), 0, uintptr(unsafe.Pointer(&inJob)))
	return result != 0 && inJob != 0
}
func helperProcessExists(pid int) bool {
	p, e := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if e != nil {
		return false
	}
	defer windows.CloseHandle(p)
	var code uint32
	return windows.GetExitCodeProcess(p, &code) == nil && code == 259
}
func TestNPMShimArgumentsBypassCmd(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "node_modules", "@openai", "codex", "bin", "codex.js")
	_ = os.MkdirAll(filepath.Dir(entry), 0700)
	_ = os.WriteFile(entry, []byte("// fixture"), 0600)
	_ = os.WriteFile(filepath.Join(dir, "node.exe"), []byte("fixture"), 0600)
	args := []string{"--agents", `{"text":"a & b %PATH% !value!"}`, "a b", `quotes"and&metacharacters`}
	cmd, e := platformCommand(filepath.Join(dir, "codex.cmd"), args...)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(cmd.Args[1:], append([]string{entry}, args...)) {
		t.Fatal(cmd.Args)
	}
	if _, e = platformCommand(filepath.Join(dir, "unknown.cmd"), args...); e == nil {
		t.Fatal("Accepted unknown shell shim")
	}
}
