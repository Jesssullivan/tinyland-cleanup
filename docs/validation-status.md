# Validation status

Updated: 2026-06-24

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
