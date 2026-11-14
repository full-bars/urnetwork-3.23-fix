#!/bin/bash
# Format guard for the blocklist sync.
# Args: upstream_sha
#
# The sync copies two generated tables from upstream. Upstream can change the table FORMAT
# (and with it the decoder in ip_blocker.go, the generator in blocker/ and their tests) in
# the same window. Copying only the tables then leaves the fork's decoder unable to read them.
#
# This script writes the upstream tables into the working tree, runs the fork's decoder and
# generator tests against them, and restores the tree.
#   exit 0  the fork reads the new tables, the sync may go on
#   exit 1  it does not. No PR must be opened. The reason and the upstream commits that changed
#           the format code are written to /tmp/cfaa_sync_info.json for the notifier.
set -uo pipefail

SHA="${1:?upstream sha}"
TABLES=(ip_security_cfaa_block.go ip_blocker_block.go)
# the code that defines the format, the tables themselves are not listed
FORMAT_PATHS=(ip_blocker.go ip_security_cfaa.go blocker)
TEST_RUN='Blocker|Cfaa|CFAA|Packed|Generated|Provenance|Telegram|Reflector|Security'
LOG=/tmp/cfaa_format_check.log
INFO=/tmp/cfaa_sync_info.json

restore() { git checkout -- "${TABLES[@]}" 2>/dev/null || true; }
trap restore EXIT

for f in "${TABLES[@]}"; do
  git show "${SHA}:${f}" > "$f" || { echo "cannot read ${f} at ${SHA}"; exit 2; }
done

echo "checking that the fork decoder reads the upstream tables at ${SHA:0:8}"
go test -count=1 -run "$TEST_RUN" . ./blocker/... > "$LOG" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then
  echo "format check passed"
  tail -n 5 "$LOG"
  exit 0
fi

echo "::error::the fork cannot read the upstream blocklist tables, upstream changed the table format"
grep -E '^(--- FAIL|FAIL|panic:)' "$LOG" | head -n 12 || true

FAILED=$(grep -E '^--- FAIL' "$LOG" | sed 's/^--- FAIL: //; s/ (.*//' | head -n 8 | paste -sd, -)
# the sync fetches one upstream commit, so ask the API which recent commits changed the format code
COMMITS=$(for p in "${FORMAT_PATHS[@]}"; do
  gh api "repos/urnetwork/connect/commits?sha=${SHA}&path=${p}&per_page=4" \
    --jq '.[] | "\(.commit.author.date[0:10]) \(.sha[0:9]) \(.commit.message | split("\n")[0])"' 2>/dev/null
done | sort -ru | head -n 8 | paste -sd'|' -)

FAILED="$FAILED" COMMITS="$COMMITS" SHA="$SHA" python3 -c "
import json, os
info_path = '$INFO'
try:
    with open(info_path) as f:
        info = json.load(f)
except Exception:
    info = {}
info['format_blocked'] = True
info['sync_needed'] = False
info['failed_tests'] = [t for t in os.environ.get('FAILED', '').split(',') if t]
info['format_commits'] = [c for c in os.environ.get('COMMITS', '').split('|') if c]
info['upstream_sha'] = os.environ['SHA']
with open(info_path, 'w') as f:
    json.dump(info, f)
"
exit 1
