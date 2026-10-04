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

const cleanupStateVersion = 1

type cleanupState struct {
	Version int                          `json:"version"`
	Plugins map[string]pluginStateRecord `json:"plugins"`
	// Inodes tracks per-monitor-path inode reclaim progress so the daemon can
	// detect inode pressure that repeated cleanup cycles fail to relieve and
	// back off instead of churning every poll interval (TIN-2170).
	Inodes map[string]inodeProgressRecord `json:"inodes,omitempty"`
}

type pluginStateRecord struct {
	LastRun          string `json:"last_run"`
	LastLevel        string `json:"last_level"`
	LastLevelValue   int    `json:"last_level_value"`
	LastBytesFreed   int64  `json:"last_bytes_freed"`
	LastItemsCleaned int    `json:"last_items_cleaned"`
	LastError        string `json:"last_error,omitempty"`
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
	if state.Version == 0 {
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
	record := pluginStateRecord{
		LastRun:          now.UTC().Format(time.RFC3339),
		LastLevel:        level.String(),
		LastLevelValue:   int(level),
		LastBytesFreed:   result.BytesFreed,
		LastItemsCleaned: result.ItemsCleaned,
	}
	if result.Error != nil {
		record.LastError = result.Error.Error()
	}
	s.Plugins[plugin] = record
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
