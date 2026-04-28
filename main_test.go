package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestRunOnceDryRunTextReportExplainsPlan(t *testing.T) {
	var output bytes.Buffer
	hostReclaims := false
	mock := &planningPlugin{
		reportingPlugin: reportingPlugin{},
		plan: plugins.CleanupPlan{
			Plugin:              "reporting",
			Level:               "critical",
			Summary:             "reporting dry-run plan",
			WouldRun:            true,
			EstimatedBytesFreed: 1024 * 1024,
			Targets: []plugins.CleanupTarget{
				{
					Type:              "cache",
					Tier:              plugins.CleanupTierWarm,
					Name:              "example-cache",
					Path:              "/tmp/example-cache",
					Bytes:             1024,
					Reclaim:           plugins.CleanupReclaimNone,
					HostReclaimsSpace: &hostReclaims,
					Protected:         true,
					Action:            "review",
					Reason:            "operator review required",
				},
			},
			Warnings: []string{"review before cleanup"},
		},
	}
	daemon := newTestDaemon(t, mock, &output)
	daemon.dryRun = true
	daemon.output = "text"

	if err := daemon.runOnce(context.Background(), monitor.LevelCritical); err != nil {
		t.Fatalf("runOnce failed: %v", err)
	}

	text := output.String()
	for _, want := range []string{
		"tinyland-cleanup dry-run report",
		"level: critical (forced)",
		"plan: estimated reclaim 1.0 MiB",
		"- reporting: would run (dry_run)",
		"reporting dry-run plan",
		"example-cache (/tmp/example-cache) [cache]: review, protected, tier=warm, reclaim=none, 1.0 KiB - operator review required",
		"review before cleanup",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text report missing %q:\n%s", want, text)
		}
	}
}

func TestRunOnceDryRunHonorsPluginFilter(t *testing.T) {
	var output bytes.Buffer
	first := &planningPlugin{
		reportingPlugin: reportingPlugin{name: "first"},
		plan: plugins.CleanupPlan{
			Plugin:   "first",
			Level:    "critical",
			Summary:  "first plan",
			WouldRun: true,
		},
	}
	second := &planningPlugin{
		reportingPlugin: reportingPlugin{name: "second"},
		plan: plugins.CleanupPlan{
			Plugin:   "second",
			Level:    "critical",
			Summary:  "second plan",
			WouldRun: true,
		},
	}
	daemon := newTestDaemonWithPlugins(t, &output, first, second)
	daemon.dryRun = true
	daemon.pluginFilter = []string{"second"}

	if err := daemon.runOnce(context.Background(), monitor.LevelCritical); err != nil {
		t.Fatalf("runOnce failed: %v", err)
	}

	report := decodeCycleReport(t, output.Bytes())
	if len(report.PluginFilter) != 1 || report.PluginFilter[0] != "second" {
		t.Fatalf("expected plugin filter [second], got %#v", report.PluginFilter)
	}
	if len(report.Plugins) != 1 {
		t.Fatalf("expected 1 plugin report, got %d", len(report.Plugins))
	}
	if report.Plugins[0].Name != "second" {
		t.Fatalf("expected second plugin, got %q", report.Plugins[0].Name)
	}
	if report.Plugins[0].Plan == nil || report.Plugins[0].Plan.Summary != "second plan" {
		t.Fatalf("expected second plan, got %#v", report.Plugins[0].Plan)
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

func TestApplyTargetUsedPercentOverride(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.TargetFree = 70

	if err := applyTargetUsedPercentOverride(cfg, 82); err != nil {
		t.Fatalf("override failed: %v", err)
	}
	if cfg.TargetFree != 82 {
		t.Fatalf("expected target used percent 82, got %d", cfg.TargetFree)
	}

	if err := applyTargetUsedPercentOverride(cfg, 0); err != nil {
		t.Fatalf("zero override should be ignored: %v", err)
	}
	if cfg.TargetFree != 82 {
		t.Fatalf("zero override should preserve existing target, got %d", cfg.TargetFree)
	}

	for _, value := range []int{-1, 100} {
		if err := applyTargetUsedPercentOverride(cfg, value); err == nil {
			t.Fatalf("expected error for target override %d", value)
		}
	}
}

func TestParsePluginFilter(t *testing.T) {
	filter, err := parsePluginFilter(" bazel, nix,bazel ")
	if err != nil {
		t.Fatalf("parsePluginFilter failed: %v", err)
	}
	if len(filter) != 2 || filter[0] != "bazel" || filter[1] != "nix" {
		t.Fatalf("unexpected filter %#v", filter)
	}

	empty, err := parsePluginFilter("")
	if err != nil {
		t.Fatalf("empty filter failed: %v", err)
	}
	if empty != nil {
		t.Fatalf("expected nil empty filter, got %#v", empty)
	}

	if _, err := parsePluginFilter("bazel,,nix"); err == nil {
		t.Fatal("expected empty plugin name error")
	}
}

func TestValidatePluginFilterRejectsUnknownPlugin(t *testing.T) {
	registry := plugins.NewRegistry()
	registry.Register(&reportingPlugin{name: "known"})

	if err := validatePluginFilter([]string{"known"}, registry); err != nil {
		t.Fatalf("known plugin should validate: %v", err)
	}
	if err := validatePluginFilter([]string{"missing"}, registry); err == nil {
		t.Fatal("expected unknown plugin error")
	}
}

func TestListPluginEntriesReportsEnabledAndPlatformSupport(t *testing.T) {
	cfg := config.DefaultConfig()
	registry := plugins.NewRegistry()
	registry.Register(&reportingPlugin{name: "all"})
	registry.Register(&reportingPlugin{name: "unsupported", supported: []string{"not-this-platform"}})
	registry.Register(&reportingPlugin{name: "disabled", disabled: true})

	entries := listPluginEntries(registry, cfg)
	if len(entries) != 3 {
		t.Fatalf("expected 3 plugin entries, got %d", len(entries))
	}

	if entries[0].Name != "all" || !entries[0].Enabled || !entries[0].Supported {
		t.Fatalf("unexpected all entry: %#v", entries[0])
	}
	if entries[1].Name != "unsupported" || !entries[1].Enabled || entries[1].Supported {
		t.Fatalf("unexpected unsupported entry: %#v", entries[1])
	}
	if entries[2].Name != "disabled" || entries[2].Enabled || !entries[2].Supported {
		t.Fatalf("unexpected disabled entry: %#v", entries[2])
	}
}

func TestWritePluginListText(t *testing.T) {
	var output bytes.Buffer
	err := writePluginList(&output, "text", []pluginListEntry{
		{
			Name:               "bazel",
			Description:        "Bazel cleanup",
			Enabled:            true,
			Supported:          true,
			SupportedPlatforms: nil,
		},
		{
			Name:               "homebrew",
			Description:        "Homebrew cleanup",
			Enabled:            false,
			Supported:          false,
			SupportedPlatforms: []string{"darwin"},
		},
	})
	if err != nil {
		t.Fatalf("writePluginList failed: %v", err)
	}

	text := output.String()
	for _, want := range []string{
		"tinyland-cleanup plugins",
		"- bazel: enabled, supported - Bazel cleanup",
		"- homebrew: disabled, unsupported on darwin - Homebrew cleanup",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("plugin list text missing %q:\n%s", want, text)
		}
	}
}

func TestWritePluginListJSON(t *testing.T) {
	var output bytes.Buffer
	err := writePluginList(&output, "json", []pluginListEntry{
		{Name: "nix", Description: "Nix cleanup", Enabled: true, Supported: true},
	})
	if err != nil {
		t.Fatalf("writePluginList failed: %v", err)
	}

	var report pluginListReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("failed to decode plugin list JSON: %v\n%s", err, output.String())
	}
	if len(report.Plugins) != 1 || report.Plugins[0].Name != "nix" {
		t.Fatalf("unexpected plugin list report: %#v", report)
	}
}

func TestRunOnceSkipsPluginDuringCooldown(t *testing.T) {
	var output bytes.Buffer
	mock := &reportingPlugin{}
	daemon := newTestDaemon(t, mock, &output)
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	daemon.now = func() time.Time { return now }
	daemon.config.Policy.Cooldown = "30m"
	daemon.config.Policy.StateFile = filepath.Join(t.TempDir(), "state.json")
	state := newCleanupState()
	state.recordPluginRun("reporting", plugins.LevelAggressive, now.Add(-10*time.Minute), plugins.CleanupResult{
		Plugin:     "reporting",
		Level:      plugins.LevelAggressive,
		BytesFreed: 1,
	})
	if err := saveCleanupState(daemon.config.Policy.StateFile, state); err != nil {
		t.Fatal(err)
	}
	daemon.diskStats = sequenceDiskStats(t,
		diskStats(1000, 100, 90),
		diskStats(1000, 100, 90),
		diskStats(1000, 100, 90),
	)

	if err := daemon.runOnce(context.Background(), monitor.LevelNone); err != nil {
		t.Fatalf("runOnce failed: %v", err)
	}

	if mock.called {
		t.Fatal("plugin should be skipped during cooldown")
	}
	report := decodeCycleReport(t, output.Bytes())
	if report.CooldownSeconds != 1800 {
		t.Fatalf("expected cooldown seconds 1800, got %d", report.CooldownSeconds)
	}
	if len(report.Plugins) != 1 {
		t.Fatalf("expected 1 plugin report, got %d", len(report.Plugins))
	}
	if report.Plugins[0].SkipReason != "cooldown" {
		t.Fatalf("expected cooldown skip reason, got %q", report.Plugins[0].SkipReason)
	}
	if report.Plugins[0].CooldownRemainingSeconds != 1200 {
		t.Fatalf("expected 1200s cooldown remaining, got %d", report.Plugins[0].CooldownRemainingSeconds)
	}
}

func TestRunOnceCriticalBypassesCooldown(t *testing.T) {
	var output bytes.Buffer
	mock := &reportingPlugin{}
	daemon := newTestDaemon(t, mock, &output)
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	daemon.now = func() time.Time { return now }
	daemon.config.Policy.Cooldown = "30m"
	daemon.config.Policy.StateFile = filepath.Join(t.TempDir(), "state.json")
	state := newCleanupState()
	state.recordPluginRun("reporting", plugins.LevelCritical, now.Add(-10*time.Minute), plugins.CleanupResult{
		Plugin: "reporting",
		Level:  plugins.LevelCritical,
	})
	if err := saveCleanupState(daemon.config.Policy.StateFile, state); err != nil {
		t.Fatal(err)
	}
	daemon.diskStats = sequenceDiskStats(t,
		diskStats(1000, 20, 98),
		diskStats(1000, 20, 98),
		diskStats(1000, 20, 98),
		diskStats(1000, 20, 98),
	)

	if err := daemon.runOnce(context.Background(), monitor.LevelNone); err != nil {
		t.Fatalf("runOnce failed: %v", err)
	}

	if !mock.called {
		t.Fatal("critical cleanup should bypass cooldown")
	}
	report := decodeCycleReport(t, output.Bytes())
	if report.Plugins[0].SkipReason == "cooldown" {
		t.Fatal("critical cleanup should not report cooldown skip")
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
		now:       time.Now,
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
	called    bool
	name      string
	disabled  bool
	supported []string
	result    plugins.CleanupResult
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
	return p.supported
}

func (p *reportingPlugin) Enabled(*config.Config) bool {
	return !p.disabled
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
