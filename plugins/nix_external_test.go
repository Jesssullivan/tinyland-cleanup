package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

const testExternalReceiptDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestNixExternalAuthorityPlanAndApply(t *testing.T) {
	t.Setenv("TINYLAND_CLEANUP_EXTERNAL_HELPER", "1")
	t.Setenv("TINYLAND_CLEANUP_EXTERNAL_OUTCOME", CleanupOutcomeCompleted)
	// An empty PATH proves external mode does not discover or call the builtin
	// nix-collect-garbage/nix-store implementation.
	t.Setenv("PATH", t.TempDir())
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Nix.GCAuthority = config.NixGCAuthorityExternal
	cfg.Nix.ExternalArgv = []string{executable, "-test.run=^TestNixExternalAuthorityHelper$"}

	plugin := NewNixPlugin()
	plugin.freeDiskSpace = func(string) (uint64, error) { return 1024, nil }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	plan := plugin.PlanCleanup(context.Background(), LevelCritical, cfg, logger)
	if !plan.WouldRun || plan.Outcome != CleanupOutcomeCompleted || plan.ReceiptDigest != testExternalReceiptDigest {
		t.Fatalf("unexpected external plan: %#v", plan)
	}
	result := plugin.Cleanup(context.Background(), LevelCritical, cfg, logger)
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	if result.Outcome != CleanupOutcomeCompleted || result.ReceiptDigest != testExternalReceiptDigest {
		t.Fatalf("unexpected external result: %#v", result)
	}
	if result.BytesFreed != 4096 || result.ItemsCleaned != 2 {
		t.Fatalf("unexpected external yield: %#v", result)
	}
}

func TestNixExternalAuthorityRejectsRelativeExecutable(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Nix.GCAuthority = config.NixGCAuthorityExternal
	cfg.Nix.ExternalArgv = []string{"controller"}
	result := NewNixPlugin().Cleanup(context.Background(), LevelCritical, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if result.Error == nil {
		t.Fatal("relative external executable must fail closed")
	}
}

func TestNixExternalAuthorityTypedOutcomes(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		outcome   string
		wantError bool
	}{
		{outcome: CleanupOutcomeCompleted},
		{outcome: CleanupOutcomeDeferred},
		{outcome: CleanupOutcomeRefused, wantError: true},
		{outcome: CleanupOutcomeNoOp},
	} {
		t.Run(test.outcome, func(t *testing.T) {
			t.Setenv("TINYLAND_CLEANUP_EXTERNAL_HELPER", "1")
			t.Setenv("TINYLAND_CLEANUP_EXTERNAL_OUTCOME", test.outcome)
			cfg := config.DefaultConfig()
			cfg.Nix.GCAuthority = config.NixGCAuthorityExternal
			cfg.Nix.ExternalArgv = []string{executable, "-test.run=^TestNixExternalAuthorityHelper$"}
			result := NewNixPlugin().Cleanup(context.Background(), LevelCritical, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if (result.Error != nil) != test.wantError {
				t.Fatalf("outcome %q error = %v, wantError=%t", test.outcome, result.Error, test.wantError)
			}
			if result.Outcome != test.outcome || result.ReceiptDigest != testExternalReceiptDigest {
				t.Fatalf("unexpected typed outcome result: %#v", result)
			}
			if test.outcome == CleanupOutcomeDeferred && result.RetryAfterSeconds != 60 {
				t.Fatalf("deferred retry = %d, want 60", result.RetryAfterSeconds)
			}
		})
	}
}

func TestNixExternalAuthorityRejectsInvalidOrOversizedResponse(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		digest    string
		oversized string
	}{
		{name: "invalid digest", digest: "sha256:not-a-digest"},
		{name: "oversized", oversized: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("TINYLAND_CLEANUP_EXTERNAL_HELPER", "1")
			t.Setenv("TINYLAND_CLEANUP_EXTERNAL_OUTCOME", CleanupOutcomeNoOp)
			t.Setenv("TINYLAND_CLEANUP_EXTERNAL_DIGEST", test.digest)
			t.Setenv("TINYLAND_CLEANUP_EXTERNAL_OVERSIZED", test.oversized)
			cfg := config.DefaultConfig()
			cfg.Nix.GCAuthority = config.NixGCAuthorityExternal
			cfg.Nix.ExternalArgv = []string{executable, "-test.run=^TestNixExternalAuthorityHelper$"}
			result := NewNixPlugin().Cleanup(context.Background(), LevelCritical, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if result.Error == nil {
				t.Fatal("invalid external response must fail closed")
			}
		})
	}
}

func TestNixExternalAuthorityUsesArgvWithoutShell(t *testing.T) {
	t.Setenv("TINYLAND_CLEANUP_EXTERNAL_HELPER", "1")
	t.Setenv("TINYLAND_CLEANUP_EXTERNAL_OUTCOME", CleanupOutcomeNoOp)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "shell-expanded")
	cfg := config.DefaultConfig()
	cfg.Nix.GCAuthority = config.NixGCAuthorityExternal
	cfg.Nix.ExternalArgv = []string{executable, "-test.run=^TestNixExternalAuthorityHelper$", "$(touch " + marker + ")"}
	result := NewNixPlugin().Cleanup(context.Background(), LevelWarning, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("argv was interpreted by a shell: %v", err)
	}
}

func TestNixExternalAuthorityHelper(t *testing.T) {
	if os.Getenv("TINYLAND_CLEANUP_EXTERNAL_HELPER") != "1" {
		return
	}
	if os.Getenv("TINYLAND_CLEANUP_EXTERNAL_OVERSIZED") == "1" {
		fmt.Fprint(os.Stdout, strings.Repeat("x", nixExternalMaxOutputBytes+1))
		os.Exit(0)
	}
	var request nixExternalRequest
	if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	outcome := os.Getenv("TINYLAND_CLEANUP_EXTERNAL_OUTCOME")
	digest := os.Getenv("TINYLAND_CLEANUP_EXTERNAL_DIGEST")
	if digest == "" {
		digest = testExternalReceiptDigest
	}
	response := nixExternalResponse{
		ProtocolVersion:     nixExternalProtocolVersion,
		Operation:           request.Operation,
		Outcome:             outcome,
		Summary:             "test authority response",
		ReceiptDigest:       digest,
		EstimatedBytesFreed: 8192,
	}
	if outcome == CleanupOutcomeCompleted && request.Operation == "apply" {
		response.BytesFreed = 4096
		response.ItemsCleaned = 2
	}
	if outcome == CleanupOutcomeDeferred {
		response.RetryAfterSeconds = 60
	}
	if err := json.NewEncoder(os.Stdout).Encode(response); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}
