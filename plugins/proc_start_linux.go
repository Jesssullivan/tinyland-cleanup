//go:build linux

package plugins

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// procClockTicks is USER_HZ, which Linux fixes at 100 on every architecture
// this daemon builds for; Go has no sysconf without cgo.
const procClockTicks = 100

var procBootTimes sync.Map // procRoot -> time.Time

// procBootTime reads btime from <procRoot>/stat once per proc root.
func procBootTime(procRoot string) time.Time {
	if v, ok := procBootTimes.Load(procRoot); ok {
		return v.(time.Time)
	}
	raw, err := os.ReadFile(filepath.Join(procRoot, "stat"))
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64); err == nil {
				t := time.Unix(n, 0)
				procBootTimes.Store(procRoot, t)
				return t
			}
		}
	}
	return time.Time{}
}

// procStartTime is a process's start time from field 22 of
// <dir>/stat (clock ticks after boot). Zero when it cannot be read.
func procStartTime(procRoot, dir string) time.Time {
	boot := procBootTime(procRoot)
	if boot.IsZero() {
		return time.Time{}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return time.Time{}
	}
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return time.Time{}
	}
	fields := strings.Fields(s[i+1:])
	// fields[0] is field 3 (state), so field 22 is fields[19].
	if len(fields) < 20 {
		return time.Time{}
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return boot.Add(time.Duration(ticks) * time.Second / procClockTicks)
}
