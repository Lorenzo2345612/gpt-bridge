//go:build darwin

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func monitorProcesses() ([]monitorProcess, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,rss=,etime=,comm=").Output()
	if err != nil {
		return nil, err
	}
	var result []monitorProcess
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		parent, _ := strconv.Atoi(f[1])
		rss, _ := strconv.ParseInt(f[2], 10, 64)
		elapsed := f[3]
		days := 0
		if a, b, ok := strings.Cut(elapsed, "-"); ok {
			days, _ = strconv.Atoi(a)
			elapsed = b
		}
		seconds := 0
		for _, v := range strings.Split(elapsed, ":") {
			n, _ := strconv.Atoi(v)
			seconds = seconds*60 + n
		}
		seconds += days * 86400
		name := filepath.Base(strings.Join(f[4:], " "))
		p := monitorProcess{PID: pid, ParentPID: parent, Name: name, RSSBytes: rss * 1024, StartedAt: time.Now().Add(-time.Duration(seconds) * time.Second).UTC()}
		p.Bridge = name == "claude-codex" && pid != os.Getpid()
		result = append(result, p)
	}
	return result, nil
}
