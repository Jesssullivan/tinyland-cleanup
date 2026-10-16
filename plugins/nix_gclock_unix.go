//go:build unix

package plugins

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// nixFlockFile opens path without following a symlink, checks it is a
// private regular file the daemon user owns, and takes a non-blocking
// exclusive flock(2) on it. flock is BSD advisory locking on both Linux and
// Darwin, the same primitive Python's fcntl.flock takes, so a Python
// collector that locks the same file excludes this pass and is excluded by
// it. The kernel releases the lock if the daemon dies.
func nixFlockFile(path string) (func(), error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open nix GC lock %s: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("stat nix GC lock %s: %w", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case !info.Mode().IsRegular():
		file.Close()
		return nil, fmt.Errorf("nix GC lock %s is not a regular file", path)
	case !ok || int(st.Uid) != os.Getuid():
		file.Close()
		return nil, fmt.Errorf("nix GC lock %s is not owned by this user", path)
	case info.Mode().Perm()&0o077 != 0:
		file.Close()
		return nil, fmt.Errorf("nix GC lock %s is readable or writable by others", path)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errNixGCLockHeld
		}
		return nil, fmt.Errorf("lock nix GC lock %s: %w", path, err)
	}
	return func() { file.Close() }, nil
}
