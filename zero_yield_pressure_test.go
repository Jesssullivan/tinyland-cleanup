package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
	"github.com/Jesssullivan/tinyland-cleanup/monitor"
)

// Operator ruling 2026-10-19 ("Quota pressure retries"): on sting the bazel,
// cache, dev-artifacts, archive-lifecycle and podman plugins sat in
// zero_yield_backoff for 6h while the dev-seat project quota
// (sting-devseat-177, /srv/fast-local/jess) was at moderate or critical.
// Pressure at moderate or higher on any monitored mount now lifts zero-yield
// suppression, the way emergency_free_gb does for the host, but at most once
// per zero_yield_pressure_min_interval since the plugin's last run.

// newQuotaPressureDaemon returns a daemon watching a calm root mount and a
// project-quota mount whose used percentage the caller controls, with one
// plugin that reclaims nothing. Cooldown and byte backoff stay out of the
// way (unmet minimum_free_gb runway, byte backoff off), so zero-yield
// suppression alone decides when the plugin runs.
func newQuotaPressureDaemon(t *testing.T, quotaUsed *float64) (*daemon, *fakeClock, *bytes.Buffer) {
	t.Helper()
	var output bytes.Buffer
	d := newTestDaemonWithPlugins(t, &output, &reportingPlugin{name: "dev-artifacts"})
	d.config.Policy.MinimumFreeGB = 500
	d.config.Policy.ByteNoProgressLimit = -1
	d.config.TargetFree = 70
	d.config.MonitoredMounts = []config.MountConfig{
		{Path: "/", Label: "root"},
		{Path: "/srv/fast-local/jess", Label: "sting-devseat-177",
			ThresholdWarning: 90, ThresholdModerate: 92, ThresholdAggressive: 95, ThresholdCritical: 98},
	}
	clock := &fakeClock{t: time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)}
	d.now = clock.Now
	d.diskStats = func(path string) (*monitor.DiskStats, error) {
		var s *monitor.DiskStats
		if path == "/srv/fast-local/jess" {
			// A 2000 GiB quota: free stays at or above the 20 GiB
			// emergency floor, so only mount pressure can lift.
			s = diskStats(2000*gib, uint64((100-*quotaUsed)*20)*gib, *quotaUsed)
		} else {
			s = diskStats(2000*gib, 1000*gib, 50)
		}
		s.Path = path
		return s, nil
	}
	return d, clock, &output
}

// driveToLongBackoff runs the schedule to the point where the plugin's
// backoff (2h) is longer than the 1h pressure interval: runs at 0m, 5m
// (suppressed until 35m), 35m (until 95m) and 95m (until 215m). It returns
// the time of the last run.
func driveToLongBackoff(t *testing.T, d *daemon, clock *fakeClock, output *bytes.Buffer) time.Time {
	t.Helper()
	start := clock.Now()
	for _, at := range []time.Duration{0, 5 * time.Minute, 35 * time.Minute, 95 * time.Minute} {
		clock.t = start.Add(at)
		if p := onlyPlugin(t, runByteCycle(t, d, output)); !p.Ran || p.ZeroYieldLifted != "" {
			t.Fatalf("at +%s: want a plain scheduled run, got %+v", at, p)
		}
	}
	return clock.Now()
}

func TestZeroYieldLiftedByQuotaMountPressure(t *testing.T) {
	used := 93.0 // moderate on the dev-seat quota, nothing on root
	d, clock, output := newQuotaPressureDaemon(t, &used)
	last := driveToLongBackoff(t, d, clock, output)

	clock.Advance(5 * time.Minute)
	rep := runByteCycle(t, d, output)
	p := onlyPlugin(t, rep)
	if rep.Level != "moderate" || rep.MonitorPath != "/srv/fast-local/jess" {
		t.Fatalf("the quota mount must drive the cycle: level=%s monitor=%s", rep.Level, rep.MonitorPath)
	}
	// Suppressed until +215m, but mount pressure brings the retry to one
	// pressure interval after the last run.
	if want := last.Add(time.Hour).Format(time.RFC3339); p.Ran || p.SkipReason != "zero_yield_backoff" || p.RetryAt != want {
		t.Fatalf("inside the pressure interval: want a skip until %s, got %+v", want, p)
	}
	if rep.NextRetryAt != p.RetryAt {
		t.Fatalf("next_retry_at = %q, want %q", rep.NextRetryAt, p.RetryAt)
	}
	clock.t = last.Add(time.Hour)
	if p := onlyPlugin(t, runByteCycle(t, d, output)); !p.Ran || p.ZeroYieldLifted != zeroYieldLiftMountPressure {
		t.Fatalf("quota pressure must lift the backoff an hour after the last run, got %+v", p)
	}
}

func TestZeroYieldNotLiftedWithoutMountPressure(t *testing.T) {
	used := 91.0 // warning on the dev-seat quota: below the moderate line
	d, clock, output := newQuotaPressureDaemon(t, &used)
	last := driveToLongBackoff(t, d, clock, output)
	until := last.Add(2 * time.Hour)

	clock.t = last.Add(time.Hour)
	if p := onlyPlugin(t, runByteCycle(t, d, output)); p.Ran || p.SkipReason != "zero_yield_backoff" ||
		p.RetryAt != until.Format(time.RFC3339) {
		t.Fatalf("at warning the full backoff applies until %s, got %+v", until, p)
	}
	clock.t = until
	if p := onlyPlugin(t, runByteCycle(t, d, output)); !p.Ran || p.ZeroYieldLifted != "" {
		t.Fatalf("want a plain scheduled run at %s, got %+v", until, p)
	}
}

// A plugin that keeps reclaiming nothing under sustained quota pressure runs
// at most once per pressure interval, however short the poll; a configured
// interval under the floor is raised to it.
func TestZeroYieldMountPressureCannotSpin(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		poll       time.Duration
		want       time.Duration
	}{
		{"default", "", 5 * time.Minute, time.Hour},
		{"floor", "1s", time.Minute, minZeroYieldPressureMinInterval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			used := 98.5 // critical on the quota, 30 GiB free
			d, clock, output := newQuotaPressureDaemon(t, &used)
			d.config.Policy.ZeroYieldPressureMinInterval = tc.configured
			if got := d.zeroYieldPolicy().pressureMin; got != tc.want {
				t.Fatalf("pressure interval = %s, want %s", got, tc.want)
			}
			policy := d.zeroYieldPolicy()
			var runs []time.Time
			lifted := 0
			for end := clock.Now().Add(12 * time.Hour); clock.Now().Before(end); clock.Advance(tc.poll) {
				p := onlyPlugin(t, runByteCycle(t, d, output))
				if !p.Ran {
					continue
				}
				if p.ZeroYieldLifted == zeroYieldLiftMountPressure {
					lifted++
					if gap := clock.Now().Sub(runs[len(runs)-1]); gap < tc.want {
						t.Fatalf("a pressure lift came %s after the last run, under %s", gap, tc.want)
					}
				}
				runs = append(runs, clock.Now())
			}
			for i := policy.limit; i < len(runs); i++ {
				if gap := runs[i].Sub(runs[i-1]); gap < tc.want && gap < policy.base {
					t.Fatalf("run %d came %s after the last one: %v", i, gap, runs)
				}
			}
			if bound := soakRunBound(policy, policy.pressureCap(), 12*time.Hour); len(runs) > bound {
				t.Fatalf("%d runs in 12h, want at most %d: %v", len(runs), bound, runs)
			}
			if lifted == 0 {
				t.Fatalf("sustained quota pressure never lifted the backoff: %v", runs)
			}
		})
	}
}

func TestPressuredMountReadsEveryMonitoredMount(t *testing.T) {
	report := cycleReport{HostFreeBeforeBytes: 100 * gib, Mounts: []mountReport{
		{Label: "root", Path: "/", Level: "critical", Error: "statfs failed"},
		{Label: "nix-store", Path: "/nix", Level: "warning"},
		{Label: "sting-devseat-177", Path: "/srv/fast-local/jess", Level: "moderate"},
	}}
	if got := pressuredMount(report); got != "sting-devseat-177" {
		t.Fatalf("pressuredMount = %q", got)
	}
	report.Mounts[2].Level = "warning"
	if got := pressuredMount(report); got != "" {
		t.Fatalf("no mount at moderate or higher, got %q", got)
	}
	// An operator run still names itself rather than mount pressure.
	d := newTestDaemonWithPlugins(t, &bytes.Buffer{}, &reportingPlugin{name: "cache"})
	report.Mounts[2].Level = "aggressive"
	if got := d.zeroYieldExemption(&reportingPlugin{name: "cache"}, report, true); got != zeroYieldLiftMountPressure {
		t.Fatalf("exemption = %q", got)
	}
	report.ForcedLevel = true
	if got := d.zeroYieldExemption(&reportingPlugin{name: "cache"}, report, true); got != "operator_run" {
		t.Fatalf("exemption with --level = %q", got)
	}
}
