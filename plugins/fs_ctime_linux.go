//go:build linux

package plugins

import (
	"os"
	"syscall"
	"time"
)

// changeTime returns the inode change time (ctime) when the platform exposes it.
func changeTime(info os.FileInfo) (time.Time, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(stat.Ctim.Sec, stat.Ctim.Nsec), true
}
