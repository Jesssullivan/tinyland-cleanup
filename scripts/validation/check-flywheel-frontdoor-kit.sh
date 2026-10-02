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

for path in Justfile justfile.flywheel .bazelrc.flywheel flake.nix flake.lock .gitignore; do
  [[ -f "$path" ]] || fail "$path is missing"
done

[[ -f .github/workflows/ci.yml ]] || fail ".github/workflows/ci.yml is missing"

require_contains Justfile '^import\? "justfile\.flywheel"$' "front-door Just import"

require_contains justfile.flywheel '^flywheel-doctor\b' "flywheel-doctor recipe"
require_contains justfile.flywheel '^flywheel-verify\b' "flywheel-verify recipe"
require_contains justfile.flywheel '^flywheel-bazel\b' "flywheel-bazel recipe"
require_contains justfile.flywheel './scripts/gloriousflywheel-bazel\.sh' "checked-in GloriousFlywheel Bazel wrapper invocation"
require_contains justfile.flywheel './scripts/validation/bazel-cache-contract\.sh --strict' "strict checked-in cache verification"
reject_contains justfile.flywheel '^set dotenv-load' "global Just dotenv-load setting"
reject_contains justfile.flywheel '^flywheel-enroll\b|^flywheel-consumer-env\b' "credential/profile materializer in unprivileged consumer kit"
reject_contains justfile.flywheel '_flywheel_env|\.env\.flywheel|exec gloriousflywheel-bazel' "ambient or packaged private-tool dependency"

require_contains flake.nix '\bbazelisk\b' "public pinned Bazel launcher"
require_contains flake.nix '\bjust\b' "public pinned operator frontend"
reject_contains flake.nix 'tinyland-inc/GloriousFlywheel|gloriousflywheel-frontdoor-tools|gloriousflywheel-profile-tools' "private or auth-capable devshell input"
reject_contains flake.lock 'tinyland-inc|[Gg]lorious[Ff]lywheel' "private source in the Nix lock closure"
require_contains .gitignore '^\.env\.\*$' "local environment ignore rule"

reject_contains scripts/gloriousflywheel-bazel.sh 'source .*env|\.env\.flywheel' "automatic repo-local environment sourcing"
require_contains scripts/gloriousflywheel-bazel.sh 'GITHUB_EVENT_NAME.*pull_request.*merge_group' "pull-request and merge-group upload refusal"

require_contains .bazelrc 'try-import %workspace%/\.bazelrc\.flywheel' "front-door Bazel rc import"
require_contains .bazelrc.flywheel '^build:ci-cached\b' "shared-cache Bazel config"
require_contains .bazelrc.flywheel '^build:ci-cached --remote_upload_local_results=false$' "read-only shared-cache build default"
require_contains .bazelrc.flywheel '^test:ci-cached --remote_upload_local_results=false$' "read-only shared-cache test default"
reject_contains .bazelrc.flywheel 'remote_upload_local_results=true' "cache publication default"
require_contains .bazelrc.flywheel '^build:executor-backed --config=ci-cached$' "executor-backed Bazel config"
require_contains .bazelrc.flywheel '^build:executor-backed --remote_local_fallback=false$' "executor fallback refusal"
reject_contains .bazelrc.flywheel 'remote_cache=' "hard-coded remote cache endpoint"
reject_contains .bazelrc.flywheel 'remote_executor=' "hard-coded remote executor endpoint"
reject_contains .bazelrc.flywheel '\.env\.flywheel' "repo-local environment fallback"
reject_contains .bazelrc.flywheel 'ubuntu-latest' "hosted-runner fallback"
reject_contains .bazelrc.flywheel 'localhost|127\.0\.0\.1' "local proof endpoint"

require_contains .github/workflows/ci.yml 'runs-on: tinyland-nix' "shared tinyland-nix CI runner"
require_contains .github/workflows/ci.yml '^  merge_group:$' "merge-queue CI trigger"
require_contains .github/workflows/ci.yml '^permissions:$' "explicit CI permission boundary"
require_contains .github/workflows/ci.yml '^  contents: read$' "read-only CI contents permission"
require_contains .github/workflows/ci.yml 'persist-credentials: false' "non-persisted CI checkout credential"
require_contains .github/workflows/ci.yml 'GF_BAZEL_REMOTE_UPLOAD: "false"' "PR cache upload refusal"
require_contains .github/workflows/ci.yml 'GF_BAZEL_SUBSTRATE_MODE: shared-cache-backed' "PR shared-cache-only mode"
require_contains .github/workflows/ci.yml 'BAZEL_REMOTE_EXECUTOR: ""' "PR executor credential removal"
require_contains .github/workflows/ci.yml 'BAZEL_REMOTE_HEADER: ""' "PR remote header removal"
require_contains .github/workflows/ci.yml 'GITHUB_TOKEN: ""' "PR ambient GitHub token removal"
reject_contains .github/workflows/ci.yml 'ubuntu-latest' "hosted-runner fallback"
reject_contains .github/workflows/ci.yml 'DeterminateSystems/nix-installer-action' "hosted-runner Nix installer fallback"

if [[ -f .github/workflows/docs-deploy.yml ]]; then
  require_contains .github/workflows/docs-deploy.yml 'pull_request:' "docs deploy pull-request build trigger"
  require_contains .github/workflows/docs-deploy.yml '^  merge_group:$' "docs merge-queue build trigger"
  require_contains .github/workflows/docs-deploy.yml 'runs-on: tinyland-nix' "docs deploy shared runner"
  require_contains .github/workflows/docs-deploy.yml '^  build:$' "separate unprivileged docs build job"
  require_contains .github/workflows/docs-deploy.yml '^      contents: read$' "read-only docs build permission"
  require_contains .github/workflows/docs-deploy.yml 'persist-credentials: false' "non-persisted docs checkout credential"
  require_contains .github/workflows/docs-deploy.yml 'BAZEL_REMOTE_EXECUTOR: ""' "docs executor credential removal"
  require_contains .github/workflows/docs-deploy.yml 'BAZEL_REMOTE_HEADER: ""' "docs remote header removal"
  require_contains .github/workflows/docs-deploy.yml '^      pages: write$' "deploy-only Pages write permission"
  require_contains .github/workflows/docs-deploy.yml '^      id-token: write$' "deploy-only OIDC permission"
  require_contains .github/workflows/docs-deploy.yml "if: github.event_name == 'push' && github.ref == 'refs/heads/main'" "protected-main-only docs deploy gate"
  require_contains .github/workflows/docs-deploy.yml 'group: pages-\$\{\{ github.ref \}\}' "ref-scoped docs concurrency"
  require_contains .github/workflows/docs-deploy.yml "'flake.lock'" "docs build lockfile trigger"
  require_contains .github/workflows/docs-deploy.yml "'scripts/gloriousflywheel-bazel.sh'" "docs build consumer-kit trigger"
  require_contains .github/workflows/docs-deploy.yml 'just flywheel-build //docs:site' "docs deploy front-door build"
  require_contains .github/workflows/docs-deploy.yml 'just flywheel-info bazel-bin' "docs deploy Bazel output discovery"
  reject_contains .github/workflows/docs-deploy.yml 'ubuntu-latest' "hosted runner fallback in docs deploy"
  reject_contains .github/workflows/docs-deploy.yml 'DeterminateSystems/nix-installer-action' "hosted-runner Nix installer fallback in docs deploy"
  reject_contains .github/workflows/docs-deploy.yml 'cp -RL bazel-bin/' "docs deploy convenience-symlink staging"
fi

echo "Flywheel front-door kit check passed"
