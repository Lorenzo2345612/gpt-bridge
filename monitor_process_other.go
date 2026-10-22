//go:build !linux && !windows && !darwin

package main

import "fmt"

func monitorProcesses() ([]monitorProcess, error) {
	return nil, fmt.Errorf("process discovery currently requires Linux or WSL; Codex log history remains available")
}
