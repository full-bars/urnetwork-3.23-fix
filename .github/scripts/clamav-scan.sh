#!/usr/bin/env bash
# Scan release files with ClamAV. The scan fails closed.
#
# It exits 0 only when all of these hold:
#   1. The signature definitions were refreshed with freshclam just now.
#   2. The EICAR self-test was detected, so the scanner is active.
#   3. The scan target holds files, and the scan counted at least one of them.
#   4. The scan found nothing and reported no error.
# A detection, a scanner failure and a stale or missing database all exit 1.
#
# usage: clamav-scan.sh <directory-or-file>
# env:   CLAMAV_REPORT_FILE      markdown report path (required)
#        CLAMAV_SKIP_REFRESH=1   skip freshclam (tests only)
#        CLAMAV_NO_SUDO=1        run freshclam without sudo (tests only)
#        CLAMAV_RETRY_DELAY      seconds per freshclam retry step (default 10)
#        CLAMAV_DB_DIR           signature directory (default /var/lib/clamav)
#        CLAMAV_MAX_DB_AGE_DAYS  fail if the newest database is older (default 3)
#        CLAMAV_MIN_FILES        fail if fewer files were scanned (default 1)
#        CLAMAV_MAX_FILESIZE, CLAMAV_MAX_SCANSIZE   scan limits
# A file over a limit is reported as a detection (--alert-exceeds-max), so a
# large file is never skipped and assumed clean.
set -euo pipefail

report=${CLAMAV_REPORT_FILE:?CLAMAV_REPORT_FILE is required}
target=${1:?scan target is required}
db_dir=${CLAMAV_DB_DIR:-/var/lib/clamav}
max_db_age=${CLAMAV_MAX_DB_AGE_DAYS:-3}
min_files=${CLAMAV_MIN_FILES:-1}
max_filesize=${CLAMAV_MAX_FILESIZE:-1000M}
max_scansize=${CLAMAV_MAX_SCANSIZE:-4000M}

eicar=$(mktemp)
trap 'rm -f "$eicar"' EXIT
# EICAR contains literal dollar signs and must not undergo shell expansion.
# shellcheck disable=SC2016
printf 'X5O!P%%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*' > "$eicar"
printf '### ClamAV\n' > "$report"

as_root() {
  if [[ $(id -u) -eq 0 || ${CLAMAV_NO_SUDO:-0} == 1 ]]; then "$@"; else sudo "$@"; fi
}

# Fetch the newest definitions. The package starts a freshclam daemon that
# holds the update lock, so stop it first. Three attempts cover a mirror blip.
refresh_definitions() {
  if [[ ${CLAMAV_SKIP_REFRESH:-0} == 1 ]]; then
    echo 'Skipping the definition refresh (CLAMAV_SKIP_REFRESH=1)'
    return 0
  fi
  if ! command -v freshclam >/dev/null 2>&1; then
    echo '::error::freshclam is not installed, so the definitions cannot be refreshed'
    return 1
  fi
  as_root systemctl stop clamav-freshclam >/dev/null 2>&1 || true
  local attempt
  for attempt in 1 2 3; do
    if as_root freshclam --stdout; then
      return 0
    fi
    echo "freshclam attempt $attempt failed"
    if [[ $attempt -lt 3 ]]; then sleep $((attempt * ${CLAMAV_RETRY_DELAY:-10})); fi
  done
  return 1
}

if ! refresh_definitions; then
  echo '::error::ClamAV definitions could not be refreshed; refusing to scan with unknown signatures'
  printf 'Scanner failure: the definitions could not be refreshed.\n' >> "$report"
  exit 1
fi

have_db() {
  compgen -G "$db_dir/*.cvd" >/dev/null 2>&1 || compgen -G "$db_dir/*.cld" >/dev/null 2>&1
}
if ! have_db; then
  echo "::error::ClamAV database unavailable in $db_dir; a scan would report clean without checking anything"
  printf 'Scanner failure: signature database unavailable.\n' >> "$report"
  exit 1
fi

scanner_version=$(clamscan --version 2>/dev/null | head -n 1 || true)
echo "Scanner: ${scanner_version:-unknown}"
printf 'Scanner: %s\n\n' "${scanner_version:-unknown}" >> "$report"

if self_output=$(clamscan --no-summary "$eicar" 2>&1); then
  self_rc=0
else
  self_rc=$?
fi
printf '%s\n' "$self_output"
eicar_pattern='[Ee][Ii][Cc][Aa][Rr][^[:cntrl:]]* FOUND'
self_ok=0
if [[ $self_rc -eq 1 && $self_output =~ $eicar_pattern ]]; then
  self_ok=1
  echo 'EICAR self-test passed (scanner detected EICAR)'
else
  echo "::error::EICAR self-test failed (exit $self_rc); scanner is not trusted"
  printf 'Scanner failure: EICAR self-test failed (exit %s).\n' "$self_rc" >> "$report"
fi

if [[ -d $target ]]; then
  if ! first_file=$(find "$target" -type f -print -quit 2>&1); then
    echo "::error::ClamAV could not inspect the scan target: $first_file"
    echo 'Scanner failure: could not inspect the scan target.' >> "$report"
    exit 1
  elif [[ -z $first_file ]]; then
    echo '::error::ClamAV scan target contains no files'
    echo 'Scanner failure: scan target contains no files.' >> "$report"
    exit 1
  fi
fi

if scan_output=$(clamscan --infected --alert-exceeds-max=yes \
    --max-filesize="$max_filesize" --max-scansize="$max_scansize" \
    --fail-if-cvd-older-than="$max_db_age" -r "$target" 2>&1); then
  scan_rc=0
else
  scan_rc=$?
fi
if [[ -n $scan_output ]]; then
  printf '%s\n' "$scan_output"
  # Markdown fences are literal, not shell command substitutions.
  # shellcheck disable=SC2016
  printf '```text\n%s\n```\n' "$scan_output" >> "$report"
fi

scanned=$(sed -n 's/^Scanned files: *//p' <<<"$scan_output" | head -n 1)

if [[ $scan_rc -eq 1 && $scan_output == *' FOUND'* ]]; then
  echo '::error::ClamAV detected malware in release artifacts'
  echo 'Detection: ClamAV found malware in release artifacts; see details above.' >> "$report"
  exit 1
elif [[ $scan_rc -ne 0 || $scan_output == *' FOUND'* ]]; then
  echo "::error::ClamAV scanner failure or inconsistent result (exit $scan_rc)"
  printf 'Scanner failure or inconsistent result (exit %s); see scan log.\n' "$scan_rc" >> "$report"
  exit 1
elif [[ $self_ok -ne 1 ]]; then
  exit 1
elif ! [[ $scanned =~ ^[0-9]+$ ]] || [[ $scanned -lt $min_files ]]; then
  echo "::error::ClamAV scanned ${scanned:-no} files; expected at least $min_files"
  printf 'Scanner failure: the scan counted %s files, expected at least %s.\n' "${scanned:-no}" "$min_files" >> "$report"
  exit 1
fi

echo "ClamAV: clean ($scanned files scanned)"
printf 'Clean: no malware detected in %s files. Definitions were refreshed before the scan and the EICAR self-test passed.\n' "$scanned" >> "$report"
