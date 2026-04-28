# Bazel Cache Policy

Bazel cleanup reports output bases, repository caches, disk caches, and
Bazelisk downloads. Real cleanup mode deletes only stale inactive output bases
and budget-excess rebuildable cache tiers after active-process inspection
succeeds.

Review with:

```sh
tinyland-cleanup --once --dry-run --level critical --output json
```

The Bazel plan includes targets for:

- `output_base`: Bazel output bases under configured roots such as
  `~/.cache/bazel/_bazel_$USER/*`, Darwin
  `/private/var/tmp/_bazel_$USER/*`, or direct explicit output bases under
  `/private/tmp`;
- `repository_cache`: shared external repository artifacts;
- `disk_cache`: local action cache entries;
- `bazelisk`: Bazelisk download cache entries.

Targets include policy tier, bounded physical byte estimates, logical byte
estimates when different, host-reclaim expectation, active-use evidence,
protected status, the planned action, and a reason. Output bases are protected
when:

- a Bazel or Bazelisk process is active;
- an active Bazel process exposes an explicit `--output_base`;
- an output-base lock or server PID file is visible;
- a configured protected workspace has `bazel-*` symlinks into that output base;
- the output base is within `keep_recent_output_bases`;
- the output base is newer than the configured stale threshold.

Default policy:

```yaml
bazel:
  roots:
    - ~/.cache/bazel
    # Darwin compiled defaults also include:
    # - /private/var/tmp/_bazel_$USER
    # - /var/tmp/_bazel_$USER
    # - /private/tmp
  workspace_roots:
    - ~/git
    - ~/src
    - ~/projects
  bazelisk_cache: ~/Library/Caches/bazelisk
  max_total_gb: 20
  keep_recent_output_bases: 5
  stale_after: 14d
  critical_stale_after: 3d
  protect_workspaces:
    - ~/git/lab
    - ~/git/GloriousFlywheel
  allow_stop_idle_servers: true
  allow_delete_active_output_bases: false
```

Runtime boundary:

- warning reports footprint only;
- moderate, aggressive, and critical classify stale inactive output bases as
  `delete_output_base` candidates in dry-run output and delete those output
  bases in real cleanup mode;
- moderate, aggressive, and critical classify stale repository cache, disk
  cache, and Bazelisk download entries as `delete_cache_tier` only when the
  total Bazel footprint exceeds `max_total_gb`;
- real cleanup skips Bazel mutation if active Bazel process inspection fails;
- cache-tier cleanup is skipped while active Bazel or Bazelisk client commands
  are visible;
- process-visible explicit `--output_base` directories are included in the plan
  even when they are outside configured output-user roots; idle/server-only
  output-base visibility protects that output base but does not globally block
  unrelated cache-tier cleanup;
- aggressive and critical cleanup may classify stale idle-server-only output
  bases as `stop_idle_server_then_delete_output_base` when
  `allow_stop_idle_servers` is enabled; real cleanup runs
  `bazel --output_base=<path> shutdown`, re-checks the output base, and deletes
  it only if it is no longer active;
- deletion normalizes writable permissions first, and on Darwin attempts to
  clear `uchg` file flags with `chflags -R nouchg`;
- after an output base is deleted, workspace roots are scanned shallowly for
  canonical repo-local `bazel-*` symlinks, and only symlinks whose raw target
  points inside that deleted output base are removed;
- byte counts use top-level allocation estimates so dry-run remains responsive
  on very large generated trees;
- Bazel output bases, repository caches, and disk caches are `warm` targets
  because they are rebuildable but expensive; Bazelisk downloads are `safe`
  targets;
- `delete_output_base`, `delete_cache_tier`, and
  `stop_idle_server_then_delete_output_base` targets advertise `reclaim=host`
  and `host_reclaims_space=true`; review and protected targets advertise
  `reclaim=none`.

Do not disable active-output-base protection on developer machines or shared
runners unless an operator has already drained the relevant jobs and accepted
the risk.
