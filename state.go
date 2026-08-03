package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/plugins"
)

const cleanupStateVersion = 2

type cleanupState struct {
	Version int                          `json:"version"`
	Plugins map[string]pluginStateRecord `json:"plugins"`
	// Inodes tracks per-monitor-path inode reclaim progress so the daemon can
	// detect inode pressure that repeated cleanup cycles fail to relieve and
	// back off instead of churning every poll interval (TIN-2170).
	Inodes map[string]inodeProgressRecord `json:"inodes,omitempty"`
	// Alerts persists bounded duplicate-alert suppression across daemon restarts.
	Alerts map[string]alertStateRecord `json:"alerts,omitempty"`
}

type pluginStateRecord struct {
	LastRun           string `json:"last_run"`
	LastLevel         string `json:"last_level"`
	LastLevelValue    int    `json:"last_level_value"`
	LastBytesFreed    int64  `json:"last_bytes_freed"`
	LastItemsCleaned  int    `json:"last_items_cleaned"`
	LastError         string `json:"last_error,omitempty"`
	LastOutcome       string `json:"last_outcome,omitempty"`
	LastReceiptDigest string `json:"last_receipt_digest,omitempty"`
	ZeroYieldCount    int    `json:"zero_yield_count,omitempty"`
	BackoffUntil      string `json:"backoff_until,omitempty"`
	BackoffReason     string `json:"backoff_reason,omitempty"`
}

type alertStateRecord struct {
	LastEmitted string `json:"last_emitted"`
	Suppressed  int    `json:"suppressed"`
}

// inodeProgressRecord records the inode state observed at the end of the most
// recent cleanup cycle for one monitored path, plus a counter of consecutive
// cycles that did not relieve inode pressure.
type inodeProgressRecord struct {
	LastRun               string  `json:"last_run"`
	LastInodesFree        uint64  `json:"last_inodes_free"`
	LastInodesUsedPercent float64 `json:"last_inodes_used_percent"`
	LastInodeLevel        string  `json:"last_inode_level"`
	NoProgressCount       int     `json:"no_progress_count"`
}

func newCleanupState() *cleanupState {
	return &cleanupState{
		Version: cleanupStateVersion,
		Plugins: map[string]pluginStateRecord{},
		Inodes:  map[string]inodeProgressRecord{},
		Alerts:  map[string]alertStateRecord{},
	}
}

func loadCleanupState(path string) (*cleanupState, error) {
	if path == "" {
		return newCleanupState(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newCleanupState(), nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return newCleanupState(), nil
	}

	state := newCleanupState()
	if err := json.Unmarshal(data, state); err != nil {
		return nil, err
	}
	if state.Plugins == nil {
		state.Plugins = map[string]pluginStateRecord{}
	}
	if state.Inodes == nil {
		state.Inodes = map[string]inodeProgressRecord{}
	}
	if state.Alerts == nil {
		state.Alerts = map[string]alertStateRecord{}
	}
	if state.Version > cleanupStateVersion {
		return nil, fmt.Errorf("cleanup state version %d is newer than supported version %d", state.Version, cleanupStateVersion)
	}
	if state.Version < cleanupStateVersion {
		state.Version = cleanupStateVersion
	}
	return state, nil
}

func saveCleanupState(path string, state *cleanupState) error {
	if path == "" || state == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeAtomicFile(path, data, 0644)
}

func writeAtomicFile(path string, data []byte, mode os.FileMode) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tinyland-cleanup-atomic-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	dirHandle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer dirHandle.Close()
	if err := dirHandle.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return nil
}

func (s *cleanupState) cooldownRemaining(plugin string, level plugins.CleanupLevel, now time.Time, cooldown time.Duration) time.Duration {
	if s == nil || cooldown <= 0 {
		return 0
	}
	record, ok := s.Plugins[plugin]
	if !ok || record.LastLevelValue < int(level) {
		return 0
	}
	lastRun, err := time.Parse(time.RFC3339, record.LastRun)
	if err != nil {
		return 0
	}
	elapsed := now.Sub(lastRun)
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed >= cooldown {
		return 0
	}
	return cooldown - elapsed
}

func (s *cleanupState) recordPluginRun(plugin string, level plugins.CleanupLevel, now time.Time, result plugins.CleanupResult) {
	s.recordPluginRunWithPolicy(plugin, level, now, result, 3, 30*time.Minute)
}

func (s *cleanupState) recordPluginRunWithPolicy(plugin string, level plugins.CleanupLevel, now time.Time, result plugins.CleanupResult, zeroYieldLimit int, zeroYieldBackoff time.Duration) {
	if s == nil {
		return
	}
	if s.Plugins == nil {
		s.Plugins = map[string]pluginStateRecord{}
	}
	previous := s.Plugins[plugin]
	record := pluginStateRecord{
		LastRun:           now.UTC().Format(time.RFC3339),
		LastLevel:         level.String(),
		LastLevelValue:    int(level),
		LastBytesFreed:    result.BytesFreed,
		LastItemsCleaned:  result.ItemsCleaned,
		LastOutcome:       result.Outcome,
		LastReceiptDigest: result.ReceiptDigest,
	}
	if result.Outcome == plugins.CleanupOutcomeDeferred {
		record.ZeroYieldCount = previous.ZeroYieldCount
		retry := time.Duration(result.RetryAfterSeconds) * time.Second
		if retry <= 0 {
			retry = zeroYieldBackoff
		}
		if retry > 0 {
			record.BackoffUntil = now.Add(retry).UTC().Format(time.RFC3339)
			record.BackoffReason = "external_deferred"
		}
	} else if result.Outcome == plugins.CleanupOutcomeRefused {
		record.ZeroYieldCount = previous.ZeroYieldCount
		if result.Error != nil {
			record.LastError = result.Error.Error()
		}
		if zeroYieldBackoff > 0 {
			record.BackoffUntil = now.Add(zeroYieldBackoff).UTC().Format(time.RFC3339)
			record.BackoffReason = "external_refused_backoff"
		}
	} else if result.Error != nil {
		record.LastError = result.Error.Error()
		record.ZeroYieldCount = previous.ZeroYieldCount
		record.BackoffUntil = previous.BackoffUntil
		record.BackoffReason = previous.BackoffReason
	} else if result.BytesFreed > 0 || result.ItemsCleaned > 0 {
		record.ZeroYieldCount = 0
		record.BackoffUntil = ""
		record.BackoffReason = ""
	} else {
		record.ZeroYieldCount = previous.ZeroYieldCount + 1
		if zeroYieldLimit <= 0 {
			zeroYieldLimit = 3
		}
		if record.ZeroYieldCount >= zeroYieldLimit && zeroYieldBackoff > 0 {
			record.BackoffUntil = now.Add(zeroYieldBackoff).UTC().Format(time.RFC3339)
			record.BackoffReason = "zero_yield_backoff"
		}
	}
	s.Plugins[plugin] = record
}

func (s *cleanupState) zeroYieldBackoffRemaining(plugin string, now time.Time) time.Duration {
	remaining, _ := s.pluginBackoff(plugin, now)
	return remaining
}

func (s *cleanupState) pluginBackoff(plugin string, now time.Time) (time.Duration, string) {
	if s == nil {
		return 0, ""
	}
	record, ok := s.Plugins[plugin]
	if !ok || record.BackoffUntil == "" {
		return 0, ""
	}
	until, err := time.Parse(time.RFC3339, record.BackoffUntil)
	if err != nil || !until.After(now) {
		return 0, ""
	}
	return until.Sub(now), record.BackoffReason
}

func (s *cleanupState) zeroYieldCount(plugin string) int {
	if s == nil {
		return 0
	}
	return s.Plugins[plugin].ZeroYieldCount
}

func (s *cleanupState) recordAlert(signature string, now time.Time, repeatInterval time.Duration) (bool, int) {
	if s == nil || signature == "" {
		return false, 0
	}
	if s.Alerts == nil {
		s.Alerts = map[string]alertStateRecord{}
	}
	record := s.Alerts[signature]
	last, err := time.Parse(time.RFC3339, record.LastEmitted)
	if err == nil && repeatInterval > 0 && now.Sub(last) < repeatInterval {
		record.Suppressed++
		s.Alerts[signature] = record
		return false, record.Suppressed
	}
	record.LastEmitted = now.UTC().Format(time.RFC3339)
	record.Suppressed = 0
	s.Alerts[signature] = record
	s.pruneAlerts(32)
	return true, 0
}

func (s *cleanupState) pruneAlerts(limit int) {
	if s == nil || limit <= 0 || len(s.Alerts) <= limit {
		return
	}
	type entry struct {
		key  string
		when time.Time
	}
	entries := make([]entry, 0, len(s.Alerts))
	for key, record := range s.Alerts {
		when, _ := time.Parse(time.RFC3339, record.LastEmitted)
		entries = append(entries, entry{key: key, when: when})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].when.Before(entries[j].when) })
	for len(entries) > limit {
		delete(s.Alerts, entries[0].key)
		entries = entries[1:]
	}
}

// inodeNoProgressCount returns the number of consecutive recent cleanup cycles
// that failed to relieve inode pressure on path.
func (s *cleanupState) inodeNoProgressCount(path string) int {
	if s == nil || s.Inodes == nil || path == "" {
		return 0
	}
	return s.Inodes[path].NoProgressCount
}

// recordInodeProgress updates the inode no-progress tracker for path. When
// improved is true (the cycle increased free inodes or cleared inode pressure)
// the counter resets; otherwise it increments so the daemon can detect
// unrelievable inode pressure and back off.
func (s *cleanupState) recordInodeProgress(path string, inodesFree uint64, usedPercent float64, level string, now time.Time, improved bool) {
	if s == nil || path == "" {
		return
	}
	if s.Inodes == nil {
		s.Inodes = map[string]inodeProgressRecord{}
	}
	record := s.Inodes[path]
	if improved {
		record.NoProgressCount = 0
	} else {
		record.NoProgressCount++
	}
	record.LastRun = now.UTC().Format(time.RFC3339)
	record.LastInodesFree = inodesFree
	record.LastInodesUsedPercent = usedPercent
	record.LastInodeLevel = level
	s.Inodes[path] = record
}
