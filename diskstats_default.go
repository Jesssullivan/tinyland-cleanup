//go:build !tinyland_sim

package main

import "github.com/Jesssullivan/tinyland-cleanup/monitor"

// simulationBuild is false in every production build. The tinyland_sim build
// tag (diskstats_sim.go) is the only way to set it, and no release, Nix
// default package or Bazel target sets that tag.
const simulationBuild = false

// simDiskStatsEnv is named here too so the startup warning compiles in both
// builds; the default build never reads it.
const simDiskStatsEnv = "TINYLAND_CLEANUP_SIM_DISKSTATS"

func diskStatsReader() func(path string) (*monitor.DiskStats, error) {
	return monitor.GetDiskStats
}

func simulationSuffix() string { return "" }
