// Package plugins provides cleanup plugin implementations.
package plugins

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// PodmanPlugin handles Podman cleanup operations.
type PodmanPlugin struct {
	environment *PodmanEnvironment
}

// PodmanEnvironment contains information about the Podman runtime environment.
type PodmanEnvironment struct {
	// Runtime is "podman" if available, "" otherwise
	Runtime string
	// NeedsVM is true on Darwin where Podman requires a VM
	NeedsVM bool
	// VMProvider is "applehv", "libkrun", "qemu", or "" (Linux)
	VMProvider string
	// VMRunning is true if a Podman machine is running
	VMRunning bool
	// MachineName is the name of the running machine
	MachineName string
	// StoragePath is the path to container storage
	StoragePath string
	// SocketPath is the path to the Podman socket
	SocketPath string
}

const podmanCompactionGiB = int64(1024 * 1024 * 1024)

type podmanCompactionPlan struct {
	MachineName               string
	Provider                  string
	DiskFormat                string
	DiskPath                  string
	ScratchDir                string
	TempPath                  string
	BackupPath                string
	ConfigEnabled             bool
	QemuImgPath               string
	QemuImgAvailable          bool
	ActiveContainers          bool
	ActiveContainerCheckError string
	DiskPathExpected          bool
	BackupExists              bool
	ScratchDirConfigured      bool
	ScratchDirAvailable       bool
	ScratchDirCrossDevice     bool
	LogicalBytes              int64
	PhysicalBytes             int64
	FreeBytes                 int64
	RequiredFreeBytes         int64
	EstimatedReclaimBytes     int64
	CanCompact                bool
	SkipReason                string
	Warnings                  []string
	Steps                     []string
}

type podmanCompactionPlanInput struct {
	MachineName               string
	Provider                  string
	DiskPath                  string
	ScratchDir                string
	ConfigEnabled             bool
	QemuImgPath               string
	QemuImgAvailable          bool
	ActiveContainers          bool
	ActiveContainerCheckError string
	DiskPathExpected          bool
	BackupExists              bool
	ScratchDirConfigured      bool
	ScratchDirAvailable       bool
	ScratchDirCrossDevice     bool
	LogicalBytes              int64
	PhysicalBytes             int64
	FreeBytes                 int64
	Config                    config.PodmanConfig
}

// NewPodmanPlugin creates a new Podman cleanup plugin.
func NewPodmanPlugin() *PodmanPlugin {
	return &PodmanPlugin{}
}

// Name returns the plugin identifier.
func (p *PodmanPlugin) Name() string {
	return "podman"
}

// Description returns the plugin description.
func (p *PodmanPlugin) Description() string {
	return "Cleans Podman images, containers, volumes, build cache, and VM disk space"
}

// SupportedPlatforms returns supported platforms (all).
func (p *PodmanPlugin) SupportedPlatforms() []string {
	return nil // All platforms
}

// Enabled checks if Podman cleanup is enabled.
func (p *PodmanPlugin) Enabled(cfg *config.Config) bool {
	return cfg.Enable.Podman
}

// PlanCleanup returns a dry-run plan without mutating Podman state.
func (p *PodmanPlugin) PlanCleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupPlan {
	plan := CleanupPlan{
		Plugin:   p.Name(),
		Level:    level.String(),
		Summary:  "Podman cleanup plan",
		WouldRun: true,
		Metadata: map[string]string{
			"cleanup_level": level.String(),
		},
	}

	if p.environment == nil {
		env, err := detectPodmanEnvironment()
		if err != nil {
			plan.WouldRun = false
			plan.SkipReason = "environment_detection_failed"
			plan.Summary = "Podman environment could not be inspected"
			plan.Warnings = append(plan.Warnings, err.Error())
			return plan
		}
		p.environment = env
	}

	if p.environment.Runtime != "podman" {
		plan.WouldRun = false
		plan.SkipReason = "podman_not_available"
		plan.Summary = "Podman is not available"
		return plan
	}

	plan.Metadata["needs_vm"] = strconv.FormatBool(p.environment.NeedsVM)
	plan.Metadata["vm_provider"] = p.environment.VMProvider
	plan.Metadata["vm_running"] = strconv.FormatBool(p.environment.VMRunning)
	plan.Metadata["machine_name"] = p.environment.MachineName

	if p.environment.NeedsVM && !p.environment.VMRunning {
		plan.WouldRun = false
		plan.SkipReason = "podman_machine_not_running"
		plan.Summary = "Podman machine is not running"
		return plan
	}

	switch level {
	case LevelWarning:
		plan.Steps = append(plan.Steps, "Prune dangling Podman images")
	case LevelModerate:
		plan.Steps = append(plan.Steps,
			"Prune dangling Podman images",
			fmt.Sprintf("Prune Podman images older than %s", cfg.Podman.PruneImagesAge),
			"Prune old stopped Podman containers",
			"Prune Podman build cache",
		)
	case LevelAggressive:
		plan.Steps = append(plan.Steps,
			"Run moderate Podman cleanup",
			"Prune unused Podman volumes",
			"Prune Podman build containers",
		)
		if runtime.GOOS == "darwin" && p.environment.VMRunning && cfg.Podman.TrimVMDisk && !p.fstrimReclaimsHostSpace() {
			plan.Warnings = append(plan.Warnings, "guest fstrim is not counted as host reclaim for this provider")
		}
	case LevelCritical:
		plan.Steps = append(plan.Steps,
			"Run full Podman system prune with volumes",
			"Prune external Podman storage when supported",
		)
		if runtime.GOOS == "darwin" && p.environment.VMRunning {
			if cfg.Podman.CleanInsideVM {
				plan.Steps = append(plan.Steps, "Run critical cleanup inside the Podman VM")
			}
			if cfg.Podman.TrimVMDisk && !p.fstrimReclaimsHostSpace() {
				plan.Warnings = append(plan.Warnings, "guest fstrim is not counted as host reclaim for this provider")
			}

			compaction := p.planOfflineCompaction(ctx, cfg, logger)
			plan.RequiredFreeBytes = compaction.RequiredFreeBytes
			if compaction.CanCompact {
				plan.EstimatedBytesFreed = compaction.EstimatedReclaimBytes
			}
			plan.Warnings = append(plan.Warnings, compaction.Warnings...)
			plan.Steps = append(plan.Steps, compaction.Steps...)
			plan.Targets = append(plan.Targets, podmanCompactionTargets(compaction)...)
			plan.Metadata["offline_compaction_enabled"] = strconv.FormatBool(compaction.ConfigEnabled)
			plan.Metadata["offline_compaction_can_run"] = strconv.FormatBool(compaction.CanCompact)
			plan.Metadata["offline_compaction_skip_reason"] = compaction.SkipReason
			plan.Metadata["offline_compaction_provider"] = compaction.Provider
			plan.Metadata["offline_compaction_format"] = compaction.DiskFormat
			plan.Metadata["offline_compaction_disk_path"] = compaction.DiskPath
			plan.Metadata["offline_compaction_scratch_dir"] = compaction.ScratchDir
			plan.Metadata["offline_compaction_temp_path"] = compaction.TempPath
			plan.Metadata["offline_compaction_backup_path"] = compaction.BackupPath
			plan.Metadata["offline_compaction_qemu_img_path"] = compaction.QemuImgPath
			plan.Metadata["offline_compaction_logical_bytes"] = strconv.FormatInt(compaction.LogicalBytes, 10)
			plan.Metadata["offline_compaction_physical_bytes"] = strconv.FormatInt(compaction.PhysicalBytes, 10)
			plan.Metadata["offline_compaction_free_bytes"] = strconv.FormatInt(compaction.FreeBytes, 10)
			plan.Metadata["offline_compaction_required_free_bytes"] = strconv.FormatInt(compaction.RequiredFreeBytes, 10)
			plan.Metadata["offline_compaction_estimated_reclaim_bytes"] = strconv.FormatInt(compaction.EstimatedReclaimBytes, 10)
			plan.Metadata["offline_compaction_active_containers"] = strconv.FormatBool(compaction.ActiveContainers)
			plan.Metadata["offline_compaction_scratch_dir_configured"] = strconv.FormatBool(compaction.ScratchDirConfigured)
			plan.Metadata["offline_compaction_scratch_dir_available"] = strconv.FormatBool(compaction.ScratchDirAvailable)
			plan.Metadata["offline_compaction_scratch_dir_cross_device"] = strconv.FormatBool(compaction.ScratchDirCrossDevice)
			plan.Metadata["target_count"] = strconv.Itoa(len(plan.Targets))
		}
	}

	return plan
}

// Cleanup performs Podman cleanup at the specified level.
func (p *PodmanPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{
		Plugin: p.Name(),
		Level:  level,
	}

	// Initialize environment detection
	if p.environment == nil {
		env, err := detectPodmanEnvironment()
		if err != nil {
			logger.Debug("podman environment detection failed", "error", err)
			return result
		}
		if env.Runtime != "podman" {
			logger.Debug("podman not available, skipping")
			return result
		}
		p.environment = env
		logger.Debug("podman environment detected",
			"needs_vm", env.NeedsVM,
			"vm_provider", env.VMProvider,
			"vm_running", env.VMRunning,
			"machine_name", env.MachineName)
	}

	// On Darwin, check if VM is running before attempting cleanup
	if p.environment.NeedsVM && !p.environment.VMRunning {
		logger.Debug("podman machine not running, skipping")
		return result
	}

	switch level {
	case LevelWarning:
		// Light cleanup: dangling images only
		result = p.cleanDangling(ctx, logger)
	case LevelModerate:
		// Moderate: + old images + old containers + build cache
		result = p.cleanModerate(ctx, cfg, logger)
	case LevelAggressive:
		// Aggressive: + volumes + VM fstrim
		result = p.cleanAggressive(ctx, cfg, logger)
	case LevelCritical:
		// Emergency: full system prune + external cleanup + VM optimization
		result = p.cleanCritical(ctx, cfg, logger)
	}

	return result
}

// cleanDangling removes dangling (untagged) images.
func (p *PodmanPlugin) cleanDangling(ctx context.Context, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: LevelWarning}

	logger.Debug("cleaning dangling podman images")
	output, err := p.runPodmanCommand(ctx, "image", "prune", "-f")
	if err != nil {
		logger.Warn("failed to prune dangling images", "error", err)
		result.Error = err
		return result
	}

	result.BytesFreed = p.parseReclaimedSpace(output)
	if result.BytesFreed > 0 {
		result.ItemsCleaned++
		logger.Debug("cleaned dangling images", "freed_mb", result.BytesFreed/(1024*1024))
	}

	return result
}

// cleanModerate performs moderate cleanup: dangling images, old images, old containers, build cache.
func (p *PodmanPlugin) cleanModerate(ctx context.Context, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: LevelModerate}

	// Clean dangling images
	logger.Debug("cleaning dangling podman images")
	if output, err := p.runPodmanCommand(ctx, "image", "prune", "-f"); err == nil {
		result.BytesFreed += p.parseReclaimedSpace(output)
		result.ItemsCleaned++
	}

	// Clean old images (with age filter)
	logger.Debug("cleaning old podman images", "age", cfg.Podman.PruneImagesAge)
	args := []string{"image", "prune", "-af", "--filter", fmt.Sprintf("until=%s", cfg.Podman.PruneImagesAge)}
	if output, err := p.runPodmanCommand(ctx, args...); err == nil {
		result.BytesFreed += p.parseReclaimedSpace(output)
		result.ItemsCleaned++
	}

	// Clean old stopped containers
	logger.Debug("cleaning old podman containers")
	if output, err := p.runPodmanCommand(ctx, "container", "prune", "-f", "--filter", "until=1h"); err == nil {
		result.BytesFreed += p.parseReclaimedSpace(output)
		result.ItemsCleaned++
	}

	// Clean build cache (important for Podman - survives normal prune)
	logger.Debug("cleaning podman build cache")
	if output, err := p.runPodmanCommand(ctx, "image", "prune", "--build-cache", "-f"); err == nil {
		result.BytesFreed += p.parseReclaimedSpace(output)
		result.ItemsCleaned++
	}

	return result
}

// cleanAggressive performs aggressive cleanup: moderate + volumes + VM fstrim.
func (p *PodmanPlugin) cleanAggressive(ctx context.Context, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := p.cleanModerate(ctx, cfg, logger)
	result.Level = LevelAggressive

	// Clean unused volumes
	logger.Debug("cleaning unused podman volumes")
	if output, err := p.runPodmanCommand(ctx, "volume", "prune", "-f"); err == nil {
		result.BytesFreed += p.parseReclaimedSpace(output)
		result.ItemsCleaned++
	}

	// Clean build containers (may interfere with active builds)
	logger.Debug("cleaning podman build containers")
	if output, err := p.runPodmanCommand(ctx, "system", "prune", "-f", "--build"); err == nil {
		result.BytesFreed += p.parseReclaimedSpace(output)
		result.ItemsCleaned++
	}

	// On Darwin, run fstrim inside VM to reclaim sparse disk space when the
	// provider reflects guest discard operations back to the host disk image.
	if runtime.GOOS == "darwin" && p.environment.VMRunning && cfg.Podman.TrimVMDisk {
		if !p.fstrimReclaimsHostSpace() {
			logger.Warn("skipping Podman VM fstrim accounting; provider does not shrink host disk images from guest fstrim",
				"machine", p.environment.MachineName,
				"provider", p.environment.VMProvider,
				"suggestion", "use offline compaction for actual host-side reclamation")
			return result
		}

		logger.Debug("running fstrim in Podman VM", "machine", p.environment.MachineName)
		if trimmed, err := p.trimVMDisk(ctx, logger); err == nil && trimmed > 0 {
			result.BytesFreed += trimmed
			result.ItemsCleaned++
			logger.Info("reclaimed sparse disk space from Podman VM", "freed_mb", trimmed/(1024*1024))
		} else if err != nil {
			logger.Warn("fstrim in Podman VM failed", "error", err)
		}
	}

	return result
}

// cleanCritical performs emergency cleanup: full system prune with volumes and external cleanup.
func (p *PodmanPlugin) cleanCritical(ctx context.Context, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: LevelCritical}

	// Full system prune with volumes
	logger.Warn("CRITICAL: running full Podman system prune with volumes")
	output, err := p.runPodmanCommand(ctx, "system", "prune", "-af", "--volumes")
	if err != nil {
		logger.Error("full system prune failed", "error", err)
		result.Error = err
		return result
	}
	result.BytesFreed = p.parseReclaimedSpace(output)
	result.ItemsCleaned++

	// Clean external/orphaned storage (transient mode)
	logger.Warn("CRITICAL: cleaning external podman storage")
	if output, err := p.runPodmanCommand(ctx, "system", "prune", "--external", "-f"); err == nil {
		result.BytesFreed += p.parseReclaimedSpace(output)
		result.ItemsCleaned++
	} else {
		// --external might not be supported on older versions
		logger.Debug("external storage cleanup not available", "error", err)
	}

	// On Darwin, aggressive VM cleanup
	if runtime.GOOS == "darwin" && p.environment.VMRunning {
		// First, clean inside the VM
		if cfg.Podman.CleanInsideVM {
			logger.Warn("CRITICAL: cleaning inside Podman VM")
			vmResult := p.cleanInsideVM(ctx, LevelCritical, logger)
			result.BytesFreed += vmResult.BytesFreed
			result.ItemsCleaned += vmResult.ItemsCleaned
		}

		// Then fstrim to reclaim space when the provider reports that discard
		// as host-side sparse image reclamation.
		if cfg.Podman.TrimVMDisk {
			if !p.fstrimReclaimsHostSpace() {
				logger.Warn("skipping Podman VM fstrim accounting; provider does not shrink host disk images from guest fstrim",
					"machine", p.environment.MachineName,
					"provider", p.environment.VMProvider,
					"suggestion", "use offline compaction for actual host-side reclamation")
			} else if trimmed, err := p.trimVMDisk(ctx, logger); err == nil && trimmed > 0 {
				result.BytesFreed += trimmed
				result.ItemsCleaned++
			} else if err != nil {
				logger.Warn("fstrim in Podman VM failed", "error", err)
			}
		}

		// Offline disk compaction (opt-in only)
		if cfg.Podman.CompactDiskOffline {
			compactFreed, err := p.compactRawDisk(ctx, cfg, logger)
			if err != nil {
				logger.Warn("Podman disk compaction failed", "error", err)
			} else if compactFreed > 0 {
				result.BytesFreed += compactFreed
				result.HostBytesFreed += compactFreed
				result.ItemsCleaned++
			}
		} else if p.environment.VMProvider == "qemu" {
			logger.Warn("CRITICAL: qcow2 disk may benefit from offline compaction",
				"suggestion", "enable podman.compact_disk_offline in config")
		}
	}

	return result
}

// runPodmanCommand executes a podman command with timeout.
func (p *PodmanPlugin) runPodmanCommand(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "podman", args...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// fstrimReclaimsHostSpace reports whether guest fstrim output can be counted
// as host bytes freed for the detected Podman machine provider.
func (p *PodmanPlugin) fstrimReclaimsHostSpace() bool {
	if runtime.GOOS != "darwin" || p.environment == nil {
		return true
	}

	// Podman on macOS with applehv stores the VM in a raw sparse file. Guest
	// fstrim reports large byte counts from inside the VM, but the raw file's
	// host allocation does not shrink unless a separate compaction pass punches
	// holes or rewrites the image. Counting those bytes as freed makes the
	// daemon report huge false-positive cleanup totals every critical cycle.
	return p.environment.VMProvider != "applehv"
}

// parseReclaimedSpace extracts bytes freed from podman output.
func (p *PodmanPlugin) parseReclaimedSpace(output string) int64 {
	// Parse "Total reclaimed space: X.XXY" or similar patterns
	// Podman uses same format as Docker
	patterns := []string{
		`reclaimed space:\s*([\d.]+)\s*([KMGT]?i?B)`,
		`Total reclaimed space:\s*([\d.]+)\s*([KMGT]?i?B)`,
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(output)
		if len(matches) >= 3 {
			value, err := strconv.ParseFloat(matches[1], 64)
			if err != nil {
				continue
			}

			unit := strings.ToUpper(matches[2])
			switch {
			case strings.HasPrefix(unit, "K"):
				return int64(value * 1024)
			case strings.HasPrefix(unit, "M"):
				return int64(value * 1024 * 1024)
			case strings.HasPrefix(unit, "G"):
				return int64(value * 1024 * 1024 * 1024)
			case strings.HasPrefix(unit, "T"):
				return int64(value * 1024 * 1024 * 1024 * 1024)
			default:
				return int64(value)
			}
		}
	}

	return 0
}

// detectPodmanEnvironment detects the Podman runtime environment.
func detectPodmanEnvironment() (*PodmanEnvironment, error) {
	env := &PodmanEnvironment{}

	// Check if podman CLI is available
	if _, err := exec.LookPath("podman"); err != nil {
		return env, nil
	}

	// Verify podman is functional
	cmd := exec.Command("podman", "info", "--format", "{{.Version.Version}}")
	if err := cmd.Run(); err != nil {
		return env, nil
	}
	env.Runtime = "podman"

	// Platform-specific detection
	switch runtime.GOOS {
	case "darwin":
		env.NeedsVM = true
		env.VMProvider = detectMachineProvider()
		env.VMRunning, env.MachineName = detectRunningMachine()
		if env.VMRunning {
			env.SocketPath = getPodmanSocket()
		}
	case "linux":
		env.NeedsVM = false
		home, _ := os.UserHomeDir()
		env.StoragePath = filepath.Join(home, ".local/share/containers/storage")
		env.SocketPath = getPodmanSocket()
	}

	return env, nil
}

// detectMachineProvider detects the Podman machine virtualization provider.
func detectMachineProvider() string {
	// Check environment variable first
	if provider := os.Getenv("CONTAINERS_MACHINE_PROVIDER"); provider != "" {
		return provider
	}

	// Check containers.conf
	home, _ := os.UserHomeDir()
	configPath := filepath.Join(home, ".config/containers/containers.conf")
	if data, err := os.ReadFile(configPath); err == nil {
		re := regexp.MustCompile(`provider\s*=\s*"([^"]+)"`)
		if matches := re.FindStringSubmatch(string(data)); len(matches) > 1 {
			return matches[1]
		}
	}

	// Default on modern macOS
	return "applehv"
}

// detectRunningMachine detects if a Podman machine is running and returns its name.
func detectRunningMachine() (bool, string) {
	cmd := exec.Command("podman", "machine", "list", "--format", "{{.Name}}\t{{.Running}}")
	output, err := cmd.Output()
	if err != nil {
		return false, ""
	}

	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) >= 2 && strings.ToLower(parts[1]) == "true" {
			// Strip trailing "*" which marks the default machine
			name := strings.TrimRight(parts[0], "*")
			return true, name
		}
	}

	return false, ""
}

// getPodmanSocket returns the Podman socket path.
func getPodmanSocket() string {
	// Check DOCKER_HOST (often set to podman socket)
	if dockerHost := os.Getenv("DOCKER_HOST"); strings.Contains(dockerHost, "podman") {
		return strings.TrimPrefix(dockerHost, "unix://")
	}

	// Check XDG_RUNTIME_DIR (Linux)
	if xdgRuntime := os.Getenv("XDG_RUNTIME_DIR"); xdgRuntime != "" {
		socketPath := filepath.Join(xdgRuntime, "podman/podman.sock")
		if _, err := os.Stat(socketPath); err == nil {
			return socketPath
		}
	}

	// Default locations
	home, _ := os.UserHomeDir()
	locations := []string{
		filepath.Join(home, ".local/share/containers/podman/machine/podman.sock"),
		"/run/podman/podman.sock",
		"/var/run/podman/podman.sock",
	}

	for _, loc := range locations {
		if _, err := os.Stat(loc); err == nil {
			return loc
		}
	}

	return ""
}

// trimVMDisk runs fstrim inside the Podman VM to reclaim sparse disk space.
// This is only applicable on Darwin where Podman uses a VM.
func (p *PodmanPlugin) trimVMDisk(ctx context.Context, logger *slog.Logger) (int64, error) {
	if !p.environment.VMRunning || p.environment.MachineName == "" {
		return 0, nil
	}

	cmd := exec.CommandContext(ctx, "podman", "machine", "ssh",
		p.environment.MachineName, "--", "sudo", "fstrim", "-av")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("fstrim failed: %w", err)
	}

	return parseFstrimOutput(string(output)), nil
}

// parseFstrimOutput extracts bytes trimmed from fstrim output.
func parseFstrimOutput(output string) int64 {
	// Parse output like "/: 1.5 GiB (1610612736 bytes) trimmed"
	re := regexp.MustCompile(`\((\d+) bytes\) trimmed`)
	var total int64
	for _, match := range re.FindAllStringSubmatch(output, -1) {
		if len(match) >= 2 {
			if bytes, err := strconv.ParseInt(match[1], 10, 64); err == nil {
				total += bytes
			}
		}
	}
	return total
}

func (p *PodmanPlugin) planOfflineCompaction(ctx context.Context, cfg *config.Config, logger *slog.Logger) podmanCompactionPlan {
	qemuImgPath, qemuImgAvailable := resolveQemuImgPath(cfg.Podman.CompactQemuImgPath)
	input := podmanCompactionPlanInput{
		MachineName:      p.environment.MachineName,
		Provider:         p.environment.VMProvider,
		ConfigEnabled:    cfg.Podman.CompactDiskOffline,
		QemuImgPath:      qemuImgPath,
		QemuImgAvailable: qemuImgAvailable,
		Config:           cfg.Podman,
	}

	diskPath, err := p.getMachineDiskPath(ctx)
	if err != nil {
		logger.Debug("Podman disk path preflight failed", "error", err)
		input.DiskPathExpected = false
		plan := buildPodmanCompactionPlan(input)
		plan.SkipReason = "disk_path_unavailable"
		plan.Warnings = append(plan.Warnings, err.Error())
		return plan
	}
	input.DiskPath = diskPath
	input.DiskPathExpected = expectedPodmanMachineDiskPath(diskPath)
	input.BackupExists = pathExists(diskPath + ".backup")
	input.ScratchDir = filepath.Dir(diskPath)
	input.ScratchDirAvailable = true

	if stat, err := os.Stat(diskPath); err == nil {
		input.LogicalBytes = stat.Size()
	} else {
		logger.Debug("Podman disk stat preflight failed", "disk", diskPath, "error", err)
		plan := buildPodmanCompactionPlan(input)
		plan.SkipReason = "disk_stat_failed"
		plan.Warnings = append(plan.Warnings, err.Error())
		return plan
	}

	if allocated, err := getFileAllocatedBytes(diskPath); err == nil {
		input.PhysicalBytes = allocated
	} else {
		logger.Debug("Podman disk allocation preflight failed", "disk", diskPath, "error", err)
		input.PhysicalBytes = input.LogicalBytes
	}

	if configuredScratchDir := strings.TrimSpace(cfg.Podman.CompactScratchDir); configuredScratchDir != "" {
		home, _ := os.UserHomeDir()
		input.ScratchDirConfigured = true
		input.ScratchDir = filepath.Clean(expandHome(configuredScratchDir, home))
		stat, err := os.Stat(input.ScratchDir)
		if err != nil {
			logger.Debug("Podman compaction scratch directory preflight failed", "scratch_dir", input.ScratchDir, "error", err)
			input.ScratchDirAvailable = false
			plan := buildPodmanCompactionPlan(input)
			plan.SkipReason = "scratch_dir_unavailable"
			plan.Warnings = append(plan.Warnings, err.Error())
			return plan
		}
		if !stat.IsDir() {
			logger.Debug("Podman compaction scratch path is not a directory", "scratch_dir", input.ScratchDir)
			input.ScratchDirAvailable = false
			plan := buildPodmanCompactionPlan(input)
			plan.SkipReason = "scratch_dir_not_directory"
			return plan
		}
	}

	if diskDevice, err := deviceID(filepath.Dir(diskPath)); err == nil {
		if scratchDevice, err := deviceID(input.ScratchDir); err == nil {
			input.ScratchDirCrossDevice = diskDevice != scratchDevice
		}
	}

	if free, err := getFreeDiskSpace(input.ScratchDir); err == nil {
		input.FreeBytes = int64FromUint64(free)
	} else {
		logger.Debug("Podman disk free-space preflight failed", "scratch_dir", input.ScratchDir, "error", err)
		plan := buildPodmanCompactionPlan(input)
		plan.SkipReason = "free_space_check_failed"
		plan.Warnings = append(plan.Warnings, err.Error())
		return plan
	}

	if cfg.Podman.CompactRequireNoActiveContainers {
		active, err := p.hasActiveContainers(ctx)
		input.ActiveContainers = active
		if err != nil {
			input.ActiveContainerCheckError = err.Error()
		}
	}

	return buildPodmanCompactionPlan(input)
}

func buildPodmanCompactionPlan(input podmanCompactionPlanInput) podmanCompactionPlan {
	diskFormat, supportedProvider := podmanDiskFormat(input.Provider)
	scratchDir := input.ScratchDir
	tempPath := ""
	backupPath := ""
	if input.DiskPath != "" {
		if scratchDir == "" {
			scratchDir = filepath.Dir(input.DiskPath)
		}
		tempPath = filepath.Join(scratchDir, filepath.Base(input.DiskPath)+".compact")
		backupPath = input.DiskPath + ".backup"
	}
	scratchDirAvailable := input.ScratchDirAvailable || (!input.ScratchDirConfigured && scratchDir != "")
	qemuImgPath := input.QemuImgPath
	if qemuImgPath == "" {
		qemuImgPath = "qemu-img"
	}

	physicalBytes := input.PhysicalBytes
	if physicalBytes <= 0 {
		physicalBytes = input.LogicalBytes
	}

	requiredFreeBytes := podmanCompactionRequiredFreeBytes(physicalBytes)
	minReclaimBytes := int64(input.Config.CompactMinReclaimGB) * podmanCompactionGiB
	estimatedReclaimBytes := physicalBytes
	if minReclaimBytes > 0 && estimatedReclaimBytes > 0 && estimatedReclaimBytes > minReclaimBytes {
		estimatedReclaimBytes -= minReclaimBytes
	} else {
		estimatedReclaimBytes = 0
	}

	plan := podmanCompactionPlan{
		MachineName:               input.MachineName,
		Provider:                  input.Provider,
		DiskFormat:                diskFormat,
		DiskPath:                  input.DiskPath,
		ScratchDir:                scratchDir,
		TempPath:                  tempPath,
		BackupPath:                backupPath,
		ConfigEnabled:             input.ConfigEnabled,
		QemuImgPath:               qemuImgPath,
		QemuImgAvailable:          input.QemuImgAvailable,
		ActiveContainers:          input.ActiveContainers,
		ActiveContainerCheckError: input.ActiveContainerCheckError,
		DiskPathExpected:          input.DiskPathExpected,
		BackupExists:              input.BackupExists,
		ScratchDirConfigured:      input.ScratchDirConfigured,
		ScratchDirAvailable:       scratchDirAvailable,
		ScratchDirCrossDevice:     input.ScratchDirCrossDevice,
		LogicalBytes:              input.LogicalBytes,
		PhysicalBytes:             physicalBytes,
		FreeBytes:                 input.FreeBytes,
		RequiredFreeBytes:         requiredFreeBytes,
		EstimatedReclaimBytes:     estimatedReclaimBytes,
		Steps: []string{
			fmt.Sprintf("Inspect Podman machine %q disk metadata", input.MachineName),
			fmt.Sprintf("Check offline compaction scratch space at %s", scratchDir),
			"Confirm no active containers are running",
			fmt.Sprintf("Stop Podman machine %q", input.MachineName),
			fmt.Sprintf("Convert %s to %s with %s convert -f %s -O %s", input.DiskPath, tempPath, qemuImgPath, diskFormat, diskFormat),
			"Verify the compacted image before replacing the original",
			"Preserve the original disk image as rollback backup until restart succeeds",
			fmt.Sprintf("Replace %s with compacted image", input.DiskPath),
			fmt.Sprintf("Start Podman machine %q and verify it boots", input.MachineName),
		},
	}

	if input.Provider == "applehv" {
		plan.Warnings = append(plan.Warnings, "applehv/raw guest fstrim does not prove host APFS allocation was reclaimed")
	}
	if input.Config.CompactMinReclaimGB > 0 {
		plan.Warnings = append(plan.Warnings,
			fmt.Sprintf("offline compaction is skipped below %dGiB physical allocation", input.Config.CompactMinReclaimGB))
	}

	switch {
	case !input.ConfigEnabled:
		plan.SkipReason = "compact_disk_offline_disabled"
	case input.MachineName == "":
		plan.SkipReason = "machine_unknown"
	case input.DiskPath == "":
		plan.SkipReason = "disk_path_unavailable"
	case !input.DiskPathExpected:
		plan.SkipReason = "disk_path_outside_podman_machine_dirs"
	case input.BackupExists:
		plan.SkipReason = "backup_path_exists"
	case !supportedProvider:
		plan.SkipReason = "unsupported_provider"
	case !providerAllowed(input.Provider, input.Config.CompactProviderAllowlist):
		plan.SkipReason = "provider_not_allowlisted"
	case input.ActiveContainerCheckError != "":
		plan.SkipReason = "active_container_check_failed"
	case input.Config.CompactRequireNoActiveContainers && input.ActiveContainers:
		plan.SkipReason = "active_containers"
	case !input.QemuImgAvailable:
		plan.SkipReason = "qemu_img_missing"
	case !scratchDirAvailable:
		plan.SkipReason = "scratch_dir_unavailable"
	case input.ScratchDirConfigured && input.ScratchDirCrossDevice:
		plan.SkipReason = "scratch_dir_cross_device_replace_unsupported"
	case physicalBytes <= 0:
		plan.SkipReason = "physical_size_unknown"
	case minReclaimBytes > 0 && physicalBytes < minReclaimBytes:
		plan.SkipReason = "below_minimum_physical_allocation"
	case input.FreeBytes < requiredFreeBytes:
		plan.SkipReason = "insufficient_free_space"
	default:
		plan.CanCompact = true
	}

	return plan
}

func podmanCompactionTargets(plan podmanCompactionPlan) []CleanupTarget {
	if plan.DiskPath == "" && plan.RequiredFreeBytes == 0 && !plan.ActiveContainers {
		return nil
	}

	var targets []CleanupTarget
	if plan.DiskPath != "" {
		action := "compact_disk_offline"
		reclaim := CleanupReclaimHost
		reason := "offline compaction is eligible and expected to reclaim host allocation"
		if !plan.CanCompact {
			action = "protect_offline_compaction"
			reclaim = CleanupReclaimNone
			reason = podmanCompactionSkipReason(plan.SkipReason)
		}
		target := CleanupTarget{
			Type:         "podman_vm_disk",
			Tier:         CleanupTierDisruptive,
			Name:         plan.MachineName,
			Path:         plan.DiskPath,
			Bytes:        plan.EstimatedReclaimBytes,
			LogicalBytes: plan.LogicalBytes,
			Active:       plan.ActiveContainers,
			Protected:    !plan.CanCompact,
			Action:       action,
			Reason:       reason,
		}
		annotateCleanupTargetPolicy(&target, target.Tier, reclaim)
		targets = append(targets, target)
	}

	if plan.RequiredFreeBytes > 0 {
		action := "review_required_free_space"
		reason := "offline compaction needs temporary scratch space for the compacted image"
		scratchPath := plan.ScratchDir
		if scratchPath == "" && plan.DiskPath != "" {
			scratchPath = filepath.Dir(plan.DiskPath)
		}
		if plan.SkipReason == "insufficient_free_space" {
			action = "protect_insufficient_free_space"
			reason = "not enough free space is available in the offline compaction scratch directory"
		} else if plan.SkipReason == "scratch_dir_unavailable" {
			action = "protect_scratch_dir_unavailable"
			reason = "configured offline compaction scratch directory is unavailable"
		} else if plan.SkipReason == "scratch_dir_not_directory" {
			action = "protect_scratch_dir_unavailable"
			reason = "configured offline compaction scratch path is not a directory"
		} else if plan.SkipReason == "scratch_dir_cross_device_replace_unsupported" {
			action = "protect_cross_device_scratch"
			reason = "configured scratch directory is on a different filesystem than the VM disk"
		}
		target := CleanupTarget{
			Type:      "podman_compaction_scratch",
			Tier:      CleanupTierDisruptive,
			Name:      "offline compaction scratch space",
			Path:      scratchPath,
			Bytes:     plan.RequiredFreeBytes,
			Protected: true,
			Action:    action,
			Reason:    reason,
		}
		annotateCleanupTargetPolicy(&target, target.Tier, CleanupReclaimNone)
		targets = append(targets, target)
	}

	if plan.ActiveContainers || plan.ActiveContainerCheckError != "" {
		action := "protect_active_containers"
		active := plan.ActiveContainers
		reason := "active Podman containers must be quiesced before offline compaction"
		if plan.ActiveContainerCheckError != "" {
			action = "protect_container_inspection"
			reason = fmt.Sprintf("could not inspect active Podman containers: %s", plan.ActiveContainerCheckError)
		}
		target := CleanupTarget{
			Type:      "podman_active_containers",
			Tier:      CleanupTierDisruptive,
			Name:      "active Podman containers",
			Active:    active,
			Protected: true,
			Action:    action,
			Reason:    reason,
		}
		annotateCleanupTargetPolicy(&target, target.Tier, CleanupReclaimNone)
		targets = append(targets, target)
	}

	return targets
}

func podmanCompactionSkipReason(reason string) string {
	switch reason {
	case "":
		return "offline compaction is not eligible"
	case "compact_disk_offline_disabled":
		return "offline compaction is disabled by config"
	case "active_containers":
		return "active Podman containers must be stopped before offline compaction"
	case "insufficient_free_space":
		return "not enough scratch free space is available for offline compaction"
	case "qemu_img_missing":
		return "qemu-img is required for offline compaction"
	case "scratch_dir_unavailable":
		return "configured offline compaction scratch directory is unavailable"
	case "scratch_dir_not_directory":
		return "configured offline compaction scratch path is not a directory"
	case "scratch_dir_cross_device_replace_unsupported":
		return "configured offline compaction scratch directory is on a different filesystem than the VM disk"
	case "backup_path_exists":
		return "existing rollback backup must be resolved before offline compaction"
	case "disk_path_outside_podman_machine_dirs":
		return "VM disk path is outside expected Podman machine directories"
	case "provider_not_allowlisted":
		return "VM provider is not allowlisted for offline compaction"
	case "unsupported_provider":
		return "VM provider is not supported for offline compaction"
	case "below_minimum_physical_allocation":
		return "VM disk physical allocation is below the configured compaction threshold"
	default:
		return "offline compaction preflight blocked compaction: " + reason
	}
}

func podmanDiskFormat(provider string) (string, bool) {
	switch provider {
	case "applehv", "libkrun":
		return "raw", true
	case "qemu":
		return "qcow2", true
	default:
		return "", false
	}
}

func providerAllowed(provider string, allowlist []string) bool {
	if len(allowlist) == 0 {
		return true
	}
	for _, allowed := range allowlist {
		if provider == allowed {
			return true
		}
	}
	return false
}

func podmanCompactionRequiredFreeBytes(physicalBytes int64) int64 {
	if physicalBytes <= 0 {
		return podmanCompactionGiB
	}

	headroom := physicalBytes / 10
	if headroom < podmanCompactionGiB {
		headroom = podmanCompactionGiB
	}
	return physicalBytes + headroom
}

func expectedPodmanMachineDiskPath(path string) bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}

	roots := []string{
		filepath.Join(home, ".local/share/containers/podman/machine"),
		filepath.Join(home, ".config/containers/podman/machine"),
	}
	return pathWithinRoots(path, roots)
}

func pathWithinRoots(path string, roots []string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}

	for _, root := range roots {
		absRoot, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(absRoot, absPath)
		if err != nil {
			continue
		}
		if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			continue
		}
		return true
	}
	return false
}

func (p *PodmanPlugin) hasActiveContainers(ctx context.Context) (bool, error) {
	output, err := p.runPodmanCommand(ctx, "ps", "-q")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) != "", nil
}

func resolveQemuImgPath(configuredPath string) (string, bool) {
	if strings.TrimSpace(configuredPath) != "" {
		home, _ := os.UserHomeDir()
		path := filepath.Clean(expandHome(strings.TrimSpace(configuredPath), home))
		info, err := os.Stat(path)
		return path, err == nil && !info.IsDir()
	}

	path, err := exec.LookPath("qemu-img")
	if err != nil {
		return "qemu-img", false
	}
	return path, true
}

func int64FromUint64(value uint64) int64 {
	maxInt64 := uint64(^uint64(0) >> 1)
	if value > maxInt64 {
		return int64(maxInt64)
	}
	return int64(value)
}

func restorePodmanDiskBackup(diskPath, backupPath string) error {
	if pathExists(diskPath) {
		failedPath := diskPath + ".failed"
		if err := os.Rename(diskPath, failedPath); err != nil {
			return err
		}
	}
	return os.Rename(backupPath, diskPath)
}

// compactRawDisk performs offline disk compaction for the Podman machine VM.
// For raw disk images (applehv, libkrun): creates a sparse copy via qemu-img.
// For qcow2 (qemu): converts to reclaim space.
// ONLY runs at Critical level with explicit opt-in via config.
func (p *PodmanPlugin) compactRawDisk(ctx context.Context, cfg *config.Config, logger *slog.Logger) (int64, error) {
	if !p.environment.VMRunning || p.environment.MachineName == "" {
		return 0, nil
	}

	plan := p.planOfflineCompaction(ctx, cfg, logger)
	if !plan.CanCompact {
		logger.Warn("skipping Podman disk compaction",
			"machine", plan.MachineName,
			"provider", plan.Provider,
			"reason", plan.SkipReason)
		return 0, nil
	}

	logger.Warn("CRITICAL: stopping Podman machine for disk compaction",
		"machine", p.environment.MachineName,
		"format", plan.DiskFormat,
		"logical_gb", fmt.Sprintf("%.1f", float64(plan.LogicalBytes)/float64(podmanCompactionGiB)),
		"physical_gb", fmt.Sprintf("%.1f", float64(plan.PhysicalBytes)/float64(podmanCompactionGiB)),
		"required_free_gb", fmt.Sprintf("%.1f", float64(plan.RequiredFreeBytes)/float64(podmanCompactionGiB)))

	// 1. Stop machine
	stopCmd := exec.CommandContext(ctx, "podman", "machine", "stop", p.environment.MachineName)
	if output, err := stopCmd.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("failed to stop machine: %w (output: %s)", err, string(output))
	}
	p.environment.VMRunning = false

	// 2. Convert to sparse copy
	logger.Info("compacting Podman machine disk", "machine", p.environment.MachineName)
	qemuImgPath := plan.QemuImgPath
	if qemuImgPath == "" {
		qemuImgPath = "qemu-img"
	}
	convertCmd := exec.CommandContext(ctx, qemuImgPath, "convert",
		"-f", plan.DiskFormat, "-O", plan.DiskFormat, plan.DiskPath, plan.TempPath)
	if output, err := convertCmd.CombinedOutput(); err != nil {
		os.Remove(plan.TempPath)
		// Restart machine before returning
		exec.CommandContext(ctx, "podman", "machine", "start", p.environment.MachineName).Run()
		p.environment.VMRunning = true
		return 0, fmt.Errorf("qemu-img convert failed: %w (output: %s)", err, string(output))
	}

	// 3. Verify if qcow2 format
	if plan.DiskFormat == "qcow2" {
		checkCmd := exec.CommandContext(ctx, qemuImgPath, "check", plan.TempPath)
		if output, err := checkCmd.CombinedOutput(); err != nil {
			os.Remove(plan.TempPath)
			exec.CommandContext(ctx, "podman", "machine", "start", p.environment.MachineName).Run()
			p.environment.VMRunning = true
			return 0, fmt.Errorf("qemu-img check failed: %w (output: %s)", err, string(output))
		}
	}

	// 4. Get compacted size
	sparseStat, err := os.Stat(plan.TempPath)
	if err != nil {
		os.Remove(plan.TempPath)
		exec.CommandContext(ctx, "podman", "machine", "start", p.environment.MachineName).Run()
		p.environment.VMRunning = true
		return 0, fmt.Errorf("cannot stat compacted disk: %w", err)
	}
	physicalAfter, err := getFileAllocatedBytes(plan.TempPath)
	if err != nil {
		os.Remove(plan.TempPath)
		exec.CommandContext(ctx, "podman", "machine", "start", p.environment.MachineName).Run()
		p.environment.VMRunning = true
		return 0, fmt.Errorf("cannot stat compacted disk allocation: %w", err)
	}

	// 5. Replace original
	if cfg.Podman.CompactKeepBackupUntilRestart {
		if err := os.Rename(plan.DiskPath, plan.BackupPath); err != nil {
			os.Remove(plan.TempPath)
			exec.CommandContext(ctx, "podman", "machine", "start", p.environment.MachineName).Run()
			p.environment.VMRunning = true
			return 0, fmt.Errorf("failed to preserve original disk backup: %w", err)
		}
		if err := os.Rename(plan.TempPath, plan.DiskPath); err != nil {
			restoreErr := os.Rename(plan.BackupPath, plan.DiskPath)
			os.Remove(plan.TempPath)
			exec.CommandContext(ctx, "podman", "machine", "start", p.environment.MachineName).Run()
			p.environment.VMRunning = true
			if restoreErr != nil {
				return 0, fmt.Errorf("failed to replace disk and restore backup: replace=%w restore=%v", err, restoreErr)
			}
			return 0, fmt.Errorf("failed to replace disk: %w", err)
		}
	} else if err := os.Rename(plan.TempPath, plan.DiskPath); err != nil {
		os.Remove(plan.TempPath)
		exec.CommandContext(ctx, "podman", "machine", "start", p.environment.MachineName).Run()
		p.environment.VMRunning = true
		return 0, fmt.Errorf("failed to replace disk: %w", err)
	}

	// 6. Restart machine
	logger.Info("restarting Podman machine after compaction", "machine", p.environment.MachineName)
	startCmd := exec.CommandContext(ctx, "podman", "machine", "start", p.environment.MachineName)
	if output, err := startCmd.CombinedOutput(); err != nil {
		logger.Error("failed to restart machine after compaction",
			"machine", p.environment.MachineName, "error", err, "output", string(output))
		if cfg.Podman.CompactKeepBackupUntilRestart {
			if restoreErr := restorePodmanDiskBackup(plan.DiskPath, plan.BackupPath); restoreErr != nil {
				return 0, fmt.Errorf("failed to restart machine after compaction and restore backup: restart=%w restore=%v", err, restoreErr)
			}
			exec.CommandContext(ctx, "podman", "machine", "start", p.environment.MachineName).Run()
		}
		p.environment.VMRunning = true
		return 0, fmt.Errorf("failed to restart machine after compaction: %w", err)
	}
	p.environment.VMRunning = true

	if cfg.Podman.CompactKeepBackupUntilRestart {
		if err := os.Remove(plan.BackupPath); err != nil {
			return 0, fmt.Errorf("compacted disk verified but backup remains at %s: %w", plan.BackupPath, err)
		}
	}

	freed := safeBytesDiff(plan.PhysicalBytes, physicalAfter)
	if freed > 0 {
		logger.Info("Podman disk compaction complete",
			"machine", p.environment.MachineName,
			"freed_gb", fmt.Sprintf("%.1f", float64(freed)/float64(podmanCompactionGiB)),
			"logical_before_gb", fmt.Sprintf("%.1f", float64(plan.LogicalBytes)/float64(podmanCompactionGiB)),
			"physical_before_gb", fmt.Sprintf("%.1f", float64(plan.PhysicalBytes)/float64(podmanCompactionGiB)),
			"logical_after_gb", fmt.Sprintf("%.1f", float64(sparseStat.Size())/float64(podmanCompactionGiB)),
			"physical_after_gb", fmt.Sprintf("%.1f", float64(physicalAfter)/float64(podmanCompactionGiB)),
		)
		return freed, nil
	}

	return 0, nil
}

// getMachineDiskPath extracts the disk image path from podman machine config.
func (p *PodmanPlugin) getMachineDiskPath(ctx context.Context) (string, error) {
	// Strategy 1: Try podman machine inspect for ImagePath/DiskPath (older Podman)
	cmd := exec.CommandContext(ctx, "podman", "machine", "inspect", p.environment.MachineName)
	if output, err := cmd.Output(); err == nil {
		outputStr := string(output)
		// Check for simple string value: "ImagePath": "/path/to/disk"
		for _, key := range []string{"ImagePath", "DiskPath"} {
			re := regexp.MustCompile(fmt.Sprintf(`"%s"\s*:\s*"([^"]+)"`, key))
			if matches := re.FindStringSubmatch(outputStr); len(matches) > 1 {
				return matches[1], nil
			}
		}

		// Extract ConfigDir for strategy 2
		configDirRe := regexp.MustCompile(`"ConfigDir"\s*:\s*\{\s*"Path"\s*:\s*"([^"]+)"`)
		if matches := configDirRe.FindStringSubmatch(outputStr); len(matches) > 1 {
			configDir := matches[1]
			return p.readDiskPathFromConfig(configDir)
		}
	}

	// Strategy 2: Read internal config JSON from known provider paths
	home, _ := os.UserHomeDir()
	providers := []string{"libkrun", "applehv", "qemu"}
	for _, provider := range providers {
		configDir := filepath.Join(home, ".config/containers/podman/machine", provider)
		if path, err := p.readDiskPathFromConfig(configDir); err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("disk path not found in machine config")
}

// readDiskPathFromConfig reads the disk image path from a machine config JSON file.
func (p *PodmanPlugin) readDiskPathFromConfig(configDir string) (string, error) {
	configFile := filepath.Join(configDir, p.environment.MachineName+".json")
	data, err := os.ReadFile(configFile)
	if err != nil {
		return "", err
	}

	// Parse nested ImagePath: {"Path": "/path/to/disk.raw"}
	re := regexp.MustCompile(`"ImagePath"\s*:\s*\{\s*"Path"\s*:\s*"([^"]+)"`)
	if matches := re.FindStringSubmatch(string(data)); len(matches) > 1 {
		return matches[1], nil
	}

	return "", fmt.Errorf("ImagePath not found in %s", configFile)
}

// cleanInsideVM runs cleanup commands inside the Podman VM.
func (p *PodmanPlugin) cleanInsideVM(ctx context.Context, level CleanupLevel, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name() + "-vm", Level: level}

	if !p.environment.VMRunning || p.environment.MachineName == "" {
		return result
	}

	var commands [][]string

	switch level {
	case LevelWarning:
		commands = [][]string{
			{"podman", "image", "prune", "-f"},
		}
	case LevelModerate:
		commands = [][]string{
			{"podman", "image", "prune", "-f"},
			{"podman", "container", "prune", "-f"},
			{"podman", "image", "prune", "--build-cache", "-f"},
		}
	case LevelAggressive:
		commands = [][]string{
			{"podman", "image", "prune", "-af"},
			{"podman", "container", "prune", "-f"},
			{"podman", "volume", "prune", "-f"},
			{"podman", "image", "prune", "--build-cache", "-f"},
		}
	case LevelCritical:
		commands = [][]string{
			{"podman", "system", "prune", "-af", "--volumes"},
		}
	}

	for _, args := range commands {
		cmd := exec.CommandContext(ctx, "podman",
			append([]string{"machine", "ssh", p.environment.MachineName, "--"}, args...)...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			logger.Debug("VM cleanup command failed", "args", args, "error", err)
			continue
		}
		freed := p.parseReclaimedSpace(string(output))
		if freed > 0 {
			result.BytesFreed += freed
			result.ItemsCleaned++
		}
	}

	return result
}
