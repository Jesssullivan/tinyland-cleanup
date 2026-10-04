# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Fixed

- dev-artifacts: per-root scan budgets (TIN-3342, #129). A workspace root
  that uses its whole share of the scan budget is now truncated on its own:
  it is reported as partial evidence (`workspace_roots_truncated`,
  `scan_truncated_paths`) and the pass continues with the next root, the
  remaining artifact families and the next scan path, instead of ending the
  whole dev-artifacts pass. Each artifact family (node_modules, venv, Rust,
  Zig) walks each scan path against its own pool of `scan_max_entries` and
  `scan_max_duration`, as in v0.4.1; root truncations no longer stop the
  later families, so a cycle can now spend every family's pool. The shared
  deadline bounds the temp, transcript and agent-worktree lanes, and the
  shared entry count (which family entries fold into) bounds the
  post-workspace lanes; when the shared budget is exhausted the remaining
  walks stop and the global cache lanes (Go build cache, pnpm, Haskell,
  LM Studio) still run, because they do not depend on walk evidence.
- dev-artifacts: per-root shares carry over. Each workspace root and temp scan
  path gets what its lane has left divided by the roots still to come, so
  roots that use less than their share leave the rest to later roots instead
  of an even split that truncated large roots while small ones left budget
  unused.
- daemon: the next cycle is scheduled a full `poll_interval` after the previous
  cycle completes, replacing the fixed ticker. A cycle that outlasts the
  interval is no longer followed by an immediate catch-up cycle; one warning is
  logged per overrun streak (TIN-3342).
- state: `state.json` is written atomically (temp file, fsync, rename). An
  undecodable state file is renamed to `state.json.corrupt-<timestamp>` and the
  cycle continues with fresh state, instead of disabling cooldown accounting
  for every later cycle (TIN-3342).

### Added

- JSON report: `cycle_duration_ms`, `next_cycle_at` (daemon mode) and
  `state_quarantined`.
- daemon: byte backoff (TIN-3342). After `policy.byte_no_progress_limit`
  (default 3) consecutive cleanup cycles under byte pressure that reclaim less
  than `policy.byte_progress_min_mb` (default 256 MiB), plugins stop bypassing
  cooldown and run at most once per `min(cooldown, policy.byte_backoff_max)`
  (default cap 30m), including at critical level. It never engages below
  `policy.emergency_free_gb` (default 20 GiB free), for `--level` or
  `--dry-run` runs, or right after the byte level rises. The counter persists
  in `state.json` (state version 2).
- daemon: zero-yield suppression (TIN-3342). After `policy.zero_yield_limit`
  (default 2) consecutive runs of a plugin that each reclaim less than
  `policy.byte_progress_min_mb`, that plugin is skipped for
  `policy.zero_yield_backoff_base` (default 30m), doubling per further
  zero-yield run up to `policy.zero_yield_backoff_max` (default 6h). It runs
  early when the level rises above its last run, when the config or binary
  version changes, after pressure clears, or below `policy.emergency_free_gb`,
  and never applies to `--level`, `--dry-run` or `--plugins` runs,
  `policy.zero_yield_exempt_plugins` or `apfs-snapshots`. In a 24-hour simulation pinned at 95% used with nothing
  reclaimable, a plugin runs 8 times instead of 288.
- JSON report: `next_retry_at`, and per-plugin `zero_yield_count`,
  `suppressed_until` and `zero_yield_lifted`; `skip_reason` gains
  `zero_yield_backoff`.
- JSON report: dev-artifacts plan metadata `workspace_roots_truncated`,
  `scan_budget_shared_exhausted`, `scan_root_share` (`carry_over`) and
  `scan_sizing_entries_visited` (entries visited while sizing artifact
  directories; counted and reported, not enforced against the entry budget).
- JSON report: `byte_no_progress_count`, `byte_backoff`,
  `byte_backoff_reason`, `byte_backoff_seconds`, `emergency_free_bytes`,
  `host_byte_level_after`, and per-plugin `retry_at`.

## [0.4.1] - 2026-10-02

### Fixed

- dev-artifacts: `temp_scan_max_roots` is now enforced per temp scan path, as
  documented, instead of as one budget shared across all `temp_scan_paths`; a
  crowded `/tmp` no longer starves later temp paths. Entry and duration budgets
  are split evenly across temp paths so total work stays bounded.
- dev-artifacts: paths that fail removal with `EACCES`/`EPERM`/`EBUSY` are
  remembered for the daemon's lifetime and logged once, instead of being sized
  and retried every cycle.

## [0.4.0] - 2026-10-02

### Removed

- The `gitlab-runner` plugin and its `enable.gitlab_runner` key. No GitLab
  runners remain in the estate. Config decoding is strict, so a config that
  still sets `enable.gitlab_runner` is rejected (exit 2); delete the key.
- The Darwin release tarballs. They were unsigned, undocumented and
  unconsumed; Darwin hosts build from source through Nix. Releases now carry
  the Linux amd64/arm64 tarballs and RPMs.

### Added

- Bazel orphan reaper: output bases whose `DO_NOT_BUILD_HERE` workspace no
  longer exists are reaped after `orphan_stale_after` (default 7d), bypassing
  `keep_recent_output_bases` but never active-use evidence or
  `protect_workspaces`. Ambiguous readings (unreadable marker, unmounted
  volume) fail closed (#124).
- `archive-lifecycle` plugin: retires archive staging pre-images only after
  proving every file is already in the archive target at the same
  uncompressed size (exact copy, gzip ISIZE, or zstd Frame_Content_Size),
  re-verified immediately before deletion. Inert until `sources` are
  configured (#125).
- `agent_transcript_codec`: transcript compression writes zstd by default
  (`.jsonl.zst`, vendored klauspost/compress); gzip stays selectable and an
  unknown codec is refused. Existing `.jsonl.gz` files are never re-encoded
  (#126).
- `inode_free_floor`, global and per mount: an absolute free-inode floor that
  replaces the percentage inode ladder. Dynamic-inode filesystems (XFS, APFS,
  ZFS, Btrfs) no longer escalate on inode percentage at all.
- `debris-report`, a report-only plugin (off by default) that logs stale
  incident and agent debris (YYYYMMDD-stamped names, bulkload scratch,
  rollback and carry trees, reclaim markers) with sizes. It never deletes.
- Cycle reports carry `ran` per plugin and `fstype`, `inodes_dynamic`,
  `inode_free_floor` and `inode_ladder_skipped` per mount.

### Fixed

- Age checks fall back to ctime when a file's mtime is more than five minutes
  in the future, so output bases stamped decades ahead (2036) age out.
- `github-runner` and `yum` report honest item and byte counts.

### Changed

- PR/merge-group Go, Bazel, and docs checks now use only public lock-pinned Nix tools plus
  the checked-in unprivileged GloriousFlywheel consumer wrapper. The private
  infrastructure flake and auth-capable front-door package are no longer in
  the devshell closure; PR jobs receive no cache publication credential,
  request read-only cache operation, and do not persist checkout credentials.
  Pages publication permissions are protected-main deploy-job-only.

## [0.3.0] - 2026-06-24

### Added

- Documentation site: MkDocs Material built through a pure-Bazel flow
  (`//docs:site` via `rules_python` + a sha256-pinned `pip.parse` lock, with a
  hermetic `//docs:site_smoke_test`), a Nix parity build (`nix build .#docs`),
  and a GitHub Pages deploy workflow.
- Single version source: `VERSION` is the human-edit point; `flake.nix` reads it
  and a CI step checks `main.go` and `MODULE.bazel` agree.

### Changed

- Streamlined the documentation set and rewrote the README into a reductive
  quickstart (install, run modes, byte/inode behavior, configuration). Added
  `installation`, `usage`, `configuration`, `plugins`, and `json-report-schema`
  pages plus `llms.txt`; removed dated/ephemeral docs (productionization plan,
  superseded validation snapshots, the CONTRIBUTING stub).

### Inode-awareness (TIN-2170/TIN-2165)

- Inode-aware disk-pressure handling (TIN-2170/TIN-2165). `DiskStats` now carries
  inode totals/used/free/percent from statfs, and `DiskMonitor` evaluates byte
  and inode thresholds in parallel: the cleanup level is the higher of the two,
  so a filesystem with ample free bytes but exhausted inodes (the honey
  nix-store small-file crunch) now escalates cleanup and fires nix-GC.
  Configurable via `inode_thresholds` and per-mount `threshold_inode_warning` /
  `threshold_inode_critical`. Filesystems that report no inode totals (APFS/ZFS
  report large dynamic counts; some report zero) never falsely escalate.
- Inode-pressure circuit breaker. The cleanup stop condition requires inode
  pressure to clear across all monitored mounts (not just the byte-pressure
  primary), and daemon state records consecutive inode "no-progress" cycles so
  inode-only critical escalation that nothing can relieve backs off to the
  cooldown cadence instead of running every plugin every poll interval. Tunable
  via `policy.inode_no_progress_limit` (default 3).
- JSON and text reports surface host and per-mount inode evidence
  (`host_inode_level`, `host_byte_level`, `max_inode_level`, `inode_backoff`,
  per-mount `byte_level`/`inode_level`/`inodes_*`).
- Repository authority, contribution, security, and productionization docs.
- Bazel/Bzlmod surface for the Go build and test graph.
- GitHub CI and release workflow scaffolding.
- Shared-cache Bazel wrapper for GloriousFlywheel runner attachment.
- JSON cleanup cycle reports with dry-run plugin plans and host free-space
  accounting.
- Podman offline compaction preflight for Darwin VM disks, including physical
  allocation accounting and active-container safety gates.
- Podman BuildKit cache planning and critical cleanup with retention guards,
  command-byte reporting, and Darwin host free-space delta accounting after
  advisory VM trim.
- Structured dry-run targets for Darwin developer caches such as JetBrains,
  Playwright, Bazelisk, and pip.
- Nix cleanup preflight plans with dry-run reclaim estimates, generation
  retention targets, daemon-contention detection, and opt-in store optimization.
- Nix real-cleanup host free-space delta accounting around GC and optional store
  optimization, separate from command-reported reclaimed bytes.
- Opt-in Home Manager generation cleanup via `home-manager remove-generations`,
  with separate minimum-retention and age-policy settings.
- Bazel cache and output-base dry-run planning with active-use detection,
  protected workspace symlink detection, and budget metadata.
- Bazel reclaim candidates now refine byte estimates with a bounded recursive
  allocation walk so stale output-base dry-runs do not understate large nested
  `execroot` and `bazel-out` trees.
- Bazel protected, recent, active, and reclaimable output-base candidates now
  share the same bounded recursive allocation walk before policy planning, so
  dry-runs and budget metadata do not understate large protected `execroot` and
  `bazel-out` trees.
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
- Nix GC-root attribution now classifies Darwin `{lsof}` roots as active open
  store paths, `{nix-process:<pid>}` roots as active Nix work, direnv flake
  roots as warm review targets, and Nix cache roots separately, reducing
  generic `unknown_root` noise on developer machines.
- Nix GC-root attribution now classifies Home Manager current/new generation
  gcroots separately from generic unknown roots.
- Nix GC-root attribution now ignores `nix-store --gc --print-roots` status
  messages such as stale-root removal lines instead of reporting them as
  generic unknown roots.
- Nix GC-root attribution dry-runs now collapse repeated roots such as
  `{lsof}` and `{nix-process:<pid>}` into one protected target per root while
  retaining raw and unique root-class counts in metadata.
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
- Darwin developer-cache and development-artifact dry-runs now summarize
  target bytes by host-reclaim candidate, deferred reclaim, protected, active
  protected, and review-only disposition.
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
- Human-readable text reports now include target paths when a target has both a
  label and filesystem path, making review-only Nix GC roots and large artifact
  targets actionable without switching to JSON.
- Bazel cleanup now distinguishes active client output bases from idle
  server-only output bases, and aggressive/critical cleanup can stop stale idle
  servers before deleting their output bases when `allow_stop_idle_servers` is
  enabled.
- Dev-artifact cleanup now scans stale inactive temporary roots for narrower
  generated-output targets, allowing Rust `target/`, `node_modules`, Python
  virtualenv, and Zig output pruning without deleting the top-level temp root.
- Dev-artifact active-process protection for `node_modules`, Python
  virtualenvs, Rust `target/`, and Zig outputs is now path-scoped when process
  cwd or command-line evidence proves the active project root; missing cwd
  evidence keeps the conservative family-wide protection fallback.
- Dev-artifact dry-runs now skip expensive workspace scans for generated-output
  families that are already protected by conservative family-wide active-use
  evidence, matching real cleanup behavior and preserving scan budget for
  reclaimable surfaces.
- Dev-artifact dry-run and cleanup filesystem walkers now observe cancellation,
  and active temporary roots are protected without expensive size walks so
  operator probes do not compete with active lab or Bazel scratch work.
- Darwin cache cleanup now treats the typed `darwin_dev_caches` plan as the
  real-cleanup authority when enabled, so `enforce: false` prevents legacy
  generic cache deletion paths from mutating developer machines.
- Nix real cleanup now dry-run preflights garbage collection and skips the
  actual GC command when there are zero reclaimable store paths and no user
  generation deletion happened, or fails closed when that preflight fails,
  avoiding pointless store-lock contention and unsafe fallback GC.
- Dev-artifacts dry-runs now surface large top-level temporary proof/output
  directories as protected review-only targets with active process path
  evidence.
- Dev-artifacts planning now has explicit scan budgets for duration, recursive
  entry count, and top-level temporary roots, with dry-run warnings and metadata
  when evidence is partial.
- APFS snapshot dry-runs now recognize `com.apple.os.update-*` local snapshots
  as protected review evidence instead of silently ignoring them.

### Changed

- Go module path moved to `github.com/Jesssullivan/tinyland-cleanup`.
- Nix package builds now pass version, source revision, and flake timestamp
  into `--version` output for provenance.
- Clarified that the legacy `target_free` config key represents the target
  maximum used-space percentage after cleanup.
- Critical Darwin cache cleanup now prefers typed developer-cache targets when
  `darwin_dev_caches.enabled` is true, avoiding broad `~/Library/Caches`
  sweeps unless the typed policy is disabled.
- Critical Podman cleanup now keeps broad `podman system prune -af --volumes`
  behind `podman.critical_system_prune`, while targeted BuildKit cache pruning
  remains enabled by default.

### Fixed

- APFS snapshot dry-run estimates now exclude protected OS-update snapshots
  from the aggregate thinning reclaim target when no date-form Time Machine
  local snapshots are present.
- Active Bazel client processes no longer globally protect unrelated stale
  output bases; active clients still protect their own output base, and shared
  cache-tier cleanup remains deferred while Bazel client work is visible.
- Pass BuildKit `--keep-storage` as the numeric MB value expected by `buildctl`
  during targeted Podman cache pruning.
- Filesystem allocation walks now account for symlink entries with `lstat`, so
  temporary symlink forests such as Nix shell roots are not reported as
  multi-GiB reclaim candidates by charging their `/nix/store` targets to the
  containing temp directory.

## [0.2.0]

### Added

- Darwin Podman fstrim accounting fix.
- Nix flake package for `tinyland-cleanup`.
