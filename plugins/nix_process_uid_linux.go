//go:build linux

package plugins

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// nixProcessUID reads a process's effective UID from /proc/<pid>/status. The
// kernel prints it unsigned, so a user-namespaced pod's host UID at or above
// 2^31 reads correctly where ps would print it negative.
func nixProcessUID(pid int) (uint32, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, fmt.Errorf("read status for process %d: %w", pid, err)
	}
	return parseNixProcStatusUID(raw)
}
