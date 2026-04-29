// Package plugins provides cleanup plugin implementations.
// devartifacts.go scans for stale development artifacts like node_modules,
// .venv, Rust target/, Zig artifacts, Go build cache, Haskell caches,
// LM Studio models, and review-only large local artifacts.
package plugins

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
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

const devArtifactRecentOutputGrace = 2 * time.Hour

var tempArtifactPathPattern = regexp.MustCompile(`(?:/private)?/tmp/[^\s"'<>]+|/var/tmp/[^\s"'<>]+`)

var errDevArtifactScanBudgetExceeded = errors.New("dev artifact scan budget exceeded")

type devArtifactScanBudget struct {
	maxDuration  time.Duration
	maxEntries   int
	tempMaxRoots int

	entries       int
	tempRoots     int
	tempRootSeen  map[string]struct{}
	truncatedPath map[string]string
}

func newDevArtifactScanBudget(cfg config.DevArtifactsConfig) *devArtifactScanBudget {
	return &devArtifactScanBudget{
		maxDuration:   parseNixPolicyDuration(cfg.ScanMaxDuration, 30*time.Second),
		maxEntries:    cfg.ScanMaxEntries,
		tempMaxRoots:  cfg.TempScanMaxRoots,
		tempRootSeen:  map[string]struct{}{},
		truncatedPath: map[string]string{},
	}
}

func optionalDevArtifactScanBudget(budgets []*devArtifactScanBudget) *devArtifactScanBudget {
	if len(budgets) == 0 {
		return nil
	}
	return budgets[0]
}

func (b *devArtifactScanBudget) context(ctx context.Context) (context.Context, context.CancelFunc) {
	if b == nil || b.maxDuration <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, b.maxDuration)
}

func (b *devArtifactScanBudget) checkPath(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		if b != nil && b.maxDuration > 0 && errors.Is(err, context.DeadlineExceeded) {
			b.markTruncated(path, fmt.Sprintf("scan duration exceeded %s", b.maxDuration))
			return errDevArtifactScanBudgetExceeded
		}
		return err
	}
	if b == nil {
		return nil
	}
	if b.maxEntries > 0 && b.entries >= b.maxEntries {
		b.markTruncated(path, fmt.Sprintf("scan entry budget exceeded %d entries", b.maxEntries))
		return errDevArtifactScanBudgetExceeded
	}
	b.entries++
	return nil
}

func (b *devArtifactScanBudget) checkTempRoot(ctx context.Context, path string) error {
	if err := b.checkPath(ctx, path); err != nil {
		return err
	}
	if b == nil {
		return nil
	}
	cleanPath := filepath.Clean(path)
	if _, ok := b.tempRootSeen[cleanPath]; ok {
		return nil
	}
	if b.tempMaxRoots > 0 && b.tempRoots >= b.tempMaxRoots {
		b.markTruncated(path, fmt.Sprintf("temporary root budget exceeded %d roots", b.tempMaxRoots))
		return errDevArtifactScanBudgetExceeded
	}
	b.tempRootSeen[cleanPath] = struct{}{}
	b.tempRoots++
	return nil
}

func (b *devArtifactScanBudget) markContextError(ctx context.Context, path string) {
	if b == nil || b.maxDuration <= 0 || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return
	}
	b.markTruncated(path, fmt.Sprintf("scan duration exceeded %s", b.maxDuration))
}

func (b *devArtifactScanBudget) markTruncated(path, reason string) {
	if b == nil {
		return
	}
	if len(b.truncatedPath) >= 20 {
		return
	}
	b.truncatedPath[path] = reason
}

func (b *devArtifactScanBudget) exhausted() bool {
	return b != nil && len(b.truncatedPath) > 0
}

func (b *devArtifactScanBudget) truncatedDetails() []string {
	if b == nil || len(b.truncatedPath) == 0 {
		return nil
	}
	details := make([]string, 0, len(b.truncatedPath))
	for path, reason := range b.truncatedPath {
		details = append(details, path+" ("+reason+")")
	}
	sort.Strings(details)
	return details
}

func (b *devArtifactScanBudget) annotatePlan(plan *CleanupPlan) {
	if b == nil {
		return
	}
	plan.Metadata["scan_max_duration"] = b.maxDuration.String()
	plan.Metadata["scan_max_entries"] = strconv.Itoa(b.maxEntries)
	plan.Metadata["temp_scan_max_roots"] = strconv.Itoa(b.tempMaxRoots)
	plan.Metadata["scan_entries_visited"] = strconv.Itoa(b.entries)
	plan.Metadata["temp_roots_visited"] = strconv.Itoa(b.tempRoots)
	plan.Metadata["scan_budget_exhausted"] = strconv.FormatBool(b.exhausted())
	if !b.exhausted() {
		return
	}
	details := b.truncatedDetails()
	plan.Metadata["scan_truncated_paths"] = strings.Join(details, "; ")
	plan.Warnings = append(plan.Warnings,
		"dev-artifacts scan budget was exhausted; dry-run evidence is partial and omitted paths are not cleanup candidates",
		"dev-artifacts scan truncated at: "+strings.Join(details, "; "),
	)
}

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
	return "Cleans stale development artifacts (node_modules, .venv, target/, zig, go cache, haskell, lmstudio) and reports large local artifacts"
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
	nodeAge, venvAge, rustAge, zigAge, mutates := devArtifactThresholds(level)
	scanBudget := newDevArtifactScanBudget(daCfg)
	scanCtx, cancelScan := scanBudget.context(ctx)
	defer cancelScan()
	plan := CleanupPlan{
		Plugin:   p.Name(),
		Level:    level.String(),
		Summary:  "Development artifact cleanup plan",
		WouldRun: true,
		Steps: []string{
			"Scan configured development workspaces for rebuildable artifact directories",
			"Surface large top-level temporary proof/output directories for manual review without deleting them",
			"Use project marker mtimes to classify stale node_modules, .venv, Rust target, and Zig artifact directories",
			"Protect artifact families when matching package manager, compiler, language server, or runtime processes are active",
			"Report large disk images and VM bundles for manual review without deleting them",
			"Honor configured protected paths before any deletion candidate is eligible",
		},
		Metadata: map[string]string{
			"scan_path_count":      strconv.Itoa(len(daCfg.ScanPaths)),
			"temp_scan_path_count": strconv.Itoa(len(daCfg.TempScanPaths)),
			"mutates":              strconv.FormatBool(mutates),
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

	tracker := newDevArtifactGitTracker()
	mountedImages := map[string]string{}
	if daCfg.LargeLocalArtifacts {
		mountedImages = largeLocalMountedDiskImages(ctx)
	}

	var targets []CleanupTarget
	if daCfg.TempArtifacts {
		activeTempRoots := activeTempArtifactRoots(ctx, daCfg.TempScanPaths, home)
		tempMinBytes := tempArtifactMinBytes(daCfg)
		tempStaleAfter := parseNixPolicyDuration(daCfg.TempArtifactStaleAfter, 6*time.Hour)
		for _, scanPath := range daCfg.TempScanPaths {
			expanded := expandHome(scanPath, home)
			if !pathExistsAndIsDir(expanded) {
				continue
			}
			p.planTemporaryArtifacts(scanCtx, expanded, tempMinBytes, tempStaleAfter, daCfg.ProtectPaths, activeTempRoots, &targets, scanBudget)
			p.planTemporaryGeneratedArtifacts(scanCtx, expanded, tempMinBytes, tempStaleAfter, nodeAge, venvAge, rustAge, zigAge, mutates, daCfg, active, activeTempRoots, tracker, &targets, scanBudget)
		}
	}
	for _, scanPath := range daCfg.ScanPaths {
		expanded := expandHome(scanPath, home)
		if !pathExistsAndIsDir(expanded) {
			continue
		}
		if daCfg.NodeModules {
			p.planNodeModules(scanCtx, expanded, nodeAge, mutates, daCfg.ProtectPaths, active, tracker, &targets, scanBudget)
		}
		if daCfg.PythonVenvs {
			p.planPythonVenvs(scanCtx, expanded, venvAge, mutates, daCfg.ProtectPaths, active, tracker, &targets, scanBudget)
		}
		if daCfg.RustTargets {
			p.planRustTargets(scanCtx, expanded, rustAge, mutates, daCfg.ProtectPaths, active, tracker, &targets, scanBudget)
		}
		if daCfg.ZigArtifacts {
			p.planZigArtifacts(scanCtx, expanded, zigAge, mutates, daCfg.ProtectPaths, active, tracker, &targets, scanBudget)
		}
		if daCfg.LargeLocalArtifacts {
			p.planLargeLocalArtifacts(scanCtx, expanded, largeLocalArtifactMinBytes(daCfg), daCfg.ProtectPaths, mountedImages, &targets, scanBudget)
		}
	}

	if daCfg.GoBuildCache {
		p.planGoBuildCache(ctx, level, active, &targets)
	}
	if daCfg.HaskellCache {
		p.planHaskellCaches(ctx, home, level, active, &targets)
	}
	if daCfg.LMStudioModels {
		p.planLMStudioModels(ctx, home, level, active, &targets)
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
	scanBudget.annotatePlan(&plan)

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
	scanBudget := newDevArtifactScanBudget(daCfg)
	scanCtx, cancelScan := scanBudget.context(ctx)
	defer cancelScan()

	// Determine staleness thresholds based on level
	nodeAge, venvAge, rustAge, zigAge, mutates := devArtifactThresholds(level)
	if !mutates {
		// Report only - no deletion
		p.reportArtifacts(scanCtx, daCfg, home, logger, scanBudget)
		if scanBudget.exhausted() {
			logger.Warn("dev artifact report stopped after scan budget was exhausted", "truncated_paths", strings.Join(scanBudget.truncatedDetails(), "; "))
		}
		return result
	}

	active, activeErr := p.activeDevArtifactProcesses(ctx)
	if activeErr != nil {
		logger.Warn("skipping dev artifact cleanup because active process inspection failed", "error", activeErr)
		return result
	}

	tracker := newDevArtifactGitTracker()

	// Scan configured paths for dev artifacts
	for _, scanPath := range daCfg.ScanPaths {
		expanded := expandHome(scanPath, home)
		if !pathExistsAndIsDir(expanded) {
			continue
		}

		if daCfg.NodeModules && !devArtifactFamilyActive(active, "node_modules") {
			freed := p.cleanNodeModules(scanCtx, expanded, nodeAge, daCfg.ProtectPaths, tracker, logger, scanBudget)
			result.BytesFreed += freed
			if freed > 0 {
				result.ItemsCleaned++
			}
		}
		if scanBudget.exhausted() {
			logger.Warn("stopping dev artifact cleanup because scan budget was exhausted", "truncated_paths", strings.Join(scanBudget.truncatedDetails(), "; "))
			return result
		}

		if daCfg.PythonVenvs && !devArtifactFamilyActive(active, "python-venv") {
			freed := p.cleanPythonVenvs(scanCtx, expanded, venvAge, daCfg.ProtectPaths, tracker, logger, scanBudget)
			result.BytesFreed += freed
			if freed > 0 {
				result.ItemsCleaned++
			}
		}
		if scanBudget.exhausted() {
			logger.Warn("stopping dev artifact cleanup because scan budget was exhausted", "truncated_paths", strings.Join(scanBudget.truncatedDetails(), "; "))
			return result
		}

		if daCfg.RustTargets && !devArtifactFamilyActive(active, "rust-target") {
			freed := p.cleanRustTargets(scanCtx, expanded, rustAge, daCfg.ProtectPaths, tracker, logger, scanBudget)
			result.BytesFreed += freed
			if freed > 0 {
				result.ItemsCleaned++
			}
		}
		if scanBudget.exhausted() {
			logger.Warn("stopping dev artifact cleanup because scan budget was exhausted", "truncated_paths", strings.Join(scanBudget.truncatedDetails(), "; "))
			return result
		}

		if daCfg.ZigArtifacts && !devArtifactFamilyActive(active, "zig-artifact") {
			freed := p.cleanZigArtifacts(scanCtx, expanded, zigAge, daCfg.ProtectPaths, tracker, logger, scanBudget)
			result.BytesFreed += freed
			if freed > 0 {
				result.ItemsCleaned++
			}
		}
		if scanBudget.exhausted() {
			logger.Warn("stopping dev artifact cleanup because scan budget was exhausted", "truncated_paths", strings.Join(scanBudget.truncatedDetails(), "; "))
			return result
		}
	}

	if daCfg.TempArtifacts {
		activeTempRoots := activeTempArtifactRoots(ctx, daCfg.TempScanPaths, home)
		tempMinBytes := tempArtifactMinBytes(daCfg)
		tempStaleAfter := parseNixPolicyDuration(daCfg.TempArtifactStaleAfter, 6*time.Hour)
		for _, scanPath := range daCfg.TempScanPaths {
			expanded := expandHome(scanPath, home)
			if !pathExistsAndIsDir(expanded) {
				continue
			}
			freed := p.cleanTemporaryGeneratedArtifacts(scanCtx, expanded, tempMinBytes, tempStaleAfter, nodeAge, venvAge, rustAge, zigAge, daCfg, active, activeTempRoots, tracker, logger, scanBudget)
			result.BytesFreed += freed
			if freed > 0 {
				result.ItemsCleaned++
			}
			if scanBudget.exhausted() {
				logger.Warn("stopping dev artifact cleanup because scan budget was exhausted", "truncated_paths", strings.Join(scanBudget.truncatedDetails(), "; "))
				return result
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

func devArtifactThresholds(level CleanupLevel) (nodeAge, venvAge, rustAge, zigAge time.Duration, mutates bool) {
	switch level {
	case LevelModerate:
		return 30 * 24 * time.Hour, 60 * 24 * time.Hour, 30 * 24 * time.Hour, 30 * 24 * time.Hour, true
	case LevelAggressive:
		return 7 * 24 * time.Hour, 14 * 24 * time.Hour, 7 * 24 * time.Hour, 7 * 24 * time.Hour, true
	case LevelCritical:
		return 0, 0, 0, 0, true
	default:
		return 0, 0, 0, 0, false
	}
}

func (p *DevArtifactsPlugin) planNodeModules(ctx context.Context, scanPath string, maxAge time.Duration, mutates bool, protectPaths []string, active map[string]string, tracker *devArtifactGitTracker, targets *[]CleanupTarget, budgets ...*devArtifactScanBudget) {
	budget := optionalDevArtifactScanBudget(budgets)
	p.findArtifactDirs(ctx, scanPath, "node_modules", "package.json", func(dir string, size int64) {
		marker := filepath.Join(filepath.Dir(dir), "package.json")
		stale := maxAge == 0 || p.isFileStale(marker, maxAge)
		*targets = append(*targets, p.devArtifactTarget("node_modules", "node_modules", dir, size, stale, mutates, p.isProtected(dir, protectPaths), "", tracker.ContainsTrackedFiles(dir), "package.json", maxAge, active))
	}, budget)
}

func (p *DevArtifactsPlugin) planPythonVenvs(ctx context.Context, scanPath string, maxAge time.Duration, mutates bool, protectPaths []string, active map[string]string, tracker *devArtifactGitTracker, targets *[]CleanupTarget, budgets ...*devArtifactScanBudget) {
	budget := optionalDevArtifactScanBudget(budgets)
	markers := []string{"pyproject.toml", "setup.py", "requirements.txt"}
	p.findArtifactDirs(ctx, scanPath, ".venv", "", func(dir string, size int64) {
		stale := maxAge == 0 || p.pythonProjectStale(filepath.Dir(dir), markers, maxAge)
		*targets = append(*targets, p.devArtifactTarget("python-venv", ".venv", dir, size, stale, mutates, p.isProtected(dir, protectPaths), "", tracker.ContainsTrackedFiles(dir), strings.Join(markers, ", "), maxAge, active))
	}, budget)
}

func (p *DevArtifactsPlugin) planRustTargets(ctx context.Context, scanPath string, maxAge time.Duration, mutates bool, protectPaths []string, active map[string]string, tracker *devArtifactGitTracker, targets *[]CleanupTarget, budgets ...*devArtifactScanBudget) {
	budget := optionalDevArtifactScanBudget(budgets)
	p.findArtifactDirs(ctx, scanPath, "target", "Cargo.toml", func(dir string, size int64) {
		marker := filepath.Join(filepath.Dir(dir), "Cargo.toml")
		stale := maxAge == 0 || p.isFileStale(marker, maxAge)
		*targets = append(*targets, p.devArtifactTarget("rust-target", "target", dir, size, stale, mutates, p.isProtected(dir, protectPaths), "", tracker.ContainsTrackedFiles(dir), "Cargo.toml", maxAge, active))
	}, budget)
}

func (p *DevArtifactsPlugin) planZigArtifacts(ctx context.Context, scanPath string, maxAge time.Duration, mutates bool, protectPaths []string, active map[string]string, tracker *devArtifactGitTracker, targets *[]CleanupTarget, budgets ...*devArtifactScanBudget) {
	budget := optionalDevArtifactScanBudget(budgets)
	for _, artifactName := range []string{".zig-cache", "zig-out"} {
		p.findArtifactDirs(ctx, scanPath, artifactName, "build.zig", func(dir string, size int64) {
			marker := filepath.Join(filepath.Dir(dir), "build.zig")
			stale := maxAge == 0 || p.isFileStale(marker, maxAge)
			protected := p.isProtected(dir, protectPaths)
			tracked := tracker.ContainsTrackedFiles(dir)
			recentReason := ""
			if !protected && !tracked {
				recentReason = devArtifactRecentOutputProtectReasonContext(ctx, dir)
			}
			*targets = append(*targets, p.devArtifactTarget("zig-artifact", artifactName, dir, size, stale, mutates, protected || recentReason != "", recentReason, tracked, "build.zig", maxAge, active))
		}, budget)
	}
}

func (p *DevArtifactsPlugin) planLargeLocalArtifacts(ctx context.Context, scanPath string, minBytes int64, protectPaths []string, mountedImages map[string]string, targets *[]CleanupTarget, budgets ...*devArtifactScanBudget) {
	budget := optionalDevArtifactScanBudget(budgets)
	p.findLargeLocalArtifacts(ctx, scanPath, minBytes, protectPaths, mountedImages, func(target CleanupTarget) {
		*targets = append(*targets, target)
	}, budget)
}

func (p *DevArtifactsPlugin) planTemporaryArtifacts(ctx context.Context, scanPath string, minBytes int64, staleAfter time.Duration, protectPaths []string, activeRoots map[string]string, targets *[]CleanupTarget, budgets ...*devArtifactScanBudget) {
	budget := optionalDevArtifactScanBudget(budgets)
	entries, err := os.ReadDir(scanPath)
	if err != nil {
		return
	}
	now := time.Now()
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(scanPath, entry.Name())
		if err := budget.checkTempRoot(ctx, path); err != nil {
			return
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		activeReason := activeRoots[canonicalTempArtifactPath(path)]
		if activeReason != "" {
			*targets = append(*targets, p.temporaryArtifactTarget(path, 0, info.ModTime(), staleAfter, now, p.isProtected(path, protectPaths), activeReason))
			continue
		}
		size, err := getDirAllocatedBytesContext(ctx, path)
		if err != nil {
			budget.markContextError(ctx, path)
			return
		}
		if size < minBytes {
			continue
		}
		*targets = append(*targets, p.temporaryArtifactTarget(path, size, info.ModTime(), staleAfter, now, p.isProtected(path, protectPaths), ""))
	}
}

func (p *DevArtifactsPlugin) planTemporaryGeneratedArtifacts(ctx context.Context, scanPath string, minBytes int64, staleAfter, nodeAge, venvAge, rustAge, zigAge time.Duration, mutates bool, daCfg config.DevArtifactsConfig, active, activeRoots map[string]string, tracker *devArtifactGitTracker, targets *[]CleanupTarget, budgets ...*devArtifactScanBudget) {
	budget := optionalDevArtifactScanBudget(budgets)
	p.forEachStaleTemporaryRoot(ctx, scanPath, minBytes, staleAfter, daCfg.ProtectPaths, activeRoots, func(root string) {
		if daCfg.NodeModules {
			p.planNodeModules(ctx, root, nodeAge, mutates, daCfg.ProtectPaths, active, tracker, targets, budget)
		}
		if daCfg.PythonVenvs {
			p.planPythonVenvs(ctx, root, venvAge, mutates, daCfg.ProtectPaths, active, tracker, targets, budget)
		}
		if daCfg.RustTargets {
			p.planRustTargets(ctx, root, rustAge, mutates, daCfg.ProtectPaths, active, tracker, targets, budget)
		}
		if daCfg.ZigArtifacts {
			p.planZigArtifacts(ctx, root, zigAge, mutates, daCfg.ProtectPaths, active, tracker, targets, budget)
		}
	}, budget)
}

func (p *DevArtifactsPlugin) cleanTemporaryGeneratedArtifacts(ctx context.Context, scanPath string, minBytes int64, staleAfter, nodeAge, venvAge, rustAge, zigAge time.Duration, daCfg config.DevArtifactsConfig, active, activeRoots map[string]string, tracker *devArtifactGitTracker, logger *slog.Logger, budgets ...*devArtifactScanBudget) int64 {
	var totalFreed int64
	budget := optionalDevArtifactScanBudget(budgets)
	p.forEachStaleTemporaryRoot(ctx, scanPath, minBytes, staleAfter, daCfg.ProtectPaths, activeRoots, func(root string) {
		logger.Debug("scanning stale temporary root for generated artifacts", "path", root)
		if daCfg.NodeModules && !devArtifactFamilyActive(active, "node_modules") {
			totalFreed += p.cleanNodeModules(ctx, root, nodeAge, daCfg.ProtectPaths, tracker, logger, budget)
		}
		if daCfg.PythonVenvs && !devArtifactFamilyActive(active, "python-venv") {
			totalFreed += p.cleanPythonVenvs(ctx, root, venvAge, daCfg.ProtectPaths, tracker, logger, budget)
		}
		if daCfg.RustTargets && !devArtifactFamilyActive(active, "rust-target") {
			totalFreed += p.cleanRustTargets(ctx, root, rustAge, daCfg.ProtectPaths, tracker, logger, budget)
		}
		if daCfg.ZigArtifacts && !devArtifactFamilyActive(active, "zig-artifact") {
			totalFreed += p.cleanZigArtifacts(ctx, root, zigAge, daCfg.ProtectPaths, tracker, logger, budget)
		}
	}, budget)
	return totalFreed
}

func (p *DevArtifactsPlugin) forEachStaleTemporaryRoot(ctx context.Context, scanPath string, minBytes int64, staleAfter time.Duration, protectPaths []string, activeRoots map[string]string, callback func(root string), budgets ...*devArtifactScanBudget) {
	budget := optionalDevArtifactScanBudget(budgets)
	entries, err := os.ReadDir(scanPath)
	if err != nil {
		return
	}
	now := time.Now()
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		if !entry.IsDir() {
			continue
		}
		root := filepath.Join(scanPath, entry.Name())
		if err := budget.checkTempRoot(ctx, root); err != nil {
			return
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if p.isProtected(root, protectPaths) {
			continue
		}
		if activeRoots[canonicalTempArtifactPath(root)] != "" {
			continue
		}
		if staleAfter > 0 && info.ModTime().After(now.Add(-staleAfter)) {
			continue
		}
		if minBytes > 0 {
			size, err := getDirAllocatedBytesContext(ctx, root)
			if err != nil {
				budget.markContextError(ctx, root)
				return
			}
			if size < minBytes {
				continue
			}
		}
		callback(root)
	}
}

func (p *DevArtifactsPlugin) temporaryArtifactTarget(path string, physicalBytes int64, modTime time.Time, staleAfter time.Duration, now time.Time, protected bool, activeReason string) CleanupTarget {
	action := "review_temp_artifact"
	reason := fmt.Sprintf("large top-level temporary artifact is older than %s; manual review required before deletion", formatDevArtifactAge(staleAfter))
	active := activeReason != ""
	isProtected := true
	switch {
	case active:
		action = "protect"
		reason = "active process references this temporary path: " + activeReason
	case protected:
		action = "protect"
		reason = "path is covered by dev_artifacts.protect_paths"
	case staleAfter > 0 && modTime.After(now.Add(-staleAfter)):
		action = "protect"
		reason = fmt.Sprintf("temporary artifact is newer than %s", formatDevArtifactAge(staleAfter))
	}
	target := CleanupTarget{
		Type:      "temporary-dev-artifact",
		Name:      filepath.Base(path),
		Path:      path,
		Bytes:     physicalBytes,
		Active:    active,
		Protected: isProtected,
		Action:    action,
		Reason:    reason,
	}
	annotateCleanupTargetPolicy(&target, CleanupTierDestructive, CleanupReclaimNone)
	return target
}

func tempArtifactMinBytes(cfg config.DevArtifactsConfig) int64 {
	if cfg.TempArtifactMinMB <= 0 {
		return 256 * 1024 * 1024
	}
	return int64(cfg.TempArtifactMinMB) * 1024 * 1024
}

func (p *DevArtifactsPlugin) findLargeLocalArtifacts(ctx context.Context, scanPath string, minBytes int64, protectPaths []string, mountedImages map[string]string, callback func(CleanupTarget), budgets ...*devArtifactScanBudget) {
	budget := optionalDevArtifactScanBudget(budgets)
	scanDepth := strings.Count(scanPath, string(os.PathSeparator))
	fileKinds := largeLocalArtifactFileKinds()
	dirKinds := largeLocalArtifactDirKinds()

	filepath.Walk(scanPath, func(path string, info os.FileInfo, err error) error {
		if err := budget.checkPath(ctx, path); err != nil {
			return err
		}
		if err != nil {
			return nil
		}

		currentDepth := strings.Count(path, string(os.PathSeparator)) - scanDepth
		if currentDepth > 4 {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		baseName := filepath.Base(path)
		lowerBase := strings.ToLower(baseName)
		if info.IsDir() {
			if strings.HasPrefix(baseName, ".") {
				return filepath.SkipDir
			}
			ext := filepath.Ext(lowerBase)
			if kind, ok := dirKinds[ext]; ok {
				size, err := getDirAllocatedBytesContext(ctx, path)
				if err != nil {
					budget.markContextError(ctx, path)
					return err
				}
				if size >= minBytes {
					callback(p.largeLocalArtifactTarget(kind, path, size, largeLocalArtifactDirLogicalBytes(ext, path), p.isProtected(path, protectPaths), mountedImages[filepath.Clean(path)]))
				}
				return filepath.SkipDir
			}
			return nil
		}

		if !info.Mode().IsRegular() {
			return nil
		}
		kind, ok := fileKinds[filepath.Ext(lowerBase)]
		if !ok {
			return nil
		}
		physicalBytes, err := getFileAllocatedBytes(path)
		if err != nil {
			physicalBytes = info.Size()
		}
		if physicalBytes < minBytes {
			return nil
		}
		callback(p.largeLocalArtifactTarget(kind, path, physicalBytes, info.Size(), p.isProtected(path, protectPaths), mountedImages[filepath.Clean(path)]))
		return nil
	})
}

func (p *DevArtifactsPlugin) largeLocalArtifactTarget(kind, path string, physicalBytes, logicalBytes int64, protected bool, mountPoint string) CleanupTarget {
	action := "review"
	reason := largeLocalArtifactReviewReason(kind)
	active := mountPoint != ""
	if active {
		action = "protect"
		reason = "disk/image artifact is mounted at " + mountPoint + "; detach before manual cleanup"
	} else if protected {
		action = "protect"
		reason = "path is covered by dev_artifacts.protect_paths"
	}
	target := CleanupTarget{
		Type:         "large-local-artifact",
		Name:         kind,
		Path:         path,
		Bytes:        physicalBytes,
		LogicalBytes: logicalBytes,
		Active:       active,
		Protected:    true,
		Action:       action,
		Reason:       reason,
	}
	annotateCleanupTargetPolicy(&target, CleanupTierDestructive, CleanupReclaimNone)
	return target
}

func largeLocalArtifactReviewReason(kind string) string {
	if kind == "sparse bundle disk image" {
		return "sparsebundle requires manual review; automatic compaction is not assumed to reclaim host space"
	}
	return "large local disk/image artifact requires manual review before removal"
}

func largeLocalArtifactDirLogicalBytes(ext string, path string) int64 {
	if ext != ".sparsebundle" {
		return 0
	}
	return sparseBundleLogicalBytes(path)
}

func sparseBundleLogicalBytes(path string) int64 {
	value, err := plistIntegerValue(filepath.Join(path, "Info.plist"), "size")
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

func plistIntegerValue(path string, key string) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	decoder := xml.NewDecoder(file)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}

		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "key" {
			continue
		}

		var foundKey string
		if err := decoder.DecodeElement(&foundKey, &start); err != nil {
			return 0, err
		}
		if strings.TrimSpace(foundKey) != key {
			continue
		}

		for {
			token, err := decoder.Token()
			if errors.Is(err, io.EOF) {
				return 0, io.EOF
			}
			if err != nil {
				return 0, err
			}
			start, ok := token.(xml.StartElement)
			if !ok {
				continue
			}
			if start.Name.Local != "integer" {
				return 0, fmt.Errorf("plist key %q is %s, not integer", key, start.Name.Local)
			}
			var rawValue string
			if err := decoder.DecodeElement(&rawValue, &start); err != nil {
				return 0, err
			}
			return strconv.ParseInt(strings.TrimSpace(rawValue), 10, 64)
		}
	}
}

func largeLocalMountedDiskImages(ctx context.Context) map[string]string {
	hdiutil, err := exec.LookPath("hdiutil")
	if err != nil {
		return nil
	}
	infoCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(infoCtx, hdiutil, "info").Output()
	if err != nil {
		return nil
	}
	return parseLargeLocalMountedDiskImages(string(output))
}

func parseLargeLocalMountedDiskImages(output string) map[string]string {
	mounted := map[string]string{}
	currentImage := ""
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "image-path") {
			if _, value, ok := strings.Cut(trimmed, ":"); ok {
				currentImage = filepath.Clean(strings.TrimSpace(value))
			}
			continue
		}
		if currentImage == "" || !strings.HasPrefix(trimmed, "/dev/") {
			continue
		}
		mountPoint := largeLocalMountPointFromHdiutilLine(trimmed)
		if mountPoint != "" {
			mounted[currentImage] = mountPoint
		}
	}
	return mounted
}

func largeLocalMountPointFromHdiutilLine(line string) string {
	parts := strings.Split(line, "\t")
	if len(parts) > 1 {
		mountPoint := strings.TrimSpace(parts[len(parts)-1])
		if strings.HasPrefix(mountPoint, "/") {
			return mountPoint
		}
	}
	fields := strings.Fields(line)
	if len(fields) > 0 && strings.HasPrefix(fields[len(fields)-1], "/") {
		return fields[len(fields)-1]
	}
	return ""
}

func largeLocalArtifactMinBytes(cfg config.DevArtifactsConfig) int64 {
	if cfg.LargeLocalArtifactMinMB <= 0 {
		return 1024 * 1024 * 1024
	}
	return int64(cfg.LargeLocalArtifactMinMB) * 1024 * 1024
}

func largeLocalArtifactFileKinds() map[string]string {
	return map[string]string{
		".dmg":   "disk image",
		".img":   "disk image",
		".iso":   "ISO image",
		".qcow2": "qcow2 disk image",
		".raw":   "raw disk image",
		".vhd":   "VHD disk image",
		".vhdx":  "VHDX disk image",
	}
}

func largeLocalArtifactDirKinds() map[string]string {
	return map[string]string{
		".pvm":          "Parallels VM bundle",
		".sparsebundle": "sparse bundle disk image",
		".utm":          "UTM VM bundle",
		".vmwarevm":     "VMware VM bundle",
	}
}

func (p *DevArtifactsPlugin) devArtifactTarget(targetType, name, path string, bytes int64, stale, mutates, protected bool, protectReason string, tracked bool, marker string, maxAge time.Duration, active map[string]string) CleanupTarget {
	activeReason, isActive := active[targetType]
	target := CleanupTarget{
		Type:      targetType,
		Name:      name,
		Path:      path,
		Bytes:     bytes,
		Active:    isActive,
		Protected: isActive || protected || tracked || !stale,
	}
	switch {
	case isActive:
		target.Action = "protect"
		target.Reason = "active development process detected: " + activeReason
	case protected:
		if protectReason == "" {
			protectReason = "path is covered by dev_artifacts.protect_paths"
		}
		target.Action = "protect"
		target.Reason = protectReason
	case tracked:
		target.Action = "protect"
		target.Reason = "artifact directory contains files tracked by Git"
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
	case "lmstudio-models", "large-local-artifact":
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
	size, err := getDirAllocatedBytesContext(ctx, dir)
	if err != nil {
		return
	}
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

func (p *DevArtifactsPlugin) planHaskellCaches(ctx context.Context, home string, level CleanupLevel, active map[string]string, targets *[]CleanupTarget) {
	ghcupCache := filepath.Join(home, ".ghcup", "cache")
	if pathExistsAndIsDir(ghcupCache) {
		bytes, err := getDirAllocatedBytesContext(ctx, ghcupCache)
		if err != nil {
			return
		}
		target := CleanupTarget{
			Type:  "haskell-ghcup-cache",
			Name:  ".ghcup/cache",
			Path:  ghcupCache,
			Bytes: bytes,
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
		bytes, err := getDirAllocatedBytesContext(ctx, cabalStore)
		if err != nil {
			return
		}
		target := CleanupTarget{
			Type:  "haskell-cabal-store",
			Name:  ".cabal/store",
			Path:  cabalStore,
			Bytes: bytes,
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

func (p *DevArtifactsPlugin) planLMStudioModels(ctx context.Context, home string, level CleanupLevel, active map[string]string, targets *[]CleanupTarget) {
	dir := filepath.Join(home, ".lmstudio", "models")
	if !pathExistsAndIsDir(dir) {
		return
	}
	bytes, err := getDirAllocatedBytesContext(ctx, dir)
	if err != nil {
		return
	}
	target := CleanupTarget{
		Type:  "lmstudio-models",
		Name:  ".lmstudio/models",
		Path:  dir,
		Bytes: bytes,
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
		case command == "zig" || command == "zls" ||
			arg0 == "zig" || arg0 == "zls":
			add("zig-artifact", "Zig toolchain process")
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

func activeTempArtifactRoots(ctx context.Context, scanPaths []string, home string) map[string]string {
	if len(scanPaths) == 0 {
		return nil
	}
	psCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(psCtx, "ps", "-axo", "comm=,args=")
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	return tempArtifactRootsFromProcessOutput(string(output), scanPaths, home)
}

func tempArtifactRootsFromProcessOutput(output string, scanPaths []string, home string) map[string]string {
	roots := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		command := filepath.Base(fields[0])
		if len(fields) > 1 {
			command = filepath.Base(fields[1])
		}
		for _, rawPath := range tempArtifactPathPattern.FindAllString(line, -1) {
			if root := tempArtifactRootForPath(rawPath, scanPaths, home); root != "" {
				if _, ok := roots[root]; !ok {
					roots[root] = command
				}
			}
		}
	}
	return roots
}

func tempArtifactRootForPath(path string, scanPaths []string, home string) string {
	path = canonicalTempArtifactPath(path)
	for _, scanPath := range scanPaths {
		root := canonicalTempArtifactPath(expandHome(scanPath, home))
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			continue
		}
		first := strings.Split(rel, string(os.PathSeparator))[0]
		if first == "" || first == "." || first == ".." {
			continue
		}
		return filepath.Join(root, first)
	}
	return ""
}

func canonicalTempArtifactPath(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	if strings.HasPrefix(path, "/tmp/") {
		if resolved, err := filepath.EvalSymlinks("/tmp"); err == nil {
			return filepath.Join(resolved, strings.TrimPrefix(path, "/tmp/"))
		}
	}
	return path
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

type devArtifactGitTracker struct {
	gitPath            string
	repoRootByDir      map[string]string
	trackedFilesByRoot map[string][]string
}

func newDevArtifactGitTracker() *devArtifactGitTracker {
	gitPath, _ := exec.LookPath("git")
	return &devArtifactGitTracker{
		gitPath:            gitPath,
		repoRootByDir:      make(map[string]string),
		trackedFilesByRoot: make(map[string][]string),
	}
}

func devArtifactContainsTrackedFiles(path string) bool {
	return newDevArtifactGitTracker().ContainsTrackedFiles(path)
}

func (t *devArtifactGitTracker) ContainsTrackedFiles(path string) bool {
	if t == nil || t.gitPath == "" {
		return false
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	repoRoot := t.repoRootForPath(absPath)
	if repoRoot == "" {
		return false
	}
	files := t.trackedFiles(repoRoot)
	if len(files) == 0 {
		return false
	}
	rel, err := filepath.Rel(repoRoot, absPath)
	if err != nil {
		return false
	}
	return devArtifactTrackedFilesContain(files, filepath.ToSlash(filepath.Clean(rel)))
}

func (t *devArtifactGitTracker) repoRootForPath(path string) string {
	info, err := os.Stat(path)
	if err == nil && !info.IsDir() {
		path = filepath.Dir(path)
	}

	var visited []string
	dir := path
	for {
		if root, ok := t.repoRootByDir[dir]; ok {
			for _, visitedDir := range visited {
				t.repoRootByDir[visitedDir] = root
			}
			return root
		}
		visited = append(visited, dir)

		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			for _, visitedDir := range visited {
				t.repoRootByDir[visitedDir] = dir
			}
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			for _, visitedDir := range visited {
				t.repoRootByDir[visitedDir] = ""
			}
			return ""
		}
		dir = parent
	}
}

func (t *devArtifactGitTracker) trackedFiles(repoRoot string) []string {
	if files, ok := t.trackedFilesByRoot[repoRoot]; ok {
		return files
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.gitPath, "-C", repoRoot, "ls-files", "-z")
	output, err := cmd.Output()
	if err != nil {
		t.trackedFilesByRoot[repoRoot] = nil
		return nil
	}

	var files []string
	for _, file := range strings.Split(string(output), "\x00") {
		if file != "" {
			files = append(files, file)
		}
	}
	sort.Strings(files)
	t.trackedFilesByRoot[repoRoot] = files
	return files
}

func devArtifactTrackedFilesContain(files []string, rel string) bool {
	if rel == "." {
		return len(files) > 0
	}
	idx := sort.SearchStrings(files, rel)
	if idx < len(files) && files[idx] == rel {
		return true
	}

	prefix := rel + "/"
	idx = sort.Search(len(files), func(i int) bool {
		return files[i] >= prefix
	})
	return idx < len(files) && strings.HasPrefix(files[idx], prefix)
}

var errRecentDevArtifactContent = errors.New("recent dev artifact content")

func devArtifactRecentOutputProtectReason(path string) string {
	return devArtifactRecentOutputProtectReasonContext(context.Background(), path)
}

func devArtifactRecentOutputProtectReasonContext(ctx context.Context, path string) string {
	if !devArtifactHasRecentContent(ctx, path, devArtifactRecentOutputGrace) {
		return ""
	}
	return fmt.Sprintf("artifact directory contains files modified within recent output grace %s", formatDevArtifactAge(devArtifactRecentOutputGrace))
}

func devArtifactHasRecentContent(ctx context.Context, path string, grace time.Duration) bool {
	cutoff := time.Now().Add(-grace)
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.ModTime().After(cutoff) {
			return errRecentDevArtifactContent
		}
		return nil
	})
	return errors.Is(err, errRecentDevArtifactContent)
}

// reportArtifacts reports sizes of all detected dev artifacts without cleaning.
func (p *DevArtifactsPlugin) reportArtifacts(ctx context.Context, daCfg config.DevArtifactsConfig, home string, logger *slog.Logger, budgets ...*devArtifactScanBudget) {
	budget := optionalDevArtifactScanBudget(budgets)
	for _, scanPath := range daCfg.ScanPaths {
		expanded := expandHome(scanPath, home)
		if !pathExistsAndIsDir(expanded) {
			continue
		}

		// Find and report node_modules
		if daCfg.NodeModules {
			p.findArtifactDirs(ctx, expanded, "node_modules", "package.json", func(dir string, size int64) {
				logger.Info("found node_modules", "path", dir, "size_mb", size/(1024*1024))
			}, budget)
		}

		// Find and report .venv
		if daCfg.PythonVenvs {
			p.findArtifactDirs(ctx, expanded, ".venv", "", func(dir string, size int64) {
				logger.Info("found .venv", "path", dir, "size_mb", size/(1024*1024))
			}, budget)
		}

		// Find and report target/
		if daCfg.RustTargets {
			p.findArtifactDirs(ctx, expanded, "target", "Cargo.toml", func(dir string, size int64) {
				logger.Info("found Rust target", "path", dir, "size_mb", size/(1024*1024))
			}, budget)
		}

		// Find and report Zig artifacts
		if daCfg.ZigArtifacts {
			for _, artifactName := range []string{".zig-cache", "zig-out"} {
				p.findArtifactDirs(ctx, expanded, artifactName, "build.zig", func(dir string, size int64) {
					logger.Info("found Zig artifact", "path", dir, "size_mb", size/(1024*1024))
				}, budget)
			}
		}

		// Find and report large local artifacts for manual review.
		if daCfg.LargeLocalArtifacts {
			p.findLargeLocalArtifacts(ctx, expanded, largeLocalArtifactMinBytes(daCfg), daCfg.ProtectPaths, nil, func(target CleanupTarget) {
				logger.Info("found large local artifact", "path", target.Path, "size_mb", target.Bytes/(1024*1024), "type", target.Name)
			}, budget)
		}
	}

	// Report Go build cache
	if daCfg.GoBuildCache {
		goCacheDir := p.getGoCacheDir(ctx)
		if goCacheDir != "" {
			size, _ := getDirSizeContext(ctx, goCacheDir)
			if size > 0 {
				logger.Info("found Go build cache", "path", goCacheDir, "size_mb", size/(1024*1024))
			}
		}
	}

	// Report Haskell caches
	if daCfg.HaskellCache {
		ghcupCache := filepath.Join(home, ".ghcup", "cache")
		cabalStore := filepath.Join(home, ".cabal", "store")
		if size, _ := getDirSizeContext(ctx, ghcupCache); size > 0 {
			logger.Info("found .ghcup/cache", "size_mb", size/(1024*1024))
		}
		if size, _ := getDirSizeContext(ctx, cabalStore); size > 0 {
			logger.Info("found .cabal/store", "size_mb", size/(1024*1024))
		}
	}

	// Report LM Studio models
	if daCfg.LMStudioModels {
		lmStudioDir := filepath.Join(home, ".lmstudio", "models")
		if size, _ := getDirSizeContext(ctx, lmStudioDir); size > 0 {
			logger.Info("found .lmstudio/models", "size_mb", size/(1024*1024))
		}
	}
}

// cleanNodeModules removes stale node_modules directories.
// A node_modules is considered stale if the sibling package.json hasn't been
// modified within the maxAge threshold.
func (p *DevArtifactsPlugin) cleanNodeModules(ctx context.Context, scanPath string, maxAge time.Duration, protectPaths []string, tracker *devArtifactGitTracker, logger *slog.Logger, budgets ...*devArtifactScanBudget) int64 {
	var totalFreed int64
	budget := optionalDevArtifactScanBudget(budgets)

	p.findArtifactDirs(ctx, scanPath, "node_modules", "package.json", func(dir string, size int64) {
		if p.isProtected(dir, protectPaths) {
			return
		}
		if tracker.ContainsTrackedFiles(dir) {
			logger.Debug("preserving node_modules containing tracked files", "path", dir)
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
	}, budget)

	if totalFreed > 0 {
		logger.Info("cleaned stale node_modules", "freed_mb", totalFreed/(1024*1024))
	}

	return totalFreed
}

// cleanPythonVenvs removes stale Python virtual environments.
// A .venv is stale if sibling pyproject.toml/setup.py/requirements.txt hasn't
// been modified within the maxAge threshold.
func (p *DevArtifactsPlugin) cleanPythonVenvs(ctx context.Context, scanPath string, maxAge time.Duration, protectPaths []string, tracker *devArtifactGitTracker, logger *slog.Logger, budgets ...*devArtifactScanBudget) int64 {
	var totalFreed int64
	pythonMarkers := []string{"pyproject.toml", "setup.py", "requirements.txt"}
	budget := optionalDevArtifactScanBudget(budgets)

	p.findArtifactDirs(ctx, scanPath, ".venv", "", func(dir string, size int64) {
		if p.isProtected(dir, protectPaths) {
			return
		}
		if tracker.ContainsTrackedFiles(dir) {
			logger.Debug("preserving .venv containing tracked files", "path", dir)
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
	}, budget)

	if totalFreed > 0 {
		logger.Info("cleaned stale Python venvs", "freed_mb", totalFreed/(1024*1024))
	}

	return totalFreed
}

// cleanRustTargets removes stale Rust target/ directories.
// A target/ is stale if sibling Cargo.toml hasn't been modified within maxAge.
func (p *DevArtifactsPlugin) cleanRustTargets(ctx context.Context, scanPath string, maxAge time.Duration, protectPaths []string, tracker *devArtifactGitTracker, logger *slog.Logger, budgets ...*devArtifactScanBudget) int64 {
	var totalFreed int64
	budget := optionalDevArtifactScanBudget(budgets)

	p.findArtifactDirs(ctx, scanPath, "target", "Cargo.toml", func(dir string, size int64) {
		if p.isProtected(dir, protectPaths) {
			return
		}
		if tracker.ContainsTrackedFiles(dir) {
			logger.Debug("preserving Rust target containing tracked files", "path", dir)
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
	}, budget)

	if totalFreed > 0 {
		logger.Info("cleaned stale Rust targets", "freed_mb", totalFreed/(1024*1024))
	}

	return totalFreed
}

// cleanZigArtifacts removes stale Zig .zig-cache and zig-out directories.
// A Zig artifact is stale if sibling build.zig hasn't been modified within maxAge.
func (p *DevArtifactsPlugin) cleanZigArtifacts(ctx context.Context, scanPath string, maxAge time.Duration, protectPaths []string, tracker *devArtifactGitTracker, logger *slog.Logger, budgets ...*devArtifactScanBudget) int64 {
	var totalFreed int64
	budget := optionalDevArtifactScanBudget(budgets)

	for _, artifactName := range []string{".zig-cache", "zig-out"} {
		p.findArtifactDirs(ctx, scanPath, artifactName, "build.zig", func(dir string, size int64) {
			if p.isProtected(dir, protectPaths) {
				return
			}
			if tracker.ContainsTrackedFiles(dir) {
				logger.Debug("preserving Zig artifact containing tracked files", "path", dir)
				return
			}
			if reason := devArtifactRecentOutputProtectReasonContext(ctx, dir); reason != "" {
				logger.Debug("preserving recent Zig artifact", "path", dir, "reason", reason)
				return
			}

			buildZig := filepath.Join(filepath.Dir(dir), "build.zig")
			if maxAge > 0 && !p.isFileStale(buildZig, maxAge) {
				return
			}

			logger.Debug("removing stale Zig artifact", "path", dir, "size_mb", size/(1024*1024))
			if err := os.RemoveAll(dir); err != nil {
				logger.Debug("failed to remove Zig artifact", "path", dir, "error", err)
				return
			}
			totalFreed += size
		}, budget)
	}

	if totalFreed > 0 {
		logger.Info("cleaned stale Zig artifacts", "freed_mb", totalFreed/(1024*1024))
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
func (p *DevArtifactsPlugin) findArtifactDirs(ctx context.Context, scanPath string, targetName string, markerFile string, callback func(dir string, size int64), budgets ...*devArtifactScanBudget) {
	budget := optionalDevArtifactScanBudget(budgets)
	scanDepth := strings.Count(scanPath, string(os.PathSeparator))

	filepath.Walk(scanPath, func(path string, info os.FileInfo, err error) error {
		if err := budget.checkPath(ctx, path); err != nil {
			return err
		}
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

		size, err := getDirSizeContext(ctx, path)
		if err != nil {
			budget.markContextError(ctx, path)
			return err
		}
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
