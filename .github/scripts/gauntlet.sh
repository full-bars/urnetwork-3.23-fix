#!/bin/bash
# gauntlet.sh — the v29 pre-release gauntlet, runs ON the droplet after boot.
# Executed as root on a fresh 1CPU/1GB Ubuntu droplet. Tests the full
# regular-person install flow, Go tooling, proxy paths, URL sources with real
# free proxies, egress, and docker.
#
# Called by do_gauntlet.sh with: $1 = JWT file path (already on the box)
set -u
JWT_FILE="${1:-/tmp/gauntlet.jwt}"
REPORT="/tmp/gauntlet-report.txt"
PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "PASS: $1" | tee -a "$REPORT"; }
bad()  { FAIL=$((FAIL+1)); echo "FAIL: $1" | tee -a "$REPORT"; }
section() { echo "" | tee -a "$REPORT"; echo "===== $1 =====" | tee -a "$REPORT"; }

echo "GAUNTLET START $(date -u +%FT%TZ)" > "$REPORT"

# ---------- A. Fresh install (regular-person path) ----------
section "A. Fresh install"
curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/Provider_Install_Linux.sh -o /tmp/install.sh
bash -n /tmp/install.sh && ok "installer syntax" || bad "installer syntax"
# PTY install: root prompt -> option 1 (create user) -> default name urnet
(cd /tmp && (sleep 2; echo "1"; sleep 2; echo "urnet") | script -qc "sh /tmp/install.sh install" /dev/null 2>&1 | tail -3) | grep -q "Installation complete\|Done. The provider is installed" \
  && ok "fresh install complete" || bad "fresh install"
[ -x /home/urnet/.local/share/urnetwork-provider/bin/urnet-tools ] && ok "Go urnet-tools installed" || bad "Go urnet-tools missing"
URN="runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/\$(id -u urnet)"

# ---------- A2. Preflight: internet + API reachability (non-starters) ----------
section "A2. Preflight connectivity"
# If the box has no internet, or cannot reach the urnetwork API, everything
# downstream is meaningless. Fail hard with a NON-STARTER verdict instead of
# confusing mid-suite FAILs.
NON_STARTER=0
if timeout 15 curl -fsS -o /dev/null -w "%{http_code}" https://api.bringyour.com/auth/verify-send 2>/dev/null | grep -qE "^[0-9]{3}$"; then
  ok "internet + api.bringyour.com reachable"
else
  bad "api.bringyour.com NOT reachable (non-starter)"
  NON_STARTER=1
fi
if timeout 10 curl -fsS -o /dev/null https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/main/README.md 2>/dev/null; then
  ok "github reachable"
else
  bad "github NOT reachable (non-starter)"
  NON_STARTER=1
fi
if [ "$NON_STARTER" = "1" ]; then
  echo "NON-STARTER: connectivity failed — not running the rest of the suite" | tee -a "$REPORT"
  echo "PASS=$PASS FAIL=$FAIL NON_STARTER=1" | tee -a "$REPORT"
  echo "GAUNTLET END $(date -u +%FT%TZ)" >> "$REPORT"
  exit 0
fi
echo "preflight OK — continuing suite" | tee -a "$REPORT"

# ---------- B. Auth ----------
section "B. Auth"
mkdir -p /home/urnet/.urnetwork
cat > /home/urnet/.urnetwork/network.json << 'EOF'
{"api_url":"https://api.bringyour.com","connect_url":"wss://connect.bringyour.com"}
EOF
cp "$JWT_FILE" /home/urnet/.urnetwork/jwt
chown -R urnet:urnet /home/urnet/.urnetwork && chmod 600 /home/urnet/.urnetwork/jwt
export XDG_RUNTIME_DIR=/run/user/$(id -u urnet)
runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) systemctl --user start urnetwork.service
sleep 8
if runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) journalctl --user -u urnetwork.service --no-pager 2>/dev/null | grep -qE "client_id: .* (new|reused)"; then
  ok "auth (client_id minted)"
else
  # Dump the journal so a failure is diagnosable (timing vs real auth issue).
  echo "--- provider journal (auth failed) ---" | tee -a "$REPORT"
  runuser -u urnet -- env XDG_RUNTIME_DIR=/run/user/$(id -u urnet) journalctl --user -u urnetwork.service --no-pager -n 30 2>/dev/null | tail -30 | tee -a "$REPORT"
  bad "auth"
fi
export PATH=/home/urnet/.local/share/urnetwork-provider/bin:$PATH

# ---------- C. Go tool basics ----------
section "C. Go tool"
V=$(urnet-tools version 2>&1)
echo "$V" | grep -qE "v3\.23\.0-fix\.29" && ok "tool version $V" || bad "tool version ($V)"
urnet-tools providers 2>&1 | grep -qE "mesocyclone|bringyour" && ok "providers discovers the account" || bad "providers"
urnet-tools status 2>&1 | grep -q "running:      true" && ok "status running" || bad "status"
urnet-tools proxy health >/dev/null 2>&1 && ok "proxy health cmd" || bad "proxy health"

# ---------- D. Proxy lifecycle ----------
section "D. Proxy lifecycle"
printf "1.1.1.1:443\n8.8.8.8:443\n9.9.9.9:443:testuser:testpass\n" > /tmp/tp.txt
urnet-tools proxy add /tmp/tp.txt 2>&1 | grep -q "added server" && ok "proxy add" || bad "proxy add"
python3 -c "import json;d=json.load(open('/home/urnet/.urnetwork/proxy'));assert len(d.get('servers',{}))==3" && ok "3 servers in state" || bad "state count"
urnet-tools proxy remove --all -f >/dev/null 2>&1
python3 -c "import json;d=json.load(open('/home/urnet/.urnetwork/proxy'));assert len(d.get('servers',{}))==0" && ok "proxy remove --all -f" || bad "proxy remove"

# ---------- E. URL source (real free proxies) ----------
section "E. URL sources + egress"
# Speed up the URL pipeline: the default refresh is 1h; for the gauntlet we
# want to observe fetch->probe->grade->admit within the run, so set a 3m
# cadence via the installer-supported runtime override file (re-read live).
printf '3m\n' > /home/urnet/.urnetwork/proxy_url_refresh
chown urnet:urnet /home/urnet/.urnetwork/proxy_url_refresh
urnet-tools proxy add-source https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/socks5.txt 2>&1 | grep -q "added source" && ok "add-source (monosans socks5)" || bad "add-source (monosans)"
urnet-tools proxy add-source https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/all/data.txt 2>&1 | grep -q "added source" && ok "add-source (proxifly all)" || bad "add-source (proxifly)"
urnet-tools summary 2>&1 | grep -q "Source URLs:        2" && ok "both sources registered" || bad "source registration"
# Give the fetch+probe time (102 monosans candidates x 4s = ~7 min on 1CPU;
# the 3m refresh cadence overlaps). 360s lets most candidates get probed.
sleep 360
CACHED=$(python3 -c "import json;d=json.load(open('/home/urnet/.urnetwork/proxy_url.json'));print(len(d.get('cache',{})))" 2>/dev/null || echo 0)
UP=$(urnet-tools summary 2>&1 | grep -oE "Up: +[0-9]+" | grep -oE "[0-9]+")
# If auth failed earlier, the URL pipeline cannot run — report it as skipped,
# not as a product failure (the auth check above already flagged the cause).
if grep -q "FAIL: auth" "$REPORT"; then
  echo "SKIP: URL cache (auth failed earlier — see auth check)" | tee -a "$REPORT"
elif [ "${CACHED:-0}" -gt 0 ]; then
  ok "URL cache populated ($CACHED)"
else
  bad "URL cache empty"
fi
if grep -q "FAIL: auth" "$REPORT"; then
  echo "SKIP: proxies up (auth failed earlier)" | tee -a "$REPORT"
elif [ "${UP:-0}" -gt 0 ]; then
  ok "proxies UP ($UP)"
else
  bad "no proxies up"
fi

# ---------- F. Docker path ----------
section "F. Docker"
apt-get update -qq >/dev/null 2>&1   # fresh boxes need an updated index first
apt-get install -y -qq docker.io >/dev/null 2>&1 && systemctl start docker && ok "docker installed" || bad "docker install"
curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/install-urnet-docker.sh -o /tmp/install-docker.sh
sh /tmp/install-docker.sh 2>&1 | grep -q "sha256 verified" && ok "install-urnet-docker.sh verified" || bad "docker installer"
/usr/local/bin/urnet-docker version 2>&1 | grep -q "v3.23.0-fix.29" && ok "urnet-docker version" || bad "urnet-docker version"
mkdir -p /tmp/docker-state && cp /home/urnet/.urnetwork/jwt /tmp/docker-state/jwt && cp /home/urnet/.urnetwork/network.json /tmp/docker-state/network.json
docker pull ghcr.io/full-bars/urnetwork-3.23-fix:latest >/dev/null 2>&1 && ok "image pulled" || bad "image pull"
docker run -d --name urnetwork-test -v /tmp/docker-state:/root/.urnetwork -e BUILD=jwt ghcr.io/full-bars/urnetwork-3.23-fix:latest >/dev/null 2>&1
sleep 8
docker ps --format "{{.Names}}" | grep -q urnetwork-test && ok "container up" || bad "container"
/usr/local/bin/urnet-docker providers 2>&1 | grep -q "urnetwork-test" && ok "urnet-docker providers" || bad "urnet-docker providers"
/usr/local/bin/urnet-docker restart urnetwork-test -f >/dev/null 2>&1 && ok "urnet-docker restart -f" || bad "urnet-docker restart"

# ---------- G. Self-update ----------
section "G. Self-update"
urnet-tools self-update -f 2>&1 | grep -qE "already on|updated" && ok "self-update -f" || bad "self-update"

# ---------- Summary ----------
section "SUMMARY"
echo "PASS=$PASS FAIL=$FAIL" | tee -a "$REPORT"
echo "GAUNTLET END $(date -u +%FT%TZ)" >> "$REPORT"
exit 0
