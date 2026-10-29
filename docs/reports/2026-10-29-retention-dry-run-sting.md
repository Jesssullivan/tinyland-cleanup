# Retention dry run on sting, 2026-10-29

Report-only run of the `retention` plugin and the `bazel` disk_cache age rule
(TIN-7495, TIN-3082), operator ruling 2026-10-29 "Fix tinyland-cleanup first".
Nothing was deleted: `--dry-run`, and every delete flag was false. No service
was deployed or restarted; the running daemon was not touched.

- Started: 2026-10-29T08:22:16Z
- Binary: built from `259ba786577f2e72d30ed00aeb2c082193bd5dc7` (PR #168 head, not yet merged), `go build` with `-mod=vendor`
- Command: `ionice -c3 nice -n19 ./tinyland-cleanup --once --dry-run --level warning --plugins retention,bazel --output json --config sting-dryrun.json`
- Config: sting's live `~/.config/tinyland-cleanup/config.yaml` plus `enable.retention: true`, `retention.tmp_agents.roots: [/srv/fast-local/tmp-agents]` (`max_dirs_per_cycle: 0`, so every root is inspected), `retention.cargo_targets.roots: [/srv/cache/{user}/cargo/build, /srv/cache/{user}/cargo-target]`, and `bazel.disk_cache_paths: [/srv/cache/{user}/bazel/disk_cache, /srv/fast-local/jess/bazel-disk-cache]` (the second is absent). Every delete flag false.
- Host: 1-minute load 3.06 at start (gate: under 100), 4.03 after; wall clock 52.8 s, max RSS 14 MB.

## Highlights

- The tmp-agents quota had about 87 GiB free (64 GiB was the 10-27 figure; the project quota now reports 128 GiB). Only 124 `nix-shell.*` roots remain (2409 on 10-27); the others were removed since 10-27, not by this lane (reported, not verified who), so `tmp_agents` finds nothing eligible today.
- 77 stale roots (2.57 GiB) are kept only because they contain a `.git` (test fixture repositories). The rule never removes a tree with `.git` inside; see the operator asks in the Linear comment.
- 3 roots have a live reference: two by a process cwd, one by `TEMPDIR` (an environment-only holder, kept even though a recency check decided first).
- All 106 Cargo build dirs under `/srv/cache/jess/cargo/build` (141.6 GiB) changed within 7 days: 37 within 1 day (36.1 GiB), 46 within 1 to 3 days (75.4 GiB), 1 within 3 to 5 days (2.1 GiB), 22 within 5 to 7 days (28.0 GiB). The 22 oldest (28.0 GiB) expire within about 2 days if not rebuilt. `/srv/cache/jess/cargo-target` holds no Cargo target dir.
- Bazel `/srv/cache/jess/bazel/disk_cache` (39.2 GiB, 323278 entries): 64803 entries, 1.86 GiB, last used more than 14 days ago. `/srv/cache` is mounted `noatime`, so last use is the entry mtime.
- No volume is under the 10 GiB floor; the closest is `/` with 13.9 GiB free.

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
| `cargo_targets.inspected` | 106 |
| `cargo_targets.kept_bytes` | 152084439040 (141.64 GiB) |
| `cargo_targets.kept_count` | 106 |
| `cargo_targets.live_count` | 0 |
| `free_space_floor./` | free_bytes=14972682240 total_bytes=74951770112 below_floor=false |
| `free_space_floor./home` | free_bytes=37659742208 total_bytes=153376899072 below_floor=false |
| `free_space_floor./nix` | free_bytes=45801066496 total_bytes=214538240000 below_floor=false |
| `free_space_floor./srv/cache/jess` | free_bytes=44472545280 total_bytes=428422987776 below_floor=false |
| `free_space_floor./srv/cache/jess/cargo-target` | free_bytes=44472545280 total_bytes=428422987776 below_floor=false |
| `free_space_floor./srv/cache/jess/cargo/build` | free_bytes=44472545280 total_bytes=428422987776 below_floor=false |
| `free_space_floor./srv/data` | free_bytes=1170260492288 total_bytes=2547742969856 below_floor=false |
| `free_space_floor./srv/fast-local` | free_bytes=326839832576 total_bytes=1717148057600 below_floor=false |
| `free_space_floor./srv/fast-local/jess` | free_bytes=217580441600 total_bytes=1556925644800 below_floor=false |
| `free_space_floor./srv/fast-local/jess/estate` | free_bytes=158423216128 total_bytes=161061273600 below_floor=false |
| `free_space_floor./srv/fast-local/tmp-agents` | free_bytes=93926248448 total_bytes=137438953472 below_floor=false |
| `free_space_floor.floor_gib` | 10 |
| `tmp_agents.delete` | false |
| `tmp_agents.eligible_bytes` | 0 (0.00 GiB) |
| `tmp_agents.eligible_count` | 0 |
| `tmp_agents.eligible_logical_bytes` | 0 (0.00 GiB) |
| `tmp_agents.inspected` | 124 |
| `tmp_agents.kept_bytes` | 2860822528 (2.66 GiB) |
| `tmp_agents.kept_count` | 121 |
| `tmp_agents.live_count` | 3 |

### retention warnings and alerts

- (none)

### retention decisions by reason

| target type | decision | count | allocated |
|---|---|---|---|
| cargo-target | recent change within max_age | 106 | 141.64 GiB |
| tmp-agents-root | kept | 77 | 2.57 GiB |
| tmp-agents-root | recent change within max_age | 45 | 0.08 GiB |
| tmp-agents-root | live reference: cwd | 2 | 0.01 GiB |

### largest eligible trees (top 15 per type)


### bazel disk_cache age rule

- `disk_cache_max_age`: 14d
- `disk_cache_age_delete`: false
- `disk_cache_age_cache_count`: 1
- `disk_cache_age_expired_bytes`: 1998065664 (1.86 GiB)
- `/srv/cache/jess/bazel/disk_cache`: action `report_expired_disk_cache_entries`, 1.86 GiB: would remove (report only; set bazel.disk_cache_age_delete): 64803 of 323278 entries (1.9 GiB of 39.2 GiB) last used more than 14 days ago

### cycle

- `dry_run`: True
- `level`: warning
- `host_free_before_bytes`: 44472545280
- `monitor_path`: /srv/cache/jess
- plugin `retention`: skip_reason=dry_run error=None
- plugin `bazel`: skip_reason=dry_run error=None
