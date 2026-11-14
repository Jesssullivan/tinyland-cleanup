package plugins

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

const repoContentsTestKey = "3f1c0b8d6e2a4f5b9c7d1e0a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e"

type repoContentsFixture struct {
	root        string
	repoCache   string
	keyDir      string
	outputBase  string
	copies      map[string]string // label -> copy dir
	contentsDir string
}

// newRepoContentsFixture builds one output user root whose repository
// contents cache holds seven copies of one key:
//
//	ref     10d old, the target of the output base's external/ symlink
//	new1    1h old  (newest unreferenced)
//	new2    2h old  (second newest unreferenced)
//	recent  12h old (inside the 24h minimum age)
//	old1    3d old
//	old2    4d old
//	held    5d old, held open by a process in tests that inject activity
func newRepoContentsFixture(t *testing.T) repoContentsFixture {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	our := filepath.Join(root, "_bazel_jess")
	f := repoContentsFixture{
		root:       root,
		repoCache:  filepath.Join(our, "cache", "repos", "v1"),
		outputBase: filepath.Join(our, "0123456789abcdef0123456789abcdef"),
		copies:     map[string]string{},
	}
	f.contentsDir = filepath.Join(f.repoCache, "contents")
	f.keyDir = filepath.Join(f.contentsDir, repoContentsTestKey)
	mustMkdir(t, filepath.Join(f.repoCache, "content_addressable", "sha256"))
	makeBazelOutputBase(t, f.outputBase)
	mustMkdir(t, filepath.Join(f.outputBase, "external"))

	now := time.Now()
	ages := []struct {
		label, uuid string
		age         time.Duration
	}{
		{"ref", "00000000-0000-4000-8000-000000000001", 10 * 24 * time.Hour},
		{"new1", "00000000-0000-4000-8000-000000000002", time.Hour},
		{"new2", "00000000-0000-4000-8000-000000000003", 2 * time.Hour},
		{"recent", "00000000-0000-4000-8000-000000000004", 12 * time.Hour},
		{"old1", "00000000-0000-4000-8000-000000000005", 3 * 24 * time.Hour},
		{"old2", "00000000-0000-4000-8000-000000000006", 4 * 24 * time.Hour},
		{"held", "00000000-0000-4000-8000-000000000007", 5 * 24 * time.Hour},
	}
	for _, a := range ages {
		dir := filepath.Join(f.keyDir, a.uuid)
		deep := filepath.Join(dir, "python", "lib", "python3.11", "site-packages")
		mustMkdir(t, deep)
		if err := os.WriteFile(filepath.Join(deep, "blob.so"), bytesOfSize(256*1024), 0o444); err != nil {
			t.Fatal(err)
		}
		rec := dir + bazelRepoContentsRecorded
		if err := os.WriteFile(rec, []byte("FILE:@@//:MODULE.bazel 0123\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Bazel leaves read-only trees; the apply path must cope.
		if err := os.Chmod(deep, 0o555); err != nil {
			t.Fatal(err)
		}
		when := now.Add(-a.age)
		for _, p := range []string{deep, filepath.Dir(deep), dir, rec} {
			if err := os.Chtimes(p, when, when); err != nil {
				t.Fatal(err)
			}
		}
		f.copies[a.label] = dir
	}
	if err := os.Symlink(f.copies["ref"], filepath.Join(f.outputBase, "external", "rules_python++python+python_3_11_x86_64-unknown-linux-gnu")); err != nil {
		t.Fatal(err)
	}
	// A plain directory in external/ is not a cache reference.
	mustMkdir(t, filepath.Join(f.outputBase, "external", "local_repo"))
	return f
}

func repoContentsTestConfig(root string, keep int) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Bazel.Roots = []string{root}
	cfg.Bazel.RepoContentsDedupe = true
	cfg.Bazel.RepoContentsMinAge = "24h"
	cfg.Bazel.RepoContentsKeepRecent = keep
	cfg.Enable.BazelBases = false
	return cfg
}

func repoContentsActivity(held ...string) func(context.Context, bbActivityOptions) *bbActivity {
	return func(_ context.Context, opts bbActivityOptions) *bbActivity {
		a := newBBActivity()
		for _, h := range held {
			a.index(filepath.Join(h, "python", "lib", "python3.11", "site-packages", "blob.so"), "pid 4242 holds blob.so", opts.Within)
		}
		return a
	}
}

func planRepoContentsFixture(t *testing.T, p *BazelPlugin, f repoContentsFixture, cfg *config.Config, level CleanupLevel) ([]CleanupTarget, map[string]string, bazelPlanState) {
	t.Helper()
	now := time.Now()
	candidates := discoverBazelRootCandidates(f.root)
	state := bazelPlanState{
		repoCaches:  measureBazelRepoCacheCandidates(context.Background(), candidates, now),
		outputBases: bazelRepoContentsOutputBases(candidates, bazelRepoContentsRoots(cfg, "")),
	}
	targets, meta := p.planBazelRepoContents(context.Background(), cfg, level, state.repoCaches, collectBazelRepoContentsRefs(state.outputBases), now)
	return targets, meta, state
}

func TestBazelRepoContentsPlanKeepsReferencedNewestRecentAndOpenCopies(t *testing.T) {
	f := newRepoContentsFixture(t)
	cfg := repoContentsTestConfig(f.root, 2)
	p := &BazelPlugin{activityScan: repoContentsActivity(f.copies["held"])}

	targets, meta, state := planRepoContentsFixture(t, p, f, cfg, LevelModerate)
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1: %#v", len(targets), targets)
	}
	tg := targets[0]
	if tg.Type != bazelRepoContentsType || tg.Action != bazelRepoContentsDelete || tg.Protected || tg.Active {
		t.Fatalf("target = %#v, want an unprotected %s", tg, bazelRepoContentsDelete)
	}
	want := "7 copies: keep 5 (1 referenced, 2 newest, 1 recent, 1 open, 0 foreign), remove 2"
	if !strings.HasPrefix(tg.Reason, want) {
		t.Fatalf("reason = %q, want prefix %q", tg.Reason, want)
	}
	if tg.Bytes < 2*256*1024 {
		t.Fatalf("reclaim bytes = %d, want at least two 256 KiB copies", tg.Bytes)
	}
	if meta["repo_contents_remove_copies"] != "2" || meta["repo_contents_keep_copies"] != "5" || meta["repo_contents_output_bases_scanned"] != "1" || meta["repo_contents_external_links"] != "1" {
		t.Fatalf("metadata = %#v", meta)
	}
	t.Logf("plan line: %s [%s]: %s, reclaim=%d", tg.Name, tg.Action, tg.Reason, tg.Bytes)

	// Dry run: planning removed nothing.
	for label, dir := range f.copies {
		if !pathExistsAndIsDir(dir) {
			t.Fatalf("plan removed copy %s", label)
		}
	}

	var result CleanupResult
	p.applyBazelRepoContents(context.Background(), cfg, targets, state.repoCaches, state.outputBases, slog.New(slog.NewTextHandler(io.Discard, nil)), &result)
	for _, label := range []string{"ref", "new1", "new2", "recent", "held"} {
		if !pathExistsAndIsDir(f.copies[label]) {
			t.Fatalf("apply removed kept copy %s", label)
		}
		if _, err := os.Stat(f.copies[label] + bazelRepoContentsRecorded); err != nil {
			t.Fatalf("apply removed recorded_inputs of kept copy %s: %v", label, err)
		}
	}
	for _, label := range []string{"old1", "old2"} {
		if _, err := os.Lstat(f.copies[label]); !os.IsNotExist(err) {
			t.Fatalf("copy %s survived apply: %v", label, err)
		}
		if _, err := os.Lstat(f.copies[label] + bazelRepoContentsRecorded); !os.IsNotExist(err) {
			t.Fatalf("recorded_inputs of %s survived apply: %v", label, err)
		}
	}
	if result.ItemsCleaned != 2 || result.BytesFreed != tg.Bytes {
		t.Fatalf("result = %#v, want 2 items and %d bytes", result, tg.Bytes)
	}
	// The referenced copy still resolves through the output base.
	if _, err := os.Stat(filepath.Join(f.outputBase, "external", "rules_python++python+python_3_11_x86_64-unknown-linux-gnu", "python")); err != nil {
		t.Fatalf("referenced copy no longer resolves: %v", err)
	}
}

func TestBazelRepoContentsReportOnlyAtWarningAndDefaultKeep(t *testing.T) {
	f := newRepoContentsFixture(t)
	cfg := repoContentsTestConfig(f.root, 3)
	p := &BazelPlugin{activityScan: repoContentsActivity()}

	targets, _, _ := planRepoContentsFixture(t, p, f, cfg, LevelWarning)
	if len(targets) != 1 || targets[0].Action != bazelRepoContentsReport {
		t.Fatalf("targets = %#v, want one %s", targets, bazelRepoContentsReport)
	}
	// keep 3: new1, new2, recent are the newest three; old1, old2 and held go.
	want := "7 copies: keep 4 (1 referenced, 3 newest, 0 recent, 0 open, 0 foreign), remove 3"
	if !strings.HasPrefix(targets[0].Reason, "would remove at moderate or higher: "+want) {
		t.Fatalf("reason = %q", targets[0].Reason)
	}
	if bazelEstimatedCandidateBytes(targets) != 0 {
		t.Fatal("a report-only target counted as reclaim")
	}
}

func TestBazelRepoContentsFailsClosed(t *testing.T) {
	f := newRepoContentsFixture(t)
	cfg := repoContentsTestConfig(f.root, 1)

	// An incomplete process inventory keeps every copy.
	p := &BazelPlugin{activityScan: func(context.Context, bbActivityOptions) *bbActivity {
		a := newBBActivity()
		a.incomplete = append(a.incomplete, "pid 7 fd unreadable")
		return a
	}}
	targets, _, state := planRepoContentsFixture(t, p, f, cfg, LevelCritical)
	if len(targets) != 1 || targets[0].Action != "keep" || !targets[0].Protected || !strings.Contains(targets[0].Reason, "activity inventory incomplete") {
		t.Fatalf("targets = %#v, want kept for an incomplete inventory", targets)
	}

	// A copy that became referenced after planning survives apply.
	p = &BazelPlugin{activityScan: repoContentsActivity()}
	targets, _, state = planRepoContentsFixture(t, p, f, cfg, LevelCritical)
	if err := os.Symlink(f.copies["old1"], filepath.Join(f.outputBase, "external", "late_repo")); err != nil {
		t.Fatal(err)
	}
	var result CleanupResult
	p.applyBazelRepoContents(context.Background(), cfg, targets, state.repoCaches, state.outputBases, slog.New(slog.NewTextHandler(io.Discard, nil)), &result)
	if !pathExistsAndIsDir(f.copies["old1"]) {
		t.Fatal("apply removed a copy that became referenced after planning")
	}

	// The dedupe switch off keeps everything.
	cfg.Bazel.RepoContentsDedupe = false
	targets, _, _ = planRepoContentsFixture(t, p, f, cfg, LevelCritical)
	if len(targets) != 1 || targets[0].Action != "keep" || !strings.Contains(targets[0].Reason, "repo_contents_dedupe is off") {
		t.Fatalf("targets = %#v, want kept with dedupe off", targets)
	}
}

func TestBazelRepoContentsCopyPathAllowed(t *testing.T) {
	ok := "/home/jess/.cache/bazel/_bazel_jess/cache/repos/v1/contents/" + repoContentsTestKey + "/00000000-0000-4000-8000-000000000001"
	if !bazelRepoContentsCopyPathAllowed(ok) {
		t.Fatalf("rejected %s", ok)
	}
	for _, bad := range []string{
		"/home/jess/.cache/bazel/_bazel_jess/cache/repos/v1/contents/" + repoContentsTestKey + "/not-a-uuid",
		"/home/jess/.cache/bazel/_bazel_jess/cache/repos/v1/other/" + repoContentsTestKey + "/00000000-0000-4000-8000-000000000001",
		"/home/jess/git/contents/" + repoContentsTestKey + "/00000000-0000-4000-8000-000000000001",
	} {
		if bazelRepoContentsCopyPathAllowed(bad) {
			t.Fatalf("allowed %s", bad)
		}
	}
}

func TestBazelRepoCacheMeasuresLargeEntryBelowTopLevel(t *testing.T) {
	root := t.TempDir()
	repoCache := filepath.Join(root, "_bazel_jess", "cache", "repos", "v1")
	deep := filepath.Join(repoCache, "contents", repoContentsTestKey, "00000000-0000-4000-8000-0000000000aa", "a", "b", "c", "d")
	mustMkdir(t, deep)
	const size = 8 * 1024 * 1024
	if err := os.WriteFile(filepath.Join(deep, "big.bin"), bytesOfSize(size), 0o644); err != nil {
		t.Fatal(err)
	}
	cas := filepath.Join(repoCache, "content_addressable", "sha256", "ab")
	mustMkdir(t, cas)
	if err := os.WriteFile(filepath.Join(cas, "file"), bytesOfSize(size/2), 0o644); err != nil {
		t.Fatal(err)
	}

	_, topLevel := estimateBazelCandidateBytes(repoCache)
	if topLevel >= size {
		t.Fatalf("top-level estimate %d already sees the large entry; the fixture is not deep enough", topLevel)
	}

	candidates := discoverBazelRootCandidates(root)
	measures := measureBazelRepoCacheCandidates(context.Background(), candidates, time.Now())
	if len(measures) != 1 {
		t.Fatalf("got %d measures, want 1", len(measures))
	}
	m := measures[0]
	if m.Partial || m.Estimated != 0 {
		t.Fatalf("measure partial=%v estimated=%d, want a complete walk", m.Partial, m.Estimated)
	}
	if m.Bytes < size+size/2 {
		t.Fatalf("measured %d bytes, want at least %d", m.Bytes, size+size/2)
	}
	var tier *bazelCandidate
	for i := range candidates {
		if candidates[i].Type == "repository_cache" {
			tier = &candidates[i]
		}
	}
	if tier == nil || tier.Physical != m.Bytes {
		t.Fatalf("repository_cache candidate = %#v, want physical %d", tier, m.Bytes)
	}
	if len(m.Keys) != 1 || len(m.Keys[0].Copies) != 1 || m.Keys[0].Copies[0].Bytes < size {
		t.Fatalf("per-entry measure = %#v", m.Keys)
	}
}

func TestBazelRepoCacheMeasureEstimatesUnwalkedCopiesFromTheirKey(t *testing.T) {
	// A pre-cancelled context walks nothing: every copy is estimated and
	// the measure is partial, never a silent zero presented as complete.
	f := newRepoContentsFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := measureBazelRepoCache(ctx, f.repoCache, time.Now(), time.Minute)
	if !m.Partial || m.Estimated != 7 {
		t.Fatalf("partial=%v estimated=%d, want partial with 7 estimated copies", m.Partial, m.Estimated)
	}
}

// singleCopyCache builds an output user root whose repository contents cache
// holds one key with one copy, and an output base. linked makes the output
// base's external/ symlink point at the copy.
func singleCopyCache(t *testing.T, linked bool) (root, repoCache, copyDir, outputBase string) {
	t.Helper()
	root = t.TempDir()
	our := filepath.Join(root, "_bazel_jess")
	repoCache = filepath.Join(our, "cache", "repos", "v1")
	copyDir = filepath.Join(repoCache, "contents", repoContentsTestKey, "00000000-0000-4000-8000-0000000000bb")
	mustMkdir(t, filepath.Join(copyDir, "lib"))
	if err := os.WriteFile(copyDir+bazelRepoContentsRecorded, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outputBase = filepath.Join(our, "fedcba9876543210fedcba9876543210")
	makeBazelOutputBase(t, outputBase)
	mustMkdir(t, filepath.Join(outputBase, "external"))
	if linked {
		if err := os.Symlink(copyDir, filepath.Join(outputBase, "external", "rules_python")); err != nil {
			t.Fatal(err)
		}
	}
	return root, repoCache, copyDir, outputBase
}

// planTierOverBudget runs the whole-tier rule with the repository cache
// forced over a 1 GiB budget and stale, then the TIN-5770 guard.
func planTierOverBudget(t *testing.T, root string, activeErr error) CleanupTarget {
	t.Helper()
	now := time.Now()
	candidates := discoverBazelRootCandidates(root)
	measures := measureBazelRepoCacheCandidates(context.Background(), candidates, now)
	for i := range candidates {
		if candidates[i].Type == "repository_cache" {
			candidates[i].Physical = 2 * bazelGiB
			candidates[i].ModTime = now.Add(-30 * 24 * time.Hour)
		} else {
			candidates[i].ModTime = now
		}
	}
	targets, _ := bazelPlanTargets(candidates, config.BazelConfig{MaxTotalGB: 1, StaleAfter: "14d", CriticalStaleAfter: "3d"}, LevelCritical, now, false)
	refs := collectBazelRepoContentsRefs(bazelRepoContentsOutputBases(candidates, []string{root}))
	targets = guardBazelRepositoryCacheTiers(targets, measures, refs, activeErr)
	for _, tg := range targets {
		if tg.Type == "repository_cache" {
			return tg
		}
	}
	t.Fatalf("no repository_cache target in %#v", targets)
	return CleanupTarget{}
}

func TestBazelRepositoryCacheTierWithheldWhileACopyIsReferenced(t *testing.T) {
	root, _, _, _ := singleCopyCache(t, true)
	tg := planTierOverBudget(t, root, nil)
	if tg.Action != "keep" || !tg.Protected || !strings.Contains(tg.Reason, "1 copies are referenced by output bases") {
		t.Fatalf("tier = %#v, want whole-tier deletion withheld for a referenced copy", tg)
	}
	if bazelTargetReclaimsHost(tg) {
		t.Fatal("withheld tier still counts as reclaim")
	}
}

func TestBazelRepositoryCacheTierWithheldWhileDuplicatesExist(t *testing.T) {
	f := newRepoContentsFixture(t)
	// No copy is referenced: drop the fixture's external/ link.
	if err := os.Remove(filepath.Join(f.outputBase, "external", "rules_python++python+python_3_11_x86_64-unknown-linux-gnu")); err != nil {
		t.Fatal(err)
	}
	tg := planTierOverBudget(t, f.root, nil)
	if tg.Action != "keep" || !tg.Protected || !strings.Contains(tg.Reason, "1 keys hold duplicate copies") {
		t.Fatalf("tier = %#v, want whole-tier deletion withheld while duplicates exist", tg)
	}
	// The dedupe class carries the reclaim instead.
	cfg := repoContentsTestConfig(f.root, 3)
	targets, _, _ := planRepoContentsFixture(t, &BazelPlugin{activityScan: repoContentsActivity()}, f, cfg, LevelCritical)
	if len(targets) != 1 || targets[0].Action != bazelRepoContentsDelete {
		t.Fatalf("dedupe targets = %#v, want one %s", targets, bazelRepoContentsDelete)
	}
}

func TestBazelRepositoryCacheTierDeletableOnlyWhenUnreferencedAndInactive(t *testing.T) {
	root, _, _, _ := singleCopyCache(t, false)
	if tg := planTierOverBudget(t, root, nil); tg.Action != "delete_cache_tier" {
		t.Fatalf("tier = %#v, want delete_cache_tier with no reference, no duplicates and no Bazel process", tg)
	}
	if tg := planTierOverBudget(t, root, os.ErrPermission); tg.Action != "keep" || !strings.Contains(tg.Reason, "process inspection failed") {
		t.Fatalf("tier = %#v, want kept when process inspection failed", tg)
	}
	// A visible Bazel process keeps the tier through the existing rule.
	now := time.Now()
	candidates := discoverBazelRootCandidates(root)
	for i := range candidates {
		candidates[i].Physical = 2 * bazelGiB
		candidates[i].ModTime = now.Add(-30 * 24 * time.Hour)
	}
	targets, _ := bazelPlanTargets(candidates, config.BazelConfig{MaxTotalGB: 1}, LevelCritical, now, true)
	for _, tg := range targets {
		if tg.Type == "repository_cache" && tg.Action == "delete_cache_tier" {
			t.Fatalf("tier deletable while a Bazel process is visible: %#v", tg)
		}
	}
}

func TestBazelUserRootLinkReachesOnlyTheRepositoryCache(t *testing.T) {
	real := t.TempDir()
	_, repoCache, copyDir, outputBase := singleCopyCache(t, true)
	userRoot := filepath.Dir(filepath.Dir(filepath.Dir(repoCache)))
	// ~/.cache/bazel/_bazel_jess -> <real output user root>, as on sting.
	home := filepath.Join(real, "cache-bazel")
	mustMkdir(t, home)
	if err := os.Symlink(userRoot, filepath.Join(home, "_bazel_jess")); err != nil {
		t.Fatal(err)
	}
	// A chained link (link -> link -> dir) is never followed.
	if err := os.Symlink(filepath.Join(home, "_bazel_jess"), filepath.Join(home, "_bazel_chain")); err != nil {
		t.Fatal(err)
	}

	// Default-like config: bazel.roots is the directory holding the link.
	candidates := NewBazelPlugin().discoverCandidates(t.TempDir(), config.BazelConfig{Roots: []string{home}}, bazelProcessInfo{})
	var sawCache, sawOutputBase int
	for _, c := range candidates {
		switch {
		case c.Type == "repository_cache" && c.Path == resolvedOrSelf(repoCache):
			sawCache++
		case bazelCandidateIsOutputBaseLike(c.Type):
			sawOutputBase++
		}
	}
	if sawCache != 1 || sawOutputBase != 0 {
		t.Fatalf("candidates = %#v, want the linked cache once and no output-base deletion candidate", candidates)
	}
	// The root's own discovery never follows the link to output bases.
	for _, c := range discoverBazelRootCandidates(home) {
		if bazelCandidateIsOutputBaseLike(c.Type) || pathInsideRoot(c.Path, resolvedOrSelf(userRoot)) {
			t.Fatalf("root discovery followed the _bazel_* link: %#v", c)
		}
	}
	// And the plan never targets the output base for deletion.
	targets, _ := bazelPlanTargets(candidates, config.BazelConfig{StaleAfter: "1h", CriticalStaleAfter: "1h"}, LevelCritical, time.Now().Add(1000*time.Hour), false)
	for _, tg := range targets {
		if tg.Path == resolvedOrSelf(outputBase) {
			t.Fatalf("output base behind the link became a target: %#v", tg)
		}
	}
	measures := measureBazelRepoCacheCandidates(context.Background(), candidates, time.Now())
	if len(measures) != 1 || len(measures[0].Keys) != 1 {
		t.Fatalf("measures = %#v, want the linked cache measured", measures)
	}
	refs := collectBazelRepoContentsRefs(bazelRepoContentsOutputBases(nil, []string{home}))
	if _, ok := refs.referencedBy(copyDir); !ok {
		t.Fatal("shallow output-base scan did not follow the top-level _bazel_* link")
	}
}

func TestDiscoverCandidatesAddsDefaultOutputRootRepositoryCache(t *testing.T) {
	home := t.TempDir()
	_, repoCache, _, outputBase := singleCopyCache(t, true)
	userRoot := filepath.Dir(filepath.Dir(filepath.Dir(repoCache)))
	mustMkdir(t, filepath.Join(home, ".cache", "bazel"))
	if err := os.Symlink(userRoot, filepath.Join(home, ".cache", "bazel", "_bazel_jess")); err != nil {
		t.Fatal(err)
	}
	// bazel.roots lists an unrelated tree, as sting's config does.
	other := t.TempDir()
	candidates := NewBazelPlugin().discoverCandidates(home, config.BazelConfig{Roots: []string{other}}, bazelProcessInfo{})
	var caches, bases int
	for _, c := range candidates {
		if c.Type == "repository_cache" && c.Path == resolvedOrSelf(repoCache) {
			caches++
		}
		if bazelCandidateIsOutputBaseLike(c.Type) {
			bases++
		}
	}
	if caches != 1 || bases != 0 {
		t.Fatalf("candidates = %#v, want the default-root repository cache only", candidates)
	}
	cfg := config.DefaultConfig()
	cfg.Bazel.Roots = []string{other}
	cfg.Enable.BazelBases = false
	obs := bazelRepoContentsOutputBases(candidates, bazelRepoContentsRoots(cfg, home))
	found := false
	for _, ob := range obs {
		if resolvedOrSelf(ob) == resolvedOrSelf(outputBase) {
			found = true
		}
	}
	if !found {
		t.Fatalf("output-base scan %v missed %s behind the default-root link", obs, outputBase)
	}
}

func TestBazelRepoContentsSkipsWhileGCLockIsHeldExclusively(t *testing.T) {
	f := newRepoContentsFixture(t)
	cfg := repoContentsTestConfig(f.root, 1)
	p := &BazelPlugin{activityScan: repoContentsActivity()}
	lock, err := os.OpenFile(filepath.Join(f.contentsDir, bazelRepoContentsGCLock), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	// The plan reports the busy lock instead of a removal.
	targets, _, state := planRepoContentsFixture(t, p, f, cfg, LevelCritical)
	if len(targets) != 1 || targets[0].Action != "keep" || !strings.HasPrefix(targets[0].Reason, "bazel active (gc_lock)") {
		t.Fatalf("targets = %#v, want kept with bazel active (gc_lock)", targets)
	}
	// A lock taken after planning stops apply too.
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	targets, _, state = planRepoContentsFixture(t, p, f, cfg, LevelCritical)
	if targets[0].Action != bazelRepoContentsDelete {
		t.Fatalf("target = %#v, want a deletion with the lock free", targets[0])
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	var held CleanupResult
	p.applyBazelRepoContents(context.Background(), cfg, targets, state.repoCaches, state.outputBases, slog.New(slog.NewTextHandler(io.Discard, nil)), &held)
	if held.ItemsCleaned != 0 {
		t.Fatalf("apply removed %d copies while the gc_lock was held", held.ItemsCleaned)
	}

	// Once the lock is free, apply removes and leaves the lock file alone.
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	var r CleanupResult
	p.applyBazelRepoContents(context.Background(), cfg, targets, state.repoCaches, state.outputBases, slog.New(slog.NewTextHandler(io.Discard, nil)), &r)
	if r.ItemsCleaned == 0 {
		t.Fatal("apply removed nothing with the gc_lock free")
	}
	if _, err := os.Stat(filepath.Join(f.contentsDir, bazelRepoContentsGCLock)); err != nil {
		t.Fatalf("gc_lock was removed: %v", err)
	}
}

func TestBazelRepoContentsDefaultMinAgeIs72h(t *testing.T) {
	if got := bazelRepoContentsPolicyFor(config.DefaultConfig().Bazel).minAge; got != 72*time.Hour {
		t.Fatalf("default min age = %s, want 72h", got)
	}
	if got := bazelRepoContentsPolicyFor(config.BazelConfig{}).minAge; got != 72*time.Hour {
		t.Fatalf("unset min age = %s, want 72h", got)
	}
}
