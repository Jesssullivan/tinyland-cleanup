package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/Jesssullivan/tinyland-cleanup/monitor"
)

// levelIndependentPlugin is a reportingPlugin that applies its own pressure
// policy (R-C645), like bazel_bases.
type levelIndependentPlugin struct {
	reportingPlugin
}

func (p *levelIndependentPlugin) LevelIndependent() bool { return true }

func TestLevelIndependentPluginRunsWithNoHostPressure(t *testing.T) {
	var output bytes.Buffer
	independent := &levelIndependentPlugin{reportingPlugin{name: "independent"}}
	independent.result.BytesFreed = 4096
	independent.result.ItemsCleaned = 1
	ladder := &reportingPlugin{name: "ladder"}
	daemon := newTestDaemonWithPlugins(t, &output, independent, ladder)
	daemon.diskStats = sequenceDiskStats(t, diskStats(1000, 700, 30))

	if err := daemon.runOnce(context.Background(), monitor.LevelNone); err != nil {
		t.Fatal(err)
	}
	if !independent.called {
		t.Fatal("a level-independent plugin did not run at cleanup level none")
	}
	if ladder.called {
		t.Fatal("a ladder plugin ran with no host pressure")
	}
	report := decodeCycleReport(t, output.Bytes())
	if report.TotalBytesFreed != 4096 {
		t.Fatalf("level-independent bytes not reported: %d", report.TotalBytesFreed)
	}
}

func TestLevelIndependentPluginIgnoresCooldown(t *testing.T) {
	var output bytes.Buffer
	independent := &levelIndependentPlugin{reportingPlugin{name: "independent"}}
	daemon := newTestDaemonWithPlugins(t, &output, independent)
	daemon.config.Policy.Cooldown = "24h"
	daemon.diskStats = sequenceDiskStats(t, diskStats(1000, 100, 90))

	for i := 0; i < 2; i++ {
		independent.called = false
		output.Reset()
		if err := daemon.runOnce(context.Background(), monitor.LevelNone); err != nil {
			t.Fatal(err)
		}
		if !independent.called {
			t.Fatalf("run %d: a level-independent plugin was held back by cooldown", i)
		}
	}
}
