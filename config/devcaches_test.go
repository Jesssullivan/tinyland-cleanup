package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDarwinDevCachePathsLoadAndRefuse(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yaml")
	if err := os.WriteFile(good, []byte("enable:\n  cache: true\ndarwin_dev_caches:\n  uv:\n    enabled: true\n    max_gb: 15\n    paths: [\"/nix/var/tinyland/user/nix-client-cache/uv\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(good)
	if err != nil {
		t.Fatalf("uv.paths should load: %v", err)
	}
	if len(cfg.DarwinDevCaches.UV.Paths) != 1 || cfg.DarwinDevCaches.UV.MaxGB != 15 {
		t.Fatalf("uv = %+v", cfg.DarwinDevCaches.UV)
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("enable:\n  cache: true\ndarwin_dev_caches:\n  uv:\n    paths: [\"relative/uv\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(bad); err == nil {
		t.Fatal("a relative uv.paths entry must refuse the config")
	}
	if d := DefaultConfig().DarwinDevCaches; len(d.UV.Paths)+len(d.Pip.Paths)+len(d.NPM.Paths)+len(d.Bun.Paths)+len(d.OpenCode.Paths) != 0 {
		t.Fatal("no family may name a path by default")
	}
}
