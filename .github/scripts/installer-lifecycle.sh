#!/bin/sh
# installer-lifecycle.sh — drive the REAL Linux installer in a distro container
# against a local fake release, then assert the install lifecycle.
#
# Why this exists: the distro matrix used to run only the pre-built binaries.
# The shipped installer (scripts/Provider_Install_Linux.sh) is what users
# actually execute, and its distro branches — package manager, dependency
# autoinstall, download client, service manager, install tree, uninstall — were
# never exercised on any distro. This harness runs that script for real.
#
# Contract (POSIX sh, no bashisms; runs inside the distro's own userland):
#   BIN_DIR      dir with provider, urnet-tools (and release-server) [required]
#   INSTALLER    path to Provider_Install_Linux.sh                [required]
#   OUT_DIR      evidence dir                                    [default /tmp/installer-lifecycle]
#   TAG          release tag to install                          [default v2026.10.20-1076956260-sn]
#   TAG2         newer tag for the upgrade pass                  [default v2026.10.21-1076956261-sn]
#   PORT, PORT2  fake release ports                              [default 18080, 18081]
#   TIMEOUT_S    generic timeout                                 [default 180]
#   INSTALL_USER non-root user to install as (needs sudo)        [default unset]
#   VARIANT      label for the summary (e.g. "debian-resolved")
#
# Exit 0 only when every mandatory check passed. CONDITIONAL lines record
# environment limits (no service manager in this cell, no HTTP client, ...).

set -u

BIN_DIR=${BIN_DIR:?BIN_DIR is required}
# Optional second build stamped with TAG2; without it the tag-switch pass can
# only assert the recorded version and the binary changing on disk.
BIN_DIR2=${BIN_DIR2:-}
INSTALLER=${INSTALLER:?INSTALLER is required}
OUT_DIR=${OUT_DIR:-/tmp/installer-lifecycle}
TAG=${TAG:-v2026.10.20-1076956260-sn}
TAG2=${TAG2:-v2026.10.21-1076956261-sn}
PORT=${PORT:-18080}
PORT2=${PORT2:-18081}
TIMEOUT_S=${TIMEOUT_S:-180}
INSTALL_USER=${INSTALL_USER:-}
VARIANT=${VARIANT:-plain}
OWNER=${OWNER:-full-bars}
REPO_SLUG=${REPO_SLUG:-sn}

PASSES=0
FAILS=0
CONDS=0

# Order randomization. A harness that always runs its checks in the same
# sequence can hide an order-dependent defect (a leftover process, a cached
# state dir, a service that only misbehaves when touched twice). The seed is
# printed and recorded, so any failing order is reproducible:
#   SHUFFLE_SEED=<seed> sh installer-lifecycle.sh
# Only order-independent steps are shuffled; install is always first and
# uninstall always last.
SEED=${SHUFFLE_SEED:-$(date +%s)$$}
printf 'order seed: %s\n' "$SEED"

# shuffle: stdin lines -> same lines in a seeded random order (POSIX awk; no
# shuf, which busybox does not reliably provide).
shuffle() {
    awk -v seed="$SEED" 'BEGIN { srand(seed) } { printf "%.6f\t%s\n", rand(), $0 }' \
        | sort -n | cut -f 2-
}

mkdir -p "$OUT_DIR"
LOG="$OUT_DIR/installer.log"
: >"$LOG"

say()  { printf '== %s\n' "$*"; }
pass() { PASSES=$((PASSES + 1)); printf '  PASS: %s\n' "$*"; }
fail() { FAILS=$((FAILS + 1)); printf '  FAIL: %s\n' "$*"; }
cond() { CONDS=$((CONDS + 1)); printf '  CONDITIONAL: %s\n' "$*"; }

# run_with_timeout <seconds> <cmd...>: portable bounded run (no coreutils-only
# `timeout` on every image; busybox has it, Debian's coreutils has it, but the
# fallback keeps the harness honest if neither does).
run_with_timeout() {
    _t=$1; shift
    if command -v timeout >/dev/null 2>&1; then
        timeout "$_t" "$@"
        return $?
    fi
    "$@" &
    _p=$!
    ( sleep "$_t"; kill -TERM "$_p" 2>/dev/null; sleep 2; kill -KILL "$_p" 2>/dev/null ) &
    _w=$!
    wait "$_p"; _rc=$?
    kill "$_w" 2>/dev/null
    return $_rc
}

# http_get <url>: use whichever client the image ships, and report which one.
# The install path is deliberately allowed to use a different client than this
# probe (that is one of the things under test), so this only gates readiness.
http_get() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsS --max-time 5 "$1" 2>/dev/null
    elif command -v wget >/dev/null 2>&1; then
        wget -q -T 5 -O - "$1" 2>/dev/null
    else
        return 127
    fi
}

wait_ready() {
    _url=$1
    _i=0
    while [ "$_i" -lt 40 ]; do
        if http_get "$_url" >/dev/null 2>&1; then return 0; fi
        # No HTTP client in the image: fall back to a fixed wait.
        if [ "$_i" -ge 20 ] && ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
            sleep 1; return 0
        fi
        sleep 0.5; _i=$((_i + 1))
    done
    return 1
}

# ------------------------------------------------------------------ identity --
say "distro identity"
{
    printf 'distro: '; ( . /etc/os-release 2>/dev/null && printf '%s %s' "${NAME:-unknown}" "${VERSION_ID:-}" ) || echo unknown
    printf 'kernel: '; uname -r
    printf 'arch:   '; uname -m
    printf 'libc:   '; (ldd --version 2>&1 | head -n 1) || echo unknown
    printf 'init:   '
    if [ -d /run/openrc ] || [ -x /sbin/openrc-run ] || [ -x /usr/sbin/openrc-run ]; then echo openrc
    elif command -v systemctl >/dev/null 2>&1; then echo systemd
    else echo none; fi
    printf 'pkg:    '
    for pm in apt-get dnf zypper pacman apk yum microdnf; do
        command -v "$pm" >/dev/null 2>&1 && { echo "$pm"; break; }
    done
    printf 'tools:  '
    for t in curl wget jq python3 tar sha256sum nc; do
        command -v "$t" >/dev/null 2>&1 && printf '%s ' "$t"
    done; echo
} | tee "$OUT_DIR/distro.txt"

say "inputs"
[ -x "$BIN_DIR/provider" ] && pass "provider binary present" || fail "provider binary missing at $BIN_DIR/provider"
[ -x "$BIN_DIR/urnet-tools" ] && pass "urnet-tools present" || fail "urnet-tools missing at $BIN_DIR/urnet-tools"
[ -x "$BIN_DIR/release-server" ] && pass "release-server present" || fail "release-server missing at $BIN_DIR/release-server"
[ -f "$INSTALLER" ] && pass "installer present: $INSTALLER" || { fail "installer missing at $INSTALLER"; }

if [ "$FAILS" -gt 0 ]; then
    echo; echo "==================== installer-lifecycle summary ===================="
    printf 'variant: %s\nchecks: %s passed, %s failed, %s conditional\n' "$VARIANT" "$PASSES" "$FAILS" "$CONDS"
    printf 'RESULT: FAIL\n' | tee "$OUT_DIR/summary.txt"
    exit 1
fi

# --------------------------------------------------- dependency-refusal path --
# The installer now installs a missing download client / JSON parser itself.
# With URNET_NO_AUTOINSTALL=1 it must still refuse cleanly and name what is
# missing, with no partial install tree. That opt-out path is asserted here;
# everything else runs the full lifecycle and lets the installer do the work.
_dep_dl=""
command -v curl >/dev/null 2>&1 && _dep_dl=curl
[ -z "$_dep_dl" ] && command -v wget >/dev/null 2>&1 && _dep_dl=wget
_dep_json=""
command -v jq >/dev/null 2>&1 && _dep_json=jq
[ -z "$_dep_json" ] && command -v python3 >/dev/null 2>&1 && _dep_json=python3

if [ "${URNET_NO_AUTOINSTALL:-0}" = "1" ] && { [ -z "$_dep_dl" ] || [ -z "$_dep_json" ]; }; then
    say "dependency-refusal path (autoinstall disabled; download client: ${_dep_dl:-none}, json parser: ${_dep_json:-none})"
    URNET_NO_AUTOINSTALL=1 run_with_timeout "$TIMEOUT_S" sh "$INSTALLER" -f install >"$OUT_DIR/refusal.log" 2>&1
    _rc=$?
    if [ "$_rc" -ne 0 ]; then
        pass "installer refused with autoinstall disabled (exit $_rc)"
    else
        fail "installer exited 0 with no download client or JSON parser"
    fi
    if grep -qE "Neither 'curl' nor 'wget'|Neither 'jq' nor 'python3'" "$OUT_DIR/refusal.log"; then
        pass "refusal names the missing dependency"
    else
        fail "refusal message does not name the missing dependency: $(head -c 200 "$OUT_DIR/refusal.log")"
    fi
    if [ -d "$HOME/.local/share/urnetwork-provider" ]; then
        fail "a partial install tree exists after the refusal"
    else
        pass "no partial install tree after the refusal"
    fi
    echo
    echo "==================== installer-lifecycle summary ===================="
    printf 'variant: %s (dependency-refusal)\n' "$VARIANT"
    printf 'seed:    %s\n' "$SEED"
    printf 'checks:  %s passed, %s failed, %s conditional\n' "$PASSES" "$FAILS" "$CONDS"
    if [ "$FAILS" -gt 0 ]; then printf 'RESULT: FAIL\n' | tee "$OUT_DIR/summary.txt"; exit 1; fi
    printf 'RESULT: PASS\nseed: %s\n' "$SEED" | tee "$OUT_DIR/summary.txt"
    exit 0
fi

if [ -z "$_dep_dl" ] || [ -z "$_dep_json" ]; then
    cond "image ships no ${_dep_dl:+download client}${_dep_dl:-} ${_dep_json:+json parser}${_dep_json:-}; the installer must autoinstall them"
else
    cond "dependencies present: $_dep_dl + $_dep_json"
fi

# ------------------------------------------------------------- release assets --
say "stage the fake release"
ARCH=$(uname -m)
case "$ARCH" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    armv7l|armv6l) ARCH=arm ;;
    i686|i386) ARCH=386 ;;
esac
WORK=$(mktemp -d 2>/dev/null || echo "/tmp/installer-lifecycle.$$")
mkdir -p "$WORK"
ASSETS="$WORK/assets-$TAG"
mkdir -p "$ASSETS/linux/$ARCH"
cp "$BIN_DIR/provider" "$ASSETS/linux/$ARCH/provider"
cp "$BIN_DIR/urnet-tools" "$ASSETS/urnet-tools"
tar -czf "$ASSETS/urnetwork-provider-$TAG.tar.gz" -C "$ASSETS" linux urnet-tools
cp "$ASSETS/urnetwork-provider-$TAG.tar.gz" "$ASSETS/urnetwork-provider-$TAG-linux-$ARCH.tar.gz"
cp "$BIN_DIR/urnet-tools" "$ASSETS/urnet-tools-linux-$ARCH"
if [ -f "$ASSETS/urnetwork-provider-$TAG.tar.gz" ]; then
    pass "staged urnetwork-provider-$TAG.tar.gz ($(wc -c <"$ASSETS/urnetwork-provider-$TAG.tar.gz") bytes)"
else
    fail "tar staging failed"
fi

# ------------------------------------------------------------------ servers --
say "fake release server"
"$BIN_DIR/release-server" -dir "$ASSETS" -addr "127.0.0.1:$PORT" -tag "$TAG" -owner "$OWNER" -repo "$REPO_SLUG" \
    >"$OUT_DIR/release-server.log" 2>&1 &
SRV1=$!
if wait_ready "http://127.0.0.1:$PORT/healthz"; then
    pass "release server answering on 127.0.0.1:$PORT (tag $TAG)"
else
    fail "release server did not answer on 127.0.0.1:$PORT"
fi
# Prove the JSON carries a real digest, which is what makes the installer take
# its digest-verified path instead of the legacy shell-tool fallback.
_json=$(http_get "http://127.0.0.1:$PORT/repos/$OWNER/$REPO_SLUG/releases/latest" || true)
case "$_json" in
    *'"digest":"sha256:'*) pass "release JSON advertises a sha256 digest" ;;
    '') cond "no HTTP client to inspect the release JSON" ;;
    *) fail "release JSON carries no sha256 digest: $(printf '%s' "$_json" | head -c 160)" ;;
esac

# --------------------------------------------------------------- install pass --
say "install (tag $TAG)"
if [ -n "$INSTALL_USER" ] && [ "$(id -u)" = "0" ]; then
    _home=$(getent passwd "$INSTALL_USER" 2>/dev/null | cut -d: -f6)
    cond "installing as non-root user $INSTALL_USER (home ${_home:-unknown})"
fi

_install_env="URNET_ALLOW_BASE_OVERRIDE=1 URNET_API_BASE=http://127.0.0.1:$PORT/repos/$OWNER/$REPO_SLUG URNET_MIRROR_BASE=http://127.0.0.1:$PORT/releases/download URNET_DL_BASE=http://127.0.0.1:$PORT"
if [ -n "${_home:-}" ]; then
    # shellcheck disable=SC2086
    run_with_timeout "$TIMEOUT_S" su -s /bin/sh "$INSTALL_USER" -c "env $_install_env sh $INSTALLER install -f" >>"$LOG" 2>&1
    _rc=$?
else
    # shellcheck disable=SC2086
    run_with_timeout "$TIMEOUT_S" env $_install_env sh "$INSTALLER" -f install >>"$LOG" 2>&1
    _rc=$?
fi
if [ "$_rc" = "0" ]; then
    pass "installer exited 0"
else
    fail "installer exited $_rc (see $LOG)"
    tail -n 25 "$LOG" | sed 's/^/    | /'
fi

# Locate the install tree: the user path, or the root-owned OpenRC path.
CANDIDATES=""
if [ -n "${_home:-}" ]; then CANDIDATES="$_home/.local/share/urnetwork-provider"; fi
CANDIDATES="$CANDIDATES $HOME/.local/share/urnetwork-provider /usr/local/lib/urnetwork-provider /root/.local/share/urnetwork-provider"
INSTALL_ROOT=""
for c in $CANDIDATES; do
    if [ -d "$c" ] && [ -x "$c/bin/urnetwork" ]; then INSTALL_ROOT="$c"; break; fi
done
if [ -n "$INSTALL_ROOT" ]; then
    pass "install tree at $INSTALL_ROOT"
else
    fail "no install tree with an executable bin/urnetwork found (looked at: $CANDIDATES)"
fi

if [ -n "$INSTALL_ROOT" ]; then
    _ver=$(cat "$INSTALL_ROOT/.version" 2>/dev/null || echo "")
    if [ "$_ver" = "$TAG" ]; then
        pass ".version records $TAG"
    else
        fail ".version is '$_ver', want '$TAG'"
    fi
    _out=$("$INSTALL_ROOT/bin/urnetwork" --version 2>&1 | head -n 2 || true)
    case "$_out" in
        *"$TAG"*) pass "installed provider reports $TAG" ;;
        *) fail "installed provider --version does not mention $TAG: $(printf '%s' "$_out" | head -c 160)" ;;
    esac
    _tver=$("$INSTALL_ROOT/bin/urnet-tools" version 2>&1 | head -n 1 || true)
    case "$_tver" in
        *"$TAG"*) pass "installed urnet-tools reports $TAG" ;;
        "") cond "no urnet-tools copy in the install tree" ;;
        *) fail "installed urnet-tools reports '$(printf %s "$_tver" | head -c 120)'" ;;
    esac
fi

# ----------------------------------------------------------------- service --
say "service registration"
if [ -d /run/openrc ] || [ -x /sbin/openrc-run ] || [ -x /usr/sbin/openrc-run ]; then
    if [ -f /etc/init.d/urnetwork ]; then
        pass "OpenRC init script written (/etc/init.d/urnetwork)"
        if grep -q 'rc_ulimit' /etc/init.d/urnetwork; then pass "init script declares rc_ulimit"; else cond "init script has no rc_ulimit"; fi
        if [ -L /etc/runlevels/default/urnetwork ]; then pass "registered in the default runlevel"; else cond "not in /etc/runlevels/default (auto-start off?)"; fi
    else
        cond "OpenRC host without /etc/init.d/urnetwork (installer ran in no-service mode)"
    fi
elif command -v systemctl >/dev/null 2>&1; then
    if [ -f "$HOME/.config/systemd/user/urnetwork.service" ]; then
        pass "systemd user unit written"
        if grep -q '^ExecStart=' "$HOME/.config/systemd/user/urnetwork.service"; then pass "unit carries ExecStart"; else fail "unit has no ExecStart"; fi
    else
        cond "systemd present but no user unit written (no user manager in this cell)"
    fi
else
    cond "no service manager in this image (binary/install/uninstall only)"
fi

# ---------------------------------------------------- middle passes (order) --
# The reinstall and the tag-switch passes are order-independent by design: each
# records what it expects the installed version to be, and the final assertion
# compares against whichever ran last. Their order is shuffled from the seed so
# a state leak between them cannot hide behind a fixed sequence.
LAST_TAG=""

pass_idempotent() {
    say "reinstall (same tag $TAG, idempotent)"
    # shellcheck disable=SC2086
    run_with_timeout "$TIMEOUT_S" env $_install_env sh "$INSTALLER" -f install >>"$LOG" 2>&1
    _rc=$?
    if [ "$_rc" = "0" ]; then pass "reinstall exited 0"; else fail "reinstall exited $_rc"; fi
    if [ -n "$INSTALL_ROOT" ]; then
        _ver=$(cat "$INSTALL_ROOT/.version" 2>/dev/null || echo "")
        [ "$_ver" = "$TAG" ] && pass "version is $TAG after the reinstall" || fail "version is '$_ver' after the reinstall, want $TAG"
        _bins=$(find "$INSTALL_ROOT/bin" -maxdepth 1 -name 'urnetwork*' 2>/dev/null | wc -l)
        if [ "$_bins" -le 2 ]; then pass "no binary pile-up ($_bins urnetwork* entries)"; else fail "$_bins urnetwork* binaries left behind"; fi
    fi
    LAST_TAG=$TAG
}

pass_tag_switch() {
    say "tag switch (tag $TAG2)"
    _ver_digest=""
    [ -n "$INSTALL_ROOT" ] && _ver_digest=$(sha256sum "$INSTALL_ROOT/bin/urnetwork" 2>/dev/null | cut -d' ' -f1 || echo "")
    _upg_env="URNET_ALLOW_BASE_OVERRIDE=1 URNET_API_BASE=http://127.0.0.1:$PORT2/repos/$OWNER/$REPO_SLUG URNET_MIRROR_BASE=http://127.0.0.1:$PORT2/releases/download URNET_DL_BASE=http://127.0.0.1:$PORT2"
    # shellcheck disable=SC2086
    run_with_timeout "$TIMEOUT_S" env $_upg_env sh "$INSTALLER" -f install >>"$LOG" 2>&1
    _rc=$?
    if [ "$_rc" = "0" ]; then pass "tag-switch install exited 0"; else fail "tag-switch install exited $_rc"; fi
    if [ -n "$INSTALL_ROOT" ]; then
        _ver=$(cat "$INSTALL_ROOT/.version" 2>/dev/null || echo "")
        [ "$_ver" = "$TAG2" ] && pass ".version advanced to $TAG2" || fail ".version is '$_ver', want '$TAG2'"
        if [ -n "$BIN_DIR2" ] && [ -x "$BIN_DIR2/provider" ]; then
            _out=$("$INSTALL_ROOT/bin/urnetwork" --version 2>&1 | head -n 2 || true)
            case "$_out" in
                *"$TAG2"*) pass "installed provider reports $TAG2" ;;
                *) fail "installed provider does not report $TAG2: $(printf '%s' "$_out" | head -c 120)" ;;
            esac
        else
            _now=$(sha256sum "$INSTALL_ROOT/bin/urnetwork" 2>/dev/null | cut -d' ' -f1 || echo "")
            if [ "$_now" != "$_ver_digest" ]; then
                pass "installed binary changed on the tag switch"
            else
                fail "installed binary did not change on the tag switch"
            fi
            cond "no second stamped build supplied; --version reports the original stamp"
        fi
    fi
    LAST_TAG=$TAG2
}

# Stage the second release before the passes so either order can run.
ASSETS2="$WORK/assets-$TAG2"
mkdir -p "$ASSETS2/linux/$ARCH"
if [ -n "$BIN_DIR2" ] && [ -x "$BIN_DIR2/provider" ]; then
    cp "$BIN_DIR2/provider" "$ASSETS2/linux/$ARCH/provider"
    cp "$BIN_DIR2/urnet-tools" "$ASSETS2/urnet-tools"
else
    cp "$BIN_DIR/provider" "$ASSETS2/linux/$ARCH/provider"
    cp "$BIN_DIR/urnet-tools" "$ASSETS2/urnet-tools"
    # A byte-identical binary cannot prove the swap; the trailing newline
    # survives exec and changes the digest the fake release advertises.
    printf '\n' >>"$ASSETS2/linux/$ARCH/provider" 2>/dev/null || true
fi
tar -czf "$ASSETS2/urnetwork-provider-$TAG2.tar.gz" -C "$ASSETS2" linux urnet-tools
cp "$ASSETS2/urnet-tools" "$ASSETS2/urnet-tools-linux-$ARCH"
"$BIN_DIR/release-server" -dir "$ASSETS2" -addr "127.0.0.1:$PORT2" -tag "$TAG2" -owner "$OWNER" -repo "$REPO_SLUG" \
    >"$OUT_DIR/release-server2.log" 2>&1 &
SRV2=$!
if wait_ready "http://127.0.0.1:$PORT2/healthz"; then
    pass "second release server answering (tag $TAG2)"
else
    fail "second release server did not answer on 127.0.0.1:$PORT2"
fi

say "middle-pass order"
printf 'middle passes:\n' | tee -a "$OUT_DIR/distro.txt"
for _pass in $(printf 'idempotent\ntag-switch\n' | shuffle); do
    printf '  %s\n' "$_pass" | tee -a "$OUT_DIR/distro.txt"
    case "$_pass" in
        idempotent) pass_idempotent ;;
        tag-switch) pass_tag_switch ;;
    esac
done
if [ -n "$INSTALL_ROOT" ]; then
    _ver=$(cat "$INSTALL_ROOT/.version" 2>/dev/null || echo "")
    [ "$_ver" = "$LAST_TAG" ] && pass "final version matches the last pass ($LAST_TAG)" || fail "final version '$_ver' does not match the last pass ($LAST_TAG)"
fi

# ---------------------------------------------------------------- uninstall --
say "uninstall"
run_with_timeout "$TIMEOUT_S" sh "$INSTALLER" -f uninstall >>"$LOG" 2>&1
_rc=$?
if [ "$_rc" = "0" ]; then pass "uninstall exited 0"; else fail "uninstall exited $_rc"; fi
if [ -n "$INSTALL_ROOT" ] && [ -d "$INSTALL_ROOT" ]; then
    fail "install tree still present after uninstall: $INSTALL_ROOT"
else
    pass "install tree removed"
fi
if [ -f /etc/init.d/urnetwork ]; then fail "OpenRC init script left behind"; else pass "no OpenRC init script left"; fi
if [ -f "$HOME/.config/systemd/user/urnetwork.service" ]; then fail "systemd user unit left behind"; else pass "no systemd unit left"; fi
if grep -q '# == urnetwork-provider start' "$HOME/.bashrc" 2>/dev/null; then
    fail "shell rc block left behind in ~/.bashrc"
else
    pass "no shell rc block left behind"
fi

# ------------------------------------------------------------------ cleanup --
kill "$SRV1" "$SRV2" 2>/dev/null
pkill -f "$INSTALL_ROOT/bin/urnetwork" 2>/dev/null || true
rm -rf "$WORK"

echo
echo "==================== installer-lifecycle summary ===================="
( . /etc/os-release 2>/dev/null && printf 'distro:  %s %s\n' "${NAME:-unknown}" "${VERSION_ID:-}" ) || echo "distro: unknown"
printf 'variant: %s\n' "$VARIANT"
printf 'seed:    %s\n' "$SEED"
printf 'checks:  %s passed, %s failed, %s conditional\n' "$PASSES" "$FAILS" "$CONDS"
printf 'reproduce: SHUFFLE_SEED=%s\n' "$SEED"
if [ "$FAILS" -gt 0 ]; then
    printf 'RESULT: FAIL\n' | tee "$OUT_DIR/summary.txt"
    exit 1
fi
printf 'RESULT: PASS\nseed: %s\n' "$SEED" | tee "$OUT_DIR/summary.txt"
printf '(%s passed, %s conditional)\n' "$PASSES" "$CONDS" | tee -a "$OUT_DIR/summary.txt"
exit 0
