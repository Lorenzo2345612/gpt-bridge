package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Cached input is a subset of input; reasoning is a subset of output.
// Neither subset is added again when displaying the total.
type monitorTokens struct {
	Input     int64 `json:"input_tokens"`
	Cached    int64 `json:"cached_input_tokens"`
	Output    int64 `json:"output_tokens"`
	Reasoning int64 `json:"reasoning_output_tokens"`
	Calls     int64 `json:"calls"`
}

func (t *monitorTokens) add(v monitorTokens) {
	t.Input += v.Input
	t.Cached += v.Cached
	t.Output += v.Output
	t.Reasoning += v.Reasoning
	t.Calls += v.Calls
}

type monitorThreadUsage struct {
	Path         string                   `json:"path"`
	Total        monitorTokens            `json:"total"`
	Today        monitorTokens            `json:"today"`
	ByModel      map[string]monitorTokens `json:"by_model"`
	TodayByModel map[string]monitorTokens `json:"today_by_model"`
	Last         time.Time                `json:"last"`
	Available    bool                     `json:"available"`
	Backlog      bool                     `json:"backlog"`
	Resets       int                      `json:"counter_resets"`
	Error        string                   `json:"error,omitempty"`
}

type monitorUsageSummary struct {
	Total        monitorTokens            `json:"total"`
	Today        monitorTokens            `json:"today"`
	ByModel      map[string]monitorTokens `json:"by_model"`
	TodayByModel map[string]monitorTokens `json:"today_by_model"`
	Date         string                   `json:"date"`
	Timezone     string                   `json:"timezone"`
	Threads      int                      `json:"threads"`
	Covered      int                      `json:"covered"`
	Backlog      bool                     `json:"backlog"`
}

type monitorRollout struct {
	path          string
	info          os.FileInfo
	offset        int64
	model         string
	previous      monitorTokens
	previousTotal int64
	hasPrevious   bool
	total         monitorTokens
	models        map[string]monitorTokens
	days          map[string]map[string]monitorTokens
	last          time.Time
	resets        int
	backlog       bool
	err           string
}

func (r *monitorRollout) consume(line []byte) {
	var event struct {
		Type      string          `json:"type"`
		Timestamp time.Time       `json:"timestamp"`
		Payload   json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(line, &event) != nil {
		return
	}
	if event.Type == "turn_context" {
		var p struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(event.Payload, &p) == nil {
			r.model = p.Model
		}
		return
	}
	if event.Type != "event_msg" {
		return
	}
	var p struct {
		Type string `json:"type"`
		Info *struct {
			Total monitorTokens `json:"total_token_usage"`
			Last  monitorTokens `json:"last_token_usage"`
		} `json:"info"`
	}
	if json.Unmarshal(event.Payload, &p) != nil || p.Type != "token_count" || p.Info == nil {
		return
	}
	cur := p.Info.Total
	sum := cur.Input + cur.Output
	if sum < 0 || cur.Input < 0 || cur.Output < 0 || cur.Cached < 0 || cur.Cached > cur.Input || cur.Reasoning < 0 || cur.Reasoning > cur.Output {
		return
	}
	if r.hasPrevious && sum == r.previousTotal {
		return
	}
	delta := monitorTokens{Input: cur.Input - r.previous.Input, Cached: cur.Cached - r.previous.Cached, Output: cur.Output - r.previous.Output, Reasoning: cur.Reasoning - r.previous.Reasoning, Calls: 1}
	if r.hasPrevious && (delta.Input < 0 || delta.Cached < 0 || delta.Output < 0 || delta.Reasoning < 0) {
		// A resumed backend may restart its counters. Count only its explicitly
		// reported last call rather than attributing an entire counter to it.
		delta = p.Info.Last
		delta.Calls = 1
		r.resets++
	}
	r.previous = cur
	r.previousTotal = sum
	r.hasPrevious = true
	if delta.Input == 0 && delta.Output == 0 {
		return
	}
	model := r.model
	if model == "" {
		model = "unknown"
	}
	r.total.add(delta)
	v := r.models[model]
	v.add(delta)
	r.models[model] = v
	day := event.Timestamp.In(time.Local).Format("2006-01-02")
	if r.days[day] == nil {
		r.days[day] = map[string]monitorTokens{}
	}
	v = r.days[day][model]
	v.add(delta)
	r.days[day][model] = v
	r.last = event.Timestamp
}

func (r *monitorRollout) update(ctx context.Context) error {
	f, err := os.Open(r.path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if r.info == nil || !os.SameFile(r.info, info) || info.Size() < r.offset {
		path := r.path
		*r = monitorRollout{path: path, info: info, models: map[string]monitorTokens{}, days: map[string]map[string]monitorTokens{}}
	}
	if _, err = f.Seek(r.offset, io.SeekStart); err != nil {
		return err
	}
	b := bufio.NewReaderSize(f, 8192)
	start := r.offset
	for r.offset-start < 16<<20 {
		if err = ctx.Err(); err != nil {
			return err
		}
		part, e := b.ReadSlice('\n')
		size := int64(len(part))
		contextLine := bytes.Contains(part, []byte(`"type": "turn_context"`)) || bytes.Contains(part, []byte(`"type":"turn_context"`))
		candidate := contextLine || bytes.Contains(part, []byte(`"type": "event_msg"`)) || bytes.Contains(part, []byte(`"type":"event_msg"`))
		var line []byte
		if candidate {
			line = append(line, part...)
		}
		for errors.Is(e, bufio.ErrBufferFull) {
			part, e = b.ReadSlice('\n')
			size += int64(len(part))
			if candidate && len(line)+len(part) <= 2<<20 {
				line = append(line, part...)
			} else {
				candidate = false
				line = nil
				if contextLine {
					r.model = ""
				}
			}
		}
		// An incomplete append is retried from the last committed line next poll.
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return e
		}
		r.offset += size
		if candidate {
			r.consume(line)
		}
	}
	r.backlog = r.offset < info.Size()
	return nil
}

func (r *monitorRollout) snapshot(day string) *monitorThreadUsage {
	u := &monitorThreadUsage{Path: r.path, Total: r.total, ByModel: r.models, TodayByModel: r.days[day], Last: r.last, Available: r.hasPrevious, Backlog: r.backlog, Resets: r.resets, Error: r.err}
	for _, v := range u.TodayByModel {
		u.Today.add(v)
	}
	return u
}

func rolloutPaths(ctx context.Context, home string, wanted map[string]bool) (map[string]string, error) {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(home, "state_5.sqlite"))}
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(250)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT id,rollout_path FROM threads")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paths := map[string]string{}
	for rows.Next() {
		var id, path string
		if err = rows.Scan(&id, &path); err != nil {
			return nil, err
		}
		if wanted[id] && path != "" {
			paths[id] = path
		}
	}
	return paths, rows.Err()
}

func (m *bridgeMonitor) updateUsage(ctx context.Context, s *monitorSnapshot, roots map[int]*monitorRoot) {
	if m.usageFiles == nil {
		m.usageFiles = map[string]*monitorRollout{}
	}
	day := time.Now().Format("2006-01-02")
	s.Usage = monitorUsageSummary{Date: day, Timezone: time.Now().Format("MST -07:00"), ByModel: map[string]monitorTokens{}, TodayByModel: map[string]monitorTokens{}}
	// Deduplicate by native thread, not process: a persistent thread can appear
	// in multiple app-server processes after a restart.
	groups := map[string]map[string]bool{}
	for _, t := range s.Threads {
		if groups[t.CodexHome] == nil {
			groups[t.CodexHome] = map[string]bool{}
		}
		groups[t.CodexHome][t.ID] = true
	}
	usages := map[string]*monitorThreadUsage{}
	for home, wanted := range groups {
		s.Usage.Threads += len(wanted)
		paths, err := rolloutPaths(ctx, home, wanted)
		if err != nil {
			s.Warnings = append(s.Warnings, "No se pudo leer el índice de tokens de Codex: "+err.Error())
			continue
		}
		ids := make([]string, 0, len(paths))
		for id := range paths {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			path := paths[id]
			r := m.usageFiles[path]
			if r == nil {
				r = &monitorRollout{path: path}
				m.usageFiles[path] = r
			}
			r.err = ""
			if err = r.update(ctx); err != nil {
				r.err = err.Error()
			}
			u := r.snapshot(day)
			usages[home+":"+id] = u
			if u.Available {
				s.Usage.Covered++
			}
			s.Usage.Backlog = s.Usage.Backlog || u.Backlog
			s.Usage.Total.add(u.Total)
			s.Usage.Today.add(u.Today)
			for model, v := range u.ByModel {
				sum := s.Usage.ByModel[model]
				sum.add(v)
				s.Usage.ByModel[model] = sum
			}
			for model, v := range u.TodayByModel {
				sum := s.Usage.TodayByModel[model]
				sum.add(v)
				s.Usage.TodayByModel[model] = sum
			}
		}
	}
	for i := range s.Threads {
		t := &s.Threads[i]
		t.Usage = usages[t.CodexHome+":"+t.ID]
	}
	for _, r := range roots {
		for i := range r.Threads {
			t := &r.Threads[i]
			t.Usage = usages[t.CodexHome+":"+t.ID]
		}
	}
}
