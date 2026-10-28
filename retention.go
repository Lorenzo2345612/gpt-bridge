package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Reuse Claude's setting instead of introducing a bridge-specific TTL. If any
// selected source cannot be read safely, skip cleanup rather than guess a TTL.
func claudeRetentionDays(cwd string, args []string) (int, error) {
	sources := map[string]bool{"user": true, "project": true, "local": true}
	for i, arg := range args {
		name, value, inline := strings.Cut(arg, "=")
		if name == "--setting-sources" {
			if !inline {
				if i+1 >= len(args) {
					return 0, fmt.Errorf("missing --setting-sources value")
				}
				value = args[i+1]
			}
			sources = map[string]bool{}
			for _, source := range strings.Split(value, ",") {
				if source != "" && source != "user" && source != "project" && source != "local" {
					return 0, fmt.Errorf("unknown settings source")
				}
				sources[source] = true
			}
		}
	}
	settings := M{}
	read := func(path string) error {
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		var value M
		if decode(raw, &value) != nil || value == nil {
			return fmt.Errorf("invalid retention settings at %s", path)
		}
		mergeSettings(settings, value)
		return nil
	}
	for _, source := range []struct{ name, path string }{
		{"user", filepath.Join(claudeConfig(), "settings.json")},
		{"project", filepath.Join(cwd, ".claude", "settings.json")},
		{"local", filepath.Join(cwd, ".claude", "settings.local.json")},
	} {
		if sources[source.name] {
			if err := read(source.path); err != nil {
				return 0, err
			}
		}
	}
	inline, _, err := mergedHostSettings(args, M{})
	if err != nil {
		return 0, err
	}
	mergeSettings(settings, inline)
	managed := "/etc/claude-code/managed-settings.json"
	if runtime.GOOS == "darwin" {
		managed = "/Library/Application Support/ClaudeCode/managed-settings.json"
	} else if runtime.GOOS == "windows" {
		managed = filepath.Join(os.Getenv("ProgramFiles"), "ClaudeCode", "managed-settings.json")
	}
	if err := read(managed); err != nil {
		return 0, err
	}
	value, exists := settings["cleanupPeriodDays"]
	if !exists {
		return 30, nil
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("invalid cleanupPeriodDays; cleanup skipped")
	}
	days, err := number.Float64()
	if err != nil || math.IsNaN(days) || math.IsInf(days, 0) || days < 1 || days != math.Trunc(days) || days > float64(math.MaxInt32) {
		return 0, fmt.Errorf("invalid cleanupPeriodDays; cleanup skipped")
	}
	return int(days), nil
}

// Expire a whole host branch group, never individual referenced receipts. Index
// and checkpoint leases exclude both concurrent creators and active executors.
// Leave the tiny lock files in place: unlinking a lock can split its ownership.
func (b *Bridge) cleanupExpiredSessions(ctx context.Context, now time.Time) error {
	if b.stateDir == "" {
		return nil
	}
	entries, err := os.ReadDir(b.stateDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() || !safeReplyHash.MatchString(entry.Name()) {
			continue
		}
		dir := filepath.Join(b.stateDir, entry.Name())
		if err := b.cleanupExpiredGroup(dir, entry.Name(), now); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bridge) cleanupExpiredGroup(dir, identity string, now time.Time) error {
	gate, err := tryStateLock(filepath.Join(dir, "index.lock"))
	if err != nil {
		// Another bridge owns this group; it decides when cleanup is safe.
		return nil
	}
	defer gate.Close()
	states, err := readCheckpoints(dir, identity)
	if err != nil {
		return err
	}
	marker := filepath.Join(dir, ".expired")
	if len(states) == 0 {
		// Finish a sweep interrupted after checkpoint removal, before receipts.
		if _, err := os.Stat(marker); err == nil {
			if err := os.RemoveAll(filepath.Join(dir, "replies")); err != nil {
				return err
			}
			return os.Remove(marker)
		}
		return nil
	}
	var accessed time.Time
	if raw, err := os.ReadFile(filepath.Join(dir, ".last-used")); err == nil {
		if decode(raw, &accessed) != nil {
			return fmt.Errorf("invalid session activity timestamp")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	leases := []*os.File{}
	defer func() {
		for _, lease := range leases {
			lease.Close()
		}
	}()
	for _, state := range states {
		activity := state.UpdatedAt
		if accessed.After(activity) {
			activity = accessed
		}
		// A host session can remain active after switching away from the bridge.
		if safeClaudeID.MatchString(state.ClaudeSessionID) {
			paths, err := filepath.Glob(filepath.Join(claudeConfig(), "projects", "*", state.ClaudeSessionID+".jsonl"))
			if err != nil {
				return err
			}
			for _, path := range paths {
				info, err := os.Stat(path)
				if err != nil {
					return err
				}
				if info.ModTime().After(activity) {
					activity = info.ModTime()
				}
			}
		}
		// Old checkpoints have no reliable workspace-specific policy. Adopt one
		// when resumed instead of applying another project's retention to them.
		if state.RetentionDays < 1 || activity.IsZero() || now.Sub(activity).Hours()/24 < float64(state.RetentionDays) ||
			state.InFlight || len(state.Pending) > 0 {
			return nil
		}
		lease, err := tryStateLock(state.path + ".lock")
		if err != nil {
			return nil
		}
		leases = append(leases, lease)
	}
	if err := writePrivateState(marker, encode(now)); err != nil {
		return err
	}
	// Remove checkpoints before receipts. A partial failure leaves all remaining
	// references intact; no surviving checkpoint can point at a purged receipt.
	for _, state := range states {
		if err := os.Remove(state.path); err != nil {
			return err
		}
	}
	// The directory barrier makes checkpoint deletion durable before payload
	// deletion on Unix. Windows removals are ordered by the filesystem calls.
	if runtime.GOOS != "windows" {
		directory, err := os.Open(dir)
		if err != nil {
			return err
		}
		err = directory.Sync()
		directory.Close()
		if err != nil {
			return err
		}
	}
	if err := os.RemoveAll(filepath.Join(dir, "replies")); err != nil {
		return err
	}
	return os.Remove(marker)
}
