package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
	"github.com/Jesssullivan/tinyland-cleanup/monitor"
	"github.com/Jesssullivan/tinyland-cleanup/plugins"
)

// hungChildPlugin starts a child process that never exits and waits for it,
// the way the cache plugin waited on `go env GOCACHE` for sixteen hours.
type hungChildPlugin struct {
	reportingPlugin
	mu   sync.Mutex
	pids []int
}

func (p *hungChildPlugin) Cleanup(ctx context.Context, level plugins.CleanupLevel, _ *config.Config, _ *slog.Logger) plugins.CleanupResult {
	cmd := exec.CommandContext(ctx, "sleep", "86400")
	if err := cmd.Start(); err != nil {
		return plugins.CleanupResult{Plugin: p.Name(), Level: level, Error: err}
	}
	p.mu.Lock()
	p.pids = append(p.pids, cmd.Process.Pid)
	p.mu.Unlock()
	cmd.Wait()
	return plugins.CleanupResult{Plugin: p.Name(), Level: level}
}

func (p *hungChildPlugin) childPIDs() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.pids...)
}

// stuckPlugin ignores its context entirely, like a plugin blocked in a system
// call on a dead mount. release lets the stuck run return.
type stuckPlugin struct {
	reportingPlugin
	mu      sync.Mutex
	calls   int
	release chan struct{}
	plan    bool
}

func (p *stuckPlugin) block() {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	<-p.release
}

func (p *stuckPlugin) Cleanup(_ context.Context, level plugins.CleanupLevel, _ *config.Config, _ *slog.Logger) plugins.CleanupResult {
	p.block()
	return plugins.CleanupResult{Plugin: p.Name(), Level: level, BytesFreed: 7}
}

func (p *stuckPlugin) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type stuckPlanner struct{ stuckPlugin }

func (p *stuckPlanner) PlanCleanup(context.Context, plugins.CleanupLevel, *config.Config, *slog.Logger) plugins.CleanupPlan {
	p.block()
	return plugins.CleanupPlan{Plugin: p.Name(), Summary: "late"}
}

type countingPlugin struct {
	reportingPlugin
	mu    sync.Mutex
	calls int
}

func (p *countingPlugin) Cleanup(_ context.Context, level plugins.CleanupLevel, _ *config.Config, _ *slog.Logger) plugins.CleanupResult {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return plugins.CleanupResult{Plugin: p.Name(), Level: level, BytesFreed: 4096, ItemsCleaned: 1}
}

func (p *countingPlugin) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func newWatchdogDaemon(t *testing.T, output *bytes.Buffer, timeout string, registered ...plugins.Plugin) *daemon {
	t.Helper()
	d := newTestDaemonWithPlugins(t, output, registered...)
	d.config.Policy.PluginTimeout = timeout
	d.pluginGrace = 300 * time.Millisecond
	// 97% used is critical, which bypasses cooldown, so every plugin is due every cycle.
	d.diskStats = sequenceDiskStats(t, diskStats(1000, 30, 97))
	return d
}

func pluginReportNamed(t *testing.T, report cycleReport, name string) pluginCycleReport {
	t.Helper()
	for _, plugin := range report.Plugins {
		if plugin.Name == name {
			return plugin
		}
	}
	t.Fatalf("no report for plugin %q in %+v", name, report.Plugins)
	return pluginCycleReport{}
}

func processGone(pid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// A child that hangs forever no longer holds the daemon: the plugin is
// cancelled at its deadline, the child is killed, and the plugins after it
// run in the same cycle.
func TestCycleProceedsPastPluginWhoseChildHangsForever(t *testing.T) {
	var output bytes.Buffer
	hung := &hungChildPlugin{reportingPlugin: reportingPlugin{name: "cache"}}
	after := &countingPlugin{reportingPlugin: reportingPlugin{name: "dev-artifacts"}}
	d := newWatchdogDaemon(t, &output, "300ms", hung, after)

	started := time.Now()
	report, err := d.runCycle(context.Background(), monitor.LevelNone)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("runCycle failed: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("cycle took %s with a 300ms plugin deadline", elapsed)
	}

	cache := pluginReportNamed(t, report, "cache")
	if cache.Wedged != wedgeCancelled {
		t.Fatalf("cache wedged = %q, want %q (report %+v)", cache.Wedged, wedgeCancelled, cache)
	}
	if !cache.Ran || !strings.Contains(cache.Error, "exceeded its 300ms deadline") {
		t.Fatalf("cache report does not say it was cancelled at its deadline: %+v", cache)
	}
	if got := after.callCount(); got != 1 {
		t.Fatalf("the plugin after the hung one ran %d times, want 1", got)
	}
	if next := pluginReportNamed(t, report, "dev-artifacts"); !next.Ran || next.BytesFreed != 4096 {
		t.Fatalf("the plugin after the hung one did not report its run: %+v", next)
	}
	if report.TotalBytesFreed != 4096 {
		t.Fatalf("total bytes freed = %d, want 4096", report.TotalBytesFreed)
	}

	pids := hung.childPIDs()
	if len(pids) != 1 {
		t.Fatalf("expected one child, got %v", pids)
	}
	if !processGone(pids[0]) {
		t.Fatalf("hung child %d is still alive after the plugin deadline", pids[0])
	}
	if len(d.inFlight) != 0 {
		t.Fatalf("a plugin that returned after cancellation was left in flight: %v", d.inFlight)
	}
}

// A plugin that never returns, whatever its context says, is left behind. The
// cycle finishes, later cycles skip it instead of starting a second copy, and
// it is eligible again once the stuck run returns.
func TestCycleAbandonsPluginThatNeverReturns(t *testing.T) {
	var output bytes.Buffer
	stuck := &stuckPlugin{reportingPlugin: reportingPlugin{name: "lima"}, release: make(chan struct{})}
	after := &countingPlugin{reportingPlugin: reportingPlugin{name: "dev-artifacts"}}
	d := newWatchdogDaemon(t, &output, "200ms", stuck, after)
	released := false
	t.Cleanup(func() {
		if !released {
			close(stuck.release)
		}
	})

	started := time.Now()
	first, err := d.runCycle(context.Background(), monitor.LevelNone)
	if err != nil {
		t.Fatalf("first cycle failed: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("first cycle took %s; deadline 200ms plus grace 300ms", elapsed)
	}
	lima := pluginReportNamed(t, first, "lima")
	if lima.Wedged != wedgeAbandoned || lima.WedgedSince == "" || !lima.Ran {
		t.Fatalf("first cycle lima report = %+v, want wedged %q", lima, wedgeAbandoned)
	}
	if !strings.Contains(lima.Error, "did not return") {
		t.Fatalf("lima error = %q", lima.Error)
	}
	if after.callCount() != 1 {
		t.Fatalf("the plugin after the stuck one ran %d times in the first cycle, want 1", after.callCount())
	}

	second, err := d.runCycle(context.Background(), monitor.LevelNone)
	if err != nil {
		t.Fatalf("second cycle failed: %v", err)
	}
	lima = pluginReportNamed(t, second, "lima")
	if lima.SkipReason != skipReasonPluginWedged || lima.Ran || lima.WouldRun {
		t.Fatalf("second cycle lima report = %+v, want skip_reason %q", lima, skipReasonPluginWedged)
	}
	if stuck.callCount() != 1 {
		t.Fatalf("a second copy of the stuck plugin was started: %d calls", stuck.callCount())
	}
	if after.callCount() != 2 {
		t.Fatalf("the plugin after the stuck one ran %d times over two cycles, want 2", after.callCount())
	}

	close(stuck.release)
	released = true
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case <-d.inFlight["lima"].done:
		default:
			if time.Now().After(deadline) {
				t.Fatal("the released plugin run never finished")
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		break
	}

	third, err := d.runCycle(context.Background(), monitor.LevelNone)
	if err != nil {
		t.Fatalf("third cycle failed: %v", err)
	}
	lima = pluginReportNamed(t, third, "lima")
	if !lima.Ran || lima.Wedged != "" || lima.BytesFreed != 7 {
		t.Fatalf("third cycle lima report = %+v, want a normal run", lima)
	}
	if stuck.callCount() != 2 {
		t.Fatalf("stuck plugin calls = %d, want 2", stuck.callCount())
	}
}

// The same holds for a dry run: a planner that never returns does not stop
// the plan for the plugins after it.
func TestDryRunProceedsPastPlannerThatNeverReturns(t *testing.T) {
	var output bytes.Buffer
	stuck := &stuckPlanner{stuckPlugin{reportingPlugin: reportingPlugin{name: "lima"}, release: make(chan struct{})}}
	t.Cleanup(func() { close(stuck.release) })
	planner := &planningPlugin{
		reportingPlugin: reportingPlugin{name: "dev-artifacts"},
		plan:            plugins.CleanupPlan{Plugin: "dev-artifacts", Summary: "would clean", EstimatedBytesFreed: 99},
	}
	d := newWatchdogDaemon(t, &output, "200ms", stuck, planner)
	d.dryRun = true

	report, err := d.runCycle(context.Background(), monitor.LevelCritical)
	if err != nil {
		t.Fatalf("dry-run cycle failed: %v", err)
	}
	lima := pluginReportNamed(t, report, "lima")
	if lima.Wedged != wedgeAbandoned || lima.Plan != nil {
		t.Fatalf("lima dry-run report = %+v", lima)
	}
	next := pluginReportNamed(t, report, "dev-artifacts")
	if next.Plan == nil || next.Plan.EstimatedBytesFreed != 99 {
		t.Fatalf("the planner after the stuck one produced no plan: %+v", next)
	}
	if report.PlannedEstimatedBytesFreed != 99 {
		t.Fatalf("planned bytes = %d, want 99", report.PlannedEstimatedBytesFreed)
	}
}

// The daemon loop keeps cycling with a plugin wedged: this is the whole point.
func TestDaemonKeepsCyclingWithWedgedPlugin(t *testing.T) {
	var output bytes.Buffer
	stuck := &stuckPlugin{reportingPlugin: reportingPlugin{name: "lima"}, release: make(chan struct{})}
	t.Cleanup(func() { close(stuck.release) })
	after := &countingPlugin{reportingPlugin: reportingPlugin{name: "dev-artifacts"}}
	d := newWatchdogDaemon(t, &output, "100ms", stuck, after)
	d.config.PollInterval = 300

	const cycles = 3
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := 0
	d.after = func(time.Duration) <-chan time.Time {
		waits++
		if waits == cycles {
			cancel()
			return make(chan time.Time)
		}
		fired := make(chan time.Time, 1)
		fired <- time.Now()
		return fired
	}

	finished := make(chan error, 1)
	go func() { finished <- d.run(ctx) }()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run returned %v, want context.Canceled", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the daemon loop did not complete three cycles with one plugin wedged")
	}
	if got := after.callCount(); got != cycles {
		t.Fatalf("the healthy plugin ran %d times over %d cycles", got, cycles)
	}
	if got := stuck.callCount(); got != 1 {
		t.Fatalf("the wedged plugin was started %d times, want 1", got)
	}
	reports := decodeCycleReports(t, output.Bytes())
	if len(reports) != cycles {
		t.Fatalf("daemon wrote %d cycle reports, want %d", len(reports), cycles)
	}
	if got := pluginReportNamed(t, reports[cycles-1], "lima").SkipReason; got != skipReasonPluginWedged {
		t.Fatalf("last cycle lima skip_reason = %q, want %q", got, skipReasonPluginWedged)
	}
}

func TestPluginTimeoutPolicy(t *testing.T) {
	d := &daemon{config: config.DefaultConfig()}
	if got := d.pluginTimeout("nix"); got != defaultPluginTimeout {
		t.Fatalf("default plugin timeout = %s, want %s", got, defaultPluginTimeout)
	}
	d.config.Policy.PluginTimeout = "10m"
	d.config.Policy.PluginTimeouts = map[string]string{"nix": "2h", "bazel": "nonsense"}
	if got := d.pluginTimeout("cache"); got != 10*time.Minute {
		t.Fatalf("configured plugin timeout = %s, want 10m", got)
	}
	if got := d.pluginTimeout("nix"); got != 2*time.Hour {
		t.Fatalf("per-plugin timeout = %s, want 2h", got)
	}
	if got := d.pluginTimeout("bazel"); got != 10*time.Minute {
		t.Fatalf("an invalid per-plugin timeout gave %s, want the 10m fallback", got)
	}

	probe, reclaim := d.execTimeouts("cache")
	if probe != plugins.DefaultExecTimeout || reclaim != plugins.DefaultExecReclaimTimeout {
		t.Fatalf("default exec timeouts = %s, %s", probe, reclaim)
	}
	d.config.Policy.ExecTimeout = "20s"
	d.config.Policy.ExecReclaimTimeout = "5m"
	d.config.Policy.ExecTimeouts = map[string]string{"cache": "5s"}
	if probe, reclaim = d.execTimeouts("cache"); probe != 5*time.Second || reclaim != 5*time.Minute {
		t.Fatalf("cache exec timeouts = %s, %s; want 5s, 5m", probe, reclaim)
	}
	if probe, _ = d.execTimeouts("nix"); probe != 20*time.Second {
		t.Fatalf("nix probe timeout = %s, want 20s", probe)
	}
}

func TestApplyMemoryLimit(t *testing.T) {
	none := func(string) string { return "" }
	cfg := config.DefaultConfig()
	t.Cleanup(func() { applyMemoryLimit(&config.Config{Policy: config.PolicyConfig{MemoryLimitMB: 1 << 30}}, none) })

	if got := applyMemoryLimit(cfg, none); got != defaultMemoryLimitMB*1024*1024 {
		t.Fatalf("default limit = %d, want %d MiB", got, defaultMemoryLimitMB)
	}
	cfg.Policy.MemoryLimitMB = 96
	if got := applyMemoryLimit(cfg, none); got != 96*1024*1024 {
		t.Fatalf("configured limit = %d, want 96 MiB", got)
	}
	cfg.Policy.MemoryLimitMB = -1
	if got := applyMemoryLimit(cfg, none); got != 0 {
		t.Fatalf("a negative limit set %d, want no limit", got)
	}
	cfg.Policy.MemoryLimitMB = 96
	env := func(key string) string {
		if key == "GOMEMLIMIT" {
			return "1GiB"
		}
		return ""
	}
	if got := applyMemoryLimit(cfg, env); got != 0 {
		t.Fatalf("GOMEMLIMIT in the environment was overridden with %d", got)
	}
}
