#!/bin/bash
set -e

echo "========================================"
echo " urnet-tools Help Text Test Suite"
echo "========================================"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/Provider_Install_Linux.sh"

TEMP_DIR=$(mktemp -d)
trap "rm -rf $TEMP_DIR" EXIT

FAILS=0

assert_contains() {
    local needle="$1" haystack="$2" msg="$3"
    if printf '%s' "$haystack" | grep -qF -- "$needle"; then
        echo "  ✅ PASS: $msg"
    else
        echo "  ❌ FAIL: $msg"
        echo "     Expected to contain: '$needle'"
        FAILS=$((FAILS + 1))
    fi
}

assert_not_contains() {
    local needle="$1" haystack="$2" msg="$3"
    if printf '%s' "$haystack" | grep -qF -- "$needle"; then
        echo "  ❌ FAIL: $msg"
        echo "     Should NOT contain: '$needle'"
        FAILS=$((FAILS + 1))
    else
        echo "  ✅ PASS: $msg"
    fi
}

SCRIPT="$REPO_ROOT/scripts/Provider_Install_Linux.sh"

# Extract library (everything before main case statement)
LIB="${TEMP_DIR}/lib.sh"
sed '/^case "$operation" in/,$d' "$SCRIPT" > "$LIB"
source "$LIB"

# ---------- HEADER ATTRIBUTION ----------
echo ""
echo "--- Test: Header attribution ---"
h=$(head -5 "$SCRIPT")
assert_contains "full-bars" "$h" "Header credits full-bars"
assert_contains "onlyinthe707" "$h" "Header credits onlyinthe707"

# ---------- SHOW_HELP ----------
echo ""
echo "--- Test: show_help ---"
help_out=$(show_help 2>&1)

# Previously hidden hub commands now visible
assert_contains "hub init" "$help_out" "Hub init listed in help"
assert_contains "hub link" "$help_out" "Hub link listed in help"
assert_contains "hub unlink" "$help_out" "Hub unlink listed in help"
assert_contains "hub update" "$help_out" "Hub update listed in help"

# Fixed log args (all|dump|-i, not a|d|-i)
assert_contains "logs [all|dump|-i]" "$help_out" "Logs shows correct all|dump|-i syntax"
assert_not_contains "logs [a|d|-i]" "$help_out" "Logs does not show hallucinated a|d|-i syntax"

# Hub link shows --token flag (not hallucinated --t)
assert_contains "--token" "$help_out" "Hub link shows --token flag"

# All sections present
assert_contains "Core Commands:" "$help_out" "Core Commands section"
assert_contains "Performance & Tuning:" "$help_out" "Performance & Tuning section"
assert_contains "Proxy Management:" "$help_out" "Proxy Management section"
assert_contains "Hub Management:" "$help_out" "Hub Management section"
assert_contains "Maintenance:" "$help_out" "Maintenance section"
assert_contains "Global Options:" "$help_out" "Global Options section"

# ---------- SET HELP ----------
echo ""
echo "--- Test: set help ---"
set_help=$(do_set help 2>&1)

assert_contains "node-name" "$set_help" "Set help lists node-name"
assert_contains "report-interval" "$set_help" "Set help lists report-interval"
assert_contains "proxy-url-max" "$set_help" "Set help lists proxy-url-max"
assert_contains "proxy-url-refresh" "$set_help" "Set help lists proxy-url-refresh"
assert_contains "cleanup-scope" "$set_help" "Set help lists cleanup-scope"
assert_contains "cleanup-interval" "$set_help" "Set help lists cleanup-interval"
assert_contains "fast-auth" "$set_help" "Set help lists fast-auth"

# ---------- RESULTS ----------
echo ""
echo "========================================"
if [ $FAILS -eq 0 ]; then
    echo "🎉 All tests passed!"
    exit 0
else
    echo "🚨 $FAILS test(s) failed."
    exit 1
fi
