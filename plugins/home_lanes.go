package plugins

// home_lanes.go reclaims stale trees that no other lane covers from a
// developer home (lab amendment A-081, TIN-5770, operator ruling 2026-11-05).
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
	uid          int
	home         string
	tmpdir       string
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
	return "Removes stale ~/.cache lanes and orphaned Hermes installs that no process holds (lab A-081)"
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
)

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
	plan.Metadata = map[string]string{"mode": hc.Mode, "cache_idle_after": hc.CacheIdleAfter, "hermes_idle_after": hc.HermesIdleAfter}
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
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].target.Bytes > candidates[j].target.Bytes })
	removed := 0
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
			// Fresh checks immediately before mutation: a plan is no lease.
			if why := p.recheck(ctx, cfg, c); why != "" {
				logger.Info("home_lanes keeps tree at removal time", "path", t.Path, "reason", why)
				continue
			}
			if err := p.reclaim(t.Path); err != nil {
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

// plan inspects every candidate of both roots. It never removes.
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
			if err != nil || !info.IsDir() {
				continue // files, sockets, symlinks and vanished entries are never candidates
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
				bytes, err := getDirAllocatedBytesContext(ctx, path)
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
				c.target.Reason = fmt.Sprintf("%s idle past %s with no live holder (A-081)", s.kind, s.idle)
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
		return "directory belongs to another or unknown user"
	}
	now := p.clock()
	cutoff := now.Add(-idle)
	newest := effectiveModTime(info, now)
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
		if d.Name() == ".git" {
			return errors.New("contains .git: Git-owner custody")
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
		if fi.Mode().IsRegular() && !(kind == homeLanesKindHermes && homeLanesHermesMetadata[d.Name()]) {
			if t, ok := bbAccessTime(fi); ok && t.After(newest) && !t.After(now) {
				newest = t
			}
		}
		return nil
	})
	if err != nil {
		return "kept: " + err.Error()
	}
	if newest.After(cutoff) {
		return "recent activity: newest use " + newest.UTC().Format(time.RFC3339) + " is within " + idle.String()
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
	idleSetting := hc.CacheIdleAfter
	if c.kind == homeLanesKindHermes {
		idleSetting = hc.HermesIdleAfter
		if why := p.hermesKeepReason(c.target.Path, hc); why != "" {
			return why
		}
	}
	idle, _ := config.ParseDuration(idleSetting)
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
func (p *HomeLanesPlugin) reclaim(path string) error {
	aside := filepath.Join(filepath.Dir(path), homeLanesAsidePrefix+filepath.Base(path))
	if err := os.Rename(path, aside); err != nil {
		return err
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
	for _, r := range []string{hc.CacheRoot, hc.HermesInstallsRoot} {
		if r == "" {
			continue
		}
		root := expandHome(r, p.home)
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), homeLanesAsidePrefix) || !e.IsDir() {
				continue
			}
			aside := filepath.Join(root, e.Name())
			if err := p.removeAside(aside); err != nil {
				logger.Warn("home_lanes could not finish a removal", "path", aside, "error", err)
			}
		}
	}
}
