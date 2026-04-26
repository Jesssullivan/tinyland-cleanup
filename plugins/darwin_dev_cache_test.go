//go:build darwin

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

func TestDarwinDeveloperCacheTargetsClassifyProtectedVersions(t *testing.T) {
	home := t.TempDir()
	cfg := config.DefaultConfig().DarwinDevCaches

	writeCacheFile(t, home, "Library/Caches/JetBrains/IntelliJIdea2024.3/cache.bin", "jetbrains")
	writeCacheFile(t, home, "Library/Caches/ms-playwright/chromium-1148/browser.bin", "old chromium")
	writeCacheFile(t, home, "Library/Caches/ms-playwright/chromium-1149/browser.bin", "new chromium")
	writeCacheFile(t, home, "Library/Caches/bazelisk/v1/bin/bazel", "v1")
	writeCacheFile(t, home, "Library/Caches/bazelisk/v2/bin/bazel", "v2")
	writeCacheFile(t, home, "Library/Caches/bazelisk/v3/bin/bazel", "v3")
	writeCacheFile(t, home, "Library/Caches/pip/http/cache.bin", "pip")

	now := time.Now()
	mustChtimes(t, filepath.Join(home, "Library/Caches/ms-playwright/chromium-1148"), now.Add(-2*time.Hour))
	mustChtimes(t, filepath.Join(home, "Library/Caches/ms-playwright/chromium-1149"), now)
	mustChtimes(t, filepath.Join(home, "Library/Caches/bazelisk/v1"), now.Add(-3*time.Hour))
	mustChtimes(t, filepath.Join(home, "Library/Caches/bazelisk/v2"), now.Add(-2*time.Hour))
	mustChtimes(t, filepath.Join(home, "Library/Caches/bazelisk/v3"), now)

	plugin := &CachePlugin{}
	targets := plugin.darwinDeveloperCacheTargets(home, cfg, map[string]bool{"goland": true}, LevelWarning)

	jetbrains := findCleanupTarget(t, targets, "jetbrains", "IntelliJIdea2024.3")
	if !jetbrains.Active || !jetbrains.Protected {
		t.Fatalf("expected active JetBrains cache to be protected: %#v", jetbrains)
	}

	oldChromium := findCleanupTarget(t, targets, "playwright", "chromium-1148")
	if oldChromium.Protected {
		t.Fatalf("expected old Playwright revision to be reviewable: %#v", oldChromium)
	}
	newChromium := findCleanupTarget(t, targets, "playwright", "chromium-1149")
	if !newChromium.Protected {
		t.Fatalf("expected newest Playwright revision to be protected: %#v", newChromium)
	}

	bazeliskV1 := findCleanupTarget(t, targets, "bazelisk", "v1")
	if bazeliskV1.Protected {
		t.Fatalf("expected oldest Bazelisk cache to be reviewable: %#v", bazeliskV1)
	}
	bazeliskV2 := findCleanupTarget(t, targets, "bazelisk", "v2")
	bazeliskV3 := findCleanupTarget(t, targets, "bazelisk", "v3")
	if !bazeliskV2.Protected || !bazeliskV3.Protected {
		t.Fatalf("expected two newest Bazelisk caches to be protected: v2=%#v v3=%#v", bazeliskV2, bazeliskV3)
	}

	pip := findCleanupTarget(t, targets, "pip", "pip")
	if pip.Bytes <= 0 {
		t.Fatalf("expected pip cache size to be measured: %#v", pip)
	}
}

func TestDarwinDeveloperCacheTargetsOptInEnforcement(t *testing.T) {
	home := t.TempDir()
	cfg := config.DefaultConfig().DarwinDevCaches
	cfg.Enforce = true

	writeCacheFile(t, home, "Library/Caches/ms-playwright/chromium-1148/browser.bin", "old chromium")
	writeCacheFile(t, home, "Library/Caches/ms-playwright/chromium-1149/browser.bin", "new chromium")
	writeCacheFile(t, home, "Library/Caches/bazelisk/v1/bin/bazel", "v1")
	writeCacheFile(t, home, "Library/Caches/bazelisk/v2/bin/bazel", "v2")
	writeCacheFile(t, home, "Library/Caches/bazelisk/v3/bin/bazel", "v3")
	writeCacheFile(t, home, "Library/Caches/JetBrains/IntelliJIdea2024.3/cache.bin", "jetbrains")

	now := time.Now()
	mustChtimes(t, filepath.Join(home, "Library/Caches/ms-playwright/chromium-1148"), now.Add(-2*time.Hour))
	mustChtimes(t, filepath.Join(home, "Library/Caches/ms-playwright/chromium-1149"), now)
	mustChtimes(t, filepath.Join(home, "Library/Caches/bazelisk/v1"), now.Add(-3*time.Hour))
	mustChtimes(t, filepath.Join(home, "Library/Caches/bazelisk/v2"), now.Add(-2*time.Hour))
	mustChtimes(t, filepath.Join(home, "Library/Caches/bazelisk/v3"), now)

	plugin := &CachePlugin{}
	targets := plugin.darwinDeveloperCacheTargets(home, cfg, map[string]bool{}, LevelModerate)

	oldChromium := findCleanupTarget(t, targets, "playwright", "chromium-1148")
	if oldChromium.Action != "delete" || oldChromium.Protected {
		t.Fatalf("expected old Playwright revision to be an opt-in delete target: %#v", oldChromium)
	}
	newChromium := findCleanupTarget(t, targets, "playwright", "chromium-1149")
	if newChromium.Action != "protect" || !newChromium.Protected {
		t.Fatalf("expected newest Playwright revision to be protected: %#v", newChromium)
	}
	bazeliskV1 := findCleanupTarget(t, targets, "bazelisk", "v1")
	if bazeliskV1.Action != "delete" || bazeliskV1.Protected {
		t.Fatalf("expected oldest Bazelisk cache to be an opt-in delete target: %#v", bazeliskV1)
	}
	jetbrains := findCleanupTarget(t, targets, "jetbrains", "IntelliJIdea2024.3")
	if jetbrains.Action != "protect" || !jetbrains.Protected {
		t.Fatalf("expected JetBrains cache to require aggressive or critical level: %#v", jetbrains)
	}
}

func TestDarwinCacheEntriesOverBudgetSelectsOldestEntries(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	entries := []darwinCacheEntry{
		{path: "/cache/new", modTime: now, bytes: 600 * 1024 * 1024},
		{path: "/cache/mid", modTime: now.Add(-1 * time.Hour), bytes: 600 * 1024 * 1024},
		{path: "/cache/old", modTime: now.Add(-2 * time.Hour), bytes: 600 * 1024 * 1024},
	}

	overBudget := darwinCacheEntriesOverBudget(entries, 1, 1)
	if !overBudget["/cache/old"] || overBudget["/cache/new"] {
		t.Fatalf("expected oldest entry over budget while newest is protected: %#v", overBudget)
	}
	if len(overBudget) != 2 {
		t.Fatalf("expected two entries to bring cache under budget, got %#v", overBudget)
	}
}

func TestDarwinDeveloperCacheTargetsEditorCaches(t *testing.T) {
	home := t.TempDir()
	cfg := config.DefaultConfig().DarwinDevCaches
	cfg.Enforce = true

	vscodeCache := filepath.Join(home, "Library/Application Support/Code/Cache")
	vscodeSettings := filepath.Join(home, "Library/Application Support/Code/User/settings.json")
	cursorCache := filepath.Join(home, "Library/Application Support/Cursor/Cache")
	writeCacheFile(t, home, "Library/Application Support/Code/Cache/cache.bin", "vscode")
	writeCacheFile(t, home, "Library/Application Support/Code/User/settings.json", "{}")
	writeCacheFile(t, home, "Library/Application Support/Cursor/Cache/cache.bin", "cursor")

	oldTime := time.Now().Add(-30 * 24 * time.Hour)
	mustChtimes(t, vscodeCache, oldTime)
	mustChtimes(t, cursorCache, oldTime)

	plugin := &CachePlugin{}
	targets := plugin.darwinDeveloperCacheTargets(home, cfg, map[string]bool{"code helper": true}, LevelModerate)

	vscode := findCleanupTarget(t, targets, "vscode-cache", "Code/Cache")
	if !vscode.Active || !vscode.Protected || vscode.Action != "protect" {
		t.Fatalf("expected active VS Code cache to be protected: %#v", vscode)
	}
	cursor := findCleanupTarget(t, targets, "cursor-cache", "Cursor/Cache")
	if cursor.Active || cursor.Protected || cursor.Action != "delete" {
		t.Fatalf("expected inactive stale Cursor cache to be an opt-in delete target: %#v", cursor)
	}
	if _, ok := findCleanupTargetMaybe(targets, "vscode-cache", "Code/User"); ok {
		t.Fatalf("editor settings directory should not be emitted as a cache target")
	}
	if _, err := os.Stat(vscodeSettings); err != nil {
		t.Fatalf("test settings fixture should exist: %v", err)
	}
}

func TestCleanupDarwinDeveloperCacheTargetsDeletesOnlyEligibleTargets(t *testing.T) {
	home := t.TempDir()
	cfg := config.DefaultConfig().DarwinDevCaches
	cfg.Enforce = true

	oldBrowser := filepath.Join(home, "Library/Caches/ms-playwright/chromium-1148")
	newBrowser := filepath.Join(home, "Library/Caches/ms-playwright/chromium-1149")
	writeCacheFile(t, home, "Library/Caches/ms-playwright/chromium-1148/browser.bin", "old chromium")
	writeCacheFile(t, home, "Library/Caches/ms-playwright/chromium-1149/browser.bin", "new chromium")

	now := time.Now()
	mustChtimes(t, oldBrowser, now.Add(-2*time.Hour))
	mustChtimes(t, newBrowser, now)

	plugin := &CachePlugin{}
	result := plugin.cleanupDarwinDeveloperCacheTargets(context.Background(), LevelModerate, home, cfg, nilLogger())
	if result.Error != nil {
		t.Fatalf("cleanup failed: %v", result.Error)
	}
	if result.ItemsCleaned != 1 {
		t.Fatalf("expected one deleted target, got %d", result.ItemsCleaned)
	}
	if pathExists(oldBrowser) {
		t.Fatalf("expected old Playwright revision to be deleted")
	}
	if !pathExists(newBrowser) {
		t.Fatalf("expected newest Playwright revision to remain")
	}
	if result.EstimatedBytesFreed <= 0 || result.BytesFreed <= 0 {
		t.Fatalf("expected positive byte accounting, got %#v", result)
	}
}

func TestHomebrewPlanTargetUsesDryRunEstimate(t *testing.T) {
	target := homebrewPlanTarget(LevelCritical, "/tmp/homebrew-cache", 10, 50, true)

	if target.Action != "clean-stale-files" {
		t.Fatalf("expected stale-file cleanup action, got %#v", target)
	}
	if target.Bytes != 50 {
		t.Fatalf("expected dry-run estimate to win over cache size, got %#v", target)
	}
	if target.Tier != CleanupTierWarm {
		t.Fatalf("expected Homebrew cleanup to be warm tier, got %#v", target)
	}
}

func TestHomebrewPlanTargetTrustsZeroDryRunEstimate(t *testing.T) {
	target := homebrewPlanTarget(LevelCritical, "/tmp/homebrew-cache", 50, 0, true)

	if target.Bytes != 0 || !target.Protected || target.Action != "keep" {
		t.Fatalf("expected zero dry-run estimate to become a kept target after plan normalization, got %#v", target)
	}
}

func TestIOSSimulatorPlanTargetsProtectsActiveWork(t *testing.T) {
	root := t.TempDir()
	devicePath := filepath.Join(root, "Devices")
	runtimesPath := filepath.Join(root, "Runtimes")
	writeFileAt(t, filepath.Join(devicePath, "device.log"), "simulator log")
	writeFileAt(t, filepath.Join(runtimesPath, "runtime.bin"), "runtime")

	targets := iosSimulatorPlanTargets(LevelCritical, devicePath, runtimesPath, true, true)

	for _, target := range targets {
		if !target.Active || !target.Protected || target.Action != "protect" {
			t.Fatalf("expected active simulator work to protect every target: %#v", targets)
		}
		if target.HostReclaimsSpace == nil || *target.HostReclaimsSpace {
			t.Fatalf("expected protected target to report no host reclaim: %#v", target)
		}
	}
	if estimated := cleanupTargetEstimatedBytes(targets); estimated != 0 {
		t.Fatalf("expected no estimated reclaim while active work is protected, got %d", estimated)
	}
}

func TestXcodePlanTargetsCriticalKeepsNewestDeviceSupport(t *testing.T) {
	xcodeDir := t.TempDir()
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)

	logPath := filepath.Join(xcodeDir, "Logs", "build.log")
	derivedPath := filepath.Join(xcodeDir, "DerivedData", "index.bin")
	archivePath := filepath.Join(xcodeDir, "Archives", "archive.xcarchive")
	writeFileAt(t, logPath, "old log")
	writeSparseFileAt(t, derivedPath, 501*1024*1024)
	writeSparseFileAt(t, archivePath, 501*1024*1024)
	mustChtimes(t, logPath, now.Add(-8*24*time.Hour))

	for i, name := range []string{"17.0", "16.4", "15.5"} {
		path := filepath.Join(xcodeDir, "iOS DeviceSupport", name, "symbols.bin")
		writeFileAt(t, path, name)
		mustChtimes(t, filepath.Dir(path), now.Add(time.Duration(-i)*time.Hour))
	}

	targets := xcodePlanTargets(LevelCritical, xcodeDir, false, now)

	logs := findCleanupTarget(t, targets, "xcode-logs", "old Xcode logs")
	if logs.Action != "delete_old_logs" || logs.Protected {
		t.Fatalf("expected old Xcode logs to be eligible: %#v", logs)
	}
	derived := findCleanupTarget(t, targets, "xcode-derived-data", "DerivedData")
	if derived.Action != "delete_derived_data" || derived.Protected {
		t.Fatalf("expected large DerivedData to be eligible: %#v", derived)
	}
	archives := findCleanupTarget(t, targets, "xcode-archives", "Archives")
	if archives.Action != "delete_archives" || archives.Protected {
		t.Fatalf("expected large Archives to be eligible: %#v", archives)
	}
	newest := findCleanupTarget(t, targets, "xcode-device-support", "17.0")
	if newest.Action != "keep" || !newest.Protected {
		t.Fatalf("expected newest DeviceSupport to be kept: %#v", newest)
	}
	oldest := findCleanupTarget(t, targets, "xcode-device-support", "15.5")
	if oldest.Action != "delete_device_support" || oldest.Protected {
		t.Fatalf("expected oldest DeviceSupport to be eligible: %#v", oldest)
	}
	if estimated := cleanupTargetEstimatedBytes(targets); estimated <= 0 {
		t.Fatalf("expected positive estimated reclaim, got %d from %#v", estimated, targets)
	}
}

func writeCacheFile(t *testing.T, home, relPath, content string) {
	t.Helper()

	path := filepath.Join(home, relPath)
	writeFileAt(t, path, content)
}

func writeFileAt(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func writeSparseFileAt(t *testing.T, path string, size int64) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func mustChtimes(t *testing.T, path string, modTime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}

func nilLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func findCleanupTarget(t *testing.T, targets []CleanupTarget, targetType, name string) CleanupTarget {
	t.Helper()
	target, ok := findCleanupTargetMaybe(targets, targetType, name)
	if ok {
		return target
	}
	t.Fatalf("target %s/%s not found in %#v", targetType, name, targets)
	return CleanupTarget{}
}

func findCleanupTargetMaybe(targets []CleanupTarget, targetType, name string) (CleanupTarget, bool) {
	for _, target := range targets {
		if target.Type == targetType && target.Name == name {
			return target, true
		}
	}
	return CleanupTarget{}, false
}
