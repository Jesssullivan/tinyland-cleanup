package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

func writeProcCgroup(t *testing.T, process, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(process, "cgroup"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// procUserUnit answers one closed question; every other shape is unknown.
func TestProcUserUnit(t *testing.T) {
	uid := 1000
	base := "0::/user.slice/user-1000.slice/user@1000.service/"
	cases := []struct {
		name, body, unit string
		ok               bool
	}{
		{"unit in app.slice", base + "app.slice/cade-annotation-page.service\n", "cade-annotation-page.service", true},
		{"unit at the manager root", base + "foo.service\n", "foo.service", true},
		{"scope in nested slices", base + "app.slice/a.slice/run-p1.scope\n", "run-p1.scope", true},
		{"no trailing newline", base + "app.slice/x.service", "x.service", true},
		{"sub-cgroup of a delegated unit", base + "app.slice/x.service/payload\n", "", false},
		{"unit under a non-slice parent", base + "x.service/y.service\n", "", false},
		{"the manager itself", base + "init.scope/\n", "", false},
		{"bare suffix", base + "app.slice/.service\n", "", false},
		{"not a unit", base + "app.slice/x.socket\n", "", false},
		{"another user's manager", "0::/user.slice/user-1001.slice/user@1001.service/app.slice/x.service\n", "", false},
		{"system unit", "0::/system.slice/x.service\n", "", false},
		{"hybrid hierarchy", "1:name=systemd:/user.slice\n" + base + "app.slice/x.service\n", "", false},
		{"v1 only", "1:name=systemd:/user.slice/user-1000.slice/user@1000.service/app.slice/x.service\n", "", false},
		{"extra blank line", base + "app.slice/x.service\n\n", "", false},
		{"empty", "", "", false},
	}
	for _, c := range cases {
		dir := t.TempDir()
		writeProcCgroup(t, dir, c.body)
		unit, ok := procUserUnit(dir, uid)
		if unit != c.unit || ok != c.ok {
			t.Errorf("%s: got %q %v, want %q %v", c.name, unit, ok, c.unit, c.ok)
		}
	}
	if _, ok := procUserUnit(t.TempDir(), uid); ok {
		t.Error("a missing cgroup file must be unknown")
	}
}

// An unreadable owned process in a listed unit leaves the inventory
// complete; the same process outside the list, or in a sub-cgroup of the
// listed unit, still makes it incomplete and names the unit.
func TestTemporaryProcActivityUninspectableUnits(t *testing.T) {
	scan := t.TempDir()
	held := filepath.Join(scan, "nix-shell.held")
	if err := os.MkdirAll(held, 0o700); err != nil {
		t.Fatal(err)
	}
	uid := os.Geteuid()
	unitLine := fmt.Sprintf("0::/user.slice/user-%d.slice/user@%d.service/app.slice/", uid, uid)
	proc := newFakeProcTree(t)
	proc.add(100, uid, "bash", "S", held)
	service := proc.add(200, uid, "python3", "S", "/")
	writeProcCgroup(t, service, unitLine+"cade-annotation-page.service\n")
	denyProcReads(t, []string{service}, []string{service})
	units := []string{"cade-annotation-page.service"}

	roots, err := temporaryRootActivityFromProc(context.Background(), proc.dir, []string{scan}, "", nil, units)
	if err != nil {
		t.Fatalf("a listed unit must leave the inventory complete: %v", err)
	}
	if temporaryRootActivityReason(roots, held) == "" {
		t.Fatalf("a readable holder's cwd must still be recorded: %#v", roots)
	}

	_, err = temporaryRootActivityFromProc(context.Background(), proc.dir, []string{scan}, "", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "pid 200 (python3) in unit cade-annotation-page.service") {
		t.Fatalf("an unlisted unit must fail closed and name the unit, got %v", err)
	}
	_, err = temporaryRootActivityFromProc(context.Background(), proc.dir, []string{scan}, "", nil, []string{"other.service"})
	if err == nil {
		t.Fatal("a different listed unit must not cover this process")
	}

	writeProcCgroup(t, service, unitLine+"cade-annotation-page.service/payload\n")
	_, err = temporaryRootActivityFromProc(context.Background(), proc.dir, []string{scan}, "", nil, units)
	if err == nil || !strings.Contains(err.Error(), "pid 200 (python3)") {
		t.Fatalf("a sub-cgroup of a listed unit must fail closed, got %v", err)
	}

	writeProcCgroup(t, service, fmt.Sprintf("0::/user.slice/user-%d.slice/user@%d.service/app.slice/cade-annotation-page.service\n", uid+1, uid+1))
	if _, err = temporaryRootActivityFromProc(context.Background(), proc.dir, []string{scan}, "", nil, units); err == nil {
		t.Fatal("a unit under another user's manager must fail closed")
	}
}

// End to end through Cleanup on the real Linux inventory path: the config key
// reaches the inventory, a stale Nix root is reaped past a listed unit's
// unreadable process, and kept when the unit is not listed.
func TestTempLaneConfigUnitsReachInventory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the /proc inventory is Linux only")
	}
	run := func(t *testing.T, units []string, timeout string) string {
		tmp := t.TempDir()
		root := filepath.Join(tmp, "nix-shell.dead01")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "env-vars"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		ageTempRoot(t, root, 72*time.Hour)
		uid := os.Geteuid()
		proc := newFakeProcTree(t)
		service := proc.add(200, uid, "python3", "S", "/")
		writeProcCgroup(t, service, fmt.Sprintf("0::/user.slice/user-%d.slice/user@%d.service/app.slice/hardened.service\n", uid, uid))
		denyProcReads(t, []string{service}, []string{service})
		saved := tempProcRoot
		tempProcRoot = proc.dir
		t.Cleanup(func() { tempProcRoot = saved })

		cfg := tempGeneratedArtifactConfig(tmp)
		cfg.DevArtifacts.NixTempRoots = true
		cfg.DevArtifacts.NixTempRootMinMB = 0
		cfg.DevArtifacts.NixTempRootStaleAfter = "24h"
		cfg.DevArtifacts.TempUninspectableOKUnits = units
		cfg.DevArtifacts.TempActivityProbeTimeout = timeout
		p := newDevArtifactsPluginWithActive(nil)
		p.tempActivity = nil // the real inventory, reading tempProcRoot
		p.Cleanup(context.Background(), LevelCritical, cfg, devArtifactTestLogger())
		return root
	}
	if root := run(t, nil, ""); !pathExists(root) {
		t.Fatal("an unreadable process in an unlisted unit must preserve the temp path")
	}
	// A deadline too short for any inventory reaches the probe and keeps
	// the root, listed unit or not.
	if root := run(t, []string{"hardened.service"}, "1ns"); !pathExists(root) {
		t.Fatal("an inventory cut by temp_activity_probe_timeout must preserve the temp path")
	}
	if root := run(t, []string{"hardened.service"}, "60s"); pathExists(root) {
		t.Fatal("a stale Nix root must be reaped once the unit is listed")
	}
}

func TestTempActivityProbeTimeout(t *testing.T) {
	for raw, want := range map[string]time.Duration{"": 10 * time.Second, "30s": 30 * time.Second, "0": 10 * time.Second, "-1s": 10 * time.Second, "soon": 10 * time.Second} {
		if got := tempActivityProbeTimeout(config.DevArtifactsConfig{TempActivityProbeTimeout: raw}); got != want {
			t.Errorf("%q: got %s, want %s", raw, got, want)
		}
	}
	if got := config.DefaultConfig().DevArtifacts.TempActivityProbeTimeout; got != "10s" {
		t.Errorf("default = %q, want 10s", got)
	}
}
