# tinyland-cleanup

A conservative disk-pressure cleanup daemon for developer machines and CI hosts.
It reclaims build-system and developer-tool caches before unmanaged disk
pressure breaks local work, runners, or hermetic builds — driven by whichever is
worse, disk bytes or filesystem inodes, and stopping as soon as the target is met.

## Where to start

- [Installation](installation.md) — Home Manager, Nix, RPM, or source
- [Usage](usage.md) — run modes, the daemon, and reading reports
- [Configuration](configuration.md) — every tunable and its Home Manager option
- [Operator workflow](operator-workflow.md) — review and apply cleanup safely

## For tooling and agents

- [JSON report schema](json-report-schema.md) — the machine-readable output
- [Plugins](plugins.md) — the cleanup families and their names

## Safety model

- Dry-run output is detailed enough to review before any deletion.
- Every plan states what it will remove and why.
- Host free space and inodes are measured before and after each cycle.
- Cleanup stops once the free-space target is met and inode pressure has cleared.
- Privileged actions, offline compaction, and service disruption are opt-in.
