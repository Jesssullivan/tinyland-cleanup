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
	"errors"
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
	// agentPrefixes match the top-level temp entries that are agent-session
	// trees; agentIdle is the idle age their roots need (review M1).
	agentPrefixes []string
	agentIdle     time.Duration
	liveCwds      func() ([]string, error)
	now           time.Time
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

		if tempDir, ok := nixTempDirOf(link, tempDirs); ok {
			age := r.now.Sub(info.ModTime())
			root := nixStaleGCRoot{AutoLink: autoLink, Link: link, StorePath: storePath, Class: nixStaleGCRootClassTemporary}
			threshold := tempStaleAfter
			agentTree := r.agentTreeOf(link, tempDir)
			if agentTree != "" && r.agentIdle > threshold {
				threshold = r.agentIdle
			}
			switch {
			case age < threshold:
				root.Reason = fmt.Sprintf("temporary out-link changed %s ago, inside %s", age.Round(time.Minute), threshold)
			case agentTree != "":
				if live, why := r.treeIsLive(agentTree); live {
					root.Reason = fmt.Sprintf("agent-session tree %s is live: %s", agentTree, why)
				} else {
					root.Stale = true
					root.Reason = fmt.Sprintf("agent-session out-link idle for %s (threshold %s) and no live process in %s", age.Round(time.Minute), threshold, agentTree)
				}
			default:
				root.Stale = true
				root.Reason = fmt.Sprintf("temporary out-link unchanged for %s (threshold %s)", age.Round(time.Minute), threshold)
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
		vcs, vcsErr := nixFindVCSRoot(checkout)
		age := r.now.Sub(newest)
		stale := false
		reason := ""
		// Review M2: a checkout is found by walking up for .git or .jj, a
		// stat error other than "does not exist" keeps the roots, and the
		// age rule applies whether or not a checkout is found.
		switch {
		case vcsErr != nil:
			reason = fmt.Sprintf("nix-direnv roots for %s kept: %v", checkout, vcsErr)
		case age < direnvStaleAfter:
			reason = fmt.Sprintf("nix-direnv roots for %s loaded %s ago, inside %s", checkout, age.Round(time.Minute), direnvStaleAfter)
		case vcs == "":
			stale = true
			reason = fmt.Sprintf("nix-direnv roots for %s (no .git or .jj above it) last loaded %s ago (threshold %s)", checkout, age.Round(time.Minute), direnvStaleAfter)
		default:
			stale = true
			reason = fmt.Sprintf("nix-direnv roots for %s (checkout %s) last loaded %s ago (threshold %s)", checkout, vcs, age.Round(time.Minute), direnvStaleAfter)
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

// nixFindVCSRoot walks up from dir for a .git or .jj entry. It returns the
// directory that has one, "" when none does, or an error when an entry
// cannot be checked for any reason other than not existing.
// nixLstat is os.Lstat; tests replace it to inject a stat error.
var nixLstat = os.Lstat

func nixFindVCSRoot(dir string) (string, error) {
	dir = filepath.Clean(dir)
	for {
		for _, marker := range []string{".git", ".jj"} {
			_, err := nixLstat(filepath.Join(dir, marker))
			if err == nil {
				return dir, nil
			}
			if !errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("cannot check %s: %w", filepath.Join(dir, marker), err)
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

// nixTempDirOf returns the temp dir a link sits under.
func nixTempDirOf(path string, dirs []string) (string, bool) {
	candidates := []string{path}
	if resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		candidates = append(candidates, filepath.Join(resolvedParent, filepath.Base(path)))
	}
	for _, candidate := range candidates {
		for _, dir := range dirs {
			if strings.HasPrefix(candidate, dir+string(filepath.Separator)) {
				return dir, true
			}
		}
	}
	return "", false
}

// agentTreeOf returns the agent-session tree a temp link sits in: the
// top-level entry under its temp dir when that entry matches an agent
// prefix (claude-*, codex*, .tmp*), else "".
func (r nixGCRootReaper) agentTreeOf(link, tempDir string) string {
	candidates := []string{link}
	if resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(link)); err == nil {
		candidates = append(candidates, filepath.Join(resolvedParent, filepath.Base(link)))
	}
	for _, candidate := range candidates {
		rel, err := filepath.Rel(tempDir, candidate)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		top := strings.Split(rel, string(filepath.Separator))[0]
		for _, pattern := range r.agentPrefixes {
			if ok, _ := filepath.Match(pattern, top); ok {
				return filepath.Join(tempDir, top)
			}
		}
	}
	return ""
}

// treeIsLive reports whether a live process of the daemon user works inside
// tree. Inspection failure counts as live.
func (r nixGCRootReaper) treeIsLive(tree string) (bool, string) {
	if r.liveCwds == nil {
		return true, "no process inspection"
	}
	cwds, err := r.liveCwds()
	if err != nil {
		return true, fmt.Sprintf("process inspection failed: %v", err)
	}
	for _, cwd := range cwds {
		if cwd == tree || strings.HasPrefix(cwd, tree+string(filepath.Separator)) {
			return true, "a live process has its working directory at " + cwd
		}
	}
	return false, ""
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
	roots, err := p.gcRootReaper(cfg).collect(nixStaleGCRootTempDirs(cfg), nixDirenvGCRootStaleAfter(cfg.Nix), nixTempGCRootStaleAfter(cfg.Nix))
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
	reaper := p.gcRootReaper(cfg)
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

func (p *NixPlugin) gcRootReaper(cfg *config.Config) nixGCRootReaper {
	reaper := newNixGCRootReaper(p.clock())
	reaper.agentPrefixes = cfg.Nix.AgentTreePrefixes
	reaper.agentIdle = parseNixPolicyDuration(cfg.Nix.AgentTreeIdleAfter, 7*24*time.Hour)
	okComms := cfg.DevArtifacts.TempUninspectableOKComms
	reaper.liveCwds = func() ([]string, error) { return nixLiveCwds(okComms) }
	if p.liveCwds != nil {
		reaper.liveCwds = p.liveCwds
	}
	if p.autoGCRootsDir != "" {
		reaper.autoDir = p.autoGCRootsDir
	}
	if p.storeDir != "" {
		reaper.storeDir = p.storeDir
	}
	return reaper
}
