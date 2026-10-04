package plugins

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/quick"
	"time"
)

// writeNodeProject creates root/package.json and root/node_modules/pkg/index.js,
// plus extra sibling directories that cost walk entries, and backdates the
// marker so the artifact is stale at every mutating level.
func writeNodeProject(t *testing.T, root string, extraDirs int) string {
	t.Helper()
	nodeModules := filepath.Join(root, "node_modules")
	if err := os.MkdirAll(filepath.Join(nodeModules, "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodeModules, "pkg", "index.js"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "package.json")
	if err := os.WriteFile(marker, []byte(`{"name":"budget"}`), 0644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < extraDirs; i++ {
		// "d" sorts before "node_modules", so these entries are walked first.
		if err := os.MkdirAll(filepath.Join(root, "d"+string(rune('a'+i))), 0755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	return nodeModules
}

// A root that uses its whole share is truncated on its own; the pass goes on
// to the next scan path instead of ending (TIN-3342 PR4, #129).
func TestCleanupRootTruncationContinuesToNextScanPath(t *testing.T) {
	p := newDevArtifactsPluginWithActive(nil)
	heavyScan := t.TempDir()
	staleScan := t.TempDir()
	heavyNodeModules := writeNodeProject(t, filepath.Join(heavyScan, "heavy"), 12)
	staleNodeModules := writeNodeProject(t, filepath.Join(staleScan, "stale"), 0)

	cfg := budgetedDevArtifactConfig(heavyScan)
	cfg.DevArtifacts.ScanPaths = []string{heavyScan, staleScan}
	cfg.DevArtifacts.ScanMaxEntries = 6
	cfg.DevArtifacts.WorkspaceScanMaxRoots = 0

	result := p.Cleanup(context.Background(), LevelCritical, cfg, devArtifactTestLogger())
	if !pathExists(heavyNodeModules) {
		t.Fatal("heavy node_modules must survive: its root was truncated before complete evidence")
	}
	if pathExists(staleNodeModules) {
		t.Fatal("stale node_modules in the next scan path should be cleaned after the heavy root truncated")
	}
	if result.ItemsCleaned != 1 || result.BytesFreed <= 0 {
		t.Fatalf("expected one cleaned item, got %#v", result)
	}
}

func TestPlanCleanupReportsRootTruncationWithoutSharedExhaustion(t *testing.T) {
	p := newDevArtifactsPluginWithActive(nil)
	heavyScan := t.TempDir()
	staleScan := t.TempDir()
	writeNodeProject(t, filepath.Join(heavyScan, "heavy"), 12)
	writeNodeProject(t, filepath.Join(staleScan, "stale"), 0)

	cfg := budgetedDevArtifactConfig(heavyScan)
	cfg.DevArtifacts.ScanPaths = []string{heavyScan, staleScan}
	cfg.DevArtifacts.ScanMaxEntries = 6
	cfg.DevArtifacts.WorkspaceScanMaxRoots = 0

	plan := p.PlanCleanup(context.Background(), LevelCritical, cfg, devArtifactTestLogger())
	if len(plan.Targets) != 1 {
		t.Fatalf("expected the stale project as the only target, got %#v", plan.Targets)
	}
	if plan.Metadata["scan_budget_exhausted"] != "true" {
		t.Fatalf("root truncation must still mark evidence partial, metadata=%#v", plan.Metadata)
	}
	if plan.Metadata["scan_budget_shared_exhausted"] != "false" {
		t.Fatalf("root truncation must not exhaust the shared budget, metadata=%#v", plan.Metadata)
	}
	if plan.Metadata["workspace_roots_truncated"] != "1" {
		t.Fatalf("expected one truncated workspace root, metadata=%#v", plan.Metadata)
	}
	if plan.Metadata["scan_sizing_entries_visited"] == "0" {
		t.Fatalf("sizing walk entries should be counted, metadata=%#v", plan.Metadata)
	}
}

// A small root leaves its unused share to the roots after it. With an even
// split of 14 entries over 2 roots (7 each) the big root would truncate
// before its node_modules (its 9th of 10 entries); with carry-over the small
// root uses 3 and the big root gets 11.
func TestPlanCleanupCarriesUnusedShareToLaterRoots(t *testing.T) {
	p := newDevArtifactsPluginWithActive(nil)
	scan := t.TempDir()
	writeNodeProject(t, filepath.Join(scan, "aaa-small"), 0)
	writeNodeProject(t, filepath.Join(scan, "zzz-big"), 7)

	cfg := budgetedDevArtifactConfig(scan)
	cfg.DevArtifacts.ScanMaxEntries = 14
	cfg.DevArtifacts.WorkspaceScanMaxRoots = 0

	plan := p.PlanCleanup(context.Background(), LevelCritical, cfg, devArtifactTestLogger())
	if len(plan.Targets) != 2 {
		t.Fatalf("expected both projects found with carry-over, got %#v metadata=%#v", plan.Targets, plan.Metadata)
	}
	if plan.Metadata["scan_budget_exhausted"] != "false" {
		t.Fatalf("carry-over scan should be complete, metadata=%#v", plan.Metadata)
	}
}

func TestRootPoolNilAndUnbounded(t *testing.T) {
	var b *devArtifactScanBudget
	if b.rootPool(3) != nil {
		t.Fatal("nil budget must give a nil pool")
	}
	var pool *devArtifactRootPool
	if pool.child(0) != nil || pool.entryShare() != 0 || pool.durationShare() != 0 {
		t.Fatal("nil pool must be inert")
	}
	unbounded := (&devArtifactScanBudget{}).rootPool(2)
	child := unbounded.child(0)
	if child.maxEntries != 0 || child.maxDuration != 0 {
		t.Fatalf("unbounded pool must give unbounded children, got %#v", child)
	}
}

func TestRootPoolDurationShareCarriesOver(t *testing.T) {
	now := time.Unix(0, 0)
	pool := &devArtifactRootPool{maxDuration: 3 * time.Minute, started: now, remainingRoots: 3, now: func() time.Time { return now }}
	if got := pool.child(0).maxDuration; got != time.Minute {
		t.Fatalf("first share = %s, want 1m", got)
	}
	// The first root finished in 10s; the remaining 2m50s splits over two.
	now = now.Add(10 * time.Second)
	if got := pool.child(0).maxDuration; got != 85*time.Second {
		t.Fatalf("second share = %s, want 1m25s", got)
	}
	// Past the deadline a root still gets the 1s floor.
	now = now.Add(time.Hour)
	if got := pool.child(0).maxDuration; got != time.Second {
		t.Fatalf("late share = %s, want 1s floor", got)
	}
}

func quickConfig() *quick.Config { return &quick.Config{MaxCount: 500} }

// Property: when every root uses its whole share and the pool has at least
// one entry per root, the shares are each at least 1 and add up to exactly
// the configured budget, so per-root shares never overspend the lane.
func TestQuickRootPoolFullUseSpendsExactlyTheBudget(t *testing.T) {
	prop := func(rawMax uint32, rawRoots uint8) bool {
		roots := int(rawRoots%64) + 1
		maxEntries := roots + int(rawMax%500000)
		pool := (&devArtifactScanBudget{maxEntries: maxEntries}).rootPool(roots)
		total := 0
		for i := 0; i < roots; i++ {
			child := pool.child(0)
			if child.maxEntries < 1 {
				return false
			}
			child.entries = child.maxEntries
			pool.done(child)
			total += child.entries
		}
		return total == maxEntries
	}
	if err := quick.Check(prop, quickConfig()); err != nil {
		t.Fatal(err)
	}
}

// Property: a root that uses less than its share never shrinks a later
// root's share (carry-over is monotone), and the lane still spends no more
// than its budget.
func TestQuickRootPoolUnderuseOnlyGrowsLaterShares(t *testing.T) {
	prop := func(rawMax uint32, use []uint8) bool {
		roots := len(use)%32 + 1
		maxEntries := roots + int(rawMax%100000)
		full := (&devArtifactScanBudget{maxEntries: maxEntries}).rootPool(roots)
		under := (&devArtifactScanBudget{maxEntries: maxEntries}).rootPool(roots)
		total := 0
		for i := 0; i < roots; i++ {
			fullChild := full.child(0)
			underChild := under.child(0)
			if underChild.maxEntries < fullChild.maxEntries {
				return false
			}
			fullChild.entries = fullChild.maxEntries
			fraction := 255
			if i < len(use) {
				fraction = int(use[i])
			}
			underChild.entries = underChild.maxEntries * fraction / 255
			full.done(fullChild)
			under.done(underChild)
			total += underChild.entries
		}
		return total <= maxEntries
	}
	if err := quick.Check(prop, quickConfig()); err != nil {
		t.Fatal(err)
	}
}

// Property: merging any mix of truncated and complete root budgets never
// exhausts the shared budget; it marks evidence partial exactly when some
// root truncated, and counts the truncated roots.
func TestQuickRootTruncationNeverExhaustsShared(t *testing.T) {
	prop := func(truncated []bool) bool {
		if len(truncated) > 64 {
			truncated = truncated[:64]
		}
		shared := newDevArtifactScanBudget(budgetedDevArtifactConfig("/").DevArtifacts)
		pool := shared.rootPool(len(truncated))
		want := 0
		for i, cut := range truncated {
			child := pool.child(0)
			child.entries = 1
			if cut {
				child.markTruncated("/root/"+string(rune('a'+i%26)), "share exhausted")
				want++
			}
			pool.done(child)
			shared.mergeWorkspaceRootBudget(child)
		}
		return !shared.exhausted() &&
			shared.partial() == (want > 0) &&
			shared.workspaceRootsTruncated == want &&
			shared.entries == len(truncated)
	}
	if err := quick.Check(prop, quickConfig()); err != nil {
		t.Fatal(err)
	}
}
