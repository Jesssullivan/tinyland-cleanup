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

var (
	version = "0.2.0"
	commit  = "dev"
	date    = "unknown"
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

	// Setup log file directory
	if err := ensureLogDir(cfg.LogFile); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create log directory: %v\n", err)
		os.Exit(1)
	}

	// Setup logging - write to both stderr and log file
	logLevel := slog.LevelInfo
	if *verbose {
		logLevel = slog.LevelDebug
	}

	// Open log file for writing
	logFile, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open log file: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	// Create multi-writer for both stderr and log file
	multiWriter := io.MultiWriter(os.Stderr, logFile)
	logger := slog.New(slog.NewTextHandler(multiWriter, &slog.HandlerOptions{
		Level: logLevel,
	}))

	// Create disk monitor
	diskMon := monitor.NewDiskMonitor(
		cfg.Thresholds.Warning,
		cfg.Thresholds.Moderate,
		cfg.Thresholds.Aggressive,
		cfg.Thresholds.Critical,
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
}

func (d *daemon) run(ctx context.Context) error {
	ticker := time.NewTicker(time.Duration(d.config.PollInterval) * time.Second)
	defer ticker.Stop()

	// Run immediately on start
	if err := d.runOnce(ctx, monitor.LevelNone); err != nil {
		d.logger.Error("initial cleanup failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
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
		Timestamp:    now.UTC().Format(time.RFC3339),
		DryRun:       d.dryRun,
		ForcedLevel:  forcedLevel != monitor.LevelNone,
		Level:        level.String(),
		MonitorPath:  d.primaryMonitorPath(assessment),
		Mounts:       assessment.Mounts,
		PluginFilter: d.pluginFilter,
	}

	cooldown := d.cleanupCooldown()
	if cooldown > 0 {
		report.CooldownSeconds = int64(cooldown / time.Second)
	}
	report.StateFile = expandPathHome(d.config.Policy.StateFile)
	state, stateErr := d.loadStateForCycle()
	if stateErr != nil {
		report.StateError = stateErr.Error()
		d.logger.Warn("failed to load cleanup state", "path", report.StateFile, "error", stateErr)
	}
	stateDirty := false

	beforeStats, beforeErr := d.getDiskStats(report.MonitorPath)
	if beforeErr != nil {
		report.HostFreeError = beforeErr.Error()
		d.logger.Warn("failed to measure host free space before cleanup", "path", report.MonitorPath, "error", beforeErr)
	} else {
		report.HostFreeBeforeBytes = beforeStats.Free
		d.updateTargetFreeStatus(&report, beforeStats)
	}

	if level == monitor.LevelNone {
		return d.writeReport(report)
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

		if !d.dryRun && report.TargetFreeMet {
			pluginReport.WouldRun = false
			pluginReport.SkipReason = "target_free_met"
			if report.StopReason == "" {
				report.StopReason = "target_free_met"
			}
			report.Plugins = append(report.Plugins, pluginReport)
			continue
		}

		if d.shouldApplyCooldown(report, level) && stateErr == nil {
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
		pluginReport.BytesFreed = result.BytesFreed
		pluginReport.EstimatedBytesFreed = result.EstimatedBytesFreed
		pluginReport.CommandBytesFreed = result.CommandBytesFreed
		pluginReport.HostBytesFreed = result.HostBytesFreed
		pluginReport.ItemsCleaned = result.ItemsCleaned
		if result.Error != nil {
			pluginReport.Error = result.Error.Error()
			report.Plugins = append(report.Plugins, pluginReport)
			d.logger.Error("plugin failed", "plugin", p.Name(), "error", result.Error)
			if stateErr == nil {
				state.recordPluginRun(p.Name(), pluginLevel, now, result)
				stateDirty = true
			}
			continue
		}

		report.Plugins = append(report.Plugins, pluginReport)
		if stateErr == nil {
			state.recordPluginRun(p.Name(), pluginLevel, now, result)
			stateDirty = true
		}
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

	return d.writeReport(report)
}

type cycleReport struct {
	Timestamp           string `json:"timestamp"`
	DryRun              bool   `json:"dry_run"`
	ForcedLevel         bool   `json:"forced_level"`
	Level               string `json:"level"`
	MonitorPath         string `json:"monitor_path"`
	HostFreeBeforeBytes uint64 `json:"host_free_before_bytes"`
	HostFreeAfterBytes  uint64 `json:"host_free_after_bytes"`
	HostFreeDeltaBytes  int64  `json:"host_free_delta_bytes"`
	HostFreeError       string `json:"host_free_error,omitempty"`
	StateFile           string `json:"state_file,omitempty"`
	StateError          string `json:"state_error,omitempty"`
	CooldownSeconds     int64  `json:"cooldown_seconds,omitempty"`
	// TargetUsedPercent is the legacy target_free config value as a maximum used percentage.
	TargetUsedPercent int `json:"target_used_percent"`
	// TargetFreeBytes is the free-space equivalent required to satisfy TargetUsedPercent.
	TargetFreeBytes uint64 `json:"target_free_bytes"`
	// TargetFreeDeficitBytes is the remaining free-space gap to the target.
	TargetFreeDeficitBytes int64 `json:"target_free_deficit_bytes"`
	// TargetFreeMet reports whether the host already satisfies the target.
	TargetFreeMet bool `json:"target_free_met"`
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
	Label       string  `json:"label"`
	Path        string  `json:"path"`
	UsedPercent float64 `json:"used_percent"`
	FreeGB      float64 `json:"free_gb"`
	FreeBytes   uint64  `json:"free_bytes"`
	Level       string  `json:"level"`
	Error       string  `json:"error,omitempty"`
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
	Level  monitor.CleanupLevel
	Mounts []mountReport
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

			// Use per-mount thresholds if configured, otherwise use global
			mountMonitor := d.monitor
			if mount.ThresholdWarning > 0 || mount.ThresholdCritical > 0 {
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
				mountMonitor = monitor.NewDiskMonitor(warning, moderate, aggressive, critical)
			}

			mountLevel := mountMonitor.CheckLevel(stats)
			assessment.Mounts = append(assessment.Mounts, mountReport{
				Label:       label,
				Path:        mount.Path,
				UsedPercent: stats.UsedPercent,
				FreeGB:      stats.FreeGB,
				FreeBytes:   stats.Free,
				Level:       mountLevel.String(),
			})

			d.logger.Info("disk status",
				"mount", label,
				"path", mount.Path,
				"used_percent", fmt.Sprintf("%.1f%%", stats.UsedPercent),
				"free_gb", fmt.Sprintf("%.1fGB", stats.FreeGB),
				"level", mountLevel.String(),
			)

			if mountLevel > assessment.Level {
				assessment.Level = mountLevel
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

		assessment.Mounts = append(assessment.Mounts, mountReport{
			Label:       monitorPath,
			Path:        monitorPath,
			UsedPercent: stats.UsedPercent,
			FreeGB:      stats.FreeGB,
			FreeBytes:   stats.Free,
			Level:       detectedLevel.String(),
		})

		d.logger.Info("disk status",
			"used_percent", fmt.Sprintf("%.1f%%", stats.UsedPercent),
			"free_gb", fmt.Sprintf("%.1fGB", stats.FreeGB),
			"level", detectedLevel.String(),
		)

		assessment.Level = detectedLevel
	}

	return assessment
}

func (d *daemon) checkMounts() monitor.CleanupLevel {
	return d.assessMounts().Level
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

func (d *daemon) loadStateForCycle() (*cleanupState, error) {
	if d.dryRun || d.config == nil {
		return newCleanupState(), nil
	}
	return loadCleanupState(expandPathHome(d.config.Policy.StateFile))
}

func (d *daemon) shouldApplyCooldown(report cycleReport, level monitor.CleanupLevel) bool {
	return !d.dryRun &&
		!report.ForcedLevel &&
		level != monitor.LevelCritical &&
		d.cleanupCooldown() > 0
}

func (d *daemon) updateHostFreeAfter(report *cycleReport, beforeStats *monitor.DiskStats, beforeErr error) {
	afterStats, afterErr := d.getDiskStats(report.MonitorPath)
	if afterErr != nil {
		report.HostFreeError = afterErr.Error()
		d.logger.Warn("failed to measure host free space after cleanup", "path", report.MonitorPath, "error", afterErr)
		return
	}

	report.HostFreeAfterBytes = afterStats.Free
	if beforeErr == nil && beforeStats != nil {
		report.HostFreeDeltaBytes = int64(afterStats.Free) - int64(beforeStats.Free)
	}
	d.updateTargetFreeStatus(report, afterStats)
}

func (d *daemon) updateTargetFreeStatus(report *cycleReport, stats *monitor.DiskStats) {
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
