#!/bin/bash
# test_provider_env.sh: verifies the mapping from the v33 operator environment
# variables (ALL_OPERATORS, AUTO_REGISTER, HOTKEY_SEED_FILE,
# ALLOW_CLIENT_REGISTRATION, MAX_MEMORY, PROVIDER_ARGS) to `provider provide`
# flags, and that the Docker start scripts pass those flags to the binary.
# Offline only: a stub provider binary records its arguments.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="${REPO_DIR:-$(cd "$HERE/../.." && pwd)}"
HELPER="$REPO/docker/scripts/provider_env.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0
t() {
    local name="$1"
    shift
    if "$@" >/dev/null 2>&1; then
        pass=$((pass+1))
        echo "PASS: $name"
        return 0
    else
        fail=$((fail+1))
        echo "FAIL: $name"
        return 1
    fi
}

# Run provider_env_flags in a clean environment. The first argument is the
# state directory; the rest are VAR=value assignments.
flags() {
    local state_dir="$1"
    shift
    env -i PATH="$PATH" HOME="$tmp/home" STATE_DIR="$state_dir" "$@" \
        sh -c '. "$0"; provider_env_flags "$STATE_DIR"' "$HELPER"
}

expect_flags() {
    local name="$1"
    local want="$2"
    local state_dir="$3"
    shift 3
    local got
    got="$(flags "$state_dir" "$@")"
    t "$name" test "$got" = "$want"
}

mkdir -p "$tmp/home" "$tmp/state"

# --- Legacy: no variables means no flags -----------------------------------
expect_flags "no variables: no flags" "" "$tmp/state"

# --- ALL_OPERATORS ----------------------------------------------------------
expect_flags "ALL_OPERATORS=1" "--all-operators" "$tmp/state" ALL_OPERATORS=1
expect_flags "ALL_OPERATORS=true (case-insensitive)" "--all-operators" "$tmp/state" ALL_OPERATORS=TRUE
expect_flags "ALL_OPERATORS=0 turns it off" "" "$tmp/state" ALL_OPERATORS=0
out="$(flags "$tmp/state" ALL_OPERATORS=maybe 2>&1)"; rc=$?
t "ALL_OPERATORS=maybe fails" test "$rc" -ne 0
t "ALL_OPERATORS=maybe names the variable" grep -q "ALL_OPERATORS must be" <<<"$out"

# --- ALLOW_CLIENT_REGISTRATION ---------------------------------------------
expect_flags "ALLOW_CLIENT_REGISTRATION=Y" "--allow-client-registration" "$tmp/state" ALLOW_CLIENT_REGISTRATION=Y
expect_flags "ALLOW_CLIENT_REGISTRATION=y (lowercase)" "--allow-client-registration" "$tmp/state" ALLOW_CLIENT_REGISTRATION=y
expect_flags "ALLOW_CLIENT_REGISTRATION=N" "" "$tmp/state" ALLOW_CLIENT_REGISTRATION=N
expect_flags "ALLOW_CLIENT_REGISTRATION=n (lowercase)" "" "$tmp/state" ALLOW_CLIENT_REGISTRATION=n

fresh="$tmp/fresh"
mkdir -p "$fresh"
expect_flags "auto on a fresh state dir adds the flag" "--allow-client-registration" "$fresh" ALLOW_CLIENT_REGISTRATION=auto

with_key="$tmp/with_key"
mkdir -p "$with_key"
printf 'seed' > "$with_key/.provider.key"
expect_flags "auto with .provider.key adds nothing" "" "$with_key" ALLOW_CLIENT_REGISTRATION=auto

with_operators="$tmp/with_operators"
mkdir -p "$with_operators/operators"
expect_flags "auto with operators/ adds nothing" "" "$with_operators" ALLOW_CLIENT_REGISTRATION=auto

both="$tmp/both"
mkdir -p "$both/operators"
printf 'seed' > "$both/.provider.key"
expect_flags "auto with both markers adds nothing" "" "$both" ALLOW_CLIENT_REGISTRATION=auto

out="$(flags "$tmp/state" ALLOW_CLIENT_REGISTRATION=maybe 2>&1)"; rc=$?
t "ALLOW_CLIENT_REGISTRATION=maybe fails" test "$rc" -ne 0
t "ALLOW_CLIENT_REGISTRATION=maybe names the values" grep -q "must be auto, Y or N" <<<"$out"

# --- HOTKEY_SEED_FILE and AUTO_REGISTER ------------------------------------
expect_flags "ALL_OPERATORS with hotkey seed, no auto-register" \
    $'--all-operators\n--hotkey_seed_file=/run/secrets/hotkey' "$tmp/state" \
    ALL_OPERATORS=1 HOTKEY_SEED_FILE=/run/secrets/hotkey
expect_flags "ALL_OPERATORS with auto-register and hotkey seed" \
    $'--all-operators\n--auto-register\n--hotkey_seed_file=/run/secrets/hotkey' "$tmp/state" \
    ALL_OPERATORS=1 AUTO_REGISTER=1 HOTKEY_SEED_FILE=/run/secrets/hotkey

out="$(flags "$tmp/state" AUTO_REGISTER=1 2>&1)"; rc=$?
t "AUTO_REGISTER without ALL_OPERATORS fails" test "$rc" -ne 0
t "AUTO_REGISTER without ALL_OPERATORS names the cause" grep -q "AUTO_REGISTER requires ALL_OPERATORS=1" <<<"$out"

out="$(flags "$tmp/state" HOTKEY_SEED_FILE=/run/secrets/hotkey 2>&1)"; rc=$?
t "HOTKEY_SEED_FILE without ALL_OPERATORS fails" test "$rc" -ne 0
t "HOTKEY_SEED_FILE without ALL_OPERATORS names the cause" grep -q "HOTKEY_SEED_FILE requires ALL_OPERATORS=1" <<<"$out"

out="$(flags "$tmp/state" ALL_OPERATORS=1 AUTO_REGISTER=1 2>&1)"; rc=$?
t "AUTO_REGISTER without a hotkey seed fails" test "$rc" -ne 0
t "AUTO_REGISTER without a hotkey seed names the cause" grep -q "AUTO_REGISTER needs HOTKEY_SEED_FILE" <<<"$out"

out="$(flags "$tmp/state" ALL_OPERATORS=1 HOTKEY_SEED_FILE="/run/secret key" 2>&1)"; rc=$?
t "hotkey path with whitespace fails" test "$rc" -ne 0
t "hotkey path with whitespace names the cause" grep -q "HOTKEY_SEED_FILE must not contain whitespace" <<<"$out"

# --- MAX_MEMORY -------------------------------------------------------------
expect_flags "MAX_MEMORY maps to --max-memory" "--max-memory=2gib" "$tmp/state" MAX_MEMORY=2gib
expect_flags "empty MAX_MEMORY adds nothing" "" "$tmp/state" MAX_MEMORY=

out="$(flags "$tmp/state" MAX_MEMORY="2 gib" 2>&1)"; rc=$?
t "MAX_MEMORY with whitespace fails" test "$rc" -ne 0
t "MAX_MEMORY with whitespace names the cause" grep -q "MAX_MEMORY must not contain whitespace" <<<"$out"

# --- PROVIDER_ARGS ----------------------------------------------------------
expect_flags "PROVIDER_ARGS splits on whitespace" \
    $'--operators-url=https://example.test/ops.yml\n--operators-refresh=30m' "$tmp/state" \
    PROVIDER_ARGS="--operators-url=https://example.test/ops.yml --operators-refresh=30m"
expect_flags "PROVIDER_ARGS does not glob" "--file=/tmp/*.txt" "$tmp/state" PROVIDER_ARGS="--file=/tmp/*.txt"

# --- Full combination and order --------------------------------------------
expect_flags "all flags: exact order, PROVIDER_ARGS last" \
    $'--all-operators\n--auto-register\n--hotkey_seed_file=/run/secrets/hotkey\n--max-memory=4gib\n--allow-client-registration\n--operators-url=https://example.test/ops.yml\n--operators-refresh=30m' \
    "$fresh" \
    ALL_OPERATORS=1 AUTO_REGISTER=1 HOTKEY_SEED_FILE=/run/secrets/hotkey \
    MAX_MEMORY=4gib ALLOW_CLIENT_REGISTRATION=auto \
    PROVIDER_ARGS="--operators-url=https://example.test/ops.yml --operators-refresh=30m"

# --- Start script wiring: the flags reach the provider binary --------------
extract_func() {
    awk -v fn="$2" '
        index($0, fn"(){") == 1 || index($0, fn"() {") == 1 { grab = 1 }
        grab { print }
        grab && /^}/ { exit }
    ' "$1"
}
run_provide_case() {
    local script_file="$1"
    shift
    local test_dir="$tmp/case_${RANDOM}_$$"
    mkdir -p "$test_dir/app" "$test_dir/home/.urnetwork"
    printf 'initial-jwt-token' > "$test_dir/home/.urnetwork/jwt"

    cat > "$test_dir/app/urnetwork_amd64_stable" << 'STUB_EOF'
#!/bin/sh
case "$1" in
    --version)
        echo "3.23.test"
        exit 0
        ;;
    provide)
        shift
        : > "$TEST_DIR/provide_args.txt"
        for arg in "$@"; do
            printf '%s\n' "$arg" >> "$TEST_DIR/provide_args.txt"
        done
        exit 0
        ;;
    *)
        exit 0
        ;;
esac
STUB_EOF
    chmod +x "$test_dir/app/urnetwork_amd64_stable"
    cp "$test_dir/app/urnetwork_amd64_stable" "$test_dir/app/urnetwork_amd64_nightly"

    local runner="$test_dir/runner.sh"
    {
        # -e mirrors production: provider_common.sh runs under set -e.
        echo "set -eu"
        echo "export TEST_DIR='$test_dir'"
        echo "APP_DIR='$test_dir/app'"
        echo "A_SYS_ARCH='amd64'"
        echo "HOME='$test_dir/home'"
        echo "JWT_FILE='$test_dir/home/.urnetwork/jwt'"
        echo "USER_AUTH=''"
        echo "PASSWORD=''"
        echo "export URNETWORK_AUTH_CODE=''"
        echo "log() { :; }"
        echo "sleep() { :; }"
        echo "pgrep() { return 1; }"
        case "$script_file" in
            start_stable.sh)  _mode=stable;  _flavor=stable ;;
            start_nightly.sh) _mode=nightly; _flavor=nightly ;;
            start_jwt.sh)     _mode=jwt;     _flavor=stable ;;
            *)                _mode=stable;  _flavor=stable ;;
        esac
        echo "PROVIDER_MODE='$_mode'"
        echo "PROVIDER_FLAVOR='$_flavor'"
        # provider_common.sh initializes these at the top; the extracted loop
        # functions reference them under set -u, so set them here too.
        echo "PROVIDER_STOPPING=0"
        echo "PROVIDER_CHILD_PID=\"\""
        echo ". '$HELPER'"
        case "$script_file" in
            start_stable.sh)  _fn=func_start_provider_stable ;;
            start_nightly.sh) _fn=func_start_provider_nightly ;;
            start_jwt.sh)     _fn=func_start_provider_jwt ;;
            *)                _fn=func_start_provider ;;
        esac
        extract_func "$REPO/docker/scripts/provider_common.sh" resolve_provider_env_args
        extract_func "$REPO/docker/scripts/provider_common.sh" func_exit_if_stopping
        extract_func "$REPO/docker/scripts/provider_common.sh" func_sleep
        extract_func "$REPO/docker/scripts/provider_common.sh" func_run_provider
        extract_func "$REPO/docker/scripts/provider_common.sh" "$_fn"
        echo "$_fn"
    } > "$runner"

    env -i PATH="$PATH" HOME="$test_dir/home" TEST_DIR="$test_dir" "$@" \
        bash "$runner" >/dev/null 2>&1 || true
    echo "$test_dir"
}

provide_args() {
    cat "$1/provide_args.txt" 2>/dev/null || true
}

# Legacy: every start script still calls bare provide when nothing is set.
for script in start_stable.sh start_nightly.sh start_jwt.sh; do
    dir="$(run_provide_case "$script")"
    t "$script: legacy provide has no extra args" test -z "$(provide_args "$dir")"
done

# Stable: the full flag set reaches argv in order.
dir="$(run_provide_case start_stable.sh \
    ALL_OPERATORS=1 AUTO_REGISTER=1 HOTKEY_SEED_FILE=/run/secrets/hotkey \
    ALLOW_CLIENT_REGISTRATION=Y MAX_MEMORY=2gib \
    PROVIDER_ARGS="--operators-url=https://example.test/ops.yml")"
want=$'--all-operators\n--auto-register\n--hotkey_seed_file=/run/secrets/hotkey\n--max-memory=2gib\n--allow-client-registration\n--operators-url=https://example.test/ops.yml'
t "start_stable.sh: mapped flags reach provide" test "$(provide_args "$dir")" = "$want"

# Nightly: hotkey without auto-register.
dir="$(run_provide_case start_nightly.sh ALL_OPERATORS=1 HOTKEY_SEED_FILE=/run/secrets/hotkey)"
want=$'--all-operators\n--hotkey_seed_file=/run/secrets/hotkey'
t "start_nightly.sh: mapped flags reach provide" test "$(provide_args "$dir")" = "$want"

# JWT: MAX_MEMORY only, legacy auth flow untouched.
dir="$(run_provide_case start_jwt.sh MAX_MEMORY=1gib)"
t "start_jwt.sh: mapped flags reach provide" test "$(provide_args "$dir")" = "--max-memory=1gib"

# Stable: ALLOW_CLIENT_REGISTRATION=auto on a fresh state dir.
dir="$(run_provide_case start_stable.sh ALLOW_CLIENT_REGISTRATION=auto)"
t "start_stable.sh: auto adds the flag on a fresh state dir" \
    test "$(provide_args "$dir")" = "--allow-client-registration"

echo ""
echo "Results: $pass passed, $fail failed"
exit $((fail > 0))
