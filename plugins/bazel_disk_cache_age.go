package plugins

// bazel_disk_cache_age.go adds per-entry age retention to the bazel plugin's
// disk_cache handling (TIN-3082). The plugin already reports a disk_cache as
// one cache tier and deletes the whole tier when the Bazel footprint exceeds
// max_total_gb. This rule instead expires individual ac/ and cas/ entries
// whose last use (the later of atime and mtime, so a noatime mount never
// makes a fresh entry look old) is older than bazel.disk_cache_max_age
// (default 14d). It reports only until bazel.disk_cache_age_delete is true,
// it is skipped while any Bazel client command is visible or the process
// inspection failed, and every entry is re-checked just before removal.
// Bazel's own tmp/ and gc/ directories are never touched.

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

const (
	bazelDiskCacheAgeType   = "disk_cache_entries"
	bazelDiskCacheAgeDelete = "delete_expired_disk_cache_entries"
	bazelDiskCacheAgeReport = "report_expired_disk_cache_entries"
)

// bazelDiskCacheEntryDirs are the entry stores of a Bazel disk cache.
var bazelDiskCacheEntryDirs = []string{"ac", "cas"}

// bazelDiskCacheAge returns the configured age, or 0 when the rule is off.
func bazelDiskCacheAge(cfg config.BazelConfig) time.Duration {
	if cfg.DiskCacheMaxAge == "" {
		return 0
	}
	d, err := config.ParseDuration(cfg.DiskCacheMaxAge)
	if err != nil || d < config.BazelDiskCacheMinMaxAge {
		return 0
	}
	return d
}

// isBazelDiskCacheDir recognises a disk cache by its ac/ or cas/ store.
func isBazelDiskCacheDir(path string) bool {
	for _, d := range bazelDiskCacheEntryDirs {
		if pathExistsAndIsDir(filepath.Join(path, d)) {
			return true
		}
	}
	return false
}

// bazelDiskCachePaths lists the disk caches the age rule covers: those the
// root discovery found plus bazel.disk_cache_paths, each once.
func bazelDiskCachePaths(cfg config.BazelConfig, candidates []bazelCandidate, home string) []string {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	var raw []string
	for _, c := range candidates {
		if c.Type == "disk_cache" {
			raw = append(raw, c.Path)
		}
	}
	for _, p := range cfg.DiskCachePaths {
		raw = append(raw, config.ExpandUserPath(p, home, name))
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range raw {
		p = filepath.Clean(p)
		real := resolvedOrSelf(p)
		if seen[real] || !isBazelDiskCacheDir(p) {
			continue
		}
		seen[real] = true
		out = append(out, p)
	}
	return out
}

// bazelDiskCacheLastUse is the later of an entry's atime and mtime.
func bazelDiskCacheLastUse(info os.FileInfo, now time.Time) time.Time {
	last := effectiveModTime(info, now)
	if at, ok := bbAccessTime(info); ok && at.After(last) && !at.After(now.Add(futureMtimeTolerance)) {
		last = at
	}
	return last
}

// bazelDiskCacheScan is what one walk of a disk cache found.
type bazelDiskCacheScan struct {
	entries, expired    int
	bytes, expiredBytes int64
	truncated           bool
	foreign             int
}

// walkBazelDiskCache visits every regular entry file under ac/ and cas/.
// visit is called for expired entries; returning an error stops the walk.
func walkBazelDiskCache(ctx context.Context, cache string, cutoff, now time.Time, limit int, visit func(path string, info os.FileInfo, allocated int64) error) (bazelDiskCacheScan, error) {
	var scan bazelDiskCacheScan
	uid := os.Geteuid()
	for _, store := range bazelDiskCacheEntryDirs {
		dir := filepath.Join(cache, store)
		if !pathExistsAndIsDir(dir) {
			continue
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			if limit > 0 && scan.entries >= limit {
				scan.truncated = true
				return fs.SkipAll
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok || int(st.Uid) != uid {
				scan.foreign++
				return nil
			}
			allocated := int64(st.Blocks) * 512
			scan.entries++
			scan.bytes += allocated
			if !bazelDiskCacheLastUse(info, now).Before(cutoff) {
				return nil
			}
			scan.expired++
			scan.expiredBytes += allocated
			if visit != nil {
				return visit(path, info, allocated)
			}
			return nil
		})
		if err != nil {
			return scan, err
		}
	}
	return scan, nil
}

// planBazelDiskCacheAge returns one target per disk cache with the bytes of
// its expired entries.
func planBazelDiskCacheAge(ctx context.Context, cfg config.BazelConfig, candidates []bazelCandidate, home string, globalActive bool, activeErr error, now time.Time) ([]CleanupTarget, map[string]string) {
	maxAge := bazelDiskCacheAge(cfg)
	meta := map[string]string{"disk_cache_max_age": cfg.DiskCacheMaxAge, "disk_cache_age_delete": fmt.Sprint(cfg.DiskCacheAgeDelete)}
	if maxAge == 0 {
		meta["disk_cache_age_rule"] = "off"
		return nil, meta
	}
	cutoff := now.Add(-maxAge)
	var targets []CleanupTarget
	var totalExpired int64
	for _, cache := range bazelDiskCachePaths(cfg, candidates, home) {
		scan, err := walkBazelDiskCache(ctx, cache, cutoff, now, cfg.DiskCacheMaxEntries, nil)
		t := CleanupTarget{
			Type: bazelDiskCacheAgeType, Name: filepath.Base(cache), Path: cache,
			Bytes: scan.expiredBytes,
		}
		summary := fmt.Sprintf("%d of %d entries (%s of %s) last used more than %s ago",
			scan.expired, scan.entries, bazelHumanBytes(scan.expiredBytes), bazelHumanBytes(scan.bytes), formatDevArtifactAge(maxAge))
		if scan.truncated {
			summary += fmt.Sprintf("; walk stopped at disk_cache_max_entries %d", cfg.DiskCacheMaxEntries)
		}
		if scan.foreign > 0 {
			summary += fmt.Sprintf("; %d entries of other users skipped", scan.foreign)
		}
		switch {
		case err != nil:
			t.Action, t.Protected, t.Reason = "keep", true, "disk cache walk failed: "+err.Error()
		case activeErr != nil:
			t.Action, t.Protected, t.Reason = "keep", true, "Bazel process inspection failed; age retention waits: "+summary
		case globalActive:
			t.Action, t.Protected, t.Active, t.Reason = "keep", true, true, "active Bazel client commands are visible; age retention waits: "+summary
		case scan.expired == 0:
			t.Action, t.Protected, t.Reason = "keep", true, summary
		case cfg.DiskCacheAgeDelete:
			t.Action, t.Reason = bazelDiskCacheAgeDelete, summary
			totalExpired += scan.expiredBytes
		default:
			t.Action, t.Reason = bazelDiskCacheAgeReport, "would remove (report only; set bazel.disk_cache_age_delete): "+summary
			totalExpired += scan.expiredBytes
		}
		annotateCleanupTargetPolicy(&t, CleanupTierWarm, hostReclaimForAction(t.Action))
		targets = append(targets, t)
	}
	meta["disk_cache_age_expired_bytes"] = fmt.Sprint(totalExpired)
	meta["disk_cache_age_cache_count"] = fmt.Sprint(len(targets))
	return targets, meta
}

// applyBazelDiskCacheAge removes expired entries of every disk cache whose
// target is marked for deletion, re-checking each entry's last use first.
func (p *BazelPlugin) applyBazelDiskCacheAge(ctx context.Context, cfg config.BazelConfig, targets []CleanupTarget, logger *slog.Logger, result *CleanupResult) {
	maxAge := bazelDiskCacheAge(cfg)
	if maxAge == 0 || !cfg.DiskCacheAgeDelete {
		return
	}
	var due []CleanupTarget
	for _, t := range targets {
		if t.Type == bazelDiskCacheAgeType && t.Action == bazelDiskCacheAgeDelete && !t.Protected {
			due = append(due, t)
		}
	}
	if len(due) == 0 {
		return
	}
	// A client may have started since planning.
	probe := p.activeBazelProcessInfo
	if p.activeProbe != nil {
		probe = p.activeProbe
	}
	if info, err := probe(ctx); err != nil || len(info.Reasons) > 0 {
		logger.Info("Bazel disk cache age retention waits for idle Bazel clients", "error", err, "active", info.Reasons)
		return
	}
	for _, t := range due {
		now := time.Now()
		cutoff := now.Add(-maxAge)
		var freed int64
		var removed int
		_, err := walkBazelDiskCache(ctx, t.Path, cutoff, now, cfg.DiskCacheMaxEntries, func(path string, info os.FileInfo, allocated int64) error {
			current, err := os.Lstat(path)
			if err != nil || !os.SameFile(info, current) || !bazelDiskCacheLastUse(current, time.Now()).Before(cutoff) {
				return nil
			}
			if err := os.Remove(path); err != nil {
				return nil
			}
			freed += allocated
			removed++
			return nil
		})
		if err != nil {
			logger.Warn("Bazel disk cache age retention stopped", "path", t.Path, "error", err)
		}
		logger.Info("Bazel disk cache age retention removed expired entries", "path", t.Path, "entries", removed, "bytes", freed, "max_age", cfg.DiskCacheMaxAge)
		result.BytesFreed += freed
		result.EstimatedBytesFreed += freed
		result.ItemsCleaned += removed
	}
}

func bazelHumanBytes(b int64) string {
	return fmt.Sprintf("%.1f GiB", float64(b)/float64(1<<30))
}
