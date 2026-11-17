//go:build !linux

package plugins

import (
	"context"
	"errors"
	"os"
)

var errTempRootIdentityChanged = errors.New("temporary root identity changed")

// makeTempRootWritable changes nothing off Linux: the repair needs O_PATH
// and /proc/self/fd. Removal then behaves as it did before
// dev_artifacts.nix_temp_root_make_writable existed.
func makeTempRootWritable(context.Context, string, os.FileInfo) error {
	return nil
}
