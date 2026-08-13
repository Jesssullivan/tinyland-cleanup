package plugins

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

func writeArchiveFile(t *testing.T, path string, payload []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0644); err != nil {
		t.Fatal(err)
	}
}

func writeGzipCounterpart(t *testing.T, path string, payload []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
}

// writeZstdCounterpart writes a minimal but spec-shaped zstd frame. Only the
// frame header is meaningful for verification, which is exactly the point:
// nothing decompresses the archive.
func writeZstdCounterpart(t *testing.T, path string, declaredSize uint32, singleSegment bool, includeFCS bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}

	var frame bytes.Buffer
	magic := make([]byte, 4)
	binary.LittleEndian.PutUint32(magic, 0xFD2FB528)
	frame.Write(magic)

	var descriptor byte
	if includeFCS {
		descriptor |= 0x80 // FCS_Field_Size flag 2 -> 4-byte field
	}
	if singleSegment {
		descriptor |= 0x20
	}
	frame.WriteByte(descriptor)
	if !singleSegment {
		frame.WriteByte(0x58) // Window_Descriptor
	}
	if includeFCS {
		fcs := make([]byte, 4)
		binary.LittleEndian.PutUint32(fcs, declaredSize)
		frame.Write(fcs)
	}
	frame.Write(make([]byte, 8)) // opaque block bytes

	if err := os.WriteFile(path, frame.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
}

func ageArchivePath(t *testing.T, path string, age time.Duration) {
	t.Helper()
	when := time.Now().Add(-age)
	if err := filepath.Walk(path, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(p, when, when)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGzipDeclaredUncompressedSize(t *testing.T) {
	dir := t.TempDir()
	payload := bytes.Repeat([]byte("session line\n"), 512)
	path := filepath.Join(dir, "rollout.jsonl.gz")
	writeGzipCounterpart(t, path, payload)

	size, err := gzipDeclaredUncompressedSize(path, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(payload)) {
		t.Fatalf("declared size = %d, want %d", size, len(payload))
	}

	if _, err := gzipDeclaredUncompressedSize(path, 1<<32); err == nil {
		t.Fatal("gzip ISIZE must refuse to prove a staging file of 4 GiB or more")
	}

	notGzip := filepath.Join(dir, "plain.bin")
	writeArchiveFile(t, notGzip, bytes.Repeat([]byte("x"), 64))
	if _, err := gzipDeclaredUncompressedSize(notGzip, 64); err == nil {
		t.Fatal("expected non-gzip payload to be rejected")
	}
}

func TestZstdDeclaredUncompressedSize(t *testing.T) {
	dir := t.TempDir()

	withFCS := filepath.Join(dir, "rollout.jsonl.zst")
	writeZstdCounterpart(t, withFCS, 254875631, false, true)
	size, err := zstdDeclaredUncompressedSize(withFCS)
	if err != nil {
		t.Fatal(err)
	}
	if size != 254875631 {
		t.Fatalf("declared size = %d, want 254875631", size)
	}

	withoutFCS := filepath.Join(dir, "streamed.jsonl.zst")
	writeZstdCounterpart(t, withoutFCS, 0, false, false)
	if _, err := zstdDeclaredUncompressedSize(withoutFCS); err == nil {
		t.Fatal("a frame with no Frame_Content_Size must be refused, not guessed")
	}

	notZstd := filepath.Join(dir, "plain.bin")
	writeArchiveFile(t, notZstd, bytes.Repeat([]byte("x"), 64))
	if _, err := zstdDeclaredUncompressedSize(notZstd); err == nil {
		t.Fatal("expected non-zstd payload to be rejected")
	}
}

func TestArchiveCounterpartProvesSize(t *testing.T) {
	dir := t.TempDir()
	payload := bytes.Repeat([]byte("a"), 4096)

	exact := filepath.Join(dir, "exact.jsonl")
	writeArchiveFile(t, exact, payload)
	if err := archiveCounterpartProvesSize(exact, int64(len(payload))); err != nil {
		t.Fatalf("exact counterpart should verify: %v", err)
	}
	if err := archiveCounterpartProvesSize(exact, int64(len(payload))+1); err == nil {
		t.Fatal("size mismatch must fail")
	}

	gz := filepath.Join(dir, "compressed.jsonl")
	writeGzipCounterpart(t, gz+".gz", payload)
	if err := archiveCounterpartProvesSize(gz, int64(len(payload))); err != nil {
		t.Fatalf("gzip counterpart should verify: %v", err)
	}
	if err := archiveCounterpartProvesSize(gz, int64(len(payload))-1); err == nil {
		t.Fatal("gzip size mismatch must fail")
	}

	zst := filepath.Join(dir, "zstd.jsonl")
	writeZstdCounterpart(t, zst+".zst", uint32(len(payload)), false, true)
	if err := archiveCounterpartProvesSize(zst, int64(len(payload))); err != nil {
		t.Fatalf("zstd counterpart should verify: %v", err)
	}

	missing := filepath.Join(dir, "missing.jsonl")
	if err := archiveCounterpartProvesSize(missing, 10); err == nil {
		t.Fatal("a missing counterpart must fail")
	}
}

// buildArchivePair lays out a staging/target pair shaped like the real codex
// session archive: staging/<YYYY>/<MM>/<DD>/*.jsonl mirrored by
// target/<YYYY>/<MM>/<DD>/*.jsonl.zst.
func buildArchivePair(t *testing.T) (string, string, []byte) {
	t.Helper()
	root := t.TempDir()
	staging := filepath.Join(root, "staging")
	target := filepath.Join(root, "target")
	payload := bytes.Repeat([]byte("rollout\n"), 1024)

	for _, day := range []string{"27", "28"} {
		writeArchiveFile(t, filepath.Join(staging, "2026", "07", day, "rollout-a.jsonl"), payload)
		writeArchiveFile(t, filepath.Join(staging, "2026", "07", day, "rollout-b.jsonl"), payload)
		writeZstdCounterpart(t, filepath.Join(target, "2026", "07", day, "rollout-a.jsonl.zst"), uint32(len(payload)), false, true)
		writeZstdCounterpart(t, filepath.Join(target, "2026", "07", day, "rollout-b.jsonl.zst"), uint32(len(payload)), false, true)
	}
	return staging, target, payload
}

func TestVerifyArchiveStagingGroupVerifiesFullDay(t *testing.T) {
	staging, target, _ := buildArchivePair(t)
	source := config.ArchiveSourceConfig{Name: "codex", StagingDir: staging, TargetDir: target}

	verdict := verifyArchiveStagingGroup(context.Background(), source, filepath.Join(staging, "2026", "07", "27"))
	if !verdict.Verified {
		t.Fatalf("expected verified group, got %q", verdict.Reason)
	}
	if verdict.FileCount != 2 || verdict.VerifiedCount != 2 {
		t.Fatalf("counts = %d/%d, want 2/2", verdict.VerifiedCount, verdict.FileCount)
	}
	if verdict.RelPath != filepath.Join("2026", "07", "27") {
		t.Fatalf("rel path = %q", verdict.RelPath)
	}
}

func TestVerifyArchiveStagingGroupFailsClosed(t *testing.T) {
	t.Run("one missing counterpart fails the whole group", func(t *testing.T) {
		staging, target, _ := buildArchivePair(t)
		if err := os.Remove(filepath.Join(target, "2026", "07", "27", "rollout-b.jsonl.zst")); err != nil {
			t.Fatal(err)
		}
		source := config.ArchiveSourceConfig{Name: "codex", StagingDir: staging, TargetDir: target}
		verdict := verifyArchiveStagingGroup(context.Background(), source, filepath.Join(staging, "2026", "07", "27"))
		if verdict.Verified {
			t.Fatal("a group with one unmatched file must not verify")
		}
	})

	t.Run("size mismatch fails the whole group", func(t *testing.T) {
		staging, target, payload := buildArchivePair(t)
		writeZstdCounterpart(t, filepath.Join(target, "2026", "07", "27", "rollout-b.jsonl.zst"), uint32(len(payload))-1, false, true)
		source := config.ArchiveSourceConfig{Name: "codex", StagingDir: staging, TargetDir: target}
		verdict := verifyArchiveStagingGroup(context.Background(), source, filepath.Join(staging, "2026", "07", "27"))
		if verdict.Verified {
			t.Fatal("a group with a size mismatch must not verify")
		}
	})

	t.Run("missing target group fails", func(t *testing.T) {
		staging, target, _ := buildArchivePair(t)
		if err := os.RemoveAll(filepath.Join(target, "2026", "07", "27")); err != nil {
			t.Fatal(err)
		}
		source := config.ArchiveSourceConfig{Name: "codex", StagingDir: staging, TargetDir: target}
		verdict := verifyArchiveStagingGroup(context.Background(), source, filepath.Join(staging, "2026", "07", "27"))
		if verdict.Verified {
			t.Fatal("a group with no target counterpart dir must not verify")
		}
	})

	t.Run("empty group fails", func(t *testing.T) {
		staging, target, _ := buildArchivePair(t)
		empty := filepath.Join(staging, "2026", "07", "29")
		if err := os.MkdirAll(empty, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(target, "2026", "07", "29"), 0755); err != nil {
			t.Fatal(err)
		}
		source := config.ArchiveSourceConfig{Name: "codex", StagingDir: staging, TargetDir: target}
		verdict := verifyArchiveStagingGroup(context.Background(), source, empty)
		if verdict.Verified {
			t.Fatal("an empty staging group must not verify")
		}
	})

	t.Run("non-regular staging entry fails", func(t *testing.T) {
		staging, target, _ := buildArchivePair(t)
		link := filepath.Join(staging, "2026", "07", "27", "rollout-c.jsonl")
		if err := os.Symlink(filepath.Join(staging, "2026", "07", "28", "rollout-a.jsonl"), link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		source := config.ArchiveSourceConfig{Name: "codex", StagingDir: staging, TargetDir: target}
		verdict := verifyArchiveStagingGroup(context.Background(), source, filepath.Join(staging, "2026", "07", "27"))
		if verdict.Verified {
			t.Fatal("a symlink in the staging group must not verify")
		}
	})
}

func archiveTestConfig(staging, target string, dryRun bool) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Enable.ArchiveLifecycle = true
	cfg.ArchiveLifecycle = config.ArchiveLifecycleConfig{
		DryRun:            dryRun,
		RetireAfter:       "14d",
		MaxGroupsPerCycle: 32,
		Sources: []config.ArchiveSourceConfig{
			{Name: "codex", StagingDir: staging, TargetDir: target, GroupDepth: 3},
		},
	}
	return cfg
}

func TestArchiveLifecyclePlanClassifiesGroups(t *testing.T) {
	staging, target, _ := buildArchivePair(t)
	ageArchivePath(t, filepath.Join(staging, "2026", "07", "27"), 60*24*time.Hour)
	// Day 28 is verified but fresh, so it must be kept, not retired.

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	plan := NewArchiveLifecyclePlugin().PlanCleanup(context.Background(), LevelModerate, archiveTestConfig(staging, target, true), logger)

	actions := map[string]string{}
	for _, tgt := range plan.Targets {
		actions[filepath.Base(tgt.Path)] = tgt.Action
	}
	if actions["27"] != "retire_archive_staging_group" {
		t.Fatalf("day 27 action = %q, want retire_archive_staging_group", actions["27"])
	}
	if actions["28"] != "keep" {
		t.Fatalf("day 28 action = %q, want keep", actions["28"])
	}
	if plan.Metadata["retirable_group_count"] != "1" {
		t.Fatalf("retirable_group_count = %q, want 1", plan.Metadata["retirable_group_count"])
	}
	if plan.Metadata["dry_run"] != "true" {
		t.Fatalf("dry_run metadata = %q, want true", plan.Metadata["dry_run"])
	}
}

func TestArchiveLifecycleDryRunDeletesNothing(t *testing.T) {
	staging, target, _ := buildArchivePair(t)
	ageArchivePath(t, staging, 60*24*time.Hour)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	result := NewArchiveLifecyclePlugin().Cleanup(context.Background(), LevelCritical, archiveTestConfig(staging, target, true), logger)

	if result.ItemsCleaned != 0 {
		t.Fatalf("ItemsCleaned = %d, want 0 under dry_run", result.ItemsCleaned)
	}
	if !pathExistsAndIsDir(filepath.Join(staging, "2026", "07", "27")) {
		t.Fatal("dry_run must not delete a verified staging group")
	}
}

func TestArchiveLifecycleRetiresVerifiedGroupsOnly(t *testing.T) {
	staging, target, _ := buildArchivePair(t)
	ageArchivePath(t, staging, 60*24*time.Hour)
	// Break day 28's archive so only day 27 is redundant.
	if err := os.Remove(filepath.Join(target, "2026", "07", "28", "rollout-a.jsonl.zst")); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	result := NewArchiveLifecyclePlugin().Cleanup(context.Background(), LevelModerate, archiveTestConfig(staging, target, false), logger)

	if result.ItemsCleaned != 1 {
		t.Fatalf("ItemsCleaned = %d, want 1", result.ItemsCleaned)
	}
	if pathExists(filepath.Join(staging, "2026", "07", "27")) {
		t.Fatal("verified staging group should be retired")
	}
	if !pathExistsAndIsDir(filepath.Join(staging, "2026", "07", "28")) {
		t.Fatal("unverified staging group must survive")
	}
	if result.BytesFreed <= 0 {
		t.Fatalf("BytesFreed = %d, want > 0", result.BytesFreed)
	}
}

func TestArchiveLifecycleReportsOnlyBelowModerate(t *testing.T) {
	staging, target, _ := buildArchivePair(t)
	ageArchivePath(t, staging, 60*24*time.Hour)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := archiveTestConfig(staging, target, false)

	result := NewArchiveLifecyclePlugin().Cleanup(context.Background(), LevelWarning, cfg, logger)
	if result.ItemsCleaned != 0 {
		t.Fatalf("ItemsCleaned = %d, want 0 at warning level", result.ItemsCleaned)
	}
	if !pathExistsAndIsDir(filepath.Join(staging, "2026", "07", "27")) {
		t.Fatal("warning level must not retire anything")
	}

	plan := NewArchiveLifecyclePlugin().PlanCleanup(context.Background(), LevelWarning, cfg, logger)
	for _, tgt := range plan.Targets {
		if tgt.Action == "retire_archive_staging_group" {
			t.Fatalf("warning level must not plan retirement: %#v", tgt)
		}
	}
}

func TestArchiveLifecycleSkipsWhenTargetDirMissing(t *testing.T) {
	staging, target, _ := buildArchivePair(t)
	ageArchivePath(t, staging, 60*24*time.Hour)
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := archiveTestConfig(staging, target, false)

	plan := NewArchiveLifecyclePlugin().PlanCleanup(context.Background(), LevelCritical, cfg, logger)
	if len(plan.Targets) != 0 {
		t.Fatalf("an absent archive target must produce no targets, got %#v", plan.Targets)
	}
	if len(plan.Warnings) == 0 {
		t.Fatal("an absent archive target must warn: it usually means an unmounted volume")
	}

	result := NewArchiveLifecyclePlugin().Cleanup(context.Background(), LevelCritical, cfg, logger)
	if result.ItemsCleaned != 0 {
		t.Fatalf("ItemsCleaned = %d, want 0", result.ItemsCleaned)
	}
	if !pathExistsAndIsDir(filepath.Join(staging, "2026", "07", "27")) {
		t.Fatal("nothing may be retired when the archive target is unreachable")
	}
}

func TestArchiveLifecycleRevalidatesBeforeDeleting(t *testing.T) {
	staging, target, _ := buildArchivePair(t)
	ageArchivePath(t, staging, 60*24*time.Hour)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	source := config.ArchiveSourceConfig{Name: "codex", StagingDir: staging, TargetDir: target, GroupDepth: 3}

	group := filepath.Join(staging, "2026", "07", "27")
	if verdict := verifyArchiveStagingGroup(context.Background(), source, group); !verdict.Verified {
		t.Fatalf("precondition: group should verify, got %q", verdict.Reason)
	}

	// A new unarchived staging file appears. The verifier is re-run against live
	// filesystem state immediately before deletion, so this must flip the
	// verdict and the group must survive.
	writeArchiveFile(t, filepath.Join(group, "rollout-late.jsonl"), []byte("late\n"))

	verdict := verifyArchiveStagingGroup(context.Background(), source, group)
	if verdict.Verified {
		t.Fatalf("re-verification must fail once an unarchived file appears: %q", verdict.Reason)
	}

	// Day 28 is still fully archived and retires normally; day 27 must not.
	result := NewArchiveLifecyclePlugin().Cleanup(context.Background(), LevelModerate, archiveTestConfig(staging, target, false), logger)
	if result.ItemsCleaned != 1 {
		t.Fatalf("ItemsCleaned = %d, want 1 (only the still-verified day)", result.ItemsCleaned)
	}
	if pathExists(filepath.Join(staging, "2026", "07", "28")) {
		t.Fatal("the still-verified day should have been retired")
	}
	if !pathExists(filepath.Join(group, "rollout-late.jsonl")) {
		t.Fatal("group with an unarchived file must survive")
	}
}

func TestArchiveSourceForStagingGroup(t *testing.T) {
	staging, target, _ := buildArchivePair(t)
	alCfg := config.ArchiveLifecycleConfig{
		Sources: []config.ArchiveSourceConfig{
			{Name: "codex", StagingDir: staging, TargetDir: target, GroupDepth: 3},
		},
	}

	source, ok := archiveSourceForStagingGroup("", alCfg, filepath.Join(staging, "2026", "07", "27"))
	if !ok || source.Name != "codex" {
		t.Fatalf("expected the codex source to own the group, got %#v ok=%v", source, ok)
	}
	if _, ok := archiveSourceForStagingGroup("", alCfg, t.TempDir()); ok {
		t.Fatal("a path outside every configured staging dir must not resolve to a source")
	}
}

func TestRetireArchiveStagingGroupRefusesRoot(t *testing.T) {
	staging, _, _ := buildArchivePair(t)
	if _, err := retireArchiveStagingGroup(context.Background(), staging, staging); err == nil {
		t.Fatal("retiring the staging root itself must be refused")
	}
	outside := t.TempDir()
	if _, err := retireArchiveStagingGroup(context.Background(), outside, staging); err == nil {
		t.Fatal("retiring a path outside the staging root must be refused")
	}
	if !pathExistsAndIsDir(outside) {
		t.Fatal("refused path must not be deleted")
	}
}

func TestAgentTranscriptDirExcluded(t *testing.T) {
	for _, name := range []string{
		".incident-backup-20260531T214357Z",
		".incident-backup-20260531T214520Z-idlehomes",
		".tmp",
		"archived_sessions",
		"backups",
		"receipts",
	} {
		if !agentTranscriptDirExcluded(name) {
			t.Errorf("%s must be excluded from in-place transcript compression", name)
		}
	}
	for _, name := range []string{"2026", "07", "27", "sessions"} {
		if agentTranscriptDirExcluded(name) {
			t.Errorf("%s is a normal session partition and must not be excluded", name)
		}
	}
}
