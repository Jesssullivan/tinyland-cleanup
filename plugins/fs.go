// Package plugins provides cleanup plugin implementations.
// fs.go contains filesystem-aware helpers that respect mount boundaries.
package plugins

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// deviceID returns the device ID for a given path.
// Used to detect mount point boundaries during traversal.
func deviceID(path string) (uint64, error) {
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Dev), nil
}

// getDirSizeSameDevice calculates directory size without crossing mount boundaries.
// It resolves the path first (following symlinks) and only counts files on
// the same device as the root directory.
func getDirSizeSameDevice(path string) int64 {
	// Resolve symlinks to get real path
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return getDirSize(path) // fallback to basic version
	}

	rootDev, err := deviceID(resolved)
	if err != nil {
		return getDirSize(path) // fallback
	}

	var size int64
	filepath.Walk(resolved, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		// Check if this entry is on a different device (mount point)
		if dev, err := deviceID(p); err == nil && dev != rootDev {
			if info.IsDir() {
				return filepath.SkipDir // Don't cross into different mounts
			}
			return nil
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size
}

// deleteOldFilesSameDevice deletes files older than maxAge without crossing
// mount point boundaries. Returns the number of bytes freed.
func deleteOldFilesSameDevice(dir string, maxAge time.Duration) int64 {
	cutoff := time.Now().Add(-maxAge)
	var freed int64

	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		// Fallback: use basic version
		deleteOldFiles(dir, maxAge)
		return 0
	}

	rootDev, err := deviceID(resolved)
	if err != nil {
		deleteOldFiles(dir, maxAge)
		return 0
	}

	filepath.Walk(resolved, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		// Don't cross mount boundaries
		if dev, err := deviceID(path); err == nil && dev != rootDev {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.IsDir() && info.ModTime().Before(cutoff) {
			size := info.Size()
			if os.Remove(path) == nil {
				freed += size
			}
		}
		return nil
	})
	return freed
}

// deleteOldFilesOwnedByUserSameDevice deletes user-owned files older than
// maxAge without crossing mount boundaries. Returns bytes freed.
func deleteOldFilesOwnedByUserSameDevice(dir string, maxAge time.Duration) int64 {
	cutoff := time.Now().Add(-maxAge)
	uid := uint32(os.Getuid())
	var freed int64

	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return 0
	}

	rootDev, err := deviceID(resolved)
	if err != nil {
		return 0
	}

	filepath.Walk(resolved, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		// Don't cross mount boundaries
		if dev, err := deviceID(path); err == nil && dev != rootDev {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.IsDir() && info.ModTime().Before(cutoff) && info.Mode().IsRegular() {
			// Check file ownership via syscall
			var stat syscall.Stat_t
			if syscall.Stat(path, &stat) == nil && stat.Uid == uid {
				size := info.Size()
				if os.Remove(path) == nil {
					freed += size
				}
			}
		}
		return nil
	})
	return freed
}

// getFreeDiskSpace returns the available disk space in bytes for the
// filesystem containing the given path.
func getFreeDiskSpace(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

// getFileAllocatedBytes returns physical blocks allocated on disk for a file.
// It falls back to apparent size if the filesystem does not report blocks.
func getFileAllocatedBytes(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Blocks <= 0 {
		return info.Size(), nil
	}

	return stat.Blocks * 512, nil
}

func getDirAllocatedBytes(path string) int64 {
	size, _ := getDirAllocatedBytesContext(context.Background(), path)
	return size
}

func getDirAllocatedBytesContext(ctx context.Context, path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		allocated, err := getFileAllocatedBytes(p)
		if err != nil {
			size += info.Size()
			return nil
		}
		size += allocated
		return nil
	})
	if err != nil {
		return size, err
	}
	return size, ctx.Err()
}

// safeBytesDiff returns the difference between two sizes, floored at 0.
// Prevents negative BytesFreed when files are added during cleanup.
func safeBytesDiff(before, after int64) int64 {
	diff := before - after
	if diff < 0 {
		return 0
	}
	return diff
}

// pathExists returns true if a path exists and is accessible.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// pathExistsAndIsDir returns true if path exists and is a directory.
func pathExistsAndIsDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
