#!/bin/sh
# install-urnet-docker — host-side installer for the urnet-docker CLI.
#
# urnet-docker manages URnetwork providers deployed as docker containers. It
# runs on the DOCKER HOST (outside the containers) and delegates into them
# via `docker exec`, so it is NOT baked into the image — docker-only users
# install it with this script:
#
#   curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/install-urnet-docker.sh | sh
#
# The same script can install urnet-tools (process/systemd variant) by
# passing the tool name as the first argument:
#
#   curl -fSsL .../install-urnet-docker.sh | sh -s -- urnet-tools
#
# Behavior: resolves the latest release, downloads the tool binary asset for
# this platform, verifies its sha256 against the release API digest, and
# installs to /usr/local/bin (or ~/.local/bin when not root). When the
# install dir is ~/.local/bin (non-root), the export is added to ~/.bashrc so
# the tool is on PATH immediately; pass -B/--no-modify-bashrc to skip that.
# The Go tool is self-updating afterwards (`urnet-tools update` /
# `urnet-docker update`).
set -e

API_BASE="https://api.github.com/repos/full-bars/urnetwork-3.23-fix"
REPO="full-bars/urnetwork-3.23-fix"

no_modify_bashrc=0
TOOL=""

pr_err() { printf "install-urnet-docker: %s\n" "$*" >&2; }

# --- flag parsing ---
# -B/--no-modify-bashrc may appear before or after the tool name, e.g.
# `sh -s -- -B urnet-tools`. TOOL is derived from the FIRST NON-FLAG arg so
# a leading flag cannot be misread as the tool name (review finding).
for arg in "$@"; do
    case "$arg" in
        -B|--no-modify-bashrc)
            no_modify_bashrc=1
            ;;
        -*)
            # unknown flag: ignore (kept for forward compat) but never
            # treat it as the tool name
            ;;
        *)
            if [ -z "$TOOL" ]; then
                TOOL="$arg"
            fi
            ;;
    esac
done
TOOL="${TOOL:-urnet-docker}"

# --- arch detection (Go names) ---
# The release matrix builds tool assets for amd64 and arm64 only; a 32-bit
# x86 host has no asset to fetch, so reject it explicitly rather than
# mapping to "386" and failing later with a misleading missing-asset error
# (verified 2026-08-12 review).
case "$(uname -m 2>/dev/null)" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    i386|i686)
        pr_err "32-bit x86 is not supported — release assets are built for amd64 and arm64 only"
        exit 1
        ;;
    *)
        pr_err "unsupported architecture: $(uname -m)"
        exit 1
        ;;
esac
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
    linux|darwin) ;;
    *)
        pr_err "unsupported OS: $OS (docker providers run on linux hosts)"
        exit 1
        ;;
esac

ASSET="${TOOL}-${OS}-${ARCH}"
INSTALL_DIR="${PREFIX:-/usr/local/bin}"
if [ "$(id -u)" != "0" ]; then
    INSTALL_DIR="${PREFIX:-$HOME/.local/bin}"
fi

# --- resolve latest release tag ---
echo "Resolving latest release..."
LATEST_JSON="$(curl -fsSL --connect-timeout 10 --retry 3 --retry-delay 2 "$API_BASE/releases/latest" 2>/dev/null || true)"
if [ -z "$LATEST_JSON" ]; then
    pr_err "failed to fetch latest release info from GitHub API"
    exit 1
fi
TAG="$(printf "%s" "$LATEST_JSON" | tr -d '\000-\037' | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
if [ -z "$TAG" ]; then
    # fallback: jq or python3
    if command -v jq > /dev/null 2>&1; then
        TAG="$(printf "%s" "$LATEST_JSON" | jq -r '.tag_name' 2>/dev/null || true)"
    elif command -v python3 > /dev/null 2>&1; then
        TAG="$(printf "%s" "$LATEST_JSON" | python3 -c 'import sys,json; print(json.load(sys.stdin)["tag_name"])' 2>/dev/null || true)"
    fi
fi
if [ -z "$TAG" ]; then
    pr_err "could not resolve latest release tag"
    exit 1
fi
echo "Latest release: $TAG"

# --- resolve the asset digest from the release API ---
DIGEST=""
if command -v jq > /dev/null 2>&1; then
    DIGEST="$(printf "%s" "$LATEST_JSON" | jq -r --arg a "$ASSET" '.assets[] | select(.name == $a) | .digest' 2>/dev/null | sed 's/^sha256://' || true)"
elif command -v python3 > /dev/null 2>&1; then
    DIGEST="$(printf "%s" "$LATEST_JSON" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(next((a.get("digest","").replace("sha256:","") for a in d.get("assets",[]) if a.get("name")==sys.argv[1]), ""))' "$ASSET" 2>/dev/null || true)"
fi
if [ -z "$DIGEST" ]; then
    pr_err "release $TAG has no $ASSET asset (release predates tool binaries, or tool name is wrong)"
    pr_err "  available tools: urnet-tools, urnet-docker"
    exit 1
fi

# --- download + verify ---
TMPDIR_T="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_T"' EXIT
TMPBIN="$TMPDIR_T/$ASSET"

DL_URL="https://dl.fullbars.xyz/releases/download/$TAG/$ASSET"
MIRROR_URL="https://github.com/$REPO/releases/download/$TAG/$ASSET"

echo "Downloading $ASSET..."
if ! curl -fsSL --connect-timeout 10 --retry 3 --retry-delay 2 "$DL_URL" -o "$TMPBIN" 2>/dev/null; then
    echo "primary download failed, trying GitHub mirror..."
    if ! curl -fsSL --connect-timeout 10 --retry 3 --retry-delay 2 "$MIRROR_URL" -o "$TMPBIN" 2>/dev/null; then
        pr_err "failed to download from both primary and mirror"
        exit 1
    fi
fi

echo "Verifying sha256..."
if command -v sha256sum > /dev/null 2>&1; then
    ACTUAL="$(sha256sum "$TMPBIN" | awk '{print $1}')"
elif command -v openssl > /dev/null 2>&1; then
    ACTUAL="$(openssl dgst -sha256 "$TMPBIN" | awk '{print $2}')"
else
    pr_err "neither sha256sum nor openssl available; cannot verify"
    exit 1
fi
if [ "$ACTUAL" != "$DIGEST" ]; then
    pr_err "sha256 mismatch: got $ACTUAL, expected $DIGEST"
    exit 1
fi
echo "sha256 verified"

# --- install ---
mkdir -p "$INSTALL_DIR"
chmod 755 "$TMPBIN"
mv -f "$TMPBIN" "$INSTALL_DIR/$TOOL"
echo "Installed $INSTALL_DIR/$TOOL ($TAG)"

# --- put the install dir on PATH ---
case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *)
        # Non-root installs land in ~/.local/bin, which is NOT on the default
        # PATH on most distros. Mirror the provider installer: append the
        # export to ~/.bashrc so the tool works immediately, with an opt-out.
        if [ "$no_modify_bashrc" -eq 0 ] && [ "$(id -u)" != "0" ] && [ -n "$HOME" ] && [ -f "$HOME/.bashrc" ]; then
            if awk '/^[[:space:]]*# == urnetwork-tools start[[:space:]]*$/ { code=1; } END { exit code; }' "$HOME/.bashrc"; then
                echo "Adding '$INSTALL_DIR' to ~/.bashrc"
                cat >> "$HOME/.bashrc" <<EOF

# == urnetwork-tools start
export PATH="\$PATH:$INSTALL_DIR"
# == urnetwork-tools end
EOF
                echo "Reload shell:        source ~/.bashrc   (or restart your terminal)"
            else
                echo "~/.bashrc is up-to-date"
            fi
        else
            echo
            echo "NOTE: $INSTALL_DIR is not on your PATH. Add it:"
            echo "  export PATH=\"\$PATH:$INSTALL_DIR\""
        fi
        ;;
esac

echo
echo "Usage:"
echo "  $TOOL providers                          # list provider containers"
echo "  $TOOL update                             # update $TOOL itself"
echo "  $TOOL exec -- <container-command>        # run a command in a container"
