package plugins

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
)

func withStreamWalkBatch(t *testing.T, n int) {
	t.Helper()
	previous := streamWalkBatch
	streamWalkBatch = n
	t.Cleanup(func() { streamWalkBatch = previous })
}

func buildWalkFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"a/b/c", "a/skip/deep", "empty", "d"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 7; i++ {
		for _, dir := range []string{"", "a", "a/b/c", "a/skip", "a/skip/deep", "d"} {
			name := filepath.Join(root, dir, fmt.Sprintf("f%d", i))
			if err := os.WriteFile(name, []byte("0123456789"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "d", "link-to-a")); err != nil {
		t.Fatal(err)
	}
	return root
}

func collectWalk(t *testing.T, walk func(string, filepath.WalkFunc) error, root string) ([]string, int64) {
	t.Helper()
	var paths []string
	var size int64
	err := walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && info.Name() == "skip" {
			return filepath.SkipDir
		}
		paths = append(paths, path)
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	return paths, size
}

// streamWalk visits exactly what filepath.Walk visits, at every batch size.
func TestStreamWalkMatchesFilepathWalk(t *testing.T) {
	root := buildWalkFixture(t)
	wantPaths, wantSize := collectWalk(t, filepath.Walk, root)
	for _, batch := range []int{1, 2, 3, 1024, -1} {
		withStreamWalkBatch(t, batch)
		gotPaths, gotSize := collectWalk(t, streamWalk, root)
		if gotSize != wantSize {
			t.Errorf("batch %d: size %d, want %d", batch, gotSize, wantSize)
		}
		if fmt.Sprint(gotPaths) != fmt.Sprint(wantPaths) {
			t.Errorf("batch %d: visited\n%v\nwant\n%v", batch, gotPaths, wantPaths)
		}
	}
}

func TestStreamWalkSkipAllAndMissingRoot(t *testing.T) {
	root := buildWalkFixture(t)
	withStreamWalkBatch(t, 2)
	visited := 0
	err := streamWalk(root, func(string, os.FileInfo, error) error {
		visited++
		if visited == 3 {
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil || visited != 3 {
		t.Fatalf("SkipAll: err=%v visited=%d, want nil and 3", err, visited)
	}

	stop := fmt.Errorf("stop")
	if err := streamWalk(root, func(string, os.FileInfo, error) error { return stop }); err != stop {
		t.Fatalf("a callback error was not returned: %v", err)
	}

	var reported error
	if err := streamWalk(filepath.Join(root, "missing"), func(_ string, _ os.FileInfo, err error) error {
		reported = err
		return nil
	}); err != nil || reported == nil {
		t.Fatalf("missing root: err=%v reported=%v", err, reported)
	}
}

func TestStreamWalkDeepTreeDoesNotHoldEveryDirectoryOpen(t *testing.T) {
	root := t.TempDir()
	path := root
	const depth = streamWalkHeldDepth + 40
	for i := 0; i < depth; i++ {
		path = filepath.Join(path, "d")
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "leaf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	maxOpen := 0
	err := streamWalk(root, func(string, os.FileInfo, error) error {
		if entries, err := os.ReadDir("/proc/self/fd"); err == nil && len(entries) > maxOpen {
			maxOpen = len(entries)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if size := getDirSize(root); size != 1 {
		t.Fatalf("getDirSize on a deep tree = %d, want 1", size)
	}
	if maxOpen > streamWalkHeldDepth+40 {
		t.Fatalf("walk held %d descriptors open on a tree %d deep", maxOpen, depth)
	}
}

// Sizing one directory with many entries holds one batch, not the whole
// listing. filepath.Walk holds every name until the directory is done.
func TestStreamWalkLiveHeapStaysFlatOnLargeDirectory(t *testing.T) {
	if testing.Short() {
		t.Skip("creates 40000 files")
	}
	root := t.TempDir()
	const files = 40000
	for i := 0; i < files; i++ {
		name := filepath.Join(root, fmt.Sprintf("entry-with-a-reasonably-long-name-%06d.o", i))
		if err := os.WriteFile(name, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	liveHeapGrowth := func(walk func(string, filepath.WalkFunc) error) (uint64, int) {
		var stats runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&stats)
		base := stats.HeapAlloc
		var peak uint64
		visited := 0
		if err := walk(root, func(string, os.FileInfo, error) error {
			visited++
			if visited%4000 == 0 {
				runtime.GC()
				runtime.ReadMemStats(&stats)
				if stats.HeapAlloc > base && stats.HeapAlloc-base > peak {
					peak = stats.HeapAlloc - base
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return peak, visited
	}

	walkPeak, walkVisited := liveHeapGrowth(filepath.Walk)
	streamPeak, streamVisited := liveHeapGrowth(streamWalk)
	t.Logf("live heap while sizing %d entries: filepath.Walk %d KiB, streamWalk %d KiB", files, walkPeak/1024, streamPeak/1024)
	if streamVisited != walkVisited {
		t.Fatalf("streamWalk visited %d entries, filepath.Walk %d", streamVisited, walkVisited)
	}
	if streamPeak > 1024*1024 {
		t.Fatalf("streamWalk held %d KiB live while walking; one batch should be well under 1 MiB", streamPeak/1024)
	}
	if walkPeak > 0 && streamPeak*2 > walkPeak {
		t.Fatalf("streamWalk (%d KiB) is not clearly below filepath.Walk (%d KiB)", streamPeak/1024, walkPeak/1024)
	}
}

func TestDevArtifactGitTrackerKeepsABoundedWindow(t *testing.T) {
	tracker := newDevArtifactGitTracker()
	for i := 0; i < devArtifactGitTrackerRepos*3; i++ {
		tracker.remember(fmt.Sprintf("/repo/%d", i), []string{"a", "b"})
	}
	if got := len(tracker.trackedFilesByRoot); got != devArtifactGitTrackerRepos {
		t.Fatalf("tracker holds %d repositories, want %d", got, devArtifactGitTrackerRepos)
	}
	if len(tracker.trackedOrder) != devArtifactGitTrackerRepos {
		t.Fatalf("tracker order holds %d entries", len(tracker.trackedOrder))
	}
	newest := fmt.Sprintf("/repo/%d", devArtifactGitTrackerRepos*3-1)
	if _, ok := tracker.trackedFilesByRoot[newest]; !ok {
		t.Fatal("the most recent repository was evicted")
	}
	tracker.remember(newest, nil)
	if len(tracker.trackedOrder) != devArtifactGitTrackerRepos {
		t.Fatal("re-remembering a repository grew the window")
	}
}

func TestReadNULRecordsStreamsOutput(t *testing.T) {
	records, err := readNULRecords(commandContext(nil, "printf", `a/b\000c d\000\000last`))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(records) != "[a/b c d last]" {
		t.Fatalf("records = %q", records)
	}
	if _, err := readNULRecords(commandContext(nil, "sh", "-c", `printf 'x\000'; exit 3`)); err == nil {
		t.Fatal("a failing command returned records without an error")
	}
}
