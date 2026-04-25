# Repository Guidance

## Role

`tinyland-cleanup` is a disk-pressure cleanup daemon for Tinyland developer
machines and CI hosts. It must be conservative by default because it runs on
workstations that host multiple hermetic build systems and local developer
state.

The canonical upstream is `github.com/Jesssullivan/tinyland-cleanup`. Treat the
old Tinyland organization fork as historical context only unless a current
operator runbook says otherwise.

## Build Authority

- Native Go commands are the fastest local validation path.
- Nix is the package authority for Darwin and Linux ingestion.
- Bazel is the hermetic build/test graph and the surface used by
  GloriousFlywheel shared-cache runners.
- GloriousFlywheel cache attachment is an acceleration contract. Do not claim
  remote execution/offload unless a runner contract proves it for this repo.

## Safety Defaults

- Prefer dry-run and plan output before destructive cleanup.
- Preserve host free-space accounting before and after every cleanup decision.
- Keep privileged operations, offline compaction, and disruptive service work
  explicitly opt-in.
- Keep Darwin and Linux/Rocky behavior separate where platform semantics differ.

## Validation

Use repo-managed caches outside the user profile when possible:

```sh
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go test ./...
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go vet ./...
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go build ./...
nix build .#default --no-link --print-build-logs
nix shell nixpkgs#bazelisk --command bazelisk --output_user_root=/tmp/tinyland-cleanup-bazel test //...
```
