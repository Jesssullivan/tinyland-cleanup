#!/usr/bin/env bash
set -euo pipefail

if [[ -n "${TEST_SRCDIR:-}" && -n "${TEST_WORKSPACE:-}" ]]; then
  cd "${TEST_SRCDIR}/${TEST_WORKSPACE}"
else
  cd "$(dirname "${BASH_SOURCE[0]}")/../.."
fi

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

fake_bazel="$tmpdir/fake-bazel"
capture="$tmpdir/bazel-args.txt"

cat >"$fake_bazel" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$@" >"${BAZEL_ARG_CAPTURE:?}"
EOF
chmod +x "$fake_bazel"

if env -i \
  PATH="$PATH" \
  HOME="${HOME:-$tmpdir/home}" \
  BAZEL_BIN="$fake_bazel" \
  BAZEL_ARG_CAPTURE="$capture" \
  BAZEL_REMOTE_CACHE="grpc://bazel-cache.example.internal:9092" \
  BAZEL_REMOTE_EXECUTOR="grpc://rbe.example.internal:8980" \
  GF_BAZEL_SUBSTRATE_MODE="executor-backed" \
  GF_BAZEL_REMOTE_EXECUTION_PLATFORM="linux-x86_64" \
  bash scripts/bazel-rbe-proof.sh --target //:bazel_cache_policy_check >"$tmpdir/missing-mode.out" 2>&1; then
  echo "expected proof wrapper to require explicit proof mode" >&2
  exit 1
fi
grep -F "GF_RBE_PROOF_MODE=explicit" "$tmpdir/missing-mode.out" >/dev/null

env -i \
  PATH="$PATH" \
  HOME="${HOME:-$tmpdir/home}" \
  BAZEL_BIN="$fake_bazel" \
  BAZEL_ARG_CAPTURE="$capture" \
  GF_RBE_PROOF_MODE="explicit" \
  BAZEL_REMOTE_CACHE="grpc://bazel-cache.example.internal:9092" \
  BAZEL_REMOTE_EXECUTOR="grpc://rbe.example.internal:8980" \
  GF_BAZEL_SUBSTRATE_MODE="executor-backed" \
  GF_BAZEL_REMOTE_EXECUTION_PLATFORM="linux-x86_64" \
  bash scripts/bazel-rbe-proof.sh \
    --target //:bazel_cache_policy_check \
    --bazel-command test \
    -- --test_output=errors

grep -Fx "test" "$capture" >/dev/null
grep -F -- "--remote_accept_cached=false" "$capture" >/dev/null
grep -F -- "--remote_executor=grpc://rbe.example.internal:8980" "$capture" >/dev/null
grep -F -- "--test_output=errors" "$capture" >/dev/null
grep -F -- "//:bazel_cache_policy_check" "$capture" >/dev/null

env -i \
  PATH="$PATH" \
  HOME="${HOME:-$tmpdir/home}" \
  BAZEL_BIN="$fake_bazel" \
  BAZEL_ARG_CAPTURE="$capture" \
  GF_RBE_PROOF_MODE="explicit" \
  BAZEL_REMOTE_CACHE="grpc://bazel-cache.example.internal:9092" \
  BAZEL_REMOTE_EXECUTOR="grpc://rbe.example.internal:8980" \
  GF_BAZEL_SUBSTRATE_MODE="executor-backed" \
  GF_BAZEL_REMOTE_EXECUTION_PLATFORM="linux-x86_64" \
  bash scripts/bazel-rbe-proof.sh --target //:bazel_cache_policy_check

grep -F -- "--remote_accept_cached=false" "$capture" >/dev/null
grep -F -- "//:bazel_cache_policy_check" "$capture" >/dev/null

if env -i \
  PATH="$PATH" \
  HOME="${HOME:-$tmpdir/home}" \
  BAZEL_BIN="$fake_bazel" \
  BAZEL_ARG_CAPTURE="$capture" \
  GF_RBE_PROOF_MODE="explicit" \
  BAZEL_REMOTE_CACHE="grpc://bazel-cache.example.internal:9092" \
  BAZEL_REMOTE_EXECUTOR="grpc://rbe.example.internal:8980" \
  GF_BAZEL_SUBSTRATE_MODE="executor-backed" \
  GF_BAZEL_REMOTE_EXECUTION_PLATFORM="linux-x86_64" \
  bash scripts/bazel-rbe-proof.sh --target //:all_tests >"$tmpdir/unlisted.out" 2>&1; then
  echo "expected unlisted target to fail eligibility validation" >&2
  exit 1
fi
grep -F "explicitly blocked from RBE proof" "$tmpdir/unlisted.out" >/dev/null

echo "bazel-rbe-proof wrapper tests passed"
