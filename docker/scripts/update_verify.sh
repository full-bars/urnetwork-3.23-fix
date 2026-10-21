#!/bin/sh
# Digest-verification helpers for the docker self-update scripts.
#
# The provider tarball is fetched from GitHub releases; the release API
# exposes a per-asset sha256 digest ("sha256:<hex>") since GitHub added the
# field. Verifying BEFORE extract/install closes the last gap in the update
# path: a tampered or corrupted tarball can no longer be swapped onto the
# running provider. On mismatch the caller must abort and leave the existing
# binary untouched.
#
# STRICT POSIX: this file is sourced by both /bin/bash (start_update.sh,
# urnet-tools.sh) and /bin/sh (start_nightly.sh, busybox ash on Alpine), so
# it must not assume bash. `local` is left out entirely — every helper keeps
# its state in uniquely-named globals (UPD_ prefix) because POSIX sh has no
# function-local scope.
#
# TOOL FALLBACKS mirror scripts/Provider_Install_Linux.sh: JSON parsing
# prefers jq, falls back to python3; hashing prefers sha256sum, falls back
# to openssl. All-toolless environments still abort (fail-safe), they just
# get one clear log line saying exactly which tools are missing.

# upd_json_digest_field <release-json> <asset-name>
# Prints the bare hex sha256 for <asset-name>, or returns non-zero when no
# parser is available or the asset carries no usable digest. Control chars
# are stripped from the input first (tr -d) to match the installer's
# hardening against hostile JSON bytes.
upd_json_digest_field() {
    UPD_raw=""
    UPD_json_clean="$(printf '%s' "$1" | tr -d '\000-\037')"
    if command -v jq >/dev/null 2>&1; then
        UPD_raw="$(printf '%s' "$UPD_json_clean" | jq -r --arg a "$2" \
            '.assets[] | select(.name == $a) | .digest // empty' 2>/dev/null)"
    elif command -v python3 >/dev/null 2>&1; then
        UPD_raw="$(printf '%s' "$UPD_json_clean" | python3 -c 'import sys, json
try:
    data = json.load(sys.stdin)
    asset = sys.argv[1]
    for a in data.get("assets", []):
        if a.get("name") == asset:
            print(a.get("digest") or "")
            break
except (json.JSONDecodeError, KeyError, ValueError):
    print("")
' "$2" 2>/dev/null)"
    else
        echo "upd_verify: neither jq nor python3 available; cannot extract expected digest from release JSON" >&2
    fi
    unset UPD_json_clean
    [ -n "$UPD_raw" ] || return 1
    # Normalize: strip "sha256:" prefix; null/None (legacy assets) -> reject.
    UPD_raw="${UPD_raw#sha256:}"
    case "$UPD_raw" in
        null|None|"") return 1 ;;
    esac
    printf '%s\n' "$UPD_raw"
    unset UPD_raw
}

# upd_asset_digest_from_json <release-json> <asset-name>
# Kept as the stable caller-facing name used by all update paths.
upd_asset_digest_from_json() {
    upd_json_digest_field "$@"
}

# --- Stable release selection ---------------------------------------------
# GitHub's /releases/latest endpoint returns the newest non-draft,
# non-prerelease release whether or not it carries a provider tarball. A
# docs-only or tool-only release published after a good one therefore stalls
# every updater on that tag until an even newer release appears. These
# helpers walk GET /releases instead and return the newest release that is:
#   - not a draft,
#   - not flagged prerelease,
#   - not named like a pre-release (-rcN, .beta2, -nightly, ...), and
#   - carrying urnetwork-provider-<tag>.tar.gz with a sha256 digest.
# The tag-name rule matters because a release published by hand can carry an
# rc tag with the prerelease flag unset; GitHub then reports it as latest.
#
# jq is required (the image installs it and every update path already uses
# it). Selection is pure: the caller owns the network and the logging.

# upd_select_stable_release_json <releases-array-json>
# Prints the first eligible stable release object (compact JSON), or nothing
# when the page holds no eligible release. Exit status is jq's: non-zero only
# when the JSON cannot be parsed.
upd_select_stable_release_json() {
    printf '%s' "$1" | jq -c '
      [ .[]
        | select(type == "object")
        | select((.draft // false) == false)
        | select((.prerelease // false) == false)
        | select(((.tag_name // "")
                  | test("(^|[-._])(rc|alpha|beta|pre|preview|nightly|snapshot|dev)[0-9]*($|[-._])"; "i")
                  | not))
        | . as $r
        | ("urnetwork-provider-" + $r.tag_name + ".tar.gz") as $want
        | select([$r.assets[]? | select(.name == $want and ((.digest // "") | test("^sha256:[0-9a-fA-F]+$")))] | length > 0)
      ] | first // empty'
}

# upd_release_select_cleanup: POSIX sh has no function-local scope; drop the
# UPD_ globals the selection walk uses (mirrors the digest helpers' rule).
upd_release_select_cleanup() {
    unset UPD_root UPD_per_page UPD_max_pages UPD_page UPD_page_json UPD_selected UPD_count
}

# --- Tarball asset selection -----------------------------------------------
# A release ships one per-arch tarball per platform/GOARCH (name carries an
# -<os>-<goarch> token: linux, freebsd, darwin, windows, ...) plus one OS-less
# multi-arch fat tarball (urnetwork-provider-vX.tar.gz). Every platform is a
# wanted build; the updater either takes the linux-<arch> tarball for this
# container or, for tooling, the fat tarball. Both jq pickers resolve
# EXACT names keyed on the release tag (upd_select_stable_release_json
# already guarantees the fat tarball asset exists with a sha256 digest), so
# decoy or wrong-platform assets can never win no matter how they sort or
# what new platform gets added. UPD_PER_ARCH_OS_RES only feeds the no-jq
# grep fallback, which cannot parse tag names.
UPD_PER_ARCH_OS_RES="darwin|freebsd|linux|windows"

# UPD_release_tag <release-json>: prints the tag_name (UPD_ global because
# POSIX sh has no function-local scope).
UPD_release_tag() {
    printf '%s' "$1" | jq -r '.tag_name // empty' 2>/dev/null
}

# upd_pick_arch_tarball_name <release-json> <goarch>
# Prints the name of the linux-<goarch> per-arch tarball, or nothing when the
# release carries none (the caller then falls back to the fat tarball).
upd_pick_arch_tarball_name() {
    UPD_tag="$(UPD_release_tag "$1")"
    [ -n "$UPD_tag" ] || return 0
    printf '%s' "$1" | jq -r --arg want "urnetwork-provider-${UPD_tag}-linux-${2}.tar.gz" \
        '.assets[] | .name | select(. == $want)
      ' 2>/dev/null | head -n1
}

# upd_pick_fat_tarball_name <release-json>
# Prints the name of the OS-less multi-arch fat tarball, or nothing when the
# release carries none.
upd_pick_fat_tarball_name() {
    UPD_tag="$(UPD_release_tag "$1")"
    [ -n "$UPD_tag" ] || return 0
    printf '%s' "$1" | jq -r --arg want "urnetwork-provider-${UPD_tag}.tar.gz" \
        '.assets[] | .name | select(. == $want)
      ' 2>/dev/null | head -n1
}

# upd_fetch_stable_release_json <api-root>
# Walks release pages (newest first) and prints the newest eligible stable
# release object. UPD_RELEASE_PER_PAGE / UPD_RELEASE_MAX_PAGES override the
# 100/10 defaults (tests use small pages). Returns 1 when no eligible release
# exists in the walked pages.
upd_fetch_stable_release_json() {
    UPD_root="$1"
    UPD_per_page="${UPD_RELEASE_PER_PAGE:-100}"
    UPD_max_pages="${UPD_RELEASE_MAX_PAGES:-10}"
    UPD_page=1
    while [ "$UPD_page" -le "$UPD_max_pages" ]; do
        if ! UPD_page_json="$(curl -sfL --connect-timeout 10 --max-time 30 \
                "$UPD_root/releases?per_page=$UPD_per_page&page=$UPD_page" 2>/dev/null)"; then
            echo "upd_verify: release listing page $UPD_page unavailable" >&2
            upd_release_select_cleanup
            return 1
        fi
        if ! UPD_selected="$(upd_select_stable_release_json "$UPD_page_json")"; then
            echo "upd_verify: could not parse release listing page $UPD_page" >&2
            upd_release_select_cleanup
            return 1
        fi
        if [ -n "$UPD_selected" ]; then
            printf '%s\n' "$UPD_selected"
            upd_release_select_cleanup
            return 0
        fi
        UPD_count="$(printf '%s' "$UPD_page_json" | jq 'length' 2>/dev/null)" || UPD_count=""
        case "$UPD_count" in
            ''|*[!0-9]*) UPD_count=0 ;;
        esac
        # A short page is the last page: stop rather than paging forever.
        if [ "$UPD_count" -lt "$UPD_per_page" ]; then
            upd_release_select_cleanup
            return 1
        fi
        UPD_page=$((UPD_page + 1))
    done
    echo "upd_verify: no stable release with a provider tarball in the newest $UPD_max_pages release pages" >&2
    upd_release_select_cleanup
    return 1
}

# upd_sha256_of <file>
# Prints the hex sha256 of $1 via sha256sum, falling back to openssl.
upd_sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v openssl >/dev/null 2>&1; then
        openssl dgst -sha256 "$1" | awk '{print $2}'
    else
        echo "upd_verify: neither sha256sum nor openssl available; cannot hash downloaded file" >&2
        return 2
    fi
}

# upd_verify_digest <file> <expected-hex> <source-label>
# Compares sha256(file) against expected hex. <source-label> ("primary",
# "mirror", ...) appears in mismatch errors so logs say WHERE bad bytes came
# from. Exit codes: 2 = no hashing tool, 1 = mismatch.
upd_verify_digest() {
    UPD_actual="$(upd_sha256_of "$1")" || return $?
    if [ -z "$UPD_actual" ] || [ "$UPD_actual" != "$2" ]; then
        echo "[ERROR] Digest mismatch for $1 (${3:-download})" >&2
        echo "[ERROR]   source:   ${3:-download}" >&2
        echo "[ERROR]   expected: $2" >&2
        echo "[ERROR]   actual:   ${UPD_actual:-<empty - tool missing or unreadable file>}" >&2
        unset UPD_actual
        return 1
    fi
    unset UPD_actual
    return 0
}
