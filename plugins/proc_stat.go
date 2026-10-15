package plugins

import (
	"os"
	"path/filepath"
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
