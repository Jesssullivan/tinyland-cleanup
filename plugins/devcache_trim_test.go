//go:build linux || darwin

package plugins

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// devCacheFixture builds a cache root whose units carry set mtimes. The trim
// runs with a clock 100 hours ahead, so the real ctime (now) is older than
// every mtime the fixture sets and the mtimes order the units.
type devCacheFixture struct {
	t    *testing.T
	root string
	base time.Time
	now  time.Time
}

func newDevCacheFixture(t *testing.T) *devCacheFixture {
	t.Helper()
	base := time.Now()
	return &devCacheFixture{t: t, root: filepath.Join(t.TempDir(), "uv"), base: base, now: base.Add(100 * time.Hour)}
}

// unit makes dir/file of size bytes and stamps every path in it at base+at.
func (f *devCacheFixture) unit(rel string, size int, at time.Duration) string {
	f.t.Helper()
	dir := filepath.Join(f.root, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	file := filepath.Join(dir, "payload")
	if err := os.WriteFile(file, []byte(strings.Repeat("u", size)), 0o644); err != nil {
		f.t.Fatal(err)
	}
	stamp := f.base.Add(at)
	for _, p := range []string{file, dir} {
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			f.t.Fatal(err)
		}
	}
	return dir
}

func (f *devCacheFixture) pol(maxBytes int64) devCacheTrimPolicy {
	return devCacheTrimPolicy{maxBytes: maxBytes, level: LevelModerate, enforce: true, now: f.now}
}

func devCacheChosen(ts []CleanupTarget) []string {
	var out []string
	for _, t := range ts {
		if strings.HasSuffix(t.Type, devCacheEntrySuffix) {
			out = append(out, filepath.Base(t.Path))
		}
	}
	return out
}

func devCacheActivity(held map[string]string, incomplete string) *bbActivity {
	a := newBBActivity()
	for p, why := range held {
		a.index(p, why, nil)
	}
	if incomplete != "" {
		a.incomplete = append(a.incomplete, incomplete)
	}
	return a
}

func TestDevCacheTrimOldestUnheldFirstDownToBudget(t *testing.T) {
	f := newDevCacheFixture(t)
	f.unit("old", 64<<10, 10*time.Hour)
	f.unit("mid", 64<<10, 20*time.Hour)
	f.unit("new", 64<<10, 30*time.Hour)
	// The three units hold about 192 KiB; a 100 KiB budget needs two to go.
	ts := devCacheTrimTargets(context.Background(), f.root, "uv-cache", "uv cache", f.pol(100<<10), devCacheActivity(nil, ""))
	got := devCacheChosen(ts)
	if strings.Join(got, ",") != "old,mid" {
		t.Fatalf("chosen = %v, want old,mid", got)
	}
	if ts[0].Type != "uv-cache" || !ts[0].Protected || ts[0].Action != "protect" {
		t.Fatalf("summary target = %+v, want a protected root summary", ts[0])
	}
	for _, tt := range ts[1:] {
		if tt.Action != "delete" || tt.Type != "uv-cache-entry" {
			t.Fatalf("entry target = %+v, want delete uv-cache-entry", tt)
		}
	}
}

func TestDevCacheTrimHeldTopLevelDirOffersItsUnheldChildren(t *testing.T) {
	f := newDevCacheFixture(t)
	live := f.unit("archive-v0/live", 64<<10, 5*time.Hour)
	f.unit("archive-v0/idle", 64<<10, 6*time.Hour)
	f.unit("wheels-v5", 64<<10, 7*time.Hour)
	held := map[string]string{filepath.Join(live, "payload"): "pid 42 maps payload"}
	ts := devCacheTrimTargets(context.Background(), f.root, "uv-cache", "uv cache", f.pol(1), devCacheActivity(held, ""))
	got := devCacheChosen(ts)
	if strings.Join(got, ",") != "idle,wheels-v5" {
		t.Fatalf("chosen = %v, want idle,wheels-v5 (live and the archive-v0 root kept)", got)
	}
	if !ts[0].Active || !strings.Contains(ts[0].Reason, "archive-v0") {
		t.Fatalf("summary should name the held bucket: %+v", ts[0])
	}
}

func TestDevCacheTrimHeldLockFileKeepsTheRoot(t *testing.T) {
	f := newDevCacheFixture(t)
	f.unit("archive-v0/a", 64<<10, 5*time.Hour)
	lock := filepath.Join(f.root, ".lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	held := map[string]string{lock: "pid 7 holds .lock"}
	ts := devCacheTrimTargets(context.Background(), f.root, "uv-cache", "uv cache", f.pol(1), devCacheActivity(held, ""))
	if got := devCacheChosen(ts); len(got) != 0 {
		t.Fatalf("chosen = %v, want nothing while the lock file is open", got)
	}
	if !strings.Contains(ts[0].Reason, ".lock is open") {
		t.Fatalf("reason = %q", ts[0].Reason)
	}
}

func TestDevCacheTrimIncompleteInventoryKeepsEverything(t *testing.T) {
	f := newDevCacheFixture(t)
	f.unit("a", 64<<10, 5*time.Hour)
	ts := devCacheTrimTargets(context.Background(), f.root, "uv-cache", "uv cache", f.pol(1), devCacheActivity(nil, "lsof listing timed out"))
	if len(ts) != 1 || !strings.Contains(ts[0].Reason, "activity inventory incomplete") {
		t.Fatalf("targets = %+v, want only the kept summary", ts)
	}
}

func TestDevCacheTrimNeverTakesAFreshUnitOrActsBelowModerate(t *testing.T) {
	f := newDevCacheFixture(t)
	f.unit("old", 64<<10, 10*time.Hour)
	f.unit("fresh", 64<<10, 99*time.Hour+30*time.Minute)
	ts := devCacheTrimTargets(context.Background(), f.root, "uv-cache", "uv cache", f.pol(1), devCacheActivity(nil, ""))
	if got := devCacheChosen(ts); strings.Join(got, ",") != "old" {
		t.Fatalf("chosen = %v, want old only (fresh is inside devCacheTrimMinAge)", got)
	}
	pol := f.pol(1)
	pol.level = LevelWarning
	ts = devCacheTrimTargets(context.Background(), f.root, "uv-cache", "uv cache", pol, devCacheActivity(nil, ""))
	if got := devCacheChosen(ts); len(got) != 0 {
		t.Fatalf("chosen at warning = %v, want none", got)
	}
}

func TestDevCacheTrimStaleEntriesGoUnderBudgetAndReviewWithoutEnforce(t *testing.T) {
	f := newDevCacheFixture(t)
	f.unit("ancient", 4<<10, 1*time.Hour)
	f.unit("recent", 4<<10, 90*time.Hour)
	pol := f.pol(0)
	pol.staleAfter = 2 * 24 * time.Hour
	pol.enforce = false
	ts := devCacheTrimTargets(context.Background(), f.root, "uv-cache", "uv cache", pol, devCacheActivity(nil, ""))
	got := devCacheChosen(ts)
	if strings.Join(got, ",") != "ancient" {
		t.Fatalf("chosen = %v, want ancient", got)
	}
	if ts[1].Action != "review" || ts[1].Protected {
		t.Fatalf("without enforce the entry is a review candidate: %+v", ts[1])
	}
}

func TestDevCacheTrimApplyRechecksEveryEntry(t *testing.T) {
	f := newDevCacheFixture(t)
	a := f.unit("a", 4<<10, 1*time.Hour)
	b := f.unit("b", 4<<10, 2*time.Hour)
	ts := devCacheTrimTargets(context.Background(), f.root, "uv-cache", "uv cache", f.pol(1), devCacheActivity(nil, ""))
	if len(devCacheChosen(ts)) != 2 {
		t.Fatalf("setup: chosen = %v", devCacheChosen(ts))
	}
	var removed []string
	scan := func() *bbActivity {
		// b gained a holder between plan and removal.
		return devCacheActivity(map[string]string{filepath.Join(b, "payload"): "pid 9 opened payload"}, "")
	}
	remove := func(p string) error { removed = append(removed, p); return nil }
	freed, items, err := devCacheTrimApply(context.Background(), ts, scan, remove, time.Now, retDiscard())
	if err != nil || items != 1 || freed <= 0 {
		t.Fatalf("apply = %d bytes, %d items, %v", freed, items, err)
	}
	if len(removed) != 1 || removed[0] != a {
		t.Fatalf("removed = %v, want only %s", removed, a)
	}
	removed = nil
	incomplete := func() *bbActivity { return devCacheActivity(nil, "lsof listing failed") }
	if _, items, _ := devCacheTrimApply(context.Background(), ts, incomplete, remove, time.Now, retDiscard()); items != 0 || len(removed) != 0 {
		t.Fatalf("an incomplete inventory at removal time must remove nothing, removed %v", removed)
	}
}

func TestConfiguredDevCacheTargetsInertWithoutPaths(t *testing.T) {
	cfg := config.DefaultConfig().DarwinDevCaches
	cfg.Enforce = true
	called := false
	scan := func([]string) *bbActivity { called = true; return newBBActivity() }
	if ts := configuredDevCacheTargets(context.Background(), cfg, t.TempDir(), "tester", LevelCritical, time.Now(), scan); ts != nil || called {
		t.Fatalf("no paths key: targets %v, inventory taken %t; want none and no inventory", ts, called)
	}
	f := newDevCacheFixture(t)
	f.unit("a", 4<<10, 1*time.Hour)
	cfg.UV.Paths = []string{f.root, filepath.Join(t.TempDir(), "missing")}
	cfg.UV.MaxGB = 0
	cfg.UV.StaleAfterDays = 1
	ts := configuredDevCacheTargets(context.Background(), cfg, t.TempDir(), "tester", LevelModerate, f.now, scan)
	if !called || len(ts) != 2 || ts[1].Type != "uv-cache-entry" {
		t.Fatalf("configured root: targets %+v, inventory taken %t", ts, called)
	}
}

func TestDarwinDevCachesPathsValidation(t *testing.T) {
	cases := []struct {
		name  string
		set   func(c *config.DarwinDevCachesConfig)
		valid bool
	}{
		{"absolute uv path", func(c *config.DarwinDevCachesConfig) {
			c.UV.Paths = []string{"/nix/var/tinyland/user/nix-client-cache/uv"}
		}, true},
		{"home relative pip path", func(c *config.DarwinDevCachesConfig) { c.Pip.Paths = []string{"~/x/pip"} }, true},
		{"relative path", func(c *config.DarwinDevCachesConfig) { c.UV.Paths = []string{"cache/uv"} }, false},
		{"root", func(c *config.DarwinDevCachesConfig) { c.UV.Paths = []string{"/"} }, false},
		{"home itself", func(c *config.DarwinDevCachesConfig) { c.UV.Paths = []string{"~"} }, false},
		{"empty", func(c *config.DarwinDevCachesConfig) { c.NPM.Paths = []string{""} }, false},
		{"versioned family", func(c *config.DarwinDevCachesConfig) { c.JetBrains.Paths = []string{"/x"} }, false},
		{"negative budget", func(c *config.DarwinDevCachesConfig) { c.UV.Paths = []string{"/x"}; c.UV.MaxGB = -1 }, false},
	}
	for _, tc := range cases {
		c := config.DefaultConfig().DarwinDevCaches
		tc.set(&c)
		if err := c.Validate(); (err == nil) != tc.valid {
			t.Errorf("%s: Validate() = %v, want valid=%t", tc.name, err, tc.valid)
		}
	}
}
