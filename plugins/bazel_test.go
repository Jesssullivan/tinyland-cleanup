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
	mustMkdir(t, filepath.Join(root, "repository_cache"))
	mustMkdir(t, filepath.Join(root, "disk_cache"))

	candidates := discoverBazelRootCandidates(root)
	byType := map[string]bool{}
	for _, candidate := range candidates {
		byType[candidate.Type] = true
	}

	for _, candidateType := range []string{"output_base", "repository_cache", "disk_cache"} {
		if !byType[candidateType] {
			t.Fatalf("expected %s candidate, got %#v", candidateType, candidates)
		}
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
	for _, name := range []string{"new", "active", "protected"} {
		if actions[name].Action != "keep" || !actions[name].Protected {
			t.Fatalf("%s target not protected as expected: %#v", name, actions[name])
		}
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
/usr/bin/zsh zsh -lc echo bazel query docs
`
	reasons := bazelBusyProcessReasons(ps)
	want := []string{"bazel build", "bazelisk test"}

	if len(reasons) != len(want) {
		t.Fatalf("got %v, want %v", reasons, want)
	}
	for i := range want {
		if reasons[i] != want[i] {
			t.Fatalf("got %v, want %v", reasons, want)
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

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	result := applyBazelCleanupTargets(context.Background(), "bazel", LevelModerate, []CleanupTarget{
		{
			Type:   "output_base",
			Name:   "stale",
			Path:   outputBase,
			Bytes:  123,
			Action: "delete_output_base",
		},
	}, logger)

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
	}, logger)

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
