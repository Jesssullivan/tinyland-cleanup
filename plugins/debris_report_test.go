package plugins

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

func writeDebrisFixture(t *testing.T, root string) map[string]bool {
	t.Helper()
	old := time.Now().Add(-72 * time.Hour)
	mk := func(rel string, dir bool, stamp time.Time) {
		path := filepath.Join(root, rel)
		if dir {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "payload"), make([]byte, 4096), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(filepath.Join(path, "payload"), stamp, stamp); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, make([]byte, 512), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	// expected matches (old)
	mk("bench-20260901-a", true, old)
	mk(".bulkload-native-abc", true, old)
	mk("rollback", true, old)
	mk("x-carry", true, old)
	mk(".RECLAIM-PENDING-1", false, old)
	mk("pre-boundary-1", true, old)
	mk("continuity-2", true, old)
	mk("repo/.bulkload-nested", true, old) // depth 1: inside max_depth 2
	// non-matches
	mk("fresh-20260922", true, time.Now())   // too young
	mk("y-carry", false, old)                // dir-only pattern, this is a file
	mk("keep", true, old)                    // no pattern
	mk("repo/sub/.bulkload-deep", true, old) // depth 2: beyond max_depth
	mk("repo/sub/rollback", true, old)       // depth 2: beyond max_depth
	// parent dirs were touched by children; restamp
	for _, rel := range []string{"repo", "repo/sub"} {
		if err := os.Chtimes(filepath.Join(root, rel), old, old); err != nil {
			t.Fatal(err)
		}
	}
	return map[string]bool{
		"bench-20260901-a":     true,
		".bulkload-native-abc": true,
		"rollback":             true,
		"x-carry":              true,
		".RECLAIM-PENDING-1":   true,
		"pre-boundary-1":       true,
		"continuity-2":         true,
		".bulkload-nested":     true,
	}
}

func debrisTestConfig(root string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Enable.DebrisReport = true
	cfg.DebrisReport.ScanPaths = []string{root}
	cfg.DebrisReport.Patterns = config.DefaultDebrisPatterns()
	cfg.DebrisReport.OlderThan = "24h"
	cfg.DebrisReport.MaxDepth = 2
	return cfg
}

func TestDebrisReportPlanListsStaleDebrisOnly(t *testing.T) {
	root := t.TempDir()
	want := writeDebrisFixture(t, root)
	plugin := NewDebrisReportPlugin()
	cfg := debrisTestConfig(root)

	plan := plugin.PlanCleanup(context.Background(), LevelWarning, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !plan.WouldRun {
		t.Fatal("expected report-only plan to be eligible")
	}
	got := map[string]bool{}
	for _, target := range plan.Targets {
		if target.Action != "report" || target.Reclaim != CleanupReclaimNone || target.Type != "debris" {
			t.Fatalf("unexpected target shape: %+v", target)
		}
		if target.Bytes <= 0 {
			t.Fatalf("expected a measured size for %s", target.Path)
		}
		got[filepath.Base(target.Path)] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("expected %s in the debris report; got %v", name, got)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("did not expect %s in the debris report", name)
		}
	}
	if plan.Metadata["host_reclaim_candidate_bytes"] != "0" {
		t.Fatalf("report-only plan must not promise host reclaim: %v", plan.Metadata)
	}
}

func TestDebrisReportCleanupNeverDeletes(t *testing.T) {
	root := t.TempDir()
	writeDebrisFixture(t, root)
	plugin := NewDebrisReportPlugin()
	cfg := debrisTestConfig(root)

	before := listTree(t, root)
	result := plugin.Cleanup(context.Background(), LevelCritical, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.BytesFreed != 0 || result.ItemsCleaned != 0 {
		t.Fatalf("report-only plugin must not claim reclaim: %+v", result)
	}
	after := listTree(t, root)
	if len(before) != len(after) {
		t.Fatalf("tree changed: before %d entries, after %d", len(before), len(after))
	}
	for path := range before {
		if !after[path] {
			t.Fatalf("%s was removed by a report-only plugin", path)
		}
	}
}

func TestDebrisReportDisabledByDefault(t *testing.T) {
	if NewDebrisReportPlugin().Enabled(config.DefaultConfig()) {
		t.Fatal("debris-report must be off unless enable.debris_report is set")
	}
}

func TestMatchDebrisPatternDirectoryOnly(t *testing.T) {
	patterns := config.DefaultDebrisPatterns()
	if _, ok := matchDebrisPattern("x-carry", false, patterns); ok {
		t.Fatal("*-carry/ must only match directories")
	}
	if pattern, ok := matchDebrisPattern("x-carry", true, patterns); !ok || pattern != "*-carry/" {
		t.Fatalf("expected *-carry/ to match directory, got %q %v", pattern, ok)
	}
	for _, name := range []string{"bench-20260923", "capture-20271231T0100Z", "notes-20250101"} {
		if _, ok := matchDebrisPattern(name, true, patterns); !ok {
			t.Fatalf("dated name %q must match in any year", name)
		}
	}
	for _, name := range []string{"notes-2025xxxx", "build-2026", "v-20261341"} {
		if _, ok := matchDebrisPattern(name, true, patterns); ok {
			t.Fatalf("%q is not a YYYYMMDD-stamped name and must not match", name)
		}
	}
}

func listTree(t *testing.T, root string) map[string]bool {
	t.Helper()
	tree := map[string]bool{}
	if err := filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		tree[path] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return tree
}
