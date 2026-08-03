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

The repo owns an unprivileged GloriousFlywheel consumer kit through `Justfile`,
`justfile.flywheel`, `.bazelrc.flywheel`, and
`scripts/gloriousflywheel-bazel.sh`. The default devshell is composed only from
public, lock-pinned Nix inputs; it does not fetch the private infrastructure
repository or contain enrollment, token-exchange, credential-helper, or cache
publication credentials. Runtime cache coordinates come from the runner or an
explicitly prepared operator environment and are never sourced from a
repo-local `.env` file. The wrapper can request upload only for the separately
protected manual proof lane:

```sh
just flywheel-doctor
just flywheel-verify
just flywheel-test //...
```

Cache-backed validation on GloriousFlywheel runners:

```sh
GF_BAZEL_SUBSTRATE_MODE=shared-cache-backed \
GF_BAZEL_REMOTE_UPLOAD=false \
BAZEL_REMOTE_CACHE=grpc://example.internal:9092 \
  just flywheel-test //...
```

Pull-request and merge-group CI dogfood the shared `tinyland-nix` runner lane and enter the
same public-input Nix devshell before Go, Bazel, and Nix package checks. Bazel
and docs CI use the checked-in front-door kit (`just flywheel-test` and
`just flywheel-build`), force remote upload off, and fail if the qualified
shared cache is absent; they do not silently fall back to a local green build.
PR and merge-group jobs have read-only repository permissions and do not persist the checkout
token. They clear credential-helper, header, executor, Attic, CACHIX, and GitHub
token environment channels. The runner-side PR cache capability must itself be
server-enforced read-only: client-side upload flags are defense in depth, not
publication authority. Pages write/OIDC authority exists only on the
protected-main deploy job. Do not move normal CI back to `ubuntu-latest` as a
cache or runner fallback.

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
