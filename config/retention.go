package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Retention rule floors: a configured age may be longer, never shorter.
const (
	// RetentionTmpAgentsMinMaxAge is the floor for tmp_agents.max_age.
	RetentionTmpAgentsMinMaxAge = time.Hour
	// RetentionCargoTargetsMinMaxAge is the floor for cargo_targets.max_age.
	RetentionCargoTargetsMinMaxAge = 24 * time.Hour
	// BazelDiskCacheMinMaxAge is the floor for bazel.disk_cache_max_age.
	BazelDiskCacheMinMaxAge = 24 * time.Hour
)

// RetentionConfig configures the age-based retention plugin (TIN-7495,
// TIN-3082): stale agent temp roots, stale Cargo build directories, a
// free-space floor alert and report-only Darwin leftovers.
//
// Every deleting rule is report-only until its own delete flag is true.
type RetentionConfig struct {
	// Interval is the shortest time between two daemon passes (default 1h).
	// A dry run (--dry-run) always plans.
	Interval string `yaml:"interval"`
	// UninspectableOKComms are process names of this user whose fd tables
	// may be unreadable without making the activity inventory incomplete.
	UninspectableOKComms []string `yaml:"uninspectable_ok_comms"`
	// TmpAgents is the stale agent temp-root rule (TIN-7495).
	TmpAgents RetentionTmpAgentsConfig `yaml:"tmp_agents"`
	// CargoTargets is the stale Cargo build-directory rule (TIN-3082).
	CargoTargets RetentionCargoTargetsConfig `yaml:"cargo_targets"`
	// FreeSpaceFloor raises an alert line for volumes under the floor.
	FreeSpaceFloor RetentionFreeSpaceFloorConfig `yaml:"free_space_floor"`
	// DarwinLeftovers reports superseded app payloads on Darwin (report only).
	DarwinLeftovers RetentionDarwinLeftoversConfig `yaml:"darwin_leftovers"`
}

// RetentionTmpAgentsConfig removes stale top-level temp roots such as
// /srv/fast-local/tmp-agents/nix-shell.* (with their pytest-of-<user>
// subtrees) once nothing inside changed within max_age and no live process
// references them (cwd, root, open file, mapping, command line or TMPDIR).
type RetentionTmpAgentsConfig struct {
	// Enabled turns the rule on (default true; it still only reports until
	// Delete is true).
	Enabled bool `yaml:"enabled"`
	// Roots are the directories whose top-level entries are inspected.
	// "~" and "{user}" are expanded; a missing root is skipped.
	Roots []string `yaml:"roots"`
	// NameGlobs are filepath.Match globs for the top-level names the rule
	// owns (default nix-shell.*).
	NameGlobs []string `yaml:"name_globs"`
	// MaxAge is how long the newest change in a tree must be past (default
	// 24h, at least 1h).
	MaxAge string `yaml:"max_age"`
	// Delete removes eligible trees. False (the default) reports only.
	Delete bool `yaml:"delete"`
	// MaxDirsPerCycle bounds the directories inspected per root per pass
	// (0 means no bound). Oldest directories go first.
	MaxDirsPerCycle int `yaml:"max_dirs_per_cycle"`
	// MaxEntriesPerDir bounds one tree walk; a larger tree is kept.
	MaxEntriesPerDir int `yaml:"max_entries_per_dir"`
	// FixtureRepoParents are globs for path elements (default pytest-of-*)
	// below which a .git directory with no remote, no include and no linked
	// work trees is a test fixture that may go with its stale tree
	// (operator ruling TIN-7495 2026-10-29). Empty keeps every .git.
	FixtureRepoParents []string `yaml:"fixture_repo_parents"`
	// Pressure is the fill-gated pass (A-084, A-095, TIN-5770). Off by
	// default.
	Pressure RetentionTmpAgentsPressureConfig `yaml:"pressure"`
}

// RetentionTmpAgentsPressureMinInterval is the floor for pressure.interval.
const RetentionTmpAgentsPressureMinInterval = time.Minute

// RetentionKeepMarker is the file name that keeps a tree from the pressure
// pass whatever keep_if_contains says (TIN-5770 skeptic S10).
const RetentionKeepMarker = ".tinyland-keep"

// RetentionTmpAgentsPressureConfig removes unheld temp entries, oldest first,
// while the volume holding a root is full (A-084 for sting, A-095 for
// petting-zoo-mini). It reads statfs on each root every daemon cycle. When
// the root's volume is at or over used_percent, or under the free-space
// floor, it removes entries whose newest change is older than max_age until
// the volume is under stop_percent and over the floor, re-reading statfs
// after each removal. Every tmp_agents guard applies, plus: a Nix GC root
// link inside keeps the tree, a keep marker inside keeps it, and a nix-shell.*
// tree that may be the TMPDIR of a live interactive nix develop shell is kept.
type RetentionTmpAgentsPressureConfig struct {
	// Enabled turns the pressure pass on (default false).
	Enabled bool `yaml:"enabled"`
	// UsedPercent is the used share of the root's volume (statfs) at or above
	// which the pass removes (default 90).
	UsedPercent float64 `yaml:"used_percent"`
	// StopPercent ends the pass once the volume is under it (default 0, which
	// means used_percent). It must not be above used_percent.
	StopPercent float64 `yaml:"stop_percent"`
	// Interval is the least time between two passes on one root (default
	// 15m, at least 1m). A free-space floor breach runs a pass at once.
	Interval string `yaml:"interval"`
	// MaxAge is how long the newest change in an entry must be past (default
	// 6h, at least 1h).
	MaxAge string `yaml:"max_age"`
	// AllNames makes every top-level entry a candidate, not only name_globs.
	// Agent-session names stay denied. It is refused on "/" or the home
	// directory, and a root under dev_artifacts.protect_paths is skipped.
	AllNames bool `yaml:"all_names"`
	// IncludeFiles makes top-level regular files candidates, judged by their
	// own mtime, ctime and holders. Links, sockets and devices never are.
	IncludeFiles bool `yaml:"include_files"`
	// ExcludeGlobs are top-level names that are never candidates.
	ExcludeGlobs []string `yaml:"exclude_globs"`
	// KeepIfContains keeps an entry that holds a file whose name matches a
	// glob, anywhere inside the walk bound. .tinyland-keep always keeps.
	KeepIfContains []string `yaml:"keep_if_contains"`
	// SocketListenerHolds counts a listening unix socket bound inside an
	// entry as a holder (Linux: /proc/net/unix; Darwin: what lsof lists).
	SocketListenerHolds bool `yaml:"socket_listener_holds"`
	// Delete removes eligible entries. False (the default) reports only.
	Delete bool `yaml:"delete"`
}

// RetentionCargoTargetsConfig removes Cargo target or build directories
// (recognised by CACHEDIR.TAG or .rustc_info.json) under the configured roots
// whose newest change is older than max_age, never while a cargo or rustc
// process runs.
type RetentionCargoTargetsConfig struct {
	// Enabled turns the rule on (default true; report only until Delete).
	Enabled bool `yaml:"enabled"`
	// Roots are searched for Cargo target directories. "~" and "{user}" are
	// expanded; a missing root is skipped. Default /srv/cache/{user}/cargo/build.
	Roots []string `yaml:"roots"`
	// MaxDepth bounds how deep below a root a target directory is looked for
	// (default 3; Cargo's {workspace-path-hash} build-dir layout is 2 deep).
	MaxDepth int `yaml:"max_depth"`
	// MaxAge is how long the newest change in a target must be past
	// (default 7d, at least 24h).
	MaxAge string `yaml:"max_age"`
	// Delete removes eligible targets. False (the default) reports only.
	Delete bool `yaml:"delete"`
	// BusyProcesses are process names (argv[0] base name, or that name
	// followed by "-") that skip the whole rule while any one runs.
	BusyProcesses []string `yaml:"busy_processes"`
	// MaxEntriesPerTarget bounds one target walk; a larger target is kept.
	MaxEntriesPerTarget int `yaml:"max_entries_per_target"`
}

// RetentionFreeSpaceFloorConfig raises an alert line for every listed volume
// whose free space is under the floor. It never deletes anything.
type RetentionFreeSpaceFloorConfig struct {
	// Enabled turns the alert on (default true).
	Enabled bool `yaml:"enabled"`
	// FloorGiB is the per-volume floor in GiB (default 10).
	FloorGiB float64 `yaml:"floor_gib"`
	// Paths are the volumes checked. Default "/" (and /System/Volumes/Data
	// on Darwin); the monitored_mounts paths and the retention roots are
	// always added.
	Paths []string `yaml:"paths"`
}

// RetentionDarwinLeftoversConfig reports superseded payloads on Darwin. It is
// report-only: there is no delete flag.
type RetentionDarwinLeftoversConfig struct {
	// Enabled turns the report on (default true on Darwin).
	Enabled bool `yaml:"enabled"`
	// FusionWebdeployRoot holds the Autodesk Fusion webdeploy builds and the
	// "Autodesk Fusion.app" symlink to the current one.
	FusionWebdeployRoot string `yaml:"fusion_webdeploy_root"`
	// ReportPaths are directories whose size is reported as-is (default the
	// Claude desktop vm_bundles directory).
	ReportPaths []string `yaml:"report_paths"`
}

// DefaultRetentionConfig returns the compiled retention defaults.
func DefaultRetentionConfig(home string) RetentionConfig {
	floorPaths := []string{"/"}
	tmpRoots := []string{"/srv/fast-local/tmp-agents"}
	if runtime.GOOS == "darwin" {
		floorPaths = []string{"/", "/System/Volumes/Data"}
		tmpRoots = []string{}
	}
	return RetentionConfig{
		Interval:             "1h",
		UninspectableOKComms: []string{"tailscaled", "(sd-pam)", "sd-pam", "systemd"},
		TmpAgents: RetentionTmpAgentsConfig{
			Enabled:            true,
			Roots:              tmpRoots,
			NameGlobs:          []string{"nix-shell.*"},
			MaxAge:             "24h",
			Delete:             false,
			MaxDirsPerCycle:    4096,
			MaxEntriesPerDir:   200000,
			FixtureRepoParents: []string{"pytest-of-*"},
			Pressure: RetentionTmpAgentsPressureConfig{
				Enabled:     false,
				UsedPercent: 90,
				Interval:    "15m",
				MaxAge:      "6h",
			},
		},
		CargoTargets: RetentionCargoTargetsConfig{
			Enabled:             true,
			Roots:               []string{"/srv/cache/{user}/cargo/build"},
			MaxDepth:            3,
			MaxAge:              "7d",
			Delete:              false,
			BusyProcesses:       []string{"cargo", "rustc"},
			MaxEntriesPerTarget: 2000000,
		},
		FreeSpaceFloor: RetentionFreeSpaceFloorConfig{
			Enabled:  true,
			FloorGiB: 10,
			Paths:    floorPaths,
		},
		DarwinLeftovers: RetentionDarwinLeftoversConfig{
			Enabled:             runtime.GOOS == "darwin",
			FusionWebdeployRoot: filepath.Join(home, "Library", "Application Support", "Autodesk", "webdeploy", "production"),
			ReportPaths:         []string{filepath.Join(home, "Library", "Application Support", "Claude", "vm_bundles")},
		},
	}
}

// Validate checks the retention section. LoadConfig calls it when the
// plugin is enabled, so a malformed policy refuses to load.
func (c RetentionConfig) Validate() error {
	if c.Interval != "" {
		if _, err := ParseDuration(c.Interval); err != nil {
			return fmt.Errorf("retention.interval: %w", err)
		}
	}
	if c.TmpAgents.Enabled {
		if err := validateRetentionRoots("retention.tmp_agents.roots", c.TmpAgents.Roots); err != nil {
			return err
		}
		if len(c.TmpAgents.NameGlobs) == 0 {
			return fmt.Errorf("retention.tmp_agents.name_globs must name at least one glob")
		}
		for _, g := range c.TmpAgents.NameGlobs {
			if _, err := filepath.Match(g, "x"); err != nil || g == "" || g == "*" {
				return fmt.Errorf("retention.tmp_agents.name_globs: %q is not a usable glob (a bare * is refused)", g)
			}
		}
		if err := validateRetentionAge("retention.tmp_agents.max_age", c.TmpAgents.MaxAge, RetentionTmpAgentsMinMaxAge); err != nil {
			return err
		}
		for _, g := range c.TmpAgents.FixtureRepoParents {
			if _, err := filepath.Match(g, "x"); err != nil || g == "" || g == "*" || strings.Contains(g, "/") {
				return fmt.Errorf("retention.tmp_agents.fixture_repo_parents: %q is not a usable path-element glob (a bare * is refused)", g)
			}
		}
		if c.TmpAgents.MaxDirsPerCycle < 0 || c.TmpAgents.MaxEntriesPerDir < 0 {
			return fmt.Errorf("retention.tmp_agents bounds must not be negative")
		}
		if err := c.TmpAgents.Pressure.validate(c.TmpAgents.Roots); err != nil {
			return err
		}
	}
	if c.CargoTargets.Enabled {
		if err := validateRetentionRoots("retention.cargo_targets.roots", c.CargoTargets.Roots); err != nil {
			return err
		}
		if err := validateRetentionAge("retention.cargo_targets.max_age", c.CargoTargets.MaxAge, RetentionCargoTargetsMinMaxAge); err != nil {
			return err
		}
		if c.CargoTargets.MaxDepth < 0 || c.CargoTargets.MaxEntriesPerTarget < 0 {
			return fmt.Errorf("retention.cargo_targets bounds must not be negative")
		}
	}
	if c.FreeSpaceFloor.FloorGiB < 0 {
		return fmt.Errorf("retention.free_space_floor.floor_gib must not be negative")
	}
	return nil
}

func (p RetentionTmpAgentsPressureConfig) validate(roots []string) error {
	if !p.Enabled {
		return nil
	}
	const key = "retention.tmp_agents.pressure"
	if p.UsedPercent <= 0 || p.UsedPercent > 100 {
		return fmt.Errorf("%s.used_percent %v must be above 0 and at most 100", key, p.UsedPercent)
	}
	if p.StopPercent < 0 || p.StopPercent > p.UsedPercent {
		return fmt.Errorf("%s.stop_percent %v must be from 0 to used_percent %v", key, p.StopPercent, p.UsedPercent)
	}
	interval, err := ParseDuration(p.Interval)
	if err != nil {
		return fmt.Errorf("%s.interval: %w", key, err)
	}
	if interval < RetentionTmpAgentsPressureMinInterval {
		return fmt.Errorf("%s.interval %s is below the floor %s", key, interval, RetentionTmpAgentsPressureMinInterval)
	}
	if err := validateRetentionAge(key+".max_age", p.MaxAge, RetentionTmpAgentsMinMaxAge); err != nil {
		return err
	}
	for _, g := range append(append([]string{}, p.ExcludeGlobs...), p.KeepIfContains...) {
		if _, err := filepath.Match(g, "x"); err != nil || g == "" || strings.Contains(g, "/") {
			return fmt.Errorf("%s: %q is not a usable name glob", key, g)
		}
	}
	if p.AllNames {
		for _, r := range roots {
			if r == "~" || r == "~/" || filepath.Clean(r) == "/" {
				return fmt.Errorf("%s.all_names is refused on root %q", key, r)
			}
		}
	}
	return nil
}

func validateRetentionRoots(key string, roots []string) error {
	for _, r := range roots {
		if r == "" {
			return fmt.Errorf("%s: empty root", key)
		}
		if r[0] == '~' {
			continue
		}
		if !filepath.IsAbs(r) || filepath.Clean(r) == "/" {
			return fmt.Errorf("%s: %q must be an absolute directory other than /", key, r)
		}
	}
	return nil
}

func validateRetentionAge(key, value string, floor time.Duration) error {
	d, err := ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	if d < floor {
		return fmt.Errorf("%s %s is below the floor %s", key, d, floor)
	}
	return nil
}

// BazelRepoContentsMinMinAge is the floor for bazel.repo_contents_min_age.
const BazelRepoContentsMinMinAge = time.Hour

// ValidateRepoContents checks the Bazel repository contents dedupe keys.
func (c BazelConfig) ValidateRepoContents() error {
	if c.RepoContentsMinAge != "" {
		d, err := ParseDuration(c.RepoContentsMinAge)
		if err != nil {
			return fmt.Errorf("bazel.repo_contents_min_age: %w", err)
		}
		if d < BazelRepoContentsMinMinAge {
			return fmt.Errorf("bazel.repo_contents_min_age %s is below the floor %s", d, BazelRepoContentsMinMinAge)
		}
	}
	if c.RepoContentsKeepRecent < 0 {
		return fmt.Errorf("bazel.repo_contents_keep_recent %d must not be negative", c.RepoContentsKeepRecent)
	}
	return nil
}

// ValidateDiskCacheAge checks the Bazel disk-cache age keys.
func (c BazelConfig) ValidateDiskCacheAge() error {
	if c.DiskCacheMaxAge == "" {
		return nil
	}
	d, err := ParseDuration(c.DiskCacheMaxAge)
	if err != nil {
		return fmt.Errorf("bazel.disk_cache_max_age: %w", err)
	}
	if d != 0 && d < BazelDiskCacheMinMaxAge {
		return fmt.Errorf("bazel.disk_cache_max_age %s is below the floor %s", d, BazelDiskCacheMinMaxAge)
	}
	for _, p := range c.DiskCachePaths {
		if p == "" || (p[0] != '~' && (!filepath.IsAbs(p) || filepath.Clean(p) == "/")) {
			return fmt.Errorf("bazel.disk_cache_paths: %q must be an absolute directory other than /", p)
		}
	}
	return nil
}

// ExpandUserPath expands a leading "~" and every "{user}" in path.
func ExpandUserPath(path, home, user string) string {
	if path == "~" {
		path = home
	} else if len(path) > 1 && path[:2] == "~/" {
		path = filepath.Join(home, path[2:])
	}
	if user == "" {
		user = os.Getenv("USER")
	}
	return strings.ReplaceAll(path, "{user}", user)
}
