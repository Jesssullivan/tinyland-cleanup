package plugins

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Lab sting 2026-10-19: bazel_bases kept nine output bases idle 55-93 h
// (134 GiB) because each held a .claude directory that an npm package ships
// (cytoscape@3.34.1, nanoid@3.3.12, resolve@1.22.12, dockerode@4.0.10) or
// that a fetched external repository carries (external/spear_resumes+).
// Those copies are reproduced by every build; they are nobody's session.
// The TIN-5268 name rule no longer counts a session-named directory below a
// node_modules element or inside an output base's generated areas, and
// everything else it denied stays denied.

func TestAgentSessionDenyIgnoresPackageAndBazelGeneratedCopies(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	tmp := filepath.Join(dir, "tmp")
	bbMkdir(t, filepath.Join(home, ".claude", "projects"))
	// A structural output base, recognised as bazel_bases discovers one.
	base := filepath.Join(dir, "cache", "bazel", "_bazel_u", "0123abcd")
	bbMakeBase(t, base, 16, time.Hour)
	// A working tree whose own .claude sits beside its node_modules.
	ws := filepath.Join(dir, "git", "app")
	bbMkdir(t, filepath.Join(ws, ".claude", "worktrees", "w"))
	bbMkdir(t, filepath.Join(ws, "node_modules", "cytoscape", ".claude"))
	// A node_modules entry linking into a real working tree (npm link, a
	// pnpm workspace package) is judged at its physical path.
	linked := filepath.Join(dir, "git", "lib")
	bbMkdir(t, filepath.Join(linked, ".claude"))
	if err := os.Symlink(linked, filepath.Join(ws, "node_modules", "lib")); err != nil {
		t.Fatal(err)
	}
	// Directories merely named execroot and external are not an output
	// base: there is no lock, DO_NOT_BUILD_HERE or javalog.properties.
	lookalike := filepath.Join(dir, "git", "lookalike")
	bbMkdir(t, filepath.Join(lookalike, "execroot"))
	bbMkdir(t, filepath.Join(lookalike, "external", "vendored", ".claude"))

	d := newAgentSessionDeny(home, 1000, tmp, nil, nil)
	for _, p := range []string{
		filepath.Join(home, ".claude", "projects", "p.jsonl"),
		filepath.Join(ws, ".claude", "settings.local.json"),
		filepath.Join(ws, ".claude", "worktrees", "w", "main.go"),
		filepath.Join(ws, "node_modules", "lib", ".claude", "settings.json"),
		filepath.Join(lookalike, "external", "vendored", ".claude", "x"),
		filepath.Join(dir, "git", "other", ".codex", "x"),
		"/tmp/tmux-1000/default",
		"/run/user/1000/tmux-1000/default",
		filepath.Join(tmp, "claude-1000", "x"),
		filepath.Join(tmp, "codex-run", "x"),
		"/tmp/pi-session/x",
	} {
		if _, denied := d.denied(p); !denied {
			t.Errorf("%s is no longer denied", p)
		}
	}
	for _, p := range []string{
		filepath.Join(ws, "node_modules", "cytoscape", ".claude", "settings.json"),
		filepath.Join(base, "execroot", "_main", "bazel-out", "k8-fastbuild", "bin", "node_modules", ".aspect_rules_js",
			"cytoscape@3.34.1", "node_modules", "cytoscape", ".claude", "settings.json"),
		filepath.Join(base, "external", "spear_resumes+", ".claude", "settings.json"),
		filepath.Join(base, "execroot", "_main", ".claude", "x"),
		filepath.Join(base, "sandbox", "linux-sandbox", "7", "execroot", "_main", ".codex", "x"),
	} {
		if why, denied := d.denied(p); denied {
			t.Errorf("%s is a generated copy but is denied: %s", p, why)
		}
	}
}

// A walk keeps going past a generated copy and still finds a real session
// directory; a workspace .claude beside node_modules is still found.
func TestBBMeasureTreeSkipsOnlyGeneratedCopies(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	only := filepath.Join(dir, "only-copies")
	bbWrite(t, filepath.Join(only, "node_modules", "nanoid", ".claude", "settings.json"), 8)
	bbWrite(t, filepath.Join(only, "a", "node_modules", ".pnpm", "resolve@1.22.12", "node_modules", "resolve", ".claude", "x"), 8)
	if _, _, denied, err := bbMeasureTree(context.Background(), only, time.Time{}, os.Getuid()); denied != "" || err != nil {
		t.Fatalf("package copies held the tree: denied=%q err=%v", denied, err)
	}
	ws := filepath.Join(dir, "workspace")
	bbWrite(t, filepath.Join(ws, ".claude", "settings.local.json"), 8)
	bbWrite(t, filepath.Join(ws, "node_modules", "dockerode", ".claude", "x"), 8)
	if _, _, denied, _ := bbMeasureTree(context.Background(), ws, time.Time{}, os.Getuid()); denied != filepath.Join(ws, ".claude") {
		t.Fatalf("workspace .claude beside node_modules: denied=%q", denied)
	}
	later := filepath.Join(dir, "later")
	bbWrite(t, filepath.Join(later, "node_modules", "pkg", ".claude", "x"), 8)
	bbWrite(t, filepath.Join(later, "src", ".codex", "history.jsonl"), 8)
	if _, _, denied, _ := bbMeasureTree(context.Background(), later, time.Time{}, os.Getuid()); denied != filepath.Join(later, "src", ".codex") {
		t.Fatalf("a session directory after a package copy: denied=%q", denied)
	}
}

func TestBazelBasesReapsBasesWhoseOnlySessionNamesAreGeneratedCopies(t *testing.T) {
	f := newBBFixture(t)
	f.cfg.BazelBases.Roots = []string{f.root, f.home}
	npm := filepath.Join(f.root, "cache", "_bazel_u", "npm")
	bbMakeBase(t, npm, 4096, 60*time.Hour)
	bbWrite(t, filepath.Join(npm, "execroot", "_main", "bazel-out", "k8-fastbuild", "bin", "node_modules", ".aspect_rules_js",
		"cytoscape@3.34.1", "node_modules", "cytoscape", ".claude", "settings.local.json"), 32)
	bbAge(t, npm, 60*time.Hour)
	ext := filepath.Join(f.root, "cache", "_bazel_u", "ext")
	bbMakeBase(t, ext, 4096, 60*time.Hour)
	bbWrite(t, filepath.Join(ext, "external", "spear_resumes+", ".claude", "settings.json"), 32)
	bbAge(t, ext, 60*time.Hour)
	// The same copy beside a session directory outside the generated areas
	// keeps the base.
	mixed := filepath.Join(f.root, "cache", "_bazel_u", "mixed")
	bbMakeBase(t, mixed, 4096, 60*time.Hour)
	bbWrite(t, filepath.Join(mixed, "execroot", "_main", "bazel-out", "bin", "node_modules", "nanoid", ".claude", "x"), 32)
	bbWrite(t, filepath.Join(mixed, ".claude", "settings.local.json"), 32)
	bbAge(t, mixed, 60*time.Hour)
	// A working tree's own .claude, beside its node_modules, and ~/.claude
	// stay out of reach, bases inside them included.
	repoBase := filepath.Join(f.root, "git", "app", ".claude", "worktrees", "w", "out")
	bbMakeBase(t, repoBase, 4096, 60*time.Hour)
	bbWrite(t, filepath.Join(f.root, "git", "app", "node_modules", "cytoscape", ".claude", "x"), 32)
	homeBase := filepath.Join(f.home, ".claude", "projects", "p", "out")
	bbMakeBase(t, homeBase, 4096, 60*time.Hour)
	before := map[string]string{
		"repo": bbManifest(t, filepath.Join(f.root, "git", "app", ".claude")),
		"home": bbManifest(t, filepath.Join(f.home, ".claude")),
	}

	cycle := f.run()
	for _, b := range []string{npm, ext} {
		if bbExists(b) {
			t.Errorf("idle base %s whose only .claude is a generated copy was kept: %v", b, bbRowPaths(cycle))
		}
		if r := bbRowFor(t, cycle, b); r.Decision != "removed" {
			t.Errorf("%s: %q (%s)", b, r.Decision, r.Reason)
		}
	}
	if r := bbRowFor(t, cycle, mixed); r.Decision != "keep" || !strings.Contains(r.Reason, filepath.Join(mixed, ".claude")) {
		t.Errorf("mixed base: %q (%s)", r.Decision, r.Reason)
	}
	if !bbExists(filepath.Join(mixed, ".claude", "settings.local.json")) {
		t.Error("a base holding a session directory outside its generated areas was removed")
	}
	after := map[string]string{
		"repo": bbManifest(t, filepath.Join(f.root, "git", "app", ".claude")),
		"home": bbManifest(t, filepath.Join(f.home, ".claude")),
	}
	for k := range before {
		if before[k] != after[k] {
			t.Errorf("agent-session tree %s changed: %v", k, bbRowPaths(cycle))
		}
	}
	for _, r := range cycle.Rows {
		if r.Path == repoBase || r.Path == homeBase {
			t.Errorf("a base inside an agent-session directory was discovered: %s (%s)", r.Path, r.Reason)
		}
	}
}

// The temp sweep and shm_scratch apply the same rule: a package copy no
// longer shields a stale tree, a working tree's own .claude still does.
func TestTempSweepAndShmScratchIgnorePackageCopiesOnly(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	copyFile := filepath.Join(tmp, "build", "node_modules", "pkg", ".claude", "old.json")
	sessionFile := filepath.Join(tmp, "build", ".claude", "old.json")
	bbWrite(t, copyFile, 64)
	bbWrite(t, sessionFile, 64)
	old := time.Now().Add(-72 * time.Hour)
	for _, p := range []string{copyFile, sessionFile} {
		_ = os.Chtimes(p, old, old)
	}
	deleteOldFilesSameDevice(tmp, time.Hour)
	if bbExists(copyFile) {
		t.Error("the temp sweep still treats a package's .claude as session data")
	}
	if !bbExists(sessionFile) {
		t.Fatal("the temp sweep aged out a working tree's .claude (TIN-5268)")
	}

	f := newShmFixture(t)
	copies := f.tree("npm-scratch", 48*time.Hour, "node_modules/pkg/.claude/x", "node_modules/pkg/index.js")
	ws := f.tree("ws-with-deps", 48*time.Hour, ".claude/x", "node_modules/pkg/.claude/y")
	f.run()
	if exists(copies) {
		t.Error("shm_scratch kept an idle tree whose only .claude is a package copy")
	}
	if !exists(ws) {
		t.Fatal("shm_scratch removed a tree holding its own .claude (TIN-5268)")
	}
}
