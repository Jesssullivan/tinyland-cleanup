package plugins

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

func TestDiscoverBazelRootCandidates(t *testing.T) {
	root := t.TempDir()
	outputBase := filepath.Join(root, "_bazel_jess", "abc123")
	makeBazelOutputBase(t, outputBase)
	explicitOutputBase := filepath.Join(root, "tinyvectors-external-smoke-ob")
	makeBazelOutputBase(t, explicitOutputBase)
	mustMkdir(t, filepath.Join(root, "repository_cache"))
	mustMkdir(t, filepath.Join(root, "disk_cache"))

	candidates := discoverBazelRootCandidates(root)
	byType := map[string]bool{}
	byPath := map[string]bool{}
	for _, candidate := range candidates {
		byType[candidate.Type] = true
		byPath[candidate.Path] = true
	}
	resolvedExplicitOutputBase, err := filepath.EvalSymlinks(explicitOutputBase)
	if err != nil {
		t.Fatal(err)
	}

	for _, candidateType := range []string{"output_base", "repository_cache", "disk_cache"} {
		if !byType[candidateType] {
			t.Fatalf("expected %s candidate, got %#v", candidateType, candidates)
		}
	}
	if !byPath[resolvedExplicitOutputBase] {
		t.Fatalf("expected direct explicit output base %s, got %#v", explicitOutputBase, candidates)
	}
}

func TestDiscoverBazelCandidatesIncludesProcessOutputBases(t *testing.T) {
	root := t.TempDir()
	outputBase := filepath.Join(root, "process-ob")
	makeBazelOutputBase(t, outputBase)

	candidates := NewBazelPlugin().discoverCandidates(root, config.BazelConfig{}, []string{outputBase})
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1: %#v", len(candidates), candidates)
	}
	resolvedOutputBase, err := filepath.EvalSymlinks(outputBase)
	if err != nil {
		t.Fatal(err)
	}
	candidate := candidates[0]
	if candidate.Type != "output_base" || candidate.Path != resolvedOutputBase || !candidate.Active {
		t.Fatalf("unexpected process output-base candidate: %#v", candidate)
	}
	if candidate.Reason != "discovered from active Bazel process --output_base" {
		t.Fatalf("unexpected reason: %q", candidate.Reason)
	}
}

func TestBazelPlanTargetsProtectsRecentActiveAndWorkspaces(t *testing.T) {
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	cfg := config.BazelConfig{
		KeepRecentOutputBases:        1,
		StaleAfter:                   "14d",
		CriticalStaleAfter:           "3d",
		AllowDeleteActiveOutputBases: false,
	}
	candidates := []bazelCandidate{
		{
			Type:     "output_base",
			Name:     "old",
			Path:     "/tmp/old",
			ModTime:  now.Add(-30 * 24 * time.Hour),
			Logical:  11,
			Physical: 10,
		},
		{
			Type:     "output_base",
			Name:     "new",
			Path:     "/tmp/new",
			ModTime:  now.Add(-1 * 24 * time.Hour),
			Physical: 20,
		},
		{
			Type:     "output_base",
			Name:     "active",
			Path:     "/tmp/active",
			ModTime:  now.Add(-40 * 24 * time.Hour),
			Physical: 30,
			Active:   true,
		},
		{
			Type:      "output_base",
			Name:      "protected",
			Path:      "/tmp/protected",
			ModTime:   now.Add(-50 * 24 * time.Hour),
			Physical:  40,
			Protected: true,
			Reason:    "reachable from configured protected workspace",
		},
	}

	targets, total := bazelPlanTargets(candidates, cfg, LevelModerate, now, false)
	if total != 100 {
		t.Fatalf("total physical = %d, want 100", total)
	}

	actions := map[string]CleanupTarget{}
	for _, target := range targets {
		actions[target.Name] = target
	}

	if actions["old"].Action != "delete_output_base" {
		t.Fatalf("old action = %q, want delete_output_base", actions["old"].Action)
	}
	if actions["old"].Tier != CleanupTierWarm || actions["old"].Reclaim != CleanupReclaimHost {
		t.Fatalf("old target policy = tier %q reclaim %q, want warm/host", actions["old"].Tier, actions["old"].Reclaim)
	}
	if actions["old"].HostReclaimsSpace == nil || !*actions["old"].HostReclaimsSpace {
		t.Fatalf("old target should be marked as host-space reclaiming: %#v", actions["old"])
	}
	if actions["old"].LogicalBytes != 11 {
		t.Fatalf("old logical bytes = %d, want 11", actions["old"].LogicalBytes)
	}
	for _, name := range []string{"new", "active", "protected"} {
		if actions[name].Action != "keep" || !actions[name].Protected {
			t.Fatalf("%s target not protected as expected: %#v", name, actions[name])
		}
	}
}

func TestBazelPlanTargetsDeletesCacheTiersOnlyWhenBudgetExceeded(t *testing.T) {
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	cfg := config.BazelConfig{
		MaxTotalGB:           1,
		StaleAfter:           "14d",
		CriticalStaleAfter:   "3d",
		AllowStopIdleServers: true,
	}
	candidates := []bazelCandidate{
		{
			Type:     "repository_cache",
			Name:     "repository_cache",
			Path:     "/tmp/repository_cache",
			ModTime:  now.Add(-30 * 24 * time.Hour),
			Physical: 2 * bazelGiB,
		},
		{
			Type:     "disk_cache",
			Name:     "disk_cache",
			Path:     "/tmp/disk_cache",
			ModTime:  now.Add(-30 * 24 * time.Hour),
			Physical: 2 * bazelGiB,
		},
		{
			Type:     "bazelisk",
			Name:     "sha256/hash",
			Path:     "/tmp/bazelisk/downloads/sha256/hash",
			ModTime:  now.Add(-30 * 24 * time.Hour),
			Physical: 2 * bazelGiB,
		},
		{
			Type:     "repository_cache",
			Name:     "fresh_repository_cache",
			Path:     "/tmp/fresh_repository_cache",
			ModTime:  now.Add(-1 * 24 * time.Hour),
			Physical: 2 * bazelGiB,
		},
	}

	targets, total := bazelPlanTargets(candidates, cfg, LevelModerate, now, false)
	if total != 8*bazelGiB {
		t.Fatalf("total physical = %d, want %d", total, 8*bazelGiB)
	}

	actions := map[string]CleanupTarget{}
	for _, target := range targets {
		actions[target.Name] = target
	}

	for _, name := range []string{"repository_cache", "disk_cache", "sha256/hash"} {
		if actions[name].Action != "delete_cache_tier" || actions[name].Protected {
			t.Fatalf("%s target should be a cache-tier deletion candidate: %#v", name, actions[name])
		}
	}
	if actions["fresh_repository_cache"].Action != "keep" || !actions["fresh_repository_cache"].Protected {
		t.Fatalf("fresh cache tier should be kept: %#v", actions["fresh_repository_cache"])
	}

	cfg.MaxTotalGB = 20
	targets, _ = bazelPlanTargets(candidates[:1], cfg, LevelModerate, now, false)
	if len(targets) != 1 {
		t.Fatalf("expected one target, got %d", len(targets))
	}
	if targets[0].Action != "review_cache_budget" {
		t.Fatalf("within-budget cache tier action = %q, want review_cache_budget", targets[0].Action)
	}
}

func TestBazelPlanTargetsProtectsCacheTiersWhenBazelActive(t *testing.T) {
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	cfg := config.BazelConfig{
		MaxTotalGB:                   1,
		StaleAfter:                   "14d",
		AllowDeleteActiveOutputBases: false,
		KeepRecentOutputBases:        0,
		CriticalStaleAfter:           "3d",
	}
	candidates := []bazelCandidate{
		{
			Type:     "disk_cache",
			Name:     "disk_cache",
			Path:     "/tmp/disk_cache",
			ModTime:  now.Add(-30 * 24 * time.Hour),
			Physical: 2 * bazelGiB,
		},
	}

	targets, _ := bazelPlanTargets(candidates, cfg, LevelModerate, now, true)
	if len(targets) != 1 {
		t.Fatalf("expected one target, got %d", len(targets))
	}
	if targets[0].Action != "keep" || !targets[0].Protected || !targets[0].Active {
		t.Fatalf("active cache tier should be protected: %#v", targets[0])
	}
}

func TestDiscoverBazeliskCandidatesPrefersSha256Downloads(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bazelisk")
	sha := filepath.Join(root, "downloads", "sha256", "abc123")
	metadata := filepath.Join(root, "downloads", "metadata", "bazelbuild")
	mustMkdir(t, sha)
	mustMkdir(t, metadata)

	candidates := discoverBazeliskCandidates(root)
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1: %#v", len(candidates), candidates)
	}
	if candidates[0].Type != "bazelisk" || candidates[0].Name != filepath.Join("sha256", "abc123") {
		t.Fatalf("unexpected Bazelisk candidate: %#v", candidates[0])
	}
	resolvedSha, err := filepath.EvalSymlinks(sha)
	if err != nil {
		t.Fatal(err)
	}
	if candidates[0].Path != resolvedSha {
		t.Fatalf("candidate path = %q, want %q", candidates[0].Path, resolvedSha)
	}
}

func TestOutputBasesProtectedByWorkspaces(t *testing.T) {
	root := t.TempDir()
	outputBase := filepath.Join(root, "_bazel_jess", "abc123")
	execrootBin := filepath.Join(outputBase, "execroot", "_main", "bazel-out", "darwin-fastbuild", "bin")
	mustMkdir(t, execrootBin)

	workspace := filepath.Join(root, "workspace")
	mustMkdir(t, workspace)
	if err := os.Symlink(execrootBin, filepath.Join(workspace, "bazel-bin")); err != nil {
		t.Fatal(err)
	}

	protected := outputBasesProtectedByWorkspaces([]string{workspace}, root)
	resolvedOutputBase, err := filepath.EvalSymlinks(outputBase)
	if err != nil {
		t.Fatal(err)
	}
	if !protected[resolvedOutputBase] {
		t.Fatalf("expected %s to be protected, got %#v", resolvedOutputBase, protected)
	}
}

func TestBazelBusyProcessReasons(t *testing.T) {
	ps := `
/nix/store/abc/bin/bazel bazel build //...
/Users/test/bin/bazelisk bazelisk test //...
/usr/bin/bazel bazel mod graph --registry=file:///tmp/registry
/usr/bin/zsh zsh -lc echo bazel query docs
`
	reasons := bazelBusyProcessReasons(ps)
	want := []string{"bazel build", "bazel mod", "bazelisk test"}

	if len(reasons) != len(want) {
		t.Fatalf("got %v, want %v", reasons, want)
	}
	for i := range want {
		if reasons[i] != want[i] {
			t.Fatalf("got %v, want %v", reasons, want)
		}
	}
}

func TestBazelBusyProcessInfoExtractsOutputBases(t *testing.T) {
	ps := `
bazel(workspace) bazel(workspace) --output_base=/private/tmp/workspace-ob --workspace_directory=/Users/test/workspace
/nix/store/abc/bin/bazel bazel test //... --output_base /private/var/tmp/_bazel_test/hash
/usr/bin/zsh zsh -lc echo --output_base=/not/bazel
`
	info := bazelBusyProcessInfo(ps)
	want := []string{"/private/tmp/workspace-ob", "/private/var/tmp/_bazel_test/hash"}
	if len(info.OutputBases) != len(want) {
		t.Fatalf("got output bases %v, want %v", info.OutputBases, want)
	}
	for i := range want {
		if info.OutputBases[i] != want[i] {
			t.Fatalf("got output bases %v, want %v", info.OutputBases, want)
		}
	}
}

func TestApplyBazelCleanupTargetsDeletesEligibleOutputBase(t *testing.T) {
	root := t.TempDir()
	outputBase := filepath.Join(root, "_bazel_jess", "stale")
	makeBazelOutputBase(t, outputBase)
	generated := filepath.Join(outputBase, "execroot", "_main", "bazel-out", "bin", "artifact")
	if err := os.MkdirAll(filepath.Dir(generated), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(generated, []byte("artifact"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(generated), 0500); err != nil {
		t.Fatal(err)
	}
	workspaceRoot := filepath.Join(root, "workspaces")
	workspace := filepath.Join(workspaceRoot, "project")
	mustMkdir(t, workspace)
	if err := os.Symlink(filepath.Dir(generated), filepath.Join(workspace, "bazel-bin")); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	result := applyBazelCleanupTargets(context.Background(), "bazel", LevelModerate, []CleanupTarget{
		{
			Type:   "output_base",
			Name:   "stale",
			Path:   outputBase,
			Bytes:  123,
			Action: "delete_output_base",
		},
	}, []string{workspaceRoot}, root, logger)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.ItemsCleaned != 1 {
		t.Fatalf("items cleaned = %d, want 1", result.ItemsCleaned)
	}
	if result.EstimatedBytesFreed != 123 || result.BytesFreed != 123 {
		t.Fatalf("unexpected bytes: estimated=%d legacy=%d", result.EstimatedBytesFreed, result.BytesFreed)
	}
	if _, err := os.Stat(outputBase); !os.IsNotExist(err) {
		t.Fatalf("expected output base to be deleted, stat err=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(workspace, "bazel-bin")); !os.IsNotExist(err) {
		t.Fatalf("expected repo-local bazel-bin symlink to be removed, stat err=%v", err)
	}
}

func TestCleanupRepoLocalBazelSymlinksOnlyRemovesLinksIntoDeletedOutputBase(t *testing.T) {
	root := t.TempDir()
	deletedOutputBase := filepath.Join(root, "_bazel_jess", "deleted")
	keptOutputBase := filepath.Join(root, "_bazel_jess", "kept")
	makeBazelOutputBase(t, deletedOutputBase)
	makeBazelOutputBase(t, keptOutputBase)

	workspaceRoot := filepath.Join(root, "workspaces")
	deletedWorkspace := filepath.Join(workspaceRoot, "deleted-workspace")
	keptWorkspace := filepath.Join(workspaceRoot, "kept-workspace")
	mustMkdir(t, deletedWorkspace)
	mustMkdir(t, keptWorkspace)
	deletedTarget := filepath.Join(deletedOutputBase, "execroot", "_main", "bazel-out")
	keptTarget := filepath.Join(keptOutputBase, "execroot", "_main", "bazel-out")
	if err := os.Symlink(deletedTarget, filepath.Join(deletedWorkspace, "bazel-out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(keptTarget, filepath.Join(keptWorkspace, "bazel-out")); err != nil {
		t.Fatal(err)
	}

	removed := cleanupRepoLocalBazelSymlinksForDeletedOutputBase([]string{workspaceRoot}, root, deletedOutputBase, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if removed != 1 {
		t.Fatalf("removed links = %d, want 1", removed)
	}
	if _, err := os.Lstat(filepath.Join(deletedWorkspace, "bazel-out")); !os.IsNotExist(err) {
		t.Fatalf("expected deleted output-base symlink to be removed, stat err=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(keptWorkspace, "bazel-out")); err != nil {
		t.Fatalf("expected unrelated Bazel symlink to remain, stat err=%v", err)
	}
}

func TestApplyBazelCleanupTargetsDeletesEligibleCacheTier(t *testing.T) {
	root := t.TempDir()
	repositoryCache := filepath.Join(root, "repository_cache")
	mustMkdir(t, repositoryCache)
	if err := os.WriteFile(filepath.Join(repositoryCache, "artifact"), []byte("artifact"), 0400); err != nil {
		t.Fatal(err)
	}

	bazeliskEntry := filepath.Join(root, "bazelisk", "downloads", "sha256", "abc123")
	mustMkdir(t, bazeliskEntry)
	if err := os.WriteFile(filepath.Join(bazeliskEntry, "bazel"), []byte("binary"), 0400); err != nil {
		t.Fatal(err)
	}
	customBazeliskEntry := filepath.Join(root, "custom-bazelisk-home", "downloads", "sha256", "def456")
	mustMkdir(t, customBazeliskEntry)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	result := applyBazelCleanupTargets(context.Background(), "bazel", LevelModerate, []CleanupTarget{
		{
			Type:   "repository_cache",
			Name:   "repository_cache",
			Path:   repositoryCache,
			Bytes:  11,
			Action: "delete_cache_tier",
		},
		{
			Type:   "bazelisk",
			Name:   "sha256/abc123",
			Path:   bazeliskEntry,
			Bytes:  13,
			Action: "delete_cache_tier",
		},
		{
			Type:   "bazelisk",
			Name:   "sha256/def456",
			Path:   customBazeliskEntry,
			Bytes:  17,
			Action: "delete_cache_tier",
		},
	}, nil, root, logger)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.ItemsCleaned != 3 {
		t.Fatalf("items cleaned = %d, want 3", result.ItemsCleaned)
	}
	if result.EstimatedBytesFreed != 41 || result.BytesFreed != 41 {
		t.Fatalf("unexpected bytes: estimated=%d legacy=%d", result.EstimatedBytesFreed, result.BytesFreed)
	}
	for _, path := range []string{repositoryCache, bazeliskEntry, customBazeliskEntry} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be deleted, stat err=%v", path, err)
		}
	}
}

func TestApplyBazelCleanupTargetsRejectsUnsafeCacheTierPath(t *testing.T) {
	root := t.TempDir()
	unsafe := filepath.Join(root, "not_repository_cache")
	mustMkdir(t, unsafe)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	result := applyBazelCleanupTargets(context.Background(), "bazel", LevelModerate, []CleanupTarget{
		{
			Type:   "repository_cache",
			Name:   "unsafe",
			Path:   unsafe,
			Bytes:  10,
			Action: "delete_cache_tier",
		},
	}, nil, root, logger)

	if result.ItemsCleaned != 0 {
		t.Fatalf("items cleaned = %d, want 0", result.ItemsCleaned)
	}
	if _, err := os.Stat(unsafe); err != nil {
		t.Fatalf("unsafe path should remain, err=%v", err)
	}
}

func TestApplyBazelCleanupTargetsSkipsProtectedAndActiveTargets(t *testing.T) {
	root := t.TempDir()
	active := filepath.Join(root, "_bazel_jess", "active")
	protected := filepath.Join(root, "_bazel_jess", "protected")
	makeBazelOutputBase(t, active)
	makeBazelOutputBase(t, protected)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	result := applyBazelCleanupTargets(context.Background(), "bazel", LevelModerate, []CleanupTarget{
		{
			Type:   "output_base",
			Name:   "active",
			Path:   active,
			Bytes:  10,
			Active: true,
			Action: "delete_output_base",
		},
		{
			Type:      "output_base",
			Name:      "protected",
			Path:      protected,
			Bytes:     20,
			Protected: true,
			Action:    "delete_output_base",
		},
	}, nil, root, logger)

	if result.ItemsCleaned != 0 {
		t.Fatalf("items cleaned = %d, want 0", result.ItemsCleaned)
	}
	for _, path := range []string{active, protected} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected %s to remain, err=%v", path, err)
		}
	}
}

func makeBazelOutputBase(t *testing.T, path string) {
	t.Helper()
	for _, name := range []string{"execroot", "action_cache", "server"} {
		mustMkdir(t, filepath.Join(path, name))
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
}
