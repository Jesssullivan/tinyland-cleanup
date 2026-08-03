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

func TestNixExternalAuthorityPlanAndApply(t *testing.T) {
	t.Setenv("TINYLAND_CLEANUP_EXTERNAL_HELPER", "1")
	t.Setenv("TINYLAND_CLEANUP_EXTERNAL_OUTCOME", CleanupOutcomeCompleted)
	// An empty PATH proves external mode does not discover or call the builtin
	// nix-collect-garbage/nix-store implementation.
	t.Setenv("PATH", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.Nix.GCAuthority = config.NixGCAuthorityExternal
	cfg.Nix.ExternalArgv = trustedExternalTestArgv(t)

	plugin := NewNixPlugin()
	plugin.freeDiskSpace = func(string) (uint64, error) { return 1024, nil }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	plan := plugin.PlanCleanup(context.Background(), LevelCritical, cfg, logger)
	if !plan.WouldRun || plan.Outcome != CleanupOutcomeCompleted || !nixExternalReceiptDigestPattern.MatchString(plan.ReceiptDigest) {
		t.Fatalf("unexpected external plan: %#v", plan)
	}
	result := plugin.Cleanup(context.Background(), LevelCritical, cfg, logger)
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	if result.Outcome != CleanupOutcomeCompleted || !nixExternalReceiptDigestPattern.MatchString(result.ReceiptDigest) {
		t.Fatalf("unexpected external result: %#v", result)
	}
	if result.ReceiptDigest == plan.ReceiptDigest {
		t.Fatal("plan and apply facts must produce different canonical receipt digests")
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
	externalArgv := trustedExternalTestArgv(t)
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
			cfg.Nix.ExternalArgv = append([]string(nil), externalArgv...)
			result := NewNixPlugin().Cleanup(context.Background(), LevelCritical, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if (result.Error != nil) != test.wantError {
				t.Fatalf("outcome %q error = %v, wantError=%t", test.outcome, result.Error, test.wantError)
			}
			if result.Outcome != test.outcome || !nixExternalReceiptDigestPattern.MatchString(result.ReceiptDigest) {
				t.Fatalf("unexpected typed outcome result: %#v", result)
			}
			if test.outcome == CleanupOutcomeDeferred && result.RetryAfterSeconds != 60 {
				t.Fatalf("deferred retry = %d, want 60", result.RetryAfterSeconds)
			}
		})
	}
}

func TestNixExternalAuthorityRejectsInvalidOrOversizedResponse(t *testing.T) {
	externalArgv := trustedExternalTestArgv(t)
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
			cfg.Nix.ExternalArgv = append([]string(nil), externalArgv...)
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
	marker := filepath.Join(t.TempDir(), "shell-expanded")
	cfg := config.DefaultConfig()
	cfg.Nix.GCAuthority = config.NixGCAuthorityExternal
	cfg.Nix.ExternalArgv = trustedExternalTestArgv(t, "$(touch "+marker+")")
	result := NewNixPlugin().Cleanup(context.Background(), LevelWarning, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("argv was interpreted by a shell: %v", err)
	}
}

func TestNixExternalAuthorityRejectsReplaceableOrSymlinkExecutable(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(dir, "controller")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := validateTrustedExternalExecutable(executable); err == nil {
		t.Fatal("same-uid replaceable external authority must be rejected")
	}
	link := filepath.Join(dir, "controller-link")
	if err := os.Symlink(executable, link); err != nil {
		t.Fatal(err)
	}
	if err := validateTrustedExternalExecutable(link); err == nil {
		t.Fatal("symlink external authority must be rejected")
	}
}

func TestNixExternalAuthorityReceiptBindsEnvelopeAndOperation(t *testing.T) {
	response := makeTestExternalResponse("plan", LevelCritical.String(), CleanupOutcomeCompleted)
	if err := validateExternalNixResponse(response, "plan", LevelCritical.String()); err != nil {
		t.Fatal(err)
	}
	response.ReceiptDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := validateExternalNixResponse(response, "plan", LevelCritical.String()); err == nil {
		t.Fatal("unbound receipt digest must be rejected")
	}
	response = makeTestExternalResponse("plan", LevelCritical.String(), CleanupOutcomeCompleted)
	response.BytesFreed = 1
	response.Receipt.BytesFreed = 1
	response.ReceiptDigest, _ = nixExternalReceiptDigest(response.Receipt)
	if err := validateExternalNixResponse(response, "plan", LevelCritical.String()); err == nil {
		t.Fatal("plan response must not claim applied work")
	}
	response = makeTestExternalResponse("apply", LevelCritical.String(), CleanupOutcomeCompleted)
	response.BytesFreed = nixExternalMaxReportedBytes + 1
	response.Receipt.BytesFreed = response.BytesFreed
	response.ReceiptDigest, _ = nixExternalReceiptDigest(response.Receipt)
	if err := validateExternalNixResponse(response, "apply", LevelCritical.String()); err == nil {
		t.Fatal("out-of-bounds numeric response must be rejected")
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
	response := makeTestExternalResponse(request.Operation, request.Level, outcome)
	if outcome == CleanupOutcomeCompleted && request.Operation == "apply" {
		response.BytesFreed = 4096
		response.ItemsCleaned = 2
	}
	if outcome == CleanupOutcomeDeferred {
		response.RetryAfterSeconds = 60
	}
	response.Receipt = nixExternalReceipt{
		Schema:              nixExternalReceiptSchema,
		ProtocolVersion:     response.ProtocolVersion,
		Operation:           response.Operation,
		Level:               response.Level,
		Outcome:             response.Outcome,
		Summary:             response.Summary,
		EstimatedBytesFreed: response.EstimatedBytesFreed,
		BytesFreed:          response.BytesFreed,
		ItemsCleaned:        response.ItemsCleaned,
		RetryAfterSeconds:   response.RetryAfterSeconds,
	}
	response.ReceiptDigest, _ = nixExternalReceiptDigest(response.Receipt)
	if digest := os.Getenv("TINYLAND_CLEANUP_EXTERNAL_DIGEST"); digest != "" {
		response.ReceiptDigest = digest
	}
	if err := json.NewEncoder(os.Stdout).Encode(response); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func makeTestExternalResponse(operation string, level string, outcome string) nixExternalResponse {
	response := nixExternalResponse{
		ProtocolVersion:     nixExternalProtocolVersion,
		Operation:           operation,
		Level:               level,
		Outcome:             outcome,
		Summary:             "test authority response",
		EstimatedBytesFreed: 8192,
	}
	response.Receipt = nixExternalReceipt{
		Schema:              nixExternalReceiptSchema,
		ProtocolVersion:     response.ProtocolVersion,
		Operation:           response.Operation,
		Level:               response.Level,
		Outcome:             response.Outcome,
		Summary:             response.Summary,
		EstimatedBytesFreed: response.EstimatedBytesFreed,
	}
	response.ReceiptDigest, _ = nixExternalReceiptDigest(response.Receipt)
	return response
}

func trustedExternalTestArgv(t *testing.T, extra ...string) []string {
	t.Helper()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{"/usr/bin/env", "/bin/env"} {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil || resolved != candidate {
			continue
		}
		if err := validateTrustedExternalExecutable(candidate); err != nil {
			continue
		}
		argv := []string{candidate, testBinary, "-test.run=^TestNixExternalAuthorityHelper$"}
		return append(argv, extra...)
	}
	t.Skip("no root-owned immutable env executable is available for external-authority protocol test")
	return nil
}
