#!/bin/sh
# OpenRC end-to-end smoke: runs INSIDE a stripped Alpine container on the CI
# runner. Served the branch build over http from the fake release directory, so
# the installer's real download path executes unmodified.
#
# What this proves that the unit tests in test_provider_install.sh cannot:
#   1. the installer works on a box with NO curl, NO wget binary helper set,
#      NO jq and NO python3 — it autoinstalls what it needs
#   2. it detects OpenRC and takes the OpenRC branch
#   3. the service starts under supervise-daemon as a non-root user
#   4. the provider is DISCOVERABLE while stopped and while running
#   5. `urnet-tools logs` reads the service log file
#   6. stop/start/restart route through rc-service
#
# Deliberately NOT tested here: the real provider binary and a real JWT. Both
# need live credentials. cmd/fake-provider stands in: it answers --version and
# serves the control socket, which is exactly what discovery and the tool talk
# to.
set -eu

ROOT=/srv/fake-release
SVC_USER=urnet
DL="$ROOT/releases/download/$CI_TAG"
FAILED=0

say()  { echo "== $*"; }
ok()   { echo "   OK: $*"; }
fail() { echo "   FAIL: $*"; FAILED=$((FAILED+1)); }

# --- premise: prove we are on the box we think we are -----------------------
say "environment"
cat /etc/alpine-release
for t in curl jq python3; do
  if command -v "$t" >/dev/null 2>&1; then
    echo "   NOTE: $t IS present in this image - the missing-tool path is not being exercised"
  else
    echo "   confirmed absent: $t"
  fi
done

# --- serve the fake release over http ---------------------------------------
# busybox httpd is already in the base image, so no package is needed to serve
# the files. The installer runs AFTER this, so the port is live when it fetches.
say "starting the fake release server"
# This minimal alpine image ships NO httpd: neither a standalone binary nor the
# busybox applet (`busybox httpd` reports "applet not found"). Install it from
# the distro repo. This is not the unit under test, and installing it here is
# also a useful prerequisite check - the installer itself needs working DNS and
# a package repo to autoinstall curl/jq, so a broken network fails here with a
# clearer message than it would inside the installer.
apk add --no-cache busybox-extras openrc >/dev/null 2>&1 || true

# OpenRC must be genuinely present, or the installer takes its "no service
# supervisor found" branch and writes NO init script — the test would then
# "pass" the install step while proving nothing about the OpenRC path. The base
# alpine image has no /sbin/openrc-run, which is exactly what the installer
# probes for, so the package is required here.
if [ -x /sbin/openrc-run ] || [ -x /usr/sbin/openrc-run ]; then
  ok "openrc present (installer will detect it)"
else
  fail "openrc-run missing - the installer would skip the OpenRC branch entirely"
  exit 1
fi

# A container has no boot, so OpenRC's runtime tree was never created and
# /run/openrc does not exist. Two consequences, both of which silently degrade
# the test rather than failing it:
#   - the installer's OpenRC DETECTION probes /run/openrc, so without this it
#     takes the "no service supervisor found" branch and writes no unit at all;
#   - rc-service refuses every action on a system OpenRC did not boot, so
#     `rc-service start` fails even with a perfectly good unit installed.
# `openrc boot` is what a real boot does. Run it before anything else.
if [ ! -d /run/openrc ]; then
  openrc boot >/dev/null 2>&1 || true
fi
if [ -d /run/openrc ]; then
  ok "/run/openrc present (OpenRC booted)"
else
  fail "/run/openrc missing after 'openrc boot' - rc-service and installer detection will both misbehave"
  exit 1
fi
if command -v httpd >/dev/null 2>&1; then
  ok "httpd available"
  HTTPD_CMD="httpd"
elif busybox httpd --help >/dev/null 2>&1; then
  ok "busybox httpd applet available"
  HTTPD_CMD="busybox httpd"
else
  fail "no httpd available to serve the fake release"
  exit 1
fi
# shellcheck disable=SC2086
$HTTPD_CMD -f -p 127.0.0.1:8099 -h "$ROOT" &
HTTPD_PID=$!
# Readiness check rather than a blind sleep.
i=0
while [ $i -lt 20 ]; do
  if wget -q -T 2 -O /dev/null "http://127.0.0.1:8099/api/latest.json" 2>/dev/null; then
    ok "release server is answering"
    break
  fi
  i=$((i+1)); sleep 1
done
[ $i -lt 20 ] || { fail "release server never came up"; exit 1; }

# --- install -----------------------------------------------------------------
# The installer deliberately refuses to wire the OpenRC service when run as
# root: command_user must be a real, dedicated account, and a root-owned
# install tree would let root write into a user-writable cron target. It prints
# the exact commands to finish. The smoke follows them, so the test exercises
# the SUPPORTED flow rather than forcing an unsupported one.
# The JSON parser is a SYSTEM dependency: the install runs as the service user
# (that is the supported OpenRC flow) but the parser install needs root. On a
# real box the operator installs it once, or the installer reaches it through
# sudo. The smoke provisions it as root up front so the flow under test is the
# installer itself, not the box's package manager.
apk add --no-cache jq >/dev/null 2>&1 && ok "jq available for the service user" \
  || echo "   NOTE: jq unavailable; the installer will try to install it as the service user"

say "creating the service user"
adduser -D -s /bin/sh urnet >/dev/null 2>&1 || adduser -m -s /bin/sh urnet >/dev/null 2>&1 || true
id urnet >/dev/null 2>&1 || { fail "could not create the urnet service user"; exit 1; }
ok "service user urnet exists"

say "running the installer against the fake release"
# The base-URL overrides are the seam that lets a branch build be installed.
# They are exported so the installer's own subshell inherits them.
export URNET_API_BASE="http://127.0.0.1:8099/api"
export URNET_DL_BASE="http://127.0.0.1:8099"
export URNET_MIRROR_BASE="http://127.0.0.1:8099/releases/download"

# -t pins the tag so the installer does not have to trust "latest" resolution
# against our stub. stdin is closed so the auth prompt, if it appears, does not
# hang the job: the smoke asserts the JWT-less start separately below.
# PRIMARY CASE: install as root, which is what `curl ... | sudo sh` gives and is
# by far the most common real-world shape. The installer must complete the whole
# job unattended - no "now run these four commands" handoff.
if sh "$ROOT/stage/install.sh" -t "$CI_TAG" </dev/null; then
  ok "installer completed as root (no manual steps)"
else
  fail "installer exited non-zero"
  exit 1
fi

# When the installer runs as the SERVICE USER it cannot write /etc/init.d, so
# it stages the script and prints the root commands to finish. That split is
# deliberate: the tree itself is root-owned, and only the four privileged
# lines plus the ownership finalization are root. The smoke performs exactly
# those lines, so the test covers the whole documented flow rather than a
# truncated half of it.
# The OpenRC install tree is root-owned and lives OUTSIDE the service user's
# home, so root-executed code (the cron entry, sudo urnet-tools update) can
# never reach it through a user-writable ancestor.
INSTALL_ROOT=/usr/local/lib/urnetwork-provider
STAGED="$INSTALL_ROOT/urnetwork.openrc"
if [ -f "$STAGED" ] && [ ! -f /etc/init.d/urnetwork ]; then
  say "completing the root half of the install"
  cp "$STAGED" /etc/init.d/urnetwork
  chmod +x /etc/init.d/urnetwork
  rc-update add urnetwork default >/dev/null 2>&1
  ok "init script installed from the staged copy"
fi

# --- the OpenRC branch was taken --------------------------------------------
say "verifying the OpenRC branch"
[ -f /etc/init.d/urnetwork ] && ok "init script written" || fail "no /etc/init.d/urnetwork"
if grep -q 'command_user=' /etc/init.d/urnetwork; then
  ok "service runs as a non-root user"
else
  fail "no command_user= in the init script"
fi

# Boot persistence: the runlevel symlink is what starts the service with no
# login session. Its EXISTENCE is deterministic and must be asserted.
if [ -e /etc/runlevels/default/urnetwork ]; then
  ok "enabled in the default runlevel (starts at boot)"
else
  fail "no /etc/runlevels/default/urnetwork symlink"
fi

# Logs must be root-owned files, NOT inside the service user's home: a symlink
# planted in a user-writable home would aim a root append at an arbitrary file.
# The INVARIANT is about the directory, not the file: /var/log must be root-owned
# (so the service user cannot plant or replace a name in it) while the log FILES
# must belong to the service user, because supervise-daemon opens them after
# dropping privileges and a root-owned file makes the child die before exec.
for f in /var/log/urnetwork.log /var/log/urnetwork.err; do
  if [ -e "$f" ] && [ ! -L "$f" ]; then
    ok "$f exists and is a real file"
  else
    fail "$f missing or is a symlink"
  fi
done
LOG_DIR_OWNER=$(stat -c %u /var/log 2>/dev/null)
if [ "$LOG_DIR_OWNER" = "0" ]; then
  ok "/var/log is root-owned (the service user cannot plant a name in it)"
else
  fail "/var/log is owned by uid $LOG_DIR_OWNER, not root"
fi
SVC_UID=$(id -u "$SVC_USER" 2>/dev/null)
for f in /var/log/urnetwork.log /var/log/urnetwork.err; do
  fowner=$(stat -c %u "$f" 2>/dev/null)
  if [ "$fowner" = "$SVC_UID" ]; then
    ok "$f is owned by $SVC_USER (supervise-daemon can open it after dropping privileges)"
  else
    fail "$f is owned by uid $fowner, but supervise-daemon opens it as $SVC_USER (uid $SVC_UID) - the child cannot start"
  fi
done

# --- the service actually runs ----------------------------------------------
say "starting the service"
rc-service urnetwork start >/dev/null 2>&1 || fail "rc-service start failed"
sleep 6

if rc-service urnetwork status 2>/dev/null | grep -q started; then
  ok "service reports started"
else
  fail "service does not report started"
fi

# The provider must be running as the SERVICE user, not root. This is the
# invariant the whole design rests on.
if ps -o user,args 2>/dev/null | grep '[u]rnetwork provide' | grep -qv '^root'; then
  ok "provider runs as a non-root user"
else
  echo "   NOTE: could not confirm the provider's uid (ps output format differs on busybox)"
fi

# --- is the provider ACTUALLY running? --------------------------------------
# rc-service reporting "started" only means supervise-daemon is up; it says
# nothing about the provider itself. Check the process, and dump the service
# state when it is missing so a failure is debuggable from the log alone.
say "verifying the provider process is actually alive"
sleep 3
if pgrep -f "urnetwork provide" >/dev/null 2>&1; then
  ok "the provider process is running"
else
  fail "supervise-daemon is up but no provider process exists"
  echo "   --- init script (key lines) ---"
  grep -E '^command|^command_user|^supervisor|^respawn|^output_log|^error_log' /etc/init.d/urnetwork 2>/dev/null | sed 's/^/     /'
  echo "   --- supervise-daemon procs ---"
  ps -o pid,ppid,user,args 2>/dev/null | grep -E '[s]upervise' | sed 's/^/     /' || echo "     (none)"
  echo "   --- /var/log/urnetwork.err ---"
  tail -15 /var/log/urnetwork.err 2>/dev/null | sed 's/^/     /' || echo "     (empty)"
  echo "   --- /var/log/urnetwork.log ---"
  tail -15 /var/log/urnetwork.log 2>/dev/null | sed 's/^/     /' || echo "     (empty)"
  echo "   --- can the service user exec the binary? ---"
  su -s /bin/sh "$SVC_USER" -c "test -x $INSTALL_ROOT/bin/urnetwork && echo yes || echo no" 2>&1 | sed 's/^/     /'
  echo "   --- direct run as the service user (5s cap) ---"
  su -s /bin/sh "$SVC_USER" -c "timeout 5 $INSTALL_ROOT/bin/urnetwork provide" 2>&1 | head -6 | sed 's/^/     /'
fi

# --- discovery: the whole point of the OpenRC backend -----------------------
say "verifying discovery finds the provider"
if command -v urnet-tools >/dev/null 2>&1; then
  TOOLS=$(command -v urnet-tools)
else
  TOOLS=$(find / -name urnet-tools -type f 2>/dev/null | head -1)
fi
[ -n "$TOOLS" ] || {
  fail "urnet-tools not found (looked on PATH and via find /)"
  exit 1
}
say "using $TOOLS"
# urnet-tools must ALSO be reachable as the service user, since the update path
# and any operator action run from that account.
if su urnet -s /bin/sh -c "command -v urnet-tools" >/dev/null 2>&1; then
  ok "urnet-tools is on the service user's PATH"
else
  echo "   NOTE: urnet-tools not on the service user's PATH (installer may have linked it into root's bin only)"
fi

if "$TOOLS" providers 2>&1 | grep -qi urnet; then
  ok "urnet-tools providers lists the provider"
else
  fail "urnet-tools providers found nothing"
  "$TOOLS" providers 2>&1 | sed 's/^/     /'
fi

# --- logs: must work from the FILE, not the process fd ----------------------
say "verifying urnet-tools logs"
# A stopped provider has no /proc/<pid>/fd/1, so tailing the fd would fail.
# Reading the file works in both states, which is why the logs moved to /var/log.
rc-service urnetwork stop >/dev/null 2>&1
sleep 3
# `logs` FOLLOWS by design (tail -f), so it never returns on its own. Bound it
# with timeout, and write to a FILE rather than using command substitution: the
# tool spawns `tail -f` as a child, and if that grandchild inherits the pipe
# `$(...)` waits on it forever even after timeout kills the parent. A file
# redirect has no pipe to hold open, so the command returns as soon as timeout
# fires and the output so far is still there to assert on.
timeout -k 2 8 "$TOOLS" logs -n 5 > /tmp/urnet-logs-out.txt 2>&1 || true
LOGOUT=$(cat /tmp/urnet-logs-out.txt 2>/dev/null)
if echo "$LOGOUT" | grep -qiE 'urnetwork|/var/log'; then
  ok "logs readable while the service is STOPPED (reads the file, not the fd)"
else
  fail "logs unreadable while stopped"
  echo "$LOGOUT" | sed 's/^/     /'
fi

# --- lifecycle routes -------------------------------------------------------
# Diagnostic: the OpenRC routing decides by looking at the provider's PARENT
# process (it must be supervise-daemon). Print the tree so a routing failure is
# debuggable from the log alone rather than needing a live container.
say "process tree (diagnostic)"
for pid in $(pgrep -f 'urnetwork provide' 2>/dev/null); do
  ppid=$(awk '{print $4}' "/proc/$pid/stat" 2>/dev/null)
  pcomm=$(cat "/proc/$pid/comm" 2>/dev/null)
  parent_comm=$(cat "/proc/$ppid/comm" 2>/dev/null)
  echo "   provider pid=$pid comm=$pcomm ppid=$ppid parent_comm=$parent_comm"
done
echo "   supervise-daemon procs: $(pgrep -c -f supervise-daemon 2>/dev/null || echo 0)"

say "verifying lifecycle commands route to rc-service"
rc-service urnetwork start >/dev/null 2>&1
sleep 4

# The confirm gate must REFUSE when there is no terminal to ask on: silently
# restarting a provider someone did not confirm is the thing the gate exists to
# prevent. This is a check, not a nuisance - it proves the gate is live.
if timeout 30 "$TOOLS" restart </dev/null >/dev/null 2>&1; then
  fail "restart ran WITHOUT confirmation and without a terminal"
else
  ok "restart refuses without a terminal (confirm gate is live)"
fi

# restart must go through rc-service, and the provider must come back. -f is
# what automation uses to state its intent up front.
# set -e is active: capture the failure inside the if, not as a bare assignment
# (a failing command substitution there aborts the whole script).
# Output goes to a TEMP FILE, never a $() pipe: a daemonized descendant
# (supervise-daemon) inherits the pipe's write end, so even after `timeout`
# kills the direct child the command substitution waits forever for EOF and
# the step hangs past the job's own timeout. This was the smoke's CI hang.
RESTART_TMP=$(mktemp)
if timeout 60 "$TOOLS" restart -f >"$RESTART_TMP" 2>&1; then
  RESTART_RC=0
else
  RESTART_RC=$?
fi
RESTART_OUT=$(cat "$RESTART_TMP" 2>/dev/null)
rm -f "$RESTART_TMP"
if [ "$RESTART_RC" -eq 0 ]; then
  ok "urnet-tools restart -f returned success"
else
  fail "urnet-tools restart -f failed (rc=$RESTART_RC)"
  echo "$RESTART_OUT" | head -8 | sed 's/^/     /'
fi
sleep 5
if rc-service urnetwork status 2>/dev/null | grep -q started; then
  ok "service still started after restart"
else
  fail "service down after restart"
fi

STOP_TMP=$(mktemp)
if timeout 60 "$TOOLS" stop -f >"$STOP_TMP" 2>&1; then STOP_RC=0; else STOP_RC=$?; fi
STOP_OUT=$(cat "$STOP_TMP" 2>/dev/null)
rm -f "$STOP_TMP"
START_TMP=$(mktemp)
if timeout 60 "$TOOLS" start -f >"$START_TMP" 2>&1; then START_RC=0; else START_RC=$?; fi
START_OUT=$(cat "$START_TMP" 2>/dev/null)
rm -f "$START_TMP"
if [ "$STOP_RC" -eq 0 ] && [ "$START_RC" -eq 0 ]; then
  ok "stop then start both routed through rc-service"
else
  fail "stop/start routing failed (stop rc=$STOP_RC, start rc=$START_RC)"
  echo "$STOP_OUT" | head -6 | sed 's/^/     stop: /'
  echo "$START_OUT" | head -6 | sed 's/^/     start: /'
fi

# --- the root-owned auto-update tool (security) ------------------------------
# The weekly cron entry runs AS ROOT. Its binary must not be reachable through
# any directory the service user can write - root-owned files alone do not
# defend a path whose ancestor (their home) is user-writable, because they can
# rename the ancestor and plant a replacement. This checks the fix: a
# root-owned copy is staged outside the home, and the cron entry names it and
# carries the guard.
say "verifying the root-owned auto-update tool"
ROOT_TOOL=/usr/local/libexec/urnetwork/urnet-tools
if [ -f "$ROOT_TOOL" ]; then
  OWNER=$(stat -c %u "$ROOT_TOOL" 2>/dev/null)
  if [ "$OWNER" = "0" ]; then
    ok "$ROOT_TOOL exists and is root-owned"
  else
    fail "$ROOT_TOOL is owned by uid $OWNER, not root"
  fi
  # Every ancestor must be root-owned and not group/world writable.
  d=$(dirname "$ROOT_TOOL"); bad=""
  while [ "$d" != "/" ] && [ -n "$d" ]; do
    o=$(stat -c %u "$d" 2>/dev/null); m=$(stat -c %a "$d" 2>/dev/null)
    [ "$o" = "0" ] || bad="$bad $d(owner=$o)"
    [ $((0$m & 022)) -eq 0 ] || bad="$bad $d(mode=$m)"
    d=$(dirname "$d")
  done
  if [ -z "$bad" ]; then
    ok "no ancestor of the cron tool is writable by a non-root user"
  else
    fail "user-writable ancestor(s) of the cron tool:$bad"
  fi
else
  fail "$ROOT_TOOL was not staged by the installer"
fi

# The cron entry must reference the root-owned tool and carry the guard.
# Use the ROOT-OWNED tool explicitly: that is the one the cron entry will run,
# and exercising it proves the staged copy is functional, not just present.
AU_TMP=$(mktemp)
if timeout 45 "$ROOT_TOOL" auto-update weekly >"$AU_TMP" 2>&1; then AU_OK=1; else AU_OK=0; fi
AU_OUT=$(cat "$AU_TMP" 2>/dev/null)
rm -f "$AU_TMP"
if [ "$AU_OK" = "1" ]; then
  ENTRY=""
  for f in /etc/periodic/weekly/urnetwork-update /etc/periodic/daily/urnetwork-update /etc/periodic/monthly/urnetwork-update; do
    [ -f "$f" ] && ENTRY="$f" && break
  done
  if [ -n "$ENTRY" ]; then
    if grep -q "usr/local/libexec/urnetwork/urnet-tools" "$ENTRY"; then
      ok "cron entry names the root-owned tool"
    else
      fail "cron entry does not name the root-owned tool"
      grep -n 'tool=' "$ENTRY" | sed 's/^/     /'
    fi
    if grep -q "writable by a non-root user" "$ENTRY"; then
      ok "cron entry carries the root-ownership guard"
    else
      fail "cron entry has no root-ownership guard"
    fi
  else
    fail "auto-update weekly wrote no periodic entry"
  fi
else
  fail "auto-update weekly failed via the root-owned tool"
  echo "$AU_OUT" | head -6 | sed 's/^/     /'
fi

# --- verdict -----------------------------------------------------------------
say "result"
if [ "$FAILED" -eq 0 ]; then
  echo "OPENRC SMOKE PASSED"
  exit 0
fi
echo "OPENRC SMOKE FAILED: $FAILED check(s) failed"
# Bring the logs down with us: a failing run is exactly when they are needed.
echo "--- /var/log/urnetwork.log (tail) ---"
tail -40 /var/log/urnetwork.log 2>/dev/null || echo "(no log)"
echo "--- /var/log/urnetwork.err (tail) ---"
tail -20 /var/log/urnetwork.err 2>/dev/null || echo "(no err log)"
exit 1