package plugins

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// Shape of `podman images --format json` on podman 5.8 (honey, 2026-10-28),
// trimmed to the fields the protected prune reads plus a few it ignores.
const podmanImagesJSONFixture = `[
  {"Id":"e077b23920bf73a28d3e715c29cdd678048432ec19c1648014790ecff588171d","ParentId":"","RepoTags":null,
   "RepoDigests":["docker.io/kicad/kicad@sha256:c8f05703e865a1caa7266851cf4986dd01d5fb1148e4f63382b6a73466ba7e71"],
   "Size":2640000000,"SharedSize":0,"Containers":0,"Names":["docker.io/kicad/kicad:10.0.7"]},
  {"Id":"9cfa957fae1d0000000000000000000000000000000000000000000000000000","Size":450000000,"Containers":1,
   "Names":["docker.io/pgvector/pgvector:0.8.0-pg17"]},
  {"Id":"5c64f40b9d1f0000000000000000000000000000000000000000000000000000","Size":1200000000,"Containers":0,
   "Names":["docker.io/opencfd/openfoam-default@sha256:adbf625ac47ed81b34018602a6caeb4076bfacc2046b2f8cd36f82adb376946b"]},
  {"Id":"aaaa000000000000000000000000000000000000000000000000000000000000","Size":10,"Containers":0,
   "Names":["localhost/a:1","localhost/a:latest"]},
  {"Id":"bbbb000000000000000000000000000000000000000000000000000000000000","Size":20,"Containers":0,"ReadOnly":true,
   "Names":["localhost/ro:1"]},
  {"Id":"cccc000000000000000000000000000000000000000000000000000000000000","Size":30,"Containers":0,"Names":null}
]`

func TestParsePodmanImagesJSON(t *testing.T) {
	images, err := parsePodmanImagesJSON(podmanImagesJSONFixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 6 {
		t.Fatalf("want 6 images, got %d", len(images))
	}
	if images[0].Names[0] != "docker.io/kicad/kicad:10.0.7" || images[0].Size != 2640000000 {
		t.Fatalf("unexpected first image: %#v", images[0])
	}
	if images[1].Containers != 1 || !images[4].ReadOnly {
		t.Fatalf("containers/readonly not parsed: %#v %#v", images[1], images[4])
	}
	for _, empty := range []string{"", "  ", "null", "[]"} {
		got, err := parsePodmanImagesJSON(empty)
		if err != nil || len(got) != 0 {
			t.Fatalf("parse %q: %v %#v", empty, err, got)
		}
	}
	if _, err := parsePodmanImagesJSON("not json"); err == nil {
		t.Fatal("want an error for malformed output")
	}
}

func TestSelectPodmanAgePruneCandidatesSkipsProtectedInUseAndReadOnly(t *testing.T) {
	images, err := parsePodmanImagesJSON(podmanImagesJSONFixture)
	if err != nil {
		t.Fatal(err)
	}
	// Duplicate listing of one image must not produce two removals.
	images = append(images, images[3])
	protected := map[string]string{
		"e077b23920bf73a28d3e715c29cdd678048432ec19c1648014790ecff588171d": "docker.io/kicad/kicad@sha256:c8f05703e865a1caa7266851cf4986dd01d5fb1148e4f63382b6a73466ba7e71",
	}
	var got []string
	for _, img := range selectPodmanAgePruneCandidates(images, protected, podmanKeep{}) {
		got = append(got, img.ID[:4])
	}
	want := []string{"5c64", "aaaa", "cccc"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}

	// Without the protection the KiCad image is a candidate, as with image prune -a.
	got = nil
	for _, img := range selectPodmanAgePruneCandidates(images, nil, podmanKeep{}) {
		got = append(got, img.ID[:4])
	}
	if got[0] != "e077" {
		t.Fatalf("unprotected listing should start with the kicad image, got %v", got)
	}
}

func TestPodmanRemoveImageArgs(t *testing.T) {
	cases := []struct {
		img  podmanImage
		want []string
	}{
		{podmanImage{ID: "aaaa", Names: []string{"localhost/a:1", "localhost/a:latest"}}, []string{"rmi", "localhost/a:1", "localhost/a:latest"}},
		{podmanImage{ID: "cccc"}, []string{"rmi", "cccc"}},
	}
	for _, c := range cases {
		got := podmanRemoveImageArgs(c.img)
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("args = %v, want %v", got, c.want)
		}
		for _, a := range got {
			if a == "-f" || a == "--force" {
				t.Fatalf("removal must never force: %v", got)
			}
		}
	}
}

func TestPodmanProtectedImagesConfigYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	doc := "podman:\n  prune_images_age: 24h\n  protected_images:\n    - docker.io/kicad/kicad@sha256:c8f05703e865a1caa7266851cf4986dd01d5fb1148e4f63382b6a73466ba7e71\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Podman.ProtectedImages) != 1 || !strings.HasSuffix(cfg.Podman.ProtectedImages[0], "7e71") {
		t.Fatalf("protected_images not parsed: %#v", cfg.Podman.ProtectedImages)
	}
	if def := config.DefaultConfig(); len(def.Podman.ProtectedImages) != 0 {
		t.Fatalf("default must protect nothing: %#v", def.Podman.ProtectedImages)
	}
}

func TestPodmanModeratePlanNamesProtectedImages(t *testing.T) {
	cfg := config.DefaultConfig()
	p := &PodmanPlugin{environment: &PodmanEnvironment{Runtime: "podman"}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	plan := p.PlanCleanup(context.Background(), LevelModerate, cfg, logger)
	if plan.Metadata["protected_images"] != "0" || !strings.Contains(strings.Join(plan.Steps, "\n"), "Prune Podman images older than 24h") {
		t.Fatalf("default plan: %#v %#v", plan.Metadata, plan.Steps)
	}

	cfg.Podman.ProtectedImages = []string{"docker.io/kicad/kicad@sha256:c8f0"}
	plan = p.PlanCleanup(context.Background(), LevelModerate, cfg, logger)
	if plan.Metadata["protected_images"] != "1" || !strings.Contains(strings.Join(plan.Steps, "\n"), "keeping 1 protected image reference(s)") {
		t.Fatalf("protected plan: %#v %#v", plan.Metadata, plan.Steps)
	}
}

// TestPodmanModerateCleanupKeepsProtectedImage drives cleanModerate against a
// fake podman on PATH and checks the exact commands it ran.
func TestPodmanModerateCleanupKeepsProtectedImage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	fixture := filepath.Join(dir, "images.json")
	if err := os.WriteFile(fixture, []byte(podmanImagesJSONFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
echo "$*" >> "` + logPath + `"
case "$1 $2" in
  "image inspect")
    case "$5" in
      docker.io/kicad/kicad@sha256:c8f0*) echo e077b23920bf73a28d3e715c29cdd678048432ec19c1648014790ecff588171d ;;
      *) echo "Error: $5: image not known" >&2; exit 125 ;;
    esac ;;
  "images --format") cat "` + fixture + `" ;;
  "rmi cccc"*) echo "Error: in use" >&2; exit 2 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := config.DefaultConfig()
	cfg.Podman.ProtectedImages = []string{
		"docker.io/kicad/kicad@sha256:c8f05703e865a1caa7266851cf4986dd01d5fb1148e4f63382b6a73466ba7e71",
		"docker.io/absent/image@sha256:0000",
	}
	p := &PodmanPlugin{environment: &PodmanEnvironment{Runtime: "podman"}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	result := p.cleanModerate(context.Background(), cfg, logger)

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	calls := string(raw)
	if strings.Contains(calls, "image prune -af") {
		t.Fatalf("protected mode must not run image prune -af:\n%s", calls)
	}
	if strings.Contains(calls, "kicad/kicad:10.0.7") && strings.Contains(calls, "rmi docker.io/kicad") {
		t.Fatalf("protected kicad image was removed:\n%s", calls)
	}
	for _, want := range []string{
		"images --format json --filter until=24h",
		"rmi docker.io/opencfd/openfoam-default@sha256:adbf",
		"rmi localhost/a:1 localhost/a:latest",
		"rmi cccc",
	} {
		if !strings.Contains(calls, want) {
			t.Fatalf("missing call %q in:\n%s", want, calls)
		}
	}
	for _, never := range []string{"rmi localhost/ro:1", "rmi docker.io/pgvector", "--force", "rmi -f"} {
		if strings.Contains(calls, never) {
			t.Fatalf("unexpected call %q in:\n%s", never, calls)
		}
	}
	// openfoam (1.2e9) + localhost/a (10); cccc failed and is not counted.
	if result.BytesFreed != 1200000010 {
		t.Fatalf("BytesFreed = %d", result.BytesFreed)
	}
}
