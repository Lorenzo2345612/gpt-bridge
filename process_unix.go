//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

type processScope struct {
	process *os.Process
	group   bool
}

func platformCommand(binary string, args ...string) (*exec.Cmd, error) {
	return exec.Command(binary, args...), nil
}
func configureBackend(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
func trackProcess(p *os.Process, backend bool) (*processScope, error) {
	return &processScope{p, backend}, nil
}
func (s *processScope) terminate() {
	if s.group {
		_ = syscall.Kill(-s.process.Pid, syscall.SIGTERM)
	} else {
		_ = s.process.Signal(syscall.SIGTERM)
	}
}
func (s *processScope) kill() {
	if s.group {
		_ = syscall.Kill(-s.process.Pid, syscall.SIGKILL)
	} else {
		_ = s.process.Kill()
	}
}
func (s *processScope) close() {}
func terminationSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
}
func signalNumber(sig os.Signal) int { return int(sig.(syscall.Signal)) }
func processExitCode(e *exec.ExitError) int {
	code := e.ExitCode()
	if code < 0 {
		return 128 + int(e.Sys().(syscall.WaitStatus).Signal())
	}
	return code
}
