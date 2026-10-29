package plugins

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"strings"

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
func readPodmanKeepFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
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

// active reports whether the age prune must take the per-image path.
func (k podmanKeep) active() bool {
	return len(k.Refs) > 0 || len(k.Labels) > 0 || k.FileErr != nil
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
