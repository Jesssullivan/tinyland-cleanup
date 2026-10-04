//go:build tinyland_sim

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSimulatedDiskStats(t *testing.T) {
	stats, err := simulatedDiskStats(`{"total_bytes": 1000, "free_bytes": 50, "fstype": "apfs"}`, "/any")
	if err != nil {
		t.Fatal(err)
	}
	if stats.UsedPercent != 95 || stats.Free != 50 || !stats.InodesDynamic || stats.Path != "/any" {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	byPath := `{"/home": {"total_bytes": 100, "free_bytes": 10}, "*": {"total_bytes": 100, "free_bytes": 90}}`
	if s, err := simulatedDiskStats(byPath, "/home"); err != nil || s.Free != 10 {
		t.Fatalf("path entry: %+v %v", s, err)
	}
	if s, err := simulatedDiskStats(byPath, "/other"); err != nil || s.Free != 90 {
		t.Fatalf("fallback entry: %+v %v", s, err)
	}

	for _, bad := range []string{"", "not json", `{"total_bytes": 10, "free_bytes": 11}`, `{"free_bytes": 1}`, `{"/x": {"total_bytes": 1, "free_bytes": 0}}`} {
		if _, err := simulatedDiskStats(bad, "/home"); err == nil {
			t.Fatalf("%q must be an error, never real stats", bad)
		}
	}
}

func TestSimulatedDiskStatsFileIsReread(t *testing.T) {
	file := filepath.Join(t.TempDir(), "stats.json")
	t.Setenv(simDiskStatsEnv, "@"+file)
	read := diskStatsReader()
	for _, free := range []string{"10", "60"} {
		if err := os.WriteFile(file, []byte(`{"total_bytes": 100, "free_bytes": `+free+`}`), 0o644); err != nil {
			t.Fatal(err)
		}
		stats, err := read("/")
		if err != nil {
			t.Fatal(err)
		}
		if got := stats.Free; (free == "10" && got != 10) || (free == "60" && got != 60) {
			t.Fatalf("free=%s read %d", free, got)
		}
	}
	if !simulationBuild || simulationSuffix() == "" {
		t.Fatal("tagged build must identify itself as a simulation build")
	}
}
