#!/usr/bin/env bash
# Partition the module's packages into disjoint test shards.
#
# Balance, not hashing: the root package and ./provider carry roughly three
# quarters of the test files between them, so a modulo-by-index split would
# leave one shard doing most of the work and the wall-clock win with it.
# Shard 0 takes the root package, shard 1 takes the provider tree, shard 2
# takes everything else — INCLUDING any package added later, so a new package
# can never fall outside the union. `--verify` proves that on every run: the
# failure mode this guards against is a shard silently testing nothing.
set -euo pipefail

SHARD_COUNT=3

mod=$(go list -m)
all=$(go list ./... | sort)

# Prefix matching via awk index(), not grep -E: the module path contains dots
# that would otherwise need escaping in a regex.
shard_pkgs() {
  printf '%s\n' "$all" | awk -v mod="$mod" -v idx="$1" '
    {
      pkg = $0
      if (pkg == mod)                                   s = 0
      else if (pkg == mod "/provider" ||
               index(pkg, mod "/provider/") == 1)       s = 1
      else                                              s = 2
      if (s == idx) print pkg
    }'
}

if [ "${1:-}" = "--verify" ]; then
  union=$( for i in $(seq 0 $((SHARD_COUNT - 1))); do shard_pkgs "$i"; done | sort )
  if [ "$union" != "$all" ]; then
    echo "FAIL: shard partition does not cover every package"
    diff <(printf '%s\n' "$all") <(printf '%s\n' "$union") || true
    exit 1
  fi
  dupes=$(printf '%s\n' "$union" | uniq -d)
  if [ -n "$dupes" ]; then
    echo "FAIL: package assigned to more than one shard:"
    printf '%s\n' "$dupes"
    exit 1
  fi
  echo "shard partition covers all $(printf '%s\n' "$all" | wc -l | tr -d ' ') packages, no overlap"
  exit 0
fi

shard_pkgs "${1:?usage: test_shard.sh <shard-index>|--verify}"
