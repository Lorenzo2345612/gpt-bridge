//go:build !windows

package main

import (
	"os"
	"syscall"
)

func helperProcessIsolated() bool      { group, _ := syscall.Getpgid(0); return group == os.Getpid() }
func helperProcessExists(pid int) bool { return syscall.Kill(pid, 0) != syscall.ESRCH }
