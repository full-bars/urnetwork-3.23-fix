#!/bin/bash
# compute-app-version.sh — prints the upstream-style app_version a provider
# release reports to the backend in its connect auth.
#
# Shape: YYYY.M.D-<code>-fix.<N.M>   e.g. 2026.10.20-1076956260-fix.34.1
#
# The backend (server/connect/transport_announce_provider_version.go) keeps
# only YYYY.M.D for its rollout gauge and stores the whole string in
# device.client_version. <code> follows the URnetwork house convention
# (upstream warpctl newVersionCode(): round(seconds since the network's
# 2023-05-23T00:00:00Z founding) * 10), the same formula as meso-miner's
# compute-meso-tag.sh. It is derived from the COMMIT time of the tagged
# commit, so every CI job and arch of one release computes the same value
# (checkouts are shallow, so tag-object dates are not reliably available).
#
# Usage:
#   scripts/compute-app-version.sh v3.23.0-fix.34.1 [commit-epoch]
#   prints nothing (exit 0) for a tag that is not v3.23.0-fix.*, so callers
#   fall back to the plain tag.
set -euo pipefail

FOUNDING_EPOCH=1684800000 # 2023-05-23T00:00:00Z

tag="${1:-}"
case "$tag" in
v3.23.0-fix.*) ;;
*) exit 0 ;;
esac
suffix="${tag#v3.23.0-fix.}"
epoch="${2:-$(git log -1 --format=%ct "$tag" 2>/dev/null || git log -1 --format=%ct)}"

code=$(((epoch - FOUNDING_EPOCH) * 10))
echo "$(date -u -d "@${epoch}" +%Y.%-m.%-d)-${code}-fix.${suffix}"
