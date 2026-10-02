package plugins

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// DebrisReportPlugin inventories stale incident and agent debris — dated
// bench directories, bulkload scratch, pre-boundary and continuity captures,
// rollback and carry trees, reclaim-pending markers — and logs each path with
// its size. It is report-only: it never deletes, and it is off by default.
type DebrisReportPlugin struct {
	now func() time.Time
}

// NewDebrisReportPlugin creates the report-only debris inventory plugin.
func NewDebrisReportPlugin() *DebrisReportPlugin {
	return &DebrisReportPlugin{now: time.Now}
}

// Name returns the plugin identifier.
func (p *DebrisReportPlugin) Name() string {
	return "debris-report"
}

// Description returns the plugin description.
func (p *DebrisReportPlugin) Description() string {
	return "Reports stale incident/agent debris (dated bench dirs, bulkload scratch, rollback and carry trees, reclaim markers); never deletes"
}

// SupportedPlatforms returns supported platforms (all).
func (p *DebrisReportPlugin) SupportedPlatforms() []string {
	return []string{}
}

// Enabled reports whether the debris report is enabled (config-gated, default off).
func (p *DebrisReportPlugin) Enabled(cfg *config.Config) bool {
	return cfg.Enable.DebrisReport
}

type debrisEntry struct {
	Path    string
	Pattern string
	Bytes   int64
	ModTime time.Time
	IsDir   bool
}

type debrisScan struct {
	entries  []debrisEntry
	warnings []string
	now      time.Time
	cutoff   time.Duration
}

// Cleanup logs every stale debris entry with its path and size. Nothing is
// removed and no bytes are claimed as freed.
func (p *DebrisReportPlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: level}

	scan := p.scan(ctx, cfg)
	var total int64
	for _, entry := range scan.entries {
		total += entry.Bytes
		logger.Info("debris candidate (report only)",
			"path", entry.Path,
			"bytes", entry.Bytes,
			"size_mb", entry.Bytes/(1024*1024),
			"age", scan.now.Sub(entry.ModTime).Round(time.Hour).String(),
			"pattern", entry.Pattern,
			"dir", entry.IsDir,
		)
	}
	for _, warning := range scan.warnings {
		logger.Warn("debris report scan warning", "warning", warning)
	}
	logger.Info("debris report complete (nothing deleted)",
		"entries", len(scan.entries),
		"total_bytes", total,
		"total_mb", total/(1024*1024),
		"older_than", scan.cutoff.String(),
	)
	return result
}

// PlanCleanup lists the same inventory as report-only targets for dry-run output.
func (p *DebrisReportPlugin) PlanCleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupPlan {
	scan := p.scan(ctx, cfg)
	plan := CleanupPlan{
		Plugin:   p.Name(),
		Level:    level.String(),
		WouldRun: true,
		Steps:    []string{"Inventory stale debris matching the configured patterns and log path + size", "Delete nothing"},
		Warnings: scan.warnings,
		Metadata: map[string]string{"older_than": scan.cutoff.String()},
	}
	var total int64
	for _, entry := range scan.entries {
		total += entry.Bytes
		target := CleanupTarget{
			Type:      "debris",
			Name:      filepath.Base(entry.Path),
			Path:      entry.Path,
			Bytes:     entry.Bytes,
			Action:    "report",
			Reason:    fmt.Sprintf("matches %s, %s old; report only", entry.Pattern, scan.now.Sub(entry.ModTime).Round(time.Hour)),
			Protected: false,
		}
		annotateCleanupTargetPolicy(&target, CleanupTierSafe, CleanupReclaimNone)
		plan.Targets = append(plan.Targets, target)
	}
	plan.Summary = fmt.Sprintf("report-only: %d stale debris entries totalling %d MiB older than %s; nothing is deleted",
		len(scan.entries), total/(1024*1024), scan.cutoff)
	annotateCleanupPlanTargetAccounting(&plan)
	return plan
}

func (p *DebrisReportPlugin) scan(ctx context.Context, cfg *config.Config) debrisScan {
	now := p.now()
	drCfg := cfg.DebrisReport
	scan := debrisScan{now: now, cutoff: parseNixPolicyDuration(drCfg.OlderThan, 24*time.Hour)}
	if raw := strings.TrimSpace(drCfg.OlderThan); raw != "" && parseNixPolicyDuration(raw, -1) < 0 {
		scan.warnings = append(scan.warnings, fmt.Sprintf("debris_report.older_than %q is not a duration; using %s", raw, scan.cutoff))
	}
	patterns := drCfg.Patterns
	if len(patterns) == 0 {
		patterns = config.DefaultDebrisPatterns()
	}
	maxDepth := drCfg.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 2
	}
	home, _ := os.UserHomeDir()
	seen := map[string]bool{}
	for _, root := range drCfg.ScanPaths {
		expanded := expandHome(root, home)
		if !pathExistsAndIsDir(expanded) {
			continue
		}
		rootDev, _ := deviceID(expanded)
		p.walk(ctx, expanded, 0, maxDepth, rootDev, patterns, seen, &scan)
		if ctx.Err() != nil {
			scan.warnings = append(scan.warnings, "debris scan stopped early: "+ctx.Err().Error())
			break
		}
	}
	sort.Slice(scan.entries, func(i, j int) bool {
		if scan.entries[i].Bytes != scan.entries[j].Bytes {
			return scan.entries[i].Bytes > scan.entries[j].Bytes
		}
		return scan.entries[i].Path < scan.entries[j].Path
	})
	return scan
}

func (p *DebrisReportPlugin) walk(ctx context.Context, dir string, depth, maxDepth int, rootDev uint64, patterns []string, seen map[string]bool, scan *debrisScan) {
	if ctx.Err() != nil || depth >= maxDepth {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		scan.warnings = append(scan.warnings, fmt.Sprintf("%s: %v", dir, err))
		return
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if seen[path] {
			continue
		}
		if dev, err := deviceID(path); err == nil && rootDev != 0 && dev != rootDev {
			continue // never cross mount boundaries
		}
		if pattern, ok := matchDebrisPattern(entry.Name(), entry.IsDir(), patterns); ok {
			seen[path] = true
			info, err := entry.Info()
			if err != nil {
				continue
			}
			modTime := effectiveModTime(info, scan.now)
			if scan.now.Sub(modTime) < scan.cutoff {
				continue
			}
			var bytes int64
			if entry.IsDir() {
				bytes, _ = getDirAllocatedBytesContext(ctx, path)
			} else if allocated, err := getFileAllocatedBytes(path); err == nil {
				bytes = allocated
			}
			scan.entries = append(scan.entries, debrisEntry{Path: path, Pattern: pattern, Bytes: bytes, ModTime: modTime, IsDir: entry.IsDir()})
			continue // do not descend into matched trees
		}
		if entry.IsDir() {
			p.walk(ctx, path, depth+1, maxDepth, rootDev, patterns, seen, scan)
		}
	}
}

// matchDebrisPattern returns the first pattern matching a base name. A
// trailing "/" on a pattern restricts it to directories.
func matchDebrisPattern(name string, isDir bool, patterns []string) (string, bool) {
	for _, pattern := range patterns {
		glob := pattern
		if strings.HasSuffix(pattern, "/") {
			if !isDir {
				continue
			}
			glob = strings.TrimSuffix(pattern, "/")
		}
		if ok, err := filepath.Match(glob, name); err == nil && ok {
			return pattern, true
		}
	}
	return "", false
}
