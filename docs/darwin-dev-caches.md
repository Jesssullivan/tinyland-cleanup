# Darwin Developer Caches

Darwin developer caches are useful but can become large enough to break local
work and CI runner jobs. The daemon now reports typed cache candidates during
dry-run JSON output without deleting IDE settings or project state.

Review with:

```sh
tinyland-cleanup --once --dry-run --level critical --output json
```

The cache plugin plan includes `targets` for known cache families:

- `jetbrains`: versioned directories under `~/Library/Caches/JetBrains`;
- `playwright`: browser revisions under `~/Library/Caches/ms-playwright`;
- `bazelisk`: downloads under `~/Library/Caches/bazelisk`;
- `pip`: caches under `~/Library/Caches/pip` and `~/.cache/pip`;
- `vscode-cache`: selected VS Code cache directories such as `Cache`,
  `CachedData`, `Code Cache`, `GPUCache`, and service worker cache storage;
- `cursor-cache`: selected Cursor cache directories with the same cache-only
  boundary.

Targets include the cache type, policy tier, name, detected version, path,
physical bytes, active-use evidence, protected status, reclaim expectation,
review action, and reason.

Safety boundaries:

- never remove `~/Library/Application Support/JetBrains`;
- never remove project workspaces;
- never remove keychains, auth databases, editor settings, extension config, or
  `User` settings directories;
- protect active JetBrains IDE versions when matching processes are running;
- protect VS Code and Cursor cache targets when matching editor processes are
  running;
- protect newest Playwright browser revisions per browser family;
- protect the newest Bazelisk cache entries.

Real deletion for these typed targets is opt-in. Leave enforcement disabled
until a dry-run has been reviewed:

```yaml
darwin_dev_caches:
  enabled: true
  enforce: false
```

When Darwin developer caches are enabled, real `cache` cleanup uses this typed
target model as the authority. With `enforce: false`, cleanup returns without
deleting legacy generic cache paths such as pip, npm, Go, Cargo, or broad
`~/Library/Caches` entries. This keeps dry-run review and real mutation aligned
on developer machines.

When `enforce: true`, moderate cleanup can delete unprotected Playwright and
Bazelisk entries outside the keep-latest policy, stale pip caches, and stale
inactive editor cache directories. Aggressive cleanup can delete inactive stale
JetBrains cache versions and the oldest inactive JetBrains versions needed to
bring `jetbrains.max_gb` back under budget. Critical cleanup can delete inactive
unprotected JetBrains and editor cache targets regardless of age. The same
protected target rules from dry-run planning are used for real cleanup.

## Relocated cache roots (`paths`)

A host can move a package cache off the home volume, for example with
`UV_CACHE_DIR` or `XDG_CACHE_HOME`. The compiled paths (`~/.cache/uv`,
`~/Library/Caches/uv` and the like) then never see it. On petting-zoo-mini the
uv cache sat at `/nix/var/tinyland/user/nix-client-cache/uv` and held 44.5 GiB
of the `/nix` volume on November 18, 2026 (TIN-5770).

The directory families `pip`, `npm`, `uv`, `bun` and `opencode` take a
`paths` key:

```yaml
darwin_dev_caches:
  enforce: true
  uv:
    enabled: true
    max_gb: 15
    stale_after_days: 14
    paths:
      - /nix/var/tinyland/user/nix-client-cache/uv
```

| Key | Default | Meaning |
| --- | --- | --- |
| `<family>.paths` | empty | Extra cache roots. `~` and `{user}` are expanded. Each must be absolute, not `/` and not the home directory. Any other family refuses the key. |
| `<family>.max_gb` | 0 (none) | For a listed root, the budget the trim brings the root down to. |
| `<family>.stale_after_days` | 14 | For a listed root, an entry whose newest change is older than this is eligible on age alone. |

A listed root is trimmed entry by entry, never removed whole:

- The units are the root's top-level directories. Files and symlinks at the
  top are never removed.
- A unit is held when the activity inventory (every command line and one
  `lsof` listing) finds an open file, cwd, executable, mapping or command line
  under it. A held top-level directory is not removed; its own entries become
  the units instead, one level down. For uv that offers the archives under
  `archive-v0` while one of them runs a Python.
- A held top-level file (the cache's `.lock` while the tool runs) keeps the
  whole root for that pass.
- An incomplete inventory keeps everything.
- At moderate pressure or above, unheld units go oldest first by the newest
  change inside them: those past `stale_after_days` first, then more until the
  root is at or under `max_gb`.
- A unit changed within the last hour is never chosen.
- Removal still needs `darwin_dev_caches.enforce: true`. Each chosen unit is
  checked again against a fresh inventory just before it is removed.

The inventory is taken only when a listed root exists, so a host that sets no
`paths` pays nothing for it. A compiled path that is also listed is trimmed the
new way. The other compiled paths keep their whole-root rule: removed at
critical, or at moderate when the root's own mtime is past `stale_after_days`.

What the trim cannot see: a process that reads a cache file without keeping it
open between reads, and a virtual environment that links into the cache with
uv's `symlink` link mode. uv's default modes on macOS (clone, then copy) leave
no link back into the cache.
