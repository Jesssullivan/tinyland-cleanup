package main

import (
	"github.com/Jesssullivan/tinyland-cleanup/plugins"
)

// recordFloors copies every enabled plugin's free-space floor readings into
// the cycle report, and into the state file when a breach starts, ends or is
// read again (TIN-5770: the floor used to be a log line nothing read). It
// reports whether the state changed and needs saving.
func (d *daemon) recordFloors(report *cycleReport, state *cleanupState, stateErr error) bool {
	var all []plugins.FloorStatus
	for _, p := range filterEnabledPlugins(d.registry.GetEnabled(d.config), d.pluginFilter) {
		if fr, ok := p.(plugins.FloorReporter); ok {
			all = append(all, fr.FloorStatuses()...)
		}
	}
	if len(all) == 0 {
		return false
	}
	report.FreeSpaceFloors = all
	if d.dryRun || stateErr != nil || state == nil {
		return false
	}
	if state.Floors == nil {
		state.Floors = map[string]plugins.FloorStatus{}
	}
	changed := false
	for _, f := range all {
		prev, ok := state.Floors[f.Path]
		if !ok || prev.Below != f.Below || prev.BreachedSince != f.BreachedSince ||
			prev.FloorBytes != f.FloorBytes || (f.Below && prev.ChecksBelow != f.ChecksBelow) {
			state.Floors[f.Path] = f
			changed = true
		}
	}
	return changed
}
