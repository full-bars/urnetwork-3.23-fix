#!/bin/bash
# test_exit75_restart.sh: verifies that exit status 75 is treated as a planned
# restart rather than a crash across Docker provider startup loops.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="${REPO_DIR:-$(cd "$HERE/../.." && pwd)}"
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

extract_func() {
    awk -v fn="$2" '
        $0 ~ "^"fn"\\(\\) *\\{" { grab=1 }
        grab { print }
        grab && /^}/ { exit }
    ' "$1"
}

run_test_case() {
    local script_file="$1"
    local exit_sequence="$2" # e.g. "75 75 75 0" or "1 1 1"
    local test_dir="$tmp/case_${RANDOM}_$$"
    mkdir -p "$test_dir/app" "$test_dir/home/.urnetwork"

    local jwt_file="$test_dir/home/.urnetwork/jwt"
    printf "initial-jwt-token" > "$jwt_file"

    printf "%s\n" "$exit_sequence" > "$test_dir/sequence.txt"
    printf "0\n" > "$test_dir/seq_idx.txt"

    # Create stub provider binary
    cat > "$test_dir/app/urnetwork_amd64_stable" << 'STUB_EOF'
#!/bin/sh
case "$1" in
    --version)
        echo "3.23.test"
        exit 0
        ;;
    provide)
        seq_file="$TEST_DIR/sequence.txt"
        idx_file="$TEST_DIR/seq_idx.txt"
        idx=$(cat "$idx_file")
        codes=$(cat "$seq_file")
        code=$(echo "$codes" | awk -v i="$((idx+1))" '{print $i}')
        echo "$((idx+1))" > "$idx_file"
        if [ -z "$code" ]; then
            exit 0
        fi
        exit "$code"
        ;;
    *)
        exit 0
        ;;
esac
STUB_EOF
    chmod +x "$test_dir/app/urnetwork_amd64_stable"

    local runner="$test_dir/runner.sh"
    {
        echo "set -u"
        echo "export TEST_DIR='$test_dir'"
        echo "APP_DIR='$test_dir/app'"
        echo "A_SYS_ARCH='amd64'"
        echo "HOME='$test_dir/home'"
        echo "JWT_FILE='$jwt_file'"
        echo "USER_AUTH=''"
        echo "PASSWORD=''"
        echo "log() { echo \"LOG: \$*\" >> '$test_dir/out.log'; }"
        echo "sleep() { :; }"
        echo "pgrep() { return 1; }"
        echo "func_do_login() { echo 'LOG: func_do_login called' >> '$test_dir/out.log'; }"
        echo "func_check_credentials() { echo 'LOG: func_check_credentials called' >> '$test_dir/out.log'; }"
        extract_func "$REPO/docker/scripts/$script_file" func_start_provider
        echo "func_start_provider"
    } > "$runner"

    timeout 5 bash "$runner" >/dev/null 2>&1 || true
    echo "$test_dir"
}

# 1. start_stable.sh: 75,75,75,0 sequence leaves JWT intact and never logs clear-JWT
out1="$(run_test_case start_stable.sh "75 75 75 0")"
t "start_stable.sh (75,75,75,0): JWT file intact" \
    test -f "$out1/home/.urnetwork/jwt"
t "start_stable.sh (75,75,75,0): JWT content unchanged" \
    sh -c "[ \"\$(cat '$out1/home/.urnetwork/jwt')\" = 'initial-jwt-token' ]"
t "start_stable.sh (75,75,75,0): never logs clearing JWT" \
    sh -c "! grep -qi 'clearing JWT' '$out1/out.log'"
t "start_stable.sh (75,75,75,0): logs planned restart" \
    grep -qi "planned restart" "$out1/out.log"

# 2. start_stable.sh: 1,1,1 sequence clears JWT and logs clear message
out2="$(run_test_case start_stable.sh "1 1 1")"
t "start_stable.sh (1,1,1): logs clearing JWT message" \
    grep -qi "clearing JWT" "$out2/out.log"
t "start_stable.sh (1,1,1): JWT file was removed" \
    sh -c "[ ! -f '$out2/home/.urnetwork/jwt' ]"

# 3. pelican_panel.sh: 75,75,75,0 sequence leaves JWT intact and never logs clear-JWT
out3="$(run_test_case pelican_panel.sh "75 75 75 0")"
t "pelican_panel.sh (75,75,75,0): JWT file intact" \
    test -f "$out3/home/.urnetwork/jwt"
t "pelican_panel.sh (75,75,75,0): never logs clearing JWT" \
    sh -c "! grep -qi 'clearing JWT' '$out3/out.log'"
t "pelican_panel.sh (75,75,75,0): logs planned restart" \
    grep -qi "planned restart" "$out3/out.log"

# 4. pelican_panel.sh: 1,1,1 sequence clears JWT
out4="$(run_test_case pelican_panel.sh "1 1 1")"
t "pelican_panel.sh (1,1,1): logs clearing JWT message" \
    grep -qi "clearing JWT" "$out4/out.log"
t "pelican_panel.sh (1,1,1): JWT file was removed" \
    sh -c "[ ! -f '$out4/home/.urnetwork/jwt' ]"

echo ""
echo "Results: $pass passed, $fail failed"
exit $((fail > 0))
