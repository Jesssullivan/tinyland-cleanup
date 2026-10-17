//go:build linux

package plugins

import "syscall"

// shmIsTmpfs reports whether path sits on a tmpfs (R-C856 covers RAM-backed
// scratch only).
func shmIsTmpfs(path string) (bool, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false, err
	}
	return uint64(st.Type) == bbMagicTmpfs, nil
}
