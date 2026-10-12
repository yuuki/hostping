#!/bin/sh
# Stops and disables the service on removal, never on an upgrade: the old
# package's script runs before the new files are unpacked, and stopping there
# would leave the host undiagnosed until someone restarts it.
#   deb prerm: $1 is "remove", or "upgrade" / "failed-upgrade".
#   rpm %preun: $1 is the number of versions left installed: 0 on erase,
#               1 or more during an upgrade.
set -e

stop=yes
case "$1" in
    upgrade | failed-upgrade) stop=no ;;
    '' | *[!0-9]*) ;;
    *) [ "$1" -ge 1 ] && stop=no ;;
esac

if [ "$stop" = yes ] && command -v systemctl >/dev/null 2>&1; then
    systemctl stop hostping.service || true
    systemctl disable hostping.service || true
fi

exit 0
