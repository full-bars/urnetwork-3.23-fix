#!/bin/bash
# test_login_exit_code.sh: verifies the shared login retry loop reports the
# auth command's REAL exit code. `AUTH_OUTPUT=$(...) || true` followed by
# `$?` captured the status of `true`, so every failure logged
# "Authentication failed (exit code: 0)" and the panic line was the only
# real diagnostic left. The fix keeps the exit status of the auth command
# itself in AUTH_EXIT_CODE.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="${REPO_DIR:-$(cd "$HERE/../.." && pwd)}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

ok() { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1"; }

extract_func() {
    awk -v fn="$2" '
        $0 ~ "^"fn"\\(\\) *\\{" { grab=1 }
        grab { print }
        grab && /^}/ { exit }
    ' "$1"
}

common="$REPO/docker/scripts/provider_common.sh"

# make_stub <exit-code> <message> <also-write-jwt: yes|no>
# A stub binary whose auth subcommand behaves like the real one: fails with a
# nonzero status (bad credentials, or a binary that cannot even exec on this
# platform) or succeeds and writes the JWT.
make_stub() {
    cat > "$tmp/urnetwork_amd64_stable" <<STUB_EOF
#!/bin/sh
case "\$1" in
    auth)
        sleep 0.2
        echo "$2"
        [ "$3" = "yes" ] && printf 'jwt-token' > "\$JWT_FILE"
        exit $1
        ;;
    *)
        exit 0
        ;;
esac
STUB_EOF
    chmod +x "$tmp/urnetwork_amd64_stable"
}

# run_login <out-dir> [timeout-seconds]
# Builds the runner around the REAL func_login_retry_loop with the same
# stubs the other docker harnesses use (log -> file, sleep -> no-op) and
# prints the runner's exit status.
run_login() {
    local out="$1" tmo="${2:-10}"
    mkdir -p "$out"
    {
        echo 'set -u'
        echo "APP_DIR='$tmp'"
        echo "A_SYS_ARCH='amd64'"
        echo "PROVIDER_FLAVOR='stable'"
        echo "USER_AUTH='test-user'"
        echo "PASSWORD='test-pass'"
        echo "JWT_FILE='$out/jwt'"
        echo "export JWT_FILE"
        echo "PROVIDER_STOPPING=0"
        echo 'PROVIDER_CHILD_PID=""'
        echo "DEBUG=false"
        echo "log() { echo \"\$*\" >> '$out/login.log'; }"
        echo "sleep() { :; }"
        extract_func "$common" func_exit_if_stopping
        extract_func "$common" func_sleep
        extract_func "$common" func_login_retry_loop
        echo 'func_login_retry_loop'
        echo 'echo "rc=$?"'
    } > "$tmp/runner.sh"
    timeout "$tmo" bash "$tmp/runner.sh" > "$out/runner.out" 2>&1
}

# 1. Failure path: the logged exit code is the auth command's own status.
make_stub 3 "panic: runtime error: bad credentials" no
out="$tmp/fail"
run_login "$out" 3
rc=$?
[ "$rc" -eq 124 ] && ok "failure loop keeps retrying (killed by timeout)" \
    || bad "failure loop ended early (rc=$rc); it must retry until stopped"
grep -q "Authentication failed (exit code: 3)" "$out/login.log" \
    && ok "failure logs the real exit code (3)" \
    || bad "failure does not log the real exit code"
if grep -q "exit code: 0" "$out/login.log"; then
    bad "failure still logs exit code 0"
else
    ok "failure never logs a fake exit code 0"
fi
grep -qi "PANIC: RUNTIME ERROR" "$out/login.log" \
    && ok "panic line is uppercased into the failure log" \
    || bad "panic line missing from the failure log"

# 2. Success path: JWT written and verified, loop returns 0.
make_stub 0 "Jwt written to $tmp/ok/jwt" yes
out="$tmp/ok"
run_login "$out" 10
rc=$?
[ "$rc" -eq 0 ] && ok "success path returns 0" || bad "success path returned $rc"
grep -q "rc=0" "$out/runner.out" && ok "func_login_retry_loop exits 0 on success" \
    || bad "func_login_retry_loop did not exit 0: $(cat "$out/runner.out")"
grep -q "Authentication successful" "$out/login.log" \
    && ok "success logs the JWT confirmation" \
    || bad "success confirmation missing"

echo ""
echo "Results: $pass passed, $fail failed"
exit $((fail > 0))
