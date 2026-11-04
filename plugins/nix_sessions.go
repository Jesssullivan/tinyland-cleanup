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

// nixArgNames returns the names one argv element can carry: its basename,
// and the basename of its first word when a process has rewritten argv[0]
// into a title. Claude Code titles its helpers "claude bg-pty-host" and
// "claude bg-spare".
func nixArgNames(arg string) []string {
	names := []string{filepath.Base(arg)}
	if fields := strings.Fields(arg); len(fields) > 1 {
		names = append(names, filepath.Base(fields[0]))
	}
	return names
}

// nixSessionMatches reports whether a process is a session: a name in comms
// is a name of argv[0] or of a script's argv[1] (see nixArgNames). The comm
// decides only when argv is unreadable or empty. A readable argv that names
// another program is not a session, even when the comm names one: Claude
// Code runs its bundled ugrep from its own binary with argv[0] "ugrep", so
// the comm of every such grep is ".claude-unwrapp". On sting those greps
// outlived their pipelines and held every hook pin (lab TIN-4405).
func nixSessionMatches(args []string, comm string, comms map[string]bool) bool {
	for i, arg := range args {
		if i > 1 {
			break
		}
		for _, name := range nixArgNames(arg) {
			if comms[name] || comms[nixSessionName(name)] {
				return true
			}
		}
	}
	for _, arg := range args {
		if arg != "" {
			return false
		}
	}
	return comm != "" && (comms[comm] || comms[nixSessionName(comm)])
}
