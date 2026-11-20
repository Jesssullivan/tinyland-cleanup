# Validation status

Updated: 2026-11-20

## v0.5.4 (TIN-5770)

- **Scope:** #188, the one change on main since v0.5.3. Lane pin expiry
  spares a link whose target is in active use (a process holds it or names
  it), or was seen in use within `nix.pin_expiry_after`, and spares every
  expired link when the process inventory is partial. It adds
  `nix.pin_expiry_state_file`, `nix.pin_expiry_lsof` and the two
  `nix.pin_expiry_uninspectable_ok_*` lists, and a `spared` receipt line.
- **Validated on Linux** (sting, x86_64, Go 1.26) by the author: the pin
  expiry tests passed (21, 12 new), with a fake inventory, a fake `/proc`
  tree and a fake lsof listing. `go test ./...` passed except
  `TestHomeLanesStaleWorkRecheckAsksGitAgain`, which depends on free space
  on /srv/fast-local and fails the same way on v0.5.3. `go vet` passed on
  Linux and, with cgo off, for darwin/arm64.
- **Store atime:** `/nix` is xfs `noatime` on sting and declared APFS
  `noatime` on PZM, so the release relies on the inventory and the
  per-link stamp, not on atime.
- **The release commit** changes only the version strings, this file and
  the changelog.
- **Not validated:** CI, the Bazel build and test graph, the release
  workflow, a Darwin test run, and any host run of pin expiry in apply
  mode.

## v0.5.3 (TIN-5770)

- **Scope:** #186 (10a34bda), the one change on main since v0.5.2. It adds
  `nix.pin_expiry_roots` with its age, depth, per-pass and receipt keys,
  adds `nix.critical_max_freed_per_pass`, records
  `last_beside_active_work` in the GC state file, and makes
  `TestPropertyZeroYieldSoak` walk all 26 polls.
- **Validated on Linux** (sting, x86_64, go 1.26.10) by the author with
  named tests: `go test ./config ./monitor ./rootjobs .` passed, and
  `./plugins` passed except `TestHomeLanesStaleWorkRecheckAsksGitAgain`,
  which depends on free space on /srv/fast-local and is unrelated to #186.
  New tests cover expiry off without roots, old owned store-pointing links,
  keep files, the hook-pin and profiles directories, the receipt and the
  dry-run plan, the age and per-pass limits, glob roots, the critical cap
  and the beside-active-work record. All 26 polls of the soak test passed
  natively, also under `env -i HOME=/homeless-shelter`.
- **The release commit** changes only the version strings, this file and
  the changelog. A grep found 0.5.3 in VERSION, main.go, MODULE.bazel and
  the BUILD.bazel x_defs, and no 0.5.2 version string there. On sting,
  `go test . ./config` at the release commit passed (go 1.26.10).
- **Not validated:** CI, the Bazel build and test graph and the release
  workflow. No runner serves this repository. No Darwin build of 0.5.3 is
  on record. The cause of the 0.5.2 soak failure is unknown: it did not
  reproduce, and the sweep only makes the test deterministic. No host has
  run pin expiry in apply mode, and no host has run 0.5.3.

## v0.5.2 (TIN-5770)

- **Scope:** #183 (6e23bafb) and #184 (d7aaf4d0), the two changes on main
  since v0.5.1. #183 changes tests only. #184 adds
  `darwin_dev_caches.<family>.paths`, `retention.tmp_agents.pressure` and
  the edge-only logging and state record of `retention.free_space_floor`.
- **Validated on Linux** (sting, x86_64, go 1.23.8) by the authors and the
  merge-gating reviews, with named tests only. For #183, review GO at
  81778aae: all 50 top-level tests in the podman by-use and Bazel
  repository contents test files passed (86 RUN, 86 PASS, 0 SKIP).
  `go test -list` gave 551 plugin tests on main and 553 on the head, so
  nothing skips on Linux. For #184, review GO at 34dd76ec:
  `go test -run '^(TestDevCache|TestConfiguredDevCache|TestDarwinDevCache|TestRetention)' ./plugins`
  (46 PASS) and `TestRecordFloorsWritesReportAndStateOnEdges` in the root
  package passed. Five mutation runs against reverted fixes each failed as
  expected. gofmt was clean, and a Darwin arm64 `go vet` and test-binary
  compile ran on Linux.
- **Validated on Darwin** by Nix builds on one host, petting-zoo-mini, on
  2026-11-18. Each built the flake package and ran its whole test suite in
  the Nix sandbox: all five Go packages `ok`. The first build was at
  6e23bafb. The second was at b8044b65, the #184 head, whose tree is the
  tree of main at d7aaf4d0. Nothing was activated.
- **The release commit** changes only the version strings, this file and
  the changelog. On sting a grep found 0.5.2 in VERSION, main.go,
  MODULE.bazel and the BUILD.bazel x_defs, and no other 0.5.1 version
  string. The flake reads VERSION. No `go build` of the release commit ran.
- **Not validated:** CI, the Bazel build and test graph and the release
  workflow. No runner serves this repository. No full `go test ./...` on
  Linux at d7aaf4d0 is on record. No host has run `paths`, the pressure
  pass or the new floor record, in dry run or apply mode. No host has run
  0.5.2.

## v0.5.1 (TIN-8526)

- **Scope:** #180 (fe664600) and #181 (4c0c3f57), the two changes on main
  since v0.5.0. Each merge commit has the same tree as its reviewed head,
  af8bfc7f for #180 and 3a0468e4 for #181.
- **Validated** on sting (Linux x86_64), by the merge-gating reviews
  (comments 6946441523 and 6955652884 on the pull requests). At af8bfc7f:
  gofmt clean, `go vet ./...`, `go test -count=1 ./...`, a race run of the
  Nix root and `home_lanes` tests, and the Darwin arm64 build and test build
  (`CGO_ENABLED=0`). At 3a0468e4: gofmt clean, `go vet ./...`,
  `go test -count=1 ./...`, and the Darwin arm64 build and vet of
  `./plugins`. All passed.
- **Mutation records.** On #180 each of five mutants (MA to ME) failed
  `go test ./plugins`. On #181 the builder killed 13 mutations and the
  reviewer reran 4 of them, all killed.
- **Read-only dry runs on sting.** A dry run of #180's rule on the agent
  temp root kept 14 roots for Git state, 6 for recent content, 1 for a bare
  repository and 5 held by a process. The #181 review ran `dev-artifacts`
  dry runs at fe664600 and at 3a0468e4 with no new key: 3,819 Nix temp root
  targets each, one root apart by live drift. Its `bazel_bases` dry runs at
  both commits were the same apart from the idle clock.
- **The release commit** was checked with gofmt on `main.go`,
  `go vet ./...`, `go test .`, `go build ./...` and `--version`, which
  prints 0.5.1.
- **Not validated:** CI, the Bazel build and test graph and the Nix package
  build. No runner serves this repository, so none of them ran. No
  apply-mode run of 0.5.1 on any host, no run on a Darwin host, and no run
  of `nix_temp_root_make_writable` or `nix_temp_root_holder_max_age`
  outside test fixtures.
- **The live 0.5.0 daemon on sting, 2026-11-17** (TIN-8526 comments
  03064e64 and 010bf2a6). The temp scan of `/srv/fast-local/tmp-agents`
  stopped at its 5 second share in every cycle from 10:12Z to 12:45Z, with
  40 roots not deletable. From 12:45Z to 13:23Z four cycles freed 0.21,
  0.55, 0.55 and 0.40 GiB, and at 13:42Z the plugin was suppressed. Stale
  Nix temp roots there fell from 2,863 at 10:48Z to 15 at 16:02Z. Eight
  roots that #180's dry run would keep for a Git checkout were removed. The
  `bazel_bases` cycle from 16:41Z to 16:55Z removed 38 of 39 eligible
  output bases, 50.2 GiB. It ran only because the unreadable annotation
  page process was stopped at about 16:34Z.

## v0.5.0 (TIN-8526)

- **Scope:** every change on main since v0.4.3. That includes the Bazel
  repository contents dedupe and stalled-mount alert (#176), the temp lane
  units and probe timeout with the critical Nix busy bound (#177),
  `home_lanes` with stale work roots and `free_target_gb` (#174) and
  `podman.prune_by_use` (#178).
- **Validated** on sting (Linux x86_64, Go 1.27.3) at d9f5536a, the main
  commit this release bumps: `go vet ./...`, `go build ./...`,
  a Darwin arm64 cross build (`CGO_ENABLED=0`) and `go test -count=1 -p 2
  ./...`, all passing. Each of #174, #177 and #178 also had read-only runs
  on sting before merge: `--dry-run` cycles and report-only plans against
  the live host, built from the pull request branches, not from d9f5536a.
  The release commit itself was checked with `go vet ./...`, `go test .`
  and `--version`.
- **Not validated:** CI, the Bazel build and test graph and the Nix package
  build. No runner has served this repository since 2026-10-23, so none of
  them ran for this release. No apply-mode run of `home_lanes` or
  `podman.prune_by_use` and no run on a Darwin host.

## v0.4.3 (TIN-5770)

- **Scope:** per-command deadlines, the plugin watchdog, the Go build cache
  located without starting `go`, the soft memory limit and batched directory
  sizing. It also releases `bazel_bases` and the TIN-5268 deny set (#143).
- **Validated** on sting (Linux x86_64, Go 1.27.1): `go vet ./...`,
  `go test ./...` in the default and `tinyland_sim` builds, `-race` on the
  new tests, and a Darwin arm64 cross build. The fixtures cover a child that
  hangs forever, one that ignores SIGTERM, one that stops itself, a
  grandchild holding the output pipe, and a plugin that never returns.
- **Not validated here:** a run on a Darwin host. The failure this release
  fixes was on Darwin; the fix was exercised there only through the cross
  build.

## v0.4.2 (TIN-3342)

- **Scope:** the five stacked TIN-3342 PRs (R-C194): completion-relative
  scheduling and atomic state (#137), byte backoff (#138), zero-yield
  suppression (#139), per-root scan budgets (#140), bounded repeated logs and
  the `tinyland_sim` consumer-test hook (PR5).
- **Validated** on neo (darwin arm64, GOMAXPROCS=2, `-p 2`): `go vet ./...`
  and `go test ./...` in the default and `tinyland_sim` builds, `-race` on
  the new daemon tests. CI Go/Bazel/Nix/Build site per PR. Not yet delivered:
  lab pins it with the simulated-pressure consumer check.

## v0.4.1 (TIN-3342)

- **Merged:** per-path `temp_scan_max_roots` budget and the remembered
  undeletable-path set in dev-artifacts (#131).
- **Validated** on sting (Linux x86_64): `go build`/`vet`/`test`, plus CI
  Go/Bazel/Nix on #131. Delivered to sting through lab's pin bump.

## v0.4.0 (TIN-3342 / TIN-3181)

- **CI restored.** The private GloriousFlywheel dev-shell input is gone (#130),
  and `main` now requires Go, Bazel and Nix package checks.
- **Merged:** the Bazel orphan reaper (#124), `archive-lifecycle` (#125), the
  zstd transcript codec (#126), the inode free floor / ctime age fallback /
  report fixes (#132), and the removal of `gitlab-runner` and the Darwin
  release legs (#133).
- **Validated** on sting (Linux x86_64): `go build`/`vet`/`test`, gofmt, a
  Darwin `CGO_ENABLED=0` cross-build, plus CI Go/Bazel/Nix on every PR.
- **Deferred to v0.5.0:** the in-use-path Nix GC safety fix (#128), the
  receipt-aware GC controller (#123), byte no-progress backoff, and the
  per-mount/per-root budget knobs in #129. The mount-boundary walker branch
  (`fix/tin-2690-heavy-roots-first`) conflicts and is not in this release.

## v0.3.0 — inode-aware disk-pressure handling (TIN-2170 / TIN-2165)

Inode-awareness is shipped, released, and deployed.

- **Merged to `main`** and released as **v0.3.0** (tarballs + RPMs + SHA256SUMS).
- **CI green**: Go (build/vet/test), Bazel (`//...`), and the Nix package build.
- **Deployed** to the fleet: honey and bumble run the inode-aware daemon; honey's
  `/nix` mount uses inode thresholds `warning 70` / `critical 90`.

### What it does

- Byte and inode pressure are evaluated independently; the cleanup level is the
  higher of the two (`max(byte, inode)`), so an inode-exhausted filesystem with
  free bytes still escalates and fires nix-GC.
- Cleanup does not stop while any monitored mount still has inode pressure.
- Daemon state records consecutive inode no-progress cycles; inode-only critical
  escalation that nothing can relieve backs off to the cooldown cadence
  (`policy.inode_no_progress_limit`, default 3).
- JSON and text reports carry host and per-mount inode evidence
  (`host_inode_level`, `host_byte_level`, `max_inode_level`, `inode_backoff`).

### Validation performed

- `go build` / `vet` / `test` / `test -race`, gofmt — green.
- Cross-platform safety confirmed: APFS reports large dynamic inode counts, so
  inode level stays `none` and never falsely escalates; ext4/xfs fixed inode
  pools (honey's `/nix`) escalate correctly.
- Strict config parsing accepts the shipped `inode_thresholds` keys.
- The lab Home Manager contract test (`tinyland-cleanup-config-test`) passes
  against the shipped binary; lab renders the inode threshold keys onto
  honey/bumble.

### Release ordering (completed)

The strict-config skew is resolved: the inode-supporting binary shipped to `main`
and lab's flake input was bumped to it before honey/bumble switched. New config
keys must always ship in the binary before lab renders them.

## Documentation site

The MkDocs Material site is built by the pure-Bazel `//docs:site` target
(`//docs:site_smoke_test` gates it) with a Nix parity build (`nix build .#docs`),
and deploys to GitHub Pages at <https://jesssullivan.github.io/tinyland-cleanup/>.

## GloriousFlywheel

The Bazel graph is shared-cache-backed; remote-execution proof remains explicit
(`scripts/bazel-rbe-proof.sh`) and is not claimed by default. See
`config/bazel-rbe-target-eligibility.json`.
