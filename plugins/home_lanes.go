package plugins

// home_lanes.go reclaims stale trees that no other lane covers from a
// developer home (lab amendment A-082, TIN-5770, operator ruling 2026-11-05).
//
// Why: on sting on 2026-11-05 /home sat at 93 percent and this daemon would
// have collected 0 bytes there, because no rule covered the two trees that
// were growing: lanes that agents left in ~/.cache, and Hermes per-checkout
// dependency state in ~/.hermes/installs whose checkout had been deleted (30
// of 44 installs).
//
// A candidate is one direct child of a configured root:
//
//   - cache lane: a directory of cache_root whose name matches
//     cache_lane_globs. Anything else in cache_root is a tool's cache and is
//     never a candidate;
//   - Hermes install: a directory of hermes_installs_root whose checkout, named
//     in inputs/.project-root, is gone. Hermes's own pm/install_states.py
//     defines this record, and like Hermes this plugin leaves a directory with
//     no record alone. The recorded path must sit under a directory of
//     hermes_checkout_roots that exists, so an unmounted volume cannot read
//     as a deleted checkout. Hermes itself proves deletion only under its data
//     root or a .worktrees directory, so it cannot see sting's checkouts.
//
// Every candidate must then pass all of these, and every check runs again
// immediately before removal:
//
//   - a real directory owned by this user, whose every entry is this user's
//     and on the same device, with no .git anywhere;
//   - never agent-session data (the compiled TIN-5268 deny set);
//   - idle: the newest modification time in the tree, and the newest access
//     time of its files, are older than the idle window. Directory access
//     times do not count, because this walk updates them, and neither do a
//     Hermes install's metadata files (homeLanesHermesMetadata);
//   - no live holder: no inspected process has its cwd or root there, an open
//     file there, a mapping there, or names it on its command line, and an
//     incomplete inventory keeps every tree;
//   - no kernel file lock on any file in the tree. /proc/locks lists every
//     process in every namespace, so this also sees a Hermes install lock or
//     generation lease that a sandboxed process holds through a bind mount,
//     which the path-based inventory cannot. An unreadable lock table keeps
//     every tree.
//
// A third kind covers roots of stale work, such as ~/build (TIN-8526,
// operator ruling R-STING-HOME-20261117-c "Home budget rules"):
//
//   - stale work entry: a directory or regular file directly under a
//     stale_work_roots directory. It is idle when no entry under it was
//     modified within stale_work_idle_after. Access times do not count here,
//     because the Git probe below reads files. A .git anywhere in the entry
//     does not keep it by itself. Each such checkout is asked, in a closed
//     question to git, whether it holds work found nowhere else
//     (homeLanesGitKeepReason). Any yes, any failed or timed-out probe, a bare
//     repository, a .hg or .jj directory, or more than homeLanesMaxCheckouts
//     checkouts keeps the entry. Every other rule above applies unchanged.
//
// free_target_gb makes removal a budget: in apply mode the free space of a
// root's mount is read before each removal, and once it reaches the target,
// or cannot be read, that mount gets no further removal this cycle.
//
// mode report plans only. Apply removes below moderate level never, and at
// most max_removals_per_cycle trees a cycle, largest first. A tree is renamed
// aside before removal, so a new launch never meets a half-removed tree, and a
// removal cut short is finished on the next cycle.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

const (
	homeLanesPluginName = "home_lanes"
	// homeLanesWalkLimit bounds the entries one tree walk may visit. A larger
	// tree is kept.
	homeLanesWalkLimit = 1000000
	// homeLanesAsidePrefix names a tree that was renamed aside for removal.
	homeLanesAsidePrefix = ".tinyland-reclaim-"
	// homeLanesProjectRootRecord is where Hermes records an install's checkout.
	homeLanesProjectRootRecord = "inputs/.project-root"
)

// homeLanesHermesMetadata are the small state files that sweeps across every
// install read. On sting on 2026-11-05 facts.json and active.json in all 30
// orphaned installs carried the same access time, refreshed about daily under
// relatime, while their packages sat days older. This plugin reads the
// checkout record itself. Their access times say nothing about use.
var homeLanesHermesMetadata = map[string]bool{
	filepath.Base(homeLanesProjectRootRecord): true,
	"facts.json":    true,
	"active.json":   true,
	"selected.json": true,
}

// errHomeLanesRecent ends a walk that met a modification inside the idle
// window.
var errHomeLanesRecent = errors.New("recent activity")

// homeLanesInode names a file the way /proc/locks does.
type homeLanesInode struct {
	major, minor, ino uint64
}

// HomeLanesPlugin removes stale ~/.cache lanes and orphaned Hermes installs.
type HomeLanesPlugin struct {
	// Test seams; nil means the real implementation.
	now          func() time.Time
	activity     func(ctx context.Context, opts bbActivityOptions) *bbActivity
	lockedInodes func() (map[homeLanesInode]bool, error)
	removeAll    func(path string) error
	freeBytes    func(path string) (uint64, error)
	// gitPath and gitTimeout override the git binary and the per-command
	// probe timeout.
	gitPath    string
	gitTimeout time.Duration
	uid        int
	home       string
	tmpdir     string
}

// NewHomeLanesPlugin creates the stale home tree reaper.
func NewHomeLanesPlugin() *HomeLanesPlugin {
	home, _ := os.UserHomeDir()
	return &HomeLanesPlugin{uid: os.Geteuid(), home: home, tmpdir: os.Getenv("TMPDIR")}
}

// Name returns the plugin identifier.
func (p *HomeLanesPlugin) Name() string { return homeLanesPluginName }

// Description returns the plugin description.
func (p *HomeLanesPlugin) Description() string {
	return "Removes stale ~/.cache lanes and orphaned Hermes installs that no process holds (lab A-082)"
}

// SupportedPlatforms returns supported platforms.
func (p *HomeLanesPlugin) SupportedPlatforms() []string { return []string{PlatformLinux} }

// Enabled checks if the plugin is enabled.
func (p *HomeLanesPlugin) Enabled(cfg *config.Config) bool { return cfg.Enable.HomeLanes }

func (p *HomeLanesPlugin) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *HomeLanesPlugin) scan(ctx context.Context, opts bbActivityOptions) *bbActivity {
	if p.activity != nil {
		return p.activity(ctx, opts)
	}
	return bbScanActivity(ctx, opts)
}

func (p *HomeLanesPlugin) locks() (map[homeLanesInode]bool, error) {
	if p.lockedInodes != nil {
		return p.lockedInodes()
	}
	return homeLanesReadLockedInodes()
}

func (p *HomeLanesPlugin) free(path string) (uint64, error) {
	if p.freeBytes != nil {
		return p.freeBytes(path)
	}
	return getFreeDiskSpace(path)
}

func (p *HomeLanesPlugin) remove(path string) error {
	if p.removeAll != nil {
		return p.removeAll(path)
	}
	return os.RemoveAll(path)
}

// homeLanesCandidate is one direct child of a root and the decision about it.
type homeLanesCandidate struct {
	target CleanupTarget
	info   os.FileInfo
	kind   string
	root   string
}

const (
	homeLanesKindCache  = "cache-lane"
	homeLanesKindHermes = "hermes-install"
	homeLanesKindStale  = "stale-work"
)

// homeLanesGiB converts free_target_gb to bytes.
const homeLanesGiB = 1 << 30

// homeLanesIdleSetting returns the configured idle window of a kind.
func homeLanesIdleSetting(hc config.HomeLanesConfig, kind string) string {
	switch kind {
	case homeLanesKindHermes:
		return hc.HermesIdleAfter
	case homeLanesKindStale:
		return hc.StaleWorkIdleAfter
	default:
		return hc.CacheIdleAfter
	}
}

func homeLanesApplies(hc config.HomeLanesConfig, level CleanupLevel) bool {
	return hc.Mode == config.HomeLanesModeApply && level >= LevelModerate
}

// PlanCleanup reports what the reaper would do, removing nothing.
func (p *HomeLanesPlugin) PlanCleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupPlan {
	plan := CleanupPlan{Plugin: p.Name(), Level: level.String()}
	hc := cfg.HomeLanes
	if err := hc.Validate(); err != nil {
		plan.SkipReason = "invalid home_lanes policy: " + err.Error()
		return plan
	}
	apply := homeLanesApplies(hc, level)
	candidates, warnings := p.plan(ctx, cfg, apply)
	p.applyFreeTarget(hc, candidates)
	plan.Warnings = warnings
	for _, c := range candidates {
		plan.Targets = append(plan.Targets, c.target)
		if !c.target.Protected && c.target.Action == "delete" {
			plan.EstimatedBytesFreed += c.target.Bytes
		}
	}
	plan.WouldRun = apply
	if !apply {
		plan.SkipReason = fmt.Sprintf("home_lanes.mode is %s at level %s; apply needs apply mode at moderate or higher", hc.Mode, level)
	}
	plan.Summary = fmt.Sprintf("%d stale tree candidate(s) inspected", len(candidates))
	plan.Metadata = map[string]string{"mode": hc.Mode, "cache_idle_after": hc.CacheIdleAfter, "hermes_idle_after": hc.HermesIdleAfter,
		"stale_work_idle_after": hc.StaleWorkIdleAfter, "free_target_gb": fmt.Sprint(hc.FreeTargetGB)}
	annotateCleanupPlanTargetAccounting(&plan)
	return plan
}

// Cleanup removes eligible trees in apply mode at moderate level or higher.
// Otherwise it logs the plan and removes nothing.
func (p *HomeLanesPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: level}
	hc := cfg.HomeLanes
	if err := hc.Validate(); err != nil {
		result.Error = err
		return result
	}
	apply := homeLanesApplies(hc, level)
	if apply {
		p.finishAside(hc, logger)
	}
	candidates, warnings := p.plan(ctx, cfg, apply)
	for _, w := range warnings {
		logger.Warn("home_lanes", "warning", w)
	}
	if !apply {
		p.applyFreeTarget(hc, candidates)
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].target.Bytes > candidates[j].target.Bytes })
	removed := 0
	// budgetMet lists the roots whose mount reached the free target, or whose
	// free space could not be read, this cycle.
	budgetMet := map[string]string{}
	for _, c := range candidates {
		t := c.target
		switch {
		case t.Protected:
			logger.Debug("home_lanes keeps tree", "path", t.Path, "reason", t.Reason)
		case !apply:
			logger.Info("home_lanes would remove tree", "path", t.Path, "bytes", t.Bytes, "reason", t.Reason)
		case removed >= hc.MaxRemovalsPerCycle:
			logger.Info("home_lanes defers tree to the next cycle", "path", t.Path, "bytes", t.Bytes)
		default:
			if why := p.freeTargetReached(hc, c.root, budgetMet); why != "" {
				logger.Info("home_lanes defers tree: free target", "path", t.Path, "bytes", t.Bytes, "reason", why)
				continue
			}
			// Fresh checks immediately before mutation: a plan is no lease.
			if why := p.recheck(ctx, cfg, c); why != "" {
				logger.Info("home_lanes keeps tree at removal time", "path", t.Path, "reason", why)
				continue
			}
			if err := p.reclaim(t.Path, c.info); err != nil {
				logger.Warn("home_lanes could not remove tree", "path", t.Path, "error", err)
				continue
			}
			removed++
			logger.Info("home_lanes removed tree", "path", t.Path, "kind", c.kind, "bytes", t.Bytes)
			result.BytesFreed += t.Bytes
			result.EstimatedBytesFreed += t.Bytes
			result.ItemsCleaned++
		}
	}
	return result
}

// freeTargetReached reads the free space of root's mount just before a
// removal. It returns why removal on that mount stops for this cycle: the
// target is reached, or free space could not be read. "" means remove.
func (p *HomeLanesPlugin) freeTargetReached(hc config.HomeLanesConfig, root string, met map[string]string) string {
	if hc.FreeTargetGB <= 0 {
		return ""
	}
	if why, ok := met[root]; ok {
		return why
	}
	target := uint64(hc.FreeTargetGB) * homeLanesGiB
	free, err := p.free(root)
	why := ""
	switch {
	case err != nil:
		why = "free space of " + root + " unreadable: " + err.Error()
	case free >= target:
		why = fmt.Sprintf("%s has %d bytes free, at or over the %d GiB target", root, free, hc.FreeTargetGB)
	}
	if why != "" {
		met[root] = why
	}
	return why
}

// applyFreeTarget marks, for a report, the eligible trees a free target
// would leave: largest first, each root's mount gains the planned bytes
// until it reaches the target. Planned bytes are allocated bytes, which can
// overstate what a removal frees (hard links, reflinks), so apply mode reads
// free space again before each removal and does not use this projection.
func (p *HomeLanesPlugin) applyFreeTarget(hc config.HomeLanesConfig, candidates []homeLanesCandidate) {
	if hc.FreeTargetGB <= 0 {
		return
	}
	target := uint64(hc.FreeTargetGB) * homeLanesGiB
	order := make([]int, 0, len(candidates))
	for i, c := range candidates {
		if !c.target.Protected {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		return candidates[order[a]].target.Bytes > candidates[order[b]].target.Bytes
	})
	projected := map[string]uint64{}
	unreadable := map[string]error{}
	for _, i := range order {
		c := &candidates[i]
		free, seen := projected[c.root]
		if !seen {
			if _, failed := unreadable[c.root]; !failed {
				f, err := p.free(c.root)
				if err != nil {
					unreadable[c.root] = err
				} else {
					free = f
					projected[c.root] = f
				}
			}
		}
		why := ""
		if err, failed := unreadable[c.root]; failed {
			why = "free target: free space of " + c.root + " unreadable: " + err.Error()
		} else if free >= target {
			why = fmt.Sprintf("free target: %s would already have %d bytes free, at or over %d GiB", c.root, free, hc.FreeTargetGB)
		}
		if why != "" {
			c.target.Protected = true
			c.target.Action = "protect"
			c.target.Reason = why
			annotateCleanupTargetPolicy(&c.target, CleanupTierDestructive, hostReclaimForAction(c.target.Action))
			continue
		}
		if c.target.Bytes > 0 {
			projected[c.root] = free + uint64(c.target.Bytes)
		}
	}
}

// plan inspects every candidate of every root. It never removes.
func (p *HomeLanesPlugin) plan(ctx context.Context, cfg *config.Config, apply bool) ([]homeLanesCandidate, []string) {
	hc := cfg.HomeLanes
	deny := newAgentSessionDeny(p.home, p.uid, p.tmpdir, cfg.AgentSession.ExtraPaths, nil)
	var warnings []string
	type rootSpec struct {
		root, kind string
		idle       time.Duration
	}
	var specs []rootSpec
	if hc.CacheRoot != "" && len(hc.CacheLaneGlobs) > 0 {
		idle, _ := config.ParseDuration(hc.CacheIdleAfter)
		specs = append(specs, rootSpec{expandHome(hc.CacheRoot, p.home), homeLanesKindCache, idle})
	}
	if hc.HermesInstallsRoot != "" {
		idle, _ := config.ParseDuration(hc.HermesIdleAfter)
		specs = append(specs, rootSpec{expandHome(hc.HermesInstallsRoot, p.home), homeLanesKindHermes, idle})
	}
	if len(hc.StaleWorkRoots) > 0 {
		idle, _ := config.ParseDuration(hc.StaleWorkIdleAfter)
		for _, r := range hc.StaleWorkRoots {
			specs = append(specs, rootSpec{filepath.Clean(expandHome(r, p.home)), homeLanesKindStale, idle})
		}
	}
	var within []string
	for _, s := range specs {
		within = append(within, s.root)
	}
	if len(within) == 0 {
		return nil, warnings
	}
	activity := p.scan(ctx, bbActivityOptions{OKComms: hc.UninspectableOKComms, Within: within})
	incomplete := activity.complete()
	locked, lockErr := p.locks()
	if lockErr != nil {
		incomplete = strings.TrimSpace(incomplete + " file lock table unreadable: " + lockErr.Error())
	}
	var out []homeLanesCandidate
	for _, s := range specs {
		entries, err := os.ReadDir(s.root)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				warnings = append(warnings, fmt.Sprintf("root %s unreadable: %v", s.root, err))
			}
			continue
		}
		for _, e := range entries {
			if ctx.Err() != nil {
				return out, append(warnings, "cycle interrupted")
			}
			name := e.Name()
			if strings.HasPrefix(name, homeLanesAsidePrefix) {
				continue
			}
			if s.kind == homeLanesKindCache && !homeLanesGlobMatch(hc.CacheLaneGlobs, name) {
				continue
			}
			path := filepath.Join(s.root, name)
			info, err := os.Lstat(path)
			if err != nil || !(info.IsDir() || (s.kind == homeLanesKindStale && info.Mode().IsRegular())) {
				// Symlinks, sockets and vanished entries are never candidates,
				// and neither are files outside a stale work root.
				continue
			}
			c := homeLanesCandidate{info: info, kind: s.kind, root: s.root, target: CleanupTarget{
				Type: s.kind, Name: name, Path: path, Action: "protect", Protected: true,
			}}
			reason := ""
			if s.kind == homeLanesKindHermes {
				reason = p.hermesKeepReason(path, hc)
			}
			if reason == "" {
				reason = p.keepReason(ctx, path, info, s.kind, s.idle, deny, locked)
			}
			if reason == "" && incomplete != "" {
				reason = incomplete
			}
			if reason == "" {
				if live := activity.blockers(path); len(live) > 0 {
					c.target.Active = true
					reason = "held: " + live[0]
				}
			}
			if reason == "" {
				bytes, err := homeLanesAllocatedBytes(ctx, path, info)
				if err != nil {
					reason = "size could not be verified"
				} else {
					c.target.Bytes = bytes
				}
			}
			if reason != "" {
				c.target.Reason = reason
			} else {
				c.target.Protected = false
				c.target.Action = "report"
				if apply {
					c.target.Action = "delete"
				}
				cite := "A-082"
				if s.kind == homeLanesKindStale {
					cite = "TIN-8526"
				}
				c.target.Reason = fmt.Sprintf("%s idle past %s with no live holder (%s)", s.kind, s.idle, cite)
			}
			annotateCleanupTargetPolicy(&c.target, CleanupTierDestructive, hostReclaimForAction(c.target.Action))
			out = append(out, c)
		}
	}
	return out, warnings
}

func homeLanesGlobMatch(globs []string, name string) bool {
	for _, g := range globs {
		if ok, _ := filepath.Match(g, name); ok {
			return true
		}
	}
	return false
}

// hermesKeepReason applies Hermes's own definition of an orphaned install.
// Empty means the install's checkout is provably gone.
func (p *HomeLanesPlugin) hermesKeepReason(state string, hc config.HomeLanesConfig) string {
	raw, err := os.ReadFile(filepath.Join(state, homeLanesProjectRootRecord))
	if err != nil {
		return "no readable checkout record: an unrecorded install is left alone, as Hermes leaves it"
	}
	root := strings.TrimSpace(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))
	if !filepath.IsAbs(root) {
		return "checkout record is not an absolute path"
	}
	for _, cr := range hc.HermesCheckoutRoots {
		checkoutRoot := filepath.Clean(expandHome(cr, p.home))
		if !bbUnder(root, checkoutRoot) {
			continue
		}
		if info, err := os.Stat(checkoutRoot); err != nil || !info.IsDir() {
			return "checkout root " + checkoutRoot + " is not visible, so a missing checkout proves nothing"
		}
		switch _, err := os.Lstat(root); {
		case err == nil:
			return "live install: checkout " + root + " exists"
		case errors.Is(err, fs.ErrNotExist):
			return ""
		default:
			return "checkout state unknown: " + err.Error()
		}
	}
	return "recorded checkout " + root + " is outside hermes_checkout_roots"
}

func homeLanesAllocatedBytes(ctx context.Context, path string, info os.FileInfo) (int64, error) {
	if info.Mode().IsRegular() {
		return getFileAllocatedBytes(path)
	}
	return getDirAllocatedBytesContext(ctx, path)
}

// homeLanesInGitDir reports whether rel, a path relative to a candidate,
// lies inside a .git directory.
func homeLanesInGitDir(rel string) bool {
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == ".git" {
			return true
		}
	}
	return false
}

// homeLanesBareRepo reports whether dir looks like a bare Git repository:
// a HEAD file with objects and refs directories beside it.
func homeLanesBareRepo(dir string) bool {
	head, err := os.Lstat(filepath.Join(dir, "HEAD"))
	if err != nil || !head.Mode().IsRegular() {
		return false
	}
	return isRealDir(filepath.Join(dir, "objects")) && isRealDir(filepath.Join(dir, "refs"))
}

// keepReason applies the structural rules: agent-session data, ownership, Git
// custody, one device, file locks and idleness. Empty means the tree passed.
func (p *HomeLanesPlugin) keepReason(ctx context.Context, path string, info os.FileInfo, kind string, idle time.Duration, deny *agentSessionDeny, locked map[homeLanesInode]bool) string {
	if why, denied := deny.denied(path); denied {
		return why
	}
	if why, denied := deny.containsDenied(path); denied {
		return why
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != p.uid {
		return "entry belongs to another or unknown user"
	}
	now := p.clock()
	cutoff := now.Add(-idle)
	newest := effectiveModTime(info, now)
	stale := kind == homeLanesKindStale
	var checkouts []string
	count := 0
	err := filepath.WalkDir(path, func(walked string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if count++; count > homeLanesWalkLimit {
			return errors.New("walk bound exceeded")
		}
		if d.Name() == ".git" && !stale {
			return errors.New("contains .git: Git-owner custody")
		}
		if stale {
			rel, _ := filepath.Rel(path, walked)
			inGit := rel != "." && homeLanesInGitDir(filepath.Dir(rel))
			switch {
			case inGit:
			case d.Name() == ".git" && walked == path:
				return errors.New("the entry itself is a .git")
			case d.Name() == ".git":
				if len(checkouts) >= homeLanesMaxCheckouts {
					return fmt.Errorf("more than %d Git checkouts inside", homeLanesMaxCheckouts)
				}
				checkouts = append(checkouts, filepath.Dir(walked))
			case d.Name() == ".hg" || d.Name() == ".jj":
				return errors.New("contains " + d.Name() + ": a repository this plugin cannot inspect")
			case d.IsDir() && homeLanesBareRepo(walked):
				return errors.New("contains a bare Git repository at " + walked)
			}
		}
		if walked != path && d.IsDir() && isAgentSessionDir(walked) {
			return errors.New("contains agent-session directory " + d.Name() + " (TIN-5268)")
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		cur, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || cur.Dev != st.Dev || int(cur.Uid) != p.uid {
			return errors.New("foreign owner, mount or unknown identity inside")
		}
		if locked[homeLanesInodeOf(cur)] {
			return errors.New("a process holds a file lock on " + walked)
		}
		if t := effectiveModTime(fi, now); t.After(newest) {
			newest = t
		}
		if !stale && fi.Mode().IsRegular() && !(kind == homeLanesKindHermes && homeLanesHermesMetadata[d.Name()]) {
			if t, ok := bbAccessTime(fi); ok && t.After(newest) && !t.After(now) {
				newest = t
			}
		}
		if newest.After(cutoff) {
			// The tree is kept for recent activity whatever the rest holds.
			return errHomeLanesRecent
		}
		return nil
	})
	if err != nil && !errors.Is(err, errHomeLanesRecent) {
		return "kept: " + err.Error()
	}
	if newest.After(cutoff) {
		return "recent activity: newest use " + newest.UTC().Format(time.RFC3339) + " is within " + idle.String()
	}
	// Git is asked last, after every cheaper rule passed, and only about an
	// idle entry.
	for _, dir := range checkouts {
		if why := p.homeLanesGitKeepReason(ctx, dir); why != "" {
			return why
		}
	}
	return ""
}

// recheck repeats every check for one tree just before removal.
func (p *HomeLanesPlugin) recheck(ctx context.Context, cfg *config.Config, c homeLanesCandidate) string {
	current, err := os.Lstat(c.target.Path)
	if err != nil || !os.SameFile(c.info, current) {
		return "directory identity changed"
	}
	hc := cfg.HomeLanes
	if c.kind == homeLanesKindHermes {
		if why := p.hermesKeepReason(c.target.Path, hc); why != "" {
			return why
		}
	}
	idle, _ := config.ParseDuration(homeLanesIdleSetting(hc, c.kind))
	activity := p.scan(ctx, bbActivityOptions{OKComms: hc.UninspectableOKComms, Within: []string{c.root}})
	if why := activity.complete(); why != "" {
		return why
	}
	if live := activity.blockers(c.target.Path); len(live) > 0 {
		return "held: " + live[0]
	}
	locked, err := p.locks()
	if err != nil {
		return "file lock table unreadable: " + err.Error()
	}
	deny := newAgentSessionDeny(p.home, p.uid, p.tmpdir, cfg.AgentSession.ExtraPaths, nil)
	return p.keepReason(ctx, c.target.Path, current, c.kind, idle, deny, locked)
}

// reclaim renames a tree aside and removes it. Bazel and Nix trees carry
// read-only directories, which removal needs writable.
//
// If what was renamed is not the inspected entry, because the path was
// replaced after the recheck, nothing is removed. It is renamed back, or, if
// the path was taken again meanwhile, to a .tinyland-held- name that
// finishAside does not match. Only if both renames fail does it stay under
// the aside name, where the next cycle's finishAside would remove it.
func (p *HomeLanesPlugin) reclaim(path string, inspected os.FileInfo) error {
	aside := filepath.Join(filepath.Dir(path), homeLanesAsidePrefix+filepath.Base(path))
	if _, err := os.Lstat(aside); err == nil {
		return errors.New("aside name " + aside + " is taken")
	}
	if err := os.Rename(path, aside); err != nil {
		return err
	}
	moved, err := os.Lstat(aside)
	if err != nil || !os.SameFile(inspected, moved) {
		if rerr := os.Rename(aside, path); rerr != nil {
			held := filepath.Join(filepath.Dir(path), fmt.Sprintf(".tinyland-held-%s-%d", filepath.Base(path), time.Now().UnixNano()))
			if herr := os.Rename(aside, held); herr != nil {
				return fmt.Errorf("renamed entry is not the inspected one and could not be moved off %s: %v; %v", aside, rerr, herr)
			}
			return errors.New("entry was replaced after the recheck; nothing removed, it is at " + held)
		}
		return errors.New("entry was replaced after the recheck; put back, nothing removed")
	}
	return p.removeAside(aside)
}

func (p *HomeLanesPlugin) removeAside(aside string) error {
	_ = filepath.WalkDir(aside, func(walked string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			if fi, ierr := d.Info(); ierr == nil && fi.Mode().Perm()&0o700 != 0o700 {
				_ = os.Chmod(walked, fi.Mode().Perm()|0o700)
			}
		}
		return nil
	})
	return p.remove(aside)
}

// finishAside completes removals an earlier cycle started.
func (p *HomeLanesPlugin) finishAside(hc config.HomeLanesConfig, logger *slog.Logger) {
	roots := append([]string{hc.CacheRoot, hc.HermesInstallsRoot}, hc.StaleWorkRoots...)
	for _, r := range roots {
		if r == "" {
			continue
		}
		root := expandHome(r, p.home)
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), homeLanesAsidePrefix) || !(e.IsDir() || e.Type().IsRegular()) {
				continue
			}
			aside := filepath.Join(root, e.Name())
			if err := p.removeAside(aside); err != nil {
				logger.Warn("home_lanes could not finish a removal", "path", aside, "error", err)
			}
		}
	}
}
