# JSON report schema

`--output json` emits one `cycleReport` object per run. Fields marked optional
are omitted when empty. The source of truth is the `cycleReport`,
`mountReport`, and `pluginCycleReport` structs in `main.go`.

> **Effectiveness signal:** `host_free_delta_bytes` and `host_inodes_free_delta`
> are the truth for whether a cycle reclaimed space. Plugin byte estimates are
> supporting evidence and may differ (sparse images, copy-on-write filesystems).

## Top-level fields

### Context
- `timestamp` (string, RFC3339) — when the cycle started
- `dry_run` (bool) — plan only, no changes
- `forced_level` (bool) — `--level` was set
- `level` (string) — `none` | `warning` | `moderate` | `aggressive` | `critical`
- `monitor_path` (string) — primary path measured

### Byte state
- `host_free_before_bytes`, `host_free_after_bytes` (uint64)
- `host_free_delta_bytes` (int64) — net bytes freed this cycle
- `host_byte_level` (string, optional) — byte-driven level for the primary path

### Inode state
- `host_inodes_total`, `host_inodes_free_before`, `host_inodes_free_after` (uint64, optional)
- `host_inodes_free_delta` (int64, optional) — net inodes freed
- `host_inodes_used_percent_before`, `host_inodes_used_percent_after` (float64, optional)
- `host_inode_level` (string, optional) — inode level for the primary path
- `max_inode_level` (string, optional) — highest inode level across all monitored mounts
- `inode_no_progress_count` (int, optional) — consecutive cycles with no inode relief
- `inode_backoff` (bool, optional) — inode circuit breaker engaged this cycle

### Target and policy
- `target_used_percent` (int) — configured max used % after cleanup (`target_free`)
- `target_free_bytes` (uint64), `target_free_deficit_bytes` (int64), `target_free_met` (bool)
- `minimum_free_bytes` (uint64, optional), `minimum_free_deficit_bytes` (int64, optional), `minimum_free_met` (bool)
- `cooldown_seconds` (int64, optional)
- `stop_reason` (string, optional) — e.g. `target_free_met`
- `state_file` (string, optional), `state_error` (string, optional)
- `host_free_error` (string, optional)

### Plan and totals
- `planned_estimated_bytes_freed`, `planned_required_free_bytes` (int64, optional) — dry-run aggregates
- `planned_targets` (int, optional)
- `total_bytes_freed` (int64), `total_items_cleaned` (int)
- `plugin_filter` (array of string, optional) — present when `--plugins` was used

### Collections
- `mounts` (array of mountReport)
- `plugins` (array of pluginCycleReport)

## mountReport
- `label`, `path` (string); `used_percent`, `free_gb` (float64); `free_bytes` (uint64)
- `byte_level` (string), `inode_level` (string, optional — absent means inodes unmeasured)
- `inodes_total`, `inodes_free` (uint64, optional); `inodes_used_percent` (float64, optional)
- `fstype` (string, optional); `inodes_dynamic` (bool, optional) — dynamic-inode filesystem whose used percentage is not a pressure signal
- `inode_free_floor` (uint64, optional); `inode_ladder_skipped` (bool, optional) — the percentage inode ladder was not consulted
- `level` (string) — combined level; `error` (string, optional)

## pluginCycleReport
- `name`, `description` (string); `level` (string); `dry_run`, `would_run` (bool)
- `ran` (bool) — `Cleanup` was actually invoked this cycle (never true in dry-run); `would_run` is eligibility, `ran` is the outcome
- `skip_reason` (string, optional) — e.g. `dry_run`, `cooldown`, `target_free_met`
- `bytes_freed`, `estimated_bytes_freed`, `command_bytes_freed`, `host_bytes_freed` (int64); `items_cleaned` (int)
- `cooldown_remaining_seconds` (int64, optional); `error` (string, optional)
- `plan` (object, optional) — dry-run plan with `targets`, byte accounting, and warnings
  - `plan.metadata` (object of string to string, optional) — plugin-specific keys. The `dev-artifacts` scan-budget keys include:
    - `temp_scan_max_roots_scope` — always `per_temp_scan_path`: `temp_scan_max_roots` is applied to each entry of `temp_scan_paths` separately
    - `temp_scan_paths_truncated` — decimal count of temp scan paths whose own budget was exhausted this cycle; their truncations also appear in `scan_truncated_paths` and set `scan_budget_exhausted` to `true`

## Example (dry-run)

```json
{
  "timestamp": "2026-06-24T14:30:00Z",
  "dry_run": true,
  "level": "critical",
  "monitor_path": "/nix",
  "host_free_before_bytes": 10737418240,
  "host_byte_level": "none",
  "host_inode_level": "critical",
  "max_inode_level": "critical",
  "target_used_percent": 70,
  "target_free_met": false,
  "plugins": [
    { "name": "nix", "would_run": true, "skip_reason": "dry_run", "estimated_bytes_freed": 3221225472 },
    { "name": "bazel", "would_run": true, "skip_reason": "dry_run", "estimated_bytes_freed": 5368709120 }
  ],
  "planned_estimated_bytes_freed": 8589934592,
  "planned_targets": 16
}
```
