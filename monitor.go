package main

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed dashboard.html
var dashboardHTML string

type monitorProcess struct {
	PID             int       `json:"pid"`
	ParentPID       int       `json:"parent_pid"`
	Name            string    `json:"name"`
	CWD             string    `json:"cwd,omitempty"`
	CodexHome       string    `json:"codex_home,omitempty"`
	ConfiguredModel string    `json:"configured_model,omitempty"`
	RSSBytes        int64     `json:"rss_bytes"`
	StartedAt       time.Time `json:"started_at"`
	Bridge          bool      `json:"-"`
	startTicks      float64
}

type monitorThread struct {
	ID             string              `json:"id"`
	PID            int                 `json:"pid"`
	ProcessUUID    string              `json:"-"`
	Model          string              `json:"model"`
	Effort         string              `json:"effort,omitempty"`
	Evidence       string              `json:"evidence"`
	LastSeen       time.Time           `json:"last_seen"`
	LastSample     time.Time           `json:"last_sample,omitempty"`
	ContextTokens  int64               `json:"context_tokens,omitempty"`
	SamplingEvents int                 `json:"sampling_events"`
	ModelChanges   int                 `json:"model_changes"`
	BridgeVersion  string              `json:"bridge_version,omitempty"`
	CodexHome      string              `json:"codex_home"`
	BridgePID      int                 `json:"bridge_pid,omitempty"`
	Alive          bool                `json:"alive"`
	LogID          int64               `json:"log_id"`
	LogTarget      string              `json:"log_target"`
	SampleLogID    int64               `json:"sample_log_id,omitempty"`
	Usage          *monitorThreadUsage `json:"usage,omitempty"`
}

type monitorRoot struct {
	monitorProcess
	Children []monitorProcess `json:"children"`
	Threads  []monitorThread  `json:"threads"`
	TotalRSS int64            `json:"total_rss"`
}

type monitorEvent struct {
	At       time.Time `json:"at"`
	ThreadID string    `json:"thread_id"`
	PID      int       `json:"pid"`
	Model    string    `json:"model"`
	Kind     string    `json:"kind"`
}

type monitorSource struct {
	Path    string    `json:"path"`
	First   time.Time `json:"first"`
	Last    time.Time `json:"last"`
	Cursor  int64     `json:"cursor"`
	Error   string    `json:"error,omitempty"`
	Backlog bool      `json:"backlog"`
}

type monitorSnapshot struct {
	Version   string              `json:"version"`
	At        time.Time           `json:"at"`
	Processes []monitorRoot       `json:"processes"`
	Threads   []monitorThread     `json:"threads"`
	Events    []monitorEvent      `json:"events"`
	Sources   []monitorSource     `json:"sources"`
	Warnings  []string            `json:"warnings"`
	Usage     monitorUsageSummary `json:"usage"`
}

type codexLogReader struct {
	home     string
	db       *sql.DB
	source   monitorSource
	versions map[string]string
	threads  map[string]*monitorThread
}

type bridgeMonitor struct {
	mu         sync.RWMutex
	snapshot   []byte
	readers    map[string]*codexLogReader
	events     []monitorEvent
	usageFiles map[string]*monitorRollout
}

var monitorTurn = regexp.MustCompile(`:turn\{[^}]*\bthread\.id=([0-9a-f-]{36})[^}]*\bmodel=([a-zA-Z0-9_.-]+)(?: codex\.turn\.reasoning_effort=([a-zA-Z0-9_-]+))?`)
var monitorPID = regexp.MustCompile(`^pid:(\d+):`)
var monitorVersion = regexp.MustCompile(`app_server\.client_version="([^"]+)"`)
var monitorContext = regexp.MustCompile(`\btotal_usage_tokens=(\d+)`)

func parseMonitorLog(home string, ts int64, body, process string) *monitorThread {
	// Never interpret tool output, prompts or quoted model names as routing
	// evidence. Only structured native session spans have this shape.
	if !strings.HasPrefix(body, "session_loop{") {
		return nil
	}
	m := monitorTurn.FindStringSubmatch(body)
	if m == nil {
		return nil
	}
	p := monitorPID.FindStringSubmatch(process)
	if p == nil {
		return nil
	}
	pid, _ := strconv.Atoi(p[1])
	t := &monitorThread{ID: m[1], PID: pid, ProcessUUID: process, Model: m[2], Effort: m[3], Evidence: "request", LastSeen: time.Unix(ts, 0).UTC(), CodexHome: home}
	if strings.Contains(body, "post sampling token usage") {
		t.Evidence = "sample"
		t.LastSample = t.LastSeen
		t.SamplingEvents = 1
		if n := monitorContext.FindStringSubmatch(body); n != nil {
			t.ContextTokens, _ = strconv.ParseInt(n[1], 10, 64)
		}
	}
	return t
}

func openCodexLog(home string) (*codexLogReader, error) {
	path := filepath.Join(home, "logs_2.sqlite")
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(250)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return &codexLogReader{home: home, db: db, source: monitorSource{Path: path}, versions: map[string]string{}, threads: map[string]*monitorThread{}}, nil
}

func (r *codexLogReader) update(ctx context.Context, add func(monitorEvent)) error {
	var minTS, maxTS sql.NullInt64
	var maxID sql.NullInt64
	if err := r.db.QueryRowContext(ctx, "SELECT min(ts),max(ts),max(id) FROM logs").Scan(&minTS, &maxTS, &maxID); err != nil {
		return err
	}
	if minTS.Valid {
		r.source.First = time.Unix(minTS.Int64, 0).UTC()
		r.source.Last = time.Unix(maxTS.Int64, 0).UTC()
	}
	if maxID.Valid && maxID.Int64 < r.source.Cursor {
		r.source.Cursor = 0
		r.threads = map[string]*monitorThread{}
		r.versions = map[string]string{}
	}
	// Page all records by ID, including uninteresting rows, so polling never
	// repeatedly scans the same retained log tail. Extract only metadata.
	rows, err := r.db.QueryContext(ctx, `SELECT id,ts,target,CASE WHEN target IN ('codex_core::session::turn','feedback_tags','codex_core::client','codex_config::loader::layer_io') THEN feedback_log_body ELSE NULL END,coalesce(process_uuid,'') FROM logs WHERE id>? ORDER BY id LIMIT 12000`, r.source.Cursor)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, ts int64
		var target, body, process string
		var nullable sql.NullString
		if err = rows.Scan(&id, &ts, &target, &nullable, &process); err != nil {
			return err
		}
		body = nullable.String
		r.source.Cursor = id
		if strings.HasPrefix(body, "app_server.request{") && strings.Contains(body, `app_server.client_name="claude_codex_bridge"`) {
			if m := monitorVersion.FindStringSubmatch(body); m != nil {
				r.versions[process] = m[1]
			}
		}
		if target != "codex_core::session::turn" && target != "feedback_tags" && target != "codex_core::client" {
			continue
		}
		n := parseMonitorLog(r.home, ts, body, process)
		if n == nil {
			continue
		}
		n.LogID = id
		n.LogTarget = target
		if n.SamplingEvents > 0 {
			n.SampleLogID = id
		}
		key := process + ":" + n.ID
		prev := r.threads[key]
		if prev == nil {
			r.threads[key] = n
			add(monitorEvent{n.LastSeen, n.ID, n.PID, n.Model, "observed"})
		} else {
			if n.Model != prev.Model {
				prev.ModelChanges++
				prev.Model = n.Model
				prev.ContextTokens = 0
				prev.Evidence = "request"
				prev.LastSample = time.Time{}
				prev.SampleLogID = 0
				add(monitorEvent{n.LastSeen, n.ID, n.PID, n.Model, "model_change"})
			}
			prev.LastSeen = n.LastSeen
			prev.LogID = n.LogID
			prev.LogTarget = n.LogTarget
			prev.Effort = n.Effort
			if n.SamplingEvents > 0 {
				prev.ContextTokens = n.ContextTokens
				prev.LastSample = n.LastSample
				prev.SamplingEvents++
				prev.Evidence = "sample"
				prev.SampleLogID = n.SampleLogID
			}
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	r.source.Backlog = r.source.Cursor < maxID.Int64
	return nil
}

func bridgeAncestor(pid int, all map[int]monitorProcess) int {
	seen := map[int]bool{}
	for pid > 0 && !seen[pid] {
		seen[pid] = true
		p, ok := all[pid]
		if !ok {
			return 0
		}
		if p.Bridge {
			return pid
		}
		pid = p.ParentPID
	}
	return 0
}

func (m *bridgeMonitor) refresh(ctx context.Context) {
	processes, processErr := monitorProcesses()
	all := map[int]monitorProcess{}
	homes := map[string]bool{codexHome(): true}
	roots := map[int]*monitorRoot{}
	s := monitorSnapshot{Version: bridgeVersion, At: time.Now().UTC(), Processes: []monitorRoot{}, Threads: []monitorThread{}, Events: []monitorEvent{}, Sources: []monitorSource{}, Warnings: []string{}}
	if processErr != nil {
		s.Warnings = append(s.Warnings, processErr.Error())
	}
	for _, p := range processes {
		all[p.PID] = p
		if p.Bridge {
			roots[p.PID] = &monitorRoot{monitorProcess: p, Children: []monitorProcess{}, Threads: []monitorThread{}, TotalRSS: p.RSSBytes}
			if p.CodexHome != "" {
				homes[p.CodexHome] = true
			}
		}
	}
	for _, p := range processes {
		if root := bridgeAncestor(p.PID, all); root > 0 && root != p.PID {
			roots[root].Children = append(roots[root].Children, p)
			roots[root].TotalRSS += p.RSSBytes
			if p.CodexHome != "" {
				homes[p.CodexHome] = true
			}
		}
	}
	for home := range homes {
		if m.readers[home] == nil {
			r, err := openCodexLog(home)
			if err != nil {
				s.Sources = append(s.Sources, monitorSource{Path: filepath.Join(home, "logs_2.sqlite"), Error: err.Error()})
				continue
			}
			m.readers[home] = r
		}
	}
	for _, r := range m.readers {
		r.source.Error = ""
		if err := r.update(ctx, func(e monitorEvent) {
			m.events = append(m.events, e)
			if len(m.events) > 200 {
				m.events = m.events[len(m.events)-200:]
			}
		}); err != nil {
			r.source.Error = err.Error()
		}
		s.Sources = append(s.Sources, r.source)
		for _, t := range r.threads {
			v := *t
			v.BridgeVersion = r.versions[v.ProcessUUID]
			p, alive := all[v.PID]
			// PID reuse must not attach a previous app-server to a new bridge.
			alive = alive && !p.StartedAt.After(v.LastSeen.Add(2*time.Second))
			if alive {
				v.BridgePID = bridgeAncestor(v.PID, all)
			}
			v.Alive = alive && v.BridgePID > 0
			if v.BridgeVersion == "" && v.BridgePID == 0 {
				continue
			}
			s.Threads = append(s.Threads, v)
			if v.Alive {
				roots[v.BridgePID].Threads = append(roots[v.BridgePID].Threads, v)
			}
		}
		// Bound monitor memory for very long running processes.
		if len(r.threads) > 5000 {
			for k, t := range r.threads {
				if time.Since(t.LastSeen) > 24*time.Hour {
					delete(r.threads, k)
				}
			}
		}
	}
	m.updateUsage(ctx, &s, roots)
	for _, r := range roots {
		sort.Slice(r.Threads, func(i, j int) bool { return r.Threads[i].LastSeen.After(r.Threads[j].LastSeen) })
		sort.Slice(r.Children, func(i, j int) bool { return r.Children[i].PID < r.Children[j].PID })
		s.Processes = append(s.Processes, *r)
	}
	sort.Slice(s.Processes, func(i, j int) bool { return s.Processes[i].PID < s.Processes[j].PID })
	sort.Slice(s.Threads, func(i, j int) bool { return s.Threads[i].LastSeen.After(s.Threads[j].LastSeen) })
	if len(s.Threads) > 300 {
		s.Threads = s.Threads[:300]
	}
	// Include events only when their native process is identified as a bridge.
	bridgeKeys := map[string]bool{}
	for _, t := range s.Threads {
		bridgeKeys[fmt.Sprintf("%d:%s", t.PID, t.ID)] = true
	}
	for i := len(m.events) - 1; i >= 0; i-- {
		e := m.events[i]
		if bridgeKeys[fmt.Sprintf("%d:%s", e.PID, e.ThreadID)] {
			s.Events = append(s.Events, e)
		}
	}
	m.mu.Lock()
	m.snapshot = encode(s)
	m.mu.Unlock()
}

func (m *bridgeMonitor) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No cross-origin requests or DNS rebinding to local process metadata.
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "127.0.0.1" && host != "localhost" && host != "[::1]" && host != "::1" {
			http.Error(w, "Local host required", 403)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host {
				http.Error(w, "Same origin required", 403)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		if r.Method != "GET" {
			http.Error(w, "Read only", 405)
			return
		}
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(dashboardHTML))
		case "/api/monitor":
			w.Header().Set("Content-Type", "application/json")
			m.mu.RLock()
			raw := append([]byte(nil), m.snapshot...)
			m.mu.RUnlock()
			if len(raw) == 0 {
				raw = []byte(`{"processes":[],"threads":[],"events":[],"sources":[],"warnings":["Iniciando lectura de registros"]}`)
			}
			_, _ = w.Write(raw)
		default:
			http.NotFound(w, r)
		}
	})
}

func runDashboard(port int) (int, error) {
	if port == 0 {
		port = 8787
	}
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return 1, err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), terminationSignals()...)
	defer cancel()
	m := &bridgeMonitor{readers: map[string]*codexLogReader{}}
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		defer func() {
			for _, r := range m.readers {
				r.db.Close()
			}
		}()
		for {
			scanCtx, stop := context.WithTimeout(ctx, 8*time.Second)
			m.refresh(scanCtx)
			stop()
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	}()
	srv := &http.Server{Handler: m.handler(), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, ErrorLog: log.New(os.Stderr, "dashboard: ", 0)}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	fmt.Printf("GPT Bridge dashboard: http://%s\nSolo lectura · registros de Codex · actualización cada 3 segundos\n", l.Addr())
	err = srv.Serve(l)
	cancel()
	<-workerDone
	if errors.Is(err, http.ErrServerClosed) {
		return 0, nil
	}
	return 1, err
}
