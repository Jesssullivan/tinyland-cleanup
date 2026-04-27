# Nix Cleanup Policy

Nix cleanup is now planned before mutation. The plugin reports dry-run store
reclaim estimates, visible profile generations, and active Nix work before it
touches generations or the store.

Review with:

```sh
tinyland-cleanup --once --dry-run --level critical --output json
```

The Nix plan includes:

- `estimated_bytes_freed` from `nix-collect-garbage --dry-run`;
- detected active Nix work such as `nix build`, `nix store gc`,
  `nix-collect-garbage`, `nix-store --gc`, `home-manager switch`,
  `darwin-rebuild`, `nixos-rebuild`, and worker-style `nix-daemon` activity;
- protected `nix_active_work`, `nix_process_inspection`, or
  `nix_store_contention` targets when cleanup is deferred before mutation;
- `retry_after` metadata derived from `daemon_busy_backoff` so operators can
  distinguish a temporary deferral from a cleanup-policy mismatch;
- `nix_daemon_contention` deferral when dry-run GC itself reports SQLite or
  store lock contention and `skip_when_daemon_busy` is enabled;
- visible GC roots when dry-run GC reports no reclaimable store space;
- generation targets with `keep_generation`, `delete_generation`, or
  `review_privileged_generation` actions;
- configured minimum user and system generation retention;
- whether critical `nix-store --optimize` is allowed.

Default policy:

```yaml
nix:
  min_user_generations: 5
  min_system_generations: 3
  delete_generations_older_than: 14d
  critical_delete_generations_older_than: 3d
  allow_store_optimize: false
  skip_when_daemon_busy: true
  daemon_busy_backoff: 30m
  max_gc_duration: 20m
  root_attribution_limit: 20
```

Runtime behavior:

- warning runs plain Nix garbage collection only;
- moderate and aggressive may delete old user profile generations selected by
  the age policy, while preserving the current generation and the minimum count;
- critical uses the stricter age policy, then runs plain Nix garbage collection;
- dry-run GC lock or SQLite contention is treated like active Nix work when
  `skip_when_daemon_busy` is enabled, so the plan is deferred instead of
  continuing into generation inspection;
- active-work, process-inspection, and store-contention deferrals are surfaced
  as protected dry-run targets with no reclaim estimate, plus retry guidance
  based on `daemon_busy_backoff`;
- real generation deletion and GC commands also treat those contention
  signatures as deferred no-op cleanup steps when `skip_when_daemon_busy` is
  enabled;
- system or nix-darwin generations are reported for operator review but are not
  deleted by the unprivileged plugin path;
- low-reclaim dry-runs run `nix-store --gc --print-roots` and emit protected
  `nix_gc_root` targets so operators can see whether profiles, gcroots,
  workspace `result` links, temporary roots, or active processes are pinning the
  store;
- `nix-store --optimize` runs only when `allow_store_optimize: true`.

Recommended Darwin developer-machine defaults are the repo defaults above.
They preserve Home Manager rollback safety, avoid fighting active
`home-manager switch` or `darwin-rebuild` work, and keep store optimization
opt-in because it can be long-running.

Recommended Rocky or Linux runner defaults:

```yaml
nix:
  min_user_generations: 3
  min_system_generations: 2
  delete_generations_older_than: 7d
  critical_delete_generations_older_than: 2d
  allow_store_optimize: false
  skip_when_daemon_busy: true
  daemon_busy_backoff: 15m
  max_gc_duration: 15m
  root_attribution_limit: 20
```

Keep `skip_when_daemon_busy` enabled on build runners. A cleanup cycle that
breaks an active Nix build or remote-cache proof is worse than a deferred GC.
Set `root_attribution_limit: 0` only for hosts where dry-run root enumeration is
too noisy or too expensive.
