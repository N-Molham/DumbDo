#!/bin/sh
# Started as root (default): fix ownership of mounted data volume, drop to 568:568.
# Started as a non-root user (e.g. compose user: 568:568): run as-is — the host
# must own the data volume with that uid/gid.
if [ "$(id -u)" = "0" ]; then
    chown -R 568:568 /app/data 2>/dev/null
    exec su-exec 568:568 "${@}"
fi
exec "${@}"
