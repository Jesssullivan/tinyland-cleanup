//go:build darwin

package plugins

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func kinfo(pid int32, comm string, start int64) unix.KinfoProc {
	var kp unix.KinfoProc
	kp.Proc.P_pid = pid
	copy(kp.Proc.P_comm[:], comm)
	kp.Proc.P_starttime = unix.Timeval{Sec: start}
	return kp
}

func TestNixDarwinSessionStartTimesMatchesAndSkipsExited(t *testing.T) {
	procs := []unix.KinfoProc{
		kinfo(10, "claude", 1_700_000_000),
		kinfo(11, "zsh", 1_700_000_100),
		kinfo(12, "node", 1_700_000_200),
		kinfo(13, "codex", 1_700_000_300),
	}
	argv := map[int][]string{
		10: {"/nix/store/x-claude/bin/claude"},
		11: {"-zsh"},
		12: {"node", "/opt/codex/bin/codex"},
	}
	read := func(pid int) ([]string, error) {
		if pid == 13 {
			return nil, fmt.Errorf("read argv for process 13: %w", os.ErrNotExist)
		}
		return argv[pid], nil
	}
	starts, err := nixDarwinSessionStartTimes([]string{"claude", "codex"},
		func() ([]unix.KinfoProc, error) { return procs, nil }, read)
	if err != nil {
		t.Fatal(err)
	}
	if len(starts) != 2 || !starts[0].Equal(time.Unix(1_700_000_000, 0)) || !starts[1].Equal(time.Unix(1_700_000_200, 0)) {
		t.Fatalf("want the claude and node-run codex sessions, exited pid 13 skipped: %v", starts)
	}
}

func TestNixDarwinSessionStartTimesFailsOnAnUnreadableStart(t *testing.T) {
	procs := []unix.KinfoProc{kinfo(10, "claude", 0)}
	_, err := nixDarwinSessionStartTimes([]string{"claude"},
		func() ([]unix.KinfoProc, error) { return procs, nil },
		func(int) ([]string, error) { return []string{"claude"}, nil })
	if err == nil {
		t.Fatal("a live session with no start time must fail inspection, which keeps every pin")
	}
}

// The real process table: this test binary is a live session of its own name.
func TestNixSessionStartTimesSeesThisProcess(t *testing.T) {
	starts, err := nixSessionStartTimes([]string{filepath.Base(os.Args[0])})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, start := range starts {
		if start.After(now.Add(time.Minute)) || start.Before(now.Add(-24*time.Hour)) {
			t.Fatalf("implausible start time %v", start)
		}
	}
	if len(starts) == 0 {
		t.Fatal("the running test binary must be seen as a session")
	}
}
