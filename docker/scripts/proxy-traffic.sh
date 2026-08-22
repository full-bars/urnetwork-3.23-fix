#!/bin/sh
# proxy-traffic -- show the persistent proxy bandwidth and session record inside the container.
# RAMLOGS-independent: these files always live on the config volume.
set -eu

health_dir="${URNETWORK_PROXY_HEALTH_DIR:-/root/.urnetwork}"
traffic_file="$health_dir/proxy_traffic.state"

if [ -f "$traffic_file" ]; then
    cat "$traffic_file"
else
    echo "No traffic report yet at $traffic_file (waiting for first heartbeat?)."
fi
