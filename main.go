// tinyland-cleanup is a cross-platform disk cleanup daemon.
//
// It monitors disk usage and performs graduated cleanup actions based on
// configurable thresholds. Supports Docker, Nix, Homebrew, Lima, iOS Simulator,
// and various cache cleanup operations.
//
// Usage:
//
//	tinyland-cleanup [flags]
//
// Flags:
//
//	-config string    Path to configuration file (default: ~/.config/tinyland-cleanup/config.yaml)
//	-daemon           Run as a daemon (default: false)
//	-once             Run cleanup once and exit (default: false)
//	-level string     Force cleanup level: none, warning, moderate, aggressive, critical
//	-dry-run          Show what would be cleaned without actually cleaning
//	-output string    Output format: text, json (default: text)
//	-list-plugins     List registered plugin names and exit
//	-plugins string   Comma-separated plugin names to run or plan
//	-target-used-percent int
//	                 Override target maximum used-space percentage after cleanup
//	-verbose          Enable verbose logging
//	-version          Print version and exit
//	-probe-volume-path string    Darwin-only: probe direct volume access and exit
//	-probe-result-path string    Path to write the key=value probe result summary
//	-probe-name string           Probe label used for the temporary write-test file
//	-probe-timeout-seconds int   Timeout per direct probe operation
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
	"github.com/Jesssullivan/tinyland-cleanup/monitor"
	"github.com/Jesssullivan/tinyland-cleanup/plugins"
)

// version mirrors the VERSION file; it is the dev default and is overridden at
// release time by -ldflags "-X main.version=<tag>". CI checks it matches VERSION.
var (
	version = "0.3.0"
	commit  = "dev"
	date    = "unknown"
)

// defaultInodeNoProgressLimit is the number of consecutive cleanup cycles that
// must fail to relieve inode pressure before the daemon stops bypassing
// cooldown for inode-only escalation. Used when policy.inode_no_progress_limit
// is unset.
const defaultInodeNoProgressLimit = 3

func main() {
	// Parse command line flags
	var (
		configPath          = flag.String("config", "", "Path to configuration file")
		runDaemon           = flag.Bool("daemon", false, "Run as a daemon")
		once                = flag.Bool("once", false, "Run cleanup once and exit")
		level               = flag.String("level", "", "Force cleanup level")
		dryRun              = flag.Bool("dry-run", false, "Show what would be cleaned")
		output              = flag.String("output", "text", "Output format: text, json")
		listPlugins         = flag.Bool("list-plugins", false, "List registered plugin names and exit")
		pluginNames         = flag.String("plugins", "", "Comma-separated plugin names to run or plan")
		targetUsed          = flag.Int("target-used-percent", 0, "Override target maximum used-space percentage after cleanup")
		verbose             = flag.Bool("verbose", false, "Enable verbose logging")
		showVersion         = flag.Bool("version", false, "Print version and exit")
		probeVolumePath     = flag.String("probe-volume-path", "", "Darwin-only: probe direct volume access and exit")
		probeResultPath     = flag.String("probe-result-path", "", "Path to write the key=value probe result summary")
		probeName           = flag.String("probe-name", "tinyland-cleanup-probe", "Probe label used for the temporary write-test file")
		probeTimeoutSeconds = flag.Int("probe-timeout-seconds", 5, "Timeout per direct probe operation")

		// Internal child-operation flags used by the direct volume probe mode.
		probeVolumeOp  = flag.String("probe-volume-op", "", "internal volume probe operation")
		probePath      = flag.String("probe-path", "", "internal volume probe path")
		probeFile      = flag.String("probe-file", "", "internal volume probe file path")
		probeErrorPath = flag.String("probe-error-path", "", "internal volume probe error path")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("tinyland-cleanup %s (%s) built %s\n", version, commit, date)
		os.Exit(0)
	}

	if *probeVolumeOp != "" {
		os.Exit(runVolumeProbeChildOperation(*probeVolumeOp, *probePath, *probeFile, *probeErrorPath))
	}

	if *probeVolumePath != "" {
		if err := runVolumeAccessProbe(*probeVolumePath, *probeResultPath, *probeName, *probeTimeoutSeconds); err != nil {
			fmt.Fprintf(os.Stderr, "volume probe failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *output != "text" && *output != "json" {
		fmt.Fprintf(os.Stderr, "invalid output format %q: expected text or json\n", *output)
		os.Exit(2)
	}
	pluginFilter, err := parsePluginFilter(*pluginNames)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	// Load configuration first to get log file path
	if *configPath == "" {
		home, _ := os.UserHomeDir()
		*configPath = filepath.Join(home, ".config", "tinyland-cleanup", "config.yaml")
	}

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		// Fall back to stderr logging if config fails
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}
	if err := applyTargetUsedPercentOverride(cfg, *targetUsed); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	// Create plugin registry and register all plugins.
	registry := plugins.NewRegistry()
	registerPlugins(registry)
	if err := validatePluginFilter(pluginFilter, registry); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	if *listPlugins {
		if err := writePluginList(os.Stdout, *output, listPluginEntries(registry, cfg)); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write plugin list: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Setup logging - write to both stderr and log file
	logLevel := slog.LevelInfo
	if *verbose {
		logLevel = slog.LevelDebug
	}

	logWriters := []io.Writer{os.Stderr}
	var logFile *os.File
	if cfg.LogFile != "" {
		if err := ensureLogDir(cfg.LogFile); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to create log directory for %s: %v; continuing with stderr logging only\n", cfg.LogFile, err)
		} else if file, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to open log file %s: %v; continuing with stderr logging only\n", cfg.LogFile, err)
		} else {
			logFile = file
			defer logFile.Close()
			logWriters = append(logWriters, logFile)
		}
	}

	// Create multi-writer for stderr and the configured log file when available.
	multiWriter := io.MultiWriter(logWriters...)
	logger := slog.New(slog.NewTextHandler(multiWriter, &slog.HandlerOptions{
		Level: logLevel,
	}))

	// Create disk monitor
	diskMon := monitor.NewDiskMonitorWithInodeThresholds(
		cfg.Thresholds.Warning,
		cfg.Thresholds.Moderate,
		cfg.Thresholds.Aggressive,
		cfg.Thresholds.Critical,
		cfg.InodeThresholds.Warning,
		cfg.InodeThresholds.Moderate,
		cfg.InodeThresholds.Aggressive,
		cfg.InodeThresholds.Critical,
	)

	// Create cleanup daemon
	d := &daemon{
		config:       cfg,
		registry:     registry,
		monitor:      diskMon,
		logger:       logger,
		dryRun:       *dryRun,
		output:       *output,
		pluginFilter: pluginFilter,
		report:       os.Stdout,
		diskStats:    monitor.GetDiskStats,
		now:          time.Now,
	}

	// Determine operation mode
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		logger.Info("received shutdown signal")
		cancel()
	}()

	// If level is specified, force that level
	if *level != "" {
		forcedLevel := parseLevel(*level)
		if err := d.runOnce(ctx, forcedLevel); err != nil {
			logger.Error("cleanup failed", "error", err)
			os.Exit(1)
		}
		return
	}

	// Run once or as daemon
	if *once || !*runDaemon {
		if err := d.runOnce(ctx, monitor.LevelNone); err != nil {
			logger.Error("cleanup failed", "error", err)
			os.Exit(1)
		}
		return
	}

	// Run as daemon
	logger.Info("starting cleanup daemon",
		"poll_interval", cfg.PollInterval,
		"warning", cfg.Thresholds.Warning,
		"moderate", cfg.Thresholds.Moderate,
		"aggressive", cfg.Thresholds.Aggressive,
		"critical", cfg.Thresholds.Critical,
		"inode_warning", cfg.InodeThresholds.Warning,
		"inode_moderate", cfg.InodeThresholds.Moderate,
		"inode_aggressive", cfg.InodeThresholds.Aggressive,
		"inode_critical", cfg.InodeThresholds.Critical,
	)

	if err := d.run(ctx); err != nil && err != context.Canceled {
		logger.Error("daemon error", "error", err)
		os.Exit(1)
	}
}

type daemon struct {
	config             *config.Config
	registry           *plugins.Registry
	monitor            *monitor.DiskMonitor
	logger             *slog.Logger
	dryRun             bool
	output             string
	pluginFilter       []string
	report             io.Writer
	diskStats          func(path string) (*monitor.DiskStats, error)
	now                func() time.Time
	runtimeState       *cleanupState
	runtimeStateLoaded bool
	runtimeStateErr    error
}

func (d *daemon) run(ctx context.Context) error {
	// Run immediately on start
	if err := d.runOnce(ctx, monitor.LevelNone); err != nil {
		d.logger.Error("initial cleanup failed", "error", err)
	}

	for {
		// Allocate the next interval only after the prior cycle completes. A long
		// scan therefore cannot leave a pending ticker event that starts another
		// cycle immediately and creates a zero-yield scan storm.
		timer := time.NewTimer(time.Duration(d.config.PollInterval) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			if err := d.runOnce(ctx, monitor.LevelNone); err != nil {
				d.logger.Error("cleanup cycle failed", "error", err)
			}
		}
	}
}

func (d *daemon) runOnce(ctx context.Context, forcedLevel monitor.CleanupLevel) error {
	assessment := d.assessMounts()
	level := forcedLevel

	if level == monitor.LevelNone {
		level = assessment.Level
	}

	now := d.currentTime()
	report := cycleReport{
		Timestamp:     now.UTC().Format(time.RFC3339),
		DryRun:        d.dryRun,
		ForcedLevel:   forcedLevel != monitor.LevelNone,
		Level:         level.String(),
		MonitorPath:   d.primaryMonitorPath(assessment),
		MaxInodeLevel: assessment.InodeLevel.String(),
		Mounts:        assessment.Mounts,
		PluginFilter:  d.pluginFilter,
	}
	// A dry-run is observational. In particular, it must not replace the
	// durable receipt from the last applied cleanup cycle.
	if !d.dryRun {
		report.LatestCycleReceipt = expandPathHome(d.config.Policy.LatestCycleReceipt)
	}

	cooldown := d.cleanupCooldown()
	if cooldown > 0 {
		report.CooldownSeconds = int64(cooldown / time.Second)
	}
	report.MinimumFreeBytes = d.minimumFreeBytes()
	report.StateFile = expandPathHome(d.config.Policy.StateFile)
	state, stateErr := d.loadStateForCycle()
	if stateErr != nil {
		report.StateError = stateErr.Error()
		d.logger.Debug("cleanup state is using in-memory pressure fallback", "path", report.StateFile, "error", stateErr)
	}
	if state != nil {
		report.InodeNoProgressCount = state.inodeNoProgressCount(report.MonitorPath)
	}
	stateDirty := false

	beforeStats, beforeErr := d.getDiskStats(report.MonitorPath)
	if beforeErr != nil {
		report.HostFreeError = beforeErr.Error()
		d.logger.Warn("failed to measure host free space before cleanup", "path", report.MonitorPath, "error", beforeErr)
	} else {
		report.HostFreeBeforeBytes = beforeStats.Free
		report.HostInodesTotal = beforeStats.InodesTotal
		report.HostInodesFreeBefore = beforeStats.InodesFree
		report.HostInodesUsedPercentBefore = beforeStats.InodesUsedPercent
		report.HostInodeLevel = d.inodeLevelForPath(report.MonitorPath, beforeStats).String()
		report.HostByteLevel = d.byteLevelForPath(report.MonitorPath, beforeStats).String()
		d.updateTargetFreeStatus(&report, beforeStats)
	}

	if level == monitor.LevelNone {
		return d.finishCycle(report)
	}

	// Engage the inode-pressure circuit breaker for this cycle when inode-only
	// escalation has repeatedly failed to free inodes, so cooldown is applied
	// instead of bypassed (TIN-2170). Forced runs and configurations without a
	// cooldown always run, so the breaker has no effect there.
	report.InodeBackoff = !report.ForcedLevel && d.cleanupCooldown() > 0 && d.inodeBackoffActive(report)
	if report.InodeBackoff {
		d.logger.Warn("inode pressure unrelieved by recent cleanup cycles; backing off to cooldown cadence",
			"path", report.MonitorPath,
			"inode_level", report.HostInodeLevel,
			"no_progress_count", report.InodeNoProgressCount,
		)
	}

	// Convert monitor level to plugin level
	pluginLevel := plugins.CleanupLevel(level)

	// Run cleanup plugins
	enabledPlugins := filterEnabledPlugins(d.registry.GetEnabled(d.config), d.pluginFilter)
	d.logger.Debug("running plugins", "count", len(enabledPlugins))

	var totalFreed int64
	var totalItems int
	for _, p := range enabledPlugins {
		pluginReport := pluginCycleReport{
			Name:        p.Name(),
			Description: p.Description(),
			Level:       level.String(),
			DryRun:      d.dryRun,
			WouldRun:    true,
		}

		if !d.dryRun && d.cleanupTargetMet(report) {
			pluginReport.WouldRun = false
			pluginReport.SkipReason = "target_free_met"
			if report.StopReason == "" {
				report.StopReason = "target_free_met"
			}
			report.Plugins = append(report.Plugins, pluginReport)
			continue
		}

		if !d.dryRun && !report.ForcedLevel && state != nil {
			if remaining, reason := state.pluginBackoff(p.Name(), now); remaining > 0 {
				pluginReport.WouldRun = false
				pluginReport.SkipReason = reason
				pluginReport.BackoffReason = reason
				pluginReport.ZeroYieldCount = state.zeroYieldCount(p.Name())
				pluginReport.BackoffRemainingSeconds = int64(remaining.Round(time.Second) / time.Second)
				report.Plugins = append(report.Plugins, pluginReport)
				continue
			}
		}

		if d.shouldApplyCooldown(report, level) && state != nil {
			if remaining := state.cooldownRemaining(p.Name(), pluginLevel, now, cooldown); remaining > 0 {
				pluginReport.WouldRun = false
				pluginReport.SkipReason = "cooldown"
				pluginReport.CooldownRemainingSeconds = int64(remaining.Round(time.Second) / time.Second)
				report.Plugins = append(report.Plugins, pluginReport)
				continue
			}
		}

		if d.dryRun {
			if planner, ok := p.(plugins.Planner); ok {
				plan := planner.PlanCleanup(ctx, pluginLevel, d.config, d.logger)
				pluginReport.Plan = &plan
				report.PlannedEstimatedBytesFreed += plan.EstimatedBytesFreed
				report.PlannedTargets += len(plan.Targets)
				if plan.RequiredFreeBytes > report.PlannedRequiredFreeBytes {
					report.PlannedRequiredFreeBytes = plan.RequiredFreeBytes
				}
			}
			pluginReport.SkipReason = "dry_run"
			d.logger.Info("dry-run plugin plan",
				"plugin", p.Name(),
				"level", level.String(),
				"description", p.Description(),
			)
			report.Plugins = append(report.Plugins, pluginReport)
			continue
		}

		result := p.Cleanup(ctx, pluginLevel, d.config, d.logger)
		if result.Outcome == "" {
			switch {
			case result.Error != nil:
				result.Outcome = plugins.CleanupOutcomeRefused
			case result.BytesFreed > 0 || result.ItemsCleaned > 0:
				result.Outcome = plugins.CleanupOutcomeCompleted
			default:
				result.Outcome = plugins.CleanupOutcomeNoOp
			}
		}
		pluginReport.BytesFreed = result.BytesFreed
		pluginReport.EstimatedBytesFreed = result.EstimatedBytesFreed
		pluginReport.CommandBytesFreed = result.CommandBytesFreed
		pluginReport.HostBytesFreed = result.HostBytesFreed
		pluginReport.ItemsCleaned = result.ItemsCleaned
		pluginReport.Warnings = append(pluginReport.Warnings, result.Warnings...)
		pluginReport.Outcome = result.Outcome
		pluginReport.ReceiptDigest = result.ReceiptDigest
		pluginReport.RetryAfterSeconds = result.RetryAfterSeconds
		if result.Error != nil {
			pluginReport.Error = result.Error.Error()
			if state != nil {
				state.recordPluginRunWithPolicy(p.Name(), pluginLevel, d.currentTime(), result, d.zeroYieldLimit(), d.zeroYieldBackoff())
				pluginReport.ZeroYieldCount = state.zeroYieldCount(p.Name())
				stateDirty = true
			}
			report.Plugins = append(report.Plugins, pluginReport)
			d.logger.Debug("plugin failed; persisted cycle alert owns warning cadence", "plugin", p.Name(), "error", result.Error)
			continue
		}

		if state != nil {
			state.recordPluginRunWithPolicy(p.Name(), pluginLevel, d.currentTime(), result, d.zeroYieldLimit(), d.zeroYieldBackoff())
			pluginReport.ZeroYieldCount = state.zeroYieldCount(p.Name())
			pluginReport.BackoffRemainingSeconds = int64(state.zeroYieldBackoffRemaining(p.Name(), d.currentTime()).Round(time.Second) / time.Second)
			_, pluginReport.BackoffReason = state.pluginBackoff(p.Name(), d.currentTime())
			stateDirty = true
		}
		report.Plugins = append(report.Plugins, pluginReport)
		if result.BytesFreed > 0 || result.ItemsCleaned > 0 {
			d.logger.Info("plugin completed",
				"plugin", p.Name(),
				"bytes_freed", result.BytesFreed,
				"items_cleaned", result.ItemsCleaned,
			)
			totalFreed += result.BytesFreed
			totalItems += result.ItemsCleaned
		}

		d.updateHostFreeAfter(&report, beforeStats, beforeErr)
	}

	report.TotalBytesFreed = totalFreed
	report.TotalItemsCleaned = totalItems

	d.updateHostFreeAfter(&report, beforeStats, beforeErr)

	// Record inode reclaim progress for the monitored path so the circuit
	// breaker can detect inode pressure that repeated cycles fail to relieve
	// (TIN-2170). A cycle counts as progress when free inodes increased or inode
	// pressure cleared; otherwise the no-progress counter advances toward the
	// backoff limit.
	if !d.dryRun && state != nil && report.HostInodesTotal > 0 {
		if parseLevel(report.MaxInodeLevel) != monitor.LevelNone {
			improved := report.HostInodesFreeDelta > 0 ||
				parseLevel(report.HostInodeLevel) == monitor.LevelNone
			state.recordInodeProgress(report.MonitorPath, report.HostInodesFreeAfter,
				report.HostInodesUsedPercentAfter, report.HostInodeLevel, now, improved)
			stateDirty = true
		} else if state.inodeNoProgressCount(report.MonitorPath) > 0 {
			// Inode pressure cleared: reset the stale no-progress counter.
			state.recordInodeProgress(report.MonitorPath, report.HostInodesFreeAfter,
				report.HostInodesUsedPercentAfter, report.HostInodeLevel, now, true)
			stateDirty = true
		}
	}

	if !d.dryRun && state != nil {
		if d.recordCycleAlert(&report, state, d.currentTime()) {
			stateDirty = true
		}
	}

	if stateDirty && state != nil {
		if err := saveCleanupState(report.StateFile, state); err != nil {
			report.StateError = err.Error()
			d.rememberRuntimeState(state, err)
			// The alert update remains in memory even when ENOSPC prevents its
			// durable save, so later cycles still suppress the same condition and
			// preserve zero-yield backoff until persistence recovers.
			d.recordCycleAlert(&report, state, d.currentTime())
			d.logger.Debug("failed to save cleanup state; retaining in-memory pressure state", "path", report.StateFile, "error", err)
		} else {
			d.rememberRuntimeState(state, nil)
		}
	}

	d.logger.Info("cleanup cycle host free-space",
		"path", report.MonitorPath,
		"level", report.Level,
		"dry_run", report.DryRun,
		"before_free_gb", bytesToGB(report.HostFreeBeforeBytes),
		"after_free_gb", bytesToGB(report.HostFreeAfterBytes),
		"delta_mb", report.HostFreeDeltaBytes/(1024*1024),
	)

	if !d.dryRun && totalFreed > 0 {
		d.logger.Info("cleanup complete",
			"total_freed_mb", totalFreed/(1024*1024),
		)
	}

	return d.finishCycle(report)
}

type cycleReport struct {
	Timestamp                   string  `json:"timestamp"`
	DryRun                      bool    `json:"dry_run"`
	ForcedLevel                 bool    `json:"forced_level"`
	Level                       string  `json:"level"`
	MonitorPath                 string  `json:"monitor_path"`
	HostFreeBeforeBytes         uint64  `json:"host_free_before_bytes"`
	HostFreeAfterBytes          uint64  `json:"host_free_after_bytes"`
	HostFreeDeltaBytes          int64   `json:"host_free_delta_bytes"`
	HostInodesTotal             uint64  `json:"host_inodes_total,omitempty"`
	HostInodesFreeBefore        uint64  `json:"host_inodes_free_before,omitempty"`
	HostInodesFreeAfter         uint64  `json:"host_inodes_free_after,omitempty"`
	HostInodesFreeDelta         int64   `json:"host_inodes_free_delta,omitempty"`
	HostInodesUsedPercentBefore float64 `json:"host_inodes_used_percent_before,omitempty"`
	HostInodesUsedPercentAfter  float64 `json:"host_inodes_used_percent_after,omitempty"`
	HostInodeLevel              string  `json:"host_inode_level,omitempty"`
	// HostByteLevel is the byte-driven cleanup level for the monitored primary
	// path, used to tell apart inode-only escalation from byte pressure.
	HostByteLevel string `json:"host_byte_level,omitempty"`
	// MaxInodeLevel is the highest inode-driven level across all monitored mounts
	// and gates the cleanup stop condition.
	MaxInodeLevel string `json:"max_inode_level,omitempty"`
	// InodeNoProgressCount is the number of consecutive prior cycles that failed
	// to relieve inode pressure on the primary path.
	InodeNoProgressCount int `json:"inode_no_progress_count,omitempty"`
	// InodeBackoff reports that the inode-pressure circuit breaker engaged this
	// cycle, so the daemon applied cooldown instead of bypassing it.
	InodeBackoff              bool   `json:"inode_backoff,omitempty"`
	HostFreeError             string `json:"host_free_error,omitempty"`
	StateFile                 string `json:"state_file,omitempty"`
	StateError                string `json:"state_error,omitempty"`
	LatestCycleReceipt        string `json:"latest_cycle_receipt,omitempty"`
	ReceiptDigest             string `json:"receipt_digest,omitempty"`
	ReceiptError              string `json:"receipt_error,omitempty"`
	CompletedAt               string `json:"completed_at,omitempty"`
	AlertDigest               string `json:"alert_digest,omitempty"`
	AlertStatus               string `json:"alert_status,omitempty"`
	SuppressedDuplicateAlerts int    `json:"suppressed_duplicate_alerts,omitempty"`
	CooldownSeconds           int64  `json:"cooldown_seconds,omitempty"`
	// TargetUsedPercent is the legacy target_free config value as a maximum used percentage.
	TargetUsedPercent int `json:"target_used_percent"`
	// TargetFreeBytes is the free-space equivalent required to satisfy TargetUsedPercent.
	TargetFreeBytes uint64 `json:"target_free_bytes"`
	// TargetFreeDeficitBytes is the remaining free-space gap to the target.
	TargetFreeDeficitBytes int64 `json:"target_free_deficit_bytes"`
	// TargetFreeMet reports whether the host already satisfies the target.
	TargetFreeMet bool `json:"target_free_met"`
	// MinimumFreeBytes is an absolute free-space runway that bypasses cooldown while unmet.
	MinimumFreeBytes uint64 `json:"minimum_free_bytes,omitempty"`
	// MinimumFreeDeficitBytes is the remaining free-space gap to the absolute runway.
	MinimumFreeDeficitBytes int64 `json:"minimum_free_deficit_bytes,omitempty"`
	// MinimumFreeMet reports whether the host already satisfies the absolute runway.
	MinimumFreeMet bool `json:"minimum_free_met"`
	// StopReason explains why remaining cleanup plugins were skipped.
	StopReason string `json:"stop_reason,omitempty"`
	// PlannedEstimatedBytesFreed aggregates dry-run plugin plan estimates.
	PlannedEstimatedBytesFreed int64 `json:"planned_estimated_bytes_freed,omitempty"`
	// PlannedRequiredFreeBytes is the largest free-space preflight requirement across plugin plans.
	PlannedRequiredFreeBytes int64 `json:"planned_required_free_bytes,omitempty"`
	// PlannedTargets is the total number of dry-run cleanup targets.
	PlannedTargets    int                 `json:"planned_targets,omitempty"`
	TotalBytesFreed   int64               `json:"total_bytes_freed"`
	TotalItemsCleaned int                 `json:"total_items_cleaned"`
	Mounts            []mountReport       `json:"mounts"`
	PluginFilter      []string            `json:"plugin_filter,omitempty"`
	Plugins           []pluginCycleReport `json:"plugins"`
}

type mountReport struct {
	Label             string  `json:"label"`
	Path              string  `json:"path"`
	UsedPercent       float64 `json:"used_percent"`
	FreeGB            float64 `json:"free_gb"`
	FreeBytes         uint64  `json:"free_bytes"`
	ByteLevel         string  `json:"byte_level"`
	InodesTotal       uint64  `json:"inodes_total,omitempty"`
	InodesFree        uint64  `json:"inodes_free,omitempty"`
	InodesUsedPercent float64 `json:"inodes_used_percent,omitempty"`
	InodeLevel        string  `json:"inode_level,omitempty"`
	Level             string  `json:"level"`
	Error             string  `json:"error,omitempty"`
}

type pluginCycleReport struct {
	Name                     string               `json:"name"`
	Description              string               `json:"description"`
	Level                    string               `json:"level"`
	DryRun                   bool                 `json:"dry_run"`
	WouldRun                 bool                 `json:"would_run"`
	SkipReason               string               `json:"skip_reason,omitempty"`
	Plan                     *plugins.CleanupPlan `json:"plan,omitempty"`
	BytesFreed               int64                `json:"bytes_freed"`
	EstimatedBytesFreed      int64                `json:"estimated_bytes_freed"`
	CommandBytesFreed        int64                `json:"command_bytes_freed"`
	HostBytesFreed           int64                `json:"host_bytes_freed"`
	ItemsCleaned             int                  `json:"items_cleaned"`
	CooldownRemainingSeconds int64                `json:"cooldown_remaining_seconds,omitempty"`
	Error                    string               `json:"error,omitempty"`
	Warnings                 []string             `json:"warnings,omitempty"`
	Outcome                  string               `json:"outcome,omitempty"`
	ReceiptDigest            string               `json:"receipt_digest,omitempty"`
	RetryAfterSeconds        int64                `json:"retry_after_seconds,omitempty"`
	ZeroYieldCount           int                  `json:"zero_yield_count,omitempty"`
	BackoffRemainingSeconds  int64                `json:"backoff_remaining_seconds,omitempty"`
	BackoffReason            string               `json:"backoff_reason,omitempty"`
}

const (
	latestCycleReceiptSchema      = "tinyland.cleanup-cycle-receipt.v1"
	latestCycleReceiptMaxBytes    = 256 * 1024
	latestCycleReceiptMaxPlugins  = 24
	latestCycleReceiptMaxWarnings = 2
	latestCycleReceiptStringBytes = 128
)

// latestCycleReceipt is deliberately separate from cycleReport. A cycle
// report may contain arbitrarily large plans and target lists; the durable
// latest-cycle control receipt must remain bounded even when those reports do
// not. ReceiptDigest is SHA-256 over the canonical compact JSON encoding of
// this struct with ReceiptDigest empty.
type latestCycleReceipt struct {
	Schema               string                     `json:"schema"`
	Timestamp            string                     `json:"timestamp"`
	CompletedAt          string                     `json:"completed_at"`
	Level                string                     `json:"level"`
	MonitorPath          string                     `json:"monitor_path"`
	HostFreeBeforeBytes  uint64                     `json:"host_free_before_bytes"`
	HostFreeAfterBytes   uint64                     `json:"host_free_after_bytes"`
	HostFreeDeltaBytes   int64                      `json:"host_free_delta_bytes"`
	HostInodesFreeBefore uint64                     `json:"host_inodes_free_before,omitempty"`
	HostInodesFreeAfter  uint64                     `json:"host_inodes_free_after,omitempty"`
	HostInodesFreeDelta  int64                      `json:"host_inodes_free_delta,omitempty"`
	StateError           string                     `json:"state_error,omitempty"`
	AlertDigest          string                     `json:"alert_digest,omitempty"`
	AlertStatus          string                     `json:"alert_status,omitempty"`
	StopReason           string                     `json:"stop_reason,omitempty"`
	TotalBytesFreed      int64                      `json:"total_bytes_freed"`
	TotalItemsCleaned    int                        `json:"total_items_cleaned"`
	PluginCount          int                        `json:"plugin_count"`
	Plugins              []latestCyclePluginReceipt `json:"plugins"`
	Truncated            bool                       `json:"truncated,omitempty"`
	ReceiptDigest        string                     `json:"receipt_digest"`
}

type latestCyclePluginReceipt struct {
	Name                    string   `json:"name"`
	Level                   string   `json:"level"`
	Outcome                 string   `json:"outcome,omitempty"`
	SkipReason              string   `json:"skip_reason,omitempty"`
	BytesFreed              int64    `json:"bytes_freed"`
	ItemsCleaned            int      `json:"items_cleaned"`
	Error                   string   `json:"error,omitempty"`
	Warnings                []string `json:"warnings,omitempty"`
	ExternalReceiptDigest   string   `json:"external_receipt_digest,omitempty"`
	RetryAfterSeconds       int64    `json:"retry_after_seconds,omitempty"`
	ZeroYieldCount          int      `json:"zero_yield_count,omitempty"`
	BackoffRemainingSeconds int64    `json:"backoff_remaining_seconds,omitempty"`
	BackoffReason           string   `json:"backoff_reason,omitempty"`
}

type pluginListReport struct {
	Plugins []pluginListEntry `json:"plugins"`
}

type pluginListEntry struct {
	Name               string   `json:"name"`
	Description        string   `json:"description"`
	Enabled            bool     `json:"enabled"`
	Supported          bool     `json:"supported"`
	SupportedPlatforms []string `json:"supported_platforms,omitempty"`
}

type mountAssessment struct {
	Level monitor.CleanupLevel
	// InodeLevel is the highest inode-driven cleanup level across all monitored
	// mounts. It is tracked separately from Level so the cleanup stop condition
	// does not declare success while any mount still has inode pressure, even
	// when that mount is not the byte-pressure primary.
	InodeLevel monitor.CleanupLevel
	Mounts     []mountReport
}

// assessMounts monitors all configured mount points and returns the highest
// cleanup level detected across all of them. Falls back to home directory
// monitoring if no mounts are configured.
func (d *daemon) assessMounts() mountAssessment {
	assessment := mountAssessment{Level: monitor.LevelNone}

	if len(d.config.MonitoredMounts) > 0 {
		// Multi-mount monitoring: check each configured mount point
		for _, mount := range d.config.MonitoredMounts {
			stats, err := d.getDiskStats(mount.Path)
			label := mount.Label
			if label == "" {
				label = mount.Path
			}
			if err != nil {
				d.logger.Warn("failed to check mount", "path", mount.Path, "label", mount.Label, "error", err)
				assessment.Mounts = append(assessment.Mounts, mountReport{
					Label: label,
					Path:  mount.Path,
					Level: monitor.LevelNone.String(),
					Error: err.Error(),
				})
				continue
			}

			mountMonitor := d.monitorForMount(mount)
			byteLevel := mountMonitor.CheckByteLevel(stats)
			inodeLevel := mountMonitor.CheckInodeLevel(stats)
			mountLevel := mountMonitor.CheckLevel(stats)
			assessment.Mounts = append(assessment.Mounts, mountReport{
				Label:             label,
				Path:              mount.Path,
				UsedPercent:       stats.UsedPercent,
				FreeGB:            stats.FreeGB,
				FreeBytes:         stats.Free,
				ByteLevel:         byteLevel.String(),
				InodesTotal:       stats.InodesTotal,
				InodesFree:        stats.InodesFree,
				InodesUsedPercent: stats.InodesUsedPercent,
				InodeLevel:        inodeLevelDisplay(inodeLevel, stats.InodesTotal),
				Level:             mountLevel.String(),
			})

			d.logger.Info("disk status",
				"mount", label,
				"path", mount.Path,
				"used_percent", fmt.Sprintf("%.1f%%", stats.UsedPercent),
				"free_gb", fmt.Sprintf("%.1fGB", stats.FreeGB),
				"inodes_used_percent", fmt.Sprintf("%.1f%%", stats.InodesUsedPercent),
				"inodes_free", stats.InodesFree,
				"byte_level", byteLevel.String(),
				"inode_level", inodeLevel.String(),
				"level", mountLevel.String(),
			)

			if mountLevel > assessment.Level {
				assessment.Level = mountLevel
			}
			if inodeLevel > assessment.InodeLevel {
				assessment.InodeLevel = inodeLevel
			}
		}
	} else {
		// Fallback: monitor home directory (original behavior)
		// On macOS, "/" is the sealed system volume, but user data is on /System/Volumes/Data
		// Using $HOME ensures we monitor the volume where data actually lives
		monitorPath := "/"
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			monitorPath = home
		}

		stats, err := d.getDiskStats(monitorPath)
		if err != nil {
			d.logger.Error("failed to check disk", "error", err)
			assessment.Mounts = append(assessment.Mounts, mountReport{
				Label: monitorPath,
				Path:  monitorPath,
				Level: monitor.LevelNone.String(),
				Error: err.Error(),
			})
			return assessment
		}
		detectedLevel := d.monitor.CheckLevel(stats)
		byteLevel := d.monitor.CheckByteLevel(stats)
		inodeLevel := d.monitor.CheckInodeLevel(stats)

		assessment.Mounts = append(assessment.Mounts, mountReport{
			Label:             monitorPath,
			Path:              monitorPath,
			UsedPercent:       stats.UsedPercent,
			FreeGB:            stats.FreeGB,
			FreeBytes:         stats.Free,
			ByteLevel:         byteLevel.String(),
			InodesTotal:       stats.InodesTotal,
			InodesFree:        stats.InodesFree,
			InodesUsedPercent: stats.InodesUsedPercent,
			InodeLevel:        inodeLevelDisplay(inodeLevel, stats.InodesTotal),
			Level:             detectedLevel.String(),
		})

		d.logger.Info("disk status",
			"used_percent", fmt.Sprintf("%.1f%%", stats.UsedPercent),
			"free_gb", fmt.Sprintf("%.1fGB", stats.FreeGB),
			"inodes_used_percent", fmt.Sprintf("%.1f%%", stats.InodesUsedPercent),
			"inodes_free", stats.InodesFree,
			"byte_level", byteLevel.String(),
			"inode_level", inodeLevel.String(),
			"level", detectedLevel.String(),
		)

		assessment.Level = detectedLevel
		assessment.InodeLevel = inodeLevel
	}

	return assessment
}

// inodeLevelDisplay returns the inode cleanup level as a string, or "" when the
// filesystem does not report a usable inode count, so callers can distinguish
// "no inode pressure" (none) from "inodes not measured" (empty).
func inodeLevelDisplay(level monitor.CleanupLevel, inodesTotal uint64) string {
	if inodesTotal == 0 {
		return ""
	}
	return level.String()
}

func (d *daemon) checkMounts() monitor.CleanupLevel {
	return d.assessMounts().Level
}

func (d *daemon) monitorForMount(mount config.MountConfig) *monitor.DiskMonitor {
	if mount.ThresholdWarning <= 0 &&
		mount.ThresholdCritical <= 0 &&
		mount.ThresholdInodeWarning <= 0 &&
		mount.ThresholdInodeCritical <= 0 {
		return d.monitor
	}

	warning := d.config.Thresholds.Warning
	moderate := d.config.Thresholds.Moderate
	aggressive := d.config.Thresholds.Aggressive
	critical := d.config.Thresholds.Critical
	if mount.ThresholdWarning > 0 {
		warning = mount.ThresholdWarning
	}
	if mount.ThresholdCritical > 0 {
		critical = mount.ThresholdCritical
	}

	inodeWarning := d.config.InodeThresholds.Warning
	inodeModerate := d.config.InodeThresholds.Moderate
	inodeAggressive := d.config.InodeThresholds.Aggressive
	inodeCritical := d.config.InodeThresholds.Critical
	if mount.ThresholdInodeWarning > 0 {
		inodeWarning = mount.ThresholdInodeWarning
	}
	if mount.ThresholdInodeCritical > 0 {
		inodeCritical = mount.ThresholdInodeCritical
	}

	return monitor.NewDiskMonitorWithInodeThresholds(
		warning,
		moderate,
		aggressive,
		critical,
		inodeWarning,
		inodeModerate,
		inodeAggressive,
		inodeCritical,
	)
}

func (d *daemon) inodeLevelForPath(path string, stats *monitor.DiskStats) monitor.CleanupLevel {
	if d.config != nil {
		for _, mount := range d.config.MonitoredMounts {
			if mount.Path == path {
				return d.monitorForMount(mount).CheckInodeLevel(stats)
			}
		}
	}
	return d.monitor.CheckInodeLevel(stats)
}

func (d *daemon) byteLevelForPath(path string, stats *monitor.DiskStats) monitor.CleanupLevel {
	if d.config != nil {
		for _, mount := range d.config.MonitoredMounts {
			if mount.Path == path {
				return d.monitorForMount(mount).CheckByteLevel(stats)
			}
		}
	}
	return d.monitor.CheckByteLevel(stats)
}

func (d *daemon) primaryMonitorPath(assessment mountAssessment) string {
	for _, mount := range assessment.Mounts {
		if mount.Error == "" && mount.Path != "" && mount.Level == assessment.Level.String() {
			return mount.Path
		}
	}
	for _, mount := range assessment.Mounts {
		if mount.Error == "" && mount.Path != "" {
			return mount.Path
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	return "/"
}

func (d *daemon) writeReport(report cycleReport) error {
	if d.output == "json" {
		encoder := json.NewEncoder(d.report)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	if d.output == "text" {
		return writeTextReport(d.report, report)
	}
	return nil
}

func (d *daemon) finishCycle(report cycleReport) error {
	report.CompletedAt = d.currentTime().UTC().Format(time.RFC3339)
	var receiptErr error
	if !report.DryRun && report.LatestCycleReceipt != "" {
		receipt := boundedLatestCycleReceipt(report)
		data, digest, err := marshalLatestCycleReceipt(receipt)
		if err == nil {
			err = writeAtomicFile(report.LatestCycleReceipt, data, 0600)
		}
		if err != nil {
			receiptErr = err
			report.ReceiptError = err.Error()
		} else {
			report.ReceiptDigest = digest
		}
	}
	reportErr := d.writeReport(report)
	if receiptErr != nil {
		return fmt.Errorf("write latest cycle receipt: %w", receiptErr)
	}
	return reportErr
}

func boundedLatestCycleReceipt(report cycleReport) latestCycleReceipt {
	receipt := latestCycleReceipt{
		Schema:               latestCycleReceiptSchema,
		Timestamp:            boundedReceiptString(report.Timestamp),
		CompletedAt:          boundedReceiptString(report.CompletedAt),
		Level:                boundedReceiptString(report.Level),
		MonitorPath:          boundedReceiptString(report.MonitorPath),
		HostFreeBeforeBytes:  report.HostFreeBeforeBytes,
		HostFreeAfterBytes:   report.HostFreeAfterBytes,
		HostFreeDeltaBytes:   report.HostFreeDeltaBytes,
		HostInodesFreeBefore: report.HostInodesFreeBefore,
		HostInodesFreeAfter:  report.HostInodesFreeAfter,
		HostInodesFreeDelta:  report.HostInodesFreeDelta,
		StateError:           boundedReceiptString(report.StateError),
		AlertDigest:          boundedReceiptString(report.AlertDigest),
		AlertStatus:          boundedReceiptString(report.AlertStatus),
		StopReason:           boundedReceiptString(report.StopReason),
		TotalBytesFreed:      report.TotalBytesFreed,
		TotalItemsCleaned:    report.TotalItemsCleaned,
		PluginCount:          len(report.Plugins),
	}
	for _, value := range []string{
		report.Timestamp,
		report.CompletedAt,
		report.Level,
		report.MonitorPath,
		report.StateError,
		report.AlertDigest,
		report.AlertStatus,
		report.StopReason,
	} {
		if len(value) > latestCycleReceiptStringBytes {
			receipt.Truncated = true
		}
	}
	pluginLimit := len(report.Plugins)
	if pluginLimit > latestCycleReceiptMaxPlugins {
		pluginLimit = latestCycleReceiptMaxPlugins
		receipt.Truncated = true
	}
	receipt.Plugins = make([]latestCyclePluginReceipt, 0, pluginLimit)
	for _, plugin := range report.Plugins[:pluginLimit] {
		for _, value := range []string{
			plugin.Name,
			plugin.Level,
			plugin.Outcome,
			plugin.SkipReason,
			plugin.Error,
			plugin.ReceiptDigest,
			plugin.BackoffReason,
		} {
			if len(value) > latestCycleReceiptStringBytes {
				receipt.Truncated = true
			}
		}
		warnings := plugin.Warnings
		if len(warnings) > latestCycleReceiptMaxWarnings {
			warnings = warnings[:latestCycleReceiptMaxWarnings]
			receipt.Truncated = true
		}
		boundedWarnings := make([]string, 0, len(warnings))
		for _, warning := range warnings {
			if len(warning) > latestCycleReceiptStringBytes {
				receipt.Truncated = true
			}
			boundedWarnings = append(boundedWarnings, boundedReceiptString(warning))
		}
		receipt.Plugins = append(receipt.Plugins, latestCyclePluginReceipt{
			Name:                    boundedReceiptString(plugin.Name),
			Level:                   boundedReceiptString(plugin.Level),
			Outcome:                 boundedReceiptString(plugin.Outcome),
			SkipReason:              boundedReceiptString(plugin.SkipReason),
			BytesFreed:              plugin.BytesFreed,
			ItemsCleaned:            plugin.ItemsCleaned,
			Error:                   boundedReceiptString(plugin.Error),
			Warnings:                boundedWarnings,
			ExternalReceiptDigest:   boundedReceiptString(plugin.ReceiptDigest),
			RetryAfterSeconds:       plugin.RetryAfterSeconds,
			ZeroYieldCount:          plugin.ZeroYieldCount,
			BackoffRemainingSeconds: plugin.BackoffRemainingSeconds,
			BackoffReason:           boundedReceiptString(plugin.BackoffReason),
		})
	}
	return receipt
}

func boundedReceiptString(value string) string {
	if len(value) <= latestCycleReceiptStringBytes {
		return value
	}
	return strings.ToValidUTF8(value[:latestCycleReceiptStringBytes], "\ufffd")
}

func marshalLatestCycleReceipt(receipt latestCycleReceipt) ([]byte, string, error) {
	digest := latestCycleReceiptDigest(receipt)
	receipt.ReceiptDigest = digest
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return nil, "", err
	}
	data = append(data, '\n')
	if len(data) > latestCycleReceiptMaxBytes {
		return nil, "", fmt.Errorf("latest cycle receipt exceeds %d-byte limit", latestCycleReceiptMaxBytes)
	}
	return data, digest, nil
}

func latestCycleReceiptDigest(receipt latestCycleReceipt) string {
	receipt.ReceiptDigest = ""
	data, _ := json.Marshal(receipt)
	digest := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", digest[:])
}

func (d *daemon) getDiskStats(path string) (*monitor.DiskStats, error) {
	if d.diskStats != nil {
		return d.diskStats(path)
	}
	return monitor.GetDiskStats(path)
}

func (d *daemon) currentTime() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

func (d *daemon) cleanupCooldown() time.Duration {
	if d.config == nil || d.config.Policy.Cooldown == "" {
		return 0
	}
	duration, err := time.ParseDuration(d.config.Policy.Cooldown)
	if err != nil || duration < 0 {
		return 0
	}
	return duration
}

func (d *daemon) zeroYieldLimit() int {
	if d.config == nil || d.config.Policy.ZeroYieldLimit <= 0 {
		return 3
	}
	return d.config.Policy.ZeroYieldLimit
}

func (d *daemon) zeroYieldBackoff() time.Duration {
	if d.config == nil || d.config.Policy.ZeroYieldBackoff == "" {
		return 30 * time.Minute
	}
	duration, err := time.ParseDuration(d.config.Policy.ZeroYieldBackoff)
	if err != nil || duration < 0 {
		return 30 * time.Minute
	}
	return duration
}

func (d *daemon) alertRepeatInterval() time.Duration {
	if d.config == nil || d.config.Policy.AlertRepeatInterval == "" {
		return time.Hour
	}
	duration, err := time.ParseDuration(d.config.Policy.AlertRepeatInterval)
	if err != nil || duration < 0 {
		return time.Hour
	}
	return duration
}

func (d *daemon) recordCycleAlert(report *cycleReport, state *cleanupState, now time.Time) bool {
	if report == nil || state == nil {
		return false
	}
	parts := make([]string, 0, len(report.Plugins)+2)
	if report.StateError != "" {
		parts = append(parts, "state:"+report.StateError)
	}
	if report.HostFreeError != "" {
		parts = append(parts, "host-free:"+report.HostFreeError)
	}
	for _, plugin := range report.Plugins {
		switch {
		case plugin.Error != "":
			parts = append(parts, plugin.Name+":error:"+plugin.Error)
		case plugin.BackoffRemainingSeconds > 0:
			parts = append(parts, plugin.Name+":"+plugin.BackoffReason)
		case plugin.Outcome == plugins.CleanupOutcomeRefused:
			parts = append(parts, plugin.Name+":refused")
		}
	}
	if len(parts) == 0 {
		return false
	}
	sort.Strings(parts)
	digest := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	signature := fmt.Sprintf("sha256:%x", digest[:])
	emit, suppressed := state.recordAlert(signature, now, d.alertRepeatInterval())
	report.AlertDigest = signature
	report.SuppressedDuplicateAlerts = suppressed
	if emit {
		report.AlertStatus = "emitted"
		d.logger.Warn("cleanup cycle alert", "digest", signature, "conditions", strings.Join(parts, "; "))
	} else {
		report.AlertStatus = "suppressed_duplicate"
	}
	return true
}

func (d *daemon) loadStateForCycle() (*cleanupState, error) {
	if d.dryRun || d.config == nil {
		return newCleanupState(), nil
	}
	if d.runtimeStateLoaded {
		return d.runtimeState, d.runtimeStateErr
	}
	state, err := loadCleanupState(expandPathHome(d.config.Policy.StateFile))
	if err != nil {
		// A load failure may mean corrupt or inaccessible authority. Do not
		// manufacture replacement control state. The in-memory fallback begins
		// only after a successfully loaded state later fails to persist.
		return nil, err
	}
	d.rememberRuntimeState(state, err)
	return state, err
}

func (d *daemon) rememberRuntimeState(state *cleanupState, err error) {
	if d == nil || state == nil {
		return
	}
	d.runtimeState = state
	d.runtimeStateLoaded = true
	d.runtimeStateErr = err
}

func (d *daemon) shouldApplyCooldown(report cycleReport, level monitor.CleanupLevel) bool {
	if report.MinimumFreeBytes > 0 && !report.MinimumFreeMet {
		return false
	}
	if d.dryRun || report.ForcedLevel || d.cleanupCooldown() <= 0 {
		return false
	}
	if level >= d.cooldownBypassLevel() {
		// An escalation at/above the bypass level normally ignores cooldown.
		// Circuit breaker: when that escalation is driven solely by inode
		// pressure that recent cycles have repeatedly failed to relieve, resume
		// applying cooldown so the daemon backs off to the cooldown cadence
		// instead of churning every poll interval (TIN-2170).
		return report.InodeBackoff
	}
	return true
}

// inodeBackoffActive reports whether the circuit breaker should engage: the
// monitor-path escalation is forced by inode pressure (not bytes) at/above the
// bypass level, and the configured number of consecutive cleanup cycles have
// failed to free inodes on that path.
func (d *daemon) inodeBackoffActive(report cycleReport) bool {
	bypass := d.cooldownBypassLevel()
	if parseLevel(report.HostInodeLevel) < bypass {
		return false
	}
	if parseLevel(report.HostByteLevel) >= bypass {
		// Bytes are independently at/above the bypass level, so cleanup is
		// expected to make byte progress; do not back off.
		return false
	}
	return report.InodeNoProgressCount >= d.inodeNoProgressLimit()
}

func (d *daemon) inodeNoProgressLimit() int {
	if d.config != nil && d.config.Policy.InodeNoProgressLimit > 0 {
		return d.config.Policy.InodeNoProgressLimit
	}
	return defaultInodeNoProgressLimit
}

func (d *daemon) cooldownBypassLevel() monitor.CleanupLevel {
	if d.config == nil {
		return monitor.LevelCritical
	}
	switch strings.TrimSpace(strings.ToLower(d.config.Policy.CooldownBypassLevel)) {
	case "warning":
		return monitor.LevelWarning
	case "moderate":
		return monitor.LevelModerate
	case "aggressive":
		return monitor.LevelAggressive
	case "critical", "":
		return monitor.LevelCritical
	default:
		d.logger.Warn("invalid policy.cooldown_bypass_level, defaulting to critical", "value", d.config.Policy.CooldownBypassLevel)
		return monitor.LevelCritical
	}
}

func (d *daemon) updateHostFreeAfter(report *cycleReport, beforeStats *monitor.DiskStats, beforeErr error) {
	afterStats, afterErr := d.getDiskStats(report.MonitorPath)
	if afterErr != nil {
		report.HostFreeError = afterErr.Error()
		d.logger.Warn("failed to measure host free space after cleanup", "path", report.MonitorPath, "error", afterErr)
		return
	}

	report.HostFreeAfterBytes = afterStats.Free
	report.HostInodesTotal = afterStats.InodesTotal
	report.HostInodesFreeAfter = afterStats.InodesFree
	report.HostInodesUsedPercentAfter = afterStats.InodesUsedPercent
	report.HostInodeLevel = d.inodeLevelForPath(report.MonitorPath, afterStats).String()
	if beforeErr == nil && beforeStats != nil {
		report.HostFreeDeltaBytes = int64(afterStats.Free) - int64(beforeStats.Free)
		report.HostInodesFreeDelta = int64(afterStats.InodesFree) - int64(beforeStats.InodesFree)
	}
	d.updateTargetFreeStatus(report, afterStats)
}

func (d *daemon) cleanupTargetMet(report cycleReport) bool {
	// Do not declare the cleanup target met while any monitored mount still has
	// inode pressure, even if it is not the byte-pressure primary path. The byte
	// target alone is insufficient because an inode-exhausted filesystem can have
	// ample free bytes (the honey nix-store crunch, TIN-2165/TIN-2170).
	return report.TargetFreeMet && parseLevel(report.MaxInodeLevel) == monitor.LevelNone
}

func (d *daemon) updateTargetFreeStatus(report *cycleReport, stats *monitor.DiskStats) {
	if report.MinimumFreeBytes > 0 {
		if stats.Free >= report.MinimumFreeBytes {
			report.MinimumFreeDeficitBytes = 0
			report.MinimumFreeMet = true
		} else {
			report.MinimumFreeDeficitBytes = int64(report.MinimumFreeBytes - stats.Free)
			report.MinimumFreeMet = false
		}
	}

	targetFreeBytes, ok := targetFreeBytes(stats.Total, d.config.TargetFree)
	if !ok {
		return
	}

	report.TargetUsedPercent = d.config.TargetFree
	report.TargetFreeBytes = targetFreeBytes
	if stats.Free >= targetFreeBytes {
		report.TargetFreeDeficitBytes = 0
		report.TargetFreeMet = true
		return
	}

	report.TargetFreeDeficitBytes = int64(targetFreeBytes - stats.Free)
	report.TargetFreeMet = false
}

func (d *daemon) minimumFreeBytes() uint64 {
	if d.config == nil || d.config.Policy.MinimumFreeGB <= 0 {
		return 0
	}
	return uint64(d.config.Policy.MinimumFreeGB) * 1024 * 1024 * 1024
}

func targetFreeBytes(totalBytes uint64, targetUsedPercent int) (uint64, bool) {
	if totalBytes == 0 || targetUsedPercent <= 0 || targetUsedPercent >= 100 {
		return 0, false
	}

	freePercent := 100 - targetUsedPercent
	return totalBytes * uint64(freePercent) / 100, true
}

func applyTargetUsedPercentOverride(cfg *config.Config, targetUsedPercent int) error {
	if targetUsedPercent == 0 {
		return nil
	}
	if targetUsedPercent <= 0 || targetUsedPercent >= 100 {
		return fmt.Errorf("invalid target-used-percent %d: expected 1-99", targetUsedPercent)
	}
	cfg.TargetFree = targetUsedPercent
	return nil
}

func parsePluginFilter(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	seen := make(map[string]struct{})
	var filter []string
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, fmt.Errorf("invalid plugins filter %q: empty plugin name", raw)
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		filter = append(filter, name)
	}
	return filter, nil
}

func validatePluginFilter(filter []string, registry *plugins.Registry) error {
	if len(filter) == 0 {
		return nil
	}

	available := make(map[string]struct{})
	for _, name := range availablePluginNames(registry) {
		available[name] = struct{}{}
	}
	for _, name := range filter {
		if _, ok := available[name]; !ok {
			return fmt.Errorf("unknown plugin %q; available plugins: %s", name, strings.Join(availablePluginNames(registry), ", "))
		}
	}
	return nil
}

func availablePluginNames(registry *plugins.Registry) []string {
	seen := make(map[string]struct{})
	for _, plugin := range registry.GetAll() {
		seen[plugin.Name()] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func filterEnabledPlugins(enabled []plugins.Plugin, filter []string) []plugins.Plugin {
	if len(filter) == 0 {
		return enabled
	}

	allowed := make(map[string]struct{}, len(filter))
	for _, name := range filter {
		allowed[name] = struct{}{}
	}
	filtered := make([]plugins.Plugin, 0, len(enabled))
	for _, plugin := range enabled {
		if _, ok := allowed[plugin.Name()]; ok {
			filtered = append(filtered, plugin)
		}
	}
	return filtered
}

func listPluginEntries(registry *plugins.Registry, cfg *config.Config) []pluginListEntry {
	registered := registry.GetAll()
	entries := make([]pluginListEntry, 0, len(registered))
	for _, plugin := range registered {
		supportedPlatforms := plugin.SupportedPlatforms()
		entries = append(entries, pluginListEntry{
			Name:               plugin.Name(),
			Description:        plugin.Description(),
			Enabled:            plugin.Enabled(cfg),
			Supported:          pluginSupportedOnCurrentPlatform(supportedPlatforms),
			SupportedPlatforms: supportedPlatforms,
		})
	}
	return entries
}

func pluginSupportedOnCurrentPlatform(supportedPlatforms []string) bool {
	if len(supportedPlatforms) == 0 {
		return true
	}
	for _, platform := range supportedPlatforms {
		if platform == runtime.GOOS {
			return true
		}
	}
	return false
}

func writePluginList(w io.Writer, output string, entries []pluginListEntry) error {
	if output == "json" {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(pluginListReport{Plugins: entries})
	}

	if _, err := fmt.Fprintln(w, "tinyland-cleanup plugins"); err != nil {
		return err
	}
	for _, entry := range entries {
		enabled := "disabled"
		if entry.Enabled {
			enabled = "enabled"
		}
		supported := "unsupported"
		if entry.Supported {
			supported = "supported"
		}
		if len(entry.SupportedPlatforms) > 0 {
			supported += " on " + strings.Join(entry.SupportedPlatforms, ",")
		}
		if _, err := fmt.Fprintf(w, "- %s: %s, %s - %s\n", entry.Name, enabled, supported, entry.Description); err != nil {
			return err
		}
	}
	return nil
}

func expandPathHome(path string) string {
	if path == "" {
		return ""
	}
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func bytesToGB(bytes uint64) string {
	return fmt.Sprintf("%.1f", float64(bytes)/(1024*1024*1024))
}

func registerPlugins(registry *plugins.Registry) {
	// Core plugins (all platforms)
	registry.Register(plugins.NewDockerPlugin())
	registry.Register(plugins.NewPodmanPlugin())
	registry.Register(plugins.NewNixPlugin())
	registry.Register(plugins.NewBazelPlugin())
	registry.Register(plugins.NewCachePlugin())
	registry.Register(plugins.NewGitLabRunnerPlugin())

	// Development artifact cleanup (all platforms)
	registry.Register(plugins.NewDevArtifactsPlugin())

	// Kubernetes plugins (disabled by default, for future use)
	registry.Register(plugins.NewEtcdPlugin())
	registry.Register(plugins.NewRKE2Plugin())

	// Platform-specific plugins
	registerLinuxPlugins(registry)
	registerDarwinPlugins(registry)
}

func parseLevel(s string) monitor.CleanupLevel {
	switch s {
	case "warning":
		return monitor.LevelWarning
	case "moderate":
		return monitor.LevelModerate
	case "aggressive":
		return monitor.LevelAggressive
	case "critical":
		return monitor.LevelCritical
	default:
		return monitor.LevelNone
	}
}

func ensureLogDir(logFile string) error {
	dir := filepath.Dir(logFile)
	return os.MkdirAll(dir, 0755)
}
