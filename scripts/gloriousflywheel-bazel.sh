#!/usr/bin/env bash
# Consumer-side Bazel wrapper for the GloriousFlywheel cache/RBE contract.
#
# Copy this file into a consumer repo, for example:
#   scripts/gloriousflywheel-bazel.sh
#
# Endpoint values come from the environment and are passed as explicit Bazel
# flags. Do not put deployment-specific remote_cache or remote_executor values
# in .bazelrc files; Bazel rc files are not the cache authority.

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
candidate_envs=("${script_dir}/../.env.flywheel.local" "${PWD}/.env.flywheel.local")
loaded_env=""
for flywheel_env in "${candidate_envs[@]}"; do
  if [[ -n ${loaded_env} && ${flywheel_env} == "${loaded_env}" ]]; then
    continue
  fi
  if [[ -f ${flywheel_env} ]]; then
    set -a
    # shellcheck disable=SC1090
    source "${flywheel_env}"
    set +a
    loaded_env="${flywheel_env}"
  fi
done

usage() {
  cat >&2 <<'EOF'
Usage: scripts/gloriousflywheel-bazel.sh <command> [args...]

Commands:
  build     Run a cache-backed Bazel build
  test      Run cache-backed Bazel tests
  run       Run a cache-backed Bazel target
  coverage  Run cache-backed Bazel coverage
  fetch     Populate external repositories through the same input contract
  info      Validate cache attachment, then run bazel info

Required environment:
  BAZEL_REMOTE_CACHE        grpc://, grpcs://, http://, or https:// endpoint
  GF_BAZEL_SUBSTRATE_MODE   shared-cache-backed, or executor-backed when
                            BAZEL_REMOTE_EXECUTOR is set

Optional environment:
  BAZEL_BIN                 Bazel executable, defaults to bazelisk
  BAZEL_REMOTE_EXECUTOR     Required in executor-backed mode
  GF_BAZEL_REMOTE_EXECUTION_PLATFORM
                            Executor platform property; defaults to
                            gloriousflywheel-rbe-linux-x86_64
  GF_BAZEL_REMOTE_UPLOAD    true only for trusted cache-writing jobs
  GF_BAZEL_LOCAL_PROOF      port-forward only for explicit localhost proofs
  BAZEL_CREDENTIAL_HELPER   Optional Bazel credential helper path or
                            scope=/path entry. Repeat with newline-separated
                            values when needed.
  BAZEL_REMOTE_HEADER       Optional remote header as name=value. Repeat with
                            newline-separated values when needed.
  BAZEL_REMOTE_CACHE_HEADER Optional cache-only header as name=value. Repeat
                            with newline-separated values when needed.
  BAZEL_REMOTE_EXEC_HEADER  Optional executor-only header as name=value. Repeat
                            with newline-separated values when needed.
  BAZEL_REMOTE_INSTANCE_NAME
                            Optional REAPI instance_name (default or
                            spoke-<slug>) for cell-backed CAS/AC/RBE.
  BAZEL_REMOTE_MAX_CONNECTIONS
                            Optional cap for remote gRPC connections; useful
                            for kubectl port-forward proof lanes.
  GF_BAZEL_JOBS             Optional Bazel jobs override for executor-backed
                            lanes; defaults to the rc profile.
  BAZEL_OUTPUT_USER_ROOT    Optional isolated Bazel output user root
  BAZEL_OUTPUT_BASE         Optional isolated Bazel output base
  BAZEL_REPOSITORY_CACHE    Optional Bazel repository cache directory
  BAZEL_DISTDIR             Optional colon-separated Bazel distdir paths
  GF_BAZEL_REPOSITORY_DISABLE_DOWNLOAD
                            true only for hermetic external-fetch proof lanes
  GF_BAZEL_INJECT_REPOSITORIES
                            Optional colon-separated repo=/absolute/path entries
EOF
}

if [[ $# -lt 1 ]]; then
  usage
  exit 2
fi

command="$1"
shift

case "${command}" in
build | test | run | coverage | fetch | info) ;;
-h | --help)
  usage
  exit 0
  ;;
*)
  echo "ERROR: unsupported Bazel command for GloriousFlywheel wrapper: ${command}" >&2
  usage
  exit 2
  ;;
esac

bazel_bin="${BAZEL_BIN:-bazelisk}"
remote_cache="${BAZEL_REMOTE_CACHE:-}"
remote_executor="${BAZEL_REMOTE_EXECUTOR:-}"
mode="${GF_BAZEL_SUBSTRATE_MODE:-}"
upload="${GF_BAZEL_REMOTE_UPLOAD:-false}"
local_proof="${GF_BAZEL_LOCAL_PROOF:-}"
remote_execution_platform="${GF_BAZEL_REMOTE_EXECUTION_PLATFORM:-gloriousflywheel-rbe-linux-x86_64}"
remote_instance_name="${BAZEL_REMOTE_INSTANCE_NAME:-}"
repository_disable_download="${GF_BAZEL_REPOSITORY_DISABLE_DOWNLOAD:-false}"
jobs="${GF_BAZEL_JOBS:-}"
remote_max_connections="${BAZEL_REMOTE_MAX_CONNECTIONS:-}"
startup_args=()
external_fetch_args=()
inject_repository_args=()
executor_args=()
auth_args=()
cache_auth_args=()
exec_auth_args=()
routing_args=()

validate_runtime_value() {
  local flag_name="$1"
  local value="$2"

  if [[ ${value} == *'${'* ]] || [[ ${value} == *'}'* ]]; then
    echo "ERROR: ${flag_name} contains a literal shell placeholder, not a real value." >&2
    exit 1
  fi
}

validate_endpoint() {
  local flag_name="$1"
  local value="$2"

  if [[ -z ${value} ]]; then
    echo "ERROR: ${flag_name} is required for GloriousFlywheel-backed Bazel work." >&2
    exit 1
  fi
  if [[ ${value} =~ (attic-cache-dev|fuzzy-dev|attic\.dev-cluster|attic\.tinyland) ]]; then
    echo "ERROR: ${flag_name} points at a stale GloriousFlywheel endpoint." >&2
    exit 1
  fi
  if [[ ${value} == *'${'* ]] || [[ ${value} == *'}'* ]]; then
    echo "ERROR: ${flag_name} is a literal shell placeholder, not a real endpoint." >&2
    exit 1
  fi
  if [[ ! ${value} =~ ^(grpc|grpcs|http|https):// ]]; then
    echo "ERROR: ${flag_name} must start with grpc://, grpcs://, http://, or https://." >&2
    exit 1
  fi
  if [[ ${value} =~ ^(grpc|grpcs|http|https)://(localhost|127\.0\.0\.1|\[::1\])(:|/|$) ]] && [[ ${local_proof} != "port-forward" ]]; then
    echo "ERROR: ${flag_name} uses localhost; set GF_BAZEL_LOCAL_PROOF=port-forward only for an explicit local proof." >&2
    exit 1
  fi
}

case "${local_proof}" in
"" | port-forward) ;;
*)
  echo "ERROR: GF_BAZEL_LOCAL_PROOF must be empty or port-forward." >&2
  exit 1
  ;;
esac

if [[ -n ${BAZEL_OUTPUT_USER_ROOT:-} ]]; then
  startup_args+=(--output_user_root="${BAZEL_OUTPUT_USER_ROOT}")
fi

if [[ -n ${BAZEL_OUTPUT_BASE:-} ]]; then
  startup_args+=(--output_base="${BAZEL_OUTPUT_BASE}")
fi

if [[ -n ${BAZEL_REPOSITORY_CACHE:-} ]]; then
  external_fetch_args+=(--repository_cache="${BAZEL_REPOSITORY_CACHE}")
fi

while IFS= read -r credential_helper; do
  if [[ -n ${credential_helper} ]]; then
    validate_runtime_value "BAZEL_CREDENTIAL_HELPER" "${credential_helper}"
    auth_args+=(--credential_helper="${credential_helper}")
  fi
done <<<"${BAZEL_CREDENTIAL_HELPER:-}"

while IFS= read -r remote_header; do
  if [[ -n ${remote_header} ]]; then
    validate_runtime_value "BAZEL_REMOTE_HEADER" "${remote_header}"
    auth_args+=(--remote_header="${remote_header}")
  fi
done <<<"${BAZEL_REMOTE_HEADER:-}"

while IFS= read -r remote_cache_header; do
  if [[ -n ${remote_cache_header} ]]; then
    validate_runtime_value "BAZEL_REMOTE_CACHE_HEADER" "${remote_cache_header}"
    cache_auth_args+=(--remote_cache_header="${remote_cache_header}")
  fi
done <<<"${BAZEL_REMOTE_CACHE_HEADER:-}"

while IFS= read -r remote_exec_header; do
  if [[ -n ${remote_exec_header} ]]; then
    validate_runtime_value "BAZEL_REMOTE_EXEC_HEADER" "${remote_exec_header}"
    exec_auth_args+=(--remote_exec_header="${remote_exec_header}")
  fi
done <<<"${BAZEL_REMOTE_EXEC_HEADER:-}"

if [[ -n ${remote_instance_name} ]]; then
  validate_runtime_value "BAZEL_REMOTE_INSTANCE_NAME" "${remote_instance_name}"
  routing_args+=(--remote_instance_name="${remote_instance_name}")
fi

if [[ -n ${BAZEL_DISTDIR:-} ]]; then
  IFS=: read -r -a bazel_distdirs <<<"${BAZEL_DISTDIR}"
  for bazel_distdir in "${bazel_distdirs[@]}"; do
    if [[ -n ${bazel_distdir} ]]; then
      external_fetch_args+=(--distdir="${bazel_distdir}")
    fi
  done
fi

if [[ -n ${GF_BAZEL_INJECT_REPOSITORIES:-} ]]; then
  IFS=: read -r -a injected_repositories <<<"${GF_BAZEL_INJECT_REPOSITORIES}"
  for inject_repository in "${injected_repositories[@]}"; do
    if [[ -z ${inject_repository} ]]; then
      continue
    fi
    if [[ ! ${inject_repository} =~ ^[A-Za-z][A-Za-z0-9_.-]*= ]]; then
      echo "ERROR: GF_BAZEL_INJECT_REPOSITORIES entries must be repo=/absolute/path." >&2
      exit 1
    fi
    inject_path="${inject_repository#*=}"
    if [[ -z ${inject_path} || ${inject_path} != /* ]]; then
      echo "ERROR: injected Bazel repositories must use absolute local paths." >&2
      exit 1
    fi
    if [[ ! -d ${inject_path} ]]; then
      echo "ERROR: injected Bazel repository path does not exist: ${inject_path}" >&2
      exit 1
    fi
    inject_repository_args+=(--inject_repository="${inject_repository}")
  done
fi

case "${repository_disable_download}" in
true | 1 | yes)
  external_fetch_args+=(--repository_disable_download)
  ;;
false | 0 | no | "") ;;
*)
  echo "ERROR: GF_BAZEL_REPOSITORY_DISABLE_DOWNLOAD must be true or false." >&2
  exit 1
  ;;
esac

validate_endpoint "BAZEL_REMOTE_CACHE" "${remote_cache}"

case "${mode}" in
shared-cache-backed)
  if [[ -n ${remote_executor} ]]; then
    echo "ERROR: BAZEL_REMOTE_EXECUTOR is set but GF_BAZEL_SUBSTRATE_MODE is shared-cache-backed." >&2
    exit 1
  fi
  if [[ ${#exec_auth_args[@]} -gt 0 ]]; then
    echo "ERROR: BAZEL_REMOTE_EXEC_HEADER requires executor-backed mode." >&2
    exit 1
  fi
  bazel_config="ci-cached"
  ;;
executor-backed)
  validate_endpoint "BAZEL_REMOTE_EXECUTOR" "${remote_executor}"
  bazel_config="executor-backed"
  executor_args+=(
    --remote_executor="${remote_executor}"
    --remote_local_fallback=false
    --spawn_strategy=remote
    --remote_default_exec_properties="gf.platform=${remote_execution_platform}"
  )
  if [[ ${#exec_auth_args[@]} -gt 0 ]]; then
    executor_args+=("${exec_auth_args[@]}")
  fi
  if [[ -n ${jobs} ]]; then
    executor_args+=(--jobs="${jobs}")
  fi
  if [[ -n ${remote_max_connections} ]]; then
    executor_args+=(--remote_max_connections="${remote_max_connections}")
  fi
  ;;
*)
  echo "ERROR: GF_BAZEL_SUBSTRATE_MODE must be shared-cache-backed or executor-backed." >&2
  exit 1
  ;;
esac

case "${upload}" in
true | 1 | yes)
  upload_arg=(--remote_upload_local_results=true)
  ;;
false | 0 | no | "")
  upload_arg=(--remote_upload_local_results=false)
  ;;
*)
  echo "ERROR: GF_BAZEL_REMOTE_UPLOAD must be true or false." >&2
  exit 1
  ;;
esac

if ! command -v "${bazel_bin}" >/dev/null 2>&1; then
  echo "ERROR: ${bazel_bin} is not on PATH; enter direnv or nix develop first." >&2
  exit 127
fi

case "${command}" in
info)
  bazel_args=(
    info
    --remote_cache="${remote_cache}"
    "${upload_arg[@]}"
  )
  if [[ ${#auth_args[@]} -gt 0 ]]; then
    bazel_args+=("${auth_args[@]}")
  fi
  if [[ ${#cache_auth_args[@]} -gt 0 ]]; then
    bazel_args+=("${cache_auth_args[@]}")
  fi
  if [[ ${#routing_args[@]} -gt 0 ]]; then
    bazel_args+=("${routing_args[@]}")
  fi
  ;;
*)
  bazel_args=(
    "${command}"
    --config="${bazel_config}"
    --remote_cache="${remote_cache}"
    "${upload_arg[@]}"
  )
  if [[ ${#auth_args[@]} -gt 0 ]]; then
    bazel_args+=("${auth_args[@]}")
  fi
  if [[ ${#cache_auth_args[@]} -gt 0 ]]; then
    bazel_args+=("${cache_auth_args[@]}")
  fi
  if [[ ${#routing_args[@]} -gt 0 ]]; then
    bazel_args+=("${routing_args[@]}")
  fi
  if [[ ${#executor_args[@]} -gt 0 ]]; then
    bazel_args+=("${executor_args[@]}")
  fi
  if [[ ${#external_fetch_args[@]} -gt 0 ]]; then
    bazel_args+=("${external_fetch_args[@]}")
  fi
  if [[ ${#inject_repository_args[@]} -gt 0 ]]; then
    bazel_args+=("${inject_repository_args[@]}")
  fi
  ;;
esac

exec_args=("${bazel_bin}")
if [[ ${#startup_args[@]} -gt 0 ]]; then
  exec_args+=("${startup_args[@]}")
fi
exec_args+=("${bazel_args[@]}" "$@")
exec "${exec_args[@]}"
