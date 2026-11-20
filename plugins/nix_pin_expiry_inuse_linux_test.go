//go:build linux

package plugins

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakePinProc builds a /proc tree with one process of this user. The real
// bbScanProc reads it; no process is inspected or started.
func fakePinProc(t *testing.T, pid string, cmdline string, links map[string]string, maps string) string {
	t.Helper()
	root := t.TempDir()
	d := filepath.Join(root, pid)
	if err := os.MkdirAll(filepath.Join(d, "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "cmdline"), []byte(cmdline), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "maps"), []byte(maps), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(d, name)); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestPinExpiryFakeProcFdSparesPin(t *testing.T) {
	f := newPinFixture(t)
	used := f.pin(t, "gcroots/kitpip", 22)
	idle := f.pin(t, "gcroots/idle", 22)
	proc := fakePinProc(t, "4242", "sh\x00-c\x00run\x00", map[string]string{
		"cwd":  "/home",
		"fd/3": filepath.Join(pinTarget(f, used), "lib", "x.so"),
	}, "")
	f.p.pinActivity = func(ctx context.Context, opts bbActivityOptions) *bbActivity {
		return bbScanProc(ctx, proc, -1, os.Getuid(), opts)
	}
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	if !pinExists(used) || pinExists(idle) {
		t.Fatal("the fd-held pin was removed or the idle one kept")
	}
}

func TestPinExpiryFakeProcMapsAndExeSparePins(t *testing.T) {
	f := newPinFixture(t)
	mapped := f.pin(t, "m", 30)
	exe := f.pin(t, "e", 30)
	maps := "7f00-7f01 r-xp 00000000 fd:01 1234  " + filepath.Join(pinTarget(f, mapped), "lib", "libfoo.so") + "\n"
	proc := fakePinProc(t, "77", "tool\x00", map[string]string{
		"cwd": filepath.Join(pinTarget(f, exe), "share"),
	}, maps)
	f.p.pinActivity = func(ctx context.Context, opts bbActivityOptions) *bbActivity {
		return bbScanProc(ctx, proc, -1, os.Getuid(), opts)
	}
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 0 {
		t.Fatalf("removed %d, want 0", n)
	}
	if !pinExists(mapped) || !pinExists(exe) {
		t.Fatal("a mapped or cwd-held pin was removed")
	}
}

func TestPinExpiryFakeLsofSparesPin(t *testing.T) {
	f := newPinFixture(t)
	used := f.pin(t, "l", 30)
	dir := t.TempDir()
	lsof := filepath.Join(dir, "lsof")
	script := "#!/bin/sh\nprintf 'p999\\nn%s\\n' '" + filepath.Join(pinTarget(f, used), "bin", "x") + "'\n"
	if err := os.WriteFile(lsof, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.cfg.Nix.PinExpiryLsof = lsof
	proc := t.TempDir() // an empty /proc: only the lsof listing names the path
	f.p.pinActivity = func(ctx context.Context, opts bbActivityOptions) *bbActivity {
		return bbScanProc(ctx, proc, -1, os.Getuid(), opts)
	}
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 0 || !pinExists(used) {
		t.Fatalf("an lsof-listed pin was removed (n=%d)", n)
	}
	// A failing lsof makes the inventory incomplete: still spared.
	if err := os.WriteFile(lsof, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 0 || !pinExists(used) {
		t.Fatalf("removed on a failed lsof (n=%d)", n)
	}
}
