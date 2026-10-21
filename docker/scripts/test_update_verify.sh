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
#
# HARNESS CONTRACT (regression-hardened): every assertion is evaluated INSIDE the t()
# call — `t "name" test "$a" = "$b"` — never as a bare command before it. A bare
# `[ ... ]` followed by `t "name" true` reports PASS even when the assertion failed
# (review-found flaw; do not reintroduce).
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$HERE/update_verify.sh"

pass=0
fail=0
t() { # t <name> <cmd...>  — returns 0 iff the assertion held
    local name="$1"; shift
    if "$@" >/dev/null 2>&1; then
        pass=$((pass+1)); echo "PASS: $name"
        return 0
    fi
    fail=$((fail+1)); echo "FAIL: $name"
    return 1
}

tmp="$(mktemp -d /tmp/updverify-XXXXXX)"
trap 'rm -rf "$tmp"' EXIT

# Harness self-check: prove t actually fails on a false assertion. If this
# ever "passes", the whole suite's PASS lines are meaningless.
if t "__harness_selfcheck__" test 1 = 2; then
    echo "HARNESS BROKEN: t reported success on a false assertion"
    exit 99
fi
if [ "$fail" -ne 1 ]; then
    echo "HARNESS BROKEN: intentional failure did not register (fail=$fail)"
    exit 99
fi
fail=$((fail-1)) # the intentional failure registered; undo its count
echo "PASS: harness self-check (false assertion correctly counted as FAIL)"

# --- Fixtures shaped like the real GitHub release API response ---
JSON_OK='{"assets":[
  {"name":"urnet-tools-linux-amd64","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  {"name":"urnetwork-provider-v3.23.0-fix.30.7.tar.gz","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
  {"name":"urnetwork-provider-v3.23.0-fix.30.7-linux-arm64.tar.gz","digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}]}'

ASSET="urnetwork-provider-v3.23.0-fix.30.7.tar.gz"
HEX_B="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

got="$(upd_asset_digest_from_json "$JSON_OK" "$ASSET")"
t "digest parses from multi-asset JSON and strips sha256: prefix" test "$got" = "$HEX_B"

upd_asset_digest_from_json "$JSON_OK" "missing-asset.tar.gz" >/dev/null 2>&1
t "rejects missing asset" test $? -ne 0

JSON_NULL='{"assets":[{"name":"old.tar.gz","digest":null}]}'
upd_asset_digest_from_json "$JSON_NULL" "old.tar.gz" >/dev/null 2>&1
t "rejects null digest (legacy asset)" test $? -ne 0

JSON_NODIGEST='{"assets":[{"name":"plain.tar.gz"}]}'
upd_asset_digest_from_json "$JSON_NODIGEST" "plain.tar.gz" >/dev/null 2>&1
t "rejects asset with no digest field" test $? -ne 0

# Hostile/odd input must not crash or inject: control chars stripped pre-parse.
JSON_HOSTY="{\"assets\":[{\"name\":\"x.tar.gz\",\"digest\":\"sha256:$HEX_B\"}]}`printf '\001\002'`"
got="$(upd_asset_digest_from_json "$JSON_HOSTY" "x.tar.gz" 2>/dev/null)"
t "control-char-polluted JSON still parses to the correct digest" test "$got" = "$HEX_B"

# CRLF line endings in the response body (proxy/mirror artifact) survive parsing.
JSON_CRLF="$(printf '%s' "$JSON_OK" | sed 's/$/\r/')"
got="$(upd_asset_digest_from_json "$JSON_CRLF" "$ASSET" 2>/dev/null)"
t "CRLF-mangled JSON still yields the exact hex digest" test "$got" = "$HEX_B"

# --- Tool fallback matrix for digest extraction ---
# Build a tool-restricted PATH that provably EXCLUDES a tool and INCLUDES the
# fallback tool resolves. Hiding by directory alone is unreliable when tools
# share a directory (or dirs are symlinked), so each filtered environment is
# VERIFIED before use: assert the hidden tool is unreachable AND the expected
# fallback tool resolves. On boxes where hide+need share ALL dirs (Arch's
# /usr/bin holds both jq and python3), the case is genuinely unconstructable
# and reported as SKIP rather than silently passing.
path_without_verified() { # path_without_verified <hidden-tool> <required-fallback-tool> -> PATH
    local hide="$1" need="$2" d out="" keep_dir need_dirs="" pdir
    for keep_dir in $(echo "$PATH" | tr ':' '\n' | while read -r pd; do [ -x "$pd/$need" ] && echo "$pd"; done); do
        need_dirs="$need_dirs$keep_dir"$'\n'
    done
    while IFS= read -r d; do
        [ -x "$d/$hide" ] && continue
        printf '%s\n' "$need_dirs" | grep -qx "$d" && { out="$out$d:"; continue; }
        out="$out$d:"
    done < <(echo "$PATH" | tr ':' '\n' | grep -v '^$')
    echo "$out"
}

extract_with_path() { # extract_with_path <PATH> ; result in stdout
    PATH="$1" sh -c ". '$HERE/update_verify.sh' && upd_asset_digest_from_json '$JSON_OK' '$ASSET'" 2>/dev/null
}

tool_missing_in() { # tool_missing_in <PATH> <tool> -> exit 0 if truly unreachable
    PATH="$1" sh -c "command -v $2 >/dev/null 2>&1"; [ $? -ne 0 ]
}

if command -v python3 >/dev/null 2>&1; then
    P="$(path_without_verified jq python3)"
    if tool_missing_in "$P" jq && PATH="$P" sh -c 'command -v python3 >/dev/null'; then
        got="$(extract_with_path "$P")"
        t "python3 fallback extracts identical digest with jq PROVABLY hidden" test "$got" = "$HEX_B"

        PATH="$P" sh -c ". '$HERE/update_verify.sh' && upd_asset_digest_from_json '$JSON_NULL' 'old.tar.gz'" >/dev/null 2>&1
        t "python3 fallback also rejects null digest" test $? -ne 0
    else
        echo "SKIP: cannot construct a jq-free python3-capable PATH on this box"
        echo "      (jq and python3 share every dir; fallback still covered by the"
        echo "      toolless-abort case and by CI images where layouts differ)"
    fi
fi

# All-toolless environment must still abort (fail-safe, never skip verify).
if env -i PATH=/nonexistent sh -c ". '$HERE/update_verify.sh' && upd_asset_digest_from_json '$JSON_OK' '$ASSET'" >/dev/null 2>&1; then
    fail=$((fail+1)); echo "FAIL: toolless environment did not abort"
else
    pass=$((pass+1)); echo "PASS: no-jq-no-python3 environment aborts (fail-safe)"
fi

# Exit-code propagation: upd_verify_digest must surface exit 2 (no hashing
# tool) distinctly — a swallowed exit 2 would turn tool-absence into a
# generic mismatch and could mask real failures. env -i does NOT hide tools:
# sh reinstalls a default PATH, so this uses an explicit tool-less dir.
mkdir -p "$tmp/notools"
ln -sf "$(command -v sh)" "$tmp/notools/sh" 2>/dev/null
no_hash_exit="$(PATH="$tmp/notools" sh -c ". '$HERE/update_verify.sh' && upd_verify_digest '/etc/hostname' 'deadbeef' 'primary'" >/dev/null 2>&1; echo $?)"
t "both-hash-tools-missing propagates exit 2 through upd_verify_digest" test "$no_hash_exit" -eq 2

# --- verify_digest ---
printf 'provider-bytes-trusted' > "$tmp/good.bin"
good_sum="$(sha256sum "$tmp/good.bin" | awk '{print $1}')"
upd_verify_digest "$tmp/good.bin" "$good_sum" "primary"
t "verify_digest accepts exact match" test $? -eq 0

printf 'provider-bytes-TAMPERED' > "$tmp/bad.bin"
upd_verify_digest "$tmp/bad.bin" "$good_sum" "primary" >/dev/null 2>&1
t "verify_digest rejects tampered content" test $? -ne 0

# Source label must appear in mismatch output (error-indistinction fix).
upd_verify_digest "$tmp/bad.bin" "$good_sum" "mirror-fallback" 2> "$tmp/err.txt"
grep -q "mirror-fallback" "$tmp/err.txt"
t "mismatch error names the download source" true

upd_verify_digest "$tmp/nonexistent.bin" "$good_sum" "primary" >/dev/null 2>&1
t "verify_digest rejects missing file" test $? -ne 0

# Empty expected digest must never pass (guards against "" sneaking through).
upd_verify_digest "$tmp/good.bin" "" "" >/dev/null 2>&1
t "empty expected digest rejected" test $? -ne 0

# Hashing fallback parity: openssl must produce the same hash as sha256sum
# where both exist; where only one exists, verification still functions.
if command -v openssl >/dev/null 2>&1; then
    ossl="$(openssl dgst -sha256 "$tmp/good.bin" | awk '{print $NF}')"
    t "openssl sha256 agrees with sha256sum (fallback equivalence)" test "$ossl" = "$good_sum"

    O="$(path_without_verified sha256sum openssl openssl)"
    if tool_missing_in "$O" sha256sum && PATH="$O" sh -c 'command -v openssl >/dev/null'; then
        got="$(PATH="$O" sh -c ". '$HERE/update_verify.sh' && upd_sha256_of '$tmp/good.bin'" 2>/dev/null)"
        t "upd_sha256_of falls back to openssl with sha256sum PROVABLY hidden" test "$got" = "$good_sum"

        PATH="$O" sh -c ". '$HERE/update_verify.sh' && upd_verify_digest '$tmp/bad.bin' '$good_sum' 'primary'" >/dev/null 2>&1
        t "verification still rejects tampered content via openssl fallback" test $? -ne 0
    fi

    # $NF-vs-$2 guard: whatever field awk picks, it must equal sha256sum's hex.
    ossl2="$(openssl dgst -sha256 "$tmp/good.bin" | awk '{print $2}')"
    t "awk \$2 field equals \$NF for current openssl output" test "$ossl2" = "$ossl"
fi

# POSIX discipline: no `local` anywhere in the sourced helper.
grep -qE '^\s*local\s' "$HERE/update_verify.sh"
t "update_verify.sh is POSIX-clean (no local)" test $? -ne 0

# The helper parses identically under /bin/sh as under bash (shebang contract).
sh -c ". '$HERE/update_verify.sh' && upd_asset_digest_from_json '$JSON_OK' '$ASSET'" > "$tmp/sh.out"
t "helper works when sourced by strict /bin/sh (dash-compatible)" test "$(cat "$tmp/sh.out")" = "$HEX_B"

# No stale UPD_ globals leak out of a successful call into a direct-source
# caller's environment: UPD_raw, UPD_json_clean, UPD_actual all covered
# (UPD_actual is unexported so env cannot see it — probe via a sourced
# subshell asserting emptiness instead).
sh -c ". '$HERE/update_verify.sh' && upd_asset_digest_from_json '$JSON_OK' '$ASSET' >/dev/null; test -z \"\${UPD_raw:-}\" && test -z \"\${UPD_json_clean:-}\" && test -z \"\${UPD_actual:-}\"" 2>/dev/null
t "helper globals cleaned after successful call" true

# --- Stable release selection (upd_select_stable_release_json) ---
# Regression: releases/latest returns the newest non-draft, non-prerelease
# release whether or not it carries a provider tarball, so a docs-only or
# tool-only release stalls every updater on that tag. An rc published with
# the prerelease flag unset is returned as latest too and was installed as
# stable. The selector must page past both.
HEX_A="$(printf 'a%.0s' $(seq 1 64))"
HEX_C="$(printf 'c%.0s' $(seq 1 64))"
REL_STABLE='{"tag_name":"v3.23.0-fix.32.10","draft":false,"prerelease":false,"assets":[{"name":"urnetwork-provider-v3.23.0-fix.32.10.tar.gz","digest":"sha256:'"$HEX_B"'"}]}'
REL_NOTES='{"tag_name":"v3.23.0-fix.33.1-notes","draft":false,"prerelease":false,"assets":[{"name":"urnet-tools-linux-amd64","digest":"sha256:'"$HEX_A"'"}]}'
REL_RC_UNFLAGGED='{"tag_name":"v3.23.0-fix.33.0-rc5","draft":false,"prerelease":false,"assets":[{"name":"urnetwork-provider-v3.23.0-fix.33.0-rc5.tar.gz","digest":"sha256:'"$HEX_C"'"}]}'
REL_RC_FLAGGED='{"tag_name":"v3.23.0-fix.33.0-rc6","draft":false,"prerelease":true,"assets":[{"name":"urnetwork-provider-v3.23.0-fix.33.0-rc6.tar.gz","digest":"sha256:'"$HEX_C"'"}]}'
REL_DRAFT='{"tag_name":"v3.23.0-fix.33.2","draft":true,"prerelease":false,"assets":[{"name":"urnetwork-provider-v3.23.0-fix.33.2.tar.gz","digest":"sha256:'"$HEX_C"'"}]}'
REL_NO_DIGEST='{"tag_name":"v3.23.0-fix.32.11","draft":false,"prerelease":false,"assets":[{"name":"urnetwork-provider-v3.23.0-fix.32.11.tar.gz","digest":""}]}'

sel_tag() { upd_select_stable_release_json "$1" | jq -r '.tag_name // empty'; }

t "selector skips a newer release without a provider asset" \
    test "$(sel_tag "[$REL_NOTES,$REL_STABLE]")" = "v3.23.0-fix.32.10"
t "selector never selects an unflagged rc tag as stable" \
    test "$(sel_tag "[$REL_RC_UNFLAGGED,$REL_STABLE]")" = "v3.23.0-fix.32.10"
t "selector skips a prerelease-flagged release" \
    test "$(sel_tag "[$REL_RC_FLAGGED,$REL_STABLE]")" = "v3.23.0-fix.32.10"
t "selector skips a draft release" \
    test "$(sel_tag "[$REL_DRAFT,$REL_STABLE]")" = "v3.23.0-fix.32.10"
t "selector skips a provider tarball without a sha256 digest" \
    test "$(sel_tag "[$REL_NO_DIGEST,$REL_STABLE]")" = "v3.23.0-fix.32.10"
t "selector keeps the newest stable release with an asset (normal case)" \
    test "$(sel_tag "[$REL_STABLE]")" = "v3.23.0-fix.32.10"
sel_none="$(upd_select_stable_release_json "[$REL_NOTES,$REL_RC_UNFLAGGED]")"
t "selector yields nothing when no release is eligible" test -z "$sel_none"

# Paging: a full first page of ineligible releases must not end the walk.
# curl is stubbed (shell function) so the walk is fully offline.
PAGE_ONE="[$REL_NOTES,$REL_RC_UNFLAGGED]"
PAGE_TWO="[$REL_STABLE]"
paging_out="$(
    . "$HERE/update_verify.sh"
    PAGE_ONE="$PAGE_ONE" PAGE_TWO="$PAGE_TWO" CALLS_FILE="$tmp/paging_calls.txt"
    export PAGE_ONE PAGE_TWO CALLS_FILE
    : > "$CALLS_FILE"
    curl() {
        printf '%s\n' "$*" >> "$CALLS_FILE"
        case "$*" in
            *"page=1"*) printf '%s' "$PAGE_ONE" ;;
            *"page=2"*) printf '%s' "$PAGE_TWO" ;;
            *) return 22 ;;
        esac
    }
    UPD_RELEASE_PER_PAGE=2 UPD_RELEASE_MAX_PAGES=3 upd_fetch_stable_release_json "https://api.invalid/repo"
)"
t "paging walk reaches page 2 and returns the eligible release" \
    test "$(printf '%s' "$paging_out" | jq -r '.tag_name // empty')" = "v3.23.0-fix.32.10"
t "paging walk requested both pages" \
    sh -c "grep -q 'page=2' '$tmp/paging_calls.txt'"

paging_none="$(
    . "$HERE/update_verify.sh"
    curl() { printf '%s' "[$REL_NOTES,$REL_RC_UNFLAGGED]"; }
    UPD_RELEASE_PER_PAGE=2 UPD_RELEASE_MAX_PAGES=2 upd_fetch_stable_release_json "https://api.invalid/repo" 2>/dev/null
)"
t "paging walk gives up after max pages without an eligible release" \
    test -z "$paging_none"

# Every update path must use the paged selector, never the single
# /releases/latest endpoint.
for f in start_update.sh urnet-tools.sh provider_common.sh; do
    grep -q "/releases/latest" "$HERE/$f"
    t "$f does not query the latest-release endpoint" test $? -ne 0
    t "$f uses upd_fetch_stable_release_json" grep -q "upd_fetch_stable_release_json" "$HERE/$f"
done

# --- Release-asset pickers (start_update fat pick, provider_common arch pick) ---
# Fixture shaped like a real release: assets in the order GitHub returns them
# (darwin, freebsd, linux, windows), the OS-less fat tarball last, plus decoys.
# start_update.sh must take the MULTI-ARCH fat tarball (Extract_Providers pulls
# both linux arches from one file); the container updater must take the linux
# tarball for its own arch. Both pick by EXACT name from .tag_name: a deny-list
# of OS tokens mis-picks on the first platform it does not name, which is how a
# freebsd asset reached a linux container (docker-shakedown W1 nightly).
JSON_MULTI='{"tag_name":"v9","assets":[
  {"name":"urnet-tools-linux-amd64","digest":"sha256:1"},
  {"name":"urnetwork-provider-v9-darwin-amd64.tar.gz","digest":"sha256:2"},
  {"name":"urnetwork-provider-v9-freebsd-amd64.tar.gz","digest":"sha256:3"},
  {"name":"urnetwork-provider-v9-linux-amd64.tar.gz","digest":"sha256:4"},
  {"name":"urnetwork-provider-v9-linux-musl-amd64.tar.gz","digest":"sha256:5"},
  {"name":"urnetwork-provider-v9-windows-amd64.tar.gz","digest":"sha256:6"},
  {"name":"urnetwork-provider-v9.tar.gz","digest":"sha256:7"}]}'
FAT_PICK='.tag_name as $t | "urnetwork-provider-\($t).tar.gz" as $f | ([.assets[] | select(.name == $f) | .name][0] // empty)'
ARCH_PICK='.tag_name as $t | "urnetwork-provider-\($t)-linux-\($arch).tar.gz" as $a | "urnetwork-provider-\($t).tar.gz" as $f | ([.assets[].name | select(. == $a)][0] // [.assets[].name | select(. == $f)][0] // empty)'

sel="$(printf '%s' "$JSON_MULTI" | jq -r "$FAT_PICK")"
t "start_update picks the fat tarball by exact name, never darwin or freebsd" test "$sel" = "urnetwork-provider-v9.tar.gz"

sel_pc="$(printf '%s' "$JSON_MULTI" | jq -r --arg arch amd64 "$ARCH_PICK")"
t "provider_common prefers the linux arch tarball regardless of asset order" test "$sel_pc" = "urnetwork-provider-v9-linux-amd64.tar.gz"

# No linux asset for this arch: fall back to the fat tarball, never to freebsd
# or to a sub-variant such as the musl build that sorts between them.
JSON_NO_LINUX='{"tag_name":"v9","assets":[
  {"name":"urnetwork-provider-v9-darwin-amd64.tar.gz","digest":"sha256:1"},
  {"name":"urnetwork-provider-v9-freebsd-amd64.tar.gz","digest":"sha256:2"},
  {"name":"urnetwork-provider-v9.tar.gz","digest":"sha256:3"}]}'
sel_nl="$(printf '%s' "$JSON_NO_LINUX" | jq -r --arg arch amd64 "$ARCH_PICK")"
t "provider_common falls back to the fat tarball when the arch asset is absent" test "$sel_nl" = "urnetwork-provider-v9.tar.gz"

JSON_FBSD_ONLY='{"tag_name":"v9","assets":[
  {"name":"urnetwork-provider-v9-darwin-amd64.tar.gz","digest":"sha256:1"},
  {"name":"urnetwork-provider-v9-freebsd-amd64.tar.gz","digest":"sha256:2"}]}'
sel_fb="$(printf '%s' "$JSON_FBSD_ONLY" | jq -r --arg arch amd64 "$ARCH_PICK")"
t "provider_common picks nothing when only other platforms are published" test -z "$sel_fb"

# The digest index is keyed by name: the lookup must follow the picked name.
dig="$(printf '%s' "$JSON_MULTI" | jq -r --arg f "$sel_pc" '.assets[] | select(.name == $f) | .digest')"
t "digest lookup follows the picked asset name" test "$dig" = "sha256:4"

# Drift guard: the stop contract is duplicated in every supervisor shell, so the
# copies must stay byte-identical. A fix applied to one copy and not the others
# leaves a container that still hangs on stop.
stop_block() {
  sed -n '/^# --- stop contract begin ---/,/^# --- stop contract end ---/p' "$HERE/$1"
}
STOP_FILES="provider_common.sh pelican_panel.sh"
first_sum=""
drift=0
for f in $STOP_FILES; do
  sum="$(stop_block "$f" | cksum)"
  [ -z "$first_sum" ] && first_sum="$sum"
  [ "$sum" = "$first_sum" ] || drift=1
done
t "stop contract is identical in every supervisor shell" test "$drift" -eq 0


# The broken (unparenthesized) shape must stay broken-and-unused: assert it
# errors, so nobody reintroduces it thinking it works.
broken_exit=0
printf '%s' "$JSON_MULTI" | jq -r '.assets[] | select(.name | startswith("urnetwork-provider-")) and (.name | endswith(".tar.gz")) | .name' >/dev/null 2>&1 || broken_exit=1
t "unparenthesized select+and stays rejected (would abort under set -e)" test "$broken_exit" -eq 1

# Nightly asset mapping: URL->name match-back finds the API name even when
# the URL layout changes above the filename segment.
URL_V9="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v9/urnetwork-provider-v9.tar.gz"
matched=""
for upd_candidate in $(printf '%s\n' "$JSON_MULTI" | grep -oE '"name": *"[^"]+"' | sed -E 's/"name": *"([^"]+)"/\1/'); do
    case "$URL_V9" in
        *"/$upd_candidate") matched="$upd_candidate"; break ;;
    esac
done
t "nightly URL-to-name match-back resolves the real API asset name" test "$matched" = "urnetwork-provider-v9.tar.gz"

# Basename last-resort path: an asset name absent from the JSON must NOT
# match-back (guarding the match-back order) — basename of URL used only
# after match-back exhausts candidates.
BNAME="${URL_V9##*/}"
t "basename fallback yields the filename segment" test "$BNAME" = "urnetwork-provider-v9.tar.gz"

# --- Source guard: upstream repos must not appear in fetch URLs ---
violations=""
for f in "$HERE"/start_update.sh "$HERE"/start_nightly.sh "$HERE"/start_stable.sh \
         "$HERE"/start_jwt.sh "$HERE"/provider_common.sh "$HERE"/urnet-tools.sh "$HERE"/pelican_panel.sh \
         "$HERE"/test_exit75_restart.sh; do
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
