#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

usage() {
  cat <<'EOF'
Usage: scripts/bazel-cache-backed.sh <build|test|coverage|run|info> [bazel args...]

Run Bazel through the tinyland-cleanup cache/REAPI control-plane contract. The
wrapper supports local compatibility mode, shared remote cache mode, and
explicit executor-backed proof mode without hardcoding private endpoints.

Environment:
  BAZEL_BIN                                 Bazel binary, default: bazelisk
  BAZEL_REMOTE_CACHE                        Optional remote cache endpoint
  BAZEL_REMOTE_EXECUTOR                     Optional remote executor endpoint
  BAZEL_REMOTE_UPLOAD                       true/false, default: false
  GF_BAZEL_SUBSTRATE_MODE                   Optional explicit substrate mode
  GF_BAZEL_REMOTE_EXECUTION_PLATFORM        Required when BAZEL_REMOTE_EXECUTOR is set
  TINYLAND_CLEANUP_BAZEL_CONFIG             Override Bazel --config selection
  TINYLAND_CLEANUP_BAZEL_OUTPUT_USER_ROOT   Optional Bazel startup --output_user_root
  TINYLAND_CLEANUP_BAZEL_DISK_CACHE         Optional local --disk_cache
  TINYLAND_CLEANUP_BAZEL_STARTUP_ARGS       Optional whitespace-separated startup args
EOF
}

if [[ $# -lt 1 ]]; then
  usage >&2
  exit 2
fi

command_name="$1"
shift

case "$command_name" in
  build | test | coverage | run | info)
    ;;
  -h | --help | help)
    usage
    exit 0
    ;;
  *)
    echo "error: unsupported Bazel command: $command_name" >&2
    usage >&2
    exit 2
    ;;
esac

if [[ "${command_name}" == "info" && $# -eq 0 ]]; then
  :
fi

bazel_bin="${BAZEL_BIN:-bazelisk}"
if ! command -v "$bazel_bin" >/dev/null 2>&1; then
  echo "error: Bazel binary not found: $bazel_bin" >&2
  echo "hint: run through nix shell nixpkgs#bazelisk or set BAZEL_BIN" >&2
  exit 127
fi

remote_cache="${BAZEL_REMOTE_CACHE:-}"
remote_executor="${BAZEL_REMOTE_EXECUTOR:-}"
declared_mode="${GF_BAZEL_SUBSTRATE_MODE:-}"
execution_platform="${GF_BAZEL_REMOTE_EXECUTION_PLATFORM:-}"

contract_args=()
if [[ -n "$remote_cache" || -n "$remote_executor" || "$declared_mode" == "shared-cache-backed" || "$declared_mode" == "executor-backed" ]]; then
  contract_args+=(--strict)
fi
if ((${#contract_args[@]} > 0)); then
  "$repo_root/scripts/validation/bazel-cache-contract.sh" "${contract_args[@]}"
else
  "$repo_root/scripts/validation/bazel-cache-contract.sh"
fi

if [[ -n "${TINYLAND_CLEANUP_BAZEL_STARTUP_ARGS:-}" ]]; then
  # shellcheck disable=SC2206
  startup_args=(${TINYLAND_CLEANUP_BAZEL_STARTUP_ARGS})
else
  startup_args=()
fi

if [[ -n "${TINYLAND_CLEANUP_BAZEL_OUTPUT_USER_ROOT:-}" ]]; then
  startup_args+=("--output_user_root=${TINYLAND_CLEANUP_BAZEL_OUTPUT_USER_ROOT}")
fi

bazel_config="${TINYLAND_CLEANUP_BAZEL_CONFIG:-}"
if [[ -z "$bazel_config" ]]; then
  if [[ -n "$remote_executor" ]]; then
    bazel_config="executor-backed"
  elif [[ -n "$remote_cache" ]]; then
    bazel_config="ci"
  else
    bazel_config="ci-local"
  fi
fi

bazel_args=("--config=${bazel_config}")

if [[ -n "${TINYLAND_CLEANUP_BAZEL_DISK_CACHE:-}" ]]; then
  bazel_args+=("--disk_cache=${TINYLAND_CLEANUP_BAZEL_DISK_CACHE}")
fi

if [[ -n "${BAZEL_REPOSITORY_CACHE:-}" ]]; then
  bazel_args+=("--repository_cache=${BAZEL_REPOSITORY_CACHE}")
fi

if [[ -n "${BAZEL_DISTDIR:-}" ]]; then
  bazel_args+=("--distdir=${BAZEL_DISTDIR}")
fi

if [[ -n "$remote_cache" ]]; then
  upload="${BAZEL_REMOTE_UPLOAD:-false}"
  case "$upload" in
    1 | true | TRUE | yes | YES)
      upload="true"
      ;;
    0 | false | FALSE | no | NO | "")
      upload="false"
      ;;
    *)
      echo "error: BAZEL_REMOTE_UPLOAD must be true or false" >&2
      exit 2
      ;;
  esac

  bazel_args+=(
    "--remote_cache=${remote_cache}"
    "--remote_upload_local_results=${upload}"
    "--remote_download_outputs=toplevel"
    "--remote_local_fallback=true"
  )
fi

if [[ -n "$remote_executor" ]]; then
  bazel_args+=("--remote_executor=${remote_executor}")
  bazel_args+=("--remote_default_exec_properties=gf.platform=${execution_platform}")
fi

if ((${#startup_args[@]} > 0)); then
  exec "$bazel_bin" "${startup_args[@]}" "$command_name" "${bazel_args[@]}" "$@"
fi

exec "$bazel_bin" "$command_name" "${bazel_args[@]}" "$@"
