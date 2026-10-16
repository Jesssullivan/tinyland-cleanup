package plugins

import "testing"

func TestNixSessionMatchesArgvScriptAndComm(t *testing.T) {
	comms := procCommSet([]string{"claude", "codex"})
	cases := []struct {
		name string
		args []string
		comm string
		want bool
	}{
		{"argv0 basename", []string{"/nix/store/abc-claude/bin/claude", "--resume"}, "claude", true},
		{"wrapped argv0", []string{"/nix/store/abc/bin/.codex-wrapped"}, ".codex-wrapped", true},
		{"script under an interpreter", []string{"node", "/usr/lib/node_modules/x/claude"}, "node", true},
		{"argv2 is not the session", []string{"bash", "-c", "claude"}, "bash", false},
		{"unreadable argv falls back to comm", nil, ".claude-unwrappe", true},
		{"neither", []string{"/bin/zsh"}, "zsh", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nixSessionMatches(tc.args, tc.comm, comms); got != tc.want {
				t.Fatalf("nixSessionMatches(%q, %q) = %v, want %v", tc.args, tc.comm, got, tc.want)
			}
		})
	}
}
