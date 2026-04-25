# Validation Status: 2026-04-25

Branch: `codex/productionization-baseline`

## Passed

```sh
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go test ./...
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go vet ./...
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go build ./...
nix build .#default --no-link --print-build-logs
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/tinyland-cleanup-linux-amd64 .
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o /tmp/tinyland-cleanup-darwin-arm64 .
```

The Nix build produced:

```text
/nix/store/75pd38gkwddpcaz7l7jcwrnmgbp6qpsf-tinyland-cleanup-0.2.0
```

Hosted PR CI also passed:

```text
GitHub Actions run 24942288608
Go: passed in 20s
Bazel: passed in 1m29s
```

## Local Darwin Blocker

```sh
nix shell nixpkgs#bazelisk --command bazelisk --output_user_root=/tmp/tinyland-cleanup-bazel test //...
```

This no longer fails at Bzlmod module resolution. It loads the module graph and
analyzes the repo targets, then stalls locally on Darwin while building the
`rules_go` builder action:

```text
GoToolchainBinaryBuild external/rules_go~0.46.0~go_sdk~tinyland_cleanup__download_0/builder
```

A second diagnostic run with local spawn strategy also failed to reach test
execution in a reasonable time:

```sh
nix shell nixpkgs#bazelisk --command bazelisk --output_user_root=/tmp/tinyland-cleanup-bazel-local test --spawn_strategy=local --strategy=GoToolchainBinaryBuild=local //...
```

Next investigation should decide whether to:

- move this repo to the newer rules_go version already used by other Tinyland
  Bazel surfaces;
- run the Bazel proof on a GloriousFlywheel `tinyland-nix` runner instead of
  local Darwin;
- avoid downloading a Go SDK through rules_go and use a proven Nix-provided
  toolchain bridge if a current Bzlmod-compatible one exists.
