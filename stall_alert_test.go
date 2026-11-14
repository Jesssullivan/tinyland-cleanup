package main

import (
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/plugins"
)

// TestStalledAlertNamesCriticalMountAfterZeroYieldCycles covers the honey
// 2026-11-14 gap (TIN-5770): /home stayed critical while every plugin
// reclaimed nothing, and only per-plugin zero-yield lines were logged.
func TestStalledAlertNamesCriticalMountAfterZeroYieldCycles(t *testing.T) {
	plugin := &reportingPlugin{name: "dev-artifacts"}
	d, clock, output := newZeroYieldDaemon(t, bytePressureStats(25, 95), plugin)
	d.config.Policy.StalledAlertCycles = 3
	d.config.Policy.ZeroYieldLimit = -1

	for i := 1; i <= 4; i++ {
		rep := runByteCycle(t, d, output)
		if i < 3 && len(rep.StalledMounts) != 0 {
			t.Fatalf("cycle %d: stalled before the limit: %+v", i, rep.StalledMounts)
		}
		if i >= 3 {
			if len(rep.StalledMounts) != 1 || rep.StalledMounts[0].Cycles != i {
				t.Fatalf("cycle %d: stalled_mounts = %+v, want one mount at %d cycles", i, rep.StalledMounts, i)
			}
		}
		clock.Advance(5 * time.Minute)
	}

	// A cycle that reclaims clears the stall.
	plugin.result = plugins.CleanupResult{BytesFreed: d.byteProgressMinBytes()}
	if rep := runByteCycle(t, d, output); len(rep.StalledMounts) != 0 {
		t.Fatalf("a reclaiming cycle left stalled_mounts = %+v", rep.StalledMounts)
	}
	plugin.result = plugins.CleanupResult{}
	clock.Advance(5 * time.Minute)
	if rep := runByteCycle(t, d, output); len(rep.StalledMounts) != 0 {
		t.Fatalf("the count did not restart after progress: %+v", rep.StalledMounts)
	}
}

func TestStalledAlertIgnoresNonCriticalMountsAndCanBeDisabled(t *testing.T) {
	d, clock, output := newZeroYieldDaemon(t, bytePressureStats(37, 92), &reportingPlugin{name: "dev-artifacts"})
	d.config.Policy.StalledAlertCycles = 1
	d.config.Policy.ZeroYieldLimit = -1
	for i := 0; i < 3; i++ {
		if rep := runByteCycle(t, d, output); len(rep.StalledMounts) != 0 {
			t.Fatalf("aggressive mount reported stalled: %+v", rep.StalledMounts)
		}
		clock.Advance(5 * time.Minute)
	}

	d2, clock2, output2 := newZeroYieldDaemon(t, bytePressureStats(25, 95), &reportingPlugin{name: "dev-artifacts"})
	d2.config.Policy.StalledAlertCycles = -1
	d2.config.Policy.ZeroYieldLimit = -1
	for i := 0; i < 8; i++ {
		if rep := runByteCycle(t, d2, output2); len(rep.StalledMounts) != 0 {
			t.Fatalf("disabled alert reported: %+v", rep.StalledMounts)
		}
		clock2.Advance(5 * time.Minute)
	}
}

// A cycle whose plugins were all held back (here zero-yield suppressed) does
// not advance the stall count; only cycles that ran a plugin do.
func TestStalledAlertCountsOnlyCyclesThatRanPlugins(t *testing.T) {
	d, clock, output := newZeroYieldDaemon(t, bytePressureStats(25, 95), &reportingPlugin{name: "dev-artifacts"})
	d.config.Policy.StalledAlertCycles = 3
	// Suppress after 2 zero-yield runs for a long time.
	d.config.Policy.ZeroYieldLimit = 2
	d.config.Policy.ZeroYieldBackoffBase = "24h"
	d.config.Policy.ZeroYieldPressureMinInterval = "24h"
	ran := 0
	for i := 0; i < 6; i++ {
		rep := runByteCycle(t, d, output)
		if onlyPlugin(t, rep).Ran {
			ran++
		}
		if len(rep.StalledMounts) != 0 {
			t.Fatalf("cycle %d: stalled after %d cycles that ran plugins: %+v", i, ran, rep.StalledMounts)
		}
		clock.Advance(5 * time.Minute)
	}
	if ran != 2 {
		t.Fatalf("plugin ran %d times, want 2 before suppression", ran)
	}
}
