#!/usr/bin/env bash
# H3 + miner workspace scaffold (Phase 0 of MIGRATION_PLAN.md).
# Clones the v2026 sibling workspace so `go build ./...` works in sn/miner.
# Usage: ./scripts/h3-workspace.sh [workspace-dir]
set -euo pipefail

WS="${1:-$HOME/h3-workspace}"
mkdir -p "$WS" && cd "$WS"

REPOS=(
  "https://github.com/urnetwork/connect.git"
  "https://github.com/urfoundation/sn.git"
  "https://github.com/urnetwork/server.git"
  "https://github.com/urnetwork/warp.git"
  "https://github.com/urnetwork/operator-proxy.git"
  "https://github.com/urnetwork/proxy.git"
  "https://github.com/urnetwork/userwireguard.git"
)

for repo in "${REPOS[@]}"; do
  name="${repo##*/}"
  name="${name%.git}"
  if [[ ! -d "$name" ]]; then
    echo "== cloning $name"
    git clone --depth=1 "$repo" "$name"
  else
    echo "== $name present"
  fi
done

echo "== smoke build: urprovider"
cd "$WS/sn"
go build -o /tmp/urprovider ./miner 2>&1 | head -20 \
  && echo "OK: /tmp/urprovider" || echo "BUILD FAILED (see above — likely missing sibling repo or branch)"