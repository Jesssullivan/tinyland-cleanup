# Repository Guidance

## Role

`tinyland-cleanup` is a disk-pressure cleanup daemon for Tinyland developer
machines and CI hosts. It must be conservative by default because it runs on
workstations that host multiple hermetic build systems and local developer
state.

The canonical upstream is `github.com/Jesssullivan/tinyland-cleanup`. Treat the
old Tinyland organization fork as historical context only unless a current
operator runbook says otherwise.

## Build Authority

- Native Go commands are the fastest local validation path.
- Nix is the package authority for Darwin and Linux ingestion.
- Bazel is the hermetic build/test graph and the surface used by
  GloriousFlywheel shared-cache runners.
- GloriousFlywheel cache attachment is an acceleration contract. Do not claim
  remote execution/offload unless a runner contract proves it for this repo.

## Safety Defaults

- Prefer dry-run and plan output before destructive cleanup.
- Preserve host free-space accounting before and after every cleanup decision.
- Keep privileged operations, offline compaction, and disruptive service work
  explicitly opt-in.
- Keep Darwin and Linux/Rocky behavior separate where platform semantics differ.

## Operating this daemon (for agents)

- CLI contract: `--once`, `--daemon`, `--level {warning,moderate,aggressive,critical}`,
  `--dry-run`, `--output {text,json}`, `--plugins <names>`, `--list-plugins`,
  `--config <path>`, `--target-used-percent <int>`, `--version`. Flags are stable.
- Machine surface: `--output json` emits a `cycleReport`. Schema:
  [docs/json-report-schema.md](docs/json-report-schema.md). `host_free_delta_bytes`
  and `host_inodes_free_delta` are the effectiveness signals.
- Plugin contract: [docs/plugins.md](docs/plugins.md); enumerate live state with
  `--list-plugins --output json`.
- Exit codes: `0` success, `1` runtime error, `2` config/flag error.
- Key files: `main.go` (flags + `cycleReport`), `config/config.go` (+
  `config/default.yaml`) for the config schema, `monitor/disk.go` for byte/inode
  thresholds, `plugins/` for cleanup logic.
- Never run a `--daemon` and a `--once` cron on the same host (cooldown conflict).

## Validation

Run the Go checks (fast authority); Nix and Bazel for graph/package changes.
Full matrix and the docs-site build: [docs/development.md](docs/development.md).

```sh
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go test ./...
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go vet ./...
env GOCACHE=/tmp/tinyland-cleanup-gocache GOFLAGS=-mod=vendor go build ./...
```
