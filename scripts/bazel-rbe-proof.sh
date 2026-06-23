#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

usage() {
  cat <<'EOF'
Usage: scripts/bazel-rbe-proof.sh [--target LABEL] [--bazel-command test] [-- ARGS...]

Run an explicit remote-execution proof for a target listed in
config/bazel-rbe-target-eligibility.json. This is intentionally not the default
CI path: remote cache hits are not RBE proof, and RBE proof mode must be opted
into with GF_RBE_PROOF_MODE=explicit.

Required environment:
  GF_RBE_PROOF_MODE=explicit
  BAZEL_REMOTE_CACHE
  BAZEL_REMOTE_EXECUTOR
  GF_BAZEL_SUBSTRATE_MODE=executor-backed
  GF_BAZEL_REMOTE_EXECUTION_PLATFORM
EOF
}

target="//:bazel_cache_policy_check"
bazel_command="test"
extra_args=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    --target)
      target="${2:?missing target after --target}"
      shift 2
      ;;
    --bazel-command)
      bazel_command="${2:?missing command after --bazel-command}"
      shift 2
      ;;
    --)
      shift
      extra_args+=("$@")
      break
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      echo "error: unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

case "$bazel_command" in
  build | test | coverage)
    ;;
  *)
    echo "error: RBE proof supports build, test, or coverage only" >&2
    exit 2
    ;;
esac

if [[ "${GF_RBE_PROOF_MODE:-}" != "explicit" ]]; then
  echo "error: set GF_RBE_PROOF_MODE=explicit before running an RBE proof" >&2
  exit 2
fi

if [[ -z "${BAZEL_REMOTE_CACHE:-}" || -z "${BAZEL_REMOTE_EXECUTOR:-}" ]]; then
  echo "error: BAZEL_REMOTE_CACHE and BAZEL_REMOTE_EXECUTOR are both required" >&2
  exit 2
fi

if [[ "${GF_BAZEL_SUBSTRATE_MODE:-}" != "executor-backed" ]]; then
  echo "error: set GF_BAZEL_SUBSTRATE_MODE=executor-backed for RBE proof mode" >&2
  exit 2
fi

"$repo_root/scripts/validation/validate-bazel-rbe-target-eligibility.py" \
  --target "$target" \
  --allow-candidate

"$repo_root/scripts/validation/bazel-cache-contract.sh" --strict

proof_args=(--remote_accept_cached=false)
if ((${#extra_args[@]} > 0)); then
  proof_args+=("${extra_args[@]}")
fi
proof_args+=("$target")

exec "$repo_root/scripts/bazel-cache-backed.sh" \
  "$bazel_command" \
  "${proof_args[@]}"
