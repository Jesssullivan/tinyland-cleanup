package plugins

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

func TestDiscoverBazelRootCandidates(t *testing.T) {
	root := t.TempDir()
	outputBase := filepath.Join(root, "_bazel_jess", "abc123")
	makeBazelOutputBase(t, outputBase)
	partialOutputBase := filepath.Join(root, "_bazel_jess", "partial123")
	mustMkdir(t, filepath.Join(partialOutputBase, "execroot", "repo"))
	repoCache := filepath.Join(root, "_bazel_jess", "cache", "repos", "v1")
	mustMkdir(t, filepath.Join(repoCache, "content_addressable"))
	explicitOutputBase := filepath.Join(root, "tinyvectors-external-smoke-ob")
	makeBazelOutputBase(t, explicitOutputBase)
	mustMkdir(t, filepath.Join(root, "repository_cache"))
	mustMkdir(t, filepath.Join(root, "disk_cache"))
	mustMkdir(t, filepath.Join(root, "bazel-tinyland", "cas", "aa"))

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
	resolvedPartialOutputBase, err := filepath.EvalSymlinks(partialOutputBase)
	if err != nil {
		t.Fatal(err)
	}
	resolvedRepoCache, err := filepath.EvalSymlinks(repoCache)
	if err != nil {
		t.Fatal(err)
	}

	for _, candidateType := range []string{"output_base", "partial_output_base", "repository_cache", "disk_cache", "remote_cache"} {
		if !byType[candidateType] {
			t.Fatalf("expected %s candidate, got %#v", candidateType, candidates)
		}
	}
	if !byPath[resolvedExplicitOutputBase] {
		t.Fatalf("expected direct explicit output base %s, got %#v", explicitOutputBase, candidates)
	}
	if !byPath[resolvedPartialOutputBase] {
		t.Fatalf("expected partial output base %s, got %#v", partialOutputBase, candidates)
	}
	if !byPath[resolvedRepoCache] {
		t.Fatalf("expected output-user-root repository cache %s, got %#v", repoCache, candidates)
	}
}

func TestDiscoverBazelCandidatesIncludesProcessOutputBases(t *testing.T) {
	root := t.TempDir()
	outputBase := filepath.Join(root, "process-ob")
	makeBazelOutputBase(t, outputBase)

	candidates := NewBazelPlugin().discoverCandidates(root, config.BazelConfig{}, bazelProcessInfo{
		OutputBases:       []string{outputBase},
		ClientOutputBases: []string{outputBase},
	})
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1: %#v", len(candidates), candidates)
	}
	resolvedOutputBase, err := filepath.EvalSymlinks(outputBase)
	if err != nil {
		t.Fatal(err)
	}
	candidate := candidates[0]
	if candidate.Type != "output_base" || candidate.Path != resolvedOutputBase || !candidate.Active || !candidate.ActiveClient {
		t.Fatalf("unexpected process output-base candidate: %#v", candidate)
	}
	if candidate.Reason != "discovered from active Bazel client --output_base" {
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

func TestRefineBazelOutputBaseCandidatesUsesRecursiveAllocation(t *testing.T) {
	outputBase := filepath.Join(t.TempDir(), "output-base")
	makeBazelOutputBase(t, outputBase)
	nested := filepath.Join(outputBase, "execroot", "repo", "bazel-out", "darwin-fastbuild", "bin")
	mustMkdir(t, nested)
	payload := filepath.Join(nested, "large.o")
	if err := os.WriteFile(payload, bytesOfSize(2*1024*1024), 0644); err != nil {
		t.Fatal(err)
	}

	candidates := []bazelCandidate{
		{
			Type:     "output_base",
			Name:     "output-base",
			Path:     outputBase,
			Physical: 1,
			Logical:  1,
		},
		{
			Type:      "output_base",
			Name:      "protected",
			Path:      outputBase,
			Physical:  1,
			Logical:   1,
			Protected: true,
		},
	}

	refined, changed, err := refineBazelOutputBaseCandidateBytes(context.Background(), candidates)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected output-base candidate bytes to be refined")
	}
	if refined[0].Physical < 2*1024*1024 {
		t.Fatalf("delete candidate physical bytes = %d, want at least payload size", refined[0].Physical)
	}
	if refined[0].Logical < 2*1024*1024 {
		t.Fatalf("delete candidate logical bytes = %d, want at least payload size", refined[0].Logical)
	}
	if refined[1].Physical < 2*1024*1024 {
		t.Fatalf("protected candidate should also be recursively measured: %#v", refined[1])
	}
}

func TestBazelPlanRefinesProtectedOutputBaseBytes(t *testing.T) {
	outputUserRoot := filepath.Join(t.TempDir(), "_bazel_jess")
	outputBase := filepath.Join(outputUserRoot, "recent")
	makeBazelOutputBase(t, outputBase)
	nested := filepath.Join(outputBase, "execroot", "repo", "bazel-out", "darwin-fastbuild", "bin")
	mustMkdir(t, nested)
	payload := filepath.Join(nested, "large.o")
	if err := os.WriteFile(payload, bytesOfSize(2*1024*1024), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultConfig()
	cfg.Bazel.Roots = []string{outputUserRoot}
	cfg.Bazel.KeepRecentOutputBases = 1
	cfg.Bazel.BazeliskCache = ""
	resolvedOutputBase, err := filepath.EvalSymlinks(outputBase)
	if err != nil {
		t.Fatal(err)
	}

	plan := NewBazelPlugin().PlanCleanup(context.Background(), LevelCritical, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var target CleanupTarget
	found := false
	for _, candidate := range plan.Targets {
		if candidate.Path == resolvedOutputBase {
			target = candidate
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected target for %s, got %#v", resolvedOutputBase, plan.Targets)
	}
	if !target.Protected || target.Action != "keep" {
		t.Fatalf("recent output base should remain protected: %#v", target)
	}
	if target.Bytes < 2*1024*1024 {
		t.Fatalf("protected target bytes = %d, want at least payload size", target.Bytes)
	}
	if plan.EstimatedBytesFreed != 0 {
		t.Fatalf("protected output base should not contribute reclaim estimate, got %d", plan.EstimatedBytesFreed)
	}
	if plan.Metadata["output_base_size_estimate_refined"] != "true" {
		t.Fatalf("missing refinement metadata: %#v", plan.Metadata)
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
			Type:     "remote_cache",
			Name:     "bazel-tinyland",
			Path:     "/tmp/.cache/bazel-tinyland",
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
	if total != 10*bazelGiB {
		t.Fatalf("total physical = %d, want %d", total, 10*bazelGiB)
	}

	actions := map[string]CleanupTarget{}
	for _, target := range targets {
		actions[target.Name] = target
	}

	for _, name := range []string{"repository_cache", "disk_cache", "sha256/hash", "bazel-tinyland"} {
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

func TestBazelPlanTargetsDeletesStalePartialOutputBase(t *testing.T) {
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	cfg := config.BazelConfig{
		KeepRecentOutputBases: 0,
		StaleAfter:            "14d",
		CriticalStaleAfter:    "3d",
	}
	candidates := []bazelCandidate{
		{
			Type:     "partial_output_base",
			Name:     "partial",
			Path:     "/tmp/partial",
			ModTime:  now.Add(-30 * 24 * time.Hour),
			Physical: 10,
		},
	}

	targets, total := bazelPlanTargets(candidates, cfg, LevelModerate, now, false)
	if total != 10 {
		t.Fatalf("total physical = %d, want 10", total)
	}
	if len(targets) != 1 {
		t.Fatalf("expected one target, got %d", len(targets))
	}
	if targets[0].Action != "delete_partial_output_base" || targets[0].Protected {
		t.Fatalf("partial output base should be an eligible delete target: %#v", targets[0])
	}
	if targets[0].Tier != CleanupTierWarm || targets[0].Reclaim != CleanupReclaimHost {
		t.Fatalf("partial output base policy = tier %q reclaim %q, want warm/host", targets[0].Tier, targets[0].Reclaim)
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

func TestBazelPlanTargetsDoesNotUseIdleServerAsGlobalCacheActivity(t *testing.T) {
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	cfg := config.BazelConfig{
		MaxTotalGB:                   1,
		StaleAfter:                   "14d",
		CriticalStaleAfter:           "3d",
		AllowDeleteActiveOutputBases: false,
		KeepRecentOutputBases:        0,
	}
	candidates := []bazelCandidate{
		{
			Type:     "output_base",
			Name:     "idle-server-output-base",
			Path:     "/tmp/idle-server-output-base",
			ModTime:  now.Add(-30 * 24 * time.Hour),
			Physical: 2 * bazelGiB,
			Active:   true,
		},
		{
			Type:     "disk_cache",
			Name:     "disk_cache",
			Path:     "/tmp/disk_cache",
			ModTime:  now.Add(-30 * 24 * time.Hour),
			Physical: 2 * bazelGiB,
		},
	}

	targets, _ := bazelPlanTargets(candidates, cfg, LevelModerate, now, false)
	actions := map[string]CleanupTarget{}
	for _, target := range targets {
		actions[target.Name] = target
	}

	if actions["idle-server-output-base"].Action != "keep" || !actions["idle-server-output-base"].Protected {
		t.Fatalf("idle server output base should be protected: %#v", actions["idle-server-output-base"])
	}
	if actions["disk_cache"].Action != "delete_cache_tier" || actions["disk_cache"].Protected || actions["disk_cache"].Active {
		t.Fatalf("idle server output base should not globally protect cache tiers: %#v", actions["disk_cache"])
	}
}

func TestBazelPlanTargetsDoesNotUseActiveClientAsGlobalOutputBaseActivity(t *testing.T) {
	now := time.Date(2026, 5, 6, 18, 0, 0, 0, time.UTC)
	cfg := config.BazelConfig{
		MaxTotalGB:                   1,
		StaleAfter:                   "14d",
		CriticalStaleAfter:           "3d",
		AllowDeleteActiveOutputBases: false,
		KeepRecentOutputBases:        0,
	}
	candidates := []bazelCandidate{
		{
			Type:         "output_base",
			Name:         "active-client-output-base",
			Path:         "/private/var/tmp/_bazel_jess/active-client",
			ModTime:      now.Add(-1 * time.Hour),
			Physical:     2 * bazelGiB,
			Active:       true,
			ActiveClient: true,
			Reason:       "discovered from active Bazel client --output_base",
		},
		{
			Type:     "output_base",
			Name:     "stale-inactive-output-base",
			Path:     "/private/var/tmp/_bazel_jess/stale-inactive",
			ModTime:  now.Add(-30 * 24 * time.Hour),
			Physical: 4 * bazelGiB,
		},
		{
			Type:     "disk_cache",
			Name:     "disk_cache",
			Path:     "/private/var/tmp/_bazel_jess/disk_cache",
			ModTime:  now.Add(-30 * 24 * time.Hour),
			Physical: 2 * bazelGiB,
		},
	}

	targets, _ := bazelPlanTargets(candidates, cfg, LevelCritical, now, true)
	actions := map[string]CleanupTarget{}
	for _, target := range targets {
		actions[target.Name] = target
	}

	if actions["active-client-output-base"].Action != "keep" || !actions["active-client-output-base"].Protected || !actions["active-client-output-base"].Active {
		t.Fatalf("active client output base should remain protected: %#v", actions["active-client-output-base"])
	}
	if actions["stale-inactive-output-base"].Action != "delete_output_base" || actions["stale-inactive-output-base"].Protected || actions["stale-inactive-output-base"].Active {
		t.Fatalf("unrelated stale output base should remain eligible despite active Bazel client elsewhere: %#v", actions["stale-inactive-output-base"])
	}
	if actions["disk_cache"].Action != "keep" || !actions["disk_cache"].Protected || !actions["disk_cache"].Active {
		t.Fatalf("shared cache tier should stay protected while Bazel client work is active: %#v", actions["disk_cache"])
	}
}

func TestBazelPlanTargetsCanStopIdleServerBeforeDeletingStaleOutputBase(t *testing.T) {
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	cfg := config.BazelConfig{
		StaleAfter:                   "14d",
		CriticalStaleAfter:           "3d",
		AllowStopIdleServers:         true,
		AllowDeleteActiveOutputBases: false,
		KeepRecentOutputBases:        0,
	}
	candidates := []bazelCandidate{
		{
			Type:          "output_base",
			Name:          "idle-server-output-base",
			Path:          "/tmp/idle-server-output-base",
			ModTime:       now.Add(-30 * 24 * time.Hour),
			Physical:      2 * bazelGiB,
			Active:        true,
			IdleServer:    true,
			ProcessServer: true,
		},
	}

	targets, _ := bazelPlanTargets(candidates, cfg, LevelAggressive, now, false)
	if len(targets) != 1 {
		t.Fatalf("expected one target, got %d", len(targets))
	}
	target := targets[0]
	if target.Action != "stop_idle_server_then_delete_output_base" || target.Protected || !target.Active {
		t.Fatalf("idle server output base should be a guarded shutdown/delete candidate: %#v", target)
	}
	if target.Reclaim != CleanupReclaimHost || target.HostReclaimsSpace == nil || !*target.HostReclaimsSpace {
		t.Fatalf("idle server shutdown/delete should advertise host reclaim: %#v", target)
	}
	if got := bazelEstimatedCandidateBytes(targets); got != 2*bazelGiB {
		t.Fatalf("estimated reclaim = %d, want %d", got, 2*bazelGiB)
	}

	targets, _ = bazelPlanTargets(candidates, cfg, LevelModerate, now, false)
	if targets[0].Action != "keep" || !targets[0].Protected {
		t.Fatalf("moderate level should not stop idle Bazel servers: %#v", targets[0])
	}

	cfg.AllowStopIdleServers = false
	targets, _ = bazelPlanTargets(candidates, cfg, LevelAggressive, now, false)
	if targets[0].Action != "keep" || !targets[0].Protected {
		t.Fatalf("disabled idle-server stop should keep target protected: %#v", targets[0])
	}

	cfg.AllowStopIdleServers = true
	candidates[0].ProcessServer = false
	targets, _ = bazelPlanTargets(candidates, cfg, LevelAggressive, now, false)
	if targets[0].Action != "keep" || !targets[0].Protected {
		t.Fatalf("pid-only server evidence should keep target protected: %#v", targets[0])
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
	if len(info.ClientOutputBases) != 1 || info.ClientOutputBases[0] != "/private/var/tmp/_bazel_test/hash" {
		t.Fatalf("unexpected client output bases: %v", info.ClientOutputBases)
	}
	if len(info.ServerOutputBases) != 1 || info.ServerOutputBases[0] != "/private/tmp/workspace-ob" {
		t.Fatalf("unexpected server output bases: %v", info.ServerOutputBases)
	}
}

func TestBazelBusyProcessInfoSeparatesServerOutputBasesFromClientCommands(t *testing.T) {
	ps := `
bazel(workspace) bazel(workspace) --output_base=/private/tmp/workspace-ob --workspace_directory=/Users/test/workspace
`
	info := bazelBusyProcessInfo(ps)
	if len(info.Reasons) != 0 {
		t.Fatalf("server process should not be reported as active client work: %v", info.Reasons)
	}
	if len(info.OutputBases) != 1 || info.OutputBases[0] != "/private/tmp/workspace-ob" {
		t.Fatalf("unexpected output bases: %v", info.OutputBases)
	}
	if len(info.ClientOutputBases) != 0 {
		t.Fatalf("server process should not report client output bases: %v", info.ClientOutputBases)
	}
	if len(info.ServerOutputBases) != 1 || info.ServerOutputBases[0] != "/private/tmp/workspace-ob" {
		t.Fatalf("unexpected server output bases: %v", info.ServerOutputBases)
	}
}

func TestBazelServerPIDsForOutputBaseFromPSSkipsActiveClients(t *testing.T) {
	ps := `
18473 bazel(workspace) bazel(workspace) --output_base=/private/tmp/workspace-ob --workspace_directory=/Users/test/workspace
777 /nix/store/abc/bin/bazel bazel test //... --output_base /private/tmp/workspace-ob
888 bazel(other) bazel(other) --output_base=/private/tmp/other-ob
`
	pids := bazelServerPIDsForOutputBaseFromPS(ps, "/private/tmp/workspace-ob")
	if len(pids) != 1 || pids[0] != 18473 {
		t.Fatalf("got pids %v, want [18473]", pids)
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

func TestApplyBazelCleanupTargetsStopsIdleServerBeforeDeletingOutputBase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake bazel shell script is Unix-only")
	}
	requireProcessTable(t)
	root := t.TempDir()
	outputBase := filepath.Join(root, "_bazel_jess", "stale-idle")
	makeBazelOutputBase(t, outputBase)
	if err := os.WriteFile(filepath.Join(outputBase, "lock"), []byte("shutdown-touched lock\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := exec.Command("sleep", "60")
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- server.Wait()
	}()
	defer func() {
		_ = server.Process.Kill()
		<-serverDone
	}()
	if err := os.WriteFile(filepath.Join(outputBase, "server", "server.pid"), []byte(strconv.Itoa(server.Process.Pid)), 0644); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(root, "bin")
	mustMkdir(t, binDir)
	bazelMarker := filepath.Join(root, "bazel-invoked")
	fakeBazel := filepath.Join(binDir, "bazel")
	if err := os.WriteFile(fakeBazel, []byte("#!/bin/sh\ntouch \"$BAZEL_INVOKED\"\nexit 42\n"), 0755); err != nil {
		t.Fatal(err)
	}
	fakeBazelisk := filepath.Join(binDir, "bazelisk")
	if err := os.WriteFile(fakeBazelisk, []byte("#!/bin/sh\ntouch \"$BAZEL_INVOKED\"\nexit 42\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BAZEL_INVOKED", bazelMarker)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	result := applyBazelCleanupTargets(context.Background(), "bazel", LevelAggressive, []CleanupTarget{
		{
			Type:   "output_base",
			Name:   "stale-idle",
			Path:   outputBase,
			Bytes:  123,
			Active: true,
			Action: "stop_idle_server_then_delete_output_base",
		},
	}, nil, root, logger)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.ItemsCleaned != 1 {
		t.Fatalf("items cleaned = %d, want 1", result.ItemsCleaned)
	}
	if _, err := os.Stat(outputBase); !os.IsNotExist(err) {
		t.Fatalf("expected output base to be deleted, stat err=%v", err)
	}
	if _, err := os.Stat(bazelMarker); !os.IsNotExist(err) {
		t.Fatalf("cleanup must not invoke bazel/bazelisk during idle-server stop, marker err=%v", err)
	}
}

func TestApplyBazelCleanupTargetsDeletesIdleOutputBaseWithoutPIDFile(t *testing.T) {
	requireProcessTable(t)
	root := t.TempDir()
	outputBase := filepath.Join(root, "_bazel_jess", "stale-idle-no-pid")
	makeBazelOutputBase(t, outputBase)
	if err := os.Remove(filepath.Join(outputBase, "server", "server.pid")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	result := applyBazelCleanupTargets(context.Background(), "bazel", LevelAggressive, []CleanupTarget{
		{
			Type:   "output_base",
			Name:   "stale-idle-no-pid",
			Path:   outputBase,
			Bytes:  123,
			Active: true,
			Action: "stop_idle_server_then_delete_output_base",
		},
	}, nil, root, logger)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.ItemsCleaned != 1 {
		t.Fatalf("items cleaned = %d, want 1", result.ItemsCleaned)
	}
	if _, err := os.Stat(outputBase); !os.IsNotExist(err) {
		t.Fatalf("expected output base to be deleted, stat err=%v", err)
	}
}

func requireProcessTable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("process-table integration test requires ps")
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

func TestDiscoverBazelRootCandidatesReportsServerLogs(t *testing.T) {
	root := t.TempDir()
	outputBase := filepath.Join(root, "_bazel_jess", "loggy")
	makeBazelOutputBase(t, outputBase)
	logPath := filepath.Join(outputBase, "java.log.neo.jess.log.java.20260702")
	if err := os.WriteFile(logPath, []byte("client env"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputBase, "not-a-bazel-log.txt"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	resolvedLogPath, err := filepath.EvalSymlinks(logPath)
	if err != nil {
		t.Fatal(err)
	}

	candidates := discoverBazelRootCandidates(outputBase)
	var found bool
	for _, candidate := range candidates {
		if candidate.Type == "server_log" && candidate.Path == resolvedLogPath && candidate.Name == filepath.Base(logPath) {
			found = true
		}
		if candidate.Type == "server_log" && strings.Contains(candidate.Path, "not-a-bazel-log") {
			t.Fatalf("unexpected non-Bazel log candidate: %+v", candidate)
		}
	}
	if !found {
		t.Fatalf("did not discover Bazel server log candidate in %+v", candidates)
	}
}

func TestBazelPlanTargetsDeletesOnlyStaleInactiveServerLogs(t *testing.T) {
	now := time.Now()
	cfg := config.DefaultConfig().Bazel
	cfg.StaleAfter = "24h"

	targets, _ := bazelPlanTargets([]bazelCandidate{
		{
			Type:     "server_log",
			Name:     "java.log.old",
			Path:     "/tmp/output-base/java.log.old",
			ModTime:  now.Add(-48 * time.Hour),
			Physical: 11,
		},
		{
			Type:     "server_log",
			Name:     "java.log.new",
			Path:     "/tmp/output-base/java.log.new",
			ModTime:  now.Add(-1 * time.Hour),
			Physical: 13,
		},
		{
			Type:     "server_log",
			Name:     "java.log.active",
			Path:     "/tmp/output-base/java.log.active",
			ModTime:  now.Add(-48 * time.Hour),
			Physical: 17,
			Active:   true,
			Reason:   "active Bazel process or output-base lock detected",
		},
	}, cfg, LevelModerate, now, false)

	actions := map[string]CleanupTarget{}
	for _, target := range targets {
		actions[target.Name] = target
	}
	if got := actions["java.log.old"].Action; got != "delete_server_log" {
		t.Fatalf("old server log action = %q, want delete_server_log", got)
	}
	if got := actions["java.log.new"].Action; got != "keep" {
		t.Fatalf("new server log action = %q, want keep", got)
	}
	if got := actions["java.log.active"].Action; got != "keep" {
		t.Fatalf("active server log action = %q, want keep", got)
	}
}

func TestApplyBazelCleanupTargetsDeletesEligibleServerLog(t *testing.T) {
	root := t.TempDir()
	outputBase := filepath.Join(root, "_bazel_jess", "loggy")
	makeBazelOutputBase(t, outputBase)
	logPath := filepath.Join(outputBase, "command.log")
	if err := os.WriteFile(logPath, []byte("client env"), 0400); err != nil {
		t.Fatal(err)
	}
	unsafePath := filepath.Join(outputBase, "not-a-bazel-log.txt")
	if err := os.WriteFile(unsafePath, []byte("keep"), 0400); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	result := applyBazelCleanupTargets(context.Background(), "bazel", LevelModerate, []CleanupTarget{
		{
			Type:   "server_log",
			Name:   "command.log",
			Path:   logPath,
			Bytes:  10,
			Action: "delete_server_log",
		},
		{
			Type:   "server_log",
			Name:   "not-a-bazel-log.txt",
			Path:   unsafePath,
			Bytes:  20,
			Action: "delete_server_log",
		},
	}, nil, root, logger)

	if result.ItemsCleaned != 1 {
		t.Fatalf("items cleaned = %d, want 1", result.ItemsCleaned)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("expected server log to be deleted, stat err=%v", err)
	}
	if _, err := os.Stat(unsafePath); err != nil {
		t.Fatalf("unsafe path should remain, err=%v", err)
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

func bytesOfSize(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	return data
}
