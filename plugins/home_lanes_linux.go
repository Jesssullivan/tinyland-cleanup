//go:build linux

package plugins

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func homeLanesInodeOf(st *syscall.Stat_t) homeLanesInode {
	dev := uint64(st.Dev)
	return homeLanesInode{bbDevMajor(dev), bbDevMinor(dev), uint64(st.Ino)}
}

// homeLanesReadLockedInodes lists every file with a flock, POSIX or OFD lock,
// from any process in any namespace.
func homeLanesReadLockedInodes() (map[homeLanesInode]bool, error) {
	f, err := os.Open("/proc/locks")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return homeLanesParseLocks(bufio.NewScanner(f))
}

func homeLanesParseLocks(sc *bufio.Scanner) (map[homeLanesInode]bool, error) {
	locked := map[homeLanesInode]bool{}
	for sc.Scan() {
		for _, tok := range strings.Fields(sc.Text()) {
			m := bbProcLockRe.FindStringSubmatch(tok)
			if m == nil {
				continue
			}
			major, _ := strconv.ParseUint(m[1], 16, 64)
			minor, _ := strconv.ParseUint(m[2], 16, 64)
			ino, _ := strconv.ParseUint(m[3], 10, 64)
			locked[homeLanesInode{major, minor, ino}] = true
			break
		}
	}
	return locked, sc.Err()
}
