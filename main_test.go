package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/Jesssullivan/tinyland-cleanup/config"
	"github.com/Jesssullivan/tinyland-cleanup/monitor"
	"github.com/Jesssullivan/tinyland-cleanup/plugins"
)

func TestRunOnceDryRunJSONReport(t *testing.T) {
	var output bytes.Buffer
	mock := &reportingPlugin{}
	daemon := newTestDaemon(t, mock, &output)
	daemon.dryRun = true

	if err := daemon.runOnce(context.Background(), monitor.LevelCritical); err != nil {
		t.Fatalf("runOnce failed: %v", err)
	}

	if mock.called {
		t.Fatal("dry-run should not call plugin cleanup")
	}

	report := decodeCycleReport(t, output.Bytes())
	if !report.DryRun {
		t.Fatal("expected dry_run report")
	}
	if report.Level != "critical" {
		t.Fatalf("expected critical level, got %q", report.Level)
	}
	if report.MonitorPath == "" {
		t.Fatal("expected monitor path")
	}
	if len(report.Plugins) != 1 {
		t.Fatalf("expected 1 plugin report, got %d", len(report.Plugins))
	}

	plugin := report.Plugins[0]
	if plugin.Name != "reporting" {
		t.Fatalf("unexpected plugin name %q", plugin.Name)
	}
	if !plugin.WouldRun {
		t.Fatal("expected dry-run plugin to be marked would_run")
	}
	if plugin.SkipReason != "dry_run" {
		t.Fatalf("expected dry_run skip reason, got %q", plugin.SkipReason)
	}
}

func TestRunOnceDryRunJSONReportIncludesPluginPlan(t *testing.T) {
	var output bytes.Buffer
	mock := &planningPlugin{
		reportingPlugin: reportingPlugin{},
		plan: plugins.CleanupPlan{
			Plugin:              "reporting",
			Level:               "critical",
			Summary:             "reporting dry-run plan",
			WouldRun:            false,
			SkipReason:          "preflight_failed",
			EstimatedBytesFreed: 100,
			RequiredFreeBytes:   42,
			Steps:               []string{"inspect", "verify"},
			Targets: []plugins.CleanupTarget{
				{Type: "cache", Name: "one", Action: "review"},
				{Type: "cache", Name: "two", Action: "review"},
			},
			Metadata: map[string]string{
				"provider": "test",
			},
		},
	}
	daemon := newTestDaemon(t, mock, &output)
	daemon.dryRun = true

	if err := daemon.runOnce(context.Background(), monitor.LevelCritical); err != nil {
		t.Fatalf("runOnce failed: %v", err)
	}

	report := decodeCycleReport(t, output.Bytes())
	if len(report.Plugins) != 1 {
		t.Fatalf("expected 1 plugin report, got %d", len(report.Plugins))
	}
	if report.Plugins[0].Plan == nil {
		t.Fatal("expected plugin dry-run plan")
	}
	if report.Plugins[0].Plan.SkipReason != "preflight_failed" {
		t.Fatalf("expected preflight_failed plan skip reason, got %q", report.Plugins[0].Plan.SkipReason)
	}
	if report.Plugins[0].SkipReason != "dry_run" {
		t.Fatalf("expected dry_run plugin skip reason, got %q", report.Plugins[0].SkipReason)
	}
	if report.PlannedEstimatedBytesFreed != 100 {
		t.Fatalf("expected planned estimated bytes 100, got %d", report.PlannedEstimatedBytesFreed)
	}
	if report.PlannedRequiredFreeBytes != 42 {
		t.Fatalf("expected planned required bytes 42, got %d", report.PlannedRequiredFreeBytes)
	}
	if report.PlannedTargets != 2 {
		t.Fatalf("expected 2 planned targets, got %d", report.PlannedTargets)
	}
}

func TestRunOnceCleanupJSONReport(t *testing.T) {
	var output bytes.Buffer
	mock := &reportingPlugin{
		result: plugins.CleanupResult{
			Plugin:              "reporting",
			Level:               plugins.LevelCritical,
			BytesFreed:          1234,
			EstimatedBytesFreed: 1000,
			CommandBytesFreed:   200,
			HostBytesFreed:      34,
			ItemsCleaned:        2,
		},
	}
	daemon := newTestDaemon(t, mock, &output)
	daemon.diskStats = sequenceDiskStats(t,
		diskStats(1000, 20, 98),
		diskStats(1000, 20, 98),
		diskStats(1000, 20, 98),
		diskStats(1000, 20, 98),
	)

	if err := daemon.runOnce(context.Background(), monitor.LevelCritical); err != nil {
		t.Fatalf("runOnce failed: %v", err)
	}

	if !mock.called {
		t.Fatal("expected plugin cleanup to run")
	}

	report := decodeCycleReport(t, output.Bytes())
	if report.DryRun {
		t.Fatal("did not expect dry_run report")
	}
	if report.TotalBytesFreed != 1234 {
		t.Fatalf("expected total bytes 1234, got %d", report.TotalBytesFreed)
	}
	if report.TotalItemsCleaned != 2 {
		t.Fatalf("expected total items 2, got %d", report.TotalItemsCleaned)
	}

	plugin := report.Plugins[0]
	if plugin.BytesFreed != 1234 {
		t.Fatalf("expected plugin bytes 1234, got %d", plugin.BytesFreed)
	}
	if plugin.EstimatedBytesFreed != 1000 {
		t.Fatalf("expected estimated bytes 1000, got %d", plugin.EstimatedBytesFreed)
	}
	if plugin.CommandBytesFreed != 200 {
		t.Fatalf("expected command bytes 200, got %d", plugin.CommandBytesFreed)
	}
	if plugin.HostBytesFreed != 34 {
		t.Fatalf("expected host bytes 34, got %d", plugin.HostBytesFreed)
	}
}

func TestRunOnceStopsAfterTargetFreeMet(t *testing.T) {
	var output bytes.Buffer
	first := &reportingPlugin{
		name: "first",
		result: plugins.CleanupResult{
			Plugin:     "first",
			Level:      plugins.LevelCritical,
			BytesFreed: 1,
		},
	}
	second := &reportingPlugin{name: "second"}
	daemon := newTestDaemonWithPlugins(t, &output, first, second)
	daemon.config.TargetFree = 70
	daemon.diskStats = sequenceDiskStats(t,
		diskStats(1000, 20, 98),
		diskStats(1000, 20, 98),
		diskStats(1000, 400, 60),
		diskStats(1000, 400, 60),
	)

	if err := daemon.runOnce(context.Background(), monitor.LevelNone); err != nil {
		t.Fatalf("runOnce failed: %v", err)
	}

	if !first.called {
		t.Fatal("expected first plugin cleanup to run")
	}
	if second.called {
		t.Fatal("second plugin should stop after target free is met")
	}

	report := decodeCycleReport(t, output.Bytes())
	if !report.TargetFreeMet {
		t.Fatal("expected target free to be met")
	}
	if report.TargetUsedPercent != 70 {
		t.Fatalf("expected target used percent 70, got %d", report.TargetUsedPercent)
	}
	if report.TargetFreeBytes != 300 {
		t.Fatalf("expected target free bytes 300, got %d", report.TargetFreeBytes)
	}
	if report.TargetFreeDeficitBytes != 0 {
		t.Fatalf("expected no target free deficit, got %d", report.TargetFreeDeficitBytes)
	}
	if report.StopReason != "target_free_met" {
		t.Fatalf("expected target_free_met stop reason, got %q", report.StopReason)
	}
	if len(report.Plugins) != 2 {
		t.Fatalf("expected 2 plugin reports, got %d", len(report.Plugins))
	}
	if report.Plugins[1].WouldRun {
		t.Fatal("expected second plugin to be marked would_run=false")
	}
	if report.Plugins[1].SkipReason != "target_free_met" {
		t.Fatalf("expected target_free_met skip reason, got %q", report.Plugins[1].SkipReason)
	}
}

func newTestDaemon(t *testing.T, plugin plugins.Plugin, output io.Writer) *daemon {
	t.Helper()

	return newTestDaemonWithPlugins(t, output, plugin)
}

func newTestDaemonWithPlugins(t *testing.T, output io.Writer, registeredPlugins ...plugins.Plugin) *daemon {
	t.Helper()

	cfg := config.DefaultConfig()
	registry := plugins.NewRegistry()
	for _, plugin := range registeredPlugins {
		registry.Register(plugin)
	}

	return &daemon{
		config:    cfg,
		registry:  registry,
		monitor:   monitor.NewDiskMonitor(80, 85, 90, 95),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		output:    "json",
		report:    output,
		diskStats: monitor.GetDiskStats,
	}
}

func diskStats(total, free uint64, usedPercent float64) *monitor.DiskStats {
	return &monitor.DiskStats{
		Total:       total,
		Used:        total - free,
		Free:        free,
		UsedPercent: usedPercent,
		FreePercent: 100 - usedPercent,
		FreeGB:      float64(free) / (1024 * 1024 * 1024),
	}
}

func sequenceDiskStats(t *testing.T, stats ...*monitor.DiskStats) func(string) (*monitor.DiskStats, error) {
	t.Helper()

	index := 0
	return func(path string) (*monitor.DiskStats, error) {
		if len(stats) == 0 {
			t.Fatal("sequenceDiskStats requires at least one stats sample")
		}
		if index >= len(stats) {
			index = len(stats) - 1
		}
		next := *stats[index]
		index++
		next.Path = path
		return &next, nil
	}
}

func decodeCycleReport(t *testing.T, data []byte) cycleReport {
	t.Helper()

	var report cycleReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("failed to decode JSON report: %v\n%s", err, string(data))
	}
	return report
}

type reportingPlugin struct {
	called bool
	name   string
	result plugins.CleanupResult
}

func (p *reportingPlugin) Name() string {
	if p.name != "" {
		return p.name
	}
	return "reporting"
}

func (p *reportingPlugin) Description() string {
	return "reports cleanup activity"
}

func (p *reportingPlugin) SupportedPlatforms() []string {
	return nil
}

func (p *reportingPlugin) Enabled(*config.Config) bool {
	return true
}

func (p *reportingPlugin) Cleanup(context.Context, plugins.CleanupLevel, *config.Config, *slog.Logger) plugins.CleanupResult {
	p.called = true
	return p.result
}

type planningPlugin struct {
	reportingPlugin
	plan plugins.CleanupPlan
}

func (p *planningPlugin) PlanCleanup(context.Context, plugins.CleanupLevel, *config.Config, *slog.Logger) plugins.CleanupPlan {
	return p.plan
}
