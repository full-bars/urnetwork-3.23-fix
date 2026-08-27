#!/bin/bash
# Digest-verification helpers for the docker self-update scripts.
#
# The provider tarball is fetched from GitHub releases; the release API
# exposes a per-asset sha256 digest ("sha256:<hex>") since GitHub added the
# field. Verifying BEFORE extract/install closes the last gap in the update
# path: a tampered or corrupted tarball can no longer be swapped onto the
# running provider. On mismatch the caller must abort and leave the existing
# binary untouched.

# asset_digest_from_json <release-json> <asset-name>
# Prints the bare hex digest for <asset-name> from a GitHub release JSON
# document, or returns non-zero when absent/unparseable. Mirrors
# get_asset_digest_from_api_response in scripts/Provider_Install_Linux.sh so
# both paths agree on the "sha256:<hex>" prefix handling and null digestion.
asset_digest_from_json() {
    local json="$1" asset="$2" raw=""
    if command -v jq >/dev/null 2>&1; then
        raw="$(printf '%s' "$json" | jq -r --arg a "$asset" \
            '.assets[] | select(.name == $a) | .digest // empty' 2>/dev/null)"
    fi
    [ -n "$raw" ] || return 1
    # Normalize: strip "sha256:" prefix; null/None (legacy assets) -> reject.
    raw="${raw#sha256:}"
    case "$raw" in
        null|None|""|"sha256:") return 1 ;;
    esac
    printf '%s\n' "$raw"
}

# verify_digest <file> <expected-hex>
# Compares sha256sum(file) against expected hex. Aborts with a distinct log
# line on mismatch or tool absence (busybox sha256sum exists on alpine).
verify_digest() {
    local file="$1" expected="$2" actual
    if ! command -v sha256sum >/dev/null 2>&1; then
        echo "[ERROR] sha256sum unavailable; refusing unverified install." >&2
        return 2
    fi
    actual="$(sha256sum "$file" | awk '{print $1}')"
    if [ "$actual" != "$expected" ]; then
        echo "[ERROR] Digest mismatch for $file" >&2
        echo "[ERROR]   expected: $expected" >&2
        echo "[ERROR]   actual:   $actual" >&2
        return 1
    fi
    return 0
}
