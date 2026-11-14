#!/usr/bin/env bash
# Tests the verdict handling of clamav-scan.sh with a mock scanner.
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
tmp=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/clamav-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/artifacts" "$tmp/empty" "$tmp/db"
# The scanner refuses to run without a database. The harness supplies one so
# the case table tests verdict handling, not signature freshness.
printf 'test\n' > "$tmp/db/main.cvd"
printf 'safe\n' > "$tmp/artifacts/provider"

cat > "$tmp/bin/clamscan" <<'MOCK'
#!/usr/bin/env bash
if [[ $1 == --version ]]; then echo 'ClamAV 0.0.0-test/1/Mon Jan 1 00:00:00 2026'; exit 0; fi
if [[ $1 != --infected ]]; then
  case "$CASE" in
    self-clean) echo "$2: OK"; exit 0 ;;
    self-error) echo 'database unavailable' >&2; exit 2 ;;
    self-wrong) echo "$2: Other.Test FOUND"; exit 1 ;;
    *) echo "$2: Eicar-Test-Signature FOUND"; exit 1 ;;
  esac
fi
echo "clamscan $*" >> "$CALLS"
summary() { printf '\n----------- SCAN SUMMARY -----------\nScanned files: %s\nInfected files: %s\n' "$1" "$2"; }
case "$CASE" in
  detection) echo 'release_tmp/provider: Test.Malware FOUND'; summary 1 1; exit 1 ;;
  scanner-error) echo 'database unavailable' >&2; exit 2 ;;
  error-with-detection) echo 'release_tmp/provider: Test.Malware FOUND'; echo 'read error' >&2; exit 2 ;;
  inconsistent) echo 'release_tmp/provider: Test.Malware FOUND'; exit 0 ;;
  missing-detection) echo 'scanner returned 1 without a match' >&2; exit 1 ;;
  zero-scanned) summary 0 0; exit 0 ;;
  no-summary) exit 0 ;;
  oversize) echo 'release_tmp/big: Heuristics.Limits.Exceeded FOUND'; summary 1 1; exit 1 ;;
  stale-db) echo 'ERROR: Virus database is older than 3 days' >&2; exit 2 ;;
  *) summary 2 0; exit 0 ;;
esac
MOCK
cat > "$tmp/bin/freshclam" <<'MOCK'
#!/usr/bin/env bash
echo freshclam >> "$CALLS"
case "${FRESH:-ok}" in
  fail) echo 'freshclam: mirror unreachable' >&2; exit 1 ;;
  third)
    n=$(( $(cat "$COUNT" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$COUNT"
    [[ $n -ge 3 ]] || { echo 'freshclam: mirror unreachable' >&2; exit 1; } ;;
esac
exit 0
MOCK
printf '#!/usr/bin/env bash\nexit 0\n' > "$tmp/bin/systemctl"
chmod +x "$tmp/bin/"*

run() { # run <case> <target> [extra env...]; sets output and rc
  local case_name=$1 target=$2; shift 2
  : > "$tmp/calls"; rm -f "$tmp/count"
  if output=$(env PATH="$tmp/bin:$PATH" CASE="$case_name" CALLS="$tmp/calls" COUNT="$tmp/count" \
      CLAMAV_NO_SUDO=1 CLAMAV_RETRY_DELAY=0 CLAMAV_DB_DIR="$tmp/db" CLAMAV_REPORT_FILE="$tmp/$case_name.report" \
      "$@" bash "$root/.github/scripts/clamav-scan.sh" "$target" 2>&1); then rc=0; else rc=$?; fi
}

for case_name in clean detection scanner-error self-clean self-error self-wrong inconsistent missing-detection error-with-detection zero-scanned no-summary oversize stale-db; do
  run "$case_name" "$tmp/artifacts"
  report="$tmp/$case_name.report"
  if [[ $case_name == clean ]]; then
    [[ $rc -eq 0 && $output == *'ClamAV: clean (2 files scanned)'* && $output == *'EICAR self-test passed'* && $(<"$report") == *'Clean: no malware detected in 2 files.'* ]] || { echo "FAIL $case_name: $output"; exit 1; }
  else
    [[ $rc -ne 0 && $output != *'ClamAV: clean'* ]] || { echo "FAIL $case_name: rc=$rc $output"; exit 1; }
  fi
  case "$case_name" in
    detection|oversize) [[ $output == *' FOUND'* && $output == *'detected malware'* && $(<"$report") == *'Detection:'* ]] ;;
    scanner-error|stale-db) [[ $output == *'scanner failure'* ]] ;;
    self-*) [[ $output == *'EICAR self-test failed'* && $(<"$report") == *'EICAR self-test failed'* ]] ;;
    error-with-detection) [[ $output == *'Test.Malware FOUND'* && $output == *'scanner failure'* ]] ;;
    inconsistent|missing-detection) [[ $output == *'inconsistent result'* && $(<"$report") == *'Scanner failure or inconsistent result'* ]] ;;
    zero-scanned|no-summary) [[ $output == *'expected at least 1'* && $(<"$report") == *'Scanner failure: the scan counted'* ]] ;;
  esac || { echo "FAIL $case_name: assertion failed (rc=$rc) $output"; exit 1; }
  printf 'PASS %s (exit %s)\n' "$case_name" "$rc"
done

# The definitions are refreshed before the scan, and the scan is strict.
run clean "$tmp/artifacts"
[[ $(sed -n 1p "$tmp/calls") == freshclam && $(sed -n 2p "$tmp/calls") == clamscan* ]] || { echo "FAIL freshclam did not run before the scan: $(<"$tmp/calls")"; exit 1; }
for flag in --alert-exceeds-max=yes --fail-if-cvd-older-than= --max-filesize= --max-scansize=; do
  [[ $(<"$tmp/calls") == *"$flag"* ]] || { echo "FAIL scan did not pass $flag: $(<"$tmp/calls")"; exit 1; }
done
echo 'PASS refresh-before-scan'

# A refresh failure is a scanner failure, even when an old database exists.
run clean "$tmp/artifacts" FRESH=fail
[[ $rc -ne 0 && $output == *'could not be refreshed'* && $output != *'ClamAV: clean'* && $(grep -c clamscan "$tmp/calls") -eq 0 ]] || { echo "FAIL refresh-fail: rc=$rc $output"; exit 1; }
echo 'PASS refresh-fail (exit 1, no scan ran)'

# A mirror blip is retried.
run clean "$tmp/artifacts" FRESH=third
[[ $rc -eq 0 && $output == *'ClamAV: clean'* && $(grep -c freshclam "$tmp/calls") -eq 3 ]] || { echo "FAIL refresh-retry: rc=$rc $output"; exit 1; }
echo 'PASS refresh-retry'

run clean "$tmp/empty"
if [[ $rc -eq 0 || $output != *'scan target contains no files'* || $output == *'ClamAV: clean'* ]]; then
  echo "FAIL empty scan target: rc=$rc $output"
  exit 1
fi
echo 'PASS empty-target (exit 1)'
