// Package plugins provides cleanup plugin implementations.
// devartifacts.go scans for stale development artifacts like node_modules,
// .venv, Rust target/, Go build cache, Haskell caches, and LM Studio models.
package plugins

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// DevArtifactsPlugin handles stale development artifact cleanup.
type DevArtifactsPlugin struct {
	activeProcesses func(context.Context) (map[string]string, error)
}

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

// PlanCleanup reports stale development artifact candidates without deleting them.
func (p *DevArtifactsPlugin) PlanCleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupPlan {
	_ = logger

	home, _ := os.UserHomeDir()
	daCfg := cfg.DevArtifacts
	nodeAge, venvAge, rustAge, mutates := devArtifactThresholds(level)
	plan := CleanupPlan{
		Plugin:   p.Name(),
		Level:    level.String(),
		Summary:  "Development artifact cleanup plan",
		WouldRun: true,
		Steps: []string{
			"Scan configured development workspaces for rebuildable artifact directories",
			"Use project marker mtimes to classify stale node_modules, .venv, and Rust target directories",
			"Protect artifact families when matching package manager, compiler, language server, or runtime processes are active",
			"Honor configured protected paths before any deletion candidate is eligible",
		},
		Metadata: map[string]string{
			"scan_path_count": strconv.Itoa(len(daCfg.ScanPaths)),
			"mutates":         strconv.FormatBool(mutates),
		},
	}
	if !mutates {
		plan.Warnings = append(plan.Warnings, "warning level reports development artifacts only; moderate or higher is required for deletion")
	}

	active, activeErr := p.activeDevArtifactProcesses(ctx)
	if activeErr != nil {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("could not inspect active development processes: %v", activeErr))
	} else if len(active) > 0 {
		plan.Metadata["active_dev_artifacts"] = strings.Join(devArtifactActivityReasons(active), ", ")
	}

	var targets []CleanupTarget
	for _, scanPath := range daCfg.ScanPaths {
		expanded := expandHome(scanPath, home)
		if !pathExistsAndIsDir(expanded) {
			continue
		}
		if daCfg.NodeModules {
			p.planNodeModules(expanded, nodeAge, mutates, daCfg.ProtectPaths, active, &targets)
		}
		if daCfg.PythonVenvs {
			p.planPythonVenvs(expanded, venvAge, mutates, daCfg.ProtectPaths, active, &targets)
		}
		if daCfg.RustTargets {
			p.planRustTargets(expanded, rustAge, mutates, daCfg.ProtectPaths, active, &targets)
		}
	}

	if daCfg.GoBuildCache {
		p.planGoBuildCache(ctx, level, active, &targets)
	}
	if daCfg.HaskellCache {
		p.planHaskellCaches(home, level, active, &targets)
	}
	if daCfg.LMStudioModels {
		p.planLMStudioModels(home, level, active, &targets)
	}

	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Bytes == targets[j].Bytes {
			return targets[i].Path < targets[j].Path
		}
		return targets[i].Bytes > targets[j].Bytes
	})

	var total, estimated int64
	for _, target := range targets {
		total += target.Bytes
		if target.Action == "delete" || target.Action == "clean-cache" {
			estimated += target.Bytes
		}
	}
	plan.Targets = targets
	plan.EstimatedBytesFreed = estimated
	plan.Metadata["target_count"] = strconv.Itoa(len(targets))
	plan.Metadata["total_physical_bytes"] = strconv.FormatInt(total, 10)

	return plan
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
	nodeAge, venvAge, rustAge, mutates := devArtifactThresholds(level)
	if !mutates {
		// Report only - no deletion
		p.reportArtifacts(ctx, daCfg, home, logger)
		return result
	}

	active, activeErr := p.activeDevArtifactProcesses(ctx)
	if activeErr != nil {
		logger.Warn("skipping dev artifact cleanup because active process inspection failed", "error", activeErr)
		return result
	}

	// Scan configured paths for dev artifacts
	for _, scanPath := range daCfg.ScanPaths {
		expanded := expandHome(scanPath, home)
		if !pathExistsAndIsDir(expanded) {
			continue
		}

		if daCfg.NodeModules && !devArtifactFamilyActive(active, "node_modules") {
			freed := p.cleanNodeModules(ctx, expanded, nodeAge, daCfg.ProtectPaths, logger)
			result.BytesFreed += freed
			if freed > 0 {
				result.ItemsCleaned++
			}
		}

		if daCfg.PythonVenvs && !devArtifactFamilyActive(active, "python-venv") {
			freed := p.cleanPythonVenvs(ctx, expanded, venvAge, daCfg.ProtectPaths, logger)
			result.BytesFreed += freed
			if freed > 0 {
				result.ItemsCleaned++
			}
		}

		if daCfg.RustTargets && !devArtifactFamilyActive(active, "rust-target") {
			freed := p.cleanRustTargets(ctx, expanded, rustAge, daCfg.ProtectPaths, logger)
			result.BytesFreed += freed
			if freed > 0 {
				result.ItemsCleaned++
			}
		}
	}

	// Go build cache (not path-dependent - it's a global cache)
	if daCfg.GoBuildCache && !devArtifactFamilyActive(active, "go-build-cache") {
		freed := p.cleanGoBuildCache(ctx, level, logger)
		result.BytesFreed += freed
		if freed > 0 {
			result.ItemsCleaned++
		}
	}

	// Haskell cache cleanup
	if daCfg.HaskellCache && !devArtifactFamilyActive(active, "haskell-cache") {
		freed := p.cleanHaskellCache(ctx, level, home, logger)
		result.BytesFreed += freed
		if freed > 0 {
			result.ItemsCleaned++
		}
	}

	// LM Studio models (opt-in only)
	if daCfg.LMStudioModels && !devArtifactFamilyActive(active, "lmstudio-models") {
		freed := p.cleanLMStudioModels(ctx, level, home, logger)
		result.BytesFreed += freed
		if freed > 0 {
			result.ItemsCleaned++
		}
	}

	return result
}

func devArtifactThresholds(level CleanupLevel) (nodeAge, venvAge, rustAge time.Duration, mutates bool) {
	switch level {
	case LevelModerate:
		return 30 * 24 * time.Hour, 60 * 24 * time.Hour, 30 * 24 * time.Hour, true
	case LevelAggressive:
		return 7 * 24 * time.Hour, 14 * 24 * time.Hour, 7 * 24 * time.Hour, true
	case LevelCritical:
		return 0, 0, 0, true
	default:
		return 0, 0, 0, false
	}
}

func (p *DevArtifactsPlugin) planNodeModules(scanPath string, maxAge time.Duration, mutates bool, protectPaths []string, active map[string]string, targets *[]CleanupTarget) {
	p.findArtifactDirs(scanPath, "node_modules", "package.json", func(dir string, size int64) {
		marker := filepath.Join(filepath.Dir(dir), "package.json")
		stale := maxAge == 0 || p.isFileStale(marker, maxAge)
		*targets = append(*targets, p.devArtifactTarget("node_modules", "node_modules", dir, size, stale, mutates, p.isProtected(dir, protectPaths), "package.json", maxAge, active))
	})
}

func (p *DevArtifactsPlugin) planPythonVenvs(scanPath string, maxAge time.Duration, mutates bool, protectPaths []string, active map[string]string, targets *[]CleanupTarget) {
	markers := []string{"pyproject.toml", "setup.py", "requirements.txt"}
	p.findArtifactDirs(scanPath, ".venv", "", func(dir string, size int64) {
		stale := maxAge == 0 || p.pythonProjectStale(filepath.Dir(dir), markers, maxAge)
		*targets = append(*targets, p.devArtifactTarget("python-venv", ".venv", dir, size, stale, mutates, p.isProtected(dir, protectPaths), strings.Join(markers, ", "), maxAge, active))
	})
}

func (p *DevArtifactsPlugin) planRustTargets(scanPath string, maxAge time.Duration, mutates bool, protectPaths []string, active map[string]string, targets *[]CleanupTarget) {
	p.findArtifactDirs(scanPath, "target", "Cargo.toml", func(dir string, size int64) {
		marker := filepath.Join(filepath.Dir(dir), "Cargo.toml")
		stale := maxAge == 0 || p.isFileStale(marker, maxAge)
		*targets = append(*targets, p.devArtifactTarget("rust-target", "target", dir, size, stale, mutates, p.isProtected(dir, protectPaths), "Cargo.toml", maxAge, active))
	})
}

func (p *DevArtifactsPlugin) devArtifactTarget(targetType, name, path string, bytes int64, stale, mutates, protected bool, marker string, maxAge time.Duration, active map[string]string) CleanupTarget {
	activeReason, isActive := active[targetType]
	target := CleanupTarget{
		Type:      targetType,
		Name:      name,
		Path:      path,
		Bytes:     bytes,
		Active:    isActive,
		Protected: isActive || protected || !stale,
	}
	switch {
	case isActive:
		target.Action = "protect"
		target.Reason = "active development process detected: " + activeReason
	case protected:
		target.Action = "protect"
		target.Reason = "path is covered by dev_artifacts.protect_paths"
	case !mutates:
		target.Action = "report"
		target.Reason = "warning level reports development artifacts without deleting them"
	case stale:
		target.Action = "delete"
		target.Reason = fmt.Sprintf("project marker %s is stale for %s", marker, formatDevArtifactAge(maxAge))
	default:
		target.Action = "protect"
		target.Reason = fmt.Sprintf("project marker %s is newer than %s", marker, formatDevArtifactAge(maxAge))
	}
	annotateCleanupTargetPolicy(&target, devArtifactTier(targetType), hostReclaimForAction(target.Action))
	return target
}

func devArtifactTier(targetType string) string {
	switch targetType {
	case "go-build-cache", "haskell-ghcup-cache":
		return CleanupTierSafe
	case "lmstudio-models":
		return CleanupTierDestructive
	default:
		return CleanupTierWarm
	}
}

func (p *DevArtifactsPlugin) pythonProjectStale(parentDir string, markers []string, maxAge time.Duration) bool {
	for _, marker := range markers {
		markerPath := filepath.Join(parentDir, marker)
		if !p.isFileStale(markerPath, maxAge) {
			return false
		}
	}
	return true
}

func (p *DevArtifactsPlugin) planGoBuildCache(ctx context.Context, level CleanupLevel, active map[string]string, targets *[]CleanupTarget) {
	dir := p.getGoCacheDir(ctx)
	if dir == "" || !pathExistsAndIsDir(dir) {
		return
	}
	size := getDirAllocatedBytes(dir)
	if size == 0 {
		return
	}
	target := CleanupTarget{
		Type:  "go-build-cache",
		Name:  "GOCACHE",
		Path:  dir,
		Bytes: size,
	}
	if activeReason, ok := active["go-build-cache"]; ok {
		target.Action = "protect"
		target.Active = true
		target.Protected = true
		target.Reason = "active development process detected: " + activeReason
		annotateCleanupTargetPolicy(&target, CleanupTierSafe, hostReclaimForAction(target.Action))
		*targets = append(*targets, target)
		return
	}
	switch level {
	case LevelModerate:
		target.Action = "clean-testcache"
		target.Protected = true
		target.Reason = "moderate level runs go clean -testcache; full build cache is preserved"
	case LevelAggressive, LevelCritical:
		target.Action = "clean-cache"
		target.Reason = "aggressive or critical level runs go clean -cache"
	default:
		target.Action = "report"
		target.Protected = true
		target.Reason = "warning level reports Go build cache without deleting it"
	}
	annotateCleanupTargetPolicy(&target, CleanupTierSafe, hostReclaimForAction(target.Action))
	*targets = append(*targets, target)
}

func (p *DevArtifactsPlugin) planHaskellCaches(home string, level CleanupLevel, active map[string]string, targets *[]CleanupTarget) {
	ghcupCache := filepath.Join(home, ".ghcup", "cache")
	if pathExistsAndIsDir(ghcupCache) {
		target := CleanupTarget{
			Type:  "haskell-ghcup-cache",
			Name:  ".ghcup/cache",
			Path:  ghcupCache,
			Bytes: getDirAllocatedBytes(ghcupCache),
		}
		if activeReason, ok := active["haskell-cache"]; ok {
			target.Action = "protect"
			target.Active = true
			target.Protected = true
			target.Reason = "active development process detected: " + activeReason
		} else if level >= LevelModerate {
			target.Action = "delete"
			target.Reason = ".ghcup/cache contains rebuildable/downloadable artifacts"
		} else {
			target.Action = "report"
			target.Protected = true
			target.Reason = "warning level reports Haskell caches without deleting them"
		}
		annotateCleanupTargetPolicy(&target, CleanupTierSafe, hostReclaimForAction(target.Action))
		*targets = append(*targets, target)
	}

	cabalStore := filepath.Join(home, ".cabal", "store")
	if pathExistsAndIsDir(cabalStore) {
		target := CleanupTarget{
			Type:  "haskell-cabal-store",
			Name:  ".cabal/store",
			Path:  cabalStore,
			Bytes: getDirAllocatedBytes(cabalStore),
		}
		if activeReason, ok := active["haskell-cache"]; ok {
			target.Action = "protect"
			target.Active = true
			target.Protected = true
			target.Reason = "active development process detected: " + activeReason
			annotateCleanupTargetPolicy(&target, CleanupTierWarm, hostReclaimForAction(target.Action))
			*targets = append(*targets, target)
			return
		}
		if level >= LevelAggressive {
			target.Action = "clean-stale-files"
			target.Reason = "aggressive or critical level deletes old .cabal/store files"
		} else {
			target.Action = "report"
			target.Protected = true
			target.Reason = "moderate and warning levels preserve .cabal/store"
		}
		annotateCleanupTargetPolicy(&target, CleanupTierWarm, hostReclaimForAction(target.Action))
		*targets = append(*targets, target)
	}
}

func (p *DevArtifactsPlugin) planLMStudioModels(home string, level CleanupLevel, active map[string]string, targets *[]CleanupTarget) {
	dir := filepath.Join(home, ".lmstudio", "models")
	if !pathExistsAndIsDir(dir) {
		return
	}
	target := CleanupTarget{
		Type:  "lmstudio-models",
		Name:  ".lmstudio/models",
		Path:  dir,
		Bytes: getDirAllocatedBytes(dir),
	}
	if activeReason, ok := active["lmstudio-models"]; ok {
		target.Action = "protect"
		target.Active = true
		target.Protected = true
		target.Reason = "active development process detected: " + activeReason
		annotateCleanupTargetPolicy(&target, CleanupTierDestructive, hostReclaimForAction(target.Action))
		*targets = append(*targets, target)
		return
	}
	if level >= LevelCritical {
		target.Action = "clean-stale-files"
		target.Reason = "critical level deletes LM Studio model files older than 30 days"
	} else {
		target.Action = "report"
		target.Protected = true
		target.Reason = "LM Studio model cleanup is opt-in and reports until critical"
	}
	annotateCleanupTargetPolicy(&target, CleanupTierDestructive, hostReclaimForAction(target.Action))
	*targets = append(*targets, target)
}

func (p *DevArtifactsPlugin) activeDevArtifactProcesses(ctx context.Context) (map[string]string, error) {
	if p.activeProcesses != nil {
		return p.activeProcesses(ctx)
	}

	psCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(psCtx, "ps", "-axo", "comm=,args=")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return devArtifactBusyProcessReasons(string(output)), nil
}

func devArtifactBusyProcessReasons(output string) map[string]string {
	active := map[string]string{}
	add := func(targetType, reason string) {
		if _, ok := active[targetType]; !ok {
			active[targetType] = reason
		}
	}

	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.ToLower(line))
		if len(fields) == 0 {
			continue
		}
		command := filepath.Base(fields[0])
		arg0 := command
		if len(fields) > 1 {
			arg0 = filepath.Base(fields[1])
		}
		normalized := strings.Join(fields, " ")

		switch {
		case command == "npm" || command == "pnpm" || command == "yarn" || command == "bun" ||
			arg0 == "npm" || arg0 == "pnpm" || arg0 == "yarn" || arg0 == "bun" ||
			command == "node" || arg0 == "node":
			add("node_modules", "Node.js package manager or runtime")
		case command == "python" || command == "python3" || command == "pip" || command == "pip3" ||
			command == "uv" || command == "poetry" || command == "pytest" ||
			arg0 == "python" || arg0 == "python3" || arg0 == "pip" || arg0 == "pip3" ||
			arg0 == "uv" || arg0 == "poetry" || arg0 == "pytest":
			add("python-venv", "Python toolchain process")
		case command == "cargo" || command == "rustc" || command == "rust-analyzer" ||
			arg0 == "cargo" || arg0 == "rustc" || arg0 == "rust-analyzer":
			add("rust-target", "Rust toolchain process")
		case (command == "go" || arg0 == "go") &&
			(strings.Contains(normalized, " go build") ||
				strings.Contains(normalized, " go test") ||
				strings.Contains(normalized, " go run") ||
				strings.Contains(normalized, " go install") ||
				strings.Contains(normalized, " go clean") ||
				strings.Contains(normalized, " go work")):
			add("go-build-cache", "Go toolchain process")
		case command == "gopls" || arg0 == "gopls":
			add("go-build-cache", "Go language server process")
		case command == "cabal" || command == "stack" || command == "ghcup" || command == "ghc" ||
			arg0 == "cabal" || arg0 == "stack" || arg0 == "ghcup" || arg0 == "ghc":
			add("haskell-cache", "Haskell toolchain process")
		case strings.Contains(normalized, "lm studio") || strings.Contains(normalized, "lmstudio"):
			add("lmstudio-models", "LM Studio process")
		}
	}
	return active
}

func devArtifactActivityReasons(active map[string]string) []string {
	reasons := make([]string, 0, len(active))
	for targetType, reason := range active {
		reasons = append(reasons, targetType+": "+reason)
	}
	sort.Strings(reasons)
	return reasons
}

func devArtifactFamilyActive(active map[string]string, targetType string) bool {
	_, ok := active[targetType]
	return ok
}

func formatDevArtifactAge(maxAge time.Duration) string {
	if maxAge <= 0 {
		return "any age"
	}
	if maxAge%(24*time.Hour) == 0 {
		days := int(maxAge / (24 * time.Hour))
		if days == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", days)
	}
	return maxAge.String()
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
