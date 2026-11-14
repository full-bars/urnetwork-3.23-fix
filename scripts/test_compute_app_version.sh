#!/bin/bash
# Checks the app_version string a release reports to the backend.
# Uses a fixed commit time, so the result does not depend on the clock or on git.
set -u
cd "$(dirname "$0")/.." || exit 2
fail=0
check() {
  local name="$1" want="$2" got="$3"
  if [ "$got" = "$want" ]; then echo "ok   $name"; else echo "FAIL $name: want '$want' got '$got'"; fail=1; fi
}
# 1794654559 is 2026-11-14 11:09:19 UTC. (1794654559 - 1684800000) * 10 = 1098545590.
check "35.10 shape" "2026.11.14-1098545590-35.10-meso" "$(scripts/compute-app-version.sh v3.23.0-fix.35.10 1794654559)"
check "35.9 shape" "2026.11.14-1098545590-35.9-meso" "$(scripts/compute-app-version.sh v3.23.0-fix.35.9 1794654559)"
check "no padding on the date" "2026.1.5-" "$(scripts/compute-app-version.sh v3.23.0-fix.35.10 1767614400 | cut -c1-9)"
check "other tags print nothing" "" "$(scripts/compute-app-version.sh v2026.11.9-1094055530-meso 1794654559)"
len=$(scripts/compute-app-version.sh v3.23.0-fix.35.10 1794654559 | wc -c)
if [ "$((len - 1))" -le 64 ]; then echo "ok   length within 64"; else echo "FAIL length $((len - 1))"; fail=1; fi
exit $fail
