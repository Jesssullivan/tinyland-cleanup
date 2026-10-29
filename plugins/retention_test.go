//go:build linux || darwin

package plugins

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// retFixture drives the retention plugin with a clock 30 days ahead, so
// trees made now (whose ctime cannot be set back) are 30 days old.
type retFixture struct {
	t          *testing.T
	root       string
	cargo      string
	plugin     *RetentionPlugin
	cfg        *config.Config
	held       map[string]string
	procs      []bbProc
	incomplete string
	removed    []string
}

func newRetFixture(t *testing.T) *retFixture {
	t.Helper()
	f := &retFixture{t: t, root: t.TempDir(), cargo: t.TempDir(), held: map[string]string{}}
	future := time.Now().Add(30 * 24 * time.Hour)
	f.plugin = &RetentionPlugin{
		now:  func() time.Time { return future },
		uid:  os.Geteuid(),
		home: t.TempDir(),
		user: "tester",
		goos: PlatformLinux,
		activity: func(_ context.Context, opts bbActivityOptions) *bbActivity {
			a := newBBActivity()
			for p, why := range f.held {
				a.index(p, why, opts.Within)
			}
			a.procs = append(a.procs, f.procs...)
			if f.incomplete != "" {
				a.incomplete = append(a.incomplete, f.incomplete)
			}
			return a
		},
		statfs: func(string) (bbUsage, error) {
			return bbUsage{BytesTotal: 100 << 30, BytesUsed: 50 << 30}, nil
		},
	}
	f.cfg = config.DefaultConfig()
	f.cfg.Enable.Retention = true
	f.cfg.Retention.TmpAgents.Roots = []string{f.root}
	f.cfg.Retention.CargoTargets.Roots = []string{f.cargo}
	f.cfg.Retention.FreeSpaceFloor.Paths = []string{f.root}
	f.cfg.MonitoredMounts = nil
	f.cfg.DevArtifacts.ProtectPaths = nil
	return f
}

func (f *retFixture) file(path string, size int) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", size)), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *retFixture) plan() CleanupPlan {
	return f.plugin.PlanCleanup(context.Background(), LevelNone, f.cfg, retDiscard())
}

func (f *retFixture) cleanup() CleanupResult {
	f.plugin.lastRun = time.Time{}
	return f.plugin.Cleanup(context.Background(), LevelNone, f.cfg, retDiscard())
}

func retDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func retTarget(t *testing.T, plan CleanupPlan, path string) CleanupTarget {
	t.Helper()
	for _, tg := range plan.Targets {
		if tg.Path == path {
			return tg
		}
	}
	t.Fatalf("no target for %s in %+v", path, plan.Targets)
	return CleanupTarget{}
}

func TestRetentionTmpAgentsDryRunDefault(t *testing.T) {
	f := newRetFixture(t)
	if f.cfg.Retention.TmpAgents.Delete || f.cfg.Retention.CargoTargets.Delete || f.cfg.Bazel.DiskCacheAgeDelete {
		t.Fatal("every deleting retention rule must default to report only")
	}
	dir := filepath.Join(f.root, "nix-shell.abc123")
	f.file(filepath.Join(dir, "pytest-of-jess", "pytest-1", "out.bin"), 64*1024)

	plan := f.plan()
	tg := retTarget(t, plan, dir)
	if tg.Action != "report" || tg.Protected {
		t.Fatalf("stale dir should be reported as eligible, got %+v", tg)
	}
	if tg.Tier != CleanupTierWarm || tg.Reclaim != CleanupReclaimNone {
		t.Fatalf("report target policy: tier %q reclaim %q", tg.Tier, tg.Reclaim)
	}
	if plan.WouldRun || plan.EstimatedBytesFreed != 0 {
		t.Fatalf("report-only plan must not claim a run: %+v", plan)
	}
	if plan.Metadata["tmp_agents.eligible_count"] != "1" {
		t.Fatalf("eligible_count = %q", plan.Metadata["tmp_agents.eligible_count"])
	}
	res := f.cleanup()
	if res.ItemsCleaned != 0 || !pathExists(dir) {
		t.Fatalf("default config removed %s (%+v)", dir, res)
	}
}

func TestRetentionTmpAgentsDeletesOnlyWithFlag(t *testing.T) {
	f := newRetFixture(t)
	f.cfg.Retention.TmpAgents.Delete = true
	stale := filepath.Join(f.root, "nix-shell.old")
	f.file(filepath.Join(stale, "pytest-of-jess", "x"), 8192)
	other := filepath.Join(f.root, "uv-cache-abc")
	f.file(filepath.Join(other, "x"), 8192)
	f.file(filepath.Join(f.root, "nix-shell.file"), 10)

	if tg := retTarget(t, f.plan(), stale); tg.Action != "delete" || tg.Reclaim != CleanupReclaimHost {
		t.Fatalf("delete target policy: %+v", tg)
	}
	res := f.cleanup()
	if pathExists(stale) || res.ItemsCleaned != 1 || res.BytesFreed <= 0 {
		t.Fatalf("stale nix-shell dir not removed: %+v", res)
	}
	if !pathExists(other) || !pathExists(filepath.Join(f.root, "nix-shell.file")) {
		t.Fatal("a name outside name_globs or a top-level file was removed")
	}
}

func TestRetentionTmpAgentsLiveReferenceKeeps(t *testing.T) {
	for _, tc := range []struct{ name, held, why string }{
		{"tmpdir", "nix-shell.live/pytest-of-jess", "pid 42 has TMPDIR=..."},
		{"cwd", "nix-shell.live", "pid 43 cwd is ..."},
		{"open-fd", "nix-shell.live/pytest-of-jess/x", "pid 44 holds ..."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetFixture(t)
			f.cfg.Retention.TmpAgents.Delete = true
			dir := filepath.Join(f.root, "nix-shell.live")
			f.file(filepath.Join(dir, "pytest-of-jess", "x"), 8192)
			f.held[filepath.Join(f.root, tc.held)] = tc.why
			tg := retTarget(t, f.plan(), dir)
			if !tg.Protected || !tg.Active || !strings.Contains(tg.Reason, "live reference") {
				t.Fatalf("live dir not kept: %+v", tg)
			}
			f.cleanup()
			if !pathExists(dir) {
				t.Fatal("live dir removed")
			}
		})
	}
}

func TestRetentionLiveReferenceRecordedBehindOtherKeepReasons(t *testing.T) {
	f := newRetFixture(t)
	f.plugin.now = time.Now // the tree is recent, so recency decides first
	dir := filepath.Join(f.root, "nix-shell.busy")
	f.file(filepath.Join(dir, "a"), 10)
	f.held[dir] = "pid 50 has TMPDIR=" + dir
	plan := f.plan()
	tg := retTarget(t, plan, dir)
	if !tg.Active || !strings.Contains(tg.Reason, "recent change") || !strings.Contains(tg.Reason, "also live reference") {
		t.Fatalf("live reference not recorded: %+v", tg)
	}
	if plan.Metadata["tmp_agents.live_count"] != "1" {
		t.Fatalf("live_count = %q", plan.Metadata["tmp_agents.live_count"])
	}
}

func TestRetentionIncompleteInventoryKeepsAll(t *testing.T) {
	f := newRetFixture(t)
	f.cfg.Retention.TmpAgents.Delete = true
	f.incomplete = "pid 9 (bash) fd unreadable"
	dir := filepath.Join(f.root, "nix-shell.x")
	f.file(filepath.Join(dir, "a"), 10)
	f.cleanup()
	if !pathExists(dir) {
		t.Fatal("an incomplete inventory must keep every tree")
	}
}

func TestRetentionRecentChangeKeeps(t *testing.T) {
	f := newRetFixture(t)
	f.cfg.Retention.TmpAgents.Delete = true
	f.plugin.now = time.Now
	dir := filepath.Join(f.root, "nix-shell.fresh")
	f.file(filepath.Join(dir, "a"), 10)
	tg := retTarget(t, f.plan(), dir)
	if !tg.Protected || !strings.Contains(tg.Reason, "recent change") {
		t.Fatalf("fresh dir not kept: %+v", tg)
	}
}

func TestRetentionSparseFileCountsAllocatedBlocks(t *testing.T) {
	f := newRetFixture(t)
	dir := filepath.Join(f.root, "nix-shell.sparse")
	if err := os.MkdirAll(filepath.Join(dir, "pytest-of-jess"), 0o755); err != nil {
		t.Fatal(err)
	}
	gguf := filepath.Join(dir, "pytest-of-jess", "model.gguf")
	fh, err := os.Create(gguf)
	if err != nil {
		t.Fatal(err)
	}
	const apparent = 4 << 30 // 4 GiB apparent, nothing written
	if err := fh.Truncate(apparent); err != nil {
		t.Fatal(err)
	}
	fh.Close()

	plan := f.plan()
	tg := retTarget(t, plan, dir)
	if tg.Bytes >= 1<<20 {
		t.Fatalf("sparse file charged %d allocated bytes", tg.Bytes)
	}
	if tg.LogicalBytes < apparent {
		t.Fatalf("logical bytes %d should show the %d apparent size", tg.LogicalBytes, apparent)
	}
	if b, _ := strconv.ParseInt(plan.Metadata["tmp_agents.eligible_bytes"], 10, 64); b >= 1<<20 {
		t.Fatalf("eligible_bytes %d counts apparent size", b)
	}
}

func gitOrSkip(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	return git
}

func retGit(t *testing.T, git, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(git, append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestRetentionDirtyGitTreeRefused(t *testing.T) {
	git := gitOrSkip(t)
	f := newRetFixture(t)
	f.cfg.Retention.TmpAgents.Delete = true
	dir := filepath.Join(f.root, "nix-shell.repo")
	repo := filepath.Join(dir, "checkout")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	retGit(t, git, repo, "init", "-q")
	f.file(filepath.Join(repo, "uncommitted.txt"), 10)

	tg := retTarget(t, f.plan(), dir)
	if !tg.Protected || !strings.Contains(tg.Reason, "Git") {
		t.Fatalf("dirty Git tree not refused: %+v", tg)
	}
	f.cleanup()
	if !pathExists(filepath.Join(repo, "uncommitted.txt")) {
		t.Fatal("dirty Git tree removed")
	}
}

func TestRetentionCargoTargetInDirtyWorkTreeRefused(t *testing.T) {
	git := gitOrSkip(t)
	f := newRetFixture(t)
	f.cfg.Retention.CargoTargets.Delete = true
	repo := f.cargo
	retGit(t, git, repo, "init", "-q")
	f.file(filepath.Join(repo, "src.rs"), 10) // untracked: the work tree is dirty
	target := filepath.Join(repo, "build", "ab", "0123456789abcd")
	f.file(filepath.Join(target, "CACHEDIR.TAG"), 10)
	f.file(filepath.Join(target, "debug", "deps", "libx.rlib"), 8192)
	f.cfg.Retention.CargoTargets.Roots = []string{filepath.Join(repo, "build")}

	tg := retTarget(t, f.plan(), target)
	if !tg.Protected || !strings.Contains(tg.Reason, "uncommitted") {
		t.Fatalf("target in a dirty work tree not refused: %+v", tg)
	}
	f.cleanup()
	if !pathExists(target) {
		t.Fatal("target in a dirty work tree removed")
	}
}

func (f *retFixture) cargoTarget(rel string) string {
	f.t.Helper()
	target := filepath.Join(f.cargo, rel)
	f.file(filepath.Join(target, "CACHEDIR.TAG"), 10)
	f.file(filepath.Join(target, ".rustc_info.json"), 10)
	f.file(filepath.Join(target, "debug", "deps", "libx.rlib"), 64*1024)
	return target
}

func TestRetentionCargoTargets(t *testing.T) {
	t.Run("report-only by default", func(t *testing.T) {
		f := newRetFixture(t)
		target := f.cargoTarget("06/01764cdca14fa1")
		tg := retTarget(t, f.plan(), target)
		if tg.Action != "report" || tg.Type != "cargo-target" {
			t.Fatalf("stale target should be reported: %+v", tg)
		}
		f.cleanup()
		if !pathExists(target) {
			t.Fatal("report-only rule removed a target")
		}
	})
	t.Run("delete with flag", func(t *testing.T) {
		f := newRetFixture(t)
		f.cfg.Retention.CargoTargets.Delete = true
		target := f.cargoTarget("06/01764cdca14fa1")
		plain := filepath.Join(f.cargo, "notatarget")
		f.file(filepath.Join(plain, "x"), 10)
		f.cleanup()
		if pathExists(target) {
			t.Fatal("stale target not removed")
		}
		if !pathExists(plain) || !pathExists(filepath.Join(f.cargo, "06")) {
			t.Fatal("something other than the target directory was removed")
		}
	})
	t.Run("busy cargo keeps every target", func(t *testing.T) {
		f := newRetFixture(t)
		f.cfg.Retention.CargoTargets.Delete = true
		f.procs = []bbProc{{pid: 77, cmdline: "/nix/store/abc-rust/bin/cargo test --release"}}
		target := f.cargoTarget("0c/aaaa")
		tg := retTarget(t, f.plan(), target)
		if !tg.Active || !strings.Contains(tg.Reason, "pid 77 (cargo)") {
			t.Fatalf("busy cargo did not keep the target: %+v", tg)
		}
		f.cleanup()
		if !pathExists(target) {
			t.Fatal("target removed while cargo runs")
		}
	})
	t.Run("live reference keeps the target", func(t *testing.T) {
		f := newRetFixture(t)
		f.cfg.Retention.CargoTargets.Delete = true
		target := f.cargoTarget("0e/bbbb")
		f.held[filepath.Join(target, "debug")] = "pid 5 cwd is it"
		f.cleanup()
		if !pathExists(target) {
			t.Fatal("referenced target removed")
		}
	})
}

func TestRetentionOtherUsersTreeKept(t *testing.T) {
	f := newRetFixture(t)
	f.cfg.Retention.TmpAgents.Delete = true
	f.plugin.uid = os.Geteuid() + 1 // every tree now belongs to "another user"
	dir := filepath.Join(f.root, "nix-shell.theirs")
	f.file(filepath.Join(dir, "a"), 10)
	tg := retTarget(t, f.plan(), dir)
	if !tg.Protected || !strings.Contains(tg.Reason, "another") {
		t.Fatalf("another user's tree not kept: %+v", tg)
	}
}

func TestRetentionProtectPathsRespected(t *testing.T) {
	f := newRetFixture(t)
	f.cfg.Retention.TmpAgents.Delete = true
	dir := filepath.Join(f.root, "nix-shell.pinned")
	f.file(filepath.Join(dir, "a"), 10)
	f.cfg.DevArtifacts.ProtectPaths = []string{dir}
	f.cleanup()
	if !pathExists(dir) {
		t.Fatal("protect_paths entry removed")
	}
}

func TestRetentionFreeSpaceFloorAlert(t *testing.T) {
	f := newRetFixture(t)
	f.plugin.statfs = func(string) (bbUsage, error) {
		return bbUsage{BytesTotal: 100 << 30, BytesUsed: 95 << 30}, nil // 5 GiB free
	}
	plan := f.plan()
	found := false
	for _, w := range plan.Warnings {
		if strings.HasPrefix(w, "ALERT free-space floor: "+f.root) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no floor alert in %v", plan.Warnings)
	}
	if !strings.Contains(plan.Metadata["free_space_floor."+f.root], "below_floor=true") {
		t.Fatalf("floor metadata: %v", plan.Metadata)
	}
	f.plugin.statfs = func(string) (bbUsage, error) { return bbUsage{BytesTotal: 100 << 30, BytesUsed: 50 << 30}, nil }
	for _, w := range f.plan().Warnings {
		if strings.HasPrefix(w, "ALERT") {
			t.Fatalf("alert above the floor: %s", w)
		}
	}
}

func TestRetentionDarwinLeftoversReportOnly(t *testing.T) {
	f := newRetFixture(t)
	f.plugin.goos = PlatformDarwin
	web := t.TempDir()
	current := filepath.Join(web, "c139e2731e42a0340acc3d34de2cbdb01237a27c")
	old := filepath.Join(web, "127ae56f1790f40cf393bcc73875a1a281e20234")
	f.file(filepath.Join(current, "Autodesk Fusion.app", "Contents", "Info.plist"), 8192)
	f.file(filepath.Join(old, "payload.bin"), 8192)
	if err := os.Symlink(filepath.Join(current, "Autodesk Fusion.app"), filepath.Join(web, fusionAppLink)); err != nil {
		t.Fatal(err)
	}
	f.cfg.Retention.DarwinLeftovers = config.RetentionDarwinLeftoversConfig{
		Enabled: true, FusionWebdeployRoot: web, ReportPaths: []string{filepath.Join(web, "missing-vm_bundles")},
	}
	plan := f.plan()
	if tg := retTarget(t, plan, old); tg.Name != "fusion-webdeploy-superseded" || !tg.Protected || tg.Bytes <= 0 {
		t.Fatalf("superseded build: %+v", tg)
	}
	if tg := retTarget(t, plan, current); tg.Name != "fusion-webdeploy-current" || !tg.Protected {
		t.Fatalf("current build: %+v", tg)
	}
	if plan.Metadata["darwin_leftovers.absent."+filepath.Join(web, "missing-vm_bundles")] != "true" {
		t.Fatalf("absent report path not recorded: %v", plan.Metadata)
	}
	f.cfg.Retention.TmpAgents.Delete = true
	f.cleanup()
	if !pathExists(old) || !pathExists(current) {
		t.Fatal("darwin leftovers must never be deleted")
	}
}

func TestRetentionIntervalThrottlesDaemonPasses(t *testing.T) {
	f := newRetFixture(t)
	f.cfg.Retention.TmpAgents.Delete = true
	f.cfg.Retention.Interval = "1h"
	if !f.plugin.due(f.cfg) {
		t.Fatal("first pass should be due")
	}
	if f.plugin.due(f.cfg) {
		t.Fatal("second pass within the interval should not be due")
	}
}

func TestRetentionRecheckRefusesChangedIdentity(t *testing.T) {
	f := newRetFixture(t)
	f.cfg.Retention.TmpAgents.Delete = true
	dir := filepath.Join(f.root, "nix-shell.swap")
	f.file(filepath.Join(dir, "a"), 10)
	pass := f.plugin.evaluate(context.Background(), f.cfg)
	if len(pass.candidates) != 1 || pass.candidates[0].target.Action != "delete" {
		t.Fatalf("expected one delete candidate, got %+v", pass.candidates)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	f.file(filepath.Join(dir, "b"), 10) // a new tree under the same name
	why := f.plugin.recheck(context.Background(), f.cfg, pass.candidates[0], newBBActivity())
	if why == "" {
		t.Fatal("recheck accepted a replaced tree")
	}
}

func TestRetentionConfigValidate(t *testing.T) {
	rc := config.DefaultRetentionConfig("/home/x")
	rc.TmpAgents.Roots = []string{"/srv/fast-local/tmp-agents"}
	if err := rc.Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	bad := rc
	bad.TmpAgents.MaxAge = "10m"
	if bad.Validate() == nil {
		t.Fatal("max_age under the 1h floor accepted")
	}
	bad = rc
	bad.TmpAgents.NameGlobs = []string{"*"}
	if bad.Validate() == nil {
		t.Fatal("bare * glob accepted")
	}
	bad = rc
	bad.CargoTargets.Roots = []string{"/"}
	if bad.Validate() == nil {
		t.Fatal("/ accepted as a root")
	}
	if got := config.ExpandUserPath("/srv/cache/{user}/cargo/build", "/h", "jess"); got != "/srv/cache/jess/cargo/build" {
		t.Fatalf("ExpandUserPath = %s", got)
	}
}

func TestBbIndexEnvironHoldsTmpdir(t *testing.T) {
	a := newBBActivity()
	opts := bbActivityOptions{EnvVars: retentionEnvVars, Within: []string{"/srv/fast-local/tmp-agents"}}
	bbIndexEnviron(a, 12, []string{"HOME=/home/x", "TMPDIR=/srv/fast-local/tmp-agents/nix-shell.q1", "PATH=/bin"}, opts)
	if got := a.blockers("/srv/fast-local/tmp-agents/nix-shell.q1"); len(got) == 0 || !strings.Contains(got[0], "TMPDIR") {
		t.Fatalf("TMPDIR not indexed: %v", got)
	}
	if got := a.blockers("/srv/fast-local/tmp-agents/nix-shell.other"); len(got) != 0 {
		t.Fatalf("unrelated dir held: %v", got)
	}
}
