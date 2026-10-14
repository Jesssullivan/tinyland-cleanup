//go:build linux || darwin

package plugins

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
)

// bbPosixLock is Bazel's output-base lock while the reaper holds it.
type bbPosixLock struct {
	f *os.File
}

// bbTakeLock takes Bazel's own lock on <output_base>/lock without blocking:
// a POSIX write lock (fcntl F_SETLK; the Bazel client's primitive) and, where
// the two are independent (Linux), flock as well. While the reaper holds it a
// starting Bazel client waits for "another command" instead of finding its
// base gone. A held lock returns its reason and no error.
func bbTakeLock(lockPath string) (bbHeldLock, string, error) {
	if !bbHoldsFlock {
		// Darwin: probe flock on its own descriptor before the POSIX lock
		// exists, so the probe can never collide with the reaper's own lock.
		g, err := os.OpenFile(lockPath, os.O_RDWR, 0)
		if err != nil {
			return nil, "", err
		}
		if err := syscall.Flock(int(g.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			g.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, "its Bazel lock is held (flock): a Bazel command is running", nil
			}
			return nil, "", err
		}
		_ = syscall.Flock(int(g.Fd()), syscall.LOCK_UN)
		g.Close()
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		return nil, "", err
	}
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lk); err != nil {
		f.Close()
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EACCES) {
			return nil, "its Bazel lock is held (fcntl): a Bazel command is running", nil
		}
		return nil, "", err
	}
	if bbHoldsFlock {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, "its Bazel lock is held (flock): a Bazel command is running", nil
			}
			return nil, "", err
		}
	}
	return &bbPosixLock{f: f}, "", nil
}

// Recheck reports another process's lock seen through the held descriptor.
func (l *bbPosixLock) Recheck() string {
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := syscall.FcntlFlock(l.f.Fd(), syscall.F_GETLK, &lk); err != nil {
		return "lock state unknown: " + err.Error()
	}
	if lk.Type != syscall.F_UNLCK {
		return fmt.Sprintf("its Bazel lock is held (fcntl, pid %d)", lk.Pid)
	}
	return l.recheckPlatform()
}

// Release drops every lock by closing the descriptor.
func (l *bbPosixLock) Release() {
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}

// bbLsofIndex adds one full lsof listing (no +D) to the inventory. A listing
// that fails with no output makes the inventory incomplete.
func bbLsofIndex(ctx context.Context, a *bbActivity, opts bbActivityOptions) {
	timeout := opts.LsofTimeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := commandContext(cctx, opts.Lsof, "-w", "-nP", "-Fpn")
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		a.lsofError = fmt.Sprintf("lsof listing failed: %v", err)
		return
	}
	if cctx.Err() != nil {
		a.lsofError = "lsof listing timed out"
		return
	}
	self := fmt.Sprintf("p%d", os.Getpid())
	skip := false
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "p") {
			skip = line == self
			continue
		}
		if skip || !strings.HasPrefix(line, "n/") {
			continue
		}
		a.index(line[1:], "lsof lists "+line[1:], opts.Within)
	}
}
