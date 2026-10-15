// Package plugins provides cleanup plugin implementations.
// nix_gcroots.go reaps stale indirect Nix GC roots before a Nix GC pass.
//
// An indirect GC root is a symlink somewhere on disk (an out-link) that Nix
// registers under /nix/var/nix/gcroots/auto. Two classes accumulate on
// developer hosts and pin store paths nobody uses:
//
//   - nix-direnv roots: .direnv/flake-profile-* and .direnv/flake-inputs/*.
//     The inputs are flake sources. nix-direnv touches every one of them each
//     time it loads a cached shell, so the newest root link in a .direnv says
//     when the checkout was last entered.
//   - temporary out-links: `nix build` results and qualification roots left
//     under temporary directories.
//
// The reaper only removes symlinks the daemon user owns that point into the
// Nix store. It never removes a directory that holds anything but such links,
// and it never touches a root under a path component named "gcroots" (named,
// deliberate pins such as gcroots/<ticket>/...) or "profiles" (profile
// generations), or a rules_nixpkgs out-link inside a Bazel output base.
package plugins

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

const (
	nixDefaultAutoGCRootsDir          = "/nix/var/nix/gcroots/auto"
	nixDefaultStoreDir                = "/nix/store"
	nixDefaultDirenvRootStaleAfter    = 7 * 24 * time.Hour
	nixDefaultTempGCRootStaleAfter    = 72 * time.Hour
	nixStaleGCRootClassDirenv         = "direnv_root"
	nixStaleGCRootClassTemporary      = "temporary_root"
	nixStaleGCRootActionRemove        = "remove_stale_gc_root"
	nixStaleGCRootActionKeep          = "keep_gc_root"
	nixBazelRulesNixpkgsOutLinkSuffix = "/bazel-support/nix-out-link"
)

// nixStaleGCRoot is one indirect root the reaper classified.
type nixStaleGCRoot struct {
	// Link is the out-link path the auto root points at.
	Link string
	// StorePath is the store path the out-link selects.
	StorePath string
	// Class is direnv_root or temporary_root.
	Class string
	// Stale reports whether the reaper selected the link for removal.
	Stale bool
	// Reason explains the selection.
	Reason string
}

// nixGCRootReaper holds the filesystem seams the reaper uses.
type nixGCRootReaper struct {
	autoDir  string
	storeDir string
	uid      int
	now      time.Time
}

func newNixGCRootReaper(now time.Time) nixGCRootReaper {
	return nixGCRootReaper{
		autoDir:  nixDefaultAutoGCRootsDir,
		storeDir: nixDefaultStoreDir,
		uid:      os.Getuid(),
		now:      now,
	}
}

// nixStaleGCRootTempDirs returns the directories under which an out-link
// counts as temporary: nix.temp_gc_root_paths when set, otherwise
// dev_artifacts.temp_scan_paths. Each is returned as written and, when it
// resolves through a symlink, as its resolved path too.
func nixStaleGCRootTempDirs(cfg *config.Config) []string {
	raw := cfg.Nix.TempGCRootPaths
	if len(raw) == 0 {
		raw = cfg.DevArtifacts.TempScanPaths
	}
	home, _ := os.UserHomeDir()
	seen := map[string]bool{}
	var dirs []string
	add := func(path string) {
		path = filepath.Clean(path)
		if path == "/" || path == "." || seen[path] {
			return
		}
		seen[path] = true
		dirs = append(dirs, path)
	}
	for _, path := range raw {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		path = expandHome(path, home)
		if !filepath.IsAbs(path) {
			continue
		}
		add(path)
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			add(resolved)
		}
	}
	return dirs
}

// collect classifies every indirect root the daemon user owns. A missing or
// unreadable auto directory is an error: the caller skips reaping and still
// runs GC.
func (r nixGCRootReaper) collect(tempDirs []string, direnvStaleAfter, tempStaleAfter time.Duration) ([]nixStaleGCRoot, error) {
	entries, err := os.ReadDir(r.autoDir)
	if err != nil {
		return nil, err
	}

	type direnvLink struct {
		link      string
		storePath string
		modTime   time.Time
	}
	direnvGroups := map[string][]direnvLink{}
	var roots []nixStaleGCRoot
	seenLinks := map[string]bool{}

	for _, entry := range entries {
		autoLink := filepath.Join(r.autoDir, entry.Name())
		link, err := os.Readlink(autoLink)
		if err != nil || !filepath.IsAbs(link) {
			continue
		}
		link = filepath.Clean(link)
		if seenLinks[link] {
			continue
		}
		seenLinks[link] = true

		storePath, info, ok := r.ownedStoreLink(link)
		if !ok || nixGCRootIsProtected(link) {
			continue
		}

		if direnvDir, ok := nixDirenvRootDir(link); ok {
			direnvGroups[direnvDir] = append(direnvGroups[direnvDir], direnvLink{link: link, storePath: storePath, modTime: info.ModTime()})
			continue
		}

		if nixPathUnderAny(link, tempDirs) {
			age := r.now.Sub(info.ModTime())
			root := nixStaleGCRoot{Link: link, StorePath: storePath, Class: nixStaleGCRootClassTemporary}
			if age >= tempStaleAfter {
				root.Stale = true
				root.Reason = fmt.Sprintf("temporary out-link unchanged for %s (threshold %s)", age.Round(time.Minute), tempStaleAfter)
			} else {
				root.Reason = fmt.Sprintf("temporary out-link changed %s ago, inside %s", age.Round(time.Minute), tempStaleAfter)
			}
			roots = append(roots, root)
		}
	}

	direnvDirs := make([]string, 0, len(direnvGroups))
	for dir := range direnvGroups {
		direnvDirs = append(direnvDirs, dir)
	}
	sort.Strings(direnvDirs)
	for _, direnvDir := range direnvDirs {
		links := direnvGroups[direnvDir]
		newest := time.Time{}
		for _, l := range links {
			if l.modTime.After(newest) {
				newest = l.modTime
			}
		}
		checkout := filepath.Dir(direnvDir)
		live := pathExists(filepath.Join(checkout, ".git"))
		age := r.now.Sub(newest)
		stale := false
		reason := ""
		switch {
		case !live:
			stale = true
			reason = fmt.Sprintf("nix-direnv root for %s, which is not a git checkout", checkout)
		case age >= direnvStaleAfter:
			stale = true
			reason = fmt.Sprintf("nix-direnv roots for %s last loaded %s ago (threshold %s)", checkout, age.Round(time.Minute), direnvStaleAfter)
		default:
			reason = fmt.Sprintf("nix-direnv roots for %s loaded %s ago, inside %s", checkout, age.Round(time.Minute), direnvStaleAfter)
		}
		for _, l := range links {
			roots = append(roots, nixStaleGCRoot{Link: l.link, StorePath: l.storePath, Class: nixStaleGCRootClassDirenv, Stale: stale, Reason: reason})
		}
	}

	sort.SliceStable(roots, func(i, j int) bool { return roots[i].Link < roots[j].Link })
	return roots, nil
}

// ownedStoreLink reports the store path a link selects when the link is a
// symlink the daemon user owns that points directly into the Nix store.
func (r nixGCRootReaper) ownedStoreLink(link string) (string, os.FileInfo, bool) {
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", nil, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != r.uid {
		return "", nil, false
	}
	dest, err := os.Readlink(link)
	if err != nil {
		return "", nil, false
	}
	dest = filepath.Clean(dest)
	storeDir := filepath.Clean(r.storeDir)
	if filepath.Dir(dest) != storeDir {
		return "", nil, false
	}
	return dest, info, true
}

// remove deletes one selected link after re-checking that it is still an
// owned symlink into the store. It never removes anything else.
func (r nixGCRootReaper) remove(root nixStaleGCRoot) error {
	storePath, _, ok := r.ownedStoreLink(root.Link)
	if !ok || storePath != root.StorePath {
		return fmt.Errorf("gc root %s changed since classification", root.Link)
	}
	if err := os.Remove(root.Link); err != nil {
		return err
	}
	if root.Class == nixStaleGCRootClassDirenv {
		// An emptied flake-inputs directory goes too; a non-empty one stays.
		parent := filepath.Dir(root.Link)
		if filepath.Base(parent) == "flake-inputs" {
			_ = os.Remove(parent)
		}
	}
	return nil
}

// nixGCRootIsProtected reports roots the reaper never touches: anything under
// a "gcroots" or "profiles" path component, and rules_nixpkgs out-links in
// Bazel output bases (the bazel_bases lane owns those trees).
func nixGCRootIsProtected(link string) bool {
	if strings.HasSuffix(link, nixBazelRulesNixpkgsOutLinkSuffix) {
		return true
	}
	for _, part := range strings.Split(filepath.Dir(link), string(filepath.Separator)) {
		switch part {
		case "gcroots", "profiles":
			return true
		}
	}
	return false
}

// nixDirenvRootDir returns the .direnv directory that holds a nix-direnv root:
// .direnv/flake-profile-*, .direnv/nix-profile-* or .direnv/flake-inputs/*.
func nixDirenvRootDir(link string) (string, bool) {
	parent := filepath.Dir(link)
	base := filepath.Base(link)
	if filepath.Base(parent) == ".direnv" && (strings.HasPrefix(base, "flake-profile-") || strings.HasPrefix(base, "nix-profile-")) {
		if strings.HasSuffix(base, ".rc") {
			return "", false
		}
		return parent, true
	}
	if filepath.Base(parent) == "flake-inputs" && filepath.Base(filepath.Dir(parent)) == ".direnv" {
		return filepath.Dir(parent), true
	}
	return "", false
}

func nixPathUnderAny(path string, dirs []string) bool {
	candidates := []string{path}
	if resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		candidates = append(candidates, filepath.Join(resolvedParent, filepath.Base(path)))
	}
	for _, candidate := range candidates {
		for _, dir := range dirs {
			if strings.HasPrefix(candidate, dir+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

func nixDirenvGCRootStaleAfter(cfg config.NixConfig) time.Duration {
	return parseNixPolicyDuration(cfg.DirenvGCRootStaleAfter, nixDefaultDirenvRootStaleAfter)
}

func nixTempGCRootStaleAfter(cfg config.NixConfig) time.Duration {
	return parseNixPolicyDuration(cfg.TempGCRootStaleAfter, nixDefaultTempGCRootStaleAfter)
}

func nixStaleGCRootTargets(roots []nixStaleGCRoot) []CleanupTarget {
	targets := make([]CleanupTarget, 0, len(roots))
	for _, root := range roots {
		action := nixStaleGCRootActionKeep
		reclaim := CleanupReclaimNone
		if root.Stale {
			action = nixStaleGCRootActionRemove
			reclaim = CleanupReclaimDeferred
		}
		target := CleanupTarget{
			Type:      "nix_gc_root",
			Name:      fmt.Sprintf("%s %s", root.Class, filepath.Base(root.Link)),
			Version:   filepath.Base(root.StorePath),
			Path:      root.Link,
			Protected: !root.Stale,
			Action:    action,
			Reason:    root.Reason,
		}
		annotateCleanupTargetPolicy(&target, CleanupTierWarm, reclaim)
		targets = append(targets, target)
	}
	return targets
}

// planStaleGCRoots is the non-mutating half: it classifies roots for a plan.
func (p *NixPlugin) planStaleGCRoots(cfg *config.Config) ([]CleanupTarget, map[string]string, []string) {
	metadata := map[string]string{"stale_gc_root_reaping": strconv.FormatBool(cfg.Nix.ReapStaleGCRoots)}
	if !cfg.Nix.ReapStaleGCRoots {
		return nil, metadata, nil
	}
	roots, err := p.gcRootReaper().collect(nixStaleGCRootTempDirs(cfg), nixDirenvGCRootStaleAfter(cfg.Nix), nixTempGCRootStaleAfter(cfg.Nix))
	if err != nil {
		return nil, metadata, []string{fmt.Sprintf("could not read indirect Nix GC roots: %v", err)}
	}
	stale := 0
	for _, root := range roots {
		if root.Stale {
			stale++
		}
	}
	metadata["stale_gc_roots"] = strconv.Itoa(stale)
	metadata["classified_gc_roots"] = strconv.Itoa(len(roots))
	return nixStaleGCRootTargets(roots), metadata, nil
}

// reapStaleGCRoots removes the stale roots before GC and returns how many it
// removed. Failure to read the auto directory skips reaping; GC still runs.
func (p *NixPlugin) reapStaleGCRoots(cfg *config.Config, logger *slog.Logger) int {
	if !cfg.Nix.ReapStaleGCRoots {
		return 0
	}
	reaper := p.gcRootReaper()
	roots, err := reaper.collect(nixStaleGCRootTempDirs(cfg), nixDirenvGCRootStaleAfter(cfg.Nix), nixTempGCRootStaleAfter(cfg.Nix))
	if err != nil {
		logger.Warn("skipping stale Nix GC root reaping; indirect roots are unreadable", "dir", reaper.autoDir, "error", err)
		return 0
	}
	removed := 0
	for _, root := range roots {
		if !root.Stale {
			continue
		}
		if err := reaper.remove(root); err != nil {
			logger.Warn("kept Nix GC root", "link", root.Link, "error", err)
			continue
		}
		removed++
		logger.Info("removed stale Nix GC root", "class", root.Class, "link", root.Link, "store_path", root.StorePath, "reason", root.Reason)
	}
	return removed
}

func (p *NixPlugin) gcRootReaper() nixGCRootReaper {
	now := time.Now()
	if p.now != nil {
		now = p.now()
	}
	reaper := newNixGCRootReaper(now)
	if p.autoGCRootsDir != "" {
		reaper.autoDir = p.autoGCRootsDir
	}
	if p.storeDir != "" {
		reaper.storeDir = p.storeDir
	}
	return reaper
}
