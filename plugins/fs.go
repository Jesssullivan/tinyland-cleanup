// Package plugins provides cleanup plugin implementations.
// fs.go contains filesystem-aware helpers that respect mount boundaries.
package plugins

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

var errFilesystemBoundary = errors.New("filesystem mount boundary encountered")
var errFilesystemIdentityChanged = errors.New("filesystem identity changed")

type pathIdentity struct {
	device  uint64
	inode   uint64
	mountID string
}

type pathIdentityFunc func(path string, info os.FileInfo) (pathIdentity, error)

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

// getFileAllocatedBytes returns physical blocks allocated on disk for a file or
// symlink. It intentionally uses lstat so symlink forests do not charge the
// allocation of their /nix/store targets to the directory that contains them.
// It falls back to apparent size if the filesystem does not report blocks.
func getFileAllocatedBytes(path string) (int64, error) {
	info, err := os.Lstat(path)
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

// getDirAllocatedBytesSameDeviceContext measures a directory only when every
// visited entry is on the root directory's device. charge is called once for
// every visited entry so callers can enforce their own traversal budget.
func getDirAllocatedBytesSameDeviceContext(ctx context.Context, path string, charge func(string) error) (int64, pathIdentity, error) {
	return getDirAllocatedBytesSameDeviceContextWithIdentity(ctx, path, charge, fileInfoIdentity)
}

func getDirAllocatedBytesSameDeviceContextWithIdentity(ctx context.Context, path string, charge func(string) error, identityFor pathIdentityFunc) (int64, pathIdentity, error) {
	var size int64
	rootIdentity, err := walkDirSameDeviceContext(ctx, path, charge, identityFor, func(_ string, info os.FileInfo) error {
		if info.IsDir() {
			return nil
		}
		allocated, err := allocatedBytesFromFileInfo(info)
		if err != nil {
			return err
		}
		size += allocated
		return nil
	})
	return size, rootIdentity, err
}

func validateDirSameDeviceContext(ctx context.Context, path string, charge func(string) error) (pathIdentity, error) {
	return walkDirSameDeviceContext(ctx, path, charge, fileInfoIdentity, nil)
}

func walkDirSameDeviceContext(ctx context.Context, path string, charge func(string) error, identityFor pathIdentityFunc, visit func(string, os.FileInfo) error) (pathIdentity, error) {
	if identityFor == nil {
		return pathIdentity{}, errors.New("filesystem identity probe is unavailable")
	}

	var rootIdentity pathIdentity
	rootSeen := false
	err := filepath.Walk(path, func(current string, info os.FileInfo, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if charge != nil {
			if err := charge(current); err != nil {
				return err
			}
		}

		identity, err := identityFor(current, info)
		if err != nil {
			return fmt.Errorf("probe filesystem identity for %s: %w", current, err)
		}
		if !rootSeen {
			if !info.IsDir() {
				return fmt.Errorf("walk root %s is not a directory", current)
			}
			rootIdentity = identity
			rootSeen = true
		} else if identity.mountID != rootIdentity.mountID {
			return fmt.Errorf("%w at %s", errFilesystemBoundary, current)
		}

		if visit != nil {
			if err := visit(current, info); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return rootIdentity, err
	}
	if err := ctx.Err(); err != nil {
		return rootIdentity, err
	}
	if !rootSeen {
		return pathIdentity{}, fmt.Errorf("walk root %s was not visited", path)
	}
	return rootIdentity, nil
}

func fileInfoIdentity(path string, info os.FileInfo) (pathIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return pathIdentity{}, errors.New("filesystem metadata does not expose device and inode IDs")
	}
	mountID, err := platformMountID(path)
	if err != nil {
		return pathIdentity{}, err
	}
	if mountID == "" {
		return pathIdentity{}, errors.New("filesystem metadata does not expose a mount identity")
	}
	return pathIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino), mountID: mountID}, nil
}

func allocatedBytesFromFileInfo(info os.FileInfo) (int64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("filesystem metadata does not expose allocated blocks")
	}
	if stat.Blocks <= 0 {
		return info.Size(), nil
	}
	return stat.Blocks * 512, nil
}

// removeAllSameDevice removes path without following symlinks or descending
// into an entry whose device differs from the validated root device.
func removeAllSameDevice(ctx context.Context, path string, rootIdentity pathIdentity, charge func(string) error) error {
	return removeAllSameDeviceWithIdentity(ctx, path, rootIdentity, charge, fileInfoIdentity)
}

func removeAllSameDeviceWithIdentity(ctx context.Context, path string, rootIdentity pathIdentity, charge func(string) error, identityFor pathIdentityFunc) error {
	if identityFor == nil {
		return errors.New("filesystem identity probe is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if charge != nil {
		if err := charge(path); err != nil {
			return err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	identity, err := identityFor(path, info)
	if err != nil {
		return fmt.Errorf("probe filesystem identity for %s: %w", path, err)
	}
	if identity.mountID != rootIdentity.mountID {
		return fmt.Errorf("%w at %s", errFilesystemBoundary, path)
	}
	if path == filepath.Clean(path) && (identity.device != rootIdentity.device || identity.inode != rootIdentity.inode || !info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("%w at %s", errFilesystemIdentityChanged, path)
	}
	if !info.IsDir() {
		return os.Remove(path)
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	type childEntry struct {
		path string
		info os.FileInfo
	}
	children := make([]childEntry, 0, len(entries))
	for _, entry := range entries {
		childPath := filepath.Join(path, entry.Name())
		childInfo, err := entry.Info()
		if err != nil {
			return err
		}
		childIdentity, err := identityFor(childPath, childInfo)
		if err != nil {
			return fmt.Errorf("probe filesystem identity for %s: %w", childPath, err)
		}
		if childIdentity.mountID != rootIdentity.mountID {
			return fmt.Errorf("%w at %s", errFilesystemBoundary, childPath)
		}
		children = append(children, childEntry{path: childPath, info: childInfo})
	}

	for _, child := range children {
		if child.info.IsDir() {
			if err := removeAllSameDeviceChild(ctx, child.path, rootIdentity, charge, identityFor); err != nil {
				return err
			}
			continue
		}
		if err := os.Remove(child.path); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

func removeAllSameDeviceChild(ctx context.Context, path string, rootIdentity pathIdentity, charge func(string) error, identityFor pathIdentityFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if charge != nil {
		if err := charge(path); err != nil {
			return err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	identity, err := identityFor(path, info)
	if err != nil {
		return err
	}
	if identity.mountID != rootIdentity.mountID {
		return fmt.Errorf("%w at %s", errFilesystemBoundary, path)
	}
	if !info.IsDir() {
		return os.Remove(path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := removeAllSameDeviceChild(ctx, filepath.Join(path, entry.Name()), rootIdentity, charge, identityFor); err != nil {
			return err
		}
	}
	return os.Remove(path)
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
