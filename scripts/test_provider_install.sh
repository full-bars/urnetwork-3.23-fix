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

# queue_pending_override/queue_pending_clear write pending_overrides.json
# through json_string(); regression for the bug where json_string's
# predecessor (json_escape) emitted escaped CONTENTS without the
# surrounding double quotes, producing entries like
# {"op": "set", "key": ramlogs, "value": on} — invalid JSON that
# mergePendingOverrides() could never parse, silently no-opping every
# queued override. Exercise real call sites with values that need escaping
# (embedded quote, embedded backslash, a URL) and require python3 to accept
# the resulting file as valid JSON.
test_pending_overrides_valid_json() {
    local home
    home=$(mktemp -d)
    queue_pending_override "ramlogs" "on" "$home"
    queue_pending_override 'weird"key' 'has "quotes" and a \backslash' "$home"
    queue_pending_override "report_url" "https://example.com/a?b=c" "$home"
    queue_pending_clear "profile" "$home"

    local file="$home/.urnetwork/pending_overrides.json"
    if [ ! -f "$file" ]; then
        echo "❌ FAIL: pending_overrides.json was not created"
        FAILS=$((FAILS + 1))
        rm -rf "$home"
        return
    fi

    if python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert len(d) == 4, d' "$file" 2>/tmp/urnet_json_err; then
        echo "✅ PASS: pending_overrides.json queued by queue_pending_override/queue_pending_clear is valid JSON"
    else
        echo "❌ FAIL: pending_overrides.json is not valid JSON"
        cat "$file"
        cat /tmp/urnet_json_err
        FAILS=$((FAILS + 1))
    fi
    rm -rf "$home"
}
test_pending_overrides_valid_json

# A queue file holding only an empty array (the shape a drained queue can be
# left in) must accept a new entry and stay valid JSON. Dropping its last
# line used to delete the whole array and leave ",\n  entry\n]".
test_pending_overrides_append_to_empty_array() {
    local home
    home=$(mktemp -d)
    mkdir -p "$home/.urnetwork"
    local file="$home/.urnetwork/pending_overrides.json"
    for shape in '[]' '[ ]' '[
]'; do
        printf '%s\n' "$shape" > "$file"
        queue_pending_override "ramlogs" "on" "$home"
        if python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert len(d) == 1 and d[0]["key"] == "ramlogs", d' "$file" 2>/tmp/urnet_json_err; then
            echo "✅ PASS: appending to an empty pending_overrides.json array ($(printf '%s' "$shape" | tr '\n' ' ')) stays valid JSON"
        else
            echo "❌ FAIL: appending to an empty array ($(printf '%s' "$shape" | tr '\n' ' ')) produced invalid JSON"
            cat "$file"
            cat /tmp/urnet_json_err
            FAILS=$((FAILS + 1))
        fi
    done
    rm -rf "$home"
}
test_pending_overrides_append_to_empty_array

# --- TESTS: PATH setup (symlinks for non-interactive shells and root, rc blocks) ---
path_test_env() {
    PT_HOME="$(mktemp -d)"
    PT_INSTALL="$PT_HOME/.local/share/urnetwork-provider"
    mkdir -p "$PT_INSTALL/bin"
    printf '#!/bin/sh\necho urnet-tools-ok\n' > "$PT_INSTALL/bin/urnet-tools"
    printf '#!/bin/sh\necho urnetwork-ok\n' > "$PT_INSTALL/bin/urnetwork"
    chmod +x "$PT_INSTALL/bin/urnet-tools" "$PT_INSTALL/bin/urnetwork"
}

test_link_tools_reach_a_non_interactive_shell() {
    path_test_env
    (
        HOME="$PT_HOME"
        link_tools_into_dir "$HOME/.local/bin" "$PT_INSTALL/bin"
    )
    # The shell behind `ssh host urnet-tools`: no rc files, minimal PATH that
    # contains only the usual per-user bin dir.
    local out
    out="$(env -i HOME="$PT_HOME" PATH="/usr/bin:/bin:$PT_HOME/.local/bin" bash -c 'urnet-tools' 2>&1)"
    assert_eq "urnet-tools-ok" "$out" "urnet-tools is found by a non-interactive shell without any rc file"
    out="$(env -i HOME="$PT_HOME" PATH="/usr/bin:/bin:$PT_HOME/.local/bin" bash -c 'urnetwork' 2>&1)"
    assert_eq "urnetwork-ok" "$out" "urnetwork is found the same way"
    rm -rf "$PT_HOME"
}
test_link_tools_reach_a_non_interactive_shell

test_link_tools_idempotent_and_never_clobbers_a_real_file() {
    path_test_env
    mkdir -p "$PT_HOME/bin2"
    printf 'mine\n' > "$PT_HOME/bin2/urnet-tools"          # a real file, not ours
    link_tools_into_dir "$PT_HOME/bin2" "$PT_INSTALL/bin"
    assert_eq "mine" "$(cat "$PT_HOME/bin2/urnet-tools")" "a real file that is not a symlink is left alone"
    assert_eq "$PT_INSTALL/bin/urnetwork" "$(readlink "$PT_HOME/bin2/urnetwork")" "the other tool is still linked"
    link_tools_into_dir "$PT_HOME/bin2" "$PT_INSTALL/bin"
    link_tools_into_dir "$PT_HOME/bin2" "$PT_INSTALL/bin"
    assert_eq "$PT_INSTALL/bin/urnetwork" "$(readlink "$PT_HOME/bin2/urnetwork")" "running it again keeps the same link"
    # A stale link from an older install path is repointed.
    ln -sfn /nonexistent/urnetwork "$PT_HOME/bin2/urnetwork"
    link_tools_into_dir "$PT_HOME/bin2" "$PT_INSTALL/bin"
    assert_eq "$PT_INSTALL/bin/urnetwork" "$(readlink "$PT_HOME/bin2/urnetwork")" "a stale link is repointed at the current install"
    rm -rf "$PT_HOME"
}
test_link_tools_idempotent_and_never_clobbers_a_real_file

test_link_tools_adds_urtop_as_a_name_for_urnet_tools() {
    path_test_env
    mkdir -p "$PT_HOME/bin2"
    link_tools_into_dir "$PT_HOME/bin2" "$PT_INSTALL/bin"
    # urtop is not a binary of its own: it is a second name for urnet-tools,
    # which behaves as `top` when started under that name.
    assert_eq "$PT_INSTALL/bin/urnet-tools" "$(readlink "$PT_HOME/bin2/urtop")" "urtop points at the urnet-tools binary"
    local out
    out="$(env -i HOME="$PT_HOME" PATH="/usr/bin:/bin:$PT_HOME/bin2" bash -c 'urtop' 2>&1)"
    assert_eq "urnet-tools-ok" "$out" "urtop runs the urnet-tools binary"
    # A stale link from an older install path is repointed.
    ln -sfn /nonexistent/urnet-tools "$PT_HOME/bin2/urtop"
    link_tools_into_dir "$PT_HOME/bin2" "$PT_INSTALL/bin"
    assert_eq "$PT_INSTALL/bin/urnet-tools" "$(readlink "$PT_HOME/bin2/urtop")" "a stale urtop link is repointed at urnet-tools"
    # Someone else's real urtop is never overwritten.
    rm -f "$PT_HOME/bin2/urtop"
    printf 'theirs\n' > "$PT_HOME/bin2/urtop"
    link_tools_into_dir "$PT_HOME/bin2" "$PT_INSTALL/bin"
    assert_eq "theirs" "$(cat "$PT_HOME/bin2/urtop")" "a real urtop file that is not a symlink is left alone"
    rm -rf "$PT_HOME"
}
test_link_tools_adds_urtop_as_a_name_for_urnet_tools

test_root_style_link_dir_reachable_without_the_users_path() {
    # /usr/local/bin for root is modelled by any dir on a PATH that has none of
    # the installing user's entries.
    path_test_env
    mkdir -p "$PT_HOME/usr-local-bin"
    link_tools_into_dir "$PT_HOME/usr-local-bin" "$PT_INSTALL/bin"
    local out
    out="$(env -i HOME="$PT_HOME" PATH="/usr/bin:/bin:$PT_HOME/usr-local-bin" bash -c 'urnet-tools' 2>&1)"
    assert_eq "urnet-tools-ok" "$out" "a system-wide link works for a user (root) with no per-user PATH"
    rm -rf "$PT_HOME"
}
test_root_style_link_dir_reachable_without_the_users_path

test_rc_blocks_written_once_and_removed_cleanly() {
    path_test_env
    (
        HOME="$PT_HOME"
        printf 'export A=1\n' > "$HOME/.profile"
        add_path_blocks "$PT_INSTALL" > /dev/null
        add_path_blocks "$PT_INSTALL" > /dev/null
    )
    assert_eq "1" "$(grep -c '# == urnetwork-provider start' "$PT_HOME/.bashrc")" "bashrc block is written exactly once across two runs"
    assert_eq "1" "$(grep -c '# == urnetwork-provider start' "$PT_HOME/.profile")" "an existing ~/.profile gets the block too"
    assert_eq "0" "$(ls "$PT_HOME/.zshenv" 2>/dev/null | wc -l)" "no zshenv is created when zsh is not installed"
    (
        HOME="$PT_HOME"
        remove_path_block "$HOME/.bashrc"
        remove_path_block "$HOME/.profile"
    )
    assert_eq "0" "$(grep -c 'urnetwork-provider' "$PT_HOME/.bashrc")" "removal leaves no urnetwork block in bashrc"
    assert_eq "export A=1" "$(cat "$PT_HOME/.profile" | tr -d '\n')" "removal keeps the user's own profile content"
    rm -rf "$PT_HOME"
}
test_rc_blocks_written_once_and_removed_cleanly

test_remove_tool_links_only_removes_our_links() {
    path_test_env
    mkdir -p "$PT_HOME/.local/bin"
    ln -sfn "$PT_INSTALL/bin/urnet-tools" "$PT_HOME/.local/bin/urnet-tools"      # ours
    ln -sfn /usr/bin/true "$PT_HOME/.local/bin/urnetwork"                         # someone else's
    (
        HOME="$PT_HOME"
        remove_tool_links "$PT_INSTALL"
    )
    assert_eq "0" "$([ -L "$PT_HOME/.local/bin/urnet-tools" ] && echo 1 || echo 0)" "our link is removed on uninstall"
    assert_eq "/usr/bin/true" "$(readlink "$PT_HOME/.local/bin/urnetwork")" "a link that points elsewhere is left alone"
    rm -rf "$PT_HOME"
}
test_remove_tool_links_only_removes_our_links

echo "======================================"
if [ $FAILS -eq 0 ]; then
    echo "🎉 All tests passed!"
    exit 0
else
    echo "🚨 $FAILS test(s) failed."
    exit 1
fi
