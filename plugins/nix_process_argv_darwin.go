//go:build darwin

package plugins

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func nixProcessArgv(pid int) ([]string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		if nixDarwinProcessMissing(pid, err) {
			return nil, fmt.Errorf("read argv for process %d: %w", pid, os.ErrNotExist)
		}
		return nil, fmt.Errorf("read argv for process %d: %w", pid, err)
	}

	argv, err := decodeNixDarwinProcessArgv(raw)
	if err != nil {
		return nil, fmt.Errorf("read argv for process %d: %w", pid, err)
	}
	return argv, nil
}

func nixDarwinProcessMissing(pid int, err error) bool {
	if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT) {
		return true
	}
	if pid <= 0 || !errors.Is(err, unix.EINVAL) {
		return false
	}

	// kern.procargs2 returns EINVAL when proc_find cannot find the PID, for a
	// zombie, and for any process owned by another user (issue #160). A
	// missing PID and a zombie have both exited and hold no store paths; an
	// existing, live process keeps its EINVAL for the caller to classify.
	if errors.Is(unix.Kill(pid, 0), unix.ESRCH) {
		return true
	}
	return nixDarwinProcessZombie(pid)
}

// nixDarwinSZomb is XNU's SZOMB process state (bsd/sys/proc.h).
const nixDarwinSZomb = 5

func nixDarwinProcessZombie(pid int) bool {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || info == nil {
		return false
	}
	return int(info.Proc.P_pid) == pid && info.Proc.P_stat == nixDarwinSZomb
}

func decodeNixDarwinProcessArgv(raw []byte) ([]string, error) {
	const argcSize = 4
	if len(raw) < argcSize {
		return nil, fmt.Errorf("malformed kern.procargs2 data: missing argc")
	}

	// Darwin's supported Go architectures are little-endian and use a 32-bit C int.
	argc := int(int32(binary.LittleEndian.Uint32(raw[:argcSize])))
	if argc < 0 || argc > len(raw)-argcSize {
		return nil, fmt.Errorf("malformed kern.procargs2 data: invalid argc %d", argc)
	}

	cursor := raw[argcSize:]
	executableEnd := bytes.IndexByte(cursor, 0)
	if executableEnd < 0 {
		return nil, fmt.Errorf("malformed kern.procargs2 data: executable path is not NUL-terminated")
	}
	cursor = cursor[executableEnd+1:]

	// XNU pads between the executable path and argv. Once argv starts, every
	// NUL is significant, including adjacent NULs for empty arguments.
	for len(cursor) > 0 && cursor[0] == 0 {
		cursor = cursor[1:]
	}

	argv := make([]string, 0, argc)
	for index := 0; index < argc; index++ {
		argumentEnd := bytes.IndexByte(cursor, 0)
		if argumentEnd < 0 {
			return nil, fmt.Errorf("malformed kern.procargs2 data: argv[%d] is not NUL-terminated", index)
		}
		argv = append(argv, string(cursor[:argumentEnd]))
		cursor = cursor[argumentEnd+1:]
	}

	return argv, nil
}
