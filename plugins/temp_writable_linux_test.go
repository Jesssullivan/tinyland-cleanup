//go:build linux

package plugins

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// readOnlyTempRoot builds a stale Nix temp root whose pytest directories
// lack owner write permission, as the October roots on sting do (TIN-8526).
// The modes are restored at cleanup so t.TempDir can remove what is left.
func readOnlyTempRoot(t *testing.T, scan, name string, age time.Duration) string {
	t.Helper()
	root := filepath.Join(scan, name)
	ro := filepath.Join(root, "pytest-of-u", "pytest-0", "case", "claim")
	deeper := filepath.Join(ro, "voices")
	if err := os.MkdirAll(deeper, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(ro, "candidate.json"), filepath.Join(deeper, "ref.wav")} {
		if err := os.WriteFile(f, []byte("fixture"), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{deeper, ro} {
		if err := os.Chmod(d, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { restoreOwnerWrite(root) })
	ageTempRoot(t, root, age)
	return root
}

func restoreOwnerWrite(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
}

func lstatMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// The repair makes a root with read-only directories removable. Without it
// os.RemoveAll fails with permission denied, as on sting.
func TestMakeTempRootWritableLetsRemovalSucceed(t *testing.T) {
	scan := t.TempDir()
	root := readOnlyTempRoot(t, scan, "nix-shell.ro0001", 72*time.Hour)
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := makeTempRootWritable(context.Background(), root, info); err != nil {
		t.Fatalf("repair failed: %v", err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("a repaired root must be removable, got %v", err)
	}
}

// A symlink inside the root is never followed: a read-only directory it
// points to outside the root keeps its mode.
func TestMakeTempRootWritableNeverFollowsSymlink(t *testing.T) {
	scan := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0o700) })
	root := readOnlyTempRoot(t, scan, "nix-shell.ro0002", 72*time.Hour)
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := makeTempRootWritable(context.Background(), root, info); err != nil {
		t.Fatalf("repair failed: %v", err)
	}
	if got := lstatMode(t, outside); got != 0o500 {
		t.Fatalf("a directory outside the root reached through a symlink must keep mode 0500, got %o", got)
	}
}

// A path that no longer names the root the final check passed changes
// nothing and reports errTempRootIdentityChanged.
func TestMakeTempRootWritableChecksIdentity(t *testing.T) {
	scan := t.TempDir()
	root := readOnlyTempRoot(t, scan, "nix-shell.ro0003", 72*time.Hour)
	other, err := os.Lstat(scan)
	if err != nil {
		t.Fatal(err)
	}
	err = makeTempRootWritable(context.Background(), root, other)
	if !errors.Is(err, errTempRootIdentityChanged) {
		t.Fatalf("want errTempRootIdentityChanged, got %v", err)
	}
	ro := filepath.Join(root, "pytest-of-u", "pytest-0", "case", "claim")
	if got := lstatMode(t, ro); got != 0o500 {
		t.Fatalf("a root that failed its identity check must not change, got %o", got)
	}
}

// Only a directory this uid owns on the root's device changes. The walk
// is called with another uid and another device to stand in for a
// directory owned by someone else and for a mount inside the root.
func TestTempRootMakeDirWritableOwnerAndDevice(t *testing.T) {
	for _, tc := range []struct {
		name      string
		uidDelta  int
		devDelta  uint64
		writeable bool
	}{
		{"own uid and device", 0, 0, true},
		{"other uid", 1, 0, false},
		{"other device", 0, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "d")
			if err := os.Mkdir(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			fd, err := syscall.Open(dir, unix.O_PATH|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Close(fd)
			var st syscall.Stat_t
			if err := syscall.Fstat(fd, &st); err != nil {
				t.Fatal(err)
			}
			visited := 0
			if err := tempRootMakeDirWritable(context.Background(), fd, &st, st.Dev+tc.devDelta, os.Geteuid()+tc.uidDelta, &visited); err != nil {
				t.Fatal(err)
			}
			if got := lstatMode(t, dir)&0o200 != 0; got != tc.writeable {
				t.Fatalf("owner write = %v, want %v (mode %o)", got, tc.writeable, lstatMode(t, dir))
			}
		})
	}
}

// The repair stops at its directory bound with an error.
func TestMakeTempRootWritableBound(t *testing.T) {
	old := tempRootWritableLimit
	tempRootWritableLimit = 2
	t.Cleanup(func() { tempRootWritableLimit = old })
	scan := t.TempDir()
	root := readOnlyTempRoot(t, scan, "nix-shell.ro0004", 72*time.Hour)
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := makeTempRootWritable(context.Background(), root, info); err == nil || !strings.Contains(err.Error(), "bound of 2 directories") {
		t.Fatalf("want the bound error, got %v", err)
	}
}

// Through the lane. With dev_artifacts.nix_temp_root_make_writable off, the
// read-only root at the head of the queue fails, is reported once over two
// passes, and the next root in the same pass is still removed. With the key
// on, both go.
func TestNixTempRootLaneReadOnlyHead(t *testing.T) {
	for _, writable := range []bool{false, true} {
		t.Run(map[bool]string{false: "key off", true: "key on"}[writable], func(t *testing.T) {
			scan := t.TempDir()
			head := readOnlyTempRoot(t, scan, "nix-shell.head01", 96*time.Hour)
			next := nixRootFixture(t, scan, "nix-shell.next01")
			cfg := nixTempRootConfig(scan).DevArtifacts
			cfg.NixTempRootMinMB = 0
			cfg.NixTempRootMakeWritable = writable
			p := newDevArtifactsPluginWithActive(nil)
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			p.cleanNixTemporaryRoots(context.Background(), scan, 0, 24*time.Hour, cfg, map[string]string{}, logger)
			if pathExists(next) {
				t.Fatal("the root after a failed head must still be removed in the same pass")
			}
			p.cleanNixTemporaryRoots(context.Background(), scan, 0, 24*time.Hour, cfg, map[string]string{}, logger)
			reports := strings.Count(logs.String(), "dev artifact path is not deletable")
			if writable {
				if pathExists(head) || reports != 0 {
					t.Fatalf("with the key on the read-only root must go; exists=%v reports=%d", pathExists(head), reports)
				}
				return
			}
			if !pathExists(head) || reports != 1 {
				t.Fatalf("with the key off the read-only root stays and is reported once; exists=%v reports=%d\n%s", pathExists(head), reports, logs.String())
			}
		})
	}
}
