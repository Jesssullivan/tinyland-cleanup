package plugins

import (
	"context"
	"os/exec"
	"time"
)

// goEnvProbeTimeout bounds every `go env` child this package starts.
//
// On 2026-10-07 the launchd daemon on a lab Mac (petting-zoo-mini) hung for
// hours on one `go env GOCACHE` child: the Go toolchain lives on an external
// /nix volume, macOS held the child on a pending privacy (TCC) prompt, and the
// child inherited only the daemon's cycle context, which has no deadline
// (lab TIN-5008, rulings R-C430/R-C429). A probe that cannot answer quickly
// is treated like a missing toolchain: the caller skips the Go cache.
var goEnvProbeTimeout = 15 * time.Second

// goEnvProbeWaitDelay bounds the wait for the child's output pipes after the
// deadline kills it, so a grandchild holding stdout cannot stall the caller.
var goEnvProbeWaitDelay = 2 * time.Second

// goEnvOutput runs `go env <name>` under goEnvProbeTimeout and returns its
// stdout, exactly like exec.CommandContext(ctx, "go", "env", name).Output()
// but bounded. It never outlives ctx either.
func goEnvOutput(ctx context.Context, name string) ([]byte, error) {
	probeCtx, cancel := context.WithTimeout(ctx, goEnvProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, "go", "env", name)
	cmd.WaitDelay = goEnvProbeWaitDelay
	return cmd.Output()
}
