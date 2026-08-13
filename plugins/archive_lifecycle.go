// Package plugins provides cleanup plugin implementations.
// archive_lifecycle.go retires archive staging pre-images whose contents are
// provably already present in a durable archive target.
package plugins

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

const (
	archiveLifecycleDefaultRetireAfter    = 14 * 24 * time.Hour
	archiveLifecycleDefaultGroupDepth     = 3
	archiveLifecycleDefaultMaxGroupsCycle = 32
)

// ArchiveLifecyclePlugin retires verified archive staging pre-images.
//
// An archival workflow that compresses a tree into durable storage commonly
// leaves the uncompressed pre-image behind "just in case", and nothing ever
// comes back to remove it. On the neo SSD estate that class is 145G of codex
// session pre-images shadowing a 14G zstd archive that already contains every
// byte of it.
//
// This plugin only ever deletes staging content it has independently proved
// redundant, one group at a time, and it refuses on any ambiguity.
type ArchiveLifecyclePlugin struct{}

// NewArchiveLifecyclePlugin creates a new archive staging lifecycle plugin.
func NewArchiveLifecyclePlugin() *ArchiveLifecyclePlugin {
	return &ArchiveLifecyclePlugin{}
}

// Name returns the plugin identifier.
func (p *ArchiveLifecyclePlugin) Name() string {
	return "archive-lifecycle"
}

// Description returns the plugin description.
func (p *ArchiveLifecyclePlugin) Description() string {
	return "Retires archive staging pre-images after proving every file is already present in the durable archive target"
}

// SupportedPlatforms returns supported platforms (all).
func (p *ArchiveLifecyclePlugin) SupportedPlatforms() []string {
	return nil
}

// Enabled checks if archive staging lifecycle handling is enabled.
func (p *ArchiveLifecyclePlugin) Enabled(cfg *config.Config) bool {
	return cfg.Enable.ArchiveLifecycle
}

// archiveGroupVerdict is the reconciliation result for one staging group.
type archiveGroupVerdict struct {
	Source        string
	StagingGroup  string
	TargetGroup   string
	RelPath       string
	Bytes         int64
	FileCount     int
	VerifiedCount int
	NewestModTime time.Time
	Verified      bool
	Reason        string
}

// PlanCleanup returns a dry-run plan without mutating staging trees.
func (p *ArchiveLifecyclePlugin) PlanCleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupPlan {
	plan, _ := p.buildCleanupPlan(ctx, level, cfg, logger)
	return plan
}

func (p *ArchiveLifecyclePlugin) buildCleanupPlan(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) (CleanupPlan, []archiveGroupVerdict) {
	alCfg := cfg.ArchiveLifecycle
	plan := CleanupPlan{
		Plugin:   p.Name(),
		Level:    level.String(),
		Summary:  "Archive staging pre-image retirement plan",
		WouldRun: true,
		Steps: []string{
			"Enumerate configured staging groups at the configured group depth",
			"Match every staging file to a target counterpart, allowing a .gz or .zst compression suffix",
			"Prove each counterpart declares exactly the staging file's uncompressed size",
			"Retire only groups where every file verified, the file count matches, and the group is older than the retirement threshold",
			"Re-verify each group immediately before deleting it",
		},
		Metadata: map[string]string{
			"cleanup_level":        level.String(),
			"dry_run":              strconv.FormatBool(alCfg.DryRun),
			"retire_after":         alCfg.RetireAfter,
			"source_count":         strconv.Itoa(len(alCfg.Sources)),
			"max_groups_per_cycle": strconv.Itoa(archiveLifecycleMaxGroups(alCfg)),
		},
	}

	if len(alCfg.Sources) == 0 {
		plan.WouldRun = false
		plan.SkipReason = "no archive_lifecycle sources are configured"
		return plan, nil
	}
	if level < LevelModerate {
		plan.WouldRun = false
		plan.SkipReason = "archive staging retirement is report-only below moderate level"
	}
	if alCfg.DryRun {
		plan.Warnings = append(plan.Warnings, "archive_lifecycle.dry_run is enabled; verified groups are reported but never deleted")
	}

	home, _ := os.UserHomeDir()
	verdicts, warnings := p.reconcileSources(ctx, home, alCfg, logger)
	plan.Warnings = append(plan.Warnings, warnings...)

	var verifiedCount int
	var verifiedBytes int64
	targets := make([]CleanupTarget, 0, len(verdicts))
	for _, verdict := range verdicts {
		target := archiveLifecycleTarget(verdict, level, alCfg)
		if target.Action == "retire_archive_staging_group" {
			verifiedCount++
			verifiedBytes += verdict.Bytes
		}
		targets = append(targets, target)
	}

	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Bytes == targets[j].Bytes {
			return targets[i].Path < targets[j].Path
		}
		return targets[i].Bytes > targets[j].Bytes
	})

	plan.Targets = targets
	plan.EstimatedBytesFreed = verifiedBytes
	plan.Metadata["retirable_group_count"] = strconv.Itoa(verifiedCount)
	annotateCleanupPlanTargetAccounting(&plan)
	return plan, verdicts
}

// Cleanup retires verified staging groups.
func (p *ArchiveLifecyclePlugin) Cleanup(ctx context.Context, level CleanupLevel, cfg *config.Config, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: level}
	alCfg := cfg.ArchiveLifecycle

	if level < LevelModerate {
		logger.Info("archive staging retirement is report-only below moderate level", "level", level.String())
		return result
	}

	plan, _ := p.buildCleanupPlan(ctx, level, cfg, logger)
	home, _ := os.UserHomeDir()

	for _, target := range plan.Targets {
		if target.Action != "retire_archive_staging_group" || target.Protected {
			continue
		}
		if err := ctx.Err(); err != nil {
			result.Error = err
			return result
		}
		if alCfg.DryRun {
			logger.Info("archive_lifecycle.dry_run is enabled; not retiring verified staging group",
				"path", target.Path, "estimated_bytes", target.Bytes, "reason", target.Reason)
			continue
		}

		source, ok := archiveSourceForStagingGroup(home, alCfg, target.Path)
		if !ok {
			logger.Warn("refusing to retire staging group outside any configured source", "path", target.Path)
			continue
		}
		// The plan can be minutes old on a 145G tree. Prove redundancy again
		// against live filesystem state immediately before deleting.
		verdict := verifyArchiveStagingGroup(ctx, source, target.Path)
		if !verdict.Verified {
			logger.Warn("skipping staging group that no longer verifies as redundant",
				"path", target.Path, "reason", verdict.Reason)
			continue
		}

		freed, err := retireArchiveStagingGroup(ctx, target.Path, expandHome(source.StagingDir, home))
		if err != nil {
			logger.Warn("failed to retire verified staging group", "path", target.Path, "error", err)
			continue
		}

		result.BytesFreed += freed
		result.EstimatedBytesFreed += freed
		result.ItemsCleaned++
		logger.Info("retired verified archive staging group",
			"source", source.Name,
			"path", target.Path,
			"target_group", verdict.TargetGroup,
			"files_verified", verdict.VerifiedCount,
			"bytes_freed", freed)
	}

	if result.ItemsCleaned == 0 {
		logger.Info("archive staging retirement found no verified redundant groups", "level", level.String())
	}
	return result
}

func (p *ArchiveLifecyclePlugin) reconcileSources(ctx context.Context, home string, alCfg config.ArchiveLifecycleConfig, logger *slog.Logger) ([]archiveGroupVerdict, []string) {
	var verdicts []archiveGroupVerdict
	var warnings []string
	budget := archiveLifecycleMaxGroups(alCfg)

	for _, source := range alCfg.Sources {
		if err := ctx.Err(); err != nil {
			warnings = append(warnings, fmt.Sprintf("archive staging reconciliation stopped early: %v", err))
			return verdicts, warnings
		}
		staging := expandHome(source.StagingDir, home)
		target := expandHome(source.TargetDir, home)
		name := source.Name
		if name == "" {
			name = filepath.Base(staging)
		}

		if staging == "" || target == "" {
			warnings = append(warnings, fmt.Sprintf("archive source %q is missing staging_dir or target_dir", name))
			continue
		}
		if !pathExistsAndIsDir(staging) {
			logger.Debug("archive staging dir not present, skipping", "source", name, "path", staging)
			continue
		}
		if !pathExistsAndIsDir(target) {
			// An absent target is the single most dangerous misconfiguration
			// here: it would make every staging group look unverified, which is
			// safe, but it usually means the archive volume is not mounted.
			warnings = append(warnings, fmt.Sprintf("archive source %q target dir %s is not present; nothing will be retired", name, target))
			continue
		}

		groups, groupWarnings := archiveStagingGroups(ctx, staging, archiveSourceGroupDepth(source))
		warnings = append(warnings, prefixArchiveWarnings(name, groupWarnings)...)

		for _, group := range groups {
			if budget <= 0 {
				warnings = append(warnings, "archive staging group budget exhausted; remaining groups deferred to a later cycle")
				return verdicts, warnings
			}
			if err := ctx.Err(); err != nil {
				warnings = append(warnings, fmt.Sprintf("archive staging reconciliation stopped early: %v", err))
				return verdicts, warnings
			}
			budget--
			resolved := source
			resolved.Name = name
			resolved.StagingDir = staging
			resolved.TargetDir = target
			verdicts = append(verdicts, verifyArchiveStagingGroup(ctx, resolved, group))
		}
	}

	return verdicts, warnings
}

// archiveStagingGroups returns directories exactly depth levels below root.
// A depth of zero treats the root itself as the single group.
func archiveStagingGroups(ctx context.Context, root string, depth int) ([]string, []string) {
	if depth <= 0 {
		return []string{filepath.Clean(root)}, nil
	}

	var warnings []string
	current := []string{filepath.Clean(root)}
	for level := 0; level < depth; level++ {
		var next []string
		for _, dir := range current {
			if err := ctx.Err(); err != nil {
				return next, warnings
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("could not read staging dir %s: %v", dir, err))
				continue
			}
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				next = append(next, filepath.Join(dir, entry.Name()))
			}
		}
		current = next
	}
	sort.Strings(current)
	return current, warnings
}

// verifyArchiveStagingGroup proves, or fails to prove, that every regular file
// in a staging group is already present in the archive target at the same
// relative path with the same uncompressed size.
//
// It is fail-closed at every step. A group is verified only when it contains at
// least one file, every file matched a counterpart, and no entry was skipped
// for any reason.
func verifyArchiveStagingGroup(ctx context.Context, source config.ArchiveSourceConfig, group string) archiveGroupVerdict {
	verdict := archiveGroupVerdict{
		Source:       source.Name,
		StagingGroup: filepath.Clean(group),
	}

	rel, err := filepath.Rel(source.StagingDir, verdict.StagingGroup)
	if err != nil || rel == ".." || len(rel) > 2 && rel[:3] == ".."+string(os.PathSeparator) {
		verdict.Reason = "staging group is not inside the configured staging dir"
		return verdict
	}
	verdict.RelPath = rel
	verdict.TargetGroup = filepath.Join(source.TargetDir, rel)

	if !pathExistsAndIsDir(verdict.TargetGroup) {
		verdict.Reason = fmt.Sprintf("archive target group %s does not exist", verdict.TargetGroup)
		return verdict
	}

	// The walk continues past a per-file failure so the manifest reports the
	// group's true size and file count. Only cancellation stops it early. A
	// recorded failure still fails the whole group.
	var firstFailure string
	recordFailure := func(reason string) {
		if firstFailure == "" {
			firstFailure = reason
		}
	}

	walkErr := filepath.Walk(verdict.StagingGroup, func(path string, info os.FileInfo, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			recordFailure(fmt.Sprintf("could not read %s: %v", path, err))
			return nil
		}
		if info.IsDir() {
			return nil
		}

		verdict.FileCount++
		verdict.Bytes += info.Size()
		if info.ModTime().After(verdict.NewestModTime) {
			verdict.NewestModTime = info.ModTime()
		}

		if !info.Mode().IsRegular() {
			// Symlinks, sockets, and devices have no meaningful archive
			// counterpart; refuse the whole group rather than guess.
			recordFailure(fmt.Sprintf("staging entry %s is not a regular file", path))
			return nil
		}

		fileRel, err := filepath.Rel(verdict.StagingGroup, path)
		if err != nil {
			recordFailure(fmt.Sprintf("could not relativize %s: %v", path, err))
			return nil
		}
		counterpart := filepath.Join(verdict.TargetGroup, fileRel)
		if err := archiveCounterpartProvesSize(counterpart, info.Size()); err != nil {
			recordFailure(fmt.Sprintf("%s: %v", fileRel, err))
			return nil
		}
		verdict.VerifiedCount++
		return nil
	})

	if walkErr != nil {
		verdict.Reason = walkErr.Error()
		return verdict
	}
	if verdict.FileCount == 0 {
		verdict.Reason = "staging group holds no regular files"
		return verdict
	}
	if firstFailure != "" || verdict.VerifiedCount != verdict.FileCount {
		verdict.Reason = fmt.Sprintf("verified %d of %d staging files; first blocker: %s",
			verdict.VerifiedCount, verdict.FileCount, firstFailure)
		return verdict
	}

	verdict.Verified = true
	verdict.Reason = fmt.Sprintf("all %d staging files are present in %s at the same uncompressed size", verdict.FileCount, verdict.TargetGroup)
	return verdict
}

// archiveCounterpartProvesSize checks the target counterpart of one staging
// file. An exact-name counterpart must have the same byte size. A .gz or .zst
// counterpart must *declare* the staging file's uncompressed size in its own
// header or trailer, which is an O(1) read and does not require decompressing
// the archive.
func archiveCounterpartProvesSize(counterpart string, stagingSize int64) error {
	if info, err := os.Lstat(counterpart); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("archive counterpart %s is not a regular file", counterpart)
		}
		if info.Size() != stagingSize {
			return fmt.Errorf("archive counterpart %s is %d bytes, staging file is %d bytes", counterpart, info.Size(), stagingSize)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("could not stat archive counterpart %s: %w", counterpart, err)
	}

	for _, suffix := range []string{".gz", ".zst"} {
		compressed := counterpart + suffix
		info, err := os.Lstat(compressed)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("could not stat archive counterpart %s: %w", compressed, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("archive counterpart %s is not a regular file", compressed)
		}

		var declared int64
		switch suffix {
		case ".gz":
			declared, err = gzipDeclaredUncompressedSize(compressed, stagingSize)
		case ".zst":
			declared, err = zstdDeclaredUncompressedSize(compressed)
		}
		if err != nil {
			return fmt.Errorf("could not read declared size of %s: %w", compressed, err)
		}
		if declared != stagingSize {
			return fmt.Errorf("archive counterpart %s declares %d uncompressed bytes, staging file is %d bytes", compressed, declared, stagingSize)
		}
		return nil
	}

	return fmt.Errorf("no archive counterpart for %s", filepath.Base(counterpart))
}

// gzipDeclaredUncompressedSize reads the ISIZE trailer of a gzip member.
//
// ISIZE is the uncompressed size modulo 2^32, so it cannot prove anything for a
// staging file of 4 GiB or more. That case is refused rather than approximated.
func gzipDeclaredUncompressedSize(path string, stagingSize int64) (int64, error) {
	if stagingSize >= 1<<32 {
		return 0, errors.New("gzip ISIZE is modulo 2^32 and cannot prove a staging file of 4 GiB or more")
	}

	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() < 18 {
		return 0, errors.New("file is too small to be a gzip member")
	}

	header := make([]byte, 2)
	if _, err := io.ReadFull(file, header); err != nil {
		return 0, err
	}
	if header[0] != 0x1f || header[1] != 0x8b {
		return 0, errors.New("file does not carry the gzip magic bytes")
	}

	trailer := make([]byte, 4)
	if _, err := file.ReadAt(trailer, info.Size()-4); err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint32(trailer)), nil
}

// zstdDeclaredUncompressedSize reads Frame_Content_Size from the first zstd
// frame header. Encoders that stream without a known input size omit the field;
// that case is refused rather than approximated.
func zstdDeclaredUncompressedSize(path string) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	header := make([]byte, 18)
	read, err := io.ReadFull(file, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, err
	}
	header = header[:read]
	if len(header) < 5 {
		return 0, errors.New("file is too small to be a zstd frame")
	}
	if binary.LittleEndian.Uint32(header[:4]) != 0xFD2FB528 {
		return 0, errors.New("file does not carry the zstd frame magic bytes")
	}

	descriptor := header[4]
	fcsFlag := descriptor >> 6
	singleSegment := descriptor&0x20 != 0
	dictIDFlag := descriptor & 0x03

	fcsSize := 0
	switch fcsFlag {
	case 0:
		if singleSegment {
			fcsSize = 1
		}
	case 1:
		fcsSize = 2
	case 2:
		fcsSize = 4
	case 3:
		fcsSize = 8
	}
	if fcsSize == 0 {
		return 0, errors.New("zstd frame does not declare Frame_Content_Size")
	}

	offset := 5
	if !singleSegment {
		offset++ // Window_Descriptor
	}
	switch dictIDFlag {
	case 1:
		offset++
	case 2:
		offset += 2
	case 3:
		offset += 4
	}
	if len(header) < offset+fcsSize {
		return 0, errors.New("zstd frame header is truncated")
	}

	field := header[offset : offset+fcsSize]
	switch fcsSize {
	case 1:
		return int64(field[0]), nil
	case 2:
		// The 2-byte form stores value - 256.
		return int64(binary.LittleEndian.Uint16(field)) + 256, nil
	case 4:
		return int64(binary.LittleEndian.Uint32(field)), nil
	default:
		value := binary.LittleEndian.Uint64(field)
		if value > 1<<62 {
			return 0, errors.New("zstd frame declares an implausible content size")
		}
		return int64(value), nil
	}
}

func archiveLifecycleTarget(verdict archiveGroupVerdict, level CleanupLevel, alCfg config.ArchiveLifecycleConfig) CleanupTarget {
	target := CleanupTarget{
		Type:  "archive-staging-group",
		Name:  archiveGroupDisplayName(verdict),
		Path:  verdict.StagingGroup,
		Bytes: verdict.Bytes,
	}

	retireAfter := archiveSourceRetireAfter(verdict, alCfg)
	aged := !verdict.NewestModTime.IsZero() && !verdict.NewestModTime.After(time.Now().Add(-retireAfter))

	switch {
	case !verdict.Verified:
		target.Action = "review"
		target.Protected = true
		target.Reason = "staging group is not provably redundant: " + verdict.Reason
	case level < LevelModerate:
		target.Action = "report"
		target.Protected = true
		target.Reason = "warning level reports verified staging groups without retiring them"
	case !aged:
		target.Action = "keep"
		target.Protected = true
		target.Reason = fmt.Sprintf("staging group is verified but newer than the %s retirement threshold", retireAfter)
	default:
		target.Action = "retire_archive_staging_group"
		target.Reason = verdict.Reason
	}

	annotateCleanupTargetPolicy(&target, CleanupTierDestructive, archiveLifecycleReclaim(target.Action))
	return target
}

func archiveLifecycleReclaim(action string) string {
	if action == "retire_archive_staging_group" {
		return CleanupReclaimHost
	}
	return CleanupReclaimNone
}

func archiveGroupDisplayName(verdict archiveGroupVerdict) string {
	if verdict.Source == "" {
		return verdict.RelPath
	}
	if verdict.RelPath == "" || verdict.RelPath == "." {
		return verdict.Source
	}
	return verdict.Source + "/" + verdict.RelPath
}

func archiveSourceRetireAfter(verdict archiveGroupVerdict, alCfg config.ArchiveLifecycleConfig) time.Duration {
	for _, source := range alCfg.Sources {
		if source.Name != verdict.Source || source.RetireAfter == "" {
			continue
		}
		return parseNixPolicyDuration(source.RetireAfter, archiveLifecycleDefaultRetireAfter)
	}
	return parseNixPolicyDuration(alCfg.RetireAfter, archiveLifecycleDefaultRetireAfter)
}

func archiveSourceGroupDepth(source config.ArchiveSourceConfig) int {
	if source.GroupDepth > 0 {
		return source.GroupDepth
	}
	return archiveLifecycleDefaultGroupDepth
}

func archiveLifecycleMaxGroups(alCfg config.ArchiveLifecycleConfig) int {
	if alCfg.MaxGroupsPerCycle > 0 {
		return alCfg.MaxGroupsPerCycle
	}
	return archiveLifecycleDefaultMaxGroupsCycle
}

// archiveSourceForStagingGroup finds the configured source that owns a planned
// staging group path, with staging and target dirs already expanded.
func archiveSourceForStagingGroup(home string, alCfg config.ArchiveLifecycleConfig, group string) (config.ArchiveSourceConfig, bool) {
	clean := filepath.Clean(group)
	for _, source := range alCfg.Sources {
		staging := expandHome(source.StagingDir, home)
		if staging == "" || !pathInsideRoot(clean, staging) {
			continue
		}
		resolved := source
		resolved.StagingDir = staging
		resolved.TargetDir = expandHome(source.TargetDir, home)
		if resolved.Name == "" {
			resolved.Name = filepath.Base(staging)
		}
		return resolved, true
	}
	return config.ArchiveSourceConfig{}, false
}

// retireArchiveStagingGroup removes a verified staging group without crossing a
// filesystem boundary, then prunes directories it emptied up to (but never
// including) the staging root.
func retireArchiveStagingGroup(ctx context.Context, group string, stagingRoot string) (int64, error) {
	group = filepath.Clean(group)
	stagingRoot = filepath.Clean(stagingRoot)
	if group == stagingRoot {
		return 0, errors.New("refusing to retire the staging root itself")
	}
	if !pathInsideRoot(group, stagingRoot) {
		return 0, fmt.Errorf("refusing to retire %s outside staging root %s", group, stagingRoot)
	}

	rootDevice, err := deviceID(group)
	if err != nil {
		return 0, fmt.Errorf("could not probe device for %s: %w", group, err)
	}

	bytes, err := getDirAllocatedBytesContext(ctx, group)
	if err != nil {
		return 0, err
	}
	if err := removeArchiveStagingTree(ctx, group, rootDevice); err != nil {
		return 0, err
	}

	pruneEmptyArchiveParents(filepath.Dir(group), stagingRoot)
	return bytes, nil
}

// removeArchiveStagingTree removes a directory tree without following symlinks
// and without descending into an entry whose device differs from the group
// root. A mount point nested inside a staging group is refused, not deleted.
func removeArchiveStagingTree(ctx context.Context, path string, rootDevice uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return os.Remove(path)
	}
	if device, err := deviceID(path); err != nil {
		return fmt.Errorf("could not probe device for %s: %w", path, err)
	} else if device != rootDevice {
		return fmt.Errorf("refusing to cross a filesystem boundary at %s", path)
	}
	if !info.IsDir() {
		return os.Remove(path)
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := removeArchiveStagingTree(ctx, filepath.Join(path, entry.Name()), rootDevice); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

func pruneEmptyArchiveParents(dir string, stagingRoot string) {
	for dir != stagingRoot && pathInsideRoot(dir, stagingRoot) {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

func prefixArchiveWarnings(source string, warnings []string) []string {
	if len(warnings) == 0 {
		return nil
	}
	prefixed := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		prefixed = append(prefixed, fmt.Sprintf("archive source %q: %s", source, warning))
	}
	return prefixed
}
