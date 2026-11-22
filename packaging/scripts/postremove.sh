#!/bin/sh
# After removal (and, on deb and rpm, after the old version is replaced).
# The prbot user and /var/lib/prbot are kept: the database there holds the
# GitHub App key and every org's settings. Delete them by hand if wanted.
if [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || :
fi
