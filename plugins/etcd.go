// Package plugins provides cleanup plugin implementations.
package plugins

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// EtcdPlugin handles etcd snapshot and WAL cleanup for Kubernetes clusters.
type EtcdPlugin struct{}

// NewEtcdPlugin creates a new etcd cleanup plugin.
func NewEtcdPlugin() *EtcdPlugin {
	return &EtcdPlugin{}
}

// Name returns the plugin identifier.
func (p *EtcdPlugin) Name() string {
	return "etcd"
}

// Description returns the plugin description.
func (p *EtcdPlugin) Description() string {
	return "Cleans old etcd snapshots, WAL files, and runs defrag when needed"
}

// SupportedPlatforms returns supported platforms (Linux only).
func (p *EtcdPlugin) SupportedPlatforms() []string {
	return []string{PlatformLinux}
}

// Enabled checks if etcd cleanup is enabled.
// NOTE: Etcd cleanup is DISABLED until config.Config is extended with Etcd settings.
// This plugin is a placeholder for future Kubernetes/etcd support.
func (p *EtcdPlugin) Enabled(cfg *config.Config) bool {
	// TODO: Add cfg.Enable.Etcd and cfg.Etcd configuration
	return false
}

// Cleanup performs etcd cleanup at the specified level.
func (p *EtcdPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{
		Plugin: p.Name(),
		Level:  level,
	}

	// Check if etcd data directory exists
	if !p.isEtcdPresent(cfg) {
		logger.Debug("etcd data directory not found, skipping")
		return result
	}

	switch level {
	case LevelWarning:
		// Light cleanup: just clean old WAL files
		result = p.cleanOldWAL(ctx, cfg, logger)
	case LevelModerate:
		// Moderate: WAL + old snapshots beyond retention
		result = p.cleanModerate(ctx, cfg, logger)
	case LevelAggressive:
		// Aggressive: + defrag if needed
		result = p.cleanAggressive(ctx, cfg, logger)
	case LevelCritical:
		// Emergency: aggressive cleanup + force defrag
		result = p.cleanCritical(ctx, cfg, logger)
	}

	return result
}

func (p *EtcdPlugin) isEtcdPresent(cfg *config.Config) bool {
	// TODO: When cfg.Etcd is added, check cfg.Etcd.DataDir
	// For now, check default k3s/RKE2 locations
	defaultPaths := []string{
		"/var/lib/rancher/rke2/server/db/etcd",
		"/var/lib/rancher/k3s/server/db/etcd",
		"/var/lib/etcd",
	}
	for _, path := range defaultPaths {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// Default etcd configuration when cfg.Etcd is not yet implemented
const (
	defaultEtcdDataDir           = "/var/lib/rancher/rke2/server/db/etcd"
	defaultEtcdWALRetentionDays  = 7
	defaultEtcdSnapshotRetention = 5
	defaultEtcdDefragThreshold   = 80
)

func (p *EtcdPlugin) cleanOldWAL(ctx context.Context, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: LevelWarning}

	// Use default data dir until cfg.Etcd is implemented
	dataDir := defaultEtcdDataDir
	walRetentionDays := defaultEtcdWALRetentionDays

	walDir := filepath.Join(dataDir, "member", "wal")
	if _, err := os.Stat(walDir); os.IsNotExist(err) {
		return result
	}

	logger.Debug("cleaning old WAL files", "dir", walDir, "retention_days", walRetentionDays)

	// Find and remove WAL files older than retention period
	cutoff := time.Now().AddDate(0, 0, -walRetentionDays)

	err := filepath.Walk(walDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip on error
		}
		if info.IsDir() {
			return nil
		}
		// Only clean .wal files that are old
		if strings.HasSuffix(info.Name(), ".wal") && info.ModTime().Before(cutoff) {
			size := info.Size()
			if err := os.Remove(path); err == nil {
				result.BytesFreed += size
				result.ItemsCleaned++
				logger.Debug("removed old WAL file", "path", path, "age_days", int(time.Since(info.ModTime()).Hours()/24))
			}
		}
		return nil
	})

	if err != nil {
		result.Error = err
	}

	return result
}

func (p *EtcdPlugin) cleanModerate(ctx context.Context, cfg *config.Config, logger *slog.Logger) CleanupResult {
	// First clean WAL files
	result := p.cleanOldWAL(ctx, cfg, logger)
	result.Level = LevelModerate

	// Use default data dir until cfg.Etcd is implemented
	dataDir := defaultEtcdDataDir
	snapshotRetention := defaultEtcdSnapshotRetention

	// Then clean old snapshots beyond retention
	snapDir := filepath.Join(dataDir, "member", "snap")
	if _, err := os.Stat(snapDir); os.IsNotExist(err) {
		return result
	}

	logger.Debug("cleaning old snapshots", "dir", snapDir, "retention", snapshotRetention)

	// Find all snapshot files
	var snapshots []string
	err := filepath.Walk(snapDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".snap") {
			snapshots = append(snapshots, path)
		}
		return nil
	})

	if err != nil {
		result.Error = err
		return result
	}

	// Sort by modification time (newest first)
	sort.Slice(snapshots, func(i, j int) bool {
		infoI, _ := os.Stat(snapshots[i])
		infoJ, _ := os.Stat(snapshots[j])
		if infoI == nil || infoJ == nil {
			return false
		}
		return infoI.ModTime().After(infoJ.ModTime())
	})

	// Remove snapshots beyond retention count
	if len(snapshots) > snapshotRetention {
		for _, snap := range snapshots[snapshotRetention:] {
			info, err := os.Stat(snap)
			if err != nil {
				continue
			}
			size := info.Size()
			if err := os.Remove(snap); err == nil {
				result.BytesFreed += size
				result.ItemsCleaned++
				logger.Debug("removed old snapshot", "path", snap)
			}
		}
	}

	return result
}

func (p *EtcdPlugin) cleanAggressive(ctx context.Context, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := p.cleanModerate(ctx, cfg, logger)
	result.Level = LevelAggressive

	// Use default threshold until cfg.Etcd is implemented
	defragThreshold := defaultEtcdDefragThreshold

	// Check disk usage and defrag if above threshold
	usage := p.getEtcdDiskUsage()
	if usage >= defragThreshold {
		logger.Info("etcd disk usage above threshold, running defrag", "usage", usage, "threshold", defragThreshold)
		p.runDefrag(ctx, logger)
	}

	return result
}

func (p *EtcdPlugin) cleanCritical(ctx context.Context, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := p.cleanModerate(ctx, cfg, logger)
	result.Level = LevelCritical

	// Always run defrag in critical mode
	logger.Warn("CRITICAL: forcing etcd defrag")
	p.runDefrag(ctx, logger)

	// Also compact the database if etcdctl is available
	p.compactDatabase(ctx, logger)

	return result
}

func (p *EtcdPlugin) getEtcdDiskUsage() int {
	// Get the mount point for etcd data dir and check its usage
	// Use default data dir until cfg.Etcd is implemented
	cmd := exec.Command("df", defaultEtcdDataDir)
	output, err := cmd.Output()
	if err != nil {
		return 0
	}

	lines := strings.Split(string(output), "\n")
	if len(lines) < 2 {
		return 0
	}

	fields := strings.Fields(lines[1])
	if len(fields) < 5 {
		return 0
	}

	// Parse percentage (remove %)
	usage := strings.TrimSuffix(fields[4], "%")
	var percent int
	fmt.Sscanf(usage, "%d", &percent)
	return percent
}

func (p *EtcdPlugin) runDefrag(ctx context.Context, logger *slog.Logger) {
	// Try etcdctl defrag first (works with RKE2/k3s)
	etcdctlPaths := []string{
		"/var/lib/rancher/rke2/bin/etcdctl",
		"/usr/local/bin/etcdctl",
		"/usr/bin/etcdctl",
	}

	var etcdctl string
	for _, path := range etcdctlPaths {
		if _, err := os.Stat(path); err == nil {
			etcdctl = path
			break
		}
	}

	if etcdctl == "" {
		logger.Debug("etcdctl not found, skipping defrag")
		return
	}

	// Build etcdctl command with RKE2 environment
	env := []string{
		"ETCDCTL_API=3",
		"ETCDCTL_CACERT=/var/lib/rancher/rke2/server/tls/etcd/server-ca.crt",
		"ETCDCTL_CERT=/var/lib/rancher/rke2/server/tls/etcd/server-client.crt",
		"ETCDCTL_KEY=/var/lib/rancher/rke2/server/tls/etcd/server-client.key",
	}

	cmd := exec.CommandContext(ctx, etcdctl, "defrag", "--endpoints=https://127.0.0.1:2379")
	cmd.Env = append(os.Environ(), env...)

	logger.Debug("running etcd defrag")
	if output, err := cmd.CombinedOutput(); err != nil {
		logger.Debug("etcd defrag failed", "error", err, "output", string(output))
	} else {
		logger.Info("etcd defrag completed successfully")
	}
}

func (p *EtcdPlugin) compactDatabase(ctx context.Context, logger *slog.Logger) {
	// This is a more aggressive operation - compact the database
	etcdctlPaths := []string{
		"/var/lib/rancher/rke2/bin/etcdctl",
		"/usr/local/bin/etcdctl",
		"/usr/bin/etcdctl",
	}

	var etcdctl string
	for _, path := range etcdctlPaths {
		if _, err := os.Stat(path); err == nil {
			etcdctl = path
			break
		}
	}

	if etcdctl == "" {
		return
	}

	env := []string{
		"ETCDCTL_API=3",
		"ETCDCTL_CACERT=/var/lib/rancher/rke2/server/tls/etcd/server-ca.crt",
		"ETCDCTL_CERT=/var/lib/rancher/rke2/server/tls/etcd/server-client.crt",
		"ETCDCTL_KEY=/var/lib/rancher/rke2/server/tls/etcd/server-client.key",
	}

	// Get current revision
	cmd := exec.CommandContext(ctx, etcdctl, "endpoint", "status", "--endpoints=https://127.0.0.1:2379", "--write-out=json")
	cmd.Env = append(os.Environ(), env...)
	output, err := cmd.Output()
	if err != nil {
		logger.Debug("failed to get etcd status", "error", err)
		return
	}

	// Parse revision from output (simplified - real impl would use json parsing)
	if !strings.Contains(string(output), "revision") {
		return
	}

	logger.Debug("etcd compaction would be performed here (skipping for safety)")
}
