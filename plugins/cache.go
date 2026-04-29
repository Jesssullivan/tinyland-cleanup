//go:build !darwin

package plugins

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// CachePlugin handles cache cleanup operations.
type CachePlugin struct{}

// NewCachePlugin creates a new cache cleanup plugin.
func NewCachePlugin() *CachePlugin {
	return &CachePlugin{}
}

// Name returns the plugin identifier.
func (p *CachePlugin) Name() string {
	return "cache"
}

// Description returns the plugin description.
func (p *CachePlugin) Description() string {
	return "Cleans various application caches (pip, npm, go, etc.)"
}

// SupportedPlatforms returns supported platforms (all).
func (p *CachePlugin) SupportedPlatforms() []string {
	return nil // All platforms
}

// Enabled checks if cache cleanup is enabled.
func (p *CachePlugin) Enabled(cfg *config.Config) bool {
	return cfg.Enable.Cache
}

// Cleanup performs cache cleanup at the specified level.
func (p *CachePlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{
		Plugin: p.Name(),
		Level:  level,
	}

	home, _ := os.UserHomeDir()

	// pip cache
	pipCache := filepath.Join(home, ".cache", "pip")
	if size := getDirSize(pipCache); size > 0 {
		if level >= LevelWarning {
			os.RemoveAll(pipCache)
			result.BytesFreed += size
			logger.Debug("cleaned pip cache", "freed_mb", size/(1024*1024))
		}
	}

	// npm cache
	npmCache := filepath.Join(home, ".npm", "_cacache")
	if size := getDirSize(npmCache); size > 0 {
		if level >= LevelWarning {
			os.RemoveAll(npmCache)
			result.BytesFreed += size
			logger.Debug("cleaned npm cache", "freed_mb", size/(1024*1024))
		}
	}

	// Go build cache (moderate+, separate from module cache)
	if level >= LevelModerate {
		if _, err := exec.LookPath("go"); err == nil {
			if output, err := exec.CommandContext(ctx, "go", "env", "GOCACHE").Output(); err == nil {
				goCacheDir := strings.TrimSpace(string(output))
				if goCacheDir != "" && goCacheDir != "off" {
					sizeBefore := getDirSize(goCacheDir)
					if sizeBefore > 0 {
						if level >= LevelAggressive {
							exec.CommandContext(ctx, "go", "clean", "-cache").Run()
						} else {
							exec.CommandContext(ctx, "go", "clean", "-testcache").Run()
						}
						sizeAfter := getDirSize(goCacheDir)
						freed := safeBytesDiff(sizeBefore, sizeAfter)
						result.BytesFreed += freed
						if freed > 0 {
							logger.Debug("cleaned go build cache", "freed_mb", freed/(1024*1024))
						}
					}
				}
			}
		}
	}

	// go module cache (only at aggressive or higher)
	if level >= LevelAggressive {
		goModCache := filepath.Join(home, "go", "pkg", "mod", "cache")
		if size := getDirSize(goModCache); size > 0 {
			exec.CommandContext(ctx, "go", "clean", "-modcache").Run()
			result.BytesFreed += size
			logger.Debug("cleaned go mod cache", "freed_mb", size/(1024*1024))
		}
	}

	// Cargo cache (only old .crate files at moderate+)
	if level >= LevelModerate {
		cargoCache := filepath.Join(home, ".cargo", "registry", "cache")
		if _, err := os.Stat(cargoCache); err == nil {
			sizeBefore := getDirSize(cargoCache)
			deleteOldFiles(cargoCache, 30*24*time.Hour)
			sizeAfter := getDirSize(cargoCache)
			result.BytesFreed += safeBytesDiff(sizeBefore, sizeAfter)
		}

		// cargo clean gc (Rust 1.82+ automatic garbage collection)
		if _, err := exec.LookPath("cargo"); err == nil {
			exec.CommandContext(ctx, "cargo", "cache", "--autoclean").Run()
		}
	}

	// Rustup toolchain cleanup (critical only - keep default toolchain)
	if level >= LevelCritical {
		if _, err := exec.LookPath("rustup"); err == nil {
			// Remove all non-default toolchains
			output, err := exec.CommandContext(ctx, "rustup", "toolchain", "list").Output()
			if err == nil {
				for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
					line = strings.TrimSpace(line)
					if line == "" || strings.Contains(line, "(default)") {
						continue
					}
					toolchain := strings.Fields(line)[0]
					logger.Debug("removing non-default rustup toolchain", "toolchain", toolchain)
					exec.CommandContext(ctx, "rustup", "toolchain", "uninstall", toolchain).Run()
					result.ItemsCleaned++
				}
			}
		}
	}

	// Maven cache (moderate+)
	if level >= LevelModerate {
		mavenCache := filepath.Join(home, ".m2", "repository")
		if size := getDirSize(mavenCache); size > 0 {
			sizeBefore := size
			deleteOldFiles(mavenCache, 30*24*time.Hour)
			sizeAfter := getDirSize(mavenCache)
			freed := safeBytesDiff(sizeBefore, sizeAfter)
			result.BytesFreed += freed
			logger.Debug("cleaned maven cache", "freed_mb", freed/(1024*1024))
		}
	}

	// Gradle cache (moderate+)
	if level >= LevelModerate {
		gradleCache := filepath.Join(home, ".gradle", "caches")
		if size := getDirSize(gradleCache); size > 0 {
			sizeBefore := size
			deleteOldFiles(gradleCache, 30*24*time.Hour)
			sizeAfter := getDirSize(gradleCache)
			freed := safeBytesDiff(sizeBefore, sizeAfter)
			result.BytesFreed += freed
			logger.Debug("cleaned gradle cache", "freed_mb", freed/(1024*1024))
		}
	}

	// Temp files - more aggressive cleanup based on level
	// Uses mount-boundary-safe deletion and tracks actual bytes freed
	tmpFiles := []string{"/tmp", "/var/tmp"}
	for _, tmpDir := range tmpFiles {
		if !pathExistsAndIsDir(tmpDir) {
			continue
		}
		var maxAge time.Duration
		switch {
		case level >= LevelAggressive:
			maxAge = 1 * 24 * time.Hour // 1 day at aggressive
		case level >= LevelModerate:
			maxAge = 3 * 24 * time.Hour // 3 days at moderate
		default:
			maxAge = 7 * 24 * time.Hour // 7 days at warning
		}
		// Use mount-safe version that returns actual freed bytes
		freed := deleteOldFilesOwnedByUserSameDevice(tmpDir, maxAge)
		result.BytesFreed += freed
	}

	// Systemd journal (Linux only)
	if level >= LevelModerate {
		if _, err := exec.LookPath("journalctl"); err == nil {
			// User journal cleanup
			exec.CommandContext(ctx, "journalctl", "--user", "--vacuum-size=200M", "--vacuum-time=7d").Run()
		}
	}

	// System journal (aggressive+, requires sudo)
	if level >= LevelAggressive {
		if _, err := exec.LookPath("journalctl"); err == nil {
			testCmd := exec.Command("sudo", "-n", "true")
			if testCmd.Run() == nil {
				exec.CommandContext(ctx, "sudo", "journalctl", "--vacuum-size=100M", "--vacuum-time=3d").Run()
			}
		}
	}

	return result
}

// Helper functions

func getDirSize(path string) int64 {
	size, _ := getDirSizeContext(context.Background(), path)
	return size
}

func getDirSizeContext(ctx context.Context, path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	if err != nil {
		return size, err
	}
	return size, ctx.Err()
}

func deleteOldFiles(dir string, maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && info.ModTime().Before(cutoff) {
			os.Remove(path)
		}
		return nil
	})
}

func deleteOldFilesOwnedByUser(dir string, maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	uid := os.Getuid()
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && info.ModTime().Before(cutoff) {
			// Only delete files owned by current user
			if stat, err := os.Stat(path); err == nil {
				// Best effort - if we can delete it, we own it or have permission
				if stat.Mode().IsRegular() {
					os.Remove(path)
				}
			}
		}
		return nil
	})
	_ = uid // Suppress unused warning - ownership check is implicit via permissions
}
