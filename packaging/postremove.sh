#!/bin/sh
# Final cleanup. We deliberately keep the edookit-mcp user, the env file and
# the state/cache dirs around so a reinstall preserves config and cookies.
# A full purge (with `apt purge` or manual cleanup on RPM) is the admin's call.
set -e

systemctl daemon-reload >/dev/null 2>&1 || true

exit 0
