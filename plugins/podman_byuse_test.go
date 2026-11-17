package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

func byUseID(c byte) string { return strings.Repeat(string(c), 64) }

func byUseInt(n int) *int { return &n }

var byUseNow = time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)

// byUseStore fakes the store time of each image directory.
func byUseStore(t *testing.T, times map[string]time.Time) {
	t.Helper()
	orig := podmanImageStoreTime
	podmanImageStoreTime = func(_ podmanStoreInfo, id string) (time.Time, error) {
		if ts, ok := times[id]; ok {
			return ts, nil
		}
		return time.Time{}, errors.New("no store directory")
	}
	t.Cleanup(func() { podmanImageStoreTime = orig })
}

func byUseVolumeBytes(t *testing.T, n int64) {
	t.Helper()
	orig := podmanVolumeBytes
	podmanVolumeBytes = func(context.Context, podmanStoreInfo, podmanByUseVolume) int64 { return n }
	t.Cleanup(func() { podmanVolumeBytes = orig })
}

func byUseDecisions(items []podmanByUseItem) map[string]bool {
	out := map[string]bool{}
	for _, it := range items {
		out[it.ID] = it.Remove
	}
	return out
}

func TestSelectPodmanByUseImagesKeepsEveryUnknownOrProtectedImage(t *testing.T) {
	old := byUseNow.Add(-72 * time.Hour)
	young := byUseNow.Add(-time.Hour)
	keep := podmanKeep{Refs: []string{"ghcr.io/x/pinned@sha256:feed"}, Labels: []string{laceinLabel}}
	images := []podmanByUseImage{
		{ID: byUseID('a'), Names: []string{"localhost/old:1"}, Containers: byUseInt(0), Created: old.Unix(), Size: 100},
		{ID: byUseID('b'), Containers: byUseInt(1), Created: old.Unix()},
		{ID: byUseID('c'), Created: old.Unix()}, // Containers missing
		{ID: byUseID('d'), Containers: byUseInt(0), Created: old.Unix()},
		{ID: byUseID('e'), RepoDigests: []string{"ghcr.io/x/pinned@sha256:feed"}, Containers: byUseInt(0), Created: old.Unix()},
		{ID: byUseID('f'), Containers: byUseInt(0), Created: old.Unix(), Labels: map[string]string{laceinLabel: "1"}},
		{ID: byUseID('0'), Containers: byUseInt(0), Created: old.Unix()}, // pulled recently
		{ID: byUseID('1'), Containers: byUseInt(0), Created: old.Unix()}, // store time unknown
		{ID: byUseID('2'), Containers: byUseInt(0), Created: young.Unix()},
		{ID: byUseID('3'), Containers: byUseInt(0), ReadOnly: true, Created: old.Unix()},
		{ID: byUseID('4'), Containers: byUseInt(0)}, // Created missing
		{ID: "abc", Containers: byUseInt(0), Created: old.Unix()},
		{ID: byUseID('a'), Containers: byUseInt(0), Created: old.Unix()}, // duplicate
	}
	byUseStore(t, map[string]time.Time{
		byUseID('a'): old, byUseID('b'): old, byUseID('c'): old, byUseID('d'): old,
		byUseID('e'): old, byUseID('f'): old, byUseID('0'): young, byUseID('2'): old,
		byUseID('3'): old, byUseID('4'): old, "abc": old,
	})
	protected := map[string]string{byUseID('d'): "ghcr.io/x/d@sha256:1"}
	items := selectPodmanByUseImages(images, protected, keep, podmanStoreInfo{}, 24*time.Hour, byUseNow)
	if len(items) != 12 {
		t.Fatalf("want 12 items (duplicate folded), got %d", len(items))
	}
	var removed []string
	for id, remove := range byUseDecisions(items) {
		if remove {
			removed = append(removed, id)
		}
	}
	if len(removed) != 1 || removed[0] != byUseID('a') {
		t.Fatalf("only the old unused unpinned image may go, got %v", removed)
	}
}

func TestSelectPodmanByUseVolumesKeepsYoungLabelledAndUnknown(t *testing.T) {
	old := byUseNow.Add(-48 * time.Hour).Format(time.RFC3339Nano)
	young := byUseNow.Add(-time.Minute).Format(time.RFC3339Nano)
	keep := podmanKeep{Labels: []string{laceinLabel}}
	volumes := []podmanByUseVolume{
		{Name: "v-old", Driver: "local", CreatedAt: old},
		{Name: "v-young", Driver: "local", CreatedAt: young},
		{Name: "v-label", Driver: "local", CreatedAt: old, Labels: map[string]string{laceinLabel: ""}},
		{Name: "v-badtime", Driver: "local", CreatedAt: "yesterday"},
		{Name: "v-nfs", Driver: "nfs", CreatedAt: old},
		{Name: "../escape", Driver: "local", CreatedAt: old},
	}
	got := byUseDecisions(selectPodmanByUseVolumes(volumes, keep, 24*time.Hour, byUseNow))
	for name, remove := range got {
		if remove != (name == "v-old") {
			t.Fatalf("volume %s remove=%v", name, remove)
		}
	}
	if len(got) != 6 {
		t.Fatalf("decisions = %v", got)
	}
}

func TestParsePodmanStoreInfoClosedAnswer(t *testing.T) {
	good, err := parsePodmanStoreInfo("true\t/home/u/.local/share/containers/storage\toverlay\n")
	if err != nil || !good.Rootless || good.Driver != "overlay" {
		t.Fatalf("good answer: %#v %v", good, err)
	}
	for _, bad := range []string{
		"", "yes\t/a\toverlay", "true\trelative\toverlay", "true\t/\toverlay",
		"true\t/a/../b\toverlay", "true\t/a\tover/lay", "true\t/a\toverlay\textra",
		"WARN something\ntrue\t/a\toverlay",
	} {
		if _, err := parsePodmanStoreInfo(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestPodmanImageStoreTimeNeedsARealDirectory(t *testing.T) {
	root := t.TempDir()
	store := podmanStoreInfo{GraphRoot: root, Driver: "overlay"}
	dir := filepath.Join(root, "overlay-images")
	if err := os.MkdirAll(filepath.Join(dir, byUseID('a')), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := podmanImageStoreTime(store, byUseID('a')); err != nil {
		t.Fatalf("real directory: %v", err)
	}
	if _, err := podmanImageStoreTime(store, byUseID('b')); err == nil {
		t.Fatal("a missing directory must be an error")
	}
	if err := os.Symlink(filepath.Join(dir, byUseID('a')), filepath.Join(dir, byUseID('c'))); err != nil {
		t.Fatal(err)
	}
	if _, err := podmanImageStoreTime(store, byUseID('c')); err == nil {
		t.Fatal("a symlink must be an error")
	}
}

// byUseFake installs a fake podman on PATH. Calls go to calls.log. An
// inspect answer is read from inspect-<id>.json; rmi fails for ids listed in
// refuse.
type byUseFake struct {
	dir, calls, keepFile, graph string
}

func newByUseFake(t *testing.T) *byUseFake {
	t.Helper()
	dir := t.TempDir()
	f := &byUseFake{
		dir:      dir,
		calls:    filepath.Join(dir, "calls.log"),
		keepFile: filepath.Join(dir, "keep-images"),
		graph:    filepath.Join(dir, "storage"),
	}
	script := `#!/bin/sh
D="` + dir + `"
echo "$*" >> "$D/calls.log"
case "$1 $2" in
  "info --format") printf 'true\t%s\toverlay\n' "$D/storage" ;;
  "images --format") cat "$D/images.json" ;;
  "volume ls") cat "$D/volumes.json" ;;
  "image inspect")
    if [ "$4" = json ]; then
      [ -f "$D/side-$5" ] && cat "$D/side-$5" >> "$D/keep-images"
      [ -f "$D/rmkeep-$5" ] && rm -f "$D/keep-images"
      [ -f "$D/inspect-$5.json" ] && { cat "$D/inspect-$5.json"; exit 0; }
      echo "Error: $5: image not known" >&2; exit 125
    fi
    [ -f "$D/pinfail" ] && { echo "Error: database is locked" >&2; exit 125; }
    id=$(awk -v r="$5" '$1==r {print $2}' "$D/pins" 2>/dev/null)
    [ -n "$id" ] && { echo "$id"; exit 0; }
    echo "Error: $5: image not known" >&2; exit 125 ;;
  "volume inspect")
    [ -f "$D/inspect-$5.json" ] && { cat "$D/inspect-$5.json"; exit 0; }
    echo "Error: no such volume $5" >&2; exit 125 ;;
  "rmi "*)
    grep -qx "$2" "$D/refuse" 2>/dev/null && { echo "Error: image in use" >&2; exit 2; } ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", dir)
	return f
}

func (f *byUseFake) write(t *testing.T, name string, v any) {
	t.Helper()
	var data []byte
	switch x := v.(type) {
	case string:
		data = []byte(x)
	default:
		var err error
		if data, err = json.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *byUseFake) log(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(f.calls)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(raw)
}

func (f *byUseFake) removals(t *testing.T) []string {
	var out []string
	for _, line := range strings.Split(f.log(t), "\n") {
		if strings.HasPrefix(line, "rmi ") || strings.HasPrefix(line, "volume rm ") || strings.Contains(line, "prune") {
			out = append(out, line)
		}
	}
	sort.Strings(out)
	return out
}

func byUseInspectImage(id, created string, labels map[string]string) []map[string]any {
	return []map[string]any{{"Id": id, "Created": created, "Labels": labels}}
}

// stage writes a live-like store: four images and three dangling volumes.
// Image a is old and unused; b is in use; c is pinned in the keep file; 9
// was pulled an hour ago. Volume v-old is old; v-young is new; v-re was
// recreated between listing and inspect.
func (f *byUseFake) stage(t *testing.T) *config.Config {
	t.Helper()
	old := time.Now().Add(-72 * time.Hour)
	oldS := old.UTC().Format(time.RFC3339Nano)
	f.write(t, "images.json", []podmanByUseImage{
		{ID: byUseID('a'), Names: []string{"localhost/a:1"}, Containers: byUseInt(0), Created: old.Unix(), Size: 1000},
		{ID: byUseID('b'), Names: []string{"localhost/b:1"}, Containers: byUseInt(2), Created: old.Unix(), Size: 2000},
		{ID: byUseID('c'), Names: []string{"ghcr.io/x/p@sha256:1"}, Containers: byUseInt(0), Created: old.Unix(), Size: 3000},
		{ID: byUseID('9'), Names: []string{"docker.io/y:1"}, Containers: byUseInt(0), Created: old.Unix(), Size: 4000},
	})
	f.write(t, "volumes.json", []podmanByUseVolume{
		{Name: "v-old", Driver: "local", CreatedAt: oldS},
		{Name: "v-young", Driver: "local", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)},
		{Name: "v-re", Driver: "local", CreatedAt: oldS},
	})
	f.write(t, "inspect-"+byUseID('a')+".json", byUseInspectImage(byUseID('a'), oldS, nil))
	f.write(t, "inspect-v-old.json", []podmanByUseVolume{{Name: "v-old", Driver: "local", CreatedAt: oldS}})
	// v-re was removed and recreated under the same name since the listing.
	// The new one is also older than the age, so only the CreatedAt
	// comparison keeps it.
	f.write(t, "inspect-v-re.json", []podmanByUseVolume{{Name: "v-re", Driver: "local", CreatedAt: time.Now().Add(-50 * time.Hour).UTC().Format(time.RFC3339Nano)}})
	f.write(t, "pins", "ghcr.io/x/p@sha256:1 "+byUseID('c')+"\n")
	f.write(t, "keep-images", "ghcr.io/x/p@sha256:1\n")
	byUseStore(t, map[string]time.Time{
		byUseID('a'): old, byUseID('b'): old, byUseID('c'): old, byUseID('9'): time.Now().Add(-time.Hour),
	})
	byUseVolumeBytes(t, 500)

	cfg := config.DefaultConfig()
	cfg.Podman.PruneByUse = "apply"
	cfg.Podman.ProtectedImagesFile = f.keepFile
	cfg.Podman.ProtectedLabels = []string{laceinLabel}
	return cfg
}

func byUseLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func byUsePlugin() *PodmanPlugin {
	return &PodmanPlugin{environment: &PodmanEnvironment{Runtime: "podman"}}
}

func TestPodmanByUseApplyRemovesOnlyRecheckedCandidatesWithoutForce(t *testing.T) {
	f := newByUseFake(t)
	cfg := f.stage(t)

	result := byUsePlugin().cleanModerate(context.Background(), cfg, byUseLogger())

	got := f.removals(t)
	want := []string{"rmi " + byUseID('a'), "volume rm v-old"}
	// The dangling-image prune of cleanModerate stays; filter it out.
	var removals []string
	for _, line := range got {
		if strings.HasPrefix(line, "image prune -f") {
			continue
		}
		removals = append(removals, line)
	}
	if strings.Join(removals, "|") != strings.Join(want, "|") {
		t.Fatalf("removals = %q, want %q\ncalls:\n%s", removals, want, f.log(t))
	}
	for _, line := range strings.Split(f.log(t), "\n") {
		forced := (strings.HasPrefix(line, "rmi") || strings.HasPrefix(line, "volume rm")) &&
			(strings.Contains(line, " -f") || strings.Contains(line, "--force"))
		if forced || strings.Contains(line, "prune -a") || strings.Contains(line, "system prune") || strings.HasPrefix(line, "volume prune") {
			t.Fatalf("forced or broad removal %q:\n%s", line, f.log(t))
		}
	}
	if !strings.Contains(f.log(t), "image inspect --format json "+byUseID('a')) || !strings.Contains(f.log(t), "volume inspect --format json v-re") {
		t.Fatalf("removals were not re-checked:\n%s", f.log(t))
	}
	if result.BytesFreed != 1500 {
		t.Fatalf("BytesFreed = %d, want 1500", result.BytesFreed)
	}
}

func TestPodmanByUseRefusedRemovalIsNotCounted(t *testing.T) {
	f := newByUseFake(t)
	cfg := f.stage(t)
	f.write(t, "refuse", byUseID('a')+"\n")
	freed, removed := byUsePlugin().pruneByUse(context.Background(), cfg, byUseLogger())
	if freed != 500 || removed != 1 {
		t.Fatalf("freed=%d removed=%d, want only the volume", freed, removed)
	}
}

func TestPodmanByUseRecheckKeepsAnImageThatGainedAProtectedLabel(t *testing.T) {
	f := newByUseFake(t)
	cfg := f.stage(t)
	old := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339Nano)
	f.write(t, "inspect-"+byUseID('a')+".json", byUseInspectImage(byUseID('a'), old, map[string]string{laceinLabel: "x"}))
	byUsePlugin().pruneByUse(context.Background(), cfg, byUseLogger())
	if strings.Contains(f.log(t), "rmi "+byUseID('a')) {
		t.Fatalf("image removed after its re-check found a protected label:\n%s", f.log(t))
	}
}

func TestPodmanByUseRecheckStopsWhenTheKeepFileChanges(t *testing.T) {
	f := newByUseFake(t)
	cfg := f.stage(t)
	// Inspecting image a appends a pin, as a harness writing its pins would.
	f.write(t, "side-"+byUseID('a'), "localhost/a:1\n")
	byUsePlugin().pruneByUse(context.Background(), cfg, byUseLogger())
	if got := f.removals(t); len(got) != 0 {
		t.Fatalf("removals after the keep file changed: %q", got)
	}
}

func TestPodmanByUseReportModeAndDryRunRemoveNothing(t *testing.T) {
	f := newByUseFake(t)
	cfg := f.stage(t)

	cfg.Podman.PruneByUse = "report"
	if freed, removed := byUsePlugin().pruneByUse(context.Background(), cfg, byUseLogger()); freed != 0 || removed != 0 {
		t.Fatalf("report mode freed=%d removed=%d", freed, removed)
	}

	cfg.Podman.PruneByUse = "apply"
	plan := byUsePlugin().PlanCleanup(context.Background(), LevelCritical, cfg, byUseLogger())
	if got := f.removals(t); len(got) != 0 {
		t.Fatalf("report mode or dry-run removed: %q", got)
	}
	if plan.Metadata["prune_by_use_images_remove"] != "1" || plan.Metadata["prune_by_use_volumes_remove"] != "2" {
		t.Fatalf("plan metadata: %#v", plan.Metadata)
	}
	// Image a (1000) plus v-old and v-re (500 each); v-re is only caught by
	// the re-check at removal time.
	if plan.Metadata["prune_by_use_planned_bytes"] != "2000" || plan.EstimatedBytesFreed != 2000 {
		t.Fatalf("planned bytes: %#v %d", plan.Metadata, plan.EstimatedBytesFreed)
	}
	deletes := 0
	for _, target := range plan.Targets {
		if strings.HasPrefix(target.Action, "delete_podman_") {
			deletes++
		}
	}
	if deletes != 3 {
		t.Fatalf("delete targets = %d, want 3: %#v", deletes, plan.Targets)
	}
}

func TestPodmanByUseCriticalRunsWithoutTheSystemPrune(t *testing.T) {
	f := newByUseFake(t)
	cfg := f.stage(t)
	cfg.Podman.BuildKitPrune = false
	byUsePlugin().cleanCritical(context.Background(), cfg, byUseLogger())
	log := f.log(t)
	if !strings.Contains(log, "rmi "+byUseID('a')) || strings.Contains(log, "system prune") {
		t.Fatalf("critical calls:\n%s", log)
	}
}

func TestPodmanByUseUntouchedWhenOffOrOffLinux(t *testing.T) {
	cases := []struct {
		name string
		mode string
		goos string
		vm   bool
	}{
		{"off", "off", "linux", false},
		{"empty", "", "linux", false},
		{"darwin", "apply", "darwin", false},
		{"vm", "apply", "linux", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newByUseFake(t)
			cfg := f.stage(t)
			cfg.Podman.PruneByUse = c.mode
			cfg.Podman.BuildKitPrune = false
			cfg.Podman.CleanInsideVM = false
			cfg.Podman.TrimVMDisk = false
			orig := podmanByUseGOOS
			podmanByUseGOOS = c.goos
			t.Cleanup(func() { podmanByUseGOOS = orig })
			p := &PodmanPlugin{environment: &PodmanEnvironment{Runtime: "podman", NeedsVM: c.vm, VMRunning: c.vm}}
			p.cleanCritical(context.Background(), cfg, byUseLogger())
			plan := p.PlanCleanup(context.Background(), LevelCritical, cfg, byUseLogger())
			if strings.Contains(f.log(t), "Host.Security.Rootless") || strings.Contains(f.log(t), "rmi ") {
				t.Fatalf("prune-by-use ran:\n%s", f.log(t))
			}
			if _, ok := plan.Metadata["prune_by_use_mode"]; ok {
				t.Fatalf("plan names the pass: %#v", plan.Metadata)
			}
		})
	}
}

func TestPodmanByUsePlanSkipsOnUnknowns(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*testing.T, *byUseFake, *config.Config)
		want  string
	}{
		{"unreadable keep file", func(t *testing.T, f *byUseFake, cfg *config.Config) {
			cfg.Podman.ProtectedImagesFile = f.dir // a directory
		}, "protected_images_file_unreadable"},
		{"pin unresolvable", func(t *testing.T, f *byUseFake, cfg *config.Config) {
			f.write(t, "pinfail", "")
		}, "protected_image_unresolved"},
		{"pin resolves to no full ID", func(t *testing.T, f *byUseFake, cfg *config.Config) {
			f.write(t, "pins", "ghcr.io/x/p@sha256:1 WARN[0000]\n")
		}, "protected_image_unresolved"},
		{"keep file missing", func(t *testing.T, f *byUseFake, cfg *config.Config) {
			if err := os.Remove(f.keepFile); err != nil {
				t.Fatal(err)
			}
		}, "protected_images_file_missing"},
		{"age below floor", func(t *testing.T, f *byUseFake, cfg *config.Config) {
			cfg.Podman.PruneImagesAge = "30m"
		}, "prune_images_age_below_floor"},
		{"age invalid", func(t *testing.T, f *byUseFake, cfg *config.Config) {
			cfg.Podman.PruneImagesAge = "soon"
		}, "prune_images_age_invalid"},
		{"rootful", func(t *testing.T, f *byUseFake, cfg *config.Config) {
			script := filepath.Join(f.dir, "podman")
			raw, _ := os.ReadFile(script)
			patched := strings.Replace(string(raw), `printf 'true\t`, `printf 'false\t`, 1)
			if err := os.WriteFile(script, []byte(patched), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "podman_not_rootless"},
		{"image listing garbage", func(t *testing.T, f *byUseFake, cfg *config.Config) {
			f.write(t, "images.json", "WARN[0000] something\n[]")
		}, "image_listing_failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newByUseFake(t)
			cfg := f.stage(t)
			c.setup(t, f, cfg)
			plan := byUsePlugin().planByUse(context.Background(), cfg, time.Now(), byUseLogger())
			if plan.SkipReason != c.want {
				t.Fatalf("skip reason = %q, want %q", plan.SkipReason, c.want)
			}
			byUsePlugin().pruneByUse(context.Background(), cfg, byUseLogger())
			if got := f.removals(t); len(got) != 0 {
				t.Fatalf("removed despite %s: %q", c.name, got)
			}
		})
	}
}

func TestPodmanByUseConfig(t *testing.T) {
	if def := config.DefaultConfig(); def.Podman.PruneByUse != "off" {
		t.Fatalf("default prune_by_use = %q", def.Podman.PruneByUse)
	}
	for _, ok := range []string{"", "off", "report", "apply", " Apply "} {
		if err := (config.PodmanConfig{PruneByUse: ok}).ValidatePruneByUse(); err != nil {
			t.Fatalf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"on", "true", "yes"} {
		if err := (config.PodmanConfig{PruneByUse: bad}).ValidatePruneByUse(); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("podman:\n  prune_by_use: on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadConfig(path); err == nil {
		t.Fatal("LoadConfig accepted prune_by_use: on")
	}
	if got := podmanByUseMode(&config.Config{Podman: config.PodmanConfig{PruneByUse: "garbage"}}); got != podmanByUseOff {
		t.Fatalf("unknown mode maps to %q", got)
	}
}

// The fixture's image c is pinned only in the keep file. A missing keep file
// must not read as "no pins" (review B2).
func TestPodmanByUseMissingKeepFileRemovesNothing(t *testing.T) {
	f := newByUseFake(t)
	cfg := f.stage(t)
	if err := os.Remove(f.keepFile); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	byUsePlugin().cleanModerate(context.Background(), cfg, logger)
	if strings.Contains(f.log(t), "rmi ") || strings.Contains(f.log(t), "volume rm ") {
		t.Fatalf("removed with the keep file missing:\n%s", f.log(t))
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "protected_images_file_missing") {
		t.Fatalf("no warning for the missing keep file:\n%s", buf.String())
	}
}

func TestPodmanByUseEmptyKeepFileWarns(t *testing.T) {
	f := newByUseFake(t)
	cfg := f.stage(t)
	f.write(t, "keep-images", "# no pins yet\n")
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	plan := byUsePlugin().planByUse(context.Background(), cfg, time.Now(), logger)
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "pins=0") {
		t.Fatalf("no Warn with the pin count for an empty keep file:\n%s", buf.String())
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "holds no pins") {
		t.Fatalf("plan warnings = %q", plan.Warnings)
	}
	dry := byUsePlugin().PlanCleanup(context.Background(), LevelModerate, cfg, byUseLogger())
	found := false
	for _, w := range dry.Warnings {
		found = found || strings.Contains(w, "holds no pins")
	}
	if !found {
		t.Fatalf("dry-run plan omits the empty keep file: %q", dry.Warnings)
	}
}

func TestPodmanByUseRecheckKeepsWhenTheKeepFileDisappears(t *testing.T) {
	f := newByUseFake(t)
	cfg := f.stage(t)
	f.write(t, "keep-images", "")
	f.write(t, "rmkeep-"+byUseID('a'), "")
	byUsePlugin().pruneByUse(context.Background(), cfg, byUseLogger())
	if got := f.removals(t); len(got) != 0 {
		t.Fatalf("removals after the keep file disappeared: %q", got)
	}
}

// A pinned digest keeps the image under any repository name, and as a bare
// digest, although podman inspect answers "image not known" for both
// (review B1). The check runs at plan time and at the re-check.
func TestPodmanByUseDigestPinKeepsImageUnderAnyName(t *testing.T) {
	const digest = "sha256:b544b2f4d2b1a5c3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9"
	for _, pin := range []string{
		"ghcr.io/xoxd-ai/blahaj/rspamd@" + digest,
		"ghcr.io/xoxd-ai/blahaj/rspamd:1@" + digest,
		digest,
		"sha256:" + byUseID('a'),
	} {
		t.Run(pin, func(t *testing.T) {
			f := newByUseFake(t)
			cfg := f.stage(t)
			old := time.Now().Add(-72 * time.Hour)
			f.write(t, "images.json", []podmanByUseImage{{
				ID: byUseID('a'), Names: []string{"localhost/rspamd:pinned"},
				RepoDigests: []string{"localhost/rspamd@" + digest},
				Containers:  byUseInt(0), Created: old.Unix(), Size: 1000,
			}})
			f.write(t, "keep-images", pin+"\n")
			plan := byUsePlugin().planByUse(context.Background(), cfg, time.Now(), byUseLogger())
			if plan.SkipReason != "" || len(plan.Items) == 0 || plan.Items[0].Remove {
				t.Fatalf("pin %s did not keep the image: %#v", pin, plan)
			}
			// The re-check alone must also keep it.
			item := podmanByUseItem{Kind: "image", ID: byUseID('a'), Remove: true}
			insp := []map[string]any{{"Id": byUseID('a'), "Created": old.UTC().Format(time.RFC3339Nano), "RepoDigests": []string{"localhost/rspamd@" + digest}}}
			f.write(t, "inspect-"+byUseID('a')+".json", insp)
			if reason := byUsePlugin().recheckPodmanByUseItem(context.Background(), cfg, plan, item, time.Now()); reason == "" {
				t.Fatalf("re-check allows removal under pin %s", pin)
			}
		})
	}
	// Control: an unrelated digest pins nothing.
	img := podmanByUseImage{ID: byUseID('a'), RepoDigests: []string{"localhost/rspamd@" + digest}}
	other := map[string]struct{}{"ghcr.io/x/y@sha256:" + byUseID('f'): {}, "sha256:" + byUseID('f'): {}}
	if podmanDigestPinned(other, img) {
		t.Fatal("an unrelated digest pinned the image")
	}
}
