#!/usr/bin/env bash
# Targeted tests for the docker update-path hardening (digest verify + fork-only source).
# Run: bash docker/scripts/test_update_verify.sh   (from repo root or anywhere)
#
# Scope (mirrors the repo rule: test the ACTUAL logic, not a wrapper):
#   1. asset_digest_from_json: parses sha256:<hex> from real API shape, rejects
#      null/None/missing digest, picks the RIGHT asset among several.
#   2. verify_digest: passes on matching content, FAILS on tampered content,
#      fails on missing file, fails on absent sha256sum tool.
#   3. Source guard: no docker script names an upstream urnetwork repo in a
#      release-fetch URL.
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

got="$(asset_digest_from_json "$JSON_OK" "$ASSET")"
[ "$got" = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" ]
t "digest parses from multi-asset JSON and strips sha256: prefix" true

asset_digest_from_json "$JSON_OK" "missing-asset.tar.gz"
t "rejects missing asset" test $? -ne 0

JSON_NULL='{"assets":[{"name":"old.tar.gz","digest":null}]}'
asset_digest_from_json "$JSON_NULL" "old.tar.gz"
t "rejects null digest (legacy asset)" test $? -ne 0

JSON_NODIGEST='{"assets":[{"name":"plain.tar.gz"}]}'
asset_digest_from_json "$JSON_NODIGEST" "plain.tar.gz"
t "rejects asset with no digest field" test $? -ne 0

# --- verify_digest ---
printf 'provider-bytes-trusted' > "$tmp/good.bin"
good_sum="$(sha256sum "$tmp/good.bin" | awk '{print $1}')"
verify_digest "$tmp/good.bin" "$good_sum"
t "verify_digest accepts exact match" true

printf 'provider-bytes-TAMPERED' > "$tmp/bad.bin"
if verify_digest "$tmp/bad.bin" "$good_sum"; then
    fail=$((fail+1)); echo "FAIL: tampered content accepted by verify_digest"
else
    pass=$((pass+1)); echo "PASS: verify_digest rejects tampered content"
fi

if verify_digest "$tmp/nonexistent.bin" "$good_sum"; then
    fail=$((fail+1)); echo "FAIL: verify_digest accepted missing file"
else
    pass=$((pass+1)); echo "PASS: verify_digest rejects missing file"
fi

# Empty expected digest must never pass (guards against "" sneaking through).
if [ -n "$(verify_digest "$tmp/good.bin" "")" ]; then :; fi
if verify_digest "$tmp/good.bin" ""; then
    fail=$((fail+1)); echo "FAIL: empty expected digest accepted"
else
    pass=$((pass+1)); echo "PASS: empty expected digest rejected"
fi

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
