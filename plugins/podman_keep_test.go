package plugins

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

const laceinLabel = "io.xoxd.lacein-harness"

func TestReadPodmanKeepFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keep-images")
	doc := "# pins written by the harness\n" +
		"ghcr.io/xoxd-ai/blahaj/rspamd@sha256:b544  # rspamd\n" +
		"\n   \n" +
		"ghcr.io/xoxd-ai/blahaj/postfix-rocky@sha256:2070\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readPodmanKeepFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ghcr.io/xoxd-ai/blahaj/rspamd@sha256:b544", "ghcr.io/xoxd-ai/blahaj/postfix-rocky@sha256:2070"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("refs = %v, want %v", got, want)
	}
	if got, err := readPodmanKeepFile(filepath.Join(dir, "absent")); err != nil || got != nil {
		t.Fatalf("missing file must hold no pins and no error: %v %v", got, err)
	}
	if _, err := readPodmanKeepFile(dir); err == nil {
		t.Fatal("a directory must be an unreadable keep file")
	}
}

func TestLoadPodmanKeepMergesConfigFileAndLabels(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".config/tinyland-cleanup"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config/tinyland-cleanup/keep-images"), []byte("a@sha256:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.PodmanConfig{
		ProtectedImages:     []string{" b@sha256:2 ", ""},
		ProtectedImagesFile: "~/.config/tinyland-cleanup/keep-images",
		ProtectedLabels:     []string{laceinLabel, " "},
	}
	keep := loadPodmanKeep(cfg, home)
	if !reflect.DeepEqual(keep.Refs, []string{"b@sha256:2", "a@sha256:1"}) || keep.FileRefs != 1 || keep.FileErr != nil {
		t.Fatalf("keep = %#v", keep)
	}
	if !reflect.DeepEqual(keep.Labels, []string{laceinLabel}) || !keep.active() {
		t.Fatalf("labels = %#v", keep.Labels)
	}
	if got := keep.labelFilterArgs(); !reflect.DeepEqual(got, []string{"--filter", "label!=" + laceinLabel}) {
		t.Fatalf("filters = %v", got)
	}
	if loadPodmanKeep(config.PodmanConfig{}, home).active() {
		t.Fatal("an empty config must not activate the keep rule")
	}
}

func TestPodmanKeepProtectsLabels(t *testing.T) {
	keep := podmanKeep{Labels: []string{laceinLabel, "owner=codex-privacy"}}
	cases := []struct {
		labels map[string]string
		want   bool
	}{
		{map[string]string{laceinLabel: "run-1"}, true},
		{map[string]string{laceinLabel: ""}, true},
		{map[string]string{"owner": "codex-privacy"}, true},
		{map[string]string{"owner": "someone-else"}, false},
		{map[string]string{"io.xoxd.lacein-harness.image": "probe"}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := keep.protectsLabels(c.labels); got != c.want {
			t.Fatalf("protectsLabels(%v) = %v, want %v", c.labels, got, c.want)
		}
	}
}

func TestSelectPodmanAgePruneCandidatesSkipsProtectedLabels(t *testing.T) {
	images := []podmanImage{
		{ID: "1111", Names: []string{"localhost/harness:probe"}, Labels: map[string]string{laceinLabel + ".image": "probe"}},
		{ID: "2222", Names: []string{"localhost/other:1"}},
	}
	keep := podmanKeep{Labels: []string{laceinLabel + ".image"}}
	got := selectPodmanAgePruneCandidates(images, nil, keep)
	if len(got) != 1 || got[0].ID != "2222" {
		t.Fatalf("candidates = %#v", got)
	}
}

// fakePodman puts a podman on PATH that logs each call and serves the images
// fixture. It returns the call log path.
func fakePodman(t *testing.T, imagesJSON string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fake")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	fixture := filepath.Join(dir, "images.json")
	if err := os.WriteFile(fixture, []byte(imagesJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
echo "$*" >> "` + logPath + `"
case "$1 $2" in
  "image inspect")
    case "$5" in
      ghcr.io/xoxd-ai/blahaj/rspamd@sha256:b544*) echo r5pa000000000000000000000000000000000000000000000000000000000000 ;;
      broken/*) echo "Error: database is locked" >&2; exit 125 ;;
      *) echo "Error: $5: image not known" >&2; exit 125 ;;
    esac ;;
  "images --format") cat "` + fixture + `" ;;
  "ps -a") cat "` + filepath.Join(dir, "ps.json") + `" ;;
  "volume ls") cat "` + filepath.Join(dir, "volumes.json") + `" ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour).Unix()
	ps := fmt.Sprintf(`[
  {"Id":"c-both","State":"exited","Created":%[1]d,"Labels":{"io.xoxd.lacein-harness":"run-1","io.xoxd.lacein-harness.image":"probe"}},
  {"Id":"c-one","State":"created","Created":%[1]d,"Labels":{"io.xoxd.lacein-harness":"run-2"}},
  {"Id":"c-plain","State":"exited","Created":%[1]d},
  {"Id":"c-young","State":"exited","Created":%[2]d},
  {"Id":"c-running","State":"running","Created":%[1]d},
  {"Id":"c-pod","State":"exited","Created":%[1]d,"Pod":"p1"}
]`, old, time.Now().Unix())
	vols := `[{"Name":"v-one","Labels":{"io.xoxd.lacein-harness":"run-2"}},{"Name":"v-plain","Labels":{}}]`
	for name, body := range map[string]string{"ps.json": ps, "volumes.json": vols} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

const laceinImagesFixture = `[
  {"Id":"r5pa000000000000000000000000000000000000000000000000000000000000","Size":500,"Containers":0,
   "Names":["ghcr.io/xoxd-ai/blahaj/rspamd@sha256:b544caf8837d89a1ec9a7a20b72a571db2228b34e38d4dae635566bbda9d9630"]},
  {"Id":"9906000000000000000000000000000000000000000000000000000000000000","Size":300,"Containers":0,
   "Names":["localhost/lacein-probe:dev"],"Labels":{"io.xoxd.lacein-harness.image":"probe"}},
  {"Id":"use0000000000000000000000000000000000000000000000000000000000000","Size":200,"Containers":1,
   "Names":["ghcr.io/xoxd-ai/blahaj/postfix-rocky@sha256:2070"]},
  {"Id":"old0000000000000000000000000000000000000000000000000000000000000","Size":100,"Containers":0,
   "Names":["docker.io/library/old:1"]}
]`

func keepTestConfig(t *testing.T) *config.Config {
	t.Helper()
	keepFile := filepath.Join(t.TempDir(), "keep-images")
	if err := os.WriteFile(keepFile, []byte("ghcr.io/xoxd-ai/blahaj/rspamd@sha256:b544caf8837d89a1ec9a7a20b72a571db2228b34e38d4dae635566bbda9d9630\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Podman.ProtectedImagesFile = keepFile
	cfg.Podman.ProtectedLabels = []string{laceinLabel, laceinLabel + ".image"}
	return cfg
}

func readCalls(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func indexOfPrefix(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

// TestPodmanAggressiveKeepRule checks the three keep rules end to end against
// a fake podman: the keep-file digest and the labelled image survive the age
// prune, every prune carries the label exclusions, volumes and networks are
// pruned before stopped containers, and no system prune runs.
func TestPodmanAggressiveKeepRule(t *testing.T) {
	logPath := fakePodman(t, laceinImagesFixture)
	cfg := keepTestConfig(t)
	p := &PodmanPlugin{environment: &PodmanEnvironment{Runtime: "podman"}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	result := p.cleanAggressive(context.Background(), cfg, logger)
	calls := readCalls(t, logPath)
	joined := strings.Join(calls, "\n")

	filters := "--filter label!=" + laceinLabel + " --filter label!=" + laceinLabel + ".image"
	for _, want := range []string{
		"volume ls --format json --filter dangling=true",
		"volume rm v-plain",
		"network prune -f " + filters,
		"ps -a --format json",
		"rm c-plain",
		"image prune -f " + filters,
		"rmi docker.io/library/old:1",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing call %q in:\n%s", want, joined)
		}
	}
	// B1: a resource with only one of the protected labels is still kept.
	for _, never := range []string{"system prune", "image prune -af", "--build-cache", "container prune", "volume prune",
		"volume rm v-one", "rm c-both", "rm c-one", "rm c-young", "rm c-running", "rm c-pod",
		"rmi ghcr.io/xoxd-ai/blahaj/rspamd", "rmi localhost/lacein-probe", "rmi ghcr.io/xoxd-ai/blahaj/postfix-rocky", "--force", " -f c-"} {
		if strings.Contains(joined, never) {
			t.Fatalf("unexpected call %q in:\n%s", never, joined)
		}
	}
	vol, net, ctr := indexOfPrefix(calls, "volume ls"), indexOfPrefix(calls, "network prune"), indexOfPrefix(calls, "ps -a")
	if vol < 0 || net < 0 || ctr < 0 || vol > ctr || net > ctr {
		t.Fatalf("volumes and networks must be pruned before stopped containers:\n%s", joined)
	}
	if result.Level != LevelAggressive {
		t.Fatalf("level = %v", result.Level)
	}
}

func TestPodmanUnreadableKeepFileSkipsTheAgePrune(t *testing.T) {
	logPath := fakePodman(t, laceinImagesFixture)
	cfg := config.DefaultConfig()
	cfg.Podman.ProtectedImagesFile = t.TempDir() // a directory: unreadable as a file
	p := &PodmanPlugin{environment: &PodmanEnvironment{Runtime: "podman"}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p.cleanModerate(context.Background(), cfg, logger)
	joined := strings.Join(readCalls(t, logPath), "\n")
	if strings.Contains(joined, "rmi ") || strings.Contains(joined, "image prune -af") || strings.Contains(joined, "images --format") {
		t.Fatalf("age prune must not run with an unreadable keep file:\n%s", joined)
	}
	plan := p.PlanCleanup(context.Background(), LevelModerate, cfg, logger)
	if !strings.Contains(strings.Join(plan.Steps, "\n"), "Skip the Podman age prune") || len(plan.Warnings) == 0 {
		t.Fatalf("plan must name the skip: %#v %#v", plan.Steps, plan.Warnings)
	}
}

func TestPodmanCriticalSystemPruneYieldsToTheKeepRule(t *testing.T) {
	logPath := fakePodman(t, laceinImagesFixture)
	cfg := keepTestConfig(t)
	cfg.Podman.CriticalSystemPrune = true
	cfg.Podman.BuildKitPrune = false
	p := &PodmanPlugin{environment: &PodmanEnvironment{Runtime: "podman"}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p.cleanCritical(context.Background(), cfg, logger)
	joined := strings.Join(readCalls(t, logPath), "\n")
	if strings.Contains(joined, "system prune") {
		t.Fatalf("system prune must not run while the keep rule is active:\n%s", joined)
	}
	if !strings.Contains(joined, "volume ls --format json --filter dangling=true") {
		t.Fatalf("targeted prunes missing:\n%s", joined)
	}
}

// S1: a configured keep file that is missing or empty still keeps the
// critical system prune off.
func TestPodmanMissingKeepFileStillBlocksSystemPrune(t *testing.T) {
	logPath := fakePodman(t, laceinImagesFixture)
	cfg := config.DefaultConfig()
	cfg.Podman.ProtectedImagesFile = filepath.Join(t.TempDir(), "not-written-yet")
	cfg.Podman.CriticalSystemPrune = true
	cfg.Podman.BuildKitPrune = false
	if !loadPodmanKeep(cfg.Podman, "").active() {
		t.Fatal("a configured keep file must activate the keep rule")
	}
	p := &PodmanPlugin{environment: &PodmanEnvironment{Runtime: "podman"}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p.cleanCritical(context.Background(), cfg, logger)
	if joined := strings.Join(readCalls(t, logPath), "\n"); strings.Contains(joined, "system prune") {
		t.Fatalf("system prune ran with a configured keep file:\n%s", joined)
	}
}

// S2: only a definite not-found means a pin is absent.
func TestPodmanInspectErrorKeepsThePin(t *testing.T) {
	logPath := fakePodman(t, laceinImagesFixture)
	cfg := config.DefaultConfig()
	cfg.Podman.ProtectedImages = []string{"broken/image@sha256:1", "absent/image@sha256:2"}
	p := &PodmanPlugin{environment: &PodmanEnvironment{Runtime: "podman"}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p.cleanModerate(context.Background(), cfg, logger)
	if joined := strings.Join(readCalls(t, logPath), "\n"); strings.Contains(joined, "rmi ") {
		t.Fatalf("an unresolved pin must stop the age prune:\n%s", joined)
	}
	if !podmanImageNotKnown("Error: absent/image: image not known") || podmanImageNotKnown("Error: database is locked") {
		t.Fatal("podmanImageNotKnown misclassifies")
	}
}

func TestReadPodmanKeepFileFailsClosedOnOddFiles(t *testing.T) {
	dir := t.TempDir()
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "gone"), dangling); err != nil {
		t.Fatal(err)
	}
	if _, err := readPodmanKeepFile(dangling); err == nil {
		t.Fatal("a dangling symlink must fail closed")
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() { _, err := readPodmanKeepFile(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO must fail closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO keep file blocked")
	}
	keep := loadPodmanKeep(config.PodmanConfig{ProtectedImagesFile: dangling}, dir)
	if keep.FileErr == nil || !keep.active() {
		t.Fatalf("dangling keep file: %#v", keep)
	}
}

func TestSelectStoppedContainers(t *testing.T) {
	now := time.Now()
	old := now.Add(-2 * time.Hour).Unix()
	keep := podmanKeep{Labels: []string{laceinLabel, laceinLabel + ".image"}}
	in := []podmanContainer{
		{ID: "one-label", State: "exited", Created: old, Labels: map[string]string{laceinLabel: "x"}},
		{ID: "plain", State: "Exited", Created: old},
		{ID: "created", State: "created", Created: old},
		{ID: "young", State: "exited", Created: now.Unix()},
		{ID: "running", State: "running", Created: old},
		{ID: "paused", State: "paused", Created: old},
		{ID: "infra", State: "exited", Created: old, IsInfra: true},
	}
	var got []string
	for _, c := range selectStoppedContainers(in, keep, now.Add(-time.Hour)) {
		got = append(got, c.ID)
	}
	if !reflect.DeepEqual(got, []string{"plain", "created"}) {
		t.Fatalf("selected %v", got)
	}
}

func TestPodmanKeepConfigYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	doc := "podman:\n  protected_images_file: ~/.config/tinyland-cleanup/keep-images\n  protected_labels:\n    - io.xoxd.lacein-harness\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Podman.ProtectedImagesFile != "~/.config/tinyland-cleanup/keep-images" || len(cfg.Podman.ProtectedLabels) != 1 {
		t.Fatalf("keep config not parsed: %#v", cfg.Podman)
	}
	def := config.DefaultConfig()
	if def.Podman.ProtectedImagesFile != "" || len(def.Podman.ProtectedLabels) != 0 {
		t.Fatalf("default must keep nothing extra: %#v", def.Podman)
	}
}
