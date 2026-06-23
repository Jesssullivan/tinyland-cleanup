package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/plugins"
)

func TestInodeProgressTracking(t *testing.T) {
	now := time.Now()
	state := newCleanupState()

	if got := state.inodeNoProgressCount("/nix"); got != 0 {
		t.Fatalf("fresh state count = %d, want 0", got)
	}

	// Three consecutive cycles with no inode relief advance the counter.
	for i := 1; i <= 3; i++ {
		state.recordInodeProgress("/nix", 40, 96, "critical", now, false)
		if got := state.inodeNoProgressCount("/nix"); got != i {
			t.Fatalf("after %d no-progress cycles count = %d, want %d", i, got, i)
		}
	}

	// A cycle that frees inodes resets the counter.
	state.recordInodeProgress("/nix", 500, 50, "warning", now, true)
	if got := state.inodeNoProgressCount("/nix"); got != 0 {
		t.Fatalf("after improvement count = %d, want 0", got)
	}

	// Tracking is independent per path.
	state.recordInodeProgress("/home", 10, 99, "critical", now, false)
	if got := state.inodeNoProgressCount("/home"); got != 1 {
		t.Fatalf("/home count = %d, want 1", got)
	}
	if got := state.inodeNoProgressCount("/nix"); got != 0 {
		t.Fatalf("/nix count = %d, want 0 (unchanged)", got)
	}

	// Records survive a save/load round trip.
	path := filepath.Join(t.TempDir(), "state.json")
	if err := saveCleanupState(path, state); err != nil {
		t.Fatalf("saveCleanupState: %v", err)
	}
	loaded, err := loadCleanupState(path)
	if err != nil {
		t.Fatalf("loadCleanupState: %v", err)
	}
	if got := loaded.inodeNoProgressCount("/home"); got != 1 {
		t.Fatalf("loaded /home count = %d, want 1", got)
	}
	if rec := loaded.Inodes["/nix"]; rec.LastInodesFree != 500 || rec.LastInodeLevel != "warning" {
		t.Fatalf("loaded /nix record = %+v, want free=500 level=warning", rec)
	}
}

func TestCleanupStateRoundTripAndCooldown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	state := newCleanupState()
	state.recordPluginRun("nix", plugins.LevelModerate, now.Add(-5*time.Minute), plugins.CleanupResult{
		Plugin:       "nix",
		Level:        plugins.LevelModerate,
		BytesFreed:   42,
		ItemsCleaned: 2,
	})

	if err := saveCleanupState(path, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadCleanupState(path)
	if err != nil {
		t.Fatal(err)
	}

	remaining := loaded.cooldownRemaining("nix", plugins.LevelModerate, now, 30*time.Minute)
	if remaining != 25*time.Minute {
		t.Fatalf("remaining = %s, want 25m", remaining)
	}
	if loaded.cooldownRemaining("nix", plugins.LevelAggressive, now, 30*time.Minute) != 0 {
		t.Fatal("higher cleanup level should bypass prior lower-level cooldown")
	}
}

func TestLoadCleanupStateMissingFile(t *testing.T) {
	state, err := loadCleanupState(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != cleanupStateVersion {
		t.Fatalf("version = %d, want %d", state.Version, cleanupStateVersion)
	}
	if len(state.Plugins) != 0 {
		t.Fatalf("expected empty plugin state, got %#v", state.Plugins)
	}
}
