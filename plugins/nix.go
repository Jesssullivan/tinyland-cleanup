package plugins

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
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

const nixDefaultCommandTimeout = 20 * time.Minute

// NixPlugin handles Nix garbage collection operations.
type NixPlugin struct {
	freeDiskSpace func(string) (uint64, error)
	// Test seams for the stale GC root reaper; empty or nil uses the real
	// /nix/var/nix/gcroots/auto, /nix/store and wall clock.
	autoGCRootsDir string
	storeDir       string
	now            func() time.Time
	// Hook-pin and live-tree seams for tests; nil uses nix-store and /proc.
	hookValid         func(ctx context.Context, storePath string) bool
	hookAddRoot       func(ctx context.Context, link, storePath string) error
	hookSessionStarts func(comms []string) ([]time.Time, error)
	liveCwds          func() ([]string, error)
	// activeWork replaces process inspection in tests; nil uses ps.
	activeWork func(ctx context.Context) ([]string, nixProcessInspection, error)
	// memState is the in-memory record of the last pass and the current run
	// of deferrals, read when nix.gc_state_file is empty.
	memState nixGCState
	// lastGCAttempt mirrors memState.LastGCAttempt for older tests.
	lastGCAttempt time.Time
}

type nixGeneration struct {
	Number    int
	CreatedAt time.Time
	Current   bool
	Scope     string
	Profile   string
}

type nixGCRoot struct {
	Root           string
	StorePath      string
	StorePathCount int
	Class          string
	Active         bool
}

// NewNixPlugin creates a new Nix cleanup plugin.
func NewNixPlugin() *NixPlugin {
	return &NixPlugin{freeDiskSpace: getFreeDiskSpace}
}

// Name returns the plugin identifier.
func (p *NixPlugin) Name() string {
	return "nix"
}

// Description returns the plugin description.
func (p *NixPlugin) Description() string {
	return "Runs Nix garbage collection with generation and daemon-contention safeguards"
}

// SupportedPlatforms returns supported platforms (all).
func (p *NixPlugin) SupportedPlatforms() []string {
	return nil // All platforms (Nix can be installed anywhere)
}

// Enabled checks if Nix cleanup is enabled.
func (p *NixPlugin) Enabled(cfg *config.Config) bool {
	return cfg.Enable.NixGC
}

// PlanCleanup returns a non-mutating Nix cleanup preflight plan.
func (p *NixPlugin) PlanCleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupPlan {
	plan := CleanupPlan{
		Plugin:   p.Name(),
		Level:    level.String(),
		Summary:  "Nix cleanup preflight plan",
		WouldRun: true,
		Steps:    nixPlanSteps(level, cfg.Nix),
		Metadata: map[string]string{
			"cleanup_level":                                          level.String(),
			"min_user_generations":                                   strconv.Itoa(cfg.Nix.MinUserGenerations),
			"min_home_manager_generations":                           strconv.Itoa(nixMinHomeManagerGenerations(cfg.Nix)),
			"min_system_generations":                                 strconv.Itoa(cfg.Nix.MinSystemGenerations),
			"max_user_generations":                                   strconv.Itoa(cfg.Nix.MaxUserGenerations),
			"max_home_manager_generations":                           strconv.Itoa(cfg.Nix.MaxHomeManagerGenerations),
			"delete_generations_older_than":                          cfg.Nix.DeleteGenerationsOlderThan,
			"critical_delete_generations_older_than":                 cfg.Nix.CriticalDeleteGenerationsOlderThan,
			"delete_home_manager_generations":                        strconv.FormatBool(cfg.Nix.DeleteHomeManagerGenerations),
			"home_manager_delete_generations_older_than":             cfg.Nix.HomeManagerDeleteGenerationsOlderThan,
			"home_manager_critical_delete_generations_older_than":    cfg.Nix.HomeManagerCriticalDeleteGenerationsOlderThan,
			"allow_store_optimize":                                   strconv.FormatBool(cfg.Nix.AllowStoreOptimize),
			"skip_when_daemon_busy":                                  strconv.FormatBool(cfg.Nix.SkipWhenDaemonBusy),
			"daemon_busy_backoff":                                    cfg.Nix.DaemonBusyBackoff,
			"max_gc_duration":                                        cfg.Nix.MaxGCDuration,
			"max_freed_per_pass":                                     cfg.Nix.MaxFreedPerPass,
			"host_measure_path":                                      nixHostMeasurePath(cfg.Nix),
			"root_attribution_limit":                                 strconv.Itoa(nixRootAttributionLimit(cfg.Nix)),
			"generation_policy_delete_older_than_level":              nixGenerationPolicyAge(level, cfg.Nix),
			"home_manager_generation_policy_delete_older_than_level": nixHomeManagerGenerationPolicyAge(level, cfg.Nix),
		},
	}

	if !p.isNixAvailable() {
		plan.WouldRun = false
		plan.SkipReason = "nix_collect_garbage_not_available"
		plan.Summary = "Nix garbage collection is not available"
		return plan
	}

	inspect := p.activeWork
	if inspect == nil {
		inspect = p.activeNixProcesses
	}
	gcState := loadNixGCState(nixGCStatePath(cfg.Nix))
	if nixGCStatePath(cfg.Nix) == "" {
		gcState = p.memState
	}
	plan.Metadata["deferred_passes"] = strconv.Itoa(gcState.DeferredPasses)
	if !gcState.FirstDeferredAt.IsZero() {
		plan.Metadata["deferred_since"] = gcState.FirstDeferredAt.UTC().Format(time.RFC3339)
	}
	plan.Metadata["max_busy_deferrals"] = strconv.Itoa(cfg.Nix.MaxBusyDeferrals)
	plan.Metadata["max_busy_deferral_duration"] = cfg.Nix.MaxBusyDeferralDuration
	busy, inspection, err := inspect(ctx)
	if inspection.MalformedRows > 0 {
		plan.Metadata["process_inspection_malformed_rows"] = strconv.Itoa(inspection.MalformedRows)
		plan.Warnings = append(plan.Warnings, inspection.warning())
	}
	if err != nil {
		if cfg.Nix.SkipWhenDaemonBusy {
			nixDeferPlan(&plan,
				"nix_process_inspection_failed",
				"Nix cleanup is deferred because active process inspection failed",
				cfg.Nix,
				[]CleanupTarget{nixDeferralTarget("nix_process_inspection", "active Nix process inspection", "protect_process_inspection", false, fmt.Sprintf("could not inspect active Nix processes: %v", err), cfg.Nix.DaemonBusyBackoff)},
				fmt.Sprintf("could not inspect active Nix processes: %v", err),
			)
			return plan
		}
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("could not inspect active Nix processes: %v", err))
	} else if len(busy) > 0 {
		plan.Metadata["active_nix_processes"] = strings.Join(busy, ", ")
		if released, bound := nixBusyDeferralBound(cfg.Nix, gcState, p.clock()); released && cfg.Nix.SkipWhenDaemonBusy {
			plan.Metadata["busy_deferral_released_by"] = bound
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("active Nix work (%s) has deferred %d due passes; %s releases the next pass, which runs behind the hook-path pins and the serialization lock", strings.Join(busy, ", "), gcState.DeferredPasses, bound))
		} else if cfg.Nix.SkipWhenDaemonBusy {
			nixDeferPlan(&plan,
				"nix_daemon_busy",
				"Nix cleanup is deferred because active Nix work was detected",
				cfg.Nix,
				nixActiveWorkTargets(busy, cfg.Nix.DaemonBusyBackoff),
			)
			return plan
		}
	}

	staleTargets, staleMetadata, staleWarnings := p.planStaleGCRoots(cfg)
	plan.Targets = append(plan.Targets, staleTargets...)
	for key, value := range staleMetadata {
		plan.Metadata[key] = value
	}
	plan.Warnings = append(plan.Warnings, staleWarnings...)

	if output, err := p.collectGarbageDryRun(ctx, cfg.Nix); err != nil {
		if reason, ok := nixContentionReason(output); ok {
			plan.Metadata["nix_contention"] = reason
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("nix-collect-garbage dry-run reported store contention: %s", reason))
			if cfg.Nix.SkipWhenDaemonBusy {
				nixDeferPlan(&plan,
					"nix_daemon_contention",
					"Nix cleanup is deferred because dry-run reported store contention",
					cfg.Nix,
					[]CleanupTarget{nixDeferralTarget("nix_store_contention", reason, "protect_store_contention", true, "Nix store lock or SQLite contention was reported by dry-run GC", cfg.Nix.DaemonBusyBackoff)},
				)
				return plan
			}
		}
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("nix-collect-garbage dry-run failed: %v", err))
	} else {
		estimated := p.parseDryRunFreedSpace(output)
		paths := p.parseDryRunStorePaths(output)
		plan.EstimatedBytesFreed = estimated
		plan.Metadata["dry_run_store_paths"] = strconv.Itoa(paths)
		if estimated == 0 {
			plan.Warnings = append(plan.Warnings, "Nix dry-run reported no reclaimable store space; live roots may be retaining the store")
			targets, metadata, warnings := p.planGCRootAttribution(ctx, cfg.Nix)
			plan.Targets = append(plan.Targets, targets...)
			for key, value := range metadata {
				plan.Metadata[key] = value
			}
			plan.Warnings = append(plan.Warnings, warnings...)
		}
	}

	targets, warnings := p.planGenerationTargets(ctx, level, cfg.Nix, logger)
	plan.Targets = append(plan.Targets, targets...)
	plan.Warnings = append(plan.Warnings, warnings...)
	plan.Metadata["generation_targets"] = strconv.Itoa(len(targets))

	if level == LevelCritical && !cfg.Nix.AllowStoreOptimize {
		plan.Warnings = append(plan.Warnings, "critical Nix store optimization is disabled by allow_store_optimize=false")
	}

	return plan
}

func nixDeferPlan(plan *CleanupPlan, skipReason string, summary string, cfg config.NixConfig, targets []CleanupTarget, warnings ...string) {
	if plan.Metadata == nil {
		plan.Metadata = map[string]string{}
	}
	plan.WouldRun = false
	plan.SkipReason = skipReason
	plan.Summary = summary
	plan.Targets = append(plan.Targets, targets...)
	plan.Metadata["deferral_reason"] = skipReason
	plan.Metadata["retry_after"] = cfg.DaemonBusyBackoff
	plan.Metadata["target_count"] = strconv.Itoa(len(plan.Targets))
	plan.Warnings = append(plan.Warnings, warnings...)
	if cfg.DaemonBusyBackoff != "" {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("retry Nix cleanup after %s once active work is idle and process inspection is available", cfg.DaemonBusyBackoff))
	}
}

// Cleanup runs every poll (the plugin is level-independent). It rewrites the
// stale GC root candidate list and refreshes the hook-path pins, then
// collects when nixGCDue says so: under pressure or on the periodic interval,
// never within min_gc_interval of the last pass (R-C796), and only once hook
// paths are pinned (H1).
func (p *NixPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{
		Plugin: p.Name(),
		Level:  level,
	}

	// Rewritten every poll while reaping is on, empty when nothing is stale,
	// so the root job never replays an old list (review H2).
	p.publishStaleGCRootCandidates(cfg, logger)

	// Pins are refreshed every poll, not only when a GC is due: two
	// hook-changing switches inside one GC gap would otherwise replace the
	// middle hook path before any refresh saw it, and a session started
	// under it would lose it to the next GC (re-review N1). A poll observes
	// every path that stayed referenced for at least one poll interval.
	pinErr := p.refreshHookPins(ctx, cfg.Nix, logger)
	if pinErr != nil {
		logger.Warn("hook-path pins could not be established", "error", pinErr)
	}

	statePath := nixGCStatePath(cfg.Nix)
	st := loadNixGCState(statePath)
	if statePath == "" {
		st = p.memState
	}
	if p.lastGCAttempt.After(st.LastGCAttempt) {
		st.LastGCAttempt = p.lastGCAttempt
	}
	saveState := func() {
		p.memState = st
		if err := saveNixGCState(statePath, st); err != nil {
			logger.Warn("could not record the Nix GC state", "path", statePath, "error", err)
		}
	}
	gcLevel, why := nixGCDue(level, cfg.Nix, st, p.clock())
	if gcLevel == LevelNone {
		logger.Debug("Nix GC not due", "reason", why, "last_gc_attempt", st.LastGCAttempt)
		return result
	}
	if pinErr != nil && cfg.Nix.HookPinRequired {
		logger.Warn("skipping Nix GC: hook-path pins could not be established", "error", pinErr)
		return result
	}
	// R-C815: a switch, a delivery or another collector in progress defers
	// the pass; it is retried on a later poll and not counted as an attempt.
	if marker, deferred := nixGCDeferredBy(cfg.Nix); deferred {
		logger.Info("deferring Nix GC: a serialized operation is in progress", "marker", marker)
		return result
	}
	release, lockErr := nixAcquireGCLock(cfg.Nix)
	if errors.Is(lockErr, errNixGCLockHeld) {
		logger.Info("deferring Nix GC: another collector holds the serialization lock", "lock", nixGCLockPath(cfg.Nix))
		return result
	}
	if lockErr != nil {
		logger.Warn("skipping Nix GC: the serialization lock is unusable", "error", lockErr)
		return result
	}
	defer release()
	if cfg.Nix.SkipWhenDaemonBusy && p.isNixAvailable() {
		if proceed := p.busyGate(ctx, cfg.Nix, &st, logger); !proceed {
			saveState()
			return result
		}
	}
	gcResult, attempted := p.runGC(ctx, gcLevel, cfg, logger)
	if attempted {
		st.recordAttempt(p.clock())
		p.lastGCAttempt = st.LastGCAttempt
		saveState()
	}
	gcResult.Level = level
	return gcResult
}

// busyGate defers a due pass while active Nix work is detected, but never
// past nixBusyDeferralBound: no single process can hold the store's GC off
// indefinitely. A pass the bound releases still runs behind the hook-path
// pins and the serialization lock. A failed process inspection always
// defers. It returns whether the pass may run, updating st's deferral run.
func (p *NixPlugin) busyGate(ctx context.Context, cfg config.NixConfig, st *nixGCState, logger *slog.Logger) bool {
	inspect := p.activeWork
	if inspect == nil {
		inspect = p.activeNixProcesses
	}
	now := p.clock()
	busy, inspection, err := inspect(ctx)
	if inspection.MalformedRows > 0 {
		logger.Warn("ignored malformed process rows during Nix process inspection",
			"count", inspection.MalformedRows,
			"samples", strings.Join(inspection.MalformedSamples, " | "))
	}
	if err != nil {
		st.recordDeferral(now, "process_inspection_failed")
		logger.Warn("could not inspect active Nix processes", "error", err,
			"deferred_passes", st.DeferredPasses, "deferred_since", st.FirstDeferredAt)
		return false
	}
	if len(busy) == 0 {
		return true
	}
	if released, bound := nixBusyDeferralBound(cfg, *st, now); released {
		logger.Error("running Nix GC despite active Nix work: the deferral bound is reached",
			"bound", bound,
			"deferred_passes", st.DeferredPasses,
			"deferred_since", st.FirstDeferredAt,
			"processes", strings.Join(busy, ", "))
		return true
	}
	st.recordDeferral(now, "active_nix_work: "+strings.Join(busy, ", "))
	logger.Warn("skipping Nix cleanup because active Nix work was detected",
		"processes", strings.Join(busy, ", "),
		"deferred_passes", st.DeferredPasses,
		"deferred_since", st.FirstDeferredAt,
		"backoff", cfg.DaemonBusyBackoff)
	return false
}

// runGC is one Nix GC pass at level. attempted is true once the dry-run
// preflight ran, which is what min_gc_interval spaces out.
func (p *NixPlugin) runGC(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) (CleanupResult, bool) {
	result := CleanupResult{
		Plugin: p.Name(),
		Level:  level,
	}

	if !p.isNixAvailable() {
		logger.Debug("nix not available, skipping")
		return result, false
	}

	// Active-work deferral is decided by busyGate in Cleanup, before this.

	generationsDeleted := 0
	if level >= LevelModerate {
		generationResult := p.deleteUserGenerationsByPolicy(ctx, level, cfg.Nix, logger)
		result.ItemsCleaned += generationResult.ItemsCleaned
		generationsDeleted = generationResult.ItemsCleaned
		if generationResult.Error != nil {
			result.Error = generationResult.Error
			return result, false
		}

		homeManagerGenerationResult := p.deleteHomeManagerGenerationsByPolicy(ctx, level, cfg.Nix, logger)
		result.ItemsCleaned += homeManagerGenerationResult.ItemsCleaned
		generationsDeleted += homeManagerGenerationResult.ItemsCleaned
		if homeManagerGenerationResult.Error != nil {
			result.Error = homeManagerGenerationResult.Error
			return result, false
		}
	}

	dryRunOutput, dryRunErr := p.collectGarbageDryRun(ctx, cfg.Nix)
	if dryRunErr != nil {
		if reason, ok := nixContentionReason(dryRunOutput); ok && cfg.Nix.SkipWhenDaemonBusy {
			logger.Warn("skipping Nix garbage collection because dry-run preflight reported store contention", "reason", reason)
			return result, true
		}
		logger.Warn("skipping Nix garbage collection because dry-run preflight failed",
			"error", dryRunErr,
			"output", strings.TrimSpace(dryRunOutput))
		result.Error = fmt.Errorf("nix garbage collection preflight failed: %w", dryRunErr)
		return result, true
	} else if p.nixDryRunHasNoGCWork(dryRunOutput) && generationsDeleted == 0 && !(level == LevelCritical && cfg.Nix.AllowStoreOptimize) {
		logger.Info("skipping Nix garbage collection because dry-run reported no reclaimable store paths")
		return result, true
	}

	// R-C815: a switch or delivery that started during the preflight still
	// defers the collection itself.
	if marker, deferred := nixGCDeferredBy(cfg.Nix); deferred {
		logger.Info("deferring Nix garbage collection: a serialized operation started", "marker", marker)
		return result, true
	}

	switch level {
	case LevelWarning, LevelModerate, LevelAggressive:
		gcResult := p.collectGarbage(ctx, level, nil, cfg.Nix, logger)
		result.BytesFreed += gcResult.BytesFreed
		result.CommandBytesFreed += gcResult.CommandBytesFreed
		result.HostBytesFreed += gcResult.HostBytesFreed
		result.ItemsCleaned += gcResult.ItemsCleaned
		result.Error = gcResult.Error
	case LevelCritical:
		gcResult := p.collectGarbageCritical(ctx, cfg.Nix, logger)
		result.BytesFreed += gcResult.BytesFreed
		result.CommandBytesFreed += gcResult.CommandBytesFreed
		result.HostBytesFreed += gcResult.HostBytesFreed
		result.ItemsCleaned += gcResult.ItemsCleaned
		result.Error = gcResult.Error
	}

	return result, true
}

func (p *NixPlugin) isNixAvailable() bool {
	_, err := exec.LookPath("nix-collect-garbage")
	return err == nil
}

func (p *NixPlugin) collectGarbageDryRun(ctx context.Context, cfg config.NixConfig) (string, error) {
	timeout := nixCommandTimeout(cfg)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := commandContext(ctx, "nix-collect-garbage", "--dry-run")
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func (p *NixPlugin) printGCRoots(ctx context.Context, cfg config.NixConfig) (string, error) {
	if _, err := exec.LookPath("nix-store"); err != nil {
		return "", err
	}

	timeout := nixCommandTimeout(cfg)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := commandContext(ctx, "nix-store", "--gc", "--print-roots")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("nix-store --gc --print-roots failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func (p *NixPlugin) planGCRootAttribution(ctx context.Context, cfg config.NixConfig) ([]CleanupTarget, map[string]string, []string) {
	limit := nixRootAttributionLimit(cfg)
	if limit == 0 {
		return nil, map[string]string{"gc_root_attribution": "disabled"}, nil
	}

	metadata := map[string]string{
		"gc_root_attribution_limit": strconv.Itoa(limit),
	}
	output, err := p.printGCRoots(ctx, cfg)
	if err != nil {
		return nil, metadata, []string{fmt.Sprintf("could not inspect Nix GC roots: %v", err)}
	}

	roots := parseNixGCRoots(output)
	uniqueRoots := nixUniqueGCRoots(roots)
	metadata["gc_root_attribution_roots"] = strconv.Itoa(len(roots))
	metadata["gc_root_attribution_classes"] = nixGCRootClassSummary(roots)
	metadata["gc_root_attribution_unique_roots"] = strconv.Itoa(len(uniqueRoots))
	metadata["gc_root_attribution_unique_classes"] = nixGCRootClassSummary(uniqueRoots)

	targets := nixGCRootTargets(roots, limit)
	if len(targets) < len(uniqueRoots) {
		metadata["gc_root_attribution_truncated"] = "true"
		return targets, metadata, []string{fmt.Sprintf("Nix GC root attribution truncated to %d of %d visible roots (%d store-path entries)", len(targets), len(uniqueRoots), len(roots))}
	}

	return targets, metadata, nil
}

func (p *NixPlugin) collectGarbage(ctx context.Context, level CleanupLevel, args []string, cfg config.NixConfig, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: level}

	// Re-review N3: bound every pass. The first pass after a switch can
	// otherwise delete a whole host's backlog at once inside nix-daemon,
	// outside this unit's Nice and idle IO class; the next pass continues
	// after min_gc_interval.
	args = append(append([]string{}, args...), nixMaxFreedArgs(cfg, logger)...)
	logger.Debug("running nix-collect-garbage", "args", strings.Join(args, " "))
	measurePath := nixHostMeasurePath(cfg)
	before, beforeOK := p.measureFreeDiskSpace(measurePath, logger)

	ctx, cancel := context.WithTimeout(ctx, nixCommandTimeout(cfg))
	defer cancel()

	cmd := reclaimCommandContext(ctx, "nix-collect-garbage", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if reason, ok := nixContentionReason(string(output)); ok && cfg.SkipWhenDaemonBusy {
			logger.Warn("skipping Nix garbage collection because store contention was reported", "reason", reason)
			return result
		}
		// com.apple.macl / App Management TCC semantics are Darwin-only; a chmod
		// EPERM on other platforms is a real failure that must stay an error.
		if storePath, appPath, ok := nixGCWedgedAppBundle(string(output)); ok && goos() == "darwin" {
			result.CommandBytesFreed = p.parseFreedSpace(string(output))
			result.BytesFreed = result.CommandBytesFreed
			result.ItemsCleaned = p.parseDeletedPaths(string(output))
			if beforeOK {
				result.HostBytesFreed = p.measuredHostDelta(before, measurePath, logger)
			}
			logger.Warn("Nix GC wedged by TCC-protected app bundle (com.apple.macl); GC stays blocked until the store path is removed from a TCC-privileged terminal",
				"store_path", storePath,
				"app_path", appPath,
				"remediation", "sudo rm -rf "+storePath)
			return result
		}
		result.Error = err
		return result
	}

	result.CommandBytesFreed = p.parseFreedSpace(string(output))
	result.BytesFreed = result.CommandBytesFreed
	result.ItemsCleaned = p.parseDeletedPaths(string(output))
	if beforeOK {
		result.HostBytesFreed = p.measuredHostDelta(before, measurePath, logger)
	}

	return result
}

func nixHostMeasurePath(cfg config.NixConfig) string {
	path := strings.TrimSpace(cfg.HostMeasurePath)
	if path == "" {
		path = "/nix/store"
	}
	home, _ := os.UserHomeDir()
	path = filepath.Clean(expandHome(path, home))
	if pathExists(path) {
		return path
	}
	if pathExists("/nix") {
		return "/nix"
	}
	if home != "" && pathExists(home) {
		return home
	}
	return "."
}

func (p *NixPlugin) measureFreeDiskSpace(path string, logger *slog.Logger) (int64, bool) {
	freeDiskSpace := p.freeDiskSpace
	if freeDiskSpace == nil {
		freeDiskSpace = getFreeDiskSpace
	}
	free, err := freeDiskSpace(path)
	if err != nil {
		logger.Debug("Nix host free-space measurement failed", "path", path, "error", err)
		return 0, false
	}
	return int64FromUint64(free), true
}

func (p *NixPlugin) measuredHostDelta(before int64, path string, logger *slog.Logger) int64 {
	after, ok := p.measureFreeDiskSpace(path, logger)
	if !ok || after <= before {
		return 0
	}
	return after - before
}

func (p *NixPlugin) collectGarbageCritical(ctx context.Context, cfg config.NixConfig, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: LevelCritical}

	logger.Warn("CRITICAL: running Nix garbage collection")
	gcResult := p.collectGarbage(ctx, LevelCritical, nil, cfg, logger)
	result.BytesFreed = gcResult.BytesFreed
	result.CommandBytesFreed = gcResult.CommandBytesFreed
	result.HostBytesFreed = gcResult.HostBytesFreed
	result.ItemsCleaned = gcResult.ItemsCleaned
	if gcResult.Error != nil {
		result.Error = gcResult.Error
		return result
	}

	if !cfg.AllowStoreOptimize {
		logger.Warn("skipping nix-store --optimize because allow_store_optimize=false")
		return result
	}

	logger.Warn("CRITICAL: running nix-store --optimize")
	measurePath := nixHostMeasurePath(cfg)
	before, beforeOK := p.measureFreeDiskSpace(measurePath, logger)
	optimizeCtx, cancel := context.WithTimeout(ctx, nixCommandTimeout(cfg))
	defer cancel()

	cmd := reclaimCommandContext(optimizeCtx, "nix-store", "--optimize")
	output, err := cmd.CombinedOutput()
	if err != nil {
		logger.Error("nix-store --optimize failed", "error", err, "output", string(output))
		return result
	}

	optimizedBytes := p.parseOptimizedSpace(string(output))
	result.BytesFreed += optimizedBytes
	result.CommandBytesFreed += optimizedBytes
	if beforeOK {
		result.HostBytesFreed += p.measuredHostDelta(before, measurePath, logger)
	}

	return result
}

func (p *NixPlugin) deleteUserGenerationsByPolicy(ctx context.Context, level CleanupLevel, cfg config.NixConfig, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: level}

	if _, err := exec.LookPath("nix-env"); err != nil {
		logger.Debug("nix-env not available, skipping generation policy deletion")
		return result
	}

	generations, err := p.listGenerations(ctx, "user", "", cfg)
	if err != nil {
		logger.Warn("could not list user Nix profile generations", "error", err)
		return result
	}

	olderThan := parseNixPolicyDuration(nixGenerationPolicyAge(level, cfg), 0)
	if olderThan <= 0 && cfg.MaxUserGenerations <= 0 {
		return result
	}

	targets := nixGenerationTargetsWithMax(generations, time.Now(), cfg.MinUserGenerations, olderThan, cfg.MaxUserGenerations)
	var generationNumbers []string
	for _, target := range targets {
		if target.Action == "delete_generation" {
			generationNumbers = append(generationNumbers, target.Version)
		}
	}
	if len(generationNumbers) == 0 {
		return result
	}

	args := append([]string{"--delete-generations"}, generationNumbers...)
	deleteCtx, cancel := context.WithTimeout(ctx, nixCommandTimeout(cfg))
	defer cancel()

	cmd := reclaimCommandContext(deleteCtx, "nix-env", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if reason, ok := nixContentionReason(string(output)); ok && cfg.SkipWhenDaemonBusy {
			logger.Warn("skipping Nix generation deletion because store contention was reported", "reason", reason)
			return result
		}
		result.Error = fmt.Errorf("nix-env %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
		return result
	}

	result.ItemsCleaned = len(generationNumbers)
	logger.Info("deleted old user Nix profile generations", "generations", strings.Join(generationNumbers, ", "))
	return result
}

func (p *NixPlugin) deleteHomeManagerGenerationsByPolicy(ctx context.Context, level CleanupLevel, cfg config.NixConfig, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: level}

	if !cfg.DeleteHomeManagerGenerations {
		return result
	}

	if _, err := exec.LookPath("home-manager"); err != nil {
		logger.Debug("home-manager not available, skipping Home Manager generation policy deletion")
		return result
	}

	home, err := os.UserHomeDir()
	if err != nil {
		logger.Warn("could not locate user home for Home Manager generation deletion", "error", err)
		return result
	}

	stateProfilesDir := filepath.Join(home, ".local", "state", "nix", "profiles")
	generations, err := discoverNixProfileLinkGenerations(stateProfilesDir, "home-manager", "home-manager")
	if err != nil {
		logger.Warn("could not inspect Home Manager profile links", "error", err)
		return result
	}
	if len(generations) == 0 {
		return result
	}

	olderThan := parseNixPolicyDuration(nixHomeManagerGenerationPolicyAge(level, cfg), 0)
	if olderThan <= 0 && cfg.MaxHomeManagerGenerations <= 0 {
		return result
	}

	targets := nixGenerationTargetsWithMax(generations, time.Now(), nixMinHomeManagerGenerations(cfg), olderThan, cfg.MaxHomeManagerGenerations)
	var generationNumbers []string
	for _, target := range targets {
		if target.Action == "delete_generation" {
			generationNumbers = append(generationNumbers, target.Version)
		}
	}
	if len(generationNumbers) == 0 {
		return result
	}

	args := append([]string{"remove-generations"}, generationNumbers...)
	deleteCtx, cancel := context.WithTimeout(ctx, nixCommandTimeout(cfg))
	defer cancel()

	cmd := reclaimCommandContext(deleteCtx, "home-manager", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if reason, ok := nixContentionReason(string(output)); ok && cfg.SkipWhenDaemonBusy {
			logger.Warn("skipping Home Manager generation deletion because store contention was reported", "reason", reason)
			return result
		}
		result.Error = fmt.Errorf("home-manager %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
		return result
	}

	result.ItemsCleaned = len(generationNumbers)
	logger.Info("deleted old Home Manager generations", "generations", strings.Join(generationNumbers, ", "))
	return result
}

func (p *NixPlugin) planGenerationTargets(ctx context.Context, level CleanupLevel, cfg config.NixConfig, logger *slog.Logger) ([]CleanupTarget, []string) {
	_ = logger

	olderThan := parseNixPolicyDuration(nixGenerationPolicyAge(level, cfg), 0)
	if olderThan <= 0 && cfg.MaxUserGenerations <= 0 && cfg.MaxHomeManagerGenerations <= 0 {
		return nil, nil
	}

	var targets []CleanupTarget
	var warnings []string
	homeManagerConfig := cfg
	if cfg.DeleteHomeManagerGenerations {
		if _, err := exec.LookPath("home-manager"); err != nil {
			warnings = append(warnings, "home-manager is not available; Home Manager generations will remain review-only")
			homeManagerConfig.DeleteHomeManagerGenerations = false
		}
	}

	_, nixEnvErr := exec.LookPath("nix-env")
	userGenerationsPlanned := false
	if nixEnvErr != nil {
		warnings = append(warnings, "nix-env is not available; falling back to lock-free profile link inspection where possible")
	} else {
		userGenerations, err := p.listGenerations(ctx, "user", "", cfg)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("could not inspect user Nix generations with nix-env: %v", err))
		} else {
			targets = append(targets, nixGenerationTargetsWithMax(userGenerations, time.Now(), cfg.MinUserGenerations, olderThan, cfg.MaxUserGenerations)...)
			userGenerationsPlanned = true
		}
	}

	if home, err := os.UserHomeDir(); err != nil {
		warnings = append(warnings, fmt.Sprintf("could not locate user home for lock-free Nix profile inspection: %v", err))
	} else {
		stateProfilesDir := filepath.Join(home, ".local", "state", "nix", "profiles")
		if !userGenerationsPlanned {
			userLinkGenerations, err := discoverNixProfileLinkGenerations(stateProfilesDir, "profile", "user")
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("could not inspect user Nix profile links: %v", err))
			} else if len(userLinkGenerations) > 0 {
				targets = append(targets, nixGenerationTargetsWithMax(userLinkGenerations, time.Now(), cfg.MinUserGenerations, olderThan, cfg.MaxUserGenerations)...)
				warnings = append(warnings, "using lock-free user Nix profile link inspection after nix-env generation inspection was unavailable")
			}
		}

		homeManagerGenerations, err := discoverNixProfileLinkGenerations(stateProfilesDir, "home-manager", "home-manager")
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("could not inspect Home Manager profile links: %v", err))
		} else if len(homeManagerGenerations) > 0 {
			homeManagerOlderThan := parseNixPolicyDuration(nixHomeManagerGenerationPolicyAge(level, cfg), olderThan)
			homeManagerTargets := nixHomeManagerGenerationTargets(
				nixGenerationTargetsWithMax(homeManagerGenerations, time.Now(), nixMinHomeManagerGenerations(cfg), homeManagerOlderThan, cfg.MaxHomeManagerGenerations),
				homeManagerConfig,
			)
			targets = append(targets, homeManagerTargets...)
		}
	}

	if nixEnvErr == nil {
		systemProfile := "/nix/var/nix/profiles/system"
		systemGenerations, err := p.listGenerations(ctx, "system", systemProfile, cfg)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("could not inspect system Nix generations: %v", err))
		} else {
			systemTargets := nixReviewOnlyGenerationTargets(
				nixGenerationTargets(systemGenerations, time.Now(), cfg.MinSystemGenerations, olderThan),
				"review_privileged_generation",
				"outside retention policy, but system generation deletion requires explicit privileged workflow",
				CleanupTierPrivileged,
			)
			targets = append(targets, systemTargets...)
		}
	}

	return targets, warnings
}

func (p *NixPlugin) listGenerations(ctx context.Context, scope, profile string, cfg config.NixConfig) ([]nixGeneration, error) {
	args := []string{}
	if profile != "" {
		args = append(args, "-p", profile)
	}
	args = append(args, "--list-generations")

	listCtx, cancel := context.WithTimeout(ctx, nixCommandTimeout(cfg))
	defer cancel()

	cmd := reclaimCommandContext(listCtx, "nix-env", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("nix-env %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}

	return parseNixGenerations(string(output), scope, profile)
}

func discoverNixProfileLinkGenerations(profileDir, profileName, scope string) ([]nixGeneration, error) {
	entries, err := os.ReadDir(profileDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	currentLink := filepath.Join(profileDir, profileName)
	currentTarget := ""
	if target, err := os.Readlink(currentLink); err == nil {
		currentTarget = filepath.Base(target)
	}

	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(profileName) + `-(\d+)-link$`)
	var generations []nixGeneration
	for _, entry := range entries {
		matches := pattern.FindStringSubmatch(entry.Name())
		if len(matches) != 2 {
			continue
		}

		number, err := strconv.Atoi(matches[1])
		if err != nil {
			continue
		}

		path := filepath.Join(profileDir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}

		generations = append(generations, nixGeneration{
			Number:    number,
			CreatedAt: info.ModTime(),
			Current:   entry.Name() == currentTarget,
			Scope:     scope,
			Profile:   path,
		})
	}

	sort.Slice(generations, func(i, j int) bool {
		return generations[i].Number < generations[j].Number
	})
	return generations, nil
}

// activeNixProcesses reports host-wide Nix work that should defer a GC pass.
// It fails closed (returns an error) only when the process table as a whole
// cannot be read: ps is missing or fails, or none of its rows parse. A single
// malformed row is counted and reported in the inspection, never fatal.
func (p *NixPlugin) activeNixProcesses(ctx context.Context) ([]string, nixProcessInspection, error) {
	psCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := commandContext(psCtx, "ps", "-axo", "pid=,ppid=,uid=,comm=")
	output, err := cmd.Output()
	if err != nil {
		return nil, nixProcessInspection{}, err
	}
	snapshots, inspection, err := nixProcessSnapshots(psCtx, string(output), nixProcessArgv, nixProcessUID)
	if err != nil {
		return nil, inspection, err
	}
	reasons, err := nixBusyProcessReasons(snapshots)
	if err != nil {
		return nil, inspection, err
	}
	if len(inspection.MalformedCandidates) > 0 {
		reasons = append(reasons, "nix-named process with unparseable ps row")
		sort.Strings(reasons)
	}
	return reasons, inspection, nil
}

func nixReviewOnlyGenerationTargets(targets []CleanupTarget, action string, deleteReason string, tier string) []CleanupTarget {
	for i := range targets {
		if targets[i].Action != "delete_generation" {
			continue
		}
		targets[i].Protected = true
		targets[i].Action = action
		targets[i].Reason = deleteReason
		annotateCleanupTargetPolicy(&targets[i], tier, CleanupReclaimDeferred)
	}
	return targets
}

func nixHomeManagerGenerationTargets(targets []CleanupTarget, cfg config.NixConfig) []CleanupTarget {
	if !cfg.DeleteHomeManagerGenerations {
		return nixReviewOnlyGenerationTargets(
			targets,
			"review_home_manager_generation",
			"outside retention policy, but Home Manager generation deletion requires delete_home_manager_generations=true",
			CleanupTierWarm,
		)
	}

	for i := range targets {
		if targets[i].Action != "delete_generation" {
			continue
		}
		targets[i].Action = "delete_home_manager_generation"
		targets[i].Reason = "outside Home Manager retention policy and eligible for home-manager remove-generations"
		annotateCleanupTargetPolicy(&targets[i], CleanupTierWarm, CleanupReclaimDeferred)
	}
	return targets
}

func nixActiveWorkTargets(reasons []string, backoff string) []CleanupTarget {
	targets := make([]CleanupTarget, 0, len(reasons))
	for _, reason := range reasons {
		targets = append(targets, nixDeferralTarget("nix_active_work", reason, "protect_active_work", true, "active Nix work is using the store", backoff))
	}
	return targets
}

func nixDeferralTarget(targetType string, name string, action string, active bool, reason string, backoff string) CleanupTarget {
	if backoff != "" {
		reason = fmt.Sprintf("%s; retry after %s once idle", reason, backoff)
	}
	target := CleanupTarget{
		Type:      targetType,
		Name:      name,
		Active:    active,
		Protected: true,
		Action:    action,
		Reason:    reason,
	}
	annotateCleanupTargetPolicy(&target, CleanupTierDisruptive, CleanupReclaimNone)
	return target
}

func nixPlanSteps(level CleanupLevel, cfg config.NixConfig) []string {
	steps := []string{
		"Inspect active Nix, Home Manager, and rebuild processes before locking the store",
		"List stale nix-direnv and temporary out-link GC roots in gc_root_candidates_file for the host's root unregister job (out-links untouched) when reap_stale_gc_roots=true",
		"Run nix-collect-garbage --dry-run to estimate reclaimable store paths",
		"List user and system profile generations and apply minimum-retention policy",
		"Preserve current profile generations and visible system generations by default",
	}

	switch level {
	case LevelWarning:
		steps = append(steps, "Run non-destructive preflight only in dry-run output; cleanup mode runs plain Nix GC without generation deletion")
	case LevelModerate, LevelAggressive:
		steps = append(steps,
			fmt.Sprintf("Delete user generations older than %s only when at least %d user generations remain", cfg.DeleteGenerationsOlderThan, cfg.MinUserGenerations),
		)
		if cfg.MaxUserGenerations > 0 {
			steps = append(steps, fmt.Sprintf("Cap user generations at %d while preserving the current generation and minimum rollback count", cfg.MaxUserGenerations))
		}
		if cfg.DeleteHomeManagerGenerations {
			steps = append(steps, fmt.Sprintf("Delete Home Manager generations older than %s only when at least %d Home Manager generations remain", nixHomeManagerGenerationPolicyAge(level, cfg), nixMinHomeManagerGenerations(cfg)))
			if cfg.MaxHomeManagerGenerations > 0 {
				steps = append(steps, fmt.Sprintf("Cap Home Manager generations at %d while preserving the current generation and minimum rollback count", cfg.MaxHomeManagerGenerations))
			}
		} else {
			steps = append(steps, "Report stale or over-cap Home Manager generations as review-only because delete_home_manager_generations=false")
		}
		steps = append(steps, "Run plain Nix GC after selected generation deletion")
	case LevelCritical:
		steps = append(steps,
			fmt.Sprintf("Delete user generations older than %s only when at least %d user generations remain", cfg.CriticalDeleteGenerationsOlderThan, cfg.MinUserGenerations),
		)
		if cfg.MaxUserGenerations > 0 {
			steps = append(steps, fmt.Sprintf("Cap user generations at %d while preserving the current generation and minimum rollback count", cfg.MaxUserGenerations))
		}
		if cfg.DeleteHomeManagerGenerations {
			steps = append(steps, fmt.Sprintf("Delete Home Manager generations older than %s only when at least %d Home Manager generations remain", nixHomeManagerGenerationPolicyAge(level, cfg), nixMinHomeManagerGenerations(cfg)))
			if cfg.MaxHomeManagerGenerations > 0 {
				steps = append(steps, fmt.Sprintf("Cap Home Manager generations at %d while preserving the current generation and minimum rollback count", cfg.MaxHomeManagerGenerations))
			}
		} else {
			steps = append(steps, "Report stale or over-cap Home Manager generations as review-only because delete_home_manager_generations=false")
		}
		steps = append(steps, "Run plain Nix GC after selected generation deletion")
		if cfg.AllowStoreOptimize {
			steps = append(steps, "Run nix-store --optimize because allow_store_optimize=true")
		} else {
			steps = append(steps, "Skip nix-store --optimize because allow_store_optimize=false")
		}
	}

	steps = append(steps, "Measure the Nix store filesystem before and after real GC for plugin-isolated host byte deltas")
	return steps
}

func nixGenerationPolicyAge(level CleanupLevel, cfg config.NixConfig) string {
	switch level {
	case LevelCritical:
		return cfg.CriticalDeleteGenerationsOlderThan
	case LevelModerate, LevelAggressive:
		return cfg.DeleteGenerationsOlderThan
	default:
		return ""
	}
}

func nixHomeManagerGenerationPolicyAge(level CleanupLevel, cfg config.NixConfig) string {
	switch level {
	case LevelCritical:
		if strings.TrimSpace(cfg.HomeManagerCriticalDeleteGenerationsOlderThan) != "" {
			return cfg.HomeManagerCriticalDeleteGenerationsOlderThan
		}
	case LevelModerate, LevelAggressive:
		if strings.TrimSpace(cfg.HomeManagerDeleteGenerationsOlderThan) != "" {
			return cfg.HomeManagerDeleteGenerationsOlderThan
		}
	}
	return nixGenerationPolicyAge(level, cfg)
}

func nixMinHomeManagerGenerations(cfg config.NixConfig) int {
	if cfg.MinHomeManagerGenerations > 0 {
		return cfg.MinHomeManagerGenerations
	}
	return cfg.MinUserGenerations
}

func nixCommandTimeout(cfg config.NixConfig) time.Duration {
	return parseNixPolicyDuration(cfg.MaxGCDuration, nixDefaultCommandTimeout)
}

func nixRootAttributionLimit(cfg config.NixConfig) int {
	if cfg.RootAttributionLimit < 0 {
		return 0
	}
	return cfg.RootAttributionLimit
}

func parseNixPolicyDuration(raw string, fallback time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}

	if duration, err := time.ParseDuration(raw); err == nil {
		return duration
	}

	re := regexp.MustCompile(`^(\d+)\s*([dDwW])$`)
	matches := re.FindStringSubmatch(raw)
	if len(matches) != 3 {
		return fallback
	}

	value, err := strconv.Atoi(matches[1])
	if err != nil {
		return fallback
	}

	switch strings.ToLower(matches[2]) {
	case "d":
		return time.Duration(value) * 24 * time.Hour
	case "w":
		return time.Duration(value) * 7 * 24 * time.Hour
	default:
		return fallback
	}
}

func parseNixGenerations(output, scope, profile string) ([]nixGeneration, error) {
	var generations []nixGeneration
	re := regexp.MustCompile(`^\s*(\d+)\s+(\d{4}-\d{2}-\d{2})\s+(\d{2}:\d{2}:\d{2})(?:\s+\(current\))?`)

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		matches := re.FindStringSubmatch(line)
		if len(matches) < 4 {
			continue
		}

		number, err := strconv.Atoi(matches[1])
		if err != nil {
			continue
		}

		createdAt, err := time.ParseInLocation("2006-01-02 15:04:05", matches[2]+" "+matches[3], time.Local)
		if err != nil {
			continue
		}

		generations = append(generations, nixGeneration{
			Number:    number,
			CreatedAt: createdAt,
			Current:   strings.Contains(line, "(current)"),
			Scope:     scope,
			Profile:   profile,
		})
	}

	return generations, nil
}

func parseNixGCRoots(output string) []nixGCRoot {
	var roots []nixGCRoot
	seen := map[string]bool{}

	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		root := line
		storePath := ""
		if before, after, ok := strings.Cut(line, " -> "); ok {
			root = strings.TrimSpace(before)
			storePath = strings.TrimSpace(after)
		} else {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				root = fields[0]
				storePath = fields[len(fields)-1]
			}
		}
		if root == "" || !looksLikeNixGCRoot(root) {
			continue
		}

		key := root + "\x00" + storePath
		if seen[key] {
			continue
		}
		seen[key] = true

		class, active := classifyNixGCRoot(root)
		roots = append(roots, nixGCRoot{
			Root:           root,
			StorePath:      storePath,
			StorePathCount: 1,
			Class:          class,
			Active:         active,
		})
	}

	sortNixGCRoots(roots)
	return roots
}

func sortNixGCRoots(roots []nixGCRoot) {
	sort.Slice(roots, func(i, j int) bool {
		iRank := nixGCRootClassRank(roots[i].Class)
		jRank := nixGCRootClassRank(roots[j].Class)
		if iRank == jRank {
			if roots[i].Class != roots[j].Class {
				return roots[i].Class < roots[j].Class
			}
			return roots[i].Root < roots[j].Root
		}
		return iRank < jRank
	})
}

func looksLikeNixGCRoot(root string) bool {
	return strings.HasPrefix(root, "/") || strings.HasPrefix(root, "{")
}

func nixGCRootClassRank(class string) int {
	switch class {
	case "process_root", "nix_process_root":
		return 0
	case "lsof_root":
		return 1
	case "temporary_root":
		return 2
	case "direnv_root":
		return 3
	case "workspace_result":
		return 4
	case "nix_cache_root":
		return 5
	case "auto_gcroot", "gcroot":
		return 6
	case "home_manager_gcroot", "profile_root":
		return 7
	default:
		return 8
	}
}

func classifyNixGCRoot(root string) (string, bool) {
	root = strings.TrimSpace(root)
	lower := strings.ToLower(root)

	switch {
	case strings.HasPrefix(root, "/proc/"):
		return "process_root", true
	case strings.HasPrefix(root, "{nix-process:"):
		return "nix_process_root", true
	case strings.HasPrefix(root, "{lsof}"):
		return "lsof_root", true
	case strings.Contains(lower, "/profiles/per-user/") ||
		strings.Contains(lower, "/nix/var/nix/profiles") ||
		strings.Contains(lower, "/.local/state/nix/profiles") ||
		strings.Contains(lower, "/.nix-profile"):
		return "profile_root", false
	case strings.Contains(lower, "/.local/state/home-manager/gcroots/"):
		return "home_manager_gcroot", false
	case strings.Contains(lower, "/.direnv/flake-inputs/") ||
		strings.Contains(lower, "/.direnv/flake-profile-"):
		return "direnv_root", false
	case strings.Contains(lower, "/.cache/nix/"):
		return "nix_cache_root", false
	case strings.Contains(lower, "/gcroots/auto/"):
		return "auto_gcroot", false
	case strings.Contains(lower, "/gcroots/"):
		return "gcroot", false
	case strings.Contains(lower, "/nix-gc-roots/"):
		return "gcroot", false
	case strings.HasSuffix(root, "/result") ||
		strings.Contains(root, "/result-"):
		return "workspace_result", false
	case strings.Contains(lower, "/var/folders/") ||
		strings.Contains(lower, "/tmp/"):
		return "temporary_root", false
	default:
		return "unknown_root", false
	}
}

func nixGCRootTargets(roots []nixGCRoot, limit int) []CleanupTarget {
	roots = nixUniqueGCRoots(roots)
	if limit <= 0 || len(roots) == 0 {
		return nil
	}

	if limit > len(roots) {
		limit = len(roots)
	}

	targets := make([]CleanupTarget, 0, limit)
	for _, root := range roots[:limit] {
		reason := "visible Nix GC root retaining store path"
		version := ""
		if root.StorePath != "" {
			version = filepath.Base(root.StorePath)
			reason = fmt.Sprintf("%s %s", reason, version)
		}
		name := fmt.Sprintf("%s %s", root.Class, filepath.Base(root.Root))
		if root.StorePathCount > 1 {
			name = fmt.Sprintf("%s (%d store paths)", name, root.StorePathCount)
			if version != "" {
				version = fmt.Sprintf("%s (+%d more)", version, root.StorePathCount-1)
				reason = fmt.Sprintf("visible Nix GC root retaining %d store paths; sample %s", root.StorePathCount, filepath.Base(root.StorePath))
			} else {
				reason = fmt.Sprintf("visible Nix GC root retaining %d store paths", root.StorePathCount)
			}
		}
		if root.Active {
			if root.StorePathCount > 1 {
				reason = fmt.Sprintf("active GC root retaining %d store paths; review the owning process before any Nix GC action", root.StorePathCount)
			} else {
				reason = "active process root; review the owning process before any Nix GC action"
			}
		}

		target := CleanupTarget{
			Type:      "nix_gc_root",
			Name:      name,
			Version:   version,
			Path:      root.Root,
			Active:    root.Active,
			Protected: true,
			Action:    nixGCRootReviewAction(root.Class),
			Reason:    reason,
		}
		annotateCleanupTargetPolicy(&target, nixGCRootTier(root.Class), nixGCRootReclaim(root.Class))
		targets = append(targets, target)
	}
	return targets
}

func nixUniqueGCRoots(roots []nixGCRoot) []nixGCRoot {
	if len(roots) == 0 {
		return nil
	}

	grouped := make([]nixGCRoot, 0, len(roots))
	index := map[string]int{}
	for _, root := range roots {
		key := strings.Join([]string{
			root.Root,
			root.Class,
			strconv.FormatBool(root.Active),
		}, "\x00")
		if idx, ok := index[key]; ok {
			grouped[idx].StorePathCount += max(root.StorePathCount, 1)
			if grouped[idx].StorePath == "" && root.StorePath != "" {
				grouped[idx].StorePath = root.StorePath
			}
			continue
		}

		if root.StorePathCount <= 0 {
			root.StorePathCount = 1
		}
		index[key] = len(grouped)
		grouped = append(grouped, root)
	}

	sortNixGCRoots(grouped)
	return grouped
}

func nixGCRootReviewAction(class string) string {
	switch class {
	case "process_root", "nix_process_root":
		return "review_active_gc_root"
	case "lsof_root":
		return "review_lsof_gc_root"
	case "temporary_root":
		return "review_temporary_gc_root"
	case "direnv_root":
		return "review_direnv_gc_root"
	case "workspace_result":
		return "review_workspace_result_root"
	case "nix_cache_root":
		return "review_nix_cache_gc_root"
	case "home_manager_gcroot":
		return "review_home_manager_gc_root"
	default:
		return "review_gc_root"
	}
}

func nixGCRootTier(class string) string {
	switch class {
	case "process_root", "nix_process_root", "lsof_root":
		return CleanupTierDisruptive
	case "direnv_root", "workspace_result", "nix_cache_root", "gcroot", "auto_gcroot", "unknown_root":
		return CleanupTierWarm
	default:
		return CleanupTierSafe
	}
}

func nixGCRootReclaim(class string) string {
	switch class {
	case "temporary_root", "direnv_root", "workspace_result", "nix_cache_root", "gcroot", "auto_gcroot":
		return CleanupReclaimDeferred
	default:
		return CleanupReclaimNone
	}
}

func nixGCRootClassSummary(roots []nixGCRoot) string {
	if len(roots) == 0 {
		return ""
	}

	counts := map[string]int{}
	for _, root := range roots {
		counts[root.Class]++
	}

	classes := make([]string, 0, len(counts))
	for class := range counts {
		classes = append(classes, class)
	}
	sort.Strings(classes)

	parts := make([]string, 0, len(classes))
	for _, class := range classes {
		parts = append(parts, fmt.Sprintf("%s=%d", class, counts[class]))
	}
	return strings.Join(parts, ", ")
}

func nixGenerationTargets(generations []nixGeneration, now time.Time, minKeep int, olderThan time.Duration) []CleanupTarget {
	return nixGenerationTargetsWithMax(generations, now, minKeep, olderThan, 0)
}

func nixGenerationTargetsWithMax(generations []nixGeneration, now time.Time, minKeep int, olderThan time.Duration, maxKeep int) []CleanupTarget {
	if minKeep < 1 {
		minKeep = 1
	}
	if maxKeep > 0 && maxKeep < minKeep {
		maxKeep = minKeep
	}

	sorted := append([]nixGeneration(nil), generations...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].Number > sorted[j].Number
		}
		return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
	})

	protectNewestIncludingCurrent := func(limit int) map[int]bool {
		protected := map[int]bool{}
		protectedCount := 0
		for _, generation := range sorted {
			if generation.Current && !protected[generation.Number] {
				protected[generation.Number] = true
				protectedCount++
			}
		}
		for _, generation := range sorted {
			if protectedCount >= limit {
				break
			}
			if protected[generation.Number] {
				continue
			}
			protected[generation.Number] = true
			protectedCount++
		}
		return protected
	}

	protectedByMinimum := protectNewestIncludingCurrent(minKeep)

	protectedByMaximum := map[int]bool{}
	if maxKeep > 0 {
		protectedByMaximum = protectNewestIncludingCurrent(maxKeep)
	}

	ageEnabled := olderThan > 0
	cutoff := now.Add(-olderThan)
	targets := make([]CleanupTarget, 0, len(sorted))
	for _, generation := range sorted {
		action := "keep_generation"
		reason := "within generation retention policy"
		switch {
		case generation.Current:
			reason = "current profile generation"
		case protectedByMinimum[generation.Number]:
			reason = fmt.Sprintf("within minimum retained %s generations", generation.Scope)
		case maxKeep > 0 && !protectedByMaximum[generation.Number]:
			action = "delete_generation"
			reason = "exceeds configured maximum generation count and outside retained minimum"
		case ageEnabled && generation.CreatedAt.After(cutoff):
			reason = "younger than configured generation age"
		case ageEnabled:
			action = "delete_generation"
			reason = "older than configured generation age and outside retained minimum"
		}
		protected := action != "delete_generation"

		nameScope := generation.Scope
		if nameScope == "" {
			nameScope = "profile"
		}
		target := CleanupTarget{
			Type:      "nix_generation",
			Name:      fmt.Sprintf("%s generation %d", nameScope, generation.Number),
			Version:   strconv.Itoa(generation.Number),
			Path:      generation.Profile,
			Protected: protected,
			Action:    action,
			Reason:    reason,
		}
		reclaim := CleanupReclaimNone
		if action == "delete_generation" {
			reclaim = CleanupReclaimDeferred
		}
		annotateCleanupTargetPolicy(&target, CleanupTierWarm, reclaim)
		targets = append(targets, target)
	}

	sort.Slice(targets, func(i, j int) bool {
		return targets[i].Name < targets[j].Name
	})
	return targets
}

type nixProcessSnapshot struct {
	PID  int
	PPID int
	UID  uint32
	// UIDUnknown is set when neither /proc nor ps yielded a usable UID. Code
	// that branches on UID must treat it as "not root".
	UIDUnknown bool
	Comm       string
	ParentComm string
	Argv       []string
	ArgvErr    error
}

type nixProcessArgvReader func(int) ([]string, error)

// nixProcessUIDReader returns a process's effective UID from an
// authoritative source (/proc/<pid>/status on Linux). An error means the
// caller falls back to the ps column.
type nixProcessUIDReader func(int) (uint32, error)

// nixProcessInspection records rows of the process table that could not be
// parsed, so a caller can report them instead of dropping them silently.
type nixProcessInspection struct {
	MalformedRows    int
	MalformedSamples []string
	// MalformedCandidates are malformed rows whose command still names a Nix
	// process. Without a usable PID they cannot be inspected, so they defer
	// the pass the same way an unreadable argv does.
	MalformedCandidates []string
}

const nixProcessMalformedSampleLimit = 3

func (i *nixProcessInspection) malformed(line string, candidate bool) {
	i.MalformedRows++
	line = strings.TrimSpace(line)
	if len(i.MalformedSamples) < nixProcessMalformedSampleLimit {
		i.MalformedSamples = append(i.MalformedSamples, line)
	}
	if candidate {
		i.MalformedCandidates = append(i.MalformedCandidates, line)
	}
}

func (i nixProcessInspection) warning() string {
	return fmt.Sprintf("ignored %d malformed process row(s) during Nix process inspection: %s",
		i.MalformedRows, strings.Join(i.MalformedSamples, " | "))
}

// parseNixPSUID parses a ps uid column. procps prints a UID at or above 2^31
// (a user-namespaced pod's host UID, for example) as a negative signed 32-bit
// value, so a negative value in int32 range is wrapped back to its uint32.
func parseNixPSUID(raw string) (uint32, bool) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	switch {
	case value >= 0 && value <= math.MaxUint32:
		return uint32(value), true
	case value < 0 && value >= math.MinInt32:
		return uint32(value + (1 << 32)), true
	default:
		return 0, false
	}
}

// parseNixProcStatusUID returns the effective UID from the "Uid:" line of a
// /proc/<pid>/status file (real, effective, saved, filesystem).
func parseNixProcStatusUID(raw []byte) (uint32, error) {
	for _, line := range strings.Split(string(raw), "\n") {
		rest, ok := strings.CutPrefix(line, "Uid:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			return 0, fmt.Errorf("parse Uid line %q", line)
		}
		uid, err := strconv.ParseUint(fields[1], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("parse effective uid from %q: %w", line, err)
		}
		return uint32(uid), nil
	}
	return 0, errors.New("no Uid line in process status")
}

// nixBusyProcessReasons returns the active Nix work in processes. Only an
// exact executable basename with a store-mutating argv shape counts
// (nixActiveWorkReason): a long-lived evaluator or editor server such as
// nixd, nixd-attrset-eval, nil or nix repl is not build work, and Nix's own
// collector already honours the temp roots of every client connected to the
// daemon.
func nixBusyProcessReasons(processes []nixProcessSnapshot) ([]string, error) {
	seen := map[string]bool{}
	var reasons []string

	add := func(reason string) {
		if !seen[reason] {
			seen[reason] = true
			reasons = append(reasons, reason)
		}
	}

	for _, process := range processes {
		if process.ArgvErr != nil {
			command := nixBusyProcessCommandName(process.Comm)
			switch {
			case command == "nix-daemon":
				if reason, busy := nixDaemonProcessReason(process, nil); busy {
					add(reason)
				}
			case command != "":
				// The argv shape cannot be checked, so a known client
				// stays busy.
				add(command)
			case nixProcessInterpreterName(process.Comm) != "":
				// An interpreter may be running a client script; without
				// argv it cannot be told apart, so inspection fails closed.
				return nil, fmt.Errorf("inspect candidate pid %d (%s): %w", process.PID, process.Comm, process.ArgvErr)
			}
			// An unknown Nix-prefixed name (nixd-attrset-ev, ...) is not a
			// known client and is not build work.
			continue
		}

		command := nixBusyProcessCommandName(process.Comm)
		interpreter := nixProcessInterpreterName(process.Comm)
		if len(process.Argv) > 0 {
			if argvCommand := nixBusyProcessCommandName(process.Argv[0]); argvCommand != "" {
				command = argvCommand
				interpreter = ""
			} else if argvInterpreter := nixProcessInterpreterName(process.Argv[0]); argvInterpreter != "" {
				interpreter = argvInterpreter
				command = ""
			} else {
				// argv[0] is authoritative over a truncated comm: nixd and
				// nixd-attrset-eval are not Nix clients.
				command = ""
			}
		}

		args := process.Argv
		if len(args) > 0 {
			args = args[1:]
		}

		if command == "" && interpreter != "" {
			var ok bool
			command, args, ok = nixInterpreterScriptProcess(interpreter, args)
			if !ok {
				continue
			}
		}
		if command == "" {
			continue
		}
		args = nixNormalizedProcessArgs(args)
		if reason, busy := nixBusyProcessReason(process, command, args); busy {
			add(reason)
		}
	}

	sort.Strings(reasons)
	return reasons, nil
}

func nixBusyProcessCommandName(raw string) string {
	name := strings.ToLower(filepath.Base(strings.Trim(raw, `"'`)))
	switch name {
	case "nix", "nix-store", "nix-build", "nix-shell", "nix-instantiate", "nix-collect-garbage", "nix-env", "nix-prefetch-url", "nix-copy-closure", "nix-channel", "home-manager", "darwin-rebuild", "nixos-rebuild", "nix-daemon":
		return name
	// Linux truncates comm to 15 bytes; these are the known clients whose
	// names are longer. argv[0], when readable, still decides.
	case "nix-collect-gar":
		return "nix-collect-garbage"
	case "nix-prefetch-ur":
		return "nix-prefetch-url"
	case "nix-copy-closur":
		return "nix-copy-closure"
	default:
		return ""
	}
}

func nixProcessCandidateName(raw string) bool {
	if nixBusyProcessCommandName(raw) != "" || nixProcessInterpreterName(raw) != "" {
		return true
	}

	// Linux comm is limited to 15 bytes. Accept every Nix-prefixed candidate,
	// then validate its exact identity from boundary-preserving argv[0].
	return nixProcessNixPrefixedName(raw)
}

func nixProcessNixPrefixedName(raw string) bool {
	name := strings.ToLower(filepath.Base(strings.TrimSpace(raw)))
	return strings.HasPrefix(name, "nix")
}

func nixProcessSnapshots(ctx context.Context, output string, readArgv nixProcessArgvReader, readUID nixProcessUIDReader) ([]nixProcessSnapshot, nixProcessInspection, error) {
	type processRow struct {
		pid        int
		ppid       int
		uid        uint32
		uidUnknown bool
		comm       string
	}

	var inspection nixProcessInspection
	var rows []processRow
	nonEmpty := 0
	parentNames := map[int]string{}
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		nonEmpty++
		fields := strings.Fields(line)
		if len(fields) < 4 {
			inspection.malformed(line, false)
			continue
		}
		comm := strings.Join(fields[3:], " ")
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, ppidErr := strconv.Atoi(fields[1])
		if pidErr != nil || ppidErr != nil {
			inspection.malformed(line, nixProcessCandidateName(comm))
			continue
		}
		uid, uidOK := parseNixPSUID(fields[2])
		if !uidOK {
			// The row still identifies a process; only its ps UID is
			// unusable. Keep it so a candidate is still inspected, and let
			// the /proc reader below supply the UID where it can.
			inspection.malformed(line, false)
		}
		rows = append(rows, processRow{pid: pid, ppid: ppid, uid: uid, uidUnknown: !uidOK, comm: comm})
		parentNames[pid] = comm
	}
	if nonEmpty > 0 && len(rows) == 0 {
		return nil, inspection, fmt.Errorf("no parseable process rows in ps output (%d malformed): %s",
			inspection.MalformedRows, strings.Join(inspection.MalformedSamples, " | "))
	}

	var snapshots []nixProcessSnapshot
	for _, row := range rows {
		if !nixProcessCandidateName(row.comm) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, inspection, err
		}
		argv, err := readArgv(row.pid)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		uid, uidUnknown := row.uid, row.uidUnknown
		if readUID != nil {
			if procUID, uidErr := readUID(row.pid); uidErr == nil {
				uid, uidUnknown = procUID, false
			}
		}
		snapshots = append(snapshots, nixProcessSnapshot{
			PID:        row.pid,
			PPID:       row.ppid,
			UID:        uid,
			UIDUnknown: uidUnknown,
			Comm:       row.comm,
			ParentComm: parentNames[row.ppid],
			Argv:       argv,
			ArgvErr:    err,
		})
	}
	return snapshots, inspection, nil
}

func nixProcessInterpreterName(raw string) string {
	name := strings.TrimPrefix(strings.ToLower(filepath.Base(raw)), "-")
	switch name {
	case "sh", "bash", "dash", "zsh", "ksh", "perl", "python", "python2", "python3":
		return name
	default:
		return ""
	}
}

func nixInterpreterScriptProcess(interpreter string, args []string) (string, []string, bool) {
	for idx := 0; idx < len(args); idx++ {
		arg := args[idx]
		if arg == "--" {
			idx++
			if idx >= len(args) {
				return "", nil, false
			}
			return nixInterpreterScriptAt(args, idx)
		}
		if arg == "-" || nixInterpreterCommandPayloadOption(interpreter, arg) {
			return "", nil, false
		}
		if strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "+") {
			if nixInterpreterOptionTakesValue(interpreter, arg) {
				idx++
			}
			continue
		}

		return nixInterpreterScriptAt(args, idx)
	}
	return "", nil, false
}

func nixInterpreterScriptAt(args []string, idx int) (string, []string, bool) {
	command := nixBusyProcessCommandName(args[idx])
	return command, nixNormalizedProcessArgs(args[idx+1:]), command != ""
}

func nixInterpreterCommandPayloadOption(interpreter, raw string) bool {
	option := strings.ToLower(raw)
	switch interpreter {
	case "sh", "bash", "dash", "zsh", "ksh":
		return option == "--command" ||
			(strings.HasPrefix(option, "-") && !strings.HasPrefix(option, "--") && strings.Contains(option[1:], "c")) ||
			(strings.HasPrefix(option, "-") && !strings.HasPrefix(option, "--") && strings.Contains(option[1:], "s"))
	case "python", "python2", "python3":
		return nixPythonCommandPayloadOption(raw)
	case "perl":
		return strings.HasPrefix(raw, "-") && !strings.HasPrefix(raw, "--") && strings.ContainsAny(raw[1:], "eE")
	default:
		return false
	}
}

func nixInterpreterOptionTakesValue(interpreter, raw string) bool {
	option := strings.ToLower(raw)
	switch interpreter {
	case "sh", "bash", "dash", "zsh", "ksh":
		switch raw {
		case "-o", "+o", "-O", "+O":
			return true
		}
		return option == "--rcfile" || option == "--init-file" || option == "--startup-file"
	case "python", "python2", "python3":
		return nixPythonOptionTakesValue(raw) || option == "--check-hash-based-pycs"
	case "perl":
		return raw == "-I" || raw == "-M" || raw == "-m"
	default:
		return false
	}
}

func nixPythonCommandPayloadOption(raw string) bool {
	if len(raw) < 2 || raw[0] != '-' || strings.HasPrefix(raw, "--") {
		return false
	}
	for idx := 1; idx < len(raw); idx++ {
		switch raw[idx] {
		case 'c', 'm':
			return true
		case 'W', 'X':
			// The rest of this token is the value for -W or -X, not more
			// interpreter options.
			return false
		}
	}
	return false
}

func nixPythonOptionTakesValue(raw string) bool {
	if len(raw) < 2 || raw[0] != '-' || strings.HasPrefix(raw, "--") {
		return false
	}
	for idx := 1; idx < len(raw); idx++ {
		switch raw[idx] {
		case 'c', 'm':
			return false
		case 'W', 'X':
			return idx == len(raw)-1
		}
	}
	return false
}

func nixNormalizedProcessArgs(args []string) []string {
	normalized := make([]string, len(args))
	for idx, arg := range args {
		normalized[idx] = strings.ToLower(arg)
	}
	return normalized
}

func nixBusyProcessReason(process nixProcessSnapshot, command string, args []string) (string, bool) {
	if command == "nix-daemon" {
		return nixDaemonProcessReason(process, args)
	}
	return nixActiveWorkReason(command, args)
}

// nixActiveWorkReason decides, from an exact client name and its lowercased
// arguments, whether a process is store-mutating Nix work that a GC pass
// should wait for. Queries, evaluators and REPLs are not.
func nixActiveWorkReason(command string, args []string) (string, bool) {
	switch command {
	case "nix":
		return nixCommandWorkReason(args)
	case "nix-build", "nix-shell", "nix-collect-garbage", "nix-copy-closure", "nix-prefetch-url":
		return command, true
	case "nix-store":
		if op := nixLegacyOperation(args, nixStoreOperations); op != "" && nixStoreBusyOperations[op] {
			return "nix-store " + op, true
		}
		if nixHasArg(args, "--repair") {
			return "nix-store --repair", true
		}
		return "", false
	case "nix-env":
		if op := nixLegacyOperation(args, nixEnvOperations); op != "" && nixEnvBusyOperations[op] {
			return "nix-env " + op, true
		}
		return "", false
	case "nix-instantiate":
		// Instantiation writes .drv files unless it only parses or
		// evaluates.
		for _, arg := range args {
			switch arg {
			case "--parse", "--eval", "--eval-only", "--find-file", "--help", "--version":
				return "", false
			}
		}
		return command, true
	case "nix-channel":
		if nixHasArg(args, "--update") || nixHasArg(args, "--rollback") {
			return command, true
		}
		return "", false
	case "home-manager":
		if sub := nixFirstWord(args, nixHomeManagerBusy); sub != "" {
			return "home-manager " + sub, true
		}
		return "", false
	case "darwin-rebuild", "nixos-rebuild":
		if sub := nixFirstWord(args, nixRebuildBusy); sub != "" {
			return command + " " + sub, true
		}
		return "", false
	default:
		return "", false
	}
}

var (
	nixHomeManagerBusy = map[string]bool{"switch": true, "build": true}
	nixRebuildBusy     = map[string]bool{
		"switch": true, "boot": true, "test": true, "build": true, "dry-build": true,
		"dry-activate": true, "activate": true, "check": true, "build-vm": true,
		"build-vm-with-bootloader": true, "build-image": true, "--rollback": true,
	}

	// nixSubcommands is the vocabulary used to find the subcommand after
	// global options whose arity is not known.
	nixSubcommands = map[string]bool{
		"build": true, "bundle": true, "copy": true, "daemon": true, "derivation": true,
		"develop": true, "doctor": true, "edit": true, "eval": true, "flake": true,
		"fmt": true, "formatter": true, "hash": true, "help": true, "help-stores": true,
		"key": true, "log": true, "nar": true, "path-info": true, "print-dev-env": true,
		"profile": true, "realisation": true, "registry": true, "repl": true, "run": true,
		"search": true, "shell": true, "store": true, "upgrade-nix": true,
		"why-depends": true, "config": true, "env": true,
	}
	// nixOptionArity are global options that take values, so a value is
	// never read as the subcommand.
	nixOptionArity = map[string]int{
		"--option": 2, "--arg": 2, "--argstr": 2, "--override-input": 2,
		"--extra-experimental-features": 1, "--experimental-features": 1,
		"--store": 1, "--eval-store": 1, "--log-format": 1, "--max-jobs": 1, "-j": 1,
		"--cores": 1, "--builders": 1, "--substituters": 1, "--extra-substituters": 1,
		"-i": 1, "-I": 1, "--include": 1, "--file": 1, "-f": 1, "--system": 1,
		"--trusted-public-keys": 1, "--extra-trusted-public-keys": 1, "--profile": 1,
		"--out-link": 1, "-o": 1, "--reference-lock-file": 1, "--output-lock-file": 1,
		"--inputs-from": 1, "--override-flake": 2, "--update-input": 1,
	}
	nixStoreReadOnly   = map[string]bool{"ping": true, "info": true, "ls": true, "cat": true, "dump-path": true, "path-from-hash-part": true, "diff-closures": true}
	nixProfileReadOnly = map[string]bool{"list": true, "history": true, "diff-closures": true}
	nixFlakeBusy       = map[string]bool{"update": true, "lock": true, "check": true, "archive": true, "prefetch": true}

	nixStoreOperations = map[string]string{
		"-r": "--realise", "--realise": "--realise", "--realize": "--realise",
		"--add": "--add", "--add-fixed": "--add-fixed", "--import": "--import",
		"--gc": "--gc", "--delete": "--delete", "--optimise": "--optimise", "--optimize": "--optimise",
		"--restore": "--restore", "--load-db": "--load-db", "--serve": "--serve",
		"--repair-path": "--repair-path", "-q": "--query", "--query": "--query",
		"--dump": "--dump", "--export": "--export", "--verify": "--verify",
		"--verify-path": "--verify-path", "-l": "--read-log", "--read-log": "--read-log",
		"--dump-db": "--dump-db", "--print-env": "--print-env", "--generate-binary-cache-key": "--generate-binary-cache-key",
	}
	nixStoreBusyOperations = map[string]bool{
		"--realise": true, "--add": true, "--add-fixed": true, "--import": true, "--gc": true,
		"--delete": true, "--optimise": true, "--restore": true, "--load-db": true,
		"--serve": true, "--repair-path": true,
	}
	nixEnvOperations = map[string]string{
		"-i": "--install", "--install": "--install", "-u": "--upgrade", "--upgrade": "--upgrade",
		"-e": "--uninstall", "--uninstall": "--uninstall", "--set": "--set",
		"--set-flag": "--set-flag", "--rollback": "--rollback", "-g": "--switch-generation",
		"--switch-generation": "--switch-generation", "--delete-generations": "--delete-generations",
		"-q": "--query", "--query": "--query", "--list-generations": "--list-generations",
	}
	nixEnvBusyOperations = map[string]bool{
		"--install": true, "--upgrade": true, "--uninstall": true, "--set": true,
		"--set-flag": true, "--rollback": true, "--switch-generation": true, "--delete-generations": true,
	}
)

// nixCommandWorkReason classifies the new-style nix CLI by its subcommand.
func nixCommandWorkReason(args []string) (string, bool) {
	sub, rest := nixSubcommand(args)
	switch sub {
	case "":
		return "", false
	case "daemon":
		if nixDaemonWorker(rest) {
			return "nix-daemon worker", true
		}
		return "", false
	case "build", "develop", "shell", "run", "copy", "bundle", "print-dev-env", "upgrade-nix", "fmt":
		return "nix " + sub, true
	case "eval":
		// An eval may realise import-from-derivation; --read-only cannot.
		if nixHasArg(rest, "--read-only") {
			return "", false
		}
		return "nix eval", true
	case "store":
		next := nixFirstBare(rest)
		if next == "" || nixStoreReadOnly[next] {
			return "", false
		}
		return "nix store " + next, true
	case "profile":
		next := nixFirstBare(rest)
		if next == "" || nixProfileReadOnly[next] {
			return "", false
		}
		return "nix profile " + next, true
	case "flake":
		if next := nixFirstBare(rest); nixFlakeBusy[next] {
			return "nix flake " + next, true
		}
		return "", false
	case "derivation":
		if nixFirstBare(rest) == "add" {
			return "nix derivation add", true
		}
		return "", false
	default:
		return "", false
	}
}

// nixSubcommand returns the first argument that names a nix subcommand,
// skipping global options and the values of options with known arity.
func nixSubcommand(args []string) (string, []string) {
	for idx := 0; idx < len(args); idx++ {
		arg := args[idx]
		if strings.HasPrefix(arg, "-") {
			idx += nixOptionArity[arg]
			continue
		}
		if nixSubcommands[arg] {
			return arg, args[idx+1:]
		}
	}
	return "", nil
}

// nixFirstBare returns the first argument that is not an option.
func nixFirstBare(args []string) string {
	for idx := 0; idx < len(args); idx++ {
		if strings.HasPrefix(args[idx], "-") {
			idx += nixOptionArity[args[idx]]
			continue
		}
		return args[idx]
	}
	return ""
}

// nixFirstWord returns the first argument found in words.
func nixFirstWord(args []string, words map[string]bool) string {
	for _, arg := range args {
		if words[arg] {
			return arg
		}
	}
	return ""
}

func nixHasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// nixLegacyOperation returns the canonical operation of a nix-store or
// nix-env invocation: the first argument naming one, where a short cluster
// such as -iA or -qR names the operation by its first letter.
func nixLegacyOperation(args []string, operations map[string]string) string {
	for _, arg := range args {
		if op, ok := operations[arg]; ok {
			return op
		}
		if len(arg) > 2 && arg[0] == '-' && arg[1] != '-' {
			if op, ok := operations[arg[:2]]; ok {
				return op
			}
		}
	}
	return ""
}

func nixDaemonProcessReason(process nixProcessSnapshot, args []string) (string, bool) {
	if nixDaemonWorker(args) || nixDaemonProcessName(process.ParentComm) == "nix-daemon" {
		return "nix-daemon worker", true
	}
	if process.UIDUnknown || process.UID != 0 {
		return "nix-daemon state unknown", true
	}
	if process.PPID == 1 {
		return "", false
	}
	parent := nixDaemonProcessName(process.ParentComm)
	if parent == "determinate-nix" || parent == "determinate-nixd" {
		return "", false
	}
	return "nix-daemon state unknown", true
}

func nixDaemonProcessName(raw string) string {
	return strings.ToLower(filepath.Base(strings.TrimSpace(raw)))
}

func nixDaemonWorker(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "--stdio", "--serve", "realise", "realize", "substitute":
			return true
		}
	}
	return false
}

func nixContentionReason(output string) (string, bool) {
	normalized := strings.ToLower(strings.Join(strings.Fields(output), " "))
	if normalized == "" {
		return "", false
	}

	switch {
	case strings.Contains(normalized, "sqlite") && strings.Contains(normalized, "busy"):
		return "sqlite database busy", true
	case strings.Contains(normalized, "database is locked"):
		return "sqlite database locked", true
	case strings.Contains(normalized, "big-lock") &&
		(strings.Contains(normalized, "resource temporarily unavailable") ||
			strings.Contains(normalized, "temporarily unavailable") ||
			strings.Contains(normalized, "locked") ||
			strings.Contains(normalized, "busy")):
		return "nix store big-lock busy", true
	case strings.Contains(normalized, "waiting for the big nix store lock"):
		return "nix store big-lock busy", true
	default:
		return "", false
	}
}

// nixGCWedgedAppBundleRe matches the chmod failure nix-collect-garbage emits
// when a dead store path's Applications/*.app bundle cannot be made writable.
// The quoted path may contain spaces ("Visual Studio Code.app").
var nixGCWedgedAppBundleRe = regexp.MustCompile(`chmod "(/nix/store/[^/"]+/(?:[^"]*/)?Applications/[^"]*\.app)": Operation not permitted`)

// nixGCWedgedAppBundle reports whether nix-collect-garbage aborted on a
// TCC-protected macOS app bundle inside the store. Launching a nix-packaged
// GUI app stamps its .app bundle with the com.apple.macl xattr; App Management
// TCC then denies chmod on the bundle even for root, so GC aborts the whole
// pass, the path stays DB-orphaned, and every later GC re-hits it first.
func nixGCWedgedAppBundle(output string) (storePath, appPath string, ok bool) {
	matches := nixGCWedgedAppBundleRe.FindStringSubmatch(output)
	if len(matches) < 2 {
		return "", "", false
	}
	appPath = matches[1]
	entry, _, found := strings.Cut(strings.TrimPrefix(appPath, "/nix/store/"), "/")
	if !found || entry == "" {
		return "", "", false
	}
	return "/nix/store/" + entry, appPath, true
}

func (p *NixPlugin) parseDryRunFreedSpace(output string) int64 {
	if value := parseNixByteQuantity(output, []string{
		`would free\s+([\d.]+)\s*(B|KiB|MiB|GiB|TiB)`,
		`([\d.]+)\s*(B|KiB|MiB|GiB|TiB)\s+would be freed`,
	}); value > 0 {
		return value
	}
	return p.parseFreedSpace(output)
}

func (p *NixPlugin) parseDryRunStorePaths(output string) int {
	patterns := []string{
		`would delete\s+(\d+)\s+store paths?`,
		`(\d+)\s+store paths?\s+would be deleted`,
	}
	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(output)
		if len(matches) >= 2 {
			count, err := strconv.Atoi(matches[1])
			if err == nil {
				return count
			}
		}
	}
	return p.parseDeletedPaths(output)
}

func (p *NixPlugin) nixDryRunHasNoGCWork(output string) bool {
	return strings.TrimSpace(output) != "" && p.parseDryRunFreedSpace(output) == 0 && p.parseDryRunStorePaths(output) == 0
}

func (p *NixPlugin) parseFreedSpace(output string) int64 {
	return parseNixByteQuantity(output, []string{
		`([\d.]+)\s*(B|KiB|MiB|GiB|TiB)\s+freed`,
	})
}

func (p *NixPlugin) parseDeletedPaths(output string) int {
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
	return parseNixByteQuantity(output, []string{
		`([\d.]+)\s*(B|KiB|MiB|GiB|TiB)\s+freed by hard-linking`,
		`saved\s*([\d.]+)\s*(B|KiB|MiB|GiB|TiB)`,
	})
}

func parseNixByteQuantity(output string, patterns []string) int64 {
	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(output)
		if len(matches) >= 3 {
			value, err := strconv.ParseFloat(matches[1], 64)
			if err != nil {
				continue
			}

			switch matches[2] {
			case "TiB":
				return int64(value * 1024 * 1024 * 1024 * 1024)
			case "GiB":
				return int64(value * 1024 * 1024 * 1024)
			case "MiB":
				return int64(value * 1024 * 1024)
			case "KiB":
				return int64(value * 1024)
			case "B":
				return int64(value)
			}
		}
	}
	return 0
}
