package plugins

import (
	"context"
	"fmt"
	"log/slog"
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
type NixPlugin struct{}

type nixGeneration struct {
	Number    int
	CreatedAt time.Time
	Current   bool
	Scope     string
	Profile   string
}

type nixGCRoot struct {
	Root      string
	StorePath string
	Class     string
	Active    bool
}

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
			"cleanup_level":                             level.String(),
			"min_user_generations":                      strconv.Itoa(cfg.Nix.MinUserGenerations),
			"min_system_generations":                    strconv.Itoa(cfg.Nix.MinSystemGenerations),
			"delete_generations_older_than":             cfg.Nix.DeleteGenerationsOlderThan,
			"critical_delete_generations_older_than":    cfg.Nix.CriticalDeleteGenerationsOlderThan,
			"allow_store_optimize":                      strconv.FormatBool(cfg.Nix.AllowStoreOptimize),
			"skip_when_daemon_busy":                     strconv.FormatBool(cfg.Nix.SkipWhenDaemonBusy),
			"daemon_busy_backoff":                       cfg.Nix.DaemonBusyBackoff,
			"max_gc_duration":                           cfg.Nix.MaxGCDuration,
			"root_attribution_limit":                    strconv.Itoa(nixRootAttributionLimit(cfg.Nix)),
			"generation_policy_delete_older_than_level": nixGenerationPolicyAge(level, cfg.Nix),
		},
	}

	if !p.isNixAvailable() {
		plan.WouldRun = false
		plan.SkipReason = "nix_collect_garbage_not_available"
		plan.Summary = "Nix garbage collection is not available"
		return plan
	}

	if busy, err := p.activeNixProcesses(ctx); err != nil {
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
		if cfg.Nix.SkipWhenDaemonBusy {
			nixDeferPlan(&plan,
				"nix_daemon_busy",
				"Nix cleanup is deferred because active Nix work was detected",
				cfg.Nix,
				nixActiveWorkTargets(busy, cfg.Nix.DaemonBusyBackoff),
			)
			return plan
		}
	}

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

// Cleanup performs Nix garbage collection at the specified level.
func (p *NixPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{
		Plugin: p.Name(),
		Level:  level,
	}

	if !p.isNixAvailable() {
		logger.Debug("nix not available, skipping")
		return result
	}

	if cfg.Nix.SkipWhenDaemonBusy {
		busy, err := p.activeNixProcesses(ctx)
		if err != nil {
			logger.Warn("could not inspect active Nix processes", "error", err)
			return result
		} else if len(busy) > 0 {
			logger.Warn("skipping Nix cleanup because active Nix work was detected",
				"processes", strings.Join(busy, ", "),
				"backoff", cfg.Nix.DaemonBusyBackoff)
			return result
		}
	}

	if level >= LevelModerate {
		generationResult := p.deleteUserGenerationsByPolicy(ctx, level, cfg.Nix, logger)
		result.ItemsCleaned += generationResult.ItemsCleaned
		if generationResult.Error != nil {
			result.Error = generationResult.Error
			return result
		}
	}

	switch level {
	case LevelWarning, LevelModerate, LevelAggressive:
		gcResult := p.collectGarbage(ctx, level, nil, cfg.Nix, logger)
		result.BytesFreed += gcResult.BytesFreed
		result.CommandBytesFreed += gcResult.CommandBytesFreed
		result.ItemsCleaned += gcResult.ItemsCleaned
		result.Error = gcResult.Error
	case LevelCritical:
		gcResult := p.collectGarbageCritical(ctx, cfg.Nix, logger)
		result.BytesFreed += gcResult.BytesFreed
		result.CommandBytesFreed += gcResult.CommandBytesFreed
		result.ItemsCleaned += gcResult.ItemsCleaned
		result.Error = gcResult.Error
	}

	return result
}

func (p *NixPlugin) isNixAvailable() bool {
	_, err := exec.LookPath("nix-collect-garbage")
	return err == nil
}

func (p *NixPlugin) collectGarbageDryRun(ctx context.Context, cfg config.NixConfig) (string, error) {
	timeout := nixCommandTimeout(cfg)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "nix-collect-garbage", "--dry-run")
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

	cmd := exec.CommandContext(ctx, "nix-store", "--gc", "--print-roots")
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
	metadata["gc_root_attribution_roots"] = strconv.Itoa(len(roots))
	metadata["gc_root_attribution_classes"] = nixGCRootClassSummary(roots)

	targets := nixGCRootTargets(roots, limit)
	if len(targets) < len(roots) {
		metadata["gc_root_attribution_truncated"] = "true"
		return targets, metadata, []string{fmt.Sprintf("Nix GC root attribution truncated to %d of %d visible roots", len(targets), len(roots))}
	}

	return targets, metadata, nil
}

func (p *NixPlugin) collectGarbage(ctx context.Context, level CleanupLevel, args []string, cfg config.NixConfig, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: level}

	logger.Debug("running nix-collect-garbage", "args", strings.Join(args, " "))

	ctx, cancel := context.WithTimeout(ctx, nixCommandTimeout(cfg))
	defer cancel()

	cmd := exec.CommandContext(ctx, "nix-collect-garbage", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if reason, ok := nixContentionReason(string(output)); ok && cfg.SkipWhenDaemonBusy {
			logger.Warn("skipping Nix garbage collection because store contention was reported", "reason", reason)
			return result
		}
		result.Error = err
		return result
	}

	result.CommandBytesFreed = p.parseFreedSpace(string(output))
	result.BytesFreed = result.CommandBytesFreed
	result.ItemsCleaned = p.parseDeletedPaths(string(output))

	return result
}

func (p *NixPlugin) collectGarbageCritical(ctx context.Context, cfg config.NixConfig, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: LevelCritical}

	logger.Warn("CRITICAL: running Nix garbage collection")
	gcResult := p.collectGarbage(ctx, LevelCritical, nil, cfg, logger)
	result.BytesFreed = gcResult.BytesFreed
	result.CommandBytesFreed = gcResult.CommandBytesFreed
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
	optimizeCtx, cancel := context.WithTimeout(ctx, nixCommandTimeout(cfg))
	defer cancel()

	cmd := exec.CommandContext(optimizeCtx, "nix-store", "--optimize")
	output, err := cmd.CombinedOutput()
	if err != nil {
		logger.Error("nix-store --optimize failed", "error", err, "output", string(output))
		return result
	}

	optimizedBytes := p.parseOptimizedSpace(string(output))
	result.BytesFreed += optimizedBytes
	result.CommandBytesFreed += optimizedBytes

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
	if olderThan <= 0 {
		return result
	}

	targets := nixGenerationTargets(generations, time.Now(), cfg.MinUserGenerations, olderThan)
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

	cmd := exec.CommandContext(deleteCtx, "nix-env", args...)
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

func (p *NixPlugin) planGenerationTargets(ctx context.Context, level CleanupLevel, cfg config.NixConfig, logger *slog.Logger) ([]CleanupTarget, []string) {
	_ = logger

	if _, err := exec.LookPath("nix-env"); err != nil {
		return nil, []string{"nix-env is not available; generation retention targets could not be inspected"}
	}

	olderThan := parseNixPolicyDuration(nixGenerationPolicyAge(level, cfg), 0)
	if olderThan <= 0 {
		return nil, nil
	}

	var targets []CleanupTarget
	var warnings []string

	userGenerations, err := p.listGenerations(ctx, "user", "", cfg)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("could not inspect user Nix generations: %v", err))
	} else {
		targets = append(targets, nixGenerationTargets(userGenerations, time.Now(), cfg.MinUserGenerations, olderThan)...)
	}

	systemProfile := "/nix/var/nix/profiles/system"
	systemGenerations, err := p.listGenerations(ctx, "system", systemProfile, cfg)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("could not inspect system Nix generations: %v", err))
	} else {
		systemTargets := nixGenerationTargets(systemGenerations, time.Now(), cfg.MinSystemGenerations, olderThan)
		for i := range systemTargets {
			if systemTargets[i].Action == "delete_generation" {
				systemTargets[i].Protected = true
				systemTargets[i].Action = "review_privileged_generation"
				systemTargets[i].Reason = "outside retention policy, but system generation deletion requires explicit privileged workflow"
				annotateCleanupTargetPolicy(&systemTargets[i], CleanupTierPrivileged, CleanupReclaimDeferred)
			}
		}
		targets = append(targets, systemTargets...)
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

	cmd := exec.CommandContext(listCtx, "nix-env", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("nix-env %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}

	return parseNixGenerations(string(output), scope, profile)
}

func (p *NixPlugin) activeNixProcesses(ctx context.Context) ([]string, error) {
	psCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(psCtx, "ps", "-axo", "comm=,args=")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return nixBusyProcessReasons(string(output)), nil
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
			"Run plain Nix GC after selected user generation deletion",
		)
	case LevelCritical:
		steps = append(steps,
			fmt.Sprintf("Delete user generations older than %s only when at least %d user generations remain", cfg.CriticalDeleteGenerationsOlderThan, cfg.MinUserGenerations),
			"Run plain Nix GC after selected user generation deletion",
		)
		if cfg.AllowStoreOptimize {
			steps = append(steps, "Run nix-store --optimize because allow_store_optimize=true")
		} else {
			steps = append(steps, "Skip nix-store --optimize because allow_store_optimize=false")
		}
	}

	steps = append(steps, "Use cleanup-cycle host free-space accounting for before/after disk deltas")
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
		if root == "" {
			continue
		}

		key := root + "\x00" + storePath
		if seen[key] {
			continue
		}
		seen[key] = true

		class, active := classifyNixGCRoot(root)
		roots = append(roots, nixGCRoot{
			Root:      root,
			StorePath: storePath,
			Class:     class,
			Active:    active,
		})
	}

	sort.Slice(roots, func(i, j int) bool {
		if roots[i].Class == roots[j].Class {
			return roots[i].Root < roots[j].Root
		}
		return roots[i].Class < roots[j].Class
	})
	return roots
}

func classifyNixGCRoot(root string) (string, bool) {
	lower := strings.ToLower(root)

	switch {
	case strings.HasPrefix(root, "/proc/"):
		return "process_root", true
	case strings.Contains(lower, "/profiles/per-user/") ||
		strings.Contains(lower, "/nix/var/nix/profiles") ||
		strings.Contains(lower, "/.local/state/nix/profiles") ||
		strings.Contains(lower, "/.nix-profile"):
		return "profile_root", false
	case strings.Contains(lower, "/gcroots/auto/"):
		return "auto_gcroot", false
	case strings.Contains(lower, "/gcroots/"):
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
	if limit <= 0 || len(roots) == 0 {
		return nil
	}

	if limit > len(roots) {
		limit = len(roots)
	}

	targets := make([]CleanupTarget, 0, limit)
	for _, root := range roots[:limit] {
		reason := "visible Nix GC root retaining store path"
		if root.StorePath != "" {
			reason = fmt.Sprintf("%s %s", reason, filepath.Base(root.StorePath))
		}
		name := fmt.Sprintf("%s %s", root.Class, filepath.Base(root.Root))
		if root.Active {
			reason = "active process root; review the owning process before any Nix GC action"
		}

		target := CleanupTarget{
			Type:      "nix_gc_root",
			Name:      name,
			Path:      root.Root,
			Active:    root.Active,
			Protected: true,
			Action:    "review_gc_root",
			Reason:    reason,
		}
		annotateCleanupTargetPolicy(&target, CleanupTierSafe, CleanupReclaimNone)
		targets = append(targets, target)
	}
	return targets
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
	if minKeep < 1 {
		minKeep = 1
	}

	sorted := append([]nixGeneration(nil), generations...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].Number > sorted[j].Number
		}
		return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
	})

	protectedByMinimum := map[int]bool{}
	for idx, generation := range sorted {
		if idx >= minKeep {
			break
		}
		protectedByMinimum[generation.Number] = true
	}

	cutoff := now.Add(-olderThan)
	targets := make([]CleanupTarget, 0, len(sorted))
	for _, generation := range sorted {
		protected := generation.Current || protectedByMinimum[generation.Number] || generation.CreatedAt.After(cutoff)
		action := "keep_generation"
		reason := "within generation retention policy"
		switch {
		case generation.Current:
			reason = "current profile generation"
		case protectedByMinimum[generation.Number]:
			reason = fmt.Sprintf("within minimum retained %s generations", generation.Scope)
		case generation.CreatedAt.After(cutoff):
			reason = "younger than configured generation age"
		default:
			action = "delete_generation"
			reason = "older than configured generation age and outside retained minimum"
		}

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

func nixBusyProcessReasons(output string) []string {
	seen := map[string]bool{}
	var reasons []string

	add := func(reason string) {
		if !seen[reason] {
			seen[reason] = true
			reasons = append(reasons, reason)
		}
	}

	nixCommandPattern := regexp.MustCompile(`\bnix\s+(build|develop|shell|flake|profile|store|copy|run|log)\b`)
	nixStoreGCPattern := regexp.MustCompile(`\bnix-store\s+.*\s--gc\b|\bnix-store\s+--gc\b`)
	nixStoreGCShortPattern := regexp.MustCompile(`\bnix\s+store\s+gc\b`)

	for _, line := range strings.Split(output, "\n") {
		normalized := strings.ToLower(strings.Join(strings.Fields(line), " "))
		if normalized == "" {
			continue
		}

		switch {
		case strings.Contains(normalized, "home-manager switch"):
			add("home-manager switch")
		case strings.Contains(normalized, "darwin-rebuild"):
			add("darwin-rebuild")
		case strings.Contains(normalized, "nixos-rebuild"):
			add("nixos-rebuild")
		case strings.Contains(normalized, "nix-collect-garbage"):
			add("nix-collect-garbage")
		case nixStoreGCShortPattern.MatchString(normalized):
			add("nix store gc")
		case nixStoreGCPattern.MatchString(normalized):
			add("nix-store --gc")
		case strings.Contains(normalized, "nix-store") && strings.ContainsAny(normalized, "-"):
			add("nix-store")
		case nixCommandPattern.MatchString(normalized):
			add("nix " + nixCommandPattern.FindStringSubmatch(normalized)[1])
		case strings.Contains(normalized, "nix-daemon") &&
			(strings.Contains(normalized, "--stdio") ||
				strings.Contains(normalized, "--serve") ||
				strings.Contains(normalized, "realise") ||
				strings.Contains(normalized, "realize") ||
				strings.Contains(normalized, "substitute")):
			add("nix-daemon worker")
		}
	}

	sort.Strings(reasons)
	return reasons
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
