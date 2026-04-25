// Package plugins provides cleanup plugin implementations.
package plugins

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// RKE2Plugin handles RKE2/k3s containerd image and cache cleanup.
type RKE2Plugin struct{}

// NewRKE2Plugin creates a new RKE2 cleanup plugin.
func NewRKE2Plugin() *RKE2Plugin {
	return &RKE2Plugin{}
}

// Name returns the plugin identifier.
func (p *RKE2Plugin) Name() string {
	return "rke2"
}

// Description returns the plugin description.
func (p *RKE2Plugin) Description() string {
	return "Cleans RKE2/k3s containerd images, old pod logs, and kubelet garbage"
}

// SupportedPlatforms returns supported platforms (Linux only).
func (p *RKE2Plugin) SupportedPlatforms() []string {
	return []string{PlatformLinux}
}

// Enabled checks if RKE2 cleanup is enabled.
// NOTE: RKE2/k3s cleanup is DISABLED until config.Config is extended with RKE2 settings.
// This plugin is a placeholder for future Kubernetes support.
func (p *RKE2Plugin) Enabled(cfg *config.Config) bool {
	// TODO: Add cfg.Enable.RKE2 to config.EnableFlags
	return false
}

// Cleanup performs RKE2/k3s cleanup at the specified level.
func (p *RKE2Plugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{
		Plugin: p.Name(),
		Level:  level,
	}

	// Check if RKE2/k3s is installed
	if !p.isRKE2Present() {
		logger.Debug("RKE2/k3s not found, skipping")
		return result
	}

	switch level {
	case LevelWarning:
		// Light cleanup: just old pod logs
		result = p.cleanOldPodLogs(ctx, logger)
	case LevelModerate:
		// Moderate: pod logs + unused images
		result = p.cleanModerate(ctx, cfg, logger)
	case LevelAggressive:
		// Aggressive: + kubelet garbage + old containers
		result = p.cleanAggressive(ctx, cfg, logger)
	case LevelCritical:
		// Emergency: full image prune
		result = p.cleanCritical(ctx, logger)
	}

	return result
}

func (p *RKE2Plugin) isRKE2Present() bool {
	// Check for RKE2 or k3s binaries
	paths := []string{
		"/usr/local/bin/rke2",
		"/var/lib/rancher/rke2/bin/kubectl",
		"/usr/local/bin/k3s",
		"/var/lib/rancher/k3s/data",
	}

	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

func (p *RKE2Plugin) getContainerdSocket() string {
	// Try RKE2 socket first
	sockets := []string{
		"/run/k3s/containerd/containerd.sock",
		"/var/run/k3s/containerd/containerd.sock",
		"/run/containerd/containerd.sock",
	}

	for _, sock := range sockets {
		if _, err := os.Stat(sock); err == nil {
			return sock
		}
	}
	return ""
}

func (p *RKE2Plugin) cleanOldPodLogs(ctx context.Context, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: LevelWarning}

	// Pod logs are typically in /var/log/pods/
	podLogDir := "/var/log/pods"
	if _, err := os.Stat(podLogDir); os.IsNotExist(err) {
		return result
	}

	logger.Debug("cleaning old pod logs", "dir", podLogDir)

	// Find and remove logs older than 7 days
	cutoff := time.Now().AddDate(0, 0, -7)

	err := filepath.Walk(podLogDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		// Only clean .log files that are old
		if strings.HasSuffix(info.Name(), ".log") && info.ModTime().Before(cutoff) {
			size := info.Size()
			if err := os.Remove(path); err == nil {
				result.BytesFreed += size
				result.ItemsCleaned++
			}
		}
		return nil
	})

	if err != nil {
		result.Error = err
	}

	// Also clean container logs in /var/log/containers
	containerLogDir := "/var/log/containers"
	if _, err := os.Stat(containerLogDir); err == nil {
		filepath.Walk(containerLogDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if strings.HasSuffix(info.Name(), ".log") && info.ModTime().Before(cutoff) {
				size := info.Size()
				if err := os.Remove(path); err == nil {
					result.BytesFreed += size
					result.ItemsCleaned++
				}
			}
			return nil
		})
	}

	return result
}

func (p *RKE2Plugin) cleanModerate(ctx context.Context, cfg *config.Config, logger *slog.Logger) CleanupResult {
	// First clean pod logs
	result := p.cleanOldPodLogs(ctx, logger)
	result.Level = LevelModerate

	// Then prune unused containerd images
	socket := p.getContainerdSocket()
	if socket == "" {
		logger.Debug("containerd socket not found")
		return result
	}

	logger.Debug("pruning unused containerd images", "socket", socket)

	// Use ctr to prune images in the k8s.io namespace
	cmd := exec.CommandContext(ctx, "ctr", "-a", socket, "-n", "k8s.io", "images", "prune")
	output, err := cmd.CombinedOutput()
	if err != nil {
		logger.Debug("ctr image prune failed", "error", err, "output", string(output))
	} else {
		result.BytesFreed += p.parseContainerdOutput(string(output))
		logger.Debug("containerd image prune completed", "output", string(output))
	}

	return result
}

func (p *RKE2Plugin) cleanAggressive(ctx context.Context, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := p.cleanModerate(ctx, cfg, logger)
	result.Level = LevelAggressive

	// Clean kubelet garbage
	logger.Debug("cleaning kubelet garbage")
	p.cleanKubeletGarbage(ctx, logger, &result)

	// Clean old containers
	socket := p.getContainerdSocket()
	if socket != "" {
		cmd := exec.CommandContext(ctx, "ctr", "-a", socket, "-n", "k8s.io", "containers", "prune")
		cmd.Run() // Best effort
	}

	return result
}

func (p *RKE2Plugin) cleanCritical(ctx context.Context, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: LevelCritical}

	logger.Warn("CRITICAL: running full containerd cleanup")

	socket := p.getContainerdSocket()
	if socket == "" {
		return result
	}

	// Remove all unused images (more aggressive)
	// This is similar to 'crictl rmi --prune' but using ctr directly
	cmd := exec.CommandContext(ctx, "ctr", "-a", socket, "-n", "k8s.io", "images", "prune", "--all")
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Try without --all flag
		cmd = exec.CommandContext(ctx, "ctr", "-a", socket, "-n", "k8s.io", "images", "prune")
		output, err = cmd.CombinedOutput()
	}

	if err == nil {
		result.BytesFreed += p.parseContainerdOutput(string(output))
	}

	// Also try crictl if available
	if _, err := exec.LookPath("crictl"); err == nil {
		logger.Debug("running crictl image prune")
		cmd := exec.CommandContext(ctx, "crictl", "rmi", "--prune")
		cmd.Run() // Best effort
	}

	// Clean all pod logs regardless of age
	podLogDir := "/var/log/pods"
	filepath.Walk(podLogDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(info.Name(), ".log") {
			size := info.Size()
			if err := os.Remove(path); err == nil {
				result.BytesFreed += size
				result.ItemsCleaned++
			}
		}
		return nil
	})

	return result
}

func (p *RKE2Plugin) cleanKubeletGarbage(ctx context.Context, logger *slog.Logger, result *CleanupResult) {
	// Kubelet stores various caches and temporary files
	kubeletDirs := []string{
		"/var/lib/kubelet/pods",
		"/var/lib/rancher/rke2/agent/pod-manifests",
	}

	for _, dir := range kubeletDirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}

		// Find and remove orphaned pod directories
		// An orphaned pod directory is one that has no running containers
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			podDir := filepath.Join(dir, entry.Name())
			info, err := entry.Info()
			if err != nil {
				continue
			}

			// If pod directory is older than 24 hours and has no recent activity, consider cleaning
			if time.Since(info.ModTime()) > 24*time.Hour {
				// Check if pod is actually orphaned (no containers running)
				if p.isPodOrphaned(podDir) {
					size := p.getDirSize(podDir)
					if err := os.RemoveAll(podDir); err == nil {
						result.BytesFreed += size
						result.ItemsCleaned++
						logger.Debug("removed orphaned pod directory", "path", podDir)
					}
				}
			}
		}
	}
}

func (p *RKE2Plugin) isPodOrphaned(podDir string) bool {
	// A pod is considered orphaned if its volumes/containers subdirectories
	// are empty or contain only stale data
	containersDir := filepath.Join(podDir, "containers")
	if _, err := os.Stat(containersDir); os.IsNotExist(err) {
		return true
	}

	entries, err := os.ReadDir(containersDir)
	if err != nil || len(entries) == 0 {
		return true
	}

	return false
}

func (p *RKE2Plugin) getDirSize(path string) int64 {
	var size int64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size
}

func (p *RKE2Plugin) parseContainerdOutput(output string) int64 {
	// containerd/ctr output can vary, try to extract any size information
	// Example patterns: "removed 5 images (1.2 GB)"
	patterns := []string{
		`([\d.]+)\s*(GB|MB|KB|B)`,
		`Total:\s*([\d.]+)\s*(GB|MB|KB|B)`,
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
			case "B":
				return int64(value)
			}
		}
	}

	return 0
}
