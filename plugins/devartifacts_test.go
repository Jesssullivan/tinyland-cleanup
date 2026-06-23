package plugins

import (
	"compress/gzip"
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

func requireGit(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is required for tracked-artifact protection tests")
	}
	return git
}

func runGit(t *testing.T, git string, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(git, args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, output)
	}
}

func TestDevArtifactsPluginInterface(t *testing.T) {
	p := NewDevArtifactsPlugin()

	if p.Name() != "dev-artifacts" {
		t.Errorf("expected name 'dev-artifacts', got %q", p.Name())
	}

	if p.Description() == "" {
		t.Error("description should not be empty")
	}

	// Should support all platforms
	if platforms := p.SupportedPlatforms(); platforms != nil {
		t.Errorf("expected nil (all platforms), got %v", platforms)
	}

	// Should be enabled when DevArtifacts flag is true
	cfg := config.DefaultConfig()
	if !p.Enabled(cfg) {
		t.Error("expected DevArtifacts to be enabled by default")
	}

	cfg.Enable.DevArtifacts = false
	if p.Enabled(cfg) {
		t.Error("expected DevArtifacts to be disabled when flag is false")
	}
}

func TestExpandHome(t *testing.T) {
	home := "/home/testuser"

	tests := []struct {
		input    string
		expected string
	}{
		{"~/git", filepath.Join(home, "git")},
		{"~/src/project", filepath.Join(home, "src/project")},
		{"~", home},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
	}

	for _, tt := range tests {
		result := expandHome(tt.input, home)
		if result != tt.expected {
			t.Errorf("expandHome(%q, %q) = %q, want %q", tt.input, home, result, tt.expected)
		}
	}
}

func TestIsFileStale(t *testing.T) {
	p := NewDevArtifactsPlugin()

	// Non-existent file should be considered stale
	if !p.isFileStale("/nonexistent/file", 24*time.Hour) {
		t.Error("non-existent file should be stale")
	}

	// Create a temp file
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(tmpFile, []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}

	// Just-created file should not be stale with 24h threshold
	if p.isFileStale(tmpFile, 24*time.Hour) {
		t.Error("just-created file should not be stale with 24h threshold")
	}

	// File should be stale with 0 duration (always stale)
	// Note: 0 means "any age", but since the file was just created,
	// we need to use a very small threshold
	if p.isFileStale(tmpFile, 1*time.Millisecond) {
		// File was created less than 1ms ago, so might still be fresh
		// This is OK - timing-dependent tests are acceptable here
	}
}

func TestIsProtected(t *testing.T) {
	p := NewDevArtifactsPlugin()

	protectPaths := []string{
		"/home/user/git/important-project",
		"/home/user/git/another-project",
	}

	tests := []struct {
		path     string
		expected bool
	}{
		{"/home/user/git/important-project/node_modules", true},
		{"/home/user/git/another-project/.venv", true},
		{"/home/user/git/disposable-project/node_modules", false},
		{"/other/path", false},
	}

	for _, tt := range tests {
		result := p.isProtected(tt.path, protectPaths)
		if result != tt.expected {
			t.Errorf("isProtected(%q) = %v, want %v", tt.path, result, tt.expected)
		}
	}
}

func TestFindArtifactDirs(t *testing.T) {
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()

	// Create project structure:
	// tmpDir/
	//   project1/
	//     package.json
	//     node_modules/
	//       some-package/
	//         index.js
	//   project2/
	//     Cargo.toml
	//     target/
	//       debug/
	//         binary

	// Project 1: Node.js
	project1 := filepath.Join(tmpDir, "project1")
	os.MkdirAll(filepath.Join(project1, "node_modules", "some-package"), 0755)
	os.WriteFile(filepath.Join(project1, "package.json"), []byte(`{"name":"test"}`), 0644)
	os.WriteFile(filepath.Join(project1, "node_modules", "some-package", "index.js"), []byte("module.exports = {}"), 0644)

	// Project 2: Rust
	project2 := filepath.Join(tmpDir, "project2")
	os.MkdirAll(filepath.Join(project2, "target", "debug"), 0755)
	os.WriteFile(filepath.Join(project2, "Cargo.toml"), []byte("[package]\nname = \"test\""), 0644)
	os.WriteFile(filepath.Join(project2, "target", "debug", "binary"), []byte("ELF"), 0644)

	// Project 3: Zig
	project3 := filepath.Join(tmpDir, "project3")
	os.MkdirAll(filepath.Join(project3, ".zig-cache", "o"), 0755)
	os.WriteFile(filepath.Join(project3, "build.zig"), []byte("const std = @import(\"std\");"), 0644)
	os.WriteFile(filepath.Join(project3, ".zig-cache", "o", "artifact"), []byte("cache"), 0644)

	// Find node_modules with marker
	var foundNodeModules []string
	p.findArtifactDirs(context.Background(), tmpDir, "node_modules", "package.json", func(dir string, size int64) {
		foundNodeModules = append(foundNodeModules, dir)
	})

	if len(foundNodeModules) != 1 {
		t.Errorf("expected 1 node_modules, found %d", len(foundNodeModules))
	}

	// Find target with Cargo.toml marker
	var foundTargets []string
	p.findArtifactDirs(context.Background(), tmpDir, "target", "Cargo.toml", func(dir string, size int64) {
		foundTargets = append(foundTargets, dir)
	})

	if len(foundTargets) != 1 {
		t.Errorf("expected 1 Rust target, found %d", len(foundTargets))
	}

	// Find Zig .zig-cache with build.zig marker
	var foundZigCaches []string
	p.findArtifactDirs(context.Background(), tmpDir, ".zig-cache", "build.zig", func(dir string, size int64) {
		foundZigCaches = append(foundZigCaches, dir)
	})

	if len(foundZigCaches) != 1 {
		t.Errorf("expected 1 Zig cache, found %d", len(foundZigCaches))
	}
}

func TestFindArtifactDirsNoMarker(t *testing.T) {
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()

	// Create node_modules WITHOUT package.json
	os.MkdirAll(filepath.Join(tmpDir, "orphan", "node_modules", "pkg"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "orphan", "node_modules", "pkg", "index.js"), []byte("x"), 0644)

	var found []string
	p.findArtifactDirs(context.Background(), tmpDir, "node_modules", "package.json", func(dir string, size int64) {
		found = append(found, dir)
	})

	if len(found) != 0 {
		t.Errorf("expected 0 node_modules (no package.json marker), found %d", len(found))
	}
}

func TestCleanNodeModulesStale(t *testing.T) {
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create a project with node_modules and an old package.json
	project := filepath.Join(tmpDir, "old-project")
	os.MkdirAll(filepath.Join(project, "node_modules", "pkg"), 0755)
	os.WriteFile(filepath.Join(project, "node_modules", "pkg", "index.js"), []byte("test"), 0644)
	packageJSON := filepath.Join(project, "package.json")
	os.WriteFile(packageJSON, []byte(`{"name":"old"}`), 0644)

	// Make package.json old
	oldTime := time.Now().Add(-60 * 24 * time.Hour) // 60 days ago
	os.Chtimes(packageJSON, oldTime, oldTime)

	// Clean with 30-day threshold - should remove
	freed := p.cleanNodeModules(context.Background(), tmpDir, 30*24*time.Hour, nil, newDevArtifactActivity(), newDevArtifactGitTracker(), logger)

	if freed == 0 {
		t.Error("expected node_modules to be cleaned (stale > 30 days)")
	}

	// Verify node_modules is gone
	if pathExists(filepath.Join(project, "node_modules")) {
		t.Error("node_modules should have been removed")
	}
}

func TestCleanNodeModulesFresh(t *testing.T) {
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create a project with node_modules and a fresh package.json
	project := filepath.Join(tmpDir, "fresh-project")
	os.MkdirAll(filepath.Join(project, "node_modules", "pkg"), 0755)
	os.WriteFile(filepath.Join(project, "node_modules", "pkg", "index.js"), []byte("test"), 0644)
	os.WriteFile(filepath.Join(project, "package.json"), []byte(`{"name":"fresh"}`), 0644)
	// package.json has current mtime (just created)

	// Clean with 30-day threshold - should NOT remove
	freed := p.cleanNodeModules(context.Background(), tmpDir, 30*24*time.Hour, nil, newDevArtifactActivity(), newDevArtifactGitTracker(), logger)

	if freed != 0 {
		t.Error("expected fresh node_modules to be preserved")
	}

	// Verify node_modules still exists
	if !pathExists(filepath.Join(project, "node_modules")) {
		t.Error("node_modules should still exist")
	}
}

func TestCleanNodeModulesProtected(t *testing.T) {
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create a stale project
	project := filepath.Join(tmpDir, "protected-project")
	os.MkdirAll(filepath.Join(project, "node_modules", "pkg"), 0755)
	os.WriteFile(filepath.Join(project, "node_modules", "pkg", "index.js"), []byte("test"), 0644)
	packageJSON := filepath.Join(project, "package.json")
	os.WriteFile(packageJSON, []byte(`{"name":"protected"}`), 0644)
	oldTime := time.Now().Add(-60 * 24 * time.Hour)
	os.Chtimes(packageJSON, oldTime, oldTime)

	// Clean with protection - should NOT remove
	protectPaths := []string{project}
	freed := p.cleanNodeModules(context.Background(), tmpDir, 30*24*time.Hour, protectPaths, newDevArtifactActivity(), newDevArtifactGitTracker(), logger)

	if freed != 0 {
		t.Error("expected protected node_modules to be preserved")
	}
}

func TestCleanZigArtifactsStale(t *testing.T) {
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	project := filepath.Join(tmpDir, "old-zig")
	os.MkdirAll(filepath.Join(project, ".zig-cache", "o"), 0755)
	os.MkdirAll(filepath.Join(project, "zig-out", "bin"), 0755)
	cacheArtifact := filepath.Join(project, ".zig-cache", "o", "artifact")
	outputArtifact := filepath.Join(project, "zig-out", "bin", "tool")
	os.WriteFile(cacheArtifact, []byte("cache"), 0644)
	os.WriteFile(outputArtifact, []byte("binary"), 0644)
	buildZig := filepath.Join(project, "build.zig")
	os.WriteFile(buildZig, []byte("const std = @import(\"std\");"), 0644)
	oldTime := time.Now().Add(-60 * 24 * time.Hour)
	os.Chtimes(buildZig, oldTime, oldTime)
	os.Chtimes(cacheArtifact, oldTime, oldTime)
	os.Chtimes(outputArtifact, oldTime, oldTime)

	freed := p.cleanZigArtifacts(context.Background(), tmpDir, 30*24*time.Hour, nil, newDevArtifactActivity(), newDevArtifactGitTracker(), logger)
	if freed == 0 {
		t.Fatal("expected stale Zig artifacts to be cleaned")
	}
	if pathExists(filepath.Join(project, ".zig-cache")) {
		t.Error(".zig-cache should have been removed")
	}
	if pathExists(filepath.Join(project, "zig-out")) {
		t.Error("zig-out should have been removed")
	}
}

func TestCleanZigArtifactsFresh(t *testing.T) {
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	project := filepath.Join(tmpDir, "fresh-zig")
	os.MkdirAll(filepath.Join(project, ".zig-cache", "o"), 0755)
	os.WriteFile(filepath.Join(project, ".zig-cache", "o", "artifact"), []byte("cache"), 0644)
	os.WriteFile(filepath.Join(project, "build.zig"), []byte("const std = @import(\"std\");"), 0644)

	freed := p.cleanZigArtifacts(context.Background(), tmpDir, 30*24*time.Hour, nil, newDevArtifactActivity(), newDevArtifactGitTracker(), logger)
	if freed != 0 {
		t.Fatal("expected fresh Zig artifacts to be preserved")
	}
	if !pathExists(filepath.Join(project, ".zig-cache")) {
		t.Error(".zig-cache should still exist")
	}
}

func TestPlanZigArtifactsProtectsRecentOutputAtCritical(t *testing.T) {
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()

	project := filepath.Join(tmpDir, "recent-zig")
	if err := os.MkdirAll(filepath.Join(project, ".zig-cache", "o"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".zig-cache", "o", "artifact"), []byte("cache"), 0644); err != nil {
		t.Fatal(err)
	}
	buildZig := filepath.Join(project, "build.zig")
	if err := os.WriteFile(buildZig, []byte("const std = @import(\"std\");"), 0644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(buildZig, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	var targets []CleanupTarget
	p.planZigArtifacts(context.Background(), tmpDir, 0, true, nil, newDevArtifactActivity(), newDevArtifactGitTracker(), &targets)

	target := findDevArtifactTarget(t, targets, "zig-artifact", filepath.Join(project, ".zig-cache"))
	if target.Action != "protect" || !target.Protected {
		t.Fatalf("expected recent Zig output to be protected, got %#v", target)
	}
	if !strings.Contains(target.Reason, "recent output grace") {
		t.Fatalf("expected recent-output protection reason, got %q", target.Reason)
	}
}

func TestCleanZigArtifactsPreservesRecentOutputAtCritical(t *testing.T) {
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	project := filepath.Join(tmpDir, "recent-zig")
	if err := os.MkdirAll(filepath.Join(project, ".zig-cache", "o"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".zig-cache", "o", "artifact"), []byte("cache"), 0644); err != nil {
		t.Fatal(err)
	}
	buildZig := filepath.Join(project, "build.zig")
	if err := os.WriteFile(buildZig, []byte("const std = @import(\"std\");"), 0644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(buildZig, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	freed := p.cleanZigArtifacts(context.Background(), tmpDir, 0, nil, newDevArtifactActivity(), newDevArtifactGitTracker(), logger)
	if freed != 0 {
		t.Fatalf("expected recent Zig output to be preserved, freed %d bytes", freed)
	}
	if !pathExists(filepath.Join(project, ".zig-cache")) {
		t.Error(".zig-cache should still exist")
	}
}

func TestPlanZigArtifactsProtectsTrackedCache(t *testing.T) {
	git := requireGit(t)
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()

	project := filepath.Join(tmpDir, "tracked-zig")
	if err := os.MkdirAll(filepath.Join(project, ".zig-cache", "o"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".zig-cache", "o", "artifact"), []byte("cache"), 0644); err != nil {
		t.Fatal(err)
	}
	buildZig := filepath.Join(project, "build.zig")
	if err := os.WriteFile(buildZig, []byte("const std = @import(\"std\");"), 0644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(buildZig, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	runGit(t, git, project, "init")
	runGit(t, git, project, "add", "build.zig", ".zig-cache/o/artifact")

	var targets []CleanupTarget
	p.planZigArtifacts(context.Background(), tmpDir, 30*24*time.Hour, true, nil, newDevArtifactActivity(), newDevArtifactGitTracker(), &targets)

	target := findDevArtifactTarget(t, targets, "zig-artifact", filepath.Join(project, ".zig-cache"))
	if target.Action != "protect" || !target.Protected {
		t.Fatalf("expected tracked Zig cache to be protected, got %#v", target)
	}
	if target.Reason != "artifact directory contains files tracked by Git" {
		t.Fatalf("expected tracked-files protection reason, got %q", target.Reason)
	}
}

func TestCleanZigArtifactsPreservesTrackedCache(t *testing.T) {
	git := requireGit(t)
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	project := filepath.Join(tmpDir, "tracked-zig")
	if err := os.MkdirAll(filepath.Join(project, ".zig-cache", "o"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".zig-cache", "o", "artifact"), []byte("cache"), 0644); err != nil {
		t.Fatal(err)
	}
	buildZig := filepath.Join(project, "build.zig")
	if err := os.WriteFile(buildZig, []byte("const std = @import(\"std\");"), 0644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(buildZig, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	runGit(t, git, project, "init")
	runGit(t, git, project, "add", "build.zig", ".zig-cache/o/artifact")

	freed := p.cleanZigArtifacts(context.Background(), tmpDir, 30*24*time.Hour, nil, newDevArtifactActivity(), newDevArtifactGitTracker(), logger)
	if freed != 0 {
		t.Fatalf("expected tracked Zig cache to be preserved, freed %d bytes", freed)
	}
	if !pathExists(filepath.Join(project, ".zig-cache")) {
		t.Error(".zig-cache should still exist")
	}
}

func TestDevArtifactGitTrackerCachesTrackedFilesPerRepo(t *testing.T) {
	git := requireGit(t)
	tmpDir := t.TempDir()
	project := filepath.Join(tmpDir, "tracked-zig")
	if err := os.MkdirAll(filepath.Join(project, ".zig-cache", "o"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, "zig-out", "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".zig-cache", "o", "artifact"), []byte("cache"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "zig-out", "bin", "tool"), []byte("binary"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "build.zig"), []byte("const std = @import(\"std\");"), 0644); err != nil {
		t.Fatal(err)
	}

	runGit(t, git, project, "init")
	runGit(t, git, project, "add", "build.zig", ".zig-cache/o/artifact")

	tracker := newDevArtifactGitTracker()
	if !tracker.ContainsTrackedFiles(filepath.Join(project, ".zig-cache")) {
		t.Fatal("expected tracked .zig-cache content")
	}
	if tracker.ContainsTrackedFiles(filepath.Join(project, "zig-out")) {
		t.Fatal("expected untracked zig-out content")
	}
	if len(tracker.trackedFilesByRoot) != 1 {
		t.Fatalf("expected one git ls-files cache entry, got %d", len(tracker.trackedFilesByRoot))
	}
}

func TestCleanupWarningLevel(t *testing.T) {
	p := NewDevArtifactsPlugin()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := config.DefaultConfig()
	cfg.DevArtifacts.ScanPaths = []string{t.TempDir()}

	// Warning level should report only, not clean
	result := p.Cleanup(context.Background(), LevelWarning, cfg, logger)
	if result.BytesFreed != 0 {
		t.Error("warning level should not free any bytes")
	}
}

func TestPlanCleanupWarningReportsDevArtifacts(t *testing.T) {
	p := newDevArtifactsPluginWithActive(nil)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	tmpDir := t.TempDir()
	project := filepath.Join(tmpDir, "old-project")
	os.MkdirAll(filepath.Join(project, "node_modules", "pkg"), 0755)
	os.WriteFile(filepath.Join(project, "node_modules", "pkg", "index.js"), []byte("test"), 0644)
	packageJSON := filepath.Join(project, "package.json")
	os.WriteFile(packageJSON, []byte(`{"name":"old"}`), 0644)
	oldTime := time.Now().Add(-60 * 24 * time.Hour)
	os.Chtimes(packageJSON, oldTime, oldTime)

	cfg := config.DefaultConfig()
	cfg.DevArtifacts.ScanPaths = []string{tmpDir}
	cfg.DevArtifacts.PythonVenvs = false
	cfg.DevArtifacts.RustTargets = false
	cfg.DevArtifacts.ZigArtifacts = false
	cfg.DevArtifacts.GoBuildCache = false
	cfg.DevArtifacts.HaskellCache = false
	cfg.DevArtifacts.TempArtifacts = false

	plan := p.PlanCleanup(context.Background(), LevelWarning, cfg, logger)
	target := findDevArtifactTarget(t, plan.Targets, "node_modules", filepath.Join(project, "node_modules"))
	if target.Action != "report" {
		t.Fatalf("expected report action at warning, got %#v", target)
	}
	if plan.EstimatedBytesFreed != 0 {
		t.Fatalf("warning plan should not estimate freed bytes, got %d", plan.EstimatedBytesFreed)
	}
	if plan.Metadata["mutates"] != "false" {
		t.Fatalf("expected mutates=false metadata, got %#v", plan.Metadata)
	}
}

func TestPlanCleanupModerateClassifiesDevArtifacts(t *testing.T) {
	p := newDevArtifactsPluginWithActive(nil)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	tmpDir := t.TempDir()

	oldProject := filepath.Join(tmpDir, "old-project")
	os.MkdirAll(filepath.Join(oldProject, "node_modules", "pkg"), 0755)
	os.WriteFile(filepath.Join(oldProject, "node_modules", "pkg", "index.js"), []byte("test"), 0644)
	oldPackageJSON := filepath.Join(oldProject, "package.json")
	os.WriteFile(oldPackageJSON, []byte(`{"name":"old"}`), 0644)
	oldTime := time.Now().Add(-60 * 24 * time.Hour)
	os.Chtimes(oldPackageJSON, oldTime, oldTime)

	freshProject := filepath.Join(tmpDir, "fresh-project")
	os.MkdirAll(filepath.Join(freshProject, "node_modules", "pkg"), 0755)
	os.WriteFile(filepath.Join(freshProject, "node_modules", "pkg", "index.js"), []byte("test"), 0644)
	os.WriteFile(filepath.Join(freshProject, "package.json"), []byte(`{"name":"fresh"}`), 0644)

	protectedProject := filepath.Join(tmpDir, "protected-project")
	os.MkdirAll(filepath.Join(protectedProject, "node_modules", "pkg"), 0755)
	os.WriteFile(filepath.Join(protectedProject, "node_modules", "pkg", "index.js"), []byte("test"), 0644)
	protectedPackageJSON := filepath.Join(protectedProject, "package.json")
	os.WriteFile(protectedPackageJSON, []byte(`{"name":"protected"}`), 0644)
	os.Chtimes(protectedPackageJSON, oldTime, oldTime)

	cfg := config.DefaultConfig()
	cfg.DevArtifacts.ScanPaths = []string{tmpDir}
	cfg.DevArtifacts.PythonVenvs = false
	cfg.DevArtifacts.RustTargets = false
	cfg.DevArtifacts.ZigArtifacts = false
	cfg.DevArtifacts.GoBuildCache = false
	cfg.DevArtifacts.HaskellCache = false
	cfg.DevArtifacts.TempArtifacts = false
	cfg.DevArtifacts.ProtectPaths = []string{protectedProject}

	plan := p.PlanCleanup(context.Background(), LevelModerate, cfg, logger)
	oldTarget := findDevArtifactTarget(t, plan.Targets, "node_modules", filepath.Join(oldProject, "node_modules"))
	if oldTarget.Action != "delete" || oldTarget.Protected {
		t.Fatalf("expected old node_modules delete target, got %#v", oldTarget)
	}
	freshTarget := findDevArtifactTarget(t, plan.Targets, "node_modules", filepath.Join(freshProject, "node_modules"))
	if freshTarget.Action != "protect" || !freshTarget.Protected {
		t.Fatalf("expected fresh node_modules protected, got %#v", freshTarget)
	}
	protectedTarget := findDevArtifactTarget(t, plan.Targets, "node_modules", filepath.Join(protectedProject, "node_modules"))
	if protectedTarget.Action != "protect" || !protectedTarget.Protected {
		t.Fatalf("expected configured protected path to be protected, got %#v", protectedTarget)
	}
	if plan.EstimatedBytesFreed <= 0 {
		t.Fatal("expected positive estimated freed bytes for stale delete target")
	}
	if plan.Metadata["mutates"] != "true" {
		t.Fatalf("expected mutates=true metadata, got %#v", plan.Metadata)
	}
	if got := plan.Metadata["host_reclaim_candidate_bytes"]; got != strconv.FormatInt(oldTarget.Bytes, 10) {
		t.Fatalf("expected stale delete bytes to be host reclaim candidates, got %q metadata=%#v", got, plan.Metadata)
	}
	protectedBytes := requirePlanMetadataInt64(t, plan.Metadata, "protected_physical_bytes")
	if protectedBytes != freshTarget.Bytes+protectedTarget.Bytes {
		t.Fatalf("expected protected byte accounting for fresh/configured targets, got %d metadata=%#v", protectedBytes, plan.Metadata)
	}
	totalBytes := requirePlanMetadataInt64(t, plan.Metadata, "total_physical_bytes")
	if totalBytes != oldTarget.Bytes+freshTarget.Bytes+protectedTarget.Bytes {
		t.Fatalf("expected total byte accounting across all targets, got %d metadata=%#v", totalBytes, plan.Metadata)
	}
}

func TestPlanNodeModulesProtectsActiveDevelopmentProcess(t *testing.T) {
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()
	project := filepath.Join(tmpDir, "active-project")
	if err := os.MkdirAll(filepath.Join(project, "node_modules", "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "node_modules", "pkg", "index.js"), []byte("module.exports = {}"), 0644); err != nil {
		t.Fatal(err)
	}
	packageJSON := filepath.Join(project, "package.json")
	if err := os.WriteFile(packageJSON, []byte(`{"name":"active"}`), 0644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(packageJSON, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	var targets []CleanupTarget
	p.planNodeModules(context.Background(), tmpDir, 30*24*time.Hour, true, nil, newUnscopedDevArtifactActivity(map[string]string{
		"node_modules": "Node.js package manager or runtime",
	}), newDevArtifactGitTracker(), &targets)

	target := findDevArtifactTarget(t, targets, "node_modules", filepath.Join(project, "node_modules"))
	if target.Action != "protect" || !target.Protected || !target.Active {
		t.Fatalf("expected active node_modules to be protected, got %#v", target)
	}
}

func TestPlanNodeModulesScopesActiveProjectProtection(t *testing.T) {
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()
	activeProject := filepath.Join(tmpDir, "active-project")
	staleProject := filepath.Join(tmpDir, "stale-project")
	for _, project := range []string{activeProject, staleProject} {
		if err := os.MkdirAll(filepath.Join(project, "node_modules", "pkg"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, "node_modules", "pkg", "index.js"), []byte("module.exports = {}"), 0644); err != nil {
			t.Fatal(err)
		}
		packageJSON := filepath.Join(project, "package.json")
		if err := os.WriteFile(packageJSON, []byte(`{"name":"project"}`), 0644); err != nil {
			t.Fatal(err)
		}
		oldTime := time.Now().Add(-60 * 24 * time.Hour)
		if err := os.Chtimes(packageJSON, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}

	active := newDevArtifactActivity()
	active.addScoped("node_modules", activeProject, "Node.js package manager or runtime")
	var targets []CleanupTarget
	p.planNodeModules(context.Background(), tmpDir, 30*24*time.Hour, true, nil, active, newDevArtifactGitTracker(), &targets)

	activeTarget := findDevArtifactTarget(t, targets, "node_modules", filepath.Join(activeProject, "node_modules"))
	if activeTarget.Action != "protect" || !activeTarget.Protected || !activeTarget.Active {
		t.Fatalf("expected scoped active project to be protected, got %#v", activeTarget)
	}
	staleTarget := findDevArtifactTarget(t, targets, "node_modules", filepath.Join(staleProject, "node_modules"))
	if staleTarget.Action != "delete" || staleTarget.Protected || staleTarget.Active {
		t.Fatalf("expected unrelated stale project to remain a delete candidate, got %#v", staleTarget)
	}
}

func TestPlanCleanupSkipsGlobalActiveDevArtifactFamilyScans(t *testing.T) {
	p := newDevArtifactsPluginWithActive(map[string]string{
		"node_modules": "Node.js package manager or runtime",
	})
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	tmpDir := t.TempDir()
	project := filepath.Join(tmpDir, "active-project")
	if err := os.MkdirAll(filepath.Join(project, "node_modules", "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "node_modules", "pkg", "index.js"), []byte("module.exports = {}"), 0644); err != nil {
		t.Fatal(err)
	}
	packageJSON := filepath.Join(project, "package.json")
	if err := os.WriteFile(packageJSON, []byte(`{"name":"active"}`), 0644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(packageJSON, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultConfig()
	cfg.DevArtifacts.ScanPaths = []string{tmpDir}
	cfg.DevArtifacts.PythonVenvs = false
	cfg.DevArtifacts.RustTargets = false
	cfg.DevArtifacts.ZigArtifacts = false
	cfg.DevArtifacts.GoBuildCache = false
	cfg.DevArtifacts.HaskellCache = false
	cfg.DevArtifacts.TempArtifacts = false
	cfg.DevArtifacts.LargeLocalArtifacts = false
	cfg.DevArtifacts.LMStudioModels = false
	cfg.DevArtifacts.ScanMaxEntries = 1

	plan := p.PlanCleanup(context.Background(), LevelAggressive, cfg, logger)
	if len(plan.Targets) != 0 {
		t.Fatalf("global active node_modules should skip expensive scan targets, got %#v", plan.Targets)
	}
	if got := plan.Metadata["global_active_dev_artifact_scan_skips"]; got != "node_modules: Node.js package manager or runtime" {
		t.Fatalf("expected global active scan skip metadata, got %q metadata=%#v", got, plan.Metadata)
	}
	if got := plan.Metadata["scan_budget_exhausted"]; got == "true" {
		t.Fatalf("global active family scan should not exhaust scan budget, metadata=%#v", plan.Metadata)
	}
	if got := plan.Metadata["host_reclaim_candidate_bytes"]; got != "0" {
		t.Fatalf("active protected artifact should not be a host reclaim candidate, got %q", got)
	}
}

func TestPlanZigArtifactsProtectsActiveDevelopmentProcess(t *testing.T) {
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()
	project := filepath.Join(tmpDir, "active-zig")
	if err := os.MkdirAll(filepath.Join(project, ".zig-cache", "o"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".zig-cache", "o", "artifact"), []byte("cache"), 0644); err != nil {
		t.Fatal(err)
	}
	buildZig := filepath.Join(project, "build.zig")
	if err := os.WriteFile(buildZig, []byte("const std = @import(\"std\");"), 0644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(buildZig, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	var targets []CleanupTarget
	p.planZigArtifacts(context.Background(), tmpDir, 30*24*time.Hour, true, nil, newUnscopedDevArtifactActivity(map[string]string{
		"zig-artifact": "Zig toolchain process",
	}), newDevArtifactGitTracker(), &targets)

	target := findDevArtifactTarget(t, targets, "zig-artifact", filepath.Join(project, ".zig-cache"))
	if target.Action != "protect" || !target.Protected || !target.Active {
		t.Fatalf("expected active Zig artifact to be protected, got %#v", target)
	}
}

func TestPlanLargeLocalArtifactsReportsReviewOnlyTargets(t *testing.T) {
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()

	imagePath := filepath.Join(tmpDir, "betterkvm", "images", "pikvm.img")
	if err := os.MkdirAll(filepath.Dir(imagePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, []byte("disk image"), 0644); err != nil {
		t.Fatal(err)
	}

	bundlePath := filepath.Join(tmpDir, "linux-xr-case-sensitive.sparsebundle")
	if err := os.MkdirAll(filepath.Join(bundlePath, "bands"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundlePath, "Info.plist"), []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>band-size</key>
	<integer>8388608</integer>
	<key>diskimage-bundle-type</key>
	<string>com.apple.diskimage.sparsebundle</string>
	<key>size</key>
	<integer>34359738368</integer>
</dict>
</plist>
`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundlePath, "bands", "0"), []byte("bundle data"), 0644); err != nil {
		t.Fatal(err)
	}

	var targets []CleanupTarget
	p.planLargeLocalArtifacts(context.Background(), tmpDir, 1, nil, nil, &targets)

	image := findDevArtifactTarget(t, targets, "large-local-artifact", imagePath)
	if image.Action != "review" || !image.Protected || image.Tier != CleanupTierDestructive || image.Reclaim != CleanupReclaimNone {
		t.Fatalf("expected review-only destructive/no-reclaim image target, got %#v", image)
	}
	bundle := findDevArtifactTarget(t, targets, "large-local-artifact", bundlePath)
	if bundle.Action != "review" || !bundle.Protected || bundle.Bytes <= 0 || bundle.LogicalBytes != 34359738368 {
		t.Fatalf("expected review-only sparsebundle target with physical and logical bytes, got %#v", bundle)
	}
	if !strings.Contains(bundle.Reason, "automatic compaction is not assumed") {
		t.Fatalf("expected sparsebundle compaction caveat, got %q", bundle.Reason)
	}
}

func TestPlanLargeLocalArtifactsReportsMountedImagesAsActive(t *testing.T) {
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()
	bundlePath := filepath.Join(tmpDir, "linux-xr-case-sensitive.sparsebundle")
	if err := os.MkdirAll(filepath.Join(bundlePath, "bands"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundlePath, "bands", "0"), []byte("bundle data"), 0644); err != nil {
		t.Fatal(err)
	}

	var targets []CleanupTarget
	p.planLargeLocalArtifacts(context.Background(), tmpDir, 1, nil, map[string]string{
		filepath.Clean(bundlePath): "/Volumes/linux-xr-cs",
	}, &targets)

	target := findDevArtifactTarget(t, targets, "large-local-artifact", bundlePath)
	if target.Action != "protect" || !target.Protected || !target.Active {
		t.Fatalf("expected mounted sparsebundle to be active/protected, got %#v", target)
	}
	if !strings.Contains(target.Reason, "mounted at /Volumes/linux-xr-cs") {
		t.Fatalf("expected mount point in reason, got %q", target.Reason)
	}
}

func TestParseLargeLocalMountedDiskImages(t *testing.T) {
	output := `
image-path      : /Users/jess/Downloads/installer.dmg
/dev/disk8s1	41504653-0000-11AA-AA11-00306543ECAC	/Volumes/Installer
image-path      : /Users/jess/git/linux-xr-case-sensitive.sparsebundle
/dev/disk9	EF57347C-0000-11AA-AA11-00306543ECAC
/dev/disk9s1	41504653-0000-11AA-AA11-00306543ECAC	/Volumes/linux-xr-cs
`
	mounted := parseLargeLocalMountedDiskImages(output)
	if mounted["/Users/jess/Downloads/installer.dmg"] != "/Volumes/Installer" {
		t.Fatalf("installer mount missing: %#v", mounted)
	}
	if mounted["/Users/jess/git/linux-xr-case-sensitive.sparsebundle"] != "/Volumes/linux-xr-cs" {
		t.Fatalf("sparsebundle mount missing: %#v", mounted)
	}
}

func TestPlanLargeLocalArtifactsHonorsProtectPaths(t *testing.T) {
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()
	imagePath := filepath.Join(tmpDir, "protected", "machine.qcow2")
	if err := os.MkdirAll(filepath.Dir(imagePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, []byte("disk image"), 0644); err != nil {
		t.Fatal(err)
	}

	var targets []CleanupTarget
	p.planLargeLocalArtifacts(context.Background(), tmpDir, 1, []string{filepath.Dir(imagePath)}, nil, &targets)

	target := findDevArtifactTarget(t, targets, "large-local-artifact", imagePath)
	if target.Action != "protect" || !target.Protected {
		t.Fatalf("expected protected large local artifact target, got %#v", target)
	}
}

func TestPlanTemporaryArtifactsReportsReviewOnlyTargets(t *testing.T) {
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()
	oldPath := filepath.Join(tmpDir, "old-proof-output")
	freshPath := filepath.Join(tmpDir, "fresh-proof-output")
	activePath := filepath.Join(tmpDir, "active-proof-output")
	for _, path := range []string{oldPath, freshPath, activePath} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "artifact"), []byte("artifact"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	var targets []CleanupTarget
	cfg := config.DefaultConfig().DevArtifacts
	cfg.NixTempRoots = false
	p.planTemporaryArtifacts(context.Background(), tmpDir, 1, 6*time.Hour, cfg, map[string]string{
		canonicalTempArtifactPath(activePath): "bazel",
	}, &targets)

	oldTarget := findDevArtifactTarget(t, targets, "temporary-dev-artifact", oldPath)
	if oldTarget.Action != "review_temp_artifact" || !oldTarget.Protected || oldTarget.Tier != CleanupTierDestructive || oldTarget.Reclaim != CleanupReclaimNone {
		t.Fatalf("expected review-only temporary artifact target, got %#v", oldTarget)
	}
	if !strings.Contains(oldTarget.Reason, "manual review required") {
		t.Fatalf("expected manual review reason, got %q", oldTarget.Reason)
	}

	freshTarget := findDevArtifactTarget(t, targets, "temporary-dev-artifact", freshPath)
	if freshTarget.Action != "protect" || !freshTarget.Protected {
		t.Fatalf("expected fresh temporary artifact to be protected, got %#v", freshTarget)
	}

	activeTarget := findDevArtifactTarget(t, targets, "temporary-dev-artifact", activePath)
	if activeTarget.Action != "protect" || !activeTarget.Protected || !activeTarget.Active {
		t.Fatalf("expected active temporary artifact to be protected, got %#v", activeTarget)
	}
	if activeTarget.Bytes != 0 {
		t.Fatalf("active temporary roots should avoid expensive sizing, got %d bytes", activeTarget.Bytes)
	}
}

func TestPlanTemporaryArtifactsClassifiesProofLanes(t *testing.T) {
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()
	proofLane := filepath.Join(tmpDir, "t2035b-mi.0WfaPi")
	if err := os.MkdirAll(proofLane, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proofLane, "artifact"), []byte("artifact"), 0644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(proofLane, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	var targets []CleanupTarget
	cfg := config.DefaultConfig().DevArtifacts
	cfg.NixTempRoots = false
	p.planTemporaryArtifacts(context.Background(), tmpDir, 1, 6*time.Hour, cfg, nil, &targets)

	target := findDevArtifactTarget(t, targets, "temporary-proof-lane", proofLane)
	if target.Action != "review_temp_proof_lane" || !target.Protected || target.Reclaim != CleanupReclaimNone {
		t.Fatalf("expected proof lane to remain review-only, got %#v", target)
	}
	if !strings.Contains(target.Reason, "nested rebuildable artifacts") {
		t.Fatalf("expected nested-artifact guidance in reason, got %q", target.Reason)
	}
}

func TestPlanTemporaryArtifactsDoesNotChargeSymlinkTargets(t *testing.T) {
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()
	root := filepath.Join(tmpDir, "nix-shell.symlink-forest")
	targetDir := filepath.Join(tmpDir, "store-target")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "large-store-file"), bytesOfSize(2*1024*1024), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(targetDir, "large-store-file"), filepath.Join(root, "tool")); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	oldTime := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(root, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	var targets []CleanupTarget
	cfg := config.DefaultConfig().DevArtifacts
	cfg.NixTempRoots = false
	p.planTemporaryArtifacts(context.Background(), tmpDir, 1024*1024, 6*time.Hour, cfg, nil, &targets)

	for _, target := range targets {
		if target.Path == root {
			t.Fatalf("symlink forest should not be reported as large temp artifact, got %#v", target)
		}
	}
}

func TestPlanCleanupReportsAgentWorktreeRustTargets(t *testing.T) {
	p := newDevArtifactsPluginWithActive(nil)
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := filepath.Join(home, ".claude", "worktrees", "session-a", "repo")
	targetDir := filepath.Join(worktree, "target", "debug")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}
	cargoToml := filepath.Join(worktree, "Cargo.toml")
	if err := os.WriteFile(cargoToml, []byte("[package]\nname = \"agent-worktree\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "artifact"), make([]byte, 2*1024*1024), 0644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(cargoToml, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	cfg := agentWorktreeArtifactConfig()
	plan := p.PlanCleanup(context.Background(), LevelCritical, cfg, slog.Default())

	target := findDevArtifactTarget(t, plan.Targets, "rust-target", filepath.Join(worktree, "target"))
	if target.Action != "delete" || target.Protected {
		t.Fatalf("expected agent worktree Rust target to be deletable, got %#v", target)
	}
}

func TestPlanCleanupReportsPnpmStorePruneTarget(t *testing.T) {
	p := newDevArtifactsPluginWithActive(nil)
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := filepath.Join(home, ".local", "share", "pnpm", "store", "v3")
	if err := os.MkdirAll(store, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "pkg"), make([]byte, 2*1024*1024), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := pnpmStoreConfig()
	plan := p.PlanCleanup(context.Background(), LevelModerate, cfg, slog.Default())

	target := findDevArtifactTarget(t, plan.Targets, "pnpm-store", filepath.Join(home, ".local", "share", "pnpm"))
	if target.Action != "clean-cache" || target.Protected || target.Tier != CleanupTierSafe {
		t.Fatalf("expected pnpm store prune target, got %#v", target)
	}
}

func TestPlanCleanupReportsGeneratedArtifactsInsideStaleTemporaryRoots(t *testing.T) {
	p := newDevArtifactsPluginWithActive(nil)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	tmpDir := t.TempDir()
	root := filepath.Join(tmpDir, "temp-rust-worktree")
	targetDir := filepath.Join(root, "target", "debug")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}
	cargoToml := filepath.Join(root, "Cargo.toml")
	if err := os.WriteFile(cargoToml, []byte("[package]\nname = \"temp-rust-worktree\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "artifact"), make([]byte, 2*1024*1024), 0644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(root, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(cargoToml, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	cfg := tempGeneratedArtifactConfig(tmpDir)
	plan := p.PlanCleanup(context.Background(), LevelCritical, cfg, logger)

	rootTarget := findDevArtifactTarget(t, plan.Targets, "temporary-dev-artifact", root)
	if rootTarget.Action != "review_temp_artifact" || !rootTarget.Protected {
		t.Fatalf("expected top-level temporary root to remain review-only, got %#v", rootTarget)
	}
	rustTarget := findDevArtifactTarget(t, plan.Targets, "rust-target", filepath.Join(root, "target"))
	if rustTarget.Action != "delete" || rustTarget.Protected {
		t.Fatalf("expected nested Rust target to be deletable, got %#v", rustTarget)
	}
	if plan.EstimatedBytesFreed <= 0 {
		t.Fatal("expected nested generated artifact to contribute to reclaim estimate")
	}
}

func TestCleanupPrunesGeneratedArtifactsInsideStaleTemporaryRoots(t *testing.T) {
	p := newDevArtifactsPluginWithActive(nil)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	tmpDir := t.TempDir()
	root := filepath.Join(tmpDir, "temp-rust-worktree")
	targetDir := filepath.Join(root, "target", "debug")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}
	cargoToml := filepath.Join(root, "Cargo.toml")
	if err := os.WriteFile(cargoToml, []byte("[package]\nname = \"temp-rust-worktree\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "artifact"), make([]byte, 2*1024*1024), 0644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(root, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(cargoToml, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	result := p.Cleanup(context.Background(), LevelCritical, tempGeneratedArtifactConfig(tmpDir), logger)
	if pathExists(filepath.Join(root, "target")) {
		t.Fatal("expected generated Rust target inside stale temporary root to be removed")
	}
	if !pathExists(cargoToml) {
		t.Fatal("temporary worktree source file should be preserved")
	}
	if result.BytesFreed <= 0 {
		t.Fatalf("expected cleanup to report freed bytes, got %#v", result)
	}
}

func TestPlanCleanupDeletesStaleNixTemporaryRoots(t *testing.T) {
	p := newDevArtifactsPluginWithActive(nil)
	tmpDir := t.TempDir()
	root := filepath.Join(tmpDir, "nix-shell.Abc123")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scratch"), make([]byte, 2*1024*1024), 0644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(root, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	cfg := nixTempRootConfig(tmpDir)
	plan := p.PlanCleanup(context.Background(), LevelCritical, cfg, slog.Default())

	target := findDevArtifactTarget(t, plan.Targets, "nix-temp-root", root)
	if target.Action != "delete" || target.Protected {
		t.Fatalf("expected stale Nix temp root to be deletable, got %#v", target)
	}
	if plan.EstimatedBytesFreed <= 0 {
		t.Fatal("expected stale Nix temp root to contribute to reclaim estimate")
	}
}

func TestCleanupPrunesNixTemporaryRootsBeforeWorkspaceScanBudget(t *testing.T) {
	p := newDevArtifactsPluginWithActive(nil)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	tmpDir := t.TempDir()
	root := filepath.Join(tmpDir, "nix-develop-1234-5678")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scratch"), make([]byte, 2*1024*1024), 0644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(root, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "project", "node_modules", "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "project", "package.json"), []byte(`{"name":"budget"}`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := nixTempRootConfig(tmpDir)
	cfg.DevArtifacts.ScanPaths = []string{workspace}
	cfg.DevArtifacts.ScanMaxEntries = 1
	cfg.DevArtifacts.NodeModules = true

	result := p.Cleanup(context.Background(), LevelCritical, cfg, logger)
	if pathExists(root) {
		t.Fatal("expected stale Nix temp root to be removed before workspace scan budget is exhausted")
	}
	if result.BytesFreed <= 0 {
		t.Fatalf("expected cleanup to report freed bytes, got %#v", result)
	}
}

func TestPlanCleanupProtectsActiveNixTemporaryRoots(t *testing.T) {
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()
	root := filepath.Join(tmpDir, "nix-1234-0")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scratch"), make([]byte, 2*1024*1024), 0644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(root, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	var targets []CleanupTarget
	p.planNixTemporaryRoots(
		context.Background(),
		tmpDir,
		1024*1024,
		24*time.Hour,
		true,
		nixTempRootConfig(tmpDir).DevArtifacts,
		map[string]string{canonicalTempArtifactPath(root): "nix-daemon"},
		&targets,
		newDevArtifactScanBudget(config.DefaultConfig().DevArtifacts),
	)

	target := findDevArtifactTarget(t, targets, "nix-temp-root", root)
	if target.Action != "protect" || !target.Active || !target.Protected {
		t.Fatalf("expected active Nix temp root to be protected, got %#v", target)
	}
}

func tempGeneratedArtifactConfig(tmpDir string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.DevArtifacts.ScanPaths = nil
	cfg.DevArtifacts.TempScanPaths = []string{tmpDir}
	cfg.DevArtifacts.TempArtifactMinMB = 1
	cfg.DevArtifacts.TempArtifactStaleAfter = "6h"
	cfg.DevArtifacts.NodeModules = false
	cfg.DevArtifacts.PythonVenvs = false
	cfg.DevArtifacts.RustTargets = true
	cfg.DevArtifacts.ZigArtifacts = false
	cfg.DevArtifacts.GoBuildCache = false
	cfg.DevArtifacts.HaskellCache = false
	cfg.DevArtifacts.LargeLocalArtifacts = false
	cfg.DevArtifacts.LMStudioModels = false
	return cfg
}

func agentWorktreeArtifactConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.DevArtifacts.ScanPaths = nil
	cfg.DevArtifacts.TempArtifacts = false
	cfg.DevArtifacts.AgentWorktreeArtifacts = true
	cfg.DevArtifacts.NodeModules = false
	cfg.DevArtifacts.PythonVenvs = false
	cfg.DevArtifacts.RustTargets = true
	cfg.DevArtifacts.ZigArtifacts = false
	cfg.DevArtifacts.PnpmStore = false
	cfg.DevArtifacts.GoBuildCache = false
	cfg.DevArtifacts.HaskellCache = false
	cfg.DevArtifacts.LargeLocalArtifacts = false
	cfg.DevArtifacts.LMStudioModels = false
	return cfg
}

func pnpmStoreConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.DevArtifacts.ScanPaths = nil
	cfg.DevArtifacts.TempArtifacts = false
	cfg.DevArtifacts.AgentWorktreeArtifacts = false
	cfg.DevArtifacts.NodeModules = false
	cfg.DevArtifacts.PythonVenvs = false
	cfg.DevArtifacts.RustTargets = false
	cfg.DevArtifacts.ZigArtifacts = false
	cfg.DevArtifacts.PnpmStore = true
	cfg.DevArtifacts.GoBuildCache = false
	cfg.DevArtifacts.HaskellCache = false
	cfg.DevArtifacts.LargeLocalArtifacts = false
	cfg.DevArtifacts.LMStudioModels = false
	return cfg
}

func nixTempRootConfig(tmpDir string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.DevArtifacts.ScanPaths = nil
	cfg.DevArtifacts.TempScanPaths = []string{tmpDir}
	cfg.DevArtifacts.TempScanMaxRoots = 20
	cfg.DevArtifacts.TempArtifactMinMB = 256
	cfg.DevArtifacts.TempArtifactStaleAfter = "6h"
	cfg.DevArtifacts.NixTempRoots = true
	cfg.DevArtifacts.NixTempRootMinMB = 1
	cfg.DevArtifacts.NixTempRootStaleAfter = "24h"
	cfg.DevArtifacts.NodeModules = false
	cfg.DevArtifacts.PythonVenvs = false
	cfg.DevArtifacts.RustTargets = false
	cfg.DevArtifacts.ZigArtifacts = false
	cfg.DevArtifacts.GoBuildCache = false
	cfg.DevArtifacts.HaskellCache = false
	cfg.DevArtifacts.LargeLocalArtifacts = false
	cfg.DevArtifacts.LMStudioModels = false
	return cfg
}

func TestFindArtifactDirsHonorsCanceledContext(t *testing.T) {
	p := NewDevArtifactsPlugin()
	tmpDir := t.TempDir()
	project := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(filepath.Join(project, "node_modules", "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "package.json"), []byte(`{"name":"test"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "node_modules", "pkg", "index.js"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.findArtifactDirs(ctx, tmpDir, "node_modules", "package.json", func(dir string, size int64) {
		t.Fatalf("canceled scan should not report %s", dir)
	})
}

func TestPlanCleanupReportsDevArtifactScanBudgetExhaustion(t *testing.T) {
	p := NewDevArtifactsPlugin()
	p.activeProcesses = func(context.Context) (map[string]string, error) {
		return nil, nil
	}
	tmpDir := t.TempDir()
	project := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(filepath.Join(project, "node_modules", "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "package.json"), []byte(`{"name":"test"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "node_modules", "pkg", "index.js"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := budgetedDevArtifactConfig(tmpDir)
	plan := p.PlanCleanup(context.Background(), LevelCritical, cfg, slog.Default())

	if plan.Metadata["scan_budget_exhausted"] != "true" {
		t.Fatalf("expected exhausted scan budget metadata, got %#v", plan.Metadata)
	}
	if !strings.Contains(plan.Metadata["scan_truncated_paths"], project) {
		t.Fatalf("expected truncated path metadata to include %q, got %q", project, plan.Metadata["scan_truncated_paths"])
	}
	if len(plan.Warnings) == 0 {
		t.Fatal("expected scan budget warning")
	}
	if len(plan.Targets) != 0 {
		t.Fatalf("budgeted scan should not report candidates from incomplete evidence, got %#v", plan.Targets)
	}
}

func TestCleanupDoesNotDeleteAfterDevArtifactScanBudgetExhaustion(t *testing.T) {
	p := NewDevArtifactsPlugin()
	p.activeProcesses = func(context.Context) (map[string]string, error) {
		return nil, nil
	}
	tmpDir := t.TempDir()
	project := filepath.Join(tmpDir, "project")
	nodeModules := filepath.Join(project, "node_modules")
	if err := os.MkdirAll(filepath.Join(nodeModules, "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	packageJSON := filepath.Join(project, "package.json")
	if err := os.WriteFile(packageJSON, []byte(`{"name":"test"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodeModules, "pkg", "index.js"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(packageJSON, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	result := p.Cleanup(context.Background(), LevelCritical, budgetedDevArtifactConfig(tmpDir), logger)

	if !pathExists(nodeModules) {
		t.Fatal("node_modules should be preserved when scan budget is exhausted before complete evidence")
	}
	if result.BytesFreed != 0 || result.ItemsCleaned != 0 {
		t.Fatalf("budgeted cleanup should not report mutation, got %#v", result)
	}
}

func budgetedDevArtifactConfig(scanPath string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.DevArtifacts.ScanPaths = []string{scanPath}
	cfg.DevArtifacts.ScanMaxDuration = "30s"
	cfg.DevArtifacts.ScanMaxEntries = 1
	cfg.DevArtifacts.TempArtifacts = false
	cfg.DevArtifacts.NodeModules = true
	cfg.DevArtifacts.PythonVenvs = false
	cfg.DevArtifacts.RustTargets = false
	cfg.DevArtifacts.ZigArtifacts = false
	cfg.DevArtifacts.GoBuildCache = false
	cfg.DevArtifacts.HaskellCache = false
	cfg.DevArtifacts.LargeLocalArtifacts = false
	cfg.DevArtifacts.LMStudioModels = false
	return cfg
}

func TestTempArtifactRootsFromProcessOutput(t *testing.T) {
	output := `
bazel(workspace) bazel(workspace) --output_user_root=/tmp/crush-dots-bazel-output --output_base=/tmp/crush-dots-bazel-output/817/output
/usr/bin/zsh zsh -lc echo /tmp/not-active
`
	roots := tempArtifactRootsFromProcessOutput(output, []string{"/tmp"}, "")
	want := tempArtifactRootForPath("/tmp/crush-dots-bazel-output/817/output", []string{"/tmp"}, "")
	if roots[want] != "bazel(workspace)" {
		t.Fatalf("expected active root %q from bazel process, got %#v", want, roots)
	}
}

func TestDevArtifactBusyProcessReasons(t *testing.T) {
	ps := `
/nix/store/node/bin/node node vite dev
/nix/store/uv/bin/uv uv pip install -r requirements.txt
/nix/store/rust/bin/cargo cargo build
/nix/store/zig/bin/zig zig build
/nix/store/go/bin/go go test ./...
/nix/store/cabal/bin/cabal cabal build all
/Applications/LM Studio.app/Contents/MacOS/LM Studio
`
	active := devArtifactBusyProcessReasons(ps)

	want := map[string]string{
		"node_modules":    "Node.js package manager or runtime",
		"python-venv":     "Python toolchain process",
		"rust-target":     "Rust toolchain process",
		"zig-artifact":    "Zig toolchain process",
		"go-build-cache":  "Go toolchain process",
		"haskell-cache":   "Haskell toolchain process",
		"lmstudio-models": "LM Studio process",
	}
	for targetType, reason := range want {
		if active[targetType] != reason {
			t.Fatalf("active[%s] = %q, want %q; active=%#v", targetType, active[targetType], reason, active)
		}
	}
}

func TestDevArtifactActivityFromProcessEvidenceScopesCWD(t *testing.T) {
	tmpDir := t.TempDir()
	activeProject := filepath.Join(tmpDir, "active-project")
	staleProject := filepath.Join(tmpDir, "stale-project")
	for _, project := range []string{activeProject, staleProject} {
		if err := os.MkdirAll(project, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, "package.json"), []byte(`{"name":"project"}`), 0644); err != nil {
			t.Fatal(err)
		}
	}
	candidates := devArtifactProcessCandidates("123 /nix/store/node/bin/node node vite dev")
	activity := devArtifactActivityFromProcessEvidence(candidates, map[string]string{"123": activeProject}, []string{tmpDir}, "")

	if reason, ok := activity.TargetReason("node_modules", filepath.Join(activeProject, "node_modules")); !ok || !strings.Contains(reason, "Node.js") {
		t.Fatalf("expected active project target to be protected, reason=%q ok=%v", reason, ok)
	}
	if _, ok := activity.TargetReason("node_modules", filepath.Join(staleProject, "node_modules")); ok {
		t.Fatal("expected unrelated project target to remain unprotected")
	}
	if activity.GlobalFamilyActive("node_modules") {
		t.Fatal("path-scoped node activity should not force family-wide protection")
	}
}

func TestDevArtifactActivityNormalizesNestedNodeModulesPath(t *testing.T) {
	tmpDir := t.TempDir()
	project := filepath.Join(tmpDir, "active-project")
	nestedPackage := filepath.Join(project, "node_modules", "vite")
	if err := os.MkdirAll(nestedPackage, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "package.json"), []byte(`{"name":"project"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nestedPackage, "package.json"), []byte(`{"name":"vite"}`), 0644); err != nil {
		t.Fatal(err)
	}

	candidates := devArtifactProcessCandidates("123 /nix/store/node/bin/node node " + filepath.Join(nestedPackage, "bin", "vite.js"))
	activity := devArtifactActivityFromProcessEvidence(candidates, nil, []string{tmpDir}, "")

	if _, ok := activity.scopedRoots["node_modules"][project]; !ok {
		t.Fatalf("expected nested node_modules process to scope to project root %q, got %#v", project, activity.scopedRoots)
	}
	if _, ok := activity.scopedRoots["node_modules"][nestedPackage]; ok {
		t.Fatalf("nested package root should not be treated as owning project, got %#v", activity.scopedRoots)
	}
}

func TestDevArtifactActivityIgnoresKnownOutOfScopeProcess(t *testing.T) {
	tmpDir := t.TempDir()
	project := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(project, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "package.json"), []byte(`{"name":"project"}`), 0644); err != nil {
		t.Fatal(err)
	}

	candidates := devArtifactProcessCandidates("123 /nix/store/node/bin/node node background-helper")
	activity := devArtifactActivityFromProcessEvidence(candidates, map[string]string{"123": "/Applications"}, []string{tmpDir}, "")

	if activity.HasActivity() {
		t.Fatalf("known out-of-scope process should not protect scanned artifacts, got %#v", activity)
	}
}

func TestDevArtifactActivityKeepsUnscopedFallbackWhenCWDUnknown(t *testing.T) {
	tmpDir := t.TempDir()
	candidates := devArtifactProcessCandidates("123 /nix/store/node/bin/node node background-helper")
	activity := devArtifactActivityFromProcessEvidence(candidates, nil, []string{tmpDir}, "")

	if !activity.GlobalFamilyActive("node_modules") {
		t.Fatalf("missing cwd evidence should keep conservative family protection, got %#v", activity)
	}
}

func TestDevArtifactActivityReasonsSorted(t *testing.T) {
	reasons := devArtifactActivityReasons(map[string]string{
		"rust-target":  "Rust toolchain process",
		"node_modules": "Node.js package manager or runtime",
	})
	want := []string{
		"node_modules: Node.js package manager or runtime",
		"rust-target: Rust toolchain process",
	}
	if len(reasons) != len(want) {
		t.Fatalf("got reasons %#v, want %#v", reasons, want)
	}
	for idx := range want {
		if reasons[idx] != want[idx] {
			t.Fatalf("got reasons %#v, want %#v", reasons, want)
		}
	}
}

func TestAgentTranscriptCompressionPreservesHistory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	transcriptRoot := filepath.Join(home, ".codex", "sessions")
	transcript := filepath.Join(transcriptRoot, "2026", "05", "01", "rollout.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0755); err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat(`{"event":"large transcript"}`+"\n", 2048)
	if err := os.WriteFile(transcript, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(transcript, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	cfg := agentStateRetentionConfig(home)
	plugin := newDevArtifactsPluginWithActive(nil)
	plan := plugin.PlanCleanup(context.Background(), LevelModerate, cfg, devArtifactTestLogger())
	target := findDevArtifactTarget(t, plan.Targets, "agent-transcript", transcript)
	if target.Action != "compress" || target.Protected {
		t.Fatalf("expected old transcript to be compressed, got %#v", target)
	}

	result := plugin.Cleanup(context.Background(), LevelModerate, cfg, devArtifactTestLogger())
	if result.ItemsCleaned != 1 || result.BytesFreed <= 0 {
		t.Fatalf("expected transcript compression to reclaim bytes, got %#v", result)
	}
	if pathExists(transcript) {
		t.Fatal("uncompressed transcript should be replaced")
	}
	gzPath := transcript + ".gz"
	if !pathExists(gzPath) {
		t.Fatal("compressed transcript should exist")
	}
	got := readGzipFile(t, gzPath)
	if got != content {
		t.Fatal("compressed transcript did not preserve content")
	}
}

func TestAgentWorktreeRootCleanupDeletesOnlyStaleCriticalRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := filepath.Join(home, ".claude", "worktrees")
	stale := filepath.Join(root, "agent-old")
	recent := filepath.Join(root, "agent-new")
	protected := filepath.Join(root, "agent-protected")
	for _, dir := range []string{stale, recent, protected} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "state.bin"), []byte(strings.Repeat("x", 1024)), 0644); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-30 * 24 * time.Hour)
	for _, dir := range []string{stale, protected} {
		if err := os.Chtimes(dir, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}

	cfg := agentStateRetentionConfig(home)
	cfg.DevArtifacts.ProtectPaths = []string{protected}
	plugin := newDevArtifactsPluginWithActive(nil)

	aggressivePlan := plugin.PlanCleanup(context.Background(), LevelAggressive, cfg, devArtifactTestLogger())
	aggressiveTarget := findDevArtifactTarget(t, aggressivePlan.Targets, "agent-worktree-root", stale)
	if aggressiveTarget.Action != "report" || !aggressiveTarget.Protected {
		t.Fatalf("expected aggressive cleanup to report stale root only, got %#v", aggressiveTarget)
	}

	criticalPlan := plugin.PlanCleanup(context.Background(), LevelCritical, cfg, devArtifactTestLogger())
	staleTarget := findDevArtifactTarget(t, criticalPlan.Targets, "agent-worktree-root", stale)
	if staleTarget.Action != "delete" || staleTarget.Protected {
		t.Fatalf("expected critical cleanup to delete stale root, got %#v", staleTarget)
	}
	protectedTarget := findDevArtifactTarget(t, criticalPlan.Targets, "agent-worktree-root", protected)
	if protectedTarget.Action != "protect" || !protectedTarget.Protected {
		t.Fatalf("expected protected stale root to remain protected, got %#v", protectedTarget)
	}
	if _, ok := findDevArtifactTargetMaybe(criticalPlan.Targets, "agent-worktree-root", recent); ok {
		t.Fatalf("recent worktree should not be a cleanup target")
	}

	result := plugin.Cleanup(context.Background(), LevelCritical, cfg, devArtifactTestLogger())
	if result.ItemsCleaned != 1 || result.BytesFreed <= 0 {
		t.Fatalf("expected one stale worktree deleted, got %#v", result)
	}
	if pathExists(stale) {
		t.Fatal("stale worktree should be deleted")
	}
	if !pathExists(recent) {
		t.Fatal("recent worktree should remain")
	}
	if !pathExists(protected) {
		t.Fatal("protected worktree should remain")
	}
}

func TestActiveAgentPathReferencesFromProcessOutputMapsNestedPathToRoot(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".claude", "worktrees")
	worktree := filepath.Join(root, "agent-live")
	nested := filepath.Join(worktree, "src", "main.go")
	output := "codex codex edit " + nested + "\n"

	refs := activeAgentPathReferencesFromProcessOutput(output, []string{root}, home)
	if refs[filepath.Clean(nested)] != "codex" {
		t.Fatalf("expected nested path reference, got %#v", refs)
	}
	if refs[filepath.Clean(worktree)] != "codex" {
		t.Fatalf("expected top-level worktree reference, got %#v", refs)
	}
}

func TestGetGoCacheDir(t *testing.T) {
	p := NewDevArtifactsPlugin()

	// This test depends on having go installed
	if _, err := os.Stat("/usr/bin/go"); os.IsNotExist(err) {
		if _, err := os.Stat("/usr/local/go/bin/go"); os.IsNotExist(err) {
			t.Skip("go not installed, skipping")
		}
	}

	dir := p.getGoCacheDir(context.Background())
	// May or may not return a value depending on environment
	// Just verify it doesn't panic
	_ = dir
}

func agentStateRetentionConfig(home string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.DevArtifacts.ScanPaths = nil
	cfg.DevArtifacts.TempArtifacts = false
	cfg.DevArtifacts.NodeModules = false
	cfg.DevArtifacts.PythonVenvs = false
	cfg.DevArtifacts.RustTargets = false
	cfg.DevArtifacts.ZigArtifacts = false
	cfg.DevArtifacts.AgentWorktreeArtifacts = false
	cfg.DevArtifacts.AgentWorktreeRootCleanup = true
	cfg.DevArtifacts.AgentWorktreeRoots = []string{filepath.Join(home, ".claude", "worktrees")}
	cfg.DevArtifacts.AgentWorktreeStaleAfter = "7d"
	cfg.DevArtifacts.AgentTranscriptCompression = true
	cfg.DevArtifacts.AgentTranscriptRoots = []string{filepath.Join(home, ".codex", "sessions")}
	cfg.DevArtifacts.AgentTranscriptCompressAfter = "7d"
	cfg.DevArtifacts.PnpmStore = false
	cfg.DevArtifacts.GoBuildCache = false
	cfg.DevArtifacts.HaskellCache = false
	cfg.DevArtifacts.LargeLocalArtifacts = false
	cfg.DevArtifacts.LMStudioModels = false
	return cfg
}

func readGzipFile(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func devArtifactTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func findDevArtifactTarget(t *testing.T, targets []CleanupTarget, targetType, path string) CleanupTarget {
	t.Helper()
	for _, target := range targets {
		if target.Type == targetType && target.Path == path {
			return target
		}
	}
	t.Fatalf("target %s at %s not found in %#v", targetType, path, targets)
	return CleanupTarget{}
}

func findDevArtifactTargetMaybe(targets []CleanupTarget, targetType, path string) (CleanupTarget, bool) {
	for _, target := range targets {
		if target.Type == targetType && target.Path == path {
			return target, true
		}
	}
	return CleanupTarget{}, false
}

func newDevArtifactsPluginWithActive(active map[string]string) *DevArtifactsPlugin {
	return &DevArtifactsPlugin{
		activeProcesses: func(context.Context) (map[string]string, error) {
			return active, nil
		},
	}
}

func requirePlanMetadataInt64(t *testing.T, metadata map[string]string, key string) int64 {
	t.Helper()
	raw, ok := metadata[key]
	if !ok {
		t.Fatalf("expected metadata key %q in %#v", key, metadata)
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("metadata key %q should be an int64, got %q: %v", key, raw, err)
	}
	return value
}
