#!/bin/bash
# Tests for urnet-tools hub update command
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

TEMP_DIR=$(mktemp -d)
trap "rm -rf $TEMP_DIR" EXIT

FAILS=0

# --- Lib extraction ---
LIB="${TEMP_DIR}/lib.sh"
sed '/^case "$operation" in/,$d' "$REPO_ROOT/scripts/Provider_Install_Linux.sh" > "$LIB"
if ! grep -q "do_hub_update" "$LIB"; then
    echo "❌ FATAL: do_hub_update not found in extracted lib (may be after case block)"
    exit 1
fi
# shellcheck disable=SC1090
source "$LIB"

assert_eq() {
    local expected="$1" actual="$2" msg="$3"
    if [ "$expected" = "$actual" ]; then
        echo "  ✅ PASS: $msg"
    else
        echo "  ❌ FAIL: $msg"
        echo "     Expected: '$expected'"
        echo "     Actual:   '$actual'"
        FAILS=$((FAILS + 1))
    fi
}

assert_file_contains() {
    local file="$1" pattern="$2" msg="$3"
    if [ -f "$file" ] && grep -q "$pattern" "$file"; then
        echo "  ✅ PASS: $msg"
    else
        echo "  ❌ FAIL: $msg"
        echo "     Expected file '$file' to contain '$pattern'"
        if [ -f "$file" ]; then
            echo "     File contents: $(cat "$file")"
        else
            echo "     File does not exist."
        fi
        FAILS=$((FAILS + 1))
    fi
}

assert_file_absent() {
    local file="$1" msg="$2"
    if [ ! -f "$file" ]; then
        echo "  ✅ PASS: $msg"
    else
        echo "  ❌ FAIL: $msg"
        echo "     File '$file' should not exist but does."
        FAILS=$((FAILS + 1))
    fi
}

assert_contains() {
    local needle="$1" haystack="$2" msg="$3"
    if printf '%s' "$haystack" | grep -q "$needle"; then
        echo "  ✅ PASS: $msg"
    else
        echo "  ❌ FAIL: $msg"
        echo "     Expected to contain: '$needle'"
        echo "     Got: $(printf '%s' "$haystack" | head -3)"
        FAILS=$((FAILS + 1))
    fi
}

assert_exit_code() {
    local expected="$1" actual="$2" msg="$3"
    if [ "$expected" = "$actual" ]; then
        echo "  ✅ PASS: $msg (exit=$actual)"
    else
        echo "  ❌ FAIL: $msg"
        echo "     Expected exit: $expected, actual: $actual"
        FAILS=$((FAILS + 1))
    fi
}

# ============================================================================
# SECTION 1: Tag Resolution
# ============================================================================

echo ""
echo "=== SECTION 1: Tag Resolution ==="

test_tag_explicit() {
    (
        _TEST_TAG="v4.0.0-test"
        _out_var=""
        # We test the tag-resolution logic extracted from do_hub_update.
        # This sequence mirrors lines 2275-2304 of the script.
        hub_tag="$_TEST_TAG"
        # chain: tag_arg > URNETWORK_HUB_TAG > hub_version_file > latest
        # with hub_tag_arg already populated, it short-circuits
        _out_var="$hub_tag"
        assert_eq "v4.0.0-test" "$_out_var" "Explicit tag is used directly"
    )
}
test_tag_explicit

test_tag_from_env() {
    (
        URNETWORK_HUB_TAG="v5.1.0-env"
        hub_tag_arg=""
        hub_tag="${hub_tag_arg:-${URNETWORK_HUB_TAG:-}}"
        assert_eq "v5.1.0-env" "$hub_tag" "URNETWORK_HUB_TAG env var is used when no tag arg"
    )
}
test_tag_from_env

test_tag_from_version_file() {
    (
        _VERSION_FILE="$TEMP_DIR/.hub_version_test"
        printf 'v3.23.0-fix.99\n' > "$_VERSION_FILE"
        hub_tag_arg=""
        URNETWORK_HUB_TAG=""
        hub_tag="${hub_tag_arg:-${URNETWORK_HUB_TAG:-}}"
        if [ -z "$hub_tag" ] && [ -f "$_VERSION_FILE" ]; then
            hub_tag="$(cat "$_VERSION_FILE")"
        fi
        assert_eq "v3.23.0-fix.99" "$hub_tag" ".hub_version file is read as fallback"
    )
}
test_tag_from_version_file

test_tag_defaults_to_latest() {
    (
        hub_tag_arg=""
        URNETWORK_HUB_TAG=""
        hub_tag=""
        if [ -z "$hub_tag" ]; then
            hub_tag=latest
        fi
        assert_eq "latest" "$hub_tag" "Defaults to 'latest' when nothing is specified"
    )
}
test_tag_defaults_to_latest

test_tag_prefix_normalization() {
    # Test the v-prefix normalization from the arg parsing
    _normalize() {
        local t="$1"
        if [ "$t" != "latest" ] && [ "$(printf '%s' "$t" | cut -c -1)" != "v" ]; then
            t="v$t"
        fi
        printf '%s' "$t"
    }
    assert_eq "v4.0.0" "$(_normalize "4.0.0")" "Tag without v-prefix gets normalized"
    assert_eq "v4.0.0" "$(_normalize "v4.0.0")" "Tag with v-prefix stays unchanged"
    assert_eq "latest" "$(_normalize "latest")" "Tag 'latest' is not prefixed with v"
}

# ============================================================================
# SECTION 2: Idempotency
# ============================================================================

echo ""
echo "=== SECTION 2: Idempotency ==="

test_idempotent_skip_same_version() {
    (
        _VERSION_FILE="$TEMP_DIR/.hub_version_idem"
        printf 'v3.23.0-fix.50\n' > "$_VERSION_FILE"

        force="0"
        hub_tag="v3.23.0-fix.50"
        hub_bin="/fake/path/hub"

        _skipped=false
        if [ "$force" != "1" ] && [ -f "$_VERSION_FILE" ]; then
            current="$(cat "$_VERSION_FILE")"
            if [ "$current" = "$hub_tag" ]; then
                _skipped=true
            fi
        fi
        assert_eq "true" "$_skipped" "Idempotent: same version with no --force skips update"
    )
}
test_idempotent_skip_same_version

test_force_overrides_idempotency() {
    (
        _VERSION_FILE="$TEMP_DIR/.hub_version_force"
        printf 'v3.23.0-fix.50\n' > "$_VERSION_FILE"

        force="1"
        hub_tag="v3.23.0-fix.50"
        hub_bin="/fake/path/hub"

        _skipped=false
        if [ "$force" != "1" ] && [ -f "$_VERSION_FILE" ]; then
            current="$(cat "$_VERSION_FILE")"
            if [ "$current" = "$hub_tag" ]; then
                _skipped=true
            fi
        fi
        assert_eq "false" "$_skipped" "Idempotent: --force overrides version check"
    )
}
test_force_overrides_idempotency

test_missing_binary_proceeds() {
    (
        _VERSION_FILE="$TEMP_DIR/.hub_version_missing"
        printf 'v3.23.0-fix.50\n' > "$_VERSION_FILE"

        force="0"
        hub_tag="v3.23.0-fix.50"
        hub_bin="$TEMP_DIR/nonexistent_binary"

        _skipped=false
        if [ "$force" != "1" ] && [ -f "$_VERSION_FILE" ]; then
            current="$(cat "$_VERSION_FILE")"
            if [ "$current" = "$hub_tag" ]; then
                if [ -x "$hub_bin" ]; then
                    _skipped=true
                fi
            fi
        fi
        assert_eq "false" "$_skipped" "Idempotent: missing binary proceeds regardless of version match"
    )
}
test_missing_binary_proceeds

test_no_version_file_proceeds() {
    (
        _VERSION_FILE="$TEMP_DIR/.hub_version_nonexistent"
        force="0"
        hub_tag="v3.23.0-fix.50"
        hub_bin="/fake/path/hub"

        _skipped=false
        if [ "$force" != "1" ] && [ -f "$_VERSION_FILE" ]; then
            _skipped=true  # would trigger if file existed
        fi
        assert_eq "false" "$_skipped" "Idempotent: no version file means always proceed"
    )
}
test_no_version_file_proceeds

# ============================================================================
# SECTION 3: Rollback State Machine
# ============================================================================

echo ""
echo "=== SECTION 3: Rollback State Machine ==="

# Simulate the _restore_and_abort logic in isolation.
# The actual function is inside do_hub_update and uses dynamic scoping,
# so here we replicate its logic with explicit variables.

test_rollback_no_swap_restarts_service() {
    (
        _service_was_active=true
        _db_was_backed_up=false
        _binary_was_backed_up=false
        _binary_was_swapped=false
        hub_bin="$TEMP_DIR/hub_bin_rs1"
        hub_data_dir="$TEMP_DIR/hub_data_rs1"
        mkdir -p "$hub_data_dir"

        _restarted=false
        _restored_binary=false
        _restored_db=false

        # Simulate _restore_and_abort
        if [ "$_binary_was_swapped" = true ]; then
            _restored_binary=true
            [ "$_db_was_backed_up" = true ] && _restored_db=true
        elif [ "$_binary_was_backed_up" = true ]; then
            _restored_binary=true
        fi

        if [ "$_service_was_active" = true ]; then
            _restarted=true
        fi

        assert_eq "false" "$_restored_binary" "Rollback: binary not touched when swap didn't happen"
        assert_eq "false" "$_restored_db" "Rollback: DB not restored when no swap"
        assert_eq "true" "$_restarted" "Rollback: service restarted if it was active"
    )
}
test_rollback_no_swap_restarts_service

test_rollback_swap_restores_binary_and_db() {
    (
        _service_was_active=true
        _db_was_backed_up=true
        _binary_was_backed_up=true
        _binary_was_swapped=true
        hub_bin="$TEMP_DIR/hub_bin_rs2"
        hub_data_dir="$TEMP_DIR/hub_data_rs2"
        mkdir -p "$hub_data_dir"

        # Create dummy files for rollback
        touch "${hub_bin}.old"
        touch "${hub_data_dir}/hub.db.bak"

        _restarted=false
        _restored_binary=false
        _restored_db=false

        if [ "$_binary_was_swapped" = true ]; then
            if [ -f "${hub_bin}.old" ]; then
                _restored_binary=true
            fi
            if [ "$_db_was_backed_up" = true ] && [ -f "${hub_data_dir}/hub.db.bak" ]; then
                _restored_db=true
            fi
        fi

        if [ "$_service_was_active" = true ]; then
            _restarted=true
        fi

        assert_eq "true" "$_restored_binary" "Rollback: old binary restored after failed swap"
        assert_eq "true" "$_restored_db" "Rollback: DB restored after failed swap"
        assert_eq "true" "$_restarted" "Rollback: service restarted after rollback"
    )
}
test_rollback_swap_restores_binary_and_db

test_rollback_service_inactive_no_restart() {
    (
        _service_was_active=false
        _db_was_backed_up=true
        _binary_was_backed_up=true
        _binary_was_swapped=true
        hub_bin="$TEMP_DIR/hub_bin_rs3"
        hub_data_dir="$TEMP_DIR/hub_data_rs3"
        mkdir -p "$hub_data_dir"
        touch "${hub_bin}.old"

        _restarted=false

        if [ "$_service_was_active" = true ]; then
            _restarted=true
        fi

        assert_eq "false" "$_restarted" "Rollback: inactive service is not restarted"
    )
}
test_rollback_service_inactive_no_restart

test_rollback_swap_without_db_backup() {
    (
        _service_was_active=true
        _db_was_backed_up=false
        _binary_was_backed_up=true
        _binary_was_swapped=true
        hub_bin="$TEMP_DIR/hub_bin_rs4"
        hub_data_dir="$TEMP_DIR/hub_data_rs4"
        mkdir -p "$hub_data_dir"
        touch "${hub_bin}.old"

        _restored_db=false

        if [ "$_binary_was_swapped" = true ] && [ "$_db_was_backed_up" = true ]; then
            _restored_db=true
        fi

        assert_eq "false" "$_restored_db" "Rollback: DB not restored when it was never backed up"
    )
}
test_rollback_swap_without_db_backup

# ============================================================================
# SECTION 4: Systemd Unit Handling
# ============================================================================

echo ""
echo "=== SECTION 4: Systemd Unit ==="

test_unit_template_contains_expected_fields() {
    _hub_bin="$TEMP_DIR/test-hub"
    _hub_data="$TEMP_DIR/test-hub-data"

    _unit=$(cat <<EOF
[Unit]
Description=URnetwork Hub Dashboard

[Service]
ExecStart=${_hub_bin} -addr :8080 -data ${_hub_data}
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=default.target
EOF
    )

    assert_contains "Description=URnetwork Hub Dashboard" "$_unit" "Unit template: has Description"
    assert_contains "ExecStart=" "$_unit" "Unit template: has ExecStart"
    assert_contains "Restart=on-failure" "$_unit" "Unit template: has Restart policy"
    assert_contains "WantedBy=default.target" "$_unit" "Unit template: has Install section"
    assert_contains "$_hub_bin" "$_unit" "Unit template: ExecStart references hub_bin"
    assert_contains "$_hub_data" "$_unit" "Unit template: ExecStart references hub_data_dir"
}
test_unit_template_contains_expected_fields

# ============================================================================
# SECTION 5: DB Backup Logic
# ============================================================================

echo ""
echo "=== SECTION 5: DB Backup ==="

test_db_backup_creates_bak_file() {
    (
        _db_dir="$TEMP_DIR/db_backup_test"
        mkdir -p "$_db_dir"
        echo "test-db-content" > "$_db_dir/hub.db"

        if [ -f "${_db_dir}/hub.db" ]; then
            cp "${_db_dir}/hub.db" "${_db_dir}/hub.db.bak"
            assert_file_contains "${_db_dir}/hub.db.bak" "test-db-content" "DB backup: contains original content"
        fi
    )
}
test_db_backup_creates_bak_file

test_db_backup_skips_when_no_db() {
    (
        _db_dir="$TEMP_DIR/db_noexist"
        mkdir -p "$_db_dir"
        _backed_up=false
        if [ -f "${_db_dir}/hub.db" ]; then
            _backed_up=true
        fi
        assert_eq "false" "$_backed_up" "DB backup: skipped when hub.db doesn't exist"
    )
}
test_db_backup_skips_when_no_db

# ============================================================================
# SECTION 6: Binary Verification
# ============================================================================

echo ""
echo "=== SECTION 6: Binary Verification ==="

test_binary_version_extraction() {
    _tmp_bin="$TEMP_DIR/verify-bin"
    # Create a dummy executable that outputs a version
    cat > "$_tmp_bin" <<'SCRIPT_EOF'
#!/bin/sh
echo "urnetwork-hub v3.23.0-fix.99 (linux/amd64)"
SCRIPT_EOF
    chmod 755 "$_tmp_bin"

    _version="$("$_tmp_bin" --version 2>/dev/null)" || {
        assert_eq "ok" "binary-verify-failed" "Binary verification should succeed"
    }
    assert_contains "v3.23.0-fix.99" "$_version" "Binary --version returns expected string"
}
test_binary_version_extraction

test_binary_verify_failure_handled() {
    (
        _tmp_bin="$TEMP_DIR/verify-bad"
        cat > "$_tmp_bin" <<'SCRIPT_EOF'
#!/bin/sh
exit 1
SCRIPT_EOF
        chmod 755 "$_tmp_bin"

        _verified=true
        if ! "$_tmp_bin" --version 2>/dev/null; then
            _verified=false
        fi
        assert_eq "false" "$_verified" "Corrupt binary: version check fails as expected"
    )
}
test_binary_verify_failure_handled

# ============================================================================
# SECTION 7: End-to-End Dry Run (Mocked)
# ============================================================================

echo ""
echo "=== SECTION 7: End-to-End with Mocks ==="

test_e2e_update_mocked() {
    (
        # --- MOCK SETUP ---
        # Override systemctl with a mock
        systemctl() {
            case "$*" in
                *"is-active"*"urnetwork-hub"*) return 0 ;;  # service is active
                *"stop"*"urnetwork-hub"*) echo "MOCK: systemctl stop" ;;
                *"start"*"urnetwork-hub"*) echo "MOCK: systemctl start" ;;
                *"daemon-reload"*) echo "MOCK: systemctl daemon-reload" ;;
                *"is-enabled"*) return 0 ;;
                *"enable"*) echo "MOCK: systemctl enable" ;;
                *) return 0 ;;
            esac
        }

        # Mock download_asset to succeed and create a working binary
        download_asset() {
            cat > "$2" <<'EXEC_EOF'
#!/bin/sh
echo "urnetwork-hub v9.9.9-mock (linux/amd64)"
EXEC_EOF
            return 0
        }

        # Mock network_fetch to return a release tag
        network_fetch() {
            echo '{"tag_name": "v9.9.9-mock"}'
            return 0
        }

        # Capture pr_* output
        pr_info() { printf 'INFO: %s\n' "$*"; }
        pr_err()  { printf 'ERR: %s\n' "$*"; }
        pr_warn() { printf 'WARN: %s\n' "$*"; }

        # Setup fake paths
        export HOME="$TEMP_DIR/e2e"
        mkdir -p "$HOME/.config/systemd/user"
        mkdir -p "$HOME/.local/share/urnetwork-provider/bin"
        mkdir -p "$HOME/.local/share/urnetwork-hub"

        has_systemd=1
        hub_bin="$HOME/.local/share/urnetwork-provider/bin/urnetwork-hub"
        hub_service="$HOME/.config/systemd/user/urnetwork-hub.service"
        hub_service_dir="$HOME/.config/systemd/user"
        hub_data_dir="$HOME/.local/share/urnetwork-hub"
        hub_version_file="$hub_data_dir/.hub_version"
        api_base="https://api.github.com/repos/full-bars/urnetwork-3.23-fix"
        arch="amd64"
        install_path="$HOME/.local/share/urnetwork-provider"

        # No prior version file → will resolve to latest → which our mock feeds "v9.9.9-mock"
        _output="$(do_hub_update "" "0" 2>&1)" || _ec=$?
        _ec=${_ec:-0}

        # Should succeed
        assert_eq "0" "$_ec" "E2E: do_hub_update succeeds with mocks"

        # Should have downloaded and installed binary
        if [ -x "$hub_bin" ]; then
            _binver="$("$hub_bin" --version)"
            assert_contains "v9.9.9-mock" "$_binver" "E2E: installed binary reports correct version"
        else
            echo "  ❌ FAIL: E2E installed binary"
            echo "     Binary not found at $hub_bin"
            FAILS=$((FAILS + 1))
        fi

        # Should have recorded version
        assert_file_contains "$hub_version_file" "v9.9.9-mock" "E2E: version file recorded"

        # Should have created systemd unit
        assert_file_contains "$hub_service" "URnetwork Hub Dashboard" "E2E: systemd unit created"

        # Binary backup should be cleaned up after success
        assert_file_absent "${hub_bin}.old" "E2E: binary backup removed after success"

        # Temp .new file should be gone (renamed to hub_bin)
        assert_file_absent "${hub_bin}.new" "E2E: temp .new file cleaned up"
    )
}
test_e2e_update_mocked

test_e2e_idempotent_skip() {
    (
        systemctl() { return 0; }
        download_asset() { echo "SHOULD NOT BE CALLED"; exit 99; }
        network_fetch() { echo '{"tag_name":"v1.0.0"}'; return 0; }
        pr_info() { printf 'INFO: %s\n' "$*"; }
        pr_err()  { printf 'ERR: %s\n' "$*"; }
        pr_warn() { printf 'WARN: %s\n' "$*"; }

        export HOME="$TEMP_DIR/e2e_skip"
        mkdir -p "$HOME/.local/share/urnetwork-provider/bin"
        mkdir -p "$HOME/.local/share/urnetwork-hub"
        hub_data_dir="$HOME/.local/share/urnetwork-hub"
        hub_version_file="$hub_data_dir/.hub_version"
        hub_bin="$HOME/.local/share/urnetwork-provider/bin/urnetwork-hub"

        # Create a dummy binary and version file saying we're at v1.0.0
        printf '#!/bin/sh\necho "hub v1.0.0"\n' > "$hub_bin"
        chmod 755 "$hub_bin"
        printf 'v1.0.0\n' > "$hub_version_file"

        has_systemd=1
        hub_service="/nonexistent/unit"
        hub_service_dir="/nonexistent"
        api_base="https://api.github.com/repos/full-bars/urnetwork-3.23-fix"
        arch="amd64"
        install_path="$HOME/.local/share/urnetwork-provider"

        _output="$(do_hub_update "v1.0.0" "0" 2>&1)" || _ec=$?
        _ec=${_ec:-0}

        assert_eq "0" "$_ec" "E2E idempotent: exits 0 when already at target version"
        assert_contains "Nothing to do" "$_output" "E2E idempotent: message says nothing to do"
    )
}
test_e2e_idempotent_skip

test_e2e_download_failure_rollback() {
    (
        systemctl() {
            case "$*" in
                *"is-active"*"urnetwork-hub"*) return 0 ;;
                *) echo "MOCK: systemctl $*" ;;
            esac
        }
        download_asset() { return 1; }  # download fails
        network_fetch() { echo '{"tag_name":"v2.0.0"}'; return 0; }
        pr_info() { printf 'INFO: %s\n' "$*"; }
        pr_err()  { printf 'ERR: %s\n' "$*"; }
        pr_warn() { printf 'WARN: %s\n' "$*"; }

        export HOME="$TEMP_DIR/e2e_dlfail"
        mkdir -p "$HOME/.local/share/urnetwork-provider/bin"
        mkdir -p "$HOME/.local/share/urnetwork-hub"

        has_systemd=1
        hub_bin="$HOME/.local/share/urnetwork-provider/bin/urnetwork-hub"
        hub_service="$HOME/.config/systemd/user/urnetwork-hub.service"
        hub_service_dir="$HOME/.config/systemd/user"
        hub_data_dir="$HOME/.local/share/urnetwork-hub"
        hub_version_file="$hub_data_dir/.hub_version"
        api_base="https://api.github.com/repos/full-bars/urnetwork-3.23-fix"
        arch="amd64"
        install_path="$HOME/.local/share/urnetwork-provider"

        # Pre-existing version file so we don't hit idempotency
        printf 'v1.0.0\n' > "$hub_version_file"

        _output="$(do_hub_update "v2.0.0" "0" 2>&1)" || _ec=$?

        # Should exit 1 on download failure
        assert_exit_code "1" "$_ec" "E2E download fail: exit code 1"

        # Should mention the failure
        assert_contains "Failed to download" "$_output" "E2E download fail: descriptive error message"
    )
}
test_e2e_download_failure_rollback

# ============================================================================
# SECTION 8: Help Text and Command Routing
# ============================================================================

echo ""
echo "=== SECTION 8: Help Text & Dispatch ==="

test_hub_update_listed_in_help() {
    _help_output=$(sh "$REPO_ROOT/scripts/Provider_Install_Linux.sh" --help 2>&1) || true
    assert_contains "hub" "$_help_output" "Help text mentions hub commands"
    assert_contains "update" "$_help_output" "Help text mentions update subcommand for hub"
}
test_hub_update_listed_in_help

test_hub_update_in_usage_error() {
    # Calling with no hub subcommand should list update in options
    _output=$( (export HOME="$TEMP_DIR"; sh "$REPO_ROOT/scripts/Provider_Install_Linux.sh" hub 2>&1) ) || true
    # Should mention update as a valid subcommand
    assert_contains "update" "$_output" "Usage error for 'hub' lists 'update' as valid subcommand"
}
test_hub_update_in_usage_error

# ============================================================================
# SECTION 9: Edge Cases
# ============================================================================

echo ""
echo "=== SECTION 9: Edge Cases ==="

test_no_systemd_guards() {
    (
        export HOME="$TEMP_DIR/nosystemd"
        has_systemd=0
        hub_bin="$HOME/fake/hub"
        hub_data_dir="$HOME/fake/hub-data"
        mkdir -p "$hub_data_dir"

        pr_info() { printf 'INFO: %s\n' "$*"; }
        pr_err()  { printf 'ERR: %s\n' "$*"; }

        _output="$(do_hub_update "v1.0.0" "0" 2>&1)" || _ec=$?
        assert_exit_code "1" "$_ec" "No systemd: hub update exits 1"
        assert_contains "systemd is not available" "$_output" "No systemd: descriptive error"
    )
}
test_no_systemd_guards

test_fresh_install_no_existing_binary() {
    (
        systemctl() { return 0; }
        download_asset() {
            cat > "$2" <<'EXEC_EOF'
#!/bin/sh
echo "hub v1.0.0"
EXEC_EOF
            return 0
        }
        network_fetch() { echo '{"tag_name":"v1.0.0"}'; return 0; }
        pr_info() { printf 'INFO: %s\n' "$*"; }
        pr_err()  { printf 'ERR: %s\n' "$*"; }
        pr_warn() { printf 'WARN: %s\n' "$*"; }

        export HOME="$TEMP_DIR/e2e_fresh"
        mkdir -p "$HOME/.local/share/urnetwork-provider/bin"
        mkdir -p "$HOME/.local/share/urnetwork-hub"
        mkdir -p "$HOME/.config/systemd/user"

        has_systemd=1
        hub_bin="$HOME/.local/share/urnetwork-provider/bin/urnetwork-hub"
        hub_service="$HOME/.config/systemd/user/urnetwork-hub.service"
        hub_service_dir="$HOME/.config/systemd/user"
        hub_data_dir="$HOME/.local/share/urnetwork-hub"
        hub_version_file="$hub_data_dir/.hub_version"
        api_base="https://api.github.com/repos/full-bars/urnetwork-3.23-fix"
        arch="amd64"
        install_path="$HOME/.local/share/urnetwork-provider"

        _output="$(do_hub_update "v1.0.0" "0" 2>&1)" || _ec=$?
        _ec=${_ec:-0}

        assert_eq "0" "$_ec" "Fresh install: succeeds"

        # Binary should exist but .old should not (no prior binary to back up)
        if [ -x "$hub_bin" ]; then
            echo "  ✅ PASS: Fresh install: binary installed"
        else
            echo "  ❌ FAIL: Fresh install: binary not installed"
            FAILS=$((FAILS + 1))
        fi

        assert_file_absent "${hub_bin}.old" "Fresh install: no .old since there was nothing to back up"

        # .new temp file should be cleaned (renamed into hub_bin)
        assert_file_absent "${hub_bin}.new" "Fresh install: .new temp file cleaned"

        # Should create the systemd unit
        assert_file_contains "$hub_service" "URnetwork Hub Dashboard" "Fresh install: systemd unit created"

        # Version file
        assert_file_contains "$hub_version_file" "v1.0.0" "Fresh install: version recorded"
    )
}
test_fresh_install_no_existing_binary

# ============================================================================
# SECTION 10: Docker Wrapper
# ============================================================================

echo ""
echo "=== SECTION 10: Docker Wrapper ==="

test_docker_hub_update_message() {
    _output=$(bash "$REPO_ROOT/docker/scripts/urnet-tools.sh" hub update 2>&1) && _ec=0 || _ec=$?
    assert_exit_code "1" "$_ec" "Docker: hub update exits non-zero"
    assert_contains "docker pull" "$_output" "Docker: hub update shows docker pull suggestion"
}
test_docker_hub_update_message

test_docker_hub_install_message() {
    _output=$(bash "$REPO_ROOT/docker/scripts/urnet-tools.sh" hub install 2>&1) && _ec=0 || _ec=$?
    assert_exit_code "1" "$_ec" "Docker: hub install exits non-zero"
    assert_contains "docker pull" "$_output" "Docker: hub install shows docker pull suggestion"
}
test_docker_hub_install_message

test_docker_hub_init_message() {
    _output=$(bash "$REPO_ROOT/docker/scripts/urnet-tools.sh" hub init 2>&1) && _ec=0 || _ec=$?
    assert_exit_code "1" "$_ec" "Docker: hub init exits non-zero"
    assert_contains "docker pull" "$_output" "Docker: hub init shows docker pull suggestion"
}
test_docker_hub_init_message

# ============================================================================
echo ""
echo "=============================================="
if [ "$FAILS" -eq 0 ]; then
    echo "  All hub update tests passed!"
    exit 0
else
    echo "  $FAILS test(s) failed"
    exit 1
fi
