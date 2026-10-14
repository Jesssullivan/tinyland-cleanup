// Package plugins provides cleanup plugin implementations.
// sudo.go provides shared sudo capability detection for plugins that need
// elevated privileges (APFS snapshots, iOS Simulator runtimes, etc.).
package plugins

import (
	"context"
	"os/exec"
	"os/user"
	"strings"
)

// SudoCapability represents the sudo availability for the current user.
type SudoCapability struct {
	// Available indicates sudo binary exists
	Available bool
	// Passwordless indicates sudo -n true succeeds (no password prompt)
	Passwordless bool
	// Groups contains the user's group memberships
	Groups []string
}

// DetectSudo checks sudo availability and passwordless status.
func DetectSudo(ctx context.Context) SudoCapability {
	cap := SudoCapability{}

	// Check if sudo binary exists
	if _, err := exec.LookPath("sudo"); err != nil {
		return cap
	}
	cap.Available = true

	// Check if passwordless sudo works
	testCmd := commandContext(ctx, "sudo", "-n", "true")
	if testCmd.Run() == nil {
		cap.Passwordless = true
	}

	// Get user groups
	if u, err := user.Current(); err == nil {
		if groupIDs, err := u.GroupIds(); err == nil {
			for _, gid := range groupIDs {
				if g, err := user.LookupGroupId(gid); err == nil {
					cap.Groups = append(cap.Groups, g.Name)
				}
			}
		}
	}

	return cap
}

// RunWithSudo executes a command with sudo -n (non-interactive).
// Returns output and error. Fails immediately if password would be required.
func RunWithSudo(ctx context.Context, args ...string) ([]byte, error) {
	cmdArgs := append([]string{"-n"}, args...)
	cmd := reclaimCommandContext(ctx, "sudo", cmdArgs...)
	return cmd.CombinedOutput()
}

// HasGroup checks if the current user is in the specified group.
func (s SudoCapability) HasGroup(name string) bool {
	for _, g := range s.Groups {
		if strings.EqualFold(g, name) {
			return true
		}
	}
	return false
}
