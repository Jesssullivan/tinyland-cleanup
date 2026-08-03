//go:build darwin || linux

package plugins

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

func TestConfigureExternalCommandTerminationUsesPrivateProcessGroup(t *testing.T) {
	cmd := exec.Command(os.DevNull)
	configureExternalCommandTermination(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("external controller must run in a private process group")
	}
	if cmd.Cancel == nil || !errors.Is(cmd.Cancel(), os.ErrProcessDone) {
		t.Fatal("external controller cancellation must safely handle a command that has not started")
	}
	if cmd.WaitDelay != nixExternalCommandWaitDelay {
		t.Fatalf("external controller wait delay = %s, want %s", cmd.WaitDelay, nixExternalCommandWaitDelay)
	}
}
