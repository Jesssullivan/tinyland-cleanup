//go:build linux

package plugins

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Liveness fixtures with real processes: a live server, a held lock (flock
// and fcntl, seen through /proc/locks), an open file and a working directory
// under the base. Each is kept; the same base is reaped once the holder exits.

// TestBBHelperProcess is not a test: it is the holder process the fixtures
// below start. BB_HELPER selects what it holds.
func TestBBHelperProcess(t *testing.T) {
	mode := os.Getenv("BB_HELPER")
	if mode == "" {
		return
	}
	path := os.Getenv("BB_HELPER_PATH")
	var keep []*os.File
	switch mode {
	case "flock", "fcntl":
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			os.Exit(2)
		}
		if mode == "flock" {
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
				os.Exit(3)
			}
		} else {
			lk := syscall.Flock_t{Type: syscall.F_WRLCK}
			if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLKW, &lk); err != nil {
				os.Exit(3)
			}
		}
		keep = append(keep, f)
	case "open":
		f, err := os.Open(path)
		if err != nil {
			os.Exit(2)
		}
		keep = append(keep, f)
	case "cwd":
		if err := os.Chdir(path); err != nil {
			os.Exit(2)
		}
	case "server":
		// argv carries --output_base=<base> after "--", like a Bazel server.
	}
	os.Stdout.WriteString("ready\n")
	time.Sleep(10 * time.Minute)
	_ = keep
	os.Exit(0)
}

func startBBHelper(t *testing.T, mode, path string, extra ...string) *exec.Cmd {
	t.Helper()
	args := append([]string{"-test.run=^TestBBHelperProcess$", "--"}, extra...)
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "BB_HELPER="+mode, "BB_HELPER_PATH="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("helper %s did not become ready: %q %v", mode, line, err)
	}
	return cmd
}

func stopBBHelper(cmd *exec.Cmd) {
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}

// realProcActivity is the real /proc inventory, with an impossible uid so a
// host process of the test user with an unreadable fd table cannot turn
// every fixture into an incomplete-inventory refusal.
func realProcActivity(ctx context.Context, opts bbActivityOptions) *bbActivity {
	return bbScanProc(ctx, "/proc", os.Getpid(), -2, opts)
}

func TestBazelBasesKeepsBaseWithLiveServer(t *testing.T) {
	f := newBBFixture(t)
	base := filepath.Join(f.root, "a", "out")
	bbMakeBase(t, base, 4096, 30*time.Hour)
	cmd := startBBHelper(t, "server", "", "--output_base="+base)
	bbWrite(t, filepath.Join(base, "server", "server.pid.txt"), 0)
	if err := os.WriteFile(filepath.Join(base, "server", "server.pid.txt"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	bbAge(t, base, 30*time.Hour)
	// Only the pid file names the server: no inventory, lock free.
	cycle := f.run()
	if !bbExists(base) {
		t.Fatalf("a base with a live server was reaped: %v", bbRowPaths(cycle))
	}
	if r := bbRowFor(t, cycle, base); !r.Live || !strings.Contains(r.Reason, "server pid") {
		t.Fatalf("kept for the wrong reason: %s", r.Reason)
	}
	stopBBHelper(cmd)
	f.run()
	if bbExists(base) {
		t.Fatal("the base was not reaped once its server exited")
	}
}

func TestBazelBasesKeepsBaseWithHeldLock(t *testing.T) {
	for _, mode := range []string{"flock", "fcntl"} {
		t.Run(mode, func(t *testing.T) {
			f := newBBFixture(t)
			base := filepath.Join(f.root, "a", "out")
			bbMakeBase(t, base, 4096, 30*time.Hour)
			cmd := startBBHelper(t, mode, filepath.Join(base, "lock"))
			bbAge(t, base, 30*time.Hour)
			// The open-file inventory stays empty: only the lock can keep it.
			cycle := f.run()
			if !bbExists(base) {
				t.Fatalf("a base whose lock is held (%s) was reaped: %v", mode, bbRowPaths(cycle))
			}
			if r := bbRowFor(t, cycle, base); !strings.Contains(r.Reason, "lock is held") {
				t.Fatalf("kept for the wrong reason: %s", r.Reason)
			}
			stopBBHelper(cmd)
			f.run()
			if bbExists(base) {
				t.Fatal("the base was not reaped once its lock was released")
			}
		})
	}
}

func TestBazelBasesKeepsBaseWithOpenFileOrCwd(t *testing.T) {
	for _, mode := range []string{"open", "cwd"} {
		t.Run(mode, func(t *testing.T) {
			f := newBBFixture(t)
			f.plugin.env.activity = realProcActivity
			base := filepath.Join(f.root, "a", "out")
			bbMakeBase(t, base, 4096, 30*time.Hour)
			target := filepath.Join(base, "execroot", "_main", "bazel-out", "out.bin")
			if mode == "cwd" {
				target = filepath.Join(base, "execroot", "_main")
			}
			cmd := startBBHelper(t, mode, target)
			bbAge(t, base, 30*time.Hour)
			cycle := f.run()
			if !bbExists(base) {
				t.Fatalf("a base with an open %s was reaped: %v", mode, bbRowPaths(cycle))
			}
			if r := bbRowFor(t, cycle, base); !r.Live || !strings.Contains(r.Reason, "pid "+strconv.Itoa(cmd.Process.Pid)) {
				t.Fatalf("kept for the wrong reason: %s", r.Reason)
			}
			stopBBHelper(cmd)
			f.run()
			if bbExists(base) {
				t.Fatal("the base was not reaped once nothing held it")
			}
		})
	}
}

func TestBazelBasesKeepsGitTrackedBase(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	f := newBBFixture(t)
	repo := filepath.Join(f.root, "repo")
	base := filepath.Join(repo, "testdata", "ob")
	bbMakeBase(t, base, 4096, 30*time.Hour)
	run := func(args ...string) {
		cmd := exec.Command(git, append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = bbGitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("add", "testdata/ob/lock", "testdata/ob/DO_NOT_BUILD_HERE")
	run("commit", "-q", "-m", "fixture")
	bbAge(t, base, 30*time.Hour)
	untracked := filepath.Join(repo, "out-untracked")
	bbMakeBase(t, untracked, 4096, 30*time.Hour)
	f.cfg.BazelBases.Git = git
	cycle := f.run()
	if !bbExists(base) {
		t.Fatalf("a base git tracks was reaped: %v", bbRowPaths(cycle))
	}
	if r := bbRowFor(t, cycle, base); !strings.Contains(r.Reason, "tracks files under it") {
		t.Fatalf("kept for the wrong reason: %s", r.Reason)
	}
	if bbExists(untracked) {
		t.Fatalf("an untracked base inside a work tree was kept: %v", bbRowPaths(cycle))
	}
}

func TestBazelBasesRenameRaceRenamesBack(t *testing.T) {
	f := newBBFixture(t)
	base := filepath.Join(f.root, "a", "out")
	bbMakeBase(t, base, 4096, 30*time.Hour)
	calls := 0
	f.plugin.env.activity = func(ctx context.Context, opts bbActivityOptions) *bbActivity {
		calls++
		act := newBBActivity()
		// The third inventory is the one taken after the rename: something
		// opened a file in the aside between the re-check and the rename.
		if calls == 3 {
			entries, _ := filepath.Glob(filepath.Join(f.root, "a", ".out"+bbAsideTag+"*"))
			for _, e := range entries {
				act.index(filepath.Join(e, "command.log"), "pid 4242 holds command.log", nil)
			}
		}
		return act
	}
	cycle := f.run()
	if !bbExists(base) {
		t.Fatalf("the base was not renamed back after a late holder: %v", bbRowPaths(cycle))
	}
	if r := bbRowFor(t, cycle, base); !strings.Contains(r.Reason, "renamed back") {
		t.Fatalf("reason: %s", r.Reason)
	}
	left, _ := filepath.Glob(filepath.Join(f.root, "a", ".out"+bbAsideTag+"*"))
	if len(left) != 0 {
		t.Fatalf("aside left behind: %v", left)
	}
}

func TestBazelBasesMountPointInsideKeepsBase(t *testing.T) {
	f := newBBFixture(t)
	base := filepath.Join(f.root, "a", "out")
	bbMakeBase(t, base, 4096, 30*time.Hour)
	f.plugin.env.mountPoints = func() ([]string, error) {
		return []string{"/", filepath.Join(base, "execroot", "mnt")}, nil
	}
	cycle := f.run()
	if !bbExists(base) {
		t.Fatal("a base with a mount point inside was reaped")
	}
	if r := bbRowFor(t, cycle, base); !strings.Contains(r.Reason, "mount point") {
		t.Fatalf("reason: %s", r.Reason)
	}
}

func TestProcLocksHolderParsesAndIgnoresSelf(t *testing.T) {
	dir := t.TempDir()
	locks := filepath.Join(dir, "locks")
	content := "1: POSIX  ADVISORY  WRITE 4242 fd:01:12345 0 EOF\n2: FLOCK  ADVISORY  WRITE 777 fd:01:999 0 EOF\n3: -> FLOCK  ADVISORY  WRITE 778 fd:01:999 0 EOF\n"
	if err := os.WriteFile(locks, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	dev := uint64(0xfd<<8 | 0x01)
	if why, err := bbProcLocksHolder(locks, dev, 12345, 1); err != nil || !strings.Contains(why, "pid 4242") {
		t.Fatalf("POSIX holder not found: %q %v", why, err)
	}
	if why, _ := bbProcLocksHolder(locks, dev, 12345, 4242); why != "" {
		t.Fatalf("own lock reported: %q", why)
	}
	if why, _ := bbProcLocksHolder(locks, dev, 999, 1); !strings.Contains(why, "FLOCK") {
		t.Fatalf("flock holder not found: %q", why)
	}
	if why, _ := bbProcLocksHolder(locks, dev, 1, 1); why != "" {
		t.Fatalf("unrelated inode reported: %q", why)
	}
	if _, err := bbProcLocksHolder(filepath.Join(dir, "missing"), dev, 1, 1); err == nil {
		t.Fatal("an unreadable lock table must be an error (keeps every base)")
	}
}
