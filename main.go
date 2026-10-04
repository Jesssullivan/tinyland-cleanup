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
	"encoding/hex"
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
	version = "0.4.1"
	commit  = "dev"
	date    = "unknown"
)

// defaultInodeNoProgressLimit is the number of consecutive cleanup cycles that
// must fail to relieve inode pressure before the daemon stops bypassing
// cooldown for inode-only escalation. Used when policy.inode_no_progress_limit
// is unset.
const defaultInodeNoProgressLimit = 3

// Byte backoff defaults (TIN-3342), used when the matching policy key is
// unset or invalid.
const (
	defaultByteNoProgressLimit = 3
	defaultByteProgressMinMB   = 256
	defaultByteBackoffMax      = 30 * time.Minute
)

// Zero-yield suppression defaults (TIN-3342), used when the matching policy
// key is unset or invalid.
const (
	defaultZeroYieldLimit       = 2
	defaultZeroYieldBackoffBase = 30 * time.Minute
	defaultZeroYieldBackoffMax  = 6 * time.Hour
)

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
	diskMon.InodeFreeFloor = cfg.InodeFreeFloor

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
		after:        time.After,
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
	config       *config.Config
	registry     *plugins.Registry
	monitor      *monitor.DiskMonitor
	logger       *slog.Logger
	dryRun       bool
	output       string
	pluginFilter []string
	report       io.Writer
	diskStats    func(path string) (*monitor.DiskStats, error)
	now          func() time.Time
	// after is the scheduler's clock seam. It defaults to time.After; tests
	// inject a fake so the wait between cycles can be observed and driven.
	after func(time.Duration) <-chan time.Time
}

// run is the daemon loop. Each pass runs one cleanup cycle and then waits a
// full poll interval measured from that cycle's completion, so a cycle that
// outlasts the interval is never followed by an immediate catch-up cycle the
// way a time.Ticker's pending tick would cause (TIN-3342).
func (d *daemon) run(ctx context.Context) error {
	interval := d.pollInterval()
	overrunning := false
	first := true

	for {
		report, err := d.runCycle(ctx, monitor.LevelNone)
		if err != nil {
			if first {
				d.logger.Error("initial cleanup failed", "error", err)
			} else {
				d.logger.Error("cleanup cycle failed", "error", err)
			}
		} else {
			nextCycleAt := d.currentTime().Add(interval)
			report.NextCycleAt = nextCycleAt.UTC().Format(time.RFC3339)
			duration := time.Duration(report.CycleDurationMs) * time.Millisecond
			if duration > interval {
				// Log once per overrun streak, not every overrunning cycle.
				if !overrunning {
					d.logger.Warn("cleanup cycle outlasted the poll interval; next cycle is scheduled from completion",
						"cycle_duration", duration.Round(time.Millisecond).String(),
						"poll_interval", interval.String(),
						"next_cycle_at", report.NextCycleAt,
					)
				}
				overrunning = true
			} else {
				overrunning = false
			}
			if werr := d.writeReport(report); werr != nil {
				d.logger.Error("cleanup cycle failed", "error", werr)
			}
		}
		first = false

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.wait(interval):
		}
	}
}

// pollInterval returns the configured poll interval, never less than one
// second, so a zero or negative value cannot spin the loop.
func (d *daemon) pollInterval() time.Duration {
	interval := time.Second
	if d.config != nil && d.config.PollInterval > 0 {
		interval = time.Duration(d.config.PollInterval) * time.Second
	}
	return interval
}

func (d *daemon) wait(interval time.Duration) <-chan time.Time {
	if d.after != nil {
		return d.after(interval)
	}
	return time.After(interval)
}

// runOnce runs one cleanup cycle and writes its report. It is the entry point
// for --once, --level and the tests; the daemon loop calls runCycle directly so
// it can add scheduling fields before the report is written.
func (d *daemon) runOnce(ctx context.Context, forcedLevel monitor.CleanupLevel) error {
	report, err := d.runCycle(ctx, forcedLevel)
	if err != nil {
		return err
	}
	return d.writeReport(report)
}

// runCycle assesses the monitored mounts, runs every eligible plugin, updates
// persisted state, and returns the cycle report without writing it.
func (d *daemon) runCycle(ctx context.Context, forcedLevel monitor.CleanupLevel) (report cycleReport, err error) {
	now := d.currentTime()
	defer func() {
		elapsed := d.currentTime().Sub(now)
		if elapsed < 0 {
			elapsed = 0
		}
		report.CycleDurationMs = elapsed.Milliseconds()
	}()

	assessment := d.assessMounts()
	level := forcedLevel

	if level == monitor.LevelNone {
		level = assessment.Level
	}

	report = cycleReport{
		Timestamp:     now.UTC().Format(time.RFC3339),
		DryRun:        d.dryRun,
		ForcedLevel:   forcedLevel != monitor.LevelNone,
		Level:         level.String(),
		MonitorPath:   d.primaryMonitorPath(assessment),
		MaxInodeLevel: assessment.InodeLevel.String(),
		Mounts:        assessment.Mounts,
		PluginFilter:  d.pluginFilter,
	}

	cooldown := d.cleanupCooldown()
	if cooldown > 0 {
		report.CooldownSeconds = int64(cooldown / time.Second)
	}
	report.MinimumFreeBytes = d.minimumFreeBytes()
	report.EmergencyFreeBytes = d.emergencyFreeBytes()
	report.StateFile = expandPathHome(d.config.Policy.StateFile)
	state, quarantined, stateErr := d.loadStateForCycle(now)
	if stateErr != nil {
		report.StateError = stateErr.Error()
		d.logger.Warn("failed to load cleanup state", "path", report.StateFile, "error", stateErr)
	}
	if quarantined != "" {
		report.StateQuarantined = quarantined
		d.logger.Warn("cleanup state was unreadable; quarantined it and continuing with fresh state",
			"path", report.StateFile,
			"quarantined_to", quarantined,
		)
	}
	if stateErr == nil {
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
		if stateErr == nil {
			report.ByteNoProgressCount = state.byteNoProgressCount(report.MonitorPath, int(parseLevel(report.HostByteLevel)))
		}
	}
	byteBackoffWasEngaged := stateErr == nil && state.byteBackoffEngaged(report.MonitorPath)

	if level == monitor.LevelNone {
		// Pressure cleared: release byte backoff and reset its counter so a
		// later episode starts from a clean slate, and lift zero-yield
		// suppression so each plugin gets one fresh run in the next episode.
		dirty := false
		if !d.dryRun && stateErr == nil && beforeErr == nil &&
			(byteBackoffWasEngaged || state.byteNoProgressPending(report.MonitorPath)) {
			state.recordByteProgress(report.MonitorPath, report.HostFreeBeforeBytes, report.HostByteLevel,
				int(monitor.LevelNone), now, true, false, false)
			if byteBackoffWasEngaged {
				d.logger.Info("byte backoff released", "path", report.MonitorPath, "reason", "pressure_cleared")
			}
			dirty = true
		}
		if !d.dryRun && stateErr == nil && state.liftZeroYieldSuppression() {
			d.logger.Info("zero-yield suppression lifted", "reason", "pressure_cleared")
			dirty = true
		}
		if dirty {
			if err := saveCleanupState(report.StateFile, state); err != nil {
				report.StateError = err.Error()
				d.logger.Warn("failed to save cleanup state", "path", report.StateFile, "error", err)
			}
		}
		return report, nil
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

	// Engage byte backoff when byte pressure has survived the configured number
	// of cleanup cycles without meaningful reclaim (TIN-3342). While engaged,
	// plugins stop bypassing cooldown and run at most once per backoff
	// interval, capped at policy.byte_backoff_max. It never engages below the
	// emergency free-space floor, for forced or dry runs, or right after the
	// byte level rises.
	if !report.ForcedLevel && !d.dryRun && stateErr == nil {
		report.ByteBackoff, report.ByteBackoffReason = d.byteBackoffActive(report)
	}
	if report.ByteBackoff {
		interval := d.byteBackoffInterval()
		report.ByteBackoffSeconds = int64(interval / time.Second)
		attrs := []any{
			"path", report.MonitorPath,
			"reason", report.ByteBackoffReason,
			"byte_level", report.HostByteLevel,
			"no_progress_count", report.ByteNoProgressCount,
			"interval", interval.String(),
			"free_bytes", report.HostFreeBeforeBytes,
		}
		if byteBackoffWasEngaged {
			d.logger.Debug("byte backoff engaged", attrs...)
		} else {
			d.logger.Warn("byte pressure unrelieved by recent cleanup cycles; engaging byte backoff", attrs...)
		}
	} else if byteBackoffWasEngaged && !report.ForcedLevel && !d.dryRun {
		reason := report.ByteBackoffReason
		if reason == "" {
			reason = "progress_or_escalation"
		}
		d.logger.Info("byte backoff released", "path", report.MonitorPath, "reason", reason)
	}

	// Convert monitor level to plugin level
	pluginLevel := plugins.CleanupLevel(level)

	// Run cleanup plugins
	enabledPlugins := filterEnabledPlugins(d.registry.GetEnabled(d.config), d.pluginFilter)
	d.logger.Debug("running plugins", "count", len(enabledPlugins))

	// Zero-yield suppression (TIN-3342): a plugin whose recent runs reclaimed
	// nothing is skipped until its retry time unless conditions changed.
	zeroYield := d.zeroYieldPolicy()
	digest := d.configDigest()
	minYield := d.byteProgressMinBytes()
	recordRun := func(p plugins.Plugin, pluginReport *pluginCycleReport, result plugins.CleanupResult) {
		state.recordPluginRun(p.Name(), pluginLevel, now, result)
		record := state.recordPluginYield(p.Name(), result.BytesFreed < minYield, now, digest, zeroYield)
		pluginReport.ZeroYieldCount = record.ZeroYieldCount
		pluginReport.SuppressedUntil = record.SuppressedUntil
		stateDirty = true
		if record.SuppressedUntil == "" {
			return
		}
		attrs := []any{
			"plugin", p.Name(),
			"reason", "zero_yield",
			"zero_yield_count", record.ZeroYieldCount,
			"retry_at", record.SuppressedUntil,
		}
		if record.ZeroYieldCount == zeroYield.limit {
			d.logger.Warn("plugin reclaimed nothing on repeated runs; suppressing it until conditions change", attrs...)
		} else {
			d.logger.Info("plugin still reclaiming nothing; zero-yield suppression extended", attrs...)
		}
	}

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

		if stateErr == nil && zeroYield.limit > 0 {
			until, count, lifted, suppressed := state.zeroYieldSuppression(p.Name(), pluginLevel, now, digest)
			if suppressed {
				if exempt := d.zeroYieldExemption(p, report, beforeErr == nil); exempt != "" {
					lifted = exempt
				} else {
					pluginReport.WouldRun = false
					pluginReport.SkipReason = "zero_yield_backoff"
					pluginReport.ZeroYieldCount = count
					pluginReport.RetryAt = until.UTC().Format(time.RFC3339)
					d.logger.Debug("plugin suppressed for zero yield",
						"plugin", p.Name(), "zero_yield_count", count, "retry_at", pluginReport.RetryAt)
					report.Plugins = append(report.Plugins, pluginReport)
					continue
				}
			}
			if lifted != "" {
				pluginReport.ZeroYieldCount = count
				pluginReport.ZeroYieldLifted = lifted
			}
		}

		if stateErr == nil {
			if pluginCooldown, reason, ok := d.pluginCooldown(report, level); ok {
				if remaining := state.cooldownRemaining(p.Name(), pluginLevel, now, pluginCooldown); remaining > 0 {
					pluginReport.WouldRun = false
					pluginReport.SkipReason = reason
					pluginReport.CooldownRemainingSeconds = int64(remaining.Round(time.Second) / time.Second)
					pluginReport.RetryAt = now.Add(remaining).UTC().Format(time.RFC3339)
					report.Plugins = append(report.Plugins, pluginReport)
					continue
				}
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
		pluginReport.Ran = true
		pluginReport.BytesFreed = result.BytesFreed
		pluginReport.EstimatedBytesFreed = result.EstimatedBytesFreed
		pluginReport.CommandBytesFreed = result.CommandBytesFreed
		pluginReport.HostBytesFreed = result.HostBytesFreed
		pluginReport.ItemsCleaned = result.ItemsCleaned
		if result.Error != nil {
			pluginReport.Error = result.Error.Error()
			d.logger.Error("plugin failed", "plugin", p.Name(), "error", result.Error)
			if stateErr == nil {
				recordRun(p, &pluginReport, result)
			}
			report.Plugins = append(report.Plugins, pluginReport)
			continue
		}

		if stateErr == nil {
			recordRun(p, &pluginReport, result)
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
	report.NextRetryAt = nextRetryAt(report.Plugins)

	d.updateHostFreeAfter(&report, beforeStats, beforeErr)

	// Record inode reclaim progress for the monitored path so the circuit
	// breaker can detect inode pressure that repeated cycles fail to relieve
	// (TIN-2170). A cycle counts as progress when free inodes increased or inode
	// pressure cleared; otherwise the no-progress counter advances toward the
	// backoff limit.
	if !d.dryRun && stateErr == nil && report.HostInodesTotal > 0 {
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

	// Record byte reclaim progress for the monitored path so byte backoff can
	// detect byte pressure that repeated cycles fail to relieve (TIN-3342). A
	// cycle counts as progress when plugins or the host free-space delta reach
	// policy.byte_progress_min_mb, or when the byte level dropped or cleared.
	if !d.dryRun && stateErr == nil && beforeErr == nil && report.HostFreeError == "" {
		beforeLevel := parseLevel(report.HostByteLevel)
		if beforeLevel != monitor.LevelNone || byteBackoffWasEngaged ||
			state.byteNoProgressPending(report.MonitorPath) {
			state.recordByteProgress(report.MonitorPath, report.HostFreeAfterBytes, report.HostByteLevel,
				int(beforeLevel), now, d.byteProgressMade(report), anyPluginRan(report.Plugins),
				// A forced run is exempt from backoff; it does not release it.
				report.ByteBackoff || (report.ForcedLevel && byteBackoffWasEngaged))
			stateDirty = true
		}
	}

	if stateDirty {
		if err := saveCleanupState(report.StateFile, state); err != nil {
			report.StateError = err.Error()
			d.logger.Warn("failed to save cleanup state", "path", report.StateFile, "error", err)
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

	return report, nil
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
	InodeBackoff bool `json:"inode_backoff,omitempty"`
	// HostByteLevelAfter is the byte-driven level for the monitored primary
	// path measured after cleanup.
	HostByteLevelAfter string `json:"host_byte_level_after,omitempty"`
	// ByteNoProgressCount is the number of consecutive prior cleanup cycles
	// that ran under byte pressure without meaningful reclaim on the primary
	// path, at or below the current byte level.
	ByteNoProgressCount int `json:"byte_no_progress_count,omitempty"`
	// ByteBackoff reports that byte backoff engaged this cycle: plugins did not
	// bypass cooldown and ran at most once per ByteBackoffSeconds.
	ByteBackoff bool `json:"byte_backoff,omitempty"`
	// ByteBackoffReason explains the byte backoff decision when the no-progress
	// limit was reached: "no_progress" when engaged, "below_emergency_floor"
	// when the emergency free-space floor kept it off.
	ByteBackoffReason string `json:"byte_backoff_reason,omitempty"`
	// ByteBackoffSeconds is the per-plugin interval applied while byte backoff
	// is engaged.
	ByteBackoffSeconds int64 `json:"byte_backoff_seconds,omitempty"`
	// EmergencyFreeBytes is the free-space floor below which byte backoff
	// never engages.
	EmergencyFreeBytes uint64 `json:"emergency_free_bytes,omitempty"`
	HostFreeError      string `json:"host_free_error,omitempty"`
	StateFile          string `json:"state_file,omitempty"`
	StateError         string `json:"state_error,omitempty"`
	// StateQuarantined is the path an undecodable state file was renamed to
	// this cycle; accounting continued with fresh state.
	StateQuarantined string `json:"state_quarantined,omitempty"`
	CooldownSeconds  int64  `json:"cooldown_seconds,omitempty"`
	// CycleDurationMs is the wall-clock duration of this cycle in milliseconds.
	CycleDurationMs int64 `json:"cycle_duration_ms"`
	// NextCycleAt is when the daemon will start its next cycle: completion plus
	// the poll interval. It is empty outside daemon mode.
	NextCycleAt string `json:"next_cycle_at,omitempty"`
	// NextRetryAt is the earliest time a plugin held back this cycle (by
	// cooldown, byte backoff or zero-yield suppression) becomes eligible again.
	NextRetryAt string `json:"next_retry_at,omitempty"`
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
	// Fstype is the filesystem type reported by statfs.
	Fstype string `json:"fstype,omitempty"`
	// InodesDynamic reports a dynamic-inode filesystem (XFS, APFS, ZFS, Btrfs)
	// whose used percentage is not a pressure signal.
	InodesDynamic bool `json:"inodes_dynamic,omitempty"`
	// InodeFreeFloor is the absolute free-inode floor in effect for the mount.
	InodeFreeFloor uint64 `json:"inode_free_floor,omitempty"`
	// InodeLadderSkipped reports that the percentage inode ladder was not
	// consulted (a floor is set, or the filesystem allocates inodes dynamically).
	InodeLadderSkipped bool   `json:"inode_ladder_skipped,omitempty"`
	Level              string `json:"level"`
	Error              string `json:"error,omitempty"`
}

type pluginCycleReport struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Level       string `json:"level"`
	DryRun      bool   `json:"dry_run"`
	// WouldRun reports eligibility: the plugin was not skipped by target,
	// cooldown, or filter. In a real cleanup cycle it is a precondition, not
	// an outcome; Ran is the outcome.
	WouldRun bool `json:"would_run"`
	// Ran reports that Cleanup was actually invoked this cycle (never true
	// in dry-run). A plugin with Ran and a non-empty Error ran and failed.
	Ran                      bool                 `json:"ran"`
	SkipReason               string               `json:"skip_reason,omitempty"`
	Plan                     *plugins.CleanupPlan `json:"plan,omitempty"`
	BytesFreed               int64                `json:"bytes_freed"`
	EstimatedBytesFreed      int64                `json:"estimated_bytes_freed"`
	CommandBytesFreed        int64                `json:"command_bytes_freed"`
	HostBytesFreed           int64                `json:"host_bytes_freed"`
	ItemsCleaned             int                  `json:"items_cleaned"`
	CooldownRemainingSeconds int64                `json:"cooldown_remaining_seconds,omitempty"`
	// RetryAt is when a plugin held back by cooldown, byte backoff or
	// zero-yield suppression becomes eligible again (RFC3339).
	RetryAt string `json:"retry_at,omitempty"`
	// ZeroYieldCount is the plugin's consecutive zero-yield run count.
	ZeroYieldCount int `json:"zero_yield_count,omitempty"`
	// SuppressedUntil is set when this run left the plugin suppressed for
	// zero yield: it is skipped until then unless conditions change.
	SuppressedUntil string `json:"suppressed_until,omitempty"`
	// ZeroYieldLifted names why a suppressed plugin ran anyway: level_rose,
	// config_changed, operator_run, safety_critical, exempt_plugin,
	// below_emergency_floor or free_unknown.
	ZeroYieldLifted string `json:"zero_yield_lifted,omitempty"`
	Error           string `json:"error,omitempty"`
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
				Label:              label,
				Path:               mount.Path,
				UsedPercent:        stats.UsedPercent,
				FreeGB:             stats.FreeGB,
				FreeBytes:          stats.Free,
				ByteLevel:          byteLevel.String(),
				InodesTotal:        stats.InodesTotal,
				InodesFree:         stats.InodesFree,
				InodesUsedPercent:  stats.InodesUsedPercent,
				InodeLevel:         inodeLevelDisplay(inodeLevel, stats.InodesTotal),
				Fstype:             stats.Fstype,
				InodesDynamic:      stats.InodesDynamic,
				InodeFreeFloor:     mountMonitor.InodeFreeFloor,
				InodeLadderSkipped: mountMonitor.InodeLadderSkipped(stats),
				Level:              mountLevel.String(),
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
		mount.ThresholdInodeCritical <= 0 &&
		mount.InodeFreeFloor == 0 {
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

	mountMonitor := monitor.NewDiskMonitorWithInodeThresholds(
		warning,
		moderate,
		aggressive,
		critical,
		inodeWarning,
		inodeModerate,
		inodeAggressive,
		inodeCritical,
	)
	mountMonitor.InodeFreeFloor = d.monitor.InodeFreeFloor
	if mount.InodeFreeFloor > 0 {
		mountMonitor.InodeFreeFloor = mount.InodeFreeFloor
	}
	return mountMonitor
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

func (d *daemon) loadStateForCycle(now time.Time) (*cleanupState, string, error) {
	if d.dryRun || d.config == nil {
		return newCleanupState(), "", nil
	}
	return loadCleanupState(expandPathHome(d.config.Policy.StateFile), now)
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

// pluginCooldown returns the interval that holds a plugin back after its last
// run this cycle, with the skip reason to report. Normal cooldown wins when it
// applies. Otherwise, while byte backoff is engaged, the byte backoff interval
// applies where pressure would have bypassed cooldown.
func (d *daemon) pluginCooldown(report cycleReport, level monitor.CleanupLevel) (time.Duration, string, bool) {
	if d.shouldApplyCooldown(report, level) {
		return d.cleanupCooldown(), "cooldown", true
	}
	if report.ByteBackoff {
		return d.byteBackoffInterval(), "byte_backoff", true
	}
	return 0, "", false
}

// byteBackoffActive reports whether byte backoff should engage this cycle and
// why. It engages when the primary path is under byte pressure, the configured
// number of consecutive cleanup cycles have failed to make byte progress at or
// below the current level, and free space is at or above the emergency floor.
func (d *daemon) byteBackoffActive(report cycleReport) (bool, string) {
	limit := d.byteNoProgressLimit()
	if limit <= 0 || report.HostByteLevel == "" {
		return false, ""
	}
	if parseLevel(report.HostByteLevel) == monitor.LevelNone {
		return false, ""
	}
	if report.ByteNoProgressCount < limit {
		return false, ""
	}
	if floor := d.emergencyFreeBytes(); floor > 0 && report.HostFreeBeforeBytes < floor {
		return false, "below_emergency_floor"
	}
	return true, "no_progress"
}

// byteProgressMade reports whether a cycle relieved byte pressure: enough
// bytes were freed by plugins or appeared on the host, or the byte level
// dropped or cleared.
func (d *daemon) byteProgressMade(report cycleReport) bool {
	before := parseLevel(report.HostByteLevel)
	after := parseLevel(report.HostByteLevelAfter)
	if before == monitor.LevelNone || after < before {
		return true
	}
	minBytes := d.byteProgressMinBytes()
	return report.TotalBytesFreed >= minBytes || report.HostFreeDeltaBytes >= minBytes
}

func anyPluginRan(reports []pluginCycleReport) bool {
	for _, plugin := range reports {
		if plugin.Ran {
			return true
		}
	}
	return false
}

// byteNoProgressLimit returns the configured byte no-progress limit. Zero uses
// the default; a negative value disables byte backoff and is returned as is.
func (d *daemon) byteNoProgressLimit() int {
	if d.config == nil || d.config.Policy.ByteNoProgressLimit == 0 {
		return defaultByteNoProgressLimit
	}
	return d.config.Policy.ByteNoProgressLimit
}

func (d *daemon) byteProgressMinBytes() int64 {
	mb := defaultByteProgressMinMB
	if d.config != nil && d.config.Policy.ByteProgressMinMB > 0 {
		mb = d.config.Policy.ByteProgressMinMB
	}
	return int64(mb) * 1024 * 1024
}

// byteBackoffMax returns the configured cap on the byte backoff interval.
func (d *daemon) byteBackoffMax() time.Duration {
	if d.config == nil || d.config.Policy.ByteBackoffMax == "" {
		return defaultByteBackoffMax
	}
	duration, err := time.ParseDuration(d.config.Policy.ByteBackoffMax)
	if err != nil || duration <= 0 {
		return defaultByteBackoffMax
	}
	return duration
}

// byteBackoffInterval is the per-plugin interval while byte backoff is
// engaged: the configured cooldown, capped at byte_backoff_max, or the cap
// itself when no cooldown is configured.
func (d *daemon) byteBackoffInterval() time.Duration {
	limit := d.byteBackoffMax()
	cooldown := d.cleanupCooldown()
	if cooldown <= 0 || cooldown > limit {
		return limit
	}
	return cooldown
}

func (d *daemon) emergencyFreeBytes() uint64 {
	if d.config == nil || d.config.Policy.EmergencyFreeGB <= 0 {
		return 0
	}
	return uint64(d.config.Policy.EmergencyFreeGB) * 1024 * 1024 * 1024
}

// zeroYieldPolicy resolves the zero-yield suppression policy. A zero limit
// uses the default; a negative limit disables suppression (limit 0).
func (d *daemon) zeroYieldPolicy() zeroYieldPolicy {
	policy := zeroYieldPolicy{
		limit: defaultZeroYieldLimit,
		base:  defaultZeroYieldBackoffBase,
		max:   defaultZeroYieldBackoffMax,
	}
	if d.config == nil {
		return policy
	}
	switch limit := d.config.Policy.ZeroYieldLimit; {
	case limit < 0:
		policy.limit = 0
	case limit > 0:
		policy.limit = limit
	}
	if duration, err := time.ParseDuration(d.config.Policy.ZeroYieldBackoffBase); err == nil && duration > 0 {
		policy.base = duration
	}
	if duration, err := time.ParseDuration(d.config.Policy.ZeroYieldBackoffMax); err == nil && duration > 0 {
		policy.max = duration
	}
	if policy.max < policy.base {
		policy.max = policy.base
	}
	return policy
}

// configDigest identifies the effective configuration and binary version. A
// change, such as a Home Manager switch that renders new budgets, lifts
// zero-yield suppression so every plugin gets a fresh attempt.
func (d *daemon) configDigest() string {
	h := sha256.New()
	h.Write([]byte(version))
	h.Write([]byte{0})
	if data, err := json.Marshal(d.config); err == nil {
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// zeroYieldExemption returns why plugin p runs this cycle even though it is
// suppressed for zero yield, or "" when suppression applies.
func (d *daemon) zeroYieldExemption(p plugins.Plugin, report cycleReport, freeKnown bool) string {
	if report.ForcedLevel || d.dryRun || len(d.pluginFilter) > 0 {
		return "operator_run"
	}
	if critical, ok := p.(plugins.SafetyCritical); ok && critical.SafetyCritical() {
		return "safety_critical"
	}
	if d.config != nil {
		for _, name := range d.config.Policy.ZeroYieldExemptPlugins {
			if strings.TrimSpace(name) == p.Name() {
				return "exempt_plugin"
			}
		}
	}
	if !freeKnown {
		return "free_unknown"
	}
	if floor := d.emergencyFreeBytes(); floor > 0 && report.HostFreeBeforeBytes < floor {
		return "below_emergency_floor"
	}
	return ""
}

// nextRetryAt returns the earliest retry or suppression time across plugins,
// or "" when no plugin is waiting.
func nextRetryAt(reports []pluginCycleReport) string {
	var earliest time.Time
	for _, plugin := range reports {
		for _, value := range []string{plugin.RetryAt, plugin.SuppressedUntil} {
			if value == "" {
				continue
			}
			at, err := time.Parse(time.RFC3339, value)
			if err != nil {
				continue
			}
			if earliest.IsZero() || at.Before(earliest) {
				earliest = at
			}
		}
	}
	if earliest.IsZero() {
		return ""
	}
	return earliest.UTC().Format(time.RFC3339)
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
	report.HostByteLevelAfter = d.byteLevelForPath(report.MonitorPath, afterStats).String()
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

	// Development artifact cleanup (all platforms)
	registry.Register(plugins.NewDevArtifactsPlugin())

	// Archive staging pre-image lifecycle (all platforms)
	registry.Register(plugins.NewArchiveLifecyclePlugin())

	// Report-only debris inventory (all platforms, off by default)
	registry.Register(plugins.NewDebrisReportPlugin())

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
