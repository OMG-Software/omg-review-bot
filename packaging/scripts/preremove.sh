#!/bin/sh
# Before removal. deb and rpm also run it on the old package during an upgrade,
# so stop and disable the service only on a real removal:
#   rpm: $1 is the number of versions left installed (0 on removal, 1 on upgrade)
#   deb: $1 is "remove", "upgrade", "failed-upgrade" or "deconfigure"
#   Arch: pre_remove runs only on removal (upgrades run pre_upgrade); $1 is the version
case "$1" in
    1 | upgrade | failed-upgrade) ;;
    *)
        if command -v systemctl >/dev/null 2>&1; then
            systemctl --no-reload disable prbot.service >/dev/null 2>&1 || :
            if [ -d /run/systemd/system ]; then
                systemctl stop prbot.service >/dev/null 2>&1 || :
            fi
        fi
        ;;
esac
