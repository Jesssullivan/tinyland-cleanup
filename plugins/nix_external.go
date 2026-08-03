package plugins

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

const (
	nixExternalProtocolVersion        = 1
	nixExternalReceiptSchema          = "tinyland.cleanup.nix-external-receipt.v1"
	nixExternalMaxOutputBytes         = 64 * 1024
	nixExternalMaxSummaryBytes        = 2048
	nixExternalMaxReportedBytes int64 = 1 << 60
	nixExternalMaxItems               = 1_000_000_000
	nixExternalMaxRetrySeconds  int64 = 7 * 24 * 60 * 60
)

var nixExternalReceiptDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type nixExternalRequest struct {
	ProtocolVersion int    `json:"protocol_version"`
	Operation       string `json:"operation"`
	Level           string `json:"level"`
}

type nixExternalResponse struct {
	ProtocolVersion     int                `json:"protocol_version"`
	Operation           string             `json:"operation"`
	Level               string             `json:"level"`
	Outcome             string             `json:"outcome"`
	Summary             string             `json:"summary"`
	Receipt             nixExternalReceipt `json:"receipt"`
	ReceiptDigest       string             `json:"receipt_digest"`
	EstimatedBytesFreed int64              `json:"estimated_bytes_freed,omitempty"`
	BytesFreed          int64              `json:"bytes_freed,omitempty"`
	ItemsCleaned        int                `json:"items_cleaned,omitempty"`
	RetryAfterSeconds   int64              `json:"retry_after_seconds,omitempty"`
}

// nixExternalReceipt is the complete, bounded proof object returned by the
// external authority. Its field order is the canonical JSON order used by
// nixExternalReceiptDigest; the response envelope must repeat the same facts.
type nixExternalReceipt struct {
	Schema              string `json:"schema"`
	ProtocolVersion     int    `json:"protocol_version"`
	Operation           string `json:"operation"`
	Level               string `json:"level"`
	Outcome             string `json:"outcome"`
	Summary             string `json:"summary"`
	EstimatedBytesFreed int64  `json:"estimated_bytes_freed"`
	BytesFreed          int64  `json:"bytes_freed"`
	ItemsCleaned        int    `json:"items_cleaned"`
	RetryAfterSeconds   int64  `json:"retry_after_seconds"`
}

type boundedCommandBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *boundedCommandBuffer) Write(data []byte) (int, error) {
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		return 0, fmt.Errorf("command output exceeds %d bytes", b.limit)
	}
	if len(data) > remaining {
		_, _ = b.buffer.Write(data[:remaining])
		return remaining, fmt.Errorf("command output exceeds %d bytes", b.limit)
	}
	return b.buffer.Write(data)
}

func (b *boundedCommandBuffer) String() string {
	return b.buffer.String()
}

func (p *NixPlugin) planExternalGC(ctx context.Context, level CleanupLevel, cfg config.NixConfig, logger *slog.Logger) CleanupPlan {
	plan := CleanupPlan{
		Plugin:   p.Name(),
		Level:    level.String(),
		Summary:  "External Nix GC authority plan",
		WouldRun: false,
		Metadata: map[string]string{
			"gc_authority":     config.NixGCAuthorityExternal,
			"protocol_version": "1",
		},
		Steps: []string{
			"Send a bounded versioned JSON plan request to the configured absolute argv",
			"Require a typed outcome and sha256 receipt digest",
			"Leave generation deletion, store GC, locking, and receipt custody to the external authority",
		},
	}

	response, stderr, err := invokeExternalNixAuthority(ctx, "plan", level, cfg)
	if err != nil {
		plan.SkipReason = "external_authority_error"
		plan.Warnings = append(plan.Warnings, err.Error())
		if stderr != "" {
			plan.Warnings = append(plan.Warnings, "external authority stderr: "+stderr)
		}
		return plan
	}

	plan.Outcome = response.Outcome
	plan.ReceiptDigest = response.ReceiptDigest
	plan.RetryAfterSeconds = response.RetryAfterSeconds
	plan.EstimatedBytesFreed = response.EstimatedBytesFreed
	if response.Summary != "" {
		plan.Summary = response.Summary
	}
	plan.Metadata["external_outcome"] = response.Outcome
	plan.Metadata["receipt_digest"] = response.ReceiptDigest

	switch response.Outcome {
	case CleanupOutcomeCompleted:
		plan.WouldRun = true
	case CleanupOutcomeNoOp:
		plan.SkipReason = "external_no_op"
	case CleanupOutcomeDeferred:
		plan.SkipReason = "external_deferred"
	case CleanupOutcomeRefused:
		plan.SkipReason = "external_refused"
	}
	logger.Debug("external Nix GC plan completed", "outcome", response.Outcome, "receipt_digest", response.ReceiptDigest)
	return plan
}

func (p *NixPlugin) cleanupExternalGC(ctx context.Context, level CleanupLevel, cfg config.NixConfig, logger *slog.Logger) CleanupResult {
	result := CleanupResult{Plugin: p.Name(), Level: level}
	measurePath := nixHostMeasurePath(cfg)
	before, beforeOK := p.measureFreeDiskSpace(measurePath, logger)

	response, stderr, err := invokeExternalNixAuthority(ctx, "apply", level, cfg)
	if err != nil {
		result.Error = err
		if stderr != "" {
			result.Error = fmt.Errorf("%w: stderr: %s", err, stderr)
		}
		return result
	}

	result.Outcome = response.Outcome
	result.ReceiptDigest = response.ReceiptDigest
	result.RetryAfterSeconds = response.RetryAfterSeconds
	result.EstimatedBytesFreed = response.EstimatedBytesFreed
	result.CommandBytesFreed = response.BytesFreed
	result.BytesFreed = response.BytesFreed
	result.ItemsCleaned = response.ItemsCleaned
	if beforeOK {
		result.HostBytesFreed = p.measuredHostDelta(before, measurePath, logger)
	}
	if response.Outcome == CleanupOutcomeRefused {
		result.Error = fmt.Errorf("external Nix GC authority refused apply: %s", response.Summary)
	}
	logger.Debug("external Nix GC apply completed", "outcome", response.Outcome, "receipt_digest", response.ReceiptDigest)
	return result
}

func invokeExternalNixAuthority(ctx context.Context, operation string, level CleanupLevel, cfg config.NixConfig) (nixExternalResponse, string, error) {
	var response nixExternalResponse
	if err := validateExternalNixArgv(cfg.ExternalArgv); err != nil {
		return response, "", err
	}
	if err := validateTrustedExternalExecutable(cfg.ExternalArgv[0]); err != nil {
		return response, "", err
	}
	request := nixExternalRequest{
		ProtocolVersion: nixExternalProtocolVersion,
		Operation:       operation,
		Level:           level.String(),
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return response, "", fmt.Errorf("encode external Nix GC request: %w", err)
	}
	requestJSON = append(requestJSON, '\n')

	commandCtx, cancel := context.WithTimeout(ctx, nixCommandTimeout(cfg))
	defer cancel()
	cmd := exec.CommandContext(commandCtx, cfg.ExternalArgv[0], cfg.ExternalArgv[1:]...)
	configureExternalCommandTermination(cmd)
	cmd.Stdin = bytes.NewReader(requestJSON)
	stdout := &boundedCommandBuffer{limit: nixExternalMaxOutputBytes}
	stderr := &boundedCommandBuffer{limit: nixExternalMaxOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
			return response, strings.TrimSpace(stderr.String()), fmt.Errorf("external Nix GC %s exceeded %s", operation, nixCommandTimeout(cfg))
		}
		return response, strings.TrimSpace(stderr.String()), fmt.Errorf("external Nix GC %s failed: %w", operation, err)
	}

	decoder := json.NewDecoder(strings.NewReader(stdout.String()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return response, strings.TrimSpace(stderr.String()), fmt.Errorf("decode external Nix GC %s response: %w", operation, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return response, strings.TrimSpace(stderr.String()), fmt.Errorf("external Nix GC %s response contains trailing data", operation)
	}
	if err := validateExternalNixResponse(response, operation, level.String()); err != nil {
		return response, strings.TrimSpace(stderr.String()), err
	}
	return response, strings.TrimSpace(stderr.String()), nil
}

func validateExternalNixArgv(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("external Nix GC argv is empty")
	}
	if len(argv) > 64 || !filepath.IsAbs(argv[0]) || filepath.Clean(argv[0]) != argv[0] {
		return fmt.Errorf("external Nix GC executable must be a clean absolute path and argv must contain at most 64 entries")
	}
	for index, value := range argv {
		if value == "" || len(value) > 4096 || strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("external Nix GC argv[%d] is invalid", index)
		}
	}
	return nil
}

func validateTrustedExternalExecutable(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve external Nix GC executable: %w", err)
	}
	if resolved != path {
		return fmt.Errorf("external Nix GC executable must not contain symlink components: %s resolves to %s", path, resolved)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat external Nix GC executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("external Nix GC executable must be an executable regular file")
	}
	if info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("external Nix GC executable must not be group- or world-writable")
	}
	if err := validateExternalPathOwner(info, path); err != nil {
		return err
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		dirInfo, err := os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("stat external Nix GC executable parent %s: %w", dir, err)
		}
		if !dirInfo.IsDir() {
			return fmt.Errorf("external Nix GC executable parent is not a directory: %s", dir)
		}
		// A trusted sticky directory (notably /tmp and /nix/store) prevents an
		// unprivileged peer from replacing an already-created child owned by a
		// different uid. Every descendant and the executable itself still has
		// to be trusted-owned and non-writable.
		isTrustedStickyBoundary := dirInfo.Mode()&os.ModeSticky != 0
		if !isTrustedStickyBoundary && dirInfo.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("external Nix GC executable parent must not be group- or world-writable: %s", dir)
		}
		if err := validateExternalPathOwner(dirInfo, dir); err != nil {
			return err
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	return nil
}

func validateExternalNixResponse(response nixExternalResponse, operation string, level string) error {
	if response.ProtocolVersion != nixExternalProtocolVersion {
		return fmt.Errorf("external Nix GC protocol version %d is unsupported", response.ProtocolVersion)
	}
	if response.Operation != operation {
		return fmt.Errorf("external Nix GC response operation %q does not match %q", response.Operation, operation)
	}
	if response.Level != level {
		return fmt.Errorf("external Nix GC response level %q does not match %q", response.Level, level)
	}
	switch response.Outcome {
	case CleanupOutcomeCompleted, CleanupOutcomeDeferred, CleanupOutcomeRefused, CleanupOutcomeNoOp:
	default:
		return fmt.Errorf("external Nix GC response outcome %q is invalid", response.Outcome)
	}
	if len(response.Summary) > nixExternalMaxSummaryBytes ||
		response.EstimatedBytesFreed < 0 || response.EstimatedBytesFreed > nixExternalMaxReportedBytes ||
		response.BytesFreed < 0 || response.BytesFreed > nixExternalMaxReportedBytes ||
		response.ItemsCleaned < 0 || response.ItemsCleaned > nixExternalMaxItems ||
		response.RetryAfterSeconds < 0 || response.RetryAfterSeconds > nixExternalMaxRetrySeconds {
		return fmt.Errorf("external Nix GC response exceeds protocol bounds")
	}
	if response.Outcome == CleanupOutcomeDeferred && response.RetryAfterSeconds == 0 {
		return fmt.Errorf("external Nix GC deferred response requires retry_after_seconds")
	}
	if response.Outcome != CleanupOutcomeCompleted && (response.BytesFreed != 0 || response.ItemsCleaned != 0) {
		return fmt.Errorf("external Nix GC %s response must not report applied work", response.Outcome)
	}
	if operation == "plan" && (response.BytesFreed != 0 || response.ItemsCleaned != 0) {
		return fmt.Errorf("external Nix GC plan response must not report applied work")
	}
	if response.Outcome != CleanupOutcomeDeferred && response.RetryAfterSeconds != 0 {
		return fmt.Errorf("external Nix GC non-deferred response must not set retry_after_seconds")
	}
	if err := validateExternalNixReceipt(response); err != nil {
		return err
	}
	return nil
}

func validateExternalNixReceipt(response nixExternalResponse) error {
	receipt := response.Receipt
	if receipt.Schema != nixExternalReceiptSchema ||
		receipt.ProtocolVersion != response.ProtocolVersion ||
		receipt.Operation != response.Operation ||
		receipt.Level != response.Level ||
		receipt.Outcome != response.Outcome ||
		receipt.Summary != response.Summary ||
		receipt.EstimatedBytesFreed != response.EstimatedBytesFreed ||
		receipt.BytesFreed != response.BytesFreed ||
		receipt.ItemsCleaned != response.ItemsCleaned ||
		receipt.RetryAfterSeconds != response.RetryAfterSeconds {
		return fmt.Errorf("external Nix GC receipt does not match the typed response envelope")
	}
	if !nixExternalReceiptDigestPattern.MatchString(response.ReceiptDigest) {
		return fmt.Errorf("external Nix GC response has invalid receipt_digest")
	}
	digest, err := nixExternalReceiptDigest(receipt)
	if err != nil {
		return err
	}
	if response.ReceiptDigest != digest {
		return fmt.Errorf("external Nix GC receipt_digest does not bind the canonical receipt")
	}
	return nil
}

func nixExternalReceiptDigest(receipt nixExternalReceipt) (string, error) {
	canonical, err := json.Marshal(receipt)
	if err != nil {
		return "", fmt.Errorf("encode canonical external Nix GC receipt: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return fmt.Sprintf("sha256:%x", digest[:]), nil
}
