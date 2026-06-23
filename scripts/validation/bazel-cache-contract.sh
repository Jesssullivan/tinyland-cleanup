#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/validation/bazel-cache-contract.sh [--strict] [--strict-nix]

Classify and validate the tinyland-cleanup Bazel cache/REAPI substrate without
printing private endpoint values.

Modes:
  compatibility-local-only  No shared Bazel cache or executor is configured.
  shared-cache-backed       BAZEL_REMOTE_CACHE is configured.
  executor-backed           BAZEL_REMOTE_EXECUTOR and BAZEL_REMOTE_CACHE are configured.

Environment:
  BAZEL_REMOTE_CACHE
  BAZEL_REMOTE_EXECUTOR
  GF_BAZEL_SUBSTRATE_MODE
  GF_BAZEL_REMOTE_EXECUTION_PLATFORM
  BAZEL_REPOSITORY_CACHE
  BAZEL_DISTDIR

Options:
  --strict      Require a shared cache or executor-backed substrate.
  --strict-nix  Check optional Attic/FlakeHub cache environment for partial config.
EOF
}

strict=false
strict_nix=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --strict)
      strict=true
      ;;
    --strict-nix)
      strict_nix=true
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
  shift
done

errors=()

add_error() {
  errors+=("$1")
}

is_set() {
  [[ -n "${1:-}" ]]
}

describe_endpoint() {
  local value="${1:-}"

  if [[ -z "$value" ]]; then
    printf 'unset'
    return
  fi

  if [[ "$value" == *"://"* ]]; then
    printf '%s://<configured>' "${value%%://*}"
    return
  fi

  printf '<configured>'
}

require_supported_cache_endpoint() {
  local name="$1"
  local value="$2"

  [[ -z "$value" ]] && return

  if [[ "$value" =~ attic-cache-dev|fuzzy-dev|attic\.dev-cluster|attic\.tinyland|bazel-cache\.fuzzy-dev ]]; then
    add_error "$name points at a stale legacy cache endpoint"
  fi

  if [[ "$value" == *'${'* || "$value" == *'}'* ]]; then
    add_error "$name contains an unevaluated placeholder"
  fi

  if [[ ! "$value" =~ ^(grpc|grpcs|http|https):// ]]; then
    add_error "$name must use grpc://, grpcs://, http://, or https://"
  fi
}

require_supported_executor_endpoint() {
  local name="$1"
  local value="$2"

  [[ -z "$value" ]] && return

  if [[ "$value" =~ fuzzy-dev|dev-cluster|attic\.tinyland ]]; then
    add_error "$name points at a stale legacy endpoint"
  fi

  if [[ "$value" == *'${'* || "$value" == *'}'* ]]; then
    add_error "$name contains an unevaluated placeholder"
  fi

  if [[ ! "$value" =~ ^(grpc|grpcs):// ]]; then
    add_error "$name must use grpc:// or grpcs://"
  fi
}

remote_cache="${BAZEL_REMOTE_CACHE:-}"
remote_executor="${BAZEL_REMOTE_EXECUTOR:-}"
declared_mode="${GF_BAZEL_SUBSTRATE_MODE:-}"
execution_platform="${GF_BAZEL_REMOTE_EXECUTION_PLATFORM:-}"
repository_cache="${BAZEL_REPOSITORY_CACHE:-}"
distdir="${BAZEL_DISTDIR:-}"

expected_mode="compatibility-local-only"
if is_set "$remote_executor"; then
  expected_mode="executor-backed"
elif is_set "$remote_cache"; then
  expected_mode="shared-cache-backed"
fi

effective_mode="${declared_mode:-$expected_mode}"

case "$effective_mode" in
  compatibility-local-only | shared-cache-backed | executor-backed)
    ;;
  *)
    add_error "GF_BAZEL_SUBSTRATE_MODE must be compatibility-local-only, shared-cache-backed, or executor-backed"
    ;;
esac

if [[ -n "$declared_mode" && "$declared_mode" != "$expected_mode" ]]; then
  add_error "GF_BAZEL_SUBSTRATE_MODE=$declared_mode does not match detected substrate $expected_mode"
fi

require_supported_cache_endpoint "BAZEL_REMOTE_CACHE" "$remote_cache"
require_supported_executor_endpoint "BAZEL_REMOTE_EXECUTOR" "$remote_executor"

if is_set "$remote_executor" && ! is_set "$remote_cache"; then
  add_error "BAZEL_REMOTE_EXECUTOR requires BAZEL_REMOTE_CACHE so proof runs can separate execution from cache reuse"
fi

if [[ "$effective_mode" == "executor-backed" && -z "$execution_platform" ]]; then
  add_error "executor-backed mode requires GF_BAZEL_REMOTE_EXECUTION_PLATFORM"
fi

if $strict && [[ "$expected_mode" == "compatibility-local-only" ]]; then
  add_error "--strict requires BAZEL_REMOTE_CACHE or BAZEL_REMOTE_EXECUTOR"
fi

if [[ -n "$repository_cache" && "$repository_cache" == *'${'* ]]; then
  add_error "BAZEL_REPOSITORY_CACHE contains an unevaluated placeholder"
fi

if [[ -n "$distdir" && "$distdir" == *'${'* ]]; then
  add_error "BAZEL_DISTDIR contains an unevaluated placeholder"
fi

if $strict_nix; then
  attic_endpoint="${ATTIC_SERVER:-}"
  attic_key="${ATTIC_PUBLIC_KEY:-}"
  nix_config="${NIX_CONFIG:-}"

  if [[ -n "$attic_endpoint" || -n "$attic_key" ]]; then
    if [[ -z "$attic_endpoint" || -z "$attic_key" ]]; then
      add_error "Attic cache environment is partial; set both ATTIC_SERVER and ATTIC_PUBLIC_KEY or neither"
    fi
  fi

  if [[ "$nix_config" == *attic* && "$nix_config" != *extra-substituters* ]]; then
    add_error "NIX_CONFIG mentions Attic but does not set extra-substituters"
  fi

  if [[ "$nix_config" == *attic* && "$nix_config" != *trusted-public-keys* && "$nix_config" != *extra-trusted-public-keys* ]]; then
    add_error "NIX_CONFIG mentions Attic but does not set trusted public keys"
  fi
fi

cat <<EOF
Bazel substrate mode: ${effective_mode}
Bazel detected substrate: ${expected_mode}
Bazel remote cache: $(describe_endpoint "$remote_cache")
Bazel remote executor: $(describe_endpoint "$remote_executor")
Bazel execution platform: ${execution_platform:-unset}
Bazel repository cache: $(if [[ -n "$repository_cache" ]]; then printf 'configured'; else printf 'unset'; fi)
Bazel distdir: $(if [[ -n "$distdir" ]]; then printf 'configured'; else printf 'unset'; fi)
EOF

if ((${#errors[@]} > 0)); then
  printf '\nBazel cache/REAPI contract failed:\n' >&2
  for error in "${errors[@]}"; do
    printf '  - %s\n' "$error" >&2
  done
  exit 1
fi

printf 'Bazel cache/REAPI contract passed\n'
