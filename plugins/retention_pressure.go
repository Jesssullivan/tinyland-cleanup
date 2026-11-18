package plugins

// retention_pressure.go is the fill-gated tmp_agents pass (A-084 for sting,
// A-095 for petting-zoo-mini, TIN-5770) and the free-space floor tracking
// that triggers it.
//
// Why: on November 18, 2026 petting-zoo-mini's /nix volume reached 19 MiB
// free. Its temp root /nix/var/tinyland/user/client-tmp held 53.5 GiB, and
// 28 test trees of a seat's runs (about 20 GiB) were all younger than the
// 24 hour tmp_agents age, so no rule could reach them. The free-space floor
// raised the same log line 118 times and changed nothing.
//
// The pass reads statfs on each tmp_agents root every daemon cycle. When the
// root's volume is at or over pressure.used_percent, or under the
// free_space_floor, it removes unheld entries oldest first whose newest
// change is older than pressure.max_age, re-reading statfs after each
// removal, until the volume is under stop_percent and over the floor.
//
// It keeps an entry when:
//
//   - the base tmp_agents guards keep it: the agent-session deny, another
//     user, dev_artifacts.protect_paths, a mount crossing, a non-fixture
//     .git, Git custody, the walk bound, an agent-session directory inside;
//   - a live process has a cwd, open file, mapping, executable or command
//     line under it, or a TMPDIR, TMP, TEMP, TEMPDIR or NIX_BUILD_TOP value
//     in its exec-time environment pointing inside it;
//   - with socket_listener_holds, a listening unix socket is bound inside;
//   - a Nix GC root in gcroots/auto points inside it (read once per pass);
//   - it holds .tinyland-keep or a keep_if_contains match;
//   - it is a nix-shell.* directory that may be the TMPDIR of a live
//     interactive `nix develop` shell (see pressureShells);
//   - the inventory is incomplete, which keeps every entry.
//
// The walk stops at the first entry newer than max_age, so a young tree
// costs a partial walk, and max_dirs_per_cycle counts walked entries only.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

const (
	retentionRuleTmpAgentsPressure = "tmp_agents.pressure"
	// retentionGCRootsAuto is where Nix registers indirect GC roots.
	retentionGCRootsAuto = "/nix/var/nix/gcroots/auto"
	// retentionShellSlack widens a shell's start time: its TMPDIR tree is
	// made by mktemp in its rc script, a moment after the shell starts.
	retentionShellSlack = time.Minute
)

// retentionPressureEnvVars are the exec-time environment values that hold a
// temp tree in the pressure pass. nix develop's rc script sets NIX_BUILD_TOP
// to the same tree as TMPDIR.
var retentionPressureEnvVars = []string{"TMPDIR", "TMP", "TEMP", "TEMPDIR", "NIX_BUILD_TOP"}

// pressureDue reports whether pressure.interval has passed since the last
// pass on root, and records this pass when it has.
func (p *RetentionPlugin) pressureDue(root string, interval time.Duration) bool {
	p.fmu.Lock()
	defer p.fmu.Unlock()
	if p.pressureLast == nil {
		p.pressureLast = map[string]time.Time{}
	}
	now := p.clock()
	if last, ok := p.pressureLast[root]; ok && now.Sub(last) < interval {
		return false
	}
	p.pressureLast[root] = now
	return true
}

// retentionPressureResult is what one pressure cycle did or would do.
type retentionPressureResult struct {
	targets  []CleanupTarget
	freed    int64
	items    int
	warnings []string
	metadata map[string]string
}

func usedPercent(u bbUsage) float64 {
	if u.BytesTotal == 0 {
		return 0
	}
	return float64(u.BytesUsed) * 100 / float64(u.BytesTotal)
}

// pressureCycle runs the pressure pass on every tmp_agents root whose volume
// is over used_percent. With apply false it plans only:
// it ignores the interval and projects the volume's use from the planned
// removals.
func (p *RetentionPlugin) pressureCycle(ctx context.Context, cfg *config.Config, apply bool, logger *slog.Logger) retentionPressureResult {
	res := retentionPressureResult{metadata: map[string]string{}}
	tc := cfg.Retention.TmpAgents
	pc := tc.Pressure
	if !tc.Enabled || !pc.Enabled {
		return res
	}
	if err := cfg.Retention.Validate(); err != nil {
		res.warnings = append(res.warnings, "invalid retention policy, pressure pass skipped: "+err.Error())
		return res
	}
	interval, _ := config.ParseDuration(pc.Interval)
	maxAge, _ := config.ParseDuration(pc.MaxAge)
	stop := pc.StopPercent
	if stop <= 0 {
		stop = pc.UsedPercent
	}
	roots, _ := p.expandRoots(tc.Roots)
	for _, root := range roots {
		u, err := p.usage(root)
		if err != nil {
			res.warnings = append(res.warnings, fmt.Sprintf("tmp_agents.pressure: %s unreadable: %v", root, err))
			continue
		}
		free := u.BytesTotal - u.BytesUsed
		key := "tmp_agents.pressure." + root
		res.metadata[key] = fmt.Sprintf("used_percent=%.1f free_bytes=%d used_trigger=%.1f stop_percent=%.1f", usedPercent(u), free, pc.UsedPercent, stop)
		if usedPercent(u) < pc.UsedPercent {
			continue
		}
		if apply && !p.pressureDue(root, interval) {
			res.metadata[key+".skipped"] = "interval"
			continue
		}
		if why := p.pressureRootRefused(cfg, root); why != "" {
			res.warnings = append(res.warnings, "tmp_agents.pressure: "+root+": "+why)
			continue
		}
		done := func(cur bbUsage) bool { return usedPercent(cur) < stop }
		p.pressureRoot(ctx, cfg, root, u, maxAge, done, apply, logger, &res)
	}
	return res
}

// pressureRootRefused keeps all_names off the home directory and off a root
// that dev_artifacts.protect_paths covers.
func (p *RetentionPlugin) pressureRootRefused(cfg *config.Config, root string) string {
	if !cfg.Retention.TmpAgents.Pressure.AllNames {
		return ""
	}
	if p.home != "" && filepath.Clean(p.home) == root {
		return "all_names is refused on the home directory"
	}
	for _, prefix := range cfg.DevArtifacts.ProtectPaths {
		if prefix != "" && (root == filepath.Clean(prefix) || bbUnder(root, filepath.Clean(prefix))) {
			return "all_names is refused on a root covered by dev_artifacts.protect_paths " + prefix
		}
	}
	return ""
}

// pressureView is what one decision reads besides the entry itself.
type pressureView struct {
	activity *bbActivity
	gcroots  []string
	shells   retentionShells
}

func (p *RetentionPlugin) pressureScan(ctx context.Context, cfg *config.Config, root string) *bbActivity {
	opts := p.activityOptions(cfg, []string{root})
	opts.EnvVars = retentionPressureEnvVars
	opts.StartTimes = true
	a := p.scan(ctx, opts)
	if cfg.Retention.TmpAgents.Pressure.SocketListenerHolds {
		table := p.socketTable
		if table == "" {
			table = retentionSocketTable()
		}
		if table != "" {
			indexSocketListeners(a, table, opts.Within)
		}
	}
	return a
}

func (p *RetentionPlugin) pressureView(ctx context.Context, cfg *config.Config, root string) (pressureView, string) {
	a := p.pressureScan(ctx, cfg, root)
	if why := a.complete(); why != "" {
		return pressureView{}, why
	}
	gc, err := readAutoGCRoots(p.gcrootsAutoDir())
	if err != nil {
		return pressureView{}, "Nix GC roots unreadable: " + err.Error()
	}
	return pressureView{activity: a, gcroots: gc, shells: pressureShells(a, root)}, ""
}

func (p *RetentionPlugin) gcrootsAutoDir() string {
	if p.gcrootsDir != "" {
		return p.gcrootsDir
	}
	return retentionGCRootsAuto
}

func (p *RetentionPlugin) pressureRoot(ctx context.Context, cfg *config.Config, root string, u bbUsage, maxAge time.Duration, done func(bbUsage) bool, apply bool, logger *slog.Logger, res *retentionPressureResult) {
	tc := cfg.Retention.TmpAgents
	pc := tc.Pressure
	key := "tmp_agents.pressure." + root
	view, why := p.pressureView(ctx, cfg, root)
	if why != "" {
		res.metadata[key+".kept_all"] = why
		res.warnings = append(res.warnings, "tmp_agents.pressure "+root+": every entry kept: "+why)
		return
	}
	entries, err := tempScanRootsOldestFirst(root)
	if err != nil {
		res.warnings = append(res.warnings, fmt.Sprintf("tmp_agents.pressure root %s unreadable: %v", root, err))
		return
	}
	deny := newAgentSessionDeny(p.home, p.uid, p.tmpdir, cfg.AgentSession.ExtraPaths, nil)
	git := newDevArtifactGitTracker()
	cur := u
	walked, removed, eligible := 0, 0, 0
	freshAt := p.clock()
	stopped := "entries exhausted"
	for _, e := range entries {
		if ctx.Err() != nil {
			stopped = "interrupted"
			break
		}
		if done(cur) {
			stopped = "volume under stop_percent and over the floor"
			break
		}
		if !p.pressureCandidate(pc, tc.NameGlobs, e) {
			continue
		}
		if tc.MaxDirsPerCycle > 0 && walked >= tc.MaxDirsPerCycle {
			stopped = fmt.Sprintf("max_dirs_per_cycle %d walked", tc.MaxDirsPerCycle)
			res.warnings = append(res.warnings, "tmp_agents.pressure "+root+": "+stopped+"; the rest wait for the next pass")
			break
		}
		t, didWalk := p.decidePressure(ctx, cfg, root, e, maxAge, view, deny, git)
		if didWalk {
			walked++
		}
		res.targets = append(res.targets, t)
		if t.Protected {
			continue
		}
		eligible++
		if !apply || t.Action != "delete" {
			// A plan, or report only: project the volume as if it went.
			cur.BytesUsed -= min(cur.BytesUsed, uint64(t.Bytes))
			continue
		}
		if p.clock().Sub(freshAt) > retentionActivityMaxAge {
			fresh, why := p.pressureView(ctx, cfg, root)
			if why != "" {
				res.warnings = append(res.warnings, "tmp_agents.pressure "+root+": stopped at removal time: "+why)
				stopped = why
				break
			}
			view, freshAt = fresh, p.clock()
		}
		again, _ := p.decidePressure(ctx, cfg, root, e, maxAge, view, deny, newDevArtifactGitTracker())
		if again.Action != "delete" {
			if logger != nil {
				logger.Info("retention pressure keeps entry at removal time", "path", e.path, "reason", again.Reason)
			}
			continue
		}
		if cur2, err := os.Lstat(e.path); err != nil || !os.SameFile(e.info, cur2) {
			continue
		}
		if err := p.remove(e.path); err != nil {
			if logger != nil {
				logger.Warn("retention pressure could not remove entry", "path", e.path, "error", err)
			}
			continue
		}
		removed++
		res.freed += t.Bytes
		res.items++
		if logger != nil {
			logger.Info("retention pressure removed entry", "path", e.path, "bytes", t.Bytes, "max_age", formatDevArtifactAge(maxAge),
				"used_percent", fmt.Sprintf("%.1f", usedPercent(cur)))
		}
		if nu, err := p.usage(root); err == nil {
			cur = nu
		}
	}
	res.metadata[key+".walked"] = fmt.Sprint(walked)
	res.metadata[key+".eligible_count"] = fmt.Sprint(eligible)
	res.metadata[key+".removed_count"] = fmt.Sprint(removed)
	res.metadata[key+".stopped"] = stopped
	res.metadata[key+".used_percent_after"] = fmt.Sprintf("%.1f", usedPercent(cur))
}

// pressureCandidate applies the type and name filters.
func (p *RetentionPlugin) pressureCandidate(pc config.RetentionTmpAgentsPressureConfig, globs []string, e tempScanRoot) bool {
	if e.name == config.RetentionKeepMarker {
		return false
	}
	mode := e.info.Mode()
	switch {
	case mode.IsDir():
	case mode.IsRegular() && pc.IncludeFiles:
	default:
		return false // links, sockets, FIFOs and devices never are
	}
	if retentionNameMatches(e.name, pc.ExcludeGlobs) {
		return false
	}
	return pc.AllNames || retentionNameMatches(e.name, globs)
}

// decidePressure decides one entry. The cheap checks come before the walk,
// and the walk stops at the first entry newer than maxAge. didWalk reports
// whether the entry was walked.
func (p *RetentionPlugin) decidePressure(ctx context.Context, cfg *config.Config, root string, e tempScanRoot, maxAge time.Duration, view pressureView, deny *agentSessionDeny, git *devArtifactGitTracker) (target CleanupTarget, didWalk bool) {
	pc := cfg.Retention.TmpAgents.Pressure
	path := e.path
	target = CleanupTarget{Type: "tmp-agents-pressure", Name: e.name, Path: path, Action: "keep", Protected: true}
	defer func() { annotateCleanupTargetPolicy(&target, CleanupTierWarm, hostReclaimForAction(target.Action)) }()

	if why, denied := deny.denied(path); denied {
		target.Reason = why
		return target, false
	}
	if why, denied := deny.containsDenied(path); denied {
		target.Reason = why
		return target, false
	}
	for _, prefix := range cfg.DevArtifacts.ProtectPaths {
		if prefix != "" && strings.HasPrefix(path, prefix) {
			target.Reason = "covered by dev_artifacts.protect_paths " + prefix
			return target, false
		}
	}
	st, ok := e.info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != p.uid {
		target.Reason = "belongs to another or unknown user"
		return target, false
	}
	if live := view.activity.blockers(path); len(live) > 0 {
		target.Active = true
		target.Reason = "live reference: " + live[0]
		return target, false
	}
	for _, g := range view.gcroots {
		if g == path || bbUnder(g, path) {
			target.Active = true
			target.Reason = "holds a Nix GC root link " + g
			return target, false
		}
	}
	if why := view.shells.holds(root, e); why != "" {
		target.Active = true
		target.Reason = why
		return target, false
	}

	now := p.clock()
	cut := now.Add(-maxAge)
	keep := append([]string{config.RetentionKeepMarker}, pc.KeepIfContains...)
	var stats retentionTreeStats
	limit := cfg.Retention.TmpAgents.MaxEntriesPerDir
	walkErr := filepath.WalkDir(path, func(walked string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		stats.entries++
		if limit > 0 && stats.entries > limit {
			return fmt.Errorf("walk bound %d exceeded", limit)
		}
		if walked != path && retentionNameMatches(d.Name(), keep) {
			return errors.New("holds keep marker " + d.Name())
		}
		if d.Name() == ".git" {
			if why := retentionFixtureRepo(path, walked, d, cfg.Retention.TmpAgents.FixtureRepoParents); why != "" {
				return errors.New("contains a Git repository (.git): Git custody (" + why + ")")
			}
			stats.fixtureRepos++
		}
		if walked != path && d.IsDir() && isAgentSessionDir(walked) {
			return errors.New("contains agent-session directory " + d.Name() + " (TIN-5268)")
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		cur, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || uint64(cur.Dev) != uint64(st.Dev) || int(cur.Uid) != p.uid {
			return errors.New("foreign owner, mount or unknown identity inside")
		}
		t := effectiveModTime(fi, now)
		if ct, ok := changeTime(fi); ok && !ct.After(now.Add(futureMtimeTolerance)) && ct.After(t) {
			t = ct
		}
		if t.After(stats.newest) {
			stats.newest = t
		}
		if t.After(cut) {
			// The age test fails here: the rest of the walk would change
			// nothing, so it stops.
			return errRetentionRecent
		}
		stats.allocated += int64(cur.Blocks) * 512
		return nil
	})
	didWalk = true
	target.Bytes = stats.allocated
	switch {
	case errors.Is(walkErr, errRetentionRecent):
		target.Reason = fmt.Sprintf("recent change %s is within %s (walk stopped there)", stats.newest.UTC().Format(time.RFC3339), formatDevArtifactAge(maxAge))
		return target, didWalk
	case walkErr != nil:
		target.Reason = "kept: " + walkErr.Error()
		return target, didWalk
	}
	if why := retentionGitReason(ctx, git, path); why != "" {
		target.Reason = why
		return target, didWalk
	}
	target.Protected = false
	reason := fmt.Sprintf("under pressure: newest change %s is older than %s with no live reference", stats.newest.UTC().Format(time.RFC3339), formatDevArtifactAge(maxAge))
	if pc.Delete {
		target.Action = "delete"
		target.Reason = reason
	} else {
		target.Action = "report"
		target.Reason = "would remove (report only; set retention.tmp_agents.pressure.delete): " + reason
	}
	return target, didWalk
}

var errRetentionRecent = errors.New("recent change")

// readAutoGCRoots lists the targets of the indirect Nix GC roots. A missing
// directory (no Nix) lists nothing; any other error is returned, and the
// caller keeps every entry.
func readAutoGCRoots(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		t, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // not a link: Nix only writes links here
		}
		if !filepath.IsAbs(t) {
			t = filepath.Join(dir, t)
		}
		out = append(out, filepath.Clean(t))
	}
	return out, nil
}

// retentionShells are the live interactive nix develop shells per root.
//
// `nix develop` with no command execs `bash --rcfile <TMPDIR>/nix-shell.XXXXXX`
// (Nix src/nix/develop.cc). The rc script deletes that file, makes a fresh
// tree with `mktemp -d -t nix-shell.XXXXXX` and exports it as NIX_BUILD_TOP,
// TMPDIR, TMP, TEMP and TEMPDIR inside the shell. Those exports never reach
// the shell's exec-time environment, which is all that /proc/<pid>/environ
// and kern.procargs2 show, so an idle shell has no holder on its own tree.
// What stays visible is the shell's command line naming the rc file under
// the root, and its start time. The tree is made after the shell starts, so
// a nix-shell.* entry whose ctime is before the earliest such shell's start
// cannot be its tree; every later one may be, and is kept.
type retentionShells map[string]retentionShell

type retentionShell struct {
	pid     int
	start   time.Time
	unknown bool
}

func pressureShells(a *bbActivity, root string) retentionShells {
	out := retentionShells{}
	if a == nil {
		return out
	}
	roots := uniquePaths(root, resolvedOrSelf(root))
	for _, proc := range a.procs {
		rc := retentionRcFile(proc.cmdline)
		if rc == "" {
			continue
		}
		dir := filepath.Dir(rc)
		in := false
		for _, r := range roots {
			if dir == r {
				in = true
				break
			}
		}
		if !in {
			continue
		}
		s := out[root]
		switch {
		case proc.start.IsZero():
			s = retentionShell{pid: proc.pid, unknown: true}
		case s.unknown:
		case s.pid == 0 || proc.start.Before(s.start):
			s = retentionShell{pid: proc.pid, start: proc.start}
		}
		out[root] = s
	}
	return out
}

// retentionRcFile returns the rc file of an interactive nix develop shell
// command line ("<shell> --rcfile <dir>/nix-shell.XXXXXX"), or "".
func retentionRcFile(cmdline string) string {
	f := strings.Fields(cmdline)
	for i := 0; i+1 < len(f); i++ {
		if f[i] != "--rcfile" {
			continue
		}
		rc := filepath.Clean(f[i+1])
		if filepath.IsAbs(rc) && strings.HasPrefix(filepath.Base(rc), "nix-shell.") {
			return rc
		}
	}
	return ""
}

// holds returns why a nix-shell.* entry may belong to a live interactive
// shell, or "".
func (s retentionShells) holds(root string, e tempScanRoot) string {
	sh, ok := s[root]
	if !ok || !e.info.IsDir() || !strings.HasPrefix(e.name, "nix-shell.") {
		return ""
	}
	if sh.unknown {
		return fmt.Sprintf("may be the TMPDIR of interactive nix develop shell pid %d (start time unknown)", sh.pid)
	}
	ct, ok := changeTime(e.info)
	if !ok || !ct.Before(sh.start.Add(-retentionShellSlack)) {
		return fmt.Sprintf("may be the TMPDIR of interactive nix develop shell pid %d started %s", sh.pid, sh.start.UTC().Format(time.RFC3339))
	}
	return ""
}
