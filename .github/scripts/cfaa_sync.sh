#!/bin/bash
# CFAA blocklist sync helper: fetches upstream file, creates branch, commits.
# Args: upstream_sha upstream_v4 upstream_v6 fork_v4 fork_v6
# Uses a STABLE branch name so the workflow's dedup guard can find prior PRs.
set -euo pipefail

UPSTREAM_SHA="$1"
UPSTREAM_V4="$2"
UPSTREAM_V6="$3"
FORK_V4="$4"
FORK_V6="$5"
UPSTREAM_FILE="ip_security_cfaa_block.go"

git config user.name "full-bars"
git config user.email "45684698+full-bars@users.noreply.github.com"

# Use stable branch name so the dedup guard in the workflow can find existing PRs.
# If the branch already exists (from a prior run), reset to origin/main first.
BRANCH_NAME="chore/cfaa-blocklist-sync"
git reset --hard origin/main --quiet
git checkout -B "$BRANCH_NAME"

# Fetch and overwrite with upstream version
git show "${UPSTREAM_SHA}:${UPSTREAM_FILE}" > "$UPSTREAM_FILE"

V4_DELTA=$((UPSTREAM_V4 - FORK_V4))
V6_DELTA=$((UPSTREAM_V6 - FORK_V6))

git add "$UPSTREAM_FILE"
git commit -S -m "chore(security): sync CFAA blocklist from upstream ${UPSTREAM_SHA} | IPv4: ${FORK_V4}->${UPSTREAM_V4} (${V4_DELTA}), IPv6: ${FORK_V6}->${UPSTREAM_V6} (${V6_DELTA}) | data-only refresh"

# Write PR body (Fix: workflow previously referenced /tmp/cfaa_pr_body.md which was never created)
cat > /tmp/cfaa_pr_body.md << PRBODY
Automated CFAA blocklist sync from urnetwork/connect upstream commit ${UPSTREAM_SHA}.

Changes:
- IPv4: ${FORK_V4} -> ${UPSTREAM_V4} (${V4_DELTA} entries)
- IPv6: ${FORK_V6} -> ${UPSTREAM_V6} (${V6_DELTA} entries)

Integrity check: Constants verified to match table record counts.

Upstream SHA: ${UPSTREAM_SHA}
PRBODY

# Update sync info JSON with branch and sync status
write_sync_info() {
  local branch_name="$1"
  python3 -c "
import json, os
info_path = '/tmp/cfaa_sync_info.json'
try:
    with open(info_path) as f:
        info = json.load(f)
except:
    info = {}
info['branch'] = '${branch_name}'
info['sync_needed'] = True
with open(info_path, 'w') as f:
    json.dump(info, f)
print(f'Sync info updated: branch=${branch_name}')
"
}
write_sync_info "$BRANCH_NAME"
