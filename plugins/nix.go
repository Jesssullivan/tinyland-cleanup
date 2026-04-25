package plugins

import (
	"context"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// NixPlugin handles Nix garbage collection operations.
type NixPlugin struct{}

// NewNixPlugin creates a new Nix cleanup plugin.
func NewNixPlugin() *NixPlugin {
	return &NixPlugin{}
}

// Name returns the plugin identifier.
func (p *NixPlugin) Name() string {
	return "nix"
}

// Description returns the plugin description.
func (p *NixPlugin) Description() string {
	return "Runs Nix garbage collection to clean old generations and store paths"
}

// SupportedPlatforms returns supported platforms (all).
func (p *NixPlugin) SupportedPlatforms() []string {
	return nil // All platforms (Nix can be installed anywhere)
}

// Enabled checks if Nix cleanup is enabled.
func (p *NixPlugin) Enabled(cfg *config.Config) bool {
	return cfg.Enable.NixGC
}

// Cleanup performs Nix garbage collection at the specified level.
func (p *NixPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{
		Plugin: p.Name(),
		Level:  level,
	}

	// Check if nix-collect-garbage is available
	if !p.isNixAvailable() {
		logger.Debug("nix not available, skipping")
		return result
	}

	switch level {
	case LevelWarning, LevelModerate:
		// Light/moderate: just nix-collect-garbage (no -d flag)
		result = p.collectGarbage(ctx, false, logger)
	case LevelAggressive:
		// Aggressive: nix-collect-garbage -d (delete old generations)
		result = p.collectGarbage(ctx, true, logger)
	case LevelCritical:
		// Critical: full GC + store optimize
		result = p.collectGarbageCritical(ctx, logger)
	}

	return result
}

func (p *NixPlugin) isNixAvailable() bool {
	_, err := exec.LookPath("nix-collect-garbage")
	return err == nil
}

func (p *NixPlugin) collectGarbage(ctx context.Context, deleteOldGenerations bool, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name()}

	args := []string{}
	if deleteOldGenerations {
		args = append(args, "-d")
		logger.Debug("running nix-collect-garbage -d")
	} else {
		logger.Debug("running nix-collect-garbage")
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "nix-collect-garbage", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		result.Error = err
		return result
	}

	result.BytesFreed = p.parseFreedSpace(string(output))
	result.ItemsCleaned = p.parseDeletedPaths(string(output))

	return result
}

func (p *NixPlugin) collectGarbageCritical(ctx context.Context, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: LevelCritical}

	// First, collect garbage with -d
	logger.Warn("CRITICAL: running nix-collect-garbage -d")
	gcResult := p.collectGarbage(ctx, true, logger)
	result.BytesFreed = gcResult.BytesFreed
	result.ItemsCleaned = gcResult.ItemsCleaned
	if gcResult.Error != nil {
		result.Error = gcResult.Error
		return result
	}

	// Then optimize the store (deduplicate)
	logger.Warn("CRITICAL: running nix-store --optimize")
	optimizeCtx, cancel := context.WithTimeout(ctx, 60*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(optimizeCtx, "nix-store", "--optimize")
	output, err := cmd.CombinedOutput()
	if err != nil {
		logger.Error("nix-store --optimize failed", "error", err, "output", string(output))
		// Don't fail the whole operation for optimize failure
	} else {
		// Parse optimization results
		optimizedBytes := p.parseOptimizedSpace(string(output))
		result.BytesFreed += optimizedBytes
	}

	return result
}

func (p *NixPlugin) parseFreedSpace(output string) int64 {
	// Parse output like:
	// "deleting '/nix/store/...' ..."
	// "1234 store paths deleted, 5678.90 MiB freed"

	patterns := []string{
		`([\d.]+)\s*MiB freed`,
		`([\d.]+)\s*GiB freed`,
		`([\d.]+)\s*KiB freed`,
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(output)
		if len(matches) >= 2 {
			value, err := strconv.ParseFloat(matches[1], 64)
			if err != nil {
				continue
			}

			if strings.Contains(pattern, "GiB") {
				return int64(value * 1024 * 1024 * 1024)
			} else if strings.Contains(pattern, "MiB") {
				return int64(value * 1024 * 1024)
			} else if strings.Contains(pattern, "KiB") {
				return int64(value * 1024)
			}
		}
	}

	return 0
}

func (p *NixPlugin) parseDeletedPaths(output string) int {
	// Parse "1234 store paths deleted"
	re := regexp.MustCompile(`(\d+)\s*store paths deleted`)
	matches := re.FindStringSubmatch(output)
	if len(matches) >= 2 {
		count, err := strconv.Atoi(matches[1])
		if err == nil {
			return count
		}
	}
	return 0
}

func (p *NixPlugin) parseOptimizedSpace(output string) int64 {
	// Parse output like "linked 1234 files, saved 567.89 MiB"
	patterns := []string{
		`saved\s*([\d.]+)\s*MiB`,
		`saved\s*([\d.]+)\s*GiB`,
		`saved\s*([\d.]+)\s*KiB`,
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(output)
		if len(matches) >= 2 {
			value, err := strconv.ParseFloat(matches[1], 64)
			if err != nil {
				continue
			}

			if strings.Contains(pattern, "GiB") {
				return int64(value * 1024 * 1024 * 1024)
			} else if strings.Contains(pattern, "MiB") {
				return int64(value * 1024 * 1024)
			} else if strings.Contains(pattern, "KiB") {
				return int64(value * 1024)
			}
		}
	}

	return 0
}
