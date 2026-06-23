// Package monitor provides disk usage monitoring functionality.
package monitor

import (
	"github.com/shirou/gopsutil/v3/disk"
)

// DiskStats represents disk usage statistics.
type DiskStats struct {
	// Path is the mount point being monitored
	Path string
	// Total bytes on the disk
	Total uint64
	// Used bytes on the disk
	Used uint64
	// Free bytes on the disk
	Free uint64
	// UsedPercent is the percentage of disk used
	UsedPercent float64
	// FreePercent is the percentage of disk free
	FreePercent float64
	// FreeGB is free space in gigabytes
	FreeGB float64
	// InodesTotal is the total inode count reported by the filesystem
	InodesTotal uint64
	// InodesUsed is the number of allocated inodes
	InodesUsed uint64
	// InodesFree is the number of available inodes
	InodesFree uint64
	// InodesUsedPercent is the percentage of inodes used
	InodesUsedPercent float64
	// InodesFreePercent is the percentage of inodes free
	InodesFreePercent float64
}

// GetDiskStats returns disk statistics for the specified path.
func GetDiskStats(path string) (*DiskStats, error) {
	usage, err := disk.Usage(path)
	if err != nil {
		return nil, err
	}

	inodesUsed := usage.InodesUsed
	if usage.InodesTotal > 0 && usage.InodesFree <= usage.InodesTotal {
		inodesUsed = usage.InodesTotal - usage.InodesFree
	}
	inodesUsedPercent := inodeUsedPercent(usage.InodesTotal, inodesUsed, usage.InodesUsedPercent)

	return &DiskStats{
		Path:              path,
		Total:             usage.Total,
		Used:              usage.Used,
		Free:              usage.Free,
		UsedPercent:       usage.UsedPercent,
		FreePercent:       100.0 - usage.UsedPercent,
		FreeGB:            float64(usage.Free) / (1024 * 1024 * 1024),
		InodesTotal:       usage.InodesTotal,
		InodesUsed:        inodesUsed,
		InodesFree:        usage.InodesFree,
		InodesUsedPercent: inodesUsedPercent,
		InodesFreePercent: inodeFreePercent(usage.InodesTotal, inodesUsedPercent),
	}, nil
}

// GetRootDiskStats returns disk statistics for the root filesystem.
func GetRootDiskStats() (*DiskStats, error) {
	return GetDiskStats("/")
}

// DiskMonitor provides disk monitoring with threshold detection.
type DiskMonitor struct {
	// ThresholdWarning percentage for warning level
	ThresholdWarning float64
	// ThresholdModerate percentage for moderate level
	ThresholdModerate float64
	// ThresholdAggressive percentage for aggressive level
	ThresholdAggressive float64
	// ThresholdCritical percentage for critical level
	ThresholdCritical float64
	// ThresholdInodeWarning percentage for inode warning level
	ThresholdInodeWarning float64
	// ThresholdInodeModerate percentage for inode moderate level
	ThresholdInodeModerate float64
	// ThresholdInodeAggressive percentage for inode aggressive level
	ThresholdInodeAggressive float64
	// ThresholdInodeCritical percentage for inode critical level
	ThresholdInodeCritical float64
}

// NewDiskMonitor creates a new disk monitor with the specified thresholds.
func NewDiskMonitor(warning, moderate, aggressive, critical int) *DiskMonitor {
	return NewDiskMonitorWithInodeThresholds(warning, moderate, aggressive, critical, warning, moderate, aggressive, critical)
}

// NewDiskMonitorWithInodeThresholds creates a disk monitor with independent byte and inode thresholds.
func NewDiskMonitorWithInodeThresholds(warning, moderate, aggressive, critical, inodeWarning, inodeModerate, inodeAggressive, inodeCritical int) *DiskMonitor {
	if inodeWarning <= 0 {
		inodeWarning = warning
	}
	if inodeModerate <= 0 {
		inodeModerate = moderate
	}
	if inodeAggressive <= 0 {
		inodeAggressive = aggressive
	}
	if inodeCritical <= 0 {
		inodeCritical = critical
	}
	return &DiskMonitor{
		ThresholdWarning:         float64(warning),
		ThresholdModerate:        float64(moderate),
		ThresholdAggressive:      float64(aggressive),
		ThresholdCritical:        float64(critical),
		ThresholdInodeWarning:    float64(inodeWarning),
		ThresholdInodeModerate:   float64(inodeModerate),
		ThresholdInodeAggressive: float64(inodeAggressive),
		ThresholdInodeCritical:   float64(inodeCritical),
	}
}

// CleanupLevel represents the cleanup severity level needed.
type CleanupLevel int

const (
	// LevelNone means no cleanup needed
	LevelNone CleanupLevel = iota
	// LevelWarning triggers light cleanup
	LevelWarning
	// LevelModerate triggers moderate cleanup
	LevelModerate
	// LevelAggressive triggers aggressive cleanup
	LevelAggressive
	// LevelCritical triggers emergency cleanup
	LevelCritical
)

// String returns the string representation of the cleanup level.
func (l CleanupLevel) String() string {
	switch l {
	case LevelNone:
		return "none"
	case LevelWarning:
		return "warning"
	case LevelModerate:
		return "moderate"
	case LevelAggressive:
		return "aggressive"
	case LevelCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// CheckLevel determines the cleanup level needed based on disk usage.
func (m *DiskMonitor) CheckLevel(stats *DiskStats) CleanupLevel {
	byteLevel := m.CheckByteLevel(stats)
	inodeLevel := m.CheckInodeLevel(stats)
	if inodeLevel > byteLevel {
		return inodeLevel
	}
	return byteLevel
}

// CheckByteLevel determines the cleanup level needed based on byte usage.
func (m *DiskMonitor) CheckByteLevel(stats *DiskStats) CleanupLevel {
	return levelForPercent(stats.UsedPercent, m.ThresholdWarning, m.ThresholdModerate, m.ThresholdAggressive, m.ThresholdCritical)
}

// CheckInodeLevel determines the cleanup level needed based on inode usage.
func (m *DiskMonitor) CheckInodeLevel(stats *DiskStats) CleanupLevel {
	if stats.InodesTotal == 0 {
		return LevelNone
	}
	return levelForPercent(stats.InodesUsedPercent, m.ThresholdInodeWarning, m.ThresholdInodeModerate, m.ThresholdInodeAggressive, m.ThresholdInodeCritical)
}

func levelForPercent(usedPercent, warning, moderate, aggressive, critical float64) CleanupLevel {
	if usedPercent >= critical {
		return LevelCritical
	}
	if usedPercent >= aggressive {
		return LevelAggressive
	}
	if usedPercent >= moderate {
		return LevelModerate
	}
	if usedPercent >= warning {
		return LevelWarning
	}
	return LevelNone
}

func inodeFreePercent(total uint64, usedPercent float64) float64 {
	if total == 0 {
		return 0
	}
	return 100.0 - usedPercent
}

func inodeUsedPercent(total uint64, used uint64, reported float64) float64 {
	if total == 0 {
		return 0
	}
	if used > 0 {
		return float64(used) / float64(total) * 100
	}
	return reported
}

// Check performs a disk check and returns the current stats and required level.
func (m *DiskMonitor) Check(path string) (*DiskStats, CleanupLevel, error) {
	stats, err := GetDiskStats(path)
	if err != nil {
		return nil, LevelNone, err
	}
	return stats, m.CheckLevel(stats), nil
}
