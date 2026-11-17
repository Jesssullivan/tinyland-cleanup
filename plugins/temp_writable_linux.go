//go:build linux

package plugins

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// errTempRootIdentityChanged reports that the path no longer names the root
// the final check passed: nothing was changed and nothing may be removed.
var errTempRootIdentityChanged = errors.New("temporary root identity changed")

// tempRootWritableLimit bounds the directories one repair visits, as the
// content walk is bounded. Tests lower it.
var tempRootWritableLimit = 100000

// makeTempRootWritable gives the owner read, write and search permission on
// the root and on every directory under it that lacks one of them, so that a
// root the daemon already decided to remove can be removed although a test
// fixture or a copied store tree left read-only directories in it
// (dev_artifacts.nix_temp_root_make_writable, TIN-8526).
//
// It only changes a directory that this process's uid owns and that is on
// the root's device. It opens every directory with O_PATH, O_DIRECTORY and
// O_NOFOLLOW relative to its parent's descriptor, so it never follows a
// symlink and never leaves the root, and it changes the mode through that
// descriptor (/proc/self/fd), never through a path. A directory owned by
// another uid or on another device is neither changed nor entered. The root
// itself must be the file identity describes; otherwise it returns
// errTempRootIdentityChanged and changes nothing.
//
// Regular files are not changed: unlinking needs write permission on the
// directory only. A repair cut by ctx or the limit returns an error and
// leaves the directories it reached writable.
func makeTempRootWritable(ctx context.Context, root string, identity os.FileInfo) error {
	fd, err := syscall.Open(root, unix.O_PATH|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("%w: %v", errTempRootIdentityChanged, err)
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return err
	}
	want, ok := identity.Sys().(*syscall.Stat_t)
	if !ok || want.Dev != st.Dev || want.Ino != st.Ino {
		return errTempRootIdentityChanged
	}
	visited := 0
	return tempRootMakeDirWritable(ctx, fd, &st, st.Dev, os.Geteuid(), &visited)
}

// tempRootMakeDirWritable repairs the directory open as fd (O_PATH) with
// status st, then its subdirectories.
func tempRootMakeDirWritable(ctx context.Context, fd int, st *syscall.Stat_t, dev uint64, uid int, visited *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if *visited++; *visited > tempRootWritableLimit {
		return fmt.Errorf("make-writable bound of %d directories exceeded", tempRootWritableLimit)
	}
	if st.Dev != dev || int(st.Uid) != uid {
		return nil
	}
	if perm := uint32(st.Mode) & 0o7777; perm&0o700 != 0o700 {
		if err := syscall.Chmod("/proc/self/fd/"+strconv.Itoa(fd), perm|0o700); err != nil {
			return err
		}
	}
	dirfd, err := syscall.Openat(fd, ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(dirfd), ".")
	names, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		return err
	}
	for _, name := range names {
		child, err := syscall.Openat(fd, name, unix.O_PATH|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			if errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ELOOP) {
				continue // a file, a symlink, or gone
			}
			return err
		}
		var cst syscall.Stat_t
		if err := syscall.Fstat(child, &cst); err != nil {
			syscall.Close(child)
			return err
		}
		err = tempRootMakeDirWritable(ctx, child, &cst, dev, uid, visited)
		syscall.Close(child)
		if err != nil {
			return err
		}
	}
	return nil
}
