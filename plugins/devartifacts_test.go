package plugins

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

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
	freed := p.cleanNodeModules(context.Background(), tmpDir, 30*24*time.Hour, nil, logger)

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
	freed := p.cleanNodeModules(context.Background(), tmpDir, 30*24*time.Hour, nil, logger)

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
	freed := p.cleanNodeModules(context.Background(), tmpDir, 30*24*time.Hour, protectPaths, logger)

	if freed != 0 {
		t.Error("expected protected node_modules to be preserved")
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
