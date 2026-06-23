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

env -i \
  PATH="$PATH" \
  HOME="${HOME:-$tmpdir/home}" \
  BAZEL_BIN="$fake_bazel" \
  BAZEL_ARG_CAPTURE="$capture" \
  TINYLAND_CLEANUP_BAZEL_OUTPUT_USER_ROOT="$tmpdir/output" \
  TINYLAND_CLEANUP_BAZEL_DISK_CACHE="$tmpdir/disk-cache" \
  bash scripts/bazel-cache-backed.sh test //:dummy

grep -F -- "--output_user_root=$tmpdir/output" "$capture" >/dev/null
grep -Fx "test" "$capture" >/dev/null
grep -F -- "--config=ci-local" "$capture" >/dev/null
grep -F -- "--disk_cache=$tmpdir/disk-cache" "$capture" >/dev/null
grep -F -- "//:dummy" "$capture" >/dev/null

env -i \
  PATH="$PATH" \
  HOME="${HOME:-$tmpdir/home}" \
  BAZEL_BIN="$fake_bazel" \
  BAZEL_ARG_CAPTURE="$capture" \
  BAZEL_REMOTE_CACHE="grpc://bazel-cache.example.internal:9092" \
  BAZEL_REMOTE_UPLOAD="true" \
  GF_BAZEL_SUBSTRATE_MODE="shared-cache-backed" \
  bash scripts/bazel-cache-backed.sh build //:all_tests

grep -Fx "build" "$capture" >/dev/null
grep -F -- "--config=ci" "$capture" >/dev/null
grep -F -- "--remote_cache=grpc://bazel-cache.example.internal:9092" "$capture" >/dev/null
grep -F -- "--remote_upload_local_results=true" "$capture" >/dev/null
grep -F -- "--remote_download_outputs=toplevel" "$capture" >/dev/null

env -i \
  PATH="$PATH" \
  HOME="${HOME:-$tmpdir/home}" \
  BAZEL_BIN="$fake_bazel" \
  BAZEL_ARG_CAPTURE="$capture" \
  BAZEL_REMOTE_CACHE="grpc://bazel-cache.example.internal:9092" \
  BAZEL_REMOTE_EXECUTOR="grpc://rbe.example.internal:8980" \
  GF_BAZEL_SUBSTRATE_MODE="executor-backed" \
  GF_BAZEL_REMOTE_EXECUTION_PLATFORM="linux-x86_64" \
  bash scripts/bazel-cache-backed.sh test //:bazel_cache_policy_check

grep -F -- "--config=executor-backed" "$capture" >/dev/null
grep -F -- "--remote_executor=grpc://rbe.example.internal:8980" "$capture" >/dev/null
grep -F -- "--remote_default_exec_properties=gf.platform=linux-x86_64" "$capture" >/dev/null

echo "bazel-cache-backed wrapper tests passed"
