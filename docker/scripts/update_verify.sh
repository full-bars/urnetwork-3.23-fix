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

# upd_asset_digest_from_json <release-json> <asset-name>
# Prints the bare hex digest for <asset-name> from a GitHub release JSON
# document, or returns non-zero when absent/unparseable. Mirrors
# get_asset_digest_from_api_response in scripts/Provider_Install_Linux.sh so
# both paths agree on the "sha256:<hex>" prefix handling and null digestion.
upd_asset_digest_from_json() {
    UPD_raw=""
    if command -v jq >/dev/null 2>&1; then
        UPD_raw="$(printf '%s' "$1" | jq -r --arg a "$2" \
            '.assets[] | select(.name == $a) | .digest // empty' 2>/dev/null)"
    else
        echo "upd_verify: jq not installed; cannot extract expected digest from release JSON" >&2
    fi
    [ -n "$UPD_raw" ] || return 1
    # Normalize: strip "sha256:" prefix; null/None (legacy assets) -> reject.
    UPD_raw="${UPD_raw#sha256:}"
    case "$UPD_raw" in
        null|None|"") return 1 ;;
    esac
    printf '%s\n' "$UPD_raw"
    unset UPD_raw
}

# upd_verify_digest <file> <expected-hex> <source-label>
# Compares sha256sum(file) against expected hex. <source-label> ("primary",
# "mirror", ...) is echoed in mismatch errors so logs say WHERE bad bytes
# came from. Distinct exit codes: 2 = no sha256sum tool, 1 = mismatch.
upd_verify_digest() {
    if ! command -v sha256sum >/dev/null 2>&1; then
        echo "[ERROR] sha256sum unavailable; refusing unverified install." >&2
        return 2
    fi
    UPD_actual="$(sha256sum "$1" | awk '{print $1}')"
    if [ "$UPD_actual" != "$2" ]; then
        echo "[ERROR] Digest mismatch for $1 (${3:-download})" >&2
        echo "[ERROR]   source:   ${3:-download}" >&2
        echo "[ERROR]   expected: $2" >&2
        echo "[ERROR]   actual:   ${UPD_actual:-<empty - download produced no file>}" >&2
        return 1
    fi
    return 0
}
