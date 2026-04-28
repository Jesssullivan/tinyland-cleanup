// Package plugins provides the cleanup plugin interface and registration.
package plugins

import (
	"context"
	"log/slog"
	"runtime"
	"strings"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// CleanupLevel represents the cleanup severity level.
type CleanupLevel int

const (
	// LevelNone means no cleanup needed
	LevelNone CleanupLevel = iota
	// LevelWarning triggers light cleanup (caches)
	LevelWarning
	// LevelModerate triggers moderate cleanup (container images)
	LevelModerate
	// LevelAggressive triggers aggressive cleanup (volumes)
	LevelAggressive
	// LevelCritical triggers emergency cleanup (everything)
	LevelCritical
)

const (
	// CleanupTierSafe is trivially rebuildable cache with low active-work impact.
	CleanupTierSafe = "safe"
	// CleanupTierWarm is rebuildable but expensive enough to slow active work.
	CleanupTierWarm = "warm"
	// CleanupTierDisruptive requires stopping services, daemons, VMs, or tools.
	CleanupTierDisruptive = "disruptive"
	// CleanupTierDestructive may remove local-only generated artifacts or large user-managed assets.
	CleanupTierDestructive = "destructive"
	// CleanupTierPrivileged requires sudo, launchd/systemd, or host package manager authority.
	CleanupTierPrivileged = "privileged"
)

const (
	// CleanupReclaimHost means the planned action is expected to increase host free space.
	CleanupReclaimHost = "host"
	// CleanupReclaimDeferred means the action only enables later host free-space reclamation.
	CleanupReclaimDeferred = "deferred"
	// CleanupReclaimNone means the plan item is review/protection metadata only.
	CleanupReclaimNone = "none"
)

// String returns the string representation of the cleanup level.
func (l CleanupLevel) String() string {
	switch l {
	case LevelNone:
		return "none"
	case LevelWarning:
		return "warning"
	case LevelModerate:
		return "moderate"
	case LevelAggressive:
		return "aggressive"
	case LevelCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// CleanupResult represents the result of a cleanup operation.
type CleanupResult struct {
	// Plugin name that performed the cleanup
	Plugin string
	// Level at which cleanup was performed
	Level CleanupLevel
	// BytesFreed is the legacy aggregate byte count reported by the plugin.
	BytesFreed int64
	// EstimatedBytesFreed is based on local size estimates before deletion.
	EstimatedBytesFreed int64
	// CommandBytesFreed is reported by an external cleanup command.
	CommandBytesFreed int64
	// HostBytesFreed is measured from host free-space deltas when isolated.
	HostBytesFreed int64
	// ItemsCleaned is the number of items cleaned (files, images, etc.)
	ItemsCleaned int
	// Error if cleanup failed
	Error error
}

// CleanupPlan describes what a dry-run cleanup cycle would do.
type CleanupPlan struct {
	// Plugin is the plugin that produced the plan.
	Plugin string `json:"plugin"`
	// Level is the cleanup level being planned.
	Level string `json:"level"`
	// Summary is a short human-readable plan summary.
	Summary string `json:"summary"`
	// WouldRun reports whether the planned action is currently eligible.
	WouldRun bool `json:"would_run"`
	// SkipReason explains why the planned action is not eligible.
	SkipReason string `json:"skip_reason,omitempty"`
	// EstimatedBytesFreed is the best available reclaim estimate.
	EstimatedBytesFreed int64 `json:"estimated_bytes_freed,omitempty"`
	// RequiredFreeBytes is the host free space needed before the action.
	RequiredFreeBytes int64 `json:"required_free_bytes,omitempty"`
	// Steps lists the operator-visible steps the cleanup would perform.
	Steps []string `json:"steps,omitempty"`
	// Targets lists concrete files, directories, or resources considered by the plan.
	Targets []CleanupTarget `json:"targets,omitempty"`
	// Warnings lists safety warnings or lossy accounting caveats.
	Warnings []string `json:"warnings,omitempty"`
	// Metadata carries plugin-specific plan facts.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// CleanupTarget is one concrete cleanup candidate in a dry-run plan.
type CleanupTarget struct {
	// Type is the cache or resource class.
	Type string `json:"type"`
	// Tier classifies the operational risk/rebuild cost of the target.
	Tier string `json:"tier,omitempty"`
	// Name is a short target label.
	Name string `json:"name"`
	// Version is the tool or cache version when detectable.
	Version string `json:"version,omitempty"`
	// Path is the local filesystem path for file-backed targets.
	Path string `json:"path,omitempty"`
	// Bytes is the measured physical target size when available.
	Bytes int64 `json:"bytes"`
	// LogicalBytes is the logical target size when it differs from physical allocation.
	LogicalBytes int64 `json:"logical_bytes,omitempty"`
	// Reclaim describes whether the planned action should reclaim host space.
	Reclaim string `json:"reclaim,omitempty"`
	// HostReclaimsSpace reports whether the planned action should increase host free space.
	HostReclaimsSpace *bool `json:"host_reclaims_space,omitempty"`
	// Active reports whether active-use evidence was detected.
	Active bool `json:"active"`
	// Protected reports whether the plan should preserve the target.
	Protected bool `json:"protected"`
	// Action describes the planned action.
	Action string `json:"action"`
	// Reason explains why the action was selected.
	Reason string `json:"reason,omitempty"`
}

func annotateCleanupTargetPolicy(target *CleanupTarget, tier string, reclaim string) {
	target.Tier = tier
	target.Reclaim = reclaim
	target.HostReclaimsSpace = hostReclaimExpectation(reclaim)
}

func hostReclaimExpectation(reclaim string) *bool {
	if reclaim == "" {
		return nil
	}
	reclaims := reclaim == CleanupReclaimHost
	return &reclaims
}

func hostReclaimForAction(action string) string {
	switch {
	case strings.HasPrefix(action, "delete"),
		action == "stop_idle_server_then_delete_output_base",
		action == "clean-cache",
		action == "clean-stale-files",
		action == "thin_local_snapshots":
		return CleanupReclaimHost
	case action == "review",
		action == "report",
		action == "protect",
		action == "keep",
		strings.HasPrefix(action, "review_"),
		strings.HasPrefix(action, "keep_"):
		return CleanupReclaimNone
	default:
		return CleanupReclaimNone
	}
}

// Plugin is the interface that cleanup plugins must implement.
type Plugin interface {
	// Name returns the plugin's unique identifier
	Name() string

	// Description returns a human-readable description
	Description() string

	// SupportedPlatforms returns the platforms this plugin supports
	// Empty slice means all platforms
	SupportedPlatforms() []string

	// Enabled returns whether this plugin is enabled
	Enabled(cfg *config.Config) bool

	// Cleanup performs cleanup at the specified level
	// level indicates the severity of cleanup needed
	// ctx allows cancellation of long-running operations
	Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult
}

// Planner is implemented by plugins that can produce a detailed dry-run plan.
type Planner interface {
	PlanCleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupPlan
}

// Registry holds registered cleanup plugins.
type Registry struct {
	plugins []Plugin
}

// NewRegistry creates a new plugin registry.
func NewRegistry() *Registry {
	return &Registry{
		plugins: make([]Plugin, 0),
	}
}

// Register adds a plugin to the registry.
func (r *Registry) Register(p Plugin) {
	r.plugins = append(r.plugins, p)
}

// GetEnabled returns all enabled plugins for the current platform and configuration.
func (r *Registry) GetEnabled(cfg *config.Config) []Plugin {
	platform := currentPlatform()
	enabled := make([]Plugin, 0)

	for _, p := range r.plugins {
		// Check if plugin is enabled in config
		if !p.Enabled(cfg) {
			continue
		}

		// Check platform support
		supported := p.SupportedPlatforms()
		if len(supported) == 0 {
			// Empty means all platforms supported
			enabled = append(enabled, p)
			continue
		}

		for _, sp := range supported {
			if sp == platform {
				enabled = append(enabled, p)
				break
			}
		}
	}

	return enabled
}

// GetAll returns all registered plugins.
func (r *Registry) GetAll() []Plugin {
	return r.plugins
}

// currentPlatform returns the current platform identifier.
func currentPlatform() string {
	// Use GOOS for simplicity - could be expanded for more specific detection
	// Return values: "darwin", "linux", "windows"
	return goos()
}

// goosValue is the actual GOOS value, stored as a variable for testing.
var goosValue = runtime.GOOS

// goos returns the current OS.
func goos() string {
	return goosValue
}

// Platform constants
const (
	PlatformDarwin  = "darwin"
	PlatformLinux   = "linux"
	PlatformWindows = "windows"
)
