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

reject_contains .bazelrc 'bazel-cache\.fuzzy-dev\.tinyland\.dev' "stale Bazel cache endpoint"
reject_contains .bazelrc '^[[:space:]]*build:remote[[:space:]]+--remote_cache=' "hard-coded remote cache endpoint"
reject_contains .bazelrc '^[[:space:]]*(build|test):.*--remote_executor=' "hard-coded remote executor endpoint"
require_contains .bazelrc 'explicit --remote_cache flag' "operator-provided remote cache comment"
require_contains .bazelrc 'build:remote --remote_upload_local_results=false' "read-only remote cache default"
require_contains .bazelrc 'build:executor-backed --config=ci' "endpoint-free executor-backed config"

contract="scripts/validation/bazel-cache-contract.sh"
require_contains "$contract" 'GF_BAZEL_SUBSTRATE_MODE' "Bazel substrate mode contract"
require_contains .bazelrc 'experimental_convenience_symlinks=ignore' "Bazel convenience symlink suppression"
require_contains "$contract" 'BAZEL_REMOTE_CACHE' "Bazel remote cache contract"
require_contains "$contract" 'BAZEL_REMOTE_EXECUTOR' "Bazel remote executor contract"
require_contains "$contract" 'executor-backed' "executor-backed mode contract"
reject_contains "$contract" 'bazel-cache\.fuzzy-dev\.tinyland\.dev' "stale Bazel cache endpoint"

eligibility="config/bazel-rbe-target-eligibility.json"
require_contains "$eligibility" 'TIN-2170' "tracker-linked RBE eligibility manifest"
require_contains "$eligibility" 'Remote cache hits are not RBE proof' "cache-vs-RBE invariant"
require_contains "$eligibility" 'executor-backed mode is explicit-proof only' "explicit RBE proof posture"

validator="scripts/validation/validate-bazel-rbe-target-eligibility.py"
require_contains "$validator" 'remote executor process evidence' "RBE proof evidence validator"
require_contains "$validator" '--allow-candidate' "candidate proof gate"

cache_wrapper="scripts/bazel-cache-backed.sh"
require_contains "$cache_wrapper" 'bazel-cache-contract.sh' "wrapper contract preflight"
require_contains "$cache_wrapper" '--remote_cache=\$\{remote_cache\}' "wrapper remote cache flag"
require_contains "$cache_wrapper" '--remote_executor=\$\{remote_executor\}' "wrapper remote executor flag"
require_contains "$cache_wrapper" '--remote_upload_local_results=\$\{upload\}' "wrapper upload mode flag"

proof_wrapper="scripts/bazel-rbe-proof.sh"
require_contains "$proof_wrapper" 'GF_RBE_PROOF_MODE=explicit' "explicit RBE proof mode gate"
require_contains "$proof_wrapper" 'validate-bazel-rbe-target-eligibility.py' "RBE eligibility validation"
require_contains "$proof_wrapper" '--remote_accept_cached=false' "forced execution proof flag"

workflow=".github/workflows/gloriousflywheel-proof.yml"
require_contains "$workflow" 'scripts/bazel-cache-backed.sh test //\.\.\.' "Bazel cache-backed wrapper invocation"
require_contains "$workflow" 'scripts/bazel-rbe-proof.sh' "Bazel RBE proof wrapper invocation"
require_contains "$workflow" 'BAZEL_REMOTE_UPLOAD' "remote upload mode wiring"
require_contains "$workflow" 'GF_RBE_PROOF_MODE=explicit' "explicit proof mode workflow gate"
reject_contains "$workflow" 'echo .*\$BAZEL_REMOTE_CACHE' "private remote cache endpoint echo"
reject_contains "$workflow" '--remote_cache=\$\{BAZEL_REMOTE_CACHE\}' "workflow-local remote cache handroll"
reject_contains "$workflow" 'bazel-cache\.nix-cache\.svc\.cluster\.local' "private Bazel cache endpoint literal"
reject_contains "$workflow" 'bazel-cache\.fuzzy-dev\.tinyland\.dev' "stale Bazel cache endpoint"

echo "Bazel cache policy check passed"
