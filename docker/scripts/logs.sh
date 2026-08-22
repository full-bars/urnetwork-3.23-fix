#!/bin/sh
# logs -- shortcut to live-tail the RAMLOGS buffer if enabled.
set -eu

log_file="/dev/shm/urnetwork.log"
mode="${1:-}"

if [ "$mode" = "dump" ]; then
    if [ -f "$log_file" ]; then
        cp "$log_file" "/root/urlogs.txt"
        echo "Logs successfully dumped to /root/urlogs.txt"
        exit 0
    else
        echo "RAMLOGS are not enabled. Dumping journald logs is not supported in Docker."
        exit 1
    fi
fi

if [ -f "$log_file" ]; then
    if [ "$mode" = "full" ] || [ "$mode" = "all" ]; then
        exec tail -n +1 -f "$log_file"
    else
        exec tail -n 1000 -f "$log_file"
    fi
else
    echo "RAMLOGS are not enabled (or file not yet created)."
    echo "Check if URNETWORK_RAMLOGS=1 is set. If not, use 'docker logs -f' instead."
    exit 1
fi
