//go:build !darwin && !linux

package plugins

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

const nixExternalCommandWaitDelay = 5 * time.Second

func configureExternalCommandTermination(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = nixExternalCommandWaitDelay
}

// External authority is intentionally unavailable where this package cannot
// prove root ownership. Builtin cleanup remains available on those platforms.
func validateExternalPathOwner(_ os.FileInfo, path string) error {
	return fmt.Errorf("external Nix GC path ownership validation is unsupported on this platform: %s", path)
}
