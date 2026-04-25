//go:build darwin

// Package plugins provides cleanup plugin implementations.
// apfs_darwin.go manages APFS snapshots and Time Machine local snapshots
// to reclaim significant disk space on macOS.
package plugins

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// APFSPlugin handles APFS snapshot thinning and Time Machine cleanup.
type APFSPlugin struct {
	sudoCap *SudoCapability
}

// NewAPFSPlugin creates a new APFS snapshot cleanup plugin.
func NewAPFSPlugin() *APFSPlugin {
	return &APFSPlugin{}
}

// Name returns the plugin identifier.
func (p *APFSPlugin) Name() string {
	return "apfs-snapshots"
}

// Description returns the plugin description.
func (p *APFSPlugin) Description() string {
	return "Thins APFS local snapshots and Time Machine caches to reclaim disk space"
}

// SupportedPlatforms returns supported platforms (Darwin only).
func (p *APFSPlugin) SupportedPlatforms() []string {
	return []string{PlatformDarwin}
}

// Enabled checks if APFS snapshot cleanup is enabled.
func (p *APFSPlugin) Enabled(cfg *config.Config) bool {
	return cfg.Enable.APFSSnapshots
}

// Cleanup performs APFS snapshot thinning at the specified level.
func (p *APFSPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{
		Plugin: p.Name(),
		Level:  level,
	}

	// Check if tmutil is available
	if _, err := exec.LookPath("tmutil"); err != nil {
		logger.Debug("tmutil not available, skipping APFS snapshot cleanup")
		return result
	}

	// Detect sudo capability (cache for session)
	if p.sudoCap == nil {
		cap := DetectSudo(ctx)
		p.sudoCap = &cap
	}

	// List current snapshots
	snapshots, err := p.listSnapshots(ctx)
	if err != nil {
		logger.Debug("failed to list snapshots", "error", err)
		return result
	}

	if len(snapshots) == 0 {
		logger.Debug("no local APFS snapshots found")
		return result
	}

	apfsCfg := cfg.APFS

	switch level {
	case LevelWarning:
		// Report only
		p.reportSnapshots(snapshots, logger)
		return result

	case LevelModerate:
		if !apfsCfg.ThinEnabled {
			return result
		}
		if !p.sudoCap.Passwordless {
			logger.Debug("passwordless sudo required for snapshot thinning, skipping")
			return result
		}
		// Request 5GB thinning at urgency 1
		result = p.thinSnapshots(ctx, 5, 1, logger)

	case LevelAggressive:
		if !apfsCfg.ThinEnabled {
			return result
		}
		if !p.sudoCap.Passwordless {
			logger.Debug("passwordless sudo required for snapshot thinning, skipping")
			return result
		}
		// Request 20GB thinning at urgency 3
		result = p.thinSnapshots(ctx, 20, 3, logger)

	case LevelCritical:
		if !p.sudoCap.Passwordless {
			logger.Warn("passwordless sudo required for critical snapshot cleanup, skipping")
			return result
		}

		// Check if Time Machine backup is active - NEVER delete during backup
		if p.isBackupActive(ctx) {
			logger.Warn("Time Machine backup in progress, skipping snapshot deletion")
			return result
		}

		// Request max thinning at urgency 4
		maxThinGB := apfsCfg.MaxThinGB
		if maxThinGB <= 0 {
			maxThinGB = 50
		}
		result = p.thinSnapshots(ctx, maxThinGB, 4, logger)

		// Delete old pre-update snapshots if configured
		if apfsCfg.DeleteOSUpdates {
			keepDays := apfsCfg.KeepRecentDays
			if keepDays <= 0 {
				keepDays = 1
			}
			deleteResult := p.deleteOldSnapshots(ctx, snapshots, keepDays, logger)
			result.BytesFreed += deleteResult.BytesFreed
			result.ItemsCleaned += deleteResult.ItemsCleaned
		}
	}

	result.Plugin = p.Name()
	result.Level = level
	return result
}

// snapshotInfo represents an APFS local snapshot.
type snapshotInfo struct {
	Date string // e.g., "2026-01-15-123456"
	Time time.Time
}

// listSnapshots lists all local APFS snapshots.
func (p *APFSPlugin) listSnapshots(ctx context.Context) ([]snapshotInfo, error) {
	cmd := exec.CommandContext(ctx, "tmutil", "listlocalsnapshots", "/")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("tmutil listlocalsnapshots failed: %w", err)
	}

	return parseSnapshotList(string(output)), nil
}

// parseSnapshotList parses tmutil listlocalsnapshots output.
// Format: "com.apple.TimeMachine.2026-01-15-123456.local" per line
func parseSnapshotList(output string) []snapshotInfo {
	var snapshots []snapshotInfo
	re := regexp.MustCompile(`(\d{4}-\d{2}-\d{2}-\d{6})`)

	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		matches := re.FindStringSubmatch(line)
		if len(matches) < 2 {
			continue
		}

		dateStr := matches[1]
		// Parse "2026-01-15-123456" format
		t, err := time.Parse("2006-01-02-150405", dateStr)
		if err != nil {
			continue
		}

		snapshots = append(snapshots, snapshotInfo{
			Date: dateStr,
			Time: t,
		})
	}

	// Sort by time (newest first)
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].Time.After(snapshots[j].Time)
	})

	return snapshots
}

// reportSnapshots logs information about existing snapshots.
func (p *APFSPlugin) reportSnapshots(snapshots []snapshotInfo, logger *slog.Logger) {
	if len(snapshots) == 0 {
		return
	}

	oldest := snapshots[len(snapshots)-1]
	newest := snapshots[0]

	logger.Info("APFS local snapshots",
		"count", len(snapshots),
		"oldest", oldest.Date,
		"newest", newest.Date,
		"estimated_size_gb", fmt.Sprintf("~%d-%d", len(snapshots)*5, len(snapshots)*15),
	)
}

// thinSnapshots requests macOS to thin local snapshots.
func (p *APFSPlugin) thinSnapshots(ctx context.Context, requestGB int, urgency int, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name()}

	requestBytes := int64(requestGB) * 1024 * 1024 * 1024

	logger.Info("requesting APFS snapshot thinning",
		"request_gb", requestGB,
		"urgency", urgency,
	)

	output, err := RunWithSudo(ctx, "tmutil", "thinlocalsnapshots", "/",
		strconv.FormatInt(requestBytes, 10),
		strconv.Itoa(urgency))
	if err != nil {
		logger.Warn("tmutil thinlocalsnapshots failed", "error", err, "output", string(output))
		return result
	}

	// Parse thinning result
	freed := parseThinOutput(string(output))
	result.BytesFreed = freed
	if freed > 0 {
		result.ItemsCleaned++
		logger.Info("APFS snapshot thinning complete",
			"freed_gb", fmt.Sprintf("%.1f", float64(freed)/(1024*1024*1024)),
		)
	}

	return result
}

// parseThinOutput parses tmutil thinlocalsnapshots output for bytes freed.
// Output format varies but may contain "Thinned local snapshots: X bytes"
func parseThinOutput(output string) int64 {
	// Try to find byte count in output
	re := regexp.MustCompile(`(\d+)\s+bytes?`)
	matches := re.FindAllStringSubmatch(output, -1)
	var maxBytes int64
	for _, match := range matches {
		if len(match) >= 2 {
			if bytes, err := strconv.ParseInt(match[1], 10, 64); err == nil && bytes > maxBytes {
				maxBytes = bytes
			}
		}
	}
	return maxBytes
}

// deleteOldSnapshots deletes snapshots older than keepDays.
// SAFETY: NEVER deletes the most recent snapshot.
func (p *APFSPlugin) deleteOldSnapshots(ctx context.Context, snapshots []snapshotInfo, keepDays int, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name() + "-delete"}

	if len(snapshots) <= 1 {
		// Never delete the only snapshot
		return result
	}

	cutoff := time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour)

	// Skip the first snapshot (most recent) - NEVER delete it
	for _, snap := range snapshots[1:] {
		if snap.Time.After(cutoff) {
			continue // Too recent to delete
		}

		logger.Warn("deleting old APFS snapshot", "date", snap.Date)
		output, err := RunWithSudo(ctx, "tmutil", "deletelocalsnapshots", snap.Date)
		if err != nil {
			logger.Debug("failed to delete snapshot", "date", snap.Date, "error", err, "output", string(output))
			continue
		}

		result.ItemsCleaned++
		// Estimate ~5-15GB per snapshot
		result.BytesFreed += 5 * 1024 * 1024 * 1024
	}

	return result
}

// isBackupActive checks if a Time Machine backup is currently running.
func (p *APFSPlugin) isBackupActive(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "tmutil", "status")
	output, err := cmd.Output()
	if err != nil {
		return false // Assume not active if we can't check
	}

	outputStr := string(output)
	// Check for "Running = 1" or "BackupPhase" in status output
	return strings.Contains(outputStr, "Running = 1") ||
		strings.Contains(outputStr, "BackupPhase")
}
