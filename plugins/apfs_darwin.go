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

const apfsGiB = int64(1024 * 1024 * 1024)

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

// PlanCleanup returns a non-mutating APFS snapshot cleanup plan.
func (p *APFSPlugin) PlanCleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupPlan {
	apfsCfg := cfg.APFS
	requestGB, urgency := apfsThinRequest(level, apfsCfg)
	plan := CleanupPlan{
		Plugin:   p.Name(),
		Level:    level.String(),
		Summary:  "APFS snapshot review plan",
		WouldRun: true,
		Steps:    apfsPlanSteps(level, apfsCfg, requestGB, urgency),
		Metadata: map[string]string{
			"cleanup_level":     level.String(),
			"thin_enabled":      strconv.FormatBool(apfsCfg.ThinEnabled),
			"max_thin_gb":       strconv.Itoa(apfsCfg.MaxThinGB),
			"keep_recent_days":  strconv.Itoa(apfsCfg.KeepRecentDays),
			"delete_os_updates": strconv.FormatBool(apfsCfg.DeleteOSUpdates),
			"request_thin_gb":   strconv.Itoa(requestGB),
			"urgency":           strconv.Itoa(urgency),
		},
	}

	if _, err := exec.LookPath("tmutil"); err != nil {
		plan.Summary = "APFS snapshot tooling is not available"
		plan.WouldRun = false
		plan.SkipReason = "tmutil_unavailable"
		return plan
	}

	if p.sudoCap == nil {
		cap := DetectSudo(ctx)
		p.sudoCap = &cap
	}
	plan.Metadata["sudo_available"] = strconv.FormatBool(p.sudoCap.Available)
	plan.Metadata["sudo_passwordless"] = strconv.FormatBool(p.sudoCap.Passwordless)

	snapshots, err := p.listSnapshots(ctx)
	if err != nil {
		plan.Summary = "APFS snapshots could not be inspected"
		plan.WouldRun = false
		plan.SkipReason = "apfs_snapshot_list_failed"
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("could not list APFS local snapshots: %v", err))
		return plan
	}

	plan.Metadata["snapshot_count"] = strconv.Itoa(len(snapshots))
	if len(snapshots) == 0 {
		plan.Summary = "No APFS local snapshots found"
		plan.WouldRun = false
		plan.SkipReason = "no_apfs_snapshots"
		return plan
	}
	timeMachineSnapshots, osUpdateSnapshots := apfsSnapshotCounts(snapshots)
	plan.Metadata["time_machine_snapshot_count"] = strconv.Itoa(timeMachineSnapshots)
	plan.Metadata["os_update_snapshot_count"] = strconv.Itoa(osUpdateSnapshots)
	plan.Metadata["newest_snapshot"] = apfsSnapshotDisplayName(snapshots[0])
	plan.Metadata["oldest_snapshot"] = apfsSnapshotDisplayName(snapshots[len(snapshots)-1])

	low, high := apfsSnapshotEstimateBytes(snapshots)
	plan.Metadata["estimated_snapshot_low_bytes"] = strconv.FormatInt(low, 10)
	plan.Metadata["estimated_snapshot_high_bytes"] = strconv.FormatInt(high, 10)

	backupActive := p.isBackupActive(ctx)
	plan.Metadata["backup_active"] = strconv.FormatBool(backupActive)

	plan.Targets = apfsPlanTargets(snapshots, level, apfsCfg, requestGB, backupActive, p.sudoCap.Passwordless, time.Now())
	plan.EstimatedBytesFreed = apfsEstimatedCandidateBytes(plan.Targets)
	plan.Metadata["target_count"] = strconv.Itoa(len(plan.Targets))

	switch {
	case level == LevelWarning:
		plan.Summary = "APFS snapshots are report-only at warning level"
		plan.WouldRun = false
		plan.SkipReason = "report_only"
	case level >= LevelModerate && !apfsCfg.ThinEnabled:
		plan.Summary = "APFS snapshot thinning is disabled"
		plan.WouldRun = false
		plan.SkipReason = "apfs_thinning_disabled"
	case level >= LevelModerate && !p.sudoCap.Passwordless:
		plan.Summary = "APFS snapshot cleanup is deferred because passwordless sudo is unavailable"
		plan.WouldRun = false
		plan.SkipReason = "sudo_required"
	case backupActive:
		plan.Summary = "APFS snapshot cleanup is deferred because Time Machine backup is active"
		plan.WouldRun = false
		plan.SkipReason = "time_machine_backup_active"
	}

	plan.Warnings = append(plan.Warnings, "APFS snapshot size is not reported by tmutil; estimates use a conservative 5-15 GiB per local snapshot")
	plan.Warnings = append(plan.Warnings, "APFS snapshot thinning requires passwordless sudo and may reclaim less than requested")
	return plan
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

	if p.isBackupActive(ctx) {
		logger.Warn("Time Machine backup in progress, skipping snapshot cleanup")
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
	Date       string // e.g., "2026-01-15-123456", or the full identifier for non-date snapshots.
	Time       time.Time
	Kind       string
	Identifier string
	HasTime    bool
}

// listSnapshots lists all local APFS snapshots.
func (p *APFSPlugin) listSnapshots(ctx context.Context) ([]snapshotInfo, error) {
	listCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(listCtx, "tmutil", "listlocalsnapshots", "/")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("tmutil listlocalsnapshots failed: %w", err)
	}

	return parseSnapshotList(string(output)), nil
}

// parseSnapshotList parses tmutil listlocalsnapshots output.
// Time Machine snapshots are date-form identifiers such as
// "com.apple.TimeMachine.2026-01-15-123456.local". macOS update snapshots are
// non-date identifiers such as "com.apple.os.update-..." and must be reported
// separately because tmutil deletion accepts date-form tokens.
func parseSnapshotList(output string) []snapshotInfo {
	var snapshots []snapshotInfo
	re := regexp.MustCompile(`(\d{4}-\d{2}-\d{2}-\d{6})`)

	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "com.apple.os.update-") {
			snapshots = append(snapshots, snapshotInfo{
				Date:       line,
				Kind:       "os-update",
				Identifier: line,
			})
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
			Date:       dateStr,
			Time:       t,
			Kind:       "time-machine",
			Identifier: line,
			HasTime:    true,
		})
	}

	// Sort timed snapshots by time (newest first), then keep non-timed
	// snapshots in tmutil's original order.
	sort.SliceStable(snapshots, func(i, j int) bool {
		iHasTime := apfsSnapshotHasTime(snapshots[i])
		jHasTime := apfsSnapshotHasTime(snapshots[j])
		switch {
		case iHasTime && jHasTime:
			return snapshots[i].Time.After(snapshots[j].Time)
		case iHasTime != jHasTime:
			return iHasTime
		default:
			return false
		}
	})

	return snapshots
}

func apfsSnapshotKind(snapshot snapshotInfo) string {
	if snapshot.Kind != "" {
		return snapshot.Kind
	}
	if apfsSnapshotHasTime(snapshot) {
		return "time-machine"
	}
	return "unknown"
}

func apfsSnapshotHasTime(snapshot snapshotInfo) bool {
	return snapshot.HasTime || !snapshot.Time.IsZero()
}

func apfsSnapshotDisplayName(snapshot snapshotInfo) string {
	if snapshot.Identifier != "" {
		return snapshot.Identifier
	}
	return snapshot.Date
}

func apfsSnapshotDeleteToken(snapshot snapshotInfo) (string, bool) {
	if apfsSnapshotHasTime(snapshot) && snapshot.Date != "" {
		return snapshot.Date, true
	}
	return "", false
}

func apfsSnapshotCounts(snapshots []snapshotInfo) (timeMachine int, osUpdate int) {
	for _, snapshot := range snapshots {
		switch apfsSnapshotKind(snapshot) {
		case "time-machine":
			timeMachine++
		case "os-update":
			osUpdate++
		}
	}
	return timeMachine, osUpdate
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
		"oldest", apfsSnapshotDisplayName(oldest),
		"newest", apfsSnapshotDisplayName(newest),
		"estimated_size_gb", fmt.Sprintf("~%d-%d", len(snapshots)*5, len(snapshots)*15),
	)
}

func apfsPlanSteps(level CleanupLevel, cfg config.APFSConfig, requestGB int, urgency int) []string {
	switch level {
	case LevelWarning:
		return []string{"List APFS local snapshots for operator review"}
	case LevelModerate, LevelAggressive:
		return []string{
			"List APFS local snapshots",
			"Confirm passwordless sudo is available",
			fmt.Sprintf("Request tmutil thinlocalsnapshots for %d GiB at urgency %d", requestGB, urgency),
			"Skip snapshot thinning while a Time Machine backup is active",
		}
	case LevelCritical:
		steps := []string{
			"List APFS local snapshots",
			"Confirm passwordless sudo is available",
			"Confirm Time Machine backup is not active",
			fmt.Sprintf("Request tmutil thinlocalsnapshots for %d GiB at urgency %d", requestGB, urgency),
		}
		if cfg.DeleteOSUpdates {
			keepDays := cfg.KeepRecentDays
			if keepDays <= 0 {
				keepDays = 1
			}
			steps = append(steps, fmt.Sprintf("Delete old local snapshots older than %d day(s), preserving the newest snapshot", keepDays))
		}
		return steps
	default:
		return []string{"Report APFS snapshot state"}
	}
}

func apfsThinRequest(level CleanupLevel, cfg config.APFSConfig) (int, int) {
	switch level {
	case LevelModerate:
		return 5, 1
	case LevelAggressive:
		return 20, 3
	case LevelCritical:
		maxThinGB := cfg.MaxThinGB
		if maxThinGB <= 0 {
			maxThinGB = 50
		}
		return maxThinGB, 4
	default:
		return 0, 0
	}
}

func apfsSnapshotEstimateBytes(snapshots []snapshotInfo) (int64, int64) {
	count := int64(len(snapshots))
	return count * 5 * apfsGiB, count * 15 * apfsGiB
}

func apfsThinnableSnapshotEstimateBytes(snapshots []snapshotInfo) (int64, int64) {
	var count int64
	for _, snapshot := range snapshots {
		if apfsSnapshotHasTime(snapshot) && apfsSnapshotKind(snapshot) != "os-update" {
			count++
		}
	}
	return count * 5 * apfsGiB, count * 15 * apfsGiB
}

func apfsPlanTargets(snapshots []snapshotInfo, level CleanupLevel, cfg config.APFSConfig, requestGB int, backupActive bool, sudoPasswordless bool, now time.Time) []CleanupTarget {
	var targets []CleanupTarget
	low, _ := apfsThinnableSnapshotEstimateBytes(snapshots)
	requestBytes := int64(requestGB) * apfsGiB
	thinEstimate := low
	if requestBytes > 0 && requestBytes < thinEstimate {
		thinEstimate = requestBytes
	}

	thinTarget := CleanupTarget{
		Type:      "apfs-local-snapshots",
		Tier:      CleanupTierPrivileged,
		Name:      "local snapshot thinning",
		Bytes:     thinEstimate,
		Protected: thinEstimate == 0 || level < LevelModerate || !cfg.ThinEnabled || !sudoPasswordless || backupActive,
		Action:    "thin_local_snapshots",
		Reason:    "request APFS local snapshot thinning through tmutil",
	}
	if thinTarget.Protected {
		thinTarget.Action = "protect"
		thinTarget.Reason = "snapshot thinning is not currently eligible"
		if thinEstimate == 0 {
			thinTarget.Reason = "no date-form Time Machine local snapshots are eligible for thinning"
		}
	}
	annotateCleanupTargetPolicy(&thinTarget, thinTarget.Tier, hostReclaimForAction(thinTarget.Action))
	targets = append(targets, thinTarget)

	keepDays := cfg.KeepRecentDays
	if keepDays <= 0 {
		keepDays = 1
	}
	cutoff := now.Add(-time.Duration(keepDays) * 24 * time.Hour)
	for i, snapshot := range snapshots {
		target := CleanupTarget{
			Type:      "apfs-snapshot",
			Tier:      CleanupTierPrivileged,
			Name:      apfsSnapshotDisplayName(snapshot),
			Version:   apfsSnapshotKind(snapshot),
			Bytes:     5 * apfsGiB,
			Protected: true,
			Action:    "keep",
			Reason:    "local snapshot is preserved by default",
		}
		switch {
		case apfsSnapshotKind(snapshot) == "os-update":
			target.Reason = "OS-update snapshot is reported for operator review; automatic deletion requires a date-form tmutil deletion token"
		case i == 0:
			target.Reason = "newest local snapshot is always preserved"
		case !apfsSnapshotHasTime(snapshot):
			target.Reason = "snapshot is missing a date-form tmutil deletion token"
		case level == LevelCritical && cfg.DeleteOSUpdates && sudoPasswordless && !backupActive && snapshot.Time.Before(cutoff):
			target.Protected = false
			target.Action = "delete_old_snapshot"
			target.Reason = "critical level may delete old local snapshots after thinning"
		case snapshot.Time.After(cutoff):
			target.Reason = "snapshot is newer than keep_recent_days"
		case !cfg.DeleteOSUpdates:
			target.Reason = "delete_os_updates is disabled"
		}
		annotateCleanupTargetPolicy(&target, target.Tier, hostReclaimForAction(target.Action))
		targets = append(targets, target)
	}
	return targets
}

func apfsEstimatedCandidateBytes(targets []CleanupTarget) int64 {
	var total int64
	for _, target := range targets {
		if target.Protected {
			continue
		}
		total += target.Bytes
	}
	return total
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
	if logger == nil {
		logger = slog.Default()
	}

	cutoff := time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour)

	// Skip the first snapshot (most recent) - NEVER delete it
	for _, snap := range snapshots[1:] {
		deleteToken, ok := apfsSnapshotDeleteToken(snap)
		if !ok {
			logger.Debug("skipping APFS snapshot without date-form deletion token", "snapshot", apfsSnapshotDisplayName(snap), "kind", apfsSnapshotKind(snap))
			continue
		}
		if snap.Time.After(cutoff) {
			continue // Too recent to delete
		}

		logger.Warn("deleting old APFS snapshot", "date", deleteToken)
		output, err := RunWithSudo(ctx, "tmutil", "deletelocalsnapshots", deleteToken)
		if err != nil {
			logger.Debug("failed to delete snapshot", "date", deleteToken, "error", err, "output", string(output))
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
	statusCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(statusCtx, "tmutil", "status")
	output, err := cmd.Output()
	if err != nil {
		return false // Assume not active if we can't check
	}

	outputStr := string(output)
	// Check for "Running = 1" or "BackupPhase" in status output
	return strings.Contains(outputStr, "Running = 1") ||
		strings.Contains(outputStr, "BackupPhase")
}
