// Package plugins provides the cleanup plugin interface and registration.
package plugins

import (
	"context"
	"log/slog"
	"runtime"

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
	// BytesFreed is the estimated bytes freed
	BytesFreed int64
	// ItemsCleaned is the number of items cleaned (files, images, etc.)
	ItemsCleaned int
	// Error if cleanup failed
	Error error
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
