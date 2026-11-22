#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fixture=scripts/testdata/release-notes/good.md
lint=scripts/release-notes-lint.sh
"$lint" "$fixture"
fail_case() {
  local name=$1 line=$2
  cp "$fixture" "$tmp/bad.md"
  printf '\n%s\n' "$line" >> "$tmp/bad.md"
  if "$lint" "$tmp/bad.md" >"$tmp/out" 2>&1; then
    echo "FAIL: $name was accepted" >&2
    exit 1
  fi
  echo "PASS: $name rejected"
}
fail_case 'em dash' 'Text — text.'
fail_case 'semicolon' 'Text; text.'
fail_case 'upstream link' '[link](https://github.com/urnetwork/connect)'
fail_case 'external link' '[link](https://example.com/)'
fail_case 'model name' 'The model is Codex.'
fail_case 'exactly' 'There are exactly two.'
fail_case 'contrast' 'Use this, not that.'
fail_case 'bullet first person' '- I made it.'
fail_case 'unknown commit' '[commit](https://github.com/full-bars/urnetwork-3.23-fix/commit/0000000000000000000000000000000000000000)'
fail_case 'line length' "$(printf '%0601d' 0)"
sed 's/\[redeploy\]//' "$fixture" > "$tmp/bad.md"
if "$lint" "$tmp/bad.md" >"$tmp/out" 2>&1; then
  echo 'FAIL: missing deploy tag accepted' >&2
  exit 1
fi
echo 'PASS: missing deploy tag rejected'
sed 's/I made the release/I I I made the release/' "$fixture" > "$tmp/bad.md"
if "$lint" "$tmp/bad.md" >"$tmp/out" 2>&1; then
  echo 'FAIL: excessive first person accepted' >&2
  exit 1
fi
echo 'PASS: excessive first person rejected'
echo 'Lint fixture checks passed.'
