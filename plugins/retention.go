package plugins

// retention.go owns age retention for stale temp, cache and build output
// (TIN-7495, TIN-3082, operator ruling 2026-10-29 "Fix tinyland-cleanup
// first").
//
// Why: on 2026-10-27 sting's 64 GiB /srv/fast-local/tmp-agents quota had
// 2.1 GB free with 2409 nix-shell.* roots whose pytest-of-jess subtrees
// allocated about 13.3 GB, rebuildable Cargo build directories under
// /srv/cache regrew with no retention, and pzm's data volume reached 0 bytes
// free. The dev_artifacts temp lane only reaps Nix roots at aggressive or
// critical level and judges them by the root's own mtime.
//
// Rules:
//
//   - tmp_agents: a top-level directory of a configured root whose name
//     matches name_globs (default nix-shell.*) is removed when nothing inside
//     changed (mtime or ctime) within max_age (default 24h) and no live
//     process references it: no cwd, root, open file, memory mapping or
//     command line there, and no TMPDIR, TMP, TEMP or TEMPDIR pointing inside.
//   - cargo_targets: a Cargo target or build directory (CACHEDIR.TAG or
//     .rustc_info.json) under a configured root whose newest change is older
//     than max_age (default 7d) is removed, never while a cargo or rustc
//     process runs anywhere on the host.
//   - free_space_floor: every listed volume with less than floor_gib free
//     raises an alert line. It never deletes.
//   - darwin_leftovers: superseded Autodesk Fusion webdeploy builds and the
//     Claude desktop vm_bundles directory are reported with their sizes. It
//     never deletes.
//
// Every deleting rule reports only until its own delete flag is true. A tree
// is always kept when it belongs to another user, crosses a mount, contains
// a .git, holds agent-session data (TIN-5268), is covered by
// dev_artifacts.protect_paths, sits in a dirty or unpushed Git work tree,
// exceeds its walk bound, or when the process inventory is incomplete. Sizes
// are allocated blocks, so sparse files count what they occupy. Every check
// runs again immediately before removal.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

const retentionPluginName = "retention"

const (
	retentionRuleTmpAgents       = "tmp_agents"
	retentionRuleCargoTargets    = "cargo_targets"
	retentionRuleDarwinLeftovers = "darwin_leftovers"
)

// retentionEnvVars are the environment variables whose values hold a temp
// root: nix-shell points TMPDIR at its nix-shell.* directory.
var retentionEnvVars = []string{"TMPDIR", "TMP", "TEMP", "TEMPDIR"}

// retentionActivityMaxAge is how old a process inventory may be when a
// removal is decided; an older one is taken again.
const retentionActivityMaxAge = 30 * time.Second

// RetentionPlugin applies the age retention rules (TIN-7495, TIN-3082).
type RetentionPlugin struct {
	// Test seams; nil or zero means the real implementation.
	now       func() time.Time
	activity  func(ctx context.Context, opts bbActivityOptions) *bbActivity
	removeAll func(path string) error
	statfs    func(path string) (bbUsage, error)
	lsof      string
	uid       int
	home      string
	user      string
	tmpdir    string
	goos      string

	mu      sync.Mutex
	lastRun time.Time
}

// NewRetentionPlugin creates the retention plugin with the host environment.
func NewRetentionPlugin() *RetentionPlugin {
	home, _ := os.UserHomeDir()
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	lsof, _ := exec.LookPath("lsof")
	return &RetentionPlugin{
		uid: os.Geteuid(), home: home, user: name, tmpdir: os.Getenv("TMPDIR"),
		lsof: lsof, goos: runtime.GOOS,
	}
}

// Name returns the plugin identifier.
func (p *RetentionPlugin) Name() string { return retentionPluginName }

// Description returns the plugin description.
func (p *RetentionPlugin) Description() string {
	return "Age retention for stale agent temp roots and Cargo build dirs, a free-space floor alert, and report-only Darwin leftovers (TIN-7495, TIN-3082)"
}

// SupportedPlatforms returns supported platforms.
func (p *RetentionPlugin) SupportedPlatforms() []string {
	return []string{PlatformLinux, PlatformDarwin}
}

// Enabled checks if the plugin is enabled.
func (p *RetentionPlugin) Enabled(cfg *config.Config) bool { return cfg.Enable.Retention }

// LevelIndependent runs the plugin every cycle: age retention and the floor
// alert do not wait for the pressure ladder. retention.interval throttles it.
func (p *RetentionPlugin) LevelIndependent() bool { return true }

func (p *RetentionPlugin) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *RetentionPlugin) scan(ctx context.Context, opts bbActivityOptions) *bbActivity {
	if p.activity != nil {
		return p.activity(ctx, opts)
	}
	return bbScanActivity(ctx, opts)
}

func (p *RetentionPlugin) remove(path string) error {
	if p.removeAll != nil {
		return p.removeAll(path)
	}
	if err := os.RemoveAll(path); err != nil {
		// Nix and Bazel copies leave read-only directories behind.
		_ = bbMakeWritable(path)
		return os.RemoveAll(path)
	}
	return nil
}

func (p *RetentionPlugin) usage(path string) (bbUsage, error) {
	if p.statfs != nil {
		return p.statfs(path)
	}
	return bbStatfsUsage(path)
}

func (p *RetentionPlugin) platform() string {
	if p.goos != "" {
		return p.goos
	}
	return runtime.GOOS
}

// retentionCandidate is one inspected tree and the decision about it.
type retentionCandidate struct {
	rule   string
	root   string
	path   string
	info   os.FileInfo
	maxAge time.Duration
	target CleanupTarget
}

// retentionPass is one evaluation of every rule.
type retentionPass struct {
	candidates []retentionCandidate
	reports    []CleanupTarget
	warnings   []string
	alerts     []string
	metadata   map[string]string
}

func (p *RetentionPlugin) activityOptions(cfg *config.Config, roots []string) bbActivityOptions {
	var within []string
	for _, r := range roots {
		within = append(within, uniquePaths(r, resolvedOrSelf(r))...)
	}
	return bbActivityOptions{
		Lsof:        p.lsof,
		LsofTimeout: 120 * time.Second,
		UseLsof:     p.platform() == PlatformDarwin,
		OKComms:     cfg.Retention.UninspectableOKComms,
		Within:      within,
		EnvVars:     retentionEnvVars,
	}
}

// expandRoots expands and cleans roots and keeps the existing directories.
func (p *RetentionPlugin) expandRoots(roots []string) (existing []string, missing []string) {
	seen := map[string]bool{}
	for _, r := range roots {
		r = filepath.Clean(config.ExpandUserPath(r, p.home, p.user))
		if seen[r] || !filepath.IsAbs(r) || r == "/" {
			continue
		}
		seen[r] = true
		if info, err := os.Stat(r); err == nil && info.IsDir() {
			existing = append(existing, r)
		} else {
			missing = append(missing, r)
		}
	}
	return existing, missing
}

// evaluate builds the whole pass. It never removes anything.
func (p *RetentionPlugin) evaluate(ctx context.Context, cfg *config.Config) *retentionPass {
	rc := cfg.Retention
	pass := &retentionPass{metadata: map[string]string{}}
	if err := rc.Validate(); err != nil {
		pass.warnings = append(pass.warnings, "invalid retention policy, deleting rules skipped: "+err.Error())
		p.evaluateFloors(cfg, nil, pass)
		return pass
	}

	var tmpRoots, cargoRoots []string
	if rc.TmpAgents.Enabled {
		var missing []string
		tmpRoots, missing = p.expandRoots(rc.TmpAgents.Roots)
		for _, m := range missing {
			pass.metadata["tmp_agents.missing_root."+m] = "true"
		}
	}
	if rc.CargoTargets.Enabled {
		var missing []string
		cargoRoots, missing = p.expandRoots(rc.CargoTargets.Roots)
		for _, m := range missing {
			pass.metadata["cargo_targets.missing_root."+m] = "true"
		}
	}

	if len(tmpRoots)+len(cargoRoots) > 0 {
		activity := p.scan(ctx, p.activityOptions(cfg, append(append([]string{}, tmpRoots...), cargoRoots...)))
		deny := newAgentSessionDeny(p.home, p.uid, p.tmpdir, cfg.AgentSession.ExtraPaths, nil)
		git := newDevArtifactGitTracker()
		if len(tmpRoots) > 0 {
			p.evaluateTmpAgents(ctx, cfg, tmpRoots, activity, deny, git, pass)
		}
		if len(cargoRoots) > 0 {
			p.evaluateCargoTargets(ctx, cfg, cargoRoots, activity, deny, git, pass)
		}
	}

	p.evaluateFloors(cfg, append(append([]string{}, tmpRoots...), cargoRoots...), pass)
	if rc.DarwinLeftovers.Enabled && p.platform() == PlatformDarwin {
		p.evaluateDarwinLeftovers(ctx, cfg, pass)
	}
	p.summarize(pass)
	return pass
}

func (p *RetentionPlugin) evaluateTmpAgents(ctx context.Context, cfg *config.Config, roots []string, activity *bbActivity, deny *agentSessionDeny, git *devArtifactGitTracker, pass *retentionPass) {
	tc := cfg.Retention.TmpAgents
	maxAge, _ := config.ParseDuration(tc.MaxAge)
	for _, root := range roots {
		entries, err := tempScanRootsOldestFirst(root)
		if err != nil {
			pass.warnings = append(pass.warnings, fmt.Sprintf("tmp_agents root %s unreadable: %v", root, err))
			continue
		}
		inspected := 0
		for _, e := range entries {
			if ctx.Err() != nil {
				pass.warnings = append(pass.warnings, "tmp_agents pass interrupted")
				return
			}
			if !e.info.IsDir() || !retentionNameMatches(e.name, tc.NameGlobs) {
				continue // files, symlinks and other names are never candidates
			}
			if tc.MaxDirsPerCycle > 0 && inspected >= tc.MaxDirsPerCycle {
				pass.warnings = append(pass.warnings, fmt.Sprintf("tmp_agents root %s: max_dirs_per_cycle %d reached; the rest wait for the next pass", root, tc.MaxDirsPerCycle))
				break
			}
			inspected++
			c := retentionCandidate{rule: retentionRuleTmpAgents, root: root, path: e.path, info: e.info, maxAge: maxAge}
			c.target = p.decide(ctx, cfg, c, tc.Delete, tc.MaxEntriesPerDir, activity, deny, git, "")
			pass.candidates = append(pass.candidates, c)
		}
	}
}

func (p *RetentionPlugin) evaluateCargoTargets(ctx context.Context, cfg *config.Config, roots []string, activity *bbActivity, deny *agentSessionDeny, git *devArtifactGitTracker, pass *retentionPass) {
	cc := cfg.Retention.CargoTargets
	maxAge, _ := config.ParseDuration(cc.MaxAge)
	busy := retentionBusyProcess(activity, cc.BusyProcesses)
	if busy != "" {
		pass.metadata["cargo_targets.busy"] = busy
	}
	depth := cc.MaxDepth
	if depth <= 0 {
		depth = 3
	}
	for _, root := range roots {
		for _, path := range findCargoTargetDirs(ctx, root, depth) {
			if ctx.Err() != nil {
				pass.warnings = append(pass.warnings, "cargo_targets pass interrupted")
				return
			}
			info, err := os.Lstat(path)
			if err != nil || !info.IsDir() {
				continue
			}
			c := retentionCandidate{rule: retentionRuleCargoTargets, root: root, path: path, info: info, maxAge: maxAge}
			c.target = p.decide(ctx, cfg, c, cc.Delete, cc.MaxEntriesPerTarget, activity, deny, git, busy)
			pass.candidates = append(pass.candidates, c)
		}
	}
}

// findCargoTargetDirs lists Cargo target or build directories under root, at
// most depth levels down. A target directory is not searched further, and
// symlinks and .git directories are never entered.
func findCargoTargetDirs(ctx context.Context, root string, depth int) []string {
	var out []string
	var walk func(dir string, level int)
	walk = func(dir string, level int) {
		if ctx.Err() != nil || level > depth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if !e.IsDir() || e.Type()&fs.ModeSymlink != 0 || e.Name() == ".git" {
				continue
			}
			path := filepath.Join(dir, e.Name())
			if isCargoTargetDir(path) {
				out = append(out, path)
				continue
			}
			walk(path, level+1)
		}
	}
	walk(root, 1)
	return out
}

// isCargoTargetDir recognises a Cargo target or build-dir by the files Cargo
// writes at its top: CACHEDIR.TAG and .rustc_info.json.
func isCargoTargetDir(path string) bool {
	return bbIsRegular(filepath.Join(path, "CACHEDIR.TAG")) || bbIsRegular(filepath.Join(path, ".rustc_info.json"))
}

// retentionBusyProcess names the first process whose argv[0] base name is a
// listed name or that name followed by "-" (cargo-nextest, rustc-wrapper).
func retentionBusyProcess(activity *bbActivity, names []string) string {
	if activity == nil {
		return ""
	}
	for _, proc := range activity.procs {
		fields := strings.Fields(proc.cmdline)
		if len(fields) == 0 {
			continue
		}
		base := filepath.Base(fields[0])
		for _, n := range names {
			if n != "" && (base == n || strings.HasPrefix(base, n+"-")) {
				return fmt.Sprintf("pid %d (%s) is running", proc.pid, base)
			}
		}
	}
	return ""
}

func retentionNameMatches(name string, globs []string) bool {
	for _, g := range globs {
		if ok, _ := filepath.Match(g, name); ok {
			return true
		}
	}
	return false
}

// decide applies every rule to one tree and returns its target. busy, when
// set, keeps the tree whatever else holds.
func (p *RetentionPlugin) decide(ctx context.Context, cfg *config.Config, c retentionCandidate, deleteFlag bool, walkLimit int, activity *bbActivity, deny *agentSessionDeny, git *devArtifactGitTracker, busy string) CleanupTarget {
	path := c.path
	target := CleanupTarget{
		Type: retentionTargetType(c.rule), Name: filepath.Base(path), Path: path,
		Action: "keep", Protected: true,
	}
	// The reclaim class follows the final action (delete: host, else none).
	defer func() { annotateCleanupTargetPolicy(&target, CleanupTierWarm, hostReclaimForAction(target.Action)) }()

	if why, denied := deny.denied(path); denied {
		target.Reason = why
		return target
	}
	if why, denied := deny.containsDenied(path); denied {
		target.Reason = why
		return target
	}
	for _, prefix := range cfg.DevArtifacts.ProtectPaths {
		if prefix != "" && strings.HasPrefix(path, prefix) {
			target.Reason = "covered by dev_artifacts.protect_paths " + prefix
			return target
		}
	}
	st, ok := c.info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != p.uid {
		target.Reason = "belongs to another or unknown user"
		return target
	}

	stats, why := p.walkTree(ctx, path, uint64(st.Dev), walkLimit)
	target.Bytes = stats.allocated
	if stats.apparent != stats.allocated {
		target.LogicalBytes = stats.apparent
	}
	if why != "" {
		target.Reason = why
		return target
	}
	now := p.clock()
	if stats.newest.After(now.Add(-c.maxAge)) {
		target.Reason = fmt.Sprintf("recent change %s is within %s", stats.newest.UTC().Format(time.RFC3339), formatDevArtifactAge(c.maxAge))
		return target
	}
	if why := retentionGitReason(ctx, git, path); why != "" {
		target.Reason = why
		return target
	}
	if busy != "" {
		target.Active = true
		target.Reason = "cargo_targets skipped while a build runs: " + busy
		return target
	}
	if why := activity.complete(); why != "" {
		target.Reason = why
		return target
	}
	if live := activity.blockers(path); len(live) > 0 {
		target.Active = true
		target.Reason = "live reference: " + live[0]
		return target
	}

	target.Protected = false
	reason := fmt.Sprintf("newest change %s is older than %s with no live reference", stats.newest.UTC().Format(time.RFC3339), formatDevArtifactAge(c.maxAge))
	if deleteFlag {
		target.Action = "delete"
		target.Reason = reason
	} else {
		target.Action = "report"
		target.Reason = "would remove (report only; set retention." + c.rule + ".delete): " + reason
	}
	return target
}

func retentionTargetType(rule string) string {
	switch rule {
	case retentionRuleTmpAgents:
		return "tmp-agents-root"
	case retentionRuleCargoTargets:
		return "cargo-target"
	default:
		return "darwin-leftover"
	}
}

// retentionGitReason keeps a tree that holds files a Git repository tracks,
// or that sits in a work tree with uncommitted changes, unpushed commits or
// no upstream.
func retentionGitReason(ctx context.Context, git *devArtifactGitTracker, path string) string {
	repo := git.repoRootForPath(path)
	if repo == "" {
		return ""
	}
	if git.ContainsTrackedFiles(path) {
		return "holds files tracked by the Git repository at " + repo
	}
	if why := gitWorktreeProtectReason(ctx, repo); why != "" {
		return why + " (" + repo + ")"
	}
	return ""
}

// retentionTreeStats is what one walk learned about a tree.
type retentionTreeStats struct {
	newest    time.Time
	allocated int64
	apparent  int64
	entries   int
}

// walkTree measures a tree and applies the structural rules. A non-empty
// reason keeps the tree.
func (p *RetentionPlugin) walkTree(ctx context.Context, path string, dev uint64, limit int) (retentionTreeStats, string) {
	var stats retentionTreeStats
	now := p.clock()
	err := filepath.WalkDir(path, func(walked string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		stats.entries++
		if limit > 0 && stats.entries > limit {
			return fmt.Errorf("walk bound %d exceeded", limit)
		}
		if d.Name() == ".git" {
			return errors.New("contains a Git repository (.git): Git custody")
		}
		if walked != path && d.IsDir() && isAgentSessionDir(walked) {
			return errors.New("contains agent-session directory " + d.Name() + " (TIN-5268)")
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		cur, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || uint64(cur.Dev) != dev || int(cur.Uid) != p.uid {
			return errors.New("foreign owner, mount or unknown identity inside")
		}
		if t := effectiveModTime(fi, now); t.After(stats.newest) {
			stats.newest = t
		}
		if ct, ok := changeTime(fi); ok && !ct.After(now.Add(futureMtimeTolerance)) && ct.After(stats.newest) {
			stats.newest = ct
		}
		// Allocated blocks, not the apparent size: pytest stages sparse
		// multi-GB fixtures that occupy nothing (TIN-7495).
		stats.allocated += int64(cur.Blocks) * 512
		if fi.Mode().IsRegular() {
			stats.apparent += fi.Size()
		} else {
			stats.apparent += int64(cur.Blocks) * 512
		}
		return nil
	})
	if err != nil {
		return stats, "kept: " + err.Error()
	}
	return stats, ""
}

// evaluateFloors raises an alert for every volume under the floor.
func (p *RetentionPlugin) evaluateFloors(cfg *config.Config, roots []string, pass *retentionPass) {
	fc := cfg.Retention.FreeSpaceFloor
	if !fc.Enabled || fc.FloorGiB <= 0 {
		return
	}
	floor := uint64(fc.FloorGiB * float64(1<<30))
	paths := append([]string{}, fc.Paths...)
	for _, m := range cfg.MonitoredMounts {
		paths = append(paths, m.Path)
	}
	paths = append(paths, roots...)
	seen := map[string]bool{}
	for _, raw := range paths {
		path := filepath.Clean(config.ExpandUserPath(raw, p.home, p.user))
		if raw == "" || seen[path] {
			continue
		}
		seen[path] = true
		u, err := p.usage(path)
		if err != nil {
			if !os.IsNotExist(err) {
				pass.warnings = append(pass.warnings, fmt.Sprintf("free_space_floor: %s unreadable: %v", path, err))
			}
			continue
		}
		free := u.BytesTotal - u.BytesUsed
		below := free < floor
		pass.metadata["free_space_floor."+path] = fmt.Sprintf("free_bytes=%d total_bytes=%d below_floor=%t", free, u.BytesTotal, below)
		if below {
			pass.alerts = append(pass.alerts, fmt.Sprintf("ALERT free-space floor: %s has %.1f GiB free, below the %.0f GiB floor", path, float64(free)/(1<<30), fc.FloorGiB))
		}
	}
	pass.metadata["free_space_floor.floor_gib"] = strconv.FormatFloat(fc.FloorGiB, 'f', -1, 64)
}

// fusionAppLink is the symlink the Fusion launcher points at the current
// webdeploy build.
const fusionAppLink = "Autodesk Fusion.app"

// evaluateDarwinLeftovers reports superseded Fusion builds and the listed
// report paths. Every target is protected: there is no delete rule.
func (p *RetentionPlugin) evaluateDarwinLeftovers(ctx context.Context, cfg *config.Config, pass *retentionPass) {
	dc := cfg.Retention.DarwinLeftovers
	report := func(name, path, reason string, bytes int64) {
		t := CleanupTarget{Type: "darwin-leftover", Name: name, Path: path, Bytes: bytes, Action: "report", Protected: true, Reason: reason}
		annotateCleanupTargetPolicy(&t, CleanupTierDestructive, CleanupReclaimNone)
		pass.reports = append(pass.reports, t)
	}
	if root := config.ExpandUserPath(dc.FusionWebdeployRoot, p.home, p.user); root != "" {
		if entries, err := os.ReadDir(root); err == nil {
			current := ""
			if target, err := filepath.EvalSymlinks(filepath.Join(root, fusionAppLink)); err == nil {
				realRoot := resolvedOrSelf(root)
				if rel, err := filepath.Rel(realRoot, target); err == nil && !strings.HasPrefix(rel, "..") {
					current = strings.Split(rel, string(os.PathSeparator))[0]
				}
			}
			pass.metadata["darwin_leftovers.fusion_current_build"] = current
			var superseded int64
			for _, e := range entries {
				if ctx.Err() != nil {
					return
				}
				if !e.IsDir() || e.Type()&fs.ModeSymlink != 0 {
					continue
				}
				path := filepath.Join(root, e.Name())
				bytes, _ := getDirAllocatedBytesContext(ctx, path)
				switch {
				case current == "":
					report("fusion-webdeploy-build", path, "Fusion webdeploy build; the current build is unknown ("+fusionAppLink+" symlink missing or unresolved); report only", bytes)
				case e.Name() == current:
					report("fusion-webdeploy-current", path, "current Fusion build (target of "+fusionAppLink+"); kept", bytes)
				default:
					superseded += bytes
					report("fusion-webdeploy-superseded", path, "superseded Fusion webdeploy build (not the target of "+fusionAppLink+"); report only, no deletion rule", bytes)
				}
			}
			pass.metadata["darwin_leftovers.fusion_superseded_bytes"] = strconv.FormatInt(superseded, 10)
		} else if !os.IsNotExist(err) {
			pass.warnings = append(pass.warnings, fmt.Sprintf("darwin_leftovers: %s unreadable: %v", root, err))
		}
	}
	for _, raw := range dc.ReportPaths {
		path := config.ExpandUserPath(raw, p.home, p.user)
		info, err := os.Lstat(path)
		if err != nil {
			pass.metadata["darwin_leftovers.absent."+path] = "true"
			continue
		}
		var bytes int64
		if info.IsDir() {
			bytes, _ = getDirAllocatedBytesContext(ctx, path)
		} else if st, ok := info.Sys().(*syscall.Stat_t); ok {
			bytes = int64(st.Blocks) * 512
		}
		report(filepath.Base(path), path, "report only, no deletion rule", bytes)
	}
}

// summarize records per-rule totals: what each deleting rule would free.
func (p *RetentionPlugin) summarize(pass *retentionPass) {
	type totals struct {
		seen, eligible, active, kept              int
		eligibleBytes, eligibleLogical, keptBytes int64
	}
	byRule := map[string]*totals{retentionRuleTmpAgents: {}, retentionRuleCargoTargets: {}}
	for _, c := range pass.candidates {
		t := byRule[c.rule]
		t.seen++
		switch {
		case !c.target.Protected:
			t.eligible++
			t.eligibleBytes += c.target.Bytes
			logical := c.target.LogicalBytes
			if logical == 0 {
				logical = c.target.Bytes
			}
			t.eligibleLogical += logical
		case c.target.Active:
			t.active++
			t.keptBytes += c.target.Bytes
		default:
			t.kept++
			t.keptBytes += c.target.Bytes
		}
	}
	for rule, t := range byRule {
		pass.metadata[rule+".inspected"] = strconv.Itoa(t.seen)
		pass.metadata[rule+".eligible_count"] = strconv.Itoa(t.eligible)
		pass.metadata[rule+".eligible_bytes"] = strconv.FormatInt(t.eligibleBytes, 10)
		pass.metadata[rule+".eligible_logical_bytes"] = strconv.FormatInt(t.eligibleLogical, 10)
		pass.metadata[rule+".live_count"] = strconv.Itoa(t.active)
		pass.metadata[rule+".kept_count"] = strconv.Itoa(t.kept)
		pass.metadata[rule+".kept_bytes"] = strconv.FormatInt(t.keptBytes, 10)
	}
}

// PlanCleanup reports what the rules would do, removing nothing.
func (p *RetentionPlugin) PlanCleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupPlan {
	plan := CleanupPlan{Plugin: p.Name(), Level: level.String()}
	pass := p.evaluate(ctx, cfg)
	rc := cfg.Retention
	for _, c := range pass.candidates {
		plan.Targets = append(plan.Targets, c.target)
		if c.target.Action == "delete" {
			plan.EstimatedBytesFreed += c.target.Bytes
		}
	}
	plan.Targets = append(plan.Targets, pass.reports...)
	plan.Warnings = append(append([]string{}, pass.alerts...), pass.warnings...)
	plan.WouldRun = rc.TmpAgents.Delete || rc.CargoTargets.Delete
	if !plan.WouldRun {
		plan.SkipReason = "retention deleting rules are report-only (no delete flag set)"
	}
	plan.Summary = fmt.Sprintf("%d tree(s) inspected; tmp_agents delete=%t, cargo_targets delete=%t", len(pass.candidates), rc.TmpAgents.Delete, rc.CargoTargets.Delete)
	plan.Steps = []string{
		"Take one process inventory: cwd, root, open files, mappings, command lines and TMPDIR/TMP/TEMP/TEMPDIR",
		"tmp_agents: inspect top-level directories matching name_globs, oldest first",
		"cargo_targets: find Cargo target dirs (CACHEDIR.TAG or .rustc_info.json) under the roots; skip all while cargo or rustc runs",
		"Keep anything of another user, across a mount, with .git, agent-session data, protect_paths, Git custody, a recent change or a live reference",
		"Measure allocated blocks (sparse files count what they occupy)",
		"Remove only when the rule's delete flag is true, re-checking every tree immediately before removal",
		"Alert on volumes under free_space_floor.floor_gib; report Darwin leftovers (never deleted)",
	}
	annotateCleanupPlanTargetAccounting(&plan)
	for k, v := range pass.metadata {
		plan.Metadata[k] = v
	}
	plan.Metadata["tmp_agents.delete"] = strconv.FormatBool(rc.TmpAgents.Delete)
	plan.Metadata["cargo_targets.delete"] = strconv.FormatBool(rc.CargoTargets.Delete)
	return plan
}

// Cleanup removes eligible trees for rules whose delete flag is true. Rules
// without the flag log what they would remove and remove nothing.
func (p *RetentionPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: level}
	if !p.due(cfg) {
		logger.Debug("retention pass not due", "interval", cfg.Retention.Interval)
		return result
	}
	pass := p.evaluate(ctx, cfg)
	for _, a := range pass.alerts {
		logger.Warn("retention free-space floor", "alert", a)
	}
	for _, w := range pass.warnings {
		logger.Warn("retention", "warning", w)
	}
	for _, t := range pass.reports {
		logger.Info("retention report (never deleted)", "path", t.Path, "bytes", t.Bytes, "reason", t.Reason)
	}

	var fresh *bbActivity
	var freshAt time.Time
	for _, c := range pass.candidates {
		t := c.target
		switch t.Action {
		case "report":
			logger.Info("retention would remove (report only)", "rule", c.rule, "path", t.Path, "bytes", t.Bytes, "logical_bytes", t.LogicalBytes, "reason", t.Reason)
			continue
		case "delete":
		default:
			logger.Debug("retention keeps tree", "rule", c.rule, "path", t.Path, "reason", t.Reason)
			continue
		}
		if ctx.Err() != nil {
			result.Error = ctx.Err()
			return result
		}
		// Fresh checks immediately before mutation: a plan is no lease.
		if fresh == nil || p.clock().Sub(freshAt) > retentionActivityMaxAge {
			fresh = p.scan(ctx, p.activityOptions(cfg, []string{c.root}))
			freshAt = p.clock()
		}
		if why := p.recheck(ctx, cfg, c, fresh); why != "" {
			logger.Info("retention keeps tree at removal time", "rule", c.rule, "path", t.Path, "reason", why)
			continue
		}
		if err := p.remove(t.Path); err != nil {
			logger.Warn("retention could not remove tree", "rule", c.rule, "path", t.Path, "error", err)
			continue
		}
		logger.Info("retention removed stale tree", "rule", c.rule, "path", t.Path, "bytes", t.Bytes, "max_age", formatDevArtifactAge(c.maxAge))
		result.BytesFreed += t.Bytes
		result.EstimatedBytesFreed += t.Bytes
		result.ItemsCleaned++
	}
	return result
}

// due reports whether retention.interval has passed since the last pass, and
// records this pass when it has.
func (p *RetentionPlugin) due(cfg *config.Config) bool {
	interval := time.Hour
	if cfg.Retention.Interval != "" {
		if d, err := config.ParseDuration(cfg.Retention.Interval); err == nil {
			interval = d
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	if !p.lastRun.IsZero() && interval > 0 && now.Sub(p.lastRun) < interval {
		return false
	}
	p.lastRun = now
	return true
}

// recheck repeats every check for one tree just before removal.
func (p *RetentionPlugin) recheck(ctx context.Context, cfg *config.Config, c retentionCandidate, activity *bbActivity) string {
	current, err := os.Lstat(c.target.Path)
	if err != nil || !os.SameFile(c.info, current) {
		return "tree identity changed"
	}
	busy := ""
	deleteFlag := false
	limit := 0
	switch c.rule {
	case retentionRuleTmpAgents:
		deleteFlag, limit = cfg.Retention.TmpAgents.Delete, cfg.Retention.TmpAgents.MaxEntriesPerDir
	case retentionRuleCargoTargets:
		deleteFlag, limit = cfg.Retention.CargoTargets.Delete, cfg.Retention.CargoTargets.MaxEntriesPerTarget
		busy = retentionBusyProcess(activity, cfg.Retention.CargoTargets.BusyProcesses)
	default:
		return "rule has no deletion"
	}
	deny := newAgentSessionDeny(p.home, p.uid, p.tmpdir, cfg.AgentSession.ExtraPaths, nil)
	again := c
	again.info = current
	t := p.decide(ctx, cfg, again, deleteFlag, limit, activity, deny, newDevArtifactGitTracker(), busy)
	if t.Action != "delete" {
		return t.Reason
	}
	return ""
}
