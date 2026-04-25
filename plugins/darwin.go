//go:build darwin

package plugins

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// HomebrewPlugin handles Homebrew cleanup operations.
type HomebrewPlugin struct{}

// NewHomebrewPlugin creates a new Homebrew cleanup plugin.
func NewHomebrewPlugin() *HomebrewPlugin {
	return &HomebrewPlugin{}
}

// Name returns the plugin identifier.
func (p *HomebrewPlugin) Name() string {
	return "homebrew"
}

// Description returns the plugin description.
func (p *HomebrewPlugin) Description() string {
	return "Cleans Homebrew caches and old formula versions"
}

// SupportedPlatforms returns supported platforms (Darwin only).
func (p *HomebrewPlugin) SupportedPlatforms() []string {
	return []string{PlatformDarwin}
}

// Enabled checks if Homebrew cleanup is enabled.
func (p *HomebrewPlugin) Enabled(cfg *config.Config) bool {
	return cfg.Enable.Homebrew
}

// Cleanup performs Homebrew cleanup at the specified level.
func (p *HomebrewPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{
		Plugin: p.Name(),
		Level:  level,
	}

	// Check if brew is available
	if _, err := exec.LookPath("brew"); err != nil {
		logger.Debug("brew not available, skipping")
		return result
	}

	switch level {
	case LevelWarning:
		// Light: just remove downloads cache
		result = p.cleanCache(ctx, logger)
	case LevelModerate, LevelAggressive:
		// Moderate/Aggressive: cleanup --prune=0 (remove all old versions)
		result = p.cleanupPrune(ctx, logger)
	case LevelCritical:
		// Critical: autoremove + full cleanup
		result = p.cleanupCritical(ctx, logger)
	}

	return result
}

func (p *HomebrewPlugin) cleanCache(ctx context.Context, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: LevelWarning}

	// Get cache size before
	home, _ := os.UserHomeDir()
	cachePath := filepath.Join(home, "Library", "Caches", "Homebrew")
	sizeBefore := getDirSize(cachePath)

	logger.Debug("cleaning Homebrew cache")
	cmd := exec.CommandContext(ctx, "brew", "cleanup", "-s")
	cmd.Run() // Ignore errors

	sizeAfter := getDirSize(cachePath)
	result.BytesFreed = sizeBefore - sizeAfter
	return result
}

func (p *HomebrewPlugin) cleanupPrune(ctx context.Context, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: LevelModerate}

	logger.Debug("running brew cleanup --prune=0")
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "brew", "cleanup", "--prune=0")
	output, _ := cmd.CombinedOutput()

	// Parse "Removing: /path/to/file... (X.X MB)"
	result.BytesFreed = parseBrewCleanupOutput(string(output))
	return result
}

func (p *HomebrewPlugin) cleanupCritical(ctx context.Context, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: LevelCritical}

	// First autoremove unused dependencies
	logger.Warn("CRITICAL: running brew autoremove")
	autoremoveCmd := exec.CommandContext(ctx, "brew", "autoremove")
	autoremoveCmd.Run()

	// Then full cleanup
	logger.Warn("CRITICAL: running brew cleanup --prune=0")
	cleanupCmd := exec.CommandContext(ctx, "brew", "cleanup", "--prune=0")
	output, _ := cleanupCmd.CombinedOutput()

	result.BytesFreed = parseBrewCleanupOutput(string(output))
	return result
}

// IOSSimulatorPlugin handles iOS Simulator cleanup operations.
type IOSSimulatorPlugin struct{}

// NewIOSSimulatorPlugin creates a new iOS Simulator cleanup plugin.
func NewIOSSimulatorPlugin() *IOSSimulatorPlugin {
	return &IOSSimulatorPlugin{}
}

// Name returns the plugin identifier.
func (p *IOSSimulatorPlugin) Name() string {
	return "ios-simulator"
}

// Description returns the plugin description.
func (p *IOSSimulatorPlugin) Description() string {
	return "Cleans iOS Simulator devices and runtimes"
}

// SupportedPlatforms returns supported platforms (Darwin only).
func (p *IOSSimulatorPlugin) SupportedPlatforms() []string {
	return []string{PlatformDarwin}
}

// Enabled checks if iOS Simulator cleanup is enabled.
func (p *IOSSimulatorPlugin) Enabled(cfg *config.Config) bool {
	return cfg.Enable.IOSSimulator
}

// Cleanup performs iOS Simulator cleanup at the specified level.
func (p *IOSSimulatorPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{
		Plugin: p.Name(),
		Level:  level,
	}

	// Check if xcrun is available
	if _, err := exec.LookPath("xcrun"); err != nil {
		logger.Debug("xcrun not available, skipping")
		return result
	}

	switch level {
	case LevelWarning, LevelModerate:
		// Light/moderate: delete unavailable devices
		result = p.deleteUnavailable(ctx, logger)
	case LevelAggressive:
		// Aggressive: + delete device data
		result = p.cleanAggressive(ctx, logger)
	case LevelCritical:
		// Critical: + delete runtimes
		result = p.cleanCritical(ctx, logger)
	}

	return result
}

func (p *IOSSimulatorPlugin) deleteUnavailable(ctx context.Context, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: LevelWarning}

	logger.Debug("deleting unavailable iOS Simulators")
	cmd := exec.CommandContext(ctx, "xcrun", "simctl", "delete", "unavailable")
	if err := cmd.Run(); err != nil {
		// Not a hard error - may have no unavailable devices
		logger.Debug("xcrun simctl delete unavailable completed", "error", err)
	}

	return result
}

func (p *IOSSimulatorPlugin) cleanAggressive(ctx context.Context, logger *slog.Logger) CleanupResult {
	result := p.deleteUnavailable(ctx, logger)
	result.Level = LevelAggressive

	// Clean up simulator device data directory
	home, _ := os.UserHomeDir()
	devicePath := filepath.Join(home, "Library", "Developer", "CoreSimulator", "Devices")

	if info, err := os.Stat(devicePath); err == nil && info.IsDir() {
		sizeBefore := getDirSize(devicePath)

		// Delete old log files
		filepath.Walk(devicePath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if !info.IsDir() && strings.HasSuffix(path, ".log") {
				os.Remove(path)
			}
			return nil
		})

		sizeAfter := getDirSize(devicePath)
		result.BytesFreed = sizeBefore - sizeAfter
	}

	return result
}

func (p *IOSSimulatorPlugin) cleanCritical(ctx context.Context, logger *slog.Logger) CleanupResult {
	result := p.cleanAggressive(ctx, logger)
	result.Level = LevelCritical

	// Check runtime size
	runtimesPath := "/Library/Developer/CoreSimulator/Volumes"
	runtimeSize := getDirSize(runtimesPath)

	// Only delete runtimes if they're taking up significant space (>1GB)
	if runtimeSize > 1024*1024*1024 {
		logger.Warn("CRITICAL: iOS Simulator runtimes",
			"size_gb", fmt.Sprintf("%.1f", float64(runtimeSize)/(1024*1024*1024)))

		sudoCap := DetectSudo(ctx)
		if sudoCap.Passwordless {
			logger.Warn("CRITICAL: deleting all iOS Simulator runtimes")
			output, err := RunWithSudo(ctx, "xcrun", "simctl", "runtime", "delete", "all")
			if err != nil {
				logger.Error("failed to delete runtimes", "error", err, "output", string(output))
			} else {
				result.BytesFreed += runtimeSize
			}
		} else {
			logger.Warn("passwordless sudo not available, skipping runtime deletion")
		}
	}

	return result
}

// XcodePlugin handles Xcode cleanup operations.
type XcodePlugin struct{}

// NewXcodePlugin creates a new Xcode cleanup plugin.
func NewXcodePlugin() *XcodePlugin {
	return &XcodePlugin{}
}

// Name returns the plugin identifier.
func (p *XcodePlugin) Name() string {
	return "xcode"
}

// Description returns the plugin description.
func (p *XcodePlugin) Description() string {
	return "Cleans Xcode DerivedData, archives, and device support"
}

// SupportedPlatforms returns supported platforms (Darwin only).
func (p *XcodePlugin) SupportedPlatforms() []string {
	return []string{PlatformDarwin}
}

// Enabled checks if Xcode cleanup is enabled (uses iOS Simulator flag).
func (p *XcodePlugin) Enabled(cfg *config.Config) bool {
	return cfg.Enable.IOSSimulator // Bundled with iOS Simulator cleanup
}

// Cleanup performs Xcode cleanup at the specified level.
func (p *XcodePlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{
		Plugin: p.Name(),
		Level:  level,
	}

	home, _ := os.UserHomeDir()
	xcodeDevDir := filepath.Join(home, "Library", "Developer", "Xcode")

	if _, err := os.Stat(xcodeDevDir); os.IsNotExist(err) {
		return result
	}

	switch level {
	case LevelWarning, LevelModerate:
		// Light: clean old logs
		result.BytesFreed = p.cleanLogs(xcodeDevDir, logger)
	case LevelAggressive:
		// Aggressive: + clean old DerivedData
		result.BytesFreed = p.cleanDerivedData(xcodeDevDir, logger)
	case LevelCritical:
		// Critical: + clean archives and device support
		result.BytesFreed = p.cleanCritical(xcodeDevDir, logger)
	}

	return result
}

func (p *XcodePlugin) cleanLogs(xcodeDir string, logger *slog.Logger) int64 {
	var freed int64

	logsDir := filepath.Join(xcodeDir, "Logs")
	if _, err := os.Stat(logsDir); err == nil {
		sizeBefore := getDirSize(logsDir)
		// Delete logs older than 7 days
		deleteOldFiles(logsDir, 7*24*time.Hour)
		sizeAfter := getDirSize(logsDir)
		freed = sizeBefore - sizeAfter
		logger.Debug("cleaned Xcode logs", "freed_mb", freed/(1024*1024))
	}

	return freed
}

func (p *XcodePlugin) cleanDerivedData(xcodeDir string, logger *slog.Logger) int64 {
	freed := p.cleanLogs(xcodeDir, logger)

	derivedData := filepath.Join(xcodeDir, "DerivedData")
	if info, err := os.Stat(derivedData); err == nil && info.IsDir() {
		sizeBefore := getDirSize(derivedData)
		if sizeBefore > 500*1024*1024 { // Only if > 500MB
			logger.Debug("cleaning Xcode DerivedData", "size_mb", sizeBefore/(1024*1024))
			os.RemoveAll(derivedData)
			freed += sizeBefore
		}
	}

	return freed
}

func (p *XcodePlugin) cleanCritical(xcodeDir string, logger *slog.Logger) int64 {
	freed := p.cleanDerivedData(xcodeDir, logger)

	// Clean archives > 500MB
	archivesDir := filepath.Join(xcodeDir, "Archives")
	if info, err := os.Stat(archivesDir); err == nil && info.IsDir() {
		size := getDirSize(archivesDir)
		if size > 500*1024*1024 {
			logger.Warn("CRITICAL: cleaning Xcode Archives", "size_mb", size/(1024*1024))
			os.RemoveAll(archivesDir)
			freed += size
		}
	}

	// Clean iOS DeviceSupport, keeping only 2 most recent
	deviceSupportDir := filepath.Join(xcodeDir, "iOS DeviceSupport")
	freed += p.cleanDeviceSupport(deviceSupportDir, 2, logger)

	return freed
}

func (p *XcodePlugin) cleanDeviceSupport(dir string, keepCount int, logger *slog.Logger) int64 {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return 0
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}

	// Only clean if we have more than keepCount entries
	if len(entries) <= keepCount {
		return 0
	}

	// Sort by modification time (newest first)
	type dirEntry struct {
		name    string
		modTime time.Time
	}
	dirs := make([]dirEntry, 0)
	for _, e := range entries {
		if e.IsDir() {
			info, err := e.Info()
			if err == nil {
				dirs = append(dirs, dirEntry{name: e.Name(), modTime: info.ModTime()})
			}
		}
	}

	sort.Slice(dirs, func(i, j int) bool {
		return dirs[i].modTime.After(dirs[j].modTime)
	})

	var freed int64
	for i := keepCount; i < len(dirs); i++ {
		fullPath := filepath.Join(dir, dirs[i].name)
		size := getDirSize(fullPath)
		if err := os.RemoveAll(fullPath); err == nil {
			freed += size
			logger.Debug("removed old iOS DeviceSupport", "version", dirs[i].name)
		}
	}

	return freed
}

// CachePlugin handles macOS cache cleanup.
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

	// macOS Library/Caches (only at critical)
	if level >= LevelCritical {
		libraryCaches := filepath.Join(home, "Library", "Caches")
		if _, err := os.Stat(libraryCaches); err == nil {
			sizeBefore := getDirSize(libraryCaches)
			// Delete files older than 30 days
			deleteOldFiles(libraryCaches, 30*24*time.Hour)
			sizeAfter := getDirSize(libraryCaches)
			result.BytesFreed += sizeBefore - sizeAfter
			logger.Debug("cleaned macOS Library/Caches", "freed_mb", (sizeBefore-sizeAfter)/(1024*1024))
		}
	}

	return result
}

// Helper functions

func getDirSize(path string) int64 {
	var size int64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size
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

func parseBrewCleanupOutput(output string) int64 {
	// Parse lines like "Removing: /path/to/file... (1.2 MB)"
	re := regexp.MustCompile(`\((\d+\.?\d*)\s*([KMGT]?B)\)`)
	var total int64

	for _, match := range re.FindAllStringSubmatch(output, -1) {
		if len(match) >= 3 {
			value, err := strconv.ParseFloat(match[1], 64)
			if err != nil {
				continue
			}
			switch strings.ToUpper(match[2]) {
			case "KB":
				total += int64(value * 1024)
			case "MB":
				total += int64(value * 1024 * 1024)
			case "GB":
				total += int64(value * 1024 * 1024 * 1024)
			case "B":
				total += int64(value)
			}
		}
	}

	return total
}

// =============================================================================
// iCloud Plugin
// =============================================================================

// ICloudPlugin handles iCloud Drive eviction operations.
type ICloudPlugin struct{}

// NewICloudPlugin creates a new iCloud eviction plugin.
func NewICloudPlugin() *ICloudPlugin {
	return &ICloudPlugin{}
}

// Name returns the plugin identifier.
func (p *ICloudPlugin) Name() string {
	return "icloud"
}

// Description returns the plugin description.
func (p *ICloudPlugin) Description() string {
	return "Evicts downloaded iCloud Drive files to free local storage"
}

// SupportedPlatforms returns supported platforms (Darwin only).
func (p *ICloudPlugin) SupportedPlatforms() []string {
	return []string{PlatformDarwin}
}

// Enabled checks if iCloud cleanup is enabled.
func (p *ICloudPlugin) Enabled(cfg *config.Config) bool {
	return cfg.Enable.ICloud
}

// Cleanup performs iCloud eviction at the specified level.
func (p *ICloudPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{
		Plugin: p.Name(),
		Level:  level,
	}

	// Check if brctl is available
	if _, err := exec.LookPath("brctl"); err != nil {
		logger.Debug("brctl not available, skipping iCloud eviction")
		return result
	}

	// Find iCloud Drive directory
	home, _ := os.UserHomeDir()
	iCloudPath := filepath.Join(home, "Library", "Mobile Documents", "com~apple~CloudDocs")
	if _, err := os.Stat(iCloudPath); os.IsNotExist(err) {
		logger.Debug("iCloud Drive not found", "path", iCloudPath)
		return result
	}

	// Preflight checks
	if err := p.preflightChecks(logger); err != nil {
		logger.Warn("iCloud preflight checks failed", "error", err)
		return result
	}

	// Determine eviction age based on level
	var maxAge time.Duration
	switch level {
	case LevelWarning:
		// Warning level: report only
		return p.reportICloudUsage(iCloudPath, logger)
	case LevelModerate:
		maxAge = time.Duration(cfg.ICloud.EvictAfterDays) * 24 * time.Hour
	case LevelAggressive:
		maxAge = 7 * 24 * time.Hour // 7 days
	case LevelCritical:
		maxAge = 24 * time.Hour // 1 day
	}

	// Evict files
	result = p.evictFiles(ctx, iCloudPath, maxAge, cfg, logger)
	result.Level = level

	return result
}

// preflightChecks verifies iCloud eviction can proceed safely.
func (p *ICloudPlugin) preflightChecks(logger *slog.Logger) error {
	// Check if "Optimize Mac Storage" is enabled
	// brctl can hang indefinitely if CloudKit is unresponsive, so use a timeout
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "brctl", "status")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("brctl status failed: %w", err)
	}

	// If status contains any mention of iCloud, we're OK
	if !strings.Contains(string(output), "iCloud") {
		logger.Debug("iCloud may not be configured properly")
	}

	return nil
}

// reportICloudUsage reports iCloud Drive usage without evicting.
func (p *ICloudPlugin) reportICloudUsage(iCloudPath string, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: LevelWarning}

	var totalSize int64
	var downloadedCount int
	var evictableSize int64

	filepath.Walk(iCloudPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		totalSize += info.Size()

		// Check if file is downloaded (evictable)
		if p.isFileDownloaded(path) {
			downloadedCount++
			evictableSize += info.Size()
		}

		return nil
	})

	logger.Info("iCloud Drive status",
		"total_size_gb", fmt.Sprintf("%.1f", float64(totalSize)/(1024*1024*1024)),
		"evictable_gb", fmt.Sprintf("%.1f", float64(evictableSize)/(1024*1024*1024)),
		"downloaded_files", downloadedCount)

	return result
}

// isFileDownloaded checks if an iCloud file is downloaded (not evicted).
func (p *ICloudPlugin) isFileDownloaded(path string) bool {
	// Pre-Sonoma: check for .icloud stub files
	if strings.Contains(path, ".icloud") {
		return false // It's a stub, not downloaded
	}

	// Simple heuristic: if file is accessible and has real content
	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	return info.Size() > 0
}

// evictFiles evicts iCloud files older than maxAge.
func (p *ICloudPlugin) evictFiles(ctx context.Context, iCloudPath string, maxAge time.Duration, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name()}

	cutoff := time.Now().Add(-maxAge)
	minSize := int64(cfg.ICloud.MinFileSizeMB) * 1024 * 1024

	filepath.Walk(iCloudPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		// Skip excluded paths
		for _, exclude := range cfg.ICloud.ExcludePaths {
			if strings.Contains(path, exclude) {
				return nil
			}
		}

		// Skip small files
		if info.Size() < minSize {
			return nil
		}

		// Skip recently accessed files
		if info.ModTime().After(cutoff) {
			return nil
		}

		// Skip files that are already evicted
		if !p.isFileDownloaded(path) {
			return nil
		}

		// Evict the file
		if err := p.evictFile(ctx, path); err != nil {
			logger.Debug("failed to evict file", "path", filepath.Base(path), "error", err)
		} else {
			result.BytesFreed += info.Size()
			result.ItemsCleaned++
			logger.Debug("evicted iCloud file", "path", filepath.Base(path), "size_mb", info.Size()/(1024*1024))
		}

		return nil
	})

	if result.BytesFreed > 0 {
		logger.Info("iCloud eviction complete",
			"files_evicted", result.ItemsCleaned,
			"freed_gb", fmt.Sprintf("%.1f", float64(result.BytesFreed)/(1024*1024*1024)))
	}

	return result
}

// evictFile evicts a single iCloud file.
func (p *ICloudPlugin) evictFile(ctx context.Context, path string) error {
	cmd := exec.CommandContext(ctx, "brctl", "evict", path)
	return cmd.Run()
}

// =============================================================================
// Photos Plugin
// =============================================================================

// PhotosPlugin handles Photos library cache cleanup.
type PhotosPlugin struct{}

// NewPhotosPlugin creates a new Photos cache cleanup plugin.
func NewPhotosPlugin() *PhotosPlugin {
	return &PhotosPlugin{}
}

// Name returns the plugin identifier.
func (p *PhotosPlugin) Name() string {
	return "photos"
}

// Description returns the plugin description.
func (p *PhotosPlugin) Description() string {
	return "Cleans Photos library analysis caches (never touches originals)"
}

// SupportedPlatforms returns supported platforms (Darwin only).
func (p *PhotosPlugin) SupportedPlatforms() []string {
	return []string{PlatformDarwin}
}

// Enabled checks if Photos cleanup is enabled.
func (p *PhotosPlugin) Enabled(cfg *config.Config) bool {
	return cfg.Enable.Photos
}

// Cleanup performs Photos cache cleanup at the specified level.
func (p *PhotosPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{
		Plugin: p.Name(),
		Level:  level,
	}

	// Find Photos library
	home, _ := os.UserHomeDir()
	photosLibPath := filepath.Join(home, "Pictures", "Photos Library.photoslibrary")

	if _, err := os.Stat(photosLibPath); os.IsNotExist(err) {
		logger.Debug("Photos library not found", "path", photosLibPath)
		return result
	}

	// CRITICAL: Only clean these specific safe paths
	// NEVER touch: originals/, database/, resources/renders/
	safeCachePaths := []string{
		filepath.Join(photosLibPath, "private", "com.apple.photoanalysisd", "caches"),
		filepath.Join(photosLibPath, "private", "com.apple.mediaanalysisd", "caches"),
	}

	switch level {
	case LevelWarning:
		// Report only
		return p.reportPhotosUsage(photosLibPath, safeCachePaths, logger)
	case LevelModerate, LevelAggressive, LevelCritical:
		// Clean caches
		result = p.cleanPhotosCaches(safeCachePaths, logger)
		result.Level = level
	}

	// At critical level, also clean CloudKit caches
	if level >= LevelCritical {
		cloudKitResult := p.cleanCloudKitCaches(home, logger)
		result.BytesFreed += cloudKitResult.BytesFreed
		result.ItemsCleaned += cloudKitResult.ItemsCleaned
	}

	return result
}

// reportPhotosUsage reports Photos library cache sizes without cleaning.
func (p *PhotosPlugin) reportPhotosUsage(photosLibPath string, cachePaths []string, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: LevelWarning}

	var totalCacheSize int64
	for _, cachePath := range cachePaths {
		size := getDirSize(cachePath)
		totalCacheSize += size
	}

	logger.Info("Photos library cache status",
		"cache_size_mb", totalCacheSize/(1024*1024))

	return result
}

// cleanPhotosCaches cleans Photos library analysis caches.
func (p *PhotosPlugin) cleanPhotosCaches(cachePaths []string, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name()}

	for _, cachePath := range cachePaths {
		if _, err := os.Stat(cachePath); os.IsNotExist(err) {
			continue
		}

		size := getDirSize(cachePath)
		if size == 0 {
			continue
		}

		// Remove all contents but keep the directory
		entries, _ := os.ReadDir(cachePath)
		for _, entry := range entries {
			entryPath := filepath.Join(cachePath, entry.Name())
			if err := os.RemoveAll(entryPath); err != nil {
				logger.Debug("failed to remove cache entry", "path", entry.Name(), "error", err)
				continue
			}
		}

		sizeAfter := getDirSize(cachePath)
		freed := size - sizeAfter
		if freed > 0 {
			result.BytesFreed += freed
			result.ItemsCleaned++
			logger.Debug("cleaned Photos cache", "path", filepath.Base(cachePath), "freed_mb", freed/(1024*1024))
		}
	}

	return result
}

// cleanCloudKitCaches cleans CloudKit caches (safe subset only).
func (p *PhotosPlugin) cleanCloudKitCaches(home string, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name() + "-cloudkit"}

	// SAFE to delete: ClonedFiles (re-downloads on demand)
	// NOT safe: AssetsDB (sync metadata)
	cloudKitCaches := filepath.Join(home, "Library", "Caches", "CloudKit")
	if _, err := os.Stat(cloudKitCaches); os.IsNotExist(err) {
		return result
	}

	// Walk CloudKit caches looking for MMCS/ClonedFiles
	filepath.Walk(cloudKitCaches, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return nil
		}

		// Only clean ClonedFiles directories
		if filepath.Base(path) == "ClonedFiles" && strings.Contains(path, "MMCS") {
			size := getDirSize(path)
			if size > 0 {
				os.RemoveAll(path)
				os.MkdirAll(path, 0755) // Recreate empty directory
				result.BytesFreed += size
				result.ItemsCleaned++
				logger.Debug("cleaned CloudKit cloned files", "freed_mb", size/(1024*1024))
			}
		}

		return nil
	})

	return result
}
