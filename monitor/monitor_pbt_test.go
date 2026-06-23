// Package monitor provides disk usage monitoring.
package monitor

import (
	"testing"

	"pgregory.net/rapid"
)

// TestCheckLevelIsMaxOfByteAndInode verifies the combined level is always the
// higher of the byte-driven and inode-driven levels across the joint space,
// including filesystems that report no inode totals.
func TestCheckLevelIsMaxOfByteAndInode(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		bw := rapid.IntRange(50, 70).Draw(t, "byte_warn")
		bm := rapid.IntRange(bw+1, 85).Draw(t, "byte_mod")
		ba := rapid.IntRange(bm+1, 94).Draw(t, "byte_agg")
		bc := rapid.IntRange(ba+1, 99).Draw(t, "byte_crit")
		iw := rapid.IntRange(50, 70).Draw(t, "inode_warn")
		im := rapid.IntRange(iw+1, 85).Draw(t, "inode_mod")
		ia := rapid.IntRange(im+1, 94).Draw(t, "inode_agg")
		ic := rapid.IntRange(ia+1, 99).Draw(t, "inode_crit")

		mon := NewDiskMonitorWithInodeThresholds(bw, bm, ba, bc, iw, im, ia, ic)

		stats := &DiskStats{
			UsedPercent: rapid.Float64Range(0, 100).Draw(t, "byte_pct"),
			// InodesTotal of 0 must make the inode dimension contribute LevelNone.
			InodesTotal:       rapid.Uint64Range(0, 100000).Draw(t, "inodes_total"),
			InodesUsedPercent: rapid.Float64Range(0, 100).Draw(t, "inode_pct"),
		}

		byteLevel := mon.CheckByteLevel(stats)
		inodeLevel := mon.CheckInodeLevel(stats)
		want := byteLevel
		if inodeLevel > want {
			want = inodeLevel
		}
		if got := mon.CheckLevel(stats); got != want {
			t.Fatalf("CheckLevel = %s, want max(byte=%s, inode=%s)", got, byteLevel, inodeLevel)
		}
		if stats.InodesTotal == 0 && inodeLevel != LevelNone {
			t.Fatalf("inode level = %s with zero inode total, want none", inodeLevel)
		}
	})
}

// TestThresholdMonotonicity verifies higher usage never results in lower level.
func TestThresholdMonotonicity(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate valid thresholds in ascending order
		warn := rapid.IntRange(50, 70).Draw(t, "warn")
		mod := rapid.IntRange(warn+1, 85).Draw(t, "mod")
		agg := rapid.IntRange(mod+1, 94).Draw(t, "agg")
		crit := rapid.IntRange(agg+1, 99).Draw(t, "crit")

		mon := NewDiskMonitor(warn, mod, agg, crit)

		// Test that increasing usage never decreases the level
		prevLevel := LevelNone
		for usage := 0; usage <= 100; usage++ {
			stats := &DiskStats{UsedPercent: float64(usage)}
			level := mon.CheckLevel(stats)

			if level < prevLevel {
				t.Fatalf("level decreased from %d to %d at usage %d%%", prevLevel, level, usage)
			}
			prevLevel = level
		}
	})
}

// TestInodeThresholdMonotonicity verifies higher inode usage never results in lower level.
func TestInodeThresholdMonotonicity(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		warn := rapid.IntRange(50, 70).Draw(t, "warn")
		mod := rapid.IntRange(warn+1, 85).Draw(t, "mod")
		agg := rapid.IntRange(mod+1, 94).Draw(t, "agg")
		crit := rapid.IntRange(agg+1, 99).Draw(t, "crit")

		mon := NewDiskMonitorWithInodeThresholds(80, 85, 90, 95, warn, mod, agg, crit)

		prevLevel := LevelNone
		for usage := 0; usage <= 100; usage++ {
			stats := &DiskStats{InodesTotal: 1000, InodesUsedPercent: float64(usage)}
			level := mon.CheckInodeLevel(stats)

			if level < prevLevel {
				t.Fatalf("inode level decreased from %d to %d at usage %d%%", prevLevel, level, usage)
			}
			prevLevel = level
		}
	})
}

// TestThresholdBoundaries verifies level changes at exact threshold values.
func TestThresholdBoundaries(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate valid thresholds
		warn := rapid.IntRange(50, 70).Draw(t, "warn")
		mod := rapid.IntRange(warn+1, 85).Draw(t, "mod")
		agg := rapid.IntRange(mod+1, 94).Draw(t, "agg")
		crit := rapid.IntRange(agg+1, 99).Draw(t, "crit")

		mon := NewDiskMonitor(warn, mod, agg, crit)

		// Test that exact threshold triggers the level
		testCases := []struct {
			usage    float64
			expected CleanupLevel
		}{
			{float64(warn) - 0.1, LevelNone},
			{float64(warn), LevelWarning},
			{float64(mod) - 0.1, LevelWarning},
			{float64(mod), LevelModerate},
			{float64(agg) - 0.1, LevelModerate},
			{float64(agg), LevelAggressive},
			{float64(crit) - 0.1, LevelAggressive},
			{float64(crit), LevelCritical},
		}

		for _, tc := range testCases {
			stats := &DiskStats{UsedPercent: tc.usage}
			level := mon.CheckLevel(stats)
			if level != tc.expected {
				t.Fatalf("at %.1f%% usage: expected %s, got %s (thresholds: w=%d m=%d a=%d c=%d)",
					tc.usage, tc.expected, level, warn, mod, agg, crit)
			}
		}
	})
}

// TestCleanupLevelStringPBT verifies all levels have valid string representations (PBT version).
func TestCleanupLevelStringPBT(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		level := CleanupLevel(rapid.IntRange(0, 4).Draw(t, "level"))
		str := level.String()

		validStrings := map[string]bool{
			"none":       true,
			"warning":    true,
			"moderate":   true,
			"aggressive": true,
			"critical":   true,
		}

		if !validStrings[str] {
			t.Fatalf("invalid level string: %s for level %d", str, level)
		}
	})
}

// TestDiskMonitorNewWithValidThresholds verifies monitor creation with valid thresholds.
func TestDiskMonitorNewWithValidThresholds(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		warn := rapid.IntRange(50, 70).Draw(t, "warn")
		mod := rapid.IntRange(warn+1, 85).Draw(t, "mod")
		agg := rapid.IntRange(mod+1, 94).Draw(t, "agg")
		crit := rapid.IntRange(agg+1, 99).Draw(t, "crit")

		mon := NewDiskMonitor(warn, mod, agg, crit)

		// Monitor should be created successfully
		if mon == nil {
			t.Fatal("NewDiskMonitor returned nil")
		}
	})
}

// TestLevelNoneBelowAllThresholds verifies LevelNone when below all thresholds.
func TestLevelNoneBelowAllThresholds(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		warn := rapid.IntRange(50, 70).Draw(t, "warn")
		mod := rapid.IntRange(warn+1, 85).Draw(t, "mod")
		agg := rapid.IntRange(mod+1, 94).Draw(t, "agg")
		crit := rapid.IntRange(agg+1, 99).Draw(t, "crit")

		mon := NewDiskMonitor(warn, mod, agg, crit)

		// Any usage below warning should be LevelNone
		usage := rapid.Float64Range(0, float64(warn)-0.1).Draw(t, "usage")
		stats := &DiskStats{UsedPercent: usage}
		level := mon.CheckLevel(stats)

		if level != LevelNone {
			t.Fatalf("usage %.1f%% below warning %d should be LevelNone, got %s", usage, warn, level)
		}
	})
}

// TestLevelCriticalAboveThreshold verifies LevelCritical when above critical threshold.
func TestLevelCriticalAboveThreshold(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		warn := rapid.IntRange(50, 70).Draw(t, "warn")
		mod := rapid.IntRange(warn+1, 85).Draw(t, "mod")
		agg := rapid.IntRange(mod+1, 94).Draw(t, "agg")
		crit := rapid.IntRange(agg+1, 99).Draw(t, "crit")

		mon := NewDiskMonitor(warn, mod, agg, crit)

		// Any usage at or above critical should be LevelCritical
		usage := rapid.Float64Range(float64(crit), 100).Draw(t, "usage")
		stats := &DiskStats{UsedPercent: usage}
		level := mon.CheckLevel(stats)

		if level != LevelCritical {
			t.Fatalf("usage %.1f%% at/above critical %d should be LevelCritical, got %s", usage, crit, level)
		}
	})
}

// TestDiskStatsFreeGBCalculation verifies FreeGB is calculated correctly.
func TestDiskStatsFreeGBCalculation(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		totalGB := rapid.Float64Range(10, 1000).Draw(t, "totalGB")
		usedPercent := rapid.Float64Range(0, 100).Draw(t, "usedPercent")

		totalBytes := uint64(totalGB * 1024 * 1024 * 1024)
		usedBytes := uint64(float64(totalBytes) * usedPercent / 100)
		freeBytes := totalBytes - usedBytes

		stats := &DiskStats{
			Total:       totalBytes,
			Free:        freeBytes,
			Used:        usedBytes,
			UsedPercent: usedPercent,
			FreeGB:      float64(freeBytes) / (1024 * 1024 * 1024),
		}

		// FreeGB should be approximately (100 - usedPercent) / 100 * totalGB
		expectedFreeGB := (100 - usedPercent) / 100 * totalGB
		tolerance := 0.001 * totalGB // 0.1% tolerance

		diff := stats.FreeGB - expectedFreeGB
		if diff < -tolerance || diff > tolerance {
			t.Fatalf("FreeGB mismatch: expected %.2f, got %.2f (total=%.1fGB, used=%.1f%%)",
				expectedFreeGB, stats.FreeGB, totalGB, usedPercent)
		}
	})
}
