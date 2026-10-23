package plugins

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// R-C796: pressure and periodic triggers, with a minimum gap even at critical.
func TestNixGCDue(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	cfg := config.NixConfig{MinGCInterval: "30m", PeriodicGCInterval: "4h"}
	cases := []struct {
		name  string
		level CleanupLevel
		last  time.Time
		want  CleanupLevel
		why   string
	}{
		{"never ran, no pressure: periodic", LevelNone, time.Time{}, LevelWarning, "periodic"},
		{"pressure after the gap", LevelWarning, now.Add(-time.Hour), LevelWarning, "pressure"},
		{"critical inside the gap is held", LevelCritical, now.Add(-10 * time.Minute), LevelNone, "min_gc_interval"},
		{"critical after the gap runs", LevelCritical, now.Add(-31 * time.Minute), LevelCritical, "pressure"},
		{"no pressure, periodic not yet due", LevelNone, now.Add(-time.Hour), LevelNone, "not_due"},
		{"no pressure, periodic due", LevelNone, now.Add(-5 * time.Hour), LevelWarning, "periodic"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, why := nixGCDue(tc.level, cfg, nixGCState{LastGCAttempt: tc.last}, now)
			if got != tc.want || why != tc.why {
				t.Fatalf("nixGCDue = %v %q, want %v %q", got, why, tc.want, tc.why)
			}
		})
	}
	off := config.NixConfig{MinGCInterval: "30m"}
	if got, _ := nixGCDue(LevelNone, off, nixGCState{}, now); got != LevelNone {
		t.Fatalf("no periodic interval and no pressure must not collect, got %v", got)
	}
}

func fakeNixGC(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	script := `#!/bin/sh
printf 'call:%s\n' "$*" >> "$NIX_GC_CALLS"
if [ "$1" = "--dry-run" ]; then
  printf '%s\n' "would delete 1 store paths" "1.0 MiB would be freed"
  exit 0
fi
printf '%s\n' "1 store paths deleted, 1.0 MiB freed"
`
	if err := os.WriteFile(filepath.Join(binDir, "nix-collect-garbage"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// Only the fake on PATH: a real nix-env must never prune the test
	// user's profile from a moderate-or-higher pass.
	t.Setenv("PATH", binDir)
	t.Setenv("NIX_GC_CALLS", calls)
	return calls
}

func gcCalls(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

func TestNixCleanupRunsPeriodicallyAndHoldsTheMinimumGapEvenAtCritical(t *testing.T) {
	calls := fakeNixGC(t)
	cfg := config.DefaultConfig()
	cfg.Nix.SkipWhenDaemonBusy = false
	cfg.Nix.HostMeasurePath = t.TempDir()
	cfg.Nix.PeriodicGCInterval = "4h"
	cfg.Nix.MinGCInterval = "30m"
	cfg.Nix.GCStateFile = filepath.Join(t.TempDir(), "nix-gc-state.json")
	now := time.Now()
	p := NewNixPlugin()
	p.now = func() time.Time { return now }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if !p.LevelIndependent() {
		t.Fatal("the nix plugin must run every poll")
	}
	p.Cleanup(context.Background(), LevelNone, cfg, logger)
	if got := gcCalls(t, calls); len(got) != 2 {
		t.Fatalf("the first periodic pass runs a dry-run and a GC with no pressure: %q", got)
	}
	now = now.Add(10 * time.Minute)
	p.Cleanup(context.Background(), LevelCritical, cfg, logger)
	if got := gcCalls(t, calls); len(got) != 2 {
		t.Fatalf("critical 10 minutes later is held by min_gc_interval: %q", got)
	}
	now = now.Add(25 * time.Minute)
	p.Cleanup(context.Background(), LevelWarning, cfg, logger)
	if got := gcCalls(t, calls); len(got) != 4 {
		t.Fatalf("pressure after the gap collects: %q", got)
	}
	now = now.Add(time.Hour)
	p.Cleanup(context.Background(), LevelNone, cfg, logger)
	if got := gcCalls(t, calls); len(got) != 4 {
		t.Fatalf("no pressure one hour later: periodic is not due yet: %q", got)
	}
}

func TestNixCleanupSkipsGCWhenRequiredHookPinsFail(t *testing.T) {
	calls := fakeNixGC(t)
	home := t.TempDir()
	settings := filepath.Join(home, "settings.json")
	if err := os.WriteFile(settings, []byte(`{"command":"/nix/store/0123456789abcdfghijklmnpqrsvwxyz-hook/bin/hook"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Nix.SkipWhenDaemonBusy = false
	cfg.Nix.HostMeasurePath = t.TempDir()
	cfg.Nix.GCStateFile = filepath.Join(t.TempDir(), "nix-gc-state.json")
	cfg.Nix.HookPinFiles = []string{settings}
	cfg.Nix.HookPinDir = filepath.Join(home, "gcroots", "hook-pins")
	cfg.Nix.HookPinRequired = true
	p := NewNixPlugin()
	p.hookValid = func(context.Context, string) bool { return true }
	p.hookAddRoot = func(context.Context, string, string) error { return errors.New("daemon refused") }
	p.hookSessionStarts = func([]string) ([]time.Time, error) { return nil, nil }

	p.Cleanup(context.Background(), LevelCritical, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := gcCalls(t, calls); len(got) != 0 {
		t.Fatalf("no GC may run without its hook pins: %q", got)
	}
}

// Re-review N1: pins are refreshed every poll, so a hook path that two
// switches replace inside one GC gap is still pinned while the session
// started under it lives.
func TestNixHookPinsRefreshEveryPollEvenWhenGCNotDue(t *testing.T) {
	calls := fakeNixGC(t)
	home := t.TempDir()
	settings := filepath.Join(home, "settings.json")
	write := func(ref string) {
		t.Helper()
		if err := os.WriteFile(settings, []byte(`{"command":"`+ref+`/bin/hook"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const (
		p1 = "/nix/store/0123456789abcdfghijklmnpqrsvwxyz-hook-1"
		p2 = "/nix/store/1123456789abcdfghijklmnpqrsvwxyz-hook-2"
		p3 = "/nix/store/2123456789abcdfghijklmnpqrsvwxyz-hook-3"
	)
	cfg := config.DefaultConfig()
	cfg.Nix.SkipWhenDaemonBusy = false
	cfg.Nix.HostMeasurePath = t.TempDir()
	cfg.Nix.PeriodicGCInterval = "4h"
	cfg.Nix.MinGCInterval = "30m"
	cfg.Nix.GCStateFile = filepath.Join(t.TempDir(), "nix-gc-state.json")
	cfg.Nix.HookPinFiles = []string{settings}
	cfg.Nix.HookPinDir = filepath.Join(home, "gcroots", "hook-pins")
	cfg.Nix.HookPinRequired = true

	now := time.Now()
	var starts []time.Time
	p := NewNixPlugin()
	p.now = func() time.Time { return now }
	p.lastGCAttempt = now // a pass just ran: GC is not due for the whole test
	p.hookValid = func(context.Context, string) bool { return true }
	p.hookAddRoot = func(_ context.Context, link, storePath string) error { return os.Symlink(storePath, link) }
	p.hookSessionStarts = func([]string) ([]time.Time, error) { return starts, nil }
	pinned := func(ref string) bool {
		dest, err := os.Readlink(filepath.Join(cfg.Nix.HookPinDir, filepath.Base(ref)))
		return err == nil && dest == ref
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	poll := func(level CleanupLevel) {
		now = now.Add(5 * time.Minute)
		p.Cleanup(context.Background(), level, cfg, logger)
	}

	write(p1)
	poll(LevelNone)
	write(p2) // switch A
	poll(LevelNone)
	starts = []time.Time{now} // a session starts under A
	write(p3)                 // switch B, inside the same GC gap
	poll(LevelCritical)       // held by min_gc_interval, but pins refresh
	if got := gcCalls(t, calls); len(got) != 0 {
		t.Fatalf("no GC is due inside the gap: %q", got)
	}
	if !pinned(p2) {
		t.Fatal("the middle hook path stays pinned while the session started under it lives")
	}
	if !pinned(p3) {
		t.Fatal("the current hook path is pinned")
	}
	if pinned(p1) {
		t.Fatal("a path no live session started under is released")
	}
}

// Re-review N3: every pass is bounded by max_freed_per_pass.
func TestNixCollectGarbageBoundsEachPass(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"50G", "--max-freed 53687091200"},
		{"10737418240", "--max-freed 10737418240"},
		{"512M", "--max-freed 536870912"},
		{"0", ""},
		{"", ""},
		{"lots", "--max-freed 53687091200"}, // invalid: the default bound, never unbounded
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			calls := fakeNixGC(t)
			cfg := config.DefaultConfig().Nix
			cfg.HostMeasurePath = t.TempDir()
			cfg.MaxFreedPerPass = tc.raw
			p := NewNixPlugin()
			p.collectGarbage(context.Background(), LevelWarning, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if got := gcCalls(t, calls); len(got) != 1 || got[0] != "call:"+tc.want {
				t.Fatalf("max_freed_per_pass %q: calls %q, want %q", tc.raw, got, "call:"+tc.want)
			}
		})
	}
	if got := config.DefaultConfig().Nix.MaxFreedPerPass; got != "50G" {
		t.Fatalf("the default bound is 50G, got %q", got)
	}
}

func TestNixCleanupBoundsTheFirstPassAtCritical(t *testing.T) {
	calls := fakeNixGC(t)
	cfg := config.DefaultConfig()
	cfg.Nix.SkipWhenDaemonBusy = false
	cfg.Nix.HostMeasurePath = t.TempDir()
	cfg.Nix.MaxFreedPerPass = "1G"
	p := NewNixPlugin()
	p.Cleanup(context.Background(), LevelCritical, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	got := gcCalls(t, calls)
	if len(got) != 2 || got[0] != "call:--dry-run" || !strings.Contains(got[1], "--max-freed 1073741824") {
		t.Fatalf("the critical pass is bounded: %q", got)
	}
}

func busyCleanupFixture(t *testing.T) (*NixPlugin, *config.Config, string, *time.Time) {
	t.Helper()
	calls := fakeNixGC(t)
	cfg := config.DefaultConfig()
	cfg.Nix.SkipWhenDaemonBusy = true
	cfg.Nix.HostMeasurePath = t.TempDir()
	cfg.Nix.PeriodicGCInterval = "4h"
	cfg.Nix.MinGCInterval = "30m"
	cfg.Nix.GCStateFile = filepath.Join(t.TempDir(), "nix-gc-state.json")
	now := time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC)
	p := NewNixPlugin()
	p.now = func() time.Time { return now }
	return p, cfg, calls, &now
}

// No single process can hold GC off indefinitely: after max_busy_deferrals
// consecutive deferrals the next due pass runs, and the gauge resets.
func TestNixBusyDeferralIsBoundedByCount(t *testing.T) {
	p, cfg, calls, now := busyCleanupFixture(t)
	cfg.Nix.MaxBusyDeferrals = 3
	cfg.Nix.MaxBusyDeferralDuration = ""
	p.activeWork = func(context.Context) ([]string, nixProcessInspection, error) {
		return []string{"nix build"}, nixProcessInspection{}, nil
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for i := 1; i <= 3; i++ {
		p.Cleanup(context.Background(), LevelNone, cfg, logger)
		if got := gcCalls(t, calls); len(got) != 0 {
			t.Fatalf("deferral %d must not collect: %q", i, got)
		}
		st := loadNixGCState(cfg.Nix.GCStateFile)
		if st.DeferredPasses != i || !strings.Contains(st.LastDeferral, "nix build") {
			t.Fatalf("after deferral %d state = %+v", i, st)
		}
		*now = now.Add(5 * time.Minute)
	}
	plan := p.PlanCleanup(context.Background(), LevelNone, cfg, logger)
	if plan.Metadata["deferred_passes"] != "3" || plan.Metadata["busy_deferral_released_by"] != "max_busy_deferrals=3" || !plan.WouldRun {
		t.Fatalf("plan must report the gauge and the release: %+v", plan.Metadata)
	}
	before := len(gcCalls(t, calls)) // the plan runs its own dry-run
	p.Cleanup(context.Background(), LevelNone, cfg, logger)
	if got := gcCalls(t, calls); len(got) != before+2 || !strings.HasPrefix(got[len(got)-1], "call:--max-freed") {
		t.Fatalf("the pass after the bound runs a dry-run and a GC: %q", got)
	}
	st := loadNixGCState(cfg.Nix.GCStateFile)
	if st.DeferredPasses != 0 || !st.FirstDeferredAt.IsZero() || !st.LastGCAttempt.Equal(*now) {
		t.Fatalf("an attempted pass resets the deferral run: %+v", st)
	}
}

func TestNixBusyDeferralIsBoundedByDuration(t *testing.T) {
	p, cfg, calls, now := busyCleanupFixture(t)
	cfg.Nix.MaxBusyDeferrals = 0
	cfg.Nix.MaxBusyDeferralDuration = "6h"
	p.activeWork = func(context.Context) ([]string, nixProcessInspection, error) {
		return []string{"nix-daemon worker"}, nixProcessInspection{}, nil
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	start := *now
	p.Cleanup(context.Background(), LevelWarning, cfg, logger)
	*now = start.Add(5*time.Hour + 59*time.Minute)
	p.Cleanup(context.Background(), LevelWarning, cfg, logger)
	if got := gcCalls(t, calls); len(got) != 0 {
		t.Fatalf("inside six hours the pass defers: %q", got)
	}
	if st := loadNixGCState(cfg.Nix.GCStateFile); st.DeferredPasses != 2 || !st.FirstDeferredAt.Equal(start) {
		t.Fatalf("state = %+v", st)
	}
	*now = start.Add(6 * time.Hour)
	p.Cleanup(context.Background(), LevelWarning, cfg, logger)
	if got := gcCalls(t, calls); len(got) != 2 {
		t.Fatalf("six hours after the first deferral the pass runs: %q", got)
	}
}

// A failed inspection defers like active work, and like active work it is
// bounded: a process the daemon cannot inspect (issue #160: a root-owned
// interpreter on Darwin) cannot hold GC off forever, even at critical.
func TestNixInspectionFailureIsReleasedByTheBound(t *testing.T) {
	p, cfg, calls, now := busyCleanupFixture(t)
	cfg.Nix.MaxBusyDeferrals = 3
	cfg.Nix.MaxBusyDeferralDuration = ""
	p.activeWork = func(context.Context) ([]string, nixProcessInspection, error) {
		return nil, nixProcessInspection{}, errors.New("inspect candidate pid 47544 (/bin/bash, uid 0): invalid argument")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for i := 1; i <= 3; i++ {
		p.Cleanup(context.Background(), LevelCritical, cfg, logger)
		if got := gcCalls(t, calls); len(got) != 0 {
			t.Fatalf("deferral %d must not collect: %q", i, got)
		}
		st := loadNixGCState(cfg.Nix.GCStateFile)
		if st.DeferredPasses != i || !strings.HasPrefix(st.LastDeferral, "process_inspection_failed: ") || !strings.Contains(st.LastDeferral, "pid 47544") {
			t.Fatalf("after deferral %d state = %+v", i, st)
		}
		*now = now.Add(10 * time.Minute)
	}
	plan := p.PlanCleanup(context.Background(), LevelCritical, cfg, logger)
	if plan.Metadata["busy_deferral_released_by"] != "max_busy_deferrals=3" || !plan.WouldRun {
		t.Fatalf("plan must report the release: %+v %+v", plan.Metadata, plan.SkipReason)
	}
	before := len(gcCalls(t, calls))
	p.Cleanup(context.Background(), LevelCritical, cfg, logger)
	got := gcCalls(t, calls)
	if len(got) != before+2 || !strings.HasPrefix(got[len(got)-1], "call:--max-freed") {
		t.Fatalf("the pass after the bound runs a dry-run and a GC: %q", got)
	}
	if st := loadNixGCState(cfg.Nix.GCStateFile); st.DeferredPasses != 0 {
		t.Fatalf("an attempted pass resets the deferral run: %+v", st)
	}
}

// Below the bound a failed inspection still fails closed.
func TestNixInspectionFailureDefersBelowTheBound(t *testing.T) {
	p, cfg, calls, now := busyCleanupFixture(t)
	cfg.Nix.MaxBusyDeferrals = 0
	cfg.Nix.MaxBusyDeferralDuration = "6h"
	p.activeWork = func(context.Context) ([]string, nixProcessInspection, error) {
		return nil, nixProcessInspection{}, errors.New("ps: not found")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for i := 0; i < 5; i++ {
		p.Cleanup(context.Background(), LevelCritical, cfg, logger)
		*now = now.Add(time.Hour)
	}
	if got := gcCalls(t, calls); len(got) != 0 {
		t.Fatalf("inside six hours an inspection failure defers: %q", got)
	}
	*now = now.Add(time.Hour)
	p.Cleanup(context.Background(), LevelCritical, cfg, logger)
	if got := gcCalls(t, calls); len(got) != 2 {
		t.Fatalf("six hours after the first deferral the pass runs: %q", got)
	}
}

// A serialization marker is never bypassed by the bound, but each deferral
// it causes is counted, so a stale marker shows in the GC state.
func TestNixMarkerDeferralIsCountedButNeverBypassed(t *testing.T) {
	p, cfg, calls, now := busyCleanupFixture(t)
	cfg.Nix.GCLockFile = filepath.Join(t.TempDir(), "serialized-gc.lock")
	marker := filepath.Join(t.TempDir(), "hm-switch.lock.d")
	if err := os.Mkdir(marker, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.Nix.GCDeferPaths = []string{marker}
	cfg.Nix.MaxBusyDeferrals = 2
	p.activeWork = func(context.Context) ([]string, nixProcessInspection, error) {
		return nil, nixProcessInspection{}, nil
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for i := 0; i < 5; i++ {
		p.Cleanup(context.Background(), LevelCritical, cfg, logger)
		*now = now.Add(10 * time.Minute)
	}
	if got := gcCalls(t, calls); len(got) != 0 {
		t.Fatalf("a marker is never bypassed: %q", got)
	}
	st := loadNixGCState(cfg.Nix.GCStateFile)
	if st.DeferredPasses != 5 || !strings.Contains(st.LastDeferral, "serialized_operation") || !strings.Contains(st.LastDeferral, "hm-switch.lock.d") {
		t.Fatalf("state = %+v", st)
	}
}

// A due pass with nix-collect-garbage off PATH (a launchd agent whose PATH
// misses the Nix profile) is a counted deferral, logged at WARN and at ERROR
// past the bound, never a silent DEBUG return (issue #163).
func TestNixUnavailableIsACountedLoudDeferral(t *testing.T) {
	p, cfg, _, now := busyCleanupFixture(t)
	cfg.Nix.MaxBusyDeferrals = 2
	cfg.Nix.MaxBusyDeferralDuration = ""
	p.activeWork = func(context.Context) ([]string, nixProcessInspection, error) {
		return nil, nixProcessInspection{}, nil
	}
	t.Setenv("PATH", t.TempDir())
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	for i := 1; i <= 3; i++ {
		p.Cleanup(context.Background(), LevelModerate, cfg, logger)
		*now = now.Add(10 * time.Minute)
	}
	st := loadNixGCState(cfg.Nix.GCStateFile)
	if st.DeferredPasses != 3 || st.LastDeferral != "nix_unavailable" || !st.LastGCAttempt.IsZero() {
		t.Fatalf("state = %+v; want 3 counted nix_unavailable deferrals and no attempt", st)
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "is not on the daemon's PATH") {
		t.Fatalf("the first deferrals log at WARN: %s", out)
	}
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, `bound="max_busy_deferrals=2"`) {
		t.Fatalf("past the bound the deferral logs at ERROR: %s", out)
	}

	// Once nix is back on PATH the next poll collects and the run resets.
	fakeNixGC(t)
	p.Cleanup(context.Background(), LevelModerate, cfg, logger)
	st = loadNixGCState(cfg.Nix.GCStateFile)
	if st.DeferredPasses != 0 || st.LastGCAttempt.IsZero() {
		t.Fatalf("state after nix returned = %+v; want an attempt and no deferrals", st)
	}
}

// A pass that is not due never probes for nix, so a host without it that is
// not under pressure logs nothing.
func TestNixUnavailableNotDueIsQuiet(t *testing.T) {
	p, cfg, _, _ := busyCleanupFixture(t)
	cfg.Nix.PeriodicGCInterval = ""
	t.Setenv("PATH", t.TempDir())
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	p.Cleanup(context.Background(), LevelNone, cfg, logger)
	if st := loadNixGCState(cfg.Nix.GCStateFile); st.DeferredPasses != 0 {
		t.Fatalf("a pass that is not due is not a deferral: %+v", st)
	}
	if logs.Len() != 0 {
		t.Fatalf("nothing is logged when no pass is due: %s", logs.String())
	}
}

// A pass the bound releases is still held by required hook-path pins.
func TestNixReleasedPassStillNeedsHookPins(t *testing.T) {
	p, cfg, calls, _ := busyCleanupFixture(t)
	home := t.TempDir()
	settings := filepath.Join(home, "settings.json")
	if err := os.WriteFile(settings, []byte(`{"command":"/nix/store/0123456789abcdfghijklmnpqrsvwxyz-hook/bin/hook"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Nix.HookPinFiles = []string{settings}
	cfg.Nix.HookPinDir = filepath.Join(home, "gcroots", "hook-pins")
	cfg.Nix.HookPinRequired = true
	cfg.Nix.MaxBusyDeferrals = 1
	if err := saveNixGCState(cfg.Nix.GCStateFile, nixGCState{DeferredPasses: 5, FirstDeferredAt: time.Unix(0, 0)}); err != nil {
		t.Fatal(err)
	}
	p.hookValid = func(context.Context, string) bool { return true }
	p.hookAddRoot = func(context.Context, string, string) error { return errors.New("daemon refused") }
	p.hookSessionStarts = func([]string) ([]time.Time, error) { return nil, nil }
	p.activeWork = func(context.Context) ([]string, nixProcessInspection, error) {
		return []string{"nix build"}, nixProcessInspection{}, nil
	}
	p.Cleanup(context.Background(), LevelCritical, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := gcCalls(t, calls); len(got) != 0 {
		t.Fatalf("a released pass may not run without its hook pins: %q", got)
	}
}

// Without a state file the deferral run is kept in memory.
func TestNixBusyDeferralCountsInMemoryWithoutAStateFile(t *testing.T) {
	p, cfg, calls, now := busyCleanupFixture(t)
	cfg.Nix.GCStateFile = ""
	cfg.Nix.MaxBusyDeferrals = 2
	p.activeWork = func(context.Context) ([]string, nixProcessInspection, error) {
		return []string{"nix build"}, nixProcessInspection{}, nil
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for i := 0; i < 3; i++ {
		p.Cleanup(context.Background(), LevelNone, cfg, logger)
		*now = now.Add(5 * time.Minute)
	}
	if got := gcCalls(t, calls); len(got) != 2 {
		t.Fatalf("the third due pass runs: %q", got)
	}
}

func TestNixBusyDeferralDefaults(t *testing.T) {
	nix := config.DefaultConfig().Nix
	if nix.MaxBusyDeferrals != 72 || nix.MaxBusyDeferralDuration != "6h" {
		t.Fatalf("defaults = %d %q, want 72 and 6h", nix.MaxBusyDeferrals, nix.MaxBusyDeferralDuration)
	}
}
