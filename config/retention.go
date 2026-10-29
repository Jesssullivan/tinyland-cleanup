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
			Enabled:          true,
			Roots:            tmpRoots,
			NameGlobs:        []string{"nix-shell.*"},
			MaxAge:           "24h",
			Delete:           false,
			MaxDirsPerCycle:  4096,
			MaxEntriesPerDir: 200000,
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
		if c.TmpAgents.MaxDirsPerCycle < 0 || c.TmpAgents.MaxEntriesPerDir < 0 {
			return fmt.Errorf("retention.tmp_agents bounds must not be negative")
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
