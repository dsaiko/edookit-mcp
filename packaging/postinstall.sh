#!/bin/sh
# Runs as RPM %post / DEB postinst, AFTER files are unpacked. Reloads systemd
# so the new unit is visible, and restarts the service on upgrade so it picks
# up the new binary. The arg distinguishes install vs upgrade across formats:
#   RPM:  $1 == 1 install, $1 == 2 upgrade
#   DEB:  $1 == "configure" (with $2 unset on first install, set on upgrade)
set -e

systemctl daemon-reload >/dev/null 2>&1 || true

# Upgrade path — restart if it's currently running. (try-restart is a no-op
# when the unit isn't running, so this is safe on fresh installs too.)
if [ "$1" = "2" ] || { [ "$1" = "configure" ] && [ -n "$2" ]; }; then
    systemctl try-restart edookit-mcp.service >/dev/null 2>&1 || true
fi

# First-install hint (only on a clean install, not an upgrade).
if [ "$1" = "1" ] || { [ "$1" = "configure" ] && [ -z "$2" ]; }; then
    cat <<'EOF'

edookit-mcp installed. Next steps:
  1) sudo $EDITOR /etc/edookit-mcp/edookit-mcp.env  # set EDOOKIT_URL/USER/PASS
  2) sudo systemctl enable --now edookit-mcp

The service listens on 127.0.0.1:9000/mcp by default. Put a TLS reverse proxy
(+ OAuth gateway) in front of it before exposing publicly.

EOF
fi

exit 0
