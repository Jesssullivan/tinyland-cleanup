# Validation Status: 2026-06-23

Branch: `jess/tin-2170-inode-awareness`
Commit: `4041891`

## Scope

This validation covers inode-aware pressure handling for TIN-2170/TIN-2165:

- byte and inode pressure are evaluated independently;
- inode pressure can escalate cleanup even when byte usage is healthy;
- real cleanup does not stop at the byte target while inode pressure remains;
- daemon state records inode no-progress cycles so unrelieved inode-only
  escalation can back off to cooldown cadence;
- JSON and text reports include host and mount inode evidence.

## Local Validation

Passed locally on aarch64-darwin:

```sh
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go test ./...
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go vet ./...
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go build ./...
nix shell nixpkgs#bazelisk --command bazelisk --output_user_root=/tmp/tinyland-cleanup-bazel test //...
nix build .#default --no-link --print-build-logs --builders '' --max-jobs 1 --cores 1
```

No cleanup mutation was performed during validation.

The Nix package build was forced to local single-job execution because the
ambient remote-builder configuration intermittently waited on remote builder
handoff. The package itself built and passed its check phase locally.

## Lab Consumer Proof

The lab Home Manager contract was validated against this working tree using the
same upstream input that lab normally consumes from GitHub:

```sh
nix build .#checks.aarch64-darwin.tinyland-cleanup-config-test \
  --no-link -L \
  --override-input tinyland-cleanup-upstream path:/Users/jess/git/tinyland-cleanup \
  --builders '' --max-jobs 1 --cores 1
```

The lab change renders `inode_thresholds` and per-mount
`threshold_inode_warning` / `threshold_inode_critical` keys, including honey's
`/nix` mount policy.

## GloriousFlywheel Proof

The Bazel workflow remains shared-cache-backed only. This validation does not
prove remote execution/offload. Continue using `scripts/bazel-cache-backed.sh`
with an explicit `BAZEL_REMOTE_CACHE` endpoint for GloriousFlywheel cache proof.
