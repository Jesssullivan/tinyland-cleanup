# Bazel Cache Policy

Bazel cleanup reports output bases, repository caches, disk caches, and
Bazelisk downloads. Real cleanup mode deletes only stale inactive output bases
after active-process inspection succeeds.

Review with:

```sh
tinyland-cleanup --once --dry-run --level critical --output json
```

The Bazel plan includes targets for:

- `output_base`: Bazel output bases under configured roots such as
  `~/.cache/bazel/_bazel_$USER/*` or Darwin `/private/var/tmp/_bazel_$USER/*`;
- `repository_cache`: shared external repository artifacts;
- `disk_cache`: local action cache entries;
- `bazelisk`: Bazelisk download cache entries.

Targets include bounded physical byte estimates, active-use evidence, protected
status, the planned action, and a reason. Output bases are protected when:

- a Bazel or Bazelisk process is active;
- an output-base lock or server PID file is visible;
- a configured protected workspace has `bazel-*` symlinks into that output base;
- the output base is within `keep_recent_output_bases`;
- the output base is newer than the configured stale threshold.

Default policy:

```yaml
bazel:
  roots:
    - ~/.cache/bazel
  bazelisk_cache: ~/Library/Caches/bazelisk
  max_total_gb: 20
  keep_recent_output_bases: 5
  stale_after: 14d
  critical_stale_after: 3d
  protect_workspaces:
    - ~/git/lab
    - ~/git/GloriousFlywheel
  allow_stop_idle_servers: true
  allow_delete_active_output_bases: false
```

Runtime boundary:

- warning reports footprint only;
- moderate, aggressive, and critical classify stale inactive output bases as
  `delete_output_base` candidates in dry-run output and delete those output
  bases in real cleanup mode;
- real cleanup skips Bazel mutation if active Bazel process inspection fails;
- deletion normalizes writable permissions first, and on Darwin attempts to
  clear `uchg` file flags with `chflags -R nouchg`;
- byte counts use top-level allocation estimates so dry-run remains responsive
  on very large generated trees;
- repository cache, disk cache, and Bazelisk entries are reported for budget
  review but not deleted yet;
- repo-local `bazel-*` symlink cleanup remains a follow-up after output-base
  deletion evidence is proven on real hosts.

Do not disable active-output-base protection on developer machines or shared
runners unless an operator has already drained the relevant jobs and accepted
the risk.
