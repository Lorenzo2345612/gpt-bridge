//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func monitorProcesses() ([]monitorProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var result []monitorProcess
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		base := filepath.Join("/proc", entry.Name())
		raw, err := os.ReadFile(filepath.Join(base, "stat"))
		if err != nil {
			continue
		}
		end := strings.LastIndexByte(string(raw), ')')
		if end < 0 {
			continue
		}
		fields := strings.Fields(string(raw[end+1:]))
		if len(fields) < 22 {
			continue
		}
		ppid, _ := strconv.Atoi(fields[1])
		name, _ := os.Readlink(filepath.Join(base, "exe"))
		name = strings.TrimSuffix(filepath.Base(name), " (deleted)")
		p := monitorProcess{PID: pid, ParentPID: ppid, Name: name}
		p.Bridge = name == "claude-codex" || name == "claude-codex.exe"
		if p.Bridge && pid == os.Getpid() {
			p.Bridge = false
		}
		rss, _ := strconv.ParseInt(fields[21], 10, 64)
		p.RSSBytes = rss * int64(os.Getpagesize())
		// /proc directory timestamps are unsuitable for PID reuse detection. Derive
		// process age from elapsed runtime reported by procps, only for bridge roots.
		if p.Bridge {
			p.CWD, _ = os.Readlink(filepath.Join(base, "cwd"))
			if cmd, e := os.ReadFile(filepath.Join(base, "cmdline")); e == nil {
				args := strings.Split(string(cmd), "\x00")
				for i, a := range args {
					if a == "--dashboard" || strings.HasPrefix(a, "--dashboard=") {
						p.Bridge = false
					}
					if (a == "--model" || a == "--codex-model") && i+1 < len(args) {
						p.ConfiguredModel = args[i+1]
					}
					if strings.HasPrefix(a, "--model=") || strings.HasPrefix(a, "--codex-model=") {
						_, p.ConfiguredModel, _ = strings.Cut(a, "=")
					}
				}
			}
		}
		if name == "codex" || p.Bridge || name == "node" {
			if env, e := os.ReadFile(filepath.Join(base, "environ")); e == nil {
				for _, v := range strings.Split(string(env), "\x00") {
					if strings.HasPrefix(v, "CODEX_HOME=") {
						p.CodexHome = strings.TrimPrefix(v, "CODEX_HOME=")
					}
					if strings.HasPrefix(v, "CLAUDE_CODEX_MODEL=") && p.ConfiguredModel == "" {
						p.ConfiguredModel = strings.TrimPrefix(v, "CLAUDE_CODEX_MODEL=")
					}
				}
			}
		}
		// Starttime is monotonic ticks (USER_HZ=100 on Linux); boot time is
		// obtained from uptime, avoiding dependence on wall-clock proc metadata.
		ticks, _ := strconv.ParseFloat(fields[19], 64)
		p.startTicks = ticks
		result = append(result, p)
	}
	if raw, e := os.ReadFile("/proc/uptime"); e == nil {
		f := strings.Fields(string(raw))
		if len(f) > 0 {
			up, _ := strconv.ParseFloat(f[0], 64)
			boot := time.Now().Add(-time.Duration(up * float64(time.Second)))
			for i := range result {
				result[i].StartedAt = boot.Add(time.Duration(result[i].startTicks / 100 * float64(time.Second))).UTC()
			}
		}
	}
	return result, nil
}
