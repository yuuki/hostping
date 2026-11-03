#!/bin/sh
# Runs on install and upgrade (deb "configure", rpm $1 = 1 or 2). It reloads
# systemd so it sees the unit, and neither enables nor restarts the service:
# check `hostpingctl topology` and /etc/default/hostping before
# `systemctl enable --now hostping` (docs/operations.md), and restart a
# running daemon yourself after an upgrade.
set -e

# /etc/default/hostping may hold an OTLP bearer token. Packages up to 0.2.2
# installed it world-readable, and a kept config file keeps its mode across
# upgrades, so tighten it here.
if [ -f /etc/default/hostping ]; then
    chmod go-rwx /etc/default/hostping || true
fi

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi

exit 0
