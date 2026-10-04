package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/plugins"
)

// cleanupStateVersion 2 adds per-path byte progress (TIN-3342). Version 1
// files load unchanged with an empty byte map, and older binaries ignore the
// new field, so a rollback keeps working.
const cleanupStateVersion = 2

type cleanupState struct {
	Version int                          `json:"version"`
	Plugins map[string]pluginStateRecord `json:"plugins"`
	// Inodes tracks per-monitor-path inode reclaim progress so the daemon can
	// detect inode pressure that repeated cleanup cycles fail to relieve and
	// back off instead of churning every poll interval (TIN-2170).
	Inodes map[string]inodeProgressRecord `json:"inodes,omitempty"`
	// Bytes tracks per-monitor-path byte reclaim progress so the daemon can
	// detect byte pressure that repeated cleanup cycles fail to relieve and
	// back off instead of rescanning every poll interval (TIN-3342).
	Bytes map[string]byteProgressRecord `json:"bytes,omitempty"`
}

type pluginStateRecord struct {
	LastRun          string `json:"last_run"`
	LastLevel        string `json:"last_level"`
	LastLevelValue   int    `json:"last_level_value"`
	LastBytesFreed   int64  `json:"last_bytes_freed"`
	LastItemsCleaned int    `json:"last_items_cleaned"`
	LastError        string `json:"last_error,omitempty"`
	// ZeroYieldCount is the number of consecutive runs that reclaimed less
	// than policy.byte_progress_min_mb (TIN-3342).
	ZeroYieldCount int `json:"zero_yield_count,omitempty"`
	// SuppressedUntil is when a zero-yield-suppressed plugin may run again
	// (RFC3339). Empty when the plugin is not suppressed.
	SuppressedUntil string `json:"suppressed_until,omitempty"`
	// ConfigDigest identifies the configuration and binary version the
	// zero-yield count was measured under. A change clears suppression.
	ConfigDigest string `json:"config_digest,omitempty"`
}

// zeroYieldPolicy is the resolved zero-yield suppression policy for a cycle.
type zeroYieldPolicy struct {
	limit int
	base  time.Duration
	max   time.Duration
}

// zeroYieldBackoff returns how long a plugin is suppressed after count
// consecutive zero-yield runs: base at the limit, doubling per further run,
// capped at max. Below the limit, or with suppression disabled, it is zero.
func zeroYieldBackoff(count int, policy zeroYieldPolicy) time.Duration {
	if policy.limit <= 0 || count < policy.limit || policy.base <= 0 {
		return 0
	}
	interval := policy.base
	for i := policy.limit; i < count && interval < policy.max; i++ {
		if policy.max > 0 && interval > policy.max/2 {
			// Doubling would pass the cap (or overflow for a huge cap).
			interval = policy.max
			break
		}
		interval *= 2
	}
	if policy.max > 0 && interval > policy.max {
		interval = policy.max
	}
	return interval
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

// byteProgressRecord records the byte state observed at the end of the most
// recent cleanup cycle for one monitored path, plus a counter of consecutive
// cycles that ran cleanup under byte pressure without meaningful reclaim.
type byteProgressRecord struct {
	LastRun        string `json:"last_run"`
	LastFreeBytes  uint64 `json:"last_free_bytes"`
	LastByteLevel  string `json:"last_byte_level"`
	LastLevelValue int    `json:"last_level_value"`
	// NoProgressCount is the number of consecutive no-progress cycles at or
	// below LastLevelValue.
	NoProgressCount int `json:"no_progress_count"`
	// Engaged records whether byte backoff was engaged on the last cycle, so
	// the engage and release transitions are logged once each.
	Engaged bool `json:"engaged,omitempty"`
}

func newCleanupState() *cleanupState {
	return &cleanupState{
		Version: cleanupStateVersion,
		Plugins: map[string]pluginStateRecord{},
		Inodes:  map[string]inodeProgressRecord{},
		Bytes:   map[string]byteProgressRecord{},
	}
}

// loadCleanupState reads the persisted cleanup state. A missing or empty file
// yields fresh state. A file that exists but does not decode is quarantined:
// it is renamed to <path>.corrupt-<timestamp> and fresh state is returned
// together with the quarantine path, so one bad write cannot disable cooldown
// and backoff accounting for every later cycle (TIN-3342). An error is
// returned only when the file cannot be read or the quarantine rename fails.
func loadCleanupState(path string, now time.Time) (*cleanupState, string, error) {
	if path == "" {
		return newCleanupState(), "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newCleanupState(), "", nil
		}
		return nil, "", err
	}
	if len(data) == 0 {
		return newCleanupState(), "", nil
	}

	state := newCleanupState()
	if err := json.Unmarshal(data, state); err != nil {
		quarantined, qerr := quarantineCleanupState(path, now)
		if qerr != nil {
			return nil, "", fmt.Errorf("decode cleanup state: %v; quarantine failed: %w", err, qerr)
		}
		return newCleanupState(), quarantined, nil
	}
	if state.Plugins == nil {
		state.Plugins = map[string]pluginStateRecord{}
	}
	if state.Inodes == nil {
		state.Inodes = map[string]inodeProgressRecord{}
	}
	if state.Bytes == nil {
		state.Bytes = map[string]byteProgressRecord{}
	}
	// Older versions only lack the byte map, which is now initialised, so the
	// state is saved as the current schema. A newer version is left as is.
	if state.Version < cleanupStateVersion {
		state.Version = cleanupStateVersion
	}
	return state, "", nil
}

// quarantineCleanupState renames an undecodable state file out of the way and
// returns the new path. It never overwrites an earlier quarantined file.
func quarantineCleanupState(path string, now time.Time) (string, error) {
	base := path + ".corrupt-" + now.UTC().Format("20060102T150405Z")
	target := base
	for i := 1; ; i++ {
		if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			return "", err
		}
		if i > 100 {
			return "", fmt.Errorf("no free quarantine name for %s", path)
		}
		target = fmt.Sprintf("%s-%d", base, i)
	}
	if err := os.Rename(path, target); err != nil {
		return "", err
	}
	return target, nil
}

// saveCleanupState writes the state atomically: it writes a temp file in the
// same directory, syncs it, and renames it over the destination, so a crash or
// restart mid-write leaves either the old state or the new one, never a
// truncated file (TIN-3342).
func saveCleanupState(path string, state *cleanupState) (err error) {
	if path == "" || state == nil {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Chmod(0644); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpName, path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir makes a completed rename durable by syncing its directory. It is
// best effort: the rename has already happened, so a failure here (for example
// a filesystem that does not support directory fsync) is not a save error.
func syncDir(dir string) {
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = f.Sync()
	_ = f.Close()
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
	if s == nil {
		return
	}
	if s.Plugins == nil {
		s.Plugins = map[string]pluginStateRecord{}
	}
	// Merge into the existing record so the zero-yield fields survive.
	record := s.Plugins[plugin]
	record.LastRun = now.UTC().Format(time.RFC3339)
	record.LastLevel = level.String()
	record.LastLevelValue = int(level)
	record.LastBytesFreed = result.BytesFreed
	record.LastItemsCleaned = result.ItemsCleaned
	record.LastError = ""
	if result.Error != nil {
		record.LastError = result.Error.Error()
	}
	s.Plugins[plugin] = record
}

// recordPluginYield updates a plugin's zero-yield count after it ran. A count
// measured under a different config digest starts over. A run with yield
// resets the count and lifts suppression; a zero-yield run advances it and,
// once the limit is reached, suppresses the plugin from now until
// now+zeroYieldBackoff. It returns the updated record.
//
// suppressible false (a permanently exempt plugin) still counts zero-yield
// runs but never sets a retry time.
func (s *cleanupState) recordPluginYield(plugin string, zeroYield bool, now time.Time, digest string, policy zeroYieldPolicy, suppressible bool) pluginStateRecord {
	if s == nil {
		return pluginStateRecord{}
	}
	if s.Plugins == nil {
		s.Plugins = map[string]pluginStateRecord{}
	}
	record := s.Plugins[plugin]
	if record.ConfigDigest != digest {
		record.ZeroYieldCount = 0
	}
	record.ConfigDigest = digest
	record.SuppressedUntil = ""
	if !zeroYield || policy.limit <= 0 {
		record.ZeroYieldCount = 0
	} else {
		record.ZeroYieldCount++
		if interval := zeroYieldBackoff(record.ZeroYieldCount, policy); suppressible && interval > 0 {
			record.SuppressedUntil = now.Add(interval).UTC().Format(time.RFC3339)
		}
	}
	s.Plugins[plugin] = record
	return record
}

// liftZeroYieldSuppression clears every plugin's zero-yield retry time and
// reports whether any was set. Counts are kept, so a plugin that still yields
// nothing is suppressed again after its one fresh run, with a longer interval.
func (s *cleanupState) liftZeroYieldSuppression() bool {
	if s == nil {
		return false
	}
	lifted := false
	for name, record := range s.Plugins {
		if record.SuppressedUntil == "" {
			continue
		}
		record.SuppressedUntil = ""
		s.Plugins[name] = record
		lifted = true
	}
	return lifted
}

// zeroYieldSuppression reports whether plugin is suppressed for zero yield
// at level and digest, until when, and its zero-yield count. Suppression
// holds only while now is before the recorded retry time, the level is not
// above the plugin's last run, and the config digest is unchanged. When the
// retry time is still ahead but a condition changed, lifted names it:
// "level_rose" or "config_changed".
func (s *cleanupState) zeroYieldSuppression(plugin string, level plugins.CleanupLevel, now time.Time, digest string) (until time.Time, count int, lifted string, suppressed bool) {
	if s == nil || s.Plugins == nil {
		return time.Time{}, 0, "", false
	}
	record, ok := s.Plugins[plugin]
	if !ok || record.SuppressedUntil == "" {
		return time.Time{}, 0, "", false
	}
	until, err := time.Parse(time.RFC3339, record.SuppressedUntil)
	if err != nil || !now.Before(until) {
		return time.Time{}, record.ZeroYieldCount, "", false
	}
	switch {
	case int(level) > record.LastLevelValue:
		return until, record.ZeroYieldCount, "level_rose", false
	case record.ConfigDigest != digest:
		return until, record.ZeroYieldCount, "config_changed", false
	}
	return until, record.ZeroYieldCount, "", true
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

// byteNoProgressCount returns the number of consecutive recent cleanup cycles
// that failed to relieve byte pressure on path at the given byte level. A
// level above the recorded one returns zero: an escalation earns a fresh
// attempt before backoff can engage again.
func (s *cleanupState) byteNoProgressCount(path string, level int) int {
	if s == nil || s.Bytes == nil || path == "" {
		return 0
	}
	record, ok := s.Bytes[path]
	if !ok || level > record.LastLevelValue {
		return 0
	}
	return record.NoProgressCount
}

// byteNoProgressPending reports whether path carries a non-zero byte
// no-progress count at any level. Unlike byteNoProgressCount it does not
// compare levels, so a streak accrued below critical is still found and reset
// when pressure clears (TIN-3342 review).
func (s *cleanupState) byteNoProgressPending(path string) bool {
	if s == nil || s.Bytes == nil || path == "" {
		return false
	}
	return s.Bytes[path].NoProgressCount > 0
}

// byteBackoffEngaged reports whether byte backoff was engaged for path on the
// most recent recorded cycle.
func (s *cleanupState) byteBackoffEngaged(path string) bool {
	if s == nil || s.Bytes == nil || path == "" {
		return false
	}
	return s.Bytes[path].Engaged
}

// recordByteProgress updates the byte no-progress tracker for path. progress
// resets the counter. Otherwise, when cleanupRan is true the counter advances;
// a cycle where every plugin was held back leaves it unchanged, so the counter
// measures cleanup attempts rather than polls. A level above the recorded one
// restarts counting from zero before the cycle is applied.
func (s *cleanupState) recordByteProgress(path string, freeBytes uint64, level string, levelValue int, now time.Time, progress, cleanupRan, engaged bool) {
	if s == nil || path == "" {
		return
	}
	if s.Bytes == nil {
		s.Bytes = map[string]byteProgressRecord{}
	}
	record := s.Bytes[path]
	if levelValue > record.LastLevelValue {
		record.NoProgressCount = 0
	}
	switch {
	case progress:
		record.NoProgressCount = 0
	case cleanupRan:
		record.NoProgressCount++
	}
	record.LastRun = now.UTC().Format(time.RFC3339)
	record.LastFreeBytes = freeBytes
	record.LastByteLevel = level
	record.LastLevelValue = levelValue
	record.Engaged = engaged
	s.Bytes[path] = record
}
