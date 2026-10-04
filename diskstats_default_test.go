//go:build !tinyland_sim

package main

import "testing"

// Guards the TIN-3342 sim hook boundary: the default build must never read
// simulated disk statistics.
func TestDefaultBuildIsNotSimulation(t *testing.T) {
	if simulationBuild || simulationSuffix() != "" {
		t.Fatal("default build reports itself as a simulation build")
	}
	t.Setenv(simDiskStatsEnv, `{"total_bytes": 100, "free_bytes": 1}`)
	stats, err := diskStatsReader()(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total == 100 {
		t.Fatal("default build read simulated disk statistics")
	}
}
