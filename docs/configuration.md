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

A mount can carry its whole byte ladder and its own target. This suits a
directory tree with an XFS project quota, where statfs reports the project's
soft limit as the size and the tree's own ruled lines are what matter, not
the volume's:

```yaml
monitored_mounts:
  - path: /srv/fast-local/jess
    label: devseat-177
    threshold_warning: 90
    threshold_moderate: 92
    threshold_aggressive: 95
    threshold_critical: 98
    target_used_percent: 92
```

- Set thresholds must ascend (warning <= moderate <= aggressive <=
  critical); a descending ladder is a config error.
- A global moderate or aggressive step applies to a mount only when it falls
  inside the mount's own band (its warning to its critical). Outside the band
  the mount has no such step. A mount with warning 92 and critical 98 is
  therefore report-only from 92 to 98, not moderate from the global 85.
- `target_used_percent` replaces `target_free` while that mount is the
  cycle's monitored path. `--target-used-percent` replaces both.

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

- A cleanup cycle under byte pressure counts as **no progress** when the
  ladder plugins freed less than `byte_progress_min_mb` MiB, the byte level
  did not drop, and, in a cycle where a ladder plugin ran, the host
  free-space delta is below the same amount.
- Ladder plugins are every plugin that is not level-independent. Bytes a
  level-independent plugin frees (the nix plugin's GC of `/nix`,
  `bazel_bases`) never count: those plugins run every cycle whatever the
  level, so their yield says nothing about whether the gated plugins help.
  A free-space delta in a cycle where every ladder plugin was held back is
  another writer's churn and does not count either.
- After `byte_no_progress_limit` consecutive no-progress cycles, byte backoff
  engages. Each plugin then runs at most once per `min(cooldown,
  byte_backoff_max)`, or once per `byte_backoff_max` when no cooldown is set.
  Skipped plugins report `skip_reason: byte_backoff` and `retry_at`.
- Every further ladder attempt that makes no progress doubles that interval,
  up to `byte_backoff_max`; an attempt that makes progress resets it. A
  mount whose deficit the enabled plugins cannot touch converges to one
  attempt per `byte_backoff_max`. With the default `byte_backoff_max` (30m,
  the default cooldown) the interval does not grow; raise it (for example to
  6h) to let it.
- Cycles in which no ladder plugin ran do not advance the counter.
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
  safety-critical plugins (`apfs-snapshots`). Exempt and safety-critical
  plugins still report `zero_yield_count` as evidence but are never given a
  `suppressed_until`, so they do not drive `next_retry_at`.
- While a lift recurs every cycle (`operator_run`, `below_emergency_floor`,
  `free_unknown`), re-extending a plugin's suppression logs at debug level
  only, so the log stays bounded.
- The count and retry time live in `state_file`, so they survive restarts.
- `zero_yield_limit: 0` uses the default of 2; a negative value disables
  suppression.

### Deadlines and the plugin watchdog

```yaml
policy:
  plugin_timeout: 45m
  plugin_timeouts: {}        # per plugin, e.g. {nix: 90m}
  exec_timeout: 60s
  exec_timeouts: {}          # per plugin, e.g. {cache: 20s}
  exec_reclaim_timeout: 15m
```

Nothing the daemon starts can hold it indefinitely:

- **Commands.** A probe or listing that sets no deadline of its own gets
  `exec_timeout`; a command that deletes or prunes gets
  `exec_reclaim_timeout`. A VM stop or disk image conversion gets 2h and a VM
  start 10m; those two are fixed. At the deadline the child is sent SIGTERM,
  then SIGKILL 5s later. The plugin report lists it under `wedged_commands`
  as `wedged:<command>`, and the same program is not started again in that
  plugin run. Deadlines a plugin sets itself (`nix.max_gc_duration`, the
  Homebrew and Docker limits, the 5-10s process and git probes) are used as
  they are.
- **Plugins.** Each plugin run has `plugin_timeout`. At the deadline its
  context is cancelled, which ends its commands; a plugin that then returns
  is reported with `wedged: deadline_cancelled`. One that does not return
  within 30s is reported with `wedged: deadline_abandoned` and the cycle
  continues with the next plugin. It is skipped with
  `skip_reason: plugin_wedged` until the stuck run returns.
- Empty or invalid values use the defaults shown. Set `plugin_timeouts` for a
  plugin whose healthy run is long on your host, for example `nix` with a
  `max_gc_duration` above 30m.

### Memory

```yaml
policy:
  memory_limit_mb: 256
```

The daemon sets this as its Go soft memory limit and returns freed heap to
the OS after every cycle. `GOMEMLIMIT` in the environment takes precedence;
a negative value sets no limit. The process itself peaks at roughly 60 to
110 MiB on a large developer host.
A service manager's memory figure for the daemon is usually much larger,
because the kernel charges the directory-entry and inode cache built by the
scans to the service's cgroup. That cache is reclaimable. Size a service
memory cap for it (lab uses `MemoryHigh` below `MemoryMax`), not for the
heap.

### Bounded repeated logs

```yaml
policy:
  log_repeat_window: 1h
```

In daemon mode, a pinned host otherwise repeats the same lines every poll.
The daemon bounds that:

- A log line is emitted when its content changes and otherwise at most once
  per `log_repeat_window`. The restated line carries `repeats_suppressed=N`,
  the number of copies held back. Error lines are restated at least every
  15 minutes.
- A line's identity is its level, message and its `plugin`, `path`, `mount`,
  `label` or `instance` attributes. Measurements that drift every cycle
  (`free_gb`, `used_percent`, `inodes_free`, the free-space deltas) are not a
  change, so the `disk status` line is logged when a level changes, not every
  poll. A new `reason` or `retry_at` is a change and is logged at once.
- A cycle report whose levels, reasons, backoff state, retry times and plugin
  outcomes match the last full report is written as one line (see the JSON
  report schema). The full report is written on any change and at least once
  per window.
- Debug lines (`--verbose`) and operator runs (`--once`, `--level`,
  non-daemon `--dry-run`) are never throttled.
- `0s` disables throttling. The throttle lives in memory, so a restart logs
  everything once.

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

## Temporary scan paths

The `dev_artifacts` temp lane walks the top-level entries of each
`temp_scan_paths` directory. It reaps stale `nix-shell.*`, `nix-develop-*`,
`nix-<n>-<n>` and `nix-build-*` roots at aggressive or critical level, prunes
rebuildable output inside other idle roots, and reaps released roots that carry
a custody receipt. Agent-session trees (`claude-*`, `codex*`, `.tmp*`, `pi-*`)
directly under a temp scan path, the host temp directories and `$TMPDIR` are
always protected, whatever the configuration.

```yaml
dev_artifacts:
  temp_scan_max_roots: 128
  temp_uninspectable_ok_comms: [tailscaled, "(sd-pam)", sd-pam, systemd]
  nix_temp_root_min_mb: 1
  nix_temp_root_stale_after: 24h
```

- `temp_scan_max_roots` bounds the roots one cycle inspects in each temp scan
  path. Roots are taken oldest first (by modification time, then by name), so
  a path with more roots than the budget reclaims its oldest stale roots this
  cycle and the rest in later cycles. The cut is reported in
  `temp_scan_paths_truncated` and `scan_truncated_paths`; the later temp paths
  still run.
- A root that a same-user process holds, as its cwd or through an open file,
  is never removed. On Linux the lane reads `/proc/<pid>/cwd` and
  `/proc/<pid>/fd` for every process of the daemon's user. If it cannot read
  them for one process, it does not know what that process holds, so the
  inventory is incomplete and every temp root of every temp scan path is kept
  that cycle ("preserving temporary scan path because activity inventory is
  incomplete").
- `temp_uninspectable_ok_comms` lists the exceptions: command names (the
  `comm` field of `/proc/<pid>/stat`) of same-user processes the kernel does
  not let the user inspect and that never work inside a temp root. The default
  covers the tailscaled SSH incubator (`tailscaled be-child ssh`), the systemd
  user manager and `(sd-pam)`. A zombie (state `Z`) holds nothing and is always
  ignored. What a listed process does expose is still recorded. Setting the
  key replaces the default list. `sshd-session` is deliberately not listed:
  it is unreadable only while a session is set up, and it may hold a
  forwarded agent socket under `/tmp`. Darwin reads the inventory with `lsof`
  and does not use this list. `bazel_bases.uninspectable_ok_comms` is the same
  rule for the Bazel output-base lane.
- `nix_temp_root_min_mb` is the size floor for reaping a stale Nix root. `0`
  sets no floor, so empty stale roots are reaped too; they cost an inode each.
  A negative value means the 1 MB default.

## Source of Truth

- Example config: [`config/default.yaml`](https://github.com/Jesssullivan/tinyland-cleanup/blob/main/config/default.yaml)
- Typed schema: [`config/config.go`](https://github.com/Jesssullivan/tinyland-cleanup/blob/main/config/config.go)
- Home Manager integration lives in the consuming `lab` repo; this upstream
  repo owns the runtime YAML contract.
