//go:build darwin

package plugins

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

func nativeTemporaryMounts(ctx context.Context) ([]string, error) {
	// NOWAIT avoids waiting for unavailable external storage. Count separately,
	// allocate one extra entry and re-count: truncation/churn is unknown custody.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	count, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil || count <= 0 || count >= maxTemporaryMounts {
		return nil, fmt.Errorf("native mount count unavailable or over bound")
	}
	stats := make([]unix.Statfs_t, count+1)
	filled, err := unix.Getfsstat(stats, unix.MNT_NOWAIT)
	if err != nil || filled <= 0 || filled >= len(stats) {
		return nil, fmt.Errorf("native mount inventory unavailable or truncated")
	}
	after, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil || after != filled {
		return nil, fmt.Errorf("native mount inventory changed during observation")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	mounts := make([]string, 0, filled)
	for _, stat := range stats[:filled] {
		end := strings.IndexByte(string(stat.Mntonname[:]), 0)
		if end <= 0 {
			return nil, fmt.Errorf("native mountpoint name unavailable or truncated")
		}
		mounts = append(mounts, string(stat.Mntonname[:end]))
	}
	return mounts, nil
}
