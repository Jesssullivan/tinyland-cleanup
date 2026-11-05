package plugins

import (
	"bufio"
	"context"
	"errors"
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

type homeLanesFixture struct {
	t         *testing.T
	home      string
	cache     string
	installs  string
	checkouts string
	plugin    *HomeLanesPlugin
	cfg       *config.Config
	scans     int
	held      map[string]string
	heldFrom  int
	lockErr   error
	removed   []string
}

func newHomeLanesFixture(t *testing.T) *homeLanesFixture {
	t.Helper()
	home := t.TempDir()
	f := &homeLanesFixture{t: t, home: home, held: map[string]string{},
		cache:     filepath.Join(home, ".cache"),
		installs:  filepath.Join(home, ".hermes", "installs"),
		checkouts: filepath.Join(home, "checkouts"),
	}
	for _, d := range []string{f.cache, f.installs, f.checkouts} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.plugin = &HomeLanesPlugin{
		uid:    os.Geteuid(),
		home:   home,
		tmpdir: t.TempDir(),
		activity: func(_ context.Context, opts bbActivityOptions) *bbActivity {
			f.scans++
			a := newBBActivity()
			if f.scans >= f.heldFrom {
				for p, why := range f.held {
					a.index(p, why, opts.Within)
				}
			}
			return a
		},
		lockedInodes: func() (map[homeLanesInode]bool, error) {
			if f.lockErr != nil {
				return nil, f.lockErr
			}
			return homeLanesReadLockedInodes()
		},
	}
	f.cfg = config.DefaultConfig()
	f.cfg.Enable.HomeLanes = true
	f.cfg.HomeLanes = config.HomeLanesConfig{
		Mode:                 config.HomeLanesModeApply,
		CacheRoot:            f.cache,
		CacheLaneGlobs:       []string{"lane-*", "*-20[0-9][0-9][0-9][0-9][0-9][0-9]*"},
		CacheIdleAfter:       "7d",
		HermesInstallsRoot:   f.installs,
		HermesCheckoutRoots:  []string{f.checkouts},
		HermesIdleAfter:      "72h",
		MaxRemovalsPerCycle:  8,
		UninspectableOKComms: config.DefaultConfig().HomeLanes.UninspectableOKComms,
	}
	return f
}

func (f *homeLanesFixture) tree(dir string, age time.Duration, files ...string) string {
	f.t.Helper()
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
	f.age(dir, age, age)
	return dir
}

// age sets every mtime and atime in dir.
func (f *homeLanesFixture) age(dir string, mtimeAge, atimeAge time.Duration) {
	f.t.Helper()
	var paths []string
	_ = filepath.Walk(dir, func(p string, _ os.FileInfo, _ error) error { paths = append(paths, p); return nil })
	for i := len(paths) - 1; i >= 0; i-- {
		if err := os.Chtimes(paths[i], time.Now().Add(-atimeAge), time.Now().Add(-mtimeAge)); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *homeLanesFixture) lane(name string, age time.Duration, files ...string) string {
	f.t.Helper()
	return f.tree(filepath.Join(f.cache, name), age, append(files, "payload")...)
}

// install makes an install state whose record names checkout.
func (f *homeLanesFixture) install(key, checkout string, age time.Duration) string {
	f.t.Helper()
	dir := filepath.Join(f.installs, key)
	if err := os.MkdirAll(filepath.Join(dir, "inputs"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, homeLanesProjectRootRecord), []byte(checkout+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return f.tree(dir, age, "environments/gen/venv/bin/python", "pm-runtime/selected.json")
}

func (f *homeLanesFixture) run(level CleanupLevel) CleanupResult {
	return f.plugin.Cleanup(context.Background(), level, f.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func (f *homeLanesFixture) plan(level CleanupLevel) CleanupPlan {
	return f.plugin.PlanCleanup(context.Background(), level, f.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func (f *homeLanesFixture) reason(name string) string {
	f.t.Helper()
	target, ok := targetFor(f.plan(LevelCritical), name)
	if !ok {
		f.t.Fatalf("%s is not a candidate", name)
	}
	return target.Reason
}

const homeLanesOld = 30 * 24 * time.Hour

func TestHomeLanesRemovesStaleLaneAndOrphanedInstall(t *testing.T) {
	f := newHomeLanesFixture(t)
	lane := f.lane("lane-f-20261027", homeLanesOld)
	dated := f.lane("signer-g3-20261022T231238Z", homeLanesOld)
	orphan := f.install("aaaa", filepath.Join(f.checkouts, "gone"), homeLanesOld)
	res := f.run(LevelModerate)
	if res.Error != nil || res.ItemsCleaned != 3 || res.BytesFreed == 0 {
		t.Fatalf("result = %+v", res)
	}
	for _, p := range []string{lane, dated, orphan} {
		if exists(p) {
			t.Fatalf("%s survived", p)
		}
	}
	entries, _ := os.ReadDir(f.cache)
	if len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
}

func TestHomeLanesRemovesNothingInReportModeOrBelowModerate(t *testing.T) {
	f := newHomeLanesFixture(t)
	lane := f.lane("lane-a", homeLanesOld)
	orphan := f.install("aaaa", filepath.Join(f.checkouts, "gone"), homeLanesOld)

	f.run(LevelWarning)
	f.cfg.HomeLanes.Mode = config.HomeLanesModeReport
	res := f.run(LevelCritical)
	if res.ItemsCleaned != 0 || !exists(lane) || !exists(orphan) {
		t.Fatalf("removed in report mode or below moderate: %+v", res)
	}
	plan := f.plan(LevelCritical)
	if plan.WouldRun || plan.SkipReason == "" {
		t.Fatalf("plan = %+v", plan)
	}
	target, ok := targetFor(plan, "lane-a")
	if !ok || target.Action != "report" || target.Protected {
		t.Fatalf("target = %+v", target)
	}
}

func TestHomeLanesOnlyAnAllowlistedRealDirectoryIsALane(t *testing.T) {
	f := newHomeLanesFixture(t)
	tool := f.tree(filepath.Join(f.cache, "nix"), homeLanesOld, "payload")
	elsewhere := f.tree(filepath.Join(t.TempDir(), "kept"), homeLanesOld, "payload")
	link := filepath.Join(f.cache, "lane-link")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(f.cache, "lane-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := f.run(LevelCritical); res.ItemsCleaned != 0 {
		t.Fatalf("result = %+v", res)
	}
	for _, p := range []string{tool, link, file, filepath.Join(elsewhere, "payload")} {
		if !exists(p) {
			t.Fatalf("%s was removed", p)
		}
	}
	if plan := f.plan(LevelCritical); len(plan.Targets) != 0 {
		t.Fatalf("targets = %+v", plan.Targets)
	}
}

func TestHomeLanesKeepsEveryInstallThatIsNotProvablyOrphaned(t *testing.T) {
	f := newHomeLanesFixture(t)
	checkout := f.tree(filepath.Join(f.checkouts, "live"), homeLanesOld, "pyproject.toml")
	live := f.install("live", checkout, homeLanesOld)
	outside := f.install("outside", "/opt/data/hermes-agent", homeLanesOld)
	relative := f.install("relative", "hermes-agent", homeLanesOld)
	unrecorded := f.tree(filepath.Join(f.installs, "unrecorded"), homeLanesOld, "pm-runtime/selected.json")

	if res := f.run(LevelCritical); res.ItemsCleaned != 0 {
		t.Fatalf("result = %+v", res)
	}
	for _, p := range []string{live, outside, relative, unrecorded} {
		if !exists(p) {
			t.Fatalf("%s was removed", p)
		}
	}
	for name, want := range map[string]string{"live": "live install", "outside": "outside hermes_checkout_roots",
		"relative": "not an absolute path", "unrecorded": "no readable checkout record"} {
		if got := f.reason(name); !strings.Contains(got, want) {
			t.Fatalf("%s: reason %q lacks %q", name, got, want)
		}
	}
}

// An unmounted volume makes a checkout read as missing, and that must not
// read as deleted.
func TestHomeLanesInvisibleCheckoutRootProvesNothing(t *testing.T) {
	f := newHomeLanesFixture(t)
	unmounted := filepath.Join(f.home, "unmounted")
	f.cfg.HomeLanes.HermesCheckoutRoots = []string{unmounted}
	state := f.install("aaaa", filepath.Join(unmounted, "repo"), homeLanesOld)
	if res := f.run(LevelCritical); res.ItemsCleaned != 0 || !exists(state) {
		t.Fatalf("result = %+v", res)
	}
	if got := f.reason("aaaa"); !strings.Contains(got, "not visible") {
		t.Fatalf("reason = %q", got)
	}
}

func TestHomeLanesAgeFloors(t *testing.T) {
	f := newHomeLanesFixture(t)
	young := f.lane("lane-young", 6*24*time.Hour)
	used := f.lane("lane-used", homeLanesOld, "tool")
	// Only a file's access time is recent: read, not written.
	if err := os.Chtimes(filepath.Join(used, "tool"), time.Now(), time.Now().Add(-homeLanesOld)); err != nil {
		t.Fatal(err)
	}
	nested := f.lane("lane-nested", homeLanesOld, "deep/fresh")
	if err := os.Chtimes(filepath.Join(nested, "deep", "fresh"), time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	orphan := f.install("aaaa", filepath.Join(f.checkouts, "gone"), 48*time.Hour)
	if res := f.run(LevelCritical); res.ItemsCleaned != 0 {
		t.Fatalf("result = %+v", res)
	}
	for _, p := range []string{young, used, nested, orphan} {
		if !exists(p) {
			t.Fatalf("%s was removed", p)
		}
	}
}

// A sweep that reads every install's metadata must not read as use, while a
// read of the install's packages must.
func TestHomeLanesHermesAccessTimes(t *testing.T) {
	f := newHomeLanesFixture(t)
	swept := f.install("swept", filepath.Join(f.checkouts, "gone-a"), homeLanesOld)
	used := f.install("used", filepath.Join(f.checkouts, "gone-b"), homeLanesOld)
	if err := os.WriteFile(filepath.Join(swept, "facts.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.age(swept, homeLanesOld, homeLanesOld)
	for _, p := range []string{filepath.Join(swept, "facts.json"), filepath.Join(swept, "pm-runtime", "selected.json")} {
		if err := os.Chtimes(p, time.Now(), time.Now().Add(-homeLanesOld)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(filepath.Join(used, "environments", "gen", "venv", "bin", "python"), time.Now(), time.Now().Add(-homeLanesOld)); err != nil {
		t.Fatal(err)
	}
	if res := f.run(LevelCritical); res.ItemsCleaned != 1 || exists(swept) || !exists(used) {
		t.Fatalf("result = %+v swept=%v used=%v", res, exists(swept), exists(used))
	}
}

func TestHomeLanesKeepsHeldTreesAndAnIncompleteInventoryKeepsAll(t *testing.T) {
	f := newHomeLanesFixture(t)
	held := f.lane("lane-held", homeLanesOld, "open")
	free := f.lane("lane-free", homeLanesOld)
	orphan := f.install("aaaa", filepath.Join(f.checkouts, "gone"), homeLanesOld)
	f.held[filepath.Join(held, "open")] = "pid 7 holds it"
	f.held[filepath.Join(orphan, "environments", "gen", "venv", "bin", "python")] = "pid 8 maps it"

	if res := f.run(LevelCritical); res.ItemsCleaned != 1 || exists(free) || !exists(held) || !exists(orphan) {
		t.Fatalf("result = %+v", res)
	}

	f.plugin.activity = func(context.Context, bbActivityOptions) *bbActivity {
		a := newBBActivity()
		a.incomplete = append(a.incomplete, "pid 9 (x) fd unreadable")
		return a
	}
	f.held = map[string]string{}
	if res := f.run(LevelCritical); res.ItemsCleaned != 0 || !exists(held) || !exists(orphan) {
		t.Fatalf("incomplete inventory removed a tree: %+v", res)
	}
}

func TestHomeLanesRechecksHoldersBeforeRemoval(t *testing.T) {
	f := newHomeLanesFixture(t)
	lane := f.lane("lane-a", homeLanesOld)
	f.held[lane] = "pid 7 arrived after the plan"
	f.heldFrom = 2 // the plan's scan sees nothing, the recheck sees the holder
	if res := f.run(LevelCritical); res.ItemsCleaned != 0 || !exists(lane) {
		t.Fatalf("result = %+v", res)
	}
}

func TestHomeLanesKeepsGitAndSessionData(t *testing.T) {
	f := newHomeLanesFixture(t)
	git := f.lane("lane-git", homeLanesOld, "repo/.git/HEAD")
	session := f.lane("lane-session", homeLanesOld, "repo/.claude/projects/x.jsonl")
	if res := f.run(LevelCritical); res.ItemsCleaned != 0 || !exists(git) || !exists(session) {
		t.Fatalf("result = %+v", res)
	}
}

// A flock taken by any process, in any namespace, keeps the tree, even when
// the path-based inventory sees nothing. This test process takes a real one.
func TestHomeLanesKernelFileLockKeepsTheTree(t *testing.T) {
	f := newHomeLanesFixture(t)
	orphan := f.install("aaaa", filepath.Join(f.checkouts, "gone"), homeLanesOld)
	lease := filepath.Join(orphan, "pm-runtime", "selected.json")
	fd, err := os.OpenFile(lease, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	if err := syscall.Flock(int(fd.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	if res := f.run(LevelCritical); res.ItemsCleaned != 0 || !exists(orphan) {
		t.Fatalf("a locked install was removed: %+v", res)
	}
	if got := f.reason("aaaa"); !strings.Contains(got, "file lock") {
		t.Fatalf("reason = %q", got)
	}
	if err := syscall.Flock(int(fd.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	f.age(orphan, homeLanesOld, homeLanesOld)
	if res := f.run(LevelCritical); res.ItemsCleaned != 1 || exists(orphan) {
		t.Fatalf("result after unlock = %+v", res)
	}
}

func TestHomeLanesUnreadableLockTableKeepsAll(t *testing.T) {
	f := newHomeLanesFixture(t)
	lane := f.lane("lane-a", homeLanesOld)
	f.lockErr = errors.New("permission denied")
	if res := f.run(LevelCritical); res.ItemsCleaned != 0 || !exists(lane) {
		t.Fatalf("result = %+v", res)
	}
	if got := f.reason("lane-a"); !strings.Contains(got, "lock table unreadable") {
		t.Fatalf("reason = %q", got)
	}
}

func TestHomeLanesParsesProcLocks(t *testing.T) {
	const table = `1: FLOCK  ADVISORY  WRITE 1234 fd:03:12345 0 EOF
2: POSIX  ADVISORY  READ 77 00:2a:9 0 EOF
2: -> POSIX  ADVISORY  WRITE 78 fd:03:555 0 EOF
`
	locked, err := homeLanesParseLocks(bufio.NewScanner(strings.NewReader(table)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []homeLanesInode{{0xfd, 3, 12345}, {0, 0x2a, 9}, {0xfd, 3, 555}} {
		if !locked[want] {
			t.Fatalf("%+v missing from %+v", want, locked)
		}
	}
}

func TestHomeLanesRemovesReadOnlyTreesAndFinishesAnInterruptedRemoval(t *testing.T) {
	f := newHomeLanesFixture(t)
	lane := f.lane("lane-a", homeLanesOld, "ro/leaf")
	ro := filepath.Join(lane, "ro")
	f.age(lane, homeLanesOld, homeLanesOld)
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })

	f.plugin.removeAll = func(string) error { return errors.New("interrupted") }
	if res := f.run(LevelCritical); res.ItemsCleaned != 0 {
		t.Fatalf("result = %+v", res)
	}
	aside := filepath.Join(f.cache, homeLanesAsidePrefix+"lane-a")
	if exists(lane) || !exists(aside) {
		t.Fatalf("lane=%v aside=%v", exists(lane), exists(aside))
	}
	if _, ok := targetFor(f.plan(LevelCritical), homeLanesAsidePrefix+"lane-a"); ok {
		t.Fatal("an aside tree must not be a candidate")
	}

	f.plugin.removeAll = nil
	f.run(LevelCritical)
	if exists(aside) {
		t.Fatal("the next cycle did not finish the removal")
	}
}

func TestHomeLanesBoundsRemovalsLargestFirst(t *testing.T) {
	f := newHomeLanesFixture(t)
	f.cfg.HomeLanes.MaxRemovalsPerCycle = 1
	small := f.lane("lane-small", homeLanesOld)
	big := f.lane("lane-big", homeLanesOld, "a", "b", "c", "d")
	if res := f.run(LevelCritical); res.ItemsCleaned != 1 || exists(big) || !exists(small) {
		t.Fatalf("result = %+v", res)
	}
}

func TestHomeLanesValidate(t *testing.T) {
	base := func() config.HomeLanesConfig {
		c := config.DefaultConfig().HomeLanes
		c.HermesInstallsRoot = "~/.hermes/installs"
		c.HermesCheckoutRoots = []string{"/srv/fast-local/tmp-agents"}
		return c
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("default policy: %v", err)
	}
	for name, mutate := range map[string]func(*config.HomeLanesConfig){
		"mode":           func(c *config.HomeLanesConfig) { c.Mode = "maybe" },
		"match all glob": func(c *config.HomeLanesConfig) { c.CacheLaneGlobs = []string{"*"} },
		"bad glob":       func(c *config.HomeLanesConfig) { c.CacheLaneGlobs = []string{"[a"} },
		"cache floor":    func(c *config.HomeLanesConfig) { c.CacheIdleAfter = "71h" },
		"hermes floor":   func(c *config.HomeLanesConfig) { c.HermesIdleAfter = "23h" },
		"no checkouts":   func(c *config.HomeLanesConfig) { c.HermesCheckoutRoots = nil },
		"relative root":  func(c *config.HomeLanesConfig) { c.HermesCheckoutRoots = []string{"srv"} },
		"no removals":    func(c *config.HomeLanesConfig) { c.MaxRemovalsPerCycle = 0 },
	} {
		c := base()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

// The real /proc inventory, with a child process that holds the tree only
// through a memory mapping, then only through its cwd.
func TestHomeLanesRealInventorySeesAHolder(t *testing.T) {
	for _, mode := range []string{"mmap", "cwd"} {
		t.Run(mode, func(t *testing.T) {
			f := newHomeLanesFixture(t)
			dir := f.lane("lane-held", homeLanesOld, "seg")
			f.plugin.activity = nil
			// Non-dumpable helpers on a dev seat would skip this test.
			f.cfg.HomeLanes.UninspectableOKComms = append(f.cfg.HomeLanes.UninspectableOKComms, "sshd-session", "pasta.avx2")
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
			if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
				t.Fatalf("helper not ready: %q %v", line, err)
			}
			a := bbScanActivity(context.Background(), bbActivityOptions{
				Within: []string{f.cache}, OKComms: f.cfg.HomeLanes.UninspectableOKComms})
			if why := a.complete(); why != "" {
				t.Skipf("inventory incomplete in this environment: %s", why)
			}
			f.run(LevelCritical)
			if !exists(dir) {
				t.Fatalf("a tree held through %s was removed", mode)
			}
		})
	}
}
