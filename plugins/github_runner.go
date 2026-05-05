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

type githubRunnerTarget struct {
	name     string
	home     string
	workDir  string
	cacheDir string
	tempDir  string
}

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

// githubRunnerTargets returns the configured runner directories to clean.
// Uses config if available, falls back to well-known defaults.
func (p *GitHubRunnerPlugin) githubRunnerTargets(cfg *config.Config) []githubRunnerTarget {
	if len(cfg.GitHubRunner.Instances) > 0 {
		targets := make([]githubRunnerTarget, 0, len(cfg.GitHubRunner.Instances))
		for _, instance := range cfg.GitHubRunner.Instances {
			targets = append(targets, normalizeGitHubRunnerTarget(
				instance.Name,
				instance.Home,
				instance.WorkDir,
				instance.CacheDir,
				instance.TempDir,
			))
		}
		return targets
	}

	return []githubRunnerTarget{
		normalizeGitHubRunnerTarget(
			"default",
			cfg.GitHubRunner.Home,
			cfg.GitHubRunner.WorkDir,
			cfg.GitHubRunner.CacheDir,
			cfg.GitHubRunner.TempDir,
		),
	}
}

func normalizeGitHubRunnerTarget(name, home, workDir, cacheDir, tempDir string) githubRunnerTarget {
	if home == "" {
		home = "/home/github-runner"
	}
	if name == "" {
		name = filepath.Base(home)
	}
	if workDir == "" {
		workDir = filepath.Join(home, "_work")
	}
	if cacheDir == "" {
		cacheDir = filepath.Join(home, "cache")
	}
	if tempDir == "" {
		tempDir = filepath.Join(home, "tmp")
	}

	return githubRunnerTarget{
		name:     name,
		home:     home,
		workDir:  workDir,
		cacheDir: cacheDir,
		tempDir:  tempDir,
	}
}

// Cleanup performs GitHub runner cleanup at the specified level.
func (p *GitHubRunnerPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{
		Plugin: p.Name(),
		Level:  level,
	}

	for _, target := range p.githubRunnerTargets(cfg) {
		result.BytesFreed += p.cleanupTarget(ctx, level, target, logger)
	}

	return result
}

func (p *GitHubRunnerPlugin) cleanupTarget(ctx context.Context, level CleanupLevel, target githubRunnerTarget, logger *slog.Logger) int64 {
	var bytesFreed int64

	// Validate that the runner home actually exists before cleaning
	if !pathExistsAndIsDir(target.home) {
		logger.Debug("github runner home not found, skipping", "instance", target.name, "path", target.home)
		return bytesFreed
	}

	// Warning level: Clean temp directory only
	if level >= LevelWarning {
		if pathExistsAndIsDir(target.tempDir) {
			freed := deleteOldFilesSameDevice(target.tempDir, 24*time.Hour)
			bytesFreed += freed
			if freed > 0 {
				logger.Debug("cleaned github runner temp", "instance", target.name, "freed_mb", freed/(1024*1024))
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
					bytesFreed += size
				}
			}
		}
	}

	// Moderate level: Clean cache and old work directories
	if level >= LevelModerate {
		// Clean cache older than 3 days
		if pathExistsAndIsDir(target.cacheDir) {
			sizeBefore := getDirSizeSameDevice(target.cacheDir)
			deleteOldFilesSameDevice(target.cacheDir, 3*24*time.Hour)
			sizeAfter := getDirSizeSameDevice(target.cacheDir)
			freed := safeBytesDiff(sizeBefore, sizeAfter)
			bytesFreed += freed
			if freed > 0 {
				logger.Debug("cleaned github runner cache", "instance", target.name, "freed_mb", freed/(1024*1024))
			}
		}

		// Clean work directories older than 1 day
		if pathExistsAndIsDir(target.workDir) {
			entries, _ := os.ReadDir(target.workDir)
			for _, entry := range entries {
				if entry.IsDir() {
					dirPath := filepath.Join(target.workDir, entry.Name())
					info, err := entry.Info()
					if err == nil && info.ModTime().Before(time.Now().Add(-24*time.Hour)) {
						size := getDirSizeSameDevice(dirPath)
						os.RemoveAll(dirPath)
						bytesFreed += size
						logger.Debug("removed old work dir", "instance", target.name, "dir", entry.Name(), "freed_mb", size/(1024*1024))
					}
				}
			}
		}
	}

	// Aggressive level: Clean all work directories and Docker resources
	if level >= LevelAggressive {
		// Remove all work directories
		if pathExistsAndIsDir(target.workDir) {
			size := getDirSizeSameDevice(target.workDir)
			if size > 0 {
				os.RemoveAll(target.workDir)
				os.MkdirAll(target.workDir, 0755)
				bytesFreed += size
				logger.Debug("cleaned all github runner work dirs", "instance", target.name, "freed_mb", size/(1024*1024))
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
		if pathExistsAndIsDir(target.cacheDir) {
			size := getDirSizeSameDevice(target.cacheDir)
			if size > 0 {
				os.RemoveAll(target.cacheDir)
				os.MkdirAll(target.cacheDir, 0755)
				bytesFreed += size
				logger.Debug("removed all github runner cache", "instance", target.name, "freed_mb", size/(1024*1024))
			}
		}
	}

	return bytesFreed
}
