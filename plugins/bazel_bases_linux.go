//go:build linux

package plugins

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Linux file system magic numbers for the inode dimension of pressure.
const (
	bbMagicXFS   = 0x58465342
	bbMagicExt   = 0xEF53
	bbMagicZFS   = 0x2fc12fc1
	bbMagicBtrfs = 0x9123683e
	bbMagicTmpfs = 0x01021994
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
	switch uint64(st.Type) {
	case bbMagicXFS:
		// XFS f_files is the imaxpct ceiling; the 2026-10-10 sting ENOSPC
		// was this percentage running out, so it counts (R-C645).
		u.FSType, u.InodesCounted = "xfs", st.Files > 0
	case bbMagicExt:
		u.FSType, u.InodesCounted = "ext", st.Files > 0
	case bbMagicZFS:
		u.FSType = "zfs"
	case bbMagicBtrfs:
		u.FSType = "btrfs"
	case bbMagicTmpfs:
		u.FSType, u.InodesCounted = "tmpfs", st.Files > 0
	default:
		u.FSType = fmt.Sprintf("0x%x", uint64(st.Type))
		u.InodesCounted = st.Files > 0
	}
	return u, nil
}

func bbAccessTime(info os.FileInfo) (time.Time, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(int64(st.Atim.Sec), int64(st.Atim.Nsec)), true
}

func bbScanActivity(ctx context.Context, opts bbActivityOptions) *bbActivity {
	return bbScanProc(ctx, "/proc", os.Getpid(), os.Getuid(), opts)
}

// bbScanProc reads every process this user can see: command lines (world
// readable) and, where readable, cwd, root, open fds and mappings. A process
// of this user whose fd table cannot be read makes the inventory incomplete,
// which keeps every base (R-C622 own_uninspectable).
func bbScanProc(ctx context.Context, procRoot string, self, uid int, opts bbActivityOptions) *bbActivity {
	a := newBBActivity()
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		a.incomplete = append(a.incomplete, "cannot list "+procRoot+": "+err.Error())
		return a
	}
	okComms := map[string]bool{}
	for _, c := range opts.OKComms {
		okComms[c] = true
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			a.incomplete = append(a.incomplete, "process scan interrupted")
			return a
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		d := filepath.Join(procRoot, e.Name())
		raw, _ := os.ReadFile(filepath.Join(d, "cmdline"))
		a.procs = append(a.procs, bbProc{pid: pid, cmdline: strings.TrimSpace(strings.ReplaceAll(string(raw), "\x00", " "))})
		for _, link := range []string{"cwd", "root"} {
			if t, err := os.Readlink(filepath.Join(d, link)); err == nil && t != "/" {
				a.index(t, fmt.Sprintf("pid %d %s is %s", pid, link, t), opts.Within)
			}
		}
		fds, ferr := os.ReadDir(filepath.Join(d, "fd"))
		if ferr != nil {
			if os.IsNotExist(ferr) {
				continue
			}
			if st, err := os.Stat(d); err == nil {
				if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) == uid {
					comm, state := bbProcStat(d)
					if state != "Z" && !okComms[comm] {
						a.incomplete = append(a.incomplete, fmt.Sprintf("pid %d (%s) fd unreadable: %v", pid, comm, ferr))
					}
				}
			}
			continue
		}
		for _, fd := range fds {
			if t, err := os.Readlink(filepath.Join(d, "fd", fd.Name())); err == nil && strings.HasPrefix(t, "/") {
				a.index(t, fmt.Sprintf("pid %d holds %s", pid, t), opts.Within)
			}
		}
		if f, err := os.Open(filepath.Join(d, "maps")); err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 64*1024), 1024*1024)
			for sc.Scan() {
				fields := strings.SplitN(sc.Text(), " ", 6)
				if len(fields) == 6 {
					path := strings.TrimSpace(fields[5])
					if strings.HasPrefix(path, "/") {
						a.index(path, fmt.Sprintf("pid %d maps %s", pid, path), opts.Within)
					}
				}
			}
			f.Close()
		}
	}
	if opts.UseLsof && opts.Lsof != "" {
		bbLsofIndex(ctx, a, opts)
	}
	return a
}

func bbProcStat(d string) (string, string) {
	raw, err := os.ReadFile(filepath.Join(d, "stat"))
	if err != nil {
		return "", ""
	}
	s := string(raw)
	open, close := strings.Index(s, "("), strings.LastIndex(s, ")")
	if open < 0 || close < open {
		return "", ""
	}
	rest := strings.Fields(s[close+1:])
	state := ""
	if len(rest) > 0 {
		state = rest[0]
	}
	return s[open+1 : close], state
}

var bbProcLockRe = regexp.MustCompile(`^([0-9a-fA-F]+):([0-9a-fA-F]+):(\d+)$`)

// bbLockHolder reads /proc/locks, which lists flock, POSIX and OFD locks of
// every process on the host whatever its user or namespace. An unreadable
// table is an error, which keeps the base.
func bbLockHolder(lockPath string) (string, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(lockPath, &st); err != nil {
		return "", err
	}
	return bbProcLocksHolder("/proc/locks", uint64(st.Dev), st.Ino, os.Getpid())
}

func bbProcLocksHolder(locksFile string, dev uint64, ino uint64, self int) (string, error) {
	f, err := os.Open(locksFile)
	if err != nil {
		return "", fmt.Errorf("%s unreadable: %w", locksFile, err)
	}
	defer f.Close()
	major, minor := bbDevMajor(dev), bbDevMinor(dev)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		for i, tok := range fields {
			m := bbProcLockRe.FindStringSubmatch(tok)
			if m == nil {
				continue
			}
			ma, _ := strconv.ParseUint(m[1], 16, 64)
			mi, _ := strconv.ParseUint(m[2], 16, 64)
			in, _ := strconv.ParseUint(m[3], 10, 64)
			if ma != major || mi != minor || in != ino {
				break
			}
			pid := 0
			if i > 0 {
				pid, _ = strconv.Atoi(fields[i-1])
			}
			if pid == self && self > 0 {
				break
			}
			kind := ""
			if len(fields) > 1 {
				kind = fields[1]
				if kind == "->" && len(fields) > 2 {
					kind = fields[2]
				}
			}
			return fmt.Sprintf("its Bazel lock is held (%s lock, pid %d): a Bazel command is running", kind, pid), nil
		}
	}
	return "", sc.Err()
}

func bbDevMajor(dev uint64) uint64 {
	return ((dev >> 8) & 0xfff) | ((dev >> 32) & 0xfffff000)
}

func bbDevMinor(dev uint64) uint64 {
	return (dev & 0xff) | ((dev >> 12) & 0xffffff00)
}

// bbHoldsFlock: Linux flock and POSIX locks are independent, so the reaper
// holds both while it renames a base.
const bbHoldsFlock = true

func (l *bbPosixLock) recheckPlatform() string {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(l.f.Fd()), &st); err != nil {
		return "lock state unknown: " + err.Error()
	}
	why, err := bbProcLocksHolder("/proc/locks", uint64(st.Dev), st.Ino, os.Getpid())
	if err != nil {
		return "lock state unknown: " + err.Error()
	}
	return why
}

func bbMountPoints() ([]string, error) {
	return bbReadMountinfo("/proc/self/mountinfo")
}

var bbOctalEscape = regexp.MustCompile(`\\([0-7]{3})`)

func bbReadMountinfo(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var points []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 {
			points = append(points, bbOctalEscape.ReplaceAllStringFunc(fields[4], func(m string) string {
				n, _ := strconv.ParseUint(m[1:], 8, 8)
				return string(rune(n))
			}))
		}
	}
	return points, sc.Err()
}
