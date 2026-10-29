//go:build linux || darwin

package plugins

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

func diskCacheFixture(t *testing.T) (cache, old, fresh, tmp string) {
	t.Helper()
	cache = filepath.Join(t.TempDir(), "bazel-disk-cache")
	old = filepath.Join(cache, "cas", "aa", "aa01")
	fresh = filepath.Join(cache, "ac", "bb", "bb02")
	tmp = filepath.Join(cache, "tmp", "inflight")
	for _, p := range []string{old, fresh, tmp} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, 16384), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-30 * 24 * time.Hour)
	for _, p := range []string{old, tmp} {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}
	return cache, old, fresh, tmp
}

func TestBazelDiskCacheAgeReportOnlyByDefault(t *testing.T) {
	cache, old, fresh, tmp := diskCacheFixture(t)
	cfg := config.DefaultConfig().Bazel
	if cfg.DiskCacheAgeDelete {
		t.Fatal("disk_cache_age_delete must default to false")
	}
	cfg.DiskCachePaths = []string{cache}
	targets, meta := planBazelDiskCacheAge(context.Background(), cfg, nil, t.TempDir(), false, nil, time.Now())
	if len(targets) != 1 {
		t.Fatalf("targets = %+v", targets)
	}
	tg := targets[0]
	if tg.Action != bazelDiskCacheAgeReport || tg.Bytes <= 0 || !strings.Contains(tg.Reason, "1 of 2 entries") {
		t.Fatalf("report target: %+v", tg)
	}
	if bazelTargetEligibleForDeletion(tg) || bazelTargetReclaimsHost(tg) {
		t.Fatal("a report target must never be eligible for deletion")
	}
	if meta["disk_cache_age_expired_bytes"] == "0" {
		t.Fatalf("meta: %v", meta)
	}
	p := &BazelPlugin{activeProbe: func(context.Context) (bazelProcessInfo, error) { return bazelProcessInfo{}, nil }}
	var res CleanupResult
	p.applyBazelDiskCacheAge(context.Background(), cfg, targets, retDiscard(), &res)
	for _, f := range []string{old, fresh, tmp} {
		if !pathExists(f) {
			t.Fatalf("report-only rule removed %s", f)
		}
	}
}

func TestBazelDiskCacheAgeDeletesExpiredEntriesOnly(t *testing.T) {
	cache, old, fresh, tmp := diskCacheFixture(t)
	cfg := config.DefaultConfig().Bazel
	cfg.DiskCachePaths = []string{cache}
	cfg.DiskCacheAgeDelete = true
	targets, _ := planBazelDiskCacheAge(context.Background(), cfg, nil, t.TempDir(), false, nil, time.Now())
	if len(targets) != 1 || targets[0].Action != bazelDiskCacheAgeDelete || !bazelTargetReclaimsHost(targets[0]) {
		t.Fatalf("targets = %+v", targets)
	}
	if bazelTargetEligibleForDeletion(targets[0]) {
		t.Fatal("the whole-tier deleter must not pick up the per-entry target")
	}
	p := &BazelPlugin{activeProbe: func(context.Context) (bazelProcessInfo, error) { return bazelProcessInfo{}, nil }}
	var res CleanupResult
	p.applyBazelDiskCacheAge(context.Background(), cfg, targets, retDiscard(), &res)
	if pathExists(old) || res.ItemsCleaned != 1 {
		t.Fatalf("expired entry not removed: %+v", res)
	}
	if !pathExists(fresh) || !pathExists(tmp) {
		t.Fatal("a fresh entry or Bazel's tmp/ was removed")
	}
}

func TestBazelDiskCacheAgeWaitsForActiveClients(t *testing.T) {
	cache, old, _, _ := diskCacheFixture(t)
	cfg := config.DefaultConfig().Bazel
	cfg.DiskCachePaths = []string{cache}
	cfg.DiskCacheAgeDelete = true
	targets, _ := planBazelDiskCacheAge(context.Background(), cfg, nil, t.TempDir(), true, nil, time.Now())
	if len(targets) != 1 || !targets[0].Protected || !targets[0].Active {
		t.Fatalf("active clients did not protect the cache: %+v", targets)
	}
	// A client that starts after planning stops the removal too.
	targets, _ = planBazelDiskCacheAge(context.Background(), cfg, nil, t.TempDir(), false, nil, time.Now())
	p := &BazelPlugin{activeProbe: func(context.Context) (bazelProcessInfo, error) {
		return bazelProcessInfo{Reasons: []string{"bazel build"}}, nil
	}}
	var res CleanupResult
	p.applyBazelDiskCacheAge(context.Background(), cfg, targets, retDiscard(), &res)
	if !pathExists(old) {
		t.Fatal("entry removed while a Bazel client runs")
	}
}

func TestBazelDiskCacheAgeRecentAtimeKeepsEntry(t *testing.T) {
	cache, old, _, _ := diskCacheFixture(t)
	// A cache hit read the entry yesterday: atime is recent, mtime old.
	if err := os.Chtimes(old, time.Now().Add(-24*time.Hour), time.Now().Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig().Bazel
	cfg.DiskCachePaths = []string{cache}
	targets, _ := planBazelDiskCacheAge(context.Background(), cfg, nil, t.TempDir(), false, nil, time.Now())
	if len(targets) != 1 || targets[0].Bytes != 0 || targets[0].Action != "keep" {
		t.Fatalf("recently read entry counted as expired: %+v", targets)
	}
}
