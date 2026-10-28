package main

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const maxMetricsBytes = 10 << 20

// requestMetric records timing only: no prompts, responses or tool arguments.
type requestMetric struct {
	start       time.Time
	firstOutput time.Time
	Time        string `json:"ts"`
	Thread      string `json:"thread,omitempty"`
	Model       string `json:"model,omitempty"`
	FreshThread bool   `json:"fresh_thread,omitempty"`
	Replay      bool   `json:"replay,omitempty"`
	// SetupMS covers executor start, thread resume and turn start/steer.
	SetupMS       int64  `json:"setup_ms"`
	FirstOutputMS int64  `json:"first_output_ms"`
	FirstTextMS   int64  `json:"first_text_ms,omitempty"`
	DurationMS    int64  `json:"duration_ms"`
	Tools         int    `json:"tools,omitempty"`
	Stop          string `json:"stop,omitempty"`
	Status        int    `json:"status,omitempty"`
	Usage         M      `json:"usage,omitempty"`
}

var metricsMu sync.Mutex

func metricsPath() string {
	return fallback(os.Getenv("CLAUDE_CODEX_METRICS_FILE"), filepath.Join(claudeConfig(), "gpt-bridge", "metrics.jsonl"))
}

func (b *Bridge) recordMetric(m requestMetric) {
	if m.Model == "" || os.Getenv("CLAUDE_CODEX_METRICS") == "0" {
		return
	}
	m.Time = m.start.UTC().Format(time.RFC3339Nano)
	m.DurationMS = time.Since(m.start).Milliseconds()
	path := metricsPath()
	metricsMu.Lock()
	defer metricsMu.Unlock()
	if info, err := os.Stat(path); err == nil && info.Size() > maxMetricsBytes {
		_ = os.Rename(path, path+".1")
	}
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(encode(m), '\n'))
}

func envInt(name string, fallbackValue int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n >= 0 {
		return n
	}
	return fallbackValue
}
