#!/bin/sh
# Runs as RPM %pre / DEB preinst, BEFORE files are unpacked, so the
# edookit-mcp user/group exists in time for file ownership to resolve.
set -e

if ! getent group edookit-mcp >/dev/null 2>&1; then
    groupadd --system edookit-mcp
fi
if ! getent passwd edookit-mcp >/dev/null 2>&1; then
    useradd --system \
        --gid edookit-mcp \
        --no-create-home \
        --home-dir /var/lib/edookit-mcp \
        --shell /sbin/nologin \
        --comment "edookit-mcp service" \
        edookit-mcp
fi

exit 0
