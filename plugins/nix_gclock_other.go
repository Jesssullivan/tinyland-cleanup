//go:build !unix

package plugins

import "errors"

// nixFlockFile has no flock(2) here, so a configured lock refuses the pass
// rather than collecting unserialized.
func nixFlockFile(path string) (func(), error) {
	return nil, errors.New("nix.gc_lock_file needs flock(2), which this platform lacks")
}

// bazelGCLockTry has no flock(2) here, so the dedupe pass treats Bazel as
// active and removes nothing.
func bazelGCLockTry(path string, create bool) (func(), error) {
	return nil, errBazelGCLockHeld
}
