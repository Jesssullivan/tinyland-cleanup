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

// pressureFixture is a retention fixture whose volume starts at 95 GiB used
// of 100 GiB and loses 5 GiB per removed entry.
type pressureFixture struct {
	*retFixture
	used    uint64
	removed []string
}

const pfGiB = uint64(1 << 30)

func newPressureFixture(t *testing.T) *pressureFixture {
	t.Helper()
	f := &pressureFixture{retFixture: newRetFixture(t), used: 95 * pfGiB}
	f.plugin.statfs = func(string) (bbUsage, error) {
		return bbUsage{BytesTotal: 100 * pfGiB, BytesUsed: f.used}, nil
	}
	f.plugin.removeAll = func(p string) error {
		f.removed = append(f.removed, filepath.Base(p))
		f.used -= 5 * pfGiB
		return os.RemoveAll(p)
	}
	f.plugin.gcrootsDir = filepath.Join(t.TempDir(), "gcroots-auto")
	f.plugin.socketTable = filepath.Join(t.TempDir(), "unix")
	if err := os.WriteFile(f.plugin.socketTable, []byte("Num RefCount Protocol Flags Type St Inode Path\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pc := &f.cfg.Retention.TmpAgents.Pressure
	pc.Enabled = true
	pc.UsedPercent = 90
	pc.StopPercent = 86
	pc.MaxAge = "1h"
	pc.Interval = "5m"
	pc.AllNames = true
	pc.Delete = true
	f.cfg.Retention.FreeSpaceFloor.FloorGiB = 1
	return f
}

// entry makes root/name with a payload and stamps the top-level mtime
// hoursAgo hours before the real clock, which orders the entries.
func (f *pressureFixture) entry(name string, hoursAgo int) string {
	f.t.Helper()
	dir := filepath.Join(f.root, name)
	f.file(filepath.Join(dir, "payload"), 4096)
	stamp := time.Now().Add(-time.Duration(hoursAgo) * time.Hour)
	if err := os.Chtimes(dir, stamp, stamp); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

func (f *pressureFixture) run() retentionPressureResult {
	f.t.Helper()
	f.plugin.pressureLast = nil
	return f.plugin.pressureCycle(context.Background(), f.cfg, true, retDiscard())
}

func (f *pressureFixture) want(names ...string) {
	f.t.Helper()
	if strings.Join(f.removed, ",") != strings.Join(names, ",") {
		f.t.Fatalf("removed %v, want %v", f.removed, names)
	}
}

func TestRetentionPressureOffByDefault(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.Retention.TmpAgents.Pressure.Enabled || cfg.Retention.TmpAgents.Pressure.Delete {
		t.Fatal("the pressure pass must be off and report-only by default")
	}
	f := newPressureFixture(t)
	f.cfg.Retention.TmpAgents.Pressure = cfg.Retention.TmpAgents.Pressure
	f.entry("job-a", 4)
	if res := f.run(); len(res.targets) != 0 || len(f.removed) != 0 {
		t.Fatalf("pressure off: targets %v, removed %v", res.targets, f.removed)
	}
}

func TestRetentionPressureRemovesOldestFirstUntilStopPercent(t *testing.T) {
	f := newPressureFixture(t)
	f.entry("job-a", 4)
	f.entry("job-b", 3)
	f.entry("job-c", 2)
	// 95 -> 90 -> 85: under stop_percent 86 after two removals.
	f.run()
	f.want("job-a", "job-b")
}

func TestRetentionPressureIdleUnderUsedPercent(t *testing.T) {
	f := newPressureFixture(t)
	f.used = 80 * pfGiB
	f.entry("job-a", 4)
	res := f.run()
	f.want()
	if !strings.Contains(res.metadata["tmp_agents.pressure."+f.root], "used_percent=80.0") {
		t.Fatalf("metadata = %v", res.metadata)
	}
}

func TestRetentionPressureKeepsHolderGCRootKeepMarkerAndRecentTrees(t *testing.T) {
	f := newPressureFixture(t)
	f.used = 99 * pfGiB
	f.cfg.Retention.TmpAgents.Pressure.StopPercent = 50
	held := f.entry("held", 9)
	f.held[filepath.Join(held, "payload")] = "pid 4 holds payload"
	rooted := f.entry("rooted", 8)
	if err := os.MkdirAll(f.plugin.gcrootsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(rooted, "result"), filepath.Join(f.plugin.gcrootsDir, "abc")); err != nil {
		t.Fatal(err)
	}
	marked := f.entry("marked", 7)
	f.file(filepath.Join(marked, "deep", config.RetentionKeepMarker), 0)
	recent := f.entry("recent", 6)
	soon := f.plugin.now().Add(-10 * time.Minute)
	if err := os.Chtimes(filepath.Join(recent, "payload"), soon, soon); err != nil {
		t.Fatal(err)
	}
	f.entry("old", 5)
	res := f.run()
	f.want("old")
	reasons := map[string]string{}
	for _, tg := range res.targets {
		reasons[tg.Name] = tg.Reason
	}
	for name, want := range map[string]string{
		"held":   "live reference",
		"rooted": "Nix GC root",
		"marked": "keep marker",
		"recent": "walk stopped",
	} {
		if !strings.Contains(reasons[name], want) {
			t.Errorf("%s reason = %q, want %q", name, reasons[name], want)
		}
	}
}

func TestRetentionPressureIncompleteInventoryKeepsEverything(t *testing.T) {
	f := newPressureFixture(t)
	f.incomplete = "lsof listing timed out"
	f.entry("job-a", 4)
	res := f.run()
	f.want()
	if !strings.Contains(res.metadata["tmp_agents.pressure."+f.root+".kept_all"], "lsof listing timed out") {
		t.Fatalf("metadata = %v", res.metadata)
	}
}

func TestRetentionPressureNameGlobsExcludesAndFiles(t *testing.T) {
	f := newPressureFixture(t)
	f.used = 99 * pfGiB
	f.cfg.Retention.TmpAgents.Pressure.StopPercent = 50
	pc := &f.cfg.Retention.TmpAgents.Pressure
	pc.AllNames = false
	pc.ExcludeGlobs = []string{"nix-shell.keep*"}
	pc.IncludeFiles = true
	f.cfg.Retention.TmpAgents.NameGlobs = []string{"nix-shell.*", "*.log"}
	f.entry("nix-shell.keepme", 6)
	f.entry("other", 5)
	f.entry("nix-shell.go", 4)
	f.file(filepath.Join(f.root, "old.log"), 10)
	stamp := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(filepath.Join(f.root, "old.log"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/", filepath.Join(f.root, "link.log")); err != nil {
		t.Fatal(err)
	}
	f.run()
	f.want("nix-shell.go", "old.log")
}

func TestRetentionPressureInteractiveDevelopShellKeepsLaterNixShellTrees(t *testing.T) {
	cases := []struct {
		name  string
		start time.Time
		keep  bool
	}{
		{"shell started before the tree", time.Now().Add(-time.Hour), true},
		{"start time unknown", time.Time{}, true},
		{"shell started after the tree", time.Now().Add(time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPressureFixture(t)
			f.cfg.Retention.TmpAgents.Pressure.StopPercent = 50
			f.entry("nix-shell.tree", 4)
			f.procs = []bbProc{{pid: 77, cmdline: "bash --rcfile " + filepath.Join(f.root, "nix-shell.rcAbC"), start: tc.start}}
			f.run()
			if tc.keep {
				f.want()
			} else {
				f.want("nix-shell.tree")
			}
		})
	}
}

func TestRetentionPressureIntervalHoldsAPassWithoutABreach(t *testing.T) {
	f := newPressureFixture(t)
	f.entry("job-a", 4)
	f.entry("job-b", 3)
	f.cfg.Retention.TmpAgents.Pressure.StopPercent = 50
	ctx := context.Background()
	f.plugin.removeAll = func(p string) error {
		f.removed = append(f.removed, filepath.Base(p))
		return os.RemoveAll(p)
	}
	f.cfg.Retention.TmpAgents.MaxDirsPerCycle = 1
	f.plugin.pressureCycle(ctx, f.cfg, true, retDiscard())
	f.plugin.pressureCycle(ctx, f.cfg, true, retDiscard())
	f.want("job-a")
}

func TestRetentionPressurePlanProjectsWithoutRemoving(t *testing.T) {
	f := newPressureFixture(t)
	f.cfg.Retention.TmpAgents.Pressure.Delete = false
	f.entry("job-a", 4)
	f.entry("job-b", 3)
	f.entry("job-c", 2)
	plan := f.plan()
	f.want()
	var report []string
	for _, tg := range plan.Targets {
		if tg.Type == "tmp-agents-pressure" && tg.Action == "report" {
			report = append(report, tg.Name)
		}
	}
	// 4 KiB entries barely move a 95 GiB projection, so all three report.
	if len(report) != 3 {
		t.Fatalf("report targets = %v", report)
	}
}

func TestRetentionPressureValidation(t *testing.T) {
	base := func() config.RetentionConfig {
		c := config.DefaultRetentionConfig("/home/x")
		c.TmpAgents.Roots = []string{"/srv/tmp"}
		c.TmpAgents.Pressure.Enabled = true
		return c
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("defaults with enabled should validate: %v", err)
	}
	for name, mut := range map[string]func(c *config.RetentionConfig){
		"stop above used":  func(c *config.RetentionConfig) { c.TmpAgents.Pressure.StopPercent = 95 },
		"used over 100":    func(c *config.RetentionConfig) { c.TmpAgents.Pressure.UsedPercent = 101 },
		"interval too low": func(c *config.RetentionConfig) { c.TmpAgents.Pressure.Interval = "10s" },
		"max_age too low":  func(c *config.RetentionConfig) { c.TmpAgents.Pressure.MaxAge = "30m" },
		"glob with slash":  func(c *config.RetentionConfig) { c.TmpAgents.Pressure.ExcludeGlobs = []string{"a/b"} },
		"all_names on home": func(c *config.RetentionConfig) {
			c.TmpAgents.Pressure.AllNames = true
			c.TmpAgents.Roots = []string{"~"}
		},
	} {
		c := base()
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: want a refusal", name)
		}
	}
}

func TestRetentionRcFileParsing(t *testing.T) {
	if got := retentionRcFile("/nix/store/x-bash/bin/bash --rcfile /t/nix-shell.Ab12Cd"); got != "/t/nix-shell.Ab12Cd" {
		t.Fatalf("got %q", got)
	}
	for _, c := range []string{"bash /t/nix-shell.Ab12Cd", "bash --rcfile /t/.bashrc", "bash --rcfile"} {
		if got := retentionRcFile(c); got != "" {
			t.Fatalf("%q: got %q, want none", c, got)
		}
	}
}
