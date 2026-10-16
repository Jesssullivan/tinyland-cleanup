package plugins

// nix_gclock.go: serialize Nix GC with the host's other collectors and
// switches (lab R-C815, TIN-5770). A host that keeps an older collector as a
// fallback (lab's serialized Darwin engines) or runs switches that must not
// overlap a GC names that collector's flock file in nix.gc_lock_file and the
// mkdir locks of its switches and deliveries in nix.gc_defer_paths. A held
// lock or an existing marker defers the pass to a later poll; nothing is
// refused for good, and nothing other tools own is created or removed.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// errNixGCLockHeld means another process holds nix.gc_lock_file.
var errNixGCLockHeld = errors.New("nix GC serialization lock is held by another process")

// nixGCLockPath is nix.gc_lock_file with ~ expanded, or "" when unset.
func nixGCLockPath(cfg config.NixConfig) string {
	if cfg.GCLockFile == "" {
		return ""
	}
	home, _ := os.UserHomeDir()
	return filepath.Clean(expandHome(cfg.GCLockFile, home))
}

// nixGCDeferredBy returns the first nix.gc_defer_paths entry that exists.
// An entry that cannot be checked for any reason other than absence also
// defers, because it may be a live marker.
func nixGCDeferredBy(cfg config.NixConfig) (string, bool) {
	home, _ := os.UserHomeDir()
	for _, raw := range cfg.GCDeferPaths {
		if raw == "" {
			continue
		}
		path := filepath.Clean(expandHome(raw, home))
		if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
			return path, true
		}
	}
	return "", false
}

// nixAcquireGCLock takes nix.gc_lock_file. It returns a release function
// (a no-op when no lock is configured), errNixGCLockHeld when another
// process holds it, or another error when the lock file is unsafe to use.
func nixAcquireGCLock(cfg config.NixConfig) (func(), error) {
	path := nixGCLockPath(cfg)
	if path == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("nix GC lock directory: %w", err)
	}
	return nixFlockFile(path)
}
