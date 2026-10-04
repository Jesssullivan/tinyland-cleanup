package main

import (
	"bytes"
	"context"
	"math"
	"testing"
	"testing/quick"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/monitor"
	"github.com/Jesssullivan/tinyland-cleanup/plugins"
)

// newZeroYieldDaemon returns a daemon whose single plugin reclaims nothing,
// with an unmet minimum_free_gb runway (so cooldown is bypassed) and byte
// backoff off, so without zero-yield suppression every cycle runs the plugin.
func newZeroYieldDaemon(t *testing.T, stats *monitor.DiskStats, plugin plugins.Plugin) (*daemon, *fakeClock, *bytes.Buffer) {
	t.Helper()
	var output bytes.Buffer
	d := newTestDaemonWithPlugins(t, &output, plugin)
	d.config.Policy.MinimumFreeGB = 40
	d.config.Policy.ByteNoProgressLimit = -1
	d.config.TargetFree = 70
	clock := &fakeClock{t: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)}
	d.now = clock.Now
	d.diskStats = sequenceDiskStats(t, stats)
	return d, clock, &output
}

func onlyPlugin(t *testing.T, rep cycleReport) pluginCycleReport {
	t.Helper()
	if len(rep.Plugins) != 1 {
		t.Fatalf("expected one plugin report, got %+v", rep.Plugins)
	}
	return rep.Plugins[0]
}

// suppress runs the zero-yield limit's worth of cycles five minutes apart and
// checks the plugin ends up suppressed.
func suppress(t *testing.T, d *daemon, clock *fakeClock, output *bytes.Buffer) {
	t.Helper()
	limit := d.zeroYieldPolicy().limit
	for i := 0; i < limit; i++ {
		if p := onlyPlugin(t, runByteCycle(t, d, output)); !p.Ran {
			t.Fatalf("cycle %d: expected the plugin to run before the limit, got %+v", i, p)
		}
		clock.Advance(5 * time.Minute)
	}
	if p := onlyPlugin(t, runByteCycle(t, d, output)); p.Ran || p.SkipReason != "zero_yield_backoff" {
		t.Fatalf("expected the plugin to be suppressed after %d zero-yield runs, got %+v", limit, p)
	}
}

func TestZeroYieldSuppressedAfterLimit(t *testing.T) {
	d, clock, output := newZeroYieldDaemon(t, bytePressureStats(25, 95), &reportingPlugin{name: "dev-artifacts"})
	start := clock.Now()

	p := onlyPlugin(t, runByteCycle(t, d, output))
	if !p.Ran || p.ZeroYieldCount != 1 || p.SuppressedUntil != "" {
		t.Fatalf("first zero-yield run must not suppress, got %+v", p)
	}
	clock.Advance(5 * time.Minute)
	rep := runByteCycle(t, d, output)
	p = onlyPlugin(t, rep)
	wantUntil := start.Add(5*time.Minute + 30*time.Minute).Format(time.RFC3339)
	if !p.Ran || p.ZeroYieldCount != 2 || p.SuppressedUntil != wantUntil {
		t.Fatalf("second zero-yield run must suppress until %s, got %+v", wantUntil, p)
	}
	if rep.NextRetryAt != wantUntil {
		t.Fatalf("next_retry_at = %q, want %q", rep.NextRetryAt, wantUntil)
	}

	clock.Advance(5 * time.Minute)
	rep = runByteCycle(t, d, output)
	p = onlyPlugin(t, rep)
	if p.Ran || p.SkipReason != "zero_yield_backoff" || p.RetryAt != wantUntil || p.ZeroYieldCount != 2 {
		t.Fatalf("expected a zero_yield_backoff skip until %s, got %+v", wantUntil, p)
	}
	if rep.NextRetryAt != wantUntil {
		t.Fatalf("next_retry_at = %q, want %q", rep.NextRetryAt, wantUntil)
	}

	// At the retry time the plugin runs once more and the interval doubles.
	clock.Advance(25 * time.Minute)
	p = onlyPlugin(t, runByteCycle(t, d, output))
	if want := clock.Now().Add(time.Hour).Format(time.RFC3339); !p.Ran || p.ZeroYieldCount != 3 || p.SuppressedUntil != want {
		t.Fatalf("expected a run at the retry time suppressed until %s, got %+v", want, p)
	}
}

func TestZeroYieldResetsOnYield(t *testing.T) {
	plugin := &reportingPlugin{name: "dev-artifacts"}
	d, clock, output := newZeroYieldDaemon(t, bytePressureStats(25, 95), plugin)
	plugin.result = plugins.CleanupResult{BytesFreed: d.byteProgressMinBytes()}
	for i := 0; i < 6; i++ {
		if p := onlyPlugin(t, runByteCycle(t, d, output)); !p.Ran || p.ZeroYieldCount != 0 {
			t.Fatalf("cycle %d: a plugin with yield is never suppressed, got %+v", i, p)
		}
		clock.Advance(5 * time.Minute)
	}
	// A sub-threshold reclaim counts as zero yield.
	plugin.result = plugins.CleanupResult{BytesFreed: 6 * 1024 * 1024, ItemsCleaned: 3}
	suppress(t, d, clock, output)
}

func TestZeroYieldLiftedWhenLevelRises(t *testing.T) {
	stats := bytePressureStats(37, 92) // aggressive
	d, clock, output := newZeroYieldDaemon(t, stats, &reportingPlugin{name: "dev-artifacts"})
	suppress(t, d, clock, output)

	*stats = *bytePressureStats(25, 95) // critical
	clock.Advance(time.Minute)
	p := onlyPlugin(t, runByteCycle(t, d, output))
	if !p.Ran || p.ZeroYieldLifted != "level_rose" {
		t.Fatalf("an escalation must lift suppression, got %+v", p)
	}
	// It is suppressed again at the new level once it yields nothing there.
	clock.Advance(time.Minute)
	if p := onlyPlugin(t, runByteCycle(t, d, output)); p.Ran || p.SkipReason != "zero_yield_backoff" {
		t.Fatalf("expected suppression at the new level, got %+v", p)
	}
}

func TestZeroYieldLiftedWhenConfigChanges(t *testing.T) {
	d, clock, output := newZeroYieldDaemon(t, bytePressureStats(25, 95), &reportingPlugin{name: "dev-artifacts"})
	suppress(t, d, clock, output)

	d.config.Policy.Cooldown = "45m" // e.g. a Home Manager switch with new policy
	clock.Advance(time.Minute)
	p := onlyPlugin(t, runByteCycle(t, d, output))
	if !p.Ran || p.ZeroYieldLifted != "config_changed" || p.ZeroYieldCount != 1 {
		t.Fatalf("a config change must lift suppression and restart the count, got %+v", p)
	}
}

func TestZeroYieldLiftedWhenPressureClears(t *testing.T) {
	stats := bytePressureStats(25, 95)
	d, clock, output := newZeroYieldDaemon(t, stats, &reportingPlugin{name: "dev-artifacts"})
	suppress(t, d, clock, output)

	*stats = *bytePressureStats(160, 65) // pressure clears
	clock.Advance(time.Minute)
	runByteCycle(t, d, output)

	*stats = *bytePressureStats(25, 95) // a new episode at the same level
	clock.Advance(time.Minute)
	p := onlyPlugin(t, runByteCycle(t, d, output))
	if !p.Ran {
		t.Fatalf("a new pressure episode earns one fresh run, got %+v", p)
	}
	// The count was kept, so one more zero-yield run suppresses it again with
	// a doubled interval.
	if want := clock.Now().Add(time.Hour).Format(time.RFC3339); p.ZeroYieldCount != 3 || p.SuppressedUntil != want {
		t.Fatalf("expected count 3 suppressed until %s, got %+v", want, p)
	}
}

func TestZeroYieldSurvivesRestart(t *testing.T) {
	d, clock, output := newZeroYieldDaemon(t, bytePressureStats(25, 95), &reportingPlugin{name: "dev-artifacts"})
	suppress(t, d, clock, output)

	restarted, _, output2 := newZeroYieldDaemon(t, bytePressureStats(25, 95), &reportingPlugin{name: "dev-artifacts"})
	restarted.config = d.config
	restarted.now = clock.Now
	if p := onlyPlugin(t, runByteCycle(t, restarted, output2)); p.Ran || p.SkipReason != "zero_yield_backoff" {
		t.Fatalf("suppression must survive a restart with the same config, got %+v", p)
	}
}

type safetyCriticalPlugin struct{ reportingPlugin }

func (p *safetyCriticalPlugin) SafetyCritical() bool { return true }

func TestZeroYieldExemptions(t *testing.T) {
	t.Run("safety critical", func(t *testing.T) {
		d, clock, output := newZeroYieldDaemon(t, bytePressureStats(25, 95),
			&safetyCriticalPlugin{reportingPlugin{name: "apfs-snapshots"}})
		for i := 0; i < 5; i++ {
			p := onlyPlugin(t, runByteCycle(t, d, output))
			if !p.Ran {
				t.Fatalf("cycle %d: a safety-critical plugin is never suppressed, got %+v", i, p)
			}
			if i >= d.zeroYieldPolicy().limit && p.ZeroYieldLifted != "safety_critical" {
				t.Fatalf("cycle %d: expected zero_yield_lifted=safety_critical, got %+v", i, p)
			}
			clock.Advance(5 * time.Minute)
		}
	})

	t.Run("exempt list", func(t *testing.T) {
		d, clock, output := newZeroYieldDaemon(t, bytePressureStats(25, 95), &reportingPlugin{name: "dev-artifacts"})
		d.config.Policy.ZeroYieldExemptPlugins = []string{"dev-artifacts"}
		for i := 0; i < 5; i++ {
			if p := onlyPlugin(t, runByteCycle(t, d, output)); !p.Ran {
				t.Fatalf("cycle %d: an exempt plugin is never suppressed, got %+v", i, p)
			}
			clock.Advance(5 * time.Minute)
		}
	})

	lifted := map[string]func(d *daemon, stats *monitor.DiskStats) monitor.CleanupLevel{
		"below_emergency_floor": func(_ *daemon, stats *monitor.DiskStats) monitor.CleanupLevel {
			*stats = *bytePressureStats(15, 97) // same critical level, under 20 GiB
			return monitor.LevelNone
		},
		"operator_run": func(*daemon, *monitor.DiskStats) monitor.CleanupLevel {
			return monitor.LevelCritical // --level critical
		},
	}
	for reason, setup := range lifted {
		t.Run(reason, func(t *testing.T) {
			stats := bytePressureStats(25, 95)
			d, clock, output := newZeroYieldDaemon(t, stats, &reportingPlugin{name: "dev-artifacts"})
			suppress(t, d, clock, output)
			forced := setup(d, stats)
			clock.Advance(time.Minute)
			output.Reset()
			if err := d.runOnce(context.Background(), forced); err != nil {
				t.Fatal(err)
			}
			if p := onlyPlugin(t, decodeCycleReport(t, output.Bytes())); !p.Ran || p.ZeroYieldLifted != reason {
				t.Fatalf("expected the plugin to run with zero_yield_lifted=%s, got %+v", reason, p)
			}
		})
	}

	t.Run("plugin filter", func(t *testing.T) {
		d, clock, output := newZeroYieldDaemon(t, bytePressureStats(25, 95), &reportingPlugin{name: "dev-artifacts"})
		suppress(t, d, clock, output)
		d.pluginFilter = []string{"dev-artifacts"}
		clock.Advance(time.Minute)
		if p := onlyPlugin(t, runByteCycle(t, d, output)); !p.Ran || p.ZeroYieldLifted != "operator_run" {
			t.Fatalf("an explicit --plugins run must lift suppression, got %+v", p)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		d, clock, output := newZeroYieldDaemon(t, bytePressureStats(25, 95), &reportingPlugin{name: "dev-artifacts"})
		d.config.Policy.ZeroYieldLimit = -1
		for i := 0; i < 6; i++ {
			if p := onlyPlugin(t, runByteCycle(t, d, output)); !p.Ran || p.SuppressedUntil != "" {
				t.Fatalf("cycle %d: a negative limit disables suppression, got %+v", i, p)
			}
			clock.Advance(5 * time.Minute)
		}
	})
}

func TestRecordPluginRunPreservesZeroYieldFields(t *testing.T) {
	s := newCleanupState()
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	policy := zeroYieldPolicy{limit: 1, base: time.Hour, max: 6 * time.Hour}
	s.recordPluginRun("p", plugins.LevelCritical, now, plugins.CleanupResult{})
	want := s.recordPluginYield("p", true, now, "digest", policy)
	s.recordPluginRun("p", plugins.LevelCritical, now.Add(time.Minute), plugins.CleanupResult{ItemsCleaned: 1})
	got := s.Plugins["p"]
	if got.ZeroYieldCount != want.ZeroYieldCount || got.SuppressedUntil != want.SuppressedUntil || got.ConfigDigest != "digest" {
		t.Fatalf("recordPluginRun must merge, not replace: want %+v, got %+v", want, got)
	}
	if got.LastItemsCleaned != 1 {
		t.Fatalf("recordPluginRun must still update the run fields, got %+v", got)
	}
}

// TestZeroYieldSoakUnderPinnedBytePressure is the headline proof for
// TIN-3342 criterion 4: 24 simulated hours at 95% used with the
// minimum_free_gb runway unmet and nothing reclaimable, at a 5 minute poll,
// with byte backoff and zero-yield suppression at their defaults. v0.4.1 runs
// the plugin every cycle (288 runs); here it runs a handful of times.
func TestZeroYieldSoakUnderPinnedBytePressure(t *testing.T) {
	runs := soakRuns(t, 5*time.Minute, 24*time.Hour)
	if len(runs) > soakRunBound(defaultPolicyForTest(), 24*time.Hour) {
		t.Fatalf("zero-yield plugin ran %d times in 24h, want at most %d: %v",
			len(runs), soakRunBound(defaultPolicyForTest(), 24*time.Hour), runs)
	}
	if len(runs) < 3 {
		t.Fatalf("the plugin must still be retried, got %d runs", len(runs))
	}
	t.Logf("zero-yield plugin ran %d times in 24h at a 5m poll (every cycle would be 288): %v", len(runs), runs)
}

func defaultPolicyForTest() zeroYieldPolicy {
	return zeroYieldPolicy{limit: defaultZeroYieldLimit, base: defaultZeroYieldBackoffBase, max: defaultZeroYieldBackoffMax}
}

// soakRunBound is the most runs the schedule allows over horizon: the runs up
// to the limit, the doubling steps up to the cap, then one run per cap, plus
// one for the run in progress at the end.
func soakRunBound(policy zeroYieldPolicy, horizon time.Duration) int {
	steps := int(math.Ceil(math.Log2(float64(policy.max) / float64(policy.base))))
	return policy.limit + steps + int(math.Ceil(float64(horizon)/float64(policy.max))) + 1
}

func soakRuns(t *testing.T, poll, horizon time.Duration) []time.Time {
	t.Helper()
	var output bytes.Buffer
	d := newTestDaemonWithPlugins(t, &output, &reportingPlugin{name: "dev-artifacts"})
	d.config.Policy.MinimumFreeGB = 40
	d.config.TargetFree = 70
	clock := &fakeClock{t: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)}
	d.now = clock.Now
	d.diskStats = sequenceDiskStats(t, bytePressureStats(25, 95))
	var runs []time.Time
	for end := clock.Now().Add(horizon); clock.Now().Before(end); clock.Advance(poll) {
		if pluginRan(runByteCycle(t, d, &output)) {
			runs = append(runs, clock.Now())
		}
	}
	return runs
}

// TestPropertyZeroYieldSoak generalises the soak over the poll interval: the
// run count stays within the schedule's bound, and once suppressed the plugin
// is never retried sooner than the base interval.
func TestPropertyZeroYieldSoak(t *testing.T) {
	policy := defaultPolicyForTest()
	property := func(pollMinutes uint8) bool {
		poll := time.Duration(pollMinutes%26+5) * time.Minute
		runs := soakRuns(t, poll, 24*time.Hour)
		if len(runs) > soakRunBound(policy, 24*time.Hour) || len(runs) < policy.limit {
			return false
		}
		for i := policy.limit; i < len(runs); i++ {
			if runs[i].Sub(runs[i-1]) < policy.base {
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 4}); err != nil {
		t.Fatal(err)
	}
}

// TestPropertyZeroYieldBackoffSchedule: the suppression interval is zero
// below the limit, within [base, max] from it on, never shrinks as the count
// grows, and reaches the cap.
func TestPropertyZeroYieldBackoffSchedule(t *testing.T) {
	property := func(limit, baseMinutes, capFactor, count uint8) bool {
		policy := zeroYieldPolicy{
			limit: int(limit%5) + 1,
			base:  time.Duration(baseMinutes%120+1) * time.Minute,
		}
		policy.max = policy.base * time.Duration(capFactor%64+1)
		n := int(count % 80)
		got := zeroYieldBackoff(n, policy)
		if n < policy.limit {
			return got == 0
		}
		next := zeroYieldBackoff(n+1, policy)
		return got >= policy.base && got <= policy.max && next >= got &&
			zeroYieldBackoff(policy.limit+64, policy) == policy.max
	}
	if err := quick.Check(property, nil); err != nil {
		t.Fatal(err)
	}
}

// TestPropertyRecordPluginYield: for any sequence of runs the count equals
// the zero-yield streak since the last yield or config change, and the plugin
// is suppressed exactly when that streak reaches the limit.
func TestPropertyRecordPluginYield(t *testing.T) {
	policy := zeroYieldPolicy{limit: 2, base: 30 * time.Minute, max: 6 * time.Hour}
	property := func(steps []uint8) bool {
		s := newCleanupState()
		now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
		digest, streak := "a", 0
		for _, step := range steps {
			zero := step&1 == 0
			if step&2 != 0 {
				digest += "x"
				streak = 0
			}
			if zero {
				streak++
			} else {
				streak = 0
			}
			record := s.recordPluginYield("p", zero, now, digest, policy)
			if record.ZeroYieldCount != streak {
				return false
			}
			_, _, _, suppressed := s.zeroYieldSuppression("p", plugins.LevelNone, now, digest)
			if suppressed != (streak >= policy.limit) {
				return false
			}
			now = now.Add(time.Minute)
		}
		return true
	}
	if err := quick.Check(property, nil); err != nil {
		t.Fatal(err)
	}
}

func TestTextReportShowsZeroYield(t *testing.T) {
	var out bytes.Buffer
	report := cycleReport{
		Level:       "critical",
		NextRetryAt: "2026-10-04T00:35:00Z",
		Plugins: []pluginCycleReport{
			{Name: "dev-artifacts", SkipReason: "zero_yield_backoff", RetryAt: "2026-10-04T00:35:00Z"},
			{Name: "bazel", Ran: true, ZeroYieldCount: 2, SuppressedUntil: "2026-10-04T00:40:00Z"},
			{Name: "cache", Ran: true, ZeroYieldLifted: "level_rose"},
		},
	}
	if err := writeTextReport(&out, report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"next plugin retry: 2026-10-04T00:35:00Z",
		"(zero_yield_backoff)",
		"zero yield: 2 runs, suppressed until 2026-10-04T00:40:00Z",
		"zero-yield suppression lifted: level_rose",
	} {
		if !bytes.Contains(out.Bytes(), []byte(want)) {
			t.Fatalf("text report missing %q:\n%s", want, out.String())
		}
	}
}
