#!/bin/sh
# Stops/disables the service ONLY on a real uninstall (not on upgrade — both
# RPM and DEB run this script also when replacing the package).
#   RPM:  $1 == 0 uninstall, $1 == 1 upgrade
#   DEB:  $1 == "remove"     (purge / pure uninstall), "upgrade" otherwise
set -e

if [ "$1" = "0" ] || [ "$1" = "remove" ]; then
    systemctl --no-reload disable --now edookit-mcp.service >/dev/null 2>&1 || true
fi

exit 0
