#!/bin/bash
# Tests for the Docker-backed 'hub install'/'hub update' path:
#   - Linux (Provider_Install_Linux.sh): opt-in via --docker
#   - macOS (Provider_Install_Mac.sh): always Docker (no native hub binary)
#   - The --docker dispatch guard in Linux's do_hub() that routes install/
#     update into the containerized path without disturbing the native one.
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

TEMP_DIR=$(mktemp -d)
trap "rm -rf $TEMP_DIR" EXIT

# Every test body runs inside a ( ... ) subshell for variable isolation
# between tests. A plain `FAILS=$((FAILS+1))` inside that subshell only
# updates the subshell's own copy and is lost when it exits — so failures
# record to this file instead, and the real count is the line count at
# the end.
FAIL_LOG="$TEMP_DIR/fails.log"
: > "$FAIL_LOG"
FAILS=0

# run_capture CMD [ARGS...]
# Runs CMD via command substitution (a real subshell), capturing combined
# stdout+stderr into $_output and its exit code into $_ec. This MUST be a
# subshell: several functions under test call `exit` directly on error
# paths (not `return`) — calling them in the current shell would kill the
# whole test runner, not just fail the one test.
#
# The tradeoff: variables a mocked dependency (e.g. a stubbed `docker`)
# sets *inside* that subshell don't propagate back out. Tests that need to
# observe a mock's side effect across this boundary use mark/is_marked
# (file-based) instead of a plain variable.
run_capture() {
    # The && / || (rather than a bare assignment + $?) is required, not
    # stylistic: under `set -e`, a command substitution used as a plain
    # assignment still propagates its exit status to the enclosing simple
    # command — an unchecked nonzero result here would abort the whole
    # script instead of just being recorded, for every "expected failure"
    # test (which is most of them).
    _output="$("$@" 2>&1)" && _ec=0 || _ec=$?
}

MARK_DIR="$TEMP_DIR/marks"
mkdir -p "$MARK_DIR"

# mark NAME — records that NAME happened. Unlike a variable, this is a
# real file write, so it survives being called from inside a run_capture
# subshell. Call reset_marks at the start of any test that uses this, to
# avoid leftover marks from an earlier test.
mark() {
    touch "$MARK_DIR/$1"
}

is_marked() {
    [ -f "$MARK_DIR/$1" ]
}

reset_marks() {
    rm -rf "$MARK_DIR"
    mkdir -p "$MARK_DIR"
}

assert_marked() {
    local name="$1" msg="$2"
    if is_marked "$name"; then
        echo "  ✅ PASS: $msg"
    else
        echo "  ❌ FAIL: $msg"
        FAILS=$((FAILS + 1)); echo "$msg" >> "$FAIL_LOG"
    fi
}

assert_not_marked() {
    local name="$1" msg="$2"
    if is_marked "$name"; then
        echo "  ❌ FAIL: $msg"
        FAILS=$((FAILS + 1)); echo "$msg" >> "$FAIL_LOG"
    else
        echo "  ✅ PASS: $msg"
    fi
}

assert_eq() {
    local expected="$1" actual="$2" msg="$3"
    if [ "$expected" = "$actual" ]; then
        echo "  ✅ PASS: $msg"
    else
        echo "  ❌ FAIL: $msg"
        echo "     Expected: '$expected'"
        echo "     Actual:   '$actual'"
        FAILS=$((FAILS + 1)); echo "$msg" >> "$FAIL_LOG"
    fi
}

assert_contains() {
    local needle="$1" haystack="$2" msg="$3"
    if printf "%s" "$haystack" | grep -qF -- "$needle"; then
        echo "  ✅ PASS: $msg"
    else
        echo "  ❌ FAIL: $msg"
        echo "     Expected to contain: '$needle'"
        echo "     Got: $(printf '%s' "$haystack" | head -5)"
        FAILS=$((FAILS + 1)); echo "$msg" >> "$FAIL_LOG"
    fi
}

assert_not_contains() {
    local needle="$1" haystack="$2" msg="$3"
    if printf '%s' "$haystack" | grep -qF -- "$needle"; then
        echo "  ❌ FAIL: $msg"
        echo "     Expected NOT to contain: '$needle'"
        echo "     Got: $(printf '%s' "$haystack" | head -5)"
        FAILS=$((FAILS + 1)); echo "$msg" >> "$FAIL_LOG"
    else
        echo "  ✅ PASS: $msg"
    fi
}

assert_exit_code() {
    local expected="$1" actual="$2" msg="$3"
    if [ "$expected" = "$actual" ]; then
        echo "  ✅ PASS: $msg (exit=$actual)"
    else
        echo "  ❌ FAIL: $msg"
        echo "     Expected exit: $expected, actual: $actual"
        FAILS=$((FAILS + 1)); echo "$msg" >> "$FAIL_LOG"
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
        FAILS=$((FAILS + 1)); echo "$msg" >> "$FAIL_LOG"
    fi
}

assert_file_absent() {
    local file="$1" msg="$2"
    if [ ! -f "$file" ]; then
        echo "  ✅ PASS: $msg"
    else
        echo "  ❌ FAIL: $msg"
        echo "     File '$file' should not exist but does."
        FAILS=$((FAILS + 1)); echo "$msg" >> "$FAIL_LOG"
    fi
}

# --- lib extraction (same technique as test_hub_update.sh: strip the
# arg-dispatch tail so sourcing only defines functions, doesn't execute
# the CLI) ---

LINUX_LIB="${TEMP_DIR}/linux_lib.sh"
sed '/^case "\$operation" in/,$d' "$REPO_ROOT/scripts/Provider_Install_Linux.sh" > "$LINUX_LIB"
if ! grep -q "do_hub_docker_install" "$LINUX_LIB"; then
    echo "❌ FATAL: do_hub_docker_install not found in extracted Linux lib"
    exit 1
fi

MAC_LIB="${TEMP_DIR}/mac_lib.sh"
sed '/^case "\$operation" in/,$d' "$REPO_ROOT/scripts/Provider_Install_Mac.sh" > "$MAC_LIB"
if ! grep -q "do_hub_docker_install" "$MAC_LIB"; then
    echo "❌ FATAL: do_hub_docker_install not found in extracted Mac lib"
    exit 1
fi

# ============================================================================
# SECTION 1: hub_docker_require (Linux)
# ============================================================================

echo ""
echo "=== SECTION 1: hub_docker_require (Linux) ==="

test_require_fails_without_docker() {
    (
        # shellcheck disable=SC1090
        source "$LINUX_LIB"
        # Shadow any real 'docker' by making it unresolvable: point PATH at
        # an empty dir (created before PATH is cleared, since mkdir itself
        # needs to be resolvable).
        mkdir -p "$TEMP_DIR/empty-bin"
        _real_path="$PATH"
        PATH="$TEMP_DIR/empty-bin"
        pr_err() { printf 'ERR: %s\n' "$*"; }

        _output="$(hub_docker_require 2>&1)" && _ec=0 || _ec=$?
        PATH="$_real_path"  # restore so the assert_* helpers below can find grep/head
        assert_exit_code "1" "$_ec" "No docker on PATH: exits 1"
        assert_contains "Docker is required" "$_output" "No docker on PATH: descriptive error"
    )
}
test_require_fails_without_docker

test_require_fails_when_daemon_down() {
    (
        # shellcheck disable=SC1090
        source "$LINUX_LIB"
        docker() { [ "$1" = "info" ] && return 1; return 0; }
        pr_err() { printf 'ERR: %s\n' "$*"; }

        _output="$(hub_docker_require 2>&1)" && _ec=0 || _ec=$?
        assert_exit_code "1" "$_ec" "Docker installed but daemon down: exits 1"
        assert_contains "not running" "$_output" "Docker daemon down: descriptive error"
    )
}
test_require_fails_when_daemon_down

test_require_succeeds_when_available() {
    (
        # shellcheck disable=SC1090
        source "$LINUX_LIB"
        docker() { return 0; }

        _ec=0
        hub_docker_require || _ec=$?
        assert_exit_code "0" "$_ec" "Docker present and running: succeeds"
    )
}
test_require_succeeds_when_available

# ============================================================================
# SECTION 2: do_hub_docker_install (Linux)
# ============================================================================

echo ""
echo "=== SECTION 2: do_hub_docker_install (Linux) ==="

test_install_happy_path_defaults() {
    (
        # shellcheck disable=SC1090
        source "$LINUX_LIB"
        export HOME="$TEMP_DIR/install_defaults"
        hub_docker_conf="$HOME/.urnetwork/hub-docker.conf"
        mkdir -p "$HOME"

        _pulled=""
        _run_args=""
        docker() {
            case "$1" in
                info) return 0 ;;
                ps) echo "" ;;  # no existing containers
                pull) _pulled="$2" ;;
                run) shift; _run_args="$*" ;;
            esac
        }
        pr_info() { printf 'INFO: %s\n' "$*"; }
        pr_err()  { printf 'ERR: %s\n' "$*"; }

        _ec=0
        do_hub_docker_install || _ec=$?
        assert_exit_code "0" "$_ec" "Install defaults: succeeds"
        assert_eq "ghcr.io/full-bars/urnetwork-3.23-fix-hub:latest" "$_pulled" "Install defaults: pulls :latest"
        assert_contains "-p 8080:8080" "$_run_args" "Install defaults: maps port 8080"
        assert_contains "-v urnetwork-hubdata:/data" "$_run_args" "Install defaults: mounts named volume"
        assert_contains "--name urnetwork-hub" "$_run_args" "Install defaults: names container urnetwork-hub"
        assert_not_contains "URNETWORK_HUB_TOKEN" "$_run_args" "Install defaults: no token env var when none given"
        assert_file_contains "$hub_docker_conf" "tag=latest" "Install defaults: conf records tag"
        assert_file_contains "$hub_docker_conf" "port=8080" "Install defaults: conf records port"
    )
}
test_install_happy_path_defaults

test_install_custom_flags() {
    (
        # shellcheck disable=SC1090
        source "$LINUX_LIB"
        export HOME="$TEMP_DIR/install_custom"
        hub_docker_conf="$HOME/.urnetwork/hub-docker.conf"
        mkdir -p "$HOME"

        _pulled=""
        _run_args=""
        docker() {
            case "$1" in
                info) return 0 ;;
                ps) echo "" ;;
                pull) _pulled="$2" ;;
                run) shift; _run_args="$*" ;;
            esac
        }
        pr_info() { :; }
        pr_err()  { printf 'ERR: %s\n' "$*"; }

        do_hub_docker_install --tag v0.3.0 --port 9090 --token secret123

        assert_eq "ghcr.io/full-bars/urnetwork-3.23-fix-hub:v0.3.0" "$_pulled" "Custom flags: pulls requested tag"
        assert_contains "-p 9090:8080" "$_run_args" "Custom flags: maps custom host port to container 8080"
        assert_contains "URNETWORK_HUB_TOKEN=secret123" "$_run_args" "Custom flags: sets token env var"
        assert_file_contains "$hub_docker_conf" "tag=v0.3.0" "Custom flags: conf records custom tag"
        assert_file_contains "$hub_docker_conf" "port=9090" "Custom flags: conf records custom port"
        assert_file_contains "$hub_docker_conf" "token=secret123" "Custom flags: conf records token"
    )
}
test_install_custom_flags

test_install_refuses_existing_container() {
    (
        # shellcheck disable=SC1090
        source "$LINUX_LIB"
        export HOME="$TEMP_DIR/install_existing"
        hub_docker_conf="$HOME/.urnetwork/hub-docker.conf"
        mkdir -p "$HOME"

        reset_marks
        docker() {
            case "$1" in
                info) return 0 ;;
                ps) echo "urnetwork-hub" ;;  # already exists
                pull) mark pull_called ;;
            esac
        }
        pr_info() { :; }
        pr_err()  { printf 'ERR: %s\n' "$*"; }

        run_capture do_hub_docker_install
        assert_exit_code "1" "$_ec" "Existing container: install refuses, exits 1"
        assert_contains "already exists" "$_output" "Existing container: descriptive error"
        assert_not_marked "pull_called" "Existing container: never attempts docker pull"
        assert_file_absent "$hub_docker_conf" "Existing container: no conf file written"
    )
}
test_install_refuses_existing_container

test_install_pull_failure() {
    (
        # shellcheck disable=SC1090
        source "$LINUX_LIB"
        export HOME="$TEMP_DIR/install_pullfail"
        hub_docker_conf="$HOME/.urnetwork/hub-docker.conf"
        mkdir -p "$HOME"

        reset_marks
        docker() {
            case "$1" in
                info) return 0 ;;
                ps) echo "" ;;
                pull) return 1 ;;
                run) mark run_called ;;
            esac
        }
        pr_info() { :; }
        pr_err()  { printf 'ERR: %s\n' "$*"; }

        run_capture do_hub_docker_install
        assert_exit_code "1" "$_ec" "Pull failure: install exits 1"
        assert_contains "docker pull failed" "$_output" "Pull failure: descriptive error"
        assert_not_marked "run_called" "Pull failure: never attempts docker run"
    )
}
test_install_pull_failure

test_install_run_failure() {
    (
        # shellcheck disable=SC1090
        source "$LINUX_LIB"
        export HOME="$TEMP_DIR/install_runfail"
        hub_docker_conf="$HOME/.urnetwork/hub-docker.conf"
        mkdir -p "$HOME"

        docker() {
            case "$1" in
                info) return 0 ;;
                ps) echo "" ;;
                pull) return 0 ;;
                run) return 1 ;;
            esac
        }
        pr_info() { :; }
        pr_err()  { printf 'ERR: %s\n' "$*"; }

        run_capture do_hub_docker_install
        assert_exit_code "1" "$_ec" "Run failure: install exits 1"
        assert_contains "Failed to start hub container" "$_output" "Run failure: descriptive error"
        assert_file_absent "$hub_docker_conf" "Run failure: no conf file written when run failed"
    )
}
test_install_run_failure

# ============================================================================
# SECTION 3: do_hub_docker_update (Linux)
# ============================================================================

echo ""
echo "=== SECTION 3: do_hub_docker_update (Linux) ==="

test_update_fails_when_not_installed() {
    (
        # shellcheck disable=SC1090
        source "$LINUX_LIB"
        export HOME="$TEMP_DIR/update_notinstalled"
        hub_docker_conf="$HOME/.urnetwork/hub-docker.conf"
        mkdir -p "$HOME"

        docker() {
            case "$1" in
                info) return 0 ;;
                ps) echo "" ;;  # no container
            esac
        }
        pr_info() { :; }
        pr_err()  { printf 'ERR: %s\n' "$*"; }

        run_capture do_hub_docker_update
        assert_exit_code "1" "$_ec" "Not installed: update exits 1"
        assert_contains "Run 'urnet-tools hub install --docker' first" "$_output" "Not installed: descriptive error"
    )
}
test_update_fails_when_not_installed

test_update_reuses_persisted_conf() {
    (
        # shellcheck disable=SC1090
        source "$LINUX_LIB"
        export HOME="$TEMP_DIR/update_persisted"
        hub_docker_conf="$HOME/.urnetwork/hub-docker.conf"
        mkdir -p "$HOME/.urnetwork"
        printf 'tag=v1.2.3\nport=9999\ntoken=fromconf\n' > "$hub_docker_conf"

        _pulled=""
        _run_args=""
        docker() {
            case "$1" in
                info) return 0 ;;
                ps) echo "urnetwork-hub" ;;
                pull) _pulled="$2" ;;
                stop|rm) return 0 ;;
                inspect)
                    # force a "differs" result so the recreate path runs
                    [ "$3" = "{{.Image}}" ] && echo "old" || echo "new"
                    ;;
                run) shift; _run_args="$*" ;;
            esac
        }
        pr_info() { :; }
        pr_err()  { printf 'ERR: %s\n' "$*"; }

        do_hub_docker_update

        assert_eq "ghcr.io/full-bars/urnetwork-3.23-fix-hub:v1.2.3" "$_pulled" "Persisted conf: reuses saved tag"
        assert_contains "-p 9999:8080" "$_run_args" "Persisted conf: reuses saved port"
        assert_contains "URNETWORK_HUB_TOKEN=fromconf" "$_run_args" "Persisted conf: reuses saved token"
    )
}
test_update_reuses_persisted_conf

test_update_explicit_tag_overrides_conf() {
    (
        # shellcheck disable=SC1090
        source "$LINUX_LIB"
        export HOME="$TEMP_DIR/update_override"
        hub_docker_conf="$HOME/.urnetwork/hub-docker.conf"
        mkdir -p "$HOME/.urnetwork"
        printf 'tag=v1.0.0\nport=8080\ntoken=\n' > "$hub_docker_conf"

        _pulled=""
        docker() {
            case "$1" in
                info) return 0 ;;
                ps) echo "urnetwork-hub" ;;
                pull) _pulled="$2" ;;
                stop|rm) return 0 ;;
                inspect) [ "$3" = "{{.Image}}" ] && echo "old" || echo "new" ;;
                run) return 0 ;;
            esac
        }
        pr_info() { :; }
        pr_err()  { printf 'ERR: %s\n' "$*"; }

        do_hub_docker_update --tag v2.0.0
        assert_eq "ghcr.io/full-bars/urnetwork-3.23-fix-hub:v2.0.0" "$_pulled" "Explicit --tag wins over persisted conf"
    )
}
test_update_explicit_tag_overrides_conf

test_update_skips_when_already_current() {
    (
        # shellcheck disable=SC1090
        source "$LINUX_LIB"
        export HOME="$TEMP_DIR/update_current"
        hub_docker_conf="$HOME/.urnetwork/hub-docker.conf"
        mkdir -p "$HOME/.urnetwork"

        reset_marks
        docker() {
            case "$1" in
                info) return 0 ;;
                ps) echo "urnetwork-hub" ;;
                pull) return 0 ;;
                inspect) echo "same-id" ;;  # running == pulled
                stop) mark stop_called ;;
                run) mark run_called ;;
            esac
        }
        pr_info() { printf 'INFO: %s\n' "$*"; }
        pr_err()  { printf 'ERR: %s\n' "$*"; }

        run_capture do_hub_docker_update
        assert_contains "Nothing to do" "$_output" "Already current: reports nothing to do"
        assert_not_marked "stop_called" "Already current: does not stop container"
        assert_not_marked "run_called" "Already current: does not recreate container"
    )
}
test_update_skips_when_already_current

test_update_force_recreates_even_when_current() {
    (
        # shellcheck disable=SC1090
        source "$LINUX_LIB"
        export HOME="$TEMP_DIR/update_force"
        hub_docker_conf="$HOME/.urnetwork/hub-docker.conf"
        mkdir -p "$HOME/.urnetwork"

        _run_called=0
        docker() {
            case "$1" in
                info) return 0 ;;
                ps) echo "urnetwork-hub" ;;
                pull) return 0 ;;
                inspect) echo "same-id" ;;
                stop|rm) return 0 ;;
                run) _run_called=1 ;;
            esac
        }
        pr_info() { :; }
        pr_err()  { printf 'ERR: %s\n' "$*"; }

        do_hub_docker_update --force
        assert_eq "1" "$_run_called" "--force: recreates even when image unchanged"
    )
}
test_update_force_recreates_even_when_current

test_update_pull_failure() {
    (
        # shellcheck disable=SC1090
        source "$LINUX_LIB"
        export HOME="$TEMP_DIR/update_pullfail"
        hub_docker_conf="$HOME/.urnetwork/hub-docker.conf"
        mkdir -p "$HOME/.urnetwork"

        docker() {
            case "$1" in
                info) return 0 ;;
                ps) echo "urnetwork-hub" ;;
                pull) return 1 ;;
            esac
        }
        pr_info() { :; }
        pr_err()  { printf 'ERR: %s\n' "$*"; }

        run_capture do_hub_docker_update
        assert_exit_code "1" "$_ec" "Update pull failure: exits 1"
        assert_contains "docker pull failed" "$_output" "Update pull failure: descriptive error"
    )
}
test_update_pull_failure

# ============================================================================
# SECTION 4: --docker dispatch guard in do_hub() (Linux)
# ============================================================================

echo ""
echo "=== SECTION 4: --docker dispatch guard (Linux) ==="

test_dispatch_docker_flag_routes_to_docker_install() {
    (
        # shellcheck disable=SC1090
        source "$LINUX_LIB"
        export HOME="$TEMP_DIR/dispatch_install"
        mkdir -p "$HOME"

        _docker_install_called=0
        _native_install_called=0
        do_hub_docker_install() { _docker_install_called=1; }
        # Native path relies on has_systemd/systemctl etc.; if the guard
        # fails to short-circuit, it will try to run real systemd commands
        # and fail loudly rather than silently — either way this flag would
        # stay 0, proving the guard actually intercepted the call.
        systemctl() { _native_install_called=1; return 0; }

        do_hub install --docker --tag v1.0.0

        assert_eq "1" "$_docker_install_called" "hub install --docker: calls do_hub_docker_install"
        assert_eq "0" "$_native_install_called" "hub install --docker: never touches systemctl"
    )
}
test_dispatch_docker_flag_routes_to_docker_install

test_dispatch_docker_flag_position_independent() {
    (
        # shellcheck disable=SC1090
        source "$LINUX_LIB"
        export HOME="$TEMP_DIR/dispatch_position"
        mkdir -p "$HOME"

        _captured_args=""
        do_hub_docker_install() { _captured_args="$*"; }

        do_hub install --tag v1.0.0 --docker --port 9000

        assert_contains "--tag v1.0.0" "$_captured_args" "Flag anywhere in args: preceding args still passed through"
        assert_contains "--port 9000" "$_captured_args" "Flag anywhere in args: trailing args still passed through"
        assert_not_contains "--docker" "$_captured_args" "Flag anywhere in args: --docker itself is stripped before delegating"
    )
}
test_dispatch_docker_flag_position_independent

test_dispatch_without_docker_flag_uses_native_path() {
    (
        # shellcheck disable=SC1090
        source "$LINUX_LIB"
        export HOME="$TEMP_DIR/dispatch_native"
        mkdir -p "$HOME"

        _docker_install_called=0
        do_hub_docker_install() { _docker_install_called=1; }
        has_systemd=0  # forces the native path to fail fast and predictably
        pr_err() { printf 'ERR: %s\n' "$*"; }

        run_capture do_hub install
        assert_eq "0" "$_docker_install_called" "hub install (no flag): does not call do_hub_docker_install"
        assert_contains "systemd is not available" "$_output" "hub install (no flag): takes native systemd path"
    )
}
test_dispatch_without_docker_flag_uses_native_path

test_dispatch_docker_flag_routes_to_docker_update() {
    (
        # shellcheck disable=SC1090
        source "$LINUX_LIB"
        export HOME="$TEMP_DIR/dispatch_update"
        mkdir -p "$HOME"

        _docker_update_called=0
        _native_update_called=0
        do_hub_docker_update() { _docker_update_called=1; }
        do_hub_update() { _native_update_called=1; }

        do_hub update --docker --force

        assert_eq "1" "$_docker_update_called" "hub update --docker: calls do_hub_docker_update"
        assert_eq "0" "$_native_update_called" "hub update --docker: never calls native do_hub_update"
    )
}
test_dispatch_docker_flag_routes_to_docker_update

# ============================================================================
# SECTION 5: macOS (always-Docker path)
# ============================================================================

echo ""
echo "=== SECTION 5: macOS Docker hub install/update ==="

test_mac_install_happy_path() {
    (
        set +e  # Mac lib's top-level arg-parsing preamble trips errexit when sourced standalone
        # shellcheck disable=SC1090
        source "$MAC_LIB"
        set -e
        export HOME="$TEMP_DIR/mac_install"
        state_dir="$HOME/.urnetwork"
        hub_docker_conf="$state_dir/hub-docker.conf"
        mkdir -p "$HOME"

        _pulled=""
        _run_args=""
        docker() {
            case "$1" in
                info) return 0 ;;
                ps) echo "" ;;
                pull) _pulled="$2" ;;
                run) shift; _run_args="$*" ;;
            esac
        }
        pr_info() { :; }
        pr_err()  { printf 'ERR: %s\n' "$*"; }

        _ec=0
        do_hub_docker_install || _ec=$?
        assert_exit_code "0" "$_ec" "Mac install defaults: succeeds"
        assert_eq "ghcr.io/full-bars/urnetwork-3.23-fix-hub:latest" "$_pulled" "Mac install defaults: pulls :latest"
        assert_contains "-p 8080:8080" "$_run_args" "Mac install defaults: maps port 8080"
        assert_file_contains "$hub_docker_conf" "tag=latest" "Mac install defaults: conf written"
    )
}
test_mac_install_happy_path

test_mac_install_no_docker_gives_darwin_specific_hint() {
    (
        set +e  # Mac lib's top-level arg-parsing preamble trips errexit when sourced standalone
        # shellcheck disable=SC1090
        source "$MAC_LIB"
        set -e
        export HOME="$TEMP_DIR/mac_nodocker"
        mkdir -p "$TEMP_DIR/empty-bin-mac"
        _real_path="$PATH"
        PATH="$TEMP_DIR/empty-bin-mac"
        pr_err() { printf 'ERR: %s\n' "$*"; }

        _output="$(hub_docker_require 2>&1)" && _ec=0 || _ec=$?
        PATH="$_real_path"  # restore so the assert_* helpers below can find grep/head
        assert_exit_code "1" "$_ec" "Mac no docker: exits 1"
        assert_contains "no native binary exists" "$_output" "Mac no docker: explains why Docker is required on macOS"
        assert_contains "Docker Desktop" "$_output" "Mac no docker: points to Docker Desktop"
    )
}
test_mac_install_no_docker_gives_darwin_specific_hint

test_mac_do_hub_skips_provider_binary_check_for_install() {
    (
        set +e  # Mac lib's top-level arg-parsing preamble trips errexit when sourced standalone
        # shellcheck disable=SC1090
        source "$MAC_LIB"
        set -e
        export HOME="$TEMP_DIR/mac_nobinary"
        state_dir="$HOME/.urnetwork"
        provider_bin="$HOME/nonexistent/urnetwork"  # deliberately absent
        mkdir -p "$HOME"

        _docker_install_called=0
        do_hub_docker_install() { _docker_install_called=1; }

        # If do_hub() still required provider_bin to exist for 'install',
        # this would exit 1 with "Provider binary not found" instead.
        # Redirect to a file rather than capturing via $(...) — command
        # substitution runs in its own subshell, which would swallow the
        # _docker_install_called=1 side effect above.
        _out_file="$TEMP_DIR/mac_nobinary_out.txt"
        do_hub install > "$_out_file" 2>&1 && _ec=0 || _ec=$?
        _output="$(cat "$_out_file")"
        assert_eq "1" "$_docker_install_called" "Mac hub install: works without the provider being installed first"
        assert_not_contains "Provider binary not found" "$_output" "Mac hub install: no provider-binary gate for install/update"
    )
}
test_mac_do_hub_skips_provider_binary_check_for_install

test_mac_link_still_requires_provider_binary() {
    (
        set +e  # Mac lib's top-level arg-parsing preamble trips errexit when sourced standalone
        # shellcheck disable=SC1090
        source "$MAC_LIB"
        set -e
        export HOME="$TEMP_DIR/mac_link_gate"
        state_dir="$HOME/.urnetwork"
        provider_bin="$HOME/nonexistent/urnetwork"
        mkdir -p "$HOME"
        pr_err() { printf 'ERR: %s\n' "$*"; }

        run_capture do_hub link https://example.com
        assert_exit_code "1" "$_ec" "Mac hub link: still gated on provider binary (unchanged behavior)"
        assert_contains "Provider binary not found" "$_output" "Mac hub link: original guard still applies to non-install/update commands"
    )
}
test_mac_link_still_requires_provider_binary

# ============================================================================
echo ""
echo "=============================================="
# Real failure count comes from the log file, not $FAILS — every test body
# runs in a ( ... ) subshell, so a plain shell variable can't accumulate
# across tests (see FAIL_LOG comment above).
FAILS=$(wc -l < "$FAIL_LOG" | tr -d ' ')
if [ "$FAILS" -eq 0 ]; then
    echo "  All Docker hub install/update tests passed!"
    exit 0
else
    echo "  $FAILS test(s) failed:"
    sed 's/^/    - /' "$FAIL_LOG"
    exit 1
fi
