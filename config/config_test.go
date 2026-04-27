package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	// Test basic defaults
	if cfg.PollInterval != 60 {
		t.Errorf("expected PollInterval=60, got %d", cfg.PollInterval)
	}

	// Test thresholds
	if cfg.Thresholds.Warning != 80 {
		t.Errorf("expected Warning=80, got %d", cfg.Thresholds.Warning)
	}
	if cfg.Thresholds.Moderate != 85 {
		t.Errorf("expected Moderate=85, got %d", cfg.Thresholds.Moderate)
	}
	if cfg.Thresholds.Aggressive != 90 {
		t.Errorf("expected Aggressive=90, got %d", cfg.Thresholds.Aggressive)
	}
	if cfg.Thresholds.Critical != 95 {
		t.Errorf("expected Critical=95, got %d", cfg.Thresholds.Critical)
	}
	if cfg.Policy.Cooldown != "30m" {
		t.Errorf("expected cooldown=30m, got %q", cfg.Policy.Cooldown)
	}
	if cfg.Policy.StateFile == "" {
		t.Error("expected default state file")
	}

	// Test enable flags
	if !cfg.Enable.Cache {
		t.Error("expected Cache enabled")
	}
	if !cfg.Enable.NixGC {
		t.Error("expected NixGC enabled")
	}
	if !cfg.Enable.Docker {
		t.Error("expected Docker enabled")
	}

	// Platform-specific defaults
	if runtime.GOOS == "darwin" {
		if !cfg.Enable.Lima {
			t.Error("expected Lima enabled on Darwin")
		}
		if !cfg.Enable.Homebrew {
			t.Error("expected Homebrew enabled on Darwin")
		}
		if !cfg.Enable.IOSSimulator {
			t.Error("expected IOSSimulator enabled on Darwin")
		}
	}
}

func TestLoadConfigNonExistent(t *testing.T) {
	cfg, err := LoadConfig("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("unexpected error for non-existent file: %v", err)
	}
	// Should return defaults
	if cfg.PollInterval != 60 {
		t.Errorf("expected default PollInterval=60, got %d", cfg.PollInterval)
	}
}

func TestLoadConfigEmpty(t *testing.T) {
	// Create temp file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty file should use defaults
	if cfg.PollInterval != 60 {
		t.Errorf("expected default PollInterval=60, got %d", cfg.PollInterval)
	}
}

func TestLoadConfigPartial(t *testing.T) {
	// Create temp file with partial config
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	content := `
poll_interval: 30
thresholds:
  warning: 70
`
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should use specified values
	if cfg.PollInterval != 30 {
		t.Errorf("expected PollInterval=30, got %d", cfg.PollInterval)
	}
	if cfg.Thresholds.Warning != 70 {
		t.Errorf("expected Warning=70, got %d", cfg.Thresholds.Warning)
	}

	// Should use defaults for unspecified values
	if cfg.Thresholds.Moderate != 85 {
		t.Errorf("expected default Moderate=85, got %d", cfg.Thresholds.Moderate)
	}
}

func TestSaveConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "subdir", "config.yaml")

	cfg := DefaultConfig()
	cfg.PollInterval = 120

	if err := SaveConfig(cfg, configPath); err != nil {
		t.Fatalf("failed to save config: %v", err)
	}

	// Reload and verify
	loaded, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("failed to load saved config: %v", err)
	}

	if loaded.PollInterval != 120 {
		t.Errorf("expected PollInterval=120, got %d", loaded.PollInterval)
	}
}

func TestLoadConfigInvalid(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	content := `
poll_interval: "not a number"
`
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(configPath)
	if err == nil {
		t.Error("expected error for invalid config")
	}
}

func TestDevArtifactsConfigDefaults(t *testing.T) {
	cfg := DefaultConfig()

	if len(cfg.DevArtifacts.ScanPaths) == 0 {
		t.Error("DevArtifacts.ScanPaths should have default paths")
	}
	if !cfg.DevArtifacts.NodeModules {
		t.Error("DevArtifacts.NodeModules should be true by default")
	}
	if !cfg.DevArtifacts.PythonVenvs {
		t.Error("DevArtifacts.PythonVenvs should be true by default")
	}
	if !cfg.DevArtifacts.RustTargets {
		t.Error("DevArtifacts.RustTargets should be true by default")
	}
	if !cfg.DevArtifacts.ZigArtifacts {
		t.Error("DevArtifacts.ZigArtifacts should be true by default")
	}
	if !cfg.DevArtifacts.GoBuildCache {
		t.Error("DevArtifacts.GoBuildCache should be true by default")
	}
	if !cfg.DevArtifacts.HaskellCache {
		t.Error("DevArtifacts.HaskellCache should be true by default")
	}
	if cfg.DevArtifacts.LMStudioModels {
		t.Error("DevArtifacts.LMStudioModels should be false by default (opt-in)")
	}
	if !cfg.DevArtifacts.LargeLocalArtifacts {
		t.Error("DevArtifacts.LargeLocalArtifacts should be true by default")
	}
	if cfg.DevArtifacts.LargeLocalArtifactMinMB <= 0 {
		t.Error("DevArtifacts.LargeLocalArtifactMinMB should be positive by default")
	}
}

func TestAPFSConfigDefaults(t *testing.T) {
	cfg := DefaultConfig()

	if !cfg.APFS.ThinEnabled {
		t.Error("APFS.ThinEnabled should be true by default")
	}
	if cfg.APFS.MaxThinGB != 50 {
		t.Errorf("APFS.MaxThinGB should be 50, got %d", cfg.APFS.MaxThinGB)
	}
	if cfg.APFS.KeepRecentDays != 1 {
		t.Errorf("APFS.KeepRecentDays should be 1, got %d", cfg.APFS.KeepRecentDays)
	}
	if !cfg.APFS.DeleteOSUpdates {
		t.Error("APFS.DeleteOSUpdates should be true by default")
	}
}

func TestEnableFlagsNewPlugins(t *testing.T) {
	cfg := DefaultConfig()

	if !cfg.Enable.DevArtifacts {
		t.Error("Enable.DevArtifacts should be true by default")
	}
	if !cfg.Enable.Bazel {
		t.Error("Enable.Bazel should be true by default")
	}
	if runtime.GOOS == "darwin" && !cfg.Enable.APFSSnapshots {
		t.Error("Enable.APFSSnapshots should be true on Darwin")
	}
}

func TestLimaCompactDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Lima.CompactOffline {
		t.Error("Lima.CompactOffline should be false by default (opt-in)")
	}
}

func TestPodmanCompactDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Podman.CompactDiskOffline {
		t.Error("Podman.CompactDiskOffline should be false by default (opt-in)")
	}
	if cfg.Podman.CompactMinReclaimGB != 8 {
		t.Errorf("Podman.CompactMinReclaimGB should default to 8, got %d", cfg.Podman.CompactMinReclaimGB)
	}
	if !cfg.Podman.CompactRequireNoActiveContainers {
		t.Error("Podman.CompactRequireNoActiveContainers should default to true")
	}
	if !cfg.Podman.CompactKeepBackupUntilRestart {
		t.Error("Podman.CompactKeepBackupUntilRestart should default to true")
	}
	if len(cfg.Podman.CompactProviderAllowlist) == 0 {
		t.Fatal("Podman.CompactProviderAllowlist should have defaults")
	}
	if cfg.Podman.CompactScratchDir != "" {
		t.Errorf("Podman.CompactScratchDir should default to empty, got %q", cfg.Podman.CompactScratchDir)
	}
	if cfg.Podman.CompactQemuImgPath != "" {
		t.Errorf("Podman.CompactQemuImgPath should default to empty, got %q", cfg.Podman.CompactQemuImgPath)
	}
}

func TestNixPolicyDefaults(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Nix.MinUserGenerations != 5 {
		t.Errorf("Nix.MinUserGenerations should be 5, got %d", cfg.Nix.MinUserGenerations)
	}
	if cfg.Nix.MinSystemGenerations != 3 {
		t.Errorf("Nix.MinSystemGenerations should be 3, got %d", cfg.Nix.MinSystemGenerations)
	}
	if cfg.Nix.DeleteGenerationsOlderThan != "14d" {
		t.Errorf("Nix.DeleteGenerationsOlderThan should be 14d, got %q", cfg.Nix.DeleteGenerationsOlderThan)
	}
	if cfg.Nix.CriticalDeleteGenerationsOlderThan != "3d" {
		t.Errorf("Nix.CriticalDeleteGenerationsOlderThan should be 3d, got %q", cfg.Nix.CriticalDeleteGenerationsOlderThan)
	}
	if cfg.Nix.AllowStoreOptimize {
		t.Error("Nix.AllowStoreOptimize should be false by default")
	}
	if !cfg.Nix.SkipWhenDaemonBusy {
		t.Error("Nix.SkipWhenDaemonBusy should be true by default")
	}
	if cfg.Nix.DaemonBusyBackoff != "30m" {
		t.Errorf("Nix.DaemonBusyBackoff should be 30m, got %q", cfg.Nix.DaemonBusyBackoff)
	}
	if cfg.Nix.MaxGCDuration != "20m" {
		t.Errorf("Nix.MaxGCDuration should be 20m, got %q", cfg.Nix.MaxGCDuration)
	}
	if cfg.Nix.RootAttributionLimit != 20 {
		t.Errorf("Nix.RootAttributionLimit should be 20, got %d", cfg.Nix.RootAttributionLimit)
	}
}

func TestBazelPolicyDefaults(t *testing.T) {
	cfg := DefaultConfig()

	if len(cfg.Bazel.Roots) == 0 {
		t.Fatal("Bazel.Roots should have defaults")
	}
	if len(cfg.Bazel.WorkspaceRoots) != 3 {
		t.Fatalf("Bazel.WorkspaceRoots should have 3 defaults, got %#v", cfg.Bazel.WorkspaceRoots)
	}
	if cfg.Bazel.BazeliskCache == "" {
		t.Fatal("Bazel.BazeliskCache should have a default")
	}
	if cfg.Bazel.MaxTotalGB != 20 {
		t.Errorf("Bazel.MaxTotalGB should be 20, got %d", cfg.Bazel.MaxTotalGB)
	}
	if cfg.Bazel.KeepRecentOutputBases != 5 {
		t.Errorf("Bazel.KeepRecentOutputBases should be 5, got %d", cfg.Bazel.KeepRecentOutputBases)
	}
	if cfg.Bazel.StaleAfter != "14d" {
		t.Errorf("Bazel.StaleAfter should be 14d, got %q", cfg.Bazel.StaleAfter)
	}
	if cfg.Bazel.CriticalStaleAfter != "3d" {
		t.Errorf("Bazel.CriticalStaleAfter should be 3d, got %q", cfg.Bazel.CriticalStaleAfter)
	}
	if len(cfg.Bazel.ProtectWorkspaces) == 0 {
		t.Fatal("Bazel.ProtectWorkspaces should have defaults")
	}
	if !cfg.Bazel.AllowStopIdleServers {
		t.Error("Bazel.AllowStopIdleServers should be true by default")
	}
	if cfg.Bazel.AllowDeleteActiveOutputBases {
		t.Error("Bazel.AllowDeleteActiveOutputBases should be false by default")
	}
}

func TestDarwinDevCacheDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if runtime.GOOS == "darwin" && !cfg.DarwinDevCaches.Enabled {
		t.Error("DarwinDevCaches.Enabled should default to true on Darwin")
	}
	if runtime.GOOS != "darwin" && cfg.DarwinDevCaches.Enabled {
		t.Error("DarwinDevCaches.Enabled should default to false outside Darwin")
	}
	if cfg.DarwinDevCaches.Enforce {
		t.Error("DarwinDevCaches.Enforce should default to false")
	}
	if cfg.DarwinDevCaches.MaxTotalGB != 15 {
		t.Errorf("DarwinDevCaches.MaxTotalGB should default to 15, got %d", cfg.DarwinDevCaches.MaxTotalGB)
	}
	if !cfg.DarwinDevCaches.JetBrains.Enabled {
		t.Error("DarwinDevCaches.JetBrains.Enabled should default to true")
	}
	if cfg.DarwinDevCaches.JetBrains.MaxGB != 8 {
		t.Errorf("DarwinDevCaches.JetBrains.MaxGB should default to 8, got %d", cfg.DarwinDevCaches.JetBrains.MaxGB)
	}
	if !cfg.DarwinDevCaches.JetBrains.KeepActiveVersions {
		t.Error("DarwinDevCaches.JetBrains.KeepActiveVersions should default to true")
	}
	if !cfg.DarwinDevCaches.Playwright.KeepLatestPerFamily {
		t.Error("DarwinDevCaches.Playwright.KeepLatestPerFamily should default to true")
	}
	if cfg.DarwinDevCaches.Bazelisk.KeepLatest != 2 {
		t.Errorf("DarwinDevCaches.Bazelisk.KeepLatest should default to 2, got %d", cfg.DarwinDevCaches.Bazelisk.KeepLatest)
	}
	if !cfg.DarwinDevCaches.VSCode.Enabled {
		t.Error("DarwinDevCaches.VSCode.Enabled should default to true")
	}
	if cfg.DarwinDevCaches.VSCode.StaleAfterDays != 14 {
		t.Errorf("DarwinDevCaches.VSCode.StaleAfterDays should default to 14, got %d", cfg.DarwinDevCaches.VSCode.StaleAfterDays)
	}
	if !cfg.DarwinDevCaches.VSCode.KeepActiveVersions {
		t.Error("DarwinDevCaches.VSCode.KeepActiveVersions should default to true")
	}
	if !cfg.DarwinDevCaches.Cursor.Enabled {
		t.Error("DarwinDevCaches.Cursor.Enabled should default to true")
	}
	if cfg.DarwinDevCaches.Cursor.StaleAfterDays != 14 {
		t.Errorf("DarwinDevCaches.Cursor.StaleAfterDays should default to 14, got %d", cfg.DarwinDevCaches.Cursor.StaleAfterDays)
	}
	if !cfg.DarwinDevCaches.Cursor.KeepActiveVersions {
		t.Error("DarwinDevCaches.Cursor.KeepActiveVersions should default to true")
	}
}

func TestLoadConfigWithNewFields(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	content := `
enable:
  dev_artifacts: true
  bazel: true
  apfs_snapshots: true
dev_artifacts:
  scan_paths:
    - ~/git
    - ~/src
  node_modules: true
  python_venvs: true
  rust_targets: false
  go_build_cache: true
  haskell_cache: false
  lmstudio_models: true
  protect_paths:
    - ~/git/important
apfs:
  thin_enabled: true
  max_thin_gb: 30
  keep_recent_days: 2
  delete_os_updates: false
lima:
  compact_offline: true
podman:
  compact_disk_offline: true
  compact_min_reclaim_gb: 12
  compact_require_no_active_containers: false
  compact_keep_backup_until_restart: false
  compact_scratch_dir: /Volumes/TinylandSSD/tinyland-cleanup-podman
  compact_qemu_img_path: /nix/store/example-qemu/bin/qemu-img
  compact_provider_allowlist:
    - applehv
nix:
  min_user_generations: 7
  min_system_generations: 4
  delete_generations_older_than: 21d
  critical_delete_generations_older_than: 5d
  allow_store_optimize: true
  skip_when_daemon_busy: false
  daemon_busy_backoff: 45m
  max_gc_duration: 10m
  root_attribution_limit: 8
bazel:
  roots:
    - ~/custom-bazel
  workspace_roots:
    - ~/custom-workspaces
  bazelisk_cache: ~/custom-bazelisk
  max_total_gb: 30
  keep_recent_output_bases: 2
  stale_after: 10d
  critical_stale_after: 2d
  protect_workspaces:
    - ~/git/important
  allow_stop_idle_servers: false
  allow_delete_active_output_bases: true
darwin_dev_caches:
  enabled: true
  enforce: true
  max_total_gb: 20
  jetbrains:
    enabled: true
    max_gb: 10
    stale_after_days: 21
    keep_active_versions: true
  playwright:
    enabled: false
    keep_latest_per_family: false
  bazelisk:
    enabled: true
    keep_latest: 3
  pip:
    enabled: true
    stale_after_days: 7
  vscode:
    enabled: true
    stale_after_days: 10
    keep_active_versions: false
  cursor:
    enabled: false
    stale_after_days: 5
`
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !cfg.Enable.DevArtifacts {
		t.Error("Enable.DevArtifacts should be true")
	}
	if !cfg.Enable.Bazel {
		t.Error("Enable.Bazel should be true")
	}
	if !cfg.Enable.APFSSnapshots {
		t.Error("Enable.APFSSnapshots should be true")
	}
	if len(cfg.DevArtifacts.ScanPaths) != 2 {
		t.Errorf("expected 2 scan paths, got %d", len(cfg.DevArtifacts.ScanPaths))
	}
	if cfg.DevArtifacts.RustTargets {
		t.Error("DevArtifacts.RustTargets should be false per config")
	}
	if !cfg.DevArtifacts.ZigArtifacts {
		t.Error("DevArtifacts.ZigArtifacts should stay true by default when omitted")
	}
	if !cfg.DevArtifacts.LMStudioModels {
		t.Error("DevArtifacts.LMStudioModels should be true per config")
	}
	if len(cfg.DevArtifacts.ProtectPaths) != 1 {
		t.Errorf("expected 1 protect path, got %d", len(cfg.DevArtifacts.ProtectPaths))
	}
	if cfg.APFS.MaxThinGB != 30 {
		t.Errorf("APFS.MaxThinGB should be 30, got %d", cfg.APFS.MaxThinGB)
	}
	if cfg.APFS.KeepRecentDays != 2 {
		t.Errorf("APFS.KeepRecentDays should be 2, got %d", cfg.APFS.KeepRecentDays)
	}
	if cfg.APFS.DeleteOSUpdates {
		t.Error("APFS.DeleteOSUpdates should be false per config")
	}
	if !cfg.Lima.CompactOffline {
		t.Error("Lima.CompactOffline should be true per config")
	}
	if !cfg.Podman.CompactDiskOffline {
		t.Error("Podman.CompactDiskOffline should be true per config")
	}
	if cfg.Podman.CompactMinReclaimGB != 12 {
		t.Errorf("Podman.CompactMinReclaimGB should be 12 per config, got %d", cfg.Podman.CompactMinReclaimGB)
	}
	if cfg.Podman.CompactRequireNoActiveContainers {
		t.Error("Podman.CompactRequireNoActiveContainers should be false per config")
	}
	if cfg.Podman.CompactKeepBackupUntilRestart {
		t.Error("Podman.CompactKeepBackupUntilRestart should be false per config")
	}
	if len(cfg.Podman.CompactProviderAllowlist) != 1 || cfg.Podman.CompactProviderAllowlist[0] != "applehv" {
		t.Errorf("unexpected Podman.CompactProviderAllowlist: %#v", cfg.Podman.CompactProviderAllowlist)
	}
	if cfg.Podman.CompactScratchDir != "/Volumes/TinylandSSD/tinyland-cleanup-podman" {
		t.Errorf("Podman.CompactScratchDir should be custom path, got %q", cfg.Podman.CompactScratchDir)
	}
	if cfg.Podman.CompactQemuImgPath != "/nix/store/example-qemu/bin/qemu-img" {
		t.Errorf("Podman.CompactQemuImgPath should be custom path, got %q", cfg.Podman.CompactQemuImgPath)
	}
	if cfg.Nix.MinUserGenerations != 7 {
		t.Errorf("Nix.MinUserGenerations should be 7 per config, got %d", cfg.Nix.MinUserGenerations)
	}
	if cfg.Nix.MinSystemGenerations != 4 {
		t.Errorf("Nix.MinSystemGenerations should be 4 per config, got %d", cfg.Nix.MinSystemGenerations)
	}
	if cfg.Nix.DeleteGenerationsOlderThan != "21d" {
		t.Errorf("Nix.DeleteGenerationsOlderThan should be 21d per config, got %q", cfg.Nix.DeleteGenerationsOlderThan)
	}
	if cfg.Nix.CriticalDeleteGenerationsOlderThan != "5d" {
		t.Errorf("Nix.CriticalDeleteGenerationsOlderThan should be 5d per config, got %q", cfg.Nix.CriticalDeleteGenerationsOlderThan)
	}
	if !cfg.Nix.AllowStoreOptimize {
		t.Error("Nix.AllowStoreOptimize should be true per config")
	}
	if cfg.Nix.SkipWhenDaemonBusy {
		t.Error("Nix.SkipWhenDaemonBusy should be false per config")
	}
	if cfg.Nix.DaemonBusyBackoff != "45m" {
		t.Errorf("Nix.DaemonBusyBackoff should be 45m per config, got %q", cfg.Nix.DaemonBusyBackoff)
	}
	if cfg.Nix.MaxGCDuration != "10m" {
		t.Errorf("Nix.MaxGCDuration should be 10m per config, got %q", cfg.Nix.MaxGCDuration)
	}
	if cfg.Nix.RootAttributionLimit != 8 {
		t.Errorf("Nix.RootAttributionLimit should be 8 per config, got %d", cfg.Nix.RootAttributionLimit)
	}
	if len(cfg.Bazel.Roots) != 1 || cfg.Bazel.Roots[0] != "~/custom-bazel" {
		t.Errorf("unexpected Bazel.Roots: %#v", cfg.Bazel.Roots)
	}
	if len(cfg.Bazel.WorkspaceRoots) != 1 || cfg.Bazel.WorkspaceRoots[0] != "~/custom-workspaces" {
		t.Errorf("unexpected Bazel.WorkspaceRoots: %#v", cfg.Bazel.WorkspaceRoots)
	}
	if cfg.Bazel.BazeliskCache != "~/custom-bazelisk" {
		t.Errorf("Bazel.BazeliskCache should be custom path, got %q", cfg.Bazel.BazeliskCache)
	}
	if cfg.Bazel.MaxTotalGB != 30 {
		t.Errorf("Bazel.MaxTotalGB should be 30 per config, got %d", cfg.Bazel.MaxTotalGB)
	}
	if cfg.Bazel.KeepRecentOutputBases != 2 {
		t.Errorf("Bazel.KeepRecentOutputBases should be 2 per config, got %d", cfg.Bazel.KeepRecentOutputBases)
	}
	if cfg.Bazel.StaleAfter != "10d" {
		t.Errorf("Bazel.StaleAfter should be 10d per config, got %q", cfg.Bazel.StaleAfter)
	}
	if cfg.Bazel.CriticalStaleAfter != "2d" {
		t.Errorf("Bazel.CriticalStaleAfter should be 2d per config, got %q", cfg.Bazel.CriticalStaleAfter)
	}
	if len(cfg.Bazel.ProtectWorkspaces) != 1 || cfg.Bazel.ProtectWorkspaces[0] != "~/git/important" {
		t.Errorf("unexpected Bazel.ProtectWorkspaces: %#v", cfg.Bazel.ProtectWorkspaces)
	}
	if cfg.Bazel.AllowStopIdleServers {
		t.Error("Bazel.AllowStopIdleServers should be false per config")
	}
	if !cfg.Bazel.AllowDeleteActiveOutputBases {
		t.Error("Bazel.AllowDeleteActiveOutputBases should be true per config")
	}
	if !cfg.DarwinDevCaches.Enabled {
		t.Error("DarwinDevCaches.Enabled should be true per config")
	}
	if !cfg.DarwinDevCaches.Enforce {
		t.Error("DarwinDevCaches.Enforce should be true per config")
	}
	if cfg.DarwinDevCaches.MaxTotalGB != 20 {
		t.Errorf("DarwinDevCaches.MaxTotalGB should be 20 per config, got %d", cfg.DarwinDevCaches.MaxTotalGB)
	}
	if cfg.DarwinDevCaches.JetBrains.MaxGB != 10 {
		t.Errorf("DarwinDevCaches.JetBrains.MaxGB should be 10 per config, got %d", cfg.DarwinDevCaches.JetBrains.MaxGB)
	}
	if cfg.DarwinDevCaches.JetBrains.StaleAfterDays != 21 {
		t.Errorf("DarwinDevCaches.JetBrains.StaleAfterDays should be 21 per config, got %d", cfg.DarwinDevCaches.JetBrains.StaleAfterDays)
	}
	if cfg.DarwinDevCaches.Playwright.Enabled {
		t.Error("DarwinDevCaches.Playwright.Enabled should be false per config")
	}
	if cfg.DarwinDevCaches.Bazelisk.KeepLatest != 3 {
		t.Errorf("DarwinDevCaches.Bazelisk.KeepLatest should be 3 per config, got %d", cfg.DarwinDevCaches.Bazelisk.KeepLatest)
	}
	if cfg.DarwinDevCaches.Pip.StaleAfterDays != 7 {
		t.Errorf("DarwinDevCaches.Pip.StaleAfterDays should be 7 per config, got %d", cfg.DarwinDevCaches.Pip.StaleAfterDays)
	}
	if cfg.DarwinDevCaches.VSCode.StaleAfterDays != 10 {
		t.Errorf("DarwinDevCaches.VSCode.StaleAfterDays should be 10 per config, got %d", cfg.DarwinDevCaches.VSCode.StaleAfterDays)
	}
	if cfg.DarwinDevCaches.VSCode.KeepActiveVersions {
		t.Error("DarwinDevCaches.VSCode.KeepActiveVersions should be false per config")
	}
	if cfg.DarwinDevCaches.Cursor.Enabled {
		t.Error("DarwinDevCaches.Cursor.Enabled should be false per config")
	}
	if cfg.DarwinDevCaches.Cursor.StaleAfterDays != 5 {
		t.Errorf("DarwinDevCaches.Cursor.StaleAfterDays should be 5 per config, got %d", cfg.DarwinDevCaches.Cursor.StaleAfterDays)
	}
}
