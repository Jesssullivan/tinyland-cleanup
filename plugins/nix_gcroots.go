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
// The reaper never deletes, moves or rewrites a user's file, and it holds no
// privilege. It publishes each stale root as a candidate: one line per root,
// "<auto entry name>\t<out-link path>", in nix.gc_root_candidates_file, a
// file the daemon user owns. A separate root job on the host (lab:
// roles/host-nix-store-gc, R-C780) reads that list, re-checks every entry and
// removes only the matching /nix/var/nix/gcroots/auto/<name> entries, the
// indirection the collector follows. The out-link itself stays where it is,
// so agent-session trees (TIN-5268) and .direnv directories keep every file;
// the link dangles once the collector has taken the store path, and
// nix-direnv treats a dangling profile as an invalid cache. Without a
// candidates file the reaper only reports.
//
// Only out-links the daemon user owns that select a store path are
// considered, and never one under a path component named "gcroots" (named,
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
	nixStaleGCRootActionRemove        = "propose_gc_root_unregister"
	nixGCRootCandidatesHeader         = "# tinyland-cleanup gc root candidates v1: <auto entry name>\t<out-link>"
	nixStaleGCRootActionKeep          = "keep_gc_root"
	nixBazelRulesNixpkgsOutLinkSuffix = "/bazel-support/nix-out-link"
)

// nixStaleGCRoot is one indirect root the reaper classified.
type nixStaleGCRoot struct {
	// AutoLink is the /nix/var/nix/gcroots/auto entry that registers Link.
	AutoLink string
	// Link is the out-link path the auto root points at. It is never touched.
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
		autoLink  string
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
			direnvGroups[direnvDir] = append(direnvGroups[direnvDir], direnvLink{autoLink: autoLink, link: link, storePath: storePath, modTime: info.ModTime()})
			continue
		}

		if nixPathUnderAny(link, tempDirs) {
			age := r.now.Sub(info.ModTime())
			root := nixStaleGCRoot{AutoLink: autoLink, Link: link, StorePath: storePath, Class: nixStaleGCRootClassTemporary}
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
			roots = append(roots, nixStaleGCRoot{AutoLink: l.autoLink, Link: l.link, StorePath: l.storePath, Class: nixStaleGCRootClassDirenv, Stale: stale, Reason: reason})
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

// writeCandidates replaces the candidates file with the stale roots, one
// "<auto entry name>\t<out-link>" line each, atomically and mode 0600. It
// changes nothing else.
func writeNixGCRootCandidates(path string, roots []nixStaleGCRoot) (int, error) {
	var b strings.Builder
	b.WriteString(nixGCRootCandidatesHeader)
	b.WriteString("\n")
	n := 0
	for _, root := range roots {
		if !root.Stale {
			continue
		}
		name := filepath.Base(root.AutoLink)
		if strings.ContainsAny(name, "\t\n") || strings.ContainsAny(root.Link, "\t\n") {
			continue
		}
		b.WriteString(name)
		b.WriteString("\t")
		b.WriteString(root.Link)
		b.WriteString("\n")
		n++
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(dir, ".gcroot-candidates-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return 0, err
	}
	return n, nil
}

func nixGCRootCandidatesPath(cfg config.NixConfig) string {
	path := strings.TrimSpace(cfg.GCRootCandidatesFile)
	if path == "" {
		return ""
	}
	home, _ := os.UserHomeDir()
	path = filepath.Clean(expandHome(path, home))
	if !filepath.IsAbs(path) {
		return ""
	}
	return path
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
	metadata["gc_root_candidates_file"] = nixGCRootCandidatesPath(cfg.Nix)
	metadata["classified_gc_roots"] = strconv.Itoa(len(roots))
	return nixStaleGCRootTargets(roots), metadata, nil
}

// publishStaleGCRootCandidates writes the stale roots to the candidates file
// for the host's root unregister job and returns how many it listed. It
// unregisters nothing itself, so it is not cleanup work and does not force a
// GC this cycle. Failure to read the auto directory skips it; GC still runs.
// Without a candidates file it only reports.
func (p *NixPlugin) publishStaleGCRootCandidates(cfg *config.Config, logger *slog.Logger) int {
	if !cfg.Nix.ReapStaleGCRoots {
		return 0
	}
	reaper := p.gcRootReaper()
	roots, err := reaper.collect(nixStaleGCRootTempDirs(cfg), nixDirenvGCRootStaleAfter(cfg.Nix), nixTempGCRootStaleAfter(cfg.Nix))
	if err != nil {
		logger.Warn("skipping stale Nix GC root candidates; indirect roots are unreadable", "dir", reaper.autoDir, "error", err)
		return 0
	}
	path := nixGCRootCandidatesPath(cfg.Nix)
	if path == "" {
		for _, root := range roots {
			if root.Stale {
				logger.Info("stale Nix GC root left registered; no gc_root_candidates_file", "class", root.Class, "link", root.Link, "reason", root.Reason)
			}
		}
		return 0
	}
	n, err := writeNixGCRootCandidates(path, roots)
	if err != nil {
		logger.Warn("could not write stale Nix GC root candidates", "path", path, "error", err)
		return 0
	}
	if n > 0 {
		logger.Info("listed stale Nix GC roots for the root unregister job; out-links untouched", "path", path, "candidates", n)
	}
	return n
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
