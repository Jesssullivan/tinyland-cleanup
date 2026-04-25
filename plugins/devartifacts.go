// Package plugins provides cleanup plugin implementations.
// devartifacts.go scans for stale development artifacts like node_modules,
// .venv, Rust target/, Go build cache, Haskell caches, and LM Studio models.
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

// DevArtifactsPlugin handles stale development artifact cleanup.
type DevArtifactsPlugin struct{}

// NewDevArtifactsPlugin creates a new development artifact cleanup plugin.
func NewDevArtifactsPlugin() *DevArtifactsPlugin {
	return &DevArtifactsPlugin{}
}

// Name returns the plugin identifier.
func (p *DevArtifactsPlugin) Name() string {
	return "dev-artifacts"
}

// Description returns the plugin description.
func (p *DevArtifactsPlugin) Description() string {
	return "Cleans stale development artifacts (node_modules, .venv, target/, go cache, haskell, lmstudio)"
}

// SupportedPlatforms returns supported platforms (all).
func (p *DevArtifactsPlugin) SupportedPlatforms() []string {
	return nil // All platforms
}

// Enabled checks if dev artifact cleanup is enabled.
func (p *DevArtifactsPlugin) Enabled(cfg *config.Config) bool {
	return cfg.Enable.DevArtifacts
}

// Cleanup performs dev artifact cleanup at the specified level.
func (p *DevArtifactsPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{
		Plugin: p.Name(),
		Level:  level,
	}

	home, _ := os.UserHomeDir()
	daCfg := cfg.DevArtifacts

	// Determine staleness thresholds based on level
	var nodeAge, venvAge, rustAge time.Duration
	switch level {
	case LevelWarning:
		// Report only - no deletion
		p.reportArtifacts(ctx, daCfg, home, logger)
		return result
	case LevelModerate:
		nodeAge = 30 * 24 * time.Hour // 30 days
		venvAge = 60 * 24 * time.Hour // 60 days
		rustAge = 30 * 24 * time.Hour // 30 days
	case LevelAggressive:
		nodeAge = 7 * 24 * time.Hour  // 7 days
		venvAge = 14 * 24 * time.Hour // 14 days
		rustAge = 7 * 24 * time.Hour  // 7 days
	case LevelCritical:
		nodeAge = 0 // ALL stale (any with untouched project)
		venvAge = 0 // ALL orphaned
		rustAge = 0 // ALL stale
	}

	// Scan configured paths for dev artifacts
	for _, scanPath := range daCfg.ScanPaths {
		expanded := expandHome(scanPath, home)
		if !pathExistsAndIsDir(expanded) {
			continue
		}

		if daCfg.NodeModules {
			freed := p.cleanNodeModules(ctx, expanded, nodeAge, daCfg.ProtectPaths, logger)
			result.BytesFreed += freed
			if freed > 0 {
				result.ItemsCleaned++
			}
		}

		if daCfg.PythonVenvs {
			freed := p.cleanPythonVenvs(ctx, expanded, venvAge, daCfg.ProtectPaths, logger)
			result.BytesFreed += freed
			if freed > 0 {
				result.ItemsCleaned++
			}
		}

		if daCfg.RustTargets {
			freed := p.cleanRustTargets(ctx, expanded, rustAge, daCfg.ProtectPaths, logger)
			result.BytesFreed += freed
			if freed > 0 {
				result.ItemsCleaned++
			}
		}
	}

	// Go build cache (not path-dependent - it's a global cache)
	if daCfg.GoBuildCache {
		freed := p.cleanGoBuildCache(ctx, level, logger)
		result.BytesFreed += freed
		if freed > 0 {
			result.ItemsCleaned++
		}
	}

	// Haskell cache cleanup
	if daCfg.HaskellCache {
		freed := p.cleanHaskellCache(ctx, level, home, logger)
		result.BytesFreed += freed
		if freed > 0 {
			result.ItemsCleaned++
		}
	}

	// LM Studio models (opt-in only)
	if daCfg.LMStudioModels {
		freed := p.cleanLMStudioModels(ctx, level, home, logger)
		result.BytesFreed += freed
		if freed > 0 {
			result.ItemsCleaned++
		}
	}

	return result
}

// reportArtifacts reports sizes of all detected dev artifacts without cleaning.
func (p *DevArtifactsPlugin) reportArtifacts(ctx context.Context, daCfg config.DevArtifactsConfig, home string, logger *slog.Logger) {
	for _, scanPath := range daCfg.ScanPaths {
		expanded := expandHome(scanPath, home)
		if !pathExistsAndIsDir(expanded) {
			continue
		}

		// Find and report node_modules
		if daCfg.NodeModules {
			p.findArtifactDirs(expanded, "node_modules", "package.json", func(dir string, size int64) {
				logger.Info("found node_modules", "path", dir, "size_mb", size/(1024*1024))
			})
		}

		// Find and report .venv
		if daCfg.PythonVenvs {
			p.findArtifactDirs(expanded, ".venv", "", func(dir string, size int64) {
				logger.Info("found .venv", "path", dir, "size_mb", size/(1024*1024))
			})
		}

		// Find and report target/
		if daCfg.RustTargets {
			p.findArtifactDirs(expanded, "target", "Cargo.toml", func(dir string, size int64) {
				logger.Info("found Rust target", "path", dir, "size_mb", size/(1024*1024))
			})
		}
	}

	// Report Go build cache
	if daCfg.GoBuildCache {
		goCacheDir := p.getGoCacheDir(ctx)
		if goCacheDir != "" {
			size := getDirSize(goCacheDir)
			if size > 0 {
				logger.Info("found Go build cache", "path", goCacheDir, "size_mb", size/(1024*1024))
			}
		}
	}

	// Report Haskell caches
	if daCfg.HaskellCache {
		ghcupCache := filepath.Join(home, ".ghcup", "cache")
		cabalStore := filepath.Join(home, ".cabal", "store")
		if size := getDirSize(ghcupCache); size > 0 {
			logger.Info("found .ghcup/cache", "size_mb", size/(1024*1024))
		}
		if size := getDirSize(cabalStore); size > 0 {
			logger.Info("found .cabal/store", "size_mb", size/(1024*1024))
		}
	}

	// Report LM Studio models
	if daCfg.LMStudioModels {
		lmStudioDir := filepath.Join(home, ".lmstudio", "models")
		if size := getDirSize(lmStudioDir); size > 0 {
			logger.Info("found .lmstudio/models", "size_mb", size/(1024*1024))
		}
	}
}

// cleanNodeModules removes stale node_modules directories.
// A node_modules is considered stale if the sibling package.json hasn't been
// modified within the maxAge threshold.
func (p *DevArtifactsPlugin) cleanNodeModules(ctx context.Context, scanPath string, maxAge time.Duration, protectPaths []string, logger *slog.Logger) int64 {
	var totalFreed int64

	p.findArtifactDirs(scanPath, "node_modules", "package.json", func(dir string, size int64) {
		if p.isProtected(dir, protectPaths) {
			return
		}

		// Check project staleness via package.json mtime
		packageJSON := filepath.Join(filepath.Dir(dir), "package.json")
		if maxAge > 0 && !p.isFileStale(packageJSON, maxAge) {
			return
		}

		logger.Debug("removing stale node_modules", "path", dir, "size_mb", size/(1024*1024))
		if err := os.RemoveAll(dir); err != nil {
			logger.Debug("failed to remove node_modules", "path", dir, "error", err)
			return
		}
		totalFreed += size
	})

	if totalFreed > 0 {
		logger.Info("cleaned stale node_modules", "freed_mb", totalFreed/(1024*1024))
	}

	return totalFreed
}

// cleanPythonVenvs removes stale Python virtual environments.
// A .venv is stale if sibling pyproject.toml/setup.py/requirements.txt hasn't
// been modified within the maxAge threshold.
func (p *DevArtifactsPlugin) cleanPythonVenvs(ctx context.Context, scanPath string, maxAge time.Duration, protectPaths []string, logger *slog.Logger) int64 {
	var totalFreed int64
	pythonMarkers := []string{"pyproject.toml", "setup.py", "requirements.txt"}

	p.findArtifactDirs(scanPath, ".venv", "", func(dir string, size int64) {
		if p.isProtected(dir, protectPaths) {
			return
		}

		// Check project staleness via any Python project marker
		parentDir := filepath.Dir(dir)
		isStale := true
		for _, marker := range pythonMarkers {
			markerPath := filepath.Join(parentDir, marker)
			if maxAge > 0 && !p.isFileStale(markerPath, maxAge) {
				isStale = false
				break
			}
		}

		// At Critical level (maxAge == 0), check if any marker file exists
		// If none exist, it's an orphaned venv
		if maxAge == 0 {
			hasMarker := false
			for _, marker := range pythonMarkers {
				if pathExists(filepath.Join(parentDir, marker)) {
					hasMarker = true
					break
				}
			}
			if hasMarker {
				// Has a marker but project might be active - still clean at Critical
				isStale = true
			}
		}

		if !isStale {
			return
		}

		logger.Debug("removing stale .venv", "path", dir, "size_mb", size/(1024*1024))
		if err := os.RemoveAll(dir); err != nil {
			logger.Debug("failed to remove .venv", "path", dir, "error", err)
			return
		}
		totalFreed += size
	})

	if totalFreed > 0 {
		logger.Info("cleaned stale Python venvs", "freed_mb", totalFreed/(1024*1024))
	}

	return totalFreed
}

// cleanRustTargets removes stale Rust target/ directories.
// A target/ is stale if sibling Cargo.toml hasn't been modified within maxAge.
func (p *DevArtifactsPlugin) cleanRustTargets(ctx context.Context, scanPath string, maxAge time.Duration, protectPaths []string, logger *slog.Logger) int64 {
	var totalFreed int64

	p.findArtifactDirs(scanPath, "target", "Cargo.toml", func(dir string, size int64) {
		if p.isProtected(dir, protectPaths) {
			return
		}

		cargoToml := filepath.Join(filepath.Dir(dir), "Cargo.toml")
		if maxAge > 0 && !p.isFileStale(cargoToml, maxAge) {
			return
		}

		logger.Debug("removing stale Rust target", "path", dir, "size_mb", size/(1024*1024))
		if err := os.RemoveAll(dir); err != nil {
			logger.Debug("failed to remove Rust target", "path", dir, "error", err)
			return
		}
		totalFreed += size
	})

	if totalFreed > 0 {
		logger.Info("cleaned stale Rust targets", "freed_mb", totalFreed/(1024*1024))
	}

	return totalFreed
}

// cleanGoBuildCache cleans the Go build cache using go clean.
func (p *DevArtifactsPlugin) cleanGoBuildCache(ctx context.Context, level CleanupLevel, logger *slog.Logger) int64 {
	if _, err := exec.LookPath("go"); err != nil {
		return 0
	}

	goCacheDir := p.getGoCacheDir(ctx)
	if goCacheDir == "" {
		return 0
	}

	sizeBefore := getDirSize(goCacheDir)
	if sizeBefore == 0 {
		return 0
	}

	switch level {
	case LevelModerate:
		// Clean test cache only
		logger.Debug("cleaning Go test cache")
		exec.CommandContext(ctx, "go", "clean", "-testcache").Run()
	case LevelAggressive:
		// Clean full build cache
		logger.Debug("cleaning Go build cache")
		exec.CommandContext(ctx, "go", "clean", "-cache").Run()
	case LevelCritical:
		// Clean everything: build cache + module cache
		logger.Debug("cleaning Go build cache and module cache")
		exec.CommandContext(ctx, "go", "clean", "-cache", "-testcache").Run()
	}

	sizeAfter := getDirSize(goCacheDir)
	freed := safeBytesDiff(sizeBefore, sizeAfter)
	if freed > 0 {
		logger.Info("cleaned Go build cache", "freed_mb", freed/(1024*1024))
	}
	return freed
}

// cleanHaskellCache cleans Haskell-related caches.
func (p *DevArtifactsPlugin) cleanHaskellCache(ctx context.Context, level CleanupLevel, home string, logger *slog.Logger) int64 {
	var totalFreed int64

	// .ghcup/cache - always safe to clean (downloaded tarballs)
	ghcupCache := filepath.Join(home, ".ghcup", "cache")
	if level >= LevelModerate {
		if size := getDirSize(ghcupCache); size > 0 {
			os.RemoveAll(ghcupCache)
			totalFreed += size
			logger.Debug("cleaned .ghcup/cache", "freed_mb", size/(1024*1024))
		}
	}

	// .cabal/store - old package builds (aggressive+)
	if level >= LevelAggressive {
		cabalStore := filepath.Join(home, ".cabal", "store")
		if _, err := os.Stat(cabalStore); err == nil {
			sizeBefore := getDirSize(cabalStore)
			deleteOldFiles(cabalStore, 30*24*time.Hour)
			sizeAfter := getDirSize(cabalStore)
			freed := safeBytesDiff(sizeBefore, sizeAfter)
			if freed > 0 {
				totalFreed += freed
				logger.Debug("cleaned old .cabal/store entries", "freed_mb", freed/(1024*1024))
			}
		}
	}

	// .ghcup old toolchain versions (critical only)
	if level >= LevelCritical {
		if _, err := exec.LookPath("ghcup"); err == nil {
			logger.Debug("running ghcup gc")
			exec.CommandContext(ctx, "ghcup", "gc", "--cache").Run()
		}

		// Clean stack cache
		stackRoot := filepath.Join(home, ".stack")
		if _, err := os.Stat(stackRoot); err == nil {
			// Stack's pantry cache can get large
			pantryCachePath := filepath.Join(stackRoot, "pantry", "hackage")
			if size := getDirSize(pantryCachePath); size > 500*1024*1024 {
				sizeBefore := size
				deleteOldFiles(pantryCachePath, 14*24*time.Hour)
				sizeAfter := getDirSize(pantryCachePath)
				freed := safeBytesDiff(sizeBefore, sizeAfter)
				totalFreed += freed
			}
		}
	}

	if totalFreed > 0 {
		logger.Info("cleaned Haskell caches", "freed_mb", totalFreed/(1024*1024))
	}

	return totalFreed
}

// cleanLMStudioModels cleans LM Studio model files.
func (p *DevArtifactsPlugin) cleanLMStudioModels(ctx context.Context, level CleanupLevel, home string, logger *slog.Logger) int64 {
	lmStudioDir := filepath.Join(home, ".lmstudio", "models")
	if !pathExistsAndIsDir(lmStudioDir) {
		return 0
	}

	switch level {
	case LevelWarning, LevelModerate:
		// Report only
		size := getDirSize(lmStudioDir)
		if size > 0 {
			logger.Info("LM Studio models", "size_mb", size/(1024*1024))
		}
		return 0
	case LevelAggressive:
		// Report only at aggressive
		size := getDirSize(lmStudioDir)
		if size > 0 {
			logger.Warn("LM Studio models taking space", "size_mb", size/(1024*1024),
				"suggestion", "manually remove unused models from ~/.lmstudio/models/")
		}
		return 0
	case LevelCritical:
		// Delete models older than 30 days
		sizeBefore := getDirSize(lmStudioDir)
		deleteOldFiles(lmStudioDir, 30*24*time.Hour)
		sizeAfter := getDirSize(lmStudioDir)
		freed := safeBytesDiff(sizeBefore, sizeAfter)
		if freed > 0 {
			logger.Warn("CRITICAL: cleaned old LM Studio models", "freed_mb", freed/(1024*1024))
		}
		return freed
	}

	return 0
}

// findArtifactDirs walks scanPath looking for directories named targetName.
// If markerFile is set, only reports dirs that have a sibling marker file.
// Callback receives the artifact dir path and its size.
// Limits directory depth to 4 levels to avoid excessive scanning.
func (p *DevArtifactsPlugin) findArtifactDirs(scanPath string, targetName string, markerFile string, callback func(dir string, size int64)) {
	scanDepth := strings.Count(scanPath, string(os.PathSeparator))

	filepath.Walk(scanPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		// Limit depth to 4 levels below scan path
		currentDepth := strings.Count(path, string(os.PathSeparator)) - scanDepth
		if currentDepth > 4 {
			return filepath.SkipDir
		}

		if !info.IsDir() {
			return nil
		}

		// Skip hidden directories other than .venv
		baseName := filepath.Base(path)
		if strings.HasPrefix(baseName, ".") && baseName != ".venv" && baseName != targetName {
			return filepath.SkipDir
		}

		if baseName != targetName {
			return nil
		}

		// If marker file is required, check for it in parent
		if markerFile != "" {
			parentDir := filepath.Dir(path)
			if !pathExists(filepath.Join(parentDir, markerFile)) {
				return filepath.SkipDir
			}
		}

		size := getDirSize(path)
		if size > 0 {
			callback(path, size)
		}

		return filepath.SkipDir // Don't descend into the artifact dir
	})
}

// getGoCacheDir returns the Go build cache directory.
func (p *DevArtifactsPlugin) getGoCacheDir(ctx context.Context) string {
	cmd := exec.CommandContext(ctx, "go", "env", "GOCACHE")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(string(output))
	if dir == "" || dir == "off" {
		return ""
	}
	return dir
}

// isFileStale returns true if a file's mtime is older than maxAge.
// Returns true if the file doesn't exist (consider project abandoned).
func (p *DevArtifactsPlugin) isFileStale(path string, maxAge time.Duration) bool {
	info, err := os.Stat(path)
	if err != nil {
		return true // File doesn't exist = project abandoned
	}
	cutoff := time.Now().Add(-maxAge)
	return info.ModTime().Before(cutoff)
}

// isProtected checks if a path is in the protect list.
func (p *DevArtifactsPlugin) isProtected(path string, protectPaths []string) bool {
	for _, protect := range protectPaths {
		if strings.HasPrefix(path, protect) {
			return true
		}
	}
	return false
}

// expandHome expands ~ to the home directory in a path.
func expandHome(path string, home string) string {
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	if path == "~" {
		return home
	}
	return path
}
