//go:build !linux && !darwin

package plugins

import (
	"context"
	"errors"
	"os"
	"time"
)

var errBBUnsupported = errors.New("bazel_bases is supported on Linux and Darwin only")

func bbStatfsUsage(string) (bbUsage, error) { return bbUsage{}, errBBUnsupported }

func bbAccessTime(os.FileInfo) (time.Time, bool) { return time.Time{}, false }

func bbScanActivity(context.Context, bbActivityOptions) *bbActivity {
	a := newBBActivity()
	a.incomplete = append(a.incomplete, errBBUnsupported.Error())
	return a
}

func bbLockHolder(string) (string, error) { return "", errBBUnsupported }

func bbTakeLock(string) (bbHeldLock, string, error) { return nil, "", errBBUnsupported }

func bbMountPoints() ([]string, error) { return nil, errBBUnsupported }
