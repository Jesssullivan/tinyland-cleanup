//go:build tinyland_sim

package main

// Simulated disk statistics for consumer tests (TIN-3342 criterion 8).
//
// Built only with -tags tinyland_sim. The binary then reads statfs results
// from TINYLAND_CLEANUP_SIM_DISKSTATS instead of the filesystem, so a test can
// hold the daemon under byte pressure without filling a disk. Plugins still
// walk and delete on the real filesystem, so a test points them at a fixture.
//
// The variable holds JSON, or "@<file>" to re-read a file on every call so a
// test can change pressure mid-run. The JSON is either one object applied to
// every path or an object keyed by path with an optional "*" fallback:
//
//	{"total_bytes": 493921239040, "free_bytes": 26843545600}
//	{"/home": {...}, "*": {...}}
//
// Fields: total_bytes, free_bytes (required), inodes_total, inodes_free,
// fstype. A missing or malformed value is a statfs error, never real stats.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Jesssullivan/tinyland-cleanup/monitor"
)

const simulationBuild = true

const simDiskStatsEnv = "TINYLAND_CLEANUP_SIM_DISKSTATS"

type simDiskStats struct {
	TotalBytes  *uint64 `json:"total_bytes"`
	FreeBytes   *uint64 `json:"free_bytes"`
	InodesTotal uint64  `json:"inodes_total"`
	InodesFree  uint64  `json:"inodes_free"`
	Fstype      string  `json:"fstype"`
}

func diskStatsReader() func(path string) (*monitor.DiskStats, error) {
	return func(path string) (*monitor.DiskStats, error) {
		return simulatedDiskStats(os.Getenv(simDiskStatsEnv), path)
	}
}

func simulationSuffix() string { return " SIMULATION BUILD" }

func simulatedDiskStats(raw, path string) (*monitor.DiskStats, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("simulation build: %s is not set", simDiskStatsEnv)
	}
	if strings.HasPrefix(raw, "@") {
		data, err := os.ReadFile(raw[1:])
		if err != nil {
			return nil, fmt.Errorf("simulation build: %w", err)
		}
		raw = string(data)
	}
	sim, err := selectSimDiskStats([]byte(raw), path)
	if err != nil {
		return nil, err
	}
	if sim.TotalBytes == nil || sim.FreeBytes == nil || *sim.TotalBytes == 0 || *sim.FreeBytes > *sim.TotalBytes {
		return nil, fmt.Errorf("simulation build: %s needs total_bytes > 0 and free_bytes <= total_bytes", simDiskStatsEnv)
	}
	total, free := *sim.TotalBytes, *sim.FreeBytes
	used := total - free
	usedPercent := float64(used) / float64(total) * 100
	stats := &monitor.DiskStats{
		Path:          path,
		Total:         total,
		Used:          used,
		Free:          free,
		UsedPercent:   usedPercent,
		FreePercent:   100 - usedPercent,
		FreeGB:        float64(free) / (1024 * 1024 * 1024),
		InodesTotal:   sim.InodesTotal,
		InodesFree:    sim.InodesFree,
		Fstype:        sim.Fstype,
		InodesDynamic: monitor.DynamicInodeFilesystem(sim.Fstype),
	}
	if sim.InodesTotal > 0 && sim.InodesFree <= sim.InodesTotal {
		stats.InodesUsed = sim.InodesTotal - sim.InodesFree
		stats.InodesUsedPercent = float64(stats.InodesUsed) / float64(sim.InodesTotal) * 100
		stats.InodesFreePercent = 100 - stats.InodesUsedPercent
	}
	return stats, nil
}

func selectSimDiskStats(data []byte, path string) (simDiskStats, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return simDiskStats{}, fmt.Errorf("simulation build: %s: %w", simDiskStatsEnv, err)
	}
	if _, single := probe["total_bytes"]; single {
		var sim simDiskStats
		err := json.Unmarshal(data, &sim)
		return sim, err
	}
	entry, ok := probe[path]
	if !ok {
		entry, ok = probe["*"]
	}
	if !ok {
		return simDiskStats{}, fmt.Errorf("simulation build: %s has no entry for %s and no \"*\" fallback", simDiskStatsEnv, path)
	}
	var sim simDiskStats
	err := json.Unmarshal(entry, &sim)
	return sim, err
}
