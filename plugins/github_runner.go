//go:build !darwin

package plugins

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// GitHubRunnerPlugin handles GitHub Actions runner cleanup operations.
type GitHubRunnerPlugin struct{}

// NewGitHubRunnerPlugin creates a new GitHub runner cleanup plugin.
func NewGitHubRunnerPlugin() *GitHubRunnerPlugin {
	return &GitHubRunnerPlugin{}
}

// Name returns the plugin identifier.
func (p *GitHubRunnerPlugin) Name() string {
	return "github_runner"
}

// Description returns the plugin description.
func (p *GitHubRunnerPlugin) Description() string {
	return "Cleans GitHub Actions runner work directories, cache, and temporary files"
}

// SupportedPlatforms returns supported platforms (Linux only).
func (p *GitHubRunnerPlugin) SupportedPlatforms() []string {
	return []string{"linux"}
}

// Enabled checks if GitHub runner cleanup is enabled.
func (p *GitHubRunnerPlugin) Enabled(cfg *config.Config) bool {
	return cfg.Enable.GitHubRunner
}

// githubRunnerPaths returns the set of directories to clean.
// Uses config if available, falls back to well-known defaults.
func (p *GitHubRunnerPlugin) githubRunnerPaths(cfg *config.Config) (runnerHome, workDir, cacheDir, tempDir string) {
	runnerHome = cfg.GitHubRunner.Home
	if runnerHome == "" {
		runnerHome = "/home/github-runner"
	}
	workDir = cfg.GitHubRunner.WorkDir
	if workDir == "" {
		workDir = filepath.Join(runnerHome, "_work")
	}
	cacheDir = filepath.Join(runnerHome, "cache")
	tempDir = filepath.Join(runnerHome, "tmp")
	return
}

// Cleanup performs GitHub runner cleanup at the specified level.
func (p *GitHubRunnerPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{
		Plugin: p.Name(),
		Level:  level,
	}

	runnerHome, workDir, cacheDir, tempDir := p.githubRunnerPaths(cfg)

	// Validate that the runner home actually exists before cleaning
	if !pathExistsAndIsDir(runnerHome) {
		logger.Debug("github runner home not found, skipping", "path", runnerHome)
		return result
	}

	// Warning level: Clean temp directory only
	if level >= LevelWarning {
		if pathExistsAndIsDir(tempDir) {
			freed := deleteOldFilesSameDevice(tempDir, 24*time.Hour)
			result.BytesFreed += freed
			if freed > 0 {
				logger.Debug("cleaned github runner temp", "freed_mb", freed/(1024*1024))
			}
		}

		// Clean /tmp artifacts from GitHub Actions
		tmpArtifacts := []string{
			"/tmp/github-*",
			"/tmp/actions-*",
			"/tmp/runner-*",
		}
		for _, pattern := range tmpArtifacts {
			matches, _ := filepath.Glob(pattern)
			for _, path := range matches {
				if info, err := os.Stat(path); err == nil && info.ModTime().Before(time.Now().Add(-24*time.Hour)) {
					size := getDirSizeSameDevice(path)
					os.RemoveAll(path)
					result.BytesFreed += size
				}
			}
		}
	}

	// Moderate level: Clean cache and old work directories
	if level >= LevelModerate {
		// Clean cache older than 3 days
		if pathExistsAndIsDir(cacheDir) {
			sizeBefore := getDirSizeSameDevice(cacheDir)
			deleteOldFilesSameDevice(cacheDir, 3*24*time.Hour)
			sizeAfter := getDirSizeSameDevice(cacheDir)
			freed := safeBytesDiff(sizeBefore, sizeAfter)
			result.BytesFreed += freed
			if freed > 0 {
				logger.Debug("cleaned github runner cache", "freed_mb", freed/(1024*1024))
			}
		}

		// Clean work directories older than 1 day
		if pathExistsAndIsDir(workDir) {
			entries, _ := os.ReadDir(workDir)
			for _, entry := range entries {
				if entry.IsDir() {
					dirPath := filepath.Join(workDir, entry.Name())
					info, err := entry.Info()
					if err == nil && info.ModTime().Before(time.Now().Add(-24*time.Hour)) {
						size := getDirSizeSameDevice(dirPath)
						os.RemoveAll(dirPath)
						result.BytesFreed += size
						logger.Debug("removed old work dir", "dir", entry.Name(), "freed_mb", size/(1024*1024))
					}
				}
			}
		}
	}

	// Aggressive level: Clean all work directories and Docker resources
	if level >= LevelAggressive {
		// Remove all work directories
		if pathExistsAndIsDir(workDir) {
			size := getDirSizeSameDevice(workDir)
			if size > 0 {
				os.RemoveAll(workDir)
				os.MkdirAll(workDir, 0755)
				result.BytesFreed += size
				logger.Debug("cleaned all github runner work dirs", "freed_mb", size/(1024*1024))
			}
		}

		// Clean Docker volumes/containers created by runner
		if _, err := exec.LookPath("docker"); err == nil {
			exec.CommandContext(ctx, "docker", "container", "prune", "-f", "--filter", "label=com.github.actions.runner").Run()
			exec.CommandContext(ctx, "docker", "volume", "prune", "-f", "--filter", "label=com.github.actions.runner").Run()
			logger.Debug("cleaned github runner docker resources")
		}
	}

	// Critical level: Nuclear cleanup
	if level >= LevelCritical {
		// Remove entire cache
		if pathExistsAndIsDir(cacheDir) {
			size := getDirSizeSameDevice(cacheDir)
			if size > 0 {
				os.RemoveAll(cacheDir)
				os.MkdirAll(cacheDir, 0755)
				result.BytesFreed += size
				logger.Debug("removed all github runner cache", "freed_mb", size/(1024*1024))
			}
		}
	}

	return result
}
