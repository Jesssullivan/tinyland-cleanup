package plugins

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

func TestCleanupLevelString(t *testing.T) {
	tests := []struct {
		level    CleanupLevel
		expected string
	}{
		{LevelNone, "none"},
		{LevelWarning, "warning"},
		{LevelModerate, "moderate"},
		{LevelAggressive, "aggressive"},
		{LevelCritical, "critical"},
		{CleanupLevel(99), "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			if got := tt.level.String(); got != tt.expected {
				t.Errorf("CleanupLevel(%d).String() = %v, want %v",
					tt.level, got, tt.expected)
			}
		})
	}
}

func TestRegistry(t *testing.T) {
	registry := NewRegistry()

	// Should start empty
	if len(registry.GetAll()) != 0 {
		t.Error("expected empty registry")
	}

	// Register a mock plugin
	mock := &mockPlugin{
		name:       "test",
		platforms:  nil, // All platforms
		enabledVal: true,
	}
	registry.Register(mock)

	// Should have one plugin
	if len(registry.GetAll()) != 1 {
		t.Errorf("expected 1 plugin, got %d", len(registry.GetAll()))
	}

	// GetEnabled should return it
	cfg := config.DefaultConfig()
	enabled := registry.GetEnabled(cfg)
	if len(enabled) != 1 {
		t.Errorf("expected 1 enabled plugin, got %d", len(enabled))
	}
}

func TestRegistryPlatformFiltering(t *testing.T) {
	registry := NewRegistry()

	// Register platform-specific plugin
	darwinPlugin := &mockPlugin{
		name:       "darwin-only",
		platforms:  []string{PlatformDarwin},
		enabledVal: true,
	}
	registry.Register(darwinPlugin)

	// Register all-platform plugin
	allPlugin := &mockPlugin{
		name:       "all-platforms",
		platforms:  nil,
		enabledVal: true,
	}
	registry.Register(allPlugin)

	cfg := config.DefaultConfig()
	enabled := registry.GetEnabled(cfg)

	// All-platform plugin should always be enabled
	foundAll := false
	for _, p := range enabled {
		if p.Name() == "all-platforms" {
			foundAll = true
		}
	}
	if !foundAll {
		t.Error("expected all-platforms plugin to be enabled")
	}
}

func TestRegistryEnabledFiltering(t *testing.T) {
	registry := NewRegistry()

	// Register disabled plugin
	disabled := &mockPlugin{
		name:       "disabled",
		platforms:  nil,
		enabledVal: false,
	}
	registry.Register(disabled)

	// Register enabled plugin
	enabled := &mockPlugin{
		name:       "enabled",
		platforms:  nil,
		enabledVal: true,
	}
	registry.Register(enabled)

	cfg := config.DefaultConfig()
	plugins := registry.GetEnabled(cfg)

	if len(plugins) != 1 {
		t.Errorf("expected 1 enabled plugin, got %d", len(plugins))
	}

	if plugins[0].Name() != "enabled" {
		t.Errorf("expected 'enabled' plugin, got '%s'", plugins[0].Name())
	}
}

// mockPlugin implements Plugin for testing
type mockPlugin struct {
	name        string
	platforms   []string
	enabledVal  bool
	cleanupFunc func(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult
}

func (m *mockPlugin) Name() string {
	return m.name
}

func (m *mockPlugin) Description() string {
	return "Mock plugin for testing"
}

func (m *mockPlugin) SupportedPlatforms() []string {
	return m.platforms
}

func (m *mockPlugin) Enabled(cfg *config.Config) bool {
	return m.enabledVal
}

func (m *mockPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	if m.cleanupFunc != nil {
		return m.cleanupFunc(ctx, level, cfg, logger)
	}
	return CleanupResult{
		Plugin: m.name,
		Level:  level,
	}
}

func TestDockerPluginName(t *testing.T) {
	p := NewDockerPlugin()
	if p.Name() != "docker" {
		t.Errorf("expected name 'docker', got '%s'", p.Name())
	}
}

func TestDockerPluginEnabled(t *testing.T) {
	p := NewDockerPlugin()
	cfg := config.DefaultConfig()

	// Default should be enabled
	if !p.Enabled(cfg) {
		t.Error("expected docker plugin to be enabled by default")
	}

	// Disable and check
	cfg.Enable.Docker = false
	if p.Enabled(cfg) {
		t.Error("expected docker plugin to be disabled")
	}
}

func TestDockerPluginParseReclaimedSpace(t *testing.T) {
	p := NewDockerPlugin()

	tests := []struct {
		name     string
		output   string
		expected int64
	}{
		{"empty", "", 0},
		{"no match", "some random output", 0},
		{"megabytes", "Total reclaimed space: 123.45 MB", 129446707}, // ~123.45 * 1024 * 1024
		{"gigabytes", "Total reclaimed space: 1.5 GB", 1610612736},   // 1.5 * 1024^3
		{"kilobytes", "Total reclaimed space: 500 KB", 512000},       // 500 * 1024
		{"bytes", "Total reclaimed space: 1000 B", 1000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.parseReclaimedSpace(tt.output)
			// Allow 1% tolerance for floating point
			diff := got - tt.expected
			if diff < 0 {
				diff = -diff
			}
			tolerance := tt.expected / 100
			if tolerance < 1 {
				tolerance = 1
			}
			if diff > tolerance && tt.expected != 0 {
				t.Errorf("parseReclaimedSpace(%q) = %d, want %d (diff: %d)",
					tt.output, got, tt.expected, diff)
			}
		})
	}
}

func TestNixPluginName(t *testing.T) {
	p := NewNixPlugin()
	if p.Name() != "nix" {
		t.Errorf("expected name 'nix', got '%s'", p.Name())
	}
}

func TestNixPluginEnabled(t *testing.T) {
	p := NewNixPlugin()
	cfg := config.DefaultConfig()

	// Default should be enabled
	if !p.Enabled(cfg) {
		t.Error("expected nix plugin to be enabled by default")
	}

	// Disable and check
	cfg.Enable.NixGC = false
	if p.Enabled(cfg) {
		t.Error("expected nix plugin to be disabled")
	}
}

func TestNixPluginParseFreedSpace(t *testing.T) {
	p := NewNixPlugin()

	tests := []struct {
		name     string
		output   string
		expected int64
	}{
		{"empty", "", 0},
		{"no match", "some random output", 0},
		{"mib", "1234 store paths deleted, 567.89 MiB freed", 595468902}, // ~567.89 * 1024^2
		{"gib", "100 store paths deleted, 1.5 GiB freed", 1610612736},    // 1.5 * 1024^3
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.parseFreedSpace(tt.output)
			// Allow 1% tolerance
			diff := got - tt.expected
			if diff < 0 {
				diff = -diff
			}
			tolerance := tt.expected / 100
			if tolerance < 1 {
				tolerance = 1
			}
			if diff > tolerance && tt.expected != 0 {
				t.Errorf("parseFreedSpace(%q) = %d, want %d",
					tt.output, got, tt.expected)
			}
		})
	}
}

func TestNixPluginParseDeletedPaths(t *testing.T) {
	p := NewNixPlugin()

	tests := []struct {
		name     string
		output   string
		expected int
	}{
		{"empty", "", 0},
		{"no match", "some random output", 0},
		{"with count", "1234 store paths deleted, 567.89 MiB freed", 1234},
		{"zero", "0 store paths deleted", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.parseDeletedPaths(tt.output)
			if got != tt.expected {
				t.Errorf("parseDeletedPaths(%q) = %d, want %d",
					tt.output, got, tt.expected)
			}
		})
	}
}

func TestMain(m *testing.M) {
	// Create a null logger for tests
	os.Exit(m.Run())
}
