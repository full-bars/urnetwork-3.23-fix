#!/bin/bash
# Write sync metadata to JSON for the notification script.
# Args: upstream_sha upstream_v4 upstream_v6 fork_v4 fork_v6 files_identical
#       [upstream_hosts fork_hosts cfaa_identical blocker_identical]
set -euo pipefail

UPSTREAM_SHA="$1"
UPSTREAM_V4="$2"
UPSTREAM_V6="$3"
FORK_V4="$4"
FORK_V6="$5"
FILES_IDENTICAL="$6"
UPSTREAM_HOSTS="${7:-0}"
FORK_HOSTS="${8:-0}"
CFAA_IDENTICAL="${9:-unknown}"
BLOCKER_IDENTICAL="${10:-unknown}"

TIMESTAMP=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# Determine sync_needed: true if files are NOT identical
if [ "$FILES_IDENTICAL" = "true" ]; then
  SYNC_NEEDED="false"
else
  SYNC_NEEDED="true"
fi

cat > /tmp/cfaa_sync_info.json << EOF
{
  "timestamp": "${TIMESTAMP}",
  "upstream_sha": "${UPSTREAM_SHA}",
  "upstream_v4": ${UPSTREAM_V4},
  "upstream_v6": ${UPSTREAM_V6},
  "fork_v4": ${FORK_V4},
  "fork_v6": ${FORK_V6},
  "upstream_hosts": ${UPSTREAM_HOSTS},
  "fork_hosts": ${FORK_HOSTS},
  "cfaa_identical": "${CFAA_IDENTICAL}",
  "blocker_identical": "${BLOCKER_IDENTICAL}",
  "sync_needed": ${SYNC_NEEDED}
}
EOF

cat /tmp/cfaa_sync_info.json
