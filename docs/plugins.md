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
| `dev-artifacts` | Stale `node_modules`, `.venv`, `target/`, Zig, Go, Haskell, LM Studio; compresses stale agent transcripts (zstd by default); reports large local artifacts |
| `debris-report` | Report only, off by default (`enable.debris_report`): logs stale incident/agent debris under `debris_report.scan_paths` (default patterns include YYYYMMDD-stamped names, `.bulkload-*`, `rollback/`, `*-carry/`) with sizes; never deletes |
| `archive-lifecycle` | Retires archive staging pre-images after proving every file already exists in the durable archive target — see [Archive staging lifecycle](archive-staging-lifecycle.md) |

### Podman protected images

At moderate level the `podman` plugin removes every unused image older than
`podman.prune_images_age` (`podman image prune -af --filter until=`). The age
filter reads the image's creation date, so an image built long ago is eligible
the moment no container uses it. To keep a large image that is pulled rarely
but run often, list it under `podman.protected_images`, pinned by digest:

```yaml
podman:
  protected_images:
    - docker.io/kicad/kicad@sha256:c8f05703e865a1caa7266851cf4986dd01d5fb1148e4f63382b6a73466ba7e71
```

With a non-empty list the age prune lists the images the age filter selects
(`podman images --format json --filter until=`) and removes each by name with
`podman rmi`, never `--force`. It skips protected images, images any container
uses (running or stopped) and read-only images, which is what `image prune -a`
keeps. A reference that is not present locally is ignored. The opt-in critical
`podman system prune -af --volumes` (`podman.critical_system_prune`) does not
honor the list.

## Linux

| Name | Cleans |
|---|---|
| `github_runner` | GitHub Actions runner caches and work directories |
| `yum` | DNF/YUM package caches |
| `shm_scratch` | Off by default (`enable.shm_scratch`). Level-independent: removes top-level directories of tmpfs roots (default `/dev/shm`) idle past `shm_scratch.idle_after` (never under 24h) that no process holds by cwd, root, open file, memory mapping or command line; never files, other users' trees, Git trees or agent-session data; `mode: report` plans only (lab R-C856) |
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

## Deadlines

Every plugin run is watched, and every command a plugin starts has a deadline
(see "Deadlines and the plugin watchdog" in
[configuration.md](configuration.md)). When writing plugin code:

- Start commands with `commandContext` (probes and listings),
  `reclaimCommandContext` (anything that deletes or prunes),
  `offlineCommandContext` (VM stop, disk image conversion) or
  `restartCommandContext` (VM start after a stop). Never call `exec.Command`
  or `exec.CommandContext` directly; a test fails the build if you do.
- Pass the `ctx` you were given. Wrap it in `context.WithTimeout` when a
  command needs a specific deadline; that deadline is used as given.
- Check `ctx.Err()` in long loops. A plugin that ignores its context is
  abandoned at its deadline and skipped until the stuck run returns.
- Size directories with `getDirSize` or `streamWalk`. Use `filepath.Walk`
  only when the walk deletes as it goes or depends on sorted order.
