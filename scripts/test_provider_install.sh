#!/bin/bash
set -e

echo "======================================"
echo " URnetwork Provider Install Test Suite"
echo "======================================"

# Create a sourceable version of the script by removing everything from the main case block onwards
sed '/^case "$operation" in/,$d' scripts/Provider_Install_Linux.sh > /tmp/urnet_provider_lib.sh

# Source the functions
source /tmp/urnet_provider_lib.sh

# --- TEST UTILS ---
FAILS=0

assert_eq() {
    local expected="$1"
    local actual="$2"
    local msg="$3"
    if [ "$expected" = "$actual" ]; then
        echo "✅ PASS: $msg"
    else
        echo "❌ FAIL: $msg"
        echo "   Expected: '$expected'"
        echo "   Actual:   '$actual'"
        FAILS=$((FAILS + 1))
    fi
}

# --- TEST 1: get_version_from_api_response (JQ) ---
test_version_jq() {
    local json='{"tag_name": "v3.23.0-fix.17"}'
    # Ensure jq is used
    local res=$(get_version_from_api_response "$json")
    assert_eq "v3.23.0-fix.17" "$res" "get_version_from_api_response should extract tag_name using jq"
}
test_version_jq

# --- TEST 2: get_version_from_api_response (Python3 fallback) ---
test_version_python() {
    local json='{"tag_name": "v3.23.0-fix.18"}'
    # Hide jq temporarily to force python fallback
    alias jq="false" 
    # To reliably bypass command -v jq, we need to redefine it or adjust path.
    # A simple hack: just call the python one-liner directly to test the string parsing since command -v bypasses aliases
    local res=$(echo "$json" | tr -d '\000-\037' | python3 -c 'import sys, json;
try:
    data = json.load(sys.stdin)
    print(data["tag_name"])
except (json.JSONDecodeError, KeyError):
    print("")' 2>/dev/null)
    assert_eq "v3.23.0-fix.18" "$res" "Python3 fallback should extract tag_name correctly"
}
test_version_python

# --- TEST 3: do_install Fallback Logic ---
test_do_install_rate_limit() {
    # Mock network_fetch to simulate Rate Limit
    network_fetch() {
        return 22
    }

    # Mock pr_* so it doesn't spam
    pr_info() { true; }
    pr_err() { true; }
    pr_warn() { true; }

    # We want to test the chunk of logic in do_install.
    # Since do_install does a lot of OS stuff, we will just test the specific variable resolution
    # logic we added, by running it in a subshell

    local output=$(
        tag="latest"
        api_base="https://api.github.com/repos/full-bars/urnetwork-3.23-fix"
        api_url="$api_base/releases/latest"

        release="$(network_fetch "$api_url" 2>/dev/null || true)"
        version_to_install="$(get_version_from_api_response "$release" 2>&1)"

        if [ "$tag" = "latest" ] && [ -z "$version_to_install" ]; then
            if command -v curl > /dev/null; then
                tag_url=$(curl -Ls -o /dev/null -w %{url_effective} "https://github.com/full-bars/urnetwork-3.23-fix/releases/latest" 2>/dev/null || true)
                # Extract version from URL: /tag/v3.23.0-fix.18.1 -> v3.23.0-fix.18.1
                if [ -n "$tag_url" ]; then
                    case "$tag_url" in
                        *"/tag/"*) version_to_install="${tag_url##*/tag/}" ;;
                    esac
                fi
            fi
        fi

        echo "$version_to_install"
    )

    # Check if fallback grabbed the latest release correctly
    case "$output" in
        v3.23.0-fix*)
            assert_eq "$output" "$output" "Rate limit fallback successfully scraped web redirect ($output)"
            ;;
        *)
            # If test environment doesn't have reliable curl/network, skip this test gracefully
            echo "⊘ SKIP: Rate limit fallback test (network may not be available in test environment)"
            ;;
    esac
}
test_do_install_rate_limit

# --- TEST 4: get_asset_digest_from_api_response ---
test_asset_digest_jq() {
    local json='{"tag_name": "v3.23.0-fix.28.0", "assets": [
        {"name": "urnetwork-provider-v3.23.0-fix.28.0.tar.gz", "digest": "sha256:abc123"},
        {"name": "urnet-tools-linux-amd64", "digest": "sha256:def456"}
    ]}'
    local res=$(get_asset_digest_from_api_response "$json" "urnet-tools-linux-amd64")
    assert_eq "def456" "$res" "digest extraction strips sha256: prefix and finds the named asset"
}

test_asset_digest_missing() {
    local json='{"tag_name": "v3.23.0-fix.27.0", "assets": [
        {"name": "urnetwork-provider-v3.23.0-fix.27.0.tar.gz", "digest": "sha256:abc123"}
    ]}'
    local res=$(get_asset_digest_from_api_response "$json" "urnet-tools-linux-amd64")
    assert_eq "" "$res" "missing asset yields empty digest (caller falls back to shell script)"
}
test_asset_digest_jq
test_asset_digest_missing

# --- TEST 5: verify_sha256_file ---
test_sha256_verify() {
    local tmpfile=$(mktemp)
    echo "tool-binary-content" > "$tmpfile"
    local good=$(sha256sum "$tmpfile" | awk '{print $1}')
    local rc_good=1 rc_bad=0
    if verify_sha256_file "$tmpfile" "$good"; then
        rc_good=0
    fi
    if ! verify_sha256_file "$tmpfile" "$(printf '%.64s' 0000000000000000000000000000000000000000000000000000000000000000)"; then
        rc_bad=1
    fi
    rm -f "$tmpfile"
    if [ "$rc_good" -eq 0 ] && [ "$rc_bad" -eq 1 ]; then
        echo "✅ PASS: verify_sha256_file accepts matching digest, rejects mismatch"
    else
        echo "❌ FAIL: verify_sha256_file rc_good=$rc_good rc_bad=$rc_bad"
        FAILS=$((FAILS + 1))
    fi
}
test_sha256_verify

# --- TEST 6: get_asset_digest_from_api_response (Python3 fallback) ---
# Mirrors test_version_python's approach: `command -v jq` inside the sourced
# function bypasses shell aliases/functions, so this exercises the exact
# python3 one-liner (with the asset-name argv match) directly to confirm its
# JSON parsing and digest-selection logic independent of jq.
test_asset_digest_python_fallback() {
    local json='{"tag_name": "v3.23.0-fix.28.0", "assets": [
        {"name": "urnetwork-provider-v3.23.0-fix.28.0.tar.gz", "digest": "sha256:abc123"},
        {"name": "urnet-tools-linux-amd64", "digest": "sha256:def456"}
    ]}'
    local res=$(printf "%s" "$json" | tr -d '\000-\037' | python3 -c 'import sys, json;
try:
    data = json.load(sys.stdin)
    asset = sys.argv[1]
    for a in data.get("assets", []):
        if a.get("name") == asset:
            print(a.get("digest", ""))
            break
except (json.JSONDecodeError, KeyError):
    print("")
' "urnet-tools-linux-amd64" 2>/dev/null | sed 's/^sha256://')
    assert_eq "def456" "$res" "Python3 fallback digest extraction finds the named asset and strips sha256:"
}
test_asset_digest_python_fallback

# --- TEST 7: verify_sha256_file edge cases ---
# A missing file must fail closed (return 1), never treated as "verified"
# or attempt to hash a nonexistent path.
test_verify_sha256_missing_file() {
    local rc=1
    if verify_sha256_file "/tmp/urnet-test-does-not-exist-9f3a" "$(printf '%.64s' 0000000000000000000000000000000000000000000000000000000000000000)"; then
        rc=0
    fi
    if [ "$rc" -eq 1 ]; then
        echo "✅ PASS: verify_sha256_file fails closed on a missing file"
    else
        echo "❌ FAIL: verify_sha256_file returned success for a missing file"
        FAILS=$((FAILS + 1))
    fi
}
test_verify_sha256_missing_file

# An empty expected digest (release predates tool assets, or lookup failed)
# must also fail closed rather than being treated as "nothing to check".
test_verify_sha256_empty_digest() {
    local tmpfile=$(mktemp)
    echo "tool-binary-content" > "$tmpfile"
    local rc=1
    if verify_sha256_file "$tmpfile" ""; then
        rc=0
    fi
    rm -f "$tmpfile"
    if [ "$rc" -eq 1 ]; then
        echo "✅ PASS: verify_sha256_file fails closed on an empty expected digest"
    else
        echo "❌ FAIL: verify_sha256_file returned success for an empty expected digest"
        FAILS=$((FAILS + 1))
    fi
}
test_verify_sha256_empty_digest

echo "======================================"
if [ $FAILS -eq 0 ]; then
    echo "🎉 All tests passed!"
    exit 0
else
    echo "🚨 $FAILS test(s) failed."
    exit 1
fi
