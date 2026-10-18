#!/bin/bash
# test_provider_env.sh: verifies the mapping from the v33 operator environment
# variables (ALL_OPERATORS, AUTO_REGISTER, HOTKEY_SEED_FILE,
# ALLOW_CLIENT_REGISTRATION, MAX_MEMORY, VERBOSE, PROVIDER_PORT,
# PROVIDER_ARGS) to `provider provide` flags, and that the Docker start
# scripts pass those flags to the binary. Also verifies the
# techroy23/Docker-UrNetwork compatibility aliases (API_URL/CONNECT_URL onto
# UR_API_URL/UR_CONNECT_URL, and their verbosity and port spellings).
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
expect_flags "ALL_OPERATORS=Y (techroy23 spelling)" "--all-operators" "$tmp/state" ALL_OPERATORS=Y
expect_flags "ALL_OPERATORS=N turns it off (techroy23 spelling)" "" "$tmp/state" ALL_OPERATORS=N
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
expect_flags "ALL_OPERATORS=Y with AUTO_REGISTER=Y (techroy23 spellings)" \
    $'--all-operators\n--auto-register\n--hotkey_seed_file=/run/secrets/hotkey' "$tmp/state" \
    ALL_OPERATORS=Y AUTO_REGISTER=Y HOTKEY_SEED_FILE=/run/secrets/hotkey

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

# --- VERBOSE (techroy23: 0 off, 1 = -v, 2 = -vv) -----------------------------
expect_flags "VERBOSE=1 maps to -v" "-v" "$tmp/state" VERBOSE=1
expect_flags "VERBOSE=2 maps to -vv" "-vv" "$tmp/state" VERBOSE=2
expect_flags "VERBOSE=0 adds nothing" "" "$tmp/state" VERBOSE=0
expect_flags "empty VERBOSE adds nothing" "" "$tmp/state" VERBOSE=
expect_flags "VERBOSE=y maps to -v (techroy23 spelling)" "-v" "$tmp/state" VERBOSE=y
expect_flags "VERBOSE=vv maps to -vv (techroy23 spelling)" "-vv" "$tmp/state" VERBOSE=vv

out="$(flags "$tmp/state" VERBOSE=3 2>&1)"; rc=$?
t "VERBOSE=3 fails" test "$rc" -ne 0
t "VERBOSE=3 names the values" grep -q "VERBOSE must be 0" <<<"$out"

# --- PROVIDER_PORT (techroy23: --port, 0 disables) ---------------------------
expect_flags "PROVIDER_PORT=8080 maps to --port" "--port=8080" "$tmp/state" PROVIDER_PORT=8080
expect_flags "PROVIDER_PORT=0 adds nothing (binary default)" "" "$tmp/state" PROVIDER_PORT=0
expect_flags "empty PROVIDER_PORT adds nothing" "" "$tmp/state" PROVIDER_PORT=
expect_flags "PROVIDER_PORT normalises leading zeros" "--port=8080" "$tmp/state" PROVIDER_PORT=08080

out="$(flags "$tmp/state" PROVIDER_PORT=abc 2>&1)"; rc=$?
t "PROVIDER_PORT=abc fails" test "$rc" -ne 0
t "PROVIDER_PORT=abc names the cause" grep -q "PROVIDER_PORT must be a number" <<<"$out"

out="$(flags "$tmp/state" PROVIDER_PORT=65536 2>&1)"; rc=$?
t "PROVIDER_PORT out of range fails" test "$rc" -ne 0
t "PROVIDER_PORT out of range names the range" grep -q "between 0 and 65535" <<<"$out"

# --- PROVIDER_ARGS ----------------------------------------------------------
expect_flags "PROVIDER_ARGS splits on whitespace" \
    $'--operators-url=https://example.test/ops.yml\n--operators-refresh=30m' "$tmp/state" \
    PROVIDER_ARGS="--operators-url=https://example.test/ops.yml --operators-refresh=30m"
expect_flags "PROVIDER_ARGS does not glob" "--file=/tmp/*.txt" "$tmp/state" PROVIDER_ARGS="--file=/tmp/*.txt"

# --- techroy23 API_URL/CONNECT_URL aliases ----------------------------------
# Source the helper in a clean environment and print the aliased UR_ pair.
alias_vars() {
    env -i PATH="$PATH" HOME="$tmp/home" "$@" \
        sh -c '. "$0"; printf "%s|%s" "${UR_API_URL:-}" "${UR_CONNECT_URL:-}"' "$HELPER"
}

got="$(alias_vars API_URL=https://api.example.test CONNECT_URL=wss://connect.example.test)"
t "API_URL/CONNECT_URL alias onto UR_API_URL/UR_CONNECT_URL" \
    test "$got" = "https://api.example.test|wss://connect.example.test"

got="$(alias_vars API_URL=https://api.example.test)"
t "only API_URL set leaves UR_CONNECT_URL empty (both-or-neither still enforced)" \
    test "$got" = "https://api.example.test|"

got="$(alias_vars API_URL=https://api.example.test UR_API_URL=https://api.ours.test)"
t "UR_API_URL wins over API_URL" test "$got" = "https://api.ours.test|"

got="$(alias_vars CONNECT_URL=wss://connect.example.test UR_CONNECT_URL=wss://connect.ours.test)"
t "UR_CONNECT_URL wins over CONNECT_URL" test "$got" = "|wss://connect.ours.test"

got="$(alias_vars API_URL=https://api.example.test CONNECT_URL=wss://connect.example.test ALL_OPERATORS=Y 2>/dev/null)"
t "ALL_OPERATORS=Y ignores API_URL/CONNECT_URL" test "$got" = "|"

alias_warn="$(alias_vars API_URL=https://api.example.test CONNECT_URL=wss://connect.example.test ALL_OPERATORS=Y 2>&1 >/dev/null)"
t "ALL_OPERATORS=Y ignoring API_URL/CONNECT_URL says so" \
    grep -q "ignoring API_URL/CONNECT_URL" <<<"$alias_warn"

got="$(alias_vars UR_API_URL=https://api.ours.test UR_CONNECT_URL=wss://connect.ours.test)"
t "UR_ names alone are untouched by the alias" test "$got" = "https://api.ours.test|wss://connect.ours.test"

# --- Full combination and order --------------------------------------------
expect_flags "all flags: exact order, PROVIDER_ARGS last" \
    $'--all-operators\n--auto-register\n--hotkey_seed_file=/run/secrets/hotkey\n--max-memory=4gib\n-vv\n--port=8080\n--allow-client-registration\n--operators-url=https://example.test/ops.yml\n--operators-refresh=30m' \
    "$fresh" \
    ALL_OPERATORS=Y AUTO_REGISTER=Y HOTKEY_SEED_FILE=/run/secrets/hotkey \
    MAX_MEMORY=4gib VERBOSE=2 PROVIDER_PORT=8080 ALLOW_CLIENT_REGISTRATION=auto \
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
        echo "set -u"
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
        echo ". '$HELPER'"
        extract_func "$REPO/docker/scripts/$script_file" func_start_provider
        echo "func_start_provider"
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

# techroy23-style env: verbosity, port, memory and operator flags reach
# provide through the real start script.
dir="$(run_provide_case start_stable.sh \
    VERBOSE=2 PROVIDER_PORT=8080 MAX_MEMORY=1gib \
    ALL_OPERATORS=Y AUTO_REGISTER=Y HOTKEY_SEED_FILE=/run/secrets/hotkey)"
want=$'--all-operators\n--auto-register\n--hotkey_seed_file=/run/secrets/hotkey\n--max-memory=1gib\n-vv\n--port=8080'
t "start_stable.sh: techroy23 env maps to provide flags" test "$(provide_args "$dir")" = "$want"

# Stable: ALLOW_CLIENT_REGISTRATION=auto on a fresh state dir.
dir="$(run_provide_case start_stable.sh ALLOW_CLIENT_REGISTRATION=auto)"
t "start_stable.sh: auto adds the flag on a fresh state dir" \
    test "$(provide_args "$dir")" = "--allow-client-registration"

echo ""
echo "Results: $pass passed, $fail failed"
exit $((fail > 0))
