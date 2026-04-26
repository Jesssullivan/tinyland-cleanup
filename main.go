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
//	-verbose          Enable verbose logging
//	-version          Print version and exit
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
		configPath  = flag.String("config", "", "Path to configuration file")
		runDaemon   = flag.Bool("daemon", false, "Run as a daemon")
		once        = flag.Bool("once", false, "Run cleanup once and exit")
		level       = flag.String("level", "", "Force cleanup level")
		dryRun      = flag.Bool("dry-run", false, "Show what would be cleaned")
		output      = flag.String("output", "text", "Output format: text, json")
		verbose     = flag.Bool("verbose", false, "Enable verbose logging")
		showVersion = flag.Bool("version", false, "Print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("tinyland-cleanup %s (%s) built %s\n", version, commit, date)
		os.Exit(0)
	}

	if *output != "text" && *output != "json" {
		fmt.Fprintf(os.Stderr, "invalid output format %q: expected text or json\n", *output)
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

	// Create plugin registry and register all plugins
	registry := plugins.NewRegistry()
	registerPlugins(registry)

	// Create disk monitor
	diskMon := monitor.NewDiskMonitor(
		cfg.Thresholds.Warning,
		cfg.Thresholds.Moderate,
		cfg.Thresholds.Aggressive,
		cfg.Thresholds.Critical,
	)

	// Create cleanup daemon
	d := &daemon{
		config:   cfg,
		registry: registry,
		monitor:  diskMon,
		logger:   logger,
		dryRun:   *dryRun,
		output:   *output,
		report:   os.Stdout,
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
	config   *config.Config
	registry *plugins.Registry
	monitor  *monitor.DiskMonitor
	logger   *slog.Logger
	dryRun   bool
	output   string
	report   io.Writer
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

	report := cycleReport{
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		DryRun:      d.dryRun,
		ForcedLevel: forcedLevel != monitor.LevelNone,
		Level:       level.String(),
		MonitorPath: d.primaryMonitorPath(assessment),
		Mounts:      assessment.Mounts,
	}

	beforeStats, beforeErr := monitor.GetDiskStats(report.MonitorPath)
	if beforeErr != nil {
		report.HostFreeError = beforeErr.Error()
		d.logger.Warn("failed to measure host free space before cleanup", "path", report.MonitorPath, "error", beforeErr)
	} else {
		report.HostFreeBeforeBytes = beforeStats.Free
	}

	if level == monitor.LevelNone {
		return d.writeReport(report)
	}

	// Convert monitor level to plugin level
	pluginLevel := plugins.CleanupLevel(level)

	// Run cleanup plugins
	enabledPlugins := d.registry.GetEnabled(d.config)
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
			continue
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
	}

	report.TotalBytesFreed = totalFreed
	report.TotalItemsCleaned = totalItems

	afterStats, afterErr := monitor.GetDiskStats(report.MonitorPath)
	if afterErr != nil {
		report.HostFreeError = afterErr.Error()
		d.logger.Warn("failed to measure host free space after cleanup", "path", report.MonitorPath, "error", afterErr)
	} else {
		report.HostFreeAfterBytes = afterStats.Free
		if beforeErr == nil {
			report.HostFreeDeltaBytes = int64(afterStats.Free) - int64(beforeStats.Free)
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
	// PlannedEstimatedBytesFreed aggregates dry-run plugin plan estimates.
	PlannedEstimatedBytesFreed int64 `json:"planned_estimated_bytes_freed,omitempty"`
	// PlannedRequiredFreeBytes is the largest free-space preflight requirement across plugin plans.
	PlannedRequiredFreeBytes int64 `json:"planned_required_free_bytes,omitempty"`
	// PlannedTargets is the total number of dry-run cleanup targets.
	PlannedTargets    int                 `json:"planned_targets,omitempty"`
	TotalBytesFreed   int64               `json:"total_bytes_freed"`
	TotalItemsCleaned int                 `json:"total_items_cleaned"`
	Mounts            []mountReport       `json:"mounts"`
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
	Name                string               `json:"name"`
	Description         string               `json:"description"`
	Level               string               `json:"level"`
	DryRun              bool                 `json:"dry_run"`
	WouldRun            bool                 `json:"would_run"`
	SkipReason          string               `json:"skip_reason,omitempty"`
	Plan                *plugins.CleanupPlan `json:"plan,omitempty"`
	BytesFreed          int64                `json:"bytes_freed"`
	EstimatedBytesFreed int64                `json:"estimated_bytes_freed"`
	CommandBytesFreed   int64                `json:"command_bytes_freed"`
	HostBytesFreed      int64                `json:"host_bytes_freed"`
	ItemsCleaned        int                  `json:"items_cleaned"`
	Error               string               `json:"error,omitempty"`
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
			stats, err := monitor.GetDiskStats(mount.Path)
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

		stats, detectedLevel, err := d.monitor.Check(monitorPath)
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
	if d.output != "json" {
		return nil
	}
	encoder := json.NewEncoder(d.report)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
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
