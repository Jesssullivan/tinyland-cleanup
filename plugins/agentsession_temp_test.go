package plugins

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// agentSessionTempFixture lays out one tree per agent-session temp name and
// one plain control tree under dir, each holding an old regular file and a
// stale node_modules, and returns the session trees and the control tree.
func agentSessionTempFixture(t *testing.T, dir string) (sessions []string, control string) {
	t.Helper()
	old := time.Now().Add(-90 * 24 * time.Hour)
	build := func(name string) string {
		root := filepath.Join(dir, name)
		nm := filepath.Join(root, "node_modules", "pkg")
		if err := os.MkdirAll(nm, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{
			filepath.Join(root, "notes.md"),
			filepath.Join(root, "package.json"),
			filepath.Join(nm, "index.js"),
		} {
			if err := os.WriteFile(f, make([]byte, 2*1024*1024), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			return os.Chtimes(path, old, old)
		}); err != nil {
			t.Fatal(err)
		}
		return root
	}
	for _, name := range []string{"claude-abcdef", "claude-501", "codex-sess1", "codex", ".tmpAbC123", "pi-session-xyz"} {
		sessions = append(sessions, build(name))
	}
	return sessions, build("plain-worktree")
}

func TestAgentSessionDenyCoversTempSessionNames(t *testing.T) {
	tmp := t.TempDir()
	sessions, control := agentSessionTempFixture(t, tmp)
	deny := newAgentSessionDeny(t.TempDir(), os.Getuid(), tmp, nil, nil)
	for _, s := range sessions {
		for _, p := range []string{s, filepath.Join(s, "notes.md"), filepath.Join(s, "node_modules", "pkg")} {
			if _, denied := deny.denied(p); !denied {
				t.Errorf("%s must be denied as agent-session data", p)
			}
		}
	}
	if why, denied := deny.denied(filepath.Join(control, "notes.md")); denied {
		t.Errorf("plain temp tree must not be denied: %s", why)
	}
	// The compiled host temp directories carry the same names.
	for _, p := range []string{"/tmp/codex-sess1/x", "/tmp/claude-abcdef/y", "/var/tmp/codex-vartmp/z", "/tmp/.tmpAbC123/w", "/tmp/pi-session-xyz/v"} {
		if _, denied := deny.denied(p); !denied {
			t.Errorf("%s must be denied as agent-session data", p)
		}
	}
	if _, denied := deny.denied("/tmp/nix-shell-123/x"); denied {
		t.Error("/tmp/nix-shell-123 is not agent-session data")
	}
}

func TestTempSweepKeepsAgentSessionTempTrees(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	sessions, control := agentSessionTempFixture(t, tmp)
	freed := deleteOldFilesOwnedByUserSameDevice(tmp, 24*time.Hour)
	for _, s := range sessions {
		for _, f := range []string{"notes.md", filepath.Join("node_modules", "pkg", "index.js")} {
			if !pathExists(filepath.Join(s, f)) {
				t.Errorf("temp sweep deleted agent-session file %s", filepath.Join(s, f))
			}
		}
	}
	if pathExists(filepath.Join(control, "notes.md")) {
		t.Error("temp sweep must still delete old files in a plain temp tree")
	}
	if freed <= 0 {
		t.Errorf("expected the sweep to free the control tree, freed=%d", freed)
	}
	// The unowned-file variant shares the deny set.
	_, control2 := agentSessionTempFixture(t, tmp)
	deleteOldFilesSameDevice(tmp, 24*time.Hour)
	for _, s := range sessions {
		if !pathExists(filepath.Join(s, "notes.md")) {
			t.Errorf("temp sweep deleted agent-session file in %s", s)
		}
	}
	if pathExists(filepath.Join(control2, "notes.md")) {
		t.Error("temp sweep must still delete old files in a plain temp tree")
	}
}

func TestDevArtifactsKeepsAgentSessionTempTreesAtCritical(t *testing.T) {
	tmp := t.TempDir()
	sessions, control := agentSessionTempFixture(t, tmp)
	// A Claude scratch session, idle far past the harness-scratch age, with
	// the lane on and its default pattern.
	session := filepath.Join(tmp, "claude-501", "-home-jess-git-lab", "11111111-idle-90d")
	scratch := filepath.Join(session, "scratchpad", "findings.md")
	if err := os.MkdirAll(filepath.Dir(scratch), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scratch, []byte("findings"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	for _, p := range []string{scratch, filepath.Dir(scratch), session, filepath.Dir(session), filepath.Join(tmp, "claude-501")} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	cfg := tempGeneratedArtifactConfig(tmp)
	cfg.DevArtifacts.HarnessScratch = true
	cfg.DevArtifacts.HarnessScratchDirPatterns = []string{"claude-*", "codex*", ".tmp*", "pi-*"}
	cfg.DevArtifacts.HarnessScratchStaleAfter = "2h"
	cfg.DevArtifacts.NodeModules = true
	callerProtect := cfg.DevArtifacts.ProtectPaths

	p := newDevArtifactsPluginWithActive(nil)
	logger := devArtifactTestLogger()
	plan := p.PlanCleanup(context.Background(), LevelCritical, cfg, logger)
	for _, target := range plan.Targets {
		if target.Action != "delete" {
			continue
		}
		for _, s := range sessions {
			if pathWithin(target.Path, s) || target.Path == s {
				t.Errorf("plan would delete inside agent-session tree: %#v", target)
			}
		}
	}
	result := p.Cleanup(context.Background(), LevelCritical, cfg, logger)
	if !pathExists(scratch) {
		t.Error("harness-scratch lane deleted a Claude session scratch directory")
	}
	for _, s := range sessions {
		for _, f := range []string{"notes.md", filepath.Join("node_modules", "pkg", "index.js")} {
			if !pathExists(filepath.Join(s, f)) {
				t.Errorf("dev_artifacts deleted %s inside an agent-session tree", filepath.Join(s, f))
			}
		}
	}
	if pathExists(filepath.Join(control, "node_modules")) {
		t.Errorf("the plain temp tree's stale node_modules should still be removed, result=%#v", result)
	}
	if len(cfg.DevArtifacts.ProtectPaths) != len(callerProtect) {
		t.Error("the caller's protect_paths must not be modified")
	}
}

func TestAgentSessionTempProtectPrefixes(t *testing.T) {
	got := agentSessionTempProtectPrefixes([]string{"/nonexistent-root/tmp", "", "/nonexistent-root/tmp"})
	want := []string{"/nonexistent-root/tmp/claude-", "/nonexistent-root/tmp/codex", "/nonexistent-root/tmp/.tmp", "/nonexistent-root/tmp/pi-"}
	if len(got) != len(want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	}
	cfg := config.DefaultConfig()
	out := withAgentSessionTempProtection(cfg.DevArtifacts, "/home/nobody")
	if len(out.ProtectPaths) <= len(cfg.DevArtifacts.ProtectPaths) {
		t.Fatal("the default config must gain the agent-session temp prefixes")
	}
}
