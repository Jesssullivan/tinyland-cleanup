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
