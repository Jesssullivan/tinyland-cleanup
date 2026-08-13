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

Targets include policy tier, physical byte estimates, logical byte estimates
when different, host-reclaim expectation, active-use evidence, protected status,
the planned action, and a reason. Output-base candidates get a bounded
recursive allocation refinement before policy planning so protected, recent,
active, and reclaimable `execroot` and `bazel-out` bytes are visible in dry-run
plans and budget metadata. Output bases are protected when:

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
  reap_orphaned_output_bases: true
  orphan_stale_after: 7d
  # orphan_workspace_mount_roots defaults per platform; see below.
```

## Orphaned output bases

Bazel writes a `DO_NOT_BUILD_HERE` file into every output base containing the
absolute path of the workspace that owns it. When that workspace is deleted —
the usual source is a removed `git worktree` — the output base becomes garbage
that no rebuild will ever reuse, and `keep_recent_output_bases` keeps sheltering
it because its mtime never advances relative to its peers.

`reap_orphaned_output_bases` classifies those bases as
`delete_orphaned_output_base` once they are older than `orphan_stale_after`
(default `7d`, independent of `stale_after` because a removed workspace cannot
come back). Orphan candidates bypass `keep_recent_output_bases`. They never
bypass active-use evidence or `protect_workspaces`.

Workspace claims resolve to exactly one of three states, and the reaper acts on
only one of them:

| State | Meaning | Action |
|---|---|---|
| `present` | the claimed workspace path exists | normal stale/retention policy |
| `removed` | the claimed path is gone and its surrounding tree is reachable | orphan reap candidate |
| `unreachable` | the claim could not be resolved safely | reported, never reaped |

Every ambiguous reading is fail-closed into `unreachable`: an unreadable or
non-regular `DO_NOT_BUILD_HERE`, a relative path, a `stat` error that is not
"does not exist" (permission denied, `EIO`, a stale network handle), or a
workspace whose nearest surviving ancestor is only a mount container.

That last rule is what keeps an unmounted volume from looking like a mass
orphaning event. `orphan_workspace_mount_roots` lists the directories that only
ever hold mount points; when the nearest surviving ancestor of a claimed
workspace is one of them, the workspace is unreachable, not removed. Compiled
defaults are `/`, `/mnt`, `/media`, `/run/media`, `/net`, and `/srv` on Linux,
and `/`, `/Volumes`, `/System/Volumes`, `/net`, and `/private/var/folders` on
Darwin. Setting the key in config replaces the compiled list entirely.

Dry-run plans carry `orphaned_workspace_output_base_count` and
`unreachable_workspace_output_base_count` in plan metadata, and every non-orphan
output-base target whose claim is `removed` or `unreachable` says so in its
reason string. Real cleanup re-reads the claim and re-checks activity
immediately before deleting each orphan, so a restored worktree or a resumed
server that appears between planning and deletion survives.

Runtime boundary:

- warning reports footprint only;
- moderate, aggressive, and critical classify stale inactive output bases as
  `delete_output_base` candidates in dry-run output and delete those output
  bases in real cleanup mode;
- moderate, aggressive, and critical classify stale repository cache, disk
  cache, and Bazelisk download entries as `delete_cache_tier` only when the
  total Bazel footprint exceeds `max_total_gb`;
- cache-tier cleanup is skipped while active Bazel or Bazelisk client commands
  are visible;
- process-visible explicit `--output_base` directories are included in the plan
  even when they are outside configured output-user roots; active clients and
  idle/server-only output-base visibility protect only their own output base and
  do not globally block stale unrelated output-base cleanup;
- partial output-base directories with `execroot/` but missing the full
  `action_cache/` + `server/` shape are reported as
  `partial_output_base` and can be deleted when stale, inactive, and outside
  retention;
- output-user-root repository caches such as `_bazel_$USER/cache/repos/v1` are
  reported as `repository_cache` candidates instead of being hidden under the
  parent `_bazel_$USER` tree;
- if active Bazel process inspection fails, cleanup continues with
  candidate-local evidence only; idle-server shutdown remains unavailable
  without process visibility, but stale inactive output bases and cache tiers
  can still be handled when their own local activity checks are clear;
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
- output-base byte counts use a bounded recursive allocation estimate and mark
  the plan partial if the refinement times out; repository cache, disk cache,
  and Bazelisk cache-tier byte counts use top-level allocation estimates so
  dry-run remains responsive on very large cache trees;
- Bazel output bases, repository caches, and disk caches are `warm` targets
  because they are rebuildable but expensive; Bazelisk downloads are `safe`
  targets;
- moderate, aggressive, and critical classify output bases whose
  `DO_NOT_BUILD_HERE` workspace was provably removed as
  `delete_orphaned_output_base` once they pass `orphan_stale_after`;
- `delete_output_base`, `delete_orphaned_output_base`, `delete_cache_tier`, and
  `stop_idle_server_then_delete_output_base` targets advertise `reclaim=host`
  and `host_reclaims_space=true`; review and protected targets advertise
  `reclaim=none`.

Do not disable active-output-base protection on developer machines or shared
runners unless an operator has already drained the relevant jobs and accepted
the risk.

## Bazel Cache and RBE Proof Contract

The repo keeps Bazel endpoint configuration out of `.bazelrc`. Operators and
GloriousFlywheel runners attach cache and executor endpoints through
environment variables consumed by `scripts/bazel-cache-backed.sh`:

```sh
BAZEL_REMOTE_CACHE=grpc://example.internal:9092 \
  bash scripts/bazel-cache-backed.sh test //...
```

By default the wrapper uses local execution with a local disk cache. When
`BAZEL_REMOTE_CACHE` is set it uses shared remote cache mode and does not upload
local results unless `BAZEL_REMOTE_UPLOAD=true` is set by a trusted proof run.

Remote execution is explicit proof-only. Use `scripts/bazel-rbe-proof.sh` with
both a cache endpoint and executor endpoint, and only for a target listed in
`config/bazel-rbe-target-eligibility.json`:

```sh
GF_RBE_PROOF_MODE=explicit \
GF_BAZEL_SUBSTRATE_MODE=executor-backed \
GF_BAZEL_REMOTE_EXECUTION_PLATFORM=linux-x86_64 \
BAZEL_REMOTE_CACHE=grpc://example-cache.internal:9092 \
BAZEL_REMOTE_EXECUTOR=grpc://example-executor.internal:8980 \
  bash scripts/bazel-rbe-proof.sh --target //:bazel_cache_policy_check
```

Remote cache hits are not RBE proof. The manifest currently marks only the
Bazel cache policy contract as an explicit proof candidate; the broad Go test
graph, cleanup runtime, and Nix packaging surfaces remain local/shared-cache
validated until a matching executor proof artifact exists.
