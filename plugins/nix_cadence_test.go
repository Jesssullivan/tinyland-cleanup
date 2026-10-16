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
