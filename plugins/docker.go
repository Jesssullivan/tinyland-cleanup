// Package plugins provides cleanup plugin implementations.
package plugins

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// DockerPlugin handles Docker cleanup operations.
type DockerPlugin struct {
	socketPath string
}

type dockerDFSummaryRow struct {
	Type             string
	Total            string
	Active           string
	SizeBytes        int64
	ReclaimableBytes int64
	Raw              string
}

// NewDockerPlugin creates a new Docker cleanup plugin.
func NewDockerPlugin() *DockerPlugin {
	return &DockerPlugin{}
}

// Name returns the plugin identifier.
func (p *DockerPlugin) Name() string {
	return "docker"
}

// Description returns the plugin description.
func (p *DockerPlugin) Description() string {
	return "Cleans Docker images, containers, volumes, networks, and build cache"
}

// SupportedPlatforms returns supported platforms (all).
func (p *DockerPlugin) SupportedPlatforms() []string {
	return nil // All platforms
}

// Enabled checks if Docker cleanup is enabled.
func (p *DockerPlugin) Enabled(cfg *config.Config) bool {
	return cfg.Enable.Docker
}

// PlanCleanup returns a non-mutating Docker cleanup plan.
func (p *DockerPlugin) PlanCleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupPlan {
	if cfg.Docker.Socket != "" {
		p.socketPath = cfg.Docker.Socket
	}

	plan := CleanupPlan{
		Plugin:   p.Name(),
		Level:    level.String(),
		Summary:  "Docker cleanup plan",
		WouldRun: true,
		Steps:    dockerPlanSteps(level, cfg.Docker),
		Metadata: map[string]string{
			"cleanup_level":              level.String(),
			"prune_images_age":           cfg.Docker.PruneImagesAge,
			"protect_running_containers": strconv.FormatBool(cfg.Docker.ProtectRunningContainers),
			"socket_configured":          strconv.FormatBool(p.socketPath != ""),
		},
	}

	if !p.isDockerAvailableContext(ctx) {
		plan.Summary = "Docker is not available"
		plan.WouldRun = false
		plan.SkipReason = "docker_unavailable"
		return plan
	}

	activeReasons, activeErr := p.activeDockerProcesses(ctx)
	if activeErr != nil {
		plan.Summary = "Docker cleanup is deferred because active process inspection failed"
		plan.WouldRun = false
		plan.SkipReason = "docker_process_inspection_failed"
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("could not inspect active Docker processes: %v", activeErr))
	} else if len(activeReasons) > 0 {
		plan.Metadata["active_docker_processes"] = strings.Join(activeReasons, ", ")
		if cfg.Docker.ProtectRunningContainers {
			plan.Summary = "Docker cleanup is deferred because active Docker work was detected"
			plan.WouldRun = false
			plan.SkipReason = "docker_active_work"
		}
	}

	dfOutput, err := p.runDockerCommandWithTimeout(ctx, 30*time.Second, "system", "df")
	if err != nil {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("could not inspect Docker disk usage: %v", err))
		plan.Metadata["system_df_available"] = "false"
	} else {
		rows := parseDockerDFSummaryRows(dfOutput)
		reclaim := dockerPlanReclaimExpectation()
		plan.Targets = dockerPlanTargets(rows, level, reclaim, len(activeReasons) > 0 && cfg.Docker.ProtectRunningContainers)
		plan.EstimatedBytesFreed = dockerEstimatedCandidateBytes(plan.Targets)
		plan.Metadata["system_df_available"] = "true"
		plan.Metadata["target_count"] = strconv.Itoa(len(plan.Targets))
		plan.Metadata["total_physical_bytes"] = strconv.FormatInt(dockerTotalSizeBytes(rows), 10)
		plan.Metadata["total_reclaimable_bytes"] = strconv.FormatInt(dockerTotalReclaimableBytes(rows), 10)
	}

	plan.Warnings = append(plan.Warnings, "Docker reported reclaimable bytes may describe daemon or VM storage and may not immediately equal host free-space delta")
	plan.Warnings = append(plan.Warnings, "Docker cleanup should not run while active build, pull, push, or compose work is detected")
	return plan
}

// Cleanup performs Docker cleanup at the specified level.
func (p *DockerPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{
		Plugin: p.Name(),
		Level:  level,
	}

	// Store socket path from config for use in commands
	if cfg.Docker.Socket != "" {
		p.socketPath = cfg.Docker.Socket
	}

	// Check if docker is available
	if !p.isDockerAvailable() {
		logger.Debug("docker not available, skipping")
		return result
	}

	if cfg.Docker.ProtectRunningContainers {
		activeReasons, err := p.activeDockerProcesses(ctx)
		if err != nil {
			logger.Warn("skipping Docker cleanup because active process inspection failed", "error", err)
			return result
		}
		if len(activeReasons) > 0 {
			logger.Warn("skipping Docker cleanup because active Docker work was detected", "active", strings.Join(activeReasons, ", "))
			return result
		}
	}

	switch level {
	case LevelWarning:
		// Light cleanup: just dangling images
		result = p.cleanDangling(ctx, logger)
	case LevelModerate:
		// Moderate: dangling + old images + old containers
		result = p.cleanModerate(ctx, cfg, logger)
	case LevelAggressive:
		// Aggressive: + volumes + build cache
		result = p.cleanAggressive(ctx, cfg, logger)
	case LevelCritical:
		// Emergency: full system prune with volumes
		result = p.cleanCritical(ctx, logger)
	}

	return result
}

func (p *DockerPlugin) isDockerAvailable() bool {
	return p.isDockerAvailableContext(context.Background())
}

func (p *DockerPlugin) isDockerAvailableContext(ctx context.Context) bool {
	availableCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(availableCtx, "docker", "info")
	if p.socketPath != "" {
		cmd.Env = append(os.Environ(), "DOCKER_HOST=unix://"+p.socketPath)
	}
	return cmd.Run() == nil
}

func (p *DockerPlugin) cleanDangling(ctx context.Context, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: LevelWarning}

	logger.Debug("cleaning dangling images")
	output, err := p.runDockerCommand(ctx, "image", "prune", "-f")
	if err != nil {
		result.Error = err
		return result
	}

	result.BytesFreed = p.parseReclaimedSpace(output)
	return result
}

func (p *DockerPlugin) cleanModerate(ctx context.Context, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: LevelModerate}

	// Clean dangling images
	logger.Debug("cleaning dangling images")
	if output, err := p.runDockerCommand(ctx, "image", "prune", "-f"); err == nil {
		result.BytesFreed += p.parseReclaimedSpace(output)
	} else {
		logger.Warn("dangling image prune failed", "error", err, "output", output)
	}

	// Clean old images
	logger.Debug("cleaning old images", "age", cfg.Docker.PruneImagesAge)
	args := []string{"image", "prune", "-af", "--filter", fmt.Sprintf("until=%s", cfg.Docker.PruneImagesAge)}
	if output, err := p.runDockerCommand(ctx, args...); err == nil {
		result.BytesFreed += p.parseReclaimedSpace(output)
	} else {
		logger.Warn("old image prune failed", "error", err, "output", output)
	}

	// Clean old stopped containers
	logger.Debug("cleaning old containers")
	if output, err := p.runDockerCommand(ctx, "container", "prune", "-f", "--filter", "until=1h"); err == nil {
		result.BytesFreed += p.parseReclaimedSpace(output)
	} else {
		logger.Warn("container prune failed", "error", err, "output", output)
	}

	// Clean old buildx cache
	logger.Debug("cleaning buildx cache")
	if output, err := p.runDockerCommand(ctx, "buildx", "prune", "-f", "--filter", "until=24h"); err == nil {
		result.BytesFreed += p.parseReclaimedSpace(output)
	} else {
		logger.Warn("buildx cache prune failed", "error", err, "output", output)
	}

	return result
}

func (p *DockerPlugin) cleanAggressive(ctx context.Context, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := p.cleanModerate(ctx, cfg, logger)
	result.Level = LevelAggressive

	// Clean unused volumes (including named volumes)
	logger.Debug("cleaning unused volumes")
	if output, err := p.runDockerCommand(ctx, "volume", "prune", "-af"); err == nil {
		result.BytesFreed += p.parseReclaimedSpace(output)
	} else {
		logger.Warn("volume prune failed", "error", err, "output", output)
	}

	// Clean unused networks
	logger.Debug("cleaning unused networks")
	if output, err := p.runDockerCommand(ctx, "network", "prune", "-f"); err == nil {
		result.BytesFreed += p.parseReclaimedSpace(output)
	} else {
		logger.Warn("network prune failed", "error", err, "output", output)
	}

	// Clean all build cache
	logger.Debug("cleaning all build cache")
	if output, err := p.runDockerCommand(ctx, "builder", "prune", "-af"); err == nil {
		result.BytesFreed += p.parseReclaimedSpace(output)
	} else {
		logger.Warn("builder cache prune failed", "error", err, "output", output)
	}

	return result
}

func (p *DockerPlugin) cleanCritical(ctx context.Context, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: LevelCritical}

	// Full system prune with volumes
	logger.Warn("CRITICAL: running full Docker system prune with volumes")
	output, err := p.runDockerCommand(ctx, "system", "prune", "-af", "--volumes")
	if err != nil {
		result.Error = err
		return result
	}

	result.BytesFreed = p.parseReclaimedSpace(output)
	return result
}

func (p *DockerPlugin) runDockerCommand(ctx context.Context, args ...string) (string, error) {
	return p.runDockerCommandWithTimeout(ctx, 5*time.Minute, args...)
}

func (p *DockerPlugin) runDockerCommandWithTimeout(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", args...)
	if p.socketPath != "" {
		cmd.Env = append(os.Environ(), "DOCKER_HOST=unix://"+p.socketPath)
	}
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func (p *DockerPlugin) activeDockerProcesses(ctx context.Context) ([]string, error) {
	psCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(psCtx, "ps", "-axo", "comm=,args=")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return dockerBusyProcessReasons(string(output)), nil
}

func dockerBusyProcessReasons(output string) []string {
	reasons := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		lower := strings.ToLower(line)
		switch {
		case strings.Contains(lower, "docker-buildx") && strings.Contains(lower, " build"):
			reasons["docker buildx"] = true
		case strings.Contains(lower, "docker buildx build"):
			reasons["docker buildx"] = true
		case strings.Contains(lower, "docker compose build"):
			reasons["docker compose build"] = true
		case strings.Contains(lower, "docker compose up"):
			reasons["docker compose up"] = true
		case strings.Contains(lower, "docker build "):
			reasons["docker build"] = true
		case strings.Contains(lower, "docker pull "):
			reasons["docker pull"] = true
		case strings.Contains(lower, "docker push "):
			reasons["docker push"] = true
		case strings.Contains(lower, "docker run "):
			reasons["docker run"] = true
		}
	}

	if len(reasons) == 0 {
		return nil
	}
	out := make([]string, 0, len(reasons))
	for reason := range reasons {
		out = append(out, reason)
	}
	sort.Strings(out)
	return out
}

func (p *DockerPlugin) parseReclaimedSpace(output string) int64 {
	// Parse "Total reclaimed space: X.XXY" or similar patterns
	// Examples:
	//   "Total reclaimed space: 1.234GB"
	//   "Total reclaimed space: 567.8MB"
	//   "reclaimed space: 123.4kB"

	patterns := []string{
		`reclaimed space:\s*([\d.]+)\s*([KMGT]?B)`,
		`Total reclaimed space:\s*([\d.]+)\s*([KMGT]?B)`,
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(output)
		if len(matches) >= 3 {
			value, err := strconv.ParseFloat(matches[1], 64)
			if err != nil {
				continue
			}

			unit := matches[2]
			switch strings.ToUpper(unit) {
			case "KB":
				return int64(value * 1024)
			case "MB":
				return int64(value * 1024 * 1024)
			case "GB":
				return int64(value * 1024 * 1024 * 1024)
			case "TB":
				return int64(value * 1024 * 1024 * 1024 * 1024)
			case "B":
				return int64(value)
			}
		}
	}

	return 0
}

func dockerPlanSteps(level CleanupLevel, cfg config.DockerConfig) []string {
	switch level {
	case LevelWarning:
		return []string{"Prune dangling Docker images"}
	case LevelModerate:
		return []string{
			"Prune dangling Docker images",
			fmt.Sprintf("Prune Docker images older than %s", cfg.PruneImagesAge),
			"Prune stopped Docker containers older than 1h",
			"Prune Docker buildx cache older than 24h",
		}
	case LevelAggressive:
		return []string{
			"Run moderate Docker cleanup",
			"Prune unused Docker volumes",
			"Prune unused Docker networks",
			"Prune all Docker builder cache",
		}
	case LevelCritical:
		return []string{"Run full Docker system prune with volumes"}
	default:
		return []string{"Report Docker cleanup state"}
	}
}

func parseDockerDFSummaryRows(output string) []dockerDFSummaryRow {
	var rows []dockerDFSummaryRow
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "TYPE") || strings.HasPrefix(line, "Docker") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}

		var row dockerDFSummaryRow
		switch {
		case len(fields) >= 6 && fields[0] == "Local" && fields[1] == "Volumes":
			row = dockerDFSummaryRow{
				Type:             "Local Volumes",
				Total:            fields[2],
				Active:           fields[3],
				SizeBytes:        parseDockerSizeBytes(fields[4]),
				ReclaimableBytes: parseDockerSizeBytes(fields[5]),
				Raw:              line,
			}
		case len(fields) >= 6 && fields[0] == "Build" && fields[1] == "Cache":
			row = dockerDFSummaryRow{
				Type:             "Build Cache",
				Total:            fields[2],
				Active:           fields[3],
				SizeBytes:        parseDockerSizeBytes(fields[4]),
				ReclaimableBytes: parseDockerSizeBytes(fields[5]),
				Raw:              line,
			}
		default:
			row = dockerDFSummaryRow{
				Type:             fields[0],
				Total:            fields[1],
				Active:           fields[2],
				SizeBytes:        parseDockerSizeBytes(fields[3]),
				ReclaimableBytes: parseDockerSizeBytes(fields[4]),
				Raw:              line,
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func parseDockerSizeBytes(value string) int64 {
	value = strings.TrimSpace(strings.Trim(value, ","))
	re := regexp.MustCompile(`(?i)^([\d.]+)\s*([kmgt]?i?b|b)$`)
	matches := re.FindStringSubmatch(value)
	if len(matches) != 3 {
		return 0
	}

	number, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0
	}

	switch strings.ToUpper(matches[2]) {
	case "B":
		return int64(number)
	case "KB", "KIB":
		return int64(number * 1024)
	case "MB", "MIB":
		return int64(number * 1024 * 1024)
	case "GB", "GIB":
		return int64(number * 1024 * 1024 * 1024)
	case "TB", "TIB":
		return int64(number * 1024 * 1024 * 1024 * 1024)
	default:
		return 0
	}
}

func dockerPlanTargets(rows []dockerDFSummaryRow, level CleanupLevel, reclaim string, activeProtected bool) []CleanupTarget {
	var targets []CleanupTarget
	for _, row := range rows {
		target, ok := dockerPlanTarget(row, level, activeProtected)
		if !ok {
			continue
		}
		annotateCleanupTargetPolicy(&target, target.Tier, reclaim)
		targets = append(targets, target)
	}
	return targets
}

func dockerPlanTarget(row dockerDFSummaryRow, level CleanupLevel, activeProtected bool) (CleanupTarget, bool) {
	target := CleanupTarget{
		Name:      row.Type,
		Bytes:     row.ReclaimableBytes,
		Active:    activeProtected,
		Protected: activeProtected,
	}
	if activeProtected {
		target.Action = "protect"
		target.Reason = "active Docker build, pull, push, run, or compose process detected"
	}

	switch row.Type {
	case "Images":
		target.Type = "docker-images"
		target.Tier = CleanupTierWarm
		if !activeProtected {
			target.Action = "prune_images"
			if level == LevelWarning {
				target.Action = "prune_dangling_images"
				target.Reason = "warning level prunes dangling images only; Docker df reclaimable is an upper bound"
			} else {
				target.Reason = "Docker images are eligible for age-filtered or dangling-image prune"
			}
		}
	case "Containers":
		if level < LevelModerate {
			return CleanupTarget{}, false
		}
		target.Type = "docker-containers"
		target.Tier = CleanupTierSafe
		if !activeProtected {
			target.Action = "prune_stopped_containers"
			target.Reason = "stopped containers older than the configured threshold are eligible"
		}
	case "Local Volumes":
		if level < LevelAggressive {
			return CleanupTarget{}, false
		}
		target.Type = "docker-volumes"
		target.Tier = CleanupTierDestructive
		if !activeProtected {
			target.Action = "prune_unused_volumes"
			target.Reason = "unused volumes are eligible only at aggressive or critical levels"
		}
	case "Build Cache":
		if level < LevelModerate {
			return CleanupTarget{}, false
		}
		target.Type = "docker-build-cache"
		target.Tier = CleanupTierWarm
		if !activeProtected {
			if level == LevelModerate {
				target.Action = "prune_old_build_cache"
				target.Reason = "moderate level prunes buildx cache older than 24h"
			} else {
				target.Action = "prune_builder_cache"
				target.Reason = "builder cache is eligible at aggressive and critical levels"
			}
		}
	default:
		return CleanupTarget{}, false
	}

	if level == LevelCritical && !activeProtected {
		target.Action = "system_prune_with_volumes"
		target.Reason = "critical level runs full Docker system prune with volumes"
	}
	return target, true
}

func dockerPlanReclaimExpectation() string {
	if runtime.GOOS == "linux" {
		return CleanupReclaimHost
	}
	return CleanupReclaimDeferred
}

func dockerEstimatedCandidateBytes(targets []CleanupTarget) int64 {
	var total int64
	for _, target := range targets {
		if target.Protected {
			continue
		}
		total += target.Bytes
	}
	return total
}

func dockerTotalSizeBytes(rows []dockerDFSummaryRow) int64 {
	var total int64
	for _, row := range rows {
		total += row.SizeBytes
	}
	return total
}

func dockerTotalReclaimableBytes(rows []dockerDFSummaryRow) int64 {
	var total int64
	for _, row := range rows {
		total += row.ReclaimableBytes
	}
	return total
}

// ProactiveCleanup checks Docker reclaimable space and cleans if needed.
// This is useful for Docker Desktop VMs that have separate disk from host.
func (p *DockerPlugin) ProactiveCleanup(ctx context.Context, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name() + "-proactive"}

	// Get Docker system df info
	output, err := p.runDockerCommand(ctx, "system", "df", "--format", "{{.Reclaimable}}")
	if err != nil {
		logger.Warn("docker system df failed", "error", err, "output", output)
		return result
	}

	// Parse reclaimable space
	reclaimableGB := p.parseReclaimableGB(output)
	if reclaimableGB < 10 {
		return result // Less than 10GB reclaimable, skip
	}

	logger.Info("proactive Docker cleanup", "reclaimable_gb", reclaimableGB)

	// Clean dangling images
	if output, err := p.runDockerCommand(ctx, "image", "prune", "-f"); err == nil {
		result.ItemsCleaned++
	} else {
		logger.Warn("proactive image prune failed", "error", err, "output", output)
	}

	// Clean old containers
	if output, err := p.runDockerCommand(ctx, "container", "prune", "-f", "--filter", "until=1h"); err == nil {
		result.ItemsCleaned++
	} else {
		logger.Warn("proactive container prune failed", "error", err, "output", output)
	}

	// Clean old build cache
	if output, err := p.runDockerCommand(ctx, "builder", "prune", "-f", "--filter", "until=24h"); err == nil {
		result.ItemsCleaned++
	} else {
		logger.Warn("proactive builder prune failed", "error", err, "output", output)
	}

	// Clean unused volumes (including named volumes)
	if output, err := p.runDockerCommand(ctx, "volume", "prune", "-af"); err == nil {
		result.ItemsCleaned++
	} else {
		logger.Warn("proactive volume prune failed", "error", err, "output", output)
	}

	return result
}

func (p *DockerPlugin) parseReclaimableGB(output string) int {
	// Parse first line which should be something like "10.5GB (50%)"
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 {
		return 0
	}

	line := lines[0]
	// Extract number and unit
	re := regexp.MustCompile(`([\d.]+)\s*([KMGT]?B)`)
	matches := re.FindStringSubmatch(line)
	if len(matches) < 3 {
		return 0
	}

	value, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0
	}

	switch strings.ToUpper(matches[2]) {
	case "GB":
		return int(value)
	case "TB":
		return int(value * 1000)
	case "MB":
		return 0 // Less than 1GB
	default:
		return 0
	}
}
