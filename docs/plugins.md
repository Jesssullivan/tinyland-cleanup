# Plugins

Each cleanup family is a plugin with a stable name. Use the name with
`--plugins` to constrain a run, and `--list-plugins --output json` to see live
names, enabled state, and platform support on the current host.

```sh
tinyland-cleanup --list-plugins
tinyland-cleanup --once --dry-run --level critical --plugins bazel,nix
```

Enabled-by-default state and which plugins register depend on the platform and
your config. The list below is the full registry.

## Cross-platform

| Name | Cleans |
|---|---|
| `docker` | Docker images, containers, volumes, networks, build cache |
| `podman` | Podman images, containers, volumes, build cache, VM disk space |
| `nix` | Nix garbage collection with generation and daemon-contention safeguards — see [Nix policy](nix-cleanup-policy.md) |
| `bazel` | Stale Bazel output bases; reports repository, disk, and Bazelisk cache policy — see [Bazel policy](bazel-cache-policy.md) |
| `cache` | Application caches (pip, npm, go, and similar); on Darwin this also owns typed IDE/tool caches such as JetBrains, VS Code, Cursor, pip, npm, uv, and bun |
| `gitlab-runner` | GitLab runner caches, build directories, stale artifacts |
| `dev-artifacts` | Stale `node_modules`, `.venv`, `target/`, Zig, Go, Haskell, LM Studio; reports large local artifacts |

## Linux

| Name | Cleans |
|---|---|
| `github_runner` | GitHub Actions runner caches and work directories |
| `yum` | DNF/YUM package caches |
| `etcd` | Old etcd snapshots and WAL files; defrag when needed (disabled by default) |
| `rke2` | RKE2/k3s containerd images, old pod logs, kubelet garbage (disabled by default) |

`etcd` and `rke2` are Kubernetes plugins kept off by default; enable them only on
control-plane or node hosts that need them.

## Darwin

| Name | Cleans |
|---|---|
| `homebrew` | Homebrew caches and old formula versions |
| `xcode` | Xcode DerivedData, archives, device support |
| `ios-simulator` | iOS Simulator devices and runtimes |
| `lima` | Lima VMs and disk resize — see [Podman/VM compaction](podman-darwin-compaction.md) |
| `apfs-snapshots` | APFS local snapshots and Time Machine caches |
| `icloud` | Evicts downloaded iCloud Drive files to free local storage |
| `photos` | Photos library analysis caches (never touches originals) |

Plugin behavior at each level and the protection rules are described in the
[operator workflow](operator-workflow.md) and the per-plugin policy docs.
