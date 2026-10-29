#!/usr/bin/env bash
# Offline startup tests: extract real functions and stub the provider, curl and sleeps. No Docker or network.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)" || exit 1
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/app" "$tmp/bin" "$tmp/functions" "$tmp/tmpfiles"

# Keep function bodies intact; redirect only the mounted proxy input for the sandbox.
python3 - "$HERE" "$tmp/functions" <<'PY'
import pathlib, re, sys
source_dir, dest = map(pathlib.Path, sys.argv[1:])
for name, source_name, functions in (
    ('common', 'provider_common.sh', ('func_check_proxy', 'func_ip_checker', 'func_login_retry_loop')),
    ('pelican', 'pelican_panel.sh', ('func_do_login',)),
):
    source = (source_dir / source_name).read_text()
    result = []
    for func in functions:
        match = re.search(r'^' + re.escape(func) + r'\(\) \{.*?^\}\n', source, re.M | re.S)
        if not match:
            raise SystemExit('missing function ' + func + ' in ' + source_name)
        result.append(match.group().replace('"/app/proxy.txt"', '"$TEST_APP/proxy.txt"'))
    (dest / (name + '.sh')).write_text('\n'.join(result))
PY
if [ "$?" -ne 0 ]; then echo 'FAIL: extraction'; exit 1; fi

cat > "$tmp/app/urnetwork_test_stable" <<'SH'
#!/bin/sh
printf '%s\n' "$@" > "$ARGV_LOG"
case "$1" in
  proxy)
    if [ "$PROXY_FAIL" = yes ]; then
      printf '%s\n' partial > "$HOME/.urnetwork/proxy"
      exit 17
    fi
    if [ "$PROXY_FAIL" = silent ]; then exit 0; fi
    if ! grep -q '^[[:space:]]*[^[:space:]#]' "$TEST_APP/proxy.txt"; then exit 0; fi
    python3 - "$HOME/.urnetwork/proxy" <<'PY'
import json, pathlib, sys
path = pathlib.Path(sys.argv[1])
data = json.loads(path.read_text())
assert data['servers'] == {}
data['servers']['new:1080'] = ''
path.write_text(json.dumps(data) + '\n')
PY
    ;;
  auth)
    mkdir -p "${JWT_FILE%/*}"
    printf '%s\n' jwt > "$JWT_FILE"
    if [ "${AUTH_ECHO_MODE:-secrets}" = clean ]; then
      printf '%s\n' 'Jwt written to test file' 'provider diagnostic'
    else
      printf '%s\n' 'Jwt written to test file' "provider echoed $USER_AUTH $PASSWORD"
    fi
    ;;
  --version) printf '%s\n' stub ;;
  *) exit 1 ;;
esac
SH
chmod +x "$tmp/app/urnetwork_test_stable"
cat > "$tmp/bin/curl" <<'SH'
#!/bin/sh
printf '%s\n' called > "$CURL_LOG"
output=
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then output=$2; shift 2; else shift; fi
done
case "$CURL_MODE" in
  fetch-fail)
    printf '%s\n' 'echo executed > "$CURL_LOG.executed"' > "$output"
    exit 22
    ;;
  script-fail) printf '%s\n' 'exit 9' > "$output" ;;
  success) printf '%s\n' 'echo executed > "$CURL_LOG.executed"' > "$output" ;;
  *) exit 1 ;;
esac
SH
chmod +x "$tmp/bin/curl"
cat > "$tmp/driver" <<'SH'
#!/bin/sh
set -e
. "$FUNCTIONS"
log() { printf '%s\n' "$*"; }
func_sleep() { :; }
func_exit_if_stopping() { :; }
APP_DIR="$TEST_APP"
A_SYS_ARCH=test
PROVIDER_FLAVOR=stable
ENABLE_IP_CHECKER=true
IP_CHECKER_URL=https://invalid.example/check
case "$ACTION" in
  proxy) func_check_proxy ;;
  ip) func_ip_checker ;;
  common-login) func_login_retry_loop ;;
  pelican-login) func_do_login ;;
esac
SH

pass=0
fail=0
report() {
  case_name=$1
  check=$2
  if "$check"; then
    pass=$((pass + 1)); printf 'PASS: %s\n' "$case_name"
  else
    fail=$((fail + 1)); printf 'FAIL: %s (exit %s)\n' "$case_name" "$rc"
    if [ -f "$out" ]; then grep -n '.' "$out" || :; fi
  fi
}
run() {
  env PATH="$tmp/bin:$PATH" HOME="$home" TEST_APP="$tmp/app" FUNCTIONS="$tmp/functions/$function_set.sh" \
    ACTION="$action" PROXY_FAIL="${proxy_fail:-no}" ARGV_LOG="$argv" CURL_MODE="${curl_mode:-fetch-fail}" \
    CURL_LOG="$curl_log" DEBUG=true USER_AUTH="${user_auth-fake-user-123}" PASSWORD="${password-fake-password-456}" \
    AUTH_ECHO_MODE="${auth_echo_mode:-secrets}" JWT_FILE="$home/.urnetwork/jwt" \
    TMPDIR="$tmp/tmpfiles" sh "$tmp/driver" > "$out" 2>&1
  rc=$?
}
proxy_kept() { [ "$rc" -eq 0 ] && cmp -s "$state/proxy" "$original" && grep -q 'kept existing proxy configuration' "$out" && [ ! -e "$argv" ]; }
proxy_none() { [ "$rc" -eq 0 ] && [ ! -e "$state/proxy" ] && grep -q 'none configured' "$out" && [ ! -e "$argv" ]; }
no_staging_left() { for stage in "$state"/.proxy-import.*; do [ ! -e "$stage" ] || return 1; done; }
proxy_failure() {
  if ! cmp -s "$state/proxy" "$original"; then
    printf '%s\n' 'live proxy config differs from the seeded bytes'
    return 1
  fi
  [ "$rc" -ne 0 ] && grep -Fq 'Proxy replacement failed' "$out" && cmp -s "$argv" "$expected_argv" && no_staging_left || return 1
  if [ "$proxy_fail" != yes ]; then grep -Fq 'no valid servers' "$out"; fi
}
proxy_invalid_healed() {
  [ "$rc" -eq 0 ] && cmp -s "$argv" "$expected_argv" && no_staging_left || return 1
  backups=("$state"/proxy.corrupt.*)
  [ "${#backups[@]}" -eq 1 ] && cmp -s "${backups[0]}" "$original" &&
    grep -Fq "$state/proxy" "$out" && grep -Fq "${backups[0]}" "$out" &&
    jq -e '.servers == {"new:1080":""}' "$state/proxy" >/dev/null
}
login_diagnostic() {
  [ "$rc" -eq 0 ] && grep -Fxq 'provider diagnostic' "$out" && ! grep -Fq 'AUTH_OUTPUT=[redacted]' "$out" || return 1
  if [ -n "$user_auth" ] || [ -n "$password" ]; then
    auth_echo_mode=secrets; run; auth_echo_mode=clean
    [ "$rc" -eq 0 ] && grep -Fxq 'DEBUG: AUTH_OUTPUT=[redacted]' "$out" &&
      { [ -z "$user_auth" ] || ! grep -Fq "$user_auth" "$out"; } &&
      { [ -z "$password" ] || ! grep -Fq "$password" "$out"; }
  fi
}
proxy_success() {
  [ "$rc" -eq 0 ] && cmp -s "$argv" "$expected_argv" && [ "$(stat -c '%a' "$state/proxy")" = 600 ] && no_staging_left || return 1
  python3 - "$state/proxy" "$expect_auth" <<'PY'
import json, pathlib, sys
path = pathlib.Path(sys.argv[1])
data = json.loads(path.read_text())
assert data['servers'] == {'new:1080': ''}
if sys.argv[2] == 'yes':
    assert data['auths']['keep']['user'] == 'alice'
PY
}
fetch_failure() { [ "$rc" -eq 0 ] && [ -f "$curl_log" ] && [ ! -e "$curl_log.executed" ] && grep -q 'IP checker fetch failed' "$out" && ! grep -Eq 'ran successfully|script failed' "$out"; }
script_failure() { [ "$rc" -eq 0 ] && grep -q 'IP checker script failed' "$out" && ! grep -Eq 'ran successfully|fetch failed' "$out" && [ ! -e "$curl_log.executed" ]; }
script_success() { [ "$rc" -eq 0 ] && grep -q 'IP checker script ran successfully' "$out" && ! grep -Eq 'fetch failed|script failed' "$out" && [ -f "$curl_log.executed" ]; }
login_safe() {
  [ "$rc" -eq 0 ] && ! grep -Fq 'fake-user-123' "$out" && ! grep -Fq 'fake-password-456' "$out" \
    && grep -qx 'DEBUG: USER_AUTH set=yes length=13' "$out" \
    && grep -qx 'DEBUG: PASSWORD set=yes length=17' "$out" \
    && grep -qx 'DEBUG: AUTH_EXIT_CODE=0' "$out" \
    && grep -qx 'DEBUG: AUTH_OUTPUT=\[redacted\]' "$out"
}

function_set=common
home="$tmp/proxy/home"; state="$home/.urnetwork"
mkdir -p "$state"
original="$tmp/proxy/original"; out="$tmp/proxy/out"; argv="$tmp/proxy/argv"; curl_log="$tmp/proxy/curl-log"
expected_argv="$tmp/proxy/expected-argv"
printf '%s\n' proxy add "--proxy_file=$tmp/app/proxy.txt" -f > "$expected_argv"
printf '%s\n' '{"auths":{"keep":{"user":"alice","password":"secret"}},"servers":{"old:1080":"keep"}}' > "$original"
cp "$original" "$state/proxy"
rm -f "$tmp/app/proxy.txt" "$argv"
action=proxy; proxy_fail=no; run
report 'no proxy mount preserves existing config' proxy_kept
rm -f "$state/proxy" "$argv"
run
report 'no proxy mount reports no config without calling provider' proxy_none
printf '%s\n' 'new:1080' > "$tmp/app/proxy.txt"
for corrupt in malformed null; do
  rm -f "$state"/proxy.corrupt.* "$argv"
  if [ "$corrupt" = malformed ]; then printf '%s\n' '{not-json' > "$original"; else printf '%s\n' 'null' > "$original"; fi
  cp "$original" "$state/proxy"
  run
  report "$corrupt saved config is backed up and healed" proxy_invalid_healed
done
printf '%s\n' '{"auths":{"keep":{"user":"alice","password":"secret"}},"servers":{"old:1080":"keep"}}' > "$original"
cp "$original" "$state/proxy"; rm -f "$argv"
proxy_fail=yes; run
report 'partial failed import preserves live config' proxy_failure
cp "$original" "$state/proxy"; rm -f "$argv"
proxy_fail=silent; run
report 'silent zero-exit write failure preserves live config' proxy_failure
proxy_fail=no
for input in empty comments; do
  cp "$original" "$state/proxy"; rm -f "$argv"
  if [ "$input" = empty ]; then : > "$tmp/app/proxy.txt"; else printf '%s\n' '# comment only' '  # another' > "$tmp/app/proxy.txt"; fi
  run
  report "$input proxy input preserves live config" proxy_failure
done
printf '%s\n' 'new:1080' > "$tmp/app/proxy.txt"
cp "$original" "$state/proxy"; rm -f "$argv"
proxy_fail=no; expect_auth=yes; run
report 'successful import replaces servers and retains auth' proxy_success
rm -f "$state/proxy" "$argv"
expect_auth=no; run
report 'first import initializes config and installs proxies' proxy_success

# Fetch failures must not execute an error response as a successful checker script.
function_set=common; action=ip; home="$tmp/ip/home"; state="$home/.urnetwork"
mkdir -p "$state"; out="$tmp/ip/out"; argv="$tmp/ip/argv"; curl_log="$tmp/ip/curl-log"
curl_mode=fetch-fail; run; report 'IP-checker fetch failure is reported and body is not run' fetch_failure
curl_mode=script-fail; rm -f "$curl_log.executed"; run; report 'IP-checker script failure is reported' script_failure
curl_mode=success; rm -f "$curl_log.executed"; run; report 'IP-checker success is reported' script_success

# The common login loop and standalone Pelican login must both redact echoed secrets.
for function_set in common pelican; do
  home="$tmp/login-$function_set/home"; mkdir -p "$home/.urnetwork"
  out="$tmp/login-$function_set/out"; argv="$tmp/login-$function_set/argv"; curl_log="$tmp/login-$function_set/curl-log"
  if [ "$function_set" = common ]; then action=common-login; else action=pelican-login; fi
  run; report "$function_set DEBUG output hides credentials" login_safe
  auth_echo_mode=clean
  for empty_field in user password both; do
    case "$empty_field" in
      user) user_auth=''; password='fake-password-456' ;;
      password) user_auth='fake-user-123'; password='' ;;
      both) user_auth=''; password='' ;;
    esac
    run; report "$function_set DEBUG shows diagnostics with empty $empty_field" login_diagnostic
  done
  unset user_auth password auth_echo_mode
done

# The nightly bootstrap must download the binary before the proxy import runs it,
# and a failed import must not abort startup. The function-level cases above cannot
# see either, because they run func_check_proxy outside the real sequence.
bootstrap_order() {
  block=$(sed -n '/^      nightly)/,/^        ;;/p' "$HERE/provider_common.sh")
  update_line=$(printf '%s\n' "$block" | grep -n 'func_check_update' | head -1 | cut -d: -f1)
  proxy_line=$(printf '%s\n' "$block" | grep -n 'func_check_proxy' | head -1 | cut -d: -f1)
  [ -n "$update_line" ] && [ -n "$proxy_line" ] && [ "$update_line" -lt "$proxy_line" ]
}
report 'nightly downloads the binary before importing proxies' bootstrap_order

bootstrap_proxy_nonfatal() {
  block=$(sed -n '/^      nightly)/,/^        ;;/p' "$HERE/provider_common.sh")
  printf '%s\n' "$block" | grep -q 'func_check_proxy || true'
}
report 'a failed proxy import does not abort nightly startup' bootstrap_proxy_nonfatal

printf 'Results: %s passed, %s failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
