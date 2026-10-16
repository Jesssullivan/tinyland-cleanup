package plugins

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

// unregisterCommand writes a fake privileged helper that removes exactly the
// named auto entry, as the lab sudo helper does, and records its argument.
func (f *gcRootFixture) unregisterCommand() []string {
	f.t.Helper()
	script := filepath.Join(f.base, "fake-unregister")
	body := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$1\" >> " + filepath.Join(f.base, "unregister.log") + "\nrm -f " + f.autoDir + "/\"$1\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		f.t.Fatal(err)
	}
	return []string{script}
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

func TestNixStaleGCRootsDirenvOutsideAGitCheckoutIsStaleAtOnce(t *testing.T) {
	f := newGCRootFixture(t)
	input, _ := f.root("scratch/copied/.direnv/flake-inputs/src-1")

	roots, err := f.reaper(time.Now()).collect(nil, 7*24*time.Hour, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !staleByLink(roots)[input] {
		t.Fatalf("a .direnv outside a git checkout is stale whatever its age: %+v", roots)
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

func TestNixStaleGCRootUnregisterTouchesNoUserFile(t *testing.T) {
	f := newGCRootFixture(t)
	direnv := f.mkdir("scratch/copied/.direnv")
	rc := filepath.Join(direnv, "flake-profile-abc.rc")
	if err := os.WriteFile(rc, []byte("export X=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := f.mkdir("scratch/copied/.direnv/bin")
	profile, profileStore := f.root("scratch/copied/.direnv/flake-profile-abc")
	input, inputStore := f.root("scratch/copied/.direnv/flake-inputs/src-1")

	reaper := f.reaper(time.Now())
	roots, err := reaper.collect(nil, 7*24*time.Hour, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	command := f.unregisterCommand()
	for _, r := range roots {
		if err := reaper.unregister(context.Background(), command, r); err != nil {
			t.Fatalf("unregister %s: %v", r.Link, err)
		}
	}
	if n := f.autoEntries(); n != 0 {
		t.Fatalf("every stale auto entry should be unregistered, %d left", n)
	}
	for link, store := range map[string]string{profile: profileStore, input: inputStore} {
		dest, err := os.Readlink(link)
		if err != nil || dest != store {
			t.Fatalf("out-link %s must stay exactly as it was: %q %v", link, dest, err)
		}
	}
	for _, kept := range []string{rc, bin, direnv, filepath.Dir(input)} {
		if _, err := os.Lstat(kept); err != nil {
			t.Fatalf("%s must stay: %v", kept, err)
		}
	}
}

// TIN-5268: agent-session data is never deleted. A stale out-link inside a
// harness session tree is only unregistered; the link and every file beside
// it survive.
func TestNixStaleGCRootInAgentSessionTreeKeepsEveryFile(t *testing.T) {
	f := newGCRootFixture(t)
	tmp := f.mkdir("tmp-agents")
	session := "tmp-agents/claude-1000/-srv-fast-local-jess-git-repo/132408fc-4933-4f0f-92c1-c1bfeaba72e3/scratchpad"
	result, store := f.root(session + "/res-default")
	notes := filepath.Join(f.base, session, "notes.md")
	if err := os.WriteFile(notes, []byte("session notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reaper := f.reaper(time.Now().Add(30 * 24 * time.Hour))
	roots, err := reaper.collect([]string{tmp}, 7*24*time.Hour, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || !roots[0].Stale {
		t.Fatalf("expected one stale temporary root: %+v", roots)
	}
	if err := reaper.unregister(context.Background(), f.unregisterCommand(), roots[0]); err != nil {
		t.Fatal(err)
	}
	if n := f.autoEntries(); n != 0 {
		t.Fatalf("the auto entry should be gone, %d left", n)
	}
	if dest, err := os.Readlink(result); err != nil || dest != store {
		t.Fatalf("the session's out-link must survive untouched: %q %v", dest, err)
	}
	if body, err := os.ReadFile(notes); err != nil || string(body) != "session notes\n" {
		t.Fatalf("session files must survive: %q %v", body, err)
	}
}

func TestNixStaleGCRootUnregisterRefusesARetargetedLink(t *testing.T) {
	f := newGCRootFixture(t)
	link, _ := f.root("scratch/copied/.direnv/flake-profile-abc")
	reaper := f.reaper(time.Now())
	roots, err := reaper.collect(nil, 7*24*time.Hour, 72*time.Hour)
	if err != nil || len(roots) != 1 {
		t.Fatalf("collect: %v %+v", err, roots)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.storeDir, "bbbbbbbb-new"), link); err != nil {
		t.Fatal(err)
	}
	if err := reaper.unregister(context.Background(), f.unregisterCommand(), roots[0]); err == nil {
		t.Fatal("a link that changed since classification must stay registered")
	}
	if n := f.autoEntries(); n != 1 {
		t.Fatalf("the auto entry must stay, %d entries", n)
	}
}

func TestNixStaleGCRootWithoutACommandOnlyReports(t *testing.T) {
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

	if got := p.reapStaleGCRoots(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); got != 0 {
		t.Fatalf("no command means nothing is unregistered, got %d", got)
	}
	if n := f.autoEntries(); n != 1 {
		t.Fatalf("the auto entry must stay, %d entries", n)
	}
	if _, err := os.Lstat(result); err != nil {
		t.Fatalf("the out-link must stay: %v", err)
	}
}

func TestNixCleanupReapsStaleRootsBeforeGC(t *testing.T) {
	f := newGCRootFixture(t)
	tmp := f.mkdir("tmp")
	result, _ := f.root("tmp/session/result")

	binDir := t.TempDir()
	callsPath := filepath.Join(t.TempDir(), "nix-gc-calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$NIX_GC_CALLS"
if [ "$1" = "--dry-run" ]; then
  exit 0
fi
printf '%s\n' "1 store paths deleted, 1.0 MiB freed"
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
	cfg.Nix.GCRootUnregisterCommand = f.unregisterCommand()

	p := NewNixPlugin()
	p.autoGCRootsDir = f.autoDir
	p.storeDir = f.storeDir
	p.now = func() time.Time { return time.Now().Add(73 * time.Hour) }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	result1 := p.Cleanup(context.Background(), LevelWarning, cfg, logger)
	if result1.Error != nil {
		t.Fatalf("cleanup: %v", result1.Error)
	}
	if n := f.autoEntries(); n != 0 {
		t.Fatalf("stale root should be unregistered before GC, %d auto entries left", n)
	}
	if _, err := os.Lstat(result); err != nil {
		t.Fatalf("the out-link itself must survive: %v", err)
	}
	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	// The dry-run reports no work, but a removed root means GC still runs.
	if got, want := string(calls), "--dry-run\n\n"; got != want {
		t.Fatalf("nix-collect-garbage calls %q, want %q", got, want)
	}
	if result1.ItemsCleaned < 2 {
		t.Fatalf("unregistered root and deleted path both count: %+v", result1)
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

	p := NewNixPlugin()
	p.autoGCRootsDir = f.autoDir
	p.storeDir = f.storeDir
	p.now = func() time.Time { return time.Now().Add(365 * 24 * time.Hour) }
	p.Cleanup(context.Background(), LevelWarning, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
