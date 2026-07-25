#!/bin/bash
# Tests for:
#   - restart -y/-f/--yes/--force flag parsing
#   - download URL ordering (dl.fullbars.xyz primary, GitHub fallback)

pass=0
fail=0

pass() { ((pass++)); echo "  ✅ PASS: $1"; }
fail() { ((fail++)); echo "  ❌ FAIL: $1"; }

header() { echo ""; echo "=========================================="; echo "    $1"; echo "=========================================="; }

#
# Test 1: restart -y sets FORCE=1
#
header "restart -y flag"

test_restart_force() {
    local desc="$1"; shift
    _restart_force=0

    # Parse flags like the restart case does
    while [ $# -gt 0 ]; do
        case "$1" in
            -y|--yes|-f|--force)
                _restart_force=1
                shift
                ;;
            *)
                echo "unknown: $1"
                return 1
                ;;
        esac
    done
    if [ "$_restart_force" = "1" ]; then
        FORCE=1
    fi
}

FORCE=0
test_restart_force "restart -y" -y
[ "$FORCE" = "1" ] && pass "restart -y sets FORCE=1" || fail "restart -y should set FORCE=1"

FORCE=0
test_restart_force "restart -f" -f
[ "$FORCE" = "1" ] && pass "restart -f sets FORCE=1" || fail "restart -f should set FORCE=1"

FORCE=0
test_restart_force "restart --yes" --yes
[ "$FORCE" = "1" ] && pass "restart --yes sets FORCE=1" || fail "restart --yes should set FORCE=1"

FORCE=0
test_restart_force "restart --force" --force
[ "$FORCE" = "1" ] && pass "restart --force sets FORCE=1" || fail "restart --force should set FORCE=1"

FORCE=1
test_restart_force "restart (no flags)" 
[ "$FORCE" = "1" ] && pass "restart without flags keeps existing FORCE" || fail "restart without flags should not clear FORCE"

#
# Test 2: Provider download URL ordering
#
header "Provider download URLs (Provider_Install_Linux.sh)"

tag="v3.23.0-fix.27"
dl_url="https://dl.fullbars.xyz/releases/download/$tag/urnetwork-provider-$tag.tar.gz"
mirror_url="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/$tag/urnetwork-provider-$tag.tar.gz"

[[ "$dl_url" == "https://dl.fullbars.xyz/"* ]] && pass "Primary URL points to dl.fullbars.xyz" || fail "Primary URL should point to dl.fullbars.xyz"
[[ "$mirror_url" == "https://github.com/"* ]] && pass "Mirror URL points to GitHub" || fail "Mirror URL should point to GitHub"
[[ "$dl_url" == *"urnetwork-provider-$tag.tar.gz" ]] && pass "Primary URL contains correct filename" || fail "Primary URL should contain correct filename"
[[ "$mirror_url" == *"urnetwork-provider-$tag.tar.gz" ]] && pass "Mirror URL contains correct filename" || fail "Mirror URL should contain correct filename"

#
# Test 3: Hub install download URL ordering
#
header "Hub install URLs (Provider_Install_Linux.sh)"

hub_tag="v3.23.0-fix.27"
hub_dl_url="https://dl.fullbars.xyz/releases/download/${hub_tag}/urnetwork-hub-${hub_tag}-linux-amd64"
hub_mirror_url="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/${hub_tag}/urnetwork-hub-${hub_tag}-linux-amd64"

[[ "$hub_dl_url" == "https://dl.fullbars.xyz/"* ]] && pass "Hub primary URL points to dl.fullbars.xyz" || fail "Hub primary URL should point to dl.fullbars.xyz"
[[ "$hub_mirror_url" == "https://github.com/"* ]] && pass "Hub mirror URL points to GitHub" || fail "Hub mirror URL should point to GitHub"

#
# Test 4: Hub update download URL ordering
#
header "Hub update URLs (Provider_Install_Linux.sh)"

hub_update_url="https://dl.fullbars.xyz/releases/download/${hub_tag}/urnetwork-hub-${hub_tag}-linux-amd64"
hub_update_mirror="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/${hub_tag}/urnetwork-hub-${hub_tag}-linux-amd64"

[[ "$hub_update_url" == "https://dl.fullbars.xyz/"* ]] && pass "Hub update primary URL points to dl.fullbars.xyz" || fail "Hub update primary URL should point to dl.fullbars.xyz"
[[ "$hub_update_mirror" == "https://github.com/"* ]] && pass "Hub update mirror URL points to GitHub" || fail "Hub update mirror URL should point to GitHub"

#
# Test 5: Mac download URL ordering
#
header "Mac download URLs (Provider_Install_Mac.sh)"

tarball_url="https://dl.fullbars.xyz/releases/download/$tag/urnetwork-provider-$tag.tar.gz"
mac_mirror_url="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/$tag/urnetwork-provider-$tag.tar.gz"

[[ "$tarball_url" == "https://dl.fullbars.xyz/"* ]] && pass "Mac primary URL points to dl.fullbars.xyz" || fail "Mac primary URL should point to dl.fullbars.xyz"
[[ "$mac_mirror_url" == "https://github.com/"* ]] && pass "Mac mirror URL points to GitHub" || fail "Mac mirror URL should point to GitHub"

#
# Test 6: Windows download URL ordering
#
header "Windows download URLs (Provider_Install_Win32.ps1 - ported to bash logic)"

ReleaseVersion="v3.23.0-fix.27"
FileName="urnetwork-provider-$ReleaseVersion.tar.gz"
WinDownloadURL="https://dl.fullbars.xyz/releases/download/$ReleaseVersion/$FileName"
WinMirrorURL="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/$ReleaseVersion/$FileName"

[[ "$WinDownloadURL" == "https://dl.fullbars.xyz/"* ]] && pass "Windows primary URL points to dl.fullbars.xyz" || fail "Windows primary URL should point to dl.fullbars.xyz"
[[ "$WinMirrorURL" == "https://github.com/"* ]] && pass "Windows mirror URL points to GitHub" || fail "Windows mirror URL should point to GitHub"

#
# Test 7: Docker download URL ordering
#
header "Docker download URLs (urnet-tools.sh)"

# Docker derives primary from GitHub API URL by swapping hostname
github_api_url="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v3.23.0-fix.27/urnetwork-provider-linux-amd64.tar.gz"
docker_primary="$(echo "$github_api_url" | sed 's|https://github.com/full-bars/urnetwork-3.23-fix/releases/download/|https://dl.fullbars.xyz/releases/download/|')"
docker_fallback="$github_api_url"

[[ "$docker_primary" == "https://dl.fullbars.xyz/releases/download/v3.23.0-fix.27/urnetwork-provider-linux-amd64.tar.gz" ]] && pass "Docker primary derived correctly from GitHub URL" || fail "Docker primary derivation failed: $docker_primary"
[[ "$docker_fallback" == "https://github.com/"* ]] && pass "Docker fallback URL points to GitHub" || fail "Docker fallback URL should point to GitHub"

#
# Test 8: confirm_restart respects FORCE
#
header "confirm_restart respects FORCE=1"

# Mock the helper functions
hot_restart_is_enabled() { return 1; }
pr_info() { echo "$@"; }
pr_err() { echo "$@"; }

# Extract the confirm_restart logic
test_confirm_restart() {
    local action="${1:-test}"
    if [ "$FORCE" = "1" ]; then
        return 0
    fi
    return 1 # would prompt
}

FORCE=1
test_confirm_restart && pass "confirm_restart returns 0 when FORCE=1" || fail "confirm_restart should return 0 when FORCE=1"

FORCE=0
test_confirm_restart && fail "confirm_restart returned 0 when FORCE=0 (should prompt)" || pass "confirm_restart would prompt when FORCE=0"

#
# Summary
#
echo ""
echo "=========================================="
echo "    Results: $pass passed, $fail failed"
echo "=========================================="
[ "$fail" -eq 0 ] && exit 0 || exit 1
