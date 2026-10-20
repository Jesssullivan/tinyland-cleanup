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
// The TIN-5268 name rule no longer counts a session-named directory that is
// an npm package's own directory, or a fetched external repository's own
// .claude, unless it holds a session signature. Everything else it denied
// stays denied, including every other place in an execroot or sandbox.

// bbPackage makes dir an npm package directory (a real directory holding a
// package.json) and returns it.
func bbPackage(t *testing.T, dir string) string {
	t.Helper()
	bbWrite(t, filepath.Join(dir, "package.json"), 16)
	return dir
}

func TestAgentSessionDenyIgnoresPackageAndBazelGeneratedCopies(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	tmp := filepath.Join(dir, "tmp")
	bbMkdir(t, filepath.Join(home, ".claude", "projects"))
	// A structural output base: execroot, external, server, lock and
	// DO_NOT_BUILD_HERE.
	base := filepath.Join(dir, "cache", "bazel", "_bazel_u", "0123abcd")
	bbMakeBase(t, base, 16, time.Hour)
	// What the four packages publish: .claude/settings.local.json (resolve
	// also .claude/notes.md).
	rulesJS := bbPackage(t, filepath.Join(base, "execroot", "_main", "bazel-out", "k8-fastbuild", "bin", "node_modules",
		".aspect_rules_js", "cytoscape@3.34.1", "node_modules", "cytoscape"))
	bbWrite(t, filepath.Join(rulesJS, ".claude", "settings.local.json"), 8)
	// What spear_resumes tracks: .claude/skills/.
	bbWrite(t, filepath.Join(base, "external", "spear_resumes+", ".claude", "skills", "spear-resume", "SKILL.md"), 8)
	// Elsewhere in the execroot or a sandbox nothing is a copy.
	bbWrite(t, filepath.Join(base, "execroot", "_main", ".claude", "settings.json"), 8)
	bbWrite(t, filepath.Join(base, "sandbox", "linux-sandbox", "7", "execroot", "_main", ".codex", "config.toml"), 8)
	// A working tree whose own .claude sits beside its node_modules.
	ws := filepath.Join(dir, "git", "app")
	bbMkdir(t, filepath.Join(ws, ".claude", "worktrees", "w"))
	cytoscape := bbPackage(t, filepath.Join(ws, "node_modules", "cytoscape"))
	bbWrite(t, filepath.Join(cytoscape, ".claude", "settings.local.json"), 8)
	resolve := bbPackage(t, filepath.Join(ws, "node_modules", "resolve"))
	bbWrite(t, filepath.Join(resolve, ".claude", "notes.md"), 8)
	scoped := bbPackage(t, filepath.Join(ws, "node_modules", "@acme", "ui"))
	bbWrite(t, filepath.Join(scoped, ".claude", "commands", "x.md"), 8)
	pkgCodex := bbPackage(t, filepath.Join(ws, "node_modules", "tool"))
	bbWrite(t, filepath.Join(pkgCodex, ".codex", "config.toml"), 8)
	// Package-like places that are not a package's own directory.
	bbWrite(t, filepath.Join(ws, "node_modules", ".claude", "x"), 8)
	bbWrite(t, filepath.Join(ws, "node_modules", ".cache", ".claude", "x"), 8)
	bbWrite(t, filepath.Join(ws, "node_modules", "nopkg", ".claude", "x"), 8)
	bbWrite(t, filepath.Join(ws, "node_modules", "cytoscape", "dist", ".claude", "x"), 8)
	// A node_modules entry linking into a real working tree (npm link, a
	// pnpm workspace package) is judged at its physical path.
	linked := bbPackage(t, filepath.Join(dir, "git", "lib"))
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
		filepath.Join(ws, "node_modules", ".claude", "x"),
		filepath.Join(ws, "node_modules", ".cache", ".claude", "x"),
		filepath.Join(ws, "node_modules", "nopkg", ".claude", "x"),
		filepath.Join(ws, "node_modules", "cytoscape", "dist", ".claude", "x"),
		filepath.Join(ws, "node_modules", "missing", ".claude", "x"),
		filepath.Join(lookalike, "external", "vendored", ".claude", "x"),
		filepath.Join(base, "external", "never-fetched", ".claude", "x"),
		filepath.Join(base, "execroot", "_main", ".claude", "settings.json"),
		filepath.Join(base, "sandbox", "linux-sandbox", "7", "execroot", "_main", ".codex", "config.toml"),
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
		filepath.Join(cytoscape, ".claude", "settings.local.json"),
		filepath.Join(resolve, ".claude", "notes.md"),
		filepath.Join(scoped, ".claude", "commands", "x.md"),
		filepath.Join(pkgCodex, ".codex", "config.toml"),
		filepath.Join(rulesJS, ".claude", "settings.local.json"),
		filepath.Join(base, "external", "spear_resumes+", ".claude", "skills", "spear-resume", "SKILL.md"),
	} {
		if why, denied := d.denied(p); denied {
			t.Errorf("%s is a generated copy but is denied: %s", p, why)
		}
	}
}

// Independent review 2026-10-19, honey probes: real session data in
// package- or base-shaped places stays denied.
func TestAgentSessionDenyKeepsSessionsInPackageAndBazelShapedPlaces(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := newAgentSessionDeny(filepath.Join(dir, "home"), 1000, "", nil, nil)
	// Probe 1: an agent worktree under a package-shaped directory.
	app := bbPackage(t, filepath.Join(dir, "src", "node_modules", "app"))
	worktree := filepath.Join(app, ".claude", "worktrees", "fix", "main.go")
	bbWrite(t, worktree, 8)
	// Probe 2: a seat directory holding repositories named execroot and
	// server and a lock file (bbIsOutputBase accepts it).
	seat := filepath.Join(dir, "seat")
	bbWrite(t, filepath.Join(seat, "execroot", ".git", "HEAD"), 8)
	bbWrite(t, filepath.Join(seat, "execroot", ".claude", "settings.local.json"), 8)
	bbWrite(t, filepath.Join(seat, "server", ".git", "HEAD"), 8)
	bbWrite(t, filepath.Join(seat, "external", "x", ".claude", "settings.json"), 8)
	bbWrite(t, filepath.Join(seat, "lock"), 0)
	// Probe 3: an agent run hosted by a Bazel action or test, with HOME in
	// the execroot's _tmp.
	base := filepath.Join(dir, "out")
	bbMakeBase(t, base, 16, time.Hour)
	rollout := filepath.Join(base, "execroot", "_main", "_tmp", "4f2a", "home", ".codex", "sessions", "2026", "rollout.jsonl")
	bbWrite(t, rollout, 8)
	transcript := filepath.Join(base, "external", "repo+", ".claude", "history.jsonl")
	bbWrite(t, transcript, 8)
	db := filepath.Join(base, "sandbox", "s", "1", ".crush", "crush.db")
	bbWrite(t, db, 8)
	projects := bbPackage(t, filepath.Join(base, "execroot", "_main", "node_modules", "tool"))
	bbWrite(t, filepath.Join(projects, ".claude", "projects", "p", "x.json"), 8)
	// A base's area that is itself a git repository is not generated.
	gitArea := filepath.Join(dir, "out2")
	bbMakeBase(t, gitArea, 16, time.Hour)
	bbMkdir(t, filepath.Join(gitArea, "external", ".git"))
	bbWrite(t, filepath.Join(gitArea, "external", "r", ".claude", "settings.json"), 8)
	// Round 2, probe 1: a git clone at node_modules/<pkg> (package.json and
	// .git) whose .claude holds only configuration.
	var round2 []string
	clone := bbPackage(t, filepath.Join(dir, "src", "node_modules", "cloned"))
	bbWrite(t, filepath.Join(clone, ".git", "HEAD"), 8)
	for _, rel := range []string{"settings.local.json", "plans/p.md", "agents/a.md", "commands/c.md"} {
		round2 = append(round2, filepath.Join(clone, ".claude", rel))
		bbWrite(t, round2[len(round2)-1], 8)
	}
	wtClone := bbPackage(t, filepath.Join(dir, "src", "node_modules", "worktree-clone"))
	bbWrite(t, filepath.Join(wtClone, ".git"), 8) // a linked work tree's .git file
	round2 = append(round2, filepath.Join(wtClone, ".claude", "settings.local.json"))
	bbWrite(t, round2[len(round2)-1], 8)
	// Round 2, probe 2: real tool data in package-shaped directories.
	for i, rel := range []string{
		".claude/file-history/f", ".claude/todos/t.json", ".claude/plans/p.md", ".claude/shell-snapshots/s.sh",
		".gstack/qa-reports/r.md", ".junie/memory/m.md", ".tmux/resurrect/last.txt",
	} {
		pkg := bbPackage(t, filepath.Join(dir, "deps", "node_modules", "p"+string(rune('a'+i))))
		round2 = append(round2, filepath.Join(pkg, rel))
		bbWrite(t, round2[len(round2)-1], 8)
	}
	// Round 2, probe 3: a config-only .claude in a repo cloned under the
	// execroot's _tmp, and configuration in a fetched repository's .claude.
	for _, rel := range []string{
		"execroot/_main/_tmp/9c1e/repo/.claude/settings.local.json",
		"execroot/_main/_tmp/9c1e/repo/.claude/commands/c.md",
		"external/fetched+/.claude/settings.local.json",
		"external/agents+/.claude/agents/a.md",
		"external/codexrepo+/.codex/config.toml",
		"external/gstackrepo+/.gstack/browse.json",
	} {
		round2 = append(round2, filepath.Join(base, rel))
		bbWrite(t, round2[len(round2)-1], 8)
	}

	for _, p := range append(round2,
		worktree,
		filepath.Join(seat, "execroot", ".claude", "settings.local.json"),
		filepath.Join(seat, "external", "x", ".claude", "settings.json"),
		rollout,
		transcript,
		db,
		filepath.Join(projects, ".claude", "projects", "p", "x.json"),
		filepath.Join(gitArea, "external", "r", ".claude", "settings.json"),
	) {
		if _, denied := d.denied(p); !denied {
			t.Errorf("%s is session data but is not denied", p)
		}
	}
	if bbIsGeneratedOutputBase(seat) {
		t.Error("a seat with repositories named execroot and server passes as an output base")
	}
	if !bbIsGeneratedOutputBase(base) {
		t.Error("a structural output base does not pass")
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
	bbWrite(t, filepath.Join(bbPackage(t, filepath.Join(only, "node_modules", "nanoid")), ".claude", "settings.json"), 8)
	bbWrite(t, filepath.Join(bbPackage(t, filepath.Join(only, "a", "node_modules", ".pnpm", "resolve@1.22.12", "node_modules", "resolve")),
		".claude", "x"), 8)
	if _, _, denied, err := bbMeasureTree(context.Background(), only, time.Time{}, os.Getuid()); denied != "" || err != nil {
		t.Fatalf("package copies held the tree: denied=%q err=%v", denied, err)
	}
	ws := filepath.Join(dir, "workspace")
	bbWrite(t, filepath.Join(ws, ".claude", "settings.local.json"), 8)
	bbWrite(t, filepath.Join(bbPackage(t, filepath.Join(ws, "node_modules", "dockerode")), ".claude", "x"), 8)
	if _, _, denied, _ := bbMeasureTree(context.Background(), ws, time.Time{}, os.Getuid()); denied != filepath.Join(ws, ".claude") {
		t.Fatalf("workspace .claude beside node_modules: denied=%q", denied)
	}
	later := filepath.Join(dir, "later")
	bbWrite(t, filepath.Join(bbPackage(t, filepath.Join(later, "node_modules", "pkg")), ".claude", "x"), 8)
	bbWrite(t, filepath.Join(later, "src", ".codex", "history.jsonl"), 8)
	if _, _, denied, _ := bbMeasureTree(context.Background(), later, time.Time{}, os.Getuid()); denied != filepath.Join(later, "src", ".codex") {
		t.Fatalf("a session directory after a package copy: denied=%q", denied)
	}
	probe := filepath.Join(dir, "probe")
	app := bbPackage(t, filepath.Join(probe, "src", "node_modules", "app"))
	bbWrite(t, filepath.Join(app, ".claude", "worktrees", "fix", "main.go"), 8)
	if _, _, denied, _ := bbMeasureTree(context.Background(), probe, time.Time{}, os.Getuid()); denied != filepath.Join(app, ".claude") {
		t.Fatalf("a worktree in a package-shaped .claude: denied=%q", denied)
	}
}

func TestBazelBasesReapsBasesWhoseOnlySessionNamesAreGeneratedCopies(t *testing.T) {
	f := newBBFixture(t)
	f.cfg.BazelBases.Roots = []string{f.root, f.home}
	npm := filepath.Join(f.root, "cache", "_bazel_u", "npm")
	bbMakeBase(t, npm, 4096, 60*time.Hour)
	bbWrite(t, filepath.Join(bbPackage(t, filepath.Join(npm, "execroot", "_main", "bazel-out", "k8-fastbuild", "bin", "node_modules",
		".aspect_rules_js", "cytoscape@3.34.1", "node_modules", "cytoscape")), ".claude", "settings.local.json"), 32)
	bbAge(t, npm, 60*time.Hour)
	ext := filepath.Join(f.root, "cache", "_bazel_u", "ext")
	bbMakeBase(t, ext, 4096, 60*time.Hour)
	bbWrite(t, filepath.Join(ext, "external", "spear_resumes+", ".claude", "skills", "spear-resume", "SKILL.md"), 32)
	bbAge(t, ext, 60*time.Hour)
	// The same copy beside a session directory outside the generated areas
	// keeps the base.
	mixed := filepath.Join(f.root, "cache", "_bazel_u", "mixed")
	bbMakeBase(t, mixed, 4096, 60*time.Hour)
	bbWrite(t, filepath.Join(bbPackage(t, filepath.Join(mixed, "execroot", "_main", "bazel-out", "bin", "node_modules", "nanoid")),
		".claude", "x"), 32)
	bbWrite(t, filepath.Join(mixed, ".claude", "settings.local.json"), 32)
	bbAge(t, mixed, 60*time.Hour)
	// Probe 3: an agent run a Bazel test hosted keeps its base.
	hosted := filepath.Join(f.root, "cache", "_bazel_u", "hosted")
	bbMakeBase(t, hosted, 4096, 60*time.Hour)
	rollout := filepath.Join(hosted, "execroot", "_main", "_tmp", "4f2a", "home", ".codex", "sessions", "2026", "rollout.jsonl")
	bbWrite(t, rollout, 32)
	bbAge(t, hosted, 60*time.Hour)
	// Round 2, probe 3: a config-only .claude in a repo cloned under the
	// execroot's _tmp keeps its base.
	tmpClone := filepath.Join(f.root, "cache", "_bazel_u", "tmpclone")
	bbMakeBase(t, tmpClone, 4096, 60*time.Hour)
	cloned := filepath.Join(tmpClone, "execroot", "_main", "_tmp", "9c1e", "repo", ".claude", "settings.local.json")
	bbWrite(t, cloned, 32)
	bbAge(t, tmpClone, 60*time.Hour)
	// Probe 2: a seat holding repositories named execroot and server and a
	// lock file is discovered as a base and must be kept whole.
	seat := filepath.Join(f.root, "seat")
	bbWrite(t, filepath.Join(seat, "execroot", ".git", "HEAD"), 8)
	bbWrite(t, filepath.Join(seat, "execroot", ".claude", "settings.local.json"), 8)
	bbWrite(t, filepath.Join(seat, "server", ".git", "HEAD"), 8)
	bbWrite(t, filepath.Join(seat, "server", "main.go"), 8)
	bbWrite(t, filepath.Join(seat, "lock"), 0)
	bbAge(t, seat, 60*time.Hour)
	// A working tree's own .claude, beside its node_modules, and ~/.claude
	// stay out of reach, bases inside them included.
	repoBase := filepath.Join(f.root, "git", "app", ".claude", "worktrees", "w", "out")
	bbMakeBase(t, repoBase, 4096, 60*time.Hour)
	bbWrite(t, filepath.Join(bbPackage(t, filepath.Join(f.root, "git", "app", "node_modules", "cytoscape")), ".claude", "x"), 32)
	homeBase := filepath.Join(f.home, ".claude", "projects", "p", "out")
	bbMakeBase(t, homeBase, 4096, 60*time.Hour)
	before := map[string]string{
		"repo": bbManifest(t, filepath.Join(f.root, "git", "app", ".claude")),
		"home": bbManifest(t, filepath.Join(f.home, ".claude")),
		"seat": bbManifest(t, seat),
	}
	// Reading the manifests refreshed atimes; age the seat again so it is
	// kept for what it holds, not for a recent use.
	bbAge(t, seat, 60*time.Hour)

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
	if r := bbRowFor(t, cycle, hosted); r.Decision != "keep" || !strings.Contains(r.Reason, "agent-session") || !bbExists(rollout) {
		t.Errorf("a base holding a hosted agent run: %q (%s)", r.Decision, r.Reason)
	}
	if r := bbRowFor(t, cycle, tmpClone); r.Decision != "keep" || !strings.Contains(r.Reason, "agent-session") || !bbExists(cloned) {
		t.Errorf("a base holding a clone's .claude under _tmp: %q (%s)", r.Decision, r.Reason)
	}
	if r := bbRowFor(t, cycle, seat); r.Decision != "keep" || !strings.Contains(r.Reason, "git root") {
		t.Errorf("seat with repositories named execroot and server: %q (%s)", r.Decision, r.Reason)
	}
	after := map[string]string{
		"repo": bbManifest(t, filepath.Join(f.root, "git", "app", ".claude")),
		"home": bbManifest(t, filepath.Join(f.home, ".claude")),
		"seat": bbManifest(t, seat),
	}
	for k := range before {
		if before[k] != after[k] {
			t.Errorf("tree %s changed: %v", k, bbRowPaths(cycle))
		}
	}
	for _, r := range cycle.Rows {
		if r.Path == repoBase || r.Path == homeBase {
			t.Errorf("a base inside an agent-session directory was discovered: %s (%s)", r.Path, r.Reason)
		}
	}
}

// The nested git guard keeps a base candidate holding a repository outside
// the generated areas, and ignores the .git a fetched repository carries.
func TestBBGuardRefusesNestedGitRoots(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clean := filepath.Join(dir, "clean")
	bbMakeBase(t, clean, 16, time.Hour)
	bbWrite(t, filepath.Join(clean, "external", "git_repo+", ".git", "HEAD"), 8)
	if why := bbGuard(context.Background(), clean, dir, []string{"/"}, nil, ""); why != "" {
		t.Fatalf("a fetched repository's .git kept the base: %s", why)
	}
	nested := filepath.Join(dir, "nested")
	bbMakeBase(t, nested, 16, time.Hour)
	bbWrite(t, filepath.Join(nested, "server", "checkout", ".git"), 8) // a work tree's .git file
	if why := bbGuard(context.Background(), nested, dir, []string{"/"}, nil, ""); !strings.Contains(why, "git root") {
		t.Fatalf("a nested work tree did not keep the base: %q", why)
	}
	named := filepath.Join(dir, "named")
	bbMakeBase(t, named, 16, time.Hour)
	bbMkdir(t, filepath.Join(named, "execroot", ".git"))
	if why := bbGuard(context.Background(), named, dir, []string{"/"}, nil, ""); !strings.Contains(why, "git root") {
		t.Fatalf("a repository named execroot did not keep the base: %q", why)
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
	pkg := bbPackage(t, filepath.Join(tmp, "build", "node_modules", "pkg"))
	copyFile := filepath.Join(pkg, ".claude", "old.json")
	sessionFile := filepath.Join(tmp, "build", ".claude", "old.json")
	worktreeFile := filepath.Join(tmp, "build", "node_modules", "app", ".claude", "worktrees", "w", "old.go")
	bbPackage(t, filepath.Join(tmp, "build", "node_modules", "app"))
	// Round 2, probe 1: a clone's config-only .claude under node_modules.
	clone := bbPackage(t, filepath.Join(tmp, "build", "node_modules", "cloned"))
	bbMkdir(t, filepath.Join(clone, ".git"))
	cloneFile := filepath.Join(clone, ".claude", "settings.local.json")
	bbWrite(t, copyFile, 64)
	bbWrite(t, sessionFile, 64)
	bbWrite(t, worktreeFile, 64)
	bbWrite(t, cloneFile, 64)
	old := time.Now().Add(-72 * time.Hour)
	for _, p := range []string{copyFile, sessionFile, worktreeFile, cloneFile} {
		_ = os.Chtimes(p, old, old)
	}
	deleteOldFilesSameDevice(tmp, time.Hour)
	if bbExists(copyFile) {
		t.Error("the temp sweep still treats a package's .claude as session data")
	}
	if !bbExists(sessionFile) || !bbExists(worktreeFile) || !bbExists(cloneFile) {
		t.Fatal("the temp sweep aged out session data (TIN-5268)")
	}

	f := newShmFixture(t)
	copies := f.tree("npm-scratch", 48*time.Hour, "node_modules/pkg/.claude/x", "node_modules/pkg/package.json")
	ws := f.tree("ws-with-deps", 48*time.Hour, ".claude/x", "node_modules/pkg/.claude/y", "node_modules/pkg/package.json")
	sessions := f.tree("pkg-sessions", 48*time.Hour, "node_modules/pkg/.codex/sessions/r.jsonl", "node_modules/pkg/package.json")
	f.run()
	if exists(copies) {
		t.Error("shm_scratch kept an idle tree whose only .claude is a package copy")
	}
	if !exists(ws) || !exists(sessions) {
		t.Fatal("shm_scratch removed a tree holding session data (TIN-5268)")
	}
}
