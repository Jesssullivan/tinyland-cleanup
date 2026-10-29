package plugins

import (
	"archive/tar"
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

// TestPodmanKeepRuleAgainstAThrowawayStore runs the keep rule against a real
// podman with its own store under a temporary directory. It is opt-in:
//
//	TINYLAND_PODMAN_STORE_TEST=1 go test ./plugins -run ThrowawayStore
//
// The images are imported from an empty tar and the containers are only
// created, never started, so nothing is pulled and nothing runs.
func TestPodmanKeepRuleAgainstAThrowawayStore(t *testing.T) {
	if os.Getenv("TINYLAND_PODMAN_STORE_TEST") != "1" {
		t.Skip("set TINYLAND_PODMAN_STORE_TEST=1 to run against a throwaway podman store")
	}
	real, err := exec.LookPath("podman")
	if err != nil {
		t.Skip("podman not on PATH")
	}
	store := t.TempDir()
	wrapDir := filepath.Join(store, "bin")
	if err := os.MkdirAll(wrapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	wrapper := "#!/bin/sh\nexec " + real +
		" --root " + filepath.Join(store, "root") +
		" --runroot " + filepath.Join(store, "runroot") +
		" --volumepath " + filepath.Join(store, "volumes") +
		" --network-config-dir " + filepath.Join(store, "networks") +
		" --tmpdir " + filepath.Join(store, "tmp") +
		" --events-backend none \"$@\"\n"
	if err := os.WriteFile(filepath.Join(wrapDir, "podman"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		// Store files can be owned through the user namespace, so remove
		// them from inside it. Never `system reset`: it also touches state
		// outside this store.
		_ = exec.Command(filepath.Join(wrapDir, "podman"), "unshare", "rm", "-rf",
			filepath.Join(store, "root"), filepath.Join(store, "runroot"), filepath.Join(store, "volumes"),
			filepath.Join(store, "networks"), filepath.Join(store, "tmp")).Run()
	})

	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(filepath.Join(wrapDir, "podman"), args...).CombinedOutput()
		if err != nil {
			t.Fatalf("podman %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	exists := func(kind, name string) bool {
		var args []string
		switch kind {
		case "image":
			args = []string{"image", "exists", name}
		case "container":
			args = []string{"container", "exists", name}
		case "volume":
			args = []string{"volume", "exists", name}
		case "network":
			args = []string{"network", "exists", name}
		}
		return exec.Command(filepath.Join(wrapDir, "podman"), args...).Run() == nil
	}

	emptyTar := filepath.Join(store, "empty.tar")
	f, err := os.Create(emptyTar)
	if err != nil {
		t.Fatal(err)
	}
	if err := tar.NewWriter(f).Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	const harness = "io.xoxd.lacein-harness"
	// A distinct ENV per image keeps the four image IDs apart.
	run("import", "--change", "ENV KEEPTEST=pinned", emptyTar, "localhost/pinned:1")
	run("import", "--change", "LABEL "+harness+".image=probe", emptyTar, "localhost/labeled:1")
	run("import", "--change", "ENV KEEPTEST=plain", emptyTar, "localhost/plain:1")
	run("import", "--change", "ENV KEEPTEST=used", emptyTar, "localhost/used:1")

	run("volume", "create", "--label", harness+"=run-1", "v-one")
	run("volume", "create", "v-plain")
	run("volume", "create", "v-used")
	run("network", "create", "--label", harness+"=run-1", "n-one")
	run("network", "create", "n-plain")
	run("network", "create", "n-used")

	// c-one carries only one of the two protected labels: the B1 case.
	run("create", "--name", "c-both", "--label", harness+"=run-1", "--label", harness+".image=probe",
		"--network", "n-used", "localhost/used:1", "/none")
	run("create", "--name", "c-one", "--label", harness+"=run-1", "-v", "v-used:/data", "localhost/used:1", "/none")
	run("create", "--name", "c-plain", "localhost/used:1", "/none")

	if !podmanImageNotKnown(func() string {
		out, _ := exec.Command(filepath.Join(wrapDir, "podman"), "image", "inspect", "--format", "{{.Id}}", "localhost/absent:1").CombinedOutput()
		return string(out)
	}()) {
		t.Fatal("podmanImageNotKnown does not recognize podman's not-found output")
	}

	keepFile := filepath.Join(store, "keep-images")
	if err := os.WriteFile(keepFile, []byte("localhost/pinned:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Podman.ProtectedImagesFile = keepFile
	cfg.Podman.ProtectedLabels = []string{harness, harness + ".image"}
	cfg.Podman.PruneImagesAge = "1s"
	keep := loadPodmanKeep(cfg.Podman, "")
	p := &PodmanPlugin{environment: &PodmanEnvironment{Runtime: "podman"}}
	var logOut io.Writer = io.Discard
	if os.Getenv("TINYLAND_PODMAN_STORE_DEBUG") == "1" {
		logOut = os.Stderr
	}
	logger := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := context.Background()

	time.Sleep(2 * time.Second) // let the images pass the 1s age filter
	p.cleanAggressive(ctx, cfg, logger)
	// Containers are minutes old, so the 1h container age keeps them all.
	p.removeStoppedContainers(ctx, keep, 0, time.Now().Add(time.Second), logger)
	p.removeUnusedVolumes(ctx, keep, logger)

	for _, c := range []struct {
		kind, name string
		want       bool
	}{
		{"image", "localhost/pinned:1", true},  // keep file
		{"image", "localhost/labeled:1", true}, // protected label
		{"image", "localhost/used:1", true},    // used by a container
		{"image", "localhost/plain:1", false},  // unprotected and unused
		{"container", "c-both", true},          // both labels
		{"container", "c-one", true},           // one label (B1)
		{"container", "c-plain", false},        // unprotected, stopped
		{"volume", "v-one", true},              // one label (B1)
		{"volume", "v-used", true},             // used by c-one
		{"volume", "v-plain", false},           // unprotected and unused
		{"network", "n-one", true},             // protected label
		{"network", "n-used", true},            // used by c-both
		{"network", "n-plain", false},          // unprotected and unused
	} {
		if got := exists(c.kind, c.name); got != c.want {
			t.Errorf("%s %s exists = %v, want %v", c.kind, c.name, got, c.want)
		}
	}
	_ = run
}
