# Contributing

Use small, reviewable changes and keep safety-sensitive behavior explicit.

Before opening a pull request, run the relevant validation:

```sh
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go test ./...
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go vet ./...
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go build ./...
nix build .#default --no-link --print-build-logs
```

For changes touching the Bazel graph, also run:

```sh
nix shell nixpkgs#bazelisk --command bazelisk --output_user_root=/tmp/tinyland-cleanup-bazel test //...
```

Do not include secrets, host-local config, or `.env` files in commits.
