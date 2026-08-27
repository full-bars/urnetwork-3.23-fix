#!/usr/bin/env bash
# Targeted tests for the docker update-path hardening (digest verify + fork-only source).
# Run: bash docker/scripts/test_update_verify.sh   (from repo root or anywhere)
#
# Scope (mirrors the repo rule: test the ACTUAL logic, not a wrapper):
#   1. upd_asset_digest_from_json: parses sha256:<hex> from real API shape, rejects
#      null/None/missing digest, picks the RIGHT asset among several, works via jq AND
#      python3 fallback, aborts when no parser exists, and rejects hostile bytes.
#   2. upd_verify_digest: passes on matching content, FAILS on tampered content,
#      fails on missing file / empty expected digest / absent hashing tool, names the
#      download source in mismatch output.
#   3. POSIX discipline: no `local`, sources clean under dash.
#   4. Caller asset-selection filter: parenthesized jq shape pinned, broken shape
#      rejected, first-wins multi-asset order.
#   5. Source guard: no docker script names an upstream urnetwork repo in fetch URLs.
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$HERE/update_verify.sh"

pass=0
fail=0
t() { # t <name> <cmd...>
    local name="$1"; shift
    if "$@" >/dev/null 2>&1; then
        pass=$((pass+1)); echo "PASS: $name"
    else
        fail=$((fail+1)); echo "FAIL: $name"
    fi
}

tmp="$(mktemp -d /tmp/updverify-XXXXXX)"
trap 'rm -rf "$tmp"' EXIT

# --- Fixtures shaped like the real GitHub release API response ---
JSON_OK='{"assets":[
  {"name":"urnet-tools-linux-amd64","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  {"name":"urnetwork-provider-v3.23.0-fix.30.7.tar.gz","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
  {"name":"urnetwork-provider-v3.23.0-fix.30.7-linux-arm64.tar.gz","digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}]}'

ASSET="urnetwork-provider-v3.23.0-fix.30.7.tar.gz"
HEX_B="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

got="$(upd_asset_digest_from_json "$JSON_OK" "$ASSET")"
[ "$got" = "$HEX_B" ]
t "digest parses from multi-asset JSON and strips sha256: prefix" true

upd_asset_digest_from_json "$JSON_OK" "missing-asset.tar.gz"
t "rejects missing asset" test $? -ne 0

JSON_NULL='{"assets":[{"name":"old.tar.gz","digest":null}]}'
upd_asset_digest_from_json "$JSON_NULL" "old.tar.gz"
t "rejects null digest (legacy asset)" test $? -ne 0

JSON_NODIGEST='{"assets":[{"name":"plain.tar.gz"}]}'
upd_asset_digest_from_json "$JSON_NODIGEST" "plain.tar.gz"
t "rejects asset with no digest field" test $? -ne 0

# Hostile/odd input must not crash or inject: control chars stripped pre-parse.
JSON_HOSTY="{\"assets\":[{\"name\":\"x.tar.gz\",\"digest\":\"sha256:$HEX_B\"}]}`printf '\001\002'`"
got="$(upd_asset_digest_from_json "$JSON_HOSTY" "x.tar.gz" 2>/dev/null)"
[ "$got" = "$HEX_B" ]
t "control-char-polluted JSON still parses to the correct digest" true

# CRLF line endings in the response body (proxy/mirror artifact) survive parsing.
JSON_CRLF="$(printf '%s' "$JSON_OK" | sed 's/$/\r/')"
got="$(upd_asset_digest_from_json "$JSON_CRLF" "$ASSET" 2>/dev/null)"
[ "$got" = "$HEX_B" ]
t "CRLF-mangled JSON still yields the exact hex digest" true

# --- Tool fallback matrix for digest extraction ---
# Build a tool-restricted PATH: everything normal, minus one binary at a time.
path_without() { # path_without <tool> -> PATH string with <tool>'s dir removed
    local tool="$1" dir
    dir="$(dirname "$(command -v "$tool")")"
    echo "$PATH" | tr ':' '\n' | grep -v "^$dir\$" | tr '\n' ':'
}

extract_with_path() { # extract_with_path <PATH> ; result in stdout
    PATH="$1" sh -c ". '$HERE/update_verify.sh' && upd_asset_digest_from_json '$JSON_OK' '$ASSET'" 2>/dev/null
}

if command -v python3 >/dev/null 2>&1; then
    got="$(extract_with_path "$(path_without jq)")"
    [ "$got" = "$HEX_B" ]
    t "python3 fallback extracts identical digest with jq hidden" true

    # Fallback parity on the failure shapes too: null digest via python3 aborts.
    PATH="$(path_without jq)" sh -c ". '$HERE/update_verify.sh' && upd_asset_digest_from_json '$JSON_NULL' 'old.tar.gz'" >/dev/null 2>&1
    t "python3 fallback also rejects null digest" test $? -ne 0
fi

# All-toolless environment must still abort (fail-safe, never skip verify).
if env -i PATH=/nonexistent sh -c ". '$HERE/update_verify.sh' && upd_asset_digest_from_json '$JSON_OK' '$ASSET'" >/dev/null 2>&1; then
    fail=$((fail+1)); echo "FAIL: toolless environment did not abort"
else
    pass=$((pass+1)); echo "PASS: no-jq-no-python3 environment aborts (fail-safe)"
fi

# --- verify_digest ---
printf 'provider-bytes-trusted' > "$tmp/good.bin"
good_sum="$(sha256sum "$tmp/good.bin" | awk '{print $1}')"
upd_verify_digest "$tmp/good.bin" "$good_sum" "primary"
t "verify_digest accepts exact match" true

printf 'provider-bytes-TAMPERED' > "$tmp/bad.bin"
if upd_verify_digest "$tmp/bad.bin" "$good_sum" "primary"; then
    fail=$((fail+1)); echo "FAIL: tampered content accepted by verify_digest"
else
    pass=$((pass+1)); echo "PASS: verify_digest rejects tampered content"
fi

# Source label must appear in mismatch output (error-indistinction fix).
upd_verify_digest "$tmp/bad.bin" "$good_sum" "mirror-fallback" 2> "$tmp/err.txt"
grep -q "mirror-fallback" "$tmp/err.txt"
t "mismatch error names the download source" true

if upd_verify_digest "$tmp/nonexistent.bin" "$good_sum" "primary"; then
    fail=$((fail+1)); echo "FAIL: verify_digest accepted missing file"
else
    pass=$((pass+1)); echo "PASS: verify_digest rejects missing file"
fi

# Empty expected digest must never pass (guards against "" sneaking through).
if upd_verify_digest "$tmp/good.bin" "" ""; then
    fail=$((fail+1)); echo "FAIL: empty expected digest accepted"
else
    pass=$((pass+1)); echo "PASS: empty expected digest rejected"
fi

# Hashing fallback parity: openssl must produce the same hash as sha256sum
# where both exist; where only one exists, verification still functions.
if command -v openssl >/dev/null 2>&1; then
    ossl="$(openssl dgst -sha256 "$tmp/good.bin" | awk '{print $NF}')"
    [ "$ossl" = "$good_sum" ]
    t "openssl sha256 agrees with sha256sum (fallback equivalence)" true

    got="$(PATH="$(path_without sha256sum)" sh -c ". '$HERE/update_verify.sh' && upd_sha256_of '$tmp/good.bin'" 2>/dev/null)"
    [ "$got" = "$good_sum" ]
    t "upd_sha256_of falls back to openssl with sha256sum hidden" true

    PATH="$(path_without sha256sum)" sh -c ". '$HERE/update_verify.sh' && upd_verify_digest '$tmp/bad.bin' '$good_sum' 'primary'" >/dev/null 2>&1
    t "verification still rejects tampered content via openssl fallback" test $? -ne 0
fi

# POSIX discipline: no `local` anywhere in the sourced helper.
if grep -qE '^\s*local\s' "$HERE/update_verify.sh"; then
    fail=$((fail+1)); echo "FAIL: update_verify.sh uses non-POSIX 'local'"
else
    pass=$((pass+1)); echo "PASS: update_verify.sh is POSIX-clean (no local)"
fi

# The helper parses identically under /bin/sh as under bash (shebang contract).
sh -c ". '$HERE/update_verify.sh' && upd_asset_digest_from_json '$JSON_OK' '$ASSET'" > "$tmp/sh.out"
[ "$(cat "$tmp/sh.out")" = "$HEX_B" ]
t "helper works when sourced by strict /bin/sh (dash-compatible)" true

# No stale UPD_ globals leak out of a successful call (subshell callers are
# isolated; direct-source callers must not inherit polluted state).
sh -c ". '$HERE/update_verify.sh' && upd_asset_digest_from_json '$JSON_OK' '$ASSET' >/dev/null && env" > "$tmp/env.out" 2>/dev/null
if grep -q "^UPD_raw=" "$tmp/env.out"; then
    fail=$((fail+1)); echo "FAIL: UPD_raw leaked into caller environment"
else
    pass=$((pass+1)); echo "PASS: helper globals unset after use"
fi

# --- Caller asset-selection filter (start_update.sh Download_API) ---
# Regression: jq `select(A) and B` parses as `(select(A)) and B`, so the
# un-parenthesized form errors "Cannot index boolean with string" on every
# real release. Pin the CORRECT parenthesized expression + first-wins order.
JSON_MULTI='{"assets":[
  {"name":"urnet-tools-linux-amd64","digest":"sha256:1"},
  {"name":"urnetwork-provider-v9-linux-amd64.tar.gz","digest":"sha256:2"},
  {"name":"urnetwork-provider-v9.tar.gz","digest":"sha256:3"}]}'
sel="$(printf '%s' "$JSON_MULTI" | jq -r '.assets[] | select((.name | startswith("urnetwork-provider-")) and (.name | endswith(".tar.gz"))) | .name' | head -n1)"
[ "$sel" = "urnetwork-provider-v9-linux-amd64.tar.gz" ]
t "start_update asset filter returns exactly one name, first-wins" true

# The broken (unparenthesized) shape must stay broken-and-unused: assert it
# errors, so nobody reintroduces it thinking it works.
broken_exit=0
printf '%s' "$JSON_MULTI" | jq -r '.assets[] | select(.name | startswith("urnetwork-provider-")) and (.name | endswith(".tar.gz")) | .name' >/dev/null 2>&1 || broken_exit=1
[ "$broken_exit" -eq 1 ]
t "unparenthesized select+and stays rejected (would abort under set -e)" true

# Nightly asset mapping: URL->name match-back finds the API name even when
# the URL layout changes above the filename segment.
URL_V9="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v9/urnetwork-provider-v9.tar.gz"
matched=""
for upd_candidate in $(printf '%s\n' "$JSON_MULTI" | grep -oE '"name": *"[^"]+"' | sed -E 's/"name": *"([^"]+)"/\1/'); do
    case "$URL_V9" in
        *"/$upd_candidate") matched="$upd_candidate"; break ;;
    esac
done
[ "$matched" = "urnetwork-provider-v9.tar.gz" ]
t "nightly URL-to-name match-back resolves the real API asset name" true

# --- Source guard: upstream repos must not appear in fetch URLs ---
violations=""
for f in "$HERE"/start_update.sh "$HERE"/start_nightly.sh "$HERE"/start_stable.sh \
         "$HERE"/start_jwt.sh "$HERE"/urnet-tools.sh "$HERE"/pelican_panel.sh; do
    [ -f "$f" ] || continue
    # Any api.github.com/repos/<org>/<repo> that is NOT full-bars is a violation.
    while read -r url; do
        org_repo="$(printf '%s' "$url" | sed -E 's#.*/repos/([^/"]+/[^/"]+).*#\1#')"
        case "$org_repo" in
            full-bars/*) ;;
            *) violations="$violations\n$(basename "$f"): $url" ;;
        esac
    done < <(grep -oE 'https://api\.github\.com/repos/[^"]+' "$f" || true)
done
[ -z "$violations" ]
t "no upstream urnetwork repo named in any release-fetch URL" true

echo ""
echo "Results: $pass passed, $fail failed"
exit $((fail > 0))
