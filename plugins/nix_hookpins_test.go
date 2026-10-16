package plugins

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

const (
	hookA = "/nix/store/0123456789abcdfghijklmnpqrsvwxyz-hook-a"
	hookB = "/nix/store/zyxwvsrqpnmlkjihgfdcba9876543210-hook-b"
)

type hookFixture struct {
	t        *testing.T
	settings string
	cfg      config.NixConfig
	now      time.Time
	starts   []time.Time
	startErr error
}

func newHookFixture(t *testing.T) *hookFixture {
	home := t.TempDir()
	f := &hookFixture{t: t, settings: filepath.Join(home, ".claude", "settings.json"), now: time.Now()}
	if err := os.MkdirAll(filepath.Dir(f.settings), 0o755); err != nil {
		t.Fatal(err)
	}
	f.cfg = config.NixConfig{
		HookPinFiles:        []string{f.settings},
		HookPinDir:          filepath.Join(home, ".local", "state", "tinyland-cleanup", "gcroots", "hook-pins"),
		HookPinSessionComms: []string{"claude", "codex"},
	}
	return f
}

func (f *hookFixture) write(refs ...string) {
	f.t.Helper()
	body := `{"hooks":{`
	for i, ref := range refs {
		if i > 0 {
			body += ","
		}
		body += `"h` + string(rune('a'+i)) + `":{"command":"` + ref + `/bin/hook --flag"}`
	}
	body += "}}"
	if err := os.WriteFile(f.settings, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *hookFixture) refresh() error {
	p := NewNixPlugin()
	p.now = func() time.Time { return f.now }
	p.hookValid = func(context.Context, string) bool { return true }
	p.hookAddRoot = func(_ context.Context, link, storePath string) error { return os.Symlink(storePath, link) }
	p.hookSessionStarts = func([]string) ([]time.Time, error) { return f.starts, f.startErr }
	return p.refreshHookPins(context.Background(), f.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func (f *hookFixture) pinned(ref string) bool {
	dest, err := os.Readlink(filepath.Join(f.cfg.HookPinDir, filepath.Base(ref)))
	return err == nil && dest == ref
}

// H1: every store path a hook config references is rooted before GC.
func TestNixHookPinsRootEveryReferencedPath(t *testing.T) {
	f := newHookFixture(t)
	f.write(hookA, hookB)
	if err := f.refresh(); err != nil {
		t.Fatal(err)
	}
	if !f.pinned(hookA) || !f.pinned(hookB) {
		t.Fatal("both referenced hook paths must be pinned")
	}
}

// H1: a path the config stops referencing stays pinned while a session that
// started before then is alive, and is released once none is.
func TestNixHookPinsOutliveTheSessionsThatSnapshottedThem(t *testing.T) {
	f := newHookFixture(t)
	f.write(hookA)
	if err := f.refresh(); err != nil {
		t.Fatal(err)
	}
	oldSession := f.now.Add(-time.Hour)
	f.starts = []time.Time{oldSession}

	f.now = f.now.Add(time.Minute)
	f.write(hookB) // a Home Manager switch replaced the hook
	if err := f.refresh(); err != nil {
		t.Fatal(err)
	}
	if !f.pinned(hookA) || !f.pinned(hookB) {
		t.Fatal("the old hook path stays pinned while the older session lives")
	}

	f.now = f.now.Add(time.Minute)
	f.starts = []time.Time{f.now} // the old session ended; a new one started after the switch
	if err := f.refresh(); err != nil {
		t.Fatal(err)
	}
	if f.pinned(hookA) {
		t.Fatal("the old hook path is released once no session that could hold it is alive")
	}
	if !f.pinned(hookB) {
		t.Fatal("the current hook path stays pinned")
	}
}

func TestNixHookPinsAreKeptWhenSessionsCannotBeInspected(t *testing.T) {
	f := newHookFixture(t)
	f.write(hookA)
	if err := f.refresh(); err != nil {
		t.Fatal(err)
	}
	f.write(hookB)
	f.startErr = errors.New("no /proc")
	f.now = f.now.Add(24 * time.Hour)
	if err := f.refresh(); err != nil {
		t.Fatal(err)
	}
	if !f.pinned(hookA) {
		t.Fatal("inspection failure keeps every pin")
	}
}

func TestNixHookPinDirMustBeAGCRootsDirectory(t *testing.T) {
	f := newHookFixture(t)
	f.write(hookA)
	f.cfg.HookPinDir = filepath.Join(t.TempDir(), "pins")
	if err := f.refresh(); err == nil {
		t.Fatal("a pin directory outside a gcroots/ component must be refused, or the reaper could list it")
	}
}
