//go:build darwin || linux

package plugins

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const nixExternalCommandWaitDelay = 5 * time.Second

// configureExternalCommandTermination gives the controller a private process
// group. Context cancellation then terminates the controller and every Nix
// subprocess it spawned, preventing an orphaned mutation from overlapping a
// later retry. WaitDelay bounds inherited stdout/stderr pipes held by a faulty
// descendant.
func configureExternalCommandTermination(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = nixExternalCommandWaitDelay
}

func validateExternalPathOwner(info os.FileInfo, path string) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("external Nix GC path ownership is unavailable: %s", path)
	}
	owner := int(stat.Uid)
	if owner != 0 {
		return fmt.Errorf("external Nix GC path must be root-owned: %s is owned by uid %d", path, owner)
	}
	return nil
}
