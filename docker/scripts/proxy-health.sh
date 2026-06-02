#!/bin/sh
# proxy-health -- show the persistent dead/degraded proxy record inside the container.
# RAMLOGS-independent: these files always live on the config volume.
set -eu

health_dir="${URNETWORK_PROXY_HEALTH_DIR:-/root/.urnetwork}"
state_file="$health_dir/proxy_health.state"
log_file="$health_dir/proxy_health.log"

if [ -f "$state_file" ]; then
    echo "== Current proxy health ($state_file) =="
    cat "$state_file"
else
    echo "No snapshot yet at $state_file (waiting for first heartbeat?)."
fi

if [ -f "$log_file" ]; then
    echo
    echo "== Proxy health events ($log_file) -- Ctrl-C to stop =="
    tail -n 20 -f "$log_file"
else
    echo "No event log yet at $log_file."
fi
