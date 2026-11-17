package plugins

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// critical_max_busy_deferrals releases a critical pass after N consecutive
// deferrals (TIN-8526), sooner than max_busy_deferrals, and the pass still
// runs a dry-run and a bounded GC.
func TestNixCriticalBusyDeferralCap(t *testing.T) {
	p, cfg, calls, now := busyCleanupFixture(t)
	cfg.Nix.MaxBusyDeferrals = 72
	cfg.Nix.MaxBusyDeferralDuration = "6h"
	cfg.Nix.CriticalMaxBusyDeferrals = 3
	p.activeWork = func(context.Context) ([]string, nixProcessInspection, error) {
		return []string{"nix develop", "nix-daemon worker"}, nixProcessInspection{}, nil
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for i := 1; i <= 3; i++ {
		p.Cleanup(context.Background(), LevelCritical, cfg, logger)
		if got := gcCalls(t, calls); len(got) != 0 {
			t.Fatalf("deferral %d must not collect: %q", i, got)
		}
		*now = now.Add(5 * time.Minute)
	}
	plan := p.PlanCleanup(context.Background(), LevelCritical, cfg, logger)
	if plan.Metadata["busy_deferral_released_by"] != "critical_max_busy_deferrals=3" || !plan.WouldRun || plan.Metadata["critical_max_busy_deferrals"] != "3" {
		t.Fatalf("plan must report the critical release: %+v %q", plan.Metadata, plan.SkipReason)
	}
	before := len(gcCalls(t, calls))
	p.Cleanup(context.Background(), LevelCritical, cfg, logger)
	got := gcCalls(t, calls)
	if len(got) != before+2 || !strings.HasPrefix(got[len(got)-1], "call:--max-freed") {
		t.Fatalf("the critical pass after the cap runs a dry-run and a GC: %q", got)
	}
	if st := loadNixGCState(cfg.Nix.GCStateFile); st.DeferredPasses != 0 {
		t.Fatalf("an attempted pass resets the deferral run: %+v", st)
	}
}

// The cap applies only at critical level: below it the general bounds hold.
func TestNixCriticalBusyDeferralCapIgnoredBelowCritical(t *testing.T) {
	p, cfg, calls, now := busyCleanupFixture(t)
	cfg.Nix.MaxBusyDeferrals = 72
	cfg.Nix.MaxBusyDeferralDuration = "6h"
	cfg.Nix.CriticalMaxBusyDeferrals = 3
	p.activeWork = func(context.Context) ([]string, nixProcessInspection, error) {
		return []string{"nix build"}, nixProcessInspection{}, nil
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for i := 1; i <= 6; i++ {
		p.Cleanup(context.Background(), LevelAggressive, cfg, logger)
		*now = now.Add(5 * time.Minute)
	}
	if got := gcCalls(t, calls); len(got) != 0 {
		t.Fatalf("an aggressive pass must not use the critical cap: %q", got)
	}
	plan := p.PlanCleanup(context.Background(), LevelAggressive, cfg, logger)
	if plan.WouldRun || plan.SkipReason != "nix_daemon_busy" {
		t.Fatalf("an aggressive plan must still defer: %+v %q", plan.Metadata, plan.SkipReason)
	}
}

// 0, the default, leaves critical passes under the general bounds only.
func TestNixCriticalBusyDeferralCapOffByDefault(t *testing.T) {
	p, cfg, calls, now := busyCleanupFixture(t)
	if cfg.Nix.CriticalMaxBusyDeferrals != 0 {
		t.Fatalf("critical_max_busy_deferrals default = %d, want 0", cfg.Nix.CriticalMaxBusyDeferrals)
	}
	cfg.Nix.MaxBusyDeferrals = 72
	cfg.Nix.MaxBusyDeferralDuration = "6h"
	p.activeWork = func(context.Context) ([]string, nixProcessInspection, error) {
		return []string{"nix build"}, nixProcessInspection{}, nil
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for i := 1; i <= 6; i++ {
		p.Cleanup(context.Background(), LevelCritical, cfg, logger)
		*now = now.Add(5 * time.Minute)
	}
	if got := gcCalls(t, calls); len(got) != 0 {
		t.Fatalf("with the cap off, six critical deferrals must not collect: %q", got)
	}
}
