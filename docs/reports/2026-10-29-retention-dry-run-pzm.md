# Retention dry run on pzm, 2026-10-29

Report-only run of the `retention` plugin and the `bazel` disk_cache age rule
(TIN-7495, TIN-3082), operator ruling 2026-10-29 "Fix tinyland-cleanup first".
Nothing was deleted: `--dry-run`, and every delete flag was false. No service
was deployed or restarted; the running daemon was not touched.

- Started: 2026-10-29T08:21:51Z
- Binary: built from `259ba786577f2e72d30ed00aeb2c082193bd5dc7` (PR #168 head, not yet merged), `go build` with `-mod=vendor`
- Command: `taskpolicy -b nice -n19 ./tinyland-cleanup --once --dry-run --level warning --plugins retention,bazel --output json --config pzm-dryrun.json`
- Config: pzm's live `~/.config/tinyland-cleanup/config.yaml` plus `enable.retention: true`, `retention.tmp_agents.roots: [/nix/var/tinyland/user/client-tmp, /private/tmp]` (pzm's `TMPDIR` is `/nix/var/tinyland/user/client-tmp`; the compiled Darwin default has no tmp root), `max_dirs_per_cycle: 0`, the default Cargo root (absent on pzm), and `bazel.disk_cache_paths: [/Volumes/BazelCache/disk-cache]` (the `--disk_cache` in `~/.bazelrc`). Every delete flag false.
- Host: `/System/Volumes/Data` had 34 GiB free before the run (stop line: 10 GiB); wall clock 13.7 s.

## Highlights

- `tmp_agents`: 758 stale `nix-shell.*` roots would be removed, 5.03 GiB allocated (4.72 GiB apparent). Two roots hold 0.88 GiB each.
- 7 roots have a live `TMPDIR` holder; all 7 are kept (`live_count` 7, cross-checked against `ps eww` at run time).
- 14 stale roots are kept for `.git` (5) or agent-session `.claude` directories (9); 435 changed within 24h.
- Darwin leftovers (report only): the superseded Fusion build `127ae56f...` is 6.7 MB; the current build `c139e273...` is 11.2 GiB. `~/Library/Application Support/Claude/vm_bundles` is absent.
- Bazel `/Volumes/BazelCache/disk-cache`: 598 entries (0.1 GiB), none older than 14 days.
- No Cargo build root exists on pzm.
- No volume is under the 10 GiB floor (`/System/Volumes/Data` 34.3 GiB free, `/nix` 34.4 GiB free).

## Plan detail

Generated from the cycle report JSON (`--output json`). Byte counts are
allocated blocks; apparent sizes are in the eligible-tree tables.

### retention plan metadata

| key | value |
|---|---|
| `cargo_targets.delete` | false |
| `cargo_targets.eligible_bytes` | 0 (0.00 GiB) |
| `cargo_targets.eligible_count` | 0 |
| `cargo_targets.eligible_logical_bytes` | 0 (0.00 GiB) |
| `cargo_targets.inspected` | 0 |
| `cargo_targets.kept_bytes` | 0 (0.00 GiB) |
| `cargo_targets.kept_count` | 0 |
| `cargo_targets.live_count` | 0 |
| `cargo_targets.missing_root./srv/cache/jsullivan2/cargo/build` | true |
| `darwin_leftovers.absent./Users/jsullivan2/Library/Application Support/Claude/vm_bundles` | true |
| `darwin_leftovers.fusion_current_build` | c139e2731e42a0340acc3d34de2cbdb01237a27c |
| `darwin_leftovers.fusion_superseded_bytes` | 7004160 (0.01 GiB) |
| `free_space_floor./` | free_bytes=36724011008 total_bytes=245107195904 below_floor=false |
| `free_space_floor./System/Volumes/Data` | free_bytes=36724011008 total_bytes=245107195904 below_floor=false |
| `free_space_floor./nix` | free_bytes=36908105728 total_bytes=170000003072 below_floor=false |
| `free_space_floor./nix/var/tinyland/user/client-tmp` | free_bytes=36908105728 total_bytes=170000003072 below_floor=false |
| `free_space_floor./private/tmp` | free_bytes=36724011008 total_bytes=245107195904 below_floor=false |
| `free_space_floor.floor_gib` | 10 |
| `tmp_agents.delete` | false |
| `tmp_agents.eligible_bytes` | 5399343104 (5.03 GiB) |
| `tmp_agents.eligible_count` | 758 |
| `tmp_agents.eligible_logical_bytes` | 5068176015 (4.72 GiB) |
| `tmp_agents.inspected` | 1207 |
| `tmp_agents.kept_bytes` | 59858944 (0.06 GiB) |
| `tmp_agents.kept_count` | 442 |
| `tmp_agents.live_count` | 7 |

### retention warnings and alerts

- (none)

### retention decisions by reason

| target type | decision | count | allocated |
|---|---|---|---|
| tmp-agents-root | would remove (report only) | 758 | 5.03 GiB |
| tmp-agents-root | kept | 14 | 0.05 GiB |
| tmp-agents-root | recent change within max_age | 435 | 0.01 GiB |

### largest eligible trees (top 15 per type)


tmp-agents-root:

| path | allocated | apparent |
|---|---|---|
| `/nix/var/tinyland/user/client-tmp/nix-shell.FKPx04` | 0.88 GiB | 0.88 GiB |
| `/nix/var/tinyland/user/client-tmp/nix-shell.D36de3` | 0.88 GiB | 0.88 GiB |
| `/nix/var/tinyland/user/client-tmp/nix-shell.BSlTyG` | 0.34 GiB | 0.34 GiB |
| `/nix/var/tinyland/user/client-tmp/nix-shell.O6pyAZ` | 0.23 GiB | 0.23 GiB |
| `/nix/var/tinyland/user/client-tmp/nix-shell.GPCcGX` | 0.21 GiB | 0.21 GiB |
| `/nix/var/tinyland/user/client-tmp/nix-shell.fHLPkA` | 0.17 GiB | 0.17 GiB |
| `/nix/var/tinyland/user/client-tmp/nix-shell.vrVe9f` | 0.02 GiB | 0.01 GiB |
| `/nix/var/tinyland/user/client-tmp/nix-shell.vUuwdO` | 0.02 GiB | 0.01 GiB |
| `/nix/var/tinyland/user/client-tmp/nix-shell.yvxPQQ` | 0.02 GiB | 0.01 GiB |
| `/nix/var/tinyland/user/client-tmp/nix-shell.j8iMTu` | 0.02 GiB | 0.01 GiB |
| `/nix/var/tinyland/user/client-tmp/nix-shell.BPFzNr` | 0.02 GiB | 0.01 GiB |
| `/nix/var/tinyland/user/client-tmp/nix-shell.IvSSoM` | 0.02 GiB | 0.01 GiB |
| `/nix/var/tinyland/user/client-tmp/nix-shell.vMfTKS` | 0.02 GiB | 0.01 GiB |
| `/nix/var/tinyland/user/client-tmp/nix-shell.lhRXGY` | 0.02 GiB | 0.01 GiB |
| `/nix/var/tinyland/user/client-tmp/nix-shell.ujNog2` | 0.02 GiB | 0.01 GiB |

### darwin leftovers (report only)

| name | path | allocated | note |
|---|---|---|---|
| fusion-webdeploy-superseded | `/Users/jsullivan2/Library/Application Support/Autodesk/webdeploy/production/127ae56f1790f40cf393bcc73875a1a281e20234` | 0.01 GiB | superseded Fusion webdeploy build (not the target of Autodesk Fusion.app); report only, no deletion rule |
| fusion-webdeploy-current | `/Users/jsullivan2/Library/Application Support/Autodesk/webdeploy/production/c139e2731e42a0340acc3d34de2cbdb01237a27c` | 11.21 GiB | current Fusion build (target of Autodesk Fusion.app); kept |

### bazel disk_cache age rule

- `disk_cache_max_age`: 14d
- `disk_cache_age_delete`: false
- `disk_cache_age_cache_count`: 1
- `disk_cache_age_expired_bytes`: 0 (0.00 GiB)
- `/Volumes/BazelCache/disk-cache`: action `keep`, 0.00 GiB: 0 of 598 entries (0.0 GiB of 0.1 GiB) last used more than 14 days ago

### cycle

- `dry_run`: True
- `level`: warning
- `host_free_before_bytes`: 36727574528
- `monitor_path`: /System/Volumes/Data
- plugin `retention`: skip_reason=dry_run error=None
- plugin `bazel`: skip_reason=dry_run error=None
