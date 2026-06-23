package monitor

import (
	"testing"
)

func TestDiskMonitorCheckLevel(t *testing.T) {
	mon := NewDiskMonitor(80, 85, 90, 95)

	tests := []struct {
		name        string
		usedPercent float64
		expected    CleanupLevel
	}{
		{"healthy", 50.0, LevelNone},
		{"below warning", 79.9, LevelNone},
		{"at warning", 80.0, LevelWarning},
		{"above warning", 82.0, LevelWarning},
		{"at moderate", 85.0, LevelModerate},
		{"above moderate", 87.0, LevelModerate},
		{"at aggressive", 90.0, LevelAggressive},
		{"above aggressive", 92.0, LevelAggressive},
		{"at critical", 95.0, LevelCritical},
		{"above critical", 98.0, LevelCritical},
		{"full disk", 100.0, LevelCritical},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stats := &DiskStats{
				Path:        "/",
				UsedPercent: tt.usedPercent,
				FreePercent: 100.0 - tt.usedPercent,
				FreeGB:      10.0, // Arbitrary
			}

			level := mon.CheckLevel(stats)
			if level != tt.expected {
				t.Errorf("CheckLevel(%v%%) = %v, want %v",
					tt.usedPercent, level, tt.expected)
			}
		})
	}
}

func TestDiskMonitorCheckLevelUsesInodePressure(t *testing.T) {
	mon := NewDiskMonitor(80, 85, 90, 95)

	stats := &DiskStats{
		Path:              "/nix",
		UsedPercent:       60,
		FreePercent:       40,
		InodesTotal:       1000,
		InodesUsed:        960,
		InodesFree:        40,
		InodesUsedPercent: 96,
		InodesFreePercent: 4,
	}

	if got := mon.CheckByteLevel(stats); got != LevelNone {
		t.Fatalf("byte level = %s, want none", got)
	}
	if got := mon.CheckInodeLevel(stats); got != LevelCritical {
		t.Fatalf("inode level = %s, want critical", got)
	}
	if got := mon.CheckLevel(stats); got != LevelCritical {
		t.Fatalf("combined level = %s, want critical", got)
	}
}

func TestDiskMonitorCheckLevelIgnoresUnavailableInodeStats(t *testing.T) {
	mon := NewDiskMonitor(80, 85, 90, 95)
	stats := &DiskStats{
		UsedPercent:       60,
		InodesUsedPercent: 99,
	}

	if got := mon.CheckInodeLevel(stats); got != LevelNone {
		t.Fatalf("inode level = %s, want none when inode totals are unavailable", got)
	}
	if got := mon.CheckLevel(stats); got != LevelNone {
		t.Fatalf("combined level = %s, want none", got)
	}
}

func TestDiskMonitorCustomInodeThresholds(t *testing.T) {
	mon := NewDiskMonitorWithInodeThresholds(80, 85, 90, 95, 50, 60, 70, 80)
	stats := &DiskStats{
		UsedPercent:       10,
		InodesTotal:       100,
		InodesUsedPercent: 75,
	}

	if got := mon.CheckLevel(stats); got != LevelAggressive {
		t.Fatalf("combined level = %s, want aggressive", got)
	}
}

func TestInodeUsedPercentPrefersCountedInodes(t *testing.T) {
	if got := inodeUsedPercent(1000, 990, 0); got != 99 {
		t.Fatalf("inode used percent = %v, want 99", got)
	}
	if got := inodeUsedPercent(1000, 0, 42); got != 42 {
		t.Fatalf("inode used percent fallback = %v, want 42", got)
	}
	if got := inodeUsedPercent(0, 99, 42); got != 0 {
		t.Fatalf("inode used percent unavailable = %v, want 0", got)
	}
}

func TestCleanupLevelString(t *testing.T) {
	tests := []struct {
		level    CleanupLevel
		expected string
	}{
		{LevelNone, "none"},
		{LevelWarning, "warning"},
		{LevelModerate, "moderate"},
		{LevelAggressive, "aggressive"},
		{LevelCritical, "critical"},
		{CleanupLevel(99), "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			if got := tt.level.String(); got != tt.expected {
				t.Errorf("CleanupLevel(%d).String() = %v, want %v",
					tt.level, got, tt.expected)
			}
		})
	}
}

func TestDiskStats(t *testing.T) {
	// Test real disk stats (should not error on any platform)
	stats, err := GetRootDiskStats()
	if err != nil {
		t.Fatalf("GetRootDiskStats() failed: %v", err)
	}

	if stats.Path != "/" {
		t.Errorf("expected path '/', got '%s'", stats.Path)
	}

	if stats.Total == 0 {
		t.Error("expected non-zero Total")
	}

	if stats.UsedPercent < 0 || stats.UsedPercent > 100 {
		t.Errorf("UsedPercent %v out of range [0,100]", stats.UsedPercent)
	}

	if stats.FreePercent < 0 || stats.FreePercent > 100 {
		t.Errorf("FreePercent %v out of range [0,100]", stats.FreePercent)
	}
	if stats.InodesTotal > 0 {
		if stats.InodesUsedPercent < 0 || stats.InodesUsedPercent > 100 {
			t.Errorf("InodesUsedPercent %v out of range [0,100]", stats.InodesUsedPercent)
		}
		if stats.InodesFreePercent < 0 || stats.InodesFreePercent > 100 {
			t.Errorf("InodesFreePercent %v out of range [0,100]", stats.InodesFreePercent)
		}
	}

	// UsedPercent + FreePercent should be ~100
	total := stats.UsedPercent + stats.FreePercent
	if total < 99.9 || total > 100.1 {
		t.Errorf("UsedPercent + FreePercent = %v, expected ~100", total)
	}
}

func TestDiskMonitorCheck(t *testing.T) {
	mon := NewDiskMonitor(80, 85, 90, 95)

	stats, level, err := mon.Check("/")
	if err != nil {
		t.Fatalf("Check() failed: %v", err)
	}

	if stats == nil {
		t.Fatal("expected non-nil stats")
	}

	// Level should be consistent with CheckLevel
	expectedLevel := mon.CheckLevel(stats)
	if level != expectedLevel {
		t.Errorf("Check() level = %v, CheckLevel(stats) = %v", level, expectedLevel)
	}
}

func TestNewDiskMonitor(t *testing.T) {
	mon := NewDiskMonitor(70, 80, 90, 95)

	if mon.ThresholdWarning != 70 {
		t.Errorf("ThresholdWarning = %v, want 70", mon.ThresholdWarning)
	}
	if mon.ThresholdModerate != 80 {
		t.Errorf("ThresholdModerate = %v, want 80", mon.ThresholdModerate)
	}
	if mon.ThresholdAggressive != 90 {
		t.Errorf("ThresholdAggressive = %v, want 90", mon.ThresholdAggressive)
	}
	if mon.ThresholdCritical != 95 {
		t.Errorf("ThresholdCritical = %v, want 95", mon.ThresholdCritical)
	}
	if mon.ThresholdInodeWarning != 70 {
		t.Errorf("ThresholdInodeWarning = %v, want 70", mon.ThresholdInodeWarning)
	}
	if mon.ThresholdInodeModerate != 80 {
		t.Errorf("ThresholdInodeModerate = %v, want 80", mon.ThresholdInodeModerate)
	}
	if mon.ThresholdInodeAggressive != 90 {
		t.Errorf("ThresholdInodeAggressive = %v, want 90", mon.ThresholdInodeAggressive)
	}
	if mon.ThresholdInodeCritical != 95 {
		t.Errorf("ThresholdInodeCritical = %v, want 95", mon.ThresholdInodeCritical)
	}
}
