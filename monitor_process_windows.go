//go:build windows

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func monitorProcesses() ([]monitorProcess, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Only process metadata is serialized, never command lines or environments.
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `[Console]::OutputEncoding=[System.Text.UTF8Encoding]::new(); @(Get-CimInstance Win32_Process | ForEach-Object { @{pid=$_.ProcessId;parent_pid=$_.ParentProcessId;name=$_.Name;rss_bytes=$_.WorkingSetSize;started_at=if ($_.CreationDate) {$_.CreationDate.ToUniversalTime().ToString('o')} else {''}} }) | ConvertTo-Json -Compress`)
	raw, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var records []struct {
		PID     int    `json:"pid"`
		Parent  int    `json:"parent_pid"`
		Name    string `json:"name"`
		RSS     any    `json:"rss_bytes"`
		Started string `json:"started_at"`
	}
	if err = json.Unmarshal(raw, &records); err != nil {
		return nil, err
	}
	var result []monitorProcess
	for _, v := range records {
		p := monitorProcess{PID: v.PID, ParentPID: v.Parent, Name: v.Name}
		p.StartedAt, _ = time.Parse(time.RFC3339Nano, v.Started)
		switch n := v.RSS.(type) {
		case float64:
			p.RSSBytes = int64(n)
		case string:
			p.RSSBytes, _ = strconv.ParseInt(n, 10, 64)
		}
		p.Bridge = strings.EqualFold(p.Name, "claude-codex.exe") && p.PID != os.Getpid()
		result = append(result, p)
	}
	return result, nil
}
