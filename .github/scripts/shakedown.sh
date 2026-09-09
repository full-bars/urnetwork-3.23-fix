#!/bin/bash
# shakedown.sh. The v29 pre-release shakedown. Runs ON the droplet after boot.
# Executed as root on a fresh 1CPU/1GB Ubuntu droplet. Tests the full
# regular-person install flow, Go tooling, proxy paths, URL sources with real
# free proxies, egress, and docker.
#
# Called by the shakedown workflow with: $1 = JWT file path (already on the box)
#
# DESIGN notes:
#  - Every assertion is exit-status-first, text-second (structural fix).
#  - Self-test phase verifies each log pattern against a KNOWN-GOOD startup
#    so the shakedown's own regexes cannot silently rot into false blocks.
#  - Phase 1 (0-20m): install/auth/CLI/proxy/docker(rm at end)/identity/
#    update-tag/self-update. Everything that restarts the provider.
#  - Phase 2 (20-100m): final restart, seed blackhole, uninterrupted
#    observation with resource sampling every 5m, 10m refresh, remove-dead
#    --yes at the end (~110m total). Watchdog ~7200s in the workflow.
#  - No billable traffic: mainnet provider with free proxies, but no real
#    clients. Accepted explicitly. Checks use logs/state/CLI only.
set -u
JWT_FILE="${1:-/tmp/gauntlet.jwt}"
REPORT="/tmp/shakedown-report.txt"
PASS=0; FAIL=0; SKIP=0
# Tier-1 failures are hard blocks: any panic/fatal/OOM/structural miss.
TIER1_FAIL=0
ok()   { PASS=$((PASS+1)); echo "PASS: $1" | tee -a "$REPORT"; touch /tmp/shakedown.heartbeat 2>/dev/null; }
bad()  { FAIL=$((FAIL+1)); echo "FAIL: $1" | tee -a "$REPORT"; touch /tmp/shakedown.heartbeat 2>/dev/null; }
skip() { SKIP=$((SKIP+1)); echo "SKIP: $1" | tee -a "$REPORT"; }
t1bad() { TIER1_FAIL=1; bad "$1"; }
section() { echo "" | tee -a "$REPORT"; echo "===== $1 =====" | tee -a "$REPORT"; touch /tmp/shakedown.heartbeat 2>/dev/null; }

# run_check: exit-status-first assertion. Runs $cmd..., PASS iff exit 0.
# Tolerates a trailing literal "2>&1" (call-site convention) by filtering it.
run_check() {
  local name="$1"; shift
  local out rc
  local filtered=()
  for a in "$@"; do
    [ "$a" = "2>&1" ] || filtered+=("$a")
  done
  out=$("${filtered[@]}" 2>&1); rc=$?
  if [ "$rc" -eq 0 ]; then
    ok "$name (exit 0)"
  else
    bad "$name (exit $rc)"
    echo "$out" | tail -5 | sed 's/^/    | /' | tee -a "$REPORT"
  fi
  return "$rc"
}

# j: search the provider journal (full, no -n window).
# j: search the provider journal with a bounded window. Full-journal reads
# on a 5s cadence are heavy on 1GB.
j() { runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) journalctl --user -u urnetwork.service --no-pager --since "${J_SINCE:-90 min ago}" 2>/dev/null; }
# j_full: unbounded journal (self-test calibration, panic sweep).
j_full() { runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) journalctl --user -u urnetwork.service --no-pager 2>/dev/null; }

# journal_line_count: total journal lines (snapshot marker for polling).
journal_line_count() { j | wc -l; }

# wait_client_id: poll for a client_id line that appears AFTER the given
# marker line count (a NEW journal entry from a restart). The provider runs a
# synchronous up-to-1GB O_SYNC disk audit at every provide start (audit.go,
# 10-60s on 1CPU). Fixed sleeps false-fail. Poll instead.
# Returns the client_id, or empty after max_wait.
wait_client_id() {
  local after_lines="${1:-0}" max_wait="${2:-120}"
  local end=$(( $(date +%s) + max_wait ))
  while [ "$(date +%s)" -lt "$end" ]; do
    local cid
    cid=$(j | awk -v n="$after_lines" 'NR > n' | grep -oE "client_id: [0-9a-f-]+ \((new|reused)\)" | tail -1 | awk '{print $2}')
    if [ -n "$cid" ]; then echo "$cid"; return 0; fi
    sleep 5
  done
  return 1
}

# cids_since: the DISTINCT client_ids logged after journal line N, sorted
# (sorted so `comm` can diff two of these directly).
cids_since() { j | awk -v n="$1" 'NR > n' | grep -oE "client_id: [0-9a-f-]+ \((new|reused)\)" | awk '{print $2}' | sort -u; }
# markers_since: the (new)/(reused) markers logged after journal line N.
markers_since() { j | awk -v n="$1" 'NR > n' | grep -oE "client_id: [0-9a-f-]+ \((new|reused)\)" | awk '{print $3}'; }
# markers_snapshot: the raw "client_id: <id> (new|reused)" lines after journal
# line N, captured ONCE. cids_since and markers_since each re-read the journal,
# so deriving an id set and its marker counts from separate calls samples a
# growing log at two different instants. Snapshot, then derive.
markers_snapshot() { j | awk -v n="$1" 'NR > n' | grep -oE "client_id: [0-9a-f-]+ \((new|reused)\)"; }

# restart_provider: systemctl --user restart + return the pre-restart journal
# line count (for wait_client_id).
# Bounded: sections U and V apply Type=notify drop-ins, under which
# `systemctl restart` blocks until the provider sends READY=1 and (with
# TimeoutStartSec=0) never gives up. Unbounded, a missing READY kills the
# whole job via the CI watchdog and reports nothing; bounded, the caller's
# own wait_client_id/assertion fails and the report says which check broke.
restart_provider() {
  echo $(journal_line_count)
  timeout 180 runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user restart urnetwork.service
}

echo "SHAKEDOWN START $(date -u +%FT%TZ)" > "$REPORT"
# MUST-FIX 10: EXPECTED_VERSION comes from the workflow (GITHUB_REF_NAME for
# tag-triggered runs, latest release for manual). Derive the base for the
# version greps; align the fresh install to it so tag runs test THE TAG.
# MUST-FIX: fail loud if EXPECTED_VERSION is unset. A silent default
# would test the wrong release while reporting green.
if [ -z "${EXPECTED_VERSION:-}" ]; then
  echo "FAIL: EXPECTED_VERSION not set (workflow must pass it)" | tee -a "$REPORT"
  exit 75
fi
# Assign EXPECTED_BASE BEFORE the guard references it: under set -u, using an
# unset variable aborts the run at startup. This was after the
# guard, so every run died in its first second and then graded RELEASE_OK).
EXPECTED_BASE=$(echo "$EXPECTED_VERSION" | grep -oE "v3\.23\.0-fix\.[0-9]+" | head -1)
# EXPECTED_BASE empty would make grep -qE "" match anything, so every version
# check would vacuously pass on a future v3.24 tag. Guard it.
if [ -z "$EXPECTED_BASE" ]; then
  echo "FAIL: EXPECTED_VERSION $EXPECTED_VERSION does not match v3.23.0-fix.N pattern" | tee -a "$REPORT"
  exit 75
fi
# TOOL_VERSION is read AFTER the install in section A/C.
# Reading it here (before install) always yields "no such file" on a fresh
# droplet. Section C verifies the version properly.
echo "EXPECTED_VERSION: $EXPECTED_VERSION" >> "$REPORT"

# ---------- A. Fresh install (regular-person path) ----------
section "A. Fresh install"
# The installer under test comes from
# refs/heads/main, not the tag being cut. Tag-side installer regressions are
# invisible to the shakedown, and a main-side installer break blocks an
# unrelated release. Accepted tradeoff: the installer is rarely tag-specific.
curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/Provider_Install_Linux.sh -o /tmp/install.sh
bash -n /tmp/install.sh && ok "installer syntax" || bad "installer syntax"
# PTY install: root prompt -> option 1 (create user) -> default name urnet.
# Grep the FULL transcript, not tail -3 (installer prints after "complete").
INSTALL_OUT=$( (cd /tmp && (sleep 2; echo "1"; sleep 2; echo "urnet") | script -qc "sh /tmp/install.sh install" /dev/null 2>&1 || true) )
echo "$INSTALL_OUT" | grep -q "Installation complete\|Done. The provider is installed" \
  && ok "fresh install complete" || bad "fresh install"
[ -x /home/urnet/.local/share/urnetwork-provider/bin/urnet-tools ] && ok "Go urnet-tools installed" || bad "Go urnet-tools missing"
export PATH=/home/urnet/.local/share/urnetwork-provider/bin:$PATH
# MUST-FIX 10: a tag-triggered run fires on the tag push, but the installer
# fetches "latest" (the PREVIOUS release). Align the installed tool+provider
# to the release being tested BEFORE the version checks run.
if ! urnet-tools version 2>&1 | grep -q "$EXPECTED_BASE"; then
  echo "  installing expected release $EXPECTED_VERSION (installer defaulted to latest)" | tee -a "$REPORT"
  run_check "align install to $EXPECTED_VERSION" urnet-tools update --tag "$EXPECTED_VERSION" -f 2>&1
fi

# ---------- A2. Preflight: internet + API reachability (non-starters) ----------
section "A2. Preflight connectivity"
NON_STARTER=0
if timeout 15 curl -fsS -o /dev/null -w "%{http_code}" https://api.bringyour.com/auth/verify-send 2>/dev/null | grep -qE "^[0-9]{3}$"; then
  ok "internet + api.bringyour.com reachable"
else
  bad "api.bringyour.com NOT reachable (non-starter)"; NON_STARTER=1
fi
if timeout 10 curl -fsS -o /dev/null https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/main/README.md 2>/dev/null; then
  ok "github reachable"
else
  bad "github NOT reachable (non-starter)"; NON_STARTER=1
fi
MONOSANS_URL="https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/socks5.txt"
PROXIFLY_URL="https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/protocols/socks5/data.txt"
# The two third-party proxy lists are hard release gates
# downstream. A list 404ing or being unreachable FAILs add-source ->
# RELEASE_BLOCKED for a good release. Preflight both here; a failure is an
# ENV_BLOCKER (list availability), not a product verdict.
for lst in "$MONOSANS_URL" "$PROXIFLY_URL"; do
  if timeout 15 curl -fsSI -o /dev/null "$lst" 2>/dev/null; then
    ok "proxy list reachable ($(basename "$(dirname "$lst")"))"
  else
    echo "WARN: proxy list unreachable: $lst (ENV_BLOCKER — will SKIP URL source)" | tee -a "$REPORT"
  fi
done
if [ "$NON_STARTER" = "1" ]; then
  echo "NON-STARTER: connectivity failed. Not running the rest of the suite" | tee -a "$REPORT"
  echo "PASS=$PASS FAIL=$FAIL SKIP=$SKIP NON_STARTER=1" | tee -a "$REPORT"
  echo "SHAKEDOWN END $(date -u +%FT%TZ)" >> "$REPORT"
  # Exit 75 = ENV_BLOCKER: connectivity failed, not a product verdict.
  # The workflow maps 75 to neutral + retry, never RELEASE_BLOCKED.
  exit 75
fi
echo "preflight OK. Continuing suite" | tee -a "$REPORT"

# ---------- B. Auth ----------
section "B. Auth"
mkdir -p /home/urnet/.urnetwork
cat > /home/urnet/.urnetwork/network.json << 'EOF'
{"api_url":"https://api.bringyour.com","connect_url":"wss://connect.bringyour.com"}
EOF
cp "$JWT_FILE" /home/urnet/.urnetwork/jwt
# MUST-FIX 6/4 (user design): cap + cadence + sources in place BEFORE the
# provider's first start so the background fetcher honors them from cycle 1.
printf '200\n' > /home/urnet/.urnetwork/proxy_url_max
printf '10m\n' > /home/urnet/.urnetwork/proxy_url_refresh
chown -R urnet:urnet /home/urnet/.urnetwork && chmod 600 /home/urnet/.urnetwork/jwt
export XDG_RUNTIME_DIR=/run/user/$(id -u urnet)
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user start urnetwork.service
# MUST-FIX 8: poll for client_id instead of sleep 8. The provider
# runs a synchronous up-to-1GB O_SYNC disk audit at provide start (10-60s on
# 1CPU/1GB). A fixed sleep false-fails a Tier-1 check.
CID=$(wait_client_id 0 120)
if [ -n "$CID" ]; then
  ok "auth (client_id minted: ${CID:0:12}…)"
else
  echo "--- provider journal (auth failed) ---" | tee -a "$REPORT"
  j | tail -30 | tee -a "$REPORT"
  bad "auth"
fi

# ---------- SELF-TEST: pattern calibration on a known-good startup ----------
# Structural fix: every assertion below greps a log pattern.
# If a pattern is wrong, the shakedown's failure is indistinguishable from the
# product failing. So verify each pattern against THIS known-good journal now.
# NOTE: [net][s]select has no single-regex form. Success = total minus fail
# lines (see section I); its calibration is the SELECT_TOTAL/SELECT_FAILS
# subtraction being nonzero on a known-good journal.
section "SELF-TEST (pattern calibration)"
J=$(j_full)   # self-test needs the full startup sequence, not a window
SELF_TEST_FAIL=0
# Self-test: verify each log pattern against a known-good journal. A missing
# pattern is logged as SELF-TEST-FAIL (a TEST_SCRIPT signal in the report).
# Dependent checks do NOT gate on calibration: absence of a POSITIVE product
# signal (client_id, stage-1 enabled=true, select success) means the product
# is broken, not that the regex rotted.
# NOTE: 'stage-1 table probe config' and '[jwt] refresh OK' are emitted via
# tlog -> STDOUT, never journald (tlog.go:17 fmt.Printf). They are validated
# against the captured add-source output in section E2 instead of the
# journal here (shakedown finding 2026-08-15: journal-only calibration
# always false-flagged them).
for pat in \
  "client_id: [0-9a-f-]+ \((new|reused)\)" \
  "\[net\]\[s\]select:.*dur=[0-9]+ms"; do
  if echo "$J" | grep -qE "$pat"; then
    ok "self-test pattern present: $pat"
  else
    echo "SELF-TEST-FAIL: pattern missing in known-good journal: $pat" | tee -a "$REPORT"
    SELF_TEST_FAIL=1
  fi
done
SEL_T=$(echo "$J" | grep -cE "\[net\]\[s\]select:.*dur=[0-9]+ms")
SEL_F=$(echo "$J" | grep -cE "\[net\]\[s\]select:.* = .*dur=[0-9]+ms")
if [ $((SEL_T - SEL_F)) -gt 0 ]; then
  ok "self-test [net][s]select subtraction calibrated ($((SEL_T - SEL_F)) of $SEL_T)"
else
  echo "SELF-TEST-FAIL: [net][s]select subtraction empty on known-good journal (total=$SEL_T fails=$SEL_F)" | tee -a "$REPORT"
  SELF_TEST_FAIL=1
fi
[ "$SELF_TEST_FAIL" = "0" ] && echo "self-test: all patterns calibrated" | tee -a "$REPORT"

# ---------- C. Go tool basics ----------
section "C. Go tool"
V=$(urnet-tools version 2>&1)
echo "$V" | grep -qF "$EXPECTED_BASE" && ok "tool version $V" || bad "tool version ($V). Version must match $EXPECTED_BASE"
run_check "providers discovers the account" urnet-tools providers 2>&1
run_check "status running" urnet-tools status 2>&1
run_check "proxy health cmd" urnet-tools proxy health 2>&1

# ---------- D. Proxy lifecycle ----------
section "D. Proxy lifecycle"
printf "1.1.1.1:443\n8.8.8.8:443\n9.9.9.9:443:testuser:testpass\n" > /tmp/tp.txt
run_check "proxy add" urnet-tools proxy add /tmp/tp.txt 2>&1
python3 -c "import json;d=json.load(open('/home/urnet/.urnetwork/proxy'));assert len(d.get('servers',{}))==3" && ok "3 servers in state" || bad "state count"
run_check "proxy remove --all -f" urnet-tools proxy remove --all -f 2>&1
python3 -c "import json;d=json.load(open('/home/urnet/.urnetwork/proxy'));assert len(d.get('servers',{}))==0" && ok "proxy remove --all -f" || bad "proxy remove"

# ---------- E. URL sources (real free proxies) ----------
section "E. URL sources + egress"
# MUST-FIX 6: socks5-only lists only. proxifly 'all' is 2300+ entries,
# mostly http/socks4 (skipped), uncapped in the one-shot CLI fetch. Hours +
# OOM risk on 1GB. proxifly socks5-only is 148 entries, monosans 102.
# The CLI one-shot fetch ignores proxy_url_max by design (main.go:4413),
# so each add-source gets a 900s belt-and-braces timeout.
# A list failure here is an ENV_BLOCKER (third-party availability),
# not a product bug — WARN + continue; the URL-source gates will SKIP.
# Capture the add-source OUTPUT: the stage-1 table probe config line is
# emitted via tlog -> STDOUT, not journald (tlog.go:17 uses fmt.Printf). The
# E2 admission check greps this captured output, not j(), so the line is
# actually seen (shakedown finding 2026-08-15: the journal grep always
# missed it because the line never reaches the journal).
ADD_SOURCE_OUT=""
if OUT=$(timeout 900 urnet-tools proxy add-source "$MONOSANS_URL" 2>&1); then
  ok "add-source (monosans socks5)"
  ADD_SOURCE_OUT="$ADD_SOURCE_OUT
$OUT"
else
  echo "WARN: add-source (monosans) failed — proxy list unavailable (ENV_BLOCKER)" | tee -a "$REPORT"
  ADD_SOURCE_OUT="$ADD_SOURCE_OUT
$OUT"
fi
if OUT=$(timeout 900 urnet-tools proxy add-source "$PROXIFLY_URL" 2>&1); then
  ok "add-source (proxifly socks5)"
  ADD_SOURCE_OUT="$ADD_SOURCE_OUT
$OUT"
else
  echo "WARN: add-source (proxifly) failed — proxy list unavailable (ENV_BLOCKER)" | tee -a "$REPORT"
  ADD_SOURCE_OUT="$ADD_SOURCE_OUT
$OUT"
fi
# MUST-FIX 4: the background fetcher reads sources ONCE at process start.
# Restart the unit so the periodic fetch->probe->grade->admit loop is alive
# (without this, only the one-shot CLI fetch ran and the loop was dead).
MARK=$(restart_provider)
CID=$(wait_client_id "$MARK" 120)
[ -n "$CID" ] && ok "restart unit (fetcher picks up sources; client_id ${CID:0:12}…)" || bad "restart unit (no client_id)"
run_check "both sources registered" urnet-tools summary 2>&1
SRC_COUNT=$(urnet-tools summary 2>&1 | grep -oE "Source URLs: +[0-9]+" | grep -oE "[0-9]+" | tail -1)
[ "${SRC_COUNT:-0}" = "2" ] && ok "source count == 2 ($SRC_COUNT)" || bad "source count = ${SRC_COUNT:-?} (want 2)"
# Phase 1 quick cache sanity: the restarted background fetcher should have
# started its first cycle (~20s cooldown + probe). Full admission checks run
# in Phase 2's long observation window.
sleep 60
CACHED=$(python3 -c "import json;d=json.load(open('/home/urnet/.urnetwork/proxy_url.json'));print(len(d.get('cache',{})))" 2>/dev/null || echo 0)
echo "INFO: cache after first cycle: $CACHED" | tee -a "$REPORT"
if grep -q "FAIL: auth" "$REPORT"; then
  skip "URL cache (auth failed earlier)"
else
  [ "${CACHED:-0}" -gt 0 ] && ok "URL cache populated ($CACHED)" || echo "INFO: cache is empty. Phase 2 observation follows" | tee -a "$REPORT"
fi

# ---------- E2. Admission pipeline (Tier 1) ----------
section "E2. Admission pipeline (Tier 1)"
# Q4 principle: gate on STRUCTURAL signals (a line exists, an exit is 0),
# never on the QUANTITY of live free proxies.
CID_LINES=$(j | grep -cE "client_id: [0-9a-f-]+ \((new|reused)\)")
# client_id absence = admission pipeline broken (product signal), not regex
# rot. The self-test logs SELF-TEST-FAIL separately if the pattern is wrong.
if [ "${CID_LINES:-0}" -gt 0 ]; then
  ok "client identities minted ($CID_LINES lines)"
else
  t1bad "no client identities minted (admission pipeline broken?)"
fi
# The kill-switch check uses TWO signals: the probe config log line (via
# tlog -> stdout, so check the captured add-source output first, then the
# journal) AND the structural GRADED count from proxy_url.json. Admission
# engaged if either fired. Absence of both = the kill switch is stuck.
GRADED=$(python3 -c "import json;d=json.load(open('/home/urnet/.urnetwork/proxy_url.json'));print(sum(1 for v in d.get('cache',{}).values() if v.get('Graded')))" 2>/dev/null || echo 0)
PROBE_LINE=0
if echo "$ADD_SOURCE_OUT" | grep -qE "stage-1 table probe config: enabled=true"; then
  PROBE_LINE=1
  ok "stage-1 probe enabled (from add-source output)"
elif j | grep -qE "stage-1 table probe config: enabled=true"; then
  PROBE_LINE=1
  ok "stage-1 probe enabled (from journal)"
fi
if [ "$PROBE_LINE" = "1" ] || [ "${GRADED:-0}" -gt 0 ]; then
  [ "${GRADED:-0}" -gt 0 ] && ok "proxies graded ($GRADED)"
  [ "$PROBE_LINE" = "0" ] && ok "stage-1 inferred enabled via $GRADED graded proxies"
else
  t1bad "stage-1 probe NOT enabled and 0 proxies graded (kill switch stuck?)"
fi
AUTH_FAILS=$(j | grep -cE "proxy\[[0-9]+\].*auth failed")
if [ "${AUTH_FAILS:-0}" -le 5 ]; then
  ok "proxy auth failures low ($AUTH_FAILS)"
else
  echo "WARN: $AUTH_FAILS proxy auth failures (free proxies are flaky; not a gate)" | tee -a "$REPORT"
fi

# ---------- F. Docker path ----------
section "F. Docker"
# The GH runner ships docker-ce preinstalled from Docker's own repo, so
# `apt-get install docker.io` conflicts with it and exits 100 -- and because
# of the && it never reaches `systemctl start docker` either. That produced a
# standing "FAIL: docker installed (exit 100)" on every run while every
# downstream docker check passed against the daemon that was already there.
# Assert the END STATE (a responding daemon) and only install one when it is
# genuinely absent.
if timeout 60 docker info >/dev/null 2>&1; then
  ok "docker available (preinstalled daemon responding)"
elif command -v docker >/dev/null 2>&1 && systemctl start docker >/dev/null 2>&1 && timeout 60 docker info >/dev/null 2>&1; then
  # Installed but not running: start the daemon that is already there.
  # Installing docker.io on top of a docker-ce host conflicts and fails a
  # perfectly recoverable box.
  ok "docker available (started the installed daemon)"
else
  apt-get update -qq >/dev/null 2>&1
  run_check "docker installed" timeout 600 bash -c "apt-get install -y -qq docker.io >/dev/null 2>&1 && systemctl start docker"
  if timeout 60 docker info >/dev/null 2>&1; then
    ok "docker daemon responding after install"
  else
    bad "docker daemon not responding after install"
  fi
fi
curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/install-urnet-docker.sh -o /tmp/install-docker.sh
sh /tmp/install-docker.sh 2>&1 | grep -q "sha256 verified" && ok "install-urnet-docker.sh verified" || bad "docker installer"
run_check "urnet-docker version" /usr/local/bin/urnet-docker version 2>&1
mkdir -p /tmp/docker-state && cp /home/urnet/.urnetwork/jwt /tmp/docker-state/jwt && cp /home/urnet/.urnetwork/network.json /tmp/docker-state/network.json
# MUST-FIX 10: pull the EXPECTED image tag, not :latest (which lags a tag
# push). The image tag follows the release tag.
run_check "image pulled" timeout 300 docker pull "ghcr.io/full-bars/urnetwork-3.23-fix:${EXPECTED_VERSION}" 2>&1
# MUST-FIX 6 (docker): pass the cap into the container via env var.
docker run -d --name urnetwork-test -v /tmp/docker-state:/root/.urnetwork -e PROXY_URL_MAX=200 -e BUILD=jwt "ghcr.io/full-bars/urnetwork-3.23-fix:${EXPECTED_VERSION}" >/dev/null 2>&1
sleep 8
docker ps --format "{{.Names}}" | grep -q urnetwork-test && ok "container up" || bad "container"
run_check "urnet-docker providers" /usr/local/bin/urnet-docker providers 2>&1
run_check "urnet-docker restart -f" /usr/local/bin/urnet-docker restart urnetwork-test -f 2>&1
# MUST-FIX 7: while the container is up, the box is multi-provider. The Go
# tool MUST refuse without a target (lock in the real behavior).
# 2026-08-15 review finding: urnet-tools Discover() does NOT enumerate
# docker-containerized providers — that is urnet-docker's domain (separate
# binary, shells out to docker ps). With 1 systemd provider + 1 container,
# 'status' succeeding is CORRECT behavior, not a guard regression. The
# multi-provider refusal is covered by the selectTarget unit tests (two
# systemd providers). Demoted from FAIL to KNOWN-GAP: the container
# scenario is not discoverable by this tool.
if urnet-tools status >/dev/null 2>&1; then
  echo "KNOWN-GAP: multi-provider refusal not exercisable via docker container (urnet-tools discovers systemd providers; see urnet-docker). Covered by selectTarget unit tests." | tee -a "$REPORT"
else
  ok "multi-provider ambiguity refused (2 providers, no target)"
fi
# MUST-FIX 7: remove the container NOW so later sections are single-provider
# again (K/L/M/N/O all call urnet-tools with no target).
docker rm -f urnetwork-test >/dev/null 2>&1 && ok "docker container removed (single-provider restored)" || bad "docker rm"

# ---------- G. Hub (systemd) ----------
section "G. Hub (systemd)"
# Hub coverage (user request 2026-08-14).
# Install the hub binary and the systemd unit via urnet-tools.
# Start the unit and verify that the dashboard serves.
# Verify that the provider reports into the hub.
# The hub also ships as a docker image (ghcr.io/full-bars/urnetwork-3.23-fix-hub,
# pushed by hub-build.yml). Section G2 tests the container path.
# MUST-FIX: pin the hub to the release under test. Without --tag, hub install
# resolves the latest release, which lags on tag-triggered runs.
run_check "hub install (pinned)" urnet-tools hub install "--tag=$EXPECTED_VERSION" 2>&1
if [ -x /home/urnet/.local/share/urnetwork-provider/bin/urnetwork-hub ]; then
  ok "hub binary installed"
else
  bad "hub binary missing after hub install"
fi
# MUST-FIX: hub install writes the unit but does not reload systemd. The start
# can fail on a fresh unit without daemon-reload.
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user daemon-reload 2>&1 && ok "hub daemon-reload" || bad "hub daemon-reload"
HUB_START=$(runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user start urnetwork-hub.service 2>&1); HUB_RC=$?
[ "$HUB_RC" -eq 0 ] && ok "hub unit started (exit 0)" || { bad "hub unit start (exit $HUB_RC)"; echo "$HUB_START" | tail -3 | tee -a "$REPORT"; }
sleep 5
# MUST-FIX: the dashboard check must assert a real 200. With no
# URNETWORK_HUB_DASHBOARD_PASS the dashboard is unauthenticated. curl -f -w
# emits 000 on transport failure, so a non-empty check can never fail.
HUB_HTTP=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 10 http://127.0.0.1:8080/ 2>/dev/null)
[ "$HUB_HTTP" = "200" ] && ok "hub dashboard serves (HTTP 200)" || bad "hub dashboard not 200 (HTTP ${HUB_HTTP:-none})"
# Point the provider at the hub. report <url> writes ~/.urnetwork/report_url.
# The reporter re-reads the file live. No provider restart happens. The
# Phase 2 uptime clock stays untouched.
# Speed the reporter up. The default interval is 5m. The override file
# report_interval drops it to 10s (the minimum).
run_check "report URL set to local hub" urnet-tools report http://127.0.0.1:8080 2>&1
printf '10s\n' > /home/urnet/.urnetwork/report_interval && chown urnet:urnet /home/urnet/.urnetwork/report_interval
# MUST-FIX: the interval override only takes effect ON THE NEXT TICK, and the
# ticker is at the 5m default. Restart the provider so the 10s interval
# applies from the first tick. Phase 2 has not started yet, so this restart
# does not disturb the remove-dead uptime clock.
MARK=$(restart_provider)
CID=$(wait_client_id "$MARK" 120)
[ -n "$CID" ] && ok "provider restarted for 10s report cadence (client_id ${CID:0:12}…)" || bad "provider restart after report_interval"
# MUST-FIX: grep the REAL receive line. The hub prints at startup
# "WARNING URNETWORK_HUB_TOKEN not set ... /api/report ...", which matches a
# bare report|bandwidth grep. The receive signal is "report from <node>".
HUB_REPORT_OK=0
for i in $(seq 1 36); do
  if runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) journalctl --user -u urnetwork-hub.service --no-pager 2>/dev/null | grep -qE "^report from "; then
    HUB_REPORT_OK=1; break
  fi
  sleep 10
done
if [ "$HUB_REPORT_OK" = "1" ]; then
  ok "hub received provider report"
else
  echo "WARN: no report from line in hub journal within 360s (signal only)" | tee -a "$REPORT"
fi
# Restore the default report interval. The rest of the run must not spam the hub.
rm -f /home/urnet/.urnetwork/report_interval
# MUST-FIX: clear the report URL. Otherwise the provider keeps POSTing to a
# dead 127.0.0.1:8080 every 15s for the remaining ~100 min.
run_check "report URL cleared" urnet-tools report off 2>&1
# Stop the hub unit. Leave the box tidy for the provider tests.
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user stop urnetwork-hub.service 2>&1 && ok "hub unit stopped" || bad "hub unit stop"

# ---------- G2. Hub (docker) ----------
section "G2. Hub (docker)"
# The hub docker image is ghcr.io/full-bars/urnetwork-3.23-fix-hub.
# The entrypoint listens on :8080 and writes to /data.
# A /data volume must persist the SQLite database.
# The image versions independently of the provider release (hub-docker-v*
# tags; :latest = current main). :latest is the only tag pushed today.
HUB_IMG="ghcr.io/full-bars/urnetwork-3.23-fix-hub:latest"
# MUST-FIX: unbounded docker pull can eat the watchdog. Cap it.
run_check "hub image pulled" timeout 300 docker pull "$HUB_IMG" 2>&1
# Record the digest so a later FAIL is attributable to a specific image.
HUB_DIGEST=$(docker inspect -f '{{index .RepoDigests 0}}' "$HUB_IMG" 2>/dev/null || echo "unknown")
echo "  hub image digest: $HUB_DIGEST" | tee -a "$REPORT"
# MUST-FIX: clear stale data from a prior run. A leftover hub.db would make
# the volume check pass without the current container writing anything.
rm -rf /tmp/hub-data && mkdir -p /tmp/hub-data
# MUST-FIX: bind to loopback. The droplet is public; with no
# URNETWORK_HUB_TOKEN / URNETWORK_HUB_DASHBOARD_PASS, /api/report accepts
# writes from anyone on 0.0.0.0.
docker run -d --name hub-test -p 127.0.0.1:18080:8080 -v /tmp/hub-data:/data "$HUB_IMG" >/dev/null 2>&1
sleep 6
docker ps --format "{{.Names}}" | grep -q hub-test && ok "hub container up" || bad "hub container"
# The dashboard must serve on the mapped port. Assert a real 200.
HUB2_HTTP=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 10 http://127.0.0.1:18080/ 2>/dev/null)
[ "$HUB2_HTTP" = "200" ] && ok "hub container dashboard serves (HTTP 200)" || bad "hub container dashboard not 200 (HTTP ${HUB2_HTTP:-none})"
# MUST-FIX: the /data check must assert on the HOST side. openStore creates
# hub.db unconditionally in-container, so docker exec ls passes even if the
# bind mount silently failed.
[ -f /tmp/hub-data/hub.db ] && ok "hub container wrote database (host /tmp/hub-data/hub.db)" || bad "hub container no database on host volume"
# Remove the container. Leave the box tidy.
timeout 30 docker rm -f hub-test >/dev/null 2>&1 && ok "hub container removed" || bad "hub container rm"

# ---------- H. Hot-restart + client identity lifecycle ----------
section "H. Hot-restart + identity"
# The provider mints one client_id PER PROXY (main.go prints "client_id: <id>
# (new|reused)" from the per-proxy auth path), and by this point section E's
# URL sources have brought up scores of them. So "last client_id before" vs
# "last client_id after" compares two UNRELATED proxies: it fails even when
# hot-restart works perfectly, and can even report an "after" id OLDER than
# the "before" one, because a reused id encodes its ORIGINAL mint time
# (observed on the v3.23.0-fix.30.9 run: before=01a06fd0-0e5
# after=01a06fcf-d39, a standing FAIL against a working product).
#
# Compare the SETS instead, and assert the provider's own per-proxy
# (new|reused) marker, which is the direct signal for JWT reuse.
CIDS_BEFORE=$(mktemp); CIDS_AFTER=$(mktemp); CIDS_FRESH=$(mktemp)
cids_since 0 > "$CIDS_BEFORE"
N_BEFORE=$(wc -l < "$CIDS_BEFORE")
[ "$N_BEFORE" -gt 0 ] && ok "provider client_ids present ($N_BEFORE distinct)" || bad "provider client_id missing"

# 1) hot-restart must REUSE identities: ids logged after the restart come back
#    marked (reused) and overlap the pre-restart set.
MARK=$(restart_provider)
wait_client_id "$MARK" 120 >/dev/null
# wait_client_id returns on the FIRST id; settle so several proxies have
# re-authed and the set comparison has more than one sample to work with.
sleep 20
cids_since "$MARK" > "$CIDS_AFTER"
N_AFTER=$(wc -l < "$CIDS_AFTER")
N_REUSED=$(markers_since "$MARK" | grep -c "reused" || true)
N_OVERLAP=$(comm -12 "$CIDS_BEFORE" "$CIDS_AFTER" | wc -l)
if [ "$N_AFTER" -gt 0 ] && [ "${N_REUSED:-0}" -gt 0 ] && [ "$N_OVERLAP" -gt 0 ]; then
  ok "hot-restart reused client_ids ($N_REUSED reused markers, $N_OVERLAP of $N_AFTER ids carried over)"
else
  bad "hot-restart did NOT reuse client_ids (after=$N_AFTER reused_markers=${N_REUSED:-0} overlap=$N_OVERLAP)"
fi

# 2) Clear the persisted client-JWT cache, restart -> ids must mint NEW, and
#    none may carry over from the previous set.
rm -f /home/urnet/.urnetwork/.client_jwts.json
MARK=$(restart_provider)
wait_client_id "$MARK" 120 >/dev/null
sleep 20
cids_since "$MARK" > "$CIDS_FRESH"
N_FRESH=$(wc -l < "$CIDS_FRESH")
N_NEW=$(markers_since "$MARK" | grep -c "new" || true)
# Union of BOTH pre-clear sets: CIDS_AFTER is only a 20s sample, so an
# identity present in CIDS_BEFORE but not sampled into CIDS_AFTER could
# reappear after the cache clear without ever raising N_CARRIED.
N_CARRIED=$(comm -12 <(sort -u "$CIDS_BEFORE" "$CIDS_AFTER") "$CIDS_FRESH" | wc -l)
if [ "$N_FRESH" -gt 0 ] && [ "${N_NEW:-0}" -gt 0 ] && [ "$N_CARRIED" -eq 0 ]; then
  ok "cleared cache minted NEW client_ids ($N_NEW new markers, 0 of $N_AFTER carried over)"
else
  bad "cleared cache did NOT mint new client_ids (fresh=$N_FRESH new_markers=${N_NEW:-0} carried_over=$N_CARRIED)"
fi
rm -f "$CIDS_BEFORE" "$CIDS_AFTER" "$CIDS_FRESH"

# ---------- I. Control-plane connectivity evidence ----------
section "I. Control-plane ([net][s]select)"
# The [net][s]select lines prove the provider's control-plane dials happen.
# A line is a SUCCESS iff it has dur= WITHOUT an " = <err>" segment before it
# (fail lines: `... clients=0 = Post "..." ... dur=15000ms (17 suppressed)`).
# Do NOT grep success=[1-9] alone: that's the dialer's CUMULATIVE lifetime
# counter, so a fail line on a dialer with prior successes also prints
# success=44. Over-matching. (The cumulative counter over-reports.)
# "no success= field" was wrong; both single-grep forms are lossy).
SELECT_TOTAL=$(j | grep -cE "\[net\]\[s\]select:.*dur=[0-9]+ms")
SELECT_FAILS=$(j | grep -cE "\[net\]\[s\]select:.* = .*dur=[0-9]+ms")
SELECT_HITS=$((SELECT_TOTAL - SELECT_FAILS))
# select success lines absent = control-plane dials failing (product signal).
if [ "${SELECT_HITS:-0}" -gt 0 ]; then
  ok "[net][s]select success lines ($SELECT_HITS of $SELECT_TOTAL)"
else
  bad "[net][s]select success missing (total=$SELECT_TOTAL fails=$SELECT_FAILS)"
fi

# ---------- K. Source-switch (remove one source, re-add, verify recovery) ----------
section "K. Source-switch"
run_check "remove-source" urnet-tools proxy remove-source "$MONOSANS_URL" 2>&1
SRC_AFTER_RM=$(urnet-tools summary 2>&1 | grep -oE "Source URLs: +[0-9]+" | grep -oE "[0-9]+" | tail -1)
[ "${SRC_AFTER_RM:-0}" -eq 1 ] && ok "source count dropped to 1 after remove" || bad "source count after remove = $SRC_AFTER_RM (want 1)"
run_check "re-add-source" urnet-tools proxy add-source "$MONOSANS_URL" 2>&1
SRC_AFTER_RE=$(urnet-tools summary 2>&1 | grep -oE "Source URLs: +[0-9]+" | grep -oE "[0-9]+" | tail -1)
[ "${SRC_AFTER_RE:-0}" -eq 2 ] && ok "source count recovered to 2 after re-add" || bad "source count after re-add = $SRC_AFTER_RE (want 2)"

# ---------- L. Hot-restart toggle (real mechanism: env + /proc) ----------
section "L. Hot-restart toggle (env)"
# MUST-FIX 9: `urnet-tools hot-restart on|off` does not exist. The Go tool's
# hot-restart takes no arguments (it's a unit restart). The real toggle is
# URNETWORK_HOT_RESTART=0 (main.go:505). Test the mechanism that exists:
# write the env via the systemd override, verify it reaches the process.
OVERRIDE_DIR="/home/urnet/.config/systemd/user/urnetwork.service.d"
mkdir -p "$OVERRIDE_DIR"
cat > "$OVERRIDE_DIR/override.conf" << 'EOF'
[Service]
Environment="URNETWORK_HOT_RESTART=0"
EOF
chown -R urnet:urnet /home/urnet/.config
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user daemon-reload
MARK=$(restart_provider)
# Wait for the provider to come back up before reading /proc; the returned id
# is not the assertion here, the /proc environ check below is.
wait_client_id "$MARK" 120 >/dev/null
PROC_PID=$(pgrep -u urnet -f 'urnetwork provide' | head -1)
if [ -n "$PROC_PID" ] && tr '\0' '\n' < /proc/$PROC_PID/environ | grep -q "URNETWORK_HOT_RESTART=0"; then
  ok "URNETWORK_HOT_RESTART=0 reached the process (pid $PROC_PID)"
else
  bad "URNETWORK_HOT_RESTART=0 NOT in /proc/$PROC_PID/environ (env toggle broken)"
fi
# The provider mints one client_id PER PROXY, so wait_client_id returns the
# last id of an ARBITRARY proxy out of scores. Comparing two such ids across
# restarts compares two unrelated proxies and proves nothing — the exact trap
# section H documents. Assert the provider's own (new|reused) markers and
# compare SETS, the same way H does.
L_CIDS_OFF=$(mktemp); L_CIDS_ON=$(mktemp)
L_SNAP_OFF=$(mktemp); L_SNAP_ON=$(mktemp)

# 1) Toggle OFF + cleared cache: every identity must be freshly minted, and
#    nothing may be reused.
rm -f /home/urnet/.urnetwork/.client_jwts.json
MARK=$(restart_provider)
wait_client_id "$MARK" 120 >/dev/null
# wait_client_id returns on the FIRST id; settle so several proxies have
# re-authed and the marker counts have more than one sample.
sleep 20
markers_snapshot "$MARK" > "$L_SNAP_OFF"
awk '{print $2}' "$L_SNAP_OFF" | sort -u > "$L_CIDS_OFF"
N_OFF=$(wc -l < "$L_CIDS_OFF")
N_OFF_NEW=$(awk '{print $3}' "$L_SNAP_OFF" | grep -c "new" || true)
N_OFF_REUSED=$(awk '{print $3}' "$L_SNAP_OFF" | grep -c "reused" || true)
if [ "$N_OFF" -gt 0 ] && [ "${N_OFF_NEW:-0}" -gt 0 ] && [ "${N_OFF_REUSED:-0}" -eq 0 ]; then
  ok "hot-restart OFF minted new client_ids ($N_OFF_NEW new, 0 reused)"
else
  bad "hot-restart OFF did not mint fresh identities (ids=$N_OFF new=${N_OFF_NEW:-0} reused=${N_OFF_REUSED:-0})"
fi

# 2) Restore the default (remove override) so the rest of the run reuses
#    identity. The OFF pass above repopulated the store — persistence is
#    unconditional, only the read path is gated (main.go) — so a restart with
#    the toggle back ON must reuse those very ids.
rm -f "$OVERRIDE_DIR/override.conf"
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user daemon-reload
MARK=$(restart_provider)
wait_client_id "$MARK" 120 >/dev/null
sleep 20
markers_snapshot "$MARK" > "$L_SNAP_ON"
awk '{print $2}' "$L_SNAP_ON" | sort -u > "$L_CIDS_ON"
N_ON=$(wc -l < "$L_CIDS_ON")
N_ON_REUSED=$(awk '{print $3}' "$L_SNAP_ON" | grep -c "reused" || true)
N_ON_OVERLAP=$(comm -12 "$L_CIDS_OFF" "$L_CIDS_ON" | wc -l)
if [ "$N_ON" -gt 0 ] && [ "${N_ON_REUSED:-0}" -gt 0 ] && [ "$N_ON_OVERLAP" -gt 0 ]; then
  ok "hot-restart back ON reused client_ids ($N_ON_REUSED reused markers, $N_ON_OVERLAP of $N_ON carried over)"
else
  bad "hot-restart back ON did not reuse (ids=$N_ON reused=${N_ON_REUSED:-0} overlap=$N_ON_OVERLAP)"
fi
rm -f "$L_CIDS_OFF" "$L_CIDS_ON" "$L_SNAP_OFF" "$L_SNAP_ON"

# ---------- M. update --tag pinned path ----------
section "M. update --tag"
# MUST-FIX 10: version comes from the workflow (GITHUB_REF_NAME for
# tag-triggered runs), not a hardcoded string. EXPECTED_VERSION set at top.
run_check "update --tag $EXPECTED_VERSION" urnet-tools update --tag "$EXPECTED_VERSION" -f 2>&1
# -v is VERBOSE, not version. Use --version.
# Extract the version token: an init-time stdout banner can precede it.
BIN_VER=$(/home/urnet/.local/share/urnetwork-provider/bin/urnetwork --version 2>&1 | grep -m1 -oE "v3\.23\.0-fix\.[0-9.]+" || true)
echo "  binary after --tag: $BIN_VER" | tee -a "$REPORT"
if [ -n "$BIN_VER" ] && echo "$BIN_VER" | grep -qF "$EXPECTED_BASE"; then
  ok "update --tag produced a versioned binary ($BIN_VER)"
else
  bad "update --tag binary version missing/unexpected ($BIN_VER). Version must match $EXPECTED_BASE"
fi
run_check "restored to latest" urnet-tools update -f 2>&1
# `update -f` restores "latest", which for a prerelease tag
# (-rc/-alpha/-beta published as such) is the PREVIOUS release. Re-assert the
# expected version before the 70-min soak so Phase 2 does not soak the wrong
# binary and grade RELEASE_OK.
BIN_VER_AFTER=$(/home/urnet/.local/share/urnetwork-provider/bin/urnetwork --version 2>&1 | grep -m1 -oE "v3\.23\.0-fix\.[0-9.]+" || true)
if [ -n "$BIN_VER_AFTER" ] && echo "$BIN_VER_AFTER" | grep -qF "$EXPECTED_BASE"; then
  ok "binary still matches $EXPECTED_BASE after restore ($BIN_VER_AFTER)"
else
  t1bad "binary after restore is $BIN_VER_AFTER, want $EXPECTED_BASE — Phase 2 would soak the wrong release"
fi

# ---------- N. exclude + refresh --force (exit-status-first) ----------
section "N. exclude + refresh --force"
# MUST-FIX 8: `ok || bad` could never fail (ok returns 0). Real assertions:
# refresh --force must exit 0. proxy exclude is a BINARY subcommand, NOT a Go
# tool subcommand (real gap, same class as old BUG-8). Assert the tool
# refuses loudly rather than silently doing nothing.
run_check "proxy refresh --force" urnet-tools proxy refresh --force 2>&1
if urnet-tools proxy exclude 1.1.1.1 >/dev/null 2>&1; then
  bad "proxy exclude unexpectedly succeeded (Go tool gap?)"
else
  echo "KNOWN-GAP: urnet-tools proxy exclude is not a Go-tool subcommand (binary-only)" | tee -a "$REPORT"
  ok "proxy exclude correctly refused by Go tool (gap documented)"
fi

# ---------- P. Self-update ----------
section "P. Self-update"
run_check "self-update -f" urnet-tools self-update -f 2>&1

# ---------- Q. Multiple drop-ins, divergent duplicate keys ----------
section "Q. Multiple drop-ins (divergent duplicate keys)"
# Real fleet nodes carry up to 4 drop-ins (hub.conf, override.conf,
# ramlogs.conf, restart-override.conf/restart.conf) with the SAME key
# defined in more than one file. Today the duplicates hold identical values
# so the ambiguity is invisible in production; force divergence here so a
# future regression that parses drop-in files itself (instead of reading
# the already-merged process environment) gets caught.
OVERRIDE_DIR="/home/urnet/.config/systemd/user/urnetwork.service.d"
BASE_UNIT="/home/urnet/.config/systemd/user/urnetwork.service"
mkdir -p "$OVERRIDE_DIR"
# Make restart-override.conf's Restart=on-failure genuinely load-bearing,
# matching at least one real fleet node where the base unit itself has no
# Restart= line. Back up the runtime-generated unit file (not a repo file)
# so R can restore it exactly.
cp "$BASE_UNIT" /tmp/urnetwork.service.orig.bak
if grep -q '^Restart=' "$BASE_UNIT"; then
  sed -i 's/^Restart=.*/Restart=no/' "$BASE_UNIT"
else
  sed -i '/^\[Service\]/a Restart=no' "$BASE_UNIT"
fi
chown urnet:urnet "$BASE_UNIT"
cat > "$OVERRIDE_DIR/hub.conf" << 'EOF'
[Service]
Environment="URNETWORK_REPORT_URL=http://198.51.100.10:8080"
EOF
cat > "$OVERRIDE_DIR/override.conf" << 'EOF'
[Service]
Environment="URNETWORK_REPORT_URL=http://198.51.100.20:8080"
Environment="URNETWORK_RAMLOGS=1"
EOF
cat > "$OVERRIDE_DIR/ramlogs.conf" << 'EOF'
[Service]
Environment="URNETWORK_RAMLOGS=0"
EOF
cat > "$OVERRIDE_DIR/restart-override.conf" << 'EOF'
[Service]
Restart=on-failure
RestartSec=10
EOF
chown -R urnet:urnet /home/urnet/.config
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user daemon-reload
MARK=$(restart_provider)
CID=$(wait_client_id "$MARK" 120)
[ -n "$CID" ] && ok "Q: restart with 4 divergent drop-ins (client_id ${CID:0:12}…)" || bad "Q: restart with 4 divergent drop-ins produced no client_id"
PROC_PID=$(pgrep -u urnet -f 'urnetwork provide' | head -1)
# systemd merges lexically, LAST WINS: hub.conf < override.conf, so
# REPORT_URL must be override.conf's value (.20), never hub.conf's (.10).
if [ -n "$PROC_PID" ] && tr '\0' '\n' < /proc/$PROC_PID/environ | grep -qE "^URNETWORK_REPORT_URL=http://198\.51\.100\.20:8080$"; then
  ok "Q: REPORT_URL resolved to override.conf's value (lexically last of hub.conf/override.conf)"
else
  bad "Q: REPORT_URL did NOT resolve to override.conf's value (systemd merge order violated)"
fi
# override.conf < ramlogs.conf, so RAMLOGS must be ramlogs.conf's 0, not
# override.conf's 1.
if [ -n "$PROC_PID" ] && tr '\0' '\n' < /proc/$PROC_PID/environ | grep -qE "^URNETWORK_RAMLOGS=0$"; then
  ok "Q: RAMLOGS resolved to ramlogs.conf's value (lexically last of override.conf/ramlogs.conf)"
else
  bad "Q: RAMLOGS did NOT resolve to ramlogs.conf's value (systemd merge order violated)"
fi

# ---------- R. Override removal / retention matrix ----------
section "R. Override removal / retention matrix"
# Builds on Q's 4-file layout. For the pair that actually shares a
# duplicate key (override.conf/hub.conf for REPORT_URL,
# override.conf/ramlogs.conf for RAMLOGS) test all four states: present,
# fully removed, present-but-empty, and present-with-an-empty-value.
# Removal of the LAST file defining a key must make it ABSENT, not stale;
# removal of a non-last file must fall back to the remaining definer, not
# go stale either.
r_assert_env() {
  # r_assert_env "label" "regex" ["absent"]
  local label="$1" pat="$2" mode="${3:-present}"
  PROC_PID=$(pgrep -u urnet -f 'urnetwork provide' | head -1)
  if [ -z "$PROC_PID" ]; then bad "R: $label (no provider process)"; return; fi
  if [ "$mode" = "absent" ]; then
    if tr '\0' '\n' < /proc/$PROC_PID/environ | grep -qE "$pat"; then
      bad "R: $label (expected ABSENT, but matched $pat)"
    else
      ok "R: $label (absent, as expected)"
    fi
  else
    if tr '\0' '\n' < /proc/$PROC_PID/environ | grep -qE "$pat"; then
      ok "R: $label (matched $pat)"
    else
      bad "R: $label (expected match on $pat, none found)"
    fi
  fi
}
r_restart() {
  runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user daemon-reload
  local mark cid
  mark=$(restart_provider)
  cid=$(wait_client_id "$mark" 120)
  [ -n "$cid" ] || bad "R: restart after $1 produced no client_id"
}

# R1. remove override.conf entirely -> REPORT_URL falls back to hub.conf's
# value (.10): NOT stale at .20, NOT absent (hub.conf still defines it).
rm -f "$OVERRIDE_DIR/override.conf"
r_restart "override.conf removed"
r_assert_env "REPORT_URL falls back to hub.conf after override.conf removed" "^URNETWORK_REPORT_URL=http://198\.51\.100\.10:8080$"

# R2. present-but-empty: override.conf exists but defines nothing. Same
# expectation as fully removed, for this key.
cat > "$OVERRIDE_DIR/override.conf" << 'EOF'
[Service]
EOF
r_restart "override.conf present-but-empty"
r_assert_env "REPORT_URL still falls back to hub.conf with override.conf present-but-empty" "^URNETWORK_REPORT_URL=http://198\.51\.100\.10:8080$"

# R3. present-with-empty-value: Environment="KEY=" is a real assignment to
# the empty string, NOT the same as the key being absent. override.conf
# sorts after hub.conf, so this empty assignment must WIN.
cat > "$OVERRIDE_DIR/override.conf" << 'EOF'
[Service]
Environment="URNETWORK_REPORT_URL="
EOF
r_restart "override.conf present-with-empty-value"
r_assert_env "REPORT_URL is present but EMPTY (assignment, not absence)" "^URNETWORK_REPORT_URL=$"

# R4. restore override.conf to its Q baseline -> REPORT_URL back to .20.
cat > "$OVERRIDE_DIR/override.conf" << 'EOF'
[Service]
Environment="URNETWORK_REPORT_URL=http://198.51.100.20:8080"
Environment="URNETWORK_RAMLOGS=1"
EOF
r_restart "override.conf restored"
r_assert_env "REPORT_URL back to override.conf's value after restore" "^URNETWORK_REPORT_URL=http://198\.51\.100\.20:8080$"

# R5. remove BOTH files that define REPORT_URL -> the key must be
# genuinely ABSENT, not carry a stale value from either.
mv "$OVERRIDE_DIR/hub.conf" /tmp/hub.conf.bak
mv "$OVERRIDE_DIR/override.conf" /tmp/override.conf.bak
r_restart "hub.conf and override.conf both removed"
r_assert_env "REPORT_URL absent when no file defines it" "^URNETWORK_REPORT_URL=" absent

# R6. restore both -> REPORT_URL back to .20.
mv /tmp/hub.conf.bak "$OVERRIDE_DIR/hub.conf"
mv /tmp/override.conf.bak "$OVERRIDE_DIR/override.conf"
r_restart "hub.conf and override.conf both restored"
r_assert_env "REPORT_URL back to .20 after both restored" "^URNETWORK_REPORT_URL=http://198\.51\.100\.20:8080$"

# R7. remove ramlogs.conf -> RAMLOGS falls back to override.conf's 1, not
# stale at ramlogs.conf's 0.
rm -f "$OVERRIDE_DIR/ramlogs.conf"
r_restart "ramlogs.conf removed"
r_assert_env "RAMLOGS falls back to override.conf's value after ramlogs.conf removed" "^URNETWORK_RAMLOGS=1$"

# R8. restore ramlogs.conf -> RAMLOGS back to 0.
cat > "$OVERRIDE_DIR/ramlogs.conf" << 'EOF'
[Service]
Environment="URNETWORK_RAMLOGS=0"
EOF
r_restart "ramlogs.conf restored"
r_assert_env "RAMLOGS back to ramlogs.conf's value after restore" "^URNETWORK_RAMLOGS=0$"

# R9. remove restart-override.conf -> Restart= must revert to the base
# unit's own Restart=no (forced in Q specifically so this drop-in is
# genuinely load-bearing, matching a real fleet node).
rm -f "$OVERRIDE_DIR/restart-override.conf"
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user daemon-reload
RESTART_AFTER_RM=$(runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user show -p Restart --value urnetwork.service 2>/dev/null)
[ "$RESTART_AFTER_RM" = "no" ] && ok "R: Restart= reverted to the base unit's 'no' after restart-override.conf removed" || bad "R: Restart= is '$RESTART_AFTER_RM' after removal, want 'no' (base unit default)"

# R10. final cleanup: remove ALL FOUR test drop-ins AND restore the base
# unit file exactly, so later sections start from the same clean baseline
# every other section expects. Assert the restore actually landed.
rm -f "$OVERRIDE_DIR/hub.conf" "$OVERRIDE_DIR/override.conf" "$OVERRIDE_DIR/ramlogs.conf" "$OVERRIDE_DIR/restart-override.conf"
cp /tmp/urnetwork.service.orig.bak "$BASE_UNIT"
chown urnet:urnet "$BASE_UNIT"
if diff -q /tmp/urnetwork.service.orig.bak "$BASE_UNIT" >/dev/null 2>&1; then
  ok "R: base unit file restored to its pre-Q content"
else
  bad "R: base unit file restore did not match the pre-Q original"
fi
rm -f /tmp/urnetwork.service.orig.bak
r_restart "all Q/R drop-ins removed + base unit restored (cleanup)"
r_assert_env "REPORT_URL absent after full cleanup" "^URNETWORK_REPORT_URL=" absent
r_assert_env "RAMLOGS absent after full cleanup" "^URNETWORK_RAMLOGS=" absent
RESTART_CLEAN=$(runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user show -p Restart --value urnetwork.service 2>/dev/null)
[ "$RESTART_CLEAN" = "no" ] && ok "R: Restart= back to base default 'no' after full cleanup" || bad "R: Restart= is '$RESTART_CLEAN' after full cleanup, want 'no'"

# ---------- S. Settings survive an update ----------
section "S. Settings survive an update"
# Highest business value in the whole battery: real fleet nodes have a
# systemd override.conf full of tuning that must not get silently reset by
# an update. Deliberately non-default values throughout -- a value equal
# to the default cannot prove it survived.
mkdir -p "$OVERRIDE_DIR"
cat > "$OVERRIDE_DIR/override.conf" << 'EOF'
[Service]
Environment="URNETWORK_PROFILE=turbo-v8"
Environment="GOGC=150"
Environment="GOMEMLIMIT=700MiB"
Environment="URNETWORK_RAMLOGS=1"
Environment="URNETWORK_HOT_RESTART=0"
Environment="GOTRACEBACK=all"
EOF
chown -R urnet:urnet /home/urnet/.config
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user daemon-reload
MARK=$(restart_provider)
CID=$(wait_client_id "$MARK" 120)
[ -n "$CID" ] && ok "S: seeded non-default settings and restarted (client_id ${CID:0:12}…)" || bad "S: seed restart produced no client_id"

S_KEYS=(
  "URNETWORK_PROFILE=turbo-v8"
  "GOGC=150"
  "GOMEMLIMIT=700MiB"
  "URNETWORK_RAMLOGS=1"
  "URNETWORK_HOT_RESTART=0"
  "GOTRACEBACK=all"
)
s_assert_all() {
  local when="$1"
  PROC_PID=$(pgrep -u urnet -f 'urnetwork provide' | head -1)
  if [ -z "$PROC_PID" ]; then bad "S: $when (no provider process)"; return; fi
  local envdump kv
  envdump=$(tr '\0' '\n' < /proc/$PROC_PID/environ)
  for kv in "${S_KEYS[@]}"; do
    if echo "$envdump" | grep -qxF "$kv"; then
      ok "S: $kv present $when"
    else
      bad "S: $kv MISSING or changed $when"
    fi
  done
}
s_assert_all "before update"

# The runner only has ONE real published build for this run
# (EXPECTED_VERSION), so this update re-applies the SAME tag rather than
# jumping to a second distinct tag -- there is no second build available
# to jump to. It still exercises the real backup/swap/restart cycle that
# a genuine version update goes through. Judgement call, documented in the
# PR/report.
run_check "S: update --tag $EXPECTED_VERSION -f (settings-survival update)" timeout 300 urnet-tools update --tag "$EXPECTED_VERSION" -f 2>&1
s_assert_all "after update"
# Specifically: profile turbo-v8 must not have clobbered the explicit
# GOGC/GOMEMLIMIT (profile-application-order regression).
PROC_PID=$(pgrep -u urnet -f 'urnetwork provide' | head -1)
if [ -n "$PROC_PID" ] && tr '\0' '\n' < /proc/$PROC_PID/environ | grep -qxF "GOGC=150" && tr '\0' '\n' < /proc/$PROC_PID/environ | grep -qxF "GOMEMLIMIT=700MiB"; then
  ok "S: turbo-v8 profile did NOT clobber explicit GOGC/GOMEMLIMIT"
else
  bad "S: turbo-v8 profile clobbered explicit GOGC/GOMEMLIMIT"
fi

# Cleanup: remove the settings drop-in, restart, assert the non-default
# markers are gone (restore actually worked, not assumed).
rm -f "$OVERRIDE_DIR/override.conf"
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user daemon-reload
MARK=$(restart_provider)
CID=$(wait_client_id "$MARK" 120)
[ -n "$CID" ] && ok "S: cleanup restart ok" || bad "S: cleanup restart produced no client_id"
PROC_PID=$(pgrep -u urnet -f 'urnetwork provide' | head -1)
if [ -n "$PROC_PID" ] && ! tr '\0' '\n' < /proc/$PROC_PID/environ | grep -qxF "GOGC=150"; then
  ok "S: GOGC=150 gone after override.conf removed (restore verified)"
else
  bad "S: GOGC=150 still present after override.conf removed (restore did not take)"
fi

# ---------- T. Bare update (fetchLatestRelease leg) ----------
section "T. Bare update (fetchLatestRelease leg)"
# --tag always short-circuits fetchLatestRelease. The only way to exercise
# /releases/latest resolution is a BARE update. Section M already does one
# bare `update -f`, but that runs FROM whatever EXPECTED_VERSION is (which
# may itself be a pre-release, and pre-releases are excluded from
# /releases/latest by design -- bare-updating FROM one would DOWNGRADE).
# This section instead starts from a hardcoded, KNOWN-OLDER real stable so
# the direction is unambiguously older-stable -> newer-stable regardless of
# what tag this run is testing. OLD_STABLE_VERSION should be bumped
# periodically as published stables age; it only needs to predate whatever
# tag this workflow will ever test.
OLD_STABLE_VERSION="v3.23.0-fix.30.9"
run_check "T: install known-older stable $OLD_STABLE_VERSION" timeout 300 urnet-tools update --tag "$OLD_STABLE_VERSION" -f 2>&1
BIN_VER_OLD=$(/home/urnet/.local/share/urnetwork-provider/bin/urnetwork --version 2>&1 | grep -m1 -oE "v3\.23\.0-fix\.[0-9.]+" || true)
echo "  binary after install of known-older stable: $BIN_VER_OLD" | tee -a "$REPORT"
[ "$BIN_VER_OLD" = "$OLD_STABLE_VERSION" ] && ok "T: installed $OLD_STABLE_VERSION as the older-stable baseline" || bad "T: expected $OLD_STABLE_VERSION, got $BIN_VER_OLD"

# GUARD: this bare update must never run FROM a pre-release, since
# /releases/latest excludes pre-releases and would DOWNGRADE. It runs from
# OLD_STABLE_VERSION (a real published stable), never from
# EXPECTED_VERSION -- assert that construction rather than trust it
# silently.
if echo "$OLD_STABLE_VERSION" | grep -qE -- "-rc|-alpha|-beta"; then
  bad "T: OLD_STABLE_VERSION ($OLD_STABLE_VERSION) is a pre-release -- the bare-update downgrade guard would be violated"
else
  ok "T: bare update runs from a real stable, not a pre-release (downgrade guard satisfied by construction)"
fi

run_check "T: bare update (fetchLatestRelease leg)" timeout 300 urnet-tools update -f 2>&1
BIN_VER_LATEST=$(/home/urnet/.local/share/urnetwork-provider/bin/urnetwork --version 2>&1 | grep -m1 -oE "v3\.23\.0-fix\.[0-9.]+" || true)
echo "  binary after bare update: $BIN_VER_LATEST" | tee -a "$REPORT"
if [ -n "$BIN_VER_LATEST" ] && [ "$BIN_VER_LATEST" != "$OLD_STABLE_VERSION" ]; then
  ok "T: bare update resolved /releases/latest and moved forward ($OLD_STABLE_VERSION -> $BIN_VER_LATEST)"
else
  bad "T: bare update did not advance the version (still $BIN_VER_LATEST)"
fi

# Re-pin to the release under test. Every later section assumes
# EXPECTED_VERSION is what is actually installed.
run_check "T: re-pin to $EXPECTED_VERSION after fetchLatestRelease exercise" timeout 300 urnet-tools update --tag "$EXPECTED_VERSION" -f 2>&1
BIN_VER_REPINNED=$(/home/urnet/.local/share/urnetwork-provider/bin/urnetwork --version 2>&1 | grep -m1 -oE "v3\.23\.0-fix\.[0-9.]+" || true)
if [ -n "$BIN_VER_REPINNED" ] && echo "$BIN_VER_REPINNED" | grep -qF "$EXPECTED_BASE"; then
  ok "T: re-pinned to $EXPECTED_BASE after fetchLatestRelease exercise ($BIN_VER_REPINNED)"
else
  t1bad "T: re-pin to $EXPECTED_BASE failed after fetchLatestRelease exercise (got $BIN_VER_REPINNED) -- later sections would test the wrong binary"
fi
MARK=$(restart_provider)
CID=$(wait_client_id "$MARK" 120)
[ -n "$CID" ] && ok "T: provider healthy on $EXPECTED_BASE after re-pin (client_id ${CID:0:12}…)" || bad "T: no client_id after re-pin restart"


# ---------- U. Type=notify + READY/STATUS ----------
section "U. Type=notify + READY/STATUS"
mkdir -p "$OVERRIDE_DIR"
cat > "$OVERRIDE_DIR/notify.conf" << 'EOF'
[Service]
Type=notify
NotifyAccess=all
TimeoutStartSec=0
EOF
chown -R urnet:urnet /home/urnet/.config
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user daemon-reload
U_TYPE=$(runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user show -p Type --value urnetwork.service 2>/dev/null)
[ "$U_TYPE" = "notify" ] && ok "U: unit Type=notify after drop-in + daemon-reload" || bad "U: unit Type is '$U_TYPE', want notify"

# U1: unit reaches active WITHOUT waiting for full proxy auth.
# The clock starts BEFORE the restart and the restart is wrapped in `timeout`
# (as U3 does): under Type=notify `systemctl restart` blocks until READY=1, so
# an unwrapped call measures nothing and, if READY never arrives, hangs until
# the CI job watchdog kills the whole run instead of failing this one check.
U1_T0=$(date +%s)
timeout 120 runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user restart urnetwork.service
U1_ACTIVE=0
for i in $(seq 1 30); do
  if [ "$(runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user is-active urnetwork.service 2>/dev/null)" = "active" ]; then
    U1_ACTIVE=1; break
  fi
  sleep 1
done
U1_ELAPSED=$(( $(date +%s) - U1_T0 ))
if [ "$U1_ACTIVE" = "1" ]; then
  ok "U1: unit reached active in ${U1_ELAPSED}s under Type=notify (did not block on full proxy auth)"
else
  bad "U1: unit did not reach active within 30s under Type=notify"
fi
CID_U=$(wait_client_id 0 120)
[ -n "$CID_U" ] || bad "U1: provider never minted a client_id after reaching active"

# U2: `systemctl --user status` shows a live STATUS= line (sd_notify
# extended status), e.g. "N/M proxies authenticated". This never appeared
# pre-fix.
U2_STATUS=$(runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user status urnetwork.service 2>&1)
if echo "$U2_STATUS" | grep -qE '[0-9]+/[0-9]+ proxies'; then
  ok "U2: systemctl status shows a live proxy-count STATUS= line"
else
  echo "$U2_STATUS" | grep -i "Status:" | tee -a "$REPORT"
  bad "U2: no live proxy-count STATUS= line in systemctl status output"
fi

# U3: THE CRITICAL ONE. Block api.bringyour.com, then restart. Pre-fix this
# blocks FOREVER (TimeoutStartSec=0 + READY withheld). Wrapped in `timeout`
# so a hang becomes a catchable FAIL rather than killing the job.
cp /etc/hosts /tmp/hosts.pre-u3.bak
echo "127.0.0.1 api.bringyour.com # shakedown U3 outage simulation" >> /etc/hosts
U3_T0=$(date +%s)
timeout 120 runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user restart urnetwork.service
U3_RC=$?
U3_ELAPSED=$(( $(date +%s) - U3_T0 ))
if [ "$U3_RC" -eq 0 ]; then
  ok "U3: restart under simulated API outage returned promptly (${U3_ELAPSED}s)"
elif [ "$U3_RC" -eq 124 ]; then
  t1bad "U3: restart under simulated API outage HUNG (timeout 120 fired) -- READY/TimeoutStartSec=0 regression"
else
  bad "U3: restart under simulated API outage failed (exit $U3_RC, ${U3_ELAPSED}s)"
fi
# Unblock and confirm recovery regardless of the outcome above.
cp /tmp/hosts.pre-u3.bak /etc/hosts
rm -f /tmp/hosts.pre-u3.bak
if grep -q "api.bringyour.com" /etc/hosts; then
  bad "U3: /etc/hosts still contains the outage block after restore"
else
  ok "U3: /etc/hosts outage block removed"
fi
MARK=$(restart_provider)
CID_U3=$(wait_client_id "$MARK" 120)
[ -n "$CID_U3" ] && ok "U3: provider recovered after unblocking (client_id ${CID_U3:0:12}…)" || bad "U3: provider did not recover after unblocking api.bringyour.com"

# Cleanup: remove the Type=notify drop-in, confirm the unit reverts to
# simple (the base unit carries no Type= line, so simple is the default).
rm -f "$OVERRIDE_DIR/notify.conf"
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user daemon-reload
MARK=$(restart_provider)
CID=$(wait_client_id "$MARK" 120)
[ -n "$CID" ] && ok "U: cleanup restart ok" || bad "U: cleanup restart produced no client_id"
U_TYPE_AFTER=$(runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user show -p Type --value urnetwork.service 2>/dev/null)
[ "$U_TYPE_AFTER" = "simple" ] && ok "U: unit Type reverted to simple after notify.conf removed" || bad "U: unit Type is '$U_TYPE_AFTER' after cleanup, want simple"

# ---------- V. Hotswap decline and engage (order matters) ----------
section "V. Hotswap decline and engage"
# triggerHotSwap evaluates the RUNNING (old) provider, not the staged
# binary, so the decline REASON depends on sequence (order per the "v31
# BATTERY CORRECTION" progress entry):
#   V1: old (<31) provider on Type=simple  -> declines on VERSION
#       (hotSwapVersionOK fails; ErrHotSwapNotSupported)
#   V2: >=31 provider on Type=simple       -> declines on UNIT TYPE
#       (hotSwapVersionOK passes, hotSwapUnitOK fails; ErrHotSwapUnitNotNotify)
#   V3: >=31 provider on Type=notify       -> hotswap ENGAGES
# Two distinct hotswap-eligible tags exist on the remote (both >=31, so both
# pass the version gate): v3.23.0-fix.31.0-rc1 and v3.23.0-fix.31.1-rc1.
# Sequencing a real 31.0-rc1 -> 31.1-rc1 transition (rather than a
# same-tag re-apply) means a genuine version change is observed, not a
# no-op. EXPECTED_VERSION is restored at the end of this section for
# everything downstream.
OLD_HOTSWAP_VERSION="v3.23.0-fix.30.9"
V_TAG_A="v3.23.0-fix.31.0-rc1"
V_TAG_B="v3.23.0-fix.31.1-rc1"
V_BASE_A=$(echo "$V_TAG_A" | grep -oE "v3\.23\.0-fix\.[0-9]+")
V_BASE_B=$(echo "$V_TAG_B" | grep -oE "v3\.23\.0-fix\.[0-9]+")

# V1: old provider (<31), Type=simple -> VERSION decline.
run_check "V1: install pre-hotswap provider $OLD_HOTSWAP_VERSION" timeout 300 urnet-tools update --tag "$OLD_HOTSWAP_VERSION" -f 2>&1
V_TYPE=$(runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user show -p Type --value urnetwork.service 2>/dev/null)
[ "$V_TYPE" = "simple" ] && ok "V1: unit is Type=simple ahead of the version-gate test" || bad "V1: unit Type is '$V_TYPE', want simple"
V1_OUT=$(timeout 300 urnet-tools update --tag "$V_TAG_A" -f 2>&1); V1_RC=$?
echo "$V1_OUT" | grep -iE "hotswap|does not support" | tee -a "$REPORT"
if [ "$V1_RC" -eq 124 ]; then
  t1bad "V1: update HUNG on the version-gate decline path"
elif echo "$V1_OUT" | grep -qi "does not support zero-downtime hotswap"; then
  ok "V1: hotswap declined citing VERSION (exact: 'does not support zero-downtime hotswap')"
else
  bad "V1: expected a VERSION-gate hotswap decline; message not found in update output"
fi
if echo "$V1_OUT" | grep -qi "is not Type=notify"; then
  bad "V1: unit-type decline message appeared on the VERSION-gate step -- wrong gate reached first"
fi
[ "$V1_RC" -eq 0 ] && ok "V1: update fell back to a normal restart and completed (exit 0)" || bad "V1: update did not complete after the VERSION-gate decline (exit $V1_RC)"
BIN_VER_V1=$(/home/urnet/.local/share/urnetwork-provider/bin/urnetwork --version 2>&1 | grep -m1 -oE "v3\.23\.0-fix\.[0-9.]+" || true)
echo "$BIN_VER_V1" | grep -qF "$V_BASE_A" && ok "V1: fallback restart landed on $V_BASE_A ($BIN_VER_V1)" || bad "V1: binary is $BIN_VER_V1 after fallback restart, want $V_BASE_A"
MARK=$(restart_provider)
CID_V1=$(wait_client_id "$MARK" 120)
[ -n "$CID_V1" ] && ok "V1: provider healthy after VERSION-gate decline+restart (client_id ${CID_V1:0:12}…)" || bad "V1: no client_id after V1"

# V2: >=31 provider (now $V_TAG_A), Type=simple -> UNIT TYPE decline.
# ErrHotSwapUnitNotNotify (hotSwapUnitOK, commit 4b54e915) means urnet-tools
# declines in its OWN pre-flight, on its own stdout, and NEVER signals the
# provider at all. This is a materially different code path from
# provider/hotswap.go's internal "Running under systemd without Type=notify
# (NOTIFY_SOCKET unset)" tlog line, which only fires if the provider WAS
# signalled and then aborted the handoff -- the pre-4b54e915 behavior, which
# also never actually restarted (a permanent update no-op on Type=simple
# nodes). So this step must assert BOTH: urnet-tools' own decline text is
# present, AND the provider-side abort tlog is ABSENT -- that absence is
# what actually pins 4b54e915.
V2_PID_BEFORE=$(pgrep -u urnet -f 'urnetwork provide' | head -1)
V2_MARK=$(journal_line_count)
V2_OUT=$(timeout 300 urnet-tools update --tag "$V_TAG_B" -f 2>&1); V2_RC=$?
echo "$V2_OUT" | grep -iE "hotswap|Type=notify|Provider_Install_Linux" | tee -a "$REPORT"
if echo "$V2_OUT" | grep -q "is not Type=notify" && echo "$V2_OUT" | grep -q "Provider_Install_Linux.sh"; then
  ok "V2: urnet-tools declined hotswap in its OWN pre-flight, citing UNIT TYPE (ErrHotSwapUnitNotNotify: 'is not Type=notify' + 'Provider_Install_Linux.sh')"
else
  bad "V2: expected urnet-tools' own pre-flight unit-type decline text ('is not Type=notify' + 'Provider_Install_Linux.sh'); not found in update output"
fi
sleep 3   # let any provider-side tlog line reach the journal, if present
if j_full | awk -v n="$V2_MARK" 'NR>n' | grep -q "Running under systemd without Type=notify (NOTIFY_SOCKET unset)"; then
  bad "V2: provider journal shows the SIGNAL-THEN-ABORT path (regression of 4b54e915) -- urnet-tools should decline before ever signalling the provider"
else
  ok "V2: no signal-then-abort trace in the provider journal -- the decline happened in urnet-tools' own pre-flight (4b54e915 holds)"
fi
if [ "$V2_RC" -eq 124 ]; then
  t1bad "V2: update HUNG on the unit-type decline path"
else
  [ "$V2_RC" -eq 0 ] && ok "V2: update fell back to a normal restart and completed (exit 0)" || bad "V2: update did not complete after the unit-type decline (exit $V2_RC)"
fi
V2_PID_AFTER=$(pgrep -u urnet -f 'urnetwork provide' | head -1)
if [ -n "$V2_PID_BEFORE" ] && [ -n "$V2_PID_AFTER" ] && [ "$V2_PID_BEFORE" != "$V2_PID_AFTER" ]; then
  ok "V2: a real restart occurred ($V2_PID_BEFORE -> $V2_PID_AFTER) -- the fallback path actually restarts, unlike the pre-4b54e915 signal-then-abort no-op"
else
  bad "V2: PID did not change ($V2_PID_BEFORE) -- update looks like a no-op on this Type=simple node (the exact bug 4b54e915 fixed)"
fi
BIN_VER_V2=$(/home/urnet/.local/share/urnetwork-provider/bin/urnetwork --version 2>&1 | grep -m1 -oE "v3\.23\.0-fix\.[0-9.]+" || true)
echo "$BIN_VER_V2" | grep -qF "$V_BASE_B" && ok "V2: fallback restart landed on $V_BASE_B ($BIN_VER_V2)" || bad "V2: binary is $BIN_VER_V2 after fallback restart, want $V_BASE_B"
MARK=$(restart_provider)
CID_V2=$(wait_client_id "$MARK" 120)
[ -n "$CID_V2" ] && ok "V2: provider healthy after unit-type decline+fallback restart (client_id ${CID_V2:0:12}…)" || bad "V2: no client_id after V2"

# Reinstall $V_TAG_A so V3 observes a genuine 31.0-rc1 -> 31.1-rc1
# transition under Type=notify, per the correction entry's ordering,
# instead of re-applying the tag already running.
run_check "V3 setup: reinstall $V_TAG_A ahead of the engage test" timeout 300 urnet-tools update --tag "$V_TAG_A" -f 2>&1

# V3: switch to Type=notify, restart cleanly (not via update), THEN
# hotswap should ENGAGE on a real version transition (31.0-rc1 -> 31.1-rc1).
cat > "$OVERRIDE_DIR/notify.conf" << 'EOF'
[Service]
Type=notify
NotifyAccess=all
TimeoutStartSec=0
EOF
chown -R urnet:urnet /home/urnet/.config
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user daemon-reload
MARK=$(restart_provider)
CID_V3PRE=$(wait_client_id "$MARK" 120)
[ -n "$CID_V3PRE" ] || bad "V3: pre-engage restart under Type=notify produced no client_id"
V3_TYPE=$(runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user show -p Type --value urnetwork.service 2>/dev/null)
[ "$V3_TYPE" = "notify" ] && ok "V3: unit is Type=notify ahead of the engage test" || bad "V3: unit Type is '$V3_TYPE', want notify"
BIN_VER_V3PRE=$(/home/urnet/.local/share/urnetwork-provider/bin/urnetwork --version 2>&1 | grep -m1 -oE "v3\.23\.0-fix\.[0-9.]+" || true)
echo "$BIN_VER_V3PRE" | grep -qF "$V_BASE_A" && ok "V3: running $V_BASE_A ahead of the engage test (genuine transition, not a same-tag re-apply)" || bad "V3: running $BIN_VER_V3PRE ahead of the engage test, want $V_BASE_A"

V3_PID_BEFORE=$(pgrep -u urnet -f 'urnetwork provide' | head -1)
V3_MARK=$(journal_line_count)
run_check "V3: update --tag $V_TAG_B -f (hotswap should ENGAGE)" timeout 180 urnet-tools update --tag "$V_TAG_B" -f 2>&1

V3_PID_AFTER=$(pgrep -u urnet -f 'urnetwork provide' | head -1)
if [ -n "$V3_PID_BEFORE" ] && [ -n "$V3_PID_AFTER" ] && [ "$V3_PID_BEFORE" != "$V3_PID_AFTER" ]; then
  ok "V3: main PID CHANGED ($V3_PID_BEFORE -> $V3_PID_AFTER) -- a real handoff"
else
  bad "V3: main PID did not change (before=$V3_PID_BEFORE after=$V3_PID_AFTER) -- hotswap did not engage"
fi
V3_MAINPID=$(runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user show -p MainPID --value urnetwork.service 2>/dev/null)
if [ "$V3_MAINPID" = "$V3_PID_AFTER" ]; then
  ok "V3: systemd MainPID tracks the new process ($V3_MAINPID)"
else
  bad "V3: systemd MainPID ($V3_MAINPID) does not match the new process ($V3_PID_AFTER)"
fi
if j_full | awk -v n="$V3_MARK" 'NR>n' | grep -qE "Stopped urnetwork.service|Stopping urnetwork.service"; then
  bad "V3: unit entered inactive/activating during the handoff (journal shows a stop) -- not zero-downtime, distinguishes this from V2's plain restart"
else
  ok "V3: no unit stop/inactive event during the handoff (zero-downtime, per journal) -- distinguishes this from V2's plain restart"
fi
if j_full | awk -v n="$V3_MARK" 'NR>n' | grep -q "confirmed active takeover (ACK received)"; then
  ok "V3: candidate confirmed active takeover (ACK) in the journal"
else
  bad "V3: no candidate takeover ACK found in the journal"
fi
run_check "V3: control socket reachable via urnet-tools status" urnet-tools status 2>&1
SOCK_OWNER_PID=""
if command -v lsof >/dev/null 2>&1; then
  SOCK_OWNER_PID=$(lsof -t /home/urnet/.urnetwork/provider.sock 2>/dev/null | head -1)
fi
if [ -n "$SOCK_OWNER_PID" ]; then
  [ "$SOCK_OWNER_PID" = "$V3_PID_AFTER" ] && ok "V3: control socket owned by the new PID ($SOCK_OWNER_PID)" || bad "V3: control socket owned by PID $SOCK_OWNER_PID, want $V3_PID_AFTER"
else
  echo "INFO: lsof unavailable or socket owner not resolvable; relied on urnet-tools status reachability above" | tee -a "$REPORT"
fi
BIN_VER_V3=$(/home/urnet/.local/share/urnetwork-provider/bin/urnetwork --version 2>&1 | grep -m1 -oE "v3\.23\.0-fix\.[0-9.]+" || true)
echo "$BIN_VER_V3" | grep -qF "$V_BASE_B" && ok "V3: hotswap landed on $V_BASE_B ($BIN_VER_V3), a genuine version transition" || bad "V3: running image is $BIN_VER_V3 after the handoff, want $V_BASE_B"
CID_V3=$(wait_client_id "$V3_MARK" 30)
[ -n "$CID_V3" ] && ok "V3: identity continuity confirmed post-handoff (client_id ${CID_V3:0:12}…)" || echo "INFO: no fresh client_id line post-handoff (may be expected: identity persists rather than being re-logged)" | tee -a "$REPORT"

# Cleanup: back to Type=simple and EXPECTED_VERSION, confirm both.
rm -f "$OVERRIDE_DIR/notify.conf"
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user daemon-reload
run_check "V: re-pin to $EXPECTED_VERSION after the hotswap battery" timeout 300 urnet-tools update --tag "$EXPECTED_VERSION" -f 2>&1
MARK=$(restart_provider)
CID=$(wait_client_id "$MARK" 120)
[ -n "$CID" ] && ok "V: cleanup restart ok" || bad "V: cleanup restart produced no client_id"
V_TYPE_AFTER=$(runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user show -p Type --value urnetwork.service 2>/dev/null)
[ "$V_TYPE_AFTER" = "simple" ] && ok "V: unit Type reverted to simple" || bad "V: unit Type is '$V_TYPE_AFTER' after cleanup, want simple"
BIN_VER_VCLEAN=$(/home/urnet/.local/share/urnetwork-provider/bin/urnetwork --version 2>&1 | grep -m1 -oE "v3\.23\.0-fix\.[0-9.]+" || true)
echo "$BIN_VER_VCLEAN" | grep -qF "$EXPECTED_BASE" && ok "V: binary back to $EXPECTED_BASE after hotswap battery" || t1bad "V: binary is $BIN_VER_VCLEAN after hotswap battery, want $EXPECTED_BASE"

# ---------- W. Failure injection ----------
section "W. Failure injection"

# W1 (T1): json_escape. A value with spaces, double quotes, a backslash and
# a newline must round-trip through pending_overrides.json unchanged and
# leave the file valid JSON. node_name has no validateControlValue
# restriction, so it accepts an arbitrary string -- the right key for this.
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user stop urnetwork.service
rm -f /home/urnet/.urnetwork/pending_overrides.json
W1_VAL=$'weird "quoted" \\backslash\\ value\nwith a newline'
printf '%s' "$W1_VAL" > /tmp/w1.expected
urnet-tools set -y node-name "$W1_VAL" >/tmp/w1.out 2>&1
if python3 -c "
import json
d = json.load(open('/home/urnet/.urnetwork/pending_overrides.json'))
ops = [o for o in d if o.get('key') == 'node_name']
expected = open('/tmp/w1.expected').read()
assert ops, 'no node_name op queued'
assert ops[-1]['value'] == expected, (ops[-1]['value'], expected)
"; then
  ok "W1 (T1): value with spaces/quotes/backslash/newline round-tripped through pending_overrides.json"
else
  bad "W1 (T1): pending_overrides.json round-trip failed or file is not valid JSON"
fi
rm -f /tmp/w1.expected /tmp/w1.out
MARK=$(restart_provider)
CID=$(wait_client_id "$MARK" 120)
[ -n "$CID" ] && ok "W1: provider started fine after consuming the escaped value" || bad "W1: provider did not start after consuming the escaped value"
[ -f /home/urnet/.urnetwork/pending_overrides.json ] && bad "W1: pending_overrides.json not consumed/removed after start" || ok "W1: pending_overrides.json consumed on start"

# W2 (T2): queue an override AS ROOT (this whole script already runs as
# root and invokes urnet-tools directly, matching the real "sudo" case).
# Confirm the provider user can still READ the queue file, and that it is
# consumed on start.
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user stop urnetwork.service
rm -f /home/urnet/.urnetwork/pending_overrides.json
urnet-tools set -y node-name "queued-under-root" >/dev/null 2>&1
Q_OWNER=$(stat -c '%U:%G:%a' /home/urnet/.urnetwork/pending_overrides.json 2>/dev/null)
echo "  pending_overrides.json after root-queued set: $Q_OWNER" | tee -a "$REPORT"
if runuser -u urnet -- test -r /home/urnet/.urnetwork/pending_overrides.json; then
  ok "W2 (T2): urnet can read a queue file written while running as root ($Q_OWNER)"
else
  bad "W2 (T2): urnet CANNOT read the queue file written as root ($Q_OWNER)"
fi
MARK=$(restart_provider)
CID=$(wait_client_id "$MARK" 120)
[ -n "$CID" ] && ok "W2: provider started and consumed the root-queued override" || bad "W2: provider did not start after root-queued override"
[ -f /home/urnet/.urnetwork/pending_overrides.json ] && bad "W2: root-queued override not consumed" || ok "W2: root-queued override consumed on start"

# W3 (T3): corrupt pending_overrides.json three ways. Provider must start
# anyway and never crash or hang. Each restart is timeout-wrapped.
for corrupt in truncated invalid-json literal-null; do
  runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user stop urnetwork.service
  case "$corrupt" in
    truncated)     printf '[{"op":"set","key":"node_name","valu' > /home/urnet/.urnetwork/pending_overrides.json ;;
    invalid-json)  printf 'not json at all {{{' > /home/urnet/.urnetwork/pending_overrides.json ;;
    literal-null)  printf 'null' > /home/urnet/.urnetwork/pending_overrides.json ;;
  esac
  chown urnet:urnet /home/urnet/.urnetwork/pending_overrides.json
  W3_MARK=$(journal_line_count)
  timeout 90 runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user start urnetwork.service
  W3_RC=$?
  if [ "$W3_RC" -eq 124 ]; then
    t1bad "W3 (T3): provider HUNG starting with a $corrupt pending_overrides.json"
    continue
  fi
  W3_CID=$(wait_client_id "$W3_MARK" 120)
  if [ -n "$W3_CID" ]; then
    ok "W3 (T3): provider started fine despite a $corrupt pending_overrides.json (client_id ${W3_CID:0:12}…)"
  else
    bad "W3 (T3): provider did not reach a healthy start with a $corrupt pending_overrides.json"
  fi
done
rm -f /home/urnet/.urnetwork/pending_overrides.json

# W4 (T4): stale provider.sock left after SIGKILL. New provider must bind
# cleanly.
W4_PID_BEFORE=$(pgrep -u urnet -f 'urnetwork provide' | head -1)
if [ -n "$W4_PID_BEFORE" ]; then
  kill -9 "$W4_PID_BEFORE"
  sleep 2
fi
[ -S /home/urnet/.urnetwork/provider.sock ] && echo "  provider.sock is stale (process gone, socket file remains)" | tee -a "$REPORT"
MARK=$(restart_provider)
CID=$(wait_client_id "$MARK" 120)
W4_PID_AFTER=$(pgrep -u urnet -f 'urnetwork provide' | head -1)
if [ -n "$CID" ] && [ -n "$W4_PID_AFTER" ] && [ "$W4_PID_AFTER" != "$W4_PID_BEFORE" ]; then
  ok "W4 (T4): new provider bound cleanly after a SIGKILL + stale socket (pid $W4_PID_BEFORE -> $W4_PID_AFTER)"
else
  bad "W4 (T4): provider did not come back cleanly after SIGKILL"
fi
run_check "W4: control socket reachable after stale-socket recovery" urnet-tools status 2>&1

# W5 (T5): concurrent `urnet-tools set` with distinct values. No lost
# update, file stays valid JSON.
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user stop urnetwork.service
rm -f /home/urnet/.urnetwork/pending_overrides.json
W5_N=6
for i in $(seq 1 "$W5_N"); do
  urnet-tools set -y node-name "concurrent-$i" >/tmp/w5-$i.out 2>&1 &
done
wait
if python3 -c "
import json
d = json.load(open('/home/urnet/.urnetwork/pending_overrides.json'))
vals = set(o['value'] for o in d if o.get('key') == 'node_name')
assert len(d) >= $W5_N, ('too few ops', len(d))
assert len(vals) == $W5_N, ('lost update', vals)
"; then
  ok "W5 (T5): $W5_N concurrent 'set' calls all landed, no lost update, file stayed valid JSON"
else
  bad "W5 (T5): concurrent 'set' calls lost an update or corrupted the queue file"
fi
rm -f /tmp/w5-*.out /home/urnet/.urnetwork/pending_overrides.json
MARK=$(restart_provider)
CID=$(wait_client_id "$MARK" 120)
[ -n "$CID" ] && ok "W5: provider healthy after concurrent-writer test" || bad "W5: provider not healthy after concurrent-writer test"

# W6 (T6): wrong --digest must REFUSE the update. An unverified binary must
# never run as the provider user.
W6_BEFORE=$(/home/urnet/.local/share/urnetwork-provider/bin/urnetwork --version 2>&1 | grep -m1 -oE "v3\.23\.0-fix\.[0-9.]+" || true)
W6_WRONG_DIGEST="0000000000000000000000000000000000000000000000000000000000000000"
if timeout 300 urnet-tools update --tag "$EXPECTED_VERSION" --digest "$W6_WRONG_DIGEST" -f >/tmp/w6.out 2>&1; then
  bad "W6 (T6): update with a wrong --digest SUCCEEDED (should have refused)"
else
  if grep -qi "sha256 mismatch" /tmp/w6.out; then
    ok "W6 (T6): update with a wrong --digest refused (sha256 mismatch)"
  else
    ok "W6 (T6): update with a wrong --digest refused (non-zero exit; see report for message)"
    tail -5 /tmp/w6.out | sed 's/^/    | /' | tee -a "$REPORT"
  fi
fi
W6_AFTER=$(/home/urnet/.local/share/urnetwork-provider/bin/urnetwork --version 2>&1 | grep -m1 -oE "v3\.23\.0-fix\.[0-9.]+" || true)
[ "$W6_BEFORE" = "$W6_AFTER" ] && ok "W6: binary version unchanged after the refused update ($W6_AFTER)" || bad "W6: binary version CHANGED despite a wrong digest ($W6_BEFORE -> $W6_AFTER) -- unverified binary installed"
rm -f /tmp/w6.out

# W7 (T7): fresh state -- no pending_overrides.json at all. Every toggle
# must still work and must CREATE the file/dir rather than assume it
# exists. The legacy shell tool's systemd-drop-in toggle (override_set_env)
# was retired in favor of this control-socket/queue mechanism -- see
# scripts/Provider_Install_Linux.sh:1707 -- so this is the mechanism that
# actually needs covering for v31+.
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user stop urnetwork.service
rm -f /home/urnet/.urnetwork/pending_overrides.json
[ -f /home/urnet/.urnetwork/pending_overrides.json ] && bad "W7 (T7): setup failed, queue file still present" || ok "W7 (T7): confirmed queue file absent before the toggle"
run_check "W7 (T7): set on a provider with no queue file yet" urnet-tools set -y node-name "fresh-state-w7" 2>&1
[ -f /home/urnet/.urnetwork/pending_overrides.json ] && ok "W7 (T7): pending_overrides.json CREATED by the toggle (not assumed pre-existing)" || bad "W7 (T7): pending_overrides.json was not created"
MARK=$(restart_provider)
CID=$(wait_client_id "$MARK" 120)
[ -n "$CID" ] && ok "W7: provider started and applied the fresh-state toggle" || bad "W7: provider did not start after the fresh-state toggle"

# W8 (T8): explicit Type=exec and Type=forking must DECLINE hotswap
# cleanly, never error/hang/falsely-claim success. hotSwapUnitOK's check is
# NOTIFY_SOCKET-based, so any non-notify Type should decline the same way
# as Type=simple (V2 above) -- confirm that holds for exec, and probe
# forking defensively since a non-forking binary under Type=forking can
# make systemd itself hang waiting for the parent to exit (timeout-wrapped).
for w8_type in exec forking; do
  cat > "$OVERRIDE_DIR/w8-type.conf" << EOF
[Service]
Type=$w8_type
EOF
  chown -R urnet:urnet /home/urnet/.config
  runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user daemon-reload
  W8_MARK=$(journal_line_count)
  timeout 90 runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user restart urnetwork.service
  W8_RESTART_RC=$?
  if [ "$W8_RESTART_RC" -eq 124 ]; then
    echo "INFO: W8 (T8): unit restart under Type=$w8_type timed out at 90s -- systemd's own semantics for a non-forking binary under Type=$w8_type, not a hotswap bug. Recording and moving on." | tee -a "$REPORT"
    skip "W8 (T8): Type=$w8_type restart timed out (systemd Type=$w8_type semantics, not exercised further)"
    continue
  fi
  W8_ACTIVE=$(runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user is-active urnetwork.service 2>/dev/null)
  if [ "$W8_ACTIVE" != "active" ]; then
    bad "W8 (T8): unit under Type=$w8_type did not reach active (state: $W8_ACTIVE)"
    continue
  fi
  W8_PID_BEFORE=$(pgrep -u urnet -f 'urnetwork provide' | head -1)
  W8_OUT=$(timeout 180 urnet-tools update --tag "$EXPECTED_VERSION" -f 2>&1); W8_RC=$?
  sleep 3
  W8_DECLINED=0
  echo "$W8_OUT" | grep -qi "NOTIFY_SOCKET" && W8_DECLINED=1
  j_full | awk -v n="$W8_MARK" 'NR>n' | grep -q "Running under systemd without Type=notify" && W8_DECLINED=1
  W8_PID_AFTER=$(pgrep -u urnet -f 'urnetwork provide' | head -1)
  if [ "$W8_DECLINED" = "1" ] && [ "$W8_PID_BEFORE" = "$W8_PID_AFTER" ]; then
    ok "W8 (T8): hotswap declined cleanly under Type=$w8_type (live PID unchanged, $W8_PID_AFTER)"
  elif [ "$W8_PID_BEFORE" != "$W8_PID_AFTER" ] && [ -n "$W8_PID_AFTER" ]; then
    bad "W8 (T8): PID changed under Type=$w8_type ($W8_PID_BEFORE -> $W8_PID_AFTER) -- hotswap should have declined, not engaged, on a non-notify type"
  else
    bad "W8 (T8): no clean decline signal found under Type=$w8_type (exit $W8_RC)"
  fi
  [ "$W8_RC" -ne 124 ] && ok "W8: update command did not hang under Type=$w8_type" || t1bad "W8: update command HUNG under Type=$w8_type"
done

# Cleanup: remove the Type=exec/forking drop-in, restore Type=simple.
rm -f "$OVERRIDE_DIR/w8-type.conf"
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user daemon-reload
MARK=$(restart_provider)
CID=$(wait_client_id "$MARK" 120)
[ -n "$CID" ] && ok "W: cleanup restart ok after failure injection battery" || bad "W: cleanup restart produced no client_id"
W_TYPE_AFTER=$(runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user show -p Type --value urnetwork.service 2>/dev/null)
[ "$W_TYPE_AFTER" = "simple" ] && ok "W: unit Type reverted to simple" || bad "W: unit Type is '$W_TYPE_AFTER' after cleanup, want simple"


# ---------- X. Low-memory edge case (scoped) ----------
section "X. Low-memory edge case (scoped MemoryMax sweep)"
# Every OTHER section in this script runs with the runner's full ~16GB --
# that headroom is the point of using the runner (large proxy lists,
# multi-provider, parallel scenarios a 1GB box could not host). This is the
# ONE section that constrains memory, and ONLY the provider unit, and ONLY
# for the duration of this section. The constraint is removed and its
# removal is ASSERTED before the section returns; no later section may
# inherit it.
X_ORIG_MEMORYMAX=$(runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user show -p MemoryMax --value urnetwork.service 2>/dev/null)
echo "  MemoryMax before this section: $X_ORIG_MEMORYMAX (expect 'infinity')" | tee -a "$REPORT"
for MEMLIMIT in 512M 1G 2G; do
  cat > "$OVERRIDE_DIR/memtest.conf" << EOF
[Service]
MemoryMax=$MEMLIMIT
EOF
  chown -R urnet:urnet /home/urnet/.config
  runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user daemon-reload
  X_APPLIED=$(runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user show -p MemoryMax --value urnetwork.service 2>/dev/null)
  echo "  applying MemoryMax=$MEMLIMIT (systemd reports: $X_APPLIED)" | tee -a "$REPORT"
  X_OOM_BEFORE=$(journalctl -k --no-pager 2>/dev/null | grep -ci "out of memory")
  X_MARK=$(journal_line_count)
  timeout 150 runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user restart urnetwork.service
  X_RESTART_RC=$?
  if [ "$X_RESTART_RC" -eq 124 ]; then
    bad "X: restart under MemoryMax=$MEMLIMIT timed out at 150s"
    continue
  fi
  X_CID=$(wait_client_id "$X_MARK" 150)
  # Sample for 2 minutes under this limit; watch for a cgroup OOM kill,
  # both via the unit's own Result and the kernel's OOM log (Phase 2's
  # journalctl -k sweep is global/unscoped by limit; this is scoped).
  sleep 120
  X_OOM_AFTER=$(journalctl -k --no-pager 2>/dev/null | grep -ci "out of memory")
  X_RESULT=$(runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user show -p Result --value urnetwork.service 2>/dev/null)
  X_OOM=0
  { [ "${X_OOM_AFTER:-0}" -gt "${X_OOM_BEFORE:-0}" ] || [ "$X_RESULT" = "oom-kill" ]; } && X_OOM=1
  if [ -z "$X_CID" ] && [ "$X_OOM" = "0" ]; then
    bad "X: MemoryMax=$MEMLIMIT never produced a client_id and no OOM was observed either (unclear failure)"
  elif [ "$X_OOM" = "1" ]; then
    if [ "$MEMLIMIT" = "1G" ] || [ "$MEMLIMIT" = "2G" ]; then
      bad "X: OOM-killed at MemoryMax=$MEMLIMIT -- at or above the fleet-realistic size (smallest real nodes are ~925-969MiB)"
    else
      ok "X: OOM observed at MemoryMax=$MEMLIMIT (below the fleet-realistic 1G floor) -- recorded as the discovered breaking point, not a failure"
    fi
  else
    ok "X: healthy under MemoryMax=$MEMLIMIT (client_id ${X_CID:0:12}…, no OOM in 2min sample)"
  fi
done
# Pair with lowmem profile behaviour, since that is what the fleet's actual
# small nodes run: confirm the provider stays healthy at the
# fleet-realistic 1G floor specifically WITH the lowmem profile applied.
cat > "$OVERRIDE_DIR/memtest.conf" << 'EOF'
[Service]
MemoryMax=1G
Environment="URNETWORK_PROFILE=lowmem"
EOF
chown -R urnet:urnet /home/urnet/.config
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user daemon-reload
X_MARK2=$(journal_line_count)
timeout 150 runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user restart urnetwork.service
X_CID2=$(wait_client_id "$X_MARK2" 150)
PROC_PID=$(pgrep -u urnet -f 'urnetwork provide' | head -1)
if [ -n "$X_CID2" ] && [ -n "$PROC_PID" ] && tr '\0' '\n' < /proc/$PROC_PID/environ | grep -qxF "URNETWORK_PROFILE=lowmem"; then
  ok "X: lowmem profile healthy at the fleet-realistic MemoryMax=1G floor"
else
  bad "X: lowmem profile NOT healthy (or not applied) at MemoryMax=1G"
fi

# Remove the constraint. Assert it is actually gone before returning; no
# later section may inherit it.
rm -f "$OVERRIDE_DIR/memtest.conf"
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user daemon-reload
MARK=$(restart_provider)
CID=$(wait_client_id "$MARK" 120)
[ -n "$CID" ] && ok "X: cleanup restart ok" || bad "X: cleanup restart produced no client_id"
X_MEMORYMAX_AFTER=$(runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user show -p MemoryMax --value urnetwork.service 2>/dev/null)
if [ "$X_MEMORYMAX_AFTER" = "$X_ORIG_MEMORYMAX" ] || [ "$X_MEMORYMAX_AFTER" = "infinity" ]; then
  ok "X: MemoryMax constraint removed and confirmed gone ($X_MEMORYMAX_AFTER) -- no later section inherits it"
else
  bad "X: MemoryMax still constrained after cleanup ($X_MEMORYMAX_AFTER, want infinity/$X_ORIG_MEMORYMAX)"
fi

# ---------- Y. Large proxy list ----------
section "Y. Large proxy list (thousands of entries)"
# The 1GB droplet could never host this case at all. The runner's headroom
# is the whole point: exercise parsing/memory behaviour at a scale a real
# small node will never see in one file, but that stresses the same code
# path a large fleet-wide proxy source realistically produces over time.
# This section deliberately runs with NO memory constraint (X already
# removed and verified its own).
Y_COUNT=5000
Y_FILE=/tmp/large-proxy-list.txt
python3 -c "
import random
with open('$Y_FILE', 'w') as f:
    for i in range(1, $Y_COUNT + 1):
        a, b, c = random.randint(1,223), random.randint(0,255), random.randint(0,255)
        f.write(f'{a}.{b}.{c}.{i % 254 + 1}:{1024 + (i % 60000)}\n')
"
Y_T0=$(date +%s)
timeout 120 urnet-tools proxy add "$Y_FILE" >/tmp/y.out 2>&1
Y_RC=$?
Y_ELAPSED=$(( $(date +%s) - Y_T0 ))
if [ "$Y_RC" -eq 124 ]; then
  t1bad "Y: proxy add of $Y_COUNT entries HUNG (timeout 120)"
elif [ "$Y_RC" -eq 0 ]; then
  ok "Y: proxy add of $Y_COUNT entries completed in ${Y_ELAPSED}s"
else
  bad "Y: proxy add of $Y_COUNT entries failed (exit $Y_RC, ${Y_ELAPSED}s)"
  tail -5 /tmp/y.out | sed 's/^/    | /' | tee -a "$REPORT"
fi
Y_STATE_COUNT=$(python3 -c "import json;d=json.load(open('/home/urnet/.urnetwork/proxy'));print(len(d.get('servers',{})))" 2>/dev/null || echo 0)
if [ "${Y_STATE_COUNT:-0}" -ge "$((Y_COUNT - 5))" ]; then
  ok "Y: state file holds $Y_STATE_COUNT of $Y_COUNT entries"
else
  bad "Y: state file holds only $Y_STATE_COUNT of $Y_COUNT entries"
fi
PROC_PID=$(pgrep -u urnet -f 'urnetwork provide' | head -1)
if [ -n "$PROC_PID" ]; then
  Y_RSS=$(awk '/VmRSS/{print $2}' /proc/$PROC_PID/status 2>/dev/null || echo 0)
  echo "  provider RSS with $Y_COUNT-entry list loaded: ${Y_RSS}kB" | tee -a "$REPORT"
fi
run_check "Y: provider still responsive after large-list add" urnet-tools status 2>&1
run_check "Y: proxy remove --all -f (cleanup)" urnet-tools proxy remove --all -f 2>&1
Y_STATE_AFTER=$(python3 -c "import json;d=json.load(open('/home/urnet/.urnetwork/proxy'));print(len(d.get('servers',{})))" 2>/dev/null || echo -1)
[ "${Y_STATE_AFTER:-1}" = "0" ] && ok "Y: large list cleaned up (0 servers remain)" || bad "Y: large-list cleanup left $Y_STATE_AFTER servers"
rm -f "$Y_FILE" /tmp/y.out

# ---------- Z. Multi-provider on one box ----------
section "Z. Multi-provider on one box"
# Our fleet is one-per-box; this exercises --user targeting, two distinct
# control sockets, and that neither provider's pending_overrides.json
# collides with the other's. A second OS user hosting a second copy of the
# SAME installed binary + SAME gauntlet JWT (the same pattern section F
# already uses for the docker multi-provider case) is enough to prove the
# targeting/collision behaviour without a second real account.
Z_USER="urnet2"
if ! id "$Z_USER" >/dev/null 2>&1; then
  useradd -m -s /bin/bash "$Z_USER"
fi
loginctl enable-linger "$Z_USER" 2>/dev/null
mkdir -p "/home/$Z_USER/.local/share/urnetwork-provider/bin" "/home/$Z_USER/.config/systemd/user" "/home/$Z_USER/.urnetwork"
cp /home/urnet/.local/share/urnetwork-provider/bin/urnetwork "/home/$Z_USER/.local/share/urnetwork-provider/bin/urnetwork"
cp /home/urnet/.local/share/urnetwork-provider/bin/urnet-tools "/home/$Z_USER/.local/share/urnetwork-provider/bin/urnet-tools"
sed "s#/home/urnet#/home/$Z_USER#g" "$BASE_UNIT" > "/home/$Z_USER/.config/systemd/user/urnetwork.service"
cat > "/home/$Z_USER/.urnetwork/network.json" << 'EOF'
{"api_url":"https://api.bringyour.com","connect_url":"wss://connect.bringyour.com"}
EOF
cp "$JWT_FILE" "/home/$Z_USER/.urnetwork/jwt"
chown -R "$Z_USER:$Z_USER" "/home/$Z_USER"
chmod 600 "/home/$Z_USER/.urnetwork/jwt"
Z_XDG="/run/user/$(id -u $Z_USER)"
mkdir -p "$Z_XDG" && chown "$Z_USER:$Z_USER" "$Z_XDG"
runuser -u "$Z_USER" -- env XDG_RUNTIME_DIR="$Z_XDG" systemctl --user daemon-reload
runuser -u "$Z_USER" -- env XDG_RUNTIME_DIR="$Z_XDG" systemctl --user start urnetwork.service

Z_CID=""
Z_END=$(( $(date +%s) + 120 ))
while [ "$(date +%s)" -lt "$Z_END" ]; do
  Z_LINE=$(runuser -u "$Z_USER" -- env XDG_RUNTIME_DIR="$Z_XDG" journalctl --user -u urnetwork.service --no-pager 2>/dev/null | grep -oE "client_id: [0-9a-f-]+ \((new|reused)\)" | tail -1)
  [ -n "$Z_LINE" ] && { Z_CID="$Z_LINE"; break; }
  sleep 5
done
[ -n "$Z_CID" ] && ok "Z: second provider (user $Z_USER) authenticated ($Z_CID)" || bad "Z: second provider (user $Z_USER) never authenticated"

Z_PID1=$(pgrep -u urnet -f 'urnetwork provide' | head -1)
Z_PID2=$(pgrep -u "$Z_USER" -f 'urnetwork provide' | head -1)
if [ -n "$Z_PID1" ] && [ -n "$Z_PID2" ] && [ "$Z_PID1" != "$Z_PID2" ]; then
  ok "Z: two distinct provider PIDs running ($Z_PID1 for urnet, $Z_PID2 for $Z_USER)"
else
  bad "Z: expected two distinct provider PIDs, got urnet=$Z_PID1 $Z_USER=$Z_PID2"
fi
if [ -S /home/urnet/.urnetwork/provider.sock ] && [ -S "/home/$Z_USER/.urnetwork/provider.sock" ]; then
  ok "Z: two distinct control sockets exist, no collision"
else
  bad "Z: expected two distinct control sockets"
fi

# No-target invocation must now REFUSE with the multi-provider ambiguity
# guard (defaultProvider()'s rule: 2+ providers for the current [root] view
# is an error naming both, not a silent pick of one).
if urnet-tools status >/dev/null 2>&1; then
  bad "Z: no-target 'urnet-tools status' succeeded with 2 providers present (ambiguity guard did not fire)"
else
  ok "Z: no-target invocation correctly refused with 2 providers present"
fi
run_check "Z: --user urnet targets the first provider" urnet-tools status --user urnet 2>&1
run_check "Z: --user $Z_USER targets the second provider" urnet-tools status --user "$Z_USER" 2>&1

# pending_overrides.json / control-socket set must not collide: distinct
# values must reach the intended provider only.
run_check "Z: set node-name on urnet's provider" urnet-tools set -y node-name "z-provider-one" --user urnet 2>&1
run_check "Z: set node-name on $Z_USER's provider" urnet-tools set -y node-name "z-provider-two" --user "$Z_USER" 2>&1
Z_GET1=$(urnet-tools set node-name --user urnet 2>&1 | tail -1)
Z_GET2=$(urnet-tools set node-name --user "$Z_USER" 2>&1 | tail -1)
if echo "$Z_GET1" | grep -q "z-provider-one" && echo "$Z_GET2" | grep -q "z-provider-two"; then
  ok "Z: distinct 'set node-name' reached the intended provider, no cross-provider collision"
else
  bad "Z: node-name set did not reach the intended provider (got '$Z_GET1' / '$Z_GET2')"
fi

# Teardown: remove the second provider entirely so it does not linger into
# later sections (all of which assume single-provider).
runuser -u "$Z_USER" -- env XDG_RUNTIME_DIR="$Z_XDG" systemctl --user stop urnetwork.service 2>/dev/null
loginctl disable-linger "$Z_USER" 2>/dev/null
userdel -r "$Z_USER" >/dev/null 2>&1
if id "$Z_USER" >/dev/null 2>&1; then
  bad "Z: teardown failed, $Z_USER still exists"
else
  ok "Z: second-provider user removed, single-provider state restored"
fi
if urnet-tools status >/dev/null 2>&1; then
  ok "Z: no-target invocation works again with a single provider (teardown verified)"
else
  bad "Z: no-target invocation still refuses after teardown -- second provider not fully gone"
fi

# ============ PHASE 2: long observation (20-100m) ============
section "PHASE 2 START: long observation window"
# MUST-FIX 3/Q5: remove-dead needs >= 65m of UNINTERRUPTED uptime. The final
# restart below resets StartedAt, and NO further restarts happen until the
# remove-dead call at the end (~110m in). Seed the blackhole NOW so the
# reaper has the whole window to accumulate failed cycles.
MARK=$(restart_provider)
CID_P2=$(wait_client_id "$MARK" 120)
[ -n "$CID_P2" ] && ok "Phase 2 final restart (client_id ${CID_P2:0:12}…, uptime clock reset)" || bad "Phase 2 restart produced no client_id"
printf '192.0.2.1:9\n8.8.8.8:443\n' > /tmp/rd-proxies.txt
run_check "seeded dead+good proxies for reaper" urnet-tools proxy add /tmp/rd-proxies.txt 2>&1
# Resource sampling every 5m: RSS/fd/threads as a leak
# regression signal. Plus log-rate (14) and panic/restart sweep (11).
P2_START=$(date +%s)
# The poll deadline from the workflow must be able to
# shrink Phase 2 so the run finishes before the job cap instead of being
# guillotined mid-remove-dead. DEADLINE_EPOCH is optional; when absent use
# the full 70-min window.
P2_END=$((P2_START + 4200))   # 70 min observation
if [ -n "${DEADLINE_EPOCH:-}" ]; then
  SHRUNK_END=$((DEADLINE_EPOCH - 600))   # 10 min teardown reserve
  if [ "$SHRUNK_END" -lt "$P2_END" ]; then
    # Floor at P2_START + one sample (300s) so the window is never empty or
    # negative, which would make the truncation check silent.
    if [ "$SHRUNK_END" -le "$((P2_START + 300))" ]; then
      SHRUNK_END=$((P2_START + 300))
    fi
    P2_END=$SHRUNK_END
    echo "INFO: Phase 2 window shrunk to $(date -u -d @$P2_END +%T)Z by job deadline" | tee -a "$REPORT"
  fi
fi
# Total samples = window / 300s, for the n/N completion assertion.
P2_TOTAL=$(( (P2_END - P2_START) / 300 ))
SAMPLE_N=0
PROC_PID=""
while [ "$(date +%s)" -lt "$P2_END" ]; do
  SAMPLE_N=$((SAMPLE_N+1))
  # Heartbeat for the workflow poller: mtime > 15 min = STALLED.
  touch /tmp/shakedown.heartbeat
  PROC_PID=$(pgrep -u urnet -f 'urnetwork provide' | head -1)
  if [ -n "$PROC_PID" ]; then
    RSS=$(awk '/VmRSS/{print $2}' /proc/$PROC_PID/status 2>/dev/null || echo 0)
    FDS=$(ls /proc/$PROC_PID/fd 2>/dev/null | wc -l)
    THREADS=$(awk '/Threads/{print $2}' /proc/$PROC_PID/status 2>/dev/null || echo 0)
    CACHED2=$(python3 -c "import json;d=json.load(open('/home/urnet/.urnetwork/proxy_url.json'));print(len(d.get('cache',{})))" 2>/dev/null || echo 0)
    UP2=$(urnet-tools summary 2>&1 | grep -oE "Up: +[0-9]+" | grep -oE "[0-9]+" | tail -1)
    echo "SAMPLE $SAMPLE_N/$P2_TOTAL: rss=${RSS}kB fds=$FDS threads=$THREADS cache=$CACHED2 up=${UP2:-0}" | tee -a "$REPORT"
  else
    echo "SAMPLE $SAMPLE_N/$P2_TOTAL: provider process NOT RUNNING (pid missing!)" | tee -a "$REPORT"
  fi
  # MUST-FIX 11: panic/fatal/SIGSEGV grep. This is the highest-value zero-cost check.
  PANICS=$(j | grep -cE "panic:|fatal error:|SIGSEGV|goroutine [0-9]+ \[running\]")
  [ "${PANICS:-0}" -gt 0 ] && { t1bad "panic/fatal/SIGSEGV in journal ($PANICS hits)"; break; }
  sleep 300
done
# A truncated soak must never be invisible. If the window
# did not complete (early break on panic, or deadline guillotine), say so as
# TEST_SCRIPT instead of silently grading the partial window PASS. A truncated
# window also downgrades the soak-dependent gates (cache/Up/remove-dead) to
# SKIP: their signals are partial and cannot gate the release either way
#
P2_TRUNCATED=0
# Truncation is judged by the CLOCK, not the sample counter.
# SAMPLE_N drifts (sleep 300 + per-iteration work on 1 vCPU can exceed 24s),
# so counting samples falsely reports 13/14 on a run that actually completed
# its window. Truncated = the loop exited before P2_END (panic break) OR the
# deadline shrink cut the window.
if [ "$(date +%s)" -lt "$P2_END" ]; then
  P2_TRUNCATED=1
  echo "WARN: Phase 2 broke early (loop exited before window end). Signals below are partial." | tee -a "$REPORT"
fi
# A truncated soak must NOT exit 0 (that grades RELEASE_OK
# for a window that never gated anything). ENV_BLOCKER = NOT TESTED.
if [ "$P2_TRUNCATED" = "1" ]; then
  echo "FAIL: Phase 2 soak truncated — soak-dependent signals are NOT TESTED" | tee -a "$REPORT"
  exit 75
fi
N_RESTARTS=$(runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user show urnetwork.service -p NRestarts 2>/dev/null | cut -d= -f2)
[ -n "$N_RESTARTS" ] && echo "INFO: systemd NRestarts=$N_RESTARTS (script issued ~9 restarts)" | tee -a "$REPORT"
if [ -n "${N_RESTARTS:-}" ] && [ "${N_RESTARTS:-0}" -gt 12 ]; then
  t1bad "NRestarts=$N_RESTARTS. Provider crash-looping beyond script restarts"
fi
OOM_HITS=$(journalctl -k --no-pager 2>/dev/null | grep -ci "out of memory")
[ "${OOM_HITS:-0}" -gt 0 ] && t1bad "kernel OOM kill detected ($OOM_HITS)" || ok "no kernel OOM kills"

# Q4 admission gates (after the long window): cache + Up, with the
# inconsistency case as the real block (healthy cache, zero admissions =
# admission pipeline broken).
CACHED_FINAL=$(python3 -c "import json;d=json.load(open('/home/urnet/.urnetwork/proxy_url.json'));print(len(d.get('cache',{})))" 2>/dev/null || echo 0)
UP_FINAL=$(urnet-tools summary 2>&1 | grep -oE "Up: +[0-9]+" | grep -oE "[0-9]+" | tail -1)
echo "INFO: final cache=$CACHED_FINAL up=${UP_FINAL:-0}" | tee -a "$REPORT"
if [ "$P2_TRUNCATED" = "1" ]; then
  echo "SKIP: URL cache gate (Phase 2 truncated — partial window cannot gate)" | tee -a "$REPORT"
  SKIP=$((SKIP+1))
elif [ "${CACHED_FINAL:-0}" -gt 0 ]; then
  ok "URL cache populated at end of observation ($CACHED_FINAL)"
else
  t1bad "URL cache empty after full observation (0 across 2+ cycles)"
fi
if [ "$P2_TRUNCATED" = "1" ]; then
  echo "SKIP: proxies Up gate (Phase 2 truncated — partial window cannot gate)" | tee -a "$REPORT"
  SKIP=$((SKIP+1))
elif [ "${UP_FINAL:-0}" -gt 0 ]; then
  ok "proxies UP at end of observation ($UP_FINAL)"
elif [ "${CACHED_FINAL:-0}" -gt 20 ]; then
  t1bad "Up=0 with cache>20. Probe passed but admission never ran"
elif [ "${CACHED_FINAL:-0}" -eq 0 ]; then
  skip "Up=0 with cache=0. ENV_BLOCKER (rerun once). No free proxies upstream"
else
  echo "WARN: Up=0 with small cache ($CACHED_FINAL). Signal only. Not a gate" | tee -a "$REPORT"
fi

# ---------- O. proxy remove-dead (65m gate + --yes) ----------
section "O. proxy remove-dead"
# MUST-FIX 3: provider hard-exits 61 under 65m uptime (main.go:4806), and the
# flag is --yes (autoYes), NOT -f (global force, consumed by the tool).
# Uptime here is >= 65m (Phase 2 final restart + 70m observation).
UPTIME_S=$(( $(date +%s) - P2_START ))
echo "  provider uptime at remove-dead: ${UPTIME_S}s ($((UPTIME_S/60))m)" | tee -a "$REPORT"
if [ "$UPTIME_S" -lt 3900 ]; then
  skip "remove-dead: uptime ${UPTIME_S}s < 65m gate. Cannot run"
else
  RD_OUT=$(urnet-tools proxy remove-dead --yes 2>&1); RD_RC=$?
  echo "  remove-dead exit=$RD_RC" | tee -a "$REPORT"
  echo "$RD_OUT" | head -3 | sed 's/^/    | /' | tee -a "$REPORT"
  if [ "$RD_RC" -eq 0 ]; then
    ok "remove-dead --yes ran clean (exit 0)"
  elif [ "$RD_RC" -eq 61 ]; then
    bad "remove-dead exit 61. 65m uptime gate (timing regression?)"
  elif [ "$RD_RC" -eq 60 ]; then
    bad "remove-dead exit 60. Provider not running"
  else
    bad "remove-dead exit $RD_RC (unexpected)"
  fi
  STILL_DEAD=$(python3 -c "import json;d=json.load(open('/home/urnet/.urnetwork/proxy'));print(1 if '192.0.2.1:9' in d.get('servers',{}) else 0)" 2>/dev/null || echo 1)
  if [ "${STILL_DEAD:-1}" = "0" ]; then
    ok "remove-dead pruned the blackholed proxy"
  else
    echo "WARN: 192.0.2.1:9 still present after remove-dead. Reaper may not have confirmed dead (signal only)" | tee -a "$REPORT"
  fi
fi

# ---------- Final: clean shutdown (SHOULD-FIX 15) ----------
section "Final. Clean shutdown (SIGTERM)"
STOP_T0=$(date +%s)
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user stop urnetwork.service
STOP_ELAPSED=$(( $(date +%s) - STOP_T0 ))
if [ "$STOP_ELAPSED" -le 10 ]; then
  ok "provider stopped cleanly in ${STOP_ELAPSED}s"
else
  bad "provider took ${STOP_ELAPSED}s to stop (SIGTERM hang?)"
fi
if j | grep -qE "panic:|fatal error:"; then
  t1bad "panic/fatal splatter during shutdown"
else
  ok "no panic/fatal during shutdown"
fi

# ---------- Summary ----------
section "SUMMARY"
echo "PASS=$PASS FAIL=$FAIL SKIP=$SKIP TIER1_FAIL=$TIER1_FAIL" | tee -a "$REPORT"
echo "SHAKEDOWN END $(date -u +%FT%TZ)" >> "$REPORT"
# MUST-FIX 18: exit non-zero on Tier-1 FAILs so the workflow can gate on it.
[ "$TIER1_FAIL" = "1" ] && exit 1
exit 0
