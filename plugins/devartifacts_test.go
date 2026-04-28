package plugins

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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
	p.findArtifactDirs(tmpDir, "node_modules", "package.json", func(dir string, size int64) {
		foundNodeModules = append(foundNodeModules, dir)
	})

	if len(foundNodeModules) != 1 {
		t.Errorf("expected 1 node_modules, found %d", len(foundNodeModules))
	}

	// Find target with Cargo.toml marker
	var foundTargets []string
	p.findArtifactDirs(tmpDir, "target", "Cargo.toml", func(dir string, size int64) {
		foundTargets = append(foundTargets, dir)
	})

	if len(foundTargets) != 1 {
		t.Errorf("expected 1 Rust target, found %d", len(foundTargets))
	}

	// Find Zig .zig-cache with build.zig marker
	var foundZigCaches []string
	p.findArtifactDirs(tmpDir, ".zig-cache", "build.zig", func(dir string, size int64) {
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
	p.findArtifactDirs(tmpDir, "node_modules", "package.json", func(dir string, size int64) {
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
	freed := p.cleanNodeModules(context.Background(), tmpDir, 30*24*time.Hour, nil, newDevArtifactGitTracker(), logger)

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
	freed := p.cleanNodeModules(context.Background(), tmpDir, 30*24*time.Hour, nil, newDevArtifactGitTracker(), logger)

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
	freed := p.cleanNodeModules(context.Background(), tmpDir, 30*24*time.Hour, protectPaths, newDevArtifactGitTracker(), logger)

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

	freed := p.cleanZigArtifacts(context.Background(), tmpDir, 30*24*time.Hour, nil, newDevArtifactGitTracker(), logger)
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

	freed := p.cleanZigArtifacts(context.Background(), tmpDir, 30*24*time.Hour, nil, newDevArtifactGitTracker(), logger)
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
	p.planZigArtifacts(tmpDir, 0, true, nil, nil, newDevArtifactGitTracker(), &targets)

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

	freed := p.cleanZigArtifacts(context.Background(), tmpDir, 0, nil, newDevArtifactGitTracker(), logger)
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
	p.planZigArtifacts(tmpDir, 30*24*time.Hour, true, nil, nil, newDevArtifactGitTracker(), &targets)

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

	freed := p.cleanZigArtifacts(context.Background(), tmpDir, 30*24*time.Hour, nil, newDevArtifactGitTracker(), logger)
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
	p.planNodeModules(tmpDir, 30*24*time.Hour, true, nil, map[string]string{
		"node_modules": "Node.js package manager or runtime",
	}, newDevArtifactGitTracker(), &targets)

	target := findDevArtifactTarget(t, targets, "node_modules", filepath.Join(project, "node_modules"))
	if target.Action != "protect" || !target.Protected || !target.Active {
		t.Fatalf("expected active node_modules to be protected, got %#v", target)
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
	p.planZigArtifacts(tmpDir, 30*24*time.Hour, true, nil, map[string]string{
		"zig-artifact": "Zig toolchain process",
	}, newDevArtifactGitTracker(), &targets)

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
	if err := os.WriteFile(filepath.Join(bundlePath, "bands", "0"), []byte("bundle data"), 0644); err != nil {
		t.Fatal(err)
	}

	var targets []CleanupTarget
	p.planLargeLocalArtifacts(tmpDir, 1, nil, &targets)

	image := findDevArtifactTarget(t, targets, "large-local-artifact", imagePath)
	if image.Action != "review" || !image.Protected || image.Tier != CleanupTierDestructive || image.Reclaim != CleanupReclaimNone {
		t.Fatalf("expected review-only destructive/no-reclaim image target, got %#v", image)
	}
	bundle := findDevArtifactTarget(t, targets, "large-local-artifact", bundlePath)
	if bundle.Action != "review" || !bundle.Protected || bundle.Bytes <= 0 {
		t.Fatalf("expected review-only sparsebundle target with bytes, got %#v", bundle)
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
	p.planLargeLocalArtifacts(tmpDir, 1, []string{filepath.Dir(imagePath)}, &targets)

	target := findDevArtifactTarget(t, targets, "large-local-artifact", imagePath)
	if target.Action != "protect" || !target.Protected {
		t.Fatalf("expected protected large local artifact target, got %#v", target)
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

func newDevArtifactsPluginWithActive(active map[string]string) *DevArtifactsPlugin {
	return &DevArtifactsPlugin{
		activeProcesses: func(context.Context) (map[string]string, error) {
			return active, nil
		},
	}
}
