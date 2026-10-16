//go:build unix

package plugins

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// holdFlock takes the lock the way lab's Python serialized engines do: a
// separate open file description with flock(2), so it conflicts with the
// plugin's own descriptor even inside one process.
func holdFlock(t *testing.T, path string) func() {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		t.Fatal(err)
	}
	return func() { f.Close() }
}

func TestNixGCLockIsExclusiveAndReleased(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "authority", "serialized-gc.lock")
	cfg := config.NixConfig{GCLockFile: lock}

	release, err := nixAcquireGCLock(cfg)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	info, err := os.Stat(lock)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file must be created 0600: %v %v", info, err)
	}
	if dir, _ := os.Stat(filepath.Dir(lock)); dir.Mode().Perm() != 0o700 {
		t.Fatalf("lock directory must be created 0700, got %v", dir.Mode().Perm())
	}
	if _, err := nixAcquireGCLock(cfg); !errors.Is(err, errNixGCLockHeld) {
		t.Fatalf("a second holder must see the lock held, got %v", err)
	}
	release()
	again, err := nixAcquireGCLock(cfg)
	if err != nil {
		t.Fatalf("after release the lock is free again: %v", err)
	}
	again()
}

func TestNixGCLockRefusesAnUnsafeFile(t *testing.T) {
	dir := t.TempDir()
	open := filepath.Join(dir, "open.lock")
	if err := os.WriteFile(open, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := nixAcquireGCLock(config.NixConfig{GCLockFile: open}); err == nil || errors.Is(err, errNixGCLockHeld) {
		t.Fatalf("a world-readable lock file must be refused, got %v", err)
	}
	link := filepath.Join(dir, "link.lock")
	if err := os.Symlink(filepath.Join(dir, "elsewhere"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := nixAcquireGCLock(config.NixConfig{GCLockFile: link}); err == nil {
		t.Fatal("a symlinked lock file must be refused")
	}
	if release, err := nixAcquireGCLock(config.NixConfig{}); err != nil {
		t.Fatalf("no lock configured is a no-op: %v", err)
	} else {
		release()
	}
}

func TestNixGCDeferredByAnExistingMarker(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "hm-switch.lock.d")
	cfg := config.NixConfig{GCDeferPaths: []string{"", filepath.Join(dir, "absent"), marker}}
	if path, deferred := nixGCDeferredBy(cfg); deferred {
		t.Fatalf("no marker exists, got %q", path)
	}
	if err := os.Mkdir(marker, 0o700); err != nil {
		t.Fatal(err)
	}
	if path, deferred := nixGCDeferredBy(cfg); !deferred || path != marker {
		t.Fatalf("an existing mkdir lock defers the pass, got %q %v", path, deferred)
	}
}

func lockTestConfig(t *testing.T) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Nix.SkipWhenDaemonBusy = false
	cfg.Nix.HostMeasurePath = t.TempDir()
	cfg.Nix.PeriodicGCInterval = "4h"
	cfg.Nix.MinGCInterval = "30m"
	cfg.Nix.GCStateFile = filepath.Join(t.TempDir(), "nix-gc-state.json")
	cfg.Nix.GCLockFile = filepath.Join(t.TempDir(), "serialized-gc.lock")
	return cfg
}

// R-C815: a pass never overlaps the fallback collector or a switch, and a
// deferred pass is retried on the next poll instead of waiting out the gap.
func TestNixCleanupDefersWhileTheLockIsHeldOrAMarkerExists(t *testing.T) {
	calls := fakeNixGC(t)
	cfg := lockTestConfig(t)
	marker := filepath.Join(t.TempDir(), "neo-signed-deliver.lock")
	cfg.Nix.GCDeferPaths = []string{marker}
	now := time.Now()
	p := NewNixPlugin()
	p.now = func() time.Time { return now }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	unlock := holdFlock(t, cfg.Nix.GCLockFile)
	p.Cleanup(context.Background(), LevelCritical, cfg, logger)
	if got := gcCalls(t, calls); len(got) != 0 {
		t.Fatalf("a held serialization lock defers the pass: %q", got)
	}
	unlock()

	if err := os.Mkdir(marker, 0o700); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Minute)
	p.Cleanup(context.Background(), LevelCritical, cfg, logger)
	if got := gcCalls(t, calls); len(got) != 0 {
		t.Fatalf("an existing marker defers the pass: %q", got)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	now = now.Add(5 * time.Minute)
	p.Cleanup(context.Background(), LevelNone, cfg, logger)
	if got := gcCalls(t, calls); len(got) != 2 {
		t.Fatalf("deferred passes are not attempts: the next poll collects: %q", got)
	}
	release, err := nixAcquireGCLock(cfg.Nix)
	if err != nil {
		t.Fatalf("the plugin releases the lock after its pass: %v", err)
	}
	release()
}

func TestNixCleanupHoldsTheLockDuringItsPass(t *testing.T) {
	binDir := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	cfg := lockTestConfig(t)
	// The fake GC reports whether it could take the lock itself: it must
	// not, because the plugin holds it for the whole pass.
	script := `#!/bin/sh
if [ "$1" != "--dry-run" ]; then
  if /usr/bin/python3 -c 'import fcntl,sys; f=open(sys.argv[1],"a"); fcntl.flock(f, fcntl.LOCK_EX|fcntl.LOCK_NB)' "$NIX_GC_LOCK" 2>/dev/null; then
    printf 'lock:free\n' >> "$NIX_GC_CALLS"
  else
    printf 'lock:held\n' >> "$NIX_GC_CALLS"
  fi
fi
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
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skip("needs /usr/bin/python3 to probe the lock from a child")
	}
	t.Setenv("PATH", binDir)
	t.Setenv("NIX_GC_CALLS", calls)
	t.Setenv("NIX_GC_LOCK", cfg.Nix.GCLockFile)
	p := NewNixPlugin()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p.Cleanup(context.Background(), LevelNone, cfg, logger)
	got := gcCalls(t, calls)
	if len(got) != 3 || got[1] != "lock:held" {
		t.Fatalf("nix-collect-garbage must run while the plugin holds the lock: %q", got)
	}
}
