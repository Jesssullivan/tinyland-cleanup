# Productionization Plan: tinyland-cleanup

Date: 2026-04-25

## Current Authority

- Source authority: `github.com/Jesssullivan/tinyland-cleanup`
- Historical fork: `tinyland-inc/tinyland-cleanup`, public and archived
- Packaging authority today: Nix flake package `.#tinyland-cleanup`
- Runtime consumers in flight: Darwin developer machines and Linux/Rocky
  builder or runner machines

## Related Work

- GitHub `#2`: durable disk-pressure policy overhaul
- GitHub `#3`: Bazel cache tiering, budgets, and active-use detection
- GitHub `#4`: Nix cleanup policy for generations, roots, and daemon contention
- GitHub `#5`: Darwin IDE and developer-tool cache budgets
- GitHub `#6`: Podman offline compaction for Darwin `applehv`
- GitHub `#7`: dry-run, telemetry, and host free-space accounting
- Linear `TIN-117`: canonical repo decision for `tinyland-cleanup`
- Linear `TIN-139`: Nix/Bazel audit and cleanup
- Linear `TIN-127`: Linux builder and publication contracts

## Style Alignment

Follow the scheduling package pattern where artifact authority is explicit:

- Nix owns host ingestion and package materialization.
- Bazel owns the hermetic graph and CI conformance checks.
- GitHub releases own public binary archives.
- GloriousFlywheel runners provide shared cache acceleration and runner
  capability classes, not repo-shaped special runners.

## Phase 1: Baseline Lock

- Add repo authority docs, FOSS metadata, and contribution/security guidance.
- Repair Go module import path to the canonical GitHub repo.
- Repair Bazel/Bzlmod so `bazel test //...` is meaningful.
- Add hosted GitHub CI for Go and Bazel validation.
- Add manual GloriousFlywheel proof workflow for shared-cache validation.

Validation note: Go, Nix, and hosted Bazel CI are green as of 2026-04-25.
Bazel now gets past the previous missing-module failure, but local Darwin
validation still has a rules_go cold-start blocker before tests execute. See
[validation-status-2026-04-25.md](validation-status-2026-04-25.md).

Exit criteria:

- `go test ./...`, `go vet ./...`, and `go build ./...` pass.
- `nix build .#default` passes.
- `bazel test //...` passes or has a documented external blocker.
- README identifies package authority and roadmap issues.

## Phase 2: Runtime Contracts

- Define policy budgets per cleanup domain.
- Add active-use detection for Bazel, Nix, Podman, and IDE caches.
- Make dry-run output a stable operator contract.
- Record free-space deltas before and after each plugin decision.
- Separate disruptive operations from normal cleanup with explicit opt-ins.

Exit criteria:

- GitHub `#2` and `#7` acceptance criteria are testable.
- Operator output can explain every removal candidate.
- Mission-critical machines can run in audit mode without mutation.

## Phase 3: Darwin Production Path

- Finalize LaunchAgent behavior and config defaults.
- Keep APFS/Podman `applehv` compaction guarded and auditable.
- Add IDE and developer-tool cache budgets for Xcode, iOS simulator,
  Homebrew, language servers, and editor caches.
- Confirm ingestion in the `lab` Home Manager module.

Exit criteria:

- Darwin dry-run and cleanup behavior is validated on real developer hosts.
- `lab` consumes the upstream flake package without in-tree source drift.

## Phase 4: Linux/Rocky Production Path

- Define systemd user/service packaging.
- Add RPM packaging once install, upgrade, config, and service semantics are
  explicit.
- Validate runner-safe behavior for Bazel, Nix, container, and CI caches.

Exit criteria:

- Rocky package installs, upgrades, and removes cleanly.
- Runner cleanup avoids active jobs and preserves remote-cache correctness.

## Phase 5: Public FOSS Readiness

- Publish tagged GitHub release artifacts and checksums.
- Add release notes and upgrade guidance.
- Decide whether RPM publication belongs in this repo or an external package
  index.
- Keep external dependencies and packaging metadata auditable.

Exit criteria:

- Fresh users can build, test, install, configure, and run dry-run mode from
  documented instructions.
- Public CI proves the same package surfaces used by internal ingestion.
