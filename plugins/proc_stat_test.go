package plugins

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Fixtures in the shape of /proc/<pid>/status, as read on honey on November 7,
// 2026. They use no real process, and nothing is signalled.

// sshdFixtureUID is the uid an SSH session fixture runs under: the test's own,
// or 1000 under root, where a session and its monitor would otherwise share one.
func sshdFixtureUID() int {
	if uid := os.Getuid(); uid != 0 {
		return uid
	}
	return 1000
}

// addStatusProc adds one fixture process with a PPid line and the four given
// uids, owned on disk by the first of them, as /proc/<pid> is by its euid.
func addStatusProc(tree *fakeProcTree, pid, ppid int, comm string, uids ...int) string {
	tree.t.Helper()
	if len(uids) == 1 {
		uids = []int{uids[0], uids[0], uids[0], uids[0]}
	}
	d := tree.add(pid, uids[1], comm, "S", "")
	status := fmt.Sprintf("Name:\t%s\nState:\tS (sleeping)\nPPid:\t%d\nUid:\t%d\t%d\t%d\t%d\n",
		comm, ppid, uids[0], uids[1], uids[2], uids[3])
	if err := os.WriteFile(filepath.Join(d, "status"), []byte(status), 0o600); err != nil {
		tree.t.Fatal(err)
	}
	if os.Getuid() == 0 {
		if err := os.Chown(d, uids[1], -1); err != nil {
			tree.t.Fatal(err)
		}
	}
	return d
}

// addSSHDChain adds the chain measured on honey on 2026-11-07: systemd, the
// sshd listener, the root sshd-session monitor and the session's own process.
// The kernel refuses that last one its fd and cwd. It returns the session's
// process directory.
func addSSHDChain(tree *fakeProcTree, session, uid int) string {
	tree.t.Helper()
	addStatusProc(tree, 1, 0, "systemd", 0)
	addStatusProc(tree, 2146, 1, "sshd", 0)
	addStatusProc(tree, session-1, 2146, "sshd-session", 0)
	return addStatusProc(tree, session, session-1, "sshd-session", uid)
}

func TestProcIsSSHDSessionChildProofIsTheParentChain(t *testing.T) {
	const user = 1000
	const pid = 3001
	type row struct {
		pid, ppid int
		comm      string
		uids      []int
	}
	for _, tc := range []struct {
		name  string
		procs []row
		uid   int
		want  bool
	}{
		{name: "measured chain: session, monitor, listener, pid 1", want: true, procs: []row{
			{1, 0, "systemd", []int{0}}, {10, 1, "sshd", []int{0}},
			{20, 10, "sshd-session", []int{0}}, {pid, 20, "sshd-session", []int{user}}}},
		{name: "sshd restarted under a live session: monitor orphaned to pid 1", want: true, procs: []row{
			{1, 0, "systemd", []int{0}}, {20, 1, "sshd-session", []int{0}}, {pid, 20, "sshd-session", []int{user}}}},
		{name: "OpenSSH before 9.8 names every process sshd", want: true, procs: []row{
			{1, 0, "systemd", []int{0}}, {10, 1, "sshd", []int{0}},
			{20, 10, "sshd", []int{0}}, {pid, 20, "sshd", []int{user}}}},
		{name: "renamed under a user shell", procs: []row{
			{1, 0, "systemd", []int{0}}, {20, 1, "bash", []int{user}}, {pid, 20, "sshd-session", []int{user}}}},
		{name: "renamed and daemonized to pid 1", procs: []row{
			{1, 0, "systemd", []int{0}}, {pid, 1, "sshd-session", []int{user}}}},
		{name: "renamed and started by a real session process", procs: []row{
			{1, 0, "systemd", []int{0}}, {10, 1, "sshd", []int{0}}, {20, 10, "sshd-session", []int{0}},
			{21, 20, "sshd-session", []int{user}}, {pid, 21, "sshd-session", []int{user}}}},
		{name: "renamed under a root parent that is not sshd", procs: []row{
			{1, 0, "systemd", []int{0}}, {20, 1, "sudo", []int{0}}, {pid, 20, "sshd-session", []int{user}}}},
		{name: "setuid program symlinked to the name, its parent a user shell", procs: []row{
			{1, 0, "systemd", []int{0}}, {10, 1, "fish", []int{user}},
			{20, 10, "sshd-session", []int{0}}, {pid, 20, "sshd-session", []int{user}}}},
		{name: "setuid program that kept its real uid", procs: []row{
			{1, 0, "systemd", []int{0}}, {20, 1, "sshd-session", []int{user, 0, 0, 0}}, {pid, 20, "sshd-session", []int{user}}}},
		{name: "sshd in a container: the chain meets the shim", procs: []row{
			{1, 0, "systemd", []int{0}}, {5, 1, "containerd-shim", []int{0}}, {10, 5, "sshd", []int{0}},
			{20, 10, "sshd-session", []int{0}}, {pid, 20, "sshd-session", []int{user}}}},
		{name: "a builder under a real monitor is not named for sshd", procs: []row{
			{1, 0, "systemd", []int{0}}, {20, 1, "sshd-session", []int{0}}, {pid, 20, "java", []int{user}}}},
		{name: "another user's session", procs: []row{
			{1, 0, "systemd", []int{0}}, {20, 1, "sshd-session", []int{0}}, {pid, 20, "sshd-session", []int{user + 1}}}},
		{name: "session process with one other uid", procs: []row{
			{1, 0, "systemd", []int{0}}, {20, 1, "sshd-session", []int{0}}, {pid, 20, "sshd-session", []int{user, user, user, 0}}}},
		{name: "the monitor has gone", procs: []row{
			{1, 0, "systemd", []int{0}}, {pid, 20, "sshd-session", []int{user}}}},
		{name: "an ancestor has gone", procs: []row{
			{1, 0, "systemd", []int{0}}, {20, 10, "sshd-session", []int{0}}, {pid, 20, "sshd-session", []int{user}}}},
		{name: "pid 1 is not root", procs: []row{
			{1, 0, "systemd", []int{user}}, {20, 1, "sshd-session", []int{0}}, {pid, 20, "sshd-session", []int{user}}}},
		{name: "a chain that never reaches pid 1", procs: []row{
			{20, 21, "sshd-session", []int{0}}, {21, 20, "sshd", []int{0}}, {pid, 20, "sshd-session", []int{user}}}},
		{name: "a chain that ends before pid 1", procs: []row{
			{20, 0, "sshd-session", []int{0}}, {pid, 20, "sshd-session", []int{user}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := newFakeProcTree(t)
			for _, p := range tc.procs {
				addStatusProc(tree, p.pid, p.ppid, p.comm, p.uids...)
			}
			if got := procIsSSHDSessionChild(tree.dir, pid, user); got != tc.want {
				t.Fatalf("procIsSSHDSessionChild = %v, want %v", got, tc.want)
			}
		})
	}
	if procIsSSHDSessionChild(t.TempDir(), pid, user) {
		t.Fatal("a process with no status was proved")
	}
}

func TestProcReadStatusUnknownIsNotProof(t *testing.T) {
	dir := t.TempDir()
	for body, name := range map[string]string{
		"Name:\tsshd-session\nUid:\t1000\t1000\t1000\t1000\n":           "no PPid",
		"PPid:\t20\nUid:\t1000\t1000\t1000\t1000\n":                     "no Name",
		"Name:\tsshd-session\nPPid:\tx\nUid:\t1000\t1000\t1000\t1000\n": "bad PPid",
		"Name:\tsshd-session\nPPid:\t20\nUid:\t1000\tx\t1000\t1000\n":   "bad Uid",
	} {
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := procReadStatus(dir); ok {
			t.Errorf("%s: status read as known", name)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte("Name:\tsshd-session\nPPid:\t20\nUid:\t1000\t1000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, ok := procReadStatus(dir); !ok || st.allUIDs(1000) {
		t.Fatalf("a Uid line short of four fields proved uid 1000: %+v %v", st, ok)
	}
}
