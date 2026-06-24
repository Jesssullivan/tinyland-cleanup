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

Per-mount overrides use `monitored_mounts`:

```yaml
monitored_mounts:
  - path: /nix
    label: nix-store
    threshold_warning: 70
    threshold_critical: 85
    threshold_inode_warning: 70
    threshold_inode_critical: 90
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
[Plugins](plugins.md) and the policy docs under [README](../README.md).

## Source of Truth

- Example config: [`config/default.yaml`](../config/default.yaml)
- Typed schema: [`config/config.go`](../config/config.go)
- Home Manager integration lives in the consuming `lab` repo; this upstream
  repo owns the runtime YAML contract.
