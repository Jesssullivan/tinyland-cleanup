//go:build linux

package plugins

import (
	"context"
	"os"
)

func nativeTemporaryMounts(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	mounts, err := parseTemporaryMountInfo(file)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return mounts, nil
}
