#!/bin/sh
# Drops the removed unit from systemd's view (deb postrm, rpm %postun).
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi

exit 0
