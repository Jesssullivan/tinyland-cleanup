//go:build linux

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

func TestGitHubRunnerTargetsUseInstances(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.GitHubRunner.Home = "/home/github-runner"
	cfg.GitHubRunner.WorkDir = "/home/github-runner/_work"
	cfg.GitHubRunner.Instances = []config.GitHubRunnerInstanceConfig{
		{
			Name: "primary",
			Home: "/runner/primary",
		},
		{
			Name:     "custom",
			Home:     "/runner/custom",
			WorkDir:  "/work/custom",
			CacheDir: "/cache/custom",
			TempDir:  "/tmp/custom",
		},
	}

	targets := NewGitHubRunnerPlugin().githubRunnerTargets(cfg)
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(targets))
	}
	if targets[0].workDir != "/runner/primary/_work" {
		t.Errorf("expected default work dir for primary, got %q", targets[0].workDir)
	}
	if targets[0].cacheDir != "/runner/primary/cache" {
		t.Errorf("expected default cache dir for primary, got %q", targets[0].cacheDir)
	}
	if targets[1].workDir != "/work/custom" {
		t.Errorf("expected custom work dir, got %q", targets[1].workDir)
	}
	if targets[1].tempDir != "/tmp/custom" {
		t.Errorf("expected custom temp dir, got %q", targets[1].tempDir)
	}
}

func TestGitHubRunnerCleanupCoversInstances(t *testing.T) {
	root := t.TempDir()
	runnerOne := filepath.Join(root, "honey")
	runnerTwo := filepath.Join(root, "honey-2")
	makeRunnerTree(t, runnerOne)
	makeRunnerTree(t, runnerTwo)

	cfg := config.DefaultConfig()
	cfg.GitHubRunner.Instances = []config.GitHubRunnerInstanceConfig{
		{Name: "honey", Home: runnerOne},
		{Name: "honey-2", Home: runnerTwo},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	result := NewGitHubRunnerPlugin().Cleanup(context.Background(), LevelModerate, cfg, logger)
	if result.BytesFreed <= 0 {
		t.Fatalf("expected cleanup to free bytes, got %d", result.BytesFreed)
	}

	for _, runner := range []string{runnerOne, runnerTwo} {
		if pathExists(filepath.Join(runner, "tmp", "old.tmp")) {
			t.Errorf("expected old temp file removed for %s", runner)
		}
		if pathExists(filepath.Join(runner, "cache", "old.cache")) {
			t.Errorf("expected old cache file removed for %s", runner)
		}
		if pathExists(filepath.Join(runner, "_work", "old-work")) {
			t.Errorf("expected old work directory removed for %s", runner)
		}
	}
}

func makeRunnerTree(t *testing.T, home string) {
	t.Helper()

	oldTime := time.Now().Add(-96 * time.Hour)
	for _, dir := range []string{
		filepath.Join(home, "tmp"),
		filepath.Join(home, "cache"),
		filepath.Join(home, "_work", "old-work"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	writeOldFile(t, filepath.Join(home, "tmp", "old.tmp"), oldTime)
	writeOldFile(t, filepath.Join(home, "cache", "old.cache"), oldTime)
	writeOldFile(t, filepath.Join(home, "_work", "old-work", "artifact"), oldTime)
	if err := os.Chtimes(filepath.Join(home, "_work", "old-work"), oldTime, oldTime); err != nil {
		t.Fatalf("chtimes old work dir: %v", err)
	}
}

func writeOldFile(t *testing.T, path string, modTime time.Time) {
	t.Helper()

	if err := os.WriteFile(path, []byte("old cleanup candidate"), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}
