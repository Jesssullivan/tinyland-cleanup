//go:build linux

package plugins

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

func nixProcessArgv(pid int) ([]string, error) {
	processDir := filepath.Join("/proc", strconv.Itoa(pid))
	raw, err := os.ReadFile(filepath.Join(processDir, "cmdline"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read argv for process %d: %w", pid, os.ErrNotExist)
		}
		return nil, fmt.Errorf("read argv for process %d: %w", pid, err)
	}

	if len(raw) == 0 {
		if _, err := os.Stat(processDir); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("read argv for process %d: %w", pid, os.ErrNotExist)
			}
			return nil, fmt.Errorf("read argv for process %d: %w", pid, err)
		}
		return []string{}, nil
	}

	return decodeNixLinuxProcessArgv(raw), nil
}

func decodeNixLinuxProcessArgv(raw []byte) []string {
	if len(raw) == 0 {
		return []string{}
	}
	if raw[len(raw)-1] == 0 {
		raw = raw[:len(raw)-1]
	}

	parts := bytes.Split(raw, []byte{0})
	argv := make([]string, len(parts))
	for index, part := range parts {
		argv[index] = string(part)
	}
	return argv
}
