//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Jobs contain only the child we start and its descendants, never the user's
// terminal. Closing the job also cleans up native tools launched via npm/Node.
type processScope struct {
	job  windows.Handle
	once sync.Once
}

func configureBackend(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}
func trackProcess(p *os.Process, _ bool) (*processScope, error) {
	job, e := windows.CreateJobObject(nil, nil)
	if e != nil {
		return nil, e
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, e = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); e != nil {
		windows.CloseHandle(job)
		return nil, e
	}
	process, e := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if e != nil {
		windows.CloseHandle(job)
		return nil, e
	}
	defer windows.CloseHandle(process)
	if e = windows.AssignProcessToJobObject(job, process); e != nil {
		windows.CloseHandle(job)
		return nil, e
	}
	return &processScope{job: job}, nil
}
func (s *processScope) terminate()    { _ = windows.TerminateJobObject(s.job, 1) }
func (s *processScope) kill()         { s.terminate() }
func (s *processScope) close()        { s.once.Do(func() { _ = windows.CloseHandle(s.job) }) }
func terminationSignals() []os.Signal { return []os.Signal{os.Interrupt, syscall.SIGTERM} }
func signalNumber(sig os.Signal) int {
	if sig == os.Interrupt {
		return 2
	}
	return 15
}
func processExitCode(e *exec.ExitError) int {
	code := e.ExitCode()
	if uint32(code) == 0xc000013a {
		return 130
	}
	return code
}

func platformCommand(binary string, args ...string) (*exec.Cmd, error) {
	ext := strings.ToLower(filepath.Ext(binary))
	if ext != ".cmd" && ext != ".bat" {
		return exec.Command(binary, args...), nil
	}
	// npm Windows shims invoke a known JS entrypoint. Launch Node directly rather
	// than feeding prompts/JSON through cmd.exe's shell expansion and quoting.
	dir := filepath.Dir(binary)
	name := strings.ToLower(strings.TrimSuffix(filepath.Base(binary), ext))
	var entry string
	switch name {
	case "codex":
		entry = filepath.Join(dir, "node_modules", "@openai", "codex", "bin", "codex.js")
	case "claude":
		entry = filepath.Join(dir, "node_modules", "@anthropic-ai", "claude-code", "cli.js")
	}
	if entry == "" {
		return nil, fmt.Errorf("unsupported Windows command shim; use a native executable or a standard npm installation")
	}
	if _, e := os.Stat(entry); e != nil {
		return nil, fmt.Errorf("cannot find the npm entrypoint for %s; use a native executable or reinstall with npm", name)
	}
	node := filepath.Join(dir, "node.exe")
	if _, e := os.Stat(node); e != nil {
		var err error
		node, err = exec.LookPath("node")
		if err != nil {
			return nil, fmt.Errorf("node.exe is required for the npm-installed %s CLI", name)
		}
	}
	return exec.Command(node, append([]string{entry}, args...)...), nil
}
