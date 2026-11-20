package plugins

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

type pinFixture struct {
	root, store, receipt, state string
	p                           *NixPlugin
	cfg                         *config.Config
	now                         time.Time
	activity                    *bbActivity
	scans                       int
}

func newPinFixture(t *testing.T) *pinFixture {
	t.Helper()
	dir := t.TempDir()
	f := &pinFixture{root: filepath.Join(dir, "lane"), store: filepath.Join(dir, "store"), receipt: filepath.Join(dir, "receipts.jsonl")}
	for _, d := range []string{f.root, f.store} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.now = time.Now()
	f.p = NewNixPlugin()
	f.p.storeDir = f.store
	f.p.now = func() time.Time { return f.now }
	f.cfg = &config.Config{}
	f.cfg.Nix.PinExpiryRoots = []string{f.root}
	f.cfg.Nix.PinExpiryReceiptFile = f.receipt
	f.state = filepath.Join(dir, "pin-state.json")
	f.cfg.Nix.PinExpiryStateFile = f.state
	// Observation began long ago, so the first-window spare does not apply
	// unless a test removes the state file.
	if err := saveNixPinUseState(f.state, nixPinUseState{ObservedSince: f.now.Add(-60 * 24 * time.Hour), Links: map[string]nixPinUseEntry{}}); err != nil {
		t.Fatal(err)
	}
	// A complete, empty inventory: nothing is in use unless a test says so.
	f.activity = newBBActivity()
	f.p.pinActivity = func(context.Context, bbActivityOptions) *bbActivity {
		f.scans++
		return f.activity
	}
	return f
}

// pin makes a link under the lane root pointing into the fake store, aged days.
func (f *pinFixture) pin(t *testing.T, rel string, days int) string {
	t.Helper()
	link := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(f.store, "abc-"+filepath.Base(rel))
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	old := f.now.Add(-time.Duration(days) * 24 * time.Hour)
	lchtimes(t, link, old)
	return link
}

// lchtimes sets a symlink's own mtime; the os package has no call for it.
func lchtimes(t *testing.T, path string, at time.Time) {
	t.Helper()
	if out, err := exec.Command("touch", "-h", "-d", at.Format("2006-01-02T15:04:05"), path).CombinedOutput(); err != nil {
		t.Skipf("cannot set symlink mtime: %v %s", err, out)
	}
}

func pinExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func pinLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestPinExpiryOffWithoutRoots(t *testing.T) {
	f := newPinFixture(t)
	old := f.pin(t, "old-result", 30)
	f.cfg.Nix.PinExpiryRoots = nil
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 0 || !pinExists(old) {
		t.Fatalf("expiry ran without roots: removed=%d", n)
	}
	targets, md, _ := f.p.planPinExpiry(context.Background(), f.cfg)
	if len(targets) != 0 || md["pin_expiry"] != "false" {
		t.Fatalf("plan not off: %v %v", targets, md)
	}
}

func TestPinExpiryRemovesOnlyOldOwnedStoreLinks(t *testing.T) {
	f := newPinFixture(t)
	old := f.pin(t, "delivery/results/old", 20)
	fresh := f.pin(t, "delivery/results/fresh", 2)
	profile := f.pin(t, "home-manager-7-link", 40)
	outside := filepath.Join(f.root, "elsewhere")
	if err := os.Symlink("/etc/hostname", outside); err != nil {
		t.Fatal(err)
	}
	ts := f.now.Add(-60 * 24 * time.Hour)
	lchtimes(t, outside, ts)
	regular := filepath.Join(f.root, "file")
	if err := os.WriteFile(regular, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(regular, ts, ts)

	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	if pinExists(old) {
		t.Fatal("old link still present")
	}
	for _, keep := range []string{fresh, profile, outside, regular} {
		if !pinExists(keep) {
			t.Fatalf("%s was removed", keep)
		}
	}
	// The store path itself is untouched: only the link goes.
	if !pinExists(filepath.Join(f.store, "abc-old")) {
		t.Fatal("store target removed")
	}
}

func TestPinExpiryKeepFiles(t *testing.T) {
	f := newPinFixture(t)
	beside := f.pin(t, "a/one", 30)
	if err := os.WriteFile(beside+".keep", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	inDir := f.pin(t, "kept/two", 30)
	if err := os.WriteFile(filepath.Join(f.root, "kept", ".keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	nested := f.pin(t, "kept/deeper/three", 30)
	gone := f.pin(t, "a/four", 30)
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	for _, keep := range []string{beside, inDir, nested} {
		if !pinExists(keep) {
			t.Fatalf("%s was removed despite a keep file", keep)
		}
	}
	if pinExists(gone) {
		t.Fatal("unprotected old link survived")
	}
}

func TestPinExpiryNeverTouchesHookPinsOrProfilesDirs(t *testing.T) {
	f := newPinFixture(t)
	hook := filepath.Join(f.root, "hook-pins")
	f.cfg.Nix.HookPinDir = hook
	hookLink := f.pin(t, "hook-pins/h1", 90)
	profLink := f.pin(t, "profiles/p1", 90)
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 0 {
		t.Fatalf("removed %d, want 0", n)
	}
	if !pinExists(hookLink) || !pinExists(profLink) {
		t.Fatal("hook pin or profiles entry removed")
	}
}

func TestPinExpiryReceiptAndDryRunPlan(t *testing.T) {
	f := newPinFixture(t)
	old := f.pin(t, "x/old", 15)
	targets, md, _ := f.p.planPinExpiry(context.Background(), f.cfg)
	if len(targets) != 1 || targets[0].Path != old || md["pin_expiry_links"] != "1" {
		t.Fatalf("plan = %v %v", targets, md)
	}
	if !pinExists(old) || pinExists(f.receipt) {
		t.Fatal("planning must not remove or write a receipt")
	}
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 1 {
		t.Fatalf("removed %d", n)
	}
	raw, err := os.ReadFile(f.receipt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), old) || !strings.Contains(string(raw), filepath.Join(f.store, "abc-old")) {
		t.Fatalf("receipt lacks link and target: %s", raw)
	}
}

func TestPinExpiryHonorsAgeAndPassLimit(t *testing.T) {
	f := newPinFixture(t)
	f.cfg.Nix.PinExpiryAfter = "7d"
	f.cfg.Nix.PinExpiryMaxPerPass = 2
	f.pin(t, "a", 10)
	f.pin(t, "b", 10)
	f.pin(t, "c", 10)
	f.pin(t, "d", 3)
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 2 {
		t.Fatalf("removed %d, want 2 (limit)", n)
	}
}

func TestPinExpiryGlobRoots(t *testing.T) {
	f := newPinFixture(t)
	f.cfg.Nix.PinExpiryRoots = []string{filepath.Join(filepath.Dir(f.root), "la*")}
	old := f.pin(t, "g", 20)
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 1 || pinExists(old) {
		t.Fatalf("glob root not expired: %d", n)
	}
}

func TestCriticalMaxFreedOverride(t *testing.T) {
	cfg := config.NixConfig{MaxFreedPerPass: "50G", CriticalMaxFreedPerPass: "10G"}
	if got := nixMaxFreedArgsAt(cfg, LevelWarning, pinLogger()); got[1] != "53687091200" {
		t.Fatalf("warning cap = %v", got)
	}
	if got := nixMaxFreedArgsAt(cfg, LevelCritical, pinLogger()); got[1] != "10737418240" {
		t.Fatalf("critical cap = %v", got)
	}
	cfg.CriticalMaxFreedPerPass = ""
	if got := nixMaxFreedArgsAt(cfg, LevelCritical, pinLogger()); got[1] != "53687091200" {
		t.Fatalf("empty override changed cap = %v", got)
	}
}

func TestBusyGateRecordsRunBesideActiveWork(t *testing.T) {
	p := NewNixPlugin()
	now := time.Now()
	p.now = func() time.Time { return now }
	p.activeWork = func(_ context.Context) ([]string, nixProcessInspection, error) {
		return []string{"nix build"}, nixProcessInspection{}, nil
	}
	cfg := config.NixConfig{CriticalMaxBusyDeferrals: 2}
	st := nixGCState{DeferredPasses: 2, FirstDeferredAt: now.Add(-time.Hour)}
	if !p.busyGate(context.Background(), LevelCritical, cfg, &st, pinLogger()) {
		t.Fatal("critical pass past the bound must run")
	}
	if st.LastBesideActiveWork.IsZero() || !strings.Contains(st.LastBesideActiveWorkBy, "critical_max_busy_deferrals") {
		t.Fatalf("state does not record the run: %+v", st)
	}
}
