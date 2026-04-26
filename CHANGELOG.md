# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added

- Repository authority, contribution, security, and productionization docs.
- Bazel/Bzlmod surface for the Go build and test graph.
- GitHub CI and release workflow scaffolding.
- Shared-cache Bazel wrapper for GloriousFlywheel runner attachment.
- JSON cleanup cycle reports with dry-run plugin plans and host free-space
  accounting.
- Podman offline compaction preflight for Darwin VM disks, including physical
  allocation accounting and active-container safety gates.
- Structured dry-run targets for Darwin developer caches such as JetBrains,
  Playwright, Bazelisk, and pip.
- Nix cleanup preflight plans with dry-run reclaim estimates, generation
  retention targets, daemon-contention detection, and opt-in store optimization.
- Bazel cache and output-base dry-run planning with active-use detection,
  protected workspace symlink detection, and budget metadata.
- Top-level dry-run summary fields for planned estimated reclaim, required free
  space, and cleanup target count.
- Target-free report fields and real-cleanup stop behavior once the configured
  target is reached.
- Bazel real-cleanup deletion for stale inactive output bases, guarded by
  active-process inspection and permission normalization.

### Changed

- Go module path moved to `github.com/Jesssullivan/tinyland-cleanup`.
- Clarified that the legacy `target_free` config key represents the target
  maximum used-space percentage after cleanup.

## [0.2.0]

### Added

- Darwin Podman fstrim accounting fix.
- Nix flake package for `tinyland-cleanup`.
