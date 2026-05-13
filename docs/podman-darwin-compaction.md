# Podman Darwin Compaction

Podman on macOS runs Linux containers inside a VM. With the `applehv` provider,
the VM disk is a raw sparse file on APFS. Guest `fstrim` can report large
trimmed byte counts inside the VM, but that does not prove the host raw image
released APFS allocation. The daemon therefore does not count `applehv` guest
`fstrim` output as host bytes freed.

Critical cleanup first handles the online path when a running buildx BuildKit
container is visible. The daemon inspects `buildctl du`, prunes only when the
reported reclaimable cache is above the configured floor, keeps recent cache and
a storage floor, then runs advisory `fstrim`. The BuildKit command total is
reported as `command_bytes_freed`; host reclaim is reported only from the
measured host free-space delta after the trim.

Broad `podman system prune -af --volumes` is not part of the default critical
path. It can remove stopped-container state and unused volumes, so it is
reported as a protected disruptive target unless `podman.critical_system_prune`
is explicitly enabled.

## Review

Use dry-run JSON before enabling offline compaction:

```sh
tinyland-cleanup --once --dry-run --level critical --output json
```

The Podman plan reports:

- provider and running state;
- stopped-machine VM disk metadata when the Podman CLI is installed but the
  Podman API socket is unavailable;
- BuildKit builder cache container, reclaimable bytes, retention policy, and
  protected skip reason when the cache is below threshold or cannot be
  inspected;
- disk format and disk path;
- scratch directory, temp image path, and rollback backup path;
- logical image size and physical host allocation;
- required temporary free space based on physical allocation plus headroom;
  cross-device scratch also accounts for the rollback backup copy;
- active-container status;
- whether `qemu-img` is available and which executable path will be used;
- protected targets for blocked VM disk compaction, scratch-space requirements,
  and active-container quiescence;
- the exact stop, convert, verify, replace, and start sequence.

`estimated_bytes_freed` is counted only when the offline compaction preflight
can run. Blocked compaction still reports the potential reclaim in
`offline_compaction_estimated_reclaim_bytes` and on protected targets for
operator review.

## Enable

BuildKit cache pruning is enabled by default only for critical Podman cleanup:

```yaml
podman:
  buildkit_prune: true
  buildkit_prune_keep_duration: 24h
  buildkit_prune_keep_storage_mb: 8192
  buildkit_prune_min_reclaim_gb: 4
  critical_system_prune: false
```

This does not stop the Podman VM or delete the builder volume wholesale. It is
still warm-cache cleanup: active builds may lose reusable cache, so the daemon
keeps the newest records and a configured storage floor.

Set `critical_system_prune: true` only for an attended emergency pass after
reviewing stopped containers and unused volumes. This enables broad
`podman system prune -af --volumes`, external storage pruning when supported,
and the matching critical VM system-prune path.

Offline compaction is opt-in because it stops the Podman machine:

```yaml
podman:
  compact_disk_offline: true
  compact_min_reclaim_gb: 8
  compact_require_no_active_containers: true
  compact_keep_backup_until_restart: true
  compact_scratch_dir: ""
  compact_qemu_img_path: ""
  compact_provider_allowlist:
    - applehv
    - libkrun
    - qemu
```

The preflight skips compaction when the disk path is outside expected Podman
machine directories, `qemu-img` is unavailable, active containers are running,
the provider is unknown, a rollback backup already exists, or the scratch
filesystem does not have enough physical free space for the compacted copy and
any required rollback backup.

When the machine is stopped, online pruning is skipped. Critical dry-run still
reports the raw VM image path, logical size, physical allocation, scratch
requirement, and protected offline-compaction target when the local Podman
machine config can be inspected. That path is evidence only unless offline
compaction is explicitly enabled and all preflight gates pass.

`compact_scratch_dir` may point at a reviewed scratch directory when the default
VM disk directory cannot hold the temporary compacted image. Same-filesystem
scratch uses filesystem renames for replacement. Cross-device scratch uses a
copy-and-verify flow: write the compacted image to scratch, write a rollback
backup to scratch, remove the original VM disk after backup succeeds, then write
the compacted image back to the original disk path with `qemu-img`. That path
requires `compact_keep_backup_until_restart: true` and enough scratch space for
both the compacted image and rollback backup.

`compact_qemu_img_path` is optional. Set it only when the daemon environment does
not have `qemu-img` on `PATH`, for example when operators intentionally provide
the executable from a Nix profile or wrapped package.

When `active_containers` or `insufficient_free_space` appears, treat the plan as
a quiescence or scratch-capacity task. Do not force compaction on an active
developer VM just because the raw image has large potential reclaim.

## Rollback Boundary

When `compact_keep_backup_until_restart` is enabled, the daemon preserves the
original disk image as a `.backup` file until the compacted image starts
successfully. If restart fails, it restores the original disk and attempts to
start the machine again. Host bytes freed are reported from physical allocation
before and after compaction, not from the raw image's logical size.
