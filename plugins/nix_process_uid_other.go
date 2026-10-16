//go:build !linux

package plugins

import (
	"fmt"
	"runtime"
)

// nixProcessUID has no authoritative source outside Linux; callers fall back
// to the ps uid column.
func nixProcessUID(pid int) (uint32, error) {
	return 0, fmt.Errorf("read uid for process %d: unsupported operating system %s", pid, runtime.GOOS)
}
