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
# Every script that still carries an inline copy of these has to stay in step
# with provider_common.sh; the harnesses below and the drift guard at the end
# all work from the same set.
stop_helpers="func_forward_stop func_install_stop_trap func_exit_if_stopping func_sleep func_run_provider"

# the harness: the real helpers around a supervision loop shaped like the
# provider loops, so a stop during the run or between runs both end it.
# APP_DIR is a private temp path on purpose: func_exit_if_stopping polls
# `pgrep -f "^$APP_DIR/urnetwork_... provide"`, and a host-wide /app would let
# any leftover container answer that poll and stretch the stop past the window
# asserted below.
mkdir -p "$tmp/app"
{
    echo '#!/bin/sh'
    echo 'set -eu'
    echo "APP_DIR=\"$tmp/app\""
    echo 'PROVIDER_CHILD_PID=""'
    echo 'PROVIDER_STOPPING=0'
    for fn in log $stop_helpers; do
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
cat >"$tmp/app/urnetwork_stub" <<'EOF'
#!/bin/sh
trap 'echo TERM >"$OUT/stub.signal"; sleep 1; exit 0' TERM
echo $$ >"$OUT/stub.pid"
while :; do sleep 1; done
EOF
chmod +x "$tmp/harness.sh" "$tmp/app/urnetwork_stub"

# the docker scenario runs in its own PID namespace, so /app there cannot be
# answered by a leftover host container and keeps the production path shape
{
    echo '#!/bin/sh'
    echo 'set -eu'
    echo 'APP_DIR=/app'
    echo 'PROVIDER_CHILD_PID=""'
    echo 'PROVIDER_STOPPING=0'
    for fn in log $stop_helpers; do
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
} >"$tmp/harness-container.sh"

wait_file() {
    local f="$1" i=0
    while [ ! -s "$f" ] && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i+1)); done
    [ -s "$f" ]
}

# 1. Local: SIGTERM to the supervisor reaches the provider and ends the loop.
out="$tmp/local"; mkdir -p "$out"
OUT="$out" STUB="$tmp/app/urnetwork_stub" sh "$tmp/harness.sh" >"$out/stdout" 2>&1 &
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
# the crash must be logged with its real exit code.
out="$tmp/crash"; mkdir -p "$out"
cat >"$tmp/app/urnetwork_crash" <<'EOF'
#!/bin/sh
echo $$ >"$OUT/stub.pid"
if [ -f "$OUT/crashed" ]; then
    while :; do sleep 1; done
fi
touch "$OUT/crashed"
exit 5
EOF
chmod +x "$tmp/app/urnetwork_crash"
OUT="$out" STUB="$tmp/app/urnetwork_crash" sh "$tmp/harness.sh" >"$out/stdout" 2>&1 &
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
    if docker run -d --name "$name" --user "$(id -u)" -v "$tmp:/t" -e OUT=/t/docker -e STUB=/t/app/urnetwork_stub \
        alpine:latest sh -c 'mkdir -p /t/docker && exec sh /t/harness-container.sh' >/dev/null 2>&1 &&
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

# 4. helper-copy drift guard: provider_common.sh is the shared source, so every
# script that still carries an inline copy of a helper must match it byte for
# byte, or a helper fix lands in one copy and the container that runs the other
# keeps the old behaviour. Only copies that exist are diffed: the wrappers that
# source the shared file (start_stable, start_jwt, start_nightly) have no
# definition of their own and must not be compared against one.
missing=""
for fn in $stop_helpers; do
    [ -n "$(extract_func "$common" "$fn")" ] || missing="$missing $fn"
done
if [ -z "$missing" ]; then
    ok "provider_common.sh defines every stop helper"
else
    bad "provider_common.sh is missing stop helpers:$missing"
fi

for other in pelican_panel start_stable start_jwt start_nightly; do
    [ -f "$REPO/docker/scripts/$other.sh" ] || continue
    for fn in $stop_helpers; do
        copy="$(extract_func "$REPO/docker/scripts/$other.sh" "$fn")"
        # empty: this helper comes from provider_common.sh, nothing to diff
        [ -n "$copy" ] || continue
        if [ "$copy" = "$(extract_func "$common" "$fn")" ]; then
            ok "$other helper copy matches provider_common: $fn"
        else
            bad "$other helper copy drifted from provider_common: $fn"
        fi
    done
done

# 5. stop-exit routing guard: the provider loop must end a stop request through
# func_exit_if_stopping, whose bounded poll waits for a hotswap successor. A
# bare exit 0 there lets PID 1 exit and Docker SIGKILL the successor midway
# through its graceful shutdown; the successor only exists during a hotswap, so
# the scenarios above cannot reach that branch on their own.
for f in provider_common.sh pelican_panel.sh; do
    body="$(extract_func "$REPO/docker/scripts/$f" func_run_provider)"
    if printf '%s\n' "$body" | grep -q 'func_exit_if_stopping' \
        && ! printf '%s\n' "$body" | grep -qE '^[[:space:]]*exit 0'; then
        ok "$f routes the stop exit through func_exit_if_stopping"
    else
        bad "$f exits the provider loop without waiting for a successor"
    fi
done

echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
