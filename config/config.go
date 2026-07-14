// Package config provides configuration parsing for tinyland-cleanup.
package config

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

// Config represents the cleanup daemon configuration.
type Config struct {
	// PollInterval in seconds between cleanup checks
	PollInterval int `yaml:"poll_interval"`

	// Thresholds for disk usage (percentage)
	Thresholds Thresholds `yaml:"thresholds"`

	// InodeThresholds for inode usage (percentage)
	InodeThresholds Thresholds `yaml:"inode_thresholds"`

	// TargetFree is the legacy config key for target maximum used percentage after cleanup.
	TargetFree int `yaml:"target_free"`

	// Policy controls daemon-level cleanup policy such as cooldown state.
	Policy PolicyConfig `yaml:"policy"`

	// LogFile path for cleanup logs
	LogFile string `yaml:"log_file"`

	// Enable flags for specific cleanup plugins
	Enable EnableFlags `yaml:"enable"`

	// Docker-specific settings
	Docker DockerConfig `yaml:"docker"`

	// Lima VM settings (Darwin)
	Lima LimaConfig `yaml:"lima"`

	// Podman-specific settings
	Podman PodmanConfig `yaml:"podman"`

	// Bazel-specific cache settings
	Bazel BazelConfig `yaml:"bazel"`

	// Nix-specific cleanup settings
	Nix NixConfig `yaml:"nix"`

	// iCloud-specific settings (Darwin)
	ICloud ICloudConfig `yaml:"icloud"`

	// GitHub Actions runner settings (Linux)
	GitHubRunner GitHubRunnerConfig `yaml:"github_runner"`

	// Monitored mount points (multi-volume support)
	MonitoredMounts []MountConfig `yaml:"monitored_mounts"`

	// Dev artifact cleanup settings
	DevArtifacts DevArtifactsConfig `yaml:"dev_artifacts"`

	// Darwin developer cache cleanup settings
	DarwinDevCaches DarwinDevCachesConfig `yaml:"darwin_dev_caches"`

	// APFS snapshot settings (Darwin)
	APFS APFSConfig `yaml:"apfs"`

	// Notification settings
	Notify NotifyConfig `yaml:"notify"`
}

// GitHubRunnerConfig holds GitHub Actions runner cleanup settings.
type GitHubRunnerConfig struct {
	// Home directory for the runner (default: /home/github-runner)
	Home string `yaml:"home"`
	// WorkDir is the work directory (default: <home>/_work)
	WorkDir string `yaml:"work_dir"`
	// CacheDir is the cache directory (default: <home>/cache)
	CacheDir string `yaml:"cache_dir"`
	// TempDir is the temp directory (default: <home>/tmp)
	TempDir string `yaml:"temp_dir"`
	// Instances describes multiple runner homes managed by the same host.
	Instances []GitHubRunnerInstanceConfig `yaml:"instances"`
}

// GitHubRunnerInstanceConfig holds cleanup settings for one GitHub Actions runner instance.
type GitHubRunnerInstanceConfig struct {
	// Name is a human-readable instance name for logs.
	Name string `yaml:"name"`
	// Home directory for this runner.
	Home string `yaml:"home"`
	// WorkDir is the work directory (default: <home>/_work)
	WorkDir string `yaml:"work_dir"`
	// CacheDir is the cache directory (default: <home>/cache)
	CacheDir string `yaml:"cache_dir"`
	// TempDir is the temp directory (default: <home>/tmp)
	TempDir string `yaml:"temp_dir"`
}

// MountConfig defines a mount point to monitor with optional custom thresholds.
type MountConfig struct {
	// Path is the mount point path
	Path string `yaml:"path"`
	// Label is a human-readable label for logging
	Label string `yaml:"label"`
	// ThresholdWarning overrides the global warning threshold
	ThresholdWarning int `yaml:"threshold_warning,omitempty"`
	// ThresholdCritical overrides the global critical threshold
	ThresholdCritical int `yaml:"threshold_critical,omitempty"`
	// ThresholdInodeWarning overrides the global inode warning threshold
	ThresholdInodeWarning int `yaml:"threshold_inode_warning,omitempty"`
	// ThresholdInodeCritical overrides the global inode critical threshold
	ThresholdInodeCritical int `yaml:"threshold_inode_critical,omitempty"`
}

// Thresholds defines disk usage thresholds for graduated cleanup.
type Thresholds struct {
	// Warning triggers level 1 cleanup (caches)
	Warning int `yaml:"warning"`
	// Moderate triggers level 2 cleanup (container images)
	Moderate int `yaml:"moderate"`
	// Aggressive triggers level 3 cleanup (volumes)
	Aggressive int `yaml:"aggressive"`
	// Critical triggers level 4 cleanup (emergency)
	Critical int `yaml:"critical"`
}

// EnableFlags controls which cleanup plugins are enabled.
type EnableFlags struct {
	// Cache cleanup (pip, npm, homebrew caches)
	Cache bool `yaml:"cache"`
	// NixGC for nix-collect-garbage
	NixGC bool `yaml:"nix_gc"`
	// Docker for container image/volume cleanup
	Docker bool `yaml:"docker"`
	// Podman for Podman container/image/volume cleanup
	Podman bool `yaml:"podman"`
	// Lima for Lima VM cleanup (Darwin)
	Lima bool `yaml:"lima"`
	// Homebrew for brew cleanup (Darwin)
	Homebrew bool `yaml:"homebrew"`
	// IOSSimulator for iOS Simulator cleanup (Darwin)
	IOSSimulator bool `yaml:"ios_simulator"`
	// GitLabRunner for GitLab CI cache cleanup
	GitLabRunner bool `yaml:"gitlab_runner"`
	// GitHubRunner for GitHub Actions runner cleanup (Linux)
	GitHubRunner bool `yaml:"github_runner"`
	// Yum for DNF/YUM package cache cleanup (Linux)
	Yum bool `yaml:"yum"`
	// ICloud for iCloud Drive eviction (Darwin)
	ICloud bool `yaml:"icloud"`
	// Photos for Photos library cache cleanup (Darwin)
	Photos bool `yaml:"photos"`
	// DevArtifacts for stale development artifact cleanup
	DevArtifacts bool `yaml:"dev_artifacts"`
	// Bazel for Bazel output base and cache cleanup planning
	Bazel bool `yaml:"bazel"`
	// APFSSnapshots for APFS snapshot thinning (Darwin)
	APFSSnapshots bool `yaml:"apfs_snapshots"`
}

// PolicyConfig holds daemon-level cleanup policy settings.
type PolicyConfig struct {
	// Cooldown skips repeated non-critical daemon-triggered plugin cleanup within this duration.
	Cooldown string `yaml:"cooldown"`
	// CooldownBypassLevel is the minimum cleanup level that ignores cooldown.
	CooldownBypassLevel string `yaml:"cooldown_bypass_level"`
	// MinimumFreeGB bypasses cooldown until the monitored filesystem has this much free space.
	MinimumFreeGB int `yaml:"minimum_free_gb"`
	// StateFile stores daemon cleanup state such as per-plugin last-run timestamps.
	StateFile string `yaml:"state_file"`
	// InodeNoProgressLimit is the number of consecutive cleanup cycles that fail
	// to relieve inode pressure before the daemon stops bypassing cooldown for
	// inode-only escalation and backs off to the cooldown cadence. Zero uses the
	// built-in default.
	InodeNoProgressLimit int `yaml:"inode_no_progress_limit"`
}

// DockerConfig holds Docker-specific cleanup settings.
type DockerConfig struct {
	// Socket path (unix:///var/run/docker.sock or ~/.colima/default/docker.sock)
	Socket string `yaml:"socket"`
	// PruneImagesAge for images older than this duration
	PruneImagesAge string `yaml:"prune_images_age"`
	// ProtectRunningContainers prevents pruning images used by running containers
	ProtectRunningContainers bool `yaml:"protect_running_containers"`
}

// LimaConfig holds Lima VM cleanup settings.
type LimaConfig struct {
	// VMNames to check for Docker cleanup
	VMNames []string `yaml:"vm_names"`
	// CompactOffline enables offline qcow2 compaction at Critical level
	CompactOffline bool `yaml:"compact_offline"`
}

// PodmanConfig holds Podman-specific cleanup settings.
type PodmanConfig struct {
	// PruneImagesAge for images older than this duration
	PruneImagesAge string `yaml:"prune_images_age"`
	// ProtectRunningContainers prevents pruning images used by running containers
	ProtectRunningContainers bool `yaml:"protect_running_containers"`
	// MachineNames to check for cleanup (Darwin)
	MachineNames []string `yaml:"machine_names"`
	// BuildKitPrune enables targeted BuildKit cache pruning at critical level
	BuildKitPrune bool `yaml:"buildkit_prune"`
	// BuildKitPruneKeepDuration preserves BuildKit records newer than this duration
	BuildKitPruneKeepDuration string `yaml:"buildkit_prune_keep_duration"`
	// BuildKitPruneKeepStorageMB preserves this much BuildKit cache after pruning
	BuildKitPruneKeepStorageMB int `yaml:"buildkit_prune_keep_storage_mb"`
	// BuildKitPruneMinReclaimGB skips BuildKit pruning below this reclaimable cache size
	BuildKitPruneMinReclaimGB int `yaml:"buildkit_prune_min_reclaim_gb"`
	// CriticalSystemPrune enables broad critical system prune with volumes.
	CriticalSystemPrune bool `yaml:"critical_system_prune"`
	// CleanInsideVM enables cleanup inside Podman machine VM (Darwin)
	CleanInsideVM bool `yaml:"clean_inside_vm"`
	// TrimVMDisk enables fstrim inside VM to reclaim sparse disk space (Darwin)
	TrimVMDisk bool `yaml:"trim_vm_disk"`
	// CompactDiskOffline enables offline raw disk compaction at Critical level
	CompactDiskOffline bool `yaml:"compact_disk_offline"`
	// CompactMinReclaimGB is the minimum physical allocation worth compacting
	CompactMinReclaimGB int `yaml:"compact_min_reclaim_gb"`
	// CompactRequireNoActiveContainers skips offline compaction when containers are running
	CompactRequireNoActiveContainers bool `yaml:"compact_require_no_active_containers"`
	// CompactKeepBackupUntilRestart preserves the original image until restart verifies the compacted image
	CompactKeepBackupUntilRestart bool `yaml:"compact_keep_backup_until_restart"`
	// CompactProviderAllowlist restricts offline compaction to known providers
	CompactProviderAllowlist []string `yaml:"compact_provider_allowlist"`
	// CompactScratchDir stores the temporary compacted image; empty keeps it beside the VM disk
	CompactScratchDir string `yaml:"compact_scratch_dir"`
	// CompactQemuImgPath overrides qemu-img discovery for offline compaction
	CompactQemuImgPath string `yaml:"compact_qemu_img_path"`
}

// BazelConfig holds Bazel output base and cache cleanup settings.
type BazelConfig struct {
	// Roots are directories that may contain Bazel output-user-root trees or caches.
	Roots []string `yaml:"roots"`
	// WorkspaceRoots are developer workspace roots scanned for repo-local bazel-* symlinks after output-base deletion.
	WorkspaceRoots []string `yaml:"workspace_roots"`
	// BazeliskCache is the Bazelisk download cache path.
	BazeliskCache string `yaml:"bazelisk_cache"`
	// MaxTotalGB is the review budget across detected Bazel caches.
	MaxTotalGB int `yaml:"max_total_gb"`
	// KeepRecentOutputBases preserves this many newest output bases.
	KeepRecentOutputBases int `yaml:"keep_recent_output_bases"`
	// StaleAfter is the normal age threshold for output-base deletion candidates.
	StaleAfter string `yaml:"stale_after"`
	// CriticalStaleAfter is the critical-level age threshold.
	CriticalStaleAfter string `yaml:"critical_stale_after"`
	// ProtectWorkspaces preserves output bases reachable from these workspaces.
	ProtectWorkspaces []string `yaml:"protect_workspaces"`
	// AllowStopIdleServers allows future cleanup to stop idle Bazel servers.
	AllowStopIdleServers bool `yaml:"allow_stop_idle_servers"`
	// AllowDeleteActiveOutputBases allows future cleanup to delete active output bases.
	AllowDeleteActiveOutputBases bool `yaml:"allow_delete_active_output_bases"`
}

// NixConfig holds Nix store and profile generation cleanup settings.
type NixConfig struct {
	// MinUserGenerations preserves at least this many user profile generations.
	MinUserGenerations int `yaml:"min_user_generations"`
	// MinHomeManagerGenerations preserves at least this many Home Manager generations.
	MinHomeManagerGenerations int `yaml:"min_home_manager_generations"`
	// MinSystemGenerations preserves at least this many system/darwin generations when visible.
	MinSystemGenerations int `yaml:"min_system_generations"`
	// MaxUserGenerations caps retained user profile generations when greater than zero.
	MaxUserGenerations int `yaml:"max_user_generations"`
	// MaxHomeManagerGenerations caps retained Home Manager generations when greater than zero.
	MaxHomeManagerGenerations int `yaml:"max_home_manager_generations"`
	// HostMeasurePath is the filesystem path used for plugin-isolated host free-space deltas.
	HostMeasurePath string `yaml:"host_measure_path"`
	// DeleteGenerationsOlderThan is the normal generation age policy.
	DeleteGenerationsOlderThan string `yaml:"delete_generations_older_than"`
	// CriticalDeleteGenerationsOlderThan is the critical-level generation age policy.
	CriticalDeleteGenerationsOlderThan string `yaml:"critical_delete_generations_older_than"`
	// DeleteHomeManagerGenerations enables home-manager remove-generations for selected generations.
	DeleteHomeManagerGenerations bool `yaml:"delete_home_manager_generations"`
	// HomeManagerDeleteGenerationsOlderThan is the normal Home Manager generation age policy.
	HomeManagerDeleteGenerationsOlderThan string `yaml:"home_manager_delete_generations_older_than"`
	// HomeManagerCriticalDeleteGenerationsOlderThan is the critical-level Home Manager generation age policy.
	HomeManagerCriticalDeleteGenerationsOlderThan string `yaml:"home_manager_critical_delete_generations_older_than"`
	// AllowStoreOptimize enables nix-store --optimize at critical level.
	AllowStoreOptimize bool `yaml:"allow_store_optimize"`
	// SkipWhenDaemonBusy skips Nix cleanup when active Nix work is detected.
	SkipWhenDaemonBusy bool `yaml:"skip_when_daemon_busy"`
	// DaemonBusyBackoff is the operator-facing backoff after busy detection.
	DaemonBusyBackoff string `yaml:"daemon_busy_backoff"`
	// MaxGCDuration bounds nix-collect-garbage and related Nix commands.
	MaxGCDuration string `yaml:"max_gc_duration"`
	// RootAttributionLimit limits visible GC root targets in low-reclaim dry-runs.
	RootAttributionLimit int `yaml:"root_attribution_limit"`
}

// ICloudConfig holds iCloud-specific cleanup settings (Darwin).
type ICloudConfig struct {
	// EvictAfterDays - only evict files not accessed for this many days
	EvictAfterDays int `yaml:"evict_after_days"`
	// ExcludePaths - paths within iCloud Drive to never evict
	ExcludePaths []string `yaml:"exclude_paths"`
	// MinFileSizeMB - only evict files larger than this (MB)
	MinFileSizeMB int `yaml:"min_file_size_mb"`
}

// DevArtifactsConfig holds development artifact cleanup settings.
type DevArtifactsConfig struct {
	// ScanPaths is the list of directories to scan for dev artifacts
	ScanPaths []string `yaml:"scan_paths"`
	// ScanMaxDuration bounds filesystem walking for incident dry-runs
	ScanMaxDuration string `yaml:"scan_max_duration"`
	// ScanMaxEntries bounds recursive filesystem entries visited per dev-artifact scan
	ScanMaxEntries int `yaml:"scan_max_entries"`
	// WorkspaceScanMaxRoots bounds top-level workspace roots visited per artifact family per cycle.
	WorkspaceScanMaxRoots int `yaml:"workspace_scan_max_roots"`
	// TempArtifacts enables review-only reporting for large top-level temp artifacts
	TempArtifacts bool `yaml:"temp_artifacts"`
	// TempScanPaths are top-level temporary directories scanned for large generated artifacts
	TempScanPaths []string `yaml:"temp_scan_paths"`
	// TempScanMaxRoots bounds top-level temp roots inspected per temp scan path
	TempScanMaxRoots int `yaml:"temp_scan_max_roots"`
	// TempArtifactMinMB is the minimum physical size for review-only temporary artifact targets
	TempArtifactMinMB int `yaml:"temp_artifact_min_mb"`
	// TempArtifactStaleAfter is the age after which temp artifacts become review candidates
	TempArtifactStaleAfter string `yaml:"temp_artifact_stale_after"`
	// HarnessScratch enables session-level cleanup of agent-harness scratch
	// containers (e.g. /private/tmp/claude-<uid>/<project>/<session-uuid>)
	// found under temp_scan_paths. Sessions are evaluated individually so a
	// huge container is never sized or protected as one opaque root.
	HarnessScratch bool `yaml:"harness_scratch"`
	// HarnessScratchDirPatterns are glob patterns matching top-level temp
	// entries treated as harness scratch containers.
	HarnessScratchDirPatterns []string `yaml:"harness_scratch_dir_patterns"`
	// HarnessScratchStaleAfter is the age after which an idle scratch session
	// becomes a deletion candidate. Deliberately its own knob (NOT
	// temp_artifact_stale_after, which gates review-only reporting) and
	// conservative by default: idle-but-open multi-day sessions are normal.
	HarnessScratchStaleAfter string `yaml:"harness_scratch_stale_after"`
	// NixTempRoots enables deletion of stale top-level nix-shell/nix-develop temp roots
	NixTempRoots bool `yaml:"nix_temp_roots"`
	// NixTempRootMinMB is the minimum physical size for stale Nix temp-root deletion candidates
	NixTempRootMinMB int `yaml:"nix_temp_root_min_mb"`
	// NixTempRootStaleAfter is the age after which Nix temp roots become deletion candidates
	NixTempRootStaleAfter string `yaml:"nix_temp_root_stale_after"`
	// NodeModules enables node_modules cleanup
	NodeModules bool `yaml:"node_modules"`
	// PythonVenvs enables .venv cleanup
	PythonVenvs bool `yaml:"python_venvs"`
	// RustTargets enables Rust target/ cleanup
	RustTargets bool `yaml:"rust_targets"`
	// ZigArtifacts enables Zig .zig-cache/ and zig-out/ cleanup
	ZigArtifacts bool `yaml:"zig_artifacts"`
	// AgentWorktreeArtifacts enables cleanup of generated outputs inside agent worktree roots.
	AgentWorktreeArtifacts bool `yaml:"agent_worktree_artifacts"`
	// AgentWorktreeRootCleanup enables deletion of stale top-level agent worktree roots.
	AgentWorktreeRootCleanup bool `yaml:"agent_worktree_root_cleanup"`
	// AgentWorktreeRoots are top-level directories containing agent scratch worktrees.
	AgentWorktreeRoots []string `yaml:"agent_worktree_roots"`
	// AgentWorktreeStaleAfter is the age after which top-level agent worktrees are cleanup candidates.
	AgentWorktreeStaleAfter string `yaml:"agent_worktree_stale_after"`
	// AgentTranscriptCompression enables gzip compression for stale agent transcript JSONL files.
	AgentTranscriptCompression bool `yaml:"agent_transcript_compression"`
	// AgentTranscriptRoots are directories scanned recursively for agent transcript JSONL files.
	AgentTranscriptRoots []string `yaml:"agent_transcript_roots"`
	// AgentTranscriptCompressAfter is the age after which JSONL transcripts are compressed.
	AgentTranscriptCompressAfter string `yaml:"agent_transcript_compress_after"`
	// PnpmStore enables pnpm store prune for the rebuildable global package store.
	PnpmStore bool `yaml:"pnpm_store"`
	// GoBuildCache enables Go build cache cleanup
	GoBuildCache bool `yaml:"go_build_cache"`
	// HaskellCache enables .ghcup/cache and .cabal/store cleanup
	HaskellCache bool `yaml:"haskell_cache"`
	// LMStudioModels enables .lmstudio model cleanup (opt-in)
	LMStudioModels bool `yaml:"lmstudio_models"`
	// LargeLocalArtifacts enables review-only reporting for large disk images and VM bundles
	LargeLocalArtifacts bool `yaml:"large_local_artifacts"`
	// LargeLocalArtifactMinMB is the minimum physical size for review-only large local artifact targets
	LargeLocalArtifactMinMB int `yaml:"large_local_artifact_min_mb"`
	// ProtectPaths are paths that should never be cleaned
	ProtectPaths []string `yaml:"protect_paths"`
}

// DarwinDevCachesConfig holds macOS developer-cache budget settings.
type DarwinDevCachesConfig struct {
	// Enabled controls typed Darwin developer-cache planning.
	Enabled bool `yaml:"enabled"`
	// Enforce enables real deletion of eligible Darwin developer-cache targets.
	// Defaults false so the typed target surface remains review-only unless operators opt in.
	Enforce bool `yaml:"enforce"`
	// MaxTotalGB is the review budget across known Darwin developer caches.
	MaxTotalGB int `yaml:"max_total_gb"`
	// JetBrains controls JetBrains cache planning.
	JetBrains DarwinDevCacheToolConfig `yaml:"jetbrains"`
	// Playwright controls Playwright browser cache planning.
	Playwright DarwinDevCacheToolConfig `yaml:"playwright"`
	// Bazelisk controls Bazelisk download cache planning.
	Bazelisk DarwinDevCacheToolConfig `yaml:"bazelisk"`
	// Pip controls pip cache planning.
	Pip DarwinDevCacheToolConfig `yaml:"pip"`
	// NPM controls npm package cache planning.
	NPM DarwinDevCacheToolConfig `yaml:"npm"`
	// UV controls uv package cache planning.
	UV DarwinDevCacheToolConfig `yaml:"uv"`
	// Bun controls Bun cache planning.
	Bun DarwinDevCacheToolConfig `yaml:"bun"`
	// OpenCode controls opencode cache planning.
	OpenCode DarwinDevCacheToolConfig `yaml:"opencode"`
	// VSCode controls Visual Studio Code cache planning.
	VSCode DarwinDevCacheToolConfig `yaml:"vscode"`
	// Cursor controls Cursor cache planning.
	Cursor DarwinDevCacheToolConfig `yaml:"cursor"`
}

// DarwinDevCacheToolConfig holds per-tool cache budget settings.
type DarwinDevCacheToolConfig struct {
	// Enabled controls this cache family.
	Enabled bool `yaml:"enabled"`
	// MaxGB is the review budget for this cache family.
	MaxGB int `yaml:"max_gb,omitempty"`
	// StaleAfterDays is the age after which entries become cleanup candidates.
	StaleAfterDays int `yaml:"stale_after_days,omitempty"`
	// KeepLatest keeps this many newest entries when versioned.
	KeepLatest int `yaml:"keep_latest,omitempty"`
	// KeepLatestPerFamily keeps the newest entry for each detected browser/tool family.
	KeepLatestPerFamily bool `yaml:"keep_latest_per_family,omitempty"`
	// KeepActiveVersions preserves versions with active-use evidence.
	KeepActiveVersions bool `yaml:"keep_active_versions,omitempty"`
}

// APFSConfig holds APFS snapshot cleanup settings (Darwin).
type APFSConfig struct {
	// ThinEnabled enables APFS snapshot thinning
	ThinEnabled bool `yaml:"thin_enabled"`
	// MaxThinGB is the maximum GB to request for thinning
	MaxThinGB int `yaml:"max_thin_gb"`
	// KeepRecentDays keeps snapshots newer than this many days
	KeepRecentDays int `yaml:"keep_recent_days"`
	// DeleteOSUpdates allows deleting pre-update snapshots at Critical level
	DeleteOSUpdates bool `yaml:"delete_os_updates"`
}

// NotifyConfig holds notification settings.
type NotifyConfig struct {
	// Enabled for notifications
	Enabled bool `yaml:"enabled"`
	// WebhookURL for Slack/Discord notifications
	WebhookURL string `yaml:"webhook_url"`
}

// DefaultConfig returns the default configuration.
func DefaultConfig() *Config {
	home, _ := os.UserHomeDir()
	logFile := filepath.Join(home, ".local", "log", "disk-cleanup.log")
	stateFile := filepath.Join(home, ".local", "state", "tinyland-cleanup", "state.json")

	defaultScanPaths := []string{
		filepath.Join(home, "git"),
		filepath.Join(home, "src"),
		filepath.Join(home, "projects"),
	}
	defaultTempScanPaths := []string{"/tmp"}
	if runtime.GOOS == "darwin" {
		defaultTempScanPaths = []string{"/private/tmp"}
	}
	bazeliskCache := filepath.Join(home, ".cache", "bazelisk")
	if runtime.GOOS == "darwin" {
		bazeliskCache = filepath.Join(home, "Library", "Caches", "bazelisk")
	}

	config := &Config{
		PollInterval: 60,
		Thresholds: Thresholds{
			Warning:    80,
			Moderate:   85,
			Aggressive: 90,
			Critical:   95,
		},
		InodeThresholds: Thresholds{
			Warning:    80,
			Moderate:   85,
			Aggressive: 90,
			Critical:   95,
		},
		TargetFree: 70,
		Policy: PolicyConfig{
			Cooldown:             "30m",
			CooldownBypassLevel:  "critical",
			MinimumFreeGB:        0,
			StateFile:            stateFile,
			InodeNoProgressLimit: 3,
		},
		LogFile: logFile,
		Enable: EnableFlags{
			Cache:         true,
			NixGC:         true,
			Docker:        true,
			Podman:        true,
			Lima:          runtime.GOOS == "darwin",
			Homebrew:      runtime.GOOS == "darwin",
			IOSSimulator:  runtime.GOOS == "darwin",
			GitLabRunner:  true,
			ICloud:        runtime.GOOS == "darwin",
			Photos:        runtime.GOOS == "darwin",
			DevArtifacts:  true,
			Bazel:         true,
			APFSSnapshots: runtime.GOOS == "darwin",
		},
		Docker: DockerConfig{
			PruneImagesAge:           "24h",
			ProtectRunningContainers: true,
		},
		Podman: PodmanConfig{
			PruneImagesAge:                   "24h",
			ProtectRunningContainers:         true,
			MachineNames:                     []string{"podman-machine-default"},
			BuildKitPrune:                    true,
			BuildKitPruneKeepDuration:        "24h",
			BuildKitPruneKeepStorageMB:       8192,
			BuildKitPruneMinReclaimGB:        4,
			CriticalSystemPrune:              false,
			CleanInsideVM:                    true,
			TrimVMDisk:                       true,
			CompactMinReclaimGB:              8,
			CompactRequireNoActiveContainers: true,
			CompactKeepBackupUntilRestart:    true,
			CompactProviderAllowlist:         []string{"applehv", "libkrun", "qemu"},
		},
		Bazel: BazelConfig{
			Roots:                 defaultBazelRoots(home),
			WorkspaceRoots:        defaultScanPaths,
			BazeliskCache:         bazeliskCache,
			MaxTotalGB:            20,
			KeepRecentOutputBases: 5,
			StaleAfter:            "14d",
			CriticalStaleAfter:    "3d",
			ProtectWorkspaces: []string{
				filepath.Join(home, "git", "lab"),
				filepath.Join(home, "git", "GloriousFlywheel"),
			},
			AllowStopIdleServers:         true,
			AllowDeleteActiveOutputBases: false,
		},
		Nix: NixConfig{
			MinUserGenerations:                            5,
			MinHomeManagerGenerations:                     5,
			MinSystemGenerations:                          3,
			MaxUserGenerations:                            0,
			MaxHomeManagerGenerations:                     0,
			HostMeasurePath:                               "/nix/store",
			DeleteGenerationsOlderThan:                    "14d",
			CriticalDeleteGenerationsOlderThan:            "3d",
			DeleteHomeManagerGenerations:                  false,
			HomeManagerDeleteGenerationsOlderThan:         "14d",
			HomeManagerCriticalDeleteGenerationsOlderThan: "3d",
			AllowStoreOptimize:                            false,
			SkipWhenDaemonBusy:                            true,
			DaemonBusyBackoff:                             "30m",
			MaxGCDuration:                                 "20m",
			RootAttributionLimit:                          20,
		},
		Lima: LimaConfig{
			VMNames: []string{"colima", "unified"},
		},
		ICloud: ICloudConfig{
			EvictAfterDays: 30,
			ExcludePaths:   []string{},
			MinFileSizeMB:  10,
		},
		DevArtifacts: DevArtifactsConfig{
			ScanPaths:              defaultScanPaths,
			ScanMaxDuration:        "30s",
			ScanMaxEntries:         250000,
			WorkspaceScanMaxRoots:  8,
			TempArtifacts:          true,
			TempScanPaths:          defaultTempScanPaths,
			TempScanMaxRoots:       128,
			TempArtifactMinMB:      256,
			TempArtifactStaleAfter: "6h",
			HarnessScratch:         true,
			HarnessScratchDirPatterns: []string{
				"claude-*",
			},
			HarnessScratchStaleAfter: "36h",
			NixTempRoots:             true,
			NixTempRootMinMB:         1,
			NixTempRootStaleAfter:    "24h",
			NodeModules:              true,
			PythonVenvs:              true,
			RustTargets:              true,
			ZigArtifacts:             true,
			AgentWorktreeArtifacts:   false,
			AgentWorktreeRootCleanup: false,
			AgentWorktreeRoots: []string{
				filepath.Join(home, ".claude", "worktrees"),
			},
			AgentWorktreeStaleAfter:      "14d",
			AgentTranscriptCompression:   false,
			AgentTranscriptRoots:         []string{filepath.Join(home, ".codex", "sessions")},
			AgentTranscriptCompressAfter: "14d",
			PnpmStore:                    false,
			GoBuildCache:                 true,
			HaskellCache:                 true,
			LMStudioModels:               false,
			LargeLocalArtifacts:          true,
			LargeLocalArtifactMinMB:      1024,
			ProtectPaths:                 []string{},
		},
		DarwinDevCaches: DarwinDevCachesConfig{
			Enabled:    runtime.GOOS == "darwin",
			Enforce:    false,
			MaxTotalGB: 15,
			JetBrains: DarwinDevCacheToolConfig{
				Enabled:            true,
				MaxGB:              8,
				StaleAfterDays:     14,
				KeepActiveVersions: true,
			},
			Playwright: DarwinDevCacheToolConfig{
				Enabled:             true,
				KeepLatestPerFamily: true,
			},
			Bazelisk: DarwinDevCacheToolConfig{
				Enabled:    true,
				KeepLatest: 2,
			},
			Pip: DarwinDevCacheToolConfig{
				Enabled:        true,
				StaleAfterDays: 14,
			},
			NPM: DarwinDevCacheToolConfig{
				Enabled:        true,
				StaleAfterDays: 14,
			},
			UV: DarwinDevCacheToolConfig{
				Enabled:        true,
				StaleAfterDays: 14,
			},
			Bun: DarwinDevCacheToolConfig{
				Enabled:        true,
				StaleAfterDays: 14,
			},
			OpenCode: DarwinDevCacheToolConfig{
				Enabled:        true,
				StaleAfterDays: 14,
			},
			VSCode: DarwinDevCacheToolConfig{
				Enabled:            true,
				MaxGB:              4,
				StaleAfterDays:     14,
				KeepActiveVersions: true,
			},
			Cursor: DarwinDevCacheToolConfig{
				Enabled:            true,
				MaxGB:              4,
				StaleAfterDays:     14,
				KeepActiveVersions: true,
			},
		},
		APFS: APFSConfig{
			ThinEnabled:     true,
			MaxThinGB:       50,
			KeepRecentDays:  1,
			DeleteOSUpdates: true,
		},
		Notify: NotifyConfig{
			Enabled: false,
		},
	}

	// Platform-specific socket defaults
	if runtime.GOOS == "darwin" {
		config.Docker.Socket = filepath.Join(home, ".colima", "default", "docker.sock")
	} else {
		config.Docker.Socket = "/var/run/docker.sock"
	}

	return config
}

func defaultBazelRoots(home string) []string {
	roots := []string{filepath.Join(home, ".cache", "bazel")}
	if runtime.GOOS == "darwin" {
		if user := os.Getenv("USER"); user != "" {
			roots = append(roots,
				filepath.Join("/private", "var", "tmp", "_bazel_"+user),
				filepath.Join("/var", "tmp", "_bazel_"+user),
			)
		}
		roots = append(roots, "/private/tmp")
	}
	return roots
}

// LoadConfig loads configuration from a YAML file, merging with defaults.
func LoadConfig(path string) (*Config, error) {
	config := DefaultConfig()

	if path == "" {
		return config, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return config, nil
		}
		return nil, err
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return config, nil
	}

	decoder := yaml.NewDecoder(bytes.NewReader(trimmed))
	decoder.KnownFields(true)
	if err := decoder.Decode(config); err != nil {
		return nil, err
	}

	return config, nil
}

// SaveConfig saves configuration to a YAML file.
func SaveConfig(config *Config, path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := yaml.Marshal(config)
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}
