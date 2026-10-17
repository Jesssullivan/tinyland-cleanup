//go:build linux

package plugins

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// The helper process for the real /proc inventory tests: it holds a file of
// the tree only through a memory mapping (the fd is closed) or only through
// its cwd, then waits to be killed.
func init() {
	switch os.Getenv("SHM_SCRATCH_TEST_HELPER") {
	case "mmap":
		f, err := os.OpenFile(os.Getenv("SHM_SCRATCH_TEST_FILE"), os.O_RDWR, 0)
		if err != nil {
			os.Exit(3)
		}
		if _, err := syscall.Mmap(int(f.Fd()), 0, 4096, syscall.PROT_READ, syscall.MAP_SHARED); err != nil {
			os.Exit(4)
		}
		f.Close()
		os.Stdout.WriteString("ready\n")
		time.Sleep(time.Minute)
		os.Exit(0)
	case "cwd":
		if err := os.Chdir(os.Getenv("SHM_SCRATCH_TEST_DIR")); err != nil {
			os.Exit(3)
		}
		os.Stdout.WriteString("ready\n")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

type shmFixture struct {
	t          *testing.T
	root       string
	plugin     *ShmScratchPlugin
	cfg        *config.Config
	scans      int
	held       map[string]string // path -> reason, applied from scan number heldFrom on
	from       int
	incomplete string
}

func newShmFixture(t *testing.T) *shmFixture {
	t.Helper()
	f := &shmFixture{t: t, root: t.TempDir(), held: map[string]string{}}
	f.plugin = &ShmScratchPlugin{
		uid:     os.Geteuid(),
		home:    t.TempDir(),
		tmpdir:  t.TempDir(),
		isTmpfs: func(string) (bool, error) { return true, nil },
		activity: func(_ context.Context, opts bbActivityOptions) *bbActivity {
			f.scans++
			a := newBBActivity()
			if f.scans >= f.from {
				for p, why := range f.held {
					a.index(p, why, opts.Within)
				}
			}
			if f.incomplete != "" {
				a.incomplete = append(a.incomplete, f.incomplete)
			}
			return a
		},
	}
	f.cfg = config.DefaultConfig()
	f.cfg.Enable.ShmScratch = true
	f.cfg.ShmScratch = config.ShmScratchConfig{Mode: config.ShmScratchModeApply, Roots: []string{f.root}, IdleAfter: "24h",
		UninspectableOKComms: config.DefaultConfig().ShmScratch.UninspectableOKComms}
	return f
}

// tree makes root/name with one file per path and sets every mtime age ago.
func (f *shmFixture) tree(name string, age time.Duration, files ...string) string {
	f.t.Helper()
	dir := filepath.Join(f.root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	for _, rel := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Repeat("x", 8192)), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	f.age(dir, age)
	return dir
}

func (f *shmFixture) age(dir string, age time.Duration) {
	f.t.Helper()
	when := time.Now().Add(-age)
	var paths []string
	_ = filepath.Walk(dir, func(p string, _ os.FileInfo, _ error) error { paths = append(paths, p); return nil })
	for i := len(paths) - 1; i >= 0; i-- {
		if err := os.Chtimes(paths[i], when, when); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *shmFixture) run() CleanupResult {
	return f.plugin.Cleanup(context.Background(), LevelNone, f.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func (f *shmFixture) plan() CleanupPlan {
	return f.plugin.PlanCleanup(context.Background(), LevelNone, f.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func targetFor(plan CleanupPlan, name string) (CleanupTarget, bool) {
	for _, t := range plan.Targets {
		if t.Name == name {
			return t, true
		}
	}
	return CleanupTarget{}, false
}

func TestShmScratchRemovesAnIdleUnheldDirectory(t *testing.T) {
	f := newShmFixture(t)
	dir := f.tree("handoff-review", 48*time.Hour, "target/debug/a.o", "notes.json")
	res := f.run()
	if res.Error != nil || exists(dir) {
		t.Fatalf("idle unheld directory kept: err=%v exists=%v", res.Error, exists(dir))
	}
	if res.ItemsCleaned != 1 || res.BytesFreed <= 0 {
		t.Fatalf("result not counted: %+v", res)
	}
}

func TestShmScratchReportModeRemovesNothing(t *testing.T) {
	f := newShmFixture(t)
	f.cfg.ShmScratch.Mode = config.ShmScratchModeReport
	dir := f.tree("old", 48*time.Hour, "a")
	if res := f.run(); res.ItemsCleaned != 0 || !exists(dir) {
		t.Fatalf("report mode removed something: %+v", res)
	}
	target, ok := targetFor(f.plan(), "old")
	if !ok || target.Action != "report" || target.Protected {
		t.Fatalf("report plan = %+v", target)
	}
}

func TestShmScratchKeepsTreesUnder24hIdle(t *testing.T) {
	f := newShmFixture(t)
	young := f.tree("young", 23*time.Hour, "a")
	mixed := f.tree("mixed", 72*time.Hour, "old", "new")
	recent := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(mixed, "new"), recent, recent); err != nil {
		t.Fatal(err)
	}
	f.run()
	if !exists(young) || !exists(mixed) {
		t.Fatal("a tree with a change inside 24h was removed")
	}
	target, _ := targetFor(f.plan(), "mixed")
	if !target.Protected || !strings.Contains(target.Reason, "recent activity") {
		t.Fatalf("mixed = %+v", target)
	}
}

func TestShmScratchKeepsHeldTrees(t *testing.T) {
	for _, why := range []string{"pid 7 is cwd", "pid 7 holds", "pid 7 maps"} {
		f := newShmFixture(t)
		dir := f.tree("held", 48*time.Hour, "seg/file")
		f.held[filepath.Join(dir, "seg", "file")] = why
		f.run()
		if !exists(dir) {
			t.Fatalf("%s: a held tree was removed", why)
		}
		target, _ := targetFor(f.plan(), "held")
		if !target.Protected || !target.Active || !strings.Contains(target.Reason, why) {
			t.Fatalf("%s: target = %+v", why, target)
		}
	}
}

func TestShmScratchIncompleteInventoryKeepsEverything(t *testing.T) {
	f := newShmFixture(t)
	f.incomplete = "pid 9 (x) fd unreadable"
	dir := f.tree("old", 48*time.Hour, "a")
	f.run()
	if !exists(dir) {
		t.Fatal("removed with an incomplete activity inventory")
	}
}

func TestShmScratchRechecksHoldersBeforeRemoval(t *testing.T) {
	f := newShmFixture(t)
	dir := f.tree("old", 48*time.Hour, "a")
	f.held[filepath.Join(dir, "a")] = "pid 8 holds it"
	f.from = 2 // clean at planning, held at the pre-removal recheck
	f.run()
	if !exists(dir) {
		t.Fatal("a holder that appeared after planning did not keep the tree")
	}
}

func TestShmScratchNeverTouchesFilesSymlinksOrSessionData(t *testing.T) {
	f := newShmFixture(t)
	file := filepath.Join(f.root, "libpod_rootless_lock_1000")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := f.tree("real", 48*time.Hour, "a")
	link := filepath.Join(f.root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, name := range []string{"claude-1000", "codex-x", ".tmpab12", "pi-session"} {
		kept = append(kept, f.tree(name, 48*time.Hour, "a"))
	}
	kept = append(kept, f.tree("nested-session", 48*time.Hour, ".claude/x"))
	kept = append(kept, f.tree("gitrepo", 48*time.Hour, ".git/HEAD"))
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(file, old, old)
	f.run()
	for _, p := range append(kept, file, link) {
		if !exists(p) {
			t.Fatalf("%s was removed", p)
		}
	}
	if exists(target) {
		t.Fatal("the plain idle tree was kept")
	}
	plan := f.plan()
	if _, ok := targetFor(plan, "libpod_rootless_lock_1000"); ok {
		t.Fatal("a top-level file became a candidate")
	}
	if _, ok := targetFor(plan, "link"); ok {
		t.Fatal("a symlink became a candidate")
	}
}

func TestShmScratchSkipsANonTmpfsRoot(t *testing.T) {
	f := newShmFixture(t)
	f.plugin.isTmpfs = func(string) (bool, error) { return false, nil }
	dir := f.tree("old", 48*time.Hour, "a")
	f.run()
	if !exists(dir) {
		t.Fatal("removed under a root that is not a tmpfs")
	}
	plan := f.plan()
	if len(plan.Targets) != 0 || len(plan.Warnings) == 0 || !strings.Contains(plan.Warnings[0], "not a tmpfs") {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestShmScratchValidate(t *testing.T) {
	good := config.ShmScratchConfig{Mode: "apply", Roots: []string{"/dev/shm"}, IdleAfter: "24h"}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]config.ShmScratchConfig{
		"under the 24h floor": {Mode: "apply", Roots: []string{"/dev/shm"}, IdleAfter: "23h"},
		"unknown mode":        {Mode: "delete", Roots: []string{"/dev/shm"}, IdleAfter: "48h"},
		"no roots":            {Mode: "apply", IdleAfter: "48h"},
		"relative root":       {Mode: "apply", Roots: []string{"dev/shm"}, IdleAfter: "48h"},
		"filesystem root":     {Mode: "apply", Roots: []string{"/"}, IdleAfter: "48h"},
	} {
		if bad.Validate() == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

// The real /proc inventory, with a child process that holds a file of the
// tree only through a memory mapping, then only through its cwd.
func TestShmScratchRealInventorySeesMappingsAndCwd(t *testing.T) {
	for _, mode := range []string{"mmap", "cwd"} {
		t.Run(mode, func(t *testing.T) {
			f := newShmFixture(t)
			dir := f.tree("held", 48*time.Hour, "seg")
			f.plugin.activity = nil // the real bbScanActivity
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(), "SHM_SCRATCH_TEST_HELPER="+mode,
				"SHM_SCRATCH_TEST_FILE="+filepath.Join(dir, "seg"), "SHM_SCRATCH_TEST_DIR="+dir)
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			line, err := bufio.NewReader(out).ReadString('\n')
			if err != nil || line != "ready\n" {
				t.Fatalf("helper not ready: %q %v", line, err)
			}
			a := bbScanActivity(context.Background(), bbActivityOptions{
				Within: []string{f.root}, OKComms: f.cfg.ShmScratch.UninspectableOKComms})
			if why := a.complete(); why != "" {
				t.Skipf("inventory incomplete in this environment: %s", why)
			}
			if live := a.blockers(dir); len(live) == 0 {
				t.Fatalf("the %s holder was not seen", mode)
			}
			f.run()
			if !exists(dir) {
				t.Fatalf("a tree held through %s was removed", mode)
			}
		})
	}
}
