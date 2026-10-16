package rootjobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readUnit(t *testing.T, name string) string {
	t.Helper()
	for _, dir := range []string{"packaging/systemd", "../packaging/systemd"} {
		if raw, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			return string(raw)
		}
	}
	t.Fatalf("unit %s not found", name)
	return ""
}

func unitLines(text string) map[string][]string {
	out := map[string][]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok {
			out[key] = append(out[key], value)
		}
	}
	return out
}

// Review M3 and R-C780: the unregister unit keeps /tmp visible (out-links
// live there), writes only the auto directory and its state, and is the
// only privilege: no sudo anywhere.
func TestGCRootUnregisterUnitIsConfinedAndKeepsTmpVisible(t *testing.T) {
	lines := unitLines(readUnit(t, "tinyland-cleanup-gcroot-unregister.service"))
	if _, ok := lines["PrivateTmp"]; ok {
		t.Fatal("PrivateTmp would hide /tmp out-links from the job")
	}
	if got := lines["ReadWritePaths"]; len(got) != 1 || got[0] != "/nix/var/nix/gcroots/auto" {
		t.Fatalf("ReadWritePaths must be the auto directory alone: %q", got)
	}
	for key, want := range map[string]string{
		"ProtectSystem":           "strict",
		"ProtectHome":             "read-only",
		"NoNewPrivileges":         "yes",
		"PrivateNetwork":          "yes",
		"CapabilityBoundingSet":   "CAP_DAC_READ_SEARCH",
		"StateDirectory":          "tinyland-cleanup",
		"RestrictAddressFamilies": "AF_UNIX",
	} {
		if got := lines[key]; len(got) != 1 || got[0] != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	if exec := lines["ExecStart"]; len(exec) != 1 || !strings.HasPrefix(exec[0], "@bindir@/tinyland-cleanup gcroot-unregister ") {
		t.Fatalf("ExecStart must run this package's binary: %q", exec)
	}
	for _, name := range []string{"tinyland-cleanup-gcroot-unregister.service", "tinyland-cleanup-profile-prune.service"} {
		for key, values := range unitLines(readUnit(t, name)) {
			for _, v := range values {
				if strings.Contains(strings.ToLower(v), "sudo") {
					t.Fatalf("%s: %s must not use sudo: %q", name, key, v)
				}
			}
		}
	}
}

// Re-review N5: profile-prune runs runuser and nix-env as root, so it keeps
// the filesystem writable, but needs no network, kernel or cgroup writes.
func TestProfilePruneUnitIsConfined(t *testing.T) {
	lines := unitLines(readUnit(t, "tinyland-cleanup-profile-prune.service"))
	for key, want := range map[string]string{
		"NoNewPrivileges":         "yes",
		"PrivateNetwork":          "yes",
		"RestrictAddressFamilies": "AF_UNIX AF_NETLINK",
		"ProtectKernelTunables":   "yes",
		"ProtectKernelModules":    "yes",
		"ProtectKernelLogs":       "yes",
		"ProtectControlGroups":    "yes",
		"LockPersonality":         "yes",
		"SystemCallArchitectures": "native",
	} {
		if got := lines[key]; len(got) != 1 || got[0] != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	for _, forbidden := range []string{"ProtectHome", "ProtectSystem", "CapabilityBoundingSet"} {
		if _, ok := lines[forbidden]; ok {
			t.Fatalf("%s would stop runuser or nix-env from pruning profiles", forbidden)
		}
	}
	if exec := lines["ExecStart"]; len(exec) != 1 || !strings.HasPrefix(exec[0], "@bindir@/tinyland-cleanup profile-prune ") {
		t.Fatalf("ExecStart must run this package's binary: %q", exec)
	}
}
