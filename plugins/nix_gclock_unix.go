//go:build unix

package plugins

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// openLockFile opens path without following a symlink and checks it is a
// regular file the daemon user owns. private also refuses group or other
// access. create makes a missing file with perm.
func openLockFile(path, label string, create bool, perm uint32, private bool) (*os.File, error) {
	flags := syscall.O_RDWR | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	if create {
		flags |= syscall.O_CREAT
	}
	fd, err := syscall.Open(path, flags, perm)
	if err != nil {
		return nil, fmt.Errorf("open %s %s: %w", label, path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("stat %s %s: %w", label, path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case !info.Mode().IsRegular():
		file.Close()
		return nil, fmt.Errorf("%s %s is not a regular file", label, path)
	case !ok || int(st.Uid) != os.Getuid():
		file.Close()
		return nil, fmt.Errorf("%s %s is not owned by this user", label, path)
	case private && info.Mode().Perm()&0o077 != 0:
		file.Close()
		return nil, fmt.Errorf("%s %s is readable or writable by others", label, path)
	}
	return file, nil
}

func lockBusy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EACCES)
}

// nixFlockFile opens path without following a symlink, checks it is a
// private regular file the daemon user owns, and takes a non-blocking
// exclusive flock(2) on it. flock is BSD advisory locking on both Linux and
// Darwin, the same primitive Python's fcntl.flock takes, so a Python
// collector that locks the same file excludes this pass and is excluded by
// it. The kernel releases the lock if the daemon dies.
func nixFlockFile(path string) (func(), error) {
	file, err := openLockFile(path, "nix GC lock", true, 0o600, true)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if lockBusy(err) {
			return nil, errNixGCLockHeld
		}
		return nil, fmt.Errorf("lock nix GC lock %s: %w", path, err)
	}
	return func() { file.Close() }, nil
}

// bazelGCLockTry takes Bazel's repository contents gc_lock exclusively
// without blocking, as Bazel's own GC does with tryGet. Bazel commands hold
// it shared, so a busy lock means a Bazel command is using the cache. Both a
// flock(2) lock and a POSIX record lock are taken, because a JVM lock on
// Linux is an fcntl record lock and those two kinds do not see each other.
// create makes a missing lock file (apply); a plan probes only an existing
// one and reports a missing file as free.
func bazelGCLockTry(path string, create bool) (func(), error) {
	file, err := openLockFile(path, "Bazel gc_lock", create, 0o644, false)
	if err != nil {
		if !create && errors.Is(err, os.ErrNotExist) {
			return func() {}, nil
		}
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if lockBusy(err) {
			return nil, errBazelGCLockHeld
		}
		return nil, fmt.Errorf("flock Bazel gc_lock %s: %w", path, err)
	}
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := syscall.FcntlFlock(file.Fd(), syscall.F_SETLK, &lk); err != nil {
		file.Close()
		if lockBusy(err) {
			return nil, errBazelGCLockHeld
		}
		return nil, fmt.Errorf("fcntl lock Bazel gc_lock %s: %w", path, err)
	}
	return func() { file.Close() }, nil
}
