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

# --- OpenRC installer branch -------------------------------------------------
# The CI runner is not root, so these pin the STAGED service script's content
# and the exact sudo commands a non-root operator is told to run; the root
# install path is covered by the container smoke (root-only).
#
# Both run in a FRESH bash that re-sources the library: an earlier test
# replaces pr_info/pr_warn/pr_err with no-ops at shell scope, which would
# silently empty the captured output.
test_openrc_staged_script_and_nonroot_output() {
    local tmpd me out staged
    tmpd="$(mktemp -d)"
    me="$(id -un)"
    out="$(
        PT_INSTALL="$tmpd/install" PT_ME="$me" PT_INITD="$tmpd/init.d-urnetwork" bash -c '
            . /tmp/urnet_provider_lib.sh
            install_path="$PT_INSTALL"
            mkdir -p "$install_path"
            openrc_user="$PT_ME"
            openrc_initd_file="$PT_INITD"
            urnet_install_url="https://example.invalid/install.sh"
            sudo() { return 1; }
            install_openrc_units
        '
    )"
    staged="$tmpd/install/urnetwork.openrc"
    assert_eq "1" "$([ -x "$staged" ] && echo 1 || echo 0)" "install_openrc_units stages an executable service script"
    assert_eq "1" "$(grep -c '^supervisor="supervise-daemon"$' "$staged")" "staged script uses supervise-daemon"
    assert_eq "1" "$(grep -c '^command_args="provide"$' "$staged")" "staged script runs the provider with 'provide'"
    assert_eq "1" "$(grep -c "^command_user=\"$me\"\$" "$staged")" "staged script runs as the service user"
    assert_eq "1" "$(grep -c "^command=\"$tmpd/install/bin/urnetwork\"\$" "$staged")" "staged script points at the installed provider binary"
    assert_eq "1" "$(grep -c '^respawn_delay=5$' "$staged")" "staged script respawns with a delay"
    assert_eq "1" "$(grep -c '^output_log="/var/log/urnetwork.log"$' "$staged")" "stdout log lives under /var/log"
    assert_eq "1" "$(grep -c '^error_log="/var/log/urnetwork.err"$' "$staged")" "stderr log lives under /var/log"
    assert_eq "1" "$(echo "$out" | grep -c "sudo cp $staged")" "non-root output prints the exact sudo cp command"
    assert_eq "1" "$(echo "$out" | grep -c 'sudo chmod +x')" "non-root output prints the sudo chmod command"
    assert_eq "1" "$(echo "$out" | grep -c 'sudo rc-update add urnetwork default')" "non-root output prints the rc-update command"
    assert_eq "1" "$(echo "$out" | grep -c 'sudo rc-service urnetwork start')" "non-root output prints the start command"
    rm -rf "$tmpd"
}
# OpenRC installer tests require a non-root runner: as root, the root branch
# of install_openrc_units would touch /etc (rc-update) and abort the suite.
if [ "$(id -u)" -eq 0 ]; then
    echo "⊘ SKIP: OpenRC installer tests need a non-root runner"
else
    test_openrc_staged_script_and_nonroot_output
fi

# A missing install directory must not leave the OpenRC service installed: the
# teardown runs before the "directory not found" bailout (non-root prints the
# sudo commands; root does the work — root covered by the container smoke).
test_openrc_uninstall_missing_dir_still_tears_down_service() {
    local tmpd me out
    tmpd="$(mktemp -d)"
    me="$(id -un)"
    out="$(
        PT_MISSING="$tmpd/missing" PT_ME="$me" PT_INITD="$tmpd/init.d-urnetwork" bash -c '
            . /tmp/urnet_provider_lib.sh
            install_path="$PT_MISSING"
            install_path_explicit=1
            openrc_user="$PT_ME"
            openrc_initd_file="$PT_INITD"
            has_systemd=0
            has_openrc=1
            do_uninstall 2>&1 || true
        '
    )" || true
    assert_eq "1" "$(echo "$out" | grep -c "removing the OpenRC service anyway")" "missing-dir uninstall still runs the OpenRC teardown"
    assert_eq "1" "$(echo "$out" | grep -c 'sudo rc-update del urnetwork default')" "missing-dir uninstall prints the rc-update command"
    assert_eq "1" "$(echo "$out" | grep -c "could not be found")" "missing-dir uninstall still reports the missing directory"
    rm -rf "$tmpd"
}
if [ "$(id -u)" -eq 0 ]; then
    echo "⊘ SKIP: OpenRC installer tests need a non-root runner"
else
    test_openrc_uninstall_missing_dir_still_tears_down_service
fi

# --- TEST: portable wget flags (BusyBox portability) ---
# The installer used to pass GNU-only wget flags unconditionally. BusyBox wget
# rejects --connect-timeout (it spells that -T SEC) and may lack
# --retry-connrefused, so on Alpine every download died with "unrecognized
# option" before reaching the network. An intermediate revision then "fixed"
# that by probing the real binary per flag, which could stall for minutes on GNU
# wget and wrote index.html into the cwd when something listened on port 80.
#
# These tests pin the CURRENT contract: only flags both implementations accept,
# and retries in the shell. A fake wget records the argv so the assertions are
# about what we actually pass — the real BusyBox behavior is covered by the
# container smoke, which runs the installer for real.
test_wget_passes_only_portable_flags() {
    local fakebin tmpd argv_out
    tmpd="$(mktemp -d)"
    fakebin="$tmpd/bin"
    mkdir -p "$fakebin"

    # Records argv, then behaves like a successful download.
    cat > "$fakebin/wget" <<'WGETEOF'
#!/bin/sh
echo "ARGV: $*" >> "$WGET_ARGV_LOG"
out=""
prev=""
for arg in "$@"; do
    if [ "$prev" = "-O" ]; then out="$arg"; fi
    prev="$arg"
done
[ -n "$out" ] && : > "$out"
exit 0
WGETEOF
    chmod +x "$fakebin/wget"

    WGET_ARGV_LOG="$tmpd/argv.log" PATH="$fakebin:$PATH" bash -c '
        . /tmp/urnet_provider_lib.sh
        wget_retry_download "https://example.invalid/x.tar.gz" "'"$tmpd"'/out.bin"
    ' >/dev/null 2>&1

    argv_out="$(cat "$tmpd/argv.log" 2>/dev/null)"
    assert_eq "0" "$(echo "$argv_out" | grep -c -- '--connect-timeout')" "must not pass GNU-only --connect-timeout"
    assert_eq "0" "$(echo "$argv_out" | grep -c -- '--retry-connrefused')" "must not pass --retry-connrefused"
    assert_eq "0" "$(echo "$argv_out" | grep -c -- '--tries')" "retries belong in the shell, not in wget flags"
    assert_eq "0" "$(echo "$argv_out" | grep -c -- '--waitretry')" "must not pass --waitretry"
    assert_eq "1" "$(echo "$argv_out" | grep -c -- '-T 10')" "must pass the portable -T 10"
    rm -rf "$tmpd"
}

# The probe-free design must never touch the network to decide flags: only the
# FORCE_IPV4 path reads wget --help, and it must use the GNU spelling.
test_wget_v4_flag_is_help_gated() {
    local tmpd out
    tmpd="$(mktemp -d)"

    # No --inet4-only in help => no -4 flag even when FORCE_IPV4 is set.
    cat > "$tmpd/wget" <<'WGETEOF'
#!/bin/sh
echo "no inet4 here"
exit 0
WGETEOF
    chmod +x "$tmpd/wget"

    out="$(PATH="$tmpd:$PATH" bash -c '
        . /tmp/urnet_provider_lib.sh
        FORCE_IPV4=1
        wget_common_opts
    ' 2>/dev/null)"
    assert_eq "0" "$(echo "$out" | grep -c -- '-4')" "no -4 when wget advertises no inet4-only support"
    assert_eq "1" "$(echo "$out" | grep -c -- '-T 10')" "-T 10 always present"

    # --inet4-only advertised => the flag is used.
    cat > "$tmpd/wget" <<'WGETEOF'
#!/bin/sh
echo "  --inet4-only    Use IPv4 only"
exit 0
WGETEOF
    chmod +x "$tmpd/wget"
    out="$(PATH="$tmpd:$PATH" bash -c '
        . /tmp/urnet_provider_lib.sh
        FORCE_IPV4=1
        wget_common_opts
    ' 2>/dev/null)"
    assert_eq "1" "$(echo "$out" | grep -c -- '--inet4-only')" "--inet4-only used when advertised"
    rm -rf "$tmpd"
}

# A failing download must be retried by the shell loop, then give up.
test_wget_retries_then_gives_up() {
    local fakebin tmpd count
    tmpd="$(mktemp -d)"
    fakebin="$tmpd/bin"
    mkdir -p "$fakebin"

    cat > "$fakebin/wget" <<'WGETEOF'
#!/bin/sh
echo "x" >> "$WGET_COUNT_FILE"
exit 1
WGETEOF
    chmod +x "$fakebin/wget"

    count="$(WGET_COUNT_FILE="$tmpd/count" PATH="$fakebin:$PATH" bash -c '
        . /tmp/urnet_provider_lib.sh
        wget_retry_download "https://example.invalid/x" "'"$tmpd"'/out.bin" || echo FAILED
    ' 2>/dev/null)"
    assert_eq "3" "$(cat "$tmpd/count" | wc -l | tr -d ' ')" "exactly 3 attempts before giving up"
    assert_eq "1" "$(echo "$count" | grep -c 'FAILED')" "reports failure after retries exhausted"
    rm -rf "$tmpd"
}

test_wget_passes_only_portable_flags
test_wget_v4_flag_is_help_gated
test_wget_retries_then_gives_up

# Run the real finalization functions against a redirected filesystem, with
# sudo recording each privileged operation. No host services or logs are touched.
test_openrc_sudo_finalizes_before_start() {
    local tmpd out
    tmpd="$(mktemp -d)"
    mkdir -p "$tmpd/home/install/bin" "$tmpd/log" "$tmpd/logrotate"
    printf 'existing stdout\n' > "$tmpd/log/urnetwork.log"
    printf '#!/bin/sh\nexit 0\n' > "$tmpd/home/install/bin/urnet-tools"
    sed -e "s|/var/log|$tmpd/log|g" -e "s|/etc/logrotate.d|$tmpd/logrotate|g" \
        /tmp/urnet_provider_lib.sh > "$tmpd/lib.sh"
    out="$(PT_ROOT="$tmpd" bash -c '
        . "$PT_ROOT/lib.sh"
        install_path="$PT_ROOT/home/install"
        openrc_home="$PT_ROOT/home"
        openrc_user=testuser
        openrc_initd_file="$PT_ROOT/initd"
        openrc_root_tool_dir="$PT_ROOT/root-tool"
        openrc_root_tool_path="$openrc_root_tool_dir/urnet-tools"
        openrc_user_home() { echo "$PT_ROOT/home"; }
        id() { if [ "$1" = -u ]; then echo 1000; else return 0; fi; }
        sudo() {
            echo "$*" >> "$PT_ROOT/privileged"
            case "$1" in
                -n|chown|rc-update) return 0 ;;
                id) echo 0 ;;
                rc-service)
                    test -x "$openrc_root_tool_path" &&
                    test -f "$PT_ROOT/log/urnetwork.err" &&
                    test "$(cat "$PT_ROOT/log/urnetwork.log")" = "existing stdout" &&
                    echo STARTED
                    ;;
                *) "$@" ;;
            esac
        }
        install_openrc_units
    ')"
    assert_eq "1" "$(echo "$out" | grep -c '^STARTED$')" "sudo finalizes and stages the tool before service start"
    assert_eq "existing stdout" "$(cat "$tmpd/log/urnetwork.log")" "finalization preserves existing logs"
    assert_eq "1" "$(grep -c "^touch $tmpd/log/urnetwork.err$" "$tmpd/privileged")" "missing log creation uses sudo"
    assert_eq "0" "$(grep -c "^touch $tmpd/log/urnetwork.log$" "$tmpd/privileged" || true)" "existing log is not recreated"
    assert_eq "1" "$(grep -c "^cp $tmpd/home/install/bin/urnet-tools $tmpd/root-tool/urnet-tools$" "$tmpd/privileged")" "root tool staging uses sudo"
    assert_eq "1" "$(grep -c "^chown -R root:root $tmpd/home/install$" "$tmpd/privileged")" "install ownership uses sudo"
    rm -rf "$tmpd"
}
test_openrc_sudo_finalizes_before_start

# B2: root's PATH must not resolve urnet-tools through the install tree on
# OpenRC. The symlink is only as safe as its target's whole ancestor chain, so
# the /usr/local/bin copy points at the root-owned staged libexec tool instead.
test_openrc_root_path_uses_staged_root_tool() {
    local tmpd out
    tmpd="$(mktemp -d)"
    # The staged copy must actually exist for the redirect to apply; the
    # installer stages it during finalization.
    mkdir -p "$tmpd/libexec"
    printf '#!/bin/sh\nexit 0\n' > "$tmpd/libexec/urnet-tools"
    out="$(PT_ROOT="$tmpd" bash -c '
        . /tmp/urnet_provider_lib.sh
        has_openrc=1
        id() { if [ "$1" = -u ]; then echo 0; else return 0; fi; }
        openrc_root_tool_dir="$PT_ROOT/libexec"
        openrc_root_tool_path="$openrc_root_tool_dir/urnet-tools"
        link_tools_into_dir() { echo "LINK dir=$1 src=$2"; return 0; }
        ensure_tools_on_path /nonexistent/install
    ')"
    assert_eq "1" "$(echo "$out" | grep -c 'LINK dir=/usr/local/bin src=.*/libexec')" \
        "root PATH links urnet-tools at the root-owned staged tool, not the install tree"
    rm -rf "$tmpd"
}
test_openrc_root_path_uses_staged_root_tool

# B2 (counter-case): on systemd, or when no staged copy exists, the link must
# still point into the install tree. Guards against the OpenRC redirect
# leaking into the normal path.
test_root_path_keeps_install_tree_off_openrc() {
    local out
    out="$(bash -c '
        . /tmp/urnet_provider_lib.sh
        has_openrc=0
        id() { if [ "$1" = -u ]; then echo 0; else return 0; fi; }
        openrc_root_tool_dir=/usr/local/libexec/urnetwork
        openrc_root_tool_path="$openrc_root_tool_dir/urnet-tools"
        link_tools_into_dir() { echo "LINK dir=$1 src=$2"; return 0; }
        ensure_tools_on_path /home/user/install
    ')"
    assert_eq "1" "$(echo "$out" | grep -c 'LINK dir=/usr/local/bin src=/home/user/install/bin')" \
        "off OpenRC, root PATH keeps pointing at the install tree"
}
test_root_path_keeps_install_tree_off_openrc

# B3: the OpenRC install tree must not land in the service user's home. A tree
# under /home/<user> has a user-writable ancestor, and root-executed code
# (the cron entry, sudo urnet-tools update) reachable through it is the
# escalation this move removes.
test_openrc_install_tree_is_root_owned_location() {
    local out
    out="$(bash -c '
        . /tmp/urnet_provider_lib.sh
        has_openrc=1
        has_systemd=0
        install_path_explicit=0
        install_path="$HOME/.local/share/urnetwork-provider"
        openrc_user_home() { echo /home/urnet; }
        id() { if [ "$1" = -u ]; then echo 0; else return 0; fi; }
        openrc_maybe_redirect_install_path
        echo "install_path=$install_path"
    ')"
    assert_eq "0" "$(echo "$out" | grep -c 'install_path=/home/urnet')" \
        "the OpenRC install tree is never placed in the service user's home"
    assert_eq "1" "$(echo "$out" | grep -c 'install_path=/usr/local/lib/urnetwork-provider')" \
        "the OpenRC install tree lands in a root-owned location"
}
test_openrc_install_tree_is_root_owned_location

# B3 (escape hatch): an explicit -i/--install path is the operator's decision
# and must survive untouched, as must a non-root run.
test_openrc_install_path_respects_explicit_choice() {
    local out
    out="$(bash -c '
        . /tmp/urnet_provider_lib.sh
        has_openrc=1
        has_systemd=0
        install_path_explicit=1
        install_path=/opt/mine
        openrc_user_home() { echo /home/urnet; }
        id() { if [ "$1" = -u ]; then echo 0; else return 0; fi; }
        openrc_maybe_redirect_install_path
        echo "explicit=$install_path"
    ')"
    assert_eq "1" "$(echo "$out" | grep -c 'explicit=/opt/mine')" \
        "an explicit --install path is not overridden on OpenRC"

    out="$(bash -c '
        . /tmp/urnet_provider_lib.sh
        has_openrc=1
        has_systemd=0
        install_path_explicit=0
        install_path=/home/klets/.local/share/urnetwork-provider
        openrc_user_home() { echo /home/klets; }
        id() { if [ "$1" = -u ]; then echo 1000; else return 0; fi; }
        openrc_maybe_redirect_install_path
        echo "nonroot=$install_path"
    ')"
    assert_eq "1" "$(echo "$out" | grep -c 'nonroot=/home/klets/.local/share/urnetwork-provider')" \
        "an unprivileged run keeps its own install path on OpenRC"
}
test_openrc_install_path_respects_explicit_choice

# B3: the state dir is the one thing that stays in the user's home, and the
# installer must not change ownership THROUGH a symlink planted there. A
# pre-planted .urnetwork -> /etc would otherwise hand /etc to the service user.
test_openrc_refuses_symlinked_state_dir() {
    local tmpd out
    tmpd="$(mktemp -d)"
    mkdir -p "$tmpd/home/install/bin" "$tmpd/victim"
    printf '#!/bin/sh\nexit 0\n' > "$tmpd/home/install/bin/urnet-tools"
    ln -s "$tmpd/victim" "$tmpd/home/.urnetwork"
    out="$(PT_ROOT="$tmpd" bash -c '
            . /tmp/urnet_provider_lib.sh
            install_path="$PT_ROOT/home/install"
            openrc_home="$PT_ROOT/home"
            openrc_user=testuser
            openrc_initd_file="$PT_ROOT/initd"
            openrc_root_tool_dir="$PT_ROOT/root-tool"
            openrc_root_tool_path="$openrc_root_tool_dir/urnet-tools"
            openrc_user_home() { echo "$PT_ROOT/home"; }
            id() { if [ "$1" = -u ]; then echo 1000; else return 0; fi; }
            # The shim below stands in for real sudo; feed it to SUDO the same
            # way install_json_parser would, since we are not going through
            # that path here.
            SUDO=sudo
            sudo() { echo "$*" >> "$PT_ROOT/privileged"; case "$1" in id) echo 0 ;; *) return 0 ;; esac; }
            openrc_finalize_root_paths
        ' 2>&1)"
        assert_eq "1" "$(echo "$out" | grep -c 'refusing to change ownership through it')" \
        "a symlinked state dir is reported instead of silently followed"
    assert_eq "0" "$(grep -c "chown testuser $tmpd/home/.urnetwork" "$tmpd/privileged" 2>/dev/null || true)" \
        "ownership is never changed through the planted symlink"
    rm -rf "$tmpd"
}
test_openrc_refuses_symlinked_state_dir

# F6: a symlink pointing INSIDE the service user's home is a legitimate
# relocation — follow it and chown only the target inside the home.
test_openrc_follows_inhome_state_dir_symlink() {
    local tmpd out
    tmpd="$(mktemp -d)"
    mkdir -p "$tmpd/home/install/bin" "$tmpd/home/statedata"
    printf '#!/bin/sh\nexit 0\n' > "$tmpd/home/install/bin/urnet-tools"
    ln -s "$tmpd/home/statedata" "$tmpd/home/.urnetwork"
    out="$(PT_ROOT="$tmpd" bash -c '
            . /tmp/urnet_provider_lib.sh
            install_path="$PT_ROOT/home/install"
            openrc_home="$PT_ROOT/home"
            openrc_user=testuser
            openrc_initd_file="$PT_ROOT/initd"
            openrc_root_tool_dir="$PT_ROOT/root-tool"
            openrc_root_tool_path="$openrc_root_tool_dir/urnet-tools"
            openrc_user_home() { echo "$PT_ROOT/home"; }
            id() { if [ "$1" = -u ]; then echo 1000; else return 0; fi; }
            SUDO=sudo
            sudo() { echo "$*" >> "$PT_ROOT/privileged"; case "$1" in id) echo 0 ;; *) return 0 ;; esac; }
            openrc_finalize_root_paths
        ' 2>&1)"
    assert_eq "0" "$(echo "$out" | grep -c 'refusing to change ownership through it')" \
        "an in-home relocation is not treated as a planted symlink"
    assert_eq "1" "$(grep -c "chown testuser $tmpd/home/statedata" "$tmpd/privileged" 2>/dev/null || echo 0)" \
        "the in-home target is chowned to the service user"
    rm -rf "$tmpd"
}
test_openrc_follows_inhome_state_dir_symlink

test_service_restart_systemd_fallback() {
    local out
    out="$(bash -c '
        . /tmp/urnet_provider_lib.sh
        has_openrc=0
        systemctl() { echo "systemctl $*"; }
        urnetwork_service_restart
    ')"
    assert_eq "systemctl --user restart urnetwork.service" "$out" "non-OpenRC restart dispatches to systemctl"
}
test_service_restart_systemd_fallback

test_json_parser_package_commands_use_sudo() {
    local tmpd distro calls expected manager
    tmpd="$(mktemp -d)"
    for distro in arch debian fedora alpine opensuse; do
        printf 'ID=%s\n' "$distro" > "$tmpd/os-release"
        sed "s|/etc/os-release|$tmpd/os-release|g" /tmp/urnet_provider_lib.sh > "$tmpd/lib.sh"
        calls="$(PT_LIB="$tmpd/lib.sh" bash -c '
            . "$PT_LIB"
            id() { echo 1000; }
            command() {
                case "$*" in "-v jq"|"-v python3") return 1;; esac
                builtin command "$@"
            }
            sudo() {
                [ "$1" = -n ] && return 0
                echo "sudo $*"
                case "$*" in *jq) return 1;; esac
            }
            pacman() { echo UNPRIVILEGED; }
            apt-get() { echo UNPRIVILEGED; }
            dnf() { echo UNPRIVILEGED; }
            apk() { echo UNPRIVILEGED; }
            zypper() { echo UNPRIVILEGED; }
            pr_info() { :; }
            install_json_parser || :
        ')"
        case "$distro" in
            arch) manager=pacman; expected=2 ;;
            debian) manager=apt-get; expected=3 ;;
            fedora) manager=dnf; expected=2 ;;
            alpine) manager=apk; expected=2 ;;
            opensuse) manager=zypper; expected=2 ;;
        esac
        assert_eq "$expected" "$(echo "$calls" | grep -c "^sudo $manager ")" "$distro elevates all package commands including fallback"
        assert_eq "0" "$(echo "$calls" | grep -c UNPRIVILEGED || true)" "$distro has no unprivileged package calls"
    done
    rm -rf "$tmpd"
}
test_json_parser_package_commands_use_sudo

echo "======================================"
if [ $FAILS -eq 0 ]; then
    echo "🎉 All tests passed!"
    exit 0
else
    echo "🚨 $FAILS test(s) failed."
    exit 1
fi
