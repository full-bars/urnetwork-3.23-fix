#!/usr/bin/env bash
# Offline state-dir tests: stage entrypoint targets, source real resolvers and stub the provider. No Docker or network.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="${REPO_DIR:-$(cd "$HERE/../.." && pwd)}"
tmp="$(mktemp -d)" || exit 1
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/app" "$tmp/tmpfiles"

# Keep the actual entrypoint logic; redirect only its /app exec targets to stubs.
python3 - "$REPO/docker/scripts/entrypoint.sh" "$tmp/entrypoint.sh" <<'PY'
import pathlib, sys
source = pathlib.Path(sys.argv[1]).read_text()
if source.count('exec /app/') != 6:
    raise SystemExit('unexpected entrypoint exec targets')
pathlib.Path(sys.argv[2]).write_text(source.replace('exec /app/', 'exec "$STUB_APP"/'))
PY
if [ "$?" -ne 0 ]; then echo 'FAIL: could not stage entrypoint'; exit 1; fi
for name in start_stable start_jwt start_nightly pelican_panel; do
  printf '%s\n' '#!/bin/sh' 'printf "%s\n" "${URNETWORK_STATE_DIR:-}" "${URNETWORK_STATE_DIR_LOGGED:-}" > "$STUB_LOG"' > "$tmp/app/$name.sh"
  chmod +x "$tmp/app/$name.sh"
done

pass=0
fail=0
report() {
  case_name=$1
  check=$2
  if "$check"; then
    pass=$((pass + 1)); printf 'PASS: %s\n' "$case_name"
  else
    fail=$((fail + 1)); printf 'FAIL: %s (exit %s)\n' "$case_name" "$rc"
    [ ! -f "$out" ] || rg -n '.' "$out" || :
  fi
}
run_entrypoint() {
  name=$1
  cwd=$2
  shift 2
  out="$tmp/$name/out"
  dispatch="$tmp/$name/dispatch"
  mkdir -p "$tmp/$name"
  ( cd "$cwd" && env -i PATH="$PATH" HOME="$home" STUB_APP="$tmp/app" STUB_LOG="$dispatch" \
      "$@" sh "$tmp/entrypoint.sh" > "$out" 2>&1 )
  rc=$?
}
entrypoint_handoff_ok() {
  [ "$rc" -eq 0 ] && rg -Fxq "$expected_state" "$dispatch" && rg -Fxq '1' "$dispatch" \
    && [ "$(rg -c 'State directory:' "$out")" = 1 ] \
    && rg -Fq "State directory: $expected_state" "$out"
}
entrypoint_rejects_unwritable_path() {
  [ "$rc" -ne 0 ] && [ ! -e "$dispatch" ] && rg -Fq 'Cannot create state directory' "$out"
}

# Absolute override with spaces must be resolved before auth and passed to startup.
home="$tmp/override-home"; expected_state="$tmp/custom state"; mkdir -p "$expected_state" "$home"
printf '%s\n' fake-session > "$expected_state/jwt"
run_entrypoint override "$tmp" AUTH=authcode URNETWORK_STATE_DIR="$expected_state"
report 'entrypoint reuses and exports an override session path' entrypoint_handoff_ok

# A relative override is anchored to the process working directory and canonicalized.
relative_root="$tmp/relative-root"; home="$tmp/relative-home"; expected_state="$relative_root/relative state"
mkdir -p "$relative_root" "$expected_state" "$home"
printf '%s\n' fake-session > "$expected_state/jwt"
run_entrypoint relative "$relative_root" AUTH=authcode URNETWORK_STATE_DIR='relative state'
report 'entrypoint resolves relative state paths with spaces' entrypoint_handoff_ok

# The default remains HOME/.urnetwork and is exported even on first auth.
home="$tmp/default-home"; expected_state="$home/.urnetwork"; mkdir -p "$home"
run_entrypoint default "$tmp" AUTH=authcode URNETWORK_AUTH_CODE=fake-code
report 'entrypoint exports the default state path' entrypoint_handoff_ok

# A file blocking state-directory creation is fatal before startup dispatch.
home="$tmp/bad-home"; mkdir -p "$home"; printf blocker > "$tmp/not-a-directory"
run_entrypoint invalid "$tmp" AUTH=authcode URNETWORK_AUTH_CODE=fake-code URNETWORK_STATE_DIR="$tmp/not-a-directory/child"
report 'entrypoint rejects an uncreatable state path' entrypoint_rejects_unwritable_path

# Source the actual provider_common resolver and startup loops with a stub binary.
common_home="$tmp/common-home"; common_state="$tmp/common custom state"; common_app="$tmp/common-app"
mkdir -p "$common_home" "$common_app" "$common_state"
cat > "$common_app/urnetwork_amd64_stable" <<'SH'
#!/bin/sh
case "$1" in
  --version) printf '%s\n' state-dir-stub ;;
  auth)
    mkdir -p "$URNETWORK_STATE_DIR"
    printf '%s\n' fake-jwt > "$URNETWORK_STATE_DIR/jwt"
    printf '%s\n' 'Jwt written to state-dir-stub'
    ;;
  *) exit 0 ;;
esac
SH
chmod +x "$common_app/urnetwork_amd64_stable"
common_out="$tmp/common.out"; common_result="$tmp/common.result"; flags_log="$tmp/provider-flags.log"
env -i PATH="$PATH" HOME="$common_home" URNETWORK_STATE_DIR="$common_state" \
  COMMON="$HERE/provider_common.sh" SCRIPT_DIR="$HERE" PROVIDER_APP="$common_app" \
  COMMON_RESULT="$common_result" FLAGS_LOG="$flags_log" \
  sh -c '
    cd "$SCRIPT_DIR" || exit 90
    PROVIDER_MODE=stable
    . "$COMMON"
    APP_DIR="$PROVIDER_APP"
    A_SYS_ARCH=amd64
    PROVIDER_FLAVOR=stable
    URNETWORK_AUTH_CODE=fake-code
    provider_env_flags() { printf "%s\n" "$1" >> "$FLAGS_LOG"; }
    resolve_provider_env_args() { PROVIDER_ENV_ARGS="$(provider_env_flags "$STATE_DIR")"; }
    func_run_provider() { provider_runs=$((provider_runs + 1)); code=0; }
    pgrep() { return 1; }
    mkdir -p "$HOME/.urnetwork"
    for starter in func_start_provider_stable func_start_provider_nightly func_start_provider_jwt; do
      : > "$STATE_DIR/update-pending"
      : > "$HOME/.urnetwork/update-pending"
      provider_runs=0
      "$starter"
      [ "$provider_runs" -eq 2 ] || exit 91
      [ ! -e "$STATE_DIR/update-pending" ] || exit 92
      [ -f "$HOME/.urnetwork/update-pending" ] || exit 93
    done
    printf "%s\n%s\n%s\n" "$STATE_DIR" "$JWT_FILE" "$URNETWORK_STATE_DIR" > "$COMMON_RESULT"
  ' > "$common_out" 2>&1
rc=$?
common_state_ok() {
  [ "$rc" -eq 0 ] && rg -Fxq "$common_state" "$common_result" && rg -Fxq "$common_state/jwt" "$common_result" \
    && [ "$(rg -c -F "$common_state" "$flags_log")" = 6 ] \
    && [ -s "$common_state/jwt" ] && [ -f "$common_home/.urnetwork/update-pending" ] \
    && [ ! -e "$common_state/update-pending" ] && [ "$(rg -c 'State directory:' "$common_out")" = 1 ]
}
out="$common_out"
report 'shared provider paths and restart markers use the override in all modes' common_state_ok

# Run the real updater with its release fetch, archive download and digest checks stubbed.
update_work="$tmp/update"; mkdir -p "$update_work/source"
printf '%s\n' replacement-provider > "$update_work/source/provider"
tar -czf "$update_work/provider.tar.gz" -C "$update_work/source" provider
cat > "$update_work/release.json" <<'JSON'
{"tag_name":"v9.99.0","assets":[{"name":"urnetwork-provider-test-linux-amd64.tar.gz","browser_download_url":"https://example.invalid/download/v9.99.0/urnetwork-provider-test-linux-amd64.tar.gz","digest":"sha256:stub"}]}
JSON
rm -f "$common_home/.urnetwork/update-pending"
update_out="$tmp/update.out"; update_log="$tmp/update.log"
env -i PATH="$PATH" HOME="$common_home" URNETWORK_STATE_DIR="$common_state" \
  COMMON="$HERE/provider_common.sh" SCRIPT_DIR="$HERE" PROVIDER_APP="$common_app" \
  UPDATE_WORK="$update_work" RELEASE_FIXTURE="$update_work/release.json" \
  TEST_ARCHIVE="$update_work/provider.tar.gz" UPDATE_LOG="$update_log" \
  sh -c '
    cd "$SCRIPT_DIR" || exit 90
    PROVIDER_MODE=stable
    . "$COMMON"
    APP_DIR="$PROVIDER_APP"
    A_SYS_ARCH=amd64
    PROVIDER_FLAVOR=stable
    API_ROOT=https://example.invalid
    VERSION_FILE="$APP_DIR/version.txt"
    printf "%s\n" old > "$VERSION_FILE"
    upd_fetch_stable_release_json() { printf "%s\n" fetch >> "$UPDATE_LOG"; cat "$RELEASE_FIXTURE"; }
    upd_pick_arch_tarball_name() { printf "%s\n" urnetwork-provider-test-linux-amd64.tar.gz; }
    upd_pick_fat_tarball_name() { return 0; }
    upd_asset_digest_from_json() { printf "%s\n" stub-digest; }
    upd_verify_digest() { [ -s "$1" ]; }
    mktemp() { mkdir -p "$UPDATE_WORK/staging"; printf "%s\n" "$UPDATE_WORK/staging"; }
    curl() {
      output=
      while [ "$#" -gt 0 ]; do
        if [ "$1" = -o ]; then output=$2; shift 2; else shift; fi
      done
      printf "%s\n" download >> "$UPDATE_LOG"
      cp "$TEST_ARCHIVE" "$output"
    }
    pkill() { return 1; }
    func_check_update
  ' > "$update_out" 2>&1
rc=$?
update_marker_ok() {
  [ "$rc" -eq 0 ] && [ -f "$common_state/update-pending" ] \
    && [ ! -e "$common_home/.urnetwork/update-pending" ] \
    && [ "$(cat "$common_app/version.txt")" = 9.99.0 ] \
    && [ -s "$common_app/urnetwork_amd64_stable" ] \
    && [ "$(rg -c '.' "$update_log")" = 2 ]
}
out="$update_out"
report 'updater writes its restart marker under the resolved state directory' update_marker_ok

# Exercise Pelican's standalone resolver and login with the real function and a provider stub.
python3 - "$HERE/pelican_panel.sh" "$tmp/pelican-functions.sh" <<'PY'
import pathlib, re, sys
source = pathlib.Path(sys.argv[1]).read_text()
functions = []
for name in ('resolve_state_dir', 'func_do_login'):
    match = re.search(r'^' + re.escape(name) + r'\(\) \{.*?^\}\n', source, re.M | re.S)
    if not match:
        raise SystemExit('missing function ' + name)
    functions.append(match.group())
pathlib.Path(sys.argv[2]).write_text('\n'.join(functions))
PY
if [ "$?" -ne 0 ]; then echo 'FAIL: Pelican extraction'; exit 1; fi
panel_home="$tmp/panel-home"; panel_state="$tmp/panel custom state"; panel_app="$tmp/panel-app"
mkdir -p "$panel_home" "$panel_app" "$panel_state"
cat > "$panel_app/urnetwork_test_stable" <<'SH'
#!/bin/sh
case "$1" in
  auth)
    mkdir -p "$URNETWORK_STATE_DIR"
    printf '%s\n' pelican-jwt > "$URNETWORK_STATE_DIR/jwt"
    printf '%s\n' 'Jwt written to panel-state-stub'
    ;;
  *) exit 1 ;;
esac
SH
chmod +x "$panel_app/urnetwork_test_stable"
panel_out="$tmp/panel.out"; panel_result="$tmp/panel.result"
env -i PATH="$PATH" HOME="$panel_home" URNETWORK_STATE_DIR="$panel_state" \
  PANEL_FUNCTIONS="$tmp/pelican-functions.sh" PANEL_APP="$panel_app" PANEL_RESULT="$panel_result" \
  USER_AUTH=fake-user PASSWORD=fake-password sh -c '
    log() { printf "%s\n" "$*"; }
    func_sleep() { :; }
    func_exit_if_stopping() { :; }
    . "$PANEL_FUNCTIONS"
    APP_DIR="$PANEL_APP"
    A_SYS_ARCH=test
    resolve_state_dir
    JWT_FILE="$STATE_DIR/jwt"
    func_do_login
    printf "%s\n%s\n" "$STATE_DIR" "$JWT_FILE" > "$PANEL_RESULT"
  ' > "$panel_out" 2>&1
rc=$?
pelican_state_ok() {
  [ "$rc" -eq 0 ] && rg -Fxq "$panel_state" "$panel_result" && rg -Fxq "$panel_state/jwt" "$panel_result" \
    && [ -s "$panel_state/jwt" ] && [ ! -e "$panel_home/.urnetwork/jwt" ] \
    && [ "$(rg -c 'State directory:' "$panel_out")" = 1 ]
}
out="$panel_out"
report 'Pelican login reads and writes the resolved state directory' pelican_state_ok

printf 'Results: %s passed, %s failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
