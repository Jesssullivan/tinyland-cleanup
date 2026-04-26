//go:build darwin

package plugins

import (
	"context"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

func TestAPFSPluginInterface(t *testing.T) {
	p := NewAPFSPlugin()

	if p.Name() != "apfs-snapshots" {
		t.Errorf("expected name 'apfs-snapshots', got %q", p.Name())
	}

	if p.Description() == "" {
		t.Error("description should not be empty")
	}

	// Should support Darwin only
	platforms := p.SupportedPlatforms()
	if len(platforms) != 1 || platforms[0] != PlatformDarwin {
		t.Errorf("expected [darwin], got %v", platforms)
	}

	// Should be enabled when APFSSnapshots flag is true
	cfg := config.DefaultConfig()
	if !p.Enabled(cfg) {
		t.Error("expected APFSSnapshots to be enabled by default on Darwin")
	}

	cfg.Enable.APFSSnapshots = false
	if p.Enabled(cfg) {
		t.Error("expected APFSSnapshots to be disabled when flag is false")
	}
}

func TestParseSnapshotList(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int
	}{
		{
			name:     "empty",
			input:    "",
			expected: 0,
		},
		{
			name: "single snapshot",
			input: `com.apple.TimeMachine.2026-01-15-123456.local
`,
			expected: 1,
		},
		{
			name: "multiple snapshots",
			input: `com.apple.TimeMachine.2026-01-10-080000.local
com.apple.TimeMachine.2026-01-12-120000.local
com.apple.TimeMachine.2026-01-15-160000.local
`,
			expected: 3,
		},
		{
			name: "with header line",
			input: `Snapshots for disk /:
com.apple.TimeMachine.2026-01-15-123456.local
`,
			expected: 1,
		},
		{
			name:     "malformed lines ignored",
			input:    "not a snapshot\ninvalid-date.local\n",
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseSnapshotList(tt.input)
			if len(result) != tt.expected {
				t.Errorf("expected %d snapshots, got %d", tt.expected, len(result))
			}
		})
	}
}

func TestParseSnapshotListOrdering(t *testing.T) {
	input := `com.apple.TimeMachine.2026-01-10-080000.local
com.apple.TimeMachine.2026-01-15-160000.local
com.apple.TimeMachine.2026-01-12-120000.local
`
	snapshots := parseSnapshotList(input)

	if len(snapshots) != 3 {
		t.Fatalf("expected 3 snapshots, got %d", len(snapshots))
	}

	// Should be sorted newest first
	if !snapshots[0].Time.After(snapshots[1].Time) {
		t.Error("snapshots should be sorted newest first")
	}
	if !snapshots[1].Time.After(snapshots[2].Time) {
		t.Error("snapshots should be sorted newest first")
	}

	// Verify newest is 2026-01-15
	if snapshots[0].Date != "2026-01-15-160000" {
		t.Errorf("expected newest snapshot date '2026-01-15-160000', got %q", snapshots[0].Date)
	}
}

func TestParseThinOutput(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int64
	}{
		{
			name:     "empty",
			input:    "",
			expected: 0,
		},
		{
			name:     "bytes freed",
			input:    "Thinned local snapshots: 5368709120 bytes",
			expected: 5368709120,
		},
		{
			name:     "multiple byte values - picks largest",
			input:    "Freed 1024 bytes from snapshot\nTotal: 5368709120 bytes freed",
			expected: 5368709120,
		},
		{
			name:     "no bytes in output",
			input:    "Thinning completed successfully",
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseThinOutput(tt.input)
			if result != tt.expected {
				t.Errorf("expected %d, got %d", tt.expected, result)
			}
		})
	}
}

func TestDeleteOldSnapshotsNeverDeleteNewest(t *testing.T) {
	p := NewAPFSPlugin()
	cap := SudoCapability{Available: true, Passwordless: false} // sudo unavailable
	p.sudoCap = &cap

	snapshots := []snapshotInfo{
		{Date: "2026-01-15-160000", Time: time.Date(2026, 1, 15, 16, 0, 0, 0, time.UTC)},
	}

	// Should not attempt deletion when there's only one snapshot
	result := p.deleteOldSnapshots(context.Background(), snapshots, 1, nil)
	if result.ItemsCleaned != 0 {
		t.Error("should not delete when only one snapshot exists")
	}
}

func TestAPFSPlanTargetsCriticalDeletesOnlyEligibleOldSnapshots(t *testing.T) {
	now := time.Date(2026, 1, 16, 12, 0, 0, 0, time.UTC)
	snapshots := []snapshotInfo{
		{Date: "2026-01-16-100000", Time: time.Date(2026, 1, 16, 10, 0, 0, 0, time.UTC)},
		{Date: "2026-01-15-180000", Time: time.Date(2026, 1, 15, 18, 0, 0, 0, time.UTC)},
		{Date: "2026-01-10-080000", Time: time.Date(2026, 1, 10, 8, 0, 0, 0, time.UTC)},
	}
	cfg := config.APFSConfig{
		ThinEnabled:     true,
		MaxThinGB:       50,
		KeepRecentDays:  1,
		DeleteOSUpdates: true,
	}

	targets := apfsPlanTargets(snapshots, LevelCritical, cfg, 50, false, true, now)
	if len(targets) != 4 {
		t.Fatalf("expected aggregate target plus 3 snapshots, got %d", len(targets))
	}

	if targets[0].Action != "thin_local_snapshots" || targets[0].Protected {
		t.Fatalf("expected eligible thinning target, got %#v", targets[0])
	}
	if targets[1].Action != "keep" || !targets[1].Protected {
		t.Fatalf("newest snapshot must be kept, got %#v", targets[1])
	}
	if targets[2].Action != "keep" || !targets[2].Protected {
		t.Fatalf("recent snapshot must be kept, got %#v", targets[2])
	}
	if targets[3].Action != "delete_old_snapshot" || targets[3].Protected {
		t.Fatalf("old snapshot should be eligible for deletion, got %#v", targets[3])
	}
	if got := apfsEstimatedCandidateBytes(targets); got != 20*apfsGiB {
		t.Fatalf("estimated candidate bytes = %d, want %d", got, 20*apfsGiB)
	}
}

func TestAPFSPlanTargetsProtectsWhenBackupActive(t *testing.T) {
	now := time.Date(2026, 1, 16, 12, 0, 0, 0, time.UTC)
	snapshots := []snapshotInfo{
		{Date: "2026-01-16-100000", Time: time.Date(2026, 1, 16, 10, 0, 0, 0, time.UTC)},
		{Date: "2026-01-10-080000", Time: time.Date(2026, 1, 10, 8, 0, 0, 0, time.UTC)},
	}
	cfg := config.APFSConfig{
		ThinEnabled:     true,
		MaxThinGB:       50,
		KeepRecentDays:  1,
		DeleteOSUpdates: true,
	}

	targets := apfsPlanTargets(snapshots, LevelCritical, cfg, 50, true, true, now)
	if got := apfsEstimatedCandidateBytes(targets); got != 0 {
		t.Fatalf("backup-active plan should estimate 0 bytes, got %d", got)
	}
	for _, target := range targets {
		if !target.Protected {
			t.Fatalf("backup-active target should be protected: %#v", target)
		}
	}
}

func TestAPFSConfigDefaults(t *testing.T) {
	cfg := config.DefaultConfig()

	if !cfg.APFS.ThinEnabled {
		t.Error("APFS.ThinEnabled should default to true")
	}
	if cfg.APFS.MaxThinGB != 50 {
		t.Errorf("APFS.MaxThinGB should default to 50, got %d", cfg.APFS.MaxThinGB)
	}
	if cfg.APFS.KeepRecentDays != 1 {
		t.Errorf("APFS.KeepRecentDays should default to 1, got %d", cfg.APFS.KeepRecentDays)
	}
	if !cfg.APFS.DeleteOSUpdates {
		t.Error("APFS.DeleteOSUpdates should default to true")
	}
}
