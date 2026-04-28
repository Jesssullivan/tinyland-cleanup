# Operator Workflow

Use this workflow when a developer machine or runner is under disk pressure.

## Review First

Start with a dry-run at the pressure level you are considering:

```sh
tinyland-cleanup --once --dry-run --level critical --output text
```

The text report is the operator explain view. It summarizes the selected level,
monitored mounts, host free-space accounting, target free-space deficit, plugin
plans, warnings, and the first few cleanup targets for review. Target rows show
the filesystem path when a target has both a short label and a path, so
review-only roots and artifacts can be acted on without switching formats.

Use JSON when another tool needs the stable report schema:

```sh
tinyland-cleanup --once --dry-run --level critical --output json
```

List registered plugin names and whether they are enabled for the current
config before choosing a bounded evidence run:

```sh
tinyland-cleanup --list-plugins
tinyland-cleanup --list-plugins --output json
```

When gathering evidence on an active workstation, constrain the run to the
plugin family under review instead of scanning every enabled cache surface:

```sh
tinyland-cleanup --once --dry-run --level critical --plugins bazel --output text
tinyland-cleanup --once --dry-run --level critical --plugins nix --output json
tinyland-cleanup --once --dry-run --level critical --plugins cache --output text
tinyland-cleanup --once --dry-run --level critical --plugins docker --output json
tinyland-cleanup --once --dry-run --level critical --plugins homebrew,ios-simulator,xcode --output json
```

The plugin filter is comma-separated and preserves the normal registry order:

```sh
tinyland-cleanup --once --dry-run --level critical --plugins bazel,nix --output json
```

For a one-off operator run, override the configured maximum used-space target
without editing config:

```sh
tinyland-cleanup --once --dry-run --level critical --target-used-percent 82
```

The JSON report includes:

- the selected cleanup level;
- monitored mount status;
- host free-space before and after the cycle;
- the configured target maximum used percentage and equivalent free-space
  deficit;
- daemon state file and configured cleanup cooldown;
- top-level dry-run totals for planned estimated reclaim, required free space,
  and cleanup target count;
- optional `plugin_filter` when `--plugins` constrains the cycle;
- enabled plugins that would run;
- plugin descriptions and dry-run skip reasons.

Dry-run mode does not call plugin cleanup methods. A plugin entry with
`skip_reason: "dry_run"` means the plugin is enabled and would run at that
level during a real cleanup cycle.

Structured plugin targets may include policy metadata:

- `tier`: cleanup risk/rebuild-cost class, such as `safe`, `warm`,
  `disruptive`, `destructive`, or `privileged`;
- `bytes`: measured physical allocation when available;
- `logical_bytes`: logical size when it differs from physical allocation;
- `reclaim`: whether the planned action is expected to reclaim host space
  directly (`host`), only enable later reclamation (`deferred`), or reclaim no
  space by itself (`none`);
- `host_reclaims_space`: boolean form of the direct host-space expectation.

Use this metadata to separate cheap cache cleanup from expensive rebuilds,
privileged actions, and review-only evidence before applying a real cleanup.

## Probe Darwin Volumes

When a Darwin machine has a mounted external volume but a daemon or LaunchAgent
cannot prove access, run direct probe mode before changing cleanup policy. This
mode does not load config and does not run cleanup plugins. It lists the target
path, reads xattrs, writes one temporary dotfile, removes that file, and writes
key-value result codes for wrapper scripts.

```sh
tinyland-cleanup \
  --probe-volume-path /Volumes/TinylandSSD/tinyland \
  --probe-result-path /tmp/tinyland-cleanup-probe.result \
  --probe-name tinyland-cleanup \
  --probe-timeout-seconds 10
```

Result files use `ls_rc`, `xattr_rc`, `touch_rc`, and `rm_rc`. Exit code `0`
for each operation means that the same binary path can perform that operation
from the current execution context. Exit code `124` means the bounded child
operation timed out.

## Apply

After reviewing the plan, run the same level without `--dry-run`:

```sh
tinyland-cleanup --once --level critical --output text
```

Use `--output json` for automation. The report distinguishes:

- `bytes_freed`: the legacy aggregate byte count reported by the plugin;
- `estimated_bytes_freed`: bytes based on local size estimates, when a plugin
  can provide them;
- `command_bytes_freed`: bytes reported by an external cleanup command, when
  available;
- `host_bytes_freed`: plugin-isolated host free-space measurement, when
  available;
- `state_file`: path used for persistent daemon cleanup state;
- `state_error`: load/save failure for daemon cleanup state, when present;
- `cooldown_seconds`: configured per-plugin cleanup cooldown;
- `cooldown_remaining_seconds`: per-plugin remaining cooldown when a plugin is
  skipped with `skip_reason: "cooldown"`;
- `target_used_percent`: the legacy `target_free` config value, interpreted as
  the desired maximum used percentage after cleanup;
- `target_free_bytes`: the host free-space equivalent of that target;
- `target_free_deficit_bytes`: remaining bytes needed to satisfy the target;
- `target_free_met`: whether the current host free space satisfies the target;
- `stop_reason`: why remaining plugins were skipped, currently
  `target_free_met` when a real cleanup cycle reaches the target;
- `planned_estimated_bytes_freed`: aggregate dry-run reclaim estimate from
  plugin plans;
- `planned_required_free_bytes`: largest free-space preflight requirement
  across plugin plans;
- `planned_targets`: total number of dry-run targets across plugin plans;
- `host_free_delta_bytes`: cycle-level host free-space delta for the monitored
  path.

The cycle-level host delta is the operator truth for whether the machine gained
usable space. Plugin byte counts are supporting evidence and may differ from
physical host reclaim, especially for sparse VM disks, container stores, and
filesystem snapshots.

For macOS Podman VM disk compaction, review
[podman-darwin-compaction.md](podman-darwin-compaction.md). Guest `fstrim` on
`applehv` raw images is advisory and is not counted as host-side reclaimed
space.

For Nix, active build, Home Manager, rebuild, `nix-store`, and worker-style
`nix-daemon` activity defers cleanup when `skip_when_daemon_busy` is enabled.
The dry-run plan reports protected active-work or store-contention targets and
`retry_after` metadata from `nix.daemon_busy_backoff`; treat those as temporary
operator backoff signals, not reclaim candidates.

For Darwin IDE and tool caches, review
[darwin-dev-caches.md](darwin-dev-caches.md). These targets are reported for
operator review, and real deletion requires `darwin_dev_caches.enforce: true`.
The typed surface covers cache-only JetBrains, Playwright, Bazelisk, pip, VS
Code, and Cursor targets while preserving settings, extension data, credentials,
and active editor or IDE processes.

For workspace build artifacts, the `dev-artifacts` plan reports rebuildable
targets such as `node_modules`, Python virtualenvs, Rust `target/` directories,
Zig `.zig-cache` and `zig-out` directories, Go build cache, Haskell caches,
opt-in LM Studio model caches, and review-only large local disk/image artifacts
such as `.dmg`, `.img`, `.qcow2`, `.raw`, `.iso`, `.sparsebundle`, `.utm`,
`.pvm`, and `.vmwarevm` targets. Warning level reports only; moderate and above
mark eligible stale artifacts as deletion or cache-clean targets while
preserving configured protected paths. Large local disk/image artifacts are
always protected and excluded from estimated reclaim because they can be
developer-owned state. Mounted disk images are reported as active protected
targets with their mount point so operators know to detach them before any
manual cleanup or compaction. Sparsebundle targets also report logical size
from `Info.plist` when available; detached APFS sparsebundles may still reject
`hdiutil compact`, so treat them as manual migrate/delete decisions rather than
automatic reclaim. The plan also protects matching artifact families when
active package manager, compiler, language server, runtime, or LM Studio
processes are visible, and it preserves any candidate artifact directory that
contains files tracked by Git. Zig `.zig-cache` and `zig-out` targets are also
preserved when they contain files modified within the recent-output grace
window, even at critical pressure.

For Docker, the plan reports Docker daemon disk-usage rows from `docker system
df`, including images, stopped containers, local volumes, and build cache when
available. Docker cleanup is deferred when active Docker build, buildx, compose,
pull, push, or run work is visible and `docker.protect_running_containers` is
enabled. Reported reclaimable bytes may describe Docker daemon or VM storage
and may not immediately equal host free-space delta on macOS or VM-backed
Docker installations.

For APFS snapshots on macOS, the plan reports local snapshot count, newest and
oldest snapshot dates, requested thinning size, sudo capability, and Time
Machine backup state. Snapshot sizes are estimates because `tmutil` does not
report per-snapshot allocation; real cleanup requires passwordless sudo and is
deferred while a Time Machine backup is active.

For Homebrew, iOS Simulator, and Xcode on macOS, the plan reports package-manager
cleanup estimates, simulator device/log/runtime targets, and Xcode logs,
DerivedData, Archives, and DeviceSupport targets. Simulator and Xcode cleanup is
deferred while active Simulator, Xcode, SourceKit, or `xcodebuild` work is
visible. Simulator runtime deletion remains critical-level and passwordless-sudo
only.

## Current Boundary

This is the first stable reporting surface. It now exposes typed targets for
selected plugins, but not per-file cleanup candidates for every plugin. Real
cleanup cycles stop remaining plugins after the host reaches the configured
target. Daemon-triggered non-critical cleanup also honors per-plugin cooldown
state; explicit `--level` runs and critical pressure bypass cooldown. Treat
broader active-use evidence and CLI target-free overrides as the next policy
layer.
