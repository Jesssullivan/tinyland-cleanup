package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	loaded, _, err := loadCleanupState(path, time.Now())
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
	loaded, _, err := loadCleanupState(path, now)
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
	state, quarantined, err := loadCleanupState(filepath.Join(t.TempDir(), "missing.json"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if quarantined != "" {
		t.Fatalf("missing file should not be quarantined, got %q", quarantined)
	}
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

func TestLoadCleanupStateQuarantinesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	corrupt := []byte("{\"version\": 1, \"plugins\": {\"nix\": ")
	if err := os.WriteFile(path, corrupt, 0644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

	state, quarantined, err := loadCleanupState(path, now)
	if err != nil {
		t.Fatalf("corrupt state must not disable accounting, got error %v", err)
	}
	if state == nil || state.Version != cleanupStateVersion || len(state.Plugins) != 0 {
		t.Fatalf("expected fresh state, got %#v", state)
	}
	want := path + ".corrupt-20261003T120000Z"
	if quarantined != want {
		t.Fatalf("quarantined = %q, want %q", quarantined, want)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("corrupt file should have been moved away, stat err = %v", err)
	}
	kept, err := os.ReadFile(quarantined)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(kept, corrupt) {
		t.Fatalf("quarantined bytes changed: %q", kept)
	}

	// A second corrupt file in the same second never overwrites the first.
	if err := os.WriteFile(path, []byte("not json"), 0644); err != nil {
		t.Fatal(err)
	}
	_, second, err := loadCleanupState(path, now)
	if err != nil {
		t.Fatal(err)
	}
	if second == quarantined || !strings.HasPrefix(second, want) {
		t.Fatalf("second quarantine = %q, want a distinct name after %q", second, want)
	}
	if kept, _ := os.ReadFile(quarantined); !bytes.Equal(kept, corrupt) {
		t.Fatal("first quarantined file was overwritten")
	}
}

func TestSaveCleanupStateIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{\"version\": 1, \"plugins\": {}}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	state := newCleanupState()
	state.recordPluginRun("nix", plugins.LevelModerate, time.Now(), plugins.CleanupResult{Plugin: "nix", BytesFreed: 7})

	if err := saveCleanupState(path, state); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("expected only state.json after save, found %v", names)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0644 {
		t.Fatalf("state mode = %v, want 0644", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded cleanupState
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("saved state does not decode: %v", err)
	}
	if decoded.Plugins["nix"].LastBytesFreed != 7 {
		t.Fatalf("saved record = %+v", decoded.Plugins["nix"])
	}
}
