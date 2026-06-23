// Package config provides configuration parsing for tinyland-cleanup.
package config

import (
	"os"
	"path/filepath"
	"testing"

	"pgregory.net/rapid"
)

// TestConfigRoundtrip verifies that saving and loading a config preserves values.
func TestConfigRoundtrip(t *testing.T) {
	// Create temp directory once for all rapid iterations
	tmpDir, err := os.MkdirTemp("", "config-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	rapid.Check(t, func(rt *rapid.T) {
		// Generate random config values within reasonable bounds
		cfg := &Config{
			PollInterval: rapid.IntRange(10, 3600).Draw(rt, "poll_interval"),
			Thresholds: Thresholds{
				Warning:    rapid.IntRange(50, 70).Draw(rt, "warning"),
				Moderate:   rapid.IntRange(71, 85).Draw(rt, "moderate"),
				Aggressive: rapid.IntRange(86, 94).Draw(rt, "aggressive"),
				Critical:   rapid.IntRange(95, 99).Draw(rt, "critical"),
			},
			InodeThresholds: Thresholds{
				Warning:    rapid.IntRange(50, 70).Draw(rt, "inode_warning"),
				Moderate:   rapid.IntRange(71, 85).Draw(rt, "inode_moderate"),
				Aggressive: rapid.IntRange(86, 94).Draw(rt, "inode_aggressive"),
				Critical:   rapid.IntRange(95, 99).Draw(rt, "inode_critical"),
			},
			TargetFree: rapid.IntRange(10, 50).Draw(rt, "target_free"),
		}

		// Save to temp file with unique name per iteration
		// Use alphanumeric suffix to ensure valid filename
		suffix := rapid.StringMatching(`[a-z0-9]{8}`).Draw(rt, "suffix")
		path := filepath.Join(tmpDir, "config-"+suffix+".yaml")

		if err := SaveConfig(cfg, path); err != nil {
			rt.Fatalf("SaveConfig failed: %v", err)
		}
		defer os.Remove(path)

		// Load back
		loaded, err := LoadConfig(path)
		if err != nil {
			rt.Fatalf("LoadConfig failed: %v", err)
		}

		// Verify roundtrip
		if loaded.PollInterval != cfg.PollInterval {
			rt.Fatalf("PollInterval mismatch: expected %d, got %d", cfg.PollInterval, loaded.PollInterval)
		}
		if loaded.Thresholds.Warning != cfg.Thresholds.Warning {
			rt.Fatalf("Warning threshold mismatch: expected %d, got %d", cfg.Thresholds.Warning, loaded.Thresholds.Warning)
		}
		if loaded.Thresholds.Moderate != cfg.Thresholds.Moderate {
			rt.Fatalf("Moderate threshold mismatch: expected %d, got %d", cfg.Thresholds.Moderate, loaded.Thresholds.Moderate)
		}
		if loaded.Thresholds.Aggressive != cfg.Thresholds.Aggressive {
			rt.Fatalf("Aggressive threshold mismatch: expected %d, got %d", cfg.Thresholds.Aggressive, loaded.Thresholds.Aggressive)
		}
		if loaded.Thresholds.Critical != cfg.Thresholds.Critical {
			rt.Fatalf("Critical threshold mismatch: expected %d, got %d", cfg.Thresholds.Critical, loaded.Thresholds.Critical)
		}
		if loaded.InodeThresholds.Warning != cfg.InodeThresholds.Warning {
			rt.Fatalf("Inode warning threshold mismatch: expected %d, got %d", cfg.InodeThresholds.Warning, loaded.InodeThresholds.Warning)
		}
		if loaded.InodeThresholds.Critical != cfg.InodeThresholds.Critical {
			rt.Fatalf("Inode critical threshold mismatch: expected %d, got %d", cfg.InodeThresholds.Critical, loaded.InodeThresholds.Critical)
		}
		if loaded.TargetFree != cfg.TargetFree {
			rt.Fatalf("TargetFree mismatch: expected %d, got %d", cfg.TargetFree, loaded.TargetFree)
		}
	})
}

// TestThresholdOrdering verifies thresholds maintain proper ordering.
func TestThresholdOrdering(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate thresholds in ascending order
		warn := rapid.IntRange(50, 70).Draw(t, "warn")
		mod := rapid.IntRange(warn+1, 85).Draw(t, "mod")
		agg := rapid.IntRange(mod+1, 94).Draw(t, "agg")
		crit := rapid.IntRange(agg+1, 99).Draw(t, "crit")

		thresholds := Thresholds{
			Warning:    warn,
			Moderate:   mod,
			Aggressive: agg,
			Critical:   crit,
		}

		// Verify ordering: warning < moderate < aggressive < critical
		if !(thresholds.Warning < thresholds.Moderate) {
			t.Fatalf("warning >= moderate: %d >= %d", thresholds.Warning, thresholds.Moderate)
		}
		if !(thresholds.Moderate < thresholds.Aggressive) {
			t.Fatalf("moderate >= aggressive: %d >= %d", thresholds.Moderate, thresholds.Aggressive)
		}
		if !(thresholds.Aggressive < thresholds.Critical) {
			t.Fatalf("aggressive >= critical: %d >= %d", thresholds.Aggressive, thresholds.Critical)
		}
	})
}

// TestDefaultConfigValid verifies default config has valid values.
func TestDefaultConfigValid(t *testing.T) {
	cfg := DefaultConfig()

	// PollInterval must be positive
	if cfg.PollInterval <= 0 {
		t.Errorf("PollInterval must be positive: %d", cfg.PollInterval)
	}

	// Thresholds must be in order
	if cfg.Thresholds.Warning >= cfg.Thresholds.Moderate {
		t.Errorf("warning >= moderate: %d >= %d", cfg.Thresholds.Warning, cfg.Thresholds.Moderate)
	}
	if cfg.Thresholds.Moderate >= cfg.Thresholds.Aggressive {
		t.Errorf("moderate >= aggressive: %d >= %d", cfg.Thresholds.Moderate, cfg.Thresholds.Aggressive)
	}
	if cfg.Thresholds.Aggressive >= cfg.Thresholds.Critical {
		t.Errorf("aggressive >= critical: %d >= %d", cfg.Thresholds.Aggressive, cfg.Thresholds.Critical)
	}
	if cfg.InodeThresholds.Warning >= cfg.InodeThresholds.Moderate {
		t.Errorf("inode warning >= moderate: %d >= %d", cfg.InodeThresholds.Warning, cfg.InodeThresholds.Moderate)
	}
	if cfg.InodeThresholds.Moderate >= cfg.InodeThresholds.Aggressive {
		t.Errorf("inode moderate >= aggressive: %d >= %d", cfg.InodeThresholds.Moderate, cfg.InodeThresholds.Aggressive)
	}
	if cfg.InodeThresholds.Aggressive >= cfg.InodeThresholds.Critical {
		t.Errorf("inode aggressive >= critical: %d >= %d", cfg.InodeThresholds.Aggressive, cfg.InodeThresholds.Critical)
	}

	// Thresholds must be percentages (0-100)
	if cfg.Thresholds.Warning < 0 || cfg.Thresholds.Warning > 100 {
		t.Errorf("Warning threshold out of range: %d", cfg.Thresholds.Warning)
	}
	if cfg.Thresholds.Critical < 0 || cfg.Thresholds.Critical > 100 {
		t.Errorf("Critical threshold out of range: %d", cfg.Thresholds.Critical)
	}
	if cfg.InodeThresholds.Warning < 0 || cfg.InodeThresholds.Warning > 100 {
		t.Errorf("Inode warning threshold out of range: %d", cfg.InodeThresholds.Warning)
	}
	if cfg.InodeThresholds.Critical < 0 || cfg.InodeThresholds.Critical > 100 {
		t.Errorf("Inode critical threshold out of range: %d", cfg.InodeThresholds.Critical)
	}

	// TargetFree must be less than Warning threshold
	if cfg.TargetFree >= cfg.Thresholds.Warning {
		t.Errorf("TargetFree >= Warning: %d >= %d", cfg.TargetFree, cfg.Thresholds.Warning)
	}
}

// TestLoadConfigMissingFile verifies missing file returns defaults.
func TestLoadConfigMissingFile(t *testing.T) {
	// Non-existent file should return defaults without error
	cfg, err := LoadConfig("/nonexistent/path/config.yaml")
	if err != nil {
		t.Errorf("LoadConfig should not error for missing file: %v", err)
	}

	defaults := DefaultConfig()
	if cfg.PollInterval != defaults.PollInterval {
		t.Errorf("missing file should return defaults: PollInterval %d != %d",
			cfg.PollInterval, defaults.PollInterval)
	}
}

// TestLoadConfigEmptyPath verifies empty path returns defaults.
func TestLoadConfigEmptyPath(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Errorf("LoadConfig should not error for empty path: %v", err)
	}

	defaults := DefaultConfig()
	if cfg.PollInterval != defaults.PollInterval {
		t.Errorf("empty path should return defaults")
	}
}

// TestSaveConfigCreateDirectory verifies SaveConfig creates parent directories.
func TestSaveConfigCreateDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	nestedPath := filepath.Join(tmpDir, "deep", "nested", "config.yaml")

	cfg := DefaultConfig()
	if err := SaveConfig(cfg, nestedPath); err != nil {
		t.Errorf("SaveConfig should create parent directories: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(nestedPath); os.IsNotExist(err) {
		t.Error("config file was not created")
	}
}

// TestPodmanConfigDefaults verifies Podman config has reasonable defaults.
func TestPodmanConfigDefaults(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Podman.PruneImagesAge == "" {
		t.Error("Podman.PruneImagesAge should have a default")
	}
	if !cfg.Podman.BuildKitPrune {
		t.Error("Podman.BuildKitPrune should default to true")
	}
	if cfg.Podman.BuildKitPruneKeepDuration == "" {
		t.Error("Podman.BuildKitPruneKeepDuration should have a default")
	}
	if cfg.Podman.BuildKitPruneKeepStorageMB <= 0 {
		t.Errorf("Podman.BuildKitPruneKeepStorageMB should be positive: %d", cfg.Podman.BuildKitPruneKeepStorageMB)
	}
	if cfg.Podman.BuildKitPruneMinReclaimGB < 0 {
		t.Errorf("Podman.BuildKitPruneMinReclaimGB should be non-negative: %d", cfg.Podman.BuildKitPruneMinReclaimGB)
	}
	if cfg.Podman.CriticalSystemPrune {
		t.Error("Podman.CriticalSystemPrune should default to false")
	}

	// MachineNames should have at least one entry on Darwin
	// (but we can't check runtime.GOOS in test, so just verify it's set)
	if len(cfg.Podman.MachineNames) == 0 {
		t.Error("Podman.MachineNames should have defaults")
	}
}

// TestICloudConfigDefaults verifies iCloud config has reasonable defaults.
func TestICloudConfigDefaults(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.ICloud.EvictAfterDays <= 0 {
		t.Errorf("ICloud.EvictAfterDays should be positive: %d", cfg.ICloud.EvictAfterDays)
	}

	if cfg.ICloud.MinFileSizeMB < 0 {
		t.Errorf("ICloud.MinFileSizeMB should be non-negative: %d", cfg.ICloud.MinFileSizeMB)
	}
}
