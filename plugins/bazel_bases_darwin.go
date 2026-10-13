//go:build darwin

package plugins

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func bbStatfsUsage(path string) (bbUsage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return bbUsage{}, err
	}
	bsize := uint64(st.Bsize)
	used := (st.Blocks - st.Bfree) * bsize
	total := used + st.Bavail*bsize
	u := bbUsage{BytesTotal: total, BytesUsed: used, InodesTotal: st.Files}
	if st.Files >= st.Ffree {
		u.InodesUsed = st.Files - st.Ffree
	}
	var name []byte
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	u.FSType = string(name)
	// APFS and ZFS have no fixed inode table; only bytes count there.
	u.InodesCounted = st.Files > 0 && u.FSType != "apfs" && u.FSType != "zfs"
	return u, nil
}

func bbAccessTime(info os.FileInfo) (time.Time, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(int64(st.Atimespec.Sec), int64(st.Atimespec.Nsec)), true
}

// bbScanActivity on Darwin: command lines through kern.procargs2 for every
// process, and one lsof listing as the open-file inventory (cwd, txt, fds,
// mappings). There is no /proc, so lsof is required: without it, or when it
// fails, the inventory is incomplete and every base is kept.
func bbScanActivity(ctx context.Context, opts bbActivityOptions) *bbActivity {
	a := newBBActivity()
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		a.incomplete = append(a.incomplete, "cannot list processes: "+err.Error())
		return a
	}
	self := os.Getpid()
	for _, kp := range procs {
		pid := int(kp.Proc.P_pid)
		if pid <= 0 || pid == self {
			continue
		}
		argv, err := nixProcessArgv(pid)
		cmdline := ""
		if err == nil {
			cmdline = strings.Join(argv, " ")
		}
		a.procs = append(a.procs, bbProc{pid: pid, cmdline: cmdline})
	}
	if opts.Lsof == "" {
		a.lsofError = "lsof is not configured; Darwin has no other open-file inventory"
		return a
	}
	if !opts.UseLsof {
		// Darwin always needs the listing; UseLsof only ever disables the
		// Linux second witness.
		opts.UseLsof = true
	}
	bbLsofIndex(ctx, a, opts)
	return a
}

// bbLockHolder probes Bazel's lock without holding it: F_GETLK reports a
// conflicting POSIX lock, and a non-blocking flock is taken and released at
// once. Both must report free.
func bbLockHolder(lockPath string) (string, error) {
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &lk); err != nil {
		return "", err
	}
	if lk.Type != syscall.F_UNLCK {
		return fmt.Sprintf("its Bazel lock is held (fcntl, pid %d): a Bazel command is running", lk.Pid), nil
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return "its Bazel lock is held (flock): a Bazel command is running", nil
		}
		return "", err
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return "", nil
}

// bbHoldsFlock: on Darwin flock and POSIX locks can interact, so the reaper
// probes flock first and then holds the POSIX lock alone.
const bbHoldsFlock = false

func (l *bbPosixLock) recheckPlatform() string { return "" }

func bbMountPoints() ([]string, error) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	buf := make([]unix.Statfs_t, n+8)
	n, err = unix.Getfsstat(buf, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	points := make([]string, 0, n)
	for _, st := range buf[:n] {
		var name []byte
		for _, c := range st.Mntonname {
			if c == 0 {
				break
			}
			name = append(name, byte(c))
		}
		points = append(points, string(name))
	}
	return points, nil
}
