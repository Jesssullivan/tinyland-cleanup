package plugins

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeFileInfo struct {
	os.FileInfo
	modTime time.Time
}

func (f fakeFileInfo) ModTime() time.Time { return f.modTime }
func (f fakeFileInfo) Sys() any           { return nil }

func TestEffectiveModTimeKeepsPastMtime(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	past := now.Add(-48 * time.Hour)
	if got := effectiveModTime(fakeFileInfo{modTime: past}, now); !got.Equal(past) {
		t.Fatalf("effectiveModTime(past) = %v, want %v", got, past)
	}
	skewed := now.Add(futureMtimeTolerance / 2)
	if got := effectiveModTime(fakeFileInfo{modTime: skewed}, now); !got.Equal(skewed) {
		t.Fatalf("effectiveModTime(within tolerance) = %v, want %v", got, skewed)
	}
}

func TestEffectiveModTimeFutureWithoutCtimeIsNow(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	future := time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := effectiveModTime(fakeFileInfo{modTime: future}, now); !got.Equal(now) {
		t.Fatalf("effectiveModTime(future, no ctime) = %v, want now %v", got, now)
	}
}

// A 2036-stamped file must not be immortal: once the wall clock moves past
// the cutoff measured from the real change time, age-based reclaim sees it.
func TestFutureMtimeFileAgesFromChangeTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bazel-output-base")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().After(time.Now().Add(24 * time.Hour)) {
		t.Fatalf("expected a future mtime, got %v", info.ModTime())
	}

	now := time.Now()
	effective := effectiveModTime(info, now)
	if effective.After(now.Add(futureMtimeTolerance)) {
		t.Fatalf("effective mtime %v must not be in the future", effective)
	}

	later := now.Add(48 * time.Hour)
	cutoff := later.Add(-24 * time.Hour)
	if !effectiveModTime(info, later).Before(cutoff) {
		t.Fatalf("future-stamped file should be stale 48h later; effective=%v cutoff=%v", effectiveModTime(info, later), cutoff)
	}
	if info.ModTime().Before(cutoff) {
		t.Fatal("raw mtime comparison should still consider the file fresh (that is the defect)")
	}
}
