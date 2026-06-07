#!/bin/sh
# logs -- shortcut to live-tail the RAMLOGS buffer if enabled.
set -eu

log_file="/dev/shm/urnetwork.log"

if [ -f "$log_file" ]; then
    exec tail -f "$log_file"
else
    echo "RAMLOGS are not enabled (or file not yet created)."
    echo "Check if URNETWORK_RAMLOGS=1 is set. If not, use 'docker logs -f' instead."
    exit 1
fi
