package plugins

import (
	"path/filepath"
	"strings"
)

// nixSessionName strips what Nix wrappers add to a session's name: a
// leading "." and a "-wrapped" or "-unwrapped" suffix, which the 15-byte
// Linux comm (16 bytes on Darwin) may truncate (".claude-unwrapp" is claude).
func nixSessionName(name string) string {
	name = strings.TrimPrefix(name, ".")
	for _, suffix := range []string{"-unwrapp", "-wrapp"} {
		if i := strings.Index(name, suffix); i > 0 {
			return name[:i]
		}
	}
	return name
}

// nixSessionMatches reports whether a process is a session: the basename of
// argv[0], of a script's argv[1], or the process's comm names one of comms.
// args may be empty when argv is unreadable; the comm alone then decides.
func nixSessionMatches(args []string, comm string, comms map[string]bool) bool {
	for i, arg := range args {
		if i > 1 {
			break
		}
		base := filepath.Base(arg)
		if comms[base] || comms[nixSessionName(base)] {
			return true
		}
	}
	return comm != "" && (comms[comm] || comms[nixSessionName(comm)])
}
