package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"testing/quick"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/monitor"
	"github.com/Jesssullivan/tinyland-cleanup/plugins"
)

const gib = uint64(1024 * 1024 * 1024)

// bytePressureStats is neo's 2026-08-01 shape: 460 GiB total with the given
// free space, at the given used percentage.
func bytePressureStats(freeGiB uint64, usedPercent float64) *monitor.DiskStats {
	return diskStats(460*gib, freeGiB*gib, usedPercent)
}

// newByteBackoffDaemon returns a daemon with a fake clock, a zero-yield
// plugin and an unmet minimum_free_gb runway, so without byte backoff every
// cycle would bypass cooldown and run the plugin.
func newByteBackoffDaemon(t *testing.T, stats *monitor.DiskStats) (*daemon, *reportingPlugin, *fakeClock, *bytes.Buffer) {
	t.Helper()
	var output bytes.Buffer
	plugin := &reportingPlugin{name: "dev-artifacts"}
	d := newTestDaemonWithPlugins(t, &output, plugin)
	d.config.Policy.MinimumFreeGB = 40
	d.config.TargetFree = 70
	clock := &fakeClock{t: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)}
	d.now = clock.Now
	d.diskStats = sequenceDiskStats(t, stats)
	return d, plugin, clock, &output
}

func runByteCycle(t *testing.T, d *daemon, output *bytes.Buffer) cycleReport {
	t.Helper()
	output.Reset()
	if err := d.runOnce(context.Background(), monitor.LevelNone); err != nil {
		t.Fatalf("runOnce failed: %v", err)
	}
	return decodeCycleReport(t, output.Bytes())
}

func pluginRan(rep cycleReport) bool {
	return len(rep.Plugins) == 1 && rep.Plugins[0].Ran
}

func TestByteBackoffEngagesAfterRepeatedNoProgress(t *testing.T) {
	d, _, clock, output := newByteBackoffDaemon(t, bytePressureStats(25, 95))
	limit := d.byteNoProgressLimit()

	for i := 0; i < limit; i++ {
		rep := runByteCycle(t, d, output)
		if rep.ByteBackoff || !pluginRan(rep) {
			t.Fatalf("cycle %d: expected the plugin to run without backoff, got backoff=%v plugins=%+v", i, rep.ByteBackoff, rep.Plugins)
		}
		clock.Advance(5 * time.Minute)
	}

	rep := runByteCycle(t, d, output)
	if !rep.ByteBackoff || rep.ByteBackoffReason != "no_progress" {
		t.Fatalf("expected byte backoff to engage, got backoff=%v reason=%q count=%d", rep.ByteBackoff, rep.ByteBackoffReason, rep.ByteNoProgressCount)
	}
	if rep.ByteBackoffSeconds != int64((30 * time.Minute).Seconds()) {
		t.Fatalf("expected a 30m backoff interval, got %ds", rep.ByteBackoffSeconds)
	}
	p := rep.Plugins[0]
	if p.Ran || p.SkipReason != "byte_backoff" || p.RetryAt == "" {
		t.Fatalf("expected a byte_backoff skip with retry_at, got %+v", p)
	}
	retryAt, err := time.Parse(time.RFC3339, p.RetryAt)
	if err != nil {
		t.Fatalf("retry_at %q: %v", p.RetryAt, err)
	}
	// The last run was 5 minutes ago, so the plugin is eligible 25 minutes on.
	if want := clock.Now().Add(25 * time.Minute); !retryAt.Equal(want) {
		t.Fatalf("retry_at = %s, want %s", retryAt, want)
	}

	// Once the interval has passed since the last run the plugin runs again,
	// still under backoff, and is then held back again.
	clock.Advance(25 * time.Minute)
	rep = runByteCycle(t, d, output)
	if !rep.ByteBackoff || !pluginRan(rep) {
		t.Fatalf("expected one backed-off run after the interval, got backoff=%v plugins=%+v", rep.ByteBackoff, rep.Plugins)
	}
	clock.Advance(5 * time.Minute)
	rep = runByteCycle(t, d, output)
	if pluginRan(rep) || rep.Plugins[0].SkipReason != "byte_backoff" {
		t.Fatalf("expected the plugin to be held back again, got %+v", rep.Plugins)
	}
}

func TestByteBackoffResetsOnProgress(t *testing.T) {
	d, plugin, clock, output := newByteBackoffDaemon(t, bytePressureStats(25, 95))
	plugin.result = plugins.CleanupResult{BytesFreed: d.byteProgressMinBytes()}

	for i := 0; i < 2*d.byteNoProgressLimit(); i++ {
		rep := runByteCycle(t, d, output)
		if rep.ByteBackoff || rep.ByteNoProgressCount != 0 || !pluginRan(rep) {
			t.Fatalf("cycle %d: progress must keep backoff off, got backoff=%v count=%d", i, rep.ByteBackoff, rep.ByteNoProgressCount)
		}
		clock.Advance(5 * time.Minute)
	}

	// A sub-threshold reclaim (the 2026-08-01 "+6 MiB" cycle) is not progress.
	plugin.result = plugins.CleanupResult{BytesFreed: 6 * 1024 * 1024}
	for i := 0; i < d.byteNoProgressLimit(); i++ {
		runByteCycle(t, d, output)
		clock.Advance(5 * time.Minute)
	}
	if rep := runByteCycle(t, d, output); !rep.ByteBackoff {
		t.Fatalf("expected sub-threshold reclaim to count as no progress, count=%d", rep.ByteNoProgressCount)
	}
}

func TestByteBackoffResetsOnLevelEscalation(t *testing.T) {
	stats := bytePressureStats(37, 92) // aggressive
	d, _, clock, output := newByteBackoffDaemon(t, stats)
	for i := 0; i <= d.byteNoProgressLimit(); i++ {
		runByteCycle(t, d, output)
		clock.Advance(5 * time.Minute)
	}
	if rep := runByteCycle(t, d, output); !rep.ByteBackoff {
		t.Fatalf("expected backoff at aggressive, count=%d", rep.ByteNoProgressCount)
	}

	*stats = *bytePressureStats(25, 95) // escalate to critical
	clock.Advance(time.Minute)
	rep := runByteCycle(t, d, output)
	if rep.ByteBackoff || rep.ByteNoProgressCount != 0 || !pluginRan(rep) {
		t.Fatalf("an escalation must earn a fresh attempt, got backoff=%v count=%d plugins=%+v", rep.ByteBackoff, rep.ByteNoProgressCount, rep.Plugins)
	}
}

func TestByteBackoffNeverEngagesBelowEmergencyFloor(t *testing.T) {
	d, _, clock, output := newByteBackoffDaemon(t, bytePressureStats(15, 97))
	if d.emergencyFreeBytes() != 20*gib {
		t.Fatalf("expected the default 20 GiB emergency floor, got %d", d.emergencyFreeBytes())
	}
	var rep cycleReport
	for i := 0; i < 3*d.byteNoProgressLimit(); i++ {
		rep = runByteCycle(t, d, output)
		if rep.ByteBackoff || !pluginRan(rep) {
			t.Fatalf("cycle %d: below the floor every cycle must run, got backoff=%v plugins=%+v", i, rep.ByteBackoff, rep.Plugins)
		}
		clock.Advance(5 * time.Minute)
	}
	if rep.ByteBackoffReason != "below_emergency_floor" {
		t.Fatalf("expected reason below_emergency_floor, got %q", rep.ByteBackoffReason)
	}
}

func TestByteBackoffPersistsAcrossDaemonRestart(t *testing.T) {
	d, _, clock, output := newByteBackoffDaemon(t, bytePressureStats(25, 95))
	for i := 0; i < d.byteNoProgressLimit(); i++ {
		runByteCycle(t, d, output)
		clock.Advance(5 * time.Minute)
	}

	restarted, _, _, output2 := newByteBackoffDaemon(t, bytePressureStats(25, 95))
	restarted.config.Policy.StateFile = d.config.Policy.StateFile
	restarted.now = clock.Now
	rep := runByteCycle(t, restarted, output2)
	if !rep.ByteBackoff || pluginRan(rep) {
		t.Fatalf("expected backoff to survive a restart, got backoff=%v plugins=%+v", rep.ByteBackoff, rep.Plugins)
	}
}

func TestByteBackoffIntervalCappedAtMax(t *testing.T) {
	d, _, clock, output := newByteBackoffDaemon(t, bytePressureStats(25, 95))
	d.config.Policy.Cooldown = "6h"
	for i := 0; i < d.byteNoProgressLimit(); i++ {
		runByteCycle(t, d, output)
		clock.Advance(5 * time.Minute)
	}
	rep := runByteCycle(t, d, output)
	if !rep.ByteBackoff || rep.ByteBackoffSeconds != int64((30*time.Minute).Seconds()) {
		t.Fatalf("expected the 6h cooldown capped to 30m, got backoff=%v interval=%ds", rep.ByteBackoff, rep.ByteBackoffSeconds)
	}

	d.config.Policy.Cooldown = ""
	if got := d.byteBackoffInterval(); got != 30*time.Minute {
		t.Fatalf("with no cooldown the interval is the cap, got %s", got)
	}
	d.config.Policy.Cooldown = "10m"
	if got := d.byteBackoffInterval(); got != 10*time.Minute {
		t.Fatalf("a cooldown under the cap is used as is, got %s", got)
	}
}

func TestByteBackoffExemptions(t *testing.T) {
	t.Run("forced level", func(t *testing.T) {
		d, _, clock, output := newByteBackoffDaemon(t, bytePressureStats(25, 95))
		for i := 0; i <= d.byteNoProgressLimit(); i++ {
			runByteCycle(t, d, output)
			clock.Advance(5 * time.Minute)
		}
		output.Reset()
		if err := d.runOnce(context.Background(), monitor.LevelCritical); err != nil {
			t.Fatal(err)
		}
		rep := decodeCycleReport(t, output.Bytes())
		if rep.ByteBackoff || !pluginRan(rep) {
			t.Fatalf("a forced level must bypass byte backoff, got %+v", rep.Plugins)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		d, _, clock, output := newByteBackoffDaemon(t, bytePressureStats(25, 95))
		d.config.Policy.ByteNoProgressLimit = -1
		for i := 0; i < 6; i++ {
			if rep := runByteCycle(t, d, output); rep.ByteBackoff || !pluginRan(rep) {
				t.Fatalf("cycle %d: a negative limit disables byte backoff", i)
			}
			clock.Advance(5 * time.Minute)
		}
	})
}

func TestByteBackoffReleasedWhenPressureClears(t *testing.T) {
	stats := bytePressureStats(25, 95)
	d, _, clock, output := newByteBackoffDaemon(t, stats)
	for i := 0; i <= d.byteNoProgressLimit(); i++ {
		runByteCycle(t, d, output)
		clock.Advance(5 * time.Minute)
	}
	*stats = *bytePressureStats(160, 65)
	runByteCycle(t, d, output)

	state, _, err := loadCleanupState(d.config.Policy.StateFile, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != cleanupStateVersion {
		t.Fatalf("state version = %d, want %d", state.Version, cleanupStateVersion)
	}
	path := rec(t, state)
	if path.Engaged || path.NoProgressCount != 0 {
		t.Fatalf("clearing pressure must release backoff and reset the count, got %+v", path)
	}
}

func rec(t *testing.T, state *cleanupState) byteProgressRecord {
	t.Helper()
	if len(state.Bytes) != 1 {
		t.Fatalf("expected one byte progress record, got %d", len(state.Bytes))
	}
	for _, r := range state.Bytes {
		return r
	}
	return byteProgressRecord{}
}

// TestPropertyByteBackoffSpacing simulates a daemon pinned under zero-yield
// byte pressure for a random poll interval and duration. Once the no-progress
// limit is reached, consecutive plugin runs are never closer than the backoff
// interval and never further apart than the 30 minute cap plus one poll.
func TestPropertyByteBackoffSpacing(t *testing.T) {
	property := func(pollMinutes, cycles uint8, cooldownMinutes uint16) bool {
		poll := time.Duration(pollMinutes%30+1) * time.Minute
		n := int(cycles%60) + 10
		cooldown := (time.Duration(cooldownMinutes%720) * time.Minute).String()

		var output bytes.Buffer
		plugin := &reportingPlugin{name: "dev-artifacts"}
		d := newTestDaemonWithPlugins(t, &output, plugin)
		d.config.Policy.Cooldown = cooldown
		d.config.Policy.MinimumFreeGB = 40
		d.config.Policy.StateFile = filepath.Join(t.TempDir(), "state.json")
		clock := &fakeClock{t: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)}
		d.now = clock.Now
		d.diskStats = sequenceDiskStats(t, bytePressureStats(25, 95))

		interval := d.byteBackoffInterval()
		if interval <= 0 || interval > 30*time.Minute {
			return false
		}
		var runs []time.Time
		for i := 0; i < n; i++ {
			output.Reset()
			if err := d.runOnce(context.Background(), monitor.LevelNone); err != nil {
				return false
			}
			rep := decodeCycleReport(t, output.Bytes())
			if pluginRan(rep) {
				runs = append(runs, clock.Now())
			}
			clock.Advance(poll)
		}
		limit := d.byteNoProgressLimit()
		if len(runs) < limit {
			return false
		}
		for i := limit; i < len(runs); i++ {
			gap := runs[i].Sub(runs[i-1])
			if gap < interval || gap >= interval+poll {
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 15}); err != nil {
		t.Fatal(err)
	}
}

// TestPropertyRecordByteProgress checks the counter in isolation: it never
// goes negative, progress always resets it, a held-back cycle leaves it
// unchanged, and an escalation restarts it.
func TestPropertyRecordByteProgress(t *testing.T) {
	property := func(steps []uint8) bool {
		s := newCleanupState()
		now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
		for _, step := range steps {
			level := int(step%4) + 1
			progress := step&0x10 != 0
			ran := step&0x20 != 0
			before := s.byteNoProgressCount("/", level)
			s.recordByteProgress("/", 0, "", level, now, progress, ran, false)
			after := s.Bytes["/"].NoProgressCount
			switch {
			case after < 0:
				return false
			case progress && after != 0:
				return false
			case !progress && ran && after != before+1:
				return false
			case !progress && !ran && after != before:
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, nil); err != nil {
		t.Fatal(err)
	}
}

func TestTextReportShowsByteBackoffAndRetryAt(t *testing.T) {
	var out bytes.Buffer
	report := cycleReport{
		Level:               "critical",
		ByteBackoff:         true,
		ByteNoProgressCount: 3,
		ByteBackoffSeconds:  1800,
		Plugins: []pluginCycleReport{{
			Name:       "dev-artifacts",
			SkipReason: "byte_backoff",
			RetryAt:    "2026-10-04T00:30:00Z",
		}},
	}
	if err := writeTextReport(&out, report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"byte backoff: pressure unrelieved after 3 cycles, plugins run at most every 1800s",
		"(byte_backoff)",
		"retry at: 2026-10-04T00:30:00Z",
	} {
		if !bytes.Contains(out.Bytes(), []byte(want)) {
			t.Fatalf("text report missing %q:\n%s", want, out.String())
		}
	}
}

// TestByteBackoffCounterResetsWhenPressureClearsBelowCritical guards the
// clean-slate rule for every byte level, not only critical: a no-progress
// streak accrued at aggressive must not carry into the next pressure episode.
func TestByteBackoffCounterResetsWhenPressureClearsBelowCritical(t *testing.T) {
	stats := bytePressureStats(37, 92) // aggressive
	d, _, clock, output := newByteBackoffDaemon(t, stats)
	limit := d.byteNoProgressLimit()
	for i := 0; i < limit-1; i++ {
		runByteCycle(t, d, output)
		clock.Advance(5 * time.Minute)
	}

	*stats = *bytePressureStats(200, 50) // pressure clears
	if rep := runByteCycle(t, d, output); rep.Level != monitor.LevelNone.String() {
		t.Fatalf("expected pressure to clear, got level %q", rep.Level)
	}
	clock.Advance(5 * time.Minute)

	*stats = *bytePressureStats(37, 92) // a new episode at the same level
	for i := 0; i < limit; i++ {
		rep := runByteCycle(t, d, output)
		if rep.ByteNoProgressCount != i || rep.ByteBackoff || !pluginRan(rep) {
			t.Fatalf("new episode cycle %d: want count=%d without backoff, got count=%d backoff=%v plugins=%+v",
				i, i, rep.ByteNoProgressCount, rep.ByteBackoff, rep.Plugins)
		}
		clock.Advance(5 * time.Minute)
	}
}
