//go:build linux

package plugins

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
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

// The skip is opt-in: retention and shm_scratch have not read whether an agent
// socket can sit under their roots, so the same session still refuses them.
func TestBBScanProcSkipsProvenSessionOnlyWhenAsked(t *testing.T) {
	uid := sshdFixtureUID()
	tree := newFakeProcTree(t)
	session := addSSHDChain(tree, 3001, uid)
	denyProcReads(t, []string{session}, []string{session})

	asked := bbScanProc(context.Background(), tree.dir, -1, uid, bbActivityOptions{SkipSSHDSessions: true})
	if why := asked.complete(); why != "" {
		t.Fatalf("a proven session process refused the inventory: %s", why)
	}
	if len(asked.skipped) != 1 || asked.skipped[0] != 3001 {
		t.Fatalf("skipped = %v, want [3001]", asked.skipped)
	}

	plain := bbScanProc(context.Background(), tree.dir, -1, uid, bbActivityOptions{})
	if why := plain.complete(); !strings.Contains(why, "pid 3001 (sshd-session) fd unreadable") {
		t.Fatalf("without the option the session must refuse, got %q", why)
	}
	if len(plain.skipped) != 0 {
		t.Fatalf("skipped without the option: %v", plain.skipped)
	}
}

func TestLanesThatHaveNotReadTheirRootsDoNotSkipSessions(t *testing.T) {
	r := &RetentionPlugin{}
	if r.activityOptions(config.DefaultConfig(), nil).SkipSSHDSessions {
		t.Error("retention skips SSH sessions without its own read")
	}
	f := newShmFixture(t)
	inner := f.plugin.activity
	asked := 0
	f.plugin.activity = func(ctx context.Context, opts bbActivityOptions) *bbActivity {
		asked++
		if opts.SkipSSHDSessions {
			t.Error("shm_scratch skips SSH sessions without its own read")
		}
		return inner(ctx, opts)
	}
	f.tree("idle", 48*time.Hour, "a")
	f.plugin.Cleanup(context.Background(), LevelWarning, f.cfg, bbDiscardLogger())
	if asked == 0 {
		t.Fatal("shm_scratch took no inventory, so nothing was checked")
	}
}

// useFixtureProc runs the plugin's inventories over the fixture tree, passing
// through the options the plugin chose.
func useFixtureProc(f *bbFixture, tree *fakeProcTree, uid int) {
	f.plugin.env.activity = func(ctx context.Context, opts bbActivityOptions) *bbActivity {
		return bbScanProc(ctx, tree.dir, -1, uid, opts)
	}
}

func TestBazelBasesReapsPastProvenSSHDSessionAndLogsIt(t *testing.T) {
	uid := sshdFixtureUID()
	tree := newFakeProcTree(t)
	var denied []string
	addStatusProc(tree, 1, 0, "systemd", 0)
	addStatusProc(tree, 2146, 1, "sshd", 0)
	for pid := 3001; pid <= 3015; pid += 2 {
		addStatusProc(tree, pid-1, 2146, "sshd-session", 0)
		denied = append(denied, addStatusProc(tree, pid, pid-1, "sshd-session", uid))
	}
	denyProcReads(t, denied, denied)

	f := newBBFixture(t)
	useFixtureProc(f, tree, uid)
	base := filepath.Join(f.root, "a", "out")
	bbMakeBase(t, base, 4096, 30*time.Hour)

	var log bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&log, nil))
	result := f.plugin.Cleanup(context.Background(), LevelWarning, f.cfg, logger)
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	if bbExists(base) {
		t.Fatalf("an idle base survived an inventory with only SSH sessions unreadable:\n%s", log.String())
	}
	line := log.String()
	if !strings.Contains(line, "inventory_skipped=8") {
		t.Fatalf("the cycle line does not count the skipped sessions:\n%s", line)
	}
	if !strings.Contains(line, `inventory_skipped_pids="[3001 3003 3005 3007 3009]"`) {
		t.Fatalf("the cycle line does not list the first few pids:\n%s", line)
	}
	if strings.Contains(line, "3011") {
		t.Fatalf("the cycle line lists more than the first few pids:\n%s", line)
	}
	raw, err := os.ReadFile(filepath.Join(f.cfg.BazelBases.ReceiptDir, "last-cycle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		Skipped int    `json:"inventory_skipped"`
		PIDs    []int  `json:"inventory_skipped_pids"`
		Refusal string `json:"refusal"`
	}
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Skipped != 8 || len(receipt.PIDs) != 5 || receipt.Refusal != "" {
		t.Fatalf("receipt: %+v", receipt)
	}
}

// A process that renames itself sshd-session and turns non-dumpable shows the
// scan nothing, so the refusal is the only thing that protects what it holds.
func TestBazelBasesRenamedUnreadableProcessHoldingBaseRefuses(t *testing.T) {
	uid := sshdFixtureUID()
	tree := newFakeProcTree(t)
	addStatusProc(tree, 1, 0, "systemd", 0)
	addStatusProc(tree, 4000, 1, "fish", uid)
	renamed := addStatusProc(tree, 4001, 4000, "sshd-session", uid)
	denyProcReads(t, []string{renamed}, []string{renamed})

	f := newBBFixture(t)
	useFixtureProc(f, tree, uid)
	base := filepath.Join(f.root, "a", "out")
	bbMakeBase(t, base, 4096, 30*time.Hour)
	cycle := f.run()
	if !bbExists(base) {
		t.Fatalf("a base was reaped past a renamed unreadable process: %v", bbRowPaths(cycle))
	}
	if cycle.Removed != 0 || !strings.Contains(cycle.Refusal, "pid 4001 (sshd-session)") {
		t.Fatalf("removed %d, refusal %q", cycle.Removed, cycle.Refusal)
	}
	if cycle.InventorySkipped != 0 {
		t.Fatalf("a renamed process counted as skipped: %d", cycle.InventorySkipped)
	}
}

// The lock and server checks read the base itself. With the only unreadable
// process skipped, each still keeps its base while an idle one beside it goes.
func TestBazelBasesLiveBaseKeptBesideSkippedSession(t *testing.T) {
	uid := sshdFixtureUID()
	tree := newFakeProcTree(t)
	session := addSSHDChain(tree, 3001, uid)
	denyProcReads(t, []string{session}, []string{session})

	f := newBBFixture(t)
	useFixtureProc(f, tree, uid)
	idle := filepath.Join(f.root, "idle", "out")
	locked := filepath.Join(f.root, "locked", "out")
	served := filepath.Join(f.root, "served", "out")
	for _, b := range []string{idle, locked, served} {
		bbMakeBase(t, b, 4096, 30*time.Hour)
	}
	realHolder := f.plugin.env.lockHolder
	f.plugin.env.lockHolder = func(lockPath string) (string, error) {
		if lockPath == filepath.Join(locked, "lock") {
			return "its Bazel lock is held (FLOCK lock, pid 4242): a Bazel command is running", nil
		}
		return realHolder(lockPath)
	}
	// A server pid must be alive: use this test's own pid, whose fixture
	// process names the base. Nothing is signalled.
	pidFile := filepath.Join(served, "server", "server.pid.txt")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	bbAge(t, served, 30*time.Hour)
	server := addStatusProc(tree, os.Getpid(), 1, "java", uid)
	if err := os.WriteFile(filepath.Join(server, "cmdline"), []byte("bazel(x)\x00--output_base="+served+"\x00"), 0o600); err != nil {
		t.Fatal(err)
	}

	cycle := f.run()
	if cycle.Refusal != "" {
		t.Fatalf("refusal: %s", cycle.Refusal)
	}
	if bbExists(idle) {
		t.Fatalf("the idle base beside the live ones was kept: %v", bbRowPaths(cycle))
	}
	if !bbExists(locked) {
		t.Fatal("a base with a held lock was reaped")
	}
	if r := bbRowFor(t, cycle, locked); !strings.Contains(r.Reason, "lock is held") {
		t.Fatalf("locked base kept for the wrong reason: %s", r.Reason)
	}
	if !bbExists(served) {
		t.Fatal("a base with a live server was reaped")
	}
	if r := bbRowFor(t, cycle, served); !strings.Contains(r.Reason, "server pid") {
		t.Fatalf("served base kept for the wrong reason: %s", r.Reason)
	}
	if cycle.InventorySkipped != 1 {
		t.Fatalf("skipped = %d, want 1", cycle.InventorySkipped)
	}
}

// One unexplained process still refuses the whole cycle, and the refusal names
// it and not the session that was proved.
func TestBazelBasesMixedUnreadableProcessesRefuse(t *testing.T) {
	uid := sshdFixtureUID()
	tree := newFakeProcTree(t)
	session := addSSHDChain(tree, 3001, uid)
	other := addStatusProc(tree, 5000, 1, "podman", uid)
	denyProcReads(t, []string{session, other}, []string{session, other})

	f := newBBFixture(t)
	useFixtureProc(f, tree, uid)
	base := filepath.Join(f.root, "a", "out")
	bbMakeBase(t, base, 4096, 30*time.Hour)
	cycle := f.run()
	if !bbExists(base) {
		t.Fatalf("a base was reaped past an unexplained unreadable process: %v", bbRowPaths(cycle))
	}
	if !strings.Contains(cycle.Refusal, "pid 5000 (podman)") {
		t.Fatalf("the refusal does not name the unexplained process: %q", cycle.Refusal)
	}
	if strings.Contains(cycle.Refusal, "3001") {
		t.Fatalf("the refusal names the proven session: %q", cycle.Refusal)
	}
	if cycle.InventorySkipped != 1 || cycle.InventorySkippedPIDs[0] != 3001 {
		t.Fatalf("skipped %d %v", cycle.InventorySkipped, cycle.InventorySkippedPIDs)
	}
}

// Both removal re-checks apply the same rule: a session survives them, and a
// process that appears unreadable after discovery keeps the base.
func TestBazelBasesRemovalRecheckKeepsTheRule(t *testing.T) {
	for _, tc := range []struct {
		name       string
		appearsAt  int
		wantReason string
	}{
		{name: "before the rename", appearsAt: 2, wantReason: "kept at removal: "},
		{name: "after the rename", appearsAt: 3, wantReason: "renamed back"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uid := sshdFixtureUID()
			tree := newFakeProcTree(t)
			session := addSSHDChain(tree, 3001, uid)
			denyProcReads(t, []string{session}, []string{session})

			f := newBBFixture(t)
			calls := 0
			f.plugin.env.activity = func(ctx context.Context, opts bbActivityOptions) *bbActivity {
				calls++
				if calls == tc.appearsAt {
					late := addStatusProc(tree, 6000, 1, "sshd-session", uid)
					denyProcReads(t, []string{late}, []string{late})
				}
				return bbScanProc(ctx, tree.dir, -1, uid, opts)
			}
			base := filepath.Join(f.root, "a", "out")
			bbMakeBase(t, base, 4096, 30*time.Hour)
			cycle := f.run()
			if !bbExists(base) {
				t.Fatalf("a base was reaped past a process that appeared before removal: %v", bbRowPaths(cycle))
			}
			r := bbRowFor(t, cycle, base)
			if !strings.Contains(r.Reason, "pid 6000") || !strings.Contains(r.Reason, tc.wantReason) {
				t.Fatalf("reason: %s", r.Reason)
			}
		})
	}
}
