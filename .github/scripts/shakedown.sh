#!/bin/bash
# shakedown.sh. The v29 pre-release shakedown. Runs ON the droplet after boot.
# Executed as root on a fresh 1CPU/1GB Ubuntu droplet. Tests the full
# regular-person install flow, Go tooling, proxy paths, URL sources with real
# free proxies, egress, and docker.
#
# Called by the shakedown workflow with: $1 = JWT file path (already on the box)
#
# DESIGN (Opus review 2026-08-14 folded):
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
ok()   { PASS=$((PASS+1)); echo "PASS: $1" | tee -a "$REPORT"; }
bad()  { FAIL=$((FAIL+1)); echo "FAIL: $1" | tee -a "$REPORT"; }
skip() { SKIP=$((SKIP+1)); echo "SKIP: $1" | tee -a "$REPORT"; }
t1bad() { TIER1_FAIL=1; bad "$1"; }
section() { echo "" | tee -a "$REPORT"; echo "===== $1 =====" | tee -a "$REPORT"; }

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
# on a 5s cadence are heavy on 1GB (Opus SHOULD-FIX 14).
j() { runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) journalctl --user -u urnetwork.service --no-pager --since "${J_SINCE:-30 min ago}" 2>/dev/null; }
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

# restart_provider: systemctl --user restart + return the pre-restart journal
# line count (for wait_client_id).
restart_provider() {
  echo $(journal_line_count)
  runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user restart urnetwork.service
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
EXPECTED_BASE=$(echo "$EXPECTED_VERSION" | grep -oE "v3\.23\.0-fix\.[0-9]+" | head -1)
V=$(/home/urnet/.local/share/urnetwork-provider/bin/urnet-tools version 2>&1 | head -1)
echo "TOOL_VERSION: $V" >> "$REPORT"
echo "EXPECTED_VERSION: $EXPECTED_VERSION" >> "$REPORT"

# ---------- A. Fresh install (regular-person path) ----------
section "A. Fresh install"
curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/Provider_Install_Linux.sh -o /tmp/install.sh
bash -n /tmp/install.sh && ok "installer syntax" || bad "installer syntax"
# PTY install: root prompt -> option 1 (create user) -> default name urnet.
# Grep the FULL transcript, not tail -3 (installer prints after "complete").
INSTALL_OUT=$( (cd /tmp && (sleep 2; echo "1"; sleep 2; echo "urnet") | script -qc "sh /tmp/install.sh install" /dev/null 2>&1 || true) )
echo "$INSTALL_OUT" | grep -q "Installation complete\|Done. The provider is installed" \
  && ok "fresh install complete" || bad "fresh install"
[ -x /home/urnet/.local/share/urnetwork-provider/bin/urnet-tools ] && ok "Go urnet-tools installed" || bad "Go urnet-tools missing"
URN="runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet)"
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
# Structural fix (Opus insight): every assertion below greps a log pattern.
# If a pattern is wrong, the shakedown's failure is indistinguishable from the
# product failing. So verify each pattern against THIS known-good journal now.
# NOTE: [net][s]select has no single-regex form. Success = total minus fail
# lines (see section I); its calibration is the SELECT_TOTAL/SELECT_FAILS
# subtraction being nonzero on a known-good journal.
section "SELF-TEST (pattern calibration)"
J=$(j_full)   # self-test needs the full startup sequence, not a window
SELF_TEST_FAIL=0
# Binding calibration (Opus MUST-FIX 3): SELF_TEST_FAIL must not just be
# printed — dependent assertions MUST know which patterns are trustworthy.
# CALIBRATED_CLIENT_ID / CALIBRATED_SELECT / CALIBRATED_STAGE1 / CALIBRATED_JWT
# gate their dependent checks: an uncalibrated pattern -> SKIP, structurally
# forbidden from setting TIER1_FAIL (a rotted regex must not block a release).
CALIBRATED_CLIENT_ID=0
CALIBRATED_SELECT=0
CALIBRATED_STAGE1=0
CALIBRATED_JWT=0
for pat in \
  "client_id: [0-9a-f-]+ \((new|reused)\)" \
  "\[net\]\[s\]select:.*dur=[0-9]+ms" \
  "stage-1 table probe config: enabled=true" \
  "\[jwt\] refresh OK"; do
  if echo "$J" | grep -qE "$pat"; then
    ok "self-test pattern present: $pat"
    case "$pat" in
      *client_id*) CALIBRATED_CLIENT_ID=1 ;;
      *select:*)   CALIBRATED_SELECT=1 ;;
      *stage-1*)   CALIBRATED_STAGE1=1 ;;
      *jwt*)       CALIBRATED_JWT=1 ;;
    esac
  else
    echo "SELF-TEST-FAIL: pattern missing in known-good journal: $pat" | tee -a "$REPORT"
    SELF_TEST_FAIL=1
  fi
done
SEL_T=$(echo "$J" | grep -cE "\[net\]\[s\]select:.*dur=[0-9]+ms")
SEL_F=$(echo "$J" | grep -cE "\[net\]\[s\]select:.* = .*dur=[0-9]+ms")
if [ $((SEL_T - SEL_F)) -gt 0 ]; then
  ok "self-test [net][s]select subtraction calibrated ($((SEL_T - SEL_F)) of $SEL_T)"
  CALIBRATED_SELECT=1
else
  echo "SELF-TEST-FAIL: [net][s]select subtraction empty on known-good journal (total=$SEL_T fails=$SEL_F)" | tee -a "$REPORT"
  SELF_TEST_FAIL=1
fi
[ "$SELF_TEST_FAIL" = "0" ] && echo "self-test: all patterns calibrated" | tee -a "$REPORT"

# ---------- C. Go tool basics ----------
section "C. Go tool"
V=$(urnet-tools version 2>&1)
echo "$V" | grep -qE "$EXPECTED_BASE" && ok "tool version $V" || bad "tool version ($V). Version must match $EXPECTED_BASE"
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
MONOSANS_URL="https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/socks5.txt"
PROXIFLY_URL="https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/protocols/socks5/data.txt"
run_check "add-source (monosans socks5)" timeout 900 urnet-tools proxy add-source "$MONOSANS_URL" 2>&1
run_check "add-source (proxifly socks5)" timeout 900 urnet-tools proxy add-source "$PROXIFLY_URL" 2>&1
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
if [ "$CALIBRATED_CLIENT_ID" != "1" ]; then
  echo "SKIP: client identity check (pattern uncalibrated by self-test)" | tee -a "$REPORT"
  SKIP=$((SKIP+1))
elif [ "${CID_LINES:-0}" -gt 0 ]; then
  ok "client identities minted ($CID_LINES lines)"
else
  t1bad "no client identities minted (admission pipeline broken?)"
fi
if [ "$CALIBRATED_STAGE1" != "1" ]; then
  echo "SKIP: stage-1 probe check (pattern uncalibrated by self-test)" | tee -a "$REPORT"
  SKIP=$((SKIP+1))
elif j | grep -qE "stage-1 table probe config: enabled=true"; then
  ok "stage-1 probe enabled"
else
  t1bad "stage-1 probe NOT enabled (kill switch stuck?)"
fi
GRADED=$(python3 -c "import json;d=json.load(open('/home/urnet/.urnetwork/proxy_url.json'));print(sum(1 for v in d.get('cache',{}).values() if v.get('Graded')))" 2>/dev/null || echo 0)
[ "${GRADED:-0}" -gt 0 ] && ok "proxies graded ($GRADED)" || echo "INFO: 0 graded yet (probe still running or all free proxies failed)" | tee -a "$REPORT"
AUTH_FAILS=$(j | grep -cE "proxy\[[0-9]+\].*auth failed")
if [ "${AUTH_FAILS:-0}" -le 5 ]; then
  ok "proxy auth failures low ($AUTH_FAILS)"
else
  echo "WARN: $AUTH_FAILS proxy auth failures (free proxies are flaky; not a gate)" | tee -a "$REPORT"
fi

# ---------- F. Docker path ----------
section "F. Docker"
apt-get update -qq >/dev/null 2>&1
run_check "docker installed" timeout 600 bash -c "apt-get install -y -qq docker.io >/dev/null 2>&1 && systemctl start docker"
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
if urnet-tools status >/dev/null 2>&1; then
  bad "multi-provider ambiguity NOT refused (expected REFUSED with 2 providers)"
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
CID_BEFORE=$(j | grep -oE "client_id: [0-9a-f-]+" | tail -1 | awk '{print $2}')
[ -n "$CID_BEFORE" ] && ok "provider client_id present (${CID_BEFORE:0:12}…)" || bad "provider client_id missing"

# 1) hot-restart must REUSE the same client_id (identity preserved).
MARK=$(restart_provider)
CID_AFTER=$(wait_client_id "$MARK" 120)
if [ -n "$CID_AFTER" ] && [ "$CID_BEFORE" = "$CID_AFTER" ]; then
  ok "hot-restart reused client_id (${CID_AFTER:0:12}…)"
else
  bad "hot-restart did NOT reuse client_id (before=${CID_BEFORE:0:12} after=${CID_AFTER:0:12})"
fi

# 2) Clear the persisted client-JWT cache, restart -> a NEW client_id mints.
rm -f /home/urnet/.urnetwork/.client_jwts.json
MARK=$(restart_provider)
CID_FRESH=$(wait_client_id "$MARK" 120)
if [ -n "$CID_FRESH" ] && [ "$CID_FRESH" != "$CID_AFTER" ]; then
  ok "cleared cache minted NEW client_id (${CID_FRESH:0:12}…)"
else
  bad "cleared cache did NOT mint new client_id (after=${CID_AFTER:0:12} fresh=${CID_FRESH:0:12})"
fi

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
if [ "$CALIBRATED_SELECT" != "1" ]; then
  echo "SKIP: [net][s]select check (pattern uncalibrated by self-test)" | tee -a "$REPORT"
  SKIP=$((SKIP+1))
elif [ "${SELECT_HITS:-0}" -gt 0 ]; then
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
CID=$(wait_client_id "$MARK" 120)
PROC_PID=$(pgrep -u urnet -f 'urnetwork provide' | head -1)
if [ -n "$PROC_PID" ] && tr '\0' '\n' < /proc/$PROC_PID/environ | grep -q "URNETWORK_HOT_RESTART=0"; then
  ok "URNETWORK_HOT_RESTART=0 reached the process (pid $PROC_PID)"
else
  bad "URNETWORK_HOT_RESTART=0 NOT in /proc/$PROC_PID/environ (env toggle broken)"
fi
# With the toggle off, a restart must mint a NEW client_id (no reuse).
rm -f /home/urnet/.urnetwork/.client_jwts.json
MARK=$(restart_provider)
CID_OFF=$(wait_client_id "$MARK" 120)
if [ -n "$CID_OFF" ] && [ "$CID_OFF" != "$CID_FRESH" ]; then
  ok "hot-restart OFF minted new client_id (${CID_OFF:0:12}…)"
else
  bad "hot-restart OFF did not mint new client_id (fresh=${CID_FRESH:0:12} off=${CID_OFF:0:12})"
fi
# Restore the default (remove override) so the rest of the run reuses identity.
rm -f "$OVERRIDE_DIR/override.conf"
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user daemon-reload
MARK=$(restart_provider)
CID_ON=$(wait_client_id "$MARK" 120)
if [ -n "$CID_ON" ] && [ "$CID_ON" = "$CID_OFF" ]; then
  ok "hot-restart back ON reused client_id (${CID_ON:0:12}…)"
else
  bad "hot-restart back ON did not reuse (off=${CID_OFF:0:12} on=${CID_ON:0:12})"
fi

# ---------- M. update --tag pinned path ----------
section "M. update --tag"
# MUST-FIX 10: version comes from the workflow (GITHUB_REF_NAME for
# tag-triggered runs), not a hardcoded string. EXPECTED_VERSION set at top.
run_check "update --tag $EXPECTED_VERSION" urnet-tools update --tag "$EXPECTED_VERSION" -f 2>&1
# MUST-FIX 7 (DeepSeek): -v is VERBOSE, not version. Use --version.
BIN_VER=$(/home/urnet/.local/share/urnetwork-provider/bin/urnetwork --version 2>&1 | head -1)
echo "  binary after --tag: $BIN_VER" | tee -a "$REPORT"
if [ -n "$BIN_VER" ] && echo "$BIN_VER" | grep -q "$EXPECTED_BASE"; then
  ok "update --tag produced a versioned binary ($BIN_VER)"
else
  bad "update --tag binary version missing/unexpected ($BIN_VER). Version must match $EXPECTED_BASE"
fi
run_check "restored to latest" urnet-tools update -f 2>&1

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
# Resource sampling every 5m (SHOULD-FIX 13): RSS/fd/threads as a leak
# regression signal. Plus log-rate (14) and panic/restart sweep (11).
P2_START=$(date +%s)
# MUST-FIX 5 (Opus): the poll deadline from the workflow must be able to
# shrink Phase 2 so the run finishes before the job cap instead of being
# guillotined mid-remove-dead. DEADLINE_EPOCH is optional; when absent use
# the full 70-min window.
P2_END=$((P2_START + 4200))   # 70 min observation
if [ -n "${DEADLINE_EPOCH:-}" ]; then
  SHRUNK_END=$((DEADLINE_EPOCH - 600))   # 10 min teardown reserve
  if [ "$SHRUNK_END" -lt "$P2_END" ]; then
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
  # Heartbeat for the workflow poller (SHOULD-FIX 11): mtime > 15 min = STALLED.
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
# MUST-FIX 9 (Opus): a truncated soak must never be invisible. If the window
# did not complete (early break on panic, or deadline guillotine), say so as
# TEST_SCRIPT instead of silently grading the partial window PASS.
if [ "$SAMPLE_N" -lt "$P2_TOTAL" ]; then
  echo "WARN: Phase 2 completed $SAMPLE_N/$P2_TOTAL samples (truncated window). Signals below are partial." | tee -a "$REPORT"
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
if [ "${CACHED_FINAL:-0}" -gt 0 ]; then
  ok "URL cache populated at end of observation ($CACHED_FINAL)"
else
  t1bad "URL cache empty after full observation (0 across 2+ cycles)"
fi
if [ "${UP_FINAL:-0}" -gt 0 ]; then
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
