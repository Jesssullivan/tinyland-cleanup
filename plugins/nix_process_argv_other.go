//go:build !darwin && !linux

package plugins

import (
	"fmt"
	"runtime"
)

func nixProcessArgv(pid int) ([]string, error) {
	return nil, fmt.Errorf("read argv for process %d: unsupported operating system %s", pid, runtime.GOOS)
}
