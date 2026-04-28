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
- Linux RPM packaging configuration, systemd unit, packaged config defaults,
  and release workflow RPM artifacts.
- Persistent daemon cleanup state with per-plugin cooldowns for non-critical
  daemon-triggered cleanup cycles.
- Structured dry-run targets for development artifacts such as stale
  `node_modules`, Python virtualenvs, Rust `target/` directories, Zig
  `.zig-cache` and `zig-out` directories, Go build cache, Haskell caches, and
  opt-in LM Studio model caches.
- Review-only large local artifact targets for disk images and VM bundles such
  as `.dmg`, `.img`, `.qcow2`, `.raw`, `.iso`, `.sparsebundle`, `.utm`, `.pvm`,
  and `.vmwarevm` paths.
- Opt-in Darwin developer-cache enforcement for typed JetBrains, Playwright,
  Bazelisk, and pip cache targets.
- Nix low-reclaim dry-runs now emit protected GC-root attribution targets so
  operators can see what is pinning the store before taking action.
- Human-readable `--output text` reports now explain dry-run and cleanup cycles
  with mount status, host free-space accounting, plugin plans, warnings, and
  representative targets.
- CLI `--target-used-percent` override for one-off cleanup runs without editing
  config.
- Bazel cache-tier budget enforcement for stale repository cache, disk cache,
  and Bazelisk download targets when total Bazel footprint exceeds the
  configured budget.
- Repo-local Bazel symlink cleanup after successful stale output-base deletion.
- Active-process protection for development artifact cleanup families such as
  Node.js, Python, Rust, Zig, Go, Haskell, and LM Studio.
- Typed Darwin developer-cache targets for VS Code and Cursor cache-only
  directories, with active-editor protection.
- CLI `--plugins` filter for bounded dry-run evidence collection and targeted
  cleanup cycles.
- CLI `--list-plugins` discovery output for plugin names, enabled state, and
  platform support.
- Dry-run cleanup targets now carry policy tier, logical byte, reclaim kind,
  and host-space reclaim expectation metadata where planner evidence is
  available.
- Review-only sparsebundle targets now report logical size from `Info.plist`
  when available, making APFS bundle physical-vs-logical accounting visible in
  dry-run plans.
- Darwin JetBrains cache planning now uses the configured `max_gb` budget to
  mark oldest inactive cache versions as opt-in aggressive cleanup candidates.
- Nix dry-run GC lock and SQLite contention now surfaces as
  `nix_daemon_contention` deferral when daemon-busy skipping is enabled.
- Nix generation deletion and GC commands now treat the same contention
  signatures as skipped cleanup rather than hard failures when daemon-busy
  skipping is enabled.

### Changed

- Go module path moved to `github.com/Jesssullivan/tinyland-cleanup`.
- Clarified that the legacy `target_free` config key represents the target
  maximum used-space percentage after cleanup.
- Critical Darwin cache cleanup now prefers typed developer-cache targets when
  `darwin_dev_caches.enabled` is true, avoiding broad `~/Library/Caches`
  sweeps unless the typed policy is disabled.

## [0.2.0]

### Added

- Darwin Podman fstrim accounting fix.
- Nix flake package for `tinyland-cleanup`.
