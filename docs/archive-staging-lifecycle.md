# Archive Staging Lifecycle

An archival workflow that compresses a tree into durable storage commonly leaves
the uncompressed pre-image behind "just in case", and nothing ever comes back to
remove it. The pre-image then outlives its purpose while occupying many times
the archive's footprint.

The `archive-lifecycle` plugin closes that loop. It retires a staging group only
after proving, from live filesystem state, that every file in it is already
present in the archive target at the same uncompressed size.

Review with:

```sh
tinyland-cleanup --once --dry-run --level moderate --plugins archive-lifecycle --output json
```

## Configuration

```yaml
enable:
  archive_lifecycle: true

archive_lifecycle:
  dry_run: false           # default; set true for a planning-only pass
  retire_after: 14d        # default group age threshold
  max_groups_per_cycle: 32 # default verification budget per cycle
  sources:
    - name: codex-session-archive
      staging_dir: /Volumes/TinylandSSD/tinyland/codex-session-archive/neo
      target_dir: /Volumes/TinylandState/tinyland-state/codex/archived_sessions
      group_depth: 3       # YYYY/MM/DD: retire one verified day at a time
      retire_after: 14d    # optional per-source override
```

`dry_run` is independent of the global `--dry-run` flag and defaults to
**false** (operator ruling R14, 2026-08-13).

The per-file verification below *is* the gate. Nothing is retired that has not
been proved byte-for-byte redundant against the archive target, the proof is
re-run against live filesystem state immediately before deletion, and every
ambiguity fails the whole group rather than the offending file. A second
planning-only default would delay reclamation without adding safety.

Set `dry_run: true` on a host that wants a planning-only pass, or use the global
`--dry-run` flag for a one-off review:

```sh
tinyland-cleanup --once --dry-run --level moderate --plugins archive-lifecycle --output json
```

Relative paths under `staging_dir` must line up with relative paths under
`target_dir`. A staging tree that carries a host prefix is configured *including*
that prefix — `.../codex-session-archive/neo`, not `.../codex-session-archive` —
so that `neo/2026/07/27/x.jsonl` reconciles against `2026/07/27/x.jsonl.zst`.

`group_depth` is the directory depth below `staging_dir` at which retirement is
decided. Day-partitioned archives use depth 3 so one verified day retires at a
time and a partially archived day never blocks the rest. Depth 0 treats the
whole staging root as a single group.

## Verification

For every regular file in a staging group, the plugin looks for a counterpart at
the mirrored relative path under the target:

| Counterpart | Proof |
|---|---|
| exact name | byte sizes must be equal |
| `<name>.gz` | the gzip `ISIZE` trailer must equal the staging size |
| `<name>.zst` | the zstd `Frame_Content_Size` field must equal the staging size |

Both compressed forms are **O(1) reads of declared metadata**. Nothing is
decompressed, so verification of a 145G staging tree costs a few thousand short
reads rather than a full decompression pass.

The `dev-artifacts` agent-transcript lane writes `.zst` counterparts that always
carry `Frame_Content_Size`, so transcripts compressed by this daemon are
verifiable here from the frame header alone. A transcript compressed with the
`gzip` codec is verifiable through its `ISIZE` trailer on the same terms.

A group is verified only when it holds at least one regular file, every file
matched a counterpart, and no entry was skipped. Any of the following fails the
**whole group**, not just the offending file:

- a missing counterpart, or one whose declared size disagrees;
- a `.gz` counterpart for a staging file of 4 GiB or more (`ISIZE` is modulo
  2^32 and cannot prove it);
- a `.zst` counterpart with no `Frame_Content_Size` (streamed without a known
  input size);
- a non-regular staging entry — symlink, socket, device;
- any unreadable directory or file;
- a missing target group directory.

A missing `target_dir` produces a warning and **zero** targets. That case almost
always means the archive volume is not mounted, and treating it as "nothing is
archived" is the safe reading.

## Runtime boundary

- `warning` reports verified groups without retiring them;
- `moderate`, `aggressive`, and `critical` retire verified groups older than
  `retire_after`, subject to `dry_run`;
- group age uses the **newest** mtime of any file in the group, never the
  directory mtime;
- retirement re-runs the full verification against live filesystem state
  immediately before deleting, so a file that appears between planning and
  deletion saves its group;
- deletion refuses to cross a filesystem boundary and refuses to follow
  symlinks; a mount point nested inside a staging group aborts that group;
- deletion never targets the staging root itself, and never a path outside the
  configured staging root;
- emptied parent directories are pruned up to, but never including, the staging
  root;
- staging groups are `destructive`-tier targets; `retire_archive_staging_group`
  advertises `reclaim=host`, and review/keep/report targets advertise
  `reclaim=none`.

## Manifest

The dry-run `CleanupPlan.Targets` list is the manifest. Each entry carries the
staging group path, measured bytes, tier, reclaim expectation, protected status,
action, and a reason that names either the verified target group and file count
or the exact failure that blocked verification. Real cleanup logs one structured
record per retirement (source, staging path, target group, files verified, bytes
freed) and one per re-validation skip.
