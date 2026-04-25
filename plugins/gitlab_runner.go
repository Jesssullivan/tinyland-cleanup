// Package plugins provides cleanup plugins for various systems.
package plugins

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// GitLabRunnerPlugin cleans up GitLab runner caches and build artifacts.
type GitLabRunnerPlugin struct{}

// NewGitLabRunnerPlugin creates a new GitLab runner cleanup plugin.
func NewGitLabRunnerPlugin() *GitLabRunnerPlugin {
	return &GitLabRunnerPlugin{}
}

// Name returns the plugin name.
func (p *GitLabRunnerPlugin) Name() string {
	return "gitlab-runner"
}

// Description returns the plugin description.
func (p *GitLabRunnerPlugin) Description() string {
	return "Cleans GitLab runner caches, build directories, and stale artifacts"
}

// SupportedPlatforms returns platforms this plugin supports (all platforms).
func (p *GitLabRunnerPlugin) SupportedPlatforms() []string {
	return []string{} // Empty means all platforms
}

// Enabled returns whether the plugin is enabled based on configuration.
func (p *GitLabRunnerPlugin) Enabled(cfg *config.Config) bool {
	return cfg.Enable.GitLabRunner
}

// Cleanup performs GitLab runner cleanup at the specified level.
func (p *GitLabRunnerPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name()}

	home, err := os.UserHomeDir()
	if err != nil {
		result.Error = fmt.Errorf("failed to get home directory: %w", err)
		return result
	}

	// Check if gitlab-runner is installed
	if _, err := exec.LookPath("gitlab-runner"); err != nil {
		logger.Debug("gitlab-runner not found, skipping")
		return result
	}

	// Define cleanup paths
	runnerPaths := p.getRunnerPaths(home)

	switch level {
	case LevelWarning:
		// Light cleanup: Clear download caches only
		result = p.cleanDownloadCache(ctx, home, logger, result)
	case LevelModerate:
		// Moderate: Clear caches and old build directories
		result = p.cleanDownloadCache(ctx, home, logger, result)
		result = p.cleanBuildDirectories(ctx, runnerPaths, 7*24*time.Hour, logger, result)
	case LevelAggressive:
		// Aggressive: Clear all caches and build dirs older than 1 day
		result = p.cleanDownloadCache(ctx, home, logger, result)
		result = p.cleanBuildDirectories(ctx, runnerPaths, 24*time.Hour, logger, result)
		result = p.cleanDockerCaches(ctx, logger, result)
	case LevelCritical:
		// Critical: Clear everything possible
		result = p.cleanDownloadCache(ctx, home, logger, result)
		result = p.cleanBuildDirectories(ctx, runnerPaths, 0, logger, result) // All builds
		result = p.cleanDockerCaches(ctx, logger, result)
		result = p.cleanAllCaches(ctx, runnerPaths, logger, result)
	}

	return result
}

// getRunnerPaths returns platform-specific GitLab runner paths.
func (p *GitLabRunnerPlugin) getRunnerPaths(home string) []string {
	paths := []string{
		filepath.Join(home, ".gitlab-runner"),
		filepath.Join(home, "builds"),
	}

	if runtime.GOOS == "linux" {
		// System-wide runner paths on Linux
		paths = append(paths,
			"/home/gitlab-runner",
			"/var/lib/gitlab-runner",
			"/etc/gitlab-runner",
		)
	}

	return paths
}

// cleanDownloadCache clears the GitLab runner download cache.
func (p *GitLabRunnerPlugin) cleanDownloadCache(ctx context.Context, home string, logger *slog.Logger, result CleanupResult) CleanupResult {
	cachePaths := []string{
		filepath.Join(home, ".gitlab-runner", "cache"),
		filepath.Join(home, "Library", "Caches", "gitlab-runner"), // macOS
	}

	for _, cachePath := range cachePaths {
		if _, err := os.Stat(cachePath); os.IsNotExist(err) {
			continue
		}

		sizeBefore := getDirSizeRunner(cachePath)
		if err := os.RemoveAll(cachePath); err != nil {
			logger.Warn("failed to clear runner cache", "path", cachePath, "error", err)
			continue
		}

		// Recreate the directory
		os.MkdirAll(cachePath, 0755)

		freed := sizeBefore
		if freed > 0 {
			result.BytesFreed += freed
			result.ItemsCleaned++
			logger.Info("cleared runner cache", "path", cachePath, "freed_mb", freed/(1024*1024))
		}
	}

	return result
}

// cleanBuildDirectories cleans old build directories.
func (p *GitLabRunnerPlugin) cleanBuildDirectories(ctx context.Context, runnerPaths []string, maxAge time.Duration, logger *slog.Logger, result CleanupResult) CleanupResult {
	for _, basePath := range runnerPaths {
		buildsDir := filepath.Join(basePath, "builds")
		if _, err := os.Stat(buildsDir); os.IsNotExist(err) {
			// Also check if basePath itself is a builds directory
			if strings.HasSuffix(basePath, "builds") {
				buildsDir = basePath
			} else {
				continue
			}
		}

		entries, err := os.ReadDir(buildsDir)
		if err != nil {
			continue
		}

		cutoff := time.Now().Add(-maxAge)
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			buildPath := filepath.Join(buildsDir, entry.Name())
			info, err := entry.Info()
			if err != nil {
				continue
			}

			// Skip if too new (unless maxAge is 0 for critical cleanup)
			if maxAge > 0 && info.ModTime().After(cutoff) {
				continue
			}

			sizeBefore := getDirSizeRunner(buildPath)
			if err := os.RemoveAll(buildPath); err != nil {
				logger.Warn("failed to remove build directory", "path", buildPath, "error", err)
				continue
			}

			if sizeBefore > 0 {
				result.BytesFreed += sizeBefore
				result.ItemsCleaned++
				logger.Debug("removed build directory", "path", buildPath, "age", time.Since(info.ModTime()))
			}
		}
	}

	return result
}

// cleanDockerCaches cleans Docker caches created by runner docker executor.
func (p *GitLabRunnerPlugin) cleanDockerCaches(ctx context.Context, logger *slog.Logger, result CleanupResult) CleanupResult {
	// Clean gitlab-runner docker cache volumes
	cmd := exec.CommandContext(ctx, "docker", "volume", "ls", "--filter", "name=runner-", "-q")
	output, err := cmd.Output()
	if err != nil {
		return result
	}

	volumes := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, vol := range volumes {
		if vol == "" {
			continue
		}

		// Only clean cache volumes
		if !strings.Contains(vol, "cache") {
			continue
		}

		rmCmd := exec.CommandContext(ctx, "docker", "volume", "rm", vol)
		if err := rmCmd.Run(); err != nil {
			logger.Debug("failed to remove volume", "volume", vol, "error", err)
			continue
		}

		result.ItemsCleaned++
		logger.Debug("removed runner cache volume", "volume", vol)
	}

	return result
}

// cleanAllCaches cleans all GitLab runner caches.
func (p *GitLabRunnerPlugin) cleanAllCaches(ctx context.Context, runnerPaths []string, logger *slog.Logger, result CleanupResult) CleanupResult {
	// Clean local cache directories (gitlab-runner cache-extractor is for S3/GCS, not local)
	for _, basePath := range runnerPaths {
		cacheDir := filepath.Join(basePath, "cache")
		if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
			continue
		}

		sizeBefore := getDirSizeRunner(cacheDir)

		// Remove all cache subdirectories
		entries, _ := os.ReadDir(cacheDir)
		for _, entry := range entries {
			if entry.IsDir() {
				os.RemoveAll(filepath.Join(cacheDir, entry.Name()))
			}
		}

		sizeAfter := getDirSizeRunner(cacheDir)
		freed := sizeBefore - sizeAfter
		if freed > 0 {
			result.BytesFreed += freed
			result.ItemsCleaned++
		}
	}

	// Also try to clean /tmp gitlab artifacts
	tmpPatterns := []string{
		"/tmp/gitlab-runner-*",
		"/tmp/build-*",
	}

	for _, pattern := range tmpPatterns {
		matches, _ := filepath.Glob(pattern)
		for _, match := range matches {
			info, err := os.Stat(match)
			if err != nil {
				continue
			}

			// Only clean files owned by current user
			size := getDirSizeRunner(match)
			if info.IsDir() {
				os.RemoveAll(match)
			} else {
				os.Remove(match)
			}

			if size > 0 {
				result.BytesFreed += size
				result.ItemsCleaned++
			}
		}
	}

	return result
}

// getDirSizeRunner returns the size of a directory in bytes.
func getDirSizeRunner(path string) int64 {
	var size int64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size
}
