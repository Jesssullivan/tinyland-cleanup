# Usage

## Run modes

`tinyland-cleanup` has three modes. Pick one.

| Mode | Command | Behavior |
|---|---|---|
| Review | `--once --dry-run` | Plan a cycle and exit; change nothing |
| One-shot | `--once` | Run one cleanup cycle and exit |
| Daemon | `--daemon` | Wait `poll_interval` after each completed cycle; persist bounded state |

- **Workstations**: run the daemon via Home Manager (it starts on login).
- **CI runners**: prefer `--once --level critical` from a cron or systemd timer.
- **Manual review**: `--once --dry-run --level critical --output json | jq`.

Do not run a daemon and a `--once` cron on the same host. Their cooldown state
conflicts and runs get skipped unexpectedly.

The daemon schedules from cycle completion, not from a continuously ticking
clock. A scan that takes longer than `poll_interval` therefore cannot trigger
an immediate second scan from a pending tick.

## Choosing a level

Without `--level`, the daemon derives the level from disk and inode pressure.
Force a level for a manual run:

```sh
tinyland-cleanup --once --dry-run --level critical
```

Levels escalate `warning` → `moderate` → `aggressive` → `critical`. Higher
levels reclaim more and, at critical, enable opt-in disruptive actions when
configured.

## Reading reports

Text is the default, for human review:

```sh
tinyland-cleanup --once --dry-run --level critical --output text
```

JSON is the stable machine surface:

```sh
tinyland-cleanup --once --dry-run --level critical --output json
```

`host_free_delta_bytes` (and `host_inodes_free_delta`) is the source of truth
for whether a cycle reclaimed space. Plugin byte estimates are supporting
evidence. Full field reference: [JSON report schema](json-report-schema.md).

## Inspecting and constraining plugins

```sh
tinyland-cleanup --list-plugins                 # names and enabled state
tinyland-cleanup --list-plugins --output json   # machine-readable
tinyland-cleanup --once --dry-run --level critical --plugins bazel,nix
```

`--plugins` constrains a run to named plugins. See [Plugins](plugins.md).

## Daemon lifecycle

**Linux (systemd user service):**

```sh
systemctl --user status tinyland-cleanup
systemctl --user restart tinyland-cleanup   # after editing config.yaml
journalctl --user -u tinyland-cleanup -f
```

**macOS (launchd agent):** label `dev.tinyland.cleanup`; logs at
`~/.local/log/disk-cleanup.log`. Home Manager loads and kickstarts it on switch.

## Exit codes

- `0` — success, or a dry-run completed without error
- `1` — runtime error (plugin failure, I/O error, shutdown signal)
- `2` — configuration or flag error (invalid `--level`, parse error)

## Common mistakes

- **Daemon plus a cron**: cooldown state conflicts. Use one or the other.
- **Hand-editing the Home Manager config**: it is overwritten on
  `home-manager switch`. Edit the Nix options instead.
- **Full disk, near-zero safe reclaim**: cleanup tools need some free space to
  run. Free a little manually (delete an ISO, detach a VM disk), then retry.
