package plugins

import (
	"strings"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

func TestNixPluginParseDryRunFreedSpace(t *testing.T) {
	p := NewNixPlugin()

	tests := []struct {
		name     string
		output   string
		expected int64
	}{
		{"would free mib", "would delete 42 store paths\nwould free 512.5 MiB", 537395200},
		{"would be freed gib", "1.25 GiB would be freed", 1342177280},
		{"fallback freed", "1234 store paths deleted, 2.0 GiB freed", 2147483648},
		{"bytes", "would free 100 B", 100},
		{"empty", "", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.parseDryRunFreedSpace(tt.output); got != tt.expected {
				t.Fatalf("parseDryRunFreedSpace(%q) = %d, want %d", tt.output, got, tt.expected)
			}
		})
	}
}

func TestNixPluginParseDryRunStorePaths(t *testing.T) {
	p := NewNixPlugin()

	tests := []struct {
		output   string
		expected int
	}{
		{"would delete 42 store paths", 42},
		{"1 store path would be deleted", 1},
		{"1234 store paths deleted, 2.0 GiB freed", 1234},
		{"", 0},
	}

	for _, tt := range tests {
		if got := p.parseDryRunStorePaths(tt.output); got != tt.expected {
			t.Fatalf("parseDryRunStorePaths(%q) = %d, want %d", tt.output, got, tt.expected)
		}
	}
}

func TestParseNixPolicyDuration(t *testing.T) {
	tests := []struct {
		raw      string
		expected time.Duration
	}{
		{"30m", 30 * time.Minute},
		{"14d", 14 * 24 * time.Hour},
		{"2w", 14 * 24 * time.Hour},
		{"bad", 9 * time.Minute},
	}

	for _, tt := range tests {
		if got := parseNixPolicyDuration(tt.raw, 9*time.Minute); got != tt.expected {
			t.Fatalf("parseNixPolicyDuration(%q) = %s, want %s", tt.raw, got, tt.expected)
		}
	}
}

func TestNixBusyProcessReasons(t *testing.T) {
	ps := `
/nix/var/nix/profiles/default/bin/nix nix build .#package
/nix/var/nix/profiles/default/bin/nix nix store gc --dry-run
/run/current-system/sw/bin/home-manager home-manager switch --flake .#jess
/nix/store/abc/bin/nix-daemon nix-daemon --daemon
/nix/store/def/bin/nix-daemon nix-daemon --stdio
/nix/store/ghi/bin/nix-store nix-store --gc --print-roots
/nix/store/jkl/bin/nix-collect-garbage nix-collect-garbage --dry-run
/usr/bin/zsh zsh -lc echo idle
`
	reasons := nixBusyProcessReasons(ps)
	want := []string{"home-manager switch", "nix build", "nix store gc", "nix-collect-garbage", "nix-daemon worker", "nix-store --gc"}

	if len(reasons) != len(want) {
		t.Fatalf("got reasons %v, want %v", reasons, want)
	}
	for i := range want {
		if reasons[i] != want[i] {
			t.Fatalf("got reasons %v, want %v", reasons, want)
		}
	}
}

func TestNixActiveWorkTargetsProtectStoreWork(t *testing.T) {
	targets := nixActiveWorkTargets([]string{"home-manager switch", "nix build"}, "30m")
	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2", len(targets))
	}

	for _, target := range targets {
		if target.Action != "protect_active_work" || !target.Active || !target.Protected {
			t.Fatalf("active Nix work target should be protected: %+v", target)
		}
		if target.Tier != CleanupTierDisruptive || target.Reclaim != CleanupReclaimNone {
			t.Fatalf("active Nix work should be disruptive/no-reclaim: %+v", target)
		}
		if target.HostReclaimsSpace == nil || *target.HostReclaimsSpace {
			t.Fatalf("active Nix work target should not reclaim host space: %+v", target)
		}
		if !strings.Contains(target.Reason, "retry after 30m") {
			t.Fatalf("active Nix work target should include retry guidance: %+v", target)
		}
	}
}

func TestNixDeferPlanAddsOperatorRetryMetadata(t *testing.T) {
	plan := CleanupPlan{
		WouldRun: true,
		Metadata: map[string]string{},
	}
	cfg := config.NixConfig{DaemonBusyBackoff: "15m"}

	nixDeferPlan(&plan, "nix_daemon_busy", "deferred", cfg, nixActiveWorkTargets([]string{"nix build"}, cfg.DaemonBusyBackoff))

	if plan.WouldRun || plan.SkipReason != "nix_daemon_busy" || plan.Summary != "deferred" {
		t.Fatalf("unexpected deferred plan state: %+v", plan)
	}
	if plan.Metadata["retry_after"] != "15m" || plan.Metadata["deferral_reason"] != "nix_daemon_busy" || plan.Metadata["target_count"] != "1" {
		t.Fatalf("deferred plan metadata missing retry details: %+v", plan.Metadata)
	}
	if len(plan.Targets) != 1 || plan.Targets[0].Action != "protect_active_work" {
		t.Fatalf("deferred plan should include active work target: %+v", plan.Targets)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "retry Nix cleanup after 15m") || !strings.Contains(plan.Warnings[0], "process inspection is available") {
		t.Fatalf("deferred plan should include retry warning: %+v", plan.Warnings)
	}
}

func TestNixContentionReason(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		expected string
		ok       bool
	}{
		{
			name:     "sqlite busy",
			output:   "error: SQLite database '/nix/var/nix/db/db.sqlite' is busy",
			expected: "sqlite database busy",
			ok:       true,
		},
		{
			name:     "database locked",
			output:   "error: cannot open SQLite database: database is locked",
			expected: "sqlite database locked",
			ok:       true,
		},
		{
			name:     "big lock unavailable",
			output:   "error: opening lock file '/nix/var/nix/db/big-lock': Resource temporarily unavailable",
			expected: "nix store big-lock busy",
			ok:       true,
		},
		{
			name:   "unrelated error",
			output: "error: path '/nix/store/missing' does not exist",
			ok:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := nixContentionReason(tt.output)
			if ok != tt.ok || got != tt.expected {
				t.Fatalf("nixContentionReason(%q) = %q, %t; want %q, %t", tt.output, got, ok, tt.expected, tt.ok)
			}
		})
	}
}

func TestParseNixGenerations(t *testing.T) {
	output := `
   1   2026-04-01 10:00:00
   2   2026-04-02 10:00:00   (current)
`
	generations, err := parseNixGenerations(output, "user", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(generations) != 2 {
		t.Fatalf("got %d generations, want 2", len(generations))
	}
	if generations[1].Number != 2 || !generations[1].Current {
		t.Fatalf("current generation not parsed correctly: %+v", generations[1])
	}
}

func TestParseNixGCRoots(t *testing.T) {
	output := `
/proc/1234/fd/5 -> /nix/store/111-source
/nix/var/nix/profiles/per-user/jess/profile-42-link -> /nix/store/222-home-manager-generation
/nix/var/nix/gcroots/auto/abc -> /nix/store/333-tool
/Users/jess/git/kernel/result -> /nix/store/444-linux-kernel
/proc/1234/fd/5 -> /nix/store/111-source
`

	roots := parseNixGCRoots(output)
	if len(roots) != 4 {
		t.Fatalf("got %d roots, want 4: %#v", len(roots), roots)
	}

	classes := map[string]int{}
	active := 0
	for _, root := range roots {
		classes[root.Class]++
		if root.Active {
			active++
		}
	}

	if classes["process_root"] != 1 {
		t.Fatalf("expected one process root, classes=%v", classes)
	}
	if classes["profile_root"] != 1 {
		t.Fatalf("expected one profile root, classes=%v", classes)
	}
	if classes["auto_gcroot"] != 1 {
		t.Fatalf("expected one auto gcroot, classes=%v", classes)
	}
	if classes["workspace_result"] != 1 {
		t.Fatalf("expected one workspace result root, classes=%v", classes)
	}
	if active != 1 {
		t.Fatalf("expected one active root, got %d", active)
	}
}

func TestNixGCRootTargetsAreProtectedAndLimited(t *testing.T) {
	roots := []nixGCRoot{
		{
			Root:      "/proc/1234/fd/5",
			StorePath: "/nix/store/111-source",
			Class:     "process_root",
			Active:    true,
		},
		{
			Root:      "/Users/jess/git/kernel/result",
			StorePath: "/nix/store/444-linux-kernel",
			Class:     "workspace_result",
		},
	}

	targets := nixGCRootTargets(roots, 1)
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(targets))
	}
	if targets[0].Action != "review_gc_root" || !targets[0].Protected {
		t.Fatalf("GC root target should be protected review-only: %+v", targets[0])
	}
	if !targets[0].Active {
		t.Fatalf("process GC root should be marked active: %+v", targets[0])
	}
}

func TestNixGenerationTargetsPreserveCurrentAndMinimum(t *testing.T) {
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	generations := []nixGeneration{
		{Number: 1, CreatedAt: now.Add(-30 * 24 * time.Hour), Scope: "user"},
		{Number: 2, CreatedAt: now.Add(-20 * 24 * time.Hour), Scope: "user"},
		{Number: 3, CreatedAt: now.Add(-10 * 24 * time.Hour), Scope: "user"},
		{Number: 4, CreatedAt: now.Add(-5 * 24 * time.Hour), Scope: "user", Current: true},
		{Number: 5, CreatedAt: now.Add(-1 * 24 * time.Hour), Scope: "user"},
	}

	targets := nixGenerationTargets(generations, now, 3, 7*24*time.Hour)
	actions := map[string]string{}
	protected := map[string]bool{}
	for _, target := range targets {
		actions[target.Version] = target.Action
		protected[target.Version] = target.Protected
	}

	if actions["1"] != "delete_generation" || protected["1"] {
		t.Fatalf("expected generation 1 to be a deletion candidate, actions=%v protected=%v", actions, protected)
	}
	if actions["2"] != "delete_generation" || protected["2"] {
		t.Fatalf("expected generation 2 to be a deletion candidate, actions=%v protected=%v", actions, protected)
	}
	for _, target := range targets {
		if target.Version == "1" {
			if target.Tier != CleanupTierWarm || target.Reclaim != CleanupReclaimDeferred {
				t.Fatalf("generation 1 policy = tier %q reclaim %q, want warm/deferred", target.Tier, target.Reclaim)
			}
			if target.HostReclaimsSpace == nil || *target.HostReclaimsSpace {
				t.Fatalf("generation deletion should not directly reclaim host space: %+v", target)
			}
		}
	}
	for _, generation := range []string{"3", "4", "5"} {
		if actions[generation] != "keep_generation" || !protected[generation] {
			t.Fatalf("expected generation %s to be protected, actions=%v protected=%v", generation, actions, protected)
		}
	}
}
