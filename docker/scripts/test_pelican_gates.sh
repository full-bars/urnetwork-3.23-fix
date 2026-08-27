#!/bin/bash
# test_pelican_gates.sh — behavioral tests for the PELICAN=yes update-disable
# gates and state-dir resolution, plus static validation of the egg JSON.
#
# Unlike test_pelican_smoke.sh (which drives the full pelican_panel.sh boot
# flow against a fake provider binary), these tests extract and execute
# individual functions in isolation — they don't need the fake binary and
# run in well under a second.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="${PELICAN_SMOKE_WT:-$(cd "$HERE/../.." && pwd)}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0; fail=0
t() { local name="$1"; shift
    if "$@" >/dev/null 2>&1; then pass=$((pass+1)); echo "PASS: $name"; return 0
    else fail=$((fail+1)); echo "FAIL: $name"; return 1; fi
}

# ---------------------------------------------------------------------------
# 1) func_check_update PELICAN gate — behavioral, not grep
#    Extract the real function, stub log/curl/wget so behavior is observable,
#    then verify the gate actually short-circuits (or doesn't) by side effect.
# ---------------------------------------------------------------------------
extract_func() {
    # $1=file $2=funcname -> prints the function body between the def line
    # and its matching top-level closing brace
    awk -v fn="$2" '
        $0 ~ "^"fn"\\(\\) *\\{" { grab=1 }
        grab { print }
        grab && /^}/ { exit }
    ' "$1"
}

run_check_update_gated() {
    local pelican="$1" script="$tmp/func_check_update_test.sh"
    {
        echo "log() { echo \"LOG: \$*\" >> '$tmp/log.txt'; }"
        echo "curl()  { echo \"NETWORK-CALL curl \$*\" >> '$tmp/calls.txt'; return 1; }"
        echo "wget()  { echo \"NETWORK-CALL wget \$*\" >> '$tmp/calls.txt'; return 1; }"
        echo "VERSION_FILE='$tmp/nonexistent-version-file'"
        extract_func "$REPO/docker/scripts/start_nightly.sh" func_check_update
        echo 'func_check_update'
    } > "$script"
    : > "$tmp/log.txt"; : > "$tmp/calls.txt"
    if [ -n "$pelican" ]; then PELICAN="$pelican" bash "$script"; else unset PELICAN; bash "$script"; fi
}

rc=0; run_check_update_gated yes || rc=$?
t "func_check_update: PELICAN=yes exits 0"               test "$rc" -eq 0
t "func_check_update: PELICAN=yes logs disabled notice"  grep -qi "disabled under Pelican" "$tmp/log.txt"
t "func_check_update: PELICAN=yes makes no network call" sh -c "[ ! -s '$tmp/calls.txt' ]"

run_check_update_gated ""   # PELICAN unset — must NOT be gated
t "func_check_update: PELICAN unset skips the gate message" \
    sh -c "! grep -qi 'disabled under Pelican' '$tmp/log.txt'"
t "func_check_update: PELICAN unset proceeds past the gate (network attempted)" \
    sh -c "[ -s '$tmp/calls.txt' ]"

# ---------------------------------------------------------------------------
# 2) do_update PELICAN gate — behavioral, not grep
# ---------------------------------------------------------------------------
run_do_update_gated() {
    local pelican="$1" script="$tmp/do_update_test.sh"
    {
        echo "uname() { echo \"ARCH-CALL uname \$*\" >> '$tmp/calls2.txt'; command uname \"\$@\"; }"
        extract_func "$REPO/docker/scripts/urnet-tools.sh" do_update
        echo 'do_update'
    } > "$script"
    : > "$tmp/calls2.txt"
    if [ -n "$pelican" ]; then PELICAN="$pelican" bash "$script"; else unset PELICAN; bash "$script"; fi
}

OUT_GATED="$(run_do_update_gated yes 2>&1)"; rc=$?
t "do_update: PELICAN=yes exits 1"           test "$rc" -eq 1
t "do_update: PELICAN=yes prints refusal"    sh -c "printf '%s' '$OUT_GATED' | grep -qi 'runtime updates are disabled'"
t "do_update: PELICAN=yes never reaches arch detection" sh -c "[ ! -s '$tmp/calls2.txt' ]"

run_do_update_gated "" >/dev/null 2>&1 || true
t "do_update: PELICAN unset proceeds past the gate (arch detection attempted)" \
    sh -c "[ -s '$tmp/calls2.txt' ]"

# ---------------------------------------------------------------------------
# 3) State-dir resolution — representative scripts (proxy-health.sh,
#    proxy-traffic.sh). Uses `sh -x` tracing so only the path-assignment
#    line must execute; full script body may need runtime state we don't have.
# ---------------------------------------------------------------------------
test_state_dir_resolution() {
    local script="$1" varname="$2"
    local home="$tmp/home"; mkdir -p "$home"
    local trace="$tmp/trace.log"
    ( unset URNETWORK_PROXY_HEALTH_DIR
      HOME="$home" timeout 3 sh -x "$REPO/docker/scripts/$script" ) >"$trace" 2>&1
    grep -q "${varname}=${home}/.urnetwork" "$trace"
}

t "proxy-health.sh resolves state dir under \$HOME (no override)" \
    test_state_dir_resolution proxy-health.sh health_dir

test_state_dir_override() {
    local home="$tmp/home2"; mkdir -p "$home"
    local trace="$tmp/trace2.log"
    ( HOME="$home" URNETWORK_PROXY_HEALTH_DIR="/explicit/override" \
      timeout 3 sh -x "$REPO/docker/scripts/proxy-health.sh" ) >"$trace" 2>&1
    grep -q "health_dir=/explicit/override" "$trace"
}
t "proxy-health.sh honors explicit URNETWORK_PROXY_HEALTH_DIR override" \
    test_state_dir_override

t "proxy-traffic.sh resolves state dir under \$HOME (no override)" \
    test_state_dir_resolution proxy-traffic.sh health_dir

# ---------------------------------------------------------------------------
# 4) Egg JSON: rules-array validity + uniqueness invariants
# ---------------------------------------------------------------------------
egg="$REPO/pelican/egg-urnetwork-323fix.json"

t "egg: every variable has a non-empty rules array" \
    jq -e '[.variables[] | select((.rules|type)!="array" or (.rules|length)==0)] | length==0' "$egg" >/dev/null

t "egg: rules tokens use known validator prefixes" \
    jq -e '[.variables[].rules[] | select(test("^(required|nullable|string|in:.+)$") | not)] | length == 0' "$egg" >/dev/null

t "egg: env_variable names are unique" \
    jq -e '[.variables[].env_variable] | (length == (unique | length))' "$egg" >/dev/null

t "egg: sort values are unique" \
    jq -e '[.variables[].sort] | (length == (unique | length))' "$egg" >/dev/null

echo
echo "Results: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
