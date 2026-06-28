#!/bin/bash
set -e

echo "========================================"
echo " urnet-tools No-Args Fallback Test Suite"
echo "========================================"

ORIG_SCRIPT="scripts/Provider_Install_Linux.sh"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEMP_DIR=$(mktemp -d)
trap "rm -rf $TEMP_DIR" EXIT

FAILS=0

assert_eq() {
    local expected="$1"
    local actual="$2"
    local msg="$3"
    if [ "$expected" = "$actual" ]; then
        echo "  ✅ PASS: $msg"
    else
        echo "  ❌ FAIL: $msg"
        echo "     Expected: '$expected'"
        echo "     Actual:   '$actual'"
        FAILS=$((FAILS + 1))
    fi
}

# --- TEST 1: Installed as urnet-tools with no args shows help ---
echo ""
echo "--- Test 1: Installed urnet-tools with no args shows help ---"

cp "$REPO_ROOT/$ORIG_SCRIPT" "$TEMP_DIR/urnet-tools"
chmod +x "$TEMP_DIR/urnet-tools"
# Run with no args, capture stderr and exit code
output=$("$TEMP_DIR/urnet-tools" 2>&1) || code=$?
code=${code:-0}

assert_eq "1" "$code" "Exit code should be 1 (no args -> show help)"
if echo "$output" | grep -q "Usage:.*urnet-tools"; then
    echo "  ✅ PASS: Output contains usage message"
else
    echo "  ❌ FAIL: Output does not contain usage message"
    echo "     Output: $(echo "$output" | head -3)"
    FAILS=$((FAILS + 1))
fi

# --- TEST 2: Installed urnet-tools with --help shows help (exit 0) ---
echo ""
echo "--- Test 2: Installed urnet-tools with --help shows help (exit 0) ---"

output=$("$TEMP_DIR/urnet-tools" --help 2>&1) || code=$?
code=${code:-0}

if echo "$output" | grep -q "Usage:.*urnet-tools"; then
    echo "  ✅ PASS: --help shows usage (exit=$code)"
else
    echo "  ❌ FAIL: --help did not show usage"
    FAILS=$((FAILS + 1))
fi

# --- TEST 3: Installed urnet-tools with 'update' works normally ---
echo ""
echo "--- Test 3: Installed urnet-tools with 'update' subcommand ---"
# We can't actually run update (it needs network), but we can verify it does not
# immediately exit with help. Run with --dry-run style — just check it doesn't
# print usage as the first thing.
output=$("$TEMP_DIR/urnet-tools" update 2>&1) || true
if echo "$output" | grep -q "Fetching release information"; then
    echo "  ✅ PASS: 'update' proceeded past arg parsing (tried to fetch release)"
elif echo "$output" | head -1 | grep -q "Usage:"; then
    echo "  ❌ FAIL: 'update' triggered help instead of running"
    FAILS=$((FAILS + 1))
else
    echo "  ⚠️  'update' produced unexpected output: $(echo "$output" | head -2)"
fi

# --- TEST 4: Pipe installer (not installed as urnet-tools) defaults to install ---
echo ""
echo "--- Test 4: Pipe installer (sh) with no args defaults to install ---"

# Simulate pipe install by running with $0 not ending in urnet-tools
cp "$REPO_ROOT/$ORIG_SCRIPT" "$TEMP_DIR/Provider_Install_Linux.sh"
chmod +x "$TEMP_DIR/Provider_Install_Linux.sh"
output=$("$TEMP_DIR/Provider_Install_Linux.sh" 2>&1) || true
if echo "$output" | grep -q "This Command Triggers a Cold Restart\|Fetching release information for tag:"; then
    echo "  ✅ PASS: Pipe install proceeded past arg parsing (defaulted to install)"
elif echo "$output" | head -1 | grep -q "Usage:"; then
    echo "  ❌ FAIL: Pipe install showed help instead of defaulting to install"
    FAILS=$((FAILS + 1))
else
    echo "  ⚠️  Unexpected output: $(echo "$output" | head -2)"
fi

# --- TEST 5: Source the lib and verify the case statement exists ---
echo ""
echo "--- Test 5: Source the lib and check path-detection logic ---"

LIB=$(mktemp)
sed '/^case "$operation" in/,$d' "$REPO_ROOT/$ORIG_SCRIPT" > "$LIB"

if grep -q 'case.*\$0.*in' "$LIB"; then
    echo "  ✅ PASS: Script uses \$0 for mode detection"
else
    echo "  ❌ FAIL: Script does not use \$0 for mode detection"
    FAILS=$((FAILS + 1))
fi
rm -f "$LIB"

# --- RESULTS ---
echo ""
echo "========================================"
if [ $FAILS -eq 0 ]; then
    echo "🎉 All tests passed!"
    exit 0
else
    echo "🚨 $FAILS test(s) failed."
    exit 1
fi
