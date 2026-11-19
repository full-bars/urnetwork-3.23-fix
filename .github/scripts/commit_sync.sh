#!/bin/bash
# Commit the CFAA sync to main and record the commit URL in the sync info JSON.
# cfaa_sync.sh made the sync commit on a local branch after cfaa_format_check.sh
# ran the decoder tests against the new tables. The tables are generated data,
# so the change goes straight to main without a pull request.
#
# The commit is applied on top of the current main with a cherry-pick. That keeps
# the change when main moved while the job ran, and signs it again because the
# workflow configures commit signing. A conflict or a failed push stops the job.
set -euo pipefail

SYNC_SHA=$(git rev-parse HEAD)

git fetch --quiet origin main
git checkout --quiet -B cfaa-sync-direct origin/main
git cherry-pick "$SYNC_SHA"
git push --quiet origin HEAD:main

COMMIT_URL="https://github.com/${GITHUB_REPOSITORY:-unknown}/commit/$(git rev-parse HEAD)"
echo "Committed to main: ${COMMIT_URL}"

# The notifier reads the key pr_url, so the commit URL goes there.
COMMIT_URL="$COMMIT_URL" python3 -c "
import json, os
info_path = os.environ.get('CFAA_SYNC_INFO', '/tmp/cfaa_sync_info.json')
try:
    with open(info_path) as f:
        info = json.load(f)
except Exception:
    info = {}
info['pr_url'] = os.environ['COMMIT_URL']
info['direct_commit'] = True
with open(info_path, 'w') as f:
    json.dump(info, f)
"
