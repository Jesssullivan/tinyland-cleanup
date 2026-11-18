//go:build linux

package plugins

import (
	"os"
	"strings"
)

// retentionUnixListenFlag is __SO_ACCEPTCON in /proc/net/unix Flags: the
// socket is listening.
const retentionUnixListenFlag = 0x10000

// indexSocketListeners marks every listening unix socket bound to a path
// under opts.Within as held. An unreadable table makes the inventory
// incomplete.
func indexSocketListeners(a *bbActivity, table string, within []string) {
	raw, err := os.ReadFile(table)
	if err != nil {
		a.incomplete = append(a.incomplete, "unix socket table unreadable: "+err.Error())
		return
	}
	indexSocketListenerTable(a, string(raw), within)
}

// indexSocketListenerTable parses /proc/net/unix text:
// Num RefCount Protocol Flags Type St Inode Path.
func indexSocketListenerTable(a *bbActivity, text string, within []string) {
	for i, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 8 || !strings.HasPrefix(f[7], "/") {
			continue
		}
		var flags uint64
		for _, c := range f[3] {
			flags <<= 4
			switch {
			case c >= '0' && c <= '9':
				flags |= uint64(c - '0')
			case c >= 'a' && c <= 'f':
				flags |= uint64(c-'a') + 10
			case c >= 'A' && c <= 'F':
				flags |= uint64(c-'A') + 10
			}
		}
		if flags&retentionUnixListenFlag == 0 {
			continue
		}
		path := strings.Join(f[7:], " ")
		a.index(path, "listening unix socket "+path, within)
	}
}

func retentionSocketTable() string { return "/proc/net/unix" }
