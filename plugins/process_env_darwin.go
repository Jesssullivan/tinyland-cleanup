//go:build darwin

package plugins

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"golang.org/x/sys/unix"
)

// darwinProcessEnv reads a process's initial environment from
// kern.procargs2, which XNU lays out as argc, the executable path, padding,
// argv and then the environment, each NUL-terminated. Processes of other
// users return EINVAL, so only this user's environments are visible.
func darwinProcessEnv(pid int) ([]string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, fmt.Errorf("read environment for process %d: %w", pid, err)
	}
	return decodeDarwinProcessEnv(raw)
}

// decodeDarwinProcessEnv returns the environment strings of a
// kern.procargs2 buffer. A truncated buffer returns what it holds.
func decodeDarwinProcessEnv(raw []byte) ([]string, error) {
	const argcSize = 4
	if len(raw) < argcSize {
		return nil, fmt.Errorf("malformed kern.procargs2 data: missing argc")
	}
	argc := int(int32(binary.LittleEndian.Uint32(raw[:argcSize])))
	if argc < 0 || argc > len(raw)-argcSize {
		return nil, fmt.Errorf("malformed kern.procargs2 data: invalid argc %d", argc)
	}
	cursor := raw[argcSize:]
	end := bytes.IndexByte(cursor, 0)
	if end < 0 {
		return nil, fmt.Errorf("malformed kern.procargs2 data: executable path is not NUL-terminated")
	}
	cursor = cursor[end+1:]
	for len(cursor) > 0 && cursor[0] == 0 {
		cursor = cursor[1:]
	}
	for i := 0; i < argc; i++ {
		end := bytes.IndexByte(cursor, 0)
		if end < 0 {
			return nil, fmt.Errorf("malformed kern.procargs2 data: argv[%d] is not NUL-terminated", i)
		}
		cursor = cursor[end+1:]
	}
	var env []string
	for len(cursor) > 0 {
		end := bytes.IndexByte(cursor, 0)
		if end <= 0 {
			break // an empty string ends the environment
		}
		env = append(env, string(cursor[:end]))
		cursor = cursor[end+1:]
	}
	return env, nil
}
