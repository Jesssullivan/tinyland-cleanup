# Operator Workflow

Use this workflow when a developer machine or runner is under disk pressure.

## Review First

Start with a dry-run at the pressure level you are considering:

```sh
tinyland-cleanup --once --dry-run --level critical --output json
```

The JSON report includes:

- the selected cleanup level;
- monitored mount status;
- host free-space before and after the cycle;
- top-level dry-run totals for planned estimated reclaim, required free space,
  and cleanup target count;
- enabled plugins that would run;
- plugin descriptions and dry-run skip reasons.

Dry-run mode does not call plugin cleanup methods. A plugin entry with
`skip_reason: "dry_run"` means the plugin is enabled and would run at that
level during a real cleanup cycle.

## Apply

After reviewing the plan, run the same level without `--dry-run`:

```sh
tinyland-cleanup --once --level critical --output json
```

The report distinguishes:

- `bytes_freed`: the legacy aggregate byte count reported by the plugin;
- `estimated_bytes_freed`: bytes based on local size estimates, when a plugin
  can provide them;
- `command_bytes_freed`: bytes reported by an external cleanup command, when
  available;
- `host_bytes_freed`: plugin-isolated host free-space measurement, when
  available;
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

For Darwin IDE and tool caches, review
[darwin-dev-caches.md](darwin-dev-caches.md). These targets are reported for
operator review before budget enforcement is enabled.

## Current Boundary

This is the first stable reporting surface. It now exposes typed targets for
selected plugins, but not per-file cleanup candidates for every plugin. Treat
broader candidate planning, active-use evidence, cooldown state, and
target-free stop behavior as the next policy layer.
