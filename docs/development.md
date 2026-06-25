# Development

`tinyland-cleanup` is a Go program. Go is the fast local authority, Nix is the
package authority, and Bazel is the hermetic build and test graph.

## Build and test (Go)

```sh
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go build ./...
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go vet ./...
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go test ./...
```

Dependencies are vendored; always pass `GOFLAGS=-mod=vendor`.

## Nix package

```sh
nix build .#default --no-link --print-build-logs
```

## Bazel graph

```sh
nix develop --command just flywheel-test //...
```

## Shared-cache and remote-execution proof

The repo imports the GloriousFlywheel front-door kit through `Justfile` and
`.bazelrc`. The default devshell includes the lightweight
`gloriousflywheel-frontdoor-tools` package, so the advertised consumer commands
resolve from repo-managed Nix tooling without pulling REAPI token-exchange or
credential-helper binaries into this first-adoption cache path:

```sh
just flywheel-doctor
just flywheel-enroll shared-cache-backed --cache-endpoint "$BAZEL_REMOTE_CACHE"
just flywheel-verify
just flywheel-test //...
```

Cache-backed validation on GloriousFlywheel runners:

```sh
BAZEL_REMOTE_CACHE=grpc://example.internal:9092 \
  bash scripts/bazel-cache-backed.sh test //...
```

Pull-request CI dogfoods the shared `tinyland-nix` runner lane and enters the
same Nix devshell before Go, Bazel, and Nix package checks. Bazel CI uses the
front-door kit (`just flywheel-test`), not a raw `bazelisk` command. Do not move
normal CI back to `ubuntu-latest` as a cache or runner fallback.

The shared runner currently executes jobs as root, so `MODULE.bazel` explicitly
opts the docs Python toolchain into rules_python's root-user escape hatch. Remove
that setting when the runner lane executes Bazel as a non-root user.

Remote-execution proof is separate from cache-backed validation:

```sh
GF_RBE_PROOF_MODE=explicit \
GF_BAZEL_SUBSTRATE_MODE=executor-backed \
GF_BAZEL_REMOTE_EXECUTION_PLATFORM=linux-x86_64 \
BAZEL_REMOTE_CACHE=grpc://example-cache.internal:9092 \
BAZEL_REMOTE_EXECUTOR=grpc://example-executor.internal:8980 \
  bash scripts/bazel-rbe-proof.sh --target //:bazel_cache_policy_check
```

## Docs

Docs are plain Markdown in `docs/`. Keep links relative and verify changed
documentation with `rg` plus the normal Go/Nix/Bazel checks when behavior or
packaging truth changes.

## Before a pull request

Run the Go checks above. For Bazel-graph changes, run the Bazel test too. Keep
changes small and reviewable, keep safety-sensitive behavior explicit, and never
commit secrets or host-local config.
