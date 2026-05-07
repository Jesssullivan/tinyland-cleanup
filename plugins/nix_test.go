package plugins

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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

func TestNixDryRunHasNoGCWork(t *testing.T) {
	p := NewNixPlugin()

	tests := []struct {
		name     string
		output   string
		expected bool
	}{
		{"zero store paths", "0 store paths deleted, 0 B freed", true},
		{"would delete nothing", "would delete 0 store paths\nwould free 0 B", true},
		{"empty output", "", false},
		{"store paths available", "would delete 2 store paths\nwould free 4.0 MiB", false},
		{"bytes available", "1.0 GiB would be freed", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.nixDryRunHasNoGCWork(tt.output); got != tt.expected {
				t.Fatalf("nixDryRunHasNoGCWork(%q) = %t, want %t", tt.output, got, tt.expected)
			}
		})
	}
}

func TestNixCleanupFailsClosedWhenDryRunPreflightFails(t *testing.T) {
	binDir := t.TempDir()
	callsPath := filepath.Join(t.TempDir(), "nix-gc-calls")
	fakeGC := filepath.Join(binDir, "nix-collect-garbage")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$NIX_GC_CALLS"
if [ "$1" = "--dry-run" ]; then
  printf '%s\n' "error: dry-run preflight failed" >&2
  exit 1
fi
printf '%s\n' "mutating GC should not run" >&2
exit 0
`
	if err := os.WriteFile(fakeGC, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("NIX_GC_CALLS", callsPath)

	cfg := config.DefaultConfig()
	cfg.Nix.SkipWhenDaemonBusy = false

	p := NewNixPlugin()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	result := p.Cleanup(context.Background(), LevelWarning, cfg, logger)
	if result.Error == nil || !strings.Contains(result.Error.Error(), "preflight failed") {
		t.Fatalf("expected dry-run preflight failure, got %+v", result)
	}
	if result.ItemsCleaned != 0 || result.BytesFreed != 0 || result.CommandBytesFreed != 0 {
		t.Fatalf("failed preflight should not report cleanup: %+v", result)
	}

	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(calls), "--dry-run\n"; got != want {
		t.Fatalf("unexpected nix-collect-garbage calls %q, want %q", got, want)
	}
}

func TestNixCollectGarbageReportsHostDeltaSeparately(t *testing.T) {
	binDir := t.TempDir()
	fakeGC := filepath.Join(binDir, "nix-collect-garbage")
	script := `#!/bin/sh
printf '%s\n' "would delete 1 store paths"
printf '%s\n' "16.0 MiB freed"
`
	if err := os.WriteFile(fakeGC, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := config.DefaultConfig().Nix
	cfg.HostMeasurePath = t.TempDir()
	p := NewNixPlugin()
	freeValues := []uint64{1000, 1256}
	p.freeDiskSpace = func(path string) (uint64, error) {
		if path != cfg.HostMeasurePath {
			t.Fatalf("expected measure path %q, got %q", cfg.HostMeasurePath, path)
		}
		if len(freeValues) == 0 {
			t.Fatal("free space measured more times than expected")
		}
		value := freeValues[0]
		freeValues = freeValues[1:]
		return value, nil
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	result := p.collectGarbage(context.Background(), LevelWarning, nil, cfg, logger)
	if result.Error != nil {
		t.Fatalf("collectGarbage returned error: %v", result.Error)
	}
	if result.CommandBytesFreed != 16*1024*1024 || result.BytesFreed != result.CommandBytesFreed {
		t.Fatalf("command bytes should remain separate command evidence: %+v", result)
	}
	if result.HostBytesFreed != 256 {
		t.Fatalf("expected measured host delta 256, got %+v", result)
	}
}

func TestNixCollectGarbageSkipsHostDeltaWhenMeasurementFails(t *testing.T) {
	binDir := t.TempDir()
	fakeGC := filepath.Join(binDir, "nix-collect-garbage")
	if err := os.WriteFile(fakeGC, []byte("#!/bin/sh\nprintf '%s\\n' '8.0 MiB freed'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := config.DefaultConfig().Nix
	cfg.HostMeasurePath = t.TempDir()
	p := NewNixPlugin()
	p.freeDiskSpace = func(string) (uint64, error) {
		return 0, errors.New("measurement unavailable")
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	result := p.collectGarbage(context.Background(), LevelWarning, nil, cfg, logger)
	if result.Error != nil {
		t.Fatalf("collectGarbage returned error: %v", result.Error)
	}
	if result.CommandBytesFreed != 8*1024*1024 {
		t.Fatalf("expected command bytes from Nix output, got %+v", result)
	}
	if result.HostBytesFreed != 0 {
		t.Fatalf("failed measurement should not invent host bytes: %+v", result)
	}
}

func TestNixHostMeasurePathDefaultsAndFallbacks(t *testing.T) {
	cfg := config.NixConfig{}
	if got := nixHostMeasurePath(cfg); got == "" {
		t.Fatal("expected non-empty default host measure path")
	}

	measurePath := t.TempDir()
	cfg.HostMeasurePath = measurePath
	if got := nixHostMeasurePath(cfg); got != measurePath {
		t.Fatalf("expected configured measure path %q, got %q", measurePath, got)
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

func TestDiscoverNixProfileLinkGenerations(t *testing.T) {
	profileDir := t.TempDir()
	for _, name := range []string{"home-manager-41-link", "home-manager-42-link"} {
		if err := os.Symlink("/nix/store/"+name, filepath.Join(profileDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("home-manager-42-link", filepath.Join(profileDir, "home-manager")); err != nil {
		t.Fatal(err)
	}

	generations, err := discoverNixProfileLinkGenerations(profileDir, "home-manager", "home-manager")
	if err != nil {
		t.Fatal(err)
	}
	if len(generations) != 2 {
		t.Fatalf("got %d generations, want 2: %+v", len(generations), generations)
	}
	if generations[0].Number != 41 || generations[0].Current {
		t.Fatalf("unexpected first generation: %+v", generations[0])
	}
	if generations[1].Number != 42 || !generations[1].Current {
		t.Fatalf("current Home Manager generation not detected: %+v", generations[1])
	}
	if generations[1].Scope != "home-manager" || generations[1].Profile != filepath.Join(profileDir, "home-manager-42-link") {
		t.Fatalf("generation metadata not populated: %+v", generations[1])
	}
}

func TestParseNixGCRoots(t *testing.T) {
	output := `
/proc/1234/fd/5 -> /nix/store/111-source
{nix-process:4321} -> /nix/store/888-active-derivation.drv
{lsof} -> /nix/store/555-open-library
/nix/var/nix/profiles/per-user/jess/profile-42-link -> /nix/store/222-home-manager-generation
/nix/var/nix/gcroots/auto/abc -> /nix/store/333-tool
/Users/jess/git/kernel/.direnv/flake-profile-a5d5b61aa8a61b7d9d765e1daf971a9a578f1cfa -> /nix/store/666-kernel-env
/Users/jess/git/kernel/result -> /nix/store/444-linux-kernel
/Users/jess/.cache/nix/flake-registry.json -> /nix/store/777-flake-registry.json
/proc/1234/fd/5 -> /nix/store/111-source
`

	roots := parseNixGCRoots(output)
	if len(roots) != 8 {
		t.Fatalf("got %d roots, want 8: %#v", len(roots), roots)
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
	if classes["nix_process_root"] != 1 {
		t.Fatalf("expected one nix process root, classes=%v", classes)
	}
	if classes["lsof_root"] != 1 {
		t.Fatalf("expected one lsof root, classes=%v", classes)
	}
	if classes["profile_root"] != 1 {
		t.Fatalf("expected one profile root, classes=%v", classes)
	}
	if classes["direnv_root"] != 1 {
		t.Fatalf("expected one direnv root, classes=%v", classes)
	}
	if classes["auto_gcroot"] != 1 {
		t.Fatalf("expected one auto gcroot, classes=%v", classes)
	}
	if classes["workspace_result"] != 1 {
		t.Fatalf("expected one workspace result root, classes=%v", classes)
	}
	if classes["nix_cache_root"] != 1 {
		t.Fatalf("expected one nix cache root, classes=%v", classes)
	}
	if active != 3 {
		t.Fatalf("expected three active roots, got %d", active)
	}
	if roots[0].Class != "nix_process_root" || roots[1].Class != "process_root" || roots[2].Class != "lsof_root" || roots[3].Class != "direnv_root" {
		t.Fatalf("roots should be sorted by operator-actionable class, got %#v", roots)
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
	if targets[0].Action != "review_active_gc_root" || !targets[0].Protected {
		t.Fatalf("GC root target should be protected review-only: %+v", targets[0])
	}
	if !targets[0].Active {
		t.Fatalf("process GC root should be marked active: %+v", targets[0])
	}
}

func TestNixGCRootTargetsUseSpecificReviewActions(t *testing.T) {
	roots := []nixGCRoot{
		{
			Root:      "{lsof}",
			StorePath: "/nix/store/111-open-library",
			Class:     "lsof_root",
			Active:    true,
		},
		{
			Root:      "/tmp/dev-env-profile-1-link",
			StorePath: "/nix/store/222-dev-env",
			Class:     "temporary_root",
		},
		{
			Root:      "/Users/jess/git/kernel/.direnv/flake-profile-a5d5b61aa8a61b7d9d765e1daf971a9a578f1cfa",
			StorePath: "/nix/store/444-kernel-env",
			Class:     "direnv_root",
		},
		{
			Root:      "/Users/jess/git/kernel/result",
			StorePath: "/nix/store/333-kernel",
			Class:     "workspace_result",
		},
		{
			Root:      "/Users/jess/.cache/nix/flake-registry.json",
			StorePath: "/nix/store/555-flake-registry.json",
			Class:     "nix_cache_root",
		},
	}

	targets := nixGCRootTargets(roots, 5)
	actions := map[string]CleanupTarget{}
	for _, target := range targets {
		actions[target.Path] = target
	}

	temporary := actions["/tmp/dev-env-profile-1-link"]
	if temporary.Action != "review_temporary_gc_root" || temporary.Reclaim != CleanupReclaimDeferred || temporary.Version != "222-dev-env" {
		t.Fatalf("temporary root target should carry specific review action and store path evidence: %+v", temporary)
	}

	lsof := actions["{lsof}"]
	if lsof.Action != "review_lsof_gc_root" || lsof.Tier != CleanupTierDisruptive || lsof.Reclaim != CleanupReclaimNone || !lsof.Active {
		t.Fatalf("lsof root target should carry active process review policy: %+v", lsof)
	}

	direnv := actions["/Users/jess/git/kernel/.direnv/flake-profile-a5d5b61aa8a61b7d9d765e1daf971a9a578f1cfa"]
	if direnv.Action != "review_direnv_gc_root" || direnv.Tier != CleanupTierWarm || direnv.Reclaim != CleanupReclaimDeferred {
		t.Fatalf("direnv root target should carry warm deferred review policy: %+v", direnv)
	}

	workspace := actions["/Users/jess/git/kernel/result"]
	if workspace.Action != "review_workspace_result_root" || workspace.Tier != CleanupTierWarm || workspace.Reclaim != CleanupReclaimDeferred {
		t.Fatalf("workspace result target should carry warm deferred review policy: %+v", workspace)
	}

	nixCache := actions["/Users/jess/.cache/nix/flake-registry.json"]
	if nixCache.Action != "review_nix_cache_gc_root" || nixCache.Tier != CleanupTierWarm || nixCache.Reclaim != CleanupReclaimDeferred {
		t.Fatalf("nix cache root target should carry warm deferred review policy: %+v", nixCache)
	}
}

func TestNixReviewOnlyGenerationTargetsProtectsSelectedGenerations(t *testing.T) {
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	generations := []nixGeneration{
		{Number: 1, CreatedAt: now.Add(-30 * 24 * time.Hour), Scope: "home-manager"},
		{Number: 2, CreatedAt: now.Add(-1 * 24 * time.Hour), Scope: "home-manager", Current: true},
	}

	targets := nixReviewOnlyGenerationTargets(
		nixGenerationTargets(generations, now, 1, 7*24*time.Hour),
		"review_home_manager_generation",
		"outside retention policy, but Home Manager generation deletion requires an explicit profile workflow",
		CleanupTierWarm,
	)

	actions := map[string]CleanupTarget{}
	for _, target := range targets {
		actions[target.Version] = target
	}
	if actions["1"].Action != "review_home_manager_generation" || !actions["1"].Protected {
		t.Fatalf("old Home Manager generation should be review-only and protected: %+v", actions["1"])
	}
	if actions["1"].Tier != CleanupTierWarm || actions["1"].Reclaim != CleanupReclaimDeferred {
		t.Fatalf("old Home Manager generation policy mismatch: %+v", actions["1"])
	}
	if actions["2"].Action != "keep_generation" || !actions["2"].Protected {
		t.Fatalf("current Home Manager generation should remain kept: %+v", actions["2"])
	}
}

func TestNixHomeManagerGenerationTargetsRequireOptIn(t *testing.T) {
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	generations := []nixGeneration{
		{Number: 1, CreatedAt: now.Add(-30 * 24 * time.Hour), Scope: "home-manager"},
		{Number: 2, CreatedAt: now.Add(-1 * 24 * time.Hour), Scope: "home-manager", Current: true},
	}

	reviewTargets := nixHomeManagerGenerationTargets(
		nixGenerationTargets(generations, now, 1, 7*24*time.Hour),
		config.NixConfig{},
	)
	reviewActions := map[string]CleanupTarget{}
	for _, target := range reviewTargets {
		reviewActions[target.Version] = target
	}
	if reviewActions["1"].Action != "review_home_manager_generation" || !reviewActions["1"].Protected {
		t.Fatalf("Home Manager generation should be review-only without opt-in: %+v", reviewActions["1"])
	}

	deleteTargets := nixHomeManagerGenerationTargets(
		nixGenerationTargets(generations, now, 1, 7*24*time.Hour),
		config.NixConfig{DeleteHomeManagerGenerations: true},
	)
	deleteActions := map[string]CleanupTarget{}
	for _, target := range deleteTargets {
		deleteActions[target.Version] = target
	}
	if deleteActions["1"].Action != "delete_home_manager_generation" || deleteActions["1"].Protected {
		t.Fatalf("Home Manager generation should be deletable with opt-in: %+v", deleteActions["1"])
	}
	if deleteActions["2"].Action != "keep_generation" || !deleteActions["2"].Protected {
		t.Fatalf("current Home Manager generation should remain kept: %+v", deleteActions["2"])
	}
}

func TestNixPluginDeleteHomeManagerGenerationsByPolicyUsesRemoveGenerations(t *testing.T) {
	tempHome := t.TempDir()
	profileDir := filepath.Join(tempHome, ".local", "state", "nix", "profiles")
	if err := os.MkdirAll(profileDir, 0755); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	for _, generation := range []struct {
		name    string
		modTime time.Time
	}{
		{"home-manager-1-link", now.Add(-72 * time.Hour)},
		{"home-manager-2-link", now.Add(-48 * time.Hour)},
		{"home-manager-3-link", now.Add(-1 * time.Hour)},
	} {
		path := filepath.Join(profileDir, generation.name)
		if err := os.WriteFile(path, []byte(generation.name), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, generation.modTime, generation.modTime); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("home-manager-3-link", filepath.Join(profileDir, "home-manager")); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	argsLog := filepath.Join(t.TempDir(), "home-manager.args")
	fakeHomeManager := filepath.Join(binDir, "home-manager")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$TINYLAND_HOME_MANAGER_ARGS_LOG\"\n"
	if err := os.WriteFile(fakeHomeManager, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", tempHome)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TINYLAND_HOME_MANAGER_ARGS_LOG", argsLog)

	p := NewNixPlugin()
	result := p.deleteHomeManagerGenerationsByPolicy(
		context.Background(),
		LevelModerate,
		config.NixConfig{
			DeleteHomeManagerGenerations:          true,
			MinHomeManagerGenerations:             1,
			DeleteGenerationsOlderThan:            "24h",
			HomeManagerDeleteGenerationsOlderThan: "24h",
			MaxGCDuration:                         "5s",
		},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if result.Error != nil {
		t.Fatalf("deleteHomeManagerGenerationsByPolicy returned error: %v", result.Error)
	}
	if result.ItemsCleaned != 2 {
		t.Fatalf("deleted %d generations, want 2", result.ItemsCleaned)
	}

	rawArgs, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(rawArgs)), "remove-generations 1 2"; got != want {
		t.Fatalf("home-manager args = %q, want %q", got, want)
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
