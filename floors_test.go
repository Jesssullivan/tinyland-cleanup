package main

import (
	"io"
	"testing"

	"github.com/Jesssullivan/tinyland-cleanup/plugins"
)

type floorPlugin struct {
	reportingPlugin
	status []plugins.FloorStatus
}

func (p *floorPlugin) FloorStatuses() []plugins.FloorStatus { return p.status }

func TestRecordFloorsWritesReportAndStateOnEdges(t *testing.T) {
	fp := &floorPlugin{reportingPlugin: reportingPlugin{name: "retention"}}
	d := newTestDaemonWithPlugins(t, io.Discard, fp)
	state := newCleanupState()
	var report cycleReport

	if d.recordFloors(&report, state, nil) || report.FreeSpaceFloors != nil {
		t.Fatal("no readings: nothing to record")
	}
	fp.status = []plugins.FloorStatus{{Path: "/nix", FreeBytes: 5 << 30, FloorBytes: 20 << 30, Below: true, BreachedSince: "2026-11-18T16:00:00Z", ChecksBelow: 1}}
	if !d.recordFloors(&report, state, nil) {
		t.Fatal("a breach start must change the state")
	}
	if len(report.FreeSpaceFloors) != 1 || state.Floors["/nix"].BreachedSince != "2026-11-18T16:00:00Z" {
		t.Fatalf("report %+v, state %+v", report.FreeSpaceFloors, state.Floors)
	}
	fp.status[0].FreeBytes = 4 << 30
	fp.status[0].ChecksBelow = 2
	if !d.recordFloors(&report, state, nil) {
		t.Fatal("another read under the floor must update the count")
	}
	fp.status = []plugins.FloorStatus{{Path: "/nix", FreeBytes: 60 << 30, FloorBytes: 20 << 30}}
	if !d.recordFloors(&report, state, nil) || state.Floors["/nix"].Below {
		t.Fatalf("recovery must be recorded: %+v", state.Floors)
	}
	fp.status[0].FreeBytes = 61 << 30
	if d.recordFloors(&report, state, nil) {
		t.Fatal("free bytes moving over the floor alone must not rewrite the state")
	}
	d.dryRun = true
	fp.status[0].Below = true
	if d.recordFloors(&report, state, nil) {
		t.Fatal("a dry run never writes state")
	}
}
