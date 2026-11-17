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
| `retention` | Off by default (`enable.retention`). Level-independent age retention (TIN-7495, TIN-3082): stale `tmp_agents` roots (`nix-shell.*` with their `pytest-of-<user>` subtrees), stale Cargo build directories, a per-volume free-space floor alert and report-only Darwin leftovers. Every deleting rule reports only until its own `delete` flag is true. See [Retention](#retention) |

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
keeps. A reference that is not present locally is ignored.

### Podman keep rule

Three things are never pruned (TIN-5770). On sting the plugin removed a
mail-lacein harness run's pinned images, networks and volumes between runs.

1. An image, network or volume that any container uses, in any state.
   Podman's prunes skip those. At the aggressive level the plugin prunes
   volumes and networks before it prunes stopped containers, and it no longer
   runs `podman system prune -f --build`, which removed every stopped
   container and then their networks.
2. Anything that carries a label in `podman.protected_labels`, given as a key
   (any value) or as `key=value`. Image and network prunes get a
   `--filter label!=<entry>` per label. Container and volume prunes do not:
   on podman 5.8 several `label!=` filters there keep only resources that
   carry every label. So the plugin lists stopped containers
   (`podman ps -a`) and unused volumes (`volume ls --filter dangling=true`),
   skips any with a protected label, and removes each one with `podman rm`
   or `podman volume rm`, never `--force`. `image prune --build-cache` is no
   longer run: it takes no label filter.
3. The images in `podman.protected_images` and, one per line, in
   `podman.protected_images_file`. `#` starts a comment. The file is read on
   every cycle, so a harness can write its pins without a config change. A
   missing file adds nothing but still turns the keep rule on. The
   prune-by-use pass below skips instead. A file that cannot be read, a
   dangling symlink or anything but a regular file skips the age prune for
   that cycle. So does a pin that `podman image inspect`
   fails on for any reason other than "image not known". A pin with a
   digest is also asked as
   `podman images --all --no-trunc --quiet --filter digest=<digest>`, and
   each image podman returns is kept. Podman answers from every digest it
   stored for the image, under any name or none. An error, a line that is
   not `sha256:<64 hex>`, or an `@` part that is not a sha256 or sha512
   digest skips the age prune. While any image pin or keep file is
   configured, the dangling-image prune (`podman image prune -f`) does not
   run: it takes no reference exclusion, and an image whose tag moved to a
   newer pull is dangling while a digest pin still names it. The age prune
   and the prune-by-use pass still remove old, unpinned dangling images.

   A pin by tag keeps the image the tag names now. Once the tag moves, the
   image it named before is not kept by that pin. Pin by digest to keep a
   given image.

```yaml
podman:
  protected_labels:
    - io.xoxd.lacein-harness
  protected_images_file: ~/.config/tinyland-cleanup/keep-images
```

While any of the three is set, the opt-in critical
`podman system prune -af --volumes` (`podman.critical_system_prune`) is
replaced by the targeted aggressive prunes, and the Darwin in-VM cleanup
(unfiltered prunes over `podman machine ssh`) is skipped; the host client's
targeted prunes already reach the VM's store.

`TINYLAND_PODMAN_STORE_TEST=1 go test ./plugins -run ThrowawayStore` checks
the rule against a real podman with a throwaway store under `$TMPDIR`.

### Podman prune by use

`podman.prune_by_use` (TIN-8526) is `off` by default. It acts only on Linux
with rootless podman. Darwin and the Podman machine paths never reach it.

| Mode | Effect |
|---|---|
| `off` | Nothing changes. |
| `report` | Logs each image and volume the pass would remove. Removes nothing. The age prune runs as before. |
| `apply` | Removes them. Replaces the moderate age prune and the aggressive dangling-volume prune. |

The pass runs at the moderate and aggressive levels. It also runs at
critical whenever the broad system prune does not: `critical_system_prune`
is off, or the keep rule sends critical through the aggressive passes. Before this, a host held
at critical removed no unused image (`plugins/podman.go`, `cleanCritical`).

An image is removed when all of these hold:

- `podman images` lists it with a full ID and `Containers: 0`.
- No pin in `protected_images` or `protected_images_file` names it. A pin
  counts when `podman image inspect` resolves it to the image's ID, or when
  it equals one of the image's names or digests. A pin's digest counts too:
  the part after `@`, a bare digest, or a bare 64-hex pin read as sha256.
  It keeps the image when it equals the image ID, the listed `Digest`, any
  listed `Digests`, or the digest of any `RepoDigests` entry, whatever the
  repository name. It also keeps each image that
  `podman images --filter digest=` returns for it. That covers an image
  with no name (`Names` null, `RepoDigests` empty) and a manifest-list
  digest.
- It carries no label in `protected_labels` and is not read-only.
- It is older than `prune_images_age`. The age runs from the later of its
  build time and the time it entered the local store. The store time is the
  modification or change time of `<graphroot>/<driver>-images/<id>`. A
  missing directory, or one that is not a plain directory, keeps the image.

Podman records no last use. An image a container used after its pull, where
that container is now gone, is eligible once the age has passed. A container
that still exists, in any state, blocks the removal.

A dangling volume (`podman volume ls --filter dangling=true`) is removed when
its driver is `local`, it carries no protected label and its `CreatedAt` is
older than `prune_images_age`. A volume whose last container was removed a
minute ago is eligible when the volume itself is old. Protect such volumes
with a label.

Just before each removal the pass asks `podman image inspect` or `podman
volume inspect` again and re-reads the keep file. It keeps the item when the
answer differs: a new protected label, a recreated volume, a store time
inside the age, or a keep file that changed during the pass. It then runs
`podman rmi <id>` or `podman volume rm <name>` without `--force`. Podman
refuses an image or volume a container uses at that moment.

An empty keep file is a choice the pass cannot tell from a lost one, so it
runs and logs `podman keep file holds no pins` at Warn with `pins=0`. The
dry-run plan carries the same warning.

The pass skips the whole cycle when the keep file is configured but missing
or cannot be read, `podman image inspect` of a pin fails for any reason but
"image not known" or resolves to anything but a full image ID, the digest
question for a pin fails or gives a line that is not a full image ID, a
pin's `@` part is not a digest the pass can compare, `podman info` gives an
unexpected answer, podman is not rootless, a listing does not parse or
`prune_images_age` is under 1h. In `apply` mode the prunes it
replaces do not run in that cycle either.

`--dry-run` plans the pass and removes nothing. The plan names each item
with `delete_podman_image`, `delete_podman_volume`, `report` or `keep`, and
gives the planned bytes. Image bytes are listed sizes, which count shared
layers once per image. Volume bytes count only the files the daemon can
read. The measured host free-space delta remains the authority.

```yaml
podman:
  prune_images_age: 24h
  prune_by_use: apply
  protected_images_file: ~/.config/tinyland-cleanup/keep-images
  protected_labels:
    - io.xoxd.lacein-harness
```

### Retention

`retention` owns age retention for the temp, cache and build output that
filled sting's `/srv/fast-local/tmp-agents` quota and pzm's data volume
(TIN-7495, TIN-3082). It runs every cycle outside the pressure ladder,
throttled by `retention.interval` (default 1h); `--dry-run` always plans.

| Rule | Removes | Default |
|---|---|---|
| `tmp_agents` | Top-level directories of `roots` matching `name_globs` (default `nix-shell.*`) whose newest mtime or ctime inside is older than `max_age` (24h) | report only (`delete: false`) |
| `cargo_targets` | Cargo target or build-dir trees (`CACHEDIR.TAG` or `.rustc_info.json`) under `roots` (default `/srv/cache/{user}/cargo/build`, `max_depth` 3) whose newest change is older than `max_age` (7d); the whole rule waits while any `cargo` or `rustc` process runs | report only (`delete: false`) |
| `free_space_floor` | Nothing: an `ALERT free-space floor` line for every volume under `floor_gib` (10). Checks `paths` (`/`, and `/System/Volumes/Data` on Darwin), every `monitored_mounts` path and every retention root | alert |
| `darwin_leftovers` | Nothing: sizes of Fusion webdeploy builds under `fusion_webdeploy_root` that are not the target of its `Autodesk Fusion.app` symlink, and of each `report_paths` entry (the Claude desktop `vm_bundles`) | report only, no delete rule |

A tree is kept when any of these hold, and every check runs again
immediately before removal:

- a live reference: a process has its cwd or root there, an open file or a
  memory mapping there, names it on its command line, or has `TMPDIR`,
  `TMP`, `TEMP` or `TEMPDIR` pointing inside it (Linux reads
  `/proc/<pid>/environ`, Darwin `kern.procargs2`);
- the process inventory is incomplete (a same-user process whose fds or
  environment cannot be read, other than `uninspectable_ok_comms` and the
  units in `uninspectable_ok_units`);
- it belongs to another user, crosses a mount, or contains `.git` or
  agent-session data (TIN-5268). One exception, `tmp_agents` only (operator
  ruling TIN-7495 2026-10-29): a test fixture repository may go with its
  stale tree. That is a real `.git` directory below a path element matching
  `tmp_agents.fixture_repo_parents` (default `pytest-of-*`) whose config has
  no `[remote]` and no `[include]` section and that has no linked work trees.
  A `.git` file, a remote, a repository anywhere else, or an empty
  `fixture_repo_parents` keeps the tree;
- it is under `dev_artifacts.protect_paths`, holds files a Git repository
  tracks, or sits in a work tree with uncommitted changes, unpushed commits
  or no upstream;
- its walk exceeds `max_entries_per_dir` / `max_entries_per_target`.

Sizes are allocated blocks, so a sparse file counts what it occupies; the
apparent size is in `logical_bytes`. Plan metadata carries per-rule
`eligible_count`, `eligible_bytes`, `eligible_logical_bytes`, `live_count`,
`kept_count` and the floor reading of every volume.

`dev_artifacts.nix_temp_roots` reaps stale Nix temp roots at aggressive or
critical level, judged by the root's own mtime, cwd/fd holders, the newest
modification inside and the Git state of checkouts inside (TIN-8526, see
[Configuration](configuration.md#temporary-scan-paths)). `tmp_agents` is the
level-independent rule that also reads `TMPDIR` and memory maps.

The Bazel `disk_cache` age rule lives in the `bazel` plugin: see
[Bazel policy](bazel-cache-policy.md#disk-cache-age-retention).

## Linux

| Name | Cleans |
|---|---|
| `github_runner` | GitHub Actions runner caches and work directories |
| `yum` | DNF/YUM package caches |
| `shm_scratch` | Off by default (`enable.shm_scratch`). Level-independent: removes top-level directories of tmpfs roots (default `/dev/shm`) idle past `shm_scratch.idle_after` (never under 24h) that no process holds by cwd, root, open file, memory mapping or command line; never files, other users' trees, Git trees or agent-session data; `mode: report` plans only (lab R-C856) |
| `home_lanes` | Off by default (`enable.home_lanes`), report mode by default. Removes direct children of `home_lanes.cache_root` that match `cache_lane_globs`, and of `hermes_installs_root` whose checkout in `inputs/.project-root` is gone under `hermes_checkout_roots`. A tree must be idle past its window (floors 72h and 24h), with no `.git`, no agent-session data, no process holding it and no kernel file lock on any of its files. A third kind, off until `stale_work_roots` names a root such as `~/build`, takes direct entries (directories or regular files) of that root when no entry under it was modified within `stale_work_idle_after` (default 14d, floor 7d). A `.git` there does not keep the entry by itself: each Git checkout at any depth inside is asked whether it has uncommitted or untracked changes, a stash, linked work trees, or commits from HEAD, a local branch or a local tag missing from every remote-tracking ref. The refs are local, so a remote repository deleted after the last fetch still reads as holding its commits. Any yes, a git that fails or runs past its timeout, a bare repository, `.hg` or `.jj` keeps the entry, and the agent-session, holder and file lock rules above still apply (TIN-8526). `uninspectable_ok_units` (default empty) lists systemd user units whose unreadable processes do not keep every tree, matched exactly on the cgroup v2 unit as `dev_artifacts.temp_uninspectable_ok_units` is. Each entry asserts the unit holds nothing under these roots. The temp list is not read here. Apply mode removes at moderate level or higher, at most `max_removals_per_cycle` trees a cycle, largest first. With `free_target_gb` above 0 it reads the root's free space before each removal and stops on that mount once the target is reached or free space cannot be read (lab A-082, TIN-8526) |
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
