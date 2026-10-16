//go:build linux

package plugins

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fakeProc(t *testing.T, comm string, argv ...string) string {
	t.Helper()
	dir := t.TempDir()
	cmdline := ""
	for _, a := range argv {
		cmdline += a + "\x00"
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644); err != nil {
		t.Fatal(err)
	}
	// pid (comm) state ppid ... starttime is field 22 (index 19 after comm).
	stat := "4242 (" + comm + ") S 1 4242 4242 0 -1 4194560 0 0 0 0 1 2 0 0 20 0 9 0 360000 0 0"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Re-review N7: the session shapes observed on sting and the Nix wrapper
// shapes match; unrelated processes do not.
func TestNixProcMatchesObservedSessionShapes(t *testing.T) {
	comms := procCommSet([]string{"claude", "codex", "pi", "opencode"})
	for _, tc := range []struct {
		name string
		dir  string
		want bool
	}{
		{"claude argv0, wrapped comm", fakeProc(t, ".claude-unwrapp", "claude", "--resume"), true},
		{"claude by store path", fakeProc(t, ".claude-unwrapp", "/nix/store/0123456789abcdfghijklmnpqrsvwxyz-claude-code-2.1/bin/.claude-unwrapped"), true},
		{"codex binary", fakeProc(t, "codex", "/nix/store/0123456789abcdfghijklmnpqrsvwxyz-codex/bin/codex", "app-server"), true},
		{"node script pi", fakeProc(t, "node", "/nix/store/0123456789abcdfghijklmnpqrsvwxyz-nodejs/bin/node", "/nix/store/0123456789abcdfghijklmnpqrsvwxyz-pi/bin/.pi-wrapped"), true},
		{"opencode", fakeProc(t, "opencode", "opencode"), true},
		{"bash", fakeProc(t, "bash", "/bin/bash", "-l"), false},
		{"claude as a later argument", fakeProc(t, "bash", "bash", "-c", "claude"), false},
		{"empty comm", fakeProc(t, "", "sleep"), false},
	} {
		if got := nixProcMatches(tc.dir, comms); got != tc.want {
			t.Errorf("%s: nixProcMatches = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNixProcStartReadsStartTime(t *testing.T) {
	boot := time.Unix(1_700_000_000, 0)
	dir := fakeProc(t, "claude (x)", "claude")
	start, ok := nixProcStart(dir, boot)
	if !ok || !start.Equal(boot.Add(3600*time.Second)) {
		t.Fatalf("starttime 360000 ticks is boot+1h, got %v %v", start, ok)
	}
}
