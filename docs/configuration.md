# Configuration

`tinyland-cleanup` reads YAML from `--config`, defaulting to
`~/.config/tinyland-cleanup/config.yaml`. Without a config file, it runs with
built-in conservative defaults.

Home Manager users should configure `tinyland.cleanup.*` options in Nix. The
rendered YAML is managed by Home Manager and overwritten on each switch.

## Pressure thresholds

Byte thresholds and inode thresholds are independent. Cleanup escalates to the
highest level triggered by either signal.

```yaml
thresholds:
  warning: 70
  moderate: 80
  aggressive: 90
  critical: 95

inode_thresholds:
  warning: 80
  moderate: 85
  aggressive: 90
  critical: 95
```

On filesystems that allocate inodes dynamically (XFS, APFS, ZFS, Btrfs) the
used percentage is not a pressure signal, and the percentage inode ladder is
skipped. Set `inode_free_floor` (an absolute free-inode count) to get inode
escalation there: fewer free inodes than the floor is critical, otherwise none.
A floor, global or per mount, replaces the ladder on every filesystem.

```yaml
inode_free_floor: 2000000
```

Per-mount overrides use `monitored_mounts`:

```yaml
monitored_mounts:
  - path: /nix
    label: nix-store
    threshold_warning: 70
    threshold_critical: 85
    threshold_inode_warning: 70
    threshold_inode_critical: 90
    inode_free_floor: 1000000
```

## Cleanup policy

```yaml
policy:
  cooldown: 6h
  cooldown_bypass_level: aggressive
  minimum_free_gb: 25
  state_file: ~/.local/state/tinyland-cleanup/state.json
```

- `cooldown` avoids repeated cleanup churn.
- `cooldown_bypass_level` lets urgent pressure bypass cooldown.
- `minimum_free_gb` keeps cleaning until the host reaches a free-space runway.
- `state_file` stores cooldown and no-progress state.

### Byte backoff

```yaml
policy:
  byte_no_progress_limit: 3
  byte_progress_min_mb: 256
  byte_backoff_max: 30m
  emergency_free_gb: 20
```

Under byte pressure, critical level and an unmet `minimum_free_gb` runway both
bypass cooldown, so every plugin runs every poll interval. When nothing is
reclaimable that becomes a scan storm. Byte backoff stops it:

- A cleanup cycle under byte pressure counts as **no progress** when plugins
  freed less than `byte_progress_min_mb` MiB, the host free-space delta is
  below the same amount, and the byte level did not drop.
- After `byte_no_progress_limit` consecutive no-progress cycles, byte backoff
  engages. Each plugin then runs at most once per `min(cooldown,
  byte_backoff_max)`, or once per `byte_backoff_max` when no cooldown is set.
  Skipped plugins report `skip_reason: byte_backoff` and `retry_at`.
- Cycles in which every plugin was held back do not advance the counter.
- Backoff releases on progress, when the byte level rises (an escalation earns
  a fresh attempt), and when pressure clears.
- It never engages while free space is below `emergency_free_gb` GiB, for
  `--level` runs, or for `--dry-run`.
- The counter lives in `state_file`, so it survives daemon restarts.
- `byte_no_progress_limit: 0` uses the default of 3; a negative value disables
  byte backoff. `emergency_free_gb: 0` disables the floor.

### Zero-yield suppression

```yaml
policy:
  zero_yield_limit: 2
  zero_yield_backoff_base: 30m
  zero_yield_backoff_max: 6h
  zero_yield_exempt_plugins: []
```

Byte backoff spaces out every plugin; zero-yield suppression stops rerunning
the particular expensive passes that keep finding nothing:

- A plugin run that reclaims less than `byte_progress_min_mb` MiB (or fails)
  is a **zero-yield** run. A run that reclaims at least that much resets the
  count.
- After `zero_yield_limit` consecutive zero-yield runs the plugin is
  suppressed for `zero_yield_backoff_base`. Each further zero-yield run doubles
  the interval, capped at `zero_yield_backoff_max`. Suppressed plugins report
  `skip_reason: zero_yield_backoff` and `retry_at`; the cycle reports
  `next_retry_at`.
- Suppression lifts early, and the report names why in `zero_yield_lifted`,
  when conditions change: the cleanup level rises above the plugin's last run
  (`level_rose`), the configuration or binary version changes, for example
  after a Home Manager switch (`config_changed`), or free space is below
  `emergency_free_gb` (`below_emergency_floor`). When pressure clears, every
  suppression is lifted and each plugin gets one fresh run in the next
  episode; the count is kept, so a plugin that still yields nothing is
  suppressed again at once.
- It never applies to `--level`, `--dry-run` or `--plugins` runs
  (`operator_run`), to plugins in `zero_yield_exempt_plugins`, or to
  safety-critical plugins (`apfs-snapshots`).
- The count and retry time live in `state_file`, so they survive restarts.
- `zero_yield_limit: 0` uses the default of 2; a negative value disables
  suppression.

## Plugins

The `enable` map controls plugin availability:

```yaml
enable:
  cache: true
  nix_gc: true
  bazel: true
  docker: false
  podman: false
  dev_artifacts: true
```

Use `tinyland-cleanup --list-plugins --output json` to inspect the effective
plugin set for a host. Plugin-specific keys are documented in
[Plugins](plugins.md) and the policy docs ([Nix](nix-cleanup-policy.md),
[Bazel](bazel-cache-policy.md), [Darwin caches](darwin-dev-caches.md)).

## Source of Truth

- Example config: [`config/default.yaml`](https://github.com/Jesssullivan/tinyland-cleanup/blob/main/config/default.yaml)
- Typed schema: [`config/config.go`](https://github.com/Jesssullivan/tinyland-cleanup/blob/main/config/config.go)
- Home Manager integration lives in the consuming `lab` repo; this upstream
  repo owns the runtime YAML contract.
