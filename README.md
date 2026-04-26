# tinyland-cleanup

`tinyland-cleanup` is a conservative disk-pressure cleanup daemon for developer
machines and CI hosts. It focuses on build-system and developer-tool caches
where unmanaged disk pressure can break local work, remote runners, or
hermetic build flows.

The current production target is Darwin developer machines plus Linux/Rocky
builder and runner machines.

## Safety Model

- Dry-run behavior must stay useful enough for operator review.
- Cleanup policy should explain what it plans to remove and why.
- Host free-space accounting should be measured before and after cleanup.
- Real cleanup should stop once the configured host free-space target is met.
- Daemon-triggered non-critical cleanup should honor cooldown state.
- Privileged actions, offline compaction, and service disruption must remain
  explicit policy choices.

## Build And Test

Fast local validation:

```sh
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go test ./...
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go vet ./...
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go build ./...
```

Nix package build:

```sh
nix build .#default --no-link --print-build-logs
```

Bazel build/test graph:

```sh
nix shell nixpkgs#bazelisk --command bazelisk --output_user_root=/tmp/tinyland-cleanup-bazel test //...
```

Shared-cache Bazel runners can use:

```sh
BAZEL_REMOTE_CACHE=grpc://bazel-cache.nix-cache.svc.cluster.local:9092 \
  scripts/bazel-cache-backed.sh test //...
```

## Operator Review

Review the cleanup plan before mutating a high-pressure machine:

```sh
tinyland-cleanup --once --dry-run --level critical --output text
```

Use JSON when another tool needs the stable report schema:

```sh
tinyland-cleanup --once --dry-run --level critical --output json
```

List available plugin names before constraining an evidence run:

```sh
tinyland-cleanup --list-plugins
```

Constrain review to specific plugins before scanning broad cache surfaces:

```sh
tinyland-cleanup --once --dry-run --level critical --plugins bazel,nix --output text
```

For a one-off run, override the configured maximum used-space target without
editing the config file:

```sh
tinyland-cleanup --once --dry-run --level critical --target-used-percent 82
```

See [docs/operator-workflow.md](docs/operator-workflow.md) for the current
dry-run, candidate policy tier, and host free-space accounting workflow.

Podman on macOS needs extra care because `applehv` raw sparse images do not
shrink from guest `fstrim` alone. See
[docs/podman-darwin-compaction.md](docs/podman-darwin-compaction.md) before
enabling offline compaction.

Darwin developer cache review is documented in
[docs/darwin-dev-caches.md](docs/darwin-dev-caches.md).

Nix store and generation cleanup policy is documented in
[docs/nix-cleanup-policy.md](docs/nix-cleanup-policy.md).

Bazel cache and output-base review is documented in
[docs/bazel-cache-policy.md](docs/bazel-cache-policy.md).

## Distribution Status

Current package authority is the Nix flake package `.#tinyland-cleanup`.
Release archives are produced from GitHub tags. No public tag has been cut yet.
Linux RPM packaging is documented in
[docs/rpm-packaging.md](docs/rpm-packaging.md); the RPM installs a systemd unit
but leaves enable/start as an explicit operator action.

## Roadmap

Open productionization work is tracked in GitHub issues:

- `#2`: durable disk-pressure policy overhaul
- `#3`: Bazel cache tiering, budgets, and active-use detection
- `#4`: Nix cleanup policy for generations, roots, and daemon contention
- `#5`: Darwin IDE and developer-tool cache budgets
- `#6`: Podman offline compaction for Darwin `applehv`
- `#9`: GloriousFlywheel shared-cache runner proof

Recently completed productionization work:

- `#7`: dry-run, telemetry, and host free-space accounting

See [docs/productionization-plan-2026-04-25.md](docs/productionization-plan-2026-04-25.md)
for the current productionization plan.

Current validation notes are tracked in
[docs/validation-status-2026-04-26.md](docs/validation-status-2026-04-26.md).
