#!/usr/bin/env bash
# Deployment classification uses every file changed by each mapped commit.
# A merge commit is compared with its first parent, so the files a merged pull
# request brought in are counted.
# Provider, internal and root Go source plus Docker and compose files require
# redeploy. Installer scripts require installer. Redeploy takes precedence.
# All other changes are no redeploy. Test Go files do not require redeploy.
set -euo pipefail
root=$(git rev-parse --show-toplevel)
exec python3 "$root/scripts/release-notes.py" "$@"
