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
		{"empty argv falls back to comm", []string{""}, ".claude-unwrapp", true},
		{"neither", []string{"/bin/zsh"}, "zsh", false},
		// lab TIN-4405: Claude Code's bundled ugrep keeps the session's comm.
		{"multi-call child names another program", []string{"ugrep", "-G", "Pattern"}, ".claude-unwrapp", false},
		{"multi-call child with an empty argv1", []string{"ugrep", ""}, ".claude-unwrapp", false},
		{"argv0 title names the session by its first word", []string{"claude bg-spare", "--bg-spare", "/tmp/cc/claim.sock"}, ".claude-unwrapp", true},
		{"argv0 title with a store path first word", []string{"/nix/store/abc/bin/codex app-server"}, "x", true},
		{"argv1 title is a script's", []string{"node", "codex --resume"}, "node", true},
		{"title whose first word is another program", []string{"ugrep -G claude"}, ".claude-unwrapp", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nixSessionMatches(tc.args, tc.comm, comms); got != tc.want {
				t.Fatalf("nixSessionMatches(%q, %q) = %v, want %v", tc.args, tc.comm, got, tc.want)
			}
		})
	}
}
