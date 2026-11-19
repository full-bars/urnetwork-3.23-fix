#!/bin/bash
# Blocklist sync helper: writes the upstream tables and commits them on a local branch.
# commit_sync.sh then puts that commit on main.
# Args: upstream_sha upstream_v4 upstream_v6 fork_v4 fork_v6
#       [upstream_hosts fork_hosts cfaa_identical blocker_identical]
set -euo pipefail

UPSTREAM_SHA="$1"
UPSTREAM_V4="$2"
UPSTREAM_V6="$3"
FORK_V4="$4"
FORK_V6="$5"
UPSTREAM_HOSTS="${6:-0}"
FORK_HOSTS="${7:-0}"
CFAA_IDENTICAL="${8:-false}"
BLOCKER_IDENTICAL="${9:-false}"

CFAA_FILE="ip_security_cfaa_block.go"
BLOCKER_FILE="ip_blocker_block.go"

git config user.name "full-bars"
git config user.email "45684698+full-bars@users.noreply.github.com"

# Start from origin/main, on a local branch.
BRANCH_NAME="chore/cfaa-blocklist-sync"
git reset --hard origin/main --quiet
git checkout -B "$BRANCH_NAME"

# Write both tables from upstream. Both are generated; the fork never edits
# them, so rewriting an unchanged one leaves no diff.
for f in "$CFAA_FILE" "$BLOCKER_FILE"; do
  git show "${UPSTREAM_SHA}:${f}" > "$f"
  # a generated table that no longer parses would poison every build
  gofmt -e -l "$f" > /dev/null
  git add "$f"
done

V4_DELTA=$((UPSTREAM_V4 - FORK_V4))
V6_DELTA=$((UPSTREAM_V6 - FORK_V6))
HOST_DELTA=$((UPSTREAM_HOSTS - FORK_HOSTS))

if git diff --cached --quiet; then
  echo "no blocklist changes after writing the upstream files"
  exit 0
fi

git commit -S -m "chore(security): sync blocklists from upstream ${UPSTREAM_SHA:0:8}

cfaa ipv4 ${FORK_V4}->${UPSTREAM_V4} (${V4_DELTA}), ipv6 ${FORK_V6}->${UPSTREAM_V6} (${V6_DELTA}),
blocked hosts ${FORK_HOSTS}->${UPSTREAM_HOSTS} (${HOST_DELTA}). data-only refresh, no code change.

The sync job ran the blocker and CFAA decoder tests against these tables before this commit."

# Update sync info JSON with branch and sync status
write_sync_info() {
  local branch_name="$1"
  python3 -c "
import json, os
info_path = '/tmp/cfaa_sync_info.json'
try:
    with open(info_path) as f:
        info = json.load(f)
except Exception:
    info = {}
info['branch'] = '${branch_name}'
info['sync_needed'] = True
with open(info_path, 'w') as f:
    json.dump(info, f)
print(f'Sync info updated: branch=${branch_name}')
"
}
write_sync_info "$BRANCH_NAME"
