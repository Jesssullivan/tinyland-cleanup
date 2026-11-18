//go:build linux

package plugins

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIndexSocketListenerTableMarksListeningPathsOnly(t *testing.T) {
	a := newBBActivity()
	table := "Num       RefCount Protocol Flags    Type St Inode Path\n" +
		"0000000000000000: 00000002 00000000 00010000 0001 01 1001 /r/job/agent.sock\n" +
		"0000000000000000: 00000003 00000000 00000000 0001 03 1002 /r/other/conn.sock\n" +
		"0000000000000000: 00000002 00000000 00010000 0001 01 1003 @abstract\n"
	indexSocketListenerTable(a, table, []string{"/r"})
	if len(a.blockers("/r/job")) == 0 {
		t.Fatal("a listening socket bound inside /r/job must hold it")
	}
	if len(a.blockers("/r/other")) != 0 {
		t.Fatal("a connected (not listening) socket must not hold /r/other")
	}
	b := newBBActivity()
	indexSocketListeners(b, filepath.Join(t.TempDir(), "missing"), nil)
	if b.complete() == "" {
		t.Fatal("an unreadable socket table must make the inventory incomplete")
	}
}

func TestProcStartTimeFromStat(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("cpu 1 2 3\nbtime 1700000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pid := filepath.Join(root, "42")
	if err := os.MkdirAll(pid, 0o755); err != nil {
		t.Fatal(err)
	}
	// Field 22 (starttime) is 500 ticks: 5 seconds after boot. The comm
	// holds a space and a parenthesis to test the parse.
	stat := "42 (ba sh)) S 1 42 42 0 -1 4194560 100 0 0 0 0 0 0 0 20 0 1 0 500 1000 10\n"
	if err := os.WriteFile(filepath.Join(pid, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	got := procStartTime(root, pid)
	if want := time.Unix(1700000005, 0); !got.Equal(want) {
		t.Fatalf("start = %v, want %v", got, want)
	}
}
