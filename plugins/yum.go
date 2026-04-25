//go:build !darwin

package plugins

import (
	"context"
	"log/slog"
	"os/exec"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// YumPlugin handles YUM/DNF cache cleanup operations.
type YumPlugin struct{}

// NewYumPlugin creates a new YUM cache cleanup plugin.
func NewYumPlugin() *YumPlugin {
	return &YumPlugin{}
}

// Name returns the plugin identifier.
func (p *YumPlugin) Name() string {
	return "yum"
}

// Description returns the plugin description.
func (p *YumPlugin) Description() string {
	return "Cleans YUM/DNF package manager caches"
}

// SupportedPlatforms returns supported platforms (Linux only).
func (p *YumPlugin) SupportedPlatforms() []string {
	return []string{"linux"}
}

// Enabled checks if YUM cleanup is enabled.
func (p *YumPlugin) Enabled(cfg *config.Config) bool {
	return cfg.Enable.Yum
}

// Cleanup performs YUM cache cleanup at the specified level.
func (p *YumPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{
		Plugin: p.Name(),
		Level:  level,
	}

	// Check if dnf or yum is available
	dnfPath, dnfErr := exec.LookPath("dnf")
	yumPath, yumErr := exec.LookPath("yum")

	if dnfErr != nil && yumErr != nil {
		// Neither dnf nor yum available, skip
		return result
	}

	// Prefer dnf over yum
	pkgManager := dnfPath
	if dnfErr != nil {
		pkgManager = yumPath
	}

	// Get cache size before cleanup
	var cacheDirs []string
	if dnfErr == nil {
		cacheDirs = []string{"/var/cache/dnf", "/var/cache/yum"}
	} else {
		cacheDirs = []string{"/var/cache/yum"}
	}

	var sizeBefore int64
	for _, dir := range cacheDirs {
		sizeBefore += getDirSize(dir)
	}

	// Moderate+: Clean all cache
	if level >= LevelModerate {
		// Run dnf/yum clean all (requires sudo for system-wide cleanup)
		testCmd := exec.Command("sudo", "-n", "true")
		if testCmd.Run() == nil {
			// Can run sudo without password
			cmd := exec.CommandContext(ctx, "sudo", pkgManager, "clean", "all")
			if err := cmd.Run(); err != nil {
				logger.Debug("yum clean failed", "error", err)
			} else {
				// Calculate freed space
				var sizeAfter int64
				for _, dir := range cacheDirs {
					sizeAfter += getDirSize(dir)
				}
				result.BytesFreed = sizeBefore - sizeAfter
				logger.Debug("cleaned yum/dnf cache", "freed_mb", result.BytesFreed/(1024*1024))
			}
		} else {
			logger.Debug("skipping yum cleanup - sudo required")
		}
	}

	return result
}
