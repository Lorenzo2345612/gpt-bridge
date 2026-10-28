package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

// monitorLatency summarizes the bridge's own request timings (metrics.jsonl):
// how long Claude Code waited, not what Codex logged.
type monitorLatency struct {
	Path        string                 `json:"path"`
	Hours       int                    `json:"hours"`
	Requests    int                    `json:"requests"`
	FirstP50    int64                  `json:"first_p50_ms"`
	FirstP90    int64                  `json:"first_p90_ms"`
	DurationP50 int64                  `json:"duration_p50_ms"`
	DurationP90 int64                  `json:"duration_p90_ms"`
	GapP90      int64                  `json:"gap_p90_ms"`
	Retries     int                    `json:"retries"`
	Tools       int                    `json:"tools"`
	Errors      map[string]int         `json:"errors"`
	Threads     []monitorLatencyThread `json:"threads"`
	Error       string                 `json:"error,omitempty"`
}

type monitorLatencyThread struct {
	Thread   string    `json:"thread"`
	Model    string    `json:"model"`
	Requests int       `json:"requests"`
	FirstP50 int64     `json:"first_p50_ms"`
	FirstP90 int64     `json:"first_p90_ms"`
	MaxGap   int64     `json:"max_gap_ms"`
	Retries  int       `json:"retries"`
	Errors   int       `json:"errors"`
	Last     time.Time `json:"last"`
	first    []int64
}

func percentile(values []int64, p float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[int(p*float64(len(sorted)-1)+0.5)]
}

func readLatency(path string, since time.Time) monitorLatency {
	out := monitorLatency{Path: path, Hours: int(time.Since(since).Round(time.Hour).Hours()), Errors: map[string]int{}, Threads: []monitorLatencyThread{}}
	var first, duration, gaps []int64
	threads := map[string]*monitorLatencyThread{}
	read := 0
	for _, name := range []string{path + ".1", path} {
		f, err := os.Open(name)
		if err != nil {
			continue
		}
		read++
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		for scanner.Scan() {
			var m requestMetric
			if json.Unmarshal(scanner.Bytes(), &m) != nil {
				continue
			}
			at, err := time.Parse(time.RFC3339Nano, m.Time)
			if err != nil || at.Before(since) {
				continue
			}
			out.Requests++
			out.Retries += m.Retries
			out.Tools += m.Tools
			duration = append(duration, m.DurationMS)
			gaps = append(gaps, m.MaxGapMS)
			t := threads[m.Thread]
			if t == nil {
				t = &monitorLatencyThread{Thread: m.Thread}
				threads[m.Thread] = t
			}
			t.Requests++
			t.Model = m.Model
			t.Retries += m.Retries
			t.MaxGap = max(t.MaxGap, m.MaxGapMS)
			if at.After(t.Last) {
				t.Last = at
			}
			if m.Status >= 400 {
				out.Errors[map[bool]string{true: "429", false: "otros"}[m.Status == 429]]++
				if m.Status == 504 {
					out.Errors["504"]++
					out.Errors["otros"]--
				}
				t.Errors++
			} else if !m.Replay && m.FirstOutputMS > 0 {
				first = append(first, m.FirstOutputMS)
				t.first = append(t.first, m.FirstOutputMS)
			}
		}
		f.Close()
	}
	if read == 0 {
		out.Error = "Sin métricas todavía: se crean con bridges 0.6.20 o posteriores"
	}
	out.FirstP50, out.FirstP90 = percentile(first, 0.5), percentile(first, 0.9)
	out.DurationP50, out.DurationP90 = percentile(duration, 0.5), percentile(duration, 0.9)
	out.GapP90 = percentile(gaps, 0.9)
	for _, t := range threads {
		t.FirstP50, t.FirstP90 = percentile(t.first, 0.5), percentile(t.first, 0.9)
		out.Threads = append(out.Threads, *t)
	}
	sort.Slice(out.Threads, func(i, j int) bool { return out.Threads[i].Last.After(out.Threads[j].Last) })
	if len(out.Threads) > 50 {
		out.Threads = out.Threads[:50]
	}
	return out
}

const dashboardPort = 8787

// ensureDashboard starts one shared, detached dashboard when the first bridge
// launches. A bridge that finds the port taken leaves it alone; if two race,
// the loser's listen fails and it exits.
func ensureDashboard() {
	if os.Getenv("CLAUDE_CODEX_DASHBOARD") == "0" {
		return
	}
	address := fmt.Sprintf("127.0.0.1:%d", dashboardPort)
	if conn, err := net.DialTimeout("tcp", address, 300*time.Millisecond); err == nil {
		conn.Close()
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	dir := filepath.Join(userHome(), ".local", "state", "gpt-bridge")
	if os.MkdirAll(dir, 0700) != nil {
		return
	}
	logFile, err := os.OpenFile(filepath.Join(dir, "dashboard.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return
	}
	defer logFile.Close()
	cmd := exec.Command(self, "--dashboard", "--port", fmt.Sprint(dashboardPort))
	cmd.Stdout, cmd.Stderr = logFile, logFile
	configureBackend(cmd)
	if cmd.Start() == nil {
		_ = os.WriteFile(filepath.Join(dir, "dashboard.pid"), []byte(fmt.Sprintln(cmd.Process.Pid)), 0600)
		_ = cmd.Process.Release()
	}
}
