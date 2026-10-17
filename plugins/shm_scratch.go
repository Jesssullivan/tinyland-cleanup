package plugins

// shm_scratch.go reclaims idle top-level directories from RAM-backed scratch
// roots such as /dev/shm (lab R-C856, TIN-5770, operator ruling 2026-10-17).
//
// Why: on sting 2026-10-17, /dev/shm held 12 GiB of agent scratch whose
// writers had gone (a 6 GiB Cargo target/, review trees, JSON dumps). tmpfs
// pages leave RAM only through swap, so those files pinned 7.1 GiB of RAM and
// kept the 7 GiB zram swap full. Under the next memory pressure the kernel
// could reclaim only page cache. Agent TMPDIRs on disk have quotas and
// cleanup lanes; /dev/shm had neither.
//
// Rule (R-C856): a top-level directory of a configured root is removed when
// it has been idle longer than idle_after (never shorter than 24h) and no
// process holds it. Everything else is kept:
//
//   - only real directories at the top level; files (POSIX shm segments,
//     sockets, lock files) and symlinks are never touched;
//   - only on a tmpfs root; any other file system is skipped;
//   - only directories owned by this user, whose every entry is this user's
//     and on the root's device, with no .git anywhere;
//   - never agent-session data (TIN-5268: claude-*, codex*, .tmp*, pi-* at
//     the top, a session directory such as .claude inside, or anything the
//     compiled deny set covers);
//   - idle means the newest modification time in the tree (ctime when an
//     mtime is in the future) is older than idle_after;
//   - no live holder: no process of any user this daemon can inspect has its
//     cwd or root there, an open file there, or a file there mapped into
//     memory, and none names it on its command line. A process of this user
//     whose fds cannot be read makes the inventory incomplete, and an
//     incomplete inventory keeps every directory;
//   - every check runs again immediately before removal.
//
// mode: report plans only; apply removes. The plugin is level-independent:
// it runs every cycle, outside the disk-pressure ladder, because a RAM-backed
// tree costs memory, not disk.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

const shmScratchPluginName = "shm_scratch"

// shmScratchWalkLimit bounds the entries one tree walk may visit.
const shmScratchWalkLimit = 200000

// ShmScratchPlugin removes idle, unheld top-level directories from RAM-backed
// scratch roots (R-C856).
type ShmScratchPlugin struct {
	// Test seams; nil means the real implementation.
	now       func() time.Time
	activity  func(ctx context.Context, opts bbActivityOptions) *bbActivity
	isTmpfs   func(path string) (bool, error)
	removeAll func(path string) error
	uid       int
	home      string
	tmpdir    string
}

// NewShmScratchPlugin creates the RAM-backed scratch reaper.
func NewShmScratchPlugin() *ShmScratchPlugin {
	home, _ := os.UserHomeDir()
	return &ShmScratchPlugin{uid: os.Geteuid(), home: home, tmpdir: os.Getenv("TMPDIR")}
}

// Name returns the plugin identifier.
func (p *ShmScratchPlugin) Name() string { return shmScratchPluginName }

// Description returns the plugin description.
func (p *ShmScratchPlugin) Description() string {
	return "Removes idle, unheld top-level directories from RAM-backed scratch roots such as /dev/shm (R-C856)"
}

// SupportedPlatforms returns supported platforms.
func (p *ShmScratchPlugin) SupportedPlatforms() []string { return []string{PlatformLinux} }

// Enabled checks if the plugin is enabled.
func (p *ShmScratchPlugin) Enabled(cfg *config.Config) bool { return cfg.Enable.ShmScratch }

// LevelIndependent runs the plugin every cycle: a tmpfs tree costs memory, so
// disk pressure is the wrong trigger.
func (p *ShmScratchPlugin) LevelIndependent() bool { return true }

func (p *ShmScratchPlugin) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *ShmScratchPlugin) scan(ctx context.Context, opts bbActivityOptions) *bbActivity {
	if p.activity != nil {
		return p.activity(ctx, opts)
	}
	return bbScanActivity(ctx, opts)
}

func (p *ShmScratchPlugin) tmpfs(path string) (bool, error) {
	if p.isTmpfs != nil {
		return p.isTmpfs(path)
	}
	return shmIsTmpfs(path)
}

func (p *ShmScratchPlugin) remove(path string) error {
	if p.removeAll != nil {
		return p.removeAll(path)
	}
	return os.RemoveAll(path)
}

// shmCandidate is one top-level directory and the decision about it.
type shmCandidate struct {
	target CleanupTarget
	info   os.FileInfo
	root   string
}

// PlanCleanup reports what the reaper would do, removing nothing.
func (p *ShmScratchPlugin) PlanCleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupPlan {
	plan := CleanupPlan{Plugin: p.Name(), Level: level.String()}
	sc := cfg.ShmScratch
	if err := sc.Validate(); err != nil {
		plan.SkipReason = "invalid shm_scratch policy: " + err.Error()
		return plan
	}
	candidates, warnings := p.plan(ctx, cfg, sc.Mode == config.ShmScratchModeApply)
	plan.Warnings = warnings
	for _, c := range candidates {
		plan.Targets = append(plan.Targets, c.target)
		if !c.target.Protected && c.target.Action == "delete" {
			plan.EstimatedBytesFreed += c.target.Bytes
		}
	}
	plan.WouldRun = sc.Mode == config.ShmScratchModeApply
	if !plan.WouldRun {
		plan.SkipReason = "shm_scratch.mode is report"
	}
	plan.Summary = fmt.Sprintf("%d top-level director(ies) inspected under %v", len(candidates), sc.Roots)
	plan.Metadata = map[string]string{"mode": sc.Mode, "idle_after": sc.IdleAfter}
	annotateCleanupPlanTargetAccounting(&plan)
	return plan
}

// Cleanup removes eligible directories in apply mode; in report mode it logs
// the plan and removes nothing.
func (p *ShmScratchPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: level}
	sc := cfg.ShmScratch
	if err := sc.Validate(); err != nil {
		result.Error = err
		return result
	}
	apply := sc.Mode == config.ShmScratchModeApply
	candidates, warnings := p.plan(ctx, cfg, apply)
	for _, w := range warnings {
		logger.Warn("shm_scratch", "warning", w)
	}
	for _, c := range candidates {
		t := c.target
		switch {
		case t.Protected:
			logger.Debug("shm_scratch keeps directory", "path", t.Path, "reason", t.Reason)
		case !apply:
			logger.Info("shm_scratch would remove directory (report mode)", "path", t.Path, "bytes", t.Bytes, "reason", t.Reason)
		default:
			// Fresh checks immediately before mutation: a plan is no lease.
			if why := p.recheck(ctx, cfg, c); why != "" {
				logger.Info("shm_scratch keeps directory at removal time", "path", t.Path, "reason", why)
				continue
			}
			if err := p.remove(t.Path); err != nil {
				logger.Warn("shm_scratch could not remove directory", "path", t.Path, "error", err)
				continue
			}
			logger.Info("shm_scratch removed idle directory", "path", t.Path, "bytes", t.Bytes, "idle_after", sc.IdleAfter)
			result.BytesFreed += t.Bytes
			result.EstimatedBytesFreed += t.Bytes
			result.ItemsCleaned++
		}
	}
	return result
}

// plan inspects every top-level directory of every root. It never removes.
func (p *ShmScratchPlugin) plan(ctx context.Context, cfg *config.Config, apply bool) ([]shmCandidate, []string) {
	sc := cfg.ShmScratch
	idle, _ := config.ParseDuration(sc.IdleAfter)
	deny := newAgentSessionDeny(p.home, p.uid, p.tmpdir, cfg.AgentSession.ExtraPaths, nil)
	var warnings []string
	var roots []string
	for _, r := range sc.Roots {
		root := filepath.Clean(r)
		ok, err := p.tmpfs(root)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("root %s skipped: %v", root, err))
			continue
		}
		if !ok {
			warnings = append(warnings, fmt.Sprintf("root %s skipped: not a tmpfs (R-C856 covers RAM-backed scratch only)", root))
			continue
		}
		roots = append(roots, root)
	}
	if len(roots) == 0 {
		return nil, warnings
	}
	activity := p.scan(ctx, bbActivityOptions{OKComms: sc.UninspectableOKComms, Within: roots})
	incomplete := activity.complete()
	var out []shmCandidate
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("root %s unreadable: %v", root, err))
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		inspected := 0
		for _, e := range entries {
			if ctx.Err() != nil {
				return out, append(warnings, "cycle interrupted")
			}
			path := filepath.Join(root, e.Name())
			info, err := os.Lstat(path)
			if err != nil || !info.IsDir() {
				continue // files, sockets, symlinks and vanished entries are never candidates
			}
			if sc.MaxRootsPerCycle > 0 && inspected >= sc.MaxRootsPerCycle {
				warnings = append(warnings, fmt.Sprintf("root %s: max_roots_per_cycle %d reached; the rest wait for the next cycle", root, sc.MaxRootsPerCycle))
				break
			}
			inspected++
			c := shmCandidate{info: info, root: root, target: CleanupTarget{
				Type: "shm-scratch-dir", Name: e.Name(), Path: path, Action: "protect", Protected: true,
			}}
			reason := p.keepReason(ctx, path, info, idle, deny)
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
				if apply {
					c.target.Action = "delete"
				} else {
					c.target.Action = "report"
				}
				c.target.Reason = fmt.Sprintf("idle past %s with no live holder (R-C856)", sc.IdleAfter)
			}
			annotateCleanupTargetPolicy(&c.target, CleanupTierDestructive, hostReclaimForAction(c.target.Action))
			out = append(out, c)
		}
	}
	return out, warnings
}

// keepReason applies the structural rules: identity, ownership, agent-session
// data, Git custody, one device, and idleness. Empty means the tree passed.
func (p *ShmScratchPlugin) keepReason(ctx context.Context, path string, info os.FileInfo, idle time.Duration, deny *agentSessionDeny) string {
	name := filepath.Base(path)
	for _, glob := range agentSessionTempNames {
		if ok, _ := filepath.Match(glob, name); ok {
			return "agent-session data (TIN-5268)"
		}
	}
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
	cutoff := p.clock().Add(-idle)
	now := p.clock()
	newest := effectiveModTime(info, now)
	count := 0
	err := filepath.WalkDir(path, func(walked string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > shmScratchWalkLimit {
			return errors.New("walk bound exceeded")
		}
		if d.Name() == ".git" {
			return errors.New("contains .git: Git-owner custody")
		}
		if walked != path && d.IsDir() && isAgentSessionDirName(d.Name()) {
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
		if t := effectiveModTime(fi, now); t.After(newest) {
			newest = t
		}
		return nil
	})
	if err != nil {
		return "kept: " + err.Error()
	}
	if newest.After(cutoff) {
		return "recent activity: newest change " + newest.UTC().Format(time.RFC3339) + " is within " + idle.String()
	}
	return ""
}

// recheck repeats every check for one directory just before removal.
func (p *ShmScratchPlugin) recheck(ctx context.Context, cfg *config.Config, c shmCandidate) string {
	current, err := os.Lstat(c.target.Path)
	if err != nil || !os.SameFile(c.info, current) {
		return "directory identity changed"
	}
	if ok, err := p.tmpfs(c.root); err != nil || !ok {
		return "root is no longer a tmpfs"
	}
	idle, _ := config.ParseDuration(cfg.ShmScratch.IdleAfter)
	deny := newAgentSessionDeny(p.home, p.uid, p.tmpdir, cfg.AgentSession.ExtraPaths, nil)
	if why := p.keepReason(ctx, c.target.Path, current, idle, deny); why != "" {
		return why
	}
	activity := p.scan(ctx, bbActivityOptions{OKComms: cfg.ShmScratch.UninspectableOKComms, Within: []string{c.root}})
	if why := activity.complete(); why != "" {
		return why
	}
	if live := activity.blockers(c.target.Path); len(live) > 0 {
		return "held: " + live[0]
	}
	return ""
}
