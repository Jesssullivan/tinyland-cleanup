package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
	"github.com/Jesssullivan/tinyland-cleanup/monitor"
	"github.com/Jesssullivan/tinyland-cleanup/plugins"
)

// The tests in this file pin the bounds on a mount that sits at critical for
// reasons the enabled plugins cannot relieve (lab sting /srv/fast-local,
// 2026-10-17: a planned XFS-project fill, a level-independent Nix GC freeing
// /nix every hour, and another writer's churn on the monitored mount).

// newCriticalBoundDaemon returns a daemon pinned at critical byte pressure
// with one zero-yield ladder plugin and, when withIndependent is set, one
// level-independent plugin that frees a GiB on every run.
func newCriticalBoundDaemon(t *testing.T, withIndependent bool) (*daemon, *reportingPlugin, *levelIndependentPlugin, *fakeClock, *bytes.Buffer) {
	t.Helper()
	var output bytes.Buffer
	ladder := &reportingPlugin{name: "dev-artifacts"}
	registered := []plugins.Plugin{ladder}
	var independent *levelIndependentPlugin
	if withIndependent {
		independent = &levelIndependentPlugin{reportingPlugin{name: "nix"}}
		independent.result = plugins.CleanupResult{BytesFreed: int64(gib), ItemsCleaned: 1}
		registered = append(registered, independent)
	}
	d := newTestDaemonWithPlugins(t, &output, registered...)
	d.config.Policy.ZeroYieldLimit = -1
	clock := &fakeClock{t: time.Date(2026, 10, 17, 21, 0, 0, 0, time.UTC)}
	d.now = clock.Now
	d.diskStats = sequenceDiskStats(t, bytePressureStats(25, 96))
	return d, ladder, independent, clock, &output
}

func ladderReport(t *testing.T, rep cycleReport, name string) pluginCycleReport {
	t.Helper()
	for _, p := range rep.Plugins {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no report for plugin %q in %+v", name, rep.Plugins)
	return pluginCycleReport{}
}

func TestLevelIndependentBytesDoNotReleaseByteBackoff(t *testing.T) {
	d, ladder, independent, clock, output := newCriticalBoundDaemon(t, true)
	limit := d.byteNoProgressLimit()

	for i := 0; i < limit; i++ {
		rep := runByteCycle(t, d, output)
		if !ladderReport(t, rep, "dev-artifacts").Ran {
			t.Fatalf("cycle %d: the ladder plugin should run before backoff", i)
		}
		if rep.LadderBytesFreed != 0 || rep.TotalBytesFreed != int64(gib) {
			t.Fatalf("cycle %d: ladder_bytes_freed=%d total=%d, want 0 and the independent GiB", i, rep.LadderBytesFreed, rep.TotalBytesFreed)
		}
		clock.Advance(5 * time.Minute)
	}

	// The independent plugin keeps freeing a GiB elsewhere every cycle. Byte
	// backoff must engage and stay engaged across a whole interval anyway.
	for i := 0; i < 12; i++ {
		ladder.called = false
		independent.called = false
		rep := runByteCycle(t, d, output)
		if !rep.ByteBackoff {
			t.Fatalf("backoff cycle %d: byte backoff released by level-independent bytes (count=%d)", i, rep.ByteNoProgressCount)
		}
		if !independent.called {
			t.Fatalf("backoff cycle %d: a level-independent plugin must still run every cycle", i)
		}
		if !ladderReport(t, rep, "nix").LevelIndependent {
			t.Fatalf("backoff cycle %d: the independent plugin is not marked level_independent", i)
		}
		clock.Advance(5 * time.Minute)
	}
}

func TestOnlyLadderRunsAdvanceTheNoProgressCount(t *testing.T) {
	d, _, _, clock, output := newCriticalBoundDaemon(t, true)
	limit := d.byteNoProgressLimit()
	for i := 0; i <= limit; i++ {
		runByteCycle(t, d, output)
		clock.Advance(5 * time.Minute)
	}
	// Backoff is engaged and the ladder plugin was held back on the last
	// cycle; the independent plugin ran. The count must not have moved.
	before := runByteCycle(t, d, output)
	if ladderReport(t, before, "dev-artifacts").Ran {
		t.Fatal("expected the ladder plugin to be held back by byte backoff")
	}
	clock.Advance(5 * time.Minute)
	after := runByteCycle(t, d, output)
	if after.ByteNoProgressCount != before.ByteNoProgressCount {
		t.Fatalf("a cycle with only level-independent runs advanced the count: %d -> %d", before.ByteNoProgressCount, after.ByteNoProgressCount)
	}
}

// growingFreeStats returns disk stats at a fixed used percentage whose free
// bytes grow by step on every read, so every cycle sees a positive host
// free-space delta: another writer deleting its own files.
func growingFreeStats(step uint64) func(string) (*monitor.DiskStats, error) {
	var mu sync.Mutex
	free := 25 * gib
	return func(path string) (*monitor.DiskStats, error) {
		mu.Lock()
		defer mu.Unlock()
		free += step
		s := bytePressureStats(free/gib, 96)
		s.Free = free
		s.Path = path
		return s, nil
	}
}

func TestHostDeltaWithoutALadderRunIsNotProgress(t *testing.T) {
	d, ladder, _, clock, output := newCriticalBoundDaemon(t, false)
	limit := d.byteNoProgressLimit()
	for i := 0; i <= limit; i++ {
		runByteCycle(t, d, output)
		clock.Advance(5 * time.Minute)
	}
	d.diskStats = growingFreeStats(gib)
	for i := 0; i < 4; i++ {
		ladder.called = false
		rep := runByteCycle(t, d, output)
		if ladder.called {
			// The interval passed; the attempt itself is not under test.
			clock.Advance(5 * time.Minute)
			continue
		}
		if rep.HostFreeDeltaBytes < d.byteProgressMinBytes() {
			t.Fatalf("cycle %d: test setup should produce a host delta, got %d", i, rep.HostFreeDeltaBytes)
		}
		clock.Advance(5 * time.Minute)
		next := runByteCycle(t, d, output)
		if !next.ByteBackoff {
			t.Fatalf("cycle %d: another writer's free-space delta released byte backoff while every ladder plugin was held back", i)
		}
		clock.Advance(5 * time.Minute)
	}
}

func TestHostDeltaWithALadderRunIsProgress(t *testing.T) {
	d, _, _, clock, output := newCriticalBoundDaemon(t, false)
	d.diskStats = growingFreeStats(gib)
	for i := 0; i < 2*d.byteNoProgressLimit(); i++ {
		rep := runByteCycle(t, d, output)
		if rep.ByteBackoff || rep.ByteNoProgressCount != 0 {
			t.Fatalf("cycle %d: a ladder run with a host delta is progress, got backoff=%v count=%d", i, rep.ByteBackoff, rep.ByteNoProgressCount)
		}
		clock.Advance(5 * time.Minute)
	}
}

func TestByteBackoffIntervalEscalatesWithoutYield(t *testing.T) {
	d, ladder, _, clock, output := newCriticalBoundDaemon(t, false)
	d.config.Policy.Cooldown = "30m"
	d.config.Policy.ByteBackoffMax = "4h"

	var runs []time.Time
	var intervals []int64
	for i := 0; i < 12*24; i++ { // 24 hours of 5-minute polls
		ladder.called = false
		rep := runByteCycle(t, d, output)
		if ladder.called {
			runs = append(runs, clock.Now())
			if rep.ByteBackoff {
				intervals = append(intervals, rep.ByteBackoffSeconds)
			}
		}
		clock.Advance(5 * time.Minute)
	}
	limit := d.byteNoProgressLimit()
	if len(runs) < limit+5 {
		t.Fatalf("expected at least %d runs in a day, got %d", limit+5, len(runs))
	}
	want := []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 4 * time.Hour}
	for i, w := range want {
		gap := runs[limit+i].Sub(runs[limit+i-1])
		if gap < w || gap > w+5*time.Minute {
			t.Fatalf("gap %d after backoff engaged = %s, want %s (runs %v)", i, gap, w, runs)
		}
	}
	if intervals[0] != int64((30 * time.Minute).Seconds()) {
		t.Fatalf("first engaged interval = %ds, want 1800", intervals[0])
	}

	// A productive attempt resets the escalation.
	ladder.result = plugins.CleanupResult{BytesFreed: d.byteProgressMinBytes()}
	for i := 0; i < 12*5; i++ {
		ladder.called = false
		runByteCycle(t, d, output)
		clock.Advance(5 * time.Minute)
		if ladder.called {
			break
		}
	}
	ladder.result = plugins.CleanupResult{}
	rep := runByteCycle(t, d, output)
	if rep.ByteBackoff || rep.ByteNoProgressCount != 0 {
		t.Fatalf("progress must reset the count and release backoff, got backoff=%v count=%d", rep.ByteBackoff, rep.ByteNoProgressCount)
	}
}

func TestByteBackoffIntervalForDefaultsDoNotGrow(t *testing.T) {
	d, _, _, _, _ := newCriticalBoundDaemon(t, false)
	limit := d.byteNoProgressLimit()
	for n := limit; n < limit+10; n++ {
		if got := d.byteBackoffIntervalFor(n); got != 30*time.Minute {
			t.Fatalf("defaults: interval for %d no-progress attempts = %s, want 30m", n, got)
		}
	}
	d.config.Policy.Cooldown = "10m"
	d.config.Policy.ByteBackoffMax = "1h"
	cases := map[int]time.Duration{limit: 10 * time.Minute, limit + 1: 20 * time.Minute, limit + 2: 40 * time.Minute, limit + 3: time.Hour, limit + 9: time.Hour}
	for n, want := range cases {
		if got := d.byteBackoffIntervalFor(n); got != want {
			t.Fatalf("interval for %d = %s, want %s", n, got, want)
		}
	}
}

func TestMountBandKeepsGlobalStepsOutOfAMountsWarningBand(t *testing.T) {
	var output bytes.Buffer
	d := newTestDaemonWithPlugins(t, &output)
	d.config.Thresholds = config.Thresholds{Warning: 75, Moderate: 85, Aggressive: 90, Critical: 95}

	levelAt := func(mount config.MountConfig, used float64) monitor.CleanupLevel {
		return d.monitorForMount(mount).CheckByteLevel(diskStats(100*gib, uint64(100-used)*gib, used))
	}
	cases := []struct {
		name  string
		mount config.MountConfig
		used  float64
		want  monitor.CleanupLevel
	}{
		// A band above the global moderate and aggressive steps: no action
		// below its own warning line, report only inside it.
		{"high band below warning", config.MountConfig{Path: "/p", ThresholdWarning: 92, ThresholdCritical: 98}, 91, monitor.LevelNone},
		{"high band in warning", config.MountConfig{Path: "/p", ThresholdWarning: 92, ThresholdCritical: 98}, 95, monitor.LevelWarning},
		{"high band critical", config.MountConfig{Path: "/p", ThresholdWarning: 92, ThresholdCritical: 98}, 98, monitor.LevelCritical},
		// Explicit steps inside the band.
		{"explicit moderate", config.MountConfig{Path: "/p", ThresholdWarning: 90, ThresholdModerate: 92, ThresholdAggressive: 95, ThresholdCritical: 98}, 93, monitor.LevelModerate},
		{"explicit aggressive", config.MountConfig{Path: "/p", ThresholdWarning: 90, ThresholdModerate: 92, ThresholdAggressive: 95, ThresholdCritical: 98}, 96, monitor.LevelAggressive},
		// Rows that exist today keep their levels: a low band (sting
		// fast-local 70/75) and a band that contains the global steps
		// (sting estate 85/95).
		{"low band warning", config.MountConfig{Path: "/p", ThresholdWarning: 70, ThresholdCritical: 75}, 72, monitor.LevelWarning},
		{"low band critical", config.MountConfig{Path: "/p", ThresholdWarning: 70, ThresholdCritical: 75}, 84, monitor.LevelCritical},
		{"wide band moderate", config.MountConfig{Path: "/p", ThresholdWarning: 85, ThresholdCritical: 95}, 86, monitor.LevelModerate},
		{"wide band aggressive", config.MountConfig{Path: "/p", ThresholdWarning: 85, ThresholdCritical: 95}, 91, monitor.LevelAggressive},
	}
	for _, tc := range cases {
		if got := levelAt(tc.mount, tc.used); got != tc.want {
			t.Errorf("%s at %.0f%%: level %s, want %s", tc.name, tc.used, got, tc.want)
		}
	}
}

func TestMountTargetUsedPercent(t *testing.T) {
	var output bytes.Buffer
	ladder := &reportingPlugin{name: "dev-artifacts"}
	d := newTestDaemonWithPlugins(t, &output, ladder)
	d.config.TargetFree = 70
	d.config.MonitoredMounts = []config.MountConfig{
		{Path: "/srv/fast-local/jess", Label: "devseat-177", ThresholdWarning: 90, ThresholdCritical: 98, TargetUsedPercent: 92},
	}
	stats := diskStats(1450*gib, 100*gib, 93.1)
	d.diskStats = func(path string) (*monitor.DiskStats, error) {
		cp := *stats
		cp.Path = path
		return &cp, nil
	}
	rep := runByteCycle(t, d, &output)
	if rep.TargetUsedPercent != 92 {
		t.Fatalf("target_used_percent = %d, want the mount's 92", rep.TargetUsedPercent)
	}
	if rep.TargetFreeMet {
		t.Fatal("93.1% used must not meet a 92% target")
	}
	// 1450 GiB at 92% used needs 116 GiB free; 100 GiB free is a 16 GiB deficit.
	if want := int64(1450*gib*8/100 - 100*gib); rep.TargetFreeDeficitBytes != want {
		t.Fatalf("deficit = %d, want %d", rep.TargetFreeDeficitBytes, want)
	}

	*stats = *diskStats(1450*gib, 120*gib, 91.7)
	rep = runByteCycle(t, d, &output)
	if !rep.TargetFreeMet {
		t.Fatalf("91.7%% used meets a 92%% target, got deficit %d", rep.TargetFreeDeficitBytes)
	}
}

func TestTargetUsedPercentFlagReplacesMountTargets(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MonitoredMounts = []config.MountConfig{{Path: "/a", TargetUsedPercent: 92}}
	if err := applyTargetUsedPercentOverride(cfg, 80); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetFree != 80 || cfg.MonitoredMounts[0].TargetUsedPercent != 0 {
		t.Fatalf("the flag must set target_free and clear mount targets, got %d and %d", cfg.TargetFree, cfg.MonitoredMounts[0].TargetUsedPercent)
	}
}

func TestLoadConfigRejectsAMountLadderThatDescends(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"descending": "monitored_mounts:\n  - path: /a\n    threshold_warning: 92\n    threshold_moderate: 90\n",
		"over 100":   "monitored_mounts:\n  - path: /a\n    threshold_critical: 101\n",
		"target 100": "monitored_mounts:\n  - path: /a\n    target_used_percent: 100\n",
	}
	for name, body := range cases {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := config.LoadConfig(path); err == nil {
			t.Errorf("%s: LoadConfig accepted an invalid mount", name)
		}
	}
	ok := filepath.Join(dir, "ok.yaml")
	body := "monitored_mounts:\n  - path: /a\n    threshold_warning: 90\n    threshold_moderate: 92\n    threshold_aggressive: 95\n    threshold_critical: 98\n    target_used_percent: 92\n"
	if err := os.WriteFile(ok, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadConfig(ok); err != nil {
		t.Fatalf("a full ascending ladder must load: %v", err)
	}
}
