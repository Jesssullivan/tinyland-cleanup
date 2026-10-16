//go:build linux

package plugins

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// nixClockTicks is USER_HZ, the unit of /proc/<pid>/stat starttime. Linux
// fixes it at 100 for userspace on every architecture lab runs.
const nixClockTicks = 100

func nixBootTime() (time.Time, error) {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "btime ") {
			secs, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(secs, 0), nil
		}
	}
	return time.Time{}, errors.New("no btime in /proc/stat")
}

// nixOwnProcs lists /proc entries owned by the daemon user.
func nixOwnProcs() ([]string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	uid := uint32(os.Getuid())
	var dirs []string
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		dir := filepath.Join("/proc", e.Name())
		info, err := os.Stat(dir)
		if err != nil {
			continue // exited
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid == uid {
			dirs = append(dirs, dir)
		}
	}
	return dirs, nil
}

func nixProcMatches(dir string, comms map[string]bool) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return false
	}
	args := bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0})
	for i, arg := range args {
		if i > 1 {
			break
		}
		if comms[filepath.Base(string(arg))] {
			return true
		}
	}
	comm, _ := procCommState(dir)
	return comms[comm]
}

func nixProcStart(dir string, boot time.Time) (time.Time, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return time.Time{}, false
	}
	s := string(raw)
	close := strings.LastIndex(s, ")")
	if close < 0 {
		return time.Time{}, false
	}
	rest := strings.Fields(s[close+1:])
	// Fields after the command: state is field 3, starttime field 22.
	if len(rest) < 20 {
		return time.Time{}, false
	}
	ticks, err := strconv.ParseInt(rest[19], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return boot.Add(time.Duration(ticks) * time.Second / nixClockTicks), true
}

// nixSessionStartTimes returns the start times of the daemon user's live
// session processes. Any process it cannot read is skipped only when it has
// already exited.
func nixSessionStartTimes(comms []string) ([]time.Time, error) {
	set := procCommSet(comms)
	boot, err := nixBootTime()
	if err != nil {
		return nil, err
	}
	dirs, err := nixOwnProcs()
	if err != nil {
		return nil, err
	}
	var starts []time.Time
	for _, dir := range dirs {
		if !nixProcMatches(dir, set) {
			continue
		}
		if start, ok := nixProcStart(dir, boot); ok {
			starts = append(starts, start)
		} else if _, err := os.Stat(dir); err == nil {
			return nil, errors.New("cannot read the start time of live session " + dir)
		}
	}
	return starts, nil
}

// nixLiveCwds returns the working directories of the daemon user's live
// processes. A process whose cwd cannot be read fails the inspection unless
// its command is in okComms (non-dumpable helpers such as the tailscaled SSH
// incubator, as dev_artifacts.temp_uninspectable_ok_comms).
func nixLiveCwds(okComms []string) ([]string, error) {
	ok := procCommSet(okComms)
	dirs, err := nixOwnProcs()
	if err != nil {
		return nil, err
	}
	var cwds []string
	for _, dir := range dirs {
		cwd, err := os.Readlink(filepath.Join(dir, "cwd"))
		if err != nil {
			if _, statErr := os.Stat(dir); statErr != nil {
				continue // exited
			}
			if comm, state := procCommState(dir); ok[comm] || state == "Z" {
				continue
			}
			return nil, errors.New("cannot read the cwd of " + dir)
		}
		cwds = append(cwds, filepath.Clean(strings.TrimSuffix(cwd, " (deleted)")))
	}
	return cwds, nil
}
