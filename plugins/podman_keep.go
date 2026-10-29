package plugins

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// podmanKeep is the Podman keep rule (TIN-5770). On sting the plugin pruned a
// mail-lacein harness run's pinned rspamd and postfix images, networks and
// volumes between runs. Three things are never pruned:
//
//  1. an image, network or volume that any container uses, in any state.
//     Podman's own prunes already skip those; the plugin keeps it true by
//     pruning volumes and networks before it prunes stopped containers, and
//     by no longer running `system prune` at the aggressive level;
//  2. anything carrying one of podman.protected_labels;
//  3. the images named in podman.protected_images or, one per line, in
//     podman.protected_images_file.
type podmanKeep struct {
	Refs     []string
	Labels   []string
	File     string
	FileErr  error
	FileRefs int
}

// loadPodmanKeep reads the keep rule for one cycle. The keep file is re-read
// every time so a harness can add a pin without a config change.
func loadPodmanKeep(cfg config.PodmanConfig, home string) podmanKeep {
	keep := podmanKeep{File: strings.TrimSpace(cfg.ProtectedImagesFile)}
	for _, ref := range cfg.ProtectedImages {
		if ref = strings.TrimSpace(ref); ref != "" {
			keep.Refs = append(keep.Refs, ref)
		}
	}
	for _, label := range cfg.ProtectedLabels {
		if label = strings.TrimSpace(label); label != "" {
			keep.Labels = append(keep.Labels, label)
		}
	}
	if keep.File != "" {
		keep.File = expandHome(keep.File, home)
		refs, err := readPodmanKeepFile(keep.File)
		keep.FileErr = err
		keep.FileRefs = len(refs)
		keep.Refs = append(keep.Refs, refs...)
	}
	return keep
}

// readPodmanKeepFile returns the image references in path, one per line.
// Blank lines and text after '#' are ignored. A missing file holds no pins.
// A dangling symlink, or anything but a regular file (a FIFO would block the
// daemon on open), is an error, so the caller fails closed.
func readPodmanKeepFile(path string) ([]string, error) {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("keep file %s: %w", path, err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("keep file %s is not a regular file (%s)", path, st.Mode().Type())
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("keep file %s changed type while opening", path)
	}
	var refs []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		if line = strings.TrimSpace(line); line != "" {
			refs = append(refs, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return refs, nil
}

// active reports whether the keep rule is on. A configured keep file counts
// even when it is missing or empty, so a harness that has not written its
// pins yet still keeps the critical system prune off.
func (k podmanKeep) active() bool {
	return len(k.Refs) > 0 || len(k.Labels) > 0 || k.File != "" || k.FileErr != nil
}

// labelFilterArgs turns each protected label into a prune exclusion filter.
func (k podmanKeep) labelFilterArgs() []string {
	var args []string
	for _, label := range k.Labels {
		args = append(args, "--filter", "label!="+label)
	}
	return args
}

// protectsLabels reports whether a resource with these labels is kept. An
// entry "key" matches the key with any value; "key=value" matches the pair.
func (k podmanKeep) protectsLabels(labels map[string]string) bool {
	for _, want := range k.Labels {
		key, value, pair := strings.Cut(want, "=")
		got, ok := labels[key]
		if ok && (!pair || got == value) {
			return true
		}
	}
	return false
}

func podmanHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// podmanContainerPruneAge matches the `container prune --filter until=1h`
// the plugin runs when no label is protected.
const podmanContainerPruneAge = time.Hour

// podmanImageNotKnown reports whether podman's error output says the image
// does not exist, the only inspect failure that means a pin is absent.
func podmanImageNotKnown(output string) bool {
	out := strings.ToLower(output)
	return strings.Contains(out, "image not known") || strings.Contains(out, "no such image")
}

// podmanContainer is the subset of `podman ps -a --format json` the keep
// rule reads.
type podmanContainer struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	State   string            `json:"State"`
	Created int64             `json:"Created"`
	Labels  map[string]string `json:"Labels"`
	Pod     string            `json:"Pod"`
	IsInfra bool              `json:"IsInfra"`
}

// podmanStoppedStates are the states `container prune` removes.
var podmanStoppedStates = map[string]bool{"exited": true, "created": true, "stopped": true, "configured": true}

// selectStoppedContainers returns the stopped containers created before
// cutoff that carry no protected label and belong to no pod.
func selectStoppedContainers(containers []podmanContainer, keep podmanKeep, cutoff time.Time) []podmanContainer {
	var out []podmanContainer
	for _, c := range containers {
		if c.ID == "" || !podmanStoppedStates[strings.ToLower(c.State)] || c.Pod != "" || c.IsInfra {
			continue
		}
		if c.Created <= 0 || !time.Unix(c.Created, 0).Before(cutoff) {
			continue
		}
		if keep.protectsLabels(c.Labels) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// removeStoppedContainers removes each unprotected stopped container older
// than age with `podman rm` (no --force: a container that started since the
// listing is refused). It returns the number removed.
func (p *PodmanPlugin) removeStoppedContainers(ctx context.Context, keep podmanKeep, age time.Duration, now time.Time, logger *slog.Logger) int {
	output, err := p.runPodmanCommand(ctx, "ps", "-a", "--format", "json")
	if err != nil {
		logger.Warn("failed to list podman containers for the keep-rule prune", "error", err)
		return 0
	}
	var containers []podmanContainer
	if trimmed := strings.TrimSpace(output); trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal([]byte(trimmed), &containers); err != nil {
			logger.Warn("failed to parse podman containers for the keep-rule prune", "error", err)
			return 0
		}
	}
	removed := 0
	for _, c := range selectStoppedContainers(containers, keep, now.Add(-age)) {
		if _, err := p.runPodmanCommand(ctx, "rm", c.ID); err != nil {
			logger.Debug("podman container removal skipped", "container", c.ID, "error", err)
			continue
		}
		removed++
	}
	return removed
}

// podmanVolume is the subset of `podman volume ls --format json` the keep
// rule reads.
type podmanVolume struct {
	Name   string            `json:"Name"`
	Labels map[string]string `json:"Labels"`
}

// removeUnusedVolumes removes each volume no container uses (podman's
// dangling filter counts containers in every state) that carries no
// protected label, with `podman volume rm` and no --force, so a volume a
// container claimed since the listing is refused.
func (p *PodmanPlugin) removeUnusedVolumes(ctx context.Context, keep podmanKeep, logger *slog.Logger) int {
	output, err := p.runPodmanCommand(ctx, "volume", "ls", "--format", "json", "--filter", "dangling=true")
	if err != nil {
		logger.Warn("failed to list podman volumes for the keep-rule prune", "error", err)
		return 0
	}
	var volumes []podmanVolume
	if trimmed := strings.TrimSpace(output); trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal([]byte(trimmed), &volumes); err != nil {
			logger.Warn("failed to parse podman volumes for the keep-rule prune", "error", err)
			return 0
		}
	}
	removed := 0
	for _, v := range volumes {
		if v.Name == "" || keep.protectsLabels(v.Labels) {
			continue
		}
		if _, err := p.runPodmanCommand(ctx, "volume", "rm", v.Name); err != nil {
			logger.Debug("podman volume removal skipped", "volume", v.Name, "error", err)
			continue
		}
		removed++
	}
	return removed
}
