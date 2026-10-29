//go:build linux

package plugins

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// retFakeProc writes /proc/<pid> with the files bbScanProc reads.
func retFakeProc(t *testing.T, root string, pid string, environ string) string {
	t.Helper()
	d := filepath.Join(root, pid)
	if err := os.MkdirAll(filepath.Join(d, "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"cmdline": "bash\x00",
		"stat":    pid + " (bash) S 1 1 1",
		"environ": environ,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(d, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestBbScanProcIndexesTmpdirEnvironment(t *testing.T) {
	proc := t.TempDir()
	retFakeProc(t, proc, "4242", "HOME=/home/x\x00TMPDIR=/srv/fast-local/tmp-agents/nix-shell.Ab12\x00")
	opts := bbActivityOptions{EnvVars: retentionEnvVars, Within: []string{"/srv/fast-local/tmp-agents"}}
	a := bbScanProc(context.Background(), proc, -1, os.Getuid(), opts)
	if why := a.complete(); why != "" {
		t.Fatalf("inventory incomplete: %s", why)
	}
	got := a.blockers("/srv/fast-local/tmp-agents/nix-shell.Ab12")
	if len(got) == 0 || !strings.Contains(got[0], "TMPDIR") {
		t.Fatalf("TMPDIR holder not found: %v", got)
	}
	// Without EnvVars the environment is not read (other lanes unchanged).
	b := bbScanProc(context.Background(), proc, -1, os.Getuid(), bbActivityOptions{Within: opts.Within})
	if len(b.blockers("/srv/fast-local/tmp-agents/nix-shell.Ab12")) != 0 {
		t.Fatal("environment read without EnvVars")
	}
}

func TestBbScanProcUnreadableEnvironmentIsIncomplete(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	proc := t.TempDir()
	d := retFakeProc(t, proc, "4343", "TMPDIR=/x\x00")
	if err := os.Chmod(filepath.Join(d, "environ"), 0); err != nil {
		t.Fatal(err)
	}
	a := bbScanProc(context.Background(), proc, -1, os.Getuid(), bbActivityOptions{EnvVars: retentionEnvVars})
	if !strings.Contains(a.complete(), "environment unreadable") {
		t.Fatalf("unreadable environment did not make the inventory incomplete: %q", a.complete())
	}
}
