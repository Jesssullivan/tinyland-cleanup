package plugins

// bazel_repo_contents.go measures Bazel's repository contents cache by entry
// and removes unreferenced duplicate copies from it (TIN-5770).
//
// The cache keeps each fetched repository at
// <repository_cache>/contents/<key>/<uuid>/ next to a file
// <uuid>.recorded_inputs. Bazel can write a new UUID copy of the same key on
// every refetch. On honey on 2026-11-14 one key held 287 copies (98 GB),
// while the plugin measured the whole tier as 96 KiB because it read only
// the top level of cache/repos/v1.
//
// A copy is kept when any output base's external/<repo> symlink resolves to
// it, when it is one of the newest repo_contents_keep_recent unreferenced
// copies of its key, when it was used within repo_contents_min_age, when it
// belongs to another user, or when a process holds or names anything under
// it. Every other copy is removed, also while a Bazel server is running:
// the symlink check protects the copies a server uses. Any doubt about the
// symlink scan or the process inventory keeps every copy.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

const (
	bazelRepoContentsType     = "repo_contents_duplicates"
	bazelRepoContentsDelete   = "delete_repo_contents_duplicates"
	bazelRepoContentsReport   = "report_repo_contents_duplicates"
	bazelRepoContentsRecorded = ".recorded_inputs"
)

var bazelRepoContentsUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// bazelRepoContentsKeyName matches a contents key: Bazel names each key
// directory by a hex digest. Anything else (Bazel's _trash/, gc_lock) is
// measured but never listed as a key, so the dedupe never touches it.
var bazelRepoContentsKeyName = regexp.MustCompile(`^[0-9a-fA-F]{32,128}$`)

// errBazelGCLockHeld reports that Bazel's contents/gc_lock is held: a Bazel
// command is using the repository contents cache.
var errBazelGCLockHeld = errors.New("bazel active (gc_lock)")

const (
	// bazelRepoContentsGCLock is Bazel's lock file in contents/
	// (LocalRepoContentsCache.LOCK_PATH).
	bazelRepoContentsGCLock = "gc_lock"
	// bazelRepoContentsProbeTimeout bounds the apply-time size walk of a
	// copy the plan did not measure.
	bazelRepoContentsProbeTimeout = 10 * time.Second
)

// bazelRepoContentsCopy is one <uuid>/ copy of a contents key.
type bazelRepoContentsCopy struct {
	UUID    string
	Dir     string
	LastUse time.Time
	// Bytes is the allocated size of the copy and its .recorded_inputs.
	Bytes    int64
	Logical  int64
	Measured bool
	Foreign  bool
	// Keep is the reason the copy stays; empty means remove.
	Keep string
}

// bazelRepoContentsKey is one contents/<key>/ directory, copies newest first.
type bazelRepoContentsKey struct {
	Key    string
	Dir    string
	Copies []bazelRepoContentsCopy
}

// bazelRepoCacheMeasure is one bounded measurement of a repository cache.
type bazelRepoCacheMeasure struct {
	Root        string
	ContentsDir string
	Keys        []bazelRepoContentsKey
	Bytes       int64
	Logical     int64
	// Estimated counts copies whose size was taken from their key's mean
	// because the time bound ran out before they were walked.
	Estimated int
	// Partial is set when the time bound cut the measurement short.
	Partial bool
	// Unreadable lists the contents/ or key directories that could not be
	// read, so copies in them are unseen.
	Unreadable []string
}

// bazelRepoContentsPolicy is the resolved dedupe policy.
type bazelRepoContentsPolicy struct {
	enabled bool
	minAge  time.Duration
	keep    int
}

func bazelRepoContentsPolicyFor(cfg config.BazelConfig) bazelRepoContentsPolicy {
	pol := bazelRepoContentsPolicy{enabled: cfg.RepoContentsDedupe, minAge: 72 * time.Hour, keep: cfg.RepoContentsKeepRecent}
	if cfg.RepoContentsMinAge != "" {
		if d, err := config.ParseDuration(cfg.RepoContentsMinAge); err == nil {
			pol.minAge = d
		}
	}
	if pol.minAge < config.BazelRepoContentsMinMinAge {
		pol.minAge = config.BazelRepoContentsMinMinAge
	}
	if pol.keep < 1 {
		pol.keep = 1
	}
	return pol
}

// bazelRepoContentsDir returns <root>/contents when it is a directory.
func bazelRepoContentsDir(root string) string {
	dir := filepath.Join(root, "contents")
	if info, err := os.Lstat(dir); err == nil && info.IsDir() {
		return dir
	}
	return ""
}

// listBazelRepoContents lists every key of a contents directory and its UUID
// copies, newest first. It stats only the top two levels.
func listBazelRepoContents(contentsDir string, now time.Time) []bazelRepoContentsKey {
	l := listBazelRepoContentsFull(contentsDir, now)
	return l.keys
}

// bazelRepoContentsListing is one listing of a contents directory.
type bazelRepoContentsListing struct {
	keys []bazelRepoContentsKey
	// others are contents/ entries that are not keys (_trash/, gc_lock).
	others []string
	// unreadable are directories the listing could not read.
	unreadable []string
}

func listBazelRepoContentsFull(contentsDir string, now time.Time) bazelRepoContentsListing {
	var l bazelRepoContentsListing
	entries, err := os.ReadDir(contentsDir)
	if err != nil {
		l.unreadable = append(l.unreadable, fmt.Sprintf("%s: %v", contentsDir, err))
		return l
	}
	uid := os.Geteuid()
	for _, e := range entries {
		keyDir := filepath.Join(contentsDir, e.Name())
		if !e.IsDir() || !bazelRepoContentsKeyName.MatchString(e.Name()) {
			l.others = append(l.others, keyDir)
			continue
		}
		children, err := os.ReadDir(keyDir)
		if err != nil {
			l.unreadable = append(l.unreadable, fmt.Sprintf("%s: %v", keyDir, err))
			continue
		}
		key := bazelRepoContentsKey{Key: e.Name(), Dir: keyDir}
		for _, c := range children {
			if !c.IsDir() || !bazelRepoContentsUUID.MatchString(c.Name()) {
				continue
			}
			dir := filepath.Join(keyDir, c.Name())
			info, err := os.Lstat(dir)
			if err != nil || !info.IsDir() {
				continue
			}
			cp := bazelRepoContentsCopy{UUID: c.Name(), Dir: dir, LastUse: effectiveModTime(info, now)}
			if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != uid {
				cp.Foreign = true
			}
			// Bazel touches <uuid>.recorded_inputs when it reuses a copy.
			if rec, err := os.Lstat(dir + bazelRepoContentsRecorded); err == nil {
				if t := effectiveModTime(rec, now); t.After(cp.LastUse) {
					cp.LastUse = t
				}
			}
			key.Copies = append(key.Copies, cp)
		}
		sort.SliceStable(key.Copies, func(i, j int) bool {
			if key.Copies[i].LastUse.Equal(key.Copies[j].LastUse) {
				return key.Copies[i].UUID < key.Copies[j].UUID
			}
			return key.Copies[i].LastUse.After(key.Copies[j].LastUse)
		})
		l.keys = append(l.keys, key)
	}
	sort.Slice(l.keys, func(i, j int) bool { return l.keys[i].Key < l.keys[j].Key })
	return l
}

// bazelRepoContentsCopyLastUse re-reads a copy's last use: the later of the
// directory mtime and its .recorded_inputs mtime.
func bazelRepoContentsCopyLastUse(dir string, now time.Time) (time.Time, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return time.Time{}, err
	}
	if !info.IsDir() {
		return time.Time{}, fmt.Errorf("%s is not a directory", dir)
	}
	last := effectiveModTime(info, now)
	if rec, err := os.Lstat(dir + bazelRepoContentsRecorded); err == nil {
		if t := effectiveModTime(rec, now); t.After(last) {
			last = t
		}
	} else if !os.IsNotExist(err) {
		return time.Time{}, err
	}
	return last, nil
}

// bazelAllocatedTree is a du-style walk: allocated bytes of every entry below
// root on root's file system, never following a symlink, each hard-linked
// inode counted once across one measurement (seen).
func bazelAllocatedTree(ctx context.Context, root string, seen map[[2]uint64]bool) (int64, int64, error) {
	var logical, physical int64
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return 0, 0, err
	}
	var rootDev uint64
	if st, ok := rootInfo.Sys().(*syscall.Stat_t); ok {
		rootDev = uint64(st.Dev)
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			logical += info.Size()
			physical += info.Size()
			return nil
		}
		if uint64(st.Dev) != rootDev {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() && uint64(st.Nlink) > 1 {
			id := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
			if seen[id] {
				return nil
			}
			seen[id] = true
		}
		logical += info.Size()
		physical += int64(st.Blocks) * 512
		return nil
	})
	return logical, physical, err
}

// measureBazelRepoCache measures a repository cache by entry within timeout.
// It walks the newest copy of every key first, then the other copies, then
// the rest of the cache. A copy the bound left unwalked takes the mean size
// of its key's walked copies, because copies of one key hold the same tree.
func measureBazelRepoCache(ctx context.Context, root string, now time.Time, timeout time.Duration) bazelRepoCacheMeasure {
	m := bazelRepoCacheMeasure{Root: root}
	var others []string
	mctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	seen := map[[2]uint64]bool{}
	walk := func(path string) (int64, int64, bool) {
		if mctx.Err() != nil {
			return 0, 0, false
		}
		l, p, err := bazelAllocatedTree(mctx, path, seen)
		if err != nil && mctx.Err() != nil {
			return l, p, false
		}
		return l, p, true
	}

	if contents := bazelRepoContentsDir(root); contents != "" {
		m.ContentsDir = contents
		listing := listBazelRepoContentsFull(contents, now)
		m.Keys = listing.keys
		m.Unreadable = listing.unreadable
		others = listing.others
		for pass := 0; pass < 2; pass++ {
			for k := range m.Keys {
				for c := range m.Keys[k].Copies {
					if (pass == 0) != (c == 0) {
						continue
					}
					cp := &m.Keys[k].Copies[c]
					l, p, ok := walk(cp.Dir)
					if !ok {
						m.Partial = true
						continue
					}
					if rec, err := os.Lstat(cp.Dir + bazelRepoContentsRecorded); err == nil {
						l += rec.Size()
						if st, ok := rec.Sys().(*syscall.Stat_t); ok {
							p += int64(st.Blocks) * 512
						}
					}
					cp.Logical, cp.Bytes, cp.Measured = l, p, true
				}
			}
		}
		for k := range m.Keys {
			key := &m.Keys[k]
			var n, sum, lsum int64
			for _, cp := range key.Copies {
				if cp.Measured {
					n++
					sum += cp.Bytes
					lsum += cp.Logical
				}
			}
			for c := range key.Copies {
				cp := &key.Copies[c]
				if !cp.Measured {
					m.Estimated++
					if n > 0 {
						cp.Bytes, cp.Logical = sum/n, lsum/n
					}
				}
				m.Bytes += cp.Bytes
				m.Logical += cp.Logical
			}
		}
	}

	for _, other := range others {
		l, p, ok := walk(other)
		if !ok {
			m.Partial = true
		}
		m.Logical += l
		m.Bytes += p
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return m
	}
	if info, err := os.Lstat(root); err == nil {
		m.Logical += info.Size()
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			m.Bytes += int64(st.Blocks) * 512
		}
	}
	for _, e := range entries {
		path := filepath.Join(root, e.Name())
		if path == m.ContentsDir {
			continue
		}
		l, p, ok := walk(path)
		if !ok {
			m.Partial = true
		}
		m.Logical += l
		m.Bytes += p
	}
	return m
}

// measureBazelRepoCacheCandidates replaces each repository_cache candidate's
// top-level estimate with a bounded per-entry measurement.
func measureBazelRepoCacheCandidates(ctx context.Context, candidates []bazelCandidate, now time.Time) []bazelRepoCacheMeasure {
	var out []bazelRepoCacheMeasure
	for i := range candidates {
		if candidates[i].Type != "repository_cache" {
			continue
		}
		m := measureBazelRepoCache(ctx, candidates[i].Path, now, bazelReclaimEstimateTimeout)
		if m.Bytes > candidates[i].Physical {
			candidates[i].Physical = m.Bytes
		}
		if m.Logical > candidates[i].Logical {
			candidates[i].Logical = m.Logical
		}
		out = append(out, m)
	}
	return out
}

// bazelRepoContentsRefs indexes the external/ symlink targets of output bases.
type bazelRepoContentsRefs struct {
	// held maps a target path and each of its ancestors to the link naming it.
	held        map[string]string
	outputBases int
	links       int
	incomplete  []string
}

func (r *bazelRepoContentsRefs) add(target, why string) {
	for _, t := range uniquePaths(filepath.Clean(target), resolvedOrSelf(target)) {
		for p := t; p != "/" && p != "."; p = filepath.Dir(p) {
			if _, ok := r.held[p]; ok {
				break
			}
			r.held[p] = why
		}
	}
}

// referencedBy reports the link that resolves to dir or below it.
func (r *bazelRepoContentsRefs) referencedBy(dir string) (string, bool) {
	for _, d := range uniquePaths(filepath.Clean(dir), resolvedOrSelf(dir)) {
		if why, ok := r.held[d]; ok {
			return why, true
		}
	}
	return "", false
}

// collectBazelRepoContentsRefs reads every <output base>/external/* symlink.
// A directory or link it cannot read makes the scan incomplete.
func collectBazelRepoContentsRefs(outputBases []string) *bazelRepoContentsRefs {
	r := &bazelRepoContentsRefs{held: map[string]string{}}
	for _, ob := range outputBases {
		ext := filepath.Join(ob, "external")
		// A relative link resolves against the real directory, as the kernel
		// does, when external/ is itself a symlink.
		realExt := ext
		if r, err := filepath.EvalSymlinks(ext); err == nil {
			realExt = r
		}
		entries, err := os.ReadDir(ext)
		if err != nil {
			if !os.IsNotExist(err) {
				r.incomplete = append(r.incomplete, fmt.Sprintf("cannot read %s: %v", ext, err))
			}
			continue
		}
		r.outputBases++
		for _, e := range entries {
			if e.Type()&os.ModeSymlink == 0 {
				continue
			}
			link := filepath.Join(ext, e.Name())
			target, err := os.Readlink(link)
			if err != nil {
				r.incomplete = append(r.incomplete, fmt.Sprintf("cannot read link %s: %v", link, err))
				continue
			}
			r.links++
			if !filepath.IsAbs(target) {
				if realExt != ext {
					r.add(filepath.Join(realExt, target), link)
				}
				target = filepath.Join(ext, target)
			}
			r.add(target, link)
		}
	}
	return r
}

// bazelRepoContentsOutputBases lists the output bases whose external/ links
// are read: every discovered output base, plus a shallow scan of the bazel
// and bazel_bases roots (the root, its children, and the children of each
// _bazel_* child) for directories that hold an external/ directory.
func bazelRepoContentsOutputBases(candidates []bazelCandidate, roots []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		p = filepath.Clean(p)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, c := range candidates {
		if bazelCandidateIsOutputBaseLike(c.Type) {
			add(c.Path)
		}
	}
	hasExternal := func(p string) bool { return pathExistsAndIsDir(filepath.Join(p, "external")) }
	for _, root := range roots {
		if root == "" {
			continue
		}
		if hasExternal(root) {
			add(root)
		}
		children, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, c := range children {
			child := filepath.Join(root, c.Name())
			if !c.IsDir() {
				target, ok := bazelOutputUserRootLinkTarget(root, c)
				if !ok {
					continue
				}
				child = target
			} else if hasExternal(child) {
				add(child)
			}
			if !strings.HasPrefix(c.Name(), "_bazel_") {
				continue
			}
			grand, err := os.ReadDir(child)
			if err != nil {
				continue
			}
			for _, g := range grand {
				if g.IsDir() && hasExternal(filepath.Join(child, g.Name())) {
					add(filepath.Join(child, g.Name()))
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// bazelRepoContentsRoots is every root the output-base scan covers.
func bazelRepoContentsRoots(cfg *config.Config, home string) []string {
	name := os.Getenv("USER")
	var roots []string
	if d := bazelDefaultOutputRoot(home); d != "" {
		roots = append(roots, d)
	}
	for _, r := range cfg.Bazel.Roots {
		roots = append(roots, expandHome(r, home))
	}
	if cfg.Enable.BazelBases {
		for _, r := range cfg.BazelBases.Roots {
			roots = append(roots, config.ExpandUserPath(r, home, name))
		}
	}
	return roots
}

// decideBazelRepoContentsKey sets Keep on every copy of key and returns the
// copies to remove. activity may be nil when no inventory was taken.
func decideBazelRepoContentsKey(key *bazelRepoContentsKey, refs *bazelRepoContentsRefs, activity *bbActivity, pol bazelRepoContentsPolicy, now time.Time) []bazelRepoContentsCopy {
	var remove []bazelRepoContentsCopy
	unreferenced := 0
	for i := range key.Copies {
		cp := &key.Copies[i]
		cp.Keep = ""
		if cp.Foreign {
			cp.Keep = "owned by another user"
			continue
		}
		if why, ok := refs.referencedBy(cp.Dir); ok {
			cp.Keep = "referenced by " + why
			continue
		}
		unreferenced++
		switch {
		case unreferenced <= pol.keep:
			cp.Keep = fmt.Sprintf("within the newest %d unreferenced copies", pol.keep)
		case now.Sub(cp.LastUse) < pol.minAge:
			cp.Keep = fmt.Sprintf("used within %s", pol.minAge)
		}
		if cp.Keep != "" {
			continue
		}
		if activity != nil {
			if b := activity.blockers(cp.Dir); len(b) > 0 {
				cp.Keep = "open: " + b[0]
				continue
			}
		}
		remove = append(remove, *cp)
	}
	return remove
}

// bazelRepoContentsSummary counts a decided key's copies by reason.
func bazelRepoContentsSummary(key bazelRepoContentsKey, remove []bazelRepoContentsCopy) (string, int64) {
	var referenced, newest, recent, open, foreign int
	for _, cp := range key.Copies {
		switch {
		case strings.HasPrefix(cp.Keep, "referenced"):
			referenced++
		case strings.HasPrefix(cp.Keep, "within the newest"):
			newest++
		case strings.HasPrefix(cp.Keep, "used within"):
			recent++
		case strings.HasPrefix(cp.Keep, "open"):
			open++
		case cp.Keep != "":
			foreign++
		}
	}
	var reclaim int64
	for _, cp := range remove {
		reclaim += cp.Bytes
	}
	kept := len(key.Copies) - len(remove)
	s := fmt.Sprintf("%d copies: keep %d (%d referenced, %d newest, %d recent, %d open, %d foreign), remove %d (%s)",
		len(key.Copies), kept, referenced, newest, recent, open, foreign, len(remove), bazelHumanBytes(reclaim))
	return s, reclaim
}

func bazelRepoContentsActivityOptions(cfg *config.Config, contentsDirs []string) bbActivityOptions {
	opts := bbActivityOptions{
		Lsof:             cfg.BazelBases.Lsof,
		LsofTimeout:      time.Duration(cfg.BazelBases.LsofTimeoutSeconds) * time.Second,
		UseLsof:          true,
		OKComms:          cfg.BazelBases.UninspectableOKComms,
		SkipSSHDSessions: true,
	}
	for _, d := range contentsDirs {
		opts.Within = append(opts.Within, uniquePaths(d, resolvedOrSelf(d))...)
	}
	return opts
}

func (p *BazelPlugin) scanActivity(ctx context.Context, opts bbActivityOptions) *bbActivity {
	if p.activityScan != nil {
		return p.activityScan(ctx, opts)
	}
	return bbScanActivity(ctx, opts)
}

// planBazelRepoContents returns one target per contents key with more than
// one copy, and the plan metadata.
func (p *BazelPlugin) planBazelRepoContents(ctx context.Context, cfg *config.Config, level CleanupLevel, measures []bazelRepoCacheMeasure, refs *bazelRepoContentsRefs, now time.Time) ([]CleanupTarget, map[string]string) {
	pol := bazelRepoContentsPolicyFor(cfg.Bazel)
	meta := map[string]string{
		"repo_contents_dedupe":      strconv.FormatBool(pol.enabled),
		"repo_contents_min_age":     pol.minAge.String(),
		"repo_contents_keep_recent": strconv.Itoa(pol.keep),
	}
	meta["repo_contents_output_bases_scanned"] = strconv.Itoa(refs.outputBases)
	meta["repo_contents_external_links"] = strconv.Itoa(refs.links)

	var contentsDirs []string
	type decided struct {
		key      bazelRepoContentsKey
		remove   []bazelRepoContentsCopy
		gcLocked bool
	}
	var unreadable int
	var all []decided
	anyRemove := false
	var keyCount, estimated int
	partial := false
	for _, m := range measures {
		estimated += m.Estimated
		partial = partial || m.Partial
		if m.ContentsDir == "" {
			continue
		}
		contentsDirs = append(contentsDirs, m.ContentsDir)
		unreadable += len(m.Unreadable)
		// Probe Bazel's gc_lock without creating it: a plan never writes.
		gcLocked := false
		if release, err := bazelGCLockTry(filepath.Join(m.ContentsDir, bazelRepoContentsGCLock), false); err != nil {
			gcLocked = true
		} else {
			release()
		}
		for _, key := range m.Keys {
			keyCount++
			if len(key.Copies) < 2 {
				continue
			}
			key.Copies = append([]bazelRepoContentsCopy(nil), key.Copies...)
			remove := decideBazelRepoContentsKey(&key, refs, nil, pol, now)
			anyRemove = anyRemove || len(remove) > 0
			all = append(all, decided{key, remove, gcLocked})
		}
	}
	meta["repo_contents_keys"] = strconv.Itoa(keyCount)
	meta["repo_contents_duplicate_keys"] = strconv.Itoa(len(all))
	if unreadable > 0 {
		meta["repo_contents_unreadable_dirs"] = strconv.Itoa(unreadable)
	}
	if estimated > 0 {
		meta["repo_contents_estimated_copies"] = strconv.Itoa(estimated)
	}
	if partial {
		meta["repository_cache_size_partial"] = "true"
	}

	// Only a plan with something to remove pays for a process inventory.
	var activityBlock string
	if anyRemove && pol.enabled && len(refs.incomplete) == 0 {
		activity := p.scanActivity(ctx, bazelRepoContentsActivityOptions(cfg, contentsDirs))
		if why := activity.complete(); why != "" {
			activityBlock = why
		} else {
			for i := range all {
				all[i].remove = decideBazelRepoContentsKey(&all[i].key, refs, activity, pol, now)
			}
		}
	}

	var targets []CleanupTarget
	var totalReclaim int64
	var removeCopies, keepCopies int
	for _, d := range all {
		summary, reclaim := bazelRepoContentsSummary(d.key, d.remove)
		t := CleanupTarget{Type: bazelRepoContentsType, Name: "contents/" + d.key.Key, Path: d.key.Dir, Bytes: reclaim}
		switch {
		case !pol.enabled:
			t.Action, t.Protected, t.Reason = "keep", true, "bazel.repo_contents_dedupe is off: "+summary
		case len(refs.incomplete) > 0:
			t.Action, t.Protected, t.Reason = "keep", true, "output-base external/ scan incomplete ("+refs.incomplete[0]+"): "+summary
		case d.gcLocked && len(d.remove) > 0:
			t.Action, t.Protected, t.Reason = "keep", true, errBazelGCLockHeld.Error()+": "+summary
		case activityBlock != "" && len(d.remove) > 0:
			t.Action, t.Protected, t.Reason = "keep", true, activityBlock+": "+summary
		case len(d.remove) == 0:
			t.Action, t.Protected, t.Reason = "keep", true, summary
		case level < LevelModerate:
			t.Action, t.Reason = bazelRepoContentsReport, "would remove at moderate or higher: "+summary
		default:
			t.Action, t.Reason = bazelRepoContentsDelete, summary
		}
		if t.Action != "keep" {
			totalReclaim += reclaim
			removeCopies += len(d.remove)
			keepCopies += len(d.key.Copies) - len(d.remove)
		} else {
			keepCopies += len(d.key.Copies)
		}
		annotateCleanupTargetPolicy(&t, CleanupTierWarm, hostReclaimForAction(t.Action))
		targets = append(targets, t)
	}
	meta["repo_contents_reclaim_bytes"] = strconv.FormatInt(totalReclaim, 10)
	meta["repo_contents_remove_copies"] = strconv.Itoa(removeCopies)
	meta["repo_contents_keep_copies"] = strconv.Itoa(keepCopies)
	return targets, meta
}

// bazelRepoContentsCopyPathAllowed checks that dir is
// <repository cache>/contents/<key>/<uuid>.
func bazelRepoContentsCopyPathAllowed(dir string) bool {
	dir = filepath.Clean(dir)
	if !bazelRepoContentsUUID.MatchString(filepath.Base(dir)) {
		return false
	}
	contents := filepath.Dir(filepath.Dir(dir))
	if filepath.Base(contents) != "contents" {
		return false
	}
	return bazelCacheTierPathAllowed("repository_cache", filepath.Dir(contents))
}

// deleteBazelRepoContentsCopy removes <uuid>.recorded_inputs first, so Bazel
// never sees a recorded entry without its tree, then the <uuid>/ tree.
func deleteBazelRepoContentsCopy(dir string, logger *slog.Logger) error {
	if !bazelRepoContentsCopyPathAllowed(dir) {
		return fmt.Errorf("refusing to delete unsafe Bazel repo contents path: %s", dir)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("refusing to delete non-directory Bazel repo contents copy: %s", dir)
	}
	rec := dir + bazelRepoContentsRecorded
	if ri, err := os.Lstat(rec); err == nil {
		if !ri.Mode().IsRegular() {
			return fmt.Errorf("refusing to delete non-regular %s", rec)
		}
		if err := os.Remove(rec); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := normalizeBazelDeletionPermissions(dir, logger); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// applyBazelRepoContents removes the unreferenced copies of every key marked
// for deletion. It re-lists each key, re-reads every external/ link and takes
// a fresh process inventory first, so a copy that became referenced, used or
// held since planning stays.
func (p *BazelPlugin) applyBazelRepoContents(ctx context.Context, cfg *config.Config, targets []CleanupTarget, measures []bazelRepoCacheMeasure, outputBases []string, logger *slog.Logger, result *CleanupResult) {
	var due []CleanupTarget
	for _, t := range targets {
		if t.Type == bazelRepoContentsType && t.Action == bazelRepoContentsDelete && !t.Protected {
			due = append(due, t)
		}
	}
	if len(due) == 0 {
		return
	}
	pol := bazelRepoContentsPolicyFor(cfg.Bazel)
	planned := map[string]int64{}
	var contentsDirs []string
	for _, m := range measures {
		if m.ContentsDir != "" {
			contentsDirs = append(contentsDirs, m.ContentsDir)
		}
		for _, k := range m.Keys {
			for _, cp := range k.Copies {
				planned[cp.Dir] = cp.Bytes
			}
		}
	}
	refs := collectBazelRepoContentsRefs(outputBases)
	if len(refs.incomplete) > 0 {
		logger.Warn("Bazel repo contents dedupe skipped: output-base external/ scan incomplete", "reason", refs.incomplete[0])
		return
	}
	activity := p.scanActivity(ctx, bazelRepoContentsActivityOptions(cfg, contentsDirs))
	if why := activity.complete(); why != "" {
		logger.Warn("Bazel repo contents dedupe skipped", "reason", why)
		return
	}
	for _, t := range due {
		if ctx.Err() != nil {
			result.Error = ctx.Err()
			return
		}
		freed, removed, kept, err := p.applyBazelRepoContentsKey(ctx, t.Path, refs, activity, pol, planned, outputBases, logger)
		if errors.Is(err, errBazelGCLockHeld) {
			logger.Info("Bazel repo contents dedupe skipped", "reason", errBazelGCLockHeld.Error(), "path", t.Path)
			continue
		}
		if err != nil {
			logger.Warn("Bazel repo contents dedupe stopped for key", "path", t.Path, "error", err)
		}
		if removed > 0 || err != nil {
			logger.Info("removed unreferenced duplicate Bazel repo contents copies",
				"key", filepath.Base(t.Path), "path", t.Path,
				"kept", kept, "removed", removed, "estimated_bytes", freed,
				"min_age", pol.minAge.String(), "keep_recent", pol.keep)
		}
		result.BytesFreed += freed
		result.EstimatedBytesFreed += freed
		result.ItemsCleaned += removed
	}
}

// applyBazelRepoContentsKey removes one key's unreferenced copies while it
// holds Bazel's contents/gc_lock exclusively, so no Bazel command can use or
// add a copy of the cache during the pass. A busy lock skips the key. Just
// before each deletion it re-reads every external/ link and the copy's last
// use, so a copy that became referenced or used since planning stays.
func (p *BazelPlugin) applyBazelRepoContentsKey(ctx context.Context, keyDir string, refs *bazelRepoContentsRefs, activity *bbActivity, pol bazelRepoContentsPolicy, planned map[string]int64, outputBases []string, logger *slog.Logger) (freed int64, removed, kept int, err error) {
	contentsDir := filepath.Dir(keyDir)
	release, err := bazelGCLockTry(filepath.Join(contentsDir, bazelRepoContentsGCLock), true)
	if err != nil {
		return 0, 0, 0, err
	}
	defer release()

	now := time.Now()
	var key *bazelRepoContentsKey
	for _, k := range listBazelRepoContents(contentsDir, now) {
		if k.Dir == keyDir {
			k := k
			key = &k
			break
		}
	}
	if key == nil {
		return 0, 0, 0, nil
	}
	remove := decideBazelRepoContentsKey(key, refs, activity, pol, now)
	kept = len(key.Copies)
	for _, cp := range remove {
		if err := ctx.Err(); err != nil {
			return freed, removed, kept, err
		}
		fresh := collectBazelRepoContentsRefs(outputBases)
		if len(fresh.incomplete) > 0 {
			return freed, removed, kept, fmt.Errorf("output-base external/ scan incomplete: %s", fresh.incomplete[0])
		}
		if why, ok := fresh.referencedBy(cp.Dir); ok {
			logger.Info("keeping Bazel repo contents copy referenced since planning", "path", cp.Dir, "link", why)
			continue
		}
		last, err := bazelRepoContentsCopyLastUse(cp.Dir, time.Now())
		if err != nil {
			logger.Info("keeping Bazel repo contents copy whose last use cannot be re-read", "path", cp.Dir, "error", err)
			continue
		}
		if time.Since(last) < pol.minAge {
			logger.Info("keeping Bazel repo contents copy used since planning", "path", cp.Dir, "last_use", last.UTC().Format(time.RFC3339))
			continue
		}
		bytes, ok := planned[cp.Dir]
		if !ok {
			pctx, cancel := context.WithTimeout(ctx, bazelRepoContentsProbeTimeout)
			_, bytes, _ = bazelAllocatedTree(pctx, cp.Dir, map[[2]uint64]bool{})
			cancel()
		}
		if err := deleteBazelRepoContentsCopy(cp.Dir, logger); err != nil {
			logger.Warn("failed to delete Bazel repo contents copy", "path", cp.Dir, "error", err)
			continue
		}
		logger.Debug("deleted unreferenced Bazel repo contents copy", "path", cp.Dir, "estimated_bytes", bytes)
		freed += bytes
		removed++
		kept--
	}
	return freed, removed, kept, nil
}

// guardBazelRepositoryCacheTiers withholds the over-budget whole-tier deletion
// of a repository cache that holds a copy an output base references, that
// holds duplicate copies of a key, or whose activity is not proven absent
// (the external/ scan or the Bazel process inspection is incomplete). The
// repo contents dedupe class reclaims such a cache instead (TIN-5770).
func guardBazelRepositoryCacheTiers(targets []CleanupTarget, measures []bazelRepoCacheMeasure, refs *bazelRepoContentsRefs, activeErr error) []CleanupTarget {
	byRoot := map[string]bazelRepoCacheMeasure{}
	for _, m := range measures {
		byRoot[filepath.Clean(m.Root)] = m
	}
	for i := range targets {
		t := &targets[i]
		if t.Type != "repository_cache" || t.Action != "delete_cache_tier" {
			continue
		}
		m := byRoot[filepath.Clean(t.Path)]
		var referenced, duplicateKeys int
		for _, k := range m.Keys {
			if len(k.Copies) > 1 {
				duplicateKeys++
			}
			for _, cp := range k.Copies {
				if _, ok := refs.referencedBy(cp.Dir); ok {
					referenced++
				}
			}
		}
		var why string
		switch {
		case referenced > 0:
			why = fmt.Sprintf("%d copies are referenced by output bases", referenced)
		case duplicateKeys > 0:
			why = fmt.Sprintf("%d keys hold duplicate copies", duplicateKeys)
		case len(m.Unreadable) > 0:
			why = "the repository contents listing is incomplete (" + m.Unreadable[0] + ")"
		case len(refs.incomplete) > 0:
			why = "the output-base external/ scan is incomplete"
		case activeErr != nil:
			why = "Bazel process inspection failed"
		default:
			continue
		}
		t.Action, t.Protected = "keep", true
		t.Reason = "whole repository cache deletion withheld because " + why + "; the repo contents dedupe class reclaims it instead"
		annotateCleanupTargetPolicy(t, CleanupTierWarm, hostReclaimForAction(t.Action))
	}
	return targets
}
