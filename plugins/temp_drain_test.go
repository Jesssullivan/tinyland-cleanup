package plugins

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// With nix_temp_root_holder_max_age unset every final check takes a fresh
// inventory. With it set, an inventory that started at most that long ago
// stands in, and an older one is replaced (TIN-8526).
func TestNixTempRootHoldersMaxAge(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	takes := 0
	h := &nixTempRootHolders{
		now:  func() time.Time { return clock },
		take: func() map[string]string { takes++; return map[string]string{} },
	}
	h.current()
	h.current()
	if takes != 2 {
		t.Fatalf("max age 0 must take a fresh inventory for every check, took %d for 2", takes)
	}

	takes = 0
	h = &nixTempRootHolders{
		maxAge: 2 * time.Second,
		now:    func() time.Time { return clock },
		take: func() map[string]string {
			takes++
			if takes == 2 {
				return map[string]string{"/scan/nix-shell.held": "cwd/open file held by pid 7"}
			}
			return map[string]string{}
		},
	}
	h.current()
	clock = clock.Add(2 * time.Second)
	if got := h.current(); takes != 1 || len(got) != 0 {
		t.Fatalf("an inventory exactly max age old must stand in, took %d", takes)
	}
	clock = clock.Add(time.Millisecond)
	if got := h.current(); takes != 2 || got["/scan/nix-shell.held"] == "" {
		t.Fatalf("an inventory older than max age must be replaced and its holder seen, took %d, got %v", takes, got)
	}
}

// Through the lane: one inventory per removed root by default, one for the
// whole pass when the key allows reuse.
func TestNixTempRootLaneInventoriesPerKey(t *testing.T) {
	for _, tc := range []struct {
		maxAge string
		want   int
	}{{"", 3}, {"0s", 3}, {"1h", 1}} {
		t.Run("max_age="+tc.maxAge, func(t *testing.T) {
			scan := t.TempDir()
			for _, name := range []string{"nix-shell.aaaaa1", "nix-shell.aaaaa2", "nix-shell.aaaaa3"} {
				nixRootFixture(t, scan, name)
			}
			cfg := nixTempRootConfig(scan).DevArtifacts
			cfg.NixTempRootHolderMaxAge = tc.maxAge
			p := newDevArtifactsPluginWithActive(nil)
			inventories := 0
			p.tempActivity = func(context.Context, []string, string) (map[string]string, error) {
				inventories++
				return map[string]string{}, nil
			}
			p.cleanNixTemporaryRoots(context.Background(), scan, 0, 24*time.Hour, cfg, map[string]string{}, devArtifactTestLogger())
			if inventories != tc.want {
				t.Fatalf("inventories = %d, want %d", inventories, tc.want)
			}
			if entries, _ := os.ReadDir(scan); len(entries) != 0 {
				t.Fatalf("every unheld stale root must go, left %d", len(entries))
			}
		})
	}
}

// Removal keeps its content check ahead of the size walk, as the plan does:
// a root kept for recent content is never sized.
func TestNixTempRootContentCheckBeforeSizeWalk(t *testing.T) {
	scan := t.TempDir()
	live := nixRootFixture(t, scan, "nix-shell.live02")
	dead := nixRootFixture(t, scan, "nix-shell.dead02")
	fresh := filepath.Join(live, "pytest-of-u", "pytest-0", "case", "fresh.log")
	if err := os.WriteFile(fresh, []byte("today"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	for _, dir := range []string{live, filepath.Dir(fresh)} {
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
	}
	p := newDevArtifactsPluginWithActive(nil)
	var sized []string
	p.dirBytes = func(ctx context.Context, path string) (int64, error) {
		sized = append(sized, path)
		return getDirAllocatedBytesContext(ctx, path)
	}
	p.cleanNixTemporaryRoots(context.Background(), scan, 0, 24*time.Hour, nixTempRootConfig(scan).DevArtifacts, map[string]string{}, devArtifactTestLogger())
	if len(sized) != 1 || sized[0] != dead {
		t.Fatalf("only the deletable root may be sized, sized %v", sized)
	}
	if !pathExists(fresh) || pathExists(dead) {
		t.Fatal("the root with recent content must stay and the stale one go")
	}
}

// The inventory resolves the scan paths once and maps each descriptor path
// once. Distinct paths still map to their own roots, a repeated path maps
// again from the cache, a scan path given through a symlink matches the
// resolved paths the kernel reports, and a path that runs through that
// symlink still resolves to its root.
func TestTemporaryProcActivityMapsEachRoot(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "scan-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(resolved, "nix-shell.aaaaaa")
	b := filepath.Join(resolved, "nix-shell.bbbbbb")
	for _, dir := range []string{a, b} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The file must exist: a path that cannot be resolved is mapped as it
	// is written.
	if err := os.WriteFile(filepath.Join(b, "g"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	uid := os.Geteuid()
	proc := newFakeProcTree(t)
	proc.add(101, uid, "bash", "S", "/", filepath.Join(a, "f"), "/etc/hosts", filepath.Join(a, "f"))
	proc.add(102, uid, "bash", "S", "/", filepath.Join(link, "nix-shell.bbbbbb", "g"), filepath.Join(a, "f"))
	roots, err := temporaryRootActivityFromProc(context.Background(), proc.dir, []string{link}, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 || roots[a] == "" || roots[b] == "" {
		t.Fatalf("want both roots held, got %v", roots)
	}
}
