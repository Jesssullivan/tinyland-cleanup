//go:build !linux

package plugins

import (
	"errors"
	"syscall"
)

func homeLanesInodeOf(st *syscall.Stat_t) homeLanesInode {
	return homeLanesInode{ino: uint64(st.Ino)}
}

// homeLanesReadLockedInodes has no lock table off Linux, which keeps every
// tree. The home_lanes plugin is Linux-only.
func homeLanesReadLockedInodes() (map[homeLanesInode]bool, error) {
	return nil, errors.New("no /proc/locks on this platform")
}
