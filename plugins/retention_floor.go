package plugins

// retention_floor.go tracks the free-space floor per volume (TIN-5770). The
// floor used to append an alert line every pass: on November 18, 2026 it
// wrote 118 lines on petting-zoo-mini that nothing read. Now a breach is
// logged when it starts and when it ends, and its state is carried to the
// cycle report and the state file through FloorReporter.

import (
	"fmt"
	"log/slog"
	"time"
)

// FloorStatus is one volume's free-space floor state, for the cycle report
// and the state file.
type FloorStatus struct {
	Path       string `json:"path"`
	FreeBytes  uint64 `json:"free_bytes"`
	FloorBytes uint64 `json:"floor_bytes"`
	Below      bool   `json:"below_floor"`
	// BreachedSince is when the volume first read under the floor in this
	// breach (RFC3339); empty while it is over the floor.
	BreachedSince string `json:"breached_since,omitempty"`
	// ChecksBelow counts the reads under the floor in this breach.
	ChecksBelow int    `json:"checks_below,omitempty"`
	CheckedAt   string `json:"checked_at"`
}

// FloorReporter is a plugin that tracks free-space floors.
type FloorReporter interface {
	FloorStatuses() []FloorStatus
}

type retentionFloorTrack struct {
	status FloorStatus
	since  time.Time
}

// noteFloor records one floor reading and logs only the edges: once when a
// breach starts and once when it ends.
func (p *RetentionPlugin) noteFloor(path string, free, floor uint64, logger *slog.Logger) bool {
	p.fmu.Lock()
	defer p.fmu.Unlock()
	if p.floors == nil {
		p.floors = map[string]*retentionFloorTrack{}
	}
	now := p.clock()
	tr := p.floors[path]
	if tr == nil {
		tr = &retentionFloorTrack{}
		p.floors[path] = tr
	}
	below := free < floor
	wasBelow := tr.status.Below
	tr.status = FloorStatus{Path: path, FreeBytes: free, FloorBytes: floor, Below: below, CheckedAt: now.UTC().Format(time.RFC3339), ChecksBelow: tr.status.ChecksBelow}
	switch {
	case below && !wasBelow:
		tr.since = now
		tr.status.ChecksBelow = 1
		if logger != nil {
			logger.Warn("retention free-space floor", "event", "breach_start", "path", path,
				"free_gib", fmt.Sprintf("%.1f", float64(free)/(1<<30)), "floor_gib", fmt.Sprintf("%.1f", float64(floor)/(1<<30)))
		}
	case below:
		tr.status.ChecksBelow++
	case wasBelow:
		if logger != nil {
			logger.Info("retention free-space floor", "event", "recovered", "path", path,
				"free_gib", fmt.Sprintf("%.1f", float64(free)/(1<<30)), "breach_duration", now.Sub(tr.since).Round(time.Second).String(),
				"checks_below", tr.status.ChecksBelow)
		}
		tr.since = time.Time{}
		tr.status.ChecksBelow = 0
	}
	if below {
		tr.status.BreachedSince = tr.since.UTC().Format(time.RFC3339)
	}
	return below
}

// FloorStatuses returns the last reading of every tracked volume, sorted by
// path.
func (p *RetentionPlugin) FloorStatuses() []FloorStatus {
	p.fmu.Lock()
	defer p.fmu.Unlock()
	out := make([]FloorStatus, 0, len(p.floors))
	for _, tr := range p.floors {
		out = append(out, tr.status)
	}
	sortFloorStatuses(out)
	return out
}

func sortFloorStatuses(s []FloorStatus) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Path < s[j-1].Path; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

