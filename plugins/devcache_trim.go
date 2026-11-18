package plugins

// devcache_trim.go trims a developer cache root that a host named in
// darwin_dev_caches.<family>.paths (TIN-5770). On petting-zoo-mini the uv
// cache lives at /nix/var/tinyland/user/nix-client-cache/uv, which the
// compiled ~/.cache/uv and ~/Library/Caches/uv paths never reach; it held
// 44.5 GiB of the /nix volume on November 18, 2026.
//
// A listed root is never removed in one piece. Its top-level directories are
// the units; a top-level directory a live process holds is not a unit, and
// its own entries are the units instead (one level down, so uv's archive-v0
// bucket gives its unpacked archives). A unit is held when the activity
// inventory finds an open file, cwd, executable, mapping or command line
// under it. An incomplete inventory keeps everything, and so does a held
// top-level file, which is the cache's lock file while the tool runs.
//
// At moderate pressure or above, unheld units go oldest first by the newest
// change inside them: first those past stale_after_days, then more until the
// root is at or under max_gb. A unit changed within devCacheTrimMinAge is
// never chosen, so a tool still filling a fresh entry is not raced. Every
// chosen unit is checked again against a fresh inventory just before
// removal.

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	osuser "os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// devCacheTrimMinAge is the least age of a unit's newest change before the
// trim may choose it.
const devCacheTrimMinAge = time.Hour

// devCacheEntrySuffix marks a trim target's type, for example uv-cache-entry.
const devCacheEntrySuffix = "-entry"

// devCacheTrimPolicy is one root's trim policy.
type devCacheTrimPolicy struct {
	// maxBytes is the budget for the whole root; 0 sets none.
	maxBytes int64
	// staleAfter makes a unit eligible on age alone; 0 sets none.
	staleAfter time.Duration
	level      CleanupLevel
	enforce    bool
	now        time.Time
}

// devCacheUnit is one removable entry of a trimmed root.
type devCacheUnit struct {
	path   string
	newest time.Time
	bytes  int64
	dir    bool
}

// devCacheTrimTargets plans one configured root. It returns a protected
// summary target for the root and one target per chosen unit. It removes
// nothing.
func devCacheTrimTargets(ctx context.Context, root, targetType, displayName string, pol devCacheTrimPolicy, activity *bbActivity) []CleanupTarget {
	summary := CleanupTarget{
		Type: targetType, Name: root, Path: root,
		Action: "protect", Protected: true,
	}
	finish := func(ts []CleanupTarget) []CleanupTarget {
		annotateCleanupTargetPolicy(&summary, CleanupTierSafe, CleanupReclaimNone)
		return append([]CleanupTarget{summary}, ts...)
	}
	if pol.level < LevelModerate {
		summary.Bytes = getDirAllocatedBytes(root)
		summary.Reason = displayName + " at a configured path: entries are trimmed at moderate pressure or above"
		return finish(nil)
	}
	if activity == nil {
		summary.Reason = displayName + " kept: no activity inventory"
		return finish(nil)
	}
	if why := activity.complete(); why != "" {
		summary.Bytes = getDirAllocatedBytes(root)
		summary.Reason = displayName + " kept: " + why
		return finish(nil)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		summary.Reason = fmt.Sprintf("%s kept: root unreadable: %v", displayName, err)
		return finish(nil)
	}
	var units []devCacheUnit
	var total int64
	var heldNotes []string
	for _, e := range entries {
		if ctx.Err() != nil {
			summary.Reason = displayName + " kept: pass interrupted"
			return finish(nil)
		}
		path := filepath.Join(root, e.Name())
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		held := activity.blockers(path)
		if !info.IsDir() {
			total += devCacheAllocated(info)
			if len(held) > 0 && info.Mode().IsRegular() {
				// The cache's own lock file is open: the tool is running in
				// this cache, so nothing is trimmed this pass.
				summary.Bytes = getDirAllocatedBytes(root)
				summary.Active = true
				summary.Reason = fmt.Sprintf("%s kept: %s is open (%s)", displayName, e.Name(), held[0])
				return finish(nil)
			}
			continue // files and links at the top are never units
		}
		if len(held) == 0 {
			u := devCacheMeasure(ctx, path, pol.now)
			units = append(units, u)
			total += u.bytes
			continue
		}
		heldNotes = append(heldNotes, e.Name()+": "+held[0])
		total += devCacheAllocated(info)
		children, err := os.ReadDir(path)
		if err != nil {
			total += getDirAllocatedBytes(path)
			continue
		}
		for _, c := range children {
			cp := filepath.Join(path, c.Name())
			cinfo, err := os.Lstat(cp)
			if err != nil {
				continue
			}
			if len(activity.blockers(cp)) > 0 || cinfo.Mode()&fs.ModeSymlink != 0 {
				if cinfo.IsDir() {
					total += getDirAllocatedBytes(cp)
				} else {
					total += devCacheAllocated(cinfo)
				}
				continue
			}
			if !cinfo.IsDir() && !cinfo.Mode().IsRegular() {
				total += devCacheAllocated(cinfo)
				continue
			}
			u := devCacheMeasure(ctx, cp, pol.now)
			units = append(units, u)
			total += u.bytes
		}
	}

	sort.Slice(units, func(i, j int) bool {
		if units[i].newest.Equal(units[j].newest) {
			return units[i].path < units[j].path
		}
		return units[i].newest.Before(units[j].newest)
	})
	chosen := map[string]string{}
	remaining := total
	minCut := pol.now.Add(-devCacheTrimMinAge)
	if pol.staleAfter > 0 {
		cut := pol.now.Add(-pol.staleAfter)
		for _, u := range units {
			if u.newest.Before(cut) && u.newest.Before(minCut) {
				chosen[u.path] = "stale: newest change " + u.newest.UTC().Format(time.RFC3339) + " is older than " + formatDevArtifactAge(pol.staleAfter)
				remaining -= u.bytes
			}
		}
	}
	if pol.maxBytes > 0 {
		for _, u := range units {
			if remaining <= pol.maxBytes {
				break
			}
			if _, ok := chosen[u.path]; ok || !u.newest.Before(minCut) {
				continue
			}
			chosen[u.path] = fmt.Sprintf("over budget: root %s above max_gb %s, oldest unheld entry first", devCacheGiB(remaining), devCacheGiB(pol.maxBytes))
			remaining -= u.bytes
		}
	}

	var targets []CleanupTarget
	var chosenBytes int64
	for _, u := range units {
		why, ok := chosen[u.path]
		if !ok {
			continue
		}
		chosenBytes += u.bytes
		action := "review"
		reason := displayName + " entry is a cleanup candidate (set darwin_dev_caches.enforce): " + why
		if pol.enforce {
			action = "delete"
			reason = displayName + " entry is eligible: " + why
		}
		t := CleanupTarget{
			Type: targetType + devCacheEntrySuffix, Name: devCacheRel(root, u.path), Path: u.path,
			Bytes: u.bytes, Action: action, Reason: reason,
		}
		annotateCleanupTargetPolicy(&t, CleanupTierSafe, hostReclaimForAction(action))
		targets = append(targets, t)
	}
	summary.Bytes = total - chosenBytes
	summary.Reason = fmt.Sprintf("%s root is never removed whole; %d of %d unheld entries chosen (%s of %s)",
		displayName, len(targets), len(units), devCacheGiB(chosenBytes), devCacheGiB(total))
	if pol.maxBytes > 0 {
		summary.Reason += "; max_gb " + devCacheGiB(pol.maxBytes)
	}
	if len(heldNotes) > 0 {
		summary.Active = true
		summary.Reason += "; held: " + strings.Join(heldNotes, "; ")
	}
	return finish(targets)
}

// devCacheMeasure walks one unit: allocated bytes and its newest mtime or
// ctime. A walk error keeps the time at now, so the unit is never chosen.
func devCacheMeasure(ctx context.Context, path string, now time.Time) devCacheUnit {
	u := devCacheUnit{path: path}
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if p == path {
			u.dir = fi.IsDir()
		}
		u.bytes += devCacheAllocated(fi)
		if t := effectiveModTime(fi, now); t.After(u.newest) {
			u.newest = t
		}
		if ct, ok := changeTime(fi); ok && !ct.After(now.Add(futureMtimeTolerance)) && ct.After(u.newest) {
			u.newest = ct
		}
		return nil
	})
	if err != nil {
		u.newest = now
	}
	return u
}

func devCacheAllocated(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Blocks > 0 {
		return int64(st.Blocks) * 512
	}
	if fi.Mode().IsRegular() {
		return fi.Size()
	}
	return 0
}

func devCacheRel(root, path string) string {
	if rel, err := filepath.Rel(filepath.Dir(root), path); err == nil {
		return rel
	}
	return filepath.Base(path)
}

func devCacheGiB(b int64) string {
	return strconv.FormatFloat(float64(b)/float64(1<<30), 'f', 1, 64) + " GiB"
}

// devCacheTrimApply removes the delete targets that devCacheTrimTargets
// chose, each after a fresh check: the same file, no holder in an inventory
// at most retentionActivityMaxAge old, and a complete inventory.
func devCacheTrimApply(ctx context.Context, targets []CleanupTarget, scan func() *bbActivity, remove func(string) error, clock func() time.Time, logger *slog.Logger) (freed int64, items int, err error) {
	var fresh *bbActivity
	var freshAt time.Time
	for _, t := range targets {
		if !strings.HasSuffix(t.Type, devCacheEntrySuffix) || t.Action != "delete" || t.Path == "" {
			continue
		}
		if ctx.Err() != nil {
			return freed, items, ctx.Err()
		}
		if fresh == nil || clock().Sub(freshAt) > retentionActivityMaxAge {
			fresh = scan()
			freshAt = clock()
		}
		if why := fresh.complete(); why != "" {
			logger.Info("developer cache entry kept at removal time", "path", t.Path, "reason", why)
			continue
		}
		if held := fresh.blockers(t.Path); len(held) > 0 {
			logger.Info("developer cache entry kept at removal time", "path", t.Path, "reason", "live reference: "+held[0])
			continue
		}
		if _, lerr := os.Lstat(t.Path); lerr != nil {
			continue
		}
		if rerr := remove(t.Path); rerr != nil {
			logger.Warn("could not remove developer cache entry", "path", t.Path, "error", rerr)
			err = rerr
			continue
		}
		freed += t.Bytes
		items++
		logger.Info("removed developer cache entry", "type", t.Type, "path", t.Path, "bytes", t.Bytes, "reason", t.Reason)
	}
	return freed, items, err
}

// devCacheRemove removes one unit; Nix and uv copies can be read-only.
func devCacheRemove(path string) error {
	if err := os.RemoveAll(path); err != nil {
		_ = bbMakeWritable(path)
		return os.RemoveAll(path)
	}
	return nil
}

// devCacheFamily is one directory family that takes a paths key.
type devCacheFamily struct {
	key, targetType, displayName string
	cfg                          func(c config.DarwinDevCachesConfig) config.DarwinDevCacheToolConfig
}

var devCacheFamilies = []devCacheFamily{
	{"pip", "pip", "pip cache", func(c config.DarwinDevCachesConfig) config.DarwinDevCacheToolConfig { return c.Pip }},
	{"npm", "npm-cache", "npm cache", func(c config.DarwinDevCachesConfig) config.DarwinDevCacheToolConfig { return c.NPM }},
	{"uv", "uv-cache", "uv cache", func(c config.DarwinDevCachesConfig) config.DarwinDevCacheToolConfig { return c.UV }},
	{"bun", "bun-cache", "Bun cache", func(c config.DarwinDevCachesConfig) config.DarwinDevCacheToolConfig { return c.Bun }},
	{"opencode", "opencode-cache", "opencode cache", func(c config.DarwinDevCachesConfig) config.DarwinDevCacheToolConfig { return c.OpenCode }},
}

// devCacheConfiguredRoots expands the enabled families' paths keys. A root
// that does not exist as a directory is skipped. The map gives each root's
// family; a root named by two families goes to the first.
func devCacheConfiguredRoots(cfg config.DarwinDevCachesConfig, home, user string) ([]string, map[string]devCacheFamily) {
	var roots []string
	fam := map[string]devCacheFamily{}
	if user == "" {
		// The Darwin agent runs under env -i, so USER can be empty.
		if u, err := osuser.Current(); err == nil {
			user = u.Username
		}
	}
	for _, f := range devCacheFamilies {
		tool := f.cfg(cfg)
		if !tool.Enabled {
			continue
		}
		for _, raw := range tool.Paths {
			p := filepath.Clean(config.ExpandUserPath(raw, home, user))
			if !filepath.IsAbs(p) || p == "/" || p == filepath.Clean(home) {
				continue
			}
			if _, seen := fam[p]; seen {
				continue
			}
			if !pathExistsAndIsDir(p) {
				continue
			}
			fam[p] = f
			roots = append(roots, p)
		}
	}
	return roots, fam
}

// devCacheActivityOptions is the inventory a trim takes: one lsof listing
// and every command line, indexed under the configured roots only.
func devCacheActivityOptions(lsof string, roots []string) bbActivityOptions {
	var within []string
	for _, r := range roots {
		within = append(within, uniquePaths(r, resolvedOrSelf(r))...)
	}
	return bbActivityOptions{Lsof: lsof, LsofTimeout: 120 * time.Second, UseLsof: true, Within: within}
}

// configuredDevCacheTargets plans every configured root. The inventory is
// taken only when a configured root exists, so a host that sets no paths
// key pays nothing for it.
func configuredDevCacheTargets(ctx context.Context, cfg config.DarwinDevCachesConfig, home, user string, level CleanupLevel, now time.Time, scan func(roots []string) *bbActivity) []CleanupTarget {
	roots, fam := devCacheConfiguredRoots(cfg, home, user)
	if len(roots) == 0 {
		return nil
	}
	var activity *bbActivity
	if level >= LevelModerate {
		activity = scan(roots)
	}
	enforce := cfg.Enforce && level >= LevelModerate
	var out []CleanupTarget
	for _, r := range roots {
		f := fam[r]
		tool := f.cfg(cfg)
		pol := devCacheTrimPolicy{
			maxBytes:   int64(tool.MaxGB) * (1 << 30),
			staleAfter: time.Duration(tool.StaleAfterDays) * 24 * time.Hour,
			level:      level,
			enforce:    enforce,
			now:        now,
		}
		out = append(out, devCacheTrimTargets(ctx, r, f.targetType, f.displayName, pol, activity)...)
	}
	return out
}
