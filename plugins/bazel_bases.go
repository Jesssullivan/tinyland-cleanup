package plugins

// bazel_bases.go is the structural Bazel output-base reaper (R-C644, R-C645).
//
// It is a port of the lab R-C622/R-C632 seat reaper
// (scripts/sting-bazel-output-prune.py, seat_reap) into the daemon:
//
//   - Discovery is structural. A directory is an output base when it holds an
//     execroot/ directory, external/ or server/, and one of lock,
//     DO_NOT_BUILD_HERE or javalog.properties. The walk never follows a
//     symlink, never leaves the root's file system and stops at each base.
//   - Idle is the newest use signal (lock, server/, command.log, java.log*,
//     execroot, action_cache, command.profile.gz). R-C645: a base idle beyond
//     idle_after (12h) is eligible; on a mount at or above
//     pressure_start_percent (70%) of bytes or inodes the window drops to
//     pressure_idle_after (2h), bases go largest first, and pressure work
//     stops once both dimensions are below pressure_stop_percent (60%).
//   - A live server (server.pid.txt), a process that names the base on its
//     command line or holds anything under it (cwd, root, fd, mapping), or a
//     held lock always keeps it. An incomplete process inventory keeps every
//     base. A base that git tracks, that holds .git, or that has a mount point
//     inside it is kept. Held copies (R-C606) and the TIN-5268 agent-session
//     deny set are never walked or removed.
//   - Removal re-checks everything for that base, takes Bazel's own lock,
//     renames the base aside on the same file system, re-checks the aside,
//     renames it back if anything holds it, and only then deletes it.
//
// The plugin is level independent: it runs whatever the host cleanup level,
// is never suppressed for zero yield, and applies its own per-mount pressure.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

const (
	bazelBasesPluginName = "bazel_bases"
	bbAsideTag           = ".tinyland-reap-"
	bbLabAsideTag        = ".rc622-reap-"
	bbLabPruneAsideTag   = ".rc409-prune-"
)

var (
	bbBazelFiles   = []string{"lock", "DO_NOT_BUILD_HERE", "javalog.properties"}
	bbSignalNames  = []string{"command.log", "java.log", "server"}
	bbExtraSignals = []string{"execroot", "action_cache", "command.profile.gz"}
)

// LevelIndependent is implemented by plugins that apply their own per-mount
// pressure and idle policy. The daemon runs them every cycle, whatever the
// host cleanup level, and never holds them back for cooldown, byte backoff,
// zero yield or a met free-space target (R-C645).
type LevelIndependent interface {
	LevelIndependent() bool
}

// bbUsage is one statfs reading of the file system holding a path.
type bbUsage struct {
	BytesTotal    uint64
	BytesUsed     uint64
	InodesTotal   uint64
	InodesUsed    uint64
	InodesCounted bool
	FSType        string
}

func (u bbUsage) bytesPct() float64 {
	if u.BytesTotal == 0 {
		return 0
	}
	return 100 * float64(u.BytesUsed) / float64(u.BytesTotal)
}

func (u bbUsage) inodesPct() float64 {
	if !u.InodesCounted || u.InodesTotal == 0 {
		return 0
	}
	return 100 * float64(u.InodesUsed) / float64(u.InodesTotal)
}

// bbEnv is the plugin's view of the host; tests replace parts of it.
type bbEnv struct {
	now         func() time.Time
	usage       func(path string) (bbUsage, error)
	activity    func(ctx context.Context, opts bbActivityOptions) *bbActivity
	lockHolder  func(lockPath string) (string, error)
	takeLock    func(lockPath string) (bbHeldLock, string, error)
	mountPoints func() ([]string, error)
	home        string
	uid         int
	tmpdir      string
}

// bbActivityOptions configures one process inventory.
type bbActivityOptions struct {
	Lsof        string
	LsofTimeout time.Duration
	UseLsof     bool
	OKComms     []string
	// Within limits the indexed paths to these trees (the roots); processes'
	// command lines are always kept.
	Within []string
}

// bbHeldLock is Bazel's output-base lock while the reaper holds it.
type bbHeldLock interface {
	// Recheck reports another holder seen through the held descriptor.
	Recheck() string
	Release()
}

type bbNoLock struct{}

func (bbNoLock) Recheck() string { return "" }
func (bbNoLock) Release()        {}

// bbActivity is one inventory of the processes this user can inspect.
type bbActivity struct {
	held       map[string]string
	procs      []bbProc
	incomplete []string
	lsofError  string
}

type bbProc struct {
	pid     int
	cmdline string
}

func newBBActivity() *bbActivity {
	return &bbActivity{held: map[string]string{}}
}

func (a *bbActivity) index(path, why string, within []string) {
	p := filepath.Clean(strings.TrimSuffix(path, " (deleted)"))
	if !filepath.IsAbs(p) {
		return
	}
	if len(within) > 0 {
		in := false
		for _, w := range within {
			if bbUnder(p, w) {
				in = true
				break
			}
		}
		if !in {
			return
		}
	}
	for p != "/" && p != "." {
		if _, ok := a.held[p]; ok {
			return
		}
		a.held[p] = why
		p = filepath.Dir(p)
	}
}

// blockers lists processes that hold target or anything under it, or name it
// on their command line.
func (a *bbActivity) blockers(target string) []string {
	var found []string
	real := resolvedOrSelf(target)
	for _, t := range uniquePaths(filepath.Clean(target), real) {
		if why, ok := a.held[t]; ok {
			found = append(found, why)
		}
	}
	for _, p := range a.procs {
		if bbCmdlineNames(p.cmdline, target) || bbCmdlineNames(p.cmdline, real) {
			found = append(found, fmt.Sprintf("pid %d names it on its command line", p.pid))
			break
		}
		named := false
		for _, ob := range bbCmdlineOutputBases(p.cmdline) {
			if r := resolvedOrSelf(ob); r == real || bbUnder(r, real) {
				named = true
				break
			}
		}
		if named {
			found = append(found, fmt.Sprintf("pid %d names it as --output_base through another path", p.pid))
			break
		}
	}
	return found
}

func (a *bbActivity) proc(pid int) (bbProc, bool) {
	for _, p := range a.procs {
		if p.pid == pid {
			return p, true
		}
	}
	return bbProc{}, false
}

func (a *bbActivity) complete() string {
	if len(a.incomplete) > 0 {
		n := len(a.incomplete)
		if n > 3 {
			n = 3
		}
		return "activity inventory incomplete: " + strings.Join(a.incomplete[:n], "; ")
	}
	if a.lsofError != "" {
		return "activity inventory incomplete: " + a.lsofError
	}
	return ""
}

func bbCmdlineNames(cmdline, path string) bool {
	if cmdline == "" || path == "" {
		return false
	}
	idx := strings.Index(cmdline, path)
	for idx >= 0 {
		end := idx + len(path)
		if end == len(cmdline) {
			return true
		}
		switch cmdline[end] {
		case ' ', '/', '"', '\'', ',', ':', '=', 0:
			return true
		}
		next := strings.Index(cmdline[end:], path)
		if next < 0 {
			return false
		}
		idx = end + next
	}
	return false
}

func defaultBBEnv() bbEnv {
	home, _ := os.UserHomeDir()
	return bbEnv{
		now:         time.Now,
		usage:       bbStatfsUsage,
		activity:    bbScanActivity,
		lockHolder:  bbLockHolder,
		takeLock:    bbTakeLock,
		mountPoints: bbMountPoints,
		home:        home,
		uid:         os.Getuid(),
		tmpdir:      os.Getenv("TMPDIR"),
	}
}

// BazelBasesPlugin reaps idle Bazel output bases by structure.
type BazelBasesPlugin struct {
	env           bbEnv
	lastDiscovery time.Time
	pressured     map[uint64]bool
	// earlyAt holds, per device, the earliest time a walk before the
	// discovery interval could remove something there: the zero time when
	// the last cycle left work behind, else when the first base kept as
	// recently used passes its pressure window. A device with no entry has
	// nothing an early walk could take. Nil until a cycle has completed.
	earlyAt map[uint64]time.Time
	// lastPartial is set when the last walk was cut short or a base's device
	// could not be read, so earlyAt may be missing a device.
	lastPartial bool
	// emptyWalks counts consecutive apply cycles that removed less than
	// early_walk_min_yield_mb. Each one doubles the wait before the next
	// early walk.
	emptyWalks int
	// lastWalk is how long the last cycle (walk, inventory and removals)
	// took; earlyGap keeps early walks to their duty share of wall time.
	lastWalk time.Duration
}

// NewBazelBasesPlugin creates the plugin with the host environment.
func NewBazelBasesPlugin() *BazelBasesPlugin {
	return &BazelBasesPlugin{env: defaultBBEnv(), pressured: map[uint64]bool{}}
}

// Name returns the plugin name.
func (p *BazelBasesPlugin) Name() string { return bazelBasesPluginName }

// Description returns the plugin description.
func (p *BazelBasesPlugin) Description() string {
	return "Reaps idle Bazel/omux output bases by structure under per-mount pressure (R-C644, R-C645)"
}

// SupportedPlatforms returns Linux and Darwin.
func (p *BazelBasesPlugin) SupportedPlatforms() []string {
	return []string{PlatformLinux, PlatformDarwin}
}

// Enabled reports whether bazel_bases is enabled.
func (p *BazelBasesPlugin) Enabled(cfg *config.Config) bool { return cfg.Enable.BazelBases }

// LevelIndependent marks the plugin as running outside the level ladder.
func (p *BazelBasesPlugin) LevelIndependent() bool { return true }

// SafetyCritical keeps the plugin out of zero-yield suppression.
func (p *BazelBasesPlugin) SafetyCritical() bool { return true }

// bbRow is one discovered output base and its decision.
type bbRow struct {
	Path          string  `json:"path"`
	Root          string  `json:"root"`
	Kind          string  `json:"kind"`
	Owner         string  `json:"owner"`
	BasesOnly     string  `json:"bases_only,omitempty"`
	Carveout      string  `json:"carveout,omitempty"`
	LastUse       string  `json:"last_use,omitempty"`
	IdleHours     float64 `json:"idle_hours"`
	WindowHours   float64 `json:"window_hours"`
	Pressure      bool    `json:"pressure"`
	Bytes         int64   `json:"bytes"`
	Inodes        int64   `json:"inodes"`
	Measured      bool    `json:"measured"`
	Sibling       string  `json:"sibling,omitempty"`
	SiblingNote   string  `json:"sibling_note,omitempty"`
	SiblingBytes  int64   `json:"sibling_bytes,omitempty"`
	SiblingInodes int64   `json:"sibling_inodes,omitempty"`
	Decision      string  `json:"decision"`
	Live          bool    `json:"live"`
	Reason        string  `json:"reason"`

	lastUse time.Time
	dev     uint64
	idle    time.Duration
	window  time.Duration
	normal  time.Duration
	hasLock bool
	// pressureWindow is the window the base would have on a mount under
	// pressure. recent marks a base kept only because it was used inside its
	// window, and again a base that must be looked at on the next walk.
	pressureWindow time.Duration
	recent         bool
	again          bool
}

// bbCycle is one discovery-and-decision pass.
type bbCycle struct {
	Timestamp     string           `json:"timestamp"`
	Mode          string           `json:"mode"`
	Apply         bool             `json:"apply"`
	Roots         []string         `json:"roots"`
	Mounts        map[string]bbDev `json:"mounts"`
	Discovered    int              `json:"discovered"`
	Eligible      int              `json:"eligible"`
	EligibleBytes int64            `json:"eligible_bytes"`
	Removed       int              `json:"removed"`
	RemovedBytes  int64            `json:"removed_bytes"`
	RemovedInodes int64            `json:"removed_inodes"`
	KeptLive      int              `json:"kept_live"`
	KeptRecent    int              `json:"kept_recent"`
	KeptOther     int              `json:"kept_other"`
	Deferred      int              `json:"deferred"`
	Skipped       []bbSkip         `json:"skipped,omitempty"`
	Errors        []string         `json:"errors,omitempty"`
	Refusal       string           `json:"refusal,omitempty"`
	Partial       bool             `json:"discovery_partial,omitempty"`
	Seconds       float64          `json:"seconds"`
	Rows          []*bbRow         `json:"rows"`
	asides        []bbAside
}

type bbDev struct {
	Path         string  `json:"path"`
	FSType       string  `json:"fs_type,omitempty"`
	BytesPercent float64 `json:"bytes_percent"`
	InodePercent float64 `json:"inode_percent,omitempty"`
	Inodes       bool    `json:"inodes_counted"`
	Pressure     bool    `json:"pressure"`
	After        *bbDev  `json:"after,omitempty"`
}

type bbSkip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type bbAside struct {
	root string
	path string
}

// bbPolicy is the validated, parsed configuration for one cycle.
type bbPolicy struct {
	cfg           config.BazelBasesConfig
	roots         []string
	rootMounts    map[string]string
	basesOnly     []string
	held          []string
	skip          []string
	skipNames     map[string]bool
	rootPressure  map[string]bbPressureLines
	earlyMinYield int64
	earlyDuty     int
	idle          time.Duration
	pressureIdle  time.Duration
	basesOnlyIdle time.Duration
	interval      time.Duration
	deny          *agentSessionDeny
}

// bbPressureLines are the start and stop percentages for one root.
type bbPressureLines struct {
	start, stop float64
}

const (
	// bbDefaultEarlyMinYieldMB is the least an early pressure walk must
	// remove to count as productive (the daemon's byte_progress_min_mb).
	bbDefaultEarlyMinYieldMB = 256
	// bbDefaultEarlyDutyPercent bounds early pressure walks to this share
	// of wall time.
	bbDefaultEarlyDutyPercent = 20
)

func (p *BazelBasesPlugin) policy(cfg *config.Config) (*bbPolicy, error) {
	bb := cfg.BazelBases
	if err := bb.Validate(); err != nil {
		return nil, err
	}
	home := p.env.home
	pol := &bbPolicy{cfg: bb, rootMounts: map[string]string{}, skipNames: map[string]bool{}}
	pol.idle, _ = config.ParseDuration(bb.IdleAfter)
	pol.pressureIdle, _ = config.ParseDuration(bb.PressureIdleAfter)
	pol.basesOnlyIdle, _ = config.ParseDuration(bb.BasesOnlyIdleAfter)
	pol.interval, _ = config.ParseDuration(bb.DiscoveryInterval)
	for _, r := range bb.Roots {
		pol.roots = append(pol.roots, filepath.Clean(expandAgentSessionHome(r, home)))
	}
	for r, m := range bb.RootMounts {
		pol.rootMounts[filepath.Clean(expandAgentSessionHome(r, home))] = filepath.Clean(m)
	}
	for _, b := range bb.BasesOnlyRoots {
		pol.basesOnly = append(pol.basesOnly, filepath.Clean(expandAgentSessionHome(b, home)))
	}
	for _, h := range bb.HeldPaths {
		pol.held = append(pol.held, filepath.Clean(expandAgentSessionHome(h, home)))
	}
	for _, sp := range bb.SkipPaths {
		pol.skip = append(pol.skip, filepath.Clean(expandAgentSessionHome(sp, home)))
	}
	pol.rootPressure = map[string]bbPressureLines{}
	for _, rp := range bb.RootPressure {
		pol.rootPressure[filepath.Clean(expandAgentSessionHome(rp.Root, home))] = bbPressureLines{start: float64(rp.StartPercent), stop: float64(rp.StopPercent)}
	}
	pol.earlyMinYield = int64(bbDefaultEarlyMinYieldMB) << 20
	if bb.EarlyWalkMinYieldMB > 0 {
		pol.earlyMinYield = int64(bb.EarlyWalkMinYieldMB) << 20
	}
	pol.earlyDuty = bbDefaultEarlyDutyPercent
	if bb.EarlyWalkMaxDutyPercent > 0 {
		pol.earlyDuty = bb.EarlyWalkMaxDutyPercent
	}
	for _, n := range bb.SkipDirNames {
		pol.skipNames[n] = true
	}
	pol.deny = newAgentSessionDeny(home, p.env.uid, p.env.tmpdir, cfg.AgentSession.ExtraPaths, cfg.AgentSession.BazelBaseCarveouts)
	// TIN-5268: a root or bases-only root inside an agent-session store is a
	// configuration error, not something to walk around.
	for _, r := range append(append([]string{}, pol.roots...), pol.basesOnly...) {
		if pol.deny.carveoutFor(r) != "" {
			continue
		}
		if why, denied := pol.deny.denied(r); denied {
			return nil, fmt.Errorf("bazel_bases root %s is agent-session data: %s", r, why)
		}
	}
	return pol, nil
}

func (pol *bbPolicy) heldBy(path string) string {
	for _, h := range pol.held {
		if pathInsideRoot(path, h) {
			return h
		}
	}
	return ""
}

// skippedBy returns the skip_paths entry that keeps the walk out of path.
func (pol *bbPolicy) skippedBy(path string) string {
	for _, sp := range pol.skip {
		if pathInsideRoot(path, sp) {
			return sp
		}
	}
	return ""
}

// pressureLines returns the start and stop percentages for root: its own
// root_pressure row, else the plugin-wide pair.
func (pol *bbPolicy) pressureLines(root string) bbPressureLines {
	if lines, ok := pol.rootPressure[root]; ok {
		return lines
	}
	return bbPressureLines{start: float64(pol.cfg.PressureStartPercent), stop: float64(pol.cfg.PressureStopPercent)}
}

func (pol *bbPolicy) basesOnlyRoot(path string) string {
	for _, b := range pol.basesOnly {
		if pathInsideRoot(path, b) {
			return b
		}
	}
	return ""
}

func (pol *bbPolicy) skipGlobbed(name string) bool {
	for _, g := range pol.cfg.SkipNameGlobs {
		if ok, _ := filepath.Match(g, name); ok {
			return true
		}
	}
	return false
}

// owner labels a base with the first owners row whose glob matches it or one
// of its ancestors (fnmatch-style, as the lab reaper's globs are written).
func (pol *bbPolicy) owner(path, root string) (string, string) {
	for _, row := range pol.cfg.Owners {
		for q := path; ; q = filepath.Dir(q) {
			if ok, _ := filepath.Match(row.Glob, q); ok {
				kind := row.Kind
				if kind == "" {
					kind = "unknown"
				}
				return row.Owner, kind
			}
			if q == "/" || q == "." || q == root {
				break
			}
		}
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return root, "unknown"
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) > 2 {
		parts = parts[:2]
	}
	return filepath.Join(root, filepath.Join(parts...)), "unknown"
}

func (pol *bbPolicy) windows(kind string) (time.Duration, time.Duration) {
	idle, pressure := pol.idle, pol.pressureIdle
	if k, ok := pol.cfg.Kinds[kind]; ok {
		if d, err := config.ParseDuration(k.IdleAfter); err == nil && d > idle {
			idle = d
		}
		if d, err := config.ParseDuration(k.PressureIdleAfter); err == nil && d > pressure {
			pressure = d
		}
	}
	if pressure > idle {
		pressure = idle
	}
	return idle, pressure
}

// bbPressureMinInterval bounds how often an apply-mode cycle walks again
// while a root's mount stays under pressure.
const bbPressureMinInterval = 5 * time.Minute

// earlyGap is the least time between the last walk and an early pressure
// walk: bbPressureMinInterval, doubled for every consecutive apply cycle
// that removed less than early_walk_min_yield_mb, and never less than the
// last walk's own duration times (100 - duty) / duty, so early walks take at
// most early_walk_max_duty_percent of wall time. On sting (2026-10-17) walks
// of 90 s to 25 min ran back to back, each removing one 45 MiB-3 GiB base,
// because a single removal reset the wait to 5 minutes.
func (p *BazelBasesPlugin) earlyGap(pol *bbPolicy) time.Duration {
	gap := bbPressureMinInterval
	for i := 0; i < p.emptyWalks && gap < pol.interval; i++ {
		gap *= 2
	}
	if duty := pol.earlyDuty; duty > 0 && duty < 100 && p.lastWalk > 0 {
		if floor := p.lastWalk * time.Duration(100-duty) / time.Duration(duty); floor > gap {
			gap = floor
		}
	}
	return gap
}

// due reports whether a cycle should run now: the discovery interval has
// passed, or (apply mode only) an early pressure walk has something to do.
// Report mode keeps the interval: it relieves nothing, so walking every poll
// would only cost IO.
//
// An early walk needs a root's mount under pressure and a reason to expect a
// removal there: the last cycle left work behind (a base deferred by the time
// budget), or a base it kept as recently used has now passed its pressure
// window. A pressured mount whose bases are all live, held, or kept for what
// is inside them gives an early walk nothing to do, and so does a mount with
// no base at all: a base created since cannot be eligible before
// pressure_idle_after, and the discovery interval finds it. Without this a
// mount that stays above the pressure line for reasons this plugin cannot
// relieve would be walked every five minutes, forever, and each walk covers
// every root.
//
// A walk that was cut short, or that has not run yet, keeps the early walk,
// because its picture of the mounts is incomplete. Every early walk waits
// earlyGap, so a run of walks that remove nothing, whatever the cause, thins
// out to the discovery interval.
func (p *BazelBasesPlugin) due(pol *bbPolicy, apply bool) bool {
	now := p.env.now()
	if p.lastDiscovery.IsZero() || now.Sub(p.lastDiscovery) >= pol.interval {
		return true
	}
	if !apply || now.Sub(p.lastDiscovery) < p.earlyGap(pol) {
		return false
	}
	for _, r := range pol.roots {
		dev, err := deviceID(r)
		if err != nil {
			continue
		}
		u, err := p.env.usage(r)
		if err != nil {
			continue
		}
		if !p.pressureState(r, dev, u, pol) {
			continue
		}
		if p.earlyAt == nil || p.lastPartial {
			return true
		}
		if at, ok := p.earlyAt[dev]; ok && !now.Before(at) {
			return true
		}
	}
	return false
}

// noteWork records, for due's pressure rule, when an early walk could next
// remove something on each device, and whether this cycle removed anything.
func (p *BazelBasesPlugin) noteWork(cycle *bbCycle, minYield int64) {
	early := map[uint64]time.Time{}
	partial := cycle.Partial
	sooner := func(dev uint64, at time.Time) {
		if cur, ok := early[dev]; !ok || at.Before(cur) {
			early[dev] = at
		}
	}
	for _, row := range cycle.Rows {
		if row.dev == 0 {
			partial = true
			continue
		}
		switch {
		case row.Decision == "deferred" || row.again:
			sooner(row.dev, time.Time{})
		case row.recent && !row.lastUse.IsZero() && row.pressureWindow > 0:
			sooner(row.dev, row.lastUse.Add(row.pressureWindow))
		}
	}
	p.earlyAt = early
	p.lastPartial = partial
	if cycle.Apply && cycle.RemovedBytes < minYield {
		p.emptyWalks++
	} else {
		p.emptyWalks = 0
	}
}

func (p *BazelBasesPlugin) pressureState(root string, dev uint64, u bbUsage, pol *bbPolicy) bool {
	lines := pol.pressureLines(root)
	start, stop := lines.start, lines.stop
	hi := u.bytesPct() >= start || u.inodesPct() >= start
	if hi {
		return true
	}
	if p.pressured[dev] && (u.bytesPct() >= stop || u.inodesPct() >= stop) {
		return true
	}
	return false
}

// PlanCleanup reports what the reaper would do, removing nothing.
func (p *BazelBasesPlugin) PlanCleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupPlan {
	plan := CleanupPlan{
		Plugin:   p.Name(),
		Level:    level.String(),
		Summary:  "Structural Bazel output-base reaper (R-C644, R-C645)",
		WouldRun: true,
		Steps: []string{
			"Walk the configured roots by structure without following symlinks or crossing file systems",
			"Skip held copies (R-C606), name-guarded copy trees outside bases-only roots (R-C632) and the TIN-5268 agent-session deny set",
			"Keep any base with a live server, a process naming or holding it, a held lock, a .git entry, a git-tracked file, or a mount point inside",
			"Eligible after 12h idle, or 2h idle on a mount at or above 70% bytes or inodes, largest first until below 60%",
			"Remove by fresh re-check, Bazel lock, rename aside, re-check, delete",
		},
		Metadata: map[string]string{"mode": cfg.BazelBases.Mode},
	}
	pol, err := p.policy(cfg)
	if err != nil {
		plan.WouldRun = false
		plan.SkipReason = err.Error()
		return plan
	}
	cycle := p.run(ctx, pol, false, logger)
	for _, row := range cycle.Rows {
		eligible := row.Decision == "eligible"
		target := CleanupTarget{
			Type:      "bazel_output_base",
			Name:      row.Kind,
			Path:      row.Path,
			Bytes:     row.Bytes + row.SiblingBytes,
			Active:    row.Live,
			Protected: !eligible,
			Action:    "keep",
			Reason:    row.Reason,
		}
		if eligible {
			target.Action = "delete_output_base"
			plan.EstimatedBytesFreed += target.Bytes
		}
		annotateCleanupTargetPolicy(&target, CleanupTierWarm, hostReclaimForAction(target.Action))
		plan.Targets = append(plan.Targets, target)
	}
	plan.Warnings = append(plan.Warnings, cycle.Errors...)
	if cycle.Refusal != "" {
		plan.Warnings = append(plan.Warnings, cycle.Refusal)
	}
	plan.Metadata["discovered"] = strconv.Itoa(cycle.Discovered)
	plan.Metadata["eligible"] = strconv.Itoa(cycle.Eligible)
	annotateCleanupPlanTargetAccounting(&plan)
	return plan
}

// Cleanup runs one cycle: report mode writes the plan, apply mode removes.
func (p *BazelBasesPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: level}
	pol, err := p.policy(cfg)
	if err != nil {
		result.Error = err
		return result
	}
	apply := pol.cfg.Mode == config.BazelBasesModeApply
	if !p.due(pol, apply) {
		return result
	}
	walkStart := p.env.now()
	cycle := p.run(ctx, pol, apply, logger)
	p.lastDiscovery = p.env.now()
	p.lastWalk = p.lastDiscovery.Sub(walkStart)
	p.noteWork(cycle, pol.earlyMinYield)
	if err := p.writeReceipts(pol, cycle); err != nil {
		logger.Warn("bazel_bases: receipt write failed", "error", err)
	}
	result.BytesFreed = cycle.RemovedBytes
	result.EstimatedBytesFreed = cycle.EligibleBytes
	result.ItemsCleaned = cycle.Removed
	logger.Info("bazel_bases cycle",
		"mode", cycle.Mode,
		"discovered", cycle.Discovered,
		"eligible", cycle.Eligible,
		"removed", cycle.Removed,
		"removed_bytes", cycle.RemovedBytes,
		"kept_live", cycle.KeptLive,
		"refusal", cycle.Refusal,
	)
	return result
}

// run discovers, decides and (when apply) removes.
func (p *BazelBasesPlugin) run(ctx context.Context, pol *bbPolicy, apply bool, logger *slog.Logger) *bbCycle {
	started := p.env.now()
	wallStart := time.Now()
	cycle := &bbCycle{
		Timestamp: started.UTC().Format(time.RFC3339),
		Mode:      pol.cfg.Mode,
		Apply:     apply,
		Roots:     pol.roots,
		Mounts:    map[string]bbDev{},
	}
	bases := p.discover(ctx, pol, cycle)
	cycle.Discovered = len(bases)

	opts := bbActivityOptions{
		Lsof:        pol.cfg.Lsof,
		LsofTimeout: time.Duration(pol.cfg.LsofTimeoutSeconds) * time.Second,
		UseLsof:     true,
		OKComms:     pol.cfg.UninspectableOKComms,
	}
	for _, r := range pol.roots {
		opts.Within = append(opts.Within, uniquePaths(r, resolvedOrSelf(r))...)
	}
	activity := p.env.activity(ctx, opts)
	mounts, mountErr := p.env.mountPoints()
	git := pol.cfg.Git
	if git == "" {
		git, _ = exec.LookPath("git")
	}

	usageByDev := map[uint64]bbUsage{}
	pressureByDev := map[uint64]bool{}
	devPath := map[uint64]string{}
	for _, b := range bases {
		dev, err := deviceID(filepath.Dir(b.Path))
		if err != nil {
			continue
		}
		if _, ok := usageByDev[dev]; ok {
			continue
		}
		u, err := p.env.usage(filepath.Dir(b.Path))
		if err != nil {
			cycle.Errors = append(cycle.Errors, fmt.Sprintf("statfs %s: %v", b.Path, err))
			continue
		}
		usageByDev[dev] = u
		pressureByDev[dev] = p.pressureState(b.Root, dev, u, pol)
		devPath[dev] = b.Root
		cycle.Mounts[b.Root] = bbDev{Path: b.Root, FSType: u.FSType, BytesPercent: round2(u.bytesPct()),
			InodePercent: round2(u.inodesPct()), Inodes: u.InodesCounted, Pressure: pressureByDev[dev]}
	}

	now := p.env.now()
	for _, b := range bases {
		row := b
		cycle.Rows = append(cycle.Rows, row)
		dev, _ := deviceID(filepath.Dir(row.Path))
		row.dev = dev
		row.Pressure = pressureByDev[dev]
		row.Owner, row.Kind = pol.owner(row.Path, row.Root)
		if row.Carveout != "" && row.Kind == "unknown" {
			row.Kind = "agent-session-carveout"
		}
		use, has := bbLastUse(row.Path, now)
		row.lastUse = use
		if !use.IsZero() {
			row.LastUse = use.UTC().Format(time.RFC3339)
		}
		if !has {
			row.Decision, row.Reason = "keep", "no last-use signal; treated as recent"
			cycle.KeptRecent++
			continue
		}
		row.idle = now.Sub(use)
		row.IdleHours = round2(row.idle.Hours())
		normal, pressured := pol.windows(row.Kind)
		if row.BasesOnly != "" && pressured < pol.basesOnlyIdle {
			pressured = pol.basesOnlyIdle
		}
		row.pressureWindow = pressured
		window := normal
		if row.Pressure {
			window = pressured
		}
		if row.BasesOnly != "" && window < pol.basesOnlyIdle {
			window = pol.basesOnlyIdle
		}
		if row.BasesOnly != "" && normal < pol.basesOnlyIdle {
			normal = pol.basesOnlyIdle
		}
		row.window, row.normal = window, normal
		row.WindowHours = round2(window.Hours())
		if row.idle <= window {
			row.Decision, row.Reason, row.recent = "keep", fmt.Sprintf("used within %s", window), true
			cycle.KeptRecent++
			continue
		}
		if why := p.safe(pol, row.Path, row.Root, row.Carveout); why != "" {
			row.Decision, row.Reason = "keep", "kept: "+why
			cycle.KeptOther++
			continue
		}
		if why := bbGuard(ctx, row.Path, row.Root, mounts, mountErr, git); why != "" {
			row.Decision, row.Reason = "keep", "kept: "+why
			cycle.KeptOther++
			continue
		}
		if live := p.liveReasons(row.Path, activity); len(live) > 0 {
			n := len(live)
			if n > 3 {
				n = 3
			}
			row.Decision, row.Reason, row.Live = "keep", "live: "+strings.Join(live[:n], "; "), true
			cycle.KeptLive++
			continue
		}
		if row.BasesOnly != "" {
			row.SiblingNote = "bases-only tree (R-C632): nothing beside the base is touched"
		} else if row.Carveout != "" {
			row.SiblingNote = "agent-session carve-out (TIN-5268): only the base itself is removed"
		} else {
			sib, note := bbSiblingUserRoot(row.Path, pol.cfg.SiblingUserRoot, bases)
			if sib != "" {
				if why := p.safe(pol, sib, row.Root, ""); why != "" {
					sib, note = "", "sibling user root kept: "+why
				} else if why := bbGuard(ctx, sib, row.Root, mounts, mountErr, git); why != "" {
					sib, note = "", "sibling user root kept: "+why
				} else if b := activity.blockers(sib); len(b) > 0 {
					sib, note = "", "sibling user root in use: "+b[0]
				}
			}
			row.Sibling, row.SiblingNote = sib, note
		}
		row.Decision = "eligible"
		row.Reason = fmt.Sprintf("idle %.1fh > %s, no live server, nothing open, lock free", row.idle.Hours(), window)
	}

	if pol.cfg.OnlyListed != "" {
		listed, err := bbReadListed(pol.cfg.OnlyListed)
		if err != nil {
			cycle.Refusal = fmt.Sprintf("only_listed %s unreadable: %v", pol.cfg.OnlyListed, err)
		}
		for _, row := range cycle.Rows {
			if row.Decision == "eligible" && (err != nil || !listed[row.Path]) {
				row.Decision, row.Reason = "keep", "eligible, but not on the reviewed list (only_listed)"
				cycle.KeptOther++
			}
		}
	}

	var eligible []*bbRow
	for _, row := range cycle.Rows {
		if row.Decision == "eligible" {
			eligible = append(eligible, row)
		}
	}
	anyPressure := false
	for _, row := range eligible {
		if row.Pressure {
			anyPressure = true
		}
	}
	budget := time.Duration(pol.cfg.MaxSecondsPerCycle) * time.Second
	if anyPressure {
		budget = time.Duration(pol.cfg.PressureMaxSeconds) * time.Second
	}
	// The work budget starts after discovery and the process inventory,
	// which max_discovery_seconds bounds on its own: a slow walk must not
	// leave every eligible base unmeasured.
	workStart := time.Now()
	deadline := workStart.Add(budget)
	measureDeadline := workStart.Add(budget / 2)

	for _, row := range eligible {
		if budget > 0 && time.Now().After(measureDeadline) {
			continue
		}
		if why := p.measure(ctx, row, measureDeadline); why != "" {
			row.Decision, row.Reason = "keep", "kept: "+why
			cycle.KeptOther++
		}
	}
	eligible = eligible[:0]
	for _, row := range cycle.Rows {
		if row.Decision == "eligible" {
			eligible = append(eligible, row)
		}
	}
	bbOrder(eligible, usageByDev)
	cycle.Eligible = len(eligible)
	for _, row := range eligible {
		cycle.EligibleBytes += row.Bytes + row.SiblingBytes
	}

	if apply {
		p.apply(ctx, pol, cycle, eligible, activity, usageByDev, pressureByDev, deadline, opts, logger)
	}
	for dev, pressed := range pressureByDev {
		if !apply {
			if pressed {
				p.pressured[dev] = true
			}
			continue
		}
		u, err := p.env.usage(devPath[dev])
		if err != nil {
			continue
		}
		p.pressured[dev] = p.pressureState(devPath[dev], dev, u, pol)
		if d, ok := cycle.Mounts[devPath[dev]]; ok {
			d.After = &bbDev{Path: devPath[dev], BytesPercent: round2(u.bytesPct()), InodePercent: round2(u.inodesPct()),
				Inodes: u.InodesCounted, Pressure: p.pressured[dev]}
			cycle.Mounts[devPath[dev]] = d
		}
	}
	cycle.Seconds = round2(time.Since(wallStart).Seconds())
	// Eligible first, in removal order; then everything kept.
	sort.SliceStable(cycle.Rows, func(i, j int) bool {
		return bbRank(cycle.Rows[i].Decision) < bbRank(cycle.Rows[j].Decision)
	})
	return cycle
}

func bbRank(decision string) int {
	switch decision {
	case "removed":
		return 0
	case "eligible":
		return 1
	case "deferred":
		return 2
	default:
		return 3
	}
}

// bbOrder sorts eligible rows: pressured mounts first, largest first by the
// pressured dimension; then everything else least recently used first.
func bbOrder(rows []*bbRow, usage map[uint64]bbUsage) {
	share := func(r *bbRow) float64 {
		u := usage[r.dev]
		var b, i float64
		if u.BytesTotal > 0 {
			b = float64(r.Bytes+r.SiblingBytes) / float64(u.BytesTotal)
		}
		if u.InodesCounted && u.InodesTotal > 0 {
			i = float64(r.Inodes+r.SiblingInodes) / float64(u.InodesTotal)
		}
		bp, ip := u.bytesPct(), u.inodesPct()
		switch {
		case bp >= ip && b > 0:
			if ip >= bp*0.999 && i > b {
				return i
			}
			return b
		case i > 0:
			if bp >= ip*0.999 && b > i {
				return b
			}
			return i
		default:
			return b
		}
	}
	sort.SliceStable(rows, func(a, b int) bool {
		ra, rb := rows[a], rows[b]
		if ra.Pressure != rb.Pressure {
			return ra.Pressure
		}
		if ra.Pressure {
			if ra.Measured != rb.Measured {
				return ra.Measured
			}
			sa, sb := share(ra), share(rb)
			if sa != sb {
				return sa > sb
			}
			return ra.Path < rb.Path
		}
		if !ra.lastUse.Equal(rb.lastUse) {
			return ra.lastUse.Before(rb.lastUse)
		}
		return ra.Path < rb.Path
	})
}

func (p *BazelBasesPlugin) apply(ctx context.Context, pol *bbPolicy, cycle *bbCycle, eligible []*bbRow,
	activity *bbActivity, usage map[uint64]bbUsage, pressure map[uint64]bool, deadline time.Time,
	opts bbActivityOptions, logger *slog.Logger) {
	if refusal := activity.complete(); refusal != "" {
		cycle.Refusal = refusal + "; nothing removed"
		for _, row := range eligible {
			row.Decision, row.Reason = "keep", cycle.Refusal
		}
		return
	}
	if cycle.Refusal != "" {
		return
	}
	stamp := p.env.now().UTC().Format("20060102T150405Z")
	removalOpts := opts
	// Linux re-checks through /proc at removal time; Darwin has no /proc and
	// keeps lsof as its open-file witness.
	removalOpts.UseLsof = currentPlatform() == PlatformDarwin

	if pol.cfg.OnlyListed == "" {
		for _, aside := range cycle.asides {
			p.removeAside(ctx, pol, aside, removalOpts, cycle)
		}
	}
	stop := float64(pol.cfg.PressureStopPercent)
	relieved := map[uint64]bool{}
	for _, row := range eligible {
		if ctx.Err() != nil || time.Now().After(deadline) {
			row.Decision, row.Reason = "deferred", "cycle time budget spent; the next cycle takes it"
			cycle.Deferred++
			continue
		}
		if row.Pressure {
			if relieved[row.dev] || p.belowStop(row.Path, stop) {
				relieved[row.dev] = true
				if row.idle <= row.normal {
					row.Decision, row.Reason = "keep", fmt.Sprintf("mount below %.0f%% bytes and inodes; only the %s window applies now", stop, row.normal)
					continue
				}
			}
		}
		use, _ := bbLastUse(row.Path, p.env.now())
		limit := row.window
		if relieved[row.dev] {
			limit = row.normal
		}
		if use.IsZero() || p.env.now().Sub(use) <= limit {
			row.Decision, row.Reason = "keep", "used again since discovery"
			// The pressure rule counts from the new use.
			row.lastUse, row.recent = use, !use.IsZero()
			continue
		}
		if !row.Measured {
			// A base measured late (the discovery pass ran out of its
			// measuring budget) gets the same TIN-5268 scan, and its
			// answer binds: a base whose scan finds an agent-session
			// directory, or cannot finish, is kept.
			if why := p.measure(ctx, row, time.Time{}); why != "" {
				row.Decision, row.Reason = "keep", "kept: "+why
				continue
			}
			if !row.Measured {
				row.Decision, row.Reason = "keep", "kept: the base could not be measured, so its agent-session scan is incomplete (TIN-5268)"
				continue
			}
		}
		removed, why := p.removeTree(ctx, pol, row.Path, row.Root, row.Carveout, true, stamp, removalOpts)
		if !removed {
			row.Decision, row.Reason = "keep", why
			continue
		}
		row.Decision = "removed"
		cycle.Removed++
		cycle.RemovedBytes += row.Bytes
		cycle.RemovedInodes += row.Inodes
		if row.Sibling != "" {
			if ok, why := p.removeTree(ctx, pol, row.Sibling, row.Root, "", false, stamp, removalOpts); ok {
				cycle.RemovedBytes += row.SiblingBytes
				cycle.RemovedInodes += row.SiblingInodes
			} else {
				row.SiblingNote = "sibling user root kept at removal: " + why
			}
		}
		p.appendReceipt(pol, row)
		logger.Info("bazel_bases removed output base", "path", row.Path, "kind", row.Kind,
			"bytes", row.Bytes, "inodes", row.Inodes, "idle_hours", row.IdleHours, "pressure", row.Pressure)
	}
}

func (p *BazelBasesPlugin) belowStop(path string, stop float64) bool {
	u, err := p.env.usage(filepath.Dir(path))
	if err != nil {
		return false
	}
	return u.bytesPct() < stop && u.inodesPct() < stop
}

// safe is everything about a path itself that keeps it: gone, not a real
// directory, real path leaving the root, held copies, agent-session data,
// .git ancestry.
func (p *BazelBasesPlugin) safe(pol *bbPolicy, path, root, carve string) string {
	st, err := os.Lstat(path)
	if err != nil {
		return "gone"
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		return "not a real directory"
	}
	real := resolvedOrSelf(path)
	rroot := resolvedOrSelf(root)
	if !pathInsideRoot(real, rroot) || real == rroot {
		return "real path leaves the root"
	}
	for _, q := range uniquePaths(path, real) {
		if h := pol.heldBy(q); h != "" {
			return "held copy (R-C606): " + h
		}
		if strings.Contains(q+"/", "/.git/") {
			return "under a .git directory"
		}
	}
	if c := pol.deny.carveoutFor(path); c != "" {
		if why, denied := pol.deny.deniedBelow(path, c); denied {
			return why
		}
	} else if why, denied := pol.deny.denied(path); denied {
		return why
	}
	if why, denied := pol.deny.containsDenied(path); denied {
		return why
	}
	return ""
}

func (p *BazelBasesPlugin) liveReasons(base string, activity *bbActivity) []string {
	var live []string
	if why := bbPidfileServer(base, activity); why != "" {
		live = append(live, why)
	}
	lock := filepath.Join(base, "lock")
	if st, err := os.Lstat(lock); err == nil && st.Mode().IsRegular() {
		held, err := p.env.lockHolder(lock)
		if err != nil {
			live = append(live, "lock state unknown: "+err.Error())
		} else if held != "" {
			live = append(live, held)
		}
	}
	live = append(live, activity.blockers(base)...)
	return live
}

// removeTree is the rename-recheck-delete sequence for one base or sibling.
func (p *BazelBasesPlugin) removeTree(ctx context.Context, pol *bbPolicy, path, root, carve string, isBase bool,
	stamp string, opts bbActivityOptions) (bool, string) {
	if why := p.safe(pol, path, root, carve); why != "" {
		return false, "kept at removal: " + why
	}
	act := p.env.activity(ctx, opts)
	if why := act.complete(); why != "" {
		return false, "kept at removal: " + why
	}
	if live := p.liveReasons(path, act); len(live) > 0 {
		return false, "live at removal: " + live[0]
	}
	var held bbHeldLock = bbNoLock{}
	lock := filepath.Join(path, "lock")
	if isBase {
		if st, err := os.Lstat(lock); err == nil && st.Mode().IsRegular() {
			h, why, err := p.env.takeLock(lock)
			if err != nil {
				return false, "kept: could not take Bazel's lock: " + err.Error()
			}
			if why != "" {
				return false, "live at removal: " + why
			}
			held = h
		}
	}
	defer held.Release()
	aside := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+bbAsideTag+stamp)
	if _, err := os.Lstat(aside); err == nil {
		return false, "kept: aside path " + aside + " already exists"
	}
	if err := os.Rename(path, aside); err != nil {
		return false, "kept: rename failed: " + err.Error()
	}
	late := p.env.activity(ctx, opts)
	lateWhy := ""
	if why := late.complete(); why != "" {
		lateWhy = why
	} else if b := late.blockers(aside); len(b) > 0 {
		lateWhy = b[0]
	} else if why := held.Recheck(); why != "" {
		lateWhy = why
	}
	if lateWhy != "" {
		if err := os.Rename(aside, path); err != nil {
			return false, fmt.Sprintf("in use right after the rename (%s); rename back failed, left at %s: %v", lateWhy, aside, err)
		}
		return false, "kept: in use right after the rename (" + lateWhy + "); renamed back"
	}
	if err := bbMakeWritable(aside); err != nil {
		return false, "delete failed after rename to " + aside + ": " + err.Error()
	}
	if err := os.RemoveAll(aside); err != nil {
		return false, "delete failed after rename to " + aside + ": " + err.Error()
	}
	return true, ""
}

func (p *BazelBasesPlugin) removeAside(ctx context.Context, pol *bbPolicy, aside bbAside, opts bbActivityOptions, cycle *bbCycle) {
	if why := p.safe(pol, aside.path, aside.root, pol.deny.carveoutFor(aside.path)); why != "" {
		return
	}
	act := p.env.activity(ctx, opts)
	if act.complete() != "" || len(act.blockers(aside.path)) > 0 {
		return
	}
	if lock := filepath.Join(aside.path, "lock"); bbIsRegular(lock) {
		if held, err := p.env.lockHolder(lock); err != nil || held != "" {
			return
		}
	}
	if err := bbMakeWritable(aside.path); err == nil {
		if err := os.RemoveAll(aside.path); err != nil {
			cycle.Errors = append(cycle.Errors, "leftover aside removal failed: "+aside.path+": "+err.Error())
		}
	}
}

// measure sizes an eligible base (and its sibling) in allocated bytes and
// inodes on its own file system. A per-repository agent-session directory
// inside it keeps the base.
func (p *BazelBasesPlugin) measure(ctx context.Context, row *bbRow, deadline time.Time) string {
	b, i, denied, err := bbMeasureTree(ctx, row.Path, deadline, p.env.uid)
	if denied != "" {
		return "holds agent-session directory " + denied + " (TIN-5268)"
	}
	var unread *bbUnreadableDir
	if errors.As(err, &unread) {
		return "directory " + unread.path + " is unreadable, so the agent-session scan is incomplete (TIN-5268)"
	}
	var foreign *bbForeignOwner
	if errors.As(err, &foreign) {
		return foreign.Error() + "; the reaper cannot remove the base whole"
	}
	if err != nil {
		return ""
	}
	row.Bytes, row.Inodes, row.Measured = b, i, true
	if row.Sibling != "" {
		sb, si, sdenied, serr := bbMeasureTree(ctx, row.Sibling, deadline, p.env.uid)
		if sdenied != "" {
			row.Sibling, row.SiblingNote = "", "sibling holds agent-session directory "+sdenied+"; kept"
		} else if serr != nil {
			row.Sibling, row.SiblingNote = "", "sibling user root could not be scanned in full ("+serr.Error()+"); kept"
		} else {
			row.SiblingBytes, row.SiblingInodes = sb, si
		}
	}
	return ""
}

func bbMeasureTree(ctx context.Context, root string, deadline time.Time, uid int) (int64, int64, string, error) {
	rootDev, err := deviceID(root)
	if err != nil {
		return 0, 0, "", err
	}
	var bytes, inodes int64
	denied := ""
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that cannot be read is a directory whose
			// contents were never checked for agent-session data: the
			// removal path would make it writable and delete it unseen.
			if d == nil || d.IsDir() {
				return &bbUnreadableDir{path: path, err: err}
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return context.DeadlineExceeded
		}
		if d.IsDir() && path != root && isAgentSessionDirName(d.Name()) {
			denied = path
			return filepath.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			if uint64(st.Dev) != rootDev {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			// An entry another user owns would stop the removal half way
			// and strand a renamed aside (a non-root daemon cannot chmod
			// or empty it), so such a base is kept whole.
			if uid > 0 && int(st.Uid) != uid {
				return &bbForeignOwner{path: path, uid: int(st.Uid)}
			}
			bytes += int64(st.Blocks) * 512
		} else {
			bytes += info.Size()
		}
		inodes++
		return nil
	})
	if denied != "" {
		return bytes, inodes, denied, nil
	}
	return bytes, inodes, "", err
}

// bbForeignOwner is an entry, inside a base, that another user owns.
type bbForeignOwner struct {
	path string
	uid  int
}

func (e *bbForeignOwner) Error() string {
	return "holds " + e.path + " owned by uid " + strconv.Itoa(e.uid)
}

// bbUnreadableDir is a directory a measuring walk could not read.
type bbUnreadableDir struct {
	path string
	err  error
}

func (e *bbUnreadableDir) Error() string {
	return "unreadable directory " + e.path + ": " + e.err.Error()
}

// discover walks the roots and returns every structural output base. Each
// directory is read once; a symlink is never followed (a DirEntry for a
// symlink is not a directory) and no file system boundary is crossed.
func (p *BazelBasesPlugin) discover(ctx context.Context, pol *bbPolicy, cycle *bbCycle) []*bbRow {
	var rows []*bbRow
	limit := time.Duration(pol.cfg.MaxDiscoverySeconds) * time.Second
	started := time.Now()
	named := 0
	skip := func(path, reason string) {
		if len(cycle.Skipped) < 50 {
			cycle.Skipped = append(cycle.Skipped, bbSkip{Path: path, Reason: reason})
		}
	}
	deny := pol.deny
	// A mount point below a root is another file system: skip it from the
	// mount table, without a stat that a hung network or FUSE file system
	// (honey's ~/tcfs) could block on forever.
	mountSet := map[string]bool{}
	if mounts, err := p.env.mountPoints(); err == nil {
		for _, m := range mounts {
			mountSet[filepath.Clean(m)] = true
		}
	}
	stopped := false
	var walk func(root string, rootDev uint64, dir string, depth int)
	walk = func(root string, rootDev uint64, dir string, depth int) {
		if stopped {
			return
		}
		if ctx.Err() != nil || (limit > 0 && time.Since(started) > limit) {
			cycle.Partial, stopped = true, true
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if dir != root && len(cycle.Errors) < 50 {
				cycle.Errors = append(cycle.Errors, fmt.Sprintf("%s: %v", dir, err))
			}
			return
		}
		names := make(map[string]bool, len(entries))
		for _, e := range entries {
			names[e.Name()] = true
		}
		if names["A-server.jar"] && (names["build-label.txt"] || names["install_base_key"]) {
			return
		}
		if bbIsOutputBase(dir, names) {
			rows = append(rows, &bbRow{
				Path:      dir,
				Root:      root,
				BasesOnly: pol.basesOnlyRoot(dir),
				Carveout:  deny.carveoutForLiteral(dir),
				Decision:  "keep",
				hasLock:   names["lock"],
			})
			return
		}
		if depth >= pol.cfg.MaxDepth {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			path := filepath.Join(dir, name)
			if pol.skipNames[name] {
				continue
			}
			if h := pol.heldBy(path); h != "" {
				skip(path, "held copy (R-C606): "+h)
				continue
			}
			if sp := pol.skippedBy(path); sp != "" {
				skip(path, "skip_paths "+sp+"; not walked")
				continue
			}
			if c := deny.carveoutForLiteral(path); c != "" {
				if why, denied := deny.deniedPath(path, c); denied {
					skip(path, why)
					continue
				}
			} else if why, denied := deny.deniedPath(path, ""); denied {
				if !deny.leadsToCarveoutLiteral(path) {
					skip(path, why+"; not walked")
					continue
				}
			}
			if strings.HasPrefix(name, ".") && (strings.Contains(name, bbAsideTag) || strings.Contains(name, bbLabAsideTag)) {
				cycle.asides = append(cycle.asides, bbAside{root: root, path: path})
				continue
			}
			if strings.HasPrefix(name, ".") && strings.Contains(name, bbLabPruneAsideTag) {
				continue
			}
			if pol.basesOnlyRoot(path) == "" && pol.skipGlobbed(name) {
				named++
				if named <= 20 {
					skip(path, "evidence/backup/transfer/archive/custody name; not walked")
				}
				continue
			}
			if mountSet[path] {
				continue
			}
			if dev, err := deviceID(path); err != nil || dev != rootDev {
				continue
			}
			walk(root, rootDev, path, depth+1)
		}
	}
	for _, root := range pol.roots {
		if mount, ok := pol.rootMounts[root]; ok && !bbIsMountPoint(mount) {
			cycle.Errors = append(cycle.Errors, fmt.Sprintf("%s: %s is not mounted; root skipped", root, mount))
			continue
		}
		st, err := os.Lstat(root)
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			cycle.Errors = append(cycle.Errors, fmt.Sprintf("%s: not a real directory; root skipped", root))
			continue
		}
		rootDev, err := deviceID(root)
		if err != nil {
			continue
		}
		walk(root, rootDev, root, 0)
	}
	if named > 20 {
		skip("", fmt.Sprintf("%d more directories skipped by name", named-20))
	}
	return rows
}

func bbReadNames(dir string) (map[string]bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}

func bbIsOutputBase(dir string, names map[string]bool) bool {
	if !names["execroot"] || !(names["external"] || names["server"]) {
		return false
	}
	found := false
	for _, n := range bbBazelFiles {
		if names[n] {
			found = true
		}
	}
	if !found {
		return false
	}
	st, err := os.Lstat(filepath.Join(dir, "execroot"))
	return err == nil && st.IsDir()
}

// bbLastUse is the newest use signal of a base (seat_last_use): mtimes of the
// base, its lock, command.log, java.log*, server/ and its entries, execroot,
// action_cache and command.profile.gz, plus the atimes of its regular files
// outside server/ (the reaper reads server.pid.txt). A future
// mtime ages from its ctime (effectiveModTime).
func bbLastUse(base string, now time.Time) (time.Time, bool) {
	paths := []string{base, filepath.Join(base, "lock")}
	has := false
	for _, n := range append(append([]string{}, bbSignalNames...), bbExtraSignals...) {
		p := filepath.Join(base, n)
		if _, err := os.Lstat(p); err == nil {
			has = true
			paths = append(paths, p)
		}
	}
	if entries, err := os.ReadDir(base); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "java.log.") {
				has = true
				paths = append(paths, filepath.Join(base, e.Name()))
			}
		}
	}
	server := filepath.Join(base, "server")
	if st, err := os.Lstat(server); err == nil && st.IsDir() {
		if entries, err := os.ReadDir(server); err == nil {
			for _, e := range entries {
				paths = append(paths, filepath.Join(server, e.Name()))
			}
		}
	}
	var newest time.Time
	for _, p := range paths {
		info, err := os.Lstat(p)
		if err != nil {
			continue
		}
		m := effectiveModTime(info, now)
		if m.After(newest) {
			newest = m
		}
		// Atimes count only for files the reaper never reads itself: reading
		// server/server.pid.txt would otherwise refresh its own signal.
		if info.Mode().IsRegular() && filepath.Dir(p) != server {
			if a, ok := bbAccessTime(info); ok && !a.After(now.Add(futureMtimeTolerance)) && a.After(newest) {
				newest = a
			}
		}
	}
	return newest, has
}

// bbPidfileServer reports a live server from server/server.pid.txt.
func bbPidfileServer(base string, activity *bbActivity) string {
	raw, err := os.ReadFile(filepath.Join(base, "server", "server.pid.txt"))
	if err != nil {
		return ""
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return ""
	}
	if !bbPidAlive(pid) {
		return ""
	}
	cmdline := ""
	if proc, ok := activity.proc(pid); ok {
		cmdline = proc.cmdline
	} else if argv, err := nixProcessArgv(pid); err == nil {
		cmdline = strings.Join(argv, " ")
	}
	if cmdline == "" || bbCmdlineNames(cmdline, base) || bbCmdlineNames(cmdline, resolvedOrSelf(base)) {
		return fmt.Sprintf("server pid %d from server.pid.txt is running", pid)
	}
	// The server may have been started through another spelling of the
	// base (a symlinked parent): compare every --output_base it names at
	// its physical path.
	real := resolvedOrSelf(base)
	for _, ob := range bbCmdlineOutputBases(cmdline) {
		if resolvedOrSelf(ob) == real {
			return fmt.Sprintf("server pid %d from server.pid.txt is running (as %s)", pid, ob)
		}
	}
	return ""
}

// bbCmdlineOutputBases returns every --output_base value on a command line.
func bbCmdlineOutputBases(cmdline string) []string {
	var out []string
	fields := strings.Fields(cmdline)
	for i, f := range fields {
		switch {
		case strings.HasPrefix(f, "--output_base="):
			out = append(out, strings.Trim(strings.TrimPrefix(f, "--output_base="), "\"'"))
		case f == "--output_base" && i+1 < len(fields):
			out = append(out, strings.Trim(fields[i+1], "\"'"))
		}
	}
	return out
}

func bbPidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// bbGuard rules out what a structural match alone cannot: a git repository
// shaped like a base, a fixture a work tree tracks, a mount point inside.
func bbGuard(ctx context.Context, path, root string, mounts []string, mountErr error, git string) string {
	if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
		return "holds a .git entry (a git repository or work tree)"
	}
	if mountErr != nil || mounts == nil {
		return "mount table unreadable"
	}
	real := resolvedOrSelf(path)
	for _, m := range mounts {
		if pathInsideRoot(m, path) || pathInsideRoot(m, real) {
			return "mount point " + m + " lies inside it"
		}
	}
	top := filepath.Clean(root)
	for d := filepath.Dir(filepath.Clean(path)); pathInsideRoot(d, top); d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			if git == "" {
				return "inside git work tree " + d + "; git unavailable, tracked state unknown"
			}
			rel, _ := filepath.Rel(d, path)
			cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
			cmd := commandContext(cctx, git, "-C", d, "ls-files", "-z", "--", rel)
			cmd.Env = bbGitEnv()
			out, err := cmd.Output()
			cancel()
			if err != nil {
				return fmt.Sprintf("inside git work tree %s; tracked state unknown: %v", d, err)
			}
			if strings.Trim(string(out), "\x00") != "" {
				return "git work tree " + d + " tracks files under it"
			}
			return ""
		}
		if d == top || d == "/" {
			break
		}
	}
	return ""
}

func bbGitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return env
}

// bbSiblingUserRoot returns the run's own output user root beside base when
// it holds nothing but that base's install base.
func bbSiblingUserRoot(base, name string, bases []*bbRow) (string, string) {
	if name == "" {
		return "", ""
	}
	sib := filepath.Join(filepath.Dir(base), name)
	st, err := os.Lstat(sib)
	if err != nil {
		return "", ""
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", "sibling user root is not a real directory; kept"
	}
	top, err := bbReadNames(sib)
	if err != nil {
		return "", "sibling user root unreadable; kept"
	}
	for n := range top {
		if n != "install" && n != "cache" {
			return "", "sibling user root holds " + n + "; kept"
		}
	}
	link := filepath.Join(base, "install")
	if lst, err := os.Lstat(link); err != nil || lst.Mode()&os.ModeSymlink == 0 {
		return "", "output base has no install link into the sibling; kept"
	}
	target := resolvedOrSelf(link)
	inst := resolvedOrSelf(filepath.Join(sib, "install"))
	if !pathInsideRoot(target, inst) || target == inst {
		return "", "output base's install link points elsewhere; sibling kept"
	}
	entries, _ := bbReadNames(filepath.Join(sib, "install"))
	nm := filepath.Base(target)
	for n := range entries {
		if n != nm && n != nm+".lock" {
			return "", "sibling install holds another install base; kept"
		}
	}
	for _, other := range bases {
		if other.Path == base {
			continue
		}
		olink := filepath.Join(other.Path, "install")
		if lst, err := os.Lstat(olink); err == nil && lst.Mode()&os.ModeSymlink != 0 && pathInsideRoot(resolvedOrSelf(olink), inst) {
			return "", "another output base (" + other.Path + ") uses the sibling; kept"
		}
	}
	return sib, ""
}

// bbMakeWritable gives the owner rwx on every directory in tree; it never
// follows a symlink (Bazel trees are read-only).
func bbMakeWritable(tree string) error {
	return filepath.WalkDir(tree, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Mode().Perm()&0o700 != 0o700 {
			return os.Chmod(path, info.Mode().Perm()|0o700)
		}
		return nil
	})
}

func bbIsRegular(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.Mode().IsRegular()
}

func bbIsMountPoint(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	pst, err := os.Stat(filepath.Dir(filepath.Clean(path)))
	if err != nil {
		return false
	}
	a, ok1 := st.Sys().(*syscall.Stat_t)
	b, ok2 := pst.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return false
	}
	return a.Dev != b.Dev || a.Ino == b.Ino
}

// bbReadListed reads a reviewed candidates TSV: by its path column, and only
// rows marked decision=eligible when it has a decision column.
func bbReadListed(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	listed := map[string]bool{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	col, dcol := 0, -1
	first := true
	for scanner.Scan() {
		cells := strings.Split(scanner.Text(), "\t")
		if first {
			first = false
			isHeader := false
			for i, c := range cells {
				if c == "path" {
					col, isHeader = i, true
				}
				if c == "decision" {
					dcol = i
				}
			}
			if isHeader {
				continue
			}
			dcol = -1
		}
		if len(cells) <= col {
			continue
		}
		if dcol >= 0 && (len(cells) <= dcol || cells[dcol] != "eligible") {
			continue
		}
		listed[strings.TrimSpace(cells[col])] = true
	}
	return listed, scanner.Err()
}

func (p *BazelBasesPlugin) receiptDir(pol *bbPolicy) string {
	dir := pol.cfg.ReceiptDir
	if dir == "" {
		return ""
	}
	return expandAgentSessionHome(dir, p.env.home)
}

// writeReceipts writes the latest plan JSON and candidates TSV.
func (p *BazelBasesPlugin) writeReceipts(pol *bbPolicy, cycle *bbCycle) error {
	dir := p.receiptDir(pol)
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cycle, "", "  ")
	if err != nil {
		return err
	}
	if err := bbWriteAtomic(filepath.Join(dir, "last-cycle.json"), data); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("decision\tpath\tkind\towner\tbytes\tinodes\tidle_hours\twindow_hours\tpressure\tlast_use\tsibling\treason\n")
	for _, r := range cycle.Rows {
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%d\t%d\t%.2f\t%.2f\t%t\t%s\t%s\t%s\n", r.Decision, r.Path, r.Kind,
			bbTSV(r.Owner), r.Bytes, r.Inodes, r.IdleHours, r.WindowHours, r.Pressure, r.LastUse, r.Sibling, bbTSV(r.Reason))
	}
	if err := bbWriteAtomic(filepath.Join(dir, "candidates.tsv"), []byte(b.String())); err != nil {
		return err
	}
	summary := *cycle
	summary.Rows = nil
	summary.Skipped = nil
	line, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	return bbAppend(filepath.Join(dir, "cycles.jsonl"), line)
}

func (p *BazelBasesPlugin) appendReceipt(pol *bbPolicy, row *bbRow) {
	dir := p.receiptDir(pol)
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	rec := struct {
		Timestamp string `json:"timestamp"`
		*bbRow
	}{p.env.now().UTC().Format(time.RFC3339), row}
	if line, err := json.Marshal(rec); err == nil {
		_ = bbAppend(filepath.Join(dir, "removals.jsonl"), line)
	}
}

func bbTSV(s string) string {
	return strings.NewReplacer("\t", " ", "\n", " ").Replace(s)
}

func bbWriteAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func bbAppend(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
