package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

func releasedRootFixture(t *testing.T) (string, string, config.DevArtifactsConfig) {
	t.Helper()
	scan := t.TempDir()
	root := filepath.Join(scan, "owned-candidate")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	receipt, _ := json.Marshal(temporaryCustodyReceipt{Path: canonicalTempArtifactPath(root), OwnerUID: os.Geteuid(), State: "released", Receipt: "TIN-5128 owned fixture release"})
	if err := os.WriteFile(filepath.Join(root, disposableTempReceipt), receipt, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifact"), []byte("rebuildable fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	ageFixture(t, root)
	cfg := config.DefaultConfig().DevArtifacts
	cfg.TempRootCleanup = true
	cfg.TempScanPaths = []string{scan}
	cfg.TempArtifactStaleAfter = "7d"
	return scan, root, cfg
}
func ageFixture(t *testing.T, root string) {
	t.Helper()
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, old, old)
	}); err != nil {
		t.Fatal(err)
	}
}
func custodyTestPlugin() *DevArtifactsPlugin {
	return &DevArtifactsPlugin{tempActivity: func(context.Context, []string, string) (map[string]string, error) { return map[string]string{}, nil }}
}
func TestConfiguredTempActivityPaths(t *testing.T) {
	scan := t.TempDir()
	root := filepath.Join(scan, "job")
	output := "bash bash -c tool --root=" + filepath.Join(root, "output") + "\n"
	roots := tempArtifactRootsFromProcessOutput(output, []string{scan}, "")
	if temporaryRootActivityReason(roots, root) == "" {
		t.Fatal("configured root reference missed")
	}
	files := "p123\x00\nn" + filepath.Join(root, "has spaces", "output") + "\x00\n"
	if temporaryRootActivityReason(tempRootsFromOpenFiles(files, []string{scan}, ""), root) == "" {
		t.Fatal("open file in configured root missed")
	}
	if temporaryRootActivityReason(tempRootsFromOpenFiles("p123\x00n"+scan+"-neighbor/job\x00", []string{scan}, ""), root) != "" {
		t.Fatal("prefix collision protected unrelated root")
	}
}
func TestDisposableTempRootSafeguards(t *testing.T) {
	cases := []string{"released", "missing receipt", "unreleased", "foreign receipt", "missing uid", "git clean", "git ignored", "nested git", "recent child", "protected child", "unknown activity", "active cwd", "replaced identity", "disabled"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			scan, root, cfg := releasedRootFixture(t)
			p := custodyTestPlugin()
			oldInfo, _ := os.Lstat(root)
			switch name {
			case "missing receipt":
				os.Remove(filepath.Join(root, disposableTempReceipt))
			case "unreleased":
				os.WriteFile(filepath.Join(root, disposableTempReceipt), []byte(fmt.Sprintf(`{"path":%q,"owner_uid":%d,"state":"active","receipt":"fixture"}`, canonicalTempArtifactPath(root), os.Geteuid())), 0600)
			case "missing uid":
				os.WriteFile(filepath.Join(root, disposableTempReceipt), []byte(fmt.Sprintf(`{"path":%q,"state":"released","receipt":"fixture"}`, canonicalTempArtifactPath(root))), 0600)
			case "foreign receipt":
				os.WriteFile(filepath.Join(root, disposableTempReceipt), []byte(fmt.Sprintf(`{"path":%q,"owner_uid":%d,"state":"released","receipt":"fixture"}`, canonicalTempArtifactPath(root), os.Geteuid()+1)), 0600)
			case "git clean", "git ignored":
				os.Mkdir(filepath.Join(root, ".git"), 0700)
				ageFixture(t, root)
			case "nested git":
				os.MkdirAll(filepath.Join(root, "nested", ".git"), 0700)
				ageFixture(t, root)
			case "recent child":
				os.WriteFile(filepath.Join(root, "artifact"), []byte("new activity"), 0600)
				old := time.Now().Add(-8 * 24 * time.Hour)
				os.Chtimes(root, old, old)
			case "protected child":
				cfg.ProtectPaths = []string{filepath.Join(root, "artifact")}
			case "unknown activity":
				p.tempActivity = func(context.Context, []string, string) (map[string]string, error) {
					return nil, fmt.Errorf("inventory denied")
				}
			case "active cwd":
				p.tempActivity = func(context.Context, []string, string) (map[string]string, error) {
					return map[string]string{canonicalTempArtifactPath(root): "cwd held"}, nil
				}
			case "replaced identity":
				os.Rename(root, root+"-preserved")
				os.Mkdir(root, 0700)
				if p.recheckTemporaryRoot(context.Background(), root, oldInfo, 7*24*time.Hour, cfg) == "" {
					t.Fatal("replacement identity allowed")
				}
				return
			case "disabled":
				cfg.TempRootCleanup = false
			}
			var targets []CleanupTarget
			p.planDisposableTemporaryRoots(context.Background(), scan, "", LevelCritical, cfg, nil, &targets, newDevArtifactScanBudget(cfg))
			if name == "released" {
				if len(targets) != 1 || targets[0].Action != "delete" {
					t.Fatalf("released fixture not eligible: %#v", targets)
				}
			} else {
				for _, target := range targets {
					if target.Action == "delete" {
						t.Fatalf("protected fixture eligible: %#v", target)
					}
				}
			}
		})
	}
}
func TestDisposableTempRootRechecksAfterPlan(t *testing.T) {
	scan, root, cfg := releasedRootFixture(t)
	p := custodyTestPlugin()
	calls := 0
	p.tempActivity = func(context.Context, []string, string) (map[string]string, error) {
		calls++
		if calls > 1 {
			return map[string]string{canonicalTempArtifactPath(root): "new open file"}, nil
		}
		return map[string]string{}, nil
	}
	p.removeAll = func(string) error { t.Fatal("deleted root after it became active"); return nil }
	freed := p.cleanDisposableTemporaryRoots(context.Background(), scan, "", LevelCritical, cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), newDevArtifactScanBudget(cfg))
	if freed != 0 || !pathExists(root) || calls < 2 {
		t.Fatalf("fresh protection failed: freed %d calls %d", freed, calls)
	}
}
func TestDisposableTempRootDeletesOwnedIdleFixture(t *testing.T) {
	scan, root, cfg := releasedRootFixture(t)
	p := custodyTestPlugin()
	freed := p.cleanDisposableTemporaryRoots(context.Background(), scan, "", LevelCritical, cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), newDevArtifactScanBudget(cfg))
	if pathExists(root) || freed <= 0 {
		t.Fatalf("owned fixture not reaped: freed %d", freed)
	}
}

func TestTemporaryProcActivityCustody(t *testing.T) {
	proc := t.TempDir()
	scan := t.TempDir()
	root := filepath.Join(scan, "job")
	// The root exists so it canonicalizes like the scan path (Darwin /var).
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	process := filepath.Join(proc, "123")
	if err := os.MkdirAll(filepath.Join(process, "fd"), 0700); err != nil {
		t.Fatal(err)
	}
	status := fmt.Sprintf("Uid: %d %d %d %d\n", os.Geteuid(), os.Geteuid(), os.Geteuid(), os.Geteuid())
	if err := os.WriteFile(filepath.Join(process, "status"), []byte(status), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(process, "cwd")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "has spaces"), filepath.Join(process, "fd", "7")); err != nil {
		t.Fatal(err)
	}
	roots, err := temporaryRootActivityFromProc(context.Background(), proc, []string{scan}, "", nil)
	if err != nil || temporaryRootActivityReason(roots, root) == "" {
		t.Fatalf("own cwd/fd reference missed: %#v %v", roots, err)
	}
	os.WriteFile(filepath.Join(process, "status"), []byte("ownership unreadable"), 0600)
	if _, err := temporaryRootActivityFromProc(context.Background(), proc, []string{scan}, "", nil); err == nil {
		t.Fatal("unknown process ownership accepted")
	}
	os.WriteFile(filepath.Join(process, "status"), []byte(fmt.Sprintf("Uid: %d %d %d %d\n", os.Geteuid()+1, os.Geteuid()+1, os.Geteuid()+1, os.Geteuid()+1)), 0600)
	roots, err = temporaryRootActivityFromProc(context.Background(), proc, []string{scan}, "", nil)
	if err != nil || len(roots) != 0 {
		t.Fatalf("foreign process treated as own: %#v %v", roots, err)
	}
}

// fakeProcTree lays out /proc-shaped fixtures for temporaryRootActivityFromProc.
type fakeProcTree struct {
	t   *testing.T
	dir string
}

func newFakeProcTree(t *testing.T) *fakeProcTree {
	t.Helper()
	return &fakeProcTree{t: t, dir: t.TempDir()}
}

// add writes one process: status (ownership), stat (comm and state), an
// optional cwd link and one fd link per entry of fds. It returns the
// process directory.
func (f *fakeProcTree) add(pid, uid int, comm, state, cwd string, fds ...string) string {
	f.t.Helper()
	process := filepath.Join(f.dir, strconv.Itoa(pid))
	if err := os.MkdirAll(filepath.Join(process, "fd"), 0o700); err != nil {
		f.t.Fatal(err)
	}
	status := fmt.Sprintf("Name:\t%s\nUid:\t%d\t%d\t%d\t%d\n", comm, uid, uid, uid, uid)
	stat := fmt.Sprintf("%d (%s) %s 1 %d %d 0\n", pid, comm, state, pid, pid)
	for name, body := range map[string]string{"status": status, "stat": stat} {
		if err := os.WriteFile(filepath.Join(process, name), []byte(body), 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	if cwd != "" {
		if err := os.Symlink(cwd, filepath.Join(process, "cwd")); err != nil {
			f.t.Fatal(err)
		}
	}
	for i, target := range fds {
		if err := os.Symlink(target, filepath.Join(process, "fd", strconv.Itoa(i+3))); err != nil {
			f.t.Fatal(err)
		}
	}
	return process
}

// denyProcReads makes cwd and fd reads of the given process directories fail
// with EACCES, as the kernel does for a non-dumpable process. The fixture
// cannot use file modes for this: the status file must stay readable, and
// root ignores modes.
func denyProcReads(t *testing.T, cwd []string, fd []string) {
	t.Helper()
	denied := func(path string, dirs []string) bool {
		for _, d := range dirs {
			if path == d || strings.HasPrefix(path, d+string(os.PathSeparator)) {
				return true
			}
		}
		return false
	}
	readlink, readDir := tempProcReadlink, tempProcReadDir
	t.Cleanup(func() { tempProcReadlink, tempProcReadDir = readlink, readDir })
	tempProcReadlink = func(path string) (string, error) {
		if filepath.Base(path) == "cwd" && denied(filepath.Dir(path), cwd) {
			return "", &fs.PathError{Op: "readlink", Path: path, Err: fs.ErrPermission}
		}
		if filepath.Base(filepath.Dir(path)) == "fd" && denied(filepath.Dir(filepath.Dir(path)), fd) {
			return "", &fs.PathError{Op: "readlink", Path: path, Err: fs.ErrPermission}
		}
		return readlink(path)
	}
	tempProcReadDir = func(path string) ([]os.DirEntry, error) {
		if filepath.Base(path) == "fd" && denied(filepath.Dir(path), fd) {
			return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}
		}
		return readDir(path)
	}
}

// The non-dumpable same-user processes seen on sting (tailscaled's SSH
// incubator, the systemd user manager, (sd-pam)) and a zombie leave the
// inventory complete; any other unreadable owned process does not.
func TestTemporaryProcActivityUninspectableComms(t *testing.T) {
	scan := t.TempDir()
	held := filepath.Join(scan, "nix-shell.held")
	partly := filepath.Join(scan, "nix-shell.partly")
	for _, root := range []string{filepath.Join(held, "src"), partly} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	uid := os.Geteuid()
	proc := newFakeProcTree(t)
	proc.add(100, uid, "bash", "S", filepath.Join(held, "src"))
	unreadable := []string{
		proc.add(200, uid, "tailscaled", "S", "/"),
		proc.add(201, uid, "systemd", "S", "/"),
		proc.add(202, uid, "(sd-pam)", "S", "/"),
		proc.add(203, uid, "atuin", "Z", ""),
	}
	// An allowed comm whose cwd is readable but whose fds are not: what it
	// exposes is still recorded.
	allowedPartly := proc.add(204, uid, "tailscaled", "S", partly)
	denyProcReads(t, unreadable, append(append([]string{}, unreadable...), allowedPartly))
	okComms := config.DefaultConfig().DevArtifacts.TempUninspectableOKComms

	roots, err := temporaryRootActivityFromProc(context.Background(), proc.dir, []string{scan}, "", okComms)
	if err != nil {
		t.Fatalf("allowed comms and a zombie must leave the inventory complete: %v", err)
	}
	if temporaryRootActivityReason(roots, held) == "" {
		t.Fatalf("a readable holder's cwd must still be recorded: %#v", roots)
	}
	if temporaryRootActivityReason(roots, partly) == "" {
		t.Fatalf("an allowed comm's readable cwd must still be recorded: %#v", roots)
	}

	if _, err := temporaryRootActivityFromProc(context.Background(), proc.dir, []string{scan}, "", nil); err == nil || !strings.Contains(err.Error(), "owned process cwd unavailable") {
		t.Fatalf("with no allowed comms an unreadable owned process must make the inventory incomplete, got %v", err)
	}

	other := proc.add(300, uid, "atuin", "S", "/")
	denyProcReads(t, []string{other}, nil)
	_, err = temporaryRootActivityFromProc(context.Background(), proc.dir, []string{scan}, "", okComms)
	if err == nil || !strings.Contains(err.Error(), "pid 300 (atuin)") {
		t.Fatalf("an unreadable owned process outside the list must fail closed and name itself, got %v", err)
	}
}

func TestTemporaryProcActivityUnreadableFDsOutsideListFailClosed(t *testing.T) {
	scan := t.TempDir()
	proc := newFakeProcTree(t)
	process := proc.add(100, os.Geteuid(), "python3", "S", "/")
	denyProcReads(t, nil, []string{process})
	_, err := temporaryRootActivityFromProc(context.Background(), proc.dir, []string{scan}, "", config.DefaultConfig().DevArtifacts.TempUninspectableOKComms)
	if err == nil || !strings.Contains(err.Error(), "open files unavailable") {
		t.Fatalf("unreadable open files of an owned process must fail closed, got %v", err)
	}
}

func ageTempRoot(t *testing.T, root string, age time.Duration) {
	t.Helper()
	when := time.Now().Add(-age)
	if err := filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, when, when)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTempScanRootsOldestFirst(t *testing.T) {
	scan := t.TempDir()
	now := time.Now().Truncate(time.Second)
	for name, age := range map[string]time.Duration{"a-new": time.Hour, "m-mid": 48 * time.Hour, "z-old": 96 * time.Hour, "b-tie": 48 * time.Hour} {
		root := filepath.Join(scan, name)
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(root, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	roots, err := tempScanRootsOldestFirst(scan)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, root := range roots {
		got = append(got, root.name)
	}
	if want := "z-old b-tie m-mid a-new"; strings.Join(got, " ") != want {
		t.Fatalf("order %q, want %q (oldest first, then name)", strings.Join(got, " "), want)
	}
}

// With more stale roots than temp_scan_max_roots, a cycle reclaims the oldest
// up to the budget and the next cycle continues; names sort opposite to age
// here, so name order would pick the newest.
func TestNixTempRootsOldestFirstWithinRootBudget(t *testing.T) {
	scan := t.TempDir()
	names := []string{"nix-shell.aaaaaa", "nix-shell.bbbbbb", "nix-shell.cccccc", "nix-shell.dddddd", "nix-shell.eeeeee", "nix-shell.ffffff"}
	for i, name := range names {
		root := filepath.Join(scan, name)
		if err := os.MkdirAll(filepath.Join(root, "build"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "build", "out"), []byte("rebuildable"), 0o600); err != nil {
			t.Fatal(err)
		}
		// aaaaaa is the newest (2 days), ffffff the oldest (7 days).
		ageTempRoot(t, root, time.Duration(48+24*i)*time.Hour)
	}
	cfg := config.DefaultConfig().DevArtifacts
	cfg.TempScanPaths = []string{scan}
	cfg.TempScanMaxRoots = 3
	cfg.NixTempRootMinMB = 0
	p := &DevArtifactsPlugin{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	budget := newDevArtifactScanBudget(cfg)
	p.cleanNixTemporaryRoots(context.Background(), scan, nixTempRootMinBytes(cfg), 24*time.Hour, nil, map[string]string{}, logger, budget)
	for i, name := range names {
		if gone := !pathExists(filepath.Join(scan, name)); gone != (i >= 3) {
			t.Errorf("%s removed=%v; the three oldest (dddddd..ffffff) must go first", name, gone)
		}
	}
	if !budget.exhausted() {
		t.Error("a path with more roots than the budget must report its truncation")
	}

	p.cleanNixTemporaryRoots(context.Background(), scan, nixTempRootMinBytes(cfg), 24*time.Hour, nil, map[string]string{}, logger, newDevArtifactScanBudget(cfg))
	for _, name := range names {
		if pathExists(filepath.Join(scan, name)) {
			t.Errorf("the next cycle must continue with the remaining roots: %s kept", name)
		}
	}
}

func TestNixTempRootMinMBZeroReapsEmptyRoots(t *testing.T) {
	if got := nixTempRootMinBytes(config.DevArtifactsConfig{NixTempRootMinMB: -1}); got != 1024*1024 {
		t.Fatalf("a negative floor must mean the 1 MB default, got %d", got)
	}
	scan := t.TempDir()
	root := filepath.Join(scan, "nix-develop-4242-0")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ageTempRoot(t, root, 72*time.Hour)
	p := &DevArtifactsPlugin{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p.cleanNixTemporaryRoots(context.Background(), scan, nixTempRootMinBytes(config.DevArtifactsConfig{NixTempRootMinMB: 1}), 24*time.Hour, nil, map[string]string{}, logger)
	if !pathExists(root) {
		t.Fatal("the 1 MB floor keeps an empty root")
	}
	p.cleanNixTemporaryRoots(context.Background(), scan, nixTempRootMinBytes(config.DevArtifactsConfig{NixTempRootMinMB: 0}), 24*time.Hour, nil, map[string]string{}, logger)
	if pathExists(root) {
		t.Fatal("nix_temp_root_min_mb 0 must reap an empty stale root")
	}
}

func TestNixTempRootNamesCoverShellAndDevelop(t *testing.T) {
	for _, name := range []string{"nix-shell.AbC123", "nix-shell.x9KQ2p", "nix-develop-12345-0", "nix-develop-1-42", "nix-12345-0", "nix-build-hello-2.12-3"} {
		if !isNixTemporaryRootName(name) {
			t.Errorf("%s must be a Nix temp root", name)
		}
	}
	for _, name := range []string{"claude-1000", "codex", "codex-sess1", ".tmpAbC123", "pi-session-xyz", "nix-shell.", "nix-develop-", "nix-develop-12345"} {
		if isNixTemporaryRootName(name) {
			t.Errorf("%s must not be a Nix temp root", name)
		}
	}
}

// End to end on one temp scan path at critical level: with the sting-shaped
// unreadable helpers present, stale Nix roots are reaped, a root held by a
// live cwd or a live open file is kept, and agent-session trees are never
// touched. An unreadable process outside the list still preserves the path.
func TestTempLaneReapsPastUninspectableHelpersAndKeepsHeldAndSessionRoots(t *testing.T) {
	run := func(t *testing.T, extraUnreadable string) (tmp string, sessions []string, control string) {
		tmp = t.TempDir()
		sessions, control = agentSessionTempFixture(t, tmp)
		// A released custody receipt does not free a session tree either.
		receiptRoot := filepath.Join(tmp, ".tmpAbC123")
		receipt, _ := json.Marshal(temporaryCustodyReceipt{Path: canonicalTempArtifactPath(receiptRoot), OwnerUID: os.Geteuid(), State: "released", Receipt: "fixture"})
		if err := os.WriteFile(filepath.Join(receiptRoot, disposableTempReceipt), receipt, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"nix-shell.dead01", "nix-develop-1-0", "nix-shell.cwd001", "nix-develop-2-0"} {
			if err := os.Mkdir(filepath.Join(tmp, name), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(tmp, "nix-shell.dead01", "env-vars"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmp, "nix-develop-2-0", "lock"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"nix-shell.dead01", "nix-develop-1-0", "nix-shell.cwd001", "nix-develop-2-0", ".tmpAbC123"} {
			ageTempRoot(t, filepath.Join(tmp, name), 72*time.Hour)
		}

		uid := os.Geteuid()
		proc := newFakeProcTree(t)
		proc.add(10, uid, "bash", "S", filepath.Join(tmp, "nix-shell.cwd001"))
		proc.add(11, uid, "nix", "S", "/", filepath.Join(tmp, "nix-develop-2-0", "lock"))
		unreadable := []string{
			proc.add(20, uid, "tailscaled", "S", "/"),
			proc.add(21, uid, "systemd", "S", "/"),
			proc.add(22, uid, "(sd-pam)", "S", "/"),
			proc.add(23, uid, "atuin", "Z", ""),
		}
		if extraUnreadable != "" {
			unreadable = append(unreadable, proc.add(30, uid, extraUnreadable, "S", "/"))
		}
		denyProcReads(t, unreadable, unreadable)

		cfg := tempGeneratedArtifactConfig(tmp)
		cfg.DevArtifacts.NodeModules = true
		cfg.DevArtifacts.NixTempRootMinMB = 0
		cfg.DevArtifacts.TempRootCleanup = true
		cfg.DevArtifacts.HarnessScratch = true
		cfg.DevArtifacts.HarnessScratchDirPatterns = []string{"claude-*", "codex*", ".tmp*", "pi-*"}
		cfg.DevArtifacts.HarnessScratchStaleAfter = "2h"
		okComms := cfg.DevArtifacts.TempUninspectableOKComms
		p := newDevArtifactsPluginWithActive(nil)
		p.tempActivity = func(ctx context.Context, paths []string, home string) (map[string]string, error) {
			return temporaryRootActivityFromProc(ctx, proc.dir, paths, home, okComms)
		}
		p.Cleanup(context.Background(), LevelCritical, cfg, devArtifactTestLogger())
		return tmp, sessions, control
	}
	assertSessionsKept := func(t *testing.T, sessions []string) {
		t.Helper()
		for _, s := range sessions {
			for _, f := range []string{"notes.md", filepath.Join("node_modules", "pkg", "index.js")} {
				if !pathExists(filepath.Join(s, f)) {
					t.Errorf("agent-session file %s was deleted", filepath.Join(s, f))
				}
			}
		}
	}

	t.Run("allowed helpers", func(t *testing.T) {
		tmp, sessions, control := run(t, "")
		for _, name := range []string{"nix-shell.dead01", "nix-develop-1-0"} {
			if pathExists(filepath.Join(tmp, name)) {
				t.Errorf("stale %s must be reaped once the inventory is complete", name)
			}
		}
		if !pathExists(filepath.Join(tmp, "nix-shell.cwd001")) {
			t.Error("a root that is a live process cwd must be kept")
		}
		if !pathExists(filepath.Join(tmp, "nix-develop-2-0", "lock")) {
			t.Error("a root with a live open file must be kept")
		}
		assertSessionsKept(t, sessions)
		if pathExists(filepath.Join(control, "node_modules")) {
			t.Error("the plain temp tree's stale node_modules should still be removed")
		}
	})
	t.Run("unreadable process outside the list", func(t *testing.T) {
		tmp, sessions, control := run(t, "atuin")
		for _, name := range []string{"nix-shell.dead01", "nix-develop-1-0"} {
			if !pathExists(filepath.Join(tmp, name)) {
				t.Errorf("an incomplete inventory must preserve %s", name)
			}
		}
		if !pathExists(filepath.Join(control, "node_modules")) {
			t.Error("an incomplete inventory must preserve the whole temp path")
		}
		assertSessionsKept(t, sessions)
	})
}
