package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// Fixtures for the bazel_bases reaper (R-C644/R-C645). They port the lab
// //tests:sting_bazel_seat_reap cases: idle windows, pressure ordering and
// stop, held copies, bases-only trees, git guards, the TIN-5268 deny set and
// its single carve-out. Liveness fixtures that need real processes live in
// bazel_bases_linux_test.go.

const mib = 1024 * 1024

// bbFixture is one temporary seat: a root, a home and a plugin wired to a
// fake statfs and an empty, complete activity inventory unless a test
// replaces them.
type bbFixture struct {
	t      *testing.T
	dir    string
	home   string
	root   string
	cfg    *config.Config
	plugin *BazelBasesPlugin
	mu     sync.Mutex
	usage  func(path string) (bbUsage, error)
}

func newBBFixture(t *testing.T) *bbFixture {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &bbFixture{t: t, dir: dir, home: filepath.Join(dir, "home"), root: filepath.Join(dir, "seat")}
	bbMkdir(t, f.home)
	bbMkdir(t, f.root)
	cfg := config.DefaultConfig()
	cfg.Enable.BazelBases = true
	cfg.BazelBases.Mode = config.BazelBasesModeApply
	cfg.BazelBases.Roots = []string{f.root}
	cfg.BazelBases.ReceiptDir = filepath.Join(dir, "receipts")
	cfg.BazelBases.Lsof = ""
	cfg.BazelBases.Git = ""
	f.cfg = cfg
	env := defaultBBEnv()
	env.home = f.home
	env.tmpdir = filepath.Join(dir, "tmp")
	env.activity = func(context.Context, bbActivityOptions) *bbActivity { return newBBActivity() }
	env.usage = func(path string) (bbUsage, error) {
		f.mu.Lock()
		u := f.usage
		f.mu.Unlock()
		if u == nil {
			return bbUsage{BytesTotal: 100 * mib, BytesUsed: 30 * mib, InodesTotal: 1000, InodesUsed: 100, InodesCounted: true}, nil
		}
		return u(path)
	}
	f.plugin = &BazelBasesPlugin{env: env, pressured: map[uint64]bool{}}
	return f
}

func (f *bbFixture) run() *bbCycle {
	f.t.Helper()
	pol, err := f.plugin.policy(f.cfg)
	if err != nil {
		f.t.Fatalf("policy: %v", err)
	}
	return f.plugin.run(context.Background(), pol, f.cfg.BazelBases.Mode == config.BazelBasesModeApply, bbDiscardLogger())
}

func bbDiscardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func bbMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func bbWrite(t *testing.T, path string, size int) {
	t.Helper()
	bbMkdir(t, filepath.Dir(path))
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7 + 3)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// bbMakeBase creates a structural output base with size bytes of action
// output, last used ago.
func bbMakeBase(t *testing.T, base string, size int, ago time.Duration) {
	t.Helper()
	bbMkdir(t, filepath.Join(base, "execroot", "_main", "bazel-out"))
	bbMkdir(t, filepath.Join(base, "external"))
	bbMkdir(t, filepath.Join(base, "server"))
	bbWrite(t, filepath.Join(base, "lock"), 0)
	bbWrite(t, filepath.Join(base, "DO_NOT_BUILD_HERE"), 12)
	bbWrite(t, filepath.Join(base, "command.log"), 64)
	bbWrite(t, filepath.Join(base, "execroot", "_main", "bazel-out", "out.bin"), size)
	bbAge(t, base, ago)
}

// bbAge sets every use signal of a base to now-ago.
func bbAge(t *testing.T, base string, ago time.Duration) {
	t.Helper()
	when := time.Now().Add(-ago)
	paths := []string{base, filepath.Join(base, "lock"), filepath.Join(base, "command.log"),
		filepath.Join(base, "server"), filepath.Join(base, "execroot"), filepath.Join(base, "DO_NOT_BUILD_HERE")}
	entries, _ := os.ReadDir(filepath.Join(base, "server"))
	for _, e := range entries {
		paths = append(paths, filepath.Join(base, "server", e.Name()))
	}
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			if err := os.Chtimes(p, when, when); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func bbRowFor(t *testing.T, c *bbCycle, path string) *bbRow {
	t.Helper()
	for _, r := range c.Rows {
		if r.Path == path {
			return r
		}
	}
	t.Fatalf("no row for %s; rows: %v", path, bbRowPaths(c))
	return nil
}

func bbRowPaths(c *bbCycle) []string {
	var out []string
	for _, r := range c.Rows {
		out = append(out, r.Decision+":"+r.Path+" ("+r.Reason+")")
	}
	return out
}

func bbExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// bbManifest is a sha256 manifest of every entry under root (path, mode,
// size, content), used to prove a tree was not touched.
func bbManifest(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	var lines []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			lines = append(lines, "ERR "+path)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		line := path + " " + info.Mode().String()
		if info.Mode().IsRegular() {
			data, _ := os.ReadFile(path)
			sum := sha256.Sum256(data)
			line += " " + hex.EncodeToString(sum[:])
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(path)
			line += " -> " + target
		}
		lines = append(lines, line)
		return nil
	})
	sort.Strings(lines)
	for _, l := range lines {
		h.Write([]byte(l + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestBazelBasesReapsIdleBaseAfterTwelveHours(t *testing.T) {
	f := newBBFixture(t)
	old := filepath.Join(f.root, "runs", "a", "out")
	recent := filepath.Join(f.root, "runs", "b", "out")
	bbMakeBase(t, old, 64*1024, 13*time.Hour)
	bbMakeBase(t, recent, 64*1024, 11*time.Hour)

	c := f.run()
	if bbExists(old) {
		t.Fatalf("base idle 13h was not reaped: %v", bbRowPaths(c))
	}
	if r := bbRowFor(t, c, old); r.Decision != "removed" {
		t.Fatalf("idle base decision %q (%s)", r.Decision, r.Reason)
	}
	if !bbExists(recent) {
		t.Fatal("base idle 11h was reaped without pressure")
	}
	if r := bbRowFor(t, c, recent); r.Decision != "keep" || !strings.Contains(r.Reason, "used within 12h") {
		t.Fatalf("recent base: %q (%s)", r.Decision, r.Reason)
	}
	if !bbExists(filepath.Join(f.root, "runs", "a")) {
		t.Fatal("the run directory beside the base was removed")
	}
	if _, err := os.Stat(filepath.Join(f.cfg.BazelBases.ReceiptDir, "removals.jsonl")); err != nil {
		t.Fatalf("no removal receipt: %v", err)
	}
}

func TestBazelBasesReportModeRemovesNothing(t *testing.T) {
	f := newBBFixture(t)
	f.cfg.BazelBases.Mode = config.BazelBasesModeReport
	old := filepath.Join(f.root, "a", "out")
	bbMakeBase(t, old, 4096, 48*time.Hour)
	result := f.plugin.Cleanup(context.Background(), LevelNone, f.cfg, bbDiscardLogger())
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	if !bbExists(old) {
		t.Fatal("report mode removed a base")
	}
	data, err := os.ReadFile(filepath.Join(f.cfg.BazelBases.ReceiptDir, "candidates.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "eligible\t"+old) {
		t.Fatalf("candidates.tsv does not list the eligible base:\n%s", data)
	}
}

// pressureUsage models one file system: used = baseline + the nominal size of
// every listed base that still exists.
func pressureUsage(total, baseline int64, sizes map[string]int64) func(string) (bbUsage, error) {
	return func(string) (bbUsage, error) {
		used := baseline
		for p, s := range sizes {
			if bbExists(p) {
				used += s
			}
		}
		return bbUsage{BytesTotal: uint64(total), BytesUsed: uint64(used), InodesTotal: 1 << 20, InodesUsed: 1000, InodesCounted: true}, nil
	}
}

func TestBazelBasesPressureReapsAtTwoHoursLargestFirstUntilBelowSixty(t *testing.T) {
	f := newBBFixture(t)
	a := filepath.Join(f.root, "a", "out")
	b := filepath.Join(f.root, "b", "out")
	c := filepath.Join(f.root, "c", "out")
	old := filepath.Join(f.root, "d", "out")
	fresh := filepath.Join(f.root, "e", "out")
	bbMakeBase(t, a, 3*mib, 3*time.Hour)
	bbMakeBase(t, b, 2*mib, 3*time.Hour)
	bbMakeBase(t, c, 1*mib, 3*time.Hour)
	bbMakeBase(t, old, 64*1024, 13*time.Hour)
	bbMakeBase(t, fresh, 4*mib, 1*time.Hour)
	// 100 units: baseline 43 + a 15 + b 12 + c 5 + d 0 = 75% at the start.
	// After a: 60% (not below 60, continue). After b: 48% (stop). c is only
	// pressure-eligible (3h < 12h) and stays; d is idle beyond 12h and goes.
	f.usage = pressureUsage(100*mib, 43*mib, map[string]int64{a: 15 * mib, b: 12 * mib, c: 5 * mib})

	cycle := f.run()
	if bbExists(a) || bbExists(b) {
		t.Fatalf("pressure did not reap the two largest idle bases: %v", bbRowPaths(cycle))
	}
	if !bbExists(c) {
		t.Fatalf("pressure kept reaping after the mount fell below 60%%: %v", bbRowPaths(cycle))
	}
	if r := bbRowFor(t, cycle, c); !strings.Contains(r.Reason, "below 60%") {
		t.Fatalf("c kept for the wrong reason: %s", r.Reason)
	}
	if bbExists(old) {
		t.Fatalf("a base idle beyond 12h must go after pressure is relieved: %v", bbRowPaths(cycle))
	}
	if !bbExists(fresh) {
		t.Fatal("a base used 1h ago was reaped under pressure (2h floor)")
	}
	var removed []string
	for _, r := range cycle.Rows {
		if r.Decision == "removed" {
			removed = append(removed, r.Path)
		}
	}
	if len(removed) < 2 || removed[0] != a || removed[1] != b {
		t.Fatalf("pressure order is not largest first: %v", removed)
	}
	if !cycle.Mounts[f.root].Pressure {
		t.Fatal("cycle did not record pressure on the mount")
	}
}

func TestBazelBasesInodePressureOrdersByInodes(t *testing.T) {
	f := newBBFixture(t)
	big := filepath.Join(f.root, "big", "out")
	many := filepath.Join(f.root, "many", "out")
	bbMakeBase(t, big, 2*mib, 3*time.Hour)
	bbMakeBase(t, many, 4096, 3*time.Hour)
	for i := 0; i < 300; i++ {
		bbWrite(t, filepath.Join(many, "execroot", "_main", "f", "f"+strconv.Itoa(i)), 1)
	}
	bbAge(t, many, 3*time.Hour)
	f.usage = func(string) (bbUsage, error) {
		inodesUsed := uint64(100)
		if bbExists(big) {
			inodesUsed += 50
		}
		if bbExists(many) {
			inodesUsed += 700
		}
		return bbUsage{BytesTotal: 100 * mib, BytesUsed: 20 * mib, InodesTotal: 1000, InodesUsed: inodesUsed, InodesCounted: true}, nil
	}
	cycle := f.run()
	if bbExists(many) {
		t.Fatalf("inode pressure did not reap the inode-heavy base: %v", bbRowPaths(cycle))
	}
	if !bbExists(big) {
		t.Fatalf("inode pressure reaped past 60%%: %v", bbRowPaths(cycle))
	}
}

func TestBazelBasesKeepsHeldCopiesAndNamedCopyTrees(t *testing.T) {
	f := newBBFixture(t)
	held := filepath.Join(f.root, "neo-transfer-20260930")
	f.cfg.BazelBases.HeldPaths = []string{filepath.Join(f.root, "held-copy")}
	inHeld := filepath.Join(f.root, "held-copy", "out")
	inNamed := filepath.Join(held, "out")
	bbMakeBase(t, inHeld, 4096, 72*time.Hour)
	bbMakeBase(t, inNamed, 4096, 72*time.Hour)
	f.run()
	if !bbExists(inHeld) || !bbExists(inNamed) {
		t.Fatal("a held or name-guarded copy was reaped")
	}
}

func TestBazelBasesBasesOnlyRootTakesOnlyTheBase(t *testing.T) {
	f := newBBFixture(t)
	copyTree := filepath.Join(f.root, "home-relocated")
	f.cfg.BazelBases.BasesOnlyRoots = []string{copyTree}
	base := filepath.Join(copyTree, "jess", ".cache", "bazel-archive", "abc")
	recent := filepath.Join(copyTree, "jess", "run2", "out")
	bbMakeBase(t, base, 4096, 72*time.Hour)
	bbMakeBase(t, recent, 4096, 20*time.Hour)
	sib := filepath.Join(filepath.Dir(base), "bazel-user-root")
	bbMkdir(t, filepath.Join(sib, "install"))
	receipt := filepath.Join(filepath.Dir(base), "receipt.json")
	bbWrite(t, receipt, 10)
	cycle := f.run()
	if bbExists(base) {
		t.Fatalf("bases-only root did not reap its idle base: %v", bbRowPaths(cycle))
	}
	if !bbExists(sib) || !bbExists(receipt) {
		t.Fatal("something beside a bases-only base was removed")
	}
	if !bbExists(recent) {
		t.Fatal("a bases-only base idle 20h (< 48h) was reaped")
	}
}

func TestBazelBasesSiblingUserRootGoesWithItsBase(t *testing.T) {
	f := newBBFixture(t)
	run := filepath.Join(f.root, "omux-run")
	base := filepath.Join(run, "out")
	bbMakeBase(t, base, 4096, 30*time.Hour)
	sib := filepath.Join(run, "bazel-user-root")
	inst := filepath.Join(sib, "install", "deadbeef")
	bbMkdir(t, inst)
	if err := os.Symlink(inst, filepath.Join(base, "install")); err != nil {
		t.Fatal(err)
	}
	bbAge(t, base, 30*time.Hour)
	log := filepath.Join(run, "run.log")
	bbWrite(t, log, 10)
	cycle := f.run()
	if bbExists(base) || bbExists(sib) {
		t.Fatalf("base and its own sibling user root were not both reaped: %v", bbRowPaths(cycle))
	}
	if !bbExists(log) {
		t.Fatal("a run log beside the base was removed")
	}
}

func TestBazelBasesGitGuards(t *testing.T) {
	f := newBBFixture(t)
	repoBase := filepath.Join(f.root, "repo-shaped")
	bbMakeBase(t, repoBase, 4096, 72*time.Hour)
	bbMkdir(t, filepath.Join(repoBase, ".git"))
	bbAge(t, repoBase, 72*time.Hour)
	under := filepath.Join(f.root, "x", ".git", "modules", "out")
	bbMakeBase(t, under, 4096, 72*time.Hour)
	cycle := f.run()
	if !bbExists(repoBase) {
		t.Fatal("a base holding .git was reaped")
	}
	if r := bbRowFor(t, cycle, repoBase); !strings.Contains(r.Reason, ".git") {
		t.Fatalf("reason: %s", r.Reason)
	}
	if !bbExists(under) {
		t.Fatal("a base under .git was walked and reaped")
	}
}

func TestBazelBasesOnlyListedTakesEligibleRowsOnly(t *testing.T) {
	f := newBBFixture(t)
	a := filepath.Join(f.root, "a", "out")
	b := filepath.Join(f.root, "b", "out")
	bbMakeBase(t, a, 4096, 30*time.Hour)
	bbMakeBase(t, b, 4096, 30*time.Hour)
	list := filepath.Join(f.dir, "reviewed.tsv")
	if err := os.WriteFile(list, []byte("decision\tpath\nkeep\t"+a+"\neligible\t"+b+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.cfg.BazelBases.OnlyListed = list
	f.run()
	if !bbExists(a) {
		t.Fatal("a row the reviewed list kept was reaped")
	}
	if bbExists(b) {
		t.Fatal("the reviewed eligible row was not reaped")
	}
}

func TestBazelBasesIncompleteInventoryKeepsEverything(t *testing.T) {
	f := newBBFixture(t)
	a := filepath.Join(f.root, "a", "out")
	bbMakeBase(t, a, 4096, 30*time.Hour)
	f.plugin.env.activity = func(context.Context, bbActivityOptions) *bbActivity {
		act := newBBActivity()
		act.incomplete = []string{"pid 42 (podman) fd unreadable"}
		return act
	}
	cycle := f.run()
	if !bbExists(a) {
		t.Fatal("an incomplete activity inventory still reaped a base")
	}
	if cycle.Refusal == "" {
		t.Fatal("no refusal recorded")
	}
}

func TestBazelBasesFutureMtimeAgesFromCtime(t *testing.T) {
	f := newBBFixture(t)
	a := filepath.Join(f.root, "a", "out")
	bbMakeBase(t, a, 4096, 0)
	future := time.Now().Add(10 * 365 * 24 * time.Hour)
	for _, p := range []string{a, filepath.Join(a, "lock"), filepath.Join(a, "command.log")} {
		if err := os.Chtimes(p, future, future); err != nil {
			t.Fatal(err)
		}
	}
	// ctime is now, so the base is recent: it must not be reaped, and it must
	// not be read as fresh forever either (its last use is not in 2036).
	cycle := f.run()
	r := bbRowFor(t, cycle, a)
	if r.Decision != "keep" || r.lastUse.After(time.Now().Add(time.Hour)) {
		t.Fatalf("future mtime: %q last use %s", r.Decision, r.lastUse)
	}
}

// TIN-5268: agent-session data is never touched, through a symlinked store,
// through a per-repository session directory, or by removing an ancestor.
// The only carve-out is a structural base under an explicitly configured
// carve-out, and then only the base itself.
func TestBazelBasesNeverTouchesAgentSessionData(t *testing.T) {
	f := newBBFixture(t)
	state := filepath.Join(f.dir, "state")
	codexPhysical := filepath.Join(state, "codex")
	bbMkdir(t, codexPhysical)
	if err := os.Symlink(codexPhysical, filepath.Join(f.home, ".codex")); err != nil {
		t.Fatal(err)
	}
	f.cfg.BazelBases.Roots = []string{f.root, f.home, state}
	f.cfg.AgentSession.BazelBaseCarveouts = []string{filepath.Join(state, "codex", "omux-*")}

	claudeBase := filepath.Join(f.home, ".claude", "projects", "p", "out")
	bbMakeBase(t, claudeBase, 4096, 96*time.Hour)
	bbWrite(t, filepath.Join(f.home, ".claude", "projects", "p", "s.jsonl"), 100)
	sessionsBase := filepath.Join(codexPhysical, "sessions", "2026", "out")
	bbMakeBase(t, sessionsBase, 4096, 96*time.Hour)
	bbWrite(t, filepath.Join(codexPhysical, "sessions", "2026", "rollout.jsonl"), 100)
	tmuxBase := filepath.Join(f.home, ".local", "state", "tmux", "out")
	bbMakeBase(t, tmuxBase, 4096, 96*time.Hour)
	repoClaude := filepath.Join(f.root, "repo", ".claude", "worktrees", "w", "out")
	bbMakeBase(t, repoClaude, 4096, 96*time.Hour)
	// A base that contains a per-repository session directory is kept whole.
	holder := filepath.Join(f.root, "holder", "out")
	bbMakeBase(t, holder, 4096, 96*time.Hour)
	bbWrite(t, filepath.Join(holder, "execroot", "_main", ".claude", "settings.json"), 10)
	bbAge(t, holder, 96*time.Hour)
	// The carve-out: an omux run's base under the Codex store goes; the
	// receipt beside it and the transcripts elsewhere in the store stay.
	omuxRun := filepath.Join(codexPhysical, "omux-123")
	omuxBase := filepath.Join(omuxRun, "out")
	bbMakeBase(t, omuxBase, 4096, 96*time.Hour)
	omuxReceipt := filepath.Join(omuxRun, "receipt.json")
	bbWrite(t, omuxReceipt, 10)
	omuxSibling := filepath.Join(omuxRun, "bazel-user-root")
	bbMkdir(t, filepath.Join(omuxSibling, "install"))

	before := map[string]string{
		"claude":   bbManifest(t, filepath.Join(f.home, ".claude")),
		"sessions": bbManifest(t, filepath.Join(codexPhysical, "sessions")),
		"tmux":     bbManifest(t, filepath.Join(f.home, ".local", "state", "tmux")),
		"repo":     bbManifest(t, filepath.Join(f.root, "repo", ".claude")),
		"holder":   bbManifest(t, holder),
	}
	f.usage = func(string) (bbUsage, error) {
		return bbUsage{BytesTotal: 100, BytesUsed: 99, InodesTotal: 100, InodesUsed: 99, InodesCounted: true}, nil
	}
	cycle := f.run()
	after := map[string]string{
		"claude":   bbManifest(t, filepath.Join(f.home, ".claude")),
		"sessions": bbManifest(t, filepath.Join(codexPhysical, "sessions")),
		"tmux":     bbManifest(t, filepath.Join(f.home, ".local", "state", "tmux")),
		"repo":     bbManifest(t, filepath.Join(f.root, "repo", ".claude")),
		"holder":   bbManifest(t, holder),
	}
	for k := range before {
		if before[k] != after[k] {
			t.Errorf("agent-session tree %s changed under 99%% pressure: %v", k, bbRowPaths(cycle))
		}
	}
	if bbExists(omuxBase) {
		t.Errorf("the carved-out omux base was not reaped: %v", bbRowPaths(cycle))
	}
	if !bbExists(omuxReceipt) || !bbExists(omuxSibling) {
		t.Error("something beside the carved-out base was removed")
	}
	if !bbExists(filepath.Join(f.home, ".codex")) {
		t.Error("the ~/.codex link was removed")
	}
}

func TestBazelBasesRefusesRootInsideAgentSessionStore(t *testing.T) {
	f := newBBFixture(t)
	bbMkdir(t, filepath.Join(f.home, ".claude"))
	f.cfg.BazelBases.Roots = []string{filepath.Join(f.home, ".claude")}
	if _, err := f.plugin.policy(f.cfg); err == nil || !strings.Contains(err.Error(), "TIN-5268") {
		t.Fatalf("a root inside ~/.claude was accepted: %v", err)
	}
}

func TestAgentSessionDenyResolvesSymlinksAndRefusesAncestors(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	home := filepath.Join(dir, "home")
	physical := filepath.Join(dir, "fast", "state", "claude")
	bbMkdir(t, physical)
	bbMkdir(t, home)
	if err := os.Symlink(physical, filepath.Join(home, ".claude")); err != nil {
		t.Fatal(err)
	}
	d := newAgentSessionDeny(home, 1000, "", []string{filepath.Join(dir, "extra-store")}, nil)
	for _, p := range []string{
		filepath.Join(home, ".claude", "x"),
		filepath.Join(physical, "projects", "y"),
		filepath.Join(dir, "extra-store", "z"),
		filepath.Join(dir, "any", "repo", ".crush", "db"),
		"/tmp/tmux-1000/default",
	} {
		if _, denied := d.denied(p); !denied {
			t.Errorf("%s is not denied", p)
		}
	}
	if _, denied := d.containsDenied(filepath.Join(dir, "fast", "state")); !denied {
		t.Error("removing the ancestor of a resolved store is not refused")
	}
	if _, denied := d.denied(filepath.Join(dir, "fast", "cache", "bazel")); denied {
		t.Error("an unrelated path is denied")
	}
}

func TestDeleteOldFilesSkipsAgentSessionStores(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("TMPDIR", dir)
	store := filepath.Join(dir, "claude-"+itoaBB(os.Getuid()))
	plain := filepath.Join(dir, "plain")
	bbWrite(t, filepath.Join(store, "session.jsonl"), 100)
	bbWrite(t, filepath.Join(plain, "old.bin"), 100)
	old := time.Now().Add(-72 * time.Hour)
	for _, p := range []string{filepath.Join(store, "session.jsonl"), filepath.Join(plain, "old.bin")} {
		_ = os.Chtimes(p, old, old)
	}
	deleteOldFilesSameDevice(dir, time.Hour)
	if !bbExists(filepath.Join(store, "session.jsonl")) {
		t.Fatal("a temp sweep aged out a harness session store (TIN-5268)")
	}
	if bbExists(filepath.Join(plain, "old.bin")) {
		t.Fatal("the temp sweep no longer removes ordinary stale files")
	}
}

func itoaBB(n int) string { return strconv.Itoa(n) }

func TestBazelHandOffKeepsOutputBasesOwnedByBazelBases(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Enable.BazelBases = true
	cfg.BazelBases.Mode = config.BazelBasesModeApply
	targets := bazelHandOffTargets([]CleanupTarget{
		{Type: "output_base", Path: "/srv/x/out", Action: "delete_output_base"},
		{Type: "disk_cache", Path: "/srv/x/disk", Action: "delete_cache_tier"},
	}, cfg)
	if targets[0].Action != "keep" || !targets[0].Protected {
		t.Fatalf("output base not handed off: %+v", targets[0])
	}
	if targets[1].Action != "delete_cache_tier" {
		t.Fatalf("cache tier changed: %+v", targets[1])
	}
	cfg.BazelBases.Mode = config.BazelBasesModeReport
	targets = bazelHandOffTargets([]CleanupTarget{{Type: "output_base", Path: "/srv/x/out", Action: "delete_output_base"}}, cfg)
	if targets[0].Action != "delete_output_base" {
		t.Fatal("report mode must leave the bazel plugin as the owner")
	}
}

func TestBazelBasesConfigFloors(t *testing.T) {
	base := config.DefaultConfig().BazelBases
	base.Roots = []string{"/srv/x"}
	cases := map[string]func(c *config.BazelBasesConfig){
		"idle below 12h":        func(c *config.BazelBasesConfig) { c.IdleAfter = "6h" },
		"pressure below 2h":     func(c *config.BazelBasesConfig) { c.PressureIdleAfter = "1h" },
		"stop not below start":  func(c *config.BazelBasesConfig) { c.PressureStopPercent = 70 },
		"no .git skip":          func(c *config.BazelBasesConfig) { c.SkipDirNames = []string{"node_modules"} },
		"bad mode":              func(c *config.BazelBasesConfig) { c.Mode = "delete" },
		"bases-only outside":    func(c *config.BazelBasesConfig) { c.BasesOnlyRoots = []string{"/elsewhere"} },
		"root inside held":      func(c *config.BazelBasesConfig) { c.HeldPaths = []string{"/srv"} },
		"kind shorter than 12h": func(c *config.BazelBasesConfig) { c.Kinds = map[string]config.BazelBasesKind{"x": {IdleAfter: "1h"}} },
		"bases-only below idle": func(c *config.BazelBasesConfig) { c.BasesOnlyIdleAfter = "6h" },
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	for name, mutate := range cases {
		c := base
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBBCmdlineNamesIsPathBounded(t *testing.T) {
	if !bbCmdlineNames("java --output_base=/srv/a/out --x", "/srv/a/out") {
		t.Fatal("exact --output_base not matched")
	}
	if !bbCmdlineNames("bazel build --output_base /srv/a/out/", "/srv/a/out") {
		t.Fatal("trailing slash not matched")
	}
	if bbCmdlineNames("java --output_base=/srv/a/out2", "/srv/a/out") {
		t.Fatal("a sibling base with a longer name matched")
	}
}

func TestBazelBasesDueHonoursIntervalAndPressure(t *testing.T) {
	f := newBBFixture(t)
	pol, err := f.plugin.policy(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	f.plugin.env.now = func() time.Time { return now }
	if !f.plugin.due(pol, true) {
		t.Fatal("the first cycle is not due")
	}
	f.plugin.lastDiscovery = now.Add(-10 * time.Minute)
	if f.plugin.due(pol, true) {
		t.Fatal("due inside the discovery interval with no pressure")
	}
	f.usage = func(string) (bbUsage, error) { return bbUsage{BytesTotal: 100, BytesUsed: 80}, nil }
	if !f.plugin.due(pol, true) {
		t.Fatal("apply mode under pressure is not due after 5 minutes")
	}
	if f.plugin.due(pol, false) {
		t.Fatal("report mode walked again under pressure inside its interval")
	}
	f.plugin.lastDiscovery = now.Add(-2 * time.Minute)
	if f.plugin.due(pol, true) {
		t.Fatal("apply mode under pressure walked again within 5 minutes")
	}
}

// A root whose mount stays above the pressure line with no output base on it
// (sting's /srv/data: 82% full of data this plugin cannot reap) must not be
// walked on every poll. The early pressure walk is for a mount where a base
// has passed its pressure window since the last walk, or work was left behind.
func TestBazelBasesPressureWithNoBaseOnTheMountKeepsTheInterval(t *testing.T) {
	f := newBBFixture(t)
	f.usage = func(string) (bbUsage, error) { return bbUsage{BytesTotal: 100, BytesUsed: 82}, nil }
	pol, err := f.plugin.policy(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	f.plugin.env.now = func() time.Time { return now }
	cleanup := func() {
		t.Helper()
		if res := f.plugin.Cleanup(context.Background(), LevelNone, f.cfg, bbDiscardLogger()); res.Error != nil {
			t.Fatal(res.Error)
		}
	}

	// An empty root on a pressured mount: one walk, then the interval.
	cleanup()
	if f.plugin.earlyAt == nil || len(f.plugin.earlyAt) != 0 || f.plugin.lastPartial {
		t.Fatalf("a completed walk of an empty root recorded early=%v partial=%v", f.plugin.earlyAt, f.plugin.lastPartial)
	}
	now = now.Add(20 * time.Minute)
	if f.plugin.due(pol, true) {
		t.Fatal("a pressured mount with no output base on it was due again inside the discovery interval")
	}
	f.plugin.lastDiscovery = now.Add(-3 * time.Hour)
	if !f.plugin.due(pol, true) {
		t.Fatal("the discovery interval no longer makes a cycle due")
	}

	// A base on the mount, used 110 minutes ago by the plugin's clock (which
	// this test has moved 20 minutes ahead): kept. No early walk while it is
	// inside the 2h pressure window; one as soon as it has left it.
	base := filepath.Join(f.root, "a", "out")
	bbMakeBase(t, base, 64*1024, 90*time.Minute)
	f.plugin.lastDiscovery = time.Time{}
	f.plugin.emptyWalks = 0
	cleanup()
	if !bbExists(base) {
		t.Fatal("a base used 110 minutes ago was reaped under pressure (2h floor)")
	}
	now = now.Add(6 * time.Minute)
	if f.plugin.due(pol, true) {
		t.Fatal("an early walk was due while the only base was still inside its pressure window")
	}
	now = now.Add(5 * time.Minute)
	if !f.plugin.due(pol, true) {
		t.Fatal("no early walk once the base had passed its pressure window")
	}
	if f.plugin.due(pol, false) {
		t.Fatal("report mode walked again under pressure inside its interval")
	}
	cleanup()
	if bbExists(base) {
		t.Fatal("the early walk did not reap the base that had passed its pressure window")
	}
	// The base held 64 KiB, under early_walk_min_yield_mb: the walk relieved
	// nothing, so it counts as empty, after the empty walk that kept the base,
	// and the next early walk waits 5m x 2^2 (lab sting 2026-10-17).
	if f.plugin.emptyWalks != 2 {
		t.Fatalf("a walk that removed only a 64 KiB base left emptyWalks at %d, want 2", f.plugin.emptyWalks)
	}
	if got := f.plugin.earlyGap(pol); got != 4*bbPressureMinInterval {
		t.Fatalf("early gap after two empty walks is %s, want %s", got, 4*bbPressureMinInterval)
	}

	// A walk that was cut short knows too little to skip the early walk.
	now = now.Add(21 * time.Minute)
	f.plugin.earlyAt, f.plugin.lastPartial = map[uint64]time.Time{}, true
	if !f.plugin.due(pol, true) {
		t.Fatal("a partial walk suppressed the early pressure walk")
	}
}

// Adversarial review 2026-10-13 (lab R-C644 L3): a pressured mount whose only
// base can never be reaped (it holds an agent-session directory) was walked on
// every five-minute poll. Nothing an early walk could do there, so the plugin
// keeps the discovery interval.
func TestBazelBasesPressureWithOnlyAKeptBaseKeepsTheInterval(t *testing.T) {
	f := newBBFixture(t)
	f.usage = func(string) (bbUsage, error) { return bbUsage{BytesTotal: 100, BytesUsed: 75}, nil }
	pol, err := f.plugin.policy(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	f.plugin.env.now = func() time.Time { return now }
	base := filepath.Join(f.root, "a", "out")
	bbMakeBase(t, base, 64*1024, 100*time.Hour)
	bbMkdir(t, filepath.Join(base, "execroot", "_main", ".claude"))
	bbAge(t, base, 100*time.Hour)
	walks := 0
	polls := int(2 * time.Hour / (5 * time.Minute))
	for i := 0; i < polls; i++ {
		if f.plugin.due(pol, true) {
			walks++
			if res := f.plugin.Cleanup(context.Background(), LevelNone, f.cfg, bbDiscardLogger()); res.Error != nil {
				t.Fatal(res.Error)
			}
		}
		now = now.Add(5*time.Minute + time.Second)
	}
	if !bbExists(base) {
		t.Fatal("the base holding .claude was removed")
	}
	// Two hours at a 30m discovery interval is at most 5 walks; the old rule
	// made 24.
	if limit := int(2*time.Hour/pol.interval) + 1; walks > limit {
		t.Fatalf("%d walks in two hours with one permanently kept base on a pressured mount; the interval allows %d", walks, limit)
	}
}

// A walk that is always cut short keeps its early walk, but a run of early
// walks that remove nothing thins out to the discovery interval.
func TestBazelBasesEmptyEarlyWalksBackOff(t *testing.T) {
	f := newBBFixture(t)
	f.usage = func(string) (bbUsage, error) { return bbUsage{BytesTotal: 100, BytesUsed: 75}, nil }
	pol, err := f.plugin.policy(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.plugin.earlyGap(pol); got != bbPressureMinInterval {
		t.Fatalf("first early gap is %s", got)
	}
	f.plugin.emptyWalks = 1
	if got := f.plugin.earlyGap(pol); got != 2*bbPressureMinInterval {
		t.Fatalf("early gap after one empty walk is %s", got)
	}
	f.plugin.emptyWalks = 40
	if got := f.plugin.earlyGap(pol); got < pol.interval || got >= 2*pol.interval {
		t.Fatalf("early gap after many empty walks is %s, interval %s", got, pol.interval)
	}
	now := time.Now()
	f.plugin.env.now = func() time.Time { return now }
	f.plugin.earlyAt, f.plugin.lastPartial = map[uint64]time.Time{}, true
	f.plugin.emptyWalks = 2
	f.plugin.lastDiscovery = now.Add(-10 * time.Minute)
	if f.plugin.due(pol, true) {
		t.Fatal("an early walk was due 10 minutes after the second empty walk (the gap is 20 minutes)")
	}
	f.plugin.lastDiscovery = now.Add(-21 * time.Minute)
	if !f.plugin.due(pol, true) {
		t.Fatal("no early walk 21 minutes after the second empty walk")
	}
}

// Adversarial review 2026-10-13: a base whose measuring was deferred by the
// discovery pass's budget was measured again at removal time, but the
// TIN-5268 answer of that late measure was dropped, so a base holding a
// per-repository agent-session directory was removed. The late answer binds.
func TestBazelBasesLateMeasureKeepsAgentSessionDirectory(t *testing.T) {
	f := newBBFixture(t)
	f.cfg.BazelBases.MaxSecondsPerCycle = 2
	base := filepath.Join(f.root, "late", "out")
	bbMakeBase(t, base, 4096, 13*time.Hour)
	session := filepath.Join(base, "execroot", "_main", ".claude", "projects", "s.jsonl")
	bbWrite(t, session, 16)
	bbAge(t, base, 13*time.Hour)
	calls := 0
	f.plugin.env.activity = func(context.Context, bbActivityOptions) *bbActivity {
		calls++
		if calls == 1 {
			// The discovery inventory spends the measuring half of the
			// cycle budget, so the pass leaves the base unmeasured.
			time.Sleep(1200 * time.Millisecond)
		}
		return newBBActivity()
	}
	cycle := f.run()
	if !bbExists(session) {
		t.Fatalf("a late-measured base holding .claude was removed (TIN-5268): %v", bbRowPaths(cycle))
	}
	if r := bbRowFor(t, cycle, base); r.Decision != "keep" || !strings.Contains(r.Reason, "agent-session") {
		t.Fatalf("late-measured base: %q (%s)", r.Decision, r.Reason)
	}
}

// A directory the measuring walk cannot read was never checked for
// agent-session data; the base is kept rather than made writable and
// deleted unseen.
func TestBazelBasesUnreadableDirectoryKeepsBase(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads every directory")
	}
	f := newBBFixture(t)
	base := filepath.Join(f.root, "sealed", "out")
	bbMakeBase(t, base, 4096, 13*time.Hour)
	sealed := filepath.Join(base, "external", "sealed")
	bbWrite(t, filepath.Join(sealed, ".codex", "history.jsonl"), 16)
	if err := os.Chmod(sealed, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o755) })
	bbAge(t, base, 13*time.Hour)
	cycle := f.run()
	_ = os.Chmod(sealed, 0o755)
	if !bbExists(filepath.Join(sealed, ".codex", "history.jsonl")) {
		t.Fatalf("a base with an unreadable directory was removed: %v", bbRowPaths(cycle))
	}
	if r := bbRowFor(t, cycle, base); r.Decision != "keep" || !strings.Contains(r.Reason, "unreadable") {
		t.Fatalf("unreadable base: %q (%s)", r.Decision, r.Reason)
	}
}

// A server started through a symlinked spelling of its base still names it.
func TestBBCmdlineOutputBasesResolveAliases(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	real := filepath.Join(dir, "real")
	bbMkdir(t, filepath.Join(real, "ob"))
	if err := os.Symlink(real, filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	cmd := "bazel(ws) -XX:+UseParallelGC -jar A-server.jar --output_base=" + filepath.Join(dir, "alias", "ob") + " --workspace_directory=/w"
	obs := bbCmdlineOutputBases(cmd)
	if len(obs) != 1 || resolvedOrSelf(obs[0]) != filepath.Join(real, "ob") {
		t.Fatalf("output bases %v", obs)
	}
	a := newBBActivity()
	a.procs = []bbProc{{pid: 4242, cmdline: cmd}}
	if b := a.blockers(filepath.Join(real, "ob")); len(b) == 0 {
		t.Fatal("a process naming the base through an alias does not block it")
	}
	if b := a.blockers(filepath.Join(real, "ob2")); len(b) != 0 {
		t.Fatalf("an unrelated base is blocked: %v", b)
	}
}

func TestAgentSessionDenyCoversRuntimeDirTmux(t *testing.T) {
	d := newAgentSessionDeny("/home/u", 1000, "", nil, nil)
	if _, denied := d.denied("/run/user/1000/tmux-1000/default"); !denied {
		t.Fatal("tmux under /run/user is not denied")
	}
}

// The walk skips a mount point below a root by the mount table alone, so a
// hung FUSE or network mount (honey's ~/tcfs) cannot block discovery.
func TestBazelBasesWalkSkipsMountPointsWithoutStat(t *testing.T) {
	f := newBBFixture(t)
	fuse := filepath.Join(f.root, "tcfs")
	inside := filepath.Join(fuse, "proj", "out")
	bbMakeBase(t, inside, 4096, 48*time.Hour)
	f.plugin.env.mountPoints = func() ([]string, error) { return []string{"/", fuse}, nil }
	cycle := f.run()
	for _, r := range cycle.Rows {
		if strings.HasPrefix(r.Path, fuse) {
			t.Fatalf("the walk entered mount point %s: %v", fuse, bbRowPaths(cycle))
		}
	}
	if !bbExists(inside) {
		t.Fatal("a base below a mount point was removed")
	}
}

// The removal pass itself honours a late measure: a row that reaches apply
// unmeasured is scanned there, and an agent-session directory keeps it.
func TestBazelBasesApplyMeasuresUnmeasuredRowAndKeepsSessionData(t *testing.T) {
	f := newBBFixture(t)
	base := filepath.Join(f.root, "unmeasured", "out")
	bbMakeBase(t, base, 4096, 13*time.Hour)
	session := filepath.Join(base, "external", "repo", ".pi", "session.jsonl")
	bbWrite(t, session, 16)
	bbAge(t, base, 13*time.Hour)
	pol, err := f.plugin.policy(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	row := &bbRow{Path: base, Root: f.root, Decision: "eligible", window: 12 * time.Hour, normal: 12 * time.Hour}
	cycle := &bbCycle{}
	f.plugin.apply(context.Background(), pol, cycle, []*bbRow{row}, newBBActivity(), map[uint64]bbUsage{},
		map[uint64]bool{}, time.Now().Add(time.Minute), bbActivityOptions{}, bbDiscardLogger())
	if !bbExists(session) {
		t.Fatal("apply removed an unmeasured base holding .pi data (TIN-5268)")
	}
	if row.Decision != "keep" || !strings.Contains(row.Reason, "agent-session") {
		t.Fatalf("unmeasured row: %q (%s)", row.Decision, row.Reason)
	}
}

// An entry owned by another user keeps the base whole.
func TestBBMeasureTreeRefusesForeignOwnedEntries(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	bbWrite(t, filepath.Join(dir, "base", "f"), 8)
	_, _, _, err := bbMeasureTree(context.Background(), filepath.Join(dir, "base"), time.Time{}, os.Getuid()+1)
	var foreign *bbForeignOwner
	if os.Getuid() > 0 && !errors.As(err, &foreign) {
		t.Fatalf("a tree owned by another uid measured cleanly: %v", err)
	}
	if _, _, _, err := bbMeasureTree(context.Background(), filepath.Join(dir, "base"), time.Time{}, os.Getuid()); err != nil {
		t.Fatalf("own tree: %v", err)
	}
}

// Lab sting 2026-10-17: the walk covered /srv/fast-local/jess/git (7M freshly
// landed files) and the estate landing, which held 1.5 GiB of the reaper's
// 324 GiB lifetime yield, and removed two bases from inside the landing.
func TestBazelBasesSkipPathsAreNotWalked(t *testing.T) {
	f := newBBFixture(t)
	git := filepath.Join(f.root, "git")
	landing := filepath.Join(f.root, "estate")
	f.cfg.BazelBases.SkipPaths = []string{git, landing}
	inGit := filepath.Join(git, "repo", "worktrees", "w1", ".artifacts", "out")
	inLanding := filepath.Join(landing, "neo-full", "git", "w2", "out")
	outside := filepath.Join(f.root, "cache", "bazel", "output", "b1")
	bbMakeBase(t, inGit, 4096, 72*time.Hour)
	bbMakeBase(t, inLanding, 4096, 72*time.Hour)
	bbMakeBase(t, outside, 4096, 72*time.Hour)
	c := f.run()
	if !bbExists(inGit) || !bbExists(inLanding) {
		t.Fatal("a base inside skip_paths was removed")
	}
	if bbExists(outside) {
		t.Fatal("the base outside skip_paths was not reaped")
	}
	for _, p := range bbRowPaths(c) {
		if strings.HasPrefix(p, git) || strings.HasPrefix(p, landing) {
			t.Fatalf("a base inside skip_paths was discovered: %s", p)
		}
	}
	found := false
	for _, sk := range c.Skipped {
		if strings.Contains(sk.Reason, "skip_paths") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no skip_paths entry in the cycle's skipped list: %+v", c.Skipped)
	}
}

func TestBazelBasesRootPressureOverridesTheRootsLines(t *testing.T) {
	f := newBBFixture(t)
	other := filepath.Join(f.dir, "other")
	bbMkdir(t, other)
	f.cfg.BazelBases.Roots = []string{f.root, other}
	f.cfg.BazelBases.RootPressure = []config.BazelBasesRootPressure{{Root: f.root, StartPercent: 92, StopPercent: 90}}
	pol, err := f.plugin.policy(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	at := func(pct uint64) bbUsage { return bbUsage{BytesTotal: 100, BytesUsed: pct} }
	// The project tree sits at its ruled fill (90%): not pressure for its
	// own lines, pressure for the plugin-wide 70.
	if f.plugin.pressureState(f.root, 1, at(90), pol) {
		t.Fatal("90% is under the root's own 92% start line")
	}
	if !f.plugin.pressureState(other, 2, at(90), pol) {
		t.Fatal("90% is over the plugin-wide 70% start line for another root")
	}
	if !f.plugin.pressureState(f.root, 1, at(93), pol) {
		t.Fatal("93% is over the root's own 92% start line")
	}
	// Hysteresis on the root's own stop line, once a cycle has recorded the
	// device as pressured.
	f.plugin.pressured[1] = true
	if !f.plugin.pressureState(f.root, 1, at(91), pol) {
		t.Fatal("91% is above the root's 90% stop line after pressure started")
	}
	if f.plugin.pressureState(f.root, 1, at(89), pol) {
		t.Fatal("89% is below the root's 90% stop line")
	}
}

func TestBazelBasesEarlyWalksKeepTheirDutyShare(t *testing.T) {
	f := newBBFixture(t)
	pol, err := f.plugin.policy(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.plugin.lastWalk = 10 * time.Minute
	if got := f.plugin.earlyGap(pol); got != 40*time.Minute {
		t.Fatalf("after a 10 minute walk at the 20%% default duty the gap is %s, want 40m", got)
	}
	f.cfg.BazelBases.EarlyWalkMaxDutyPercent = 50
	pol, err = f.plugin.policy(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.plugin.earlyGap(pol); got != 10*time.Minute {
		t.Fatalf("at 50%% duty the gap is %s, want 10m", got)
	}
	f.plugin.lastWalk = 30 * time.Second
	if got := f.plugin.earlyGap(pol); got != bbPressureMinInterval {
		t.Fatalf("a short walk keeps the %s minimum, got %s", bbPressureMinInterval, got)
	}
}

func TestBazelBasesEarlyWalkYieldFloor(t *testing.T) {
	f := newBBFixture(t)
	pol, err := f.plugin.policy(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if pol.earlyMinYield != 256<<20 {
		t.Fatalf("default early walk yield floor %d, want 256 MiB", pol.earlyMinYield)
	}
	f.plugin.noteWork(&bbCycle{Apply: true, Removed: 1, RemovedBytes: 45 << 20}, pol.earlyMinYield)
	f.plugin.noteWork(&bbCycle{Apply: true, Removed: 1, RemovedBytes: 45 << 20}, pol.earlyMinYield)
	if f.plugin.emptyWalks != 2 {
		t.Fatalf("two 45 MiB removals left emptyWalks at %d, want 2", f.plugin.emptyWalks)
	}
	f.plugin.noteWork(&bbCycle{Apply: true, Removed: 1, RemovedBytes: 3 << 30}, pol.earlyMinYield)
	if f.plugin.emptyWalks != 0 {
		t.Fatalf("a 3 GiB removal left emptyWalks at %d, want 0", f.plugin.emptyWalks)
	}
}

func TestBazelBasesWalkScopeConfigIsValidated(t *testing.T) {
	base := config.DefaultConfig().BazelBases
	base.Mode = config.BazelBasesModeApply
	base.Roots = []string{"/srv/fast-local/jess"}
	cases := map[string]func(c *config.BazelBasesConfig){
		"pressure root not a root": func(c *config.BazelBasesConfig) {
			c.RootPressure = []config.BazelBasesRootPressure{{Root: "/home/jess", StartPercent: 92, StopPercent: 90}}
		},
		"stop above start": func(c *config.BazelBasesConfig) {
			c.RootPressure = []config.BazelBasesRootPressure{{Root: "/srv/fast-local/jess", StartPercent: 90, StopPercent: 92}}
		},
		"relative skip path":  func(c *config.BazelBasesConfig) { c.SkipPaths = []string{"git"} },
		"skip path is a root": func(c *config.BazelBasesConfig) { c.SkipPaths = []string{"/srv/fast-local/jess/"} },
		"duty over 100":       func(c *config.BazelBasesConfig) { c.EarlyWalkMaxDutyPercent = 101 },
	}
	for name, mutate := range cases {
		c := base
		c.RootPressure, c.SkipPaths = nil, nil
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
	ok := base
	ok.SkipPaths = []string{"/srv/fast-local/jess/git", "~/estate"}
	ok.RootPressure = []config.BazelBasesRootPressure{{Root: "/srv/fast-local/jess", StartPercent: 92, StopPercent: 90}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a valid walk scope was refused: %v", err)
	}
}
