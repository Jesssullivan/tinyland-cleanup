#!/usr/bin/env bash
set -euo pipefail

if [[ -n "${TEST_SRCDIR:-}" && -n "${TEST_WORKSPACE:-}" ]]; then
  cd "${TEST_SRCDIR}/${TEST_WORKSPACE}"
else
  repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
  cd "$repo_root"
fi

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

require_contains() {
  local path="$1"
  local pattern="$2"
  local description="$3"

  if ! grep -Eq -- "$pattern" "$path"; then
    fail "$description missing from $path"
  fi
}

reject_contains() {
  local path="$1"
  local pattern="$2"
  local description="$3"

  if grep -Eq -- "$pattern" "$path"; then
    fail "$description found in $path"
  fi
}

for path in Justfile justfile.flywheel .bazelrc.flywheel scripts/gloriousflywheel-bazel.sh; do
  [[ -f "$path" ]] || fail "$path is missing"
done

[[ -f .github/workflows/ci.yml ]] || fail ".github/workflows/ci.yml is missing"

require_contains Justfile '^import\? "justfile\.flywheel"$' "front-door Just import"

require_contains justfile.flywheel '^flywheel-doctor\b' "flywheel-doctor recipe"
require_contains justfile.flywheel '^flywheel-enroll\b' "flywheel-enroll recipe"
require_contains justfile.flywheel '^flywheel-verify\b' "flywheel-verify recipe"
require_contains justfile.flywheel '^flywheel-bazel\b' "flywheel-bazel recipe"
require_contains justfile.flywheel 'gloriousflywheel-bazel' "GloriousFlywheel Bazel wrapper invocation"

require_contains .bazelrc 'try-import %workspace%/\.bazelrc\.flywheel' "front-door Bazel rc import"
require_contains .bazelrc.flywheel '^build:ci-cached\b' "shared-cache Bazel config"
require_contains .bazelrc.flywheel '^build:executor-backed --config=ci-cached$' "executor-backed Bazel config"
require_contains .bazelrc.flywheel '^build:executor-backed --remote_local_fallback=false$' "executor fallback refusal"
reject_contains .bazelrc.flywheel 'remote_cache=' "hard-coded remote cache endpoint"
reject_contains .bazelrc.flywheel 'remote_executor=' "hard-coded remote executor endpoint"
reject_contains .bazelrc.flywheel 'ubuntu-latest' "hosted-runner fallback"
reject_contains .bazelrc.flywheel 'localhost|127\.0\.0\.1' "local proof endpoint"

require_contains scripts/gloriousflywheel-bazel.sh 'GF_BAZEL_SUBSTRATE_MODE' "wrapper substrate-mode contract"
require_contains scripts/gloriousflywheel-bazel.sh 'BAZEL_REMOTE_CACHE' "wrapper cache endpoint contract"
require_contains scripts/gloriousflywheel-bazel.sh 'BAZEL_REMOTE_EXECUTOR' "wrapper executor endpoint contract"
require_contains scripts/gloriousflywheel-bazel.sh 'remote_local_fallback=false' "wrapper executor fallback refusal"
reject_contains scripts/gloriousflywheel-bazel.sh 'ubuntu-latest' "hosted-runner fallback"

require_contains .github/workflows/ci.yml 'runs-on: tinyland-nix' "shared tinyland-nix CI runner"
reject_contains .github/workflows/ci.yml 'ubuntu-latest' "hosted-runner fallback"
reject_contains .github/workflows/ci.yml 'DeterminateSystems/nix-installer-action' "hosted-runner Nix installer fallback"

echo "Flywheel front-door kit check passed"
