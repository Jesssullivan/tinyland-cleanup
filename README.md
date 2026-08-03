# tinyland-cleanup

A conservative disk-pressure cleanup daemon for developer machines and CI hosts.
It reclaims build-system and developer-tool caches before unmanaged disk
pressure breaks local work, runners, or hermetic builds — and stops as soon as
the target is met.

Targets: Darwin developer machines and Linux/Rocky builder and runner hosts.

## Safety model

- Dry-run output is detailed enough to review before any deletion.
- Every plan states what it will remove and why.
- Host free space and inodes are measured before and after each cycle.
- Cleanup stops once the free-space target is met and inode pressure has cleared.
- Daemon cleanup honors cooldown below the configured bypass level.
- Three consecutive zero-yield attempts open a bounded per-plugin backoff.
- State and the latest cycle receipt are atomically replaced and digest-bound.
- Privileged actions, offline compaction, and service disruption are opt-in.

## Quick start

### Home Manager (recommended)

```nix
tinyland.cleanup.enable = true;
```

Run `home-manager switch`. The daemon installs and starts itself — a launchd
agent on macOS, a systemd user service on Linux — and renders its config from
your Nix options. Tunables are in [Installation](docs/installation.md).

### Manual

```sh
nix build github:Jesssullivan/tinyland-cleanup#default
install -Dm755 result/bin/tinyland-cleanup ~/.local/bin/tinyland-cleanup
tinyland-cleanup --once --dry-run --level critical   # review with built-in defaults
```

The binary runs on built-in defaults when no config exists. To customize, copy
`config/default.yaml` to `~/.config/tinyland-cleanup/config.yaml` and edit it.
Linux hosts can install the RPM instead — see [Installation](docs/installation.md).

## Run modes

| Command | What it does | Use when |
|---|---|---|
| `--once --dry-run` | Plan only; change nothing | Reviewing a pressured host |
| `--once` | Run one cleanup cycle and exit | Cron / systemd timer on a runner |
| `--daemon` | Wait `poll_interval` after each completed cycle, keep bounded state | Workstations (via Home Manager) |

Pick one mode. Do not run `--daemon` and a `--once` cron together — their
cooldown state conflicts. Details in [Usage](docs/usage.md).

## Byte and inode pressure

Cleanup is driven by **whichever is worse**: disk bytes or filesystem inodes.
A Nix store can exhaust inodes (millions of small files) while bytes still look
fine, so an inode-only spike escalates cleanup and triggers `nix-collect-garbage`.
If inode pressure persists with no relief for `policy.inode_no_progress_limit`
cycles (default 3), the daemon backs off to the cooldown cadence instead of
churning every poll.

## Configuration

With Home Manager, set options in Nix; the generated
`~/.config/tinyland-cleanup/config.yaml` is overwritten on each switch — do not
hand-edit it. Without Home Manager, copy `config/default.yaml`, edit it, and
restart the daemon. Every key maps to a Home Manager option in
[Configuration](docs/configuration.md).

## Documentation

- [Installation](docs/installation.md) — Nix, Home Manager, RPM, source
- [Usage](docs/usage.md) — run modes, daemon lifecycle, reports, exit codes
- [Configuration](docs/configuration.md) — config keys and Home Manager options
- [Operator workflow](docs/operator-workflow.md) — dry-run review and cleanup
- [JSON report schema](docs/json-report-schema.md) — machine-readable output
- [Plugins](docs/plugins.md) — what each cleanup plugin does
- Policy — [Nix](docs/nix-cleanup-policy.md) · [Bazel](docs/bazel-cache-policy.md) · [Darwin caches](docs/darwin-dev-caches.md) · [Podman compaction](docs/podman-darwin-compaction.md)
- [Development](docs/development.md) — build and test
- Agents — [AGENTS.md](AGENTS.md) and [llms.txt](llms.txt)

## Status

Package authority is the Nix flake (`.#tinyland-cleanup`). Release archives and
RPMs are built from `v*` tags. Open work is tracked in
[GitHub issues](https://github.com/Jesssullivan/tinyland-cleanup/issues).
