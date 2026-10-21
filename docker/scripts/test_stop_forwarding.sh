#!/bin/bash
# test_stop_forwarding.sh: verifies that the Docker supervisor shell forwards
# SIGTERM to the provider and exits once it has stopped, instead of ignoring
# `docker stop` until Docker SIGKILLs the container (the shakedown's X3 check).
# The real helpers are extracted from provider_common.sh; the provider is a
# stub that records the signal it received.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="${REPO_DIR:-$(cd "$HERE/../.." && pwd)}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

ok() { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1"; }

extract_func() {
    awk -v fn="$2" '
        $0 ~ "^"fn"\\(\\) *\\{" { grab=1 }
        grab { print }
        grab && /^}/ { exit }
    ' "$1"
}

common="$REPO/docker/scripts/provider_common.sh"

# the harness: the real helpers around a supervision loop shaped like the
# provider loops, so a stop during the run or between runs both end it
{
    echo '#!/bin/sh'
    echo 'set -eu'
    echo 'APP_DIR=/app'
    echo 'PROVIDER_CHILD_PID=""'
    echo 'PROVIDER_STOPPING=0'
    for fn in log func_forward_stop func_install_stop_trap func_exit_if_stopping func_sleep func_run_provider; do
        extract_func "$common" "$fn"
    done
    cat <<'EOF'
func_install_stop_trap
while :; do
    func_exit_if_stopping
    func_run_provider "$STUB"
    echo "provider exited code=$code; restarting" >>"$OUT/supervisor.log"
done
EOF
} >"$tmp/harness.sh"

# the stub provider: exits on SIGTERM after a short shutdown, like the provider
cat >"$tmp/stub.sh" <<'EOF'
#!/bin/sh
trap 'echo TERM >"$OUT/stub.signal"; sleep 1; exit 0' TERM
echo $$ >"$OUT/stub.pid"
while :; do sleep 1; done
EOF
chmod +x "$tmp/harness.sh" "$tmp/stub.sh"

wait_file() {
    local f="$1" i=0
    while [ ! -s "$f" ] && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i+1)); done
    [ -s "$f" ]
}

# 1. Local: SIGTERM to the supervisor reaches the provider and ends the loop.
out="$tmp/local"; mkdir -p "$out"
OUT="$out" STUB="$tmp/stub.sh" sh "$tmp/harness.sh" >"$out/stdout" 2>&1 &
sup=$!
if wait_file "$out/stub.pid"; then
    kill -TERM "$sup"
    i=0
    while kill -0 "$sup" 2>/dev/null && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i+1)); done
    if kill -0 "$sup" 2>/dev/null; then
        bad "supervisor still running 5s after SIGTERM"
        kill -KILL "$sup" 2>/dev/null
    else
        wait "$sup"; rc=$?
        [ "$rc" -eq 0 ] && ok "supervisor exits 0 after SIGTERM" || bad "supervisor exit code $rc, want 0"
    fi
    [ -s "$out/stub.signal" ] && ok "provider received SIGTERM" || bad "provider never received SIGTERM"
    if kill -0 "$(cat "$out/stub.pid")" 2>/dev/null; then
        bad "provider still running after the supervisor exited"
        kill -KILL "$(cat "$out/stub.pid")" 2>/dev/null
    else
        ok "provider stopped"
    fi
    [ ! -s "$out/supervisor.log" ] && ok "supervisor did not restart the provider after the stop" || bad "supervisor restarted the provider after the stop"
else
    bad "stub provider never started"
    kill -KILL "$sup" 2>/dev/null
fi

# 2. set -e parity: production runs these helpers under `set -e`, so a
# provider crash (wait returns non-zero) must not abort the supervisor, and
# the crash must be logged with its real exit code. Also pins pelican's
# standalone helper copies to stay byte-identical with provider_common's:
# they drift silently otherwise, since pelican is the copy that runs as
# PID 1 under the Pelican egg.
out="$tmp/crash"; mkdir -p "$out"
for fn in func_forward_stop func_install_stop_trap func_exit_if_stopping func_sleep func_run_provider; do
    diff <(extract_func "$common" "$fn") \
         <(extract_func "$REPO/docker/scripts/pelican_panel.sh" "$fn") >/dev/null 2>&1 \
        && ok "pelican helper copy matches provider_common: $fn" \
        || bad "pelican helper copy drifted from provider_common: $fn"
done
cat >"$tmp/crash_stub.sh" <<'EOF'
#!/bin/sh
echo $$ >"$OUT/stub.pid"
if [ -f "$OUT/crashed" ]; then
    while :; do sleep 1; done
fi
touch "$OUT/crashed"
exit 5
EOF
chmod +x "$tmp/crash_stub.sh"
OUT="$out" STUB="$tmp/crash_stub.sh" sh "$tmp/harness.sh" >"$out/stdout" 2>&1 &
sup=$!
if wait_file "$out/stub.pid"; then
    sleep 2
    if kill -0 "$sup" 2>/dev/null; then
        ok "supervisor survives a provider crash under set -e"
    else
        bad "supervisor died on the first provider crash (set -e + failing wait?)"
    fi
    grep -q "provider exited code=5; restarting" "$out/supervisor.log" \
        && ok "crash logged with the real exit code (5)" \
        || bad "crash exit code not logged as 5"
    kill -TERM "$sup" 2>/dev/null && sleep 1
    kill -KILL "$sup" 2>/dev/null
    kill -KILL "$(cat "$out/stub.pid")" 2>/dev/null
else
    bad "crash scenario stub never started"
fi

# 3. Docker: the same shell as PID 1, stopped with `docker stop` (default 10s
# timeout). Without the trap PID 1 ignores SIGTERM and the stop takes the full
# timeout. Skipped when docker is not available.
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    name="urnet-stop-test-$$"
    # --user keeps everything in the mount owned by the invoking user, so the
    # EXIT trap can rm -rf $tmp without hitting root-owned leftovers.
    if docker run -d --name "$name" --user "$(id -u)" -v "$tmp:/t" -e OUT=/t/docker -e STUB=/t/stub.sh \
        alpine:latest sh -c 'mkdir -p /t/docker && exec sh /t/harness.sh' >/dev/null 2>&1 &&
        wait_file "$tmp/docker/stub.pid"; then
        t0=$(date +%s)
        docker stop "$name" >/dev/null 2>&1
        elapsed=$(( $(date +%s) - t0 ))
        [ "$elapsed" -lt 5 ] && ok "docker stop finished in ${elapsed}s (< 5s)" || bad "docker stop took ${elapsed}s (PID 1 ignored SIGTERM?)"
        [ -s "$tmp/docker/stub.signal" ] && ok "provider in the container received SIGTERM" || bad "provider in the container never received SIGTERM"
        rc=$(docker inspect -f '{{.State.ExitCode}}' "$name" 2>/dev/null)
        [ "$rc" = "0" ] && ok "container exit code 0" || bad "container exit code $rc, want 0"
    else
        bad "docker harness container did not start"
    fi
    docker rm -f "$name" >/dev/null 2>&1
else
    echo "SKIP: docker not available"
fi

echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
