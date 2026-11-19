#!/bin/bash
# Test for .github/scripts/commit_sync.sh with throwaway repositories. Run from the repo root.
set -euo pipefail
SCRIPT="$(pwd)/.github/scripts/commit_sync.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid
export GITHUB_REPOSITORY=example/repo CFAA_SYNC_INFO="$T/info.json"
fail() { echo "FAIL: $*"; exit 1; }

new_world() { # origin (bare), work clone with a sync commit on a local branch
  rm -rf "$T/origin.git" "$T/work"; git init -q --bare -b main "$T/origin.git"
  git clone -q "$T/origin.git" "$T/work" 2>/dev/null; cd "$T/work"
  echo base > table.go; echo keep > other.txt; git add -A; git commit -q -m base; git push -q origin main
  git checkout -q -b chore/cfaa-blocklist-sync; echo synced > table.go; git commit -q -am "chore(security): sync blocklists"
  echo '{"upstream_sha":"abc"}' > "$CFAA_SYNC_INFO"
}
main_file() { git -C "$T/origin.git" show main:"$1"; }

# 1. a normal run puts the change on main and records the commit URL
new_world; bash "$SCRIPT" >/dev/null
[ "$(main_file table.go)" = synced ] || fail "main does not have the synced table"
python3 - <<PY || fail "sync info"
import json; i=json.load(open("$CFAA_SYNC_INFO"))
assert i["direct_commit"] is True and i["pr_url"].startswith("https://github.com/example/repo/commit/") and i["upstream_sha"]=="abc", i
PY
echo "ok 1 normal run"

# 2. main moved while the job ran: the change still lands and the other commit stays
new_world; git clone -q "$T/origin.git" "$T/other" 2>/dev/null; (cd "$T/other"; echo moved > other.txt; git commit -q -am bot; git push -q origin main)
cd "$T/work"; bash "$SCRIPT" >/dev/null
[ "$(main_file table.go)" = synced ] && [ "$(main_file other.txt)" = moved ] || fail "main moved case"
echo "ok 2 main moved"

# 3. a conflict stops the job and leaves main untouched
new_world; git clone -q "$T/origin.git" "$T/other2" 2>/dev/null; (cd "$T/other2"; echo different > table.go; git commit -q -am "other change"; git push -q origin main)
cd "$T/work"; if bash "$SCRIPT" >/dev/null 2>&1; then fail "a conflict must fail"; fi
[ "$(main_file table.go)" = different ] || fail "a failed run changed main"
echo "ok 3 conflict fails loudly"

# 4. a rejected push stops the job
new_world; git -C "$T/origin.git" config receive.denyCurrentBranch updateInstead
cat > "$T/origin.git/hooks/pre-receive" <<'HOOK'
#!/bin/sh
echo "push refused" >&2; exit 1
HOOK
chmod +x "$T/origin.git/hooks/pre-receive"; cd "$T/work"
if bash "$SCRIPT" >/dev/null 2>&1; then fail "a refused push must fail"; fi
echo "ok 4 refused push fails loudly"
echo "ALL PASSED"
