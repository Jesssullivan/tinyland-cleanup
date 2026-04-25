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
			compactFreed, err := p.compactRawDisk(ctx, logger)
			if err != nil {
				logger.Warn("Podman disk compaction failed", "error", err)
			} else if compactFreed > 0 {
				result.BytesFreed += compactFreed
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

// compactRawDisk performs offline disk compaction for the Podman machine VM.
// For raw disk images (applehv, libkrun): creates a sparse copy via qemu-img.
// For qcow2 (qemu): converts to reclaim space.
// ONLY runs at Critical level with explicit opt-in via config.
func (p *PodmanPlugin) compactRawDisk(ctx context.Context, logger *slog.Logger) (int64, error) {
	if !p.environment.VMRunning || p.environment.MachineName == "" {
		return 0, nil
	}

	// Check if qemu-img is available
	if _, err := exec.LookPath("qemu-img"); err != nil {
		return 0, fmt.Errorf("qemu-img not available: %w", err)
	}

	// Get raw disk file path from podman machine inspect
	diskPath, err := p.getMachineDiskPath(ctx)
	if err != nil {
		return 0, fmt.Errorf("cannot determine disk path: %w", err)
	}
	if diskPath == "" {
		return 0, fmt.Errorf("empty disk path for machine %s", p.environment.MachineName)
	}

	// Get current size
	stat, err := os.Stat(diskPath)
	if err != nil {
		return 0, fmt.Errorf("cannot stat disk: %w", err)
	}
	sizeBefore := stat.Size()

	// Safety check: ensure enough free space for the temporary copy.
	// qemu-img convert writes a full copy before we can remove the original,
	// so we need at least the disk image size in free space.
	diskDir := filepath.Dir(diskPath)
	freeSpace, err := getFreeDiskSpace(diskDir)
	if err != nil {
		return 0, fmt.Errorf("cannot check free space: %w", err)
	}
	if freeSpace < uint64(sizeBefore) {
		logger.Warn("skipping Podman disk compaction: insufficient free space",
			"disk_size_gb", fmt.Sprintf("%.1f", float64(sizeBefore)/(1024*1024*1024)),
			"free_gb", fmt.Sprintf("%.1f", float64(freeSpace)/(1024*1024*1024)))
		return 0, nil
	}

	// Determine disk format based on provider
	var diskFormat string
	switch p.environment.VMProvider {
	case "applehv", "libkrun":
		diskFormat = "raw"
	case "qemu":
		diskFormat = "qcow2"
	default:
		return 0, fmt.Errorf("unsupported VM provider for compaction: %s", p.environment.VMProvider)
	}

	sparsePath := diskPath + ".sparse"

	logger.Warn("CRITICAL: stopping Podman machine for disk compaction",
		"machine", p.environment.MachineName, "format", diskFormat)

	// 1. Stop machine
	stopCmd := exec.CommandContext(ctx, "podman", "machine", "stop", p.environment.MachineName)
	if output, err := stopCmd.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("failed to stop machine: %w (output: %s)", err, string(output))
	}
	p.environment.VMRunning = false

	// 2. Convert to sparse copy
	logger.Info("compacting Podman machine disk", "machine", p.environment.MachineName)
	convertCmd := exec.CommandContext(ctx, "qemu-img", "convert",
		"-f", diskFormat, "-O", diskFormat, diskPath, sparsePath)
	if output, err := convertCmd.CombinedOutput(); err != nil {
		os.Remove(sparsePath)
		// Restart machine before returning
		exec.CommandContext(ctx, "podman", "machine", "start", p.environment.MachineName).Run()
		p.environment.VMRunning = true
		return 0, fmt.Errorf("qemu-img convert failed: %w (output: %s)", err, string(output))
	}

	// 3. Verify if qcow2 format
	if diskFormat == "qcow2" {
		checkCmd := exec.CommandContext(ctx, "qemu-img", "check", sparsePath)
		if output, err := checkCmd.CombinedOutput(); err != nil {
			os.Remove(sparsePath)
			exec.CommandContext(ctx, "podman", "machine", "start", p.environment.MachineName).Run()
			p.environment.VMRunning = true
			return 0, fmt.Errorf("qemu-img check failed: %w (output: %s)", err, string(output))
		}
	}

	// 4. Get compacted size
	sparseStat, err := os.Stat(sparsePath)
	if err != nil {
		os.Remove(sparsePath)
		exec.CommandContext(ctx, "podman", "machine", "start", p.environment.MachineName).Run()
		p.environment.VMRunning = true
		return 0, fmt.Errorf("cannot stat compacted disk: %w", err)
	}

	// 5. Replace original
	if err := os.Rename(sparsePath, diskPath); err != nil {
		os.Remove(sparsePath)
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
	}
	p.environment.VMRunning = true

	freed := sizeBefore - sparseStat.Size()
	if freed > 0 {
		logger.Info("Podman disk compaction complete",
			"machine", p.environment.MachineName,
			"freed_gb", fmt.Sprintf("%.1f", float64(freed)/(1024*1024*1024)),
			"before_gb", fmt.Sprintf("%.1f", float64(sizeBefore)/(1024*1024*1024)),
			"after_gb", fmt.Sprintf("%.1f", float64(sparseStat.Size())/(1024*1024*1024)),
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
