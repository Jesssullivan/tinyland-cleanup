package plugins

// Adversarial tests from the independent review of PR #176 at 75c7991
// (TIN-5770), kept as regression tests.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func advLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A1: a running Bazel command holds Bazel's own shared gc_lock on
// contents/gc_lock. Does apply still delete?
func TestAdvGCLockIgnored(t *testing.T) {
	f := newRepoContentsFixture(t)
	cfg := repoContentsTestConfig(f.root, 1)
	p := &BazelPlugin{activityScan: repoContentsActivity()}
	lock, err := os.OpenFile(filepath.Join(f.contentsDir, "gc_lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	targets, _, state := planRepoContentsFixture(t, p, f, cfg, LevelCritical)
	var r CleanupResult
	p.applyBazelRepoContents(context.Background(), cfg, targets, state.repoCaches, state.outputBases, advLogger(), &r)
	t.Logf("deleted %d copies while a shared gc_lock was held", r.ItemsCleaned)
	if r.ItemsCleaned > 0 {
		t.Errorf("FINDING: deleted %d copies while Bazel's shared gc_lock was held", r.ItemsCleaned)
	}
}

// A2: link created after the refs scan in apply (simulated in the activity
// seam, which runs after collectBazelRepoContentsRefs), without a touch.
func TestAdvLinkAfterApplyRefScan(t *testing.T) {
	f := newRepoContentsFixture(t)
	cfg := repoContentsTestConfig(f.root, 1)
	p := &BazelPlugin{activityScan: repoContentsActivity()}
	targets, _, state := planRepoContentsFixture(t, p, f, cfg, LevelCritical)
	p.activityScan = func(ctx context.Context, o bbActivityOptions) *bbActivity {
		_ = os.Symlink(f.copies["old1"], filepath.Join(f.outputBase, "external", "late"))
		return newBBActivity()
	}
	var r CleanupResult
	p.applyBazelRepoContents(context.Background(), cfg, targets, state.repoCaches, state.outputBases, advLogger(), &r)
	if !pathExistsAndIsDir(f.copies["old1"]) {
		t.Errorf("FINDING: copy linked after the apply ref scan (no touch) was deleted")
	}
}

// A2b: same but Bazel also touches recorded_inputs (its real behavior).
func TestAdvLinkAndTouchAfterApplyRefScan(t *testing.T) {
	f := newRepoContentsFixture(t)
	cfg := repoContentsTestConfig(f.root, 1)
	p := &BazelPlugin{activityScan: repoContentsActivity()}
	targets, _, state := planRepoContentsFixture(t, p, f, cfg, LevelCritical)
	p.activityScan = func(ctx context.Context, o bbActivityOptions) *bbActivity {
		now := time.Now()
		_ = os.Chtimes(f.copies["old1"]+bazelRepoContentsRecorded, now, now)
		_ = os.Symlink(f.copies["old1"], filepath.Join(f.outputBase, "external", "late"))
		return newBBActivity()
	}
	var r CleanupResult
	p.applyBazelRepoContents(context.Background(), cfg, targets, state.repoCaches, state.outputBases, advLogger(), &r)
	if !pathExistsAndIsDir(f.copies["old1"]) {
		t.Errorf("FINDING: touched+linked copy deleted")
	}
}

// A3: relative link and a two-hop chain from external/.
func TestAdvRelativeAndChainLinks(t *testing.T) {
	f := newRepoContentsFixture(t)
	cfg := repoContentsTestConfig(f.root, 1)
	ext := filepath.Join(f.outputBase, "external")
	rel, _ := filepath.Rel(ext, f.copies["old1"])
	if err := os.Symlink(rel, filepath.Join(ext, "relrepo")); err != nil {
		t.Fatal(err)
	}
	hop := filepath.Join(t.TempDir(), "hop")
	if err := os.Symlink(f.copies["old2"], hop); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hop, filepath.Join(ext, "chain")); err != nil {
		t.Fatal(err)
	}
	// link to a subdirectory of a copy
	if err := os.Symlink(filepath.Join(f.copies["held"], "python"), filepath.Join(ext, "sub")); err != nil {
		t.Fatal(err)
	}
	p := &BazelPlugin{activityScan: repoContentsActivity()}
	targets, _, state := planRepoContentsFixture(t, p, f, cfg, LevelCritical)
	var r CleanupResult
	p.applyBazelRepoContents(context.Background(), cfg, targets, state.repoCaches, state.outputBases, advLogger(), &r)
	for _, l := range []string{"old1", "old2", "held"} {
		if !pathExistsAndIsDir(f.copies[l]) {
			t.Errorf("FINDING: %s deleted despite link", l)
		}
	}
}

// A3b: output base reached through a symlinked external/ dir with a
// relative ../ link (kernel resolves relative to the real dir).
func TestAdvSymlinkedExternalRelative(t *testing.T) {
	f := newRepoContentsFixture(t)
	cfg := repoContentsTestConfig(f.root, 1)
	ext := filepath.Join(f.outputBase, "external")
	realExt := filepath.Join(t.TempDir(), "a", "external")
	mustMkdir(t, realExt)
	rel, _ := filepath.Rel(realExt, f.copies["old1"])
	if err := os.Symlink(rel, filepath.Join(realExt, "relrepo")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(ext); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realExt, ext); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ext, "relrepo", "python")); err != nil {
		t.Fatalf("setup: link does not resolve: %v", err)
	}
	p := &BazelPlugin{activityScan: repoContentsActivity()}
	targets, _, state := planRepoContentsFixture(t, p, f, cfg, LevelCritical)
	var r CleanupResult
	p.applyBazelRepoContents(context.Background(), cfg, targets, state.repoCaches, state.outputBases, advLogger(), &r)
	if !pathExistsAndIsDir(f.copies["old1"]) {
		t.Errorf("FINDING: copy referenced via relative link under symlinked external/ deleted")
	}
}

// A4: a UUID entry that is a symlink to an outside directory.
func TestAdvUUIDSymlinkNotFollowed(t *testing.T) {
	f := newRepoContentsFixture(t)
	cfg := repoContentsTestConfig(f.root, 1)
	outside := filepath.Join(t.TempDir(), "victim")
	mustMkdir(t, outside)
	_ = os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0o644)
	link := filepath.Join(f.keyDir, "00000000-0000-4000-8000-0000000000ee")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	_ = os.Chtimes(outside, old, old)
	p := &BazelPlugin{activityScan: repoContentsActivity()}
	targets, _, state := planRepoContentsFixture(t, p, f, cfg, LevelCritical)
	var r CleanupResult
	p.applyBazelRepoContents(context.Background(), cfg, targets, state.repoCaches, state.outputBases, advLogger(), &r)
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Errorf("FINDING: outside dir touched: %v", err)
	}
	if err := deleteBazelRepoContentsCopy(link, advLogger()); err == nil {
		t.Errorf("FINDING: delete accepted a symlink copy")
	}
}

// A5: output base outside every scanned root links an old copy.
func TestAdvOutputBaseOutsideRoots(t *testing.T) {
	f := newRepoContentsFixture(t)
	cfg := repoContentsTestConfig(f.root, 1)
	ob := filepath.Join(t.TempDir(), "custom_output_base")
	makeBazelOutputBase(t, ob)
	mustMkdir(t, filepath.Join(ob, "external"))
	_ = os.Symlink(f.copies["old1"], filepath.Join(ob, "external", "r"))
	p := &BazelPlugin{activityScan: repoContentsActivity()}
	targets, _, state := planRepoContentsFixture(t, p, f, cfg, LevelCritical)
	var r CleanupResult
	p.applyBazelRepoContents(context.Background(), cfg, targets, state.repoCaches, state.outputBases, advLogger(), &r)
	if !pathExistsAndIsDir(f.copies["old1"]) {
		t.Logf("EXPECTED-BY-DESIGN: copy linked only from an out-of-roots output base was deleted")
	}
}

// A6: whole-tier guard with an unreadable key dir holding a referenced copy.
func TestAdvTierGuardUnreadableKey(t *testing.T) {
	root, _, copyDir, _ := singleCopyCache(t, true)
	keyDir := filepath.Dir(copyDir)
	if err := os.Chmod(keyDir, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(keyDir, 0o755) }()
	tg := planTierOverBudget(t, root, nil)
	t.Logf("tier action=%s reason=%s", tg.Action, tg.Reason)
	if tg.Action == "delete_cache_tier" {
		t.Errorf("FINDING: whole-tier delete planned although a key dir was unreadable (its linked copy unseen)")
	}
}

// A7: _trash dir from Bazel's own GC listed as a key.
func TestAdvTrashKey(t *testing.T) {
	f := newRepoContentsFixture(t)
	for _, u := range []string{"00000000-0000-4000-8000-0000000000a1", "00000000-0000-4000-8000-0000000000a2"} {
		mustMkdir(t, filepath.Join(f.contentsDir, "_trash", u))
	}
	keys := listBazelRepoContents(f.contentsDir, time.Now())
	for _, k := range keys {
		t.Logf("key %s copies=%d", k.Key, len(k.Copies))
		if k.Key == "_trash" {
			t.Errorf("FINDING: Bazel's _trash/ listed as a key")
		}
	}
	if len(keys) != 1 {
		t.Errorf("got %d keys, want only the fixture key", len(keys))
	}
}
