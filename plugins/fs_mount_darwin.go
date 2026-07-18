//go:build darwin

package plugins

import (
	"fmt"
	"path/filepath"
	"syscall"
)

func platformMountID(path string) (string, error) {
	const mntNowait = 2
	n, err := syscall.Getfsstat(nil, mntNowait)
	if err != nil {
		return "", err
	}
	stats := make([]syscall.Statfs_t, n)
	n, err = syscall.Getfsstat(stats, mntNowait)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(path)
	bestPoint := ""
	for i := 0; i < n; i++ {
		point := int8String(stats[i].Mntonname[:])
		if pathWithin(point, clean) && len(point) > len(bestPoint) {
			bestPoint = point
		}
	}
	if bestPoint == "" {
		return "", fmt.Errorf("no mount identity for %s", path)
	}
	return bestPoint, nil
}

func int8String(value []int8) string {
	b := make([]byte, 0, len(value))
	for _, c := range value {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}
