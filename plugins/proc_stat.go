package plugins

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procCommState reads the command name and state letter from <dir>/stat
// (dir is /proc/<pid> or a fixture). The command name is the text between
// the first "(" and the last ")", so "(sd-pam)" reads as "(sd-pam)". An
// unreadable or malformed stat returns empty strings, which no
// uninspectable-ok list matches.
func procCommState(dir string) (comm, state string) {
	raw, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return "", ""
	}
	s := string(raw)
	open, close := strings.Index(s, "("), strings.LastIndex(s, ")")
	if open < 0 || close < open {
		return "", ""
	}
	if rest := strings.Fields(s[close+1:]); len(rest) > 0 {
		state = rest[0]
	}
	return s[open+1 : close], state
}

// procCommSet indexes an uninspectable-ok comm list for lookup.
func procCommSet(comms []string) map[string]bool {
	set := make(map[string]bool, len(comms))
	for _, c := range comms {
		if c != "" {
			set[c] = true
		}
	}
	return set
}

// procStatus is what /proc/<pid>/status says about a process's identity.
// Any process may read it for any other, dumpable or not.
type procStatus struct {
	name string
	ppid int
	uids []int
}

// allUIDs reports whether the real, effective, saved and file system uids all
// equal uid. A setuid program that has not yet called setuid fails it.
func (s procStatus) allUIDs(uid int) bool {
	if len(s.uids) != 4 {
		return false
	}
	for _, u := range s.uids {
		if u != uid {
			return false
		}
	}
	return true
}

// procReadStatus reads <dir>/status. The kernel escapes newlines in Name, so a
// renamed process cannot forge the lines after it. False means unknown.
func procReadStatus(dir string) (procStatus, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "status"))
	if err != nil {
		return procStatus{}, false
	}
	var st procStatus
	hasPPid := false
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "Name":
			st.name = value
		case "PPid":
			st.ppid, err = strconv.Atoi(value)
			if err != nil {
				return procStatus{}, false
			}
			hasPPid = true
		case "Uid":
			for _, field := range strings.Fields(value) {
				uid, err := strconv.Atoi(field)
				if err != nil {
					return procStatus{}, false
				}
				st.uids = append(st.uids, uid)
			}
		}
	}
	return st, hasPPid && st.name != ""
}

// procSSHDComms are the command names OpenSSH processes carry: sshd-session
// since OpenSSH 9.8, sshd for the listener and for older session processes.
var procSSHDComms = map[string]bool{"sshd-session": true, "sshd": true}

// procSSHDChainLimit bounds the walk from a session's monitor to pid 1. The
// real chain is the monitor, the listener and pid 1, or the monitor and pid 1
// once sshd has restarted under a live session or socket activation started it.
const procSSHDChainLimit = 8

// procIsSSHDSessionChild reports whether pid is the post-auth session process
// OpenSSH runs under uid. That process turns itself non-dumpable, so the
// kernel refuses uid its cwd, root, fd and maps, and only root can see what it
// holds. It is sshd's code and runs no build. It holds its session's sockets
// and pty, which on OpenSSH 9.9 can include a forwarded agent socket under
// /tmp, so a lane whose roots can hold such a thing must not use this answer
// without its own read.
//
// A name and a dumpable flag prove nothing: any process can write its own comm
// and call prctl(PR_SET_DUMPABLE, 0). The proof is the parent chain, read from
// /proc/<pid>/status, which every process can read for every other:
//
//   - pid is named for sshd, and all four of its uids are uid;
//   - its parent is the privileged monitor: named for sshd, and all four of
//     its uids are 0. OpenSSH forks the session process from that monitor and
//     every other user process of the session from the session process, so a
//     shell, or anything a shell starts, has a user-owned parent;
//   - every ancestor from there to pid 1 is named for sshd with all uids 0.
//
// A process cannot choose its parent. An orphan goes to pid 1 or a subreaper,
// never to a monitor. Forging the chain takes root: a symlink named
// sshd-session to sudo or su, kept running after its own parent exits. Root
// can remove the base directly, so this check does not defend against it.
func procIsSSHDSessionChild(procRoot string, pid, uid int) bool {
	read := func(p int) (procStatus, bool) {
		return procReadStatus(filepath.Join(procRoot, strconv.Itoa(p)))
	}
	session, ok := read(pid)
	if !ok || !procSSHDComms[session.name] || !session.allUIDs(uid) || session.ppid <= 1 {
		return false
	}
	ancestor := session.ppid
	for hop := 0; hop < procSSHDChainLimit; hop++ {
		st, ok := read(ancestor)
		if !ok || !st.allUIDs(0) {
			return false
		}
		if ancestor == 1 {
			return true
		}
		if !procSSHDComms[st.name] {
			return false
		}
		ancestor = st.ppid
	}
	return false
}
