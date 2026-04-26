#!/usr/bin/env sh
set -eu

if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload >/dev/null 2>&1 || true
fi

cat <<'MSG'
tinyland-cleanup installed.

Review configuration:
  /etc/tinyland-cleanup/config.yaml

Run a dry-run before enabling the daemon:
  tinyland-cleanup --once --dry-run --level critical --output json --config /etc/tinyland-cleanup/config.yaml

Enable manually when ready:
  systemctl enable --now tinyland-cleanup.service
MSG
