#!/bin/bash
# test_jwt_local_validate.sh: validates local JWT payload decoding,
# exp and nbf clock skew (5 minutes), and malformed token rejection.
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

# shellcheck source=jwt_verify.sh
. "$REPO/docker/scripts/jwt_verify.sh"

# Helper to build synthetic JWT with base64url payload
make_jwt() {
    local payload="$1"
    local hdr="eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"
    local b64
    b64="$(printf '%s' "$payload" | base64 | tr -d '\r\n[:space:]' | tr '+/' '-_' | tr -d '=')"
    printf '%s.%s.synthetic_sig' "$hdr" "$b64"
}

now="$(date +%s)"

# 1. Valid token (exp 1h future, nbf 1h past)
make_jwt "{\"sub\":\"12345\",\"exp\":$((now + 3600)),\"nbf\":$((now - 3600))}" > "$tmp/valid.jwt"
t "valid JWT accepted" validate_jwt_file "$tmp/valid.jwt"

# 2. Expired token (exp 10m ago > 5m clock skew)
make_jwt "{\"sub\":\"12345\",\"exp\":$((now - 600))}" > "$tmp/expired.jwt"
t "expired JWT rejected" sh -c "! validate_jwt_file '$tmp/expired.jwt'"

# 3. NBF in future (nbf 10m future > 5m clock skew)
make_jwt "{\"sub\":\"12345\",\"nbf\":$((now + 600)),\"exp\":$((now + 3600))}" > "$tmp/nbf_future.jwt"
t "nbf-in-future JWT rejected" sh -c "! validate_jwt_file '$tmp/nbf_future.jwt'"

# 4. Within skew (exp 2m ago <= 5m clock skew)
make_jwt "{\"sub\":\"12345\",\"exp\":$((now - 120))}" > "$tmp/skew_exp.jwt"
t "within-skew expired (2m ago) accepted" validate_jwt_file "$tmp/skew_exp.jwt"

# 5. Within skew (nbf 2m future <= 5m clock skew)
make_jwt "{\"sub\":\"12345\",\"nbf\":$((now + 120)),\"exp\":$((now + 3600))}" > "$tmp/skew_nbf.jwt"
t "within-skew nbf (2m future) accepted" validate_jwt_file "$tmp/skew_nbf.jwt"

# 6. Malformed tests
: > "$tmp/empty.jwt"
t "malformed: empty file rejected" sh -c "! validate_jwt_file '$tmp/empty.jwt'"

printf "not-a-jwt-token" > "$tmp/garbage.jwt"
t "malformed: arbitrary text rejected" sh -c "! validate_jwt_file '$tmp/garbage.jwt'"

printf "part1.part2" > "$tmp/two_parts.jwt"
t "malformed: two parts rejected" sh -c "! validate_jwt_file '$tmp/two_parts.jwt'"

printf "p1.p2.p3.p4" > "$tmp/four_parts.jwt"
t "malformed: four parts rejected" sh -c "! validate_jwt_file '$tmp/four_parts.jwt'"

printf "eyJhbGciOiJIUzI1NiJ9.!!!invalid-base64!!!.sig" > "$tmp/bad_b64.jwt"
t "malformed: invalid base64 rejected" sh -c "! validate_jwt_file '$tmp/bad_b64.jwt'"

make_jwt "this is not json" > "$tmp/bad_json.jwt"
t "malformed: non-JSON payload rejected" sh -c "! validate_jwt_file '$tmp/bad_json.jwt'"

make_jwt "{\"exp\":\"not-a-number\"}" > "$tmp/bad_exp.jwt"
t "malformed: non-numeric exp rejected" sh -c "! validate_jwt_file '$tmp/bad_exp.jwt'"

make_jwt "{\"nbf\":\"not-a-number\"}" > "$tmp/bad_nbf.jwt"
t "malformed: non-numeric nbf rejected" sh -c "! validate_jwt_file '$tmp/bad_nbf.jwt'"

# 7. Entrypoint integration & validation tests
ep_out=""

# AUTH=userpass requires USER_AUTH and PASSWORD
ep_out="$(AUTH=userpass USER_AUTH="" PASSWORD="" HOME="$tmp/h1" sh "$REPO/docker/scripts/entrypoint.sh" 2>&1 || true)"
t "entrypoint: AUTH=userpass requires USER_AUTH and PASSWORD" grep -q "ERROR: AUTH=userpass requires USER_AUTH and PASSWORD to be set" <<<"$ep_out"

# AUTH=userpass with only USER_AUTH missing PASSWORD
ep_out="$(AUTH=userpass USER_AUTH="test@example.com" PASSWORD="" HOME="$tmp/h1" sh "$REPO/docker/scripts/entrypoint.sh" 2>&1 || true)"
t "entrypoint: AUTH=userpass names missing PASSWORD" grep -q "ERROR: AUTH=userpass requires PASSWORD to be set" <<<"$ep_out"

# AUTH=authcode requires auth code or stored JWT
ep_out="$(AUTH=authcode HOME="$tmp/h2" sh "$REPO/docker/scripts/entrypoint.sh" 2>&1 || true)"
t "entrypoint: AUTH=authcode requires AUTHCODE" grep -q "ERROR: AUTH=authcode requires AUTHCODE" <<<"$ep_out"

# AUTH=invalid rejected
ep_out="$(AUTH=invalid HOME="$tmp/h3" sh "$REPO/docker/scripts/entrypoint.sh" 2>&1 || true)"
t "entrypoint: invalid AUTH rejected" grep -q "ERROR: Invalid AUTH 'invalid'" <<<"$ep_out"

# BUILD without AUTH logs deprecation hint
ep_out="$(BUILD=stable USER_AUTH="u" PASSWORD="p" HOME="$tmp/h4" sh "$REPO/docker/scripts/entrypoint.sh" 2>&1 || true)"
t "entrypoint: BUILD without AUTH logs deprecation hint" grep -q "BUILD is deprecated for auth selection, use AUTH=userpass|authcode" <<<"$ep_out"

# Conflicting BUILD and AUTH logs warning and BUILD wins
ep_out="$(BUILD=stable AUTH=authcode URNETWORK_AUTH_CODE="code" HOME="$tmp/h5" sh "$REPO/docker/scripts/entrypoint.sh" 2>&1 || true)"
t "entrypoint: conflicting AUTH and BUILD logs warning" grep -q "WARNING: Conflicting AUTH='authcode' and BUILD='stable' specified" <<<"$ep_out"

# Expired token in volume without auth code is removed and exits with error
mkdir -p "$tmp/h6/.urnetwork"
make_jwt "{\"sub\":\"123\",\"exp\":$((now - 600))}" > "$tmp/h6/.urnetwork/jwt"
ep_out="$(AUTH=authcode HOME="$tmp/h6" sh "$REPO/docker/scripts/entrypoint.sh" 2>&1 || true)"
t "entrypoint: expired token logged and removed" grep -q "Stored JWT expired or invalid - removing stale token" <<<"$ep_out"
t "entrypoint: expired token file deleted" test ! -f "$tmp/h6/.urnetwork/jwt"
t "entrypoint: expired token without code exits with error" grep -q "ERROR: AUTH=authcode requires AUTHCODE" <<<"$ep_out"

# Expired token in volume with AUTHCODE available is removed and re-authenticates
mkdir -p "$tmp/h7/.urnetwork"
make_jwt "{\"sub\":\"123\",\"exp\":$((now - 600))}" > "$tmp/h7/.urnetwork/jwt"
ep_out="$(AUTH=authcode AUTHCODE="new_code" HOME="$tmp/h7" sh "$REPO/docker/scripts/entrypoint.sh" 2>&1 || true)"
t "entrypoint: expired token removed when AUTHCODE present" test ! -f "$tmp/h7/.urnetwork/jwt"
t "entrypoint: proceeds to re-auth with AUTHCODE" grep -q "Authentication requested via environment variable" <<<"$ep_out"

# Startup safety messages: wallet reminder and volume warning
t "entrypoint: logs wallet reminder" grep -q "INFO: A payout wallet and consent must be set" <<<"$ep_out"
t "entrypoint: logs volume warning for resolved state directory" rg -Fq "WARNING: Without a persistent volume mounted at $tmp/h7/.urnetwork" <<<"$ep_out"

echo ""
echo "Results: $pass passed, $fail failed"
exit $((fail > 0))
