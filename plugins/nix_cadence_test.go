package plugins

import (
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
