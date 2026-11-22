#!/bin/sh
# After install and after upgrade, on deb, rpm and Arch packages. On Arch this
# body is pasted into post_install() and post_upgrade(), so it must not exit.

# Create the prbot user. systemd-sysusers reads the packaged sysusers.d file;
# useradd covers minimal systems and containers without systemd.
if ! getent passwd prbot >/dev/null 2>&1; then
    if command -v systemd-sysusers >/dev/null 2>&1; then
        systemd-sysusers /usr/lib/sysusers.d/prbot.conf ||
            echo "prbot: systemd-sysusers failed; create the prbot user before starting the service" >&2
    elif command -v useradd >/dev/null 2>&1; then
        useradd --system --user-group --home-dir /var/lib/prbot --no-create-home \
            --shell /usr/sbin/nologin --comment "LLM pull request review bot" prbot ||
            echo "prbot: useradd failed; create the prbot user before starting the service" >&2
    else
        echo "prbot: no systemd-sysusers or useradd; create the prbot user before starting the service" >&2
    fi
fi

# Load the unit, and on an upgrade restart a running bot on the new binary.
# try-restart leaves a stopped service stopped, so a fresh install doesn't start it.
if [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || :
    systemctl try-restart prbot.service >/dev/null 2>&1 || :
fi
