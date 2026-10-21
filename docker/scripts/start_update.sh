#!/bin/bash
set -Eeuo pipefail

# === Logging Helper ===
log() {
  echo "$(date '+%Y-%m-%d %H:%M:%S') >>> UrNetwork >>> $*"
}

# Resolve this script's directory (update_verify.sh lives alongside it).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=update_verify.sh
. "$SCRIPT_DIR/update_verify.sh"

# === Trap errors and print the failing line + function ===
trap 'log "[ERROR] Failure at line $LINENO in function $FUNCNAME"; exit 1' ERR

log "[INFO] Starting provider update process"

# Upstream repo constant: only OUR fork is a valid update source. The
# upstream urnetwork/* repos ship the vanilla provider — installing from them
# would silently replace this fork's hardened binary, so they are never named.
readonly UPSTREAM_REPO="full-bars/urnetwork-3.23-fix"
readonly API_ROOT="https://api.github.com/repos/full-bars/urnetwork-3.23-fix"

# === Function to download release tar.gz from GitHub API ===
Download_API() {
    local suffix="$1" # e.g. "stable" or "nightly" (asset is per-release tag)

    log "[INFO] Download_API → Repo: $UPSTREAM_REPO | Suffix: $suffix"

    local release_json tag
    # Walk the releases listing instead of trusting releases/latest: GitHub
    # calls the newest non-draft, non-prerelease release "latest" whether or
    # not it carries a provider tarball, so a docs-only or tool-only release
    # used to stall every update on that tag. The helper pages past releases
    # without urnetwork-provider-<tag>.tar.gz plus its sha256 digest, and it
    # never selects a pre-release (flagged, or only named like one) as stable.
    release_json="$(upd_fetch_stable_release_json "$API_ROOT")" || {
        log "[ERROR] No stable release with a verified urnetwork-provider tarball found"
        exit 1
    }
    tag="$(printf '%s' "$release_json" | jq -r '.tag_name // empty')"
    [ -n "$tag" ] || {
        log "[ERROR] Selected release carries no tag_name"
        exit 1
    }
    log "[INFO] Selected stable release: $tag"

    local filename download_url asset_digest
    # The shared picker (update_verify.sh) selects the MULTI-ARCH fat tarball:
    # Extract_Providers pulls BOTH linux/amd64 and linux/arm64 from the one
    # file, so per-arch assets (which carry an -<os>-<goarch> token) are
    # excluded. The OS token list is shared with the nightly updater, so a new
    # release platform can never be mistaken for the fat tarball again.
    filename="$(upd_pick_fat_tarball_name "$release_json")"
    download_url="$(echo "$release_json" | jq -r --arg f "$filename" \
        '.assets[] | select(.name == $f) | .browser_download_url')"
    [ -n "$filename" ] && [ -n "$download_url" ] && [ "$download_url" != "null" ] || {
        log "[ERROR] No urnetwork-provider-*.tar.gz asset found in latest release"
        exit 1
    }

    # Digest BEFORE downloading anything: if the API does not publish a
    # digest for this asset we refuse the whole update rather than install
    # unverified bytes.
    asset_digest="$(upd_asset_digest_from_json "$release_json" "$filename")" || {
        log "[ERROR] Release API returned no sha256 digest for $filename; refusing to update without verification"
        exit 1
    }
    log "[INFO] Expected digest: $asset_digest"

    log "[INFO] Download URL: $download_url"
    log "[INFO] Filename: $filename"

    log "[INFO] Downloading $filename..."
    # TLS verification ON (-k removed): an unverifiable channel defeats the
    # digest check's purpose.
    curl -fL -A "Mozilla/5.0" -o "$filename" "$download_url"
    log "[INFO] Downloaded: $filename"

    upd_verify_digest "$filename" "$asset_digest" "release-download" || {
        rm -f "$filename"
        log "[ERROR] Downloaded tarball failed digest verification; nothing installed."
        exit 1
    }
    log "[INFO] Digest verified OK"

    echo "$filename $suffix" >> download_list.txt
}

# === Function to extract provider binaries from tar.gz ===
Extract_Providers() {
    local filename="$1"
    local suffix="$2"

    log "[INFO] Extract_Providers → File: $filename | Suffix: $suffix"

    mkdir -p /app

    log "[INFO] Extracting amd64 provider..."
    tar --warning=no-unknown-keyword --extract --file="$filename" --strip-components=2 "linux/amd64/provider" -O > "/app/urnetwork_amd64_${suffix}"
    chmod +x "/app/urnetwork_amd64_${suffix}"
    log "[INFO] Extracted amd64 → /app/urnetwork_amd64_${suffix}"

    log "[INFO] Extracting arm64 provider..."
    tar --warning=no-unknown-keyword --extract --file="$filename" --strip-components=2 "linux/arm64/provider" -O > "/app/urnetwork_arm64_${suffix}"
    chmod +x "/app/urnetwork_arm64_${suffix}"
    log "[INFO] Extracted arm64 → /app/urnetwork_arm64_${suffix}"

    rm -f "$filename" || true
    log "[INFO] Deleted archive: $filename"
}

# === Phase 1: Download (single fork release, digest-verified) ===
log "[INFO] Phase 1: Download release"
Download_API "stable"

# === Phase 2: Extract ===
log "[INFO] Phase 2: Extract providers"
while read -r filename suffix; do
    Extract_Providers "$filename" "$suffix"
done < download_list.txt

# === Phase 3: Cleanup list ===
rm -f download_list.txt || true
log "[INFO] Cleaned up download_list.txt"
log "[INFO] Provider update process completed successfully"
