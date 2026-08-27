#!/bin/bash
# Local PELICAN boot smoke: verifies pelican_panel.sh under PELICAN=yes:
#   (1) empty credentials fail fast with a clear message,
#   (2) BUILD=jwt routes to `auth-provide <code>` on the provider binary,
#   (3) stable mode routes through user/pass auth then provide.
#
# Mechanics: a fake provider binary is staged in a temp APP_DIR; copies of
# pelican_panel.sh are sed-patched to point APP_DIR there and shorten the
# auth/retry sleeps so the whole suite stays under ~30s. The unpatched
# script still covers test (1) because it fails before any exec.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
wt="${PELICAN_SMOKE_WT:-$(cd "$HERE/../.." && pwd)}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/app" "$tmp/home/.urnetwork"
cat > "$tmp/app/urnetwork_amd64_stable" <<EOF
#!/bin/sh
echo "FAKE-CALL: \$*" >> "$tmp/calls.log"
# Emulate the real provider: write the JWT to \$HOME/.urnetwork/jwt.
mkdir -p "\$HOME/.urnetwork"; : > "\$HOME/.urnetwork/jwt"; echo "fake-jwt" > "\$HOME/.urnetwork/jwt"
case "\$1" in
  --version) echo "v.test"; exit 0 ;;
  auth-provide) echo "Jwt written to \$HOME/.urnetwork/jwt"; exit 0 ;;
  auth) echo "Jwt written to \$HOME/.urnetwork/jwt"; exit 0 ;;
  provide) echo "Provider"; sleep 30; exit 0 ;;
esac
exit 0
EOF
chmod +x "$tmp/app/urnetwork_amd64_stable"

for b in jwt stable; do
  sed -e "s|^APP_DIR=\"/app\"$|APP_DIR=\"$tmp/app\"|" \
      -e 's/sleep 15$/sleep 0.05/' \
      -e 's/^\( *\)sleep 5$/\1sleep 0.05/' \
      -e 's/sleep 60$/sleep 0.05/' \
      -e 's/sleep 300$/sleep 0.05/' \
      "$wt/docker/scripts/pelican_panel.sh" > "$tmp/panel_$b.sh"
done

pass=0; fail=0
t() { local name="$1"; shift
    if "$@" >/dev/null 2>&1; then pass=$((pass+1)); echo "PASS: $name"; return 0
    else fail=$((fail+1)); echo "FAIL: $name"; return 1; fi
}
base_env="export PELICAN=yes DEBUG=false ENABLE_VNSTAT=false ENABLE_IP_CHECKER=false HOME=$tmp/home"

# --- (1) missing credentials (unpatched script fails before any exec) ---
OUT="$(PELICAN=yes BUILD=stable USER_AUTH='' PASSWORD='' timeout 10 \
      sh "$wt/docker/scripts/pelican_panel.sh" 2>&1 </dev/null)"
rc=$?
t "empty creds exit non-zero"      test "$rc" -ne 0
t "error names USER_AUTH/PASSWORD" grep -q "USER_AUTH or PASSWORD not set" <<<"$OUT"

# --- (2) jwt mode ---
rm -f "$tmp/calls.log"
( eval "$base_env"
  export APP_DIR="$tmp/app" BUILD=jwt AUTHCODE="code-test"
  cd "$tmp/app"; timeout 8 sh "$tmp/panel_jwt.sh" ) >"$tmp/jwt.log" 2>&1 </dev/null &
JP=$!; sleep 4; kill $JP 2>/dev/null; wait $JP 2>/dev/null
JWTLOG="$(cat "$tmp/jwt.log")"; CALLS="$(cat "$tmp/calls.log" 2>/dev/null)"

t "jwt mode calls auth-provide with code" grep -q "FAKE-CALL: auth-provide code-test" <<<"${JWTLOG}${CALLS}"
t "jwt mode logged running build version" grep -q "Running UrNetwork build vv.test"   <<<"$JWTLOG"

# --- (3) stable/user-pass mode ---
rm -f "$tmp/calls.log"
( eval "$base_env"
  export APP_DIR="$tmp/app" BUILD=stable USER_AUTH=test@example.net PASSWORD=pw-test
  cd "$tmp/app"; timeout 8 sh "$tmp/panel_stable.sh" ) >"$tmp/stable.log" 2>&1 </dev/null &
SP=$!; sleep 5; kill $SP 2>/dev/null; wait $SP 2>/dev/null
SLOG="$(cat "$tmp/stable.log")"; SCALLS="$(cat "$tmp/calls.log" 2>/dev/null)"

t "stable mode called auth with creds" grep -q -- "--user_auth=test@example.net" <<<"${SLOG}${SCALLS}"
t "stable mode reached provide"        grep -q "FAKE-CALL: provide"              <<<"${SLOG}${SCALLS}"
if ! grep -q "FAKE-CALL: provide" <<<"${SLOG}${SCALLS}"; then
    echo "--- stable-mode artifacts (for triage):"
    tail -8 "$tmp/stable.log"
    echo "--- calls:"; cat "$tmp/calls.log" 2>/dev/null || echo "(none)"
fi

echo
echo "Results: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
