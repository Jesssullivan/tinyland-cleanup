package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDefaultYAMLRetentionLoads(t *testing.T) {
	cfg, err := LoadConfig("default.yaml")
	if err != nil {
		t.Fatalf("default.yaml does not load: %v", err)
	}
	if cfg.Enable.Retention {
		t.Fatal("retention must be off by default")
	}
	if cfg.Retention.TmpAgents.Delete || cfg.Retention.CargoTargets.Delete || cfg.Bazel.DiskCacheAgeDelete {
		t.Fatal("every deleting retention rule must default to report only")
	}
	if err := cfg.Retention.Validate(); err != nil {
		t.Fatalf("default retention invalid: %v", err)
	}
}

func TestRetentionCompiledDefaults(t *testing.T) {
	c := DefaultConfig()
	if c.Enable.Retention || c.Retention.TmpAgents.Delete || c.Retention.CargoTargets.Delete || c.Bazel.DiskCacheAgeDelete {
		t.Fatal("retention defaults must not delete")
	}
	if c.Retention.TmpAgents.MaxAge != "24h" || c.Retention.CargoTargets.MaxAge != "7d" || c.Bazel.DiskCacheMaxAge != "14d" || c.Retention.FreeSpaceFloor.FloorGiB != 10 {
		t.Fatalf("unexpected defaults: %+v %+v", c.Retention, c.Bazel)
	}
	if runtime.GOOS == "darwin" {
		found := false
		for _, p := range c.Retention.FreeSpaceFloor.Paths {
			found = found || p == "/System/Volumes/Data"
		}
		if !found {
			t.Fatal("Darwin floor must cover /System/Volumes/Data")
		}
	}
}

func TestLoadConfigRefusesBadRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	body := "enable:\n  retention: true\nretention:\n  tmp_agents:\n    max_age: 5m\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("max_age under the floor loaded")
	}
	body = "bazel:\n  disk_cache_max_age: 1h\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("disk_cache_max_age under the floor loaded")
	}
}
