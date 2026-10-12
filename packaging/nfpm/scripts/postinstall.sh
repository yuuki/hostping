#!/bin/sh
# Runs on install and upgrade (deb "configure", rpm $1 = 1 or 2). It reloads
# systemd so it sees the unit, and neither enables nor restarts the service:
# check `hostpingctl topology` and /etc/default/hostping before
# `systemctl enable --now hostping` (docs/operations.md), and restart a
# running daemon yourself after an upgrade.
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi

exit 0
