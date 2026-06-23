#!/usr/bin/env bash
set -euo pipefail

if [[ -n "${TEST_SRCDIR:-}" && -n "${TEST_WORKSPACE:-}" ]]; then
  cd "${TEST_SRCDIR}/${TEST_WORKSPACE}"
else
  cd "$(dirname "${BASH_SOURCE[0]}")/../.."
fi

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

run_contract() {
  env -i \
    PATH="$PATH" \
    HOME="${HOME:-$tmpdir/home}" \
    "$@" \
    bash scripts/validation/bazel-cache-contract.sh
}

run_contract >"$tmpdir/local.out"
grep -F "Bazel substrate mode: compatibility-local-only" "$tmpdir/local.out" >/dev/null
grep -F "Bazel cache/REAPI contract passed" "$tmpdir/local.out" >/dev/null

env -i \
  PATH="$PATH" \
  HOME="${HOME:-$tmpdir/home}" \
  BAZEL_REMOTE_CACHE="grpc://bazel-cache.example.internal:9092" \
  GF_BAZEL_SUBSTRATE_MODE="shared-cache-backed" \
  bash scripts/validation/bazel-cache-contract.sh --strict >"$tmpdir/cache.out"
grep -F "Bazel substrate mode: shared-cache-backed" "$tmpdir/cache.out" >/dev/null
grep -F "Bazel remote cache: grpc://<configured>" "$tmpdir/cache.out" >/dev/null

if env -i \
  PATH="$PATH" \
  HOME="${HOME:-$tmpdir/home}" \
  BAZEL_REMOTE_EXECUTOR="grpc://rbe.example.internal:8980" \
  GF_BAZEL_SUBSTRATE_MODE="executor-backed" \
  GF_BAZEL_REMOTE_EXECUTION_PLATFORM="linux-x86_64" \
  bash scripts/validation/bazel-cache-contract.sh --strict >"$tmpdir/executor-missing-cache.out" 2>&1; then
  echo "expected executor without cache to fail" >&2
  exit 1
fi
grep -F "BAZEL_REMOTE_EXECUTOR requires BAZEL_REMOTE_CACHE" "$tmpdir/executor-missing-cache.out" >/dev/null

if env -i \
  PATH="$PATH" \
  HOME="${HOME:-$tmpdir/home}" \
  BAZEL_REMOTE_CACHE="grpc://bazel-cache.fuzzy-dev.invalid:9092" \
  GF_BAZEL_SUBSTRATE_MODE="shared-cache-backed" \
  bash scripts/validation/bazel-cache-contract.sh --strict >"$tmpdir/stale.out" 2>&1; then
  echo "expected stale cache endpoint to fail" >&2
  exit 1
fi
grep -F "stale legacy cache endpoint" "$tmpdir/stale.out" >/dev/null

echo "bazel-cache-contract tests passed"
