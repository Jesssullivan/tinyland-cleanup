//go:build linux || darwin

package plugins

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestRetentionPressureFloorBreachRunsAtOnceAndLogsEdgesOnly(t *testing.T) {
	f := newPressureFixture(t)
	f.used = 70 * pfGiB // under used_percent 90
	f.cfg.Retention.FreeSpaceFloor.FloorGiB = 40
	f.entry("job-a", 4)
	f.entry("job-b", 3)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	ctx := context.Background()
	// The floor breach (30 GiB free under 40) runs the pass with no interval
	// wait, and it stops once the volume is over the floor (65, then 60 used).
	f.plugin.pressureCycle(ctx, f.cfg, true, logger)
	f.want("job-a", "job-b")
	st := f.plugin.FloorStatuses()
	if len(st) != 1 || !st[0].Below || st[0].BreachedSince == "" {
		t.Fatalf("floor status = %+v", st)
	}
	f.used = 75 * pfGiB
	f.plugin.pressureCycle(ctx, f.cfg, true, logger)
	f.used = 10 * pfGiB
	f.plugin.pressureCycle(ctx, f.cfg, true, logger)
	log := buf.String()
	if strings.Count(log, "event=breach_start") != 1 || strings.Count(log, "event=recovered") != 1 {
		t.Fatalf("want one breach_start and one recovered line, got:\n%s", log)
	}
	if !strings.Contains(log, "checks_below=2") {
		t.Fatalf("recovery line should count the reads under the floor:\n%s", log)
	}
	if st := f.plugin.FloorStatuses(); st[0].Below || st[0].BreachedSince != "" {
		t.Fatalf("after recovery: %+v", st)
	}
}

// TestRetentionFloorReadCountsOncePerCycle: on a cycle when the base pass is
// due, the pressure pass and the base pass both read the tmp_agents root.
// The floor tracking counts that root once.
func TestRetentionFloorReadCountsOncePerCycle(t *testing.T) {
	f := newPressureFixture(t)
	f.used = 70 * pfGiB
	f.cfg.Retention.FreeSpaceFloor.FloorGiB = 40
	f.plugin.Cleanup(context.Background(), LevelModerate, f.cfg, retDiscard())
	for _, st := range f.plugin.FloorStatuses() {
		if st.Path == f.root {
			if !st.Below || st.ChecksBelow != 1 {
				t.Fatalf("floor status = %+v, want one read under the floor", st)
			}
			return
		}
	}
	t.Fatalf("no floor status for %s: %+v", f.root, f.plugin.FloorStatuses())
}
