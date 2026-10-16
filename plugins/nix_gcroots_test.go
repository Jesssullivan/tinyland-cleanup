package plugins

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// gcRootFixture lays out a fake auto-roots directory, store directory and
// home tree. Each root is an out-link registered the way Nix registers an
// indirect root: an entry in auto/ pointing at the out-link.
type gcRootFixture struct {
	t        *testing.T
	base     string
	autoDir  string
	storeDir string
	n        int
}

func newGCRootFixture(t *testing.T) *gcRootFixture {
	t.Helper()
	base := t.TempDir()
	f := &gcRootFixture{
		t:        t,
		base:     base,
		autoDir:  filepath.Join(base, "auto"),
		storeDir: filepath.Join(base, "store"),
	}
	if err := os.MkdirAll(f.autoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// root creates an out-link at rel (under base) selecting a store path and
// registers it in auto/. It returns the out-link path and its store path.
func (f *gcRootFixture) root(rel string) (string, string) {
	f.t.Helper()
	f.n++
	link := filepath.Join(f.base, rel)
	storePath := filepath.Join(f.storeDir, strings.Repeat("a", 8)+"-"+filepath.Base(rel)+"-"+string(rune('a'+f.n%26)))
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(storePath, link); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(link, filepath.Join(f.autoDir, "root"+string(rune('a'+f.n%26))+strings.Repeat("x", f.n))); err != nil {
		f.t.Fatal(err)
	}
	return link, storePath
}

func (f *gcRootFixture) mkdir(rel string) string {
	f.t.Helper()
	path := filepath.Join(f.base, rel)
	if err := os.MkdirAll(path, 0o755); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f *gcRootFixture) reaper(now time.Time) nixGCRootReaper {
	r := newNixGCRootReaper(now)
	r.autoDir = f.autoDir
	r.storeDir = f.storeDir
	return r
}

// candidatesFile is where the fixture's reaper lists stale roots.
func (f *gcRootFixture) candidatesFile() string {
	return filepath.Join(f.base, "state", "tinyland-cleanup", "gcroot-candidates")
}

// candidates returns the listed "<name>\t<link>" lines, header excluded.
func (f *gcRootFixture) candidates() []string {
	f.t.Helper()
	body, err := os.ReadFile(f.candidatesFile())
	if err != nil {
		f.t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	return lines
}

func (f *gcRootFixture) plugin(now time.Time) *NixPlugin {
	p := NewNixPlugin()
	p.autoGCRootsDir = f.autoDir
	p.storeDir = f.storeDir
	p.now = func() time.Time { return now }
	p.liveCwds = func() ([]string, error) { return nil, nil }
	return p
}

func (f *gcRootFixture) autoEntries() int {
	f.t.Helper()
	entries, err := os.ReadDir(f.autoDir)
	if err != nil {
		f.t.Fatal(err)
	}
	return len(entries)
}

func staleByLink(roots []nixStaleGCRoot) map[string]bool {
	out := map[string]bool{}
	for _, r := range roots {
		out[r.Link] = r.Stale
	}
	return out
}

func TestNixStaleGCRootsDirenvFreshCheckoutIsKept(t *testing.T) {
	f := newGCRootFixture(t)
	f.mkdir("git/live/.git")
	profile, _ := f.root("git/live/.direnv/flake-profile-abc")
	input, _ := f.root("git/live/.direnv/flake-inputs/src-1")

	roots, err := f.reaper(time.Now().Add(time.Hour)).collect(nil, 7*24*time.Hour, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got := staleByLink(roots)
	if len(got) != 2 || got[profile] || got[input] {
		t.Fatalf("a checkout loaded an hour ago keeps its roots: %+v", roots)
	}
	for _, r := range roots {
		if r.Class != nixStaleGCRootClassDirenv {
			t.Fatalf("expected direnv class, got %+v", r)
		}
	}
}

func TestNixStaleGCRootsDirenvStaleCheckoutLosesEveryRoot(t *testing.T) {
	f := newGCRootFixture(t)
	f.mkdir("git/old/.git")
	profile, _ := f.root("git/old/.direnv/flake-profile-abc")
	input, _ := f.root("git/old/.direnv/flake-inputs/src-1")

	roots, err := f.reaper(time.Now().Add(8*24*time.Hour)).collect(nil, 7*24*time.Hour, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got := staleByLink(roots)
	if !got[profile] || !got[input] {
		t.Fatalf("a checkout unloaded for 8 days loses every nix-direnv root: %+v", roots)
	}
}

// M2: a .direnv with no .git or .jj above it is not stale at once; the
// 7-day age rule applies to it too.
func TestNixStaleGCRootsDirenvWithoutAVCSFollowsTheAgeRule(t *testing.T) {
	f := newGCRootFixture(t)
	input, _ := f.root("scratch/copied/.direnv/flake-inputs/src-1")

	roots, err := f.reaper(time.Now().Add(time.Hour)).collect(nil, 7*24*time.Hour, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if staleByLink(roots)[input] {
		t.Fatalf("a recently loaded .direnv with no checkout above it is kept: %+v", roots)
	}
	roots, err = f.reaper(time.Now().Add(8*24*time.Hour)).collect(nil, 7*24*time.Hour, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !staleByLink(roots)[input] {
		t.Fatalf("after 7 days it is stale: %+v", roots)
	}
}

// M2: a monorepo subdirectory or a jj checkout is found by walking up.
func TestNixStaleGCRootsFindTheCheckoutAboveASubdirectory(t *testing.T) {
	for _, marker := range []string{".git", ".jj"} {
		t.Run(marker, func(t *testing.T) {
			f := newGCRootFixture(t)
			f.mkdir("repo/" + marker)
			profile, _ := f.root("repo/services/api/.direnv/flake-profile-abc")
			roots, err := f.reaper(time.Now().Add(time.Hour)).collect(nil, 7*24*time.Hour, 72*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if staleByLink(roots)[profile] {
				t.Fatalf("a fresh subdirectory .direnv under %s is kept: %+v", marker, roots)
			}
			if vcs, err := nixFindVCSRoot(filepath.Join(f.base, "repo/services/api")); err != nil || vcs != filepath.Join(f.base, "repo") {
				t.Fatalf("walk-up found %q %v", vcs, err)
			}
		})
	}
}

// M2: any stat error other than "does not exist" keeps the roots.
func TestNixStaleGCRootsKeepDirenvRootsOnAStatError(t *testing.T) {
	f := newGCRootFixture(t)
	profile, _ := f.root("scratch/copied/.direnv/flake-profile-abc")
	orig := nixLstat
	nixLstat = func(path string) (os.FileInfo, error) {
		if filepath.Base(path) == ".git" {
			return nil, syscall.EACCES
		}
		return orig(path)
	}
	defer func() { nixLstat = orig }()

	roots, err := f.reaper(time.Now().Add(30*24*time.Hour)).collect(nil, 7*24*time.Hour, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if staleByLink(roots)[profile] {
		t.Fatalf("a stat error must keep the roots: %+v", roots)
	}
}

// M1: an out-link in a live agent-session tree is never listed; an idle one
// needs the 7-day idle rule, not the 72 h temp rule.
func TestNixStaleGCRootsProtectLiveAgentSessionTrees(t *testing.T) {
	f := newGCRootFixture(t)
	tmp := f.mkdir("tmp-agents")
	result, _ := f.root("tmp-agents/claude-1000/-srv-repo/session-1/scratchpad/result")
	codex, _ := f.root("tmp-agents/codex-abc/result")
	plain, _ := f.root("tmp-agents/hm-qualification/candidate")
	tree := filepath.Join(tmp, "claude-1000")

	reaper := func(now time.Time, cwds []string, err error) nixGCRootReaper {
		r := f.reaper(now)
		r.agentPrefixes = []string{"claude-*", "codex*", ".tmp*"}
		r.agentIdle = 7 * 24 * time.Hour
		r.liveCwds = func() ([]string, error) { return cwds, err }
		return r
	}
	collect := func(r nixGCRootReaper) map[string]bool {
		t.Helper()
		roots, err := r.collect([]string{tmp}, 7*24*time.Hour, 72*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return staleByLink(roots)
	}

	got := collect(reaper(time.Now().Add(73*time.Hour), nil, nil))
	if got[result] || got[codex] {
		t.Fatalf("72 h is not enough idle time for an agent-session tree: %v", got)
	}
	if !got[plain] {
		t.Fatalf("a plain temp out-link still follows the 72 h rule: %v", got)
	}
	got = collect(reaper(time.Now().Add(8*24*time.Hour), []string{filepath.Join(tree, "-srv-repo", "session-1")}, nil))
	if got[result] {
		t.Fatalf("a live process in the tree keeps its roots: %v", got)
	}
	if !got[codex] {
		t.Fatalf("an idle tree with no live process is listed after 7 days: %v", got)
	}
	got = collect(reaper(time.Now().Add(8*24*time.Hour), nil, errors.New("no /proc")))
	if got[result] || got[codex] {
		t.Fatalf("inspection failure keeps every agent-session root: %v", got)
	}
}

func TestNixStaleGCRootsTemporaryOutLinksAgeOut(t *testing.T) {
	f := newGCRootFixture(t)
	tmp := f.mkdir("tmp")
	result, _ := f.root("tmp/session/scratchpad/result")
	elsewhere, _ := f.root("git/repo/result")

	young, err := f.reaper(time.Now().Add(time.Hour)).collect([]string{tmp}, 7*24*time.Hour, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got := staleByLink(young); len(got) != 1 || got[result] {
		t.Fatalf("an hour-old temporary out-link is kept and a non-temp out-link is not classified: %+v", young)
	}

	old, err := f.reaper(time.Now().Add(73*time.Hour)).collect([]string{tmp}, 7*24*time.Hour, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got := staleByLink(old)
	if !got[result] {
		t.Fatalf("a 73h-old temporary out-link is stale: %+v", old)
	}
	if _, classified := got[elsewhere]; classified {
		t.Fatalf("an out-link outside every temp dir is left alone: %+v", old)
	}
}

func TestNixStaleGCRootsResolveSymlinkedTempDirs(t *testing.T) {
	f := newGCRootFixture(t)
	real := f.mkdir("srv/scratch/home-tmp")
	alias := filepath.Join(f.base, "home", "tmp")
	if err := os.MkdirAll(filepath.Dir(alias), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	result, _ := f.root("srv/scratch/home-tmp/qualification/candidate")

	cfg := config.DefaultConfig()
	cfg.Nix.TempGCRootPaths = []string{alias}
	roots, err := f.reaper(time.Now().Add(73*time.Hour)).collect(nixStaleGCRootTempDirs(cfg), 7*24*time.Hour, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !staleByLink(roots)[result] {
		t.Fatalf("a temp dir named through a symlink covers out-links under its target: %+v", roots)
	}
}

func TestNixStaleGCRootsNeverTouchNamedPinsProfilesOrBazelOutLinks(t *testing.T) {
	f := newGCRootFixture(t)
	tmp := f.mkdir("tmp")
	f.root("tmp/state/gcroots/tin-2447-pg15-recovery-pins-20261008/postgresql")
	f.root(".local/state/nix/gcroots/tin-4405-restored/1")
	f.root("tmp/profiles/profile-3-link")
	f.root("tmp/bazel-out/external/rules_nixpkgs_core++nix_pkg+bash/bazel-support/nix-out-link")
	f.root("scratch/copied/gcroots/.direnv/flake-inputs/src-1")

	roots, err := f.reaper(time.Now().Add(365*24*time.Hour)).collect([]string{tmp}, 7*24*time.Hour, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 0 {
		t.Fatalf("named gcroots pins, profiles and Bazel nix-out-links are never classified: %+v", roots)
	}
}

func TestNixStaleGCRootsSkipForeignOwnersAndNonStoreTargets(t *testing.T) {
	f := newGCRootFixture(t)
	tmp := f.mkdir("tmp")
	f.root("tmp/session/result")

	notStore := filepath.Join(tmp, "not-store")
	if err := os.Symlink(filepath.Join(f.base, "elsewhere"), notStore); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(notStore, filepath.Join(f.autoDir, "notstore")); err != nil {
		t.Fatal(err)
	}

	foreign := f.reaper(time.Now().Add(365 * 24 * time.Hour))
	foreign.uid = os.Getuid() + 1
	roots, err := foreign.collect([]string{tmp}, 7*24*time.Hour, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 0 {
		t.Fatalf("links the daemon user does not own are never classified: %+v", roots)
	}

	roots, err = f.reaper(time.Now().Add(365*24*time.Hour)).collect([]string{tmp}, 7*24*time.Hour, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range roots {
		if r.Link == notStore {
			t.Fatalf("a link that does not select a store path is never classified: %+v", roots)
		}
	}
}

func TestNixStaleGCRootCandidatesListOnlyStaleRootsAndTouchNoUserFile(t *testing.T) {
	f := newGCRootFixture(t)
	direnv := f.mkdir("scratch/copied/.direnv")
	rc := filepath.Join(direnv, "flake-profile-abc.rc")
	if err := os.WriteFile(rc, []byte("export X=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile, profileStore := f.root("scratch/copied/.direnv/flake-profile-abc")
	input, inputStore := f.root("scratch/copied/.direnv/flake-inputs/src-1")
	tmp := f.mkdir("tmp")
	fresh, _ := f.root("tmp/young/result")

	cfg := config.DefaultConfig()
	cfg.Nix.ReapStaleGCRoots = true
	cfg.Nix.GCRootCandidatesFile = f.candidatesFile()
	// Eight days on, the direnv roots pass the 7-day rule; the temp out-link
	// is held back by a 30-day temp threshold.
	cfg.Nix.TempGCRootPaths = []string{tmp}
	cfg.Nix.TempGCRootStaleAfter = "30d"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if n := f.plugin(time.Now().Add(8*24*time.Hour)).publishStaleGCRootCandidates(cfg, logger); n != 2 {
		t.Fatalf("expected the two stale roots listed, got %d", n)
	}
	lines := f.candidates()
	if len(lines) != 2 {
		t.Fatalf("expected two candidate lines, got %q", lines)
	}
	listed := map[string]bool{}
	for _, line := range lines {
		name, link, ok := strings.Cut(line, "\t")
		if !ok || strings.Contains(name, "/") {
			t.Fatalf("malformed candidate line %q", line)
		}
		if dest, err := os.Readlink(filepath.Join(f.autoDir, name)); err != nil || dest != link {
			t.Fatalf("candidate %q must name the auto entry that registers its link: %q %v", line, dest, err)
		}
		listed[link] = true
	}
	if !listed[profile] || !listed[input] || listed[fresh] {
		t.Fatalf("only the stale checkout's roots are listed: %v", listed)
	}
	info, err := os.Stat(f.candidatesFile())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the candidates file is 0600: %v %v", info, err)
	}
	if n := f.autoEntries(); n != 3 {
		t.Fatalf("the unprivileged reaper unregisters nothing itself, %d auto entries", n)
	}
	for link, store := range map[string]string{profile: profileStore, input: inputStore} {
		if dest, err := os.Readlink(link); err != nil || dest != store {
			t.Fatalf("out-link %s must stay exactly as it was: %q %v", link, dest, err)
		}
	}
	if body, err := os.ReadFile(rc); err != nil || string(body) != "export X=1\n" {
		t.Fatalf(".rc must stay: %q %v", body, err)
	}
}

// TIN-5268: agent-session data is never deleted. A stale out-link inside a
// harness session tree is only listed; the link and every file beside it
// survive, and so does its auto entry until the root job acts.
func TestNixStaleGCRootInAgentSessionTreeKeepsEveryFile(t *testing.T) {
	f := newGCRootFixture(t)
	tmp := f.mkdir("tmp-agents")
	session := "tmp-agents/claude-1000/-srv-fast-local-jess-git-repo/132408fc-4933-4f0f-92c1-c1bfeaba72e3/scratchpad"
	result, store := f.root(session + "/res-default")
	notes := filepath.Join(f.base, session, "notes.md")
	if err := os.WriteFile(notes, []byte("session notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultConfig()
	cfg.Nix.ReapStaleGCRoots = true
	cfg.Nix.TempGCRootPaths = []string{tmp}
	cfg.Nix.GCRootCandidatesFile = f.candidatesFile()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if n := f.plugin(time.Now().Add(30*24*time.Hour)).publishStaleGCRootCandidates(cfg, logger); n != 1 {
		t.Fatalf("expected one listed root, got %d", n)
	}
	if lines := f.candidates(); len(lines) != 1 || !strings.HasSuffix(lines[0], "\t"+result) {
		t.Fatalf("the session out-link is listed: %q", lines)
	}
	if dest, err := os.Readlink(result); err != nil || dest != store {
		t.Fatalf("the session's out-link must survive untouched: %q %v", dest, err)
	}
	if body, err := os.ReadFile(notes); err != nil || string(body) != "session notes\n" {
		t.Fatalf("session files must survive: %q %v", body, err)
	}
	if n := f.autoEntries(); n != 1 {
		t.Fatalf("the auto entry stays until the root job acts, %d entries", n)
	}
}

func TestNixStaleGCRootCandidatesAreReplacedEachCycle(t *testing.T) {
	f := newGCRootFixture(t)
	tmp := f.mkdir("tmp")
	f.root("tmp/session/result")
	cfg := config.DefaultConfig()
	cfg.Nix.ReapStaleGCRoots = true
	cfg.Nix.TempGCRootPaths = []string{tmp}
	cfg.Nix.GCRootCandidatesFile = f.candidatesFile()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.plugin(time.Now().Add(73*time.Hour)).publishStaleGCRootCandidates(cfg, logger)
	if len(f.candidates()) != 1 {
		t.Fatalf("one candidate expected: %q", f.candidates())
	}
	f.plugin(time.Now()).publishStaleGCRootCandidates(cfg, logger)
	if lines := f.candidates(); len(lines) != 0 {
		t.Fatalf("a root that is no longer stale leaves the list: %q", lines)
	}
}

func TestNixStaleGCRootWithoutACandidatesFileOnlyReports(t *testing.T) {
	f := newGCRootFixture(t)
	tmp := f.mkdir("tmp")
	result, _ := f.root("tmp/session/result")

	cfg := config.DefaultConfig()
	cfg.Nix.ReapStaleGCRoots = true
	cfg.Nix.TempGCRootPaths = []string{tmp}
	if got := f.plugin(time.Now().Add(73*time.Hour)).publishStaleGCRootCandidates(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); got != 0 {
		t.Fatalf("no file means nothing is listed, got %d", got)
	}
	if _, err := os.Stat(f.candidatesFile()); !os.IsNotExist(err) {
		t.Fatalf("no candidates file may be written: %v", err)
	}
	if n := f.autoEntries(); n != 1 {
		t.Fatalf("the auto entry must stay, %d entries", n)
	}
	if _, err := os.Lstat(result); err != nil {
		t.Fatalf("the out-link must stay: %v", err)
	}
}

func TestNixCleanupListsStaleRootsBeforeGC(t *testing.T) {
	f := newGCRootFixture(t)
	tmp := f.mkdir("tmp")
	result, _ := f.root("tmp/session/result")

	binDir := t.TempDir()
	callsPath := filepath.Join(t.TempDir(), "nix-gc-calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$NIX_GC_CALLS"
if [ "$1" = "--dry-run" ]; then
  printf '%s\n' "0 store paths would be deleted, 0.0 KiB would be freed"
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "nix-collect-garbage"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("NIX_GC_CALLS", callsPath)

	cfg := config.DefaultConfig()
	cfg.Nix.SkipWhenDaemonBusy = false
	cfg.Nix.HostMeasurePath = tmp
	cfg.Nix.ReapStaleGCRoots = true
	cfg.Nix.TempGCRootPaths = []string{tmp}
	cfg.Nix.GCRootCandidatesFile = f.candidatesFile()

	res := f.plugin(time.Now().Add(73*time.Hour)).Cleanup(context.Background(), LevelWarning, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if res.Error != nil {
		t.Fatalf("cleanup: %v", res.Error)
	}
	if lines := f.candidates(); len(lines) != 1 || !strings.HasSuffix(lines[0], "\t"+result) {
		t.Fatalf("the stale root is listed before the preflight: %q", lines)
	}
	if _, err := os.Lstat(result); err != nil {
		t.Fatalf("the out-link itself must survive: %v", err)
	}
	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	// Listing is not cleanup work: with nothing to collect only the dry-run runs.
	if got, want := string(calls), "--dry-run\n"; got != want {
		t.Fatalf("nix-collect-garbage calls %q, want %q", got, want)
	}
}

func TestNixCleanupLeavesRootsAloneWhenReapingIsOff(t *testing.T) {
	f := newGCRootFixture(t)
	tmp := f.mkdir("tmp")
	result, _ := f.root("tmp/session/result")

	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "nix-collect-garbage"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := config.DefaultConfig()
	cfg.Nix.SkipWhenDaemonBusy = false
	cfg.Nix.TempGCRootPaths = []string{tmp}

	cfg.Nix.GCRootCandidatesFile = f.candidatesFile()
	f.plugin(time.Now().Add(365*24*time.Hour)).Cleanup(context.Background(), LevelWarning, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := os.Stat(f.candidatesFile()); !os.IsNotExist(err) {
		t.Fatalf("reap_stale_gc_roots off writes no candidates file: %v", err)
	}
	if _, err := os.Lstat(result); err != nil {
		t.Fatalf("reap_stale_gc_roots defaults off; the root must stay: %v", err)
	}
}

func TestNixPlanListsStaleRootsWithoutRemovingThem(t *testing.T) {
	f := newGCRootFixture(t)
	tmp := f.mkdir("tmp")
	result, _ := f.root("tmp/session/result")

	cfg := config.DefaultConfig()
	cfg.Nix.ReapStaleGCRoots = true
	cfg.Nix.TempGCRootPaths = []string{tmp}
	p := NewNixPlugin()
	p.autoGCRootsDir = f.autoDir
	p.storeDir = f.storeDir
	p.now = func() time.Time { return time.Now().Add(73 * time.Hour) }

	targets, metadata, warnings := p.planStaleGCRoots(cfg)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if metadata["stale_gc_roots"] != "1" || len(targets) != 1 {
		t.Fatalf("plan should list one stale root: %+v %+v", metadata, targets)
	}
	if targets[0].Action != nixStaleGCRootActionRemove || targets[0].Protected || targets[0].Path != result {
		t.Fatalf("unexpected plan target: %+v", targets[0])
	}
	if _, err := os.Lstat(result); err != nil {
		t.Fatalf("planning must not remove anything: %v", err)
	}
}
