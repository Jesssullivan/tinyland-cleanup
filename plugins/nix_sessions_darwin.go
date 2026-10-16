//go:build darwin

package plugins

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// nixSessionStartTimes returns the start times of the daemon user's live
// session processes on Darwin (lab R-C815), so a hook pin is released once no
// session that started while it was referenced is alive, as on Linux. The
// process table comes from sysctl kern.proc.uid (the user's own processes,
// with p_comm and p_starttime) and argv from kern.procargs2, the reader the
// busy-process check already uses. A process that exits mid-scan is skipped;
// a live session whose start time reads as zero fails the inspection, which
// keeps every pin.
func nixSessionStartTimes(comms []string) ([]time.Time, error) {
	return nixDarwinSessionStartTimes(comms, func() ([]unix.KinfoProc, error) {
		return unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	}, nixProcessArgv)
}

func nixDarwinSessionStartTimes(comms []string, list func() ([]unix.KinfoProc, error), readArgv func(int) ([]string, error)) ([]time.Time, error) {
	set := procCommSet(comms)
	procs, err := list()
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	var starts []time.Time
	for i := range procs {
		proc := &procs[i].Proc
		pid := int(proc.P_pid)
		if pid <= 0 {
			continue
		}
		comm := unix.ByteSliceToString(proc.P_comm[:])
		args, argErr := readArgv(pid)
		if errors.Is(argErr, os.ErrNotExist) {
			continue // exited
		}
		if !nixSessionMatches(args, comm, set) {
			continue
		}
		tv := proc.P_starttime
		if tv.Sec <= 0 {
			return nil, fmt.Errorf("cannot read the start time of live session %d", pid)
		}
		starts = append(starts, time.Unix(tv.Sec, int64(tv.Usec)*int64(time.Microsecond)))
	}
	return starts, nil
}

// nixLiveCwds needs each process's working directory, which Darwin exposes
// only through libproc. It fails here, which keeps every agent-tree root.
func nixLiveCwds(okComms []string) ([]string, error) {
	return nil, errors.New("working-directory inspection is not implemented on darwin")
}
