#!/usr/bin/env bash
# Builds miranda-medical-card for linux/amd64 and ships the binary plus the
# systemd unit to the server. config/*.yaml and .env are never touched —
# they hold server-specific secrets and are managed separately on the
# target host (see this repo's README for the one-time first-deploy step
# that copies them there).
#
# Also builds and ships medical-dev (docs/cli/medical_dev.md) alongside the
# service binary — it's the only supported way to run one-off operations
# against the live database (reprocessing a document, backfilling data;
# see e.g. its backfill-titles command) and is easy to forget to rebuild by
# hand after code changes, leaving a stale copy on the server silently
# diverging from what the running service actually does. Not a systemd
# service (it's a CLI, invoked on demand over SSH), so no unit file for it.
set -euo pipefail
cd "$(dirname "$0")/.."

remote_host="archer@miranda"
remote_dir="miranda-medical-card"
service_name="miranda-medical-card"
dev_tool_name="medical-dev"
build_out="dist/miranda-medical-card-linux-amd64"
dev_build_out="dist/medical-dev-linux-amd64"
unit_file="$(mktemp)"
trap 'rm -f "$unit_file"' EXIT

echo "==> Building $build_out (GOOS=linux GOARCH=amd64, CGO_ENABLED=0)"
mkdir -p dist
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$build_out" ./cmd/miranda-medical-card

echo "==> Building $dev_build_out (GOOS=linux GOARCH=amd64, CGO_ENABLED=0)"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$dev_build_out" ./cmd/medical-dev

cat >"$unit_file" <<EOF
[Unit]
Description=${service_name} MCP server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%h/${remote_dir}
ExecStart=%h/${remote_dir}/${service_name}
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
EOF

echo "==> Uploading binaries and systemd unit to ${remote_host}:~/${remote_dir}"
ssh "$remote_host" "mkdir -p ~/${remote_dir}/data ~/.config/systemd/user"
scp -q "$build_out" "${remote_host}:~/${remote_dir}/${service_name}.new"
scp -q "$dev_build_out" "${remote_host}:~/${remote_dir}/${dev_tool_name}.new"
scp -q "$unit_file" "${remote_host}:~/.config/systemd/user/${service_name}.service"

echo "==> Installing and restarting on ${remote_host}"
ssh "$remote_host" bash -s -- "$remote_dir" "$service_name" "$dev_tool_name" <<'REMOTE'
set -euo pipefail
remote_dir="$1"
service_name="$2"
dev_tool_name="$3"

cd ~/"$remote_dir"

chmod +x ~/"$remote_dir"/"$service_name".new
mv ~/"$remote_dir"/"$service_name".new ~/"$remote_dir"/"$service_name"

# medical-dev is a CLI, not a running service — no process to stop/start,
# just swap the binary in (still via .new + mv, so a mid-transfer failure
# never leaves a half-written, unexecutable binary in place).
chmod +x ~/"$remote_dir"/"$dev_tool_name".new
mv ~/"$remote_dir"/"$dev_tool_name".new ~/"$remote_dir"/"$dev_tool_name"

systemctl --user daemon-reload
systemctl --user enable --now "$service_name" >/dev/null
systemctl --user restart "$service_name"

if [ "$(loginctl show-user "$(whoami)" --property=Linger --value 2>/dev/null)" != "yes" ]; then
  echo "WARNING: lingering is not enabled — service will stop when SSH session ends." >&2
  echo "  Run once: loginctl enable-linger $(whoami)" >&2
fi

echo "--- systemctl --user status $service_name ---"
systemctl --user --no-pager -l status "$service_name" || true

echo "--- health check ---"
# -k: the server presents a self-signed certificate (see internal/tlscert),
# so there's no CA chain for curl to verify here. TLS is still negotiated;
# this only skips certificate validation for this local liveness probe.
for i in 1 2 3 4 5; do
  if curl -fsSk -m 2 https://localhost:8791/healthz; then
    echo
    exit 0
  fi
  sleep 1
done
echo "healthz check failed after restart" >&2
exit 1
REMOTE

echo "==> Deploy complete"
