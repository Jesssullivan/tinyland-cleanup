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

Passed in this session on aarch64-darwin (Go is the fast local authority):

```sh
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go build ./...   # PASS
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go vet ./...     # PASS
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go test ./...    # PASS
gofmt -l .                                                                     # clean
```

New tests cover: inode-driven escalation, the combined-level `max(byte, inode)`
property, the unavailable-inode guard, the inode-pressure circuit breaker
engaging after repeated no-progress cycles, the multi-mount stop condition, and
inode no-progress state round-tripping.

Strict config parsing (`KnownFields(true)`) verified for the shipped configs:

```sh
tinyland-cleanup -config config/default.yaml -list-plugins           # PASS
tinyland-cleanup -config packaging/linux/config.yaml -list-plugins   # PASS
```

Report smoke test (`-once -dry-run` monitoring `/` and `/nix` on this APFS host)
confirmed the intended safe behavior: both mounts read byte `critical` (disk is
genuinely ~95% full) while inode level is `none` (0.2% / 2.9% inodes used), i.e.
APFS dynamic inode counts do not falsely escalate. `host_byte_level`,
`host_inode_level`, and `max_inode_level` render distinctly in JSON.

No cleanup mutation was performed during validation.

### Not re-run in this session

`nix build .#default` (Nix package authority) and
`bazelisk test //...` (hermetic Bazel graph) are part of the validation contract
(AGENTS.md) but were not re-run here; they remain CI's responsibility before
release.

## Lab Consumer Proof

The lab Home Manager contract was checked against this working tree using the
same upstream input that lab normally consumes from GitHub:

```sh
nix eval  .#checks.aarch64-darwin.tinyland-cleanup-config-test.drvPath \
  --override-input tinyland-cleanup-upstream path:/Users/jess/git/tinyland-cleanup   # evaluates
nix build .#checks.aarch64-darwin.tinyland-cleanup-config-test \
  --override-input tinyland-cleanup-upstream path:/Users/jess/git/tinyland-cleanup \
  --no-link -L --builders '' --max-jobs 4                                            # see status below
```

Verified by reading `lab/nix/home-manager/tinyland-cleanup.nix`: the module
renders global `inode_thresholds` and per-mount `threshold_inode_warning` /
`threshold_inode_critical`; `lab/nix/hosts/honey.nix` sets honey's `/nix` mount
to `thresholdInodeWarning = 70` / `thresholdInodeCritical = 90`. The
`tinyland-cleanup-config-test` check is exposed at
`flake.nix` `checks.aarch64-darwin.tinyland-cleanup-config-test` and its
derivation evaluates cleanly against this working tree via `--override-input`.

> RELEASE ORDERING (do not skip): lab's flake input is still pinned to
> `debbdaa` (byte-only `main`), and the lab home-manager change is on branch
> `chore/codex-0.142.0`. A byte-only binary rejects an `inode_thresholds` config
> under strict parsing, so the binary with inode support MUST ship to the pinned
> rev BEFORE lab's flake input is bumped and switched onto honey/bumble.

## GloriousFlywheel Proof

The Bazel workflow remains shared-cache-backed only. This validation does not
prove remote execution/offload. Continue using `scripts/bazel-cache-backed.sh`
with an explicit `BAZEL_REMOTE_CACHE` endpoint for GloriousFlywheel cache proof.
