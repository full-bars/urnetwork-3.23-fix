#!/bin/sh
# URNetwork Provider Entrypoint - shared implementation
# ----------------------------------------------------
# start_stable.sh, start_nightly.sh and start_jwt.sh are thin wrappers:
# each one sets PROVIDER_MODE (and PROVIDER_FLAVOR, which selects the
# urnetwork_<arch>_<flavor> binary name), sources this file, and calls main.
#
# The bootstrap sequence, proxy setup, architecture detection, identity
# reporting, IP checker and vnStat retirement warning are shared. The
# authentication and supervision paths stay per-mode because their contracts
# differ (USER_AUTH/PASSWORD for stable/nightly, auth code or positional JWT
# for jwt) and must not change.

# Exit immediately if any command fails
set -e

# === Mode Selection ===
# Set by the wrapper before sourcing; the defaults keep a direct source safe.
PROVIDER_MODE="${PROVIDER_MODE:-stable}"
PROVIDER_FLAVOR="${PROVIDER_FLAVOR:-stable}"

# === Configuration Variables ===
export TZ="America/Tijuana"
APP_DIR="/app"
JWT_FILE="$HOME/.urnetwork/jwt"
ENABLE_VNSTAT="${ENABLE_VNSTAT:-}"
ENABLE_IP_CHECKER="${ENABLE_IP_CHECKER:-false}"
IP_CHECKER_URL="https://raw.githubusercontent.com/techroy23/IP-Checker/refs/heads/main/app.sh"

# Sourcing JWT verification helper
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [ -f "$SCRIPT_DIR/jwt_verify.sh" ]; then
  . "$SCRIPT_DIR/jwt_verify.sh"
elif [ -f "/app/jwt_verify.sh" ]; then
  . "/app/jwt_verify.sh"
fi

# Sourcing the v33 operator flag mapping (ALL_OPERATORS, AUTO_REGISTER,
# HOTKEY_SEED_FILE, ALLOW_CLIENT_REGISTRATION, MAX_MEMORY, PROVIDER_ARGS).
if [ -f "$SCRIPT_DIR/provider_env.sh" ]; then
  . "$SCRIPT_DIR/provider_env.sh"
elif [ -f "/app/provider_env.sh" ]; then
  . "/app/provider_env.sh"
fi

# Nightly self-update configuration. Only the nightly wrapper ever runs the
# update path, so only it sources the digest helpers (stable/jwt must not
# depend on update_verify.sh being present).
if [ "$PROVIDER_MODE" = "nightly" ]; then
    # shellcheck source=update_verify.sh
    . "$SCRIPT_DIR/update_verify.sh"
    API_ROOT="https://api.github.com/repos/full-bars/urnetwork-3.23-fix"
    VERSION_FILE="$APP_DIR/version.txt"
    UPDATE_TIME="12:00"
fi

# === Logging Helper ===
log() {
  echo "$(date '+%Y-%m-%d %H:%M:%S') >>> UrNetwork >>> $*"
}

# === Stop Forwarding ===
# `docker stop` sends SIGTERM to PID 1, which is this shell. A shell running as
# PID 1 has no default action for SIGTERM, so without a trap the provider never
# hears it and Docker SIGKILLs the container after its timeout, skipping the
# provider's own shutdown (final metrics counter flush, clean close).
PROVIDER_CHILD_PID=""
PROVIDER_STOPPING=0

func_forward_stop() {
    PROVIDER_STOPPING=1
    if [ -n "$PROVIDER_CHILD_PID" ]; then
        kill -TERM "$PROVIDER_CHILD_PID" 2>/dev/null || true
    fi
    # a hotswap successor is not this shell's child; skip the tracked child
    # (already signalled above) so the provider sees exactly one TERM
    for _successor in $(pgrep -f "^$APP_DIR/urnetwork_[^ ]* (auth-)?provide" 2>/dev/null); do
        [ "$_successor" = "$PROVIDER_CHILD_PID" ] || kill -TERM "$_successor" 2>/dev/null || true
    done
}

func_install_stop_trap() {
    trap func_forward_stop TERM INT
}

func_exit_if_stopping() {
    if [ "$PROVIDER_STOPPING" -eq 1 ]; then
        # a hotswap successor is not our child, so wait cannot reap it; poll
        # (bounded below Docker's 10s stop timeout) so PID 1 does not exit
        # and SIGKILL the successor in the middle of its graceful shutdown
        _stop_i=0
        while [ "$_stop_i" -lt 8 ] \
            && pgrep -f "^$APP_DIR/urnetwork_[^ ]* (auth-)?provide" >/dev/null 2>&1; do
            sleep 1
            _stop_i=$((_stop_i+1))
        done
        log "[INFO] Stop requested; supervisor exiting."
        exit 0
    fi
}

# func_sleep is the interruptible supervisor sleep. busybox ash defers a
# trapped signal until the foreground command returns, so a bare foreground
# `sleep N` swallows a `docker stop` for its whole duration and Docker
# SIGKILLs the container; backgrounding the sleep lets the trap run. The
# wait keeps the elapsed time, and every sleep doubles as a stopping
# checkpoint. Both the entry and the post-spawn flag checks matter: a trap
# consumed at a command boundary between them leaves the flag set with no
# pending signal, and the sleep would otherwise run to its full duration.
func_sleep() {
    func_exit_if_stopping
    sleep "$1" &
    _sleep_pid=$!
    if [ "$PROVIDER_STOPPING" -eq 1 ]; then
        kill "$_sleep_pid" 2>/dev/null || true
    fi
    wait "$_sleep_pid" || true
    func_exit_if_stopping
}

# func_run_provider runs the provider in the background so the stop trap can
# fire while it runs, and sets code to its exit status. `|| code=$?` keeps
# the run alive under set -e for a crashed provider or a wait interrupted by
# the TERM trap (status 143): a bare failing wait would abort the supervisor
# instead of handling the exit. wait repeats until the provider has really
# exited, so a graceful shutdown is never cut short.
func_run_provider() {
    "$@" &
    PROVIDER_CHILD_PID=$!
    # a TERM that landed between the loop's stopping check and this spawn
    # left the stop flag set with nobody to signal; forward now, otherwise
    # wait would outlive the stop request while the fresh provider runs on
    if [ "$PROVIDER_STOPPING" -eq 1 ]; then
        func_forward_stop
    fi
    code=0
    while :; do
        wait "$PROVIDER_CHILD_PID" || code=$?
        kill -0 "$PROVIDER_CHILD_PID" 2>/dev/null || break
    done
    PROVIDER_CHILD_PID=""
    if [ "$PROVIDER_STOPPING" -eq 1 ]; then
        log "[INFO] Stop requested; UrNetwork exited (code=$code)."
        # the drained child is gone, but a hotswap successor may still be
        # shutting down; func_exit_if_stopping bounds the wait so PID 1 does
        # not exit and hand Docker a SIGKILL for that successor
        func_exit_if_stopping
    fi
}

# resolve_provider_env_args fills PROVIDER_ENV_ARGS from provider_env_flags.
# An invalid operator setting is fatal: the mapping logs the offending setting,
# and providing with a half-applied flag set would be worse. With none of the
# variables set the result is empty and the command line stays exactly
# `provide`.
resolve_provider_env_args() {
    if ! PROVIDER_ENV_ARGS="$(provider_env_flags "$HOME/.urnetwork")"; then
        log "[ERROR] Invalid provider environment; fix the setting logged above and restart"
        exit 1
    fi
}

# === Directory Validation ===
func_check_dir() {
    [ -d "$APP_DIR" ] || {
        log "[ERROR] APP_DIR '$APP_DIR' does not exist." >&2
        exit 1
    }
    cd "$APP_DIR" || {
        log "[ERROR] Cannot cd to '$APP_DIR'." >&2
        exit 1
    }
}

# === Credential Validation ===
func_check_credentials() {
    if [ -z "$USER_AUTH" ] || [ -z "$PASSWORD" ]; then
        log "[ERROR] USER_AUTH or PASSWORD not set"
        log "[ERROR] Please provide both -e USER_AUTH and -e PASSWORD"
        exit 1
    else
        log "[INFO] Credentials found"
    fi
}

# === Proxy Setup ===
func_check_proxy() {
    log "[INFO] Checking proxy configuration"
    # ls -la ~/.urnetwork/ 2>/dev/null || log "~/.urnetwork/ not found"
    rm -f ~/.urnetwork/proxy || true
    if [ -f "/app/proxy.txt" ]; then
        log "[INFO] proxy.txt found; adding proxy"
		PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_${PROVIDER_FLAVOR}"
        "$PROVIDER_BIN" proxy add --proxy_file="/app/proxy.txt"
    else
        log "[INFO] No proxy.txt found; skipping proxy"
    fi
}

# === Architecture Detection ===
func_get_architecture() {
    case "$(uname -m)" in
      x86_64)  A_SYS_ARCH=amd64  ;;
      aarch64) A_SYS_ARCH=arm64  ;;
      *)
        log "[ERROR] Unsupported arch $(uname -m)" >&2
        exit 1
        ;;
    esac
}

# === Custom Network Selection ===
# UR_API_URL / UR_CONNECT_URL let operators point the provider at a
# custom API + connect backend without a custom-built binary, using
# the `provider choose_network` command. Both must be set together
# (the binary's choose_network requires both positional args). Saved
# to ~/.urnetwork/network.json, so — like the JWT — it survives
# restarts if that directory is on a mounted volume.
func_choose_network() {
    if [ -n "$UR_API_URL" ] && [ -n "$UR_CONNECT_URL" ]; then
        PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_${PROVIDER_FLAVOR}"
        log "[INFO] Custom network requested: api_url=$UR_API_URL connect_url=$UR_CONNECT_URL"
        if ! "$PROVIDER_BIN" choose_network "$UR_API_URL" "$UR_CONNECT_URL"; then
            log "[ERROR] choose_network rejected UR_API_URL/UR_CONNECT_URL — check scheme (http(s):// for UR_API_URL, ws(s):// for UR_CONNECT_URL)"
            exit 1
        fi
    elif [ -n "$UR_API_URL" ] || [ -n "$UR_CONNECT_URL" ]; then
        log "[ERROR] UR_API_URL and UR_CONNECT_URL must both be set together"
        exit 1
    fi
}

# === Client Identity Reporting ===
# Fetches the public IP used to build this node's dashboard identity label
# (node name @ redacted-IP [version]). Always on; no configuration required.
# Distinct from func_ip_checker below, which is an opt-in diagnostic.
func_report_identity() {
  # Use curl ip.me -4 with a 5s timeout to avoid hanging startup
  URNETWORK_PUBLIC_IP="$(curl -s --max-time 5 --retry 0 ip.me -4 || echo "")"
  export URNETWORK_PUBLIC_IP
  if [ -n "$URNETWORK_PUBLIC_IP" ]; then
    log "[INFO] Public IP detected: $URNETWORK_PUBLIC_IP"
  else
    log "[WARN] Could not detect public IP (timeout or service unreachable)"
  fi
}

# === Public IP Checker (diagnostic) ===
# Opt-in (ENABLE_IP_CHECKER=true). Runs the external techroy23 IP-Checker
# script to log the full public IP to the console. Distinct from the dashboard
# reporter above (func_report_identity), which only sends a redacted IP to the backend.
func_ip_checker() {
  if [ "$ENABLE_IP_CHECKER" = "true" ]; then
    log "[INFO] Checking current public IP..."
    if curl -fsSL "$IP_CHECKER_URL" | sh; then
      log "[INFO] IP checker script ran successfully"
    else
      log "[WARN] Could not fetch or execute IP checker script"
    fi
  else
    log "[INFO] IP checker disabled"
  fi
}

# === vnStat Monitoring (Retired) ===
func_start_vnstat() {
    VNSTAT_LC="$(printf '%s' "${ENABLE_VNSTAT:-}" | tr '[:upper:]' '[:lower:]')"
    if [ "$VNSTAT_LC" = "true" ] || [ "$VNSTAT_LC" = "1" ] || [ "$VNSTAT_LC" = "yes" ]; then
        log "[WARN] vnStat has been retired and removed; use URNETWORK_METRICS and Prometheus for telemetry"
    fi
}

# === Authentication (USER_AUTH/PASSWORD) ===
# stable and nightly differ here on purpose: stable reuses a valid stored JWT,
# nightly always starts from a fresh one. The retry loop itself is shared.
func_do_login_stable() {
    PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_${PROVIDER_FLAVOR}"

    # If a JWT already exists (e.g. persisted via a Docker volume), validate it locally.
    # If expired or invalid, delete it so re-authentication can proceed cleanly.
    if [ -s "$JWT_FILE" ]; then
        if command -v validate_jwt_file >/dev/null 2>&1; then
            if ! validate_jwt_file "$JWT_FILE"; then
                log "[WARN] Stored JWT expired or invalid - removing stale token"
                rm -f "$JWT_FILE"
            fi
        fi
    fi

    if [ -s "$JWT_FILE" ]; then
        log "[INFO] Existing JWT found at $JWT_FILE - skipping authentication"
        return 0
    fi

    log "[INFO] No JWT found, starting authentication flow..."
    func_login_retry_loop
}

func_do_login_nightly() {
    rm -f "$JWT_FILE" || true
    log "[INFO] Removed existing JWT (if any)"
    func_login_retry_loop
}

func_login_retry_loop() {
    PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_${PROVIDER_FLAVOR}"

    # Retry loop for authentication
    while true; do
        func_exit_if_stopping
        log "[INFO] Sleeping 15s before obtaining new JWT..."
        func_sleep 15

        log "[INFO] Attempting authentication..."

        # Capture the auth command output for parsing. The `|| cmd` form keeps
        # the real exit status in AUTH_EXIT_CODE: `|| true` followed by $? gave
        # 0 for every failure, so the failure log said "exit code: 0" and the
        # panic line was the only real diagnostic left.
        AUTH_EXIT_CODE=0
        AUTH_OUTPUT=$("$PROVIDER_BIN" auth --user_auth="$USER_AUTH" --password="$PASSWORD" -f 2>&1) || AUTH_EXIT_CODE=$?
        PANIC_LINE=$(echo "$AUTH_OUTPUT" | grep -E '^panic:' || true)

        if [ "${DEBUG:-false}" = "true" ]; then
            echo "DEBUG: USER_AUTH=$USER_AUTH"
            echo "DEBUG: PASSWORD=$PASSWORD"
            echo "DEBUG: AUTH_EXIT_CODE=$AUTH_EXIT_CODE"
            echo "DEBUG: AUTH_OUTPUT=$AUTH_OUTPUT"
        fi

        # Check for success message in output
        if echo "$AUTH_OUTPUT" | grep -q "Jwt written to"; then
            log "[INFO] Authentication successful - JWT written"
            func_sleep 5

            # Verify JWT file exists as backup check
            if [ -s "$JWT_FILE" ]; then
                log "[INFO] JWT file verified at $JWT_FILE"
                func_sleep 5
                return 0
            else
                log "[WARN] Success message found but JWT file missing - retrying"
                log "[INFO] Will retry authentication in 1 minutes (60 seconds)..."
                func_sleep 60
            fi
        else
            # Authentication failed - output exit code and auth output
            log "[ERROR] Authentication failed (exit code: $AUTH_EXIT_CODE)" >&2
			log "[ERROR] $(echo "$PANIC_LINE" | tr '[:lower:]' '[:upper:]')" >&2
            log "[INFO] Will retry authentication in 5 minutes (300 seconds)..."
            func_sleep 300
        fi
    done
}

# === Provider Update Check (nightly only) ===
# === Release Asset Selection ===
# Releases ship per-arch tarballs (urnetwork-provider-vX-linux-amd64.tar.gz,
# ...-darwin-amd64.tar.gz, ...) PLUS a multi-arch fat tarball
# (urnetwork-provider-vX.tar.gz) that contains linux/<arch>/provider for
# every arch. darwin/windows assets sort first, so a blind head -n1 on
# "any provider tarball" grabs a darwin binary (and GitHub's release JSON
# is a single line, so naive line-grepping grabs the last token of the
# whole body instead of any URL). Uses grep -oE, which works for both
# one-line and pretty-printed JSON.
func_check_update() {
    # Pelican mode: the panel owns updates via image re-pull. A runtime
    # fetch would silently replace the binary baked into the published
    # image with whatever the release API serves, bypassing the operator.
    if [ "${PELICAN:-}" = "yes" ]; then
        log "[INFO] Runtime updates disabled under Pelican (update by re-pulling the image)"
        return 0
    fi
    if [ -f "$VERSION_FILE" ]; then
        CURRENT_VERSION="$(cat "$VERSION_FILE")"
    else
        CURRENT_VERSION=""
    fi
    log "[INFO] Current provider version: ${CURRENT_VERSION:-none}"

    # busybox mktemp (the only mktemp in alpine) requires XXXXXX as the last
    # characters of the template; a suffix fails with Invalid argument. Stage
    # the whole update inside one busybox-safe temp dir so a single rm -rf
    # cleans up every path, success or failure.
    UPDATE_TMP="$(mktemp -d /tmp/urnetwork-update-XXXXXX)" || {
        log "[ERROR] Could not create update staging dir." >&2
        return 0
    }
    RESP_FILE="$UPDATE_TMP/release.json"

    # Version compare BEFORE downloading anything: the updater's only job is
    # to install a NEWER release, so if we are already current there is
    # nothing to fetch and nothing to verify. (An earlier draft fetched the
    # tarball first and skipped on version match afterwards — dead bytes, no
    # verification, silent discard on corruption.) The arch-aware filter keeps
    # darwin/windows assets out of the version probe.
    # Resolve the newest INSTALLABLE stable release once: the selection pages
    # past releases without a provider tarball and never treats a release as
    # stable when GitHub flags it prerelease or its tag is named like one.
    # One fetch serves both the version compare and the asset/digest pick.
    RESP_VERSION_FILE="$UPDATE_TMP/version.json"
    RELEASE_JSON="$(upd_fetch_stable_release_json "$API_ROOT" 2>"$UPDATE_TMP/select.err")" || RELEASE_JSON=""
    if [ -z "$RELEASE_JSON" ]; then
        log "[ERROR] No stable release with a verified provider tarball found." >&2
        if [ -s "$UPDATE_TMP/select.err" ]; then
            while IFS= read -r SELECT_ERR_LINE; do
                log "[ERROR] $SELECT_ERR_LINE" >&2
            done < "$UPDATE_TMP/select.err"
        fi
        log "[INFO] Update skipped; existing provider left untouched."
        rm -rf "$UPDATE_TMP"
        return 0
    fi
    printf '%s\n' "$RELEASE_JSON" > "$RESP_VERSION_FILE"
    HTTP_CODE=200
    VERSION_JSON="$RELEASE_JSON"
    if [ -n "$VERSION_JSON" ]; then
        # The tag identifies the release regardless of which asset shape
        # carries the provider for this arch; deriving it from a linux URL
        # went blind whenever the only shipping asset was the fat tarball,
        # so the 12:00 watcher re-downloaded the archive every day.
        LATEST_VERSION_CHECK="$(printf '%s\n' "$VERSION_JSON" \
          | jq -r '.tag_name // empty' \
          | sed 's/^v//')"
        CURRENT_VERSION=""
        if [ -f "$VERSION_FILE" ]; then
            CURRENT_VERSION="$(cat "$VERSION_FILE")"
        fi
        log "[INFO] Current provider version: ${CURRENT_VERSION:-none}"
        if [ "$LATEST_VERSION_CHECK" = "$CURRENT_VERSION" ] && [ -n "$CURRENT_VERSION" ]; then
            log "[INFO] Already at latest provider version; skipping."
            rm -rf "$UPDATE_TMP"
            return 0
        fi
        log "[INFO] Latest provider version: ${LATEST_VERSION_CHECK:-unknown}"
    fi

    printf '%s\n' "$RELEASE_JSON" > "$RESP_FILE"
    # Pick the asset by NAME from the release JSON itself (jq with a grep
    # fallback for degenerate responses) and take its URL from the same
    # object. The asset name must never be derived from the URL: the digest
    # index is keyed by the API's asset name, so deriving it from
    # basename(URL) would silently couple verification to GitHub's URL
    # layout.
    ASSET_NAME=""
    DOWNLOAD_URL=""
    if command -v jq >/dev/null 2>&1 && [ -n "$RELEASE_JSON" ]; then
        # Preference 1: per-arch tarball for this container (~11MB vs ~84MB).
        # Preference 2: the multi-arch fat tarball (no OS token in the name);
        # the extract step below can still pull linux/${A_SYS_ARCH}/provider
        # from it. The pickers live in update_verify.sh so start_update.sh
        # shares the exact same per-arch OS list; the freebsd-amd64 tarball
        # slipped through the old inline filter and nightly installed it.
        ASSET_NAME="$(upd_pick_arch_tarball_name "$RELEASE_JSON" "$A_SYS_ARCH")"
        [ -n "$ASSET_NAME" ] || ASSET_NAME="$(upd_pick_fat_tarball_name "$RELEASE_JSON")"
        if [ -n "$ASSET_NAME" ]; then
            DOWNLOAD_URL="$(printf '%s\n' "$RELEASE_JSON" \
              | jq -r --arg f "$ASSET_NAME" '.assets[] | select(.name == $f) | .browser_download_url' 2>/dev/null)"
        fi
    fi
    if [ -z "$ASSET_NAME" ] || [ -z "$DOWNLOAD_URL" ]; then
        # Fallback (no jq / unexpected JSON shape): extract candidate URLs
        # with grep -oE — GitHub's release JSON is a single line, so naive
        # line-grepping would grab the last token of the whole body instead
        # of any URL. Preference 1: per-arch tarball for this container
        # (~11MB vs ~84MB). Preference 2: the multi-arch fat tarball (no OS
        # token in the name); the extract step below can still pull
        # linux/${A_SYS_ARCH}/provider from it.
        CAND_URLS="$(printf '%s\n' "$RELEASE_JSON" \
          | grep -oE '"browser_download_url": *"https://[^"]+"' \
          | sed -E 's/.*"([^"]+)"$/\1/' \
          | grep '/urnetwork-provider-[^/]*\.tar\.gz$' || true)"
        DOWNLOAD_URL="$(printf '%s\n' "$CAND_URLS" \
          | grep -E -- "linux-${A_SYS_ARCH}\.tar\.gz$" \
          | head -n1)"
        if [ -z "$DOWNLOAD_URL" ]; then
            DOWNLOAD_URL="$(printf '%s\n' "$CAND_URLS" \
              | grep -Ev -- "-($UPD_PER_ARCH_OS_RES)-" \
              | head -n1)"
        fi
        [ -n "$DOWNLOAD_URL" ] || {
            log "[ERROR] No urnetwork-provider-*.tar.gz asset in GitHub response." >&2
            log "[ERROR] HTTP status: $HTTP_CODE" >&2
            log "[ERROR] Raw response:" >&2
            log "$RELEASE_JSON" | jq . >&2
            rm -rf "$UPDATE_TMP"
            return 0
        }
        for upd_candidate in $(printf '%s\n' "$RELEASE_JSON" | grep -oE '"name": *"[^"]+"' | sed -E 's/"name": *"([^"]+)"/\1/'); do
            case "$DOWNLOAD_URL" in
                *"/$upd_candidate") ASSET_NAME="$upd_candidate"; break ;;
            esac
        done
        [ -n "$ASSET_NAME" ] || ASSET_NAME="${DOWNLOAD_URL##*/}"
    fi
    [ -n "$DOWNLOAD_URL" ] || {
        log "[ERROR] No .tar.gz URL in GitHub response." >&2
        log "[ERROR] HTTP status: $HTTP_CODE" >&2
        rm -rf "$UPDATE_TMP"
        return 0
    }
    log "$DOWNLOAD_URL"

    # Digest verification: refuse to install bytes we cannot verify. The
    # release API publishes a per-asset sha256 digest; a missing digest
    # (legacy asset or tampered response) aborts the update rather than
    # risking an unverified binary replacing the running provider.
    EXPECTED_DIGEST="$(upd_asset_digest_from_json "$RELEASE_JSON" "$ASSET_NAME")" || {
        log "[ERROR] Release API returned no sha256 digest for $ASSET_NAME." >&2
        log "[INFO] Update aborted; existing provider left untouched."
        rm -rf "$UPDATE_TMP"
        return 0
    }
    log "[INFO] Expected digest: $EXPECTED_DIGEST"

    LATEST_VERSION="$(printf '%s\n' "$DOWNLOAD_URL" \
      | sed -E 's#.*/download/v([^/]+)/.*#\1#')"
    log "[INFO] Updating provider to ( $LATEST_VERSION )"

    ARCHIVE="$UPDATE_TMP/urnetwork-provider.tar.gz"
    # -f makes curl fail on any HTTP error, so a bad response cannot be
    # mistaken for a successful download. A connect timeout keeps a stalled
    # connection from hanging the daily watcher forever.
    curl -sfL --connect-timeout 30 "$DOWNLOAD_URL" -o "$ARCHIVE" || {
        log "[ERROR] Download failed for $DOWNLOAD_URL" >&2
        log "[INFO] Update aborted; existing provider left untouched."
        rm -rf "$UPDATE_TMP"
        return 0
    }
    # Verify BEFORE extract: the tarball must match the release API digest
    # exactly, else the update aborts and the running binary stays as-is.
    if ! upd_verify_digest "$ARCHIVE" "$EXPECTED_DIGEST" "release-download"; then
        log "[ERROR] Downloaded tarball failed digest verification." >&2
        log "[INFO] Update aborted; existing provider left untouched."
        rm -rf "$UPDATE_TMP"
        return 0
    fi
    log "[INFO] Digest verified OK"
    # Asset layout differs by kind: per-arch tarballs carry provider/ at the
    # root; the multi-arch fat tarball nests it under linux/<arch>/. Try the
    # nested path first, then the root path.
    PROVIDER_IN_TARBALL="linux/${A_SYS_ARCH}/provider"
    if ! tar -tzf "$ARCHIVE" "$PROVIDER_IN_TARBALL" >/dev/null 2>&1; then
        PROVIDER_IN_TARBALL="provider"
    fi
    tar -xzf "$ARCHIVE" -C "$UPDATE_TMP" "$PROVIDER_IN_TARBALL" || {
        log "[ERROR] Failed to extract provider from tarball." >&2
        log "[INFO] Update aborted; existing provider left untouched."
        rm -rf "$UPDATE_TMP"
        return 0
    }
    [ -f "$UPDATE_TMP/$PROVIDER_IN_TARBALL" ] || {
        log "[ERROR] Provider binary not found in tarball." >&2
        log "[INFO] Update aborted; existing provider left untouched."
        rm -rf "$UPDATE_TMP"
        return 0
    }

    # Only now do the destructive steps. Stage the new binary on the target
    # filesystem, then atomically rename it into place: if /app is a separate
    # mount from /tmp, staging here keeps the swap a rename, so a failed copy
    # cannot truncate the running provider. Every state write is checked so a
    # partial failure cannot orphan a half-installed provider.
    STAGED="$APP_DIR/.urnetwork_${A_SYS_ARCH}_${PROVIDER_FLAVOR}.new"
    if ! cp -f "$UPDATE_TMP/$PROVIDER_IN_TARBALL" "$STAGED"; then
        log "[ERROR] Failed to stage new provider binary." >&2
        log "[INFO] Update aborted; existing provider left untouched."
        rm -rf "$UPDATE_TMP" "$STAGED"
        return 0
    fi
    if ! mv -f "$STAGED" "$APP_DIR/urnetwork_${A_SYS_ARCH}_${PROVIDER_FLAVOR}"; then
        log "[ERROR] Failed to install new provider binary." >&2
        log "[INFO] Update aborted; existing provider left untouched."
        rm -rf "$UPDATE_TMP" "$STAGED"
        return 0
    fi
    if ! echo "$LATEST_VERSION" > "$VERSION_FILE"; then
        log "[ERROR] Failed to record provider version." >&2
        log "[INFO] Update aborted; the previous provider binary is still installed."
        rm -rf "$UPDATE_TMP"
        return 0
    fi
    mkdir -p "$HOME/.urnetwork"
    if ! touch "$HOME/.urnetwork/update-pending"; then
        log "[ERROR] Failed to set restart marker." >&2
        log "[INFO] Update installed; restart marker not set, so the provider was not restarted."
        rm -rf "$UPDATE_TMP"
        return 0
    fi
    rm -rf "$UPDATE_TMP"
    log "[INFO] Update provider complete"

    if pkill -f "^/app/urnetwork_${A_SYS_ARCH}_${PROVIDER_FLAVOR} provide" 2>/dev/null; then
        log "[INFO] Provider process terminated for restart."
    else
        log "[INFO] No running provider process found."
    fi
}

# === Provider Lifecycle Management ===
func_start_provider_stable(){
    failures=0
    while :; do
        func_exit_if_stopping
        log "[INFO] Starting UrNetwork (attempt #$((failures+1)))"
        PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_${PROVIDER_FLAVOR}"
		BIN_VER="$($PROVIDER_BIN --version)"
		log "[INFO] Running UrNetwork build ${BIN_VER}"
        # Capture the real exit code (set -e safe; avoids the `|| true` that pinned code to 0)
        resolve_provider_env_args
        # shellcheck disable=SC2086  # PROVIDER_ENV_ARGS is a deliberately split flag list
        func_run_provider "$PROVIDER_BIN" provide $PROVIDER_ENV_ARGS
        if [ "$code" -eq 0 ]; then
            if [ -f "$HOME/.urnetwork/update-pending" ]; then
                rm -f "$HOME/.urnetwork/update-pending"
                log "[INFO] Binary updated — restarting provider."
                failures=0
                continue
            fi
            if [ -f "$HOME/.urnetwork/hotswap-pending" ] || pgrep -f "urnetwork.*provide" >/dev/null 2>&1; then
                rm -f "$HOME/.urnetwork/hotswap-pending"
                log "[INFO] UrNetwork hotswap successor active; continuing supervision."
                failures=0
                while pgrep -f "urnetwork.*provide" >/dev/null 2>&1; do
                    func_sleep 2
                done
                func_exit_if_stopping
                continue
            fi
            log " [INFO] UrNetwork exited cleanly."
            break
        fi
        if [ "$code" -eq 78 ]; then
            log "[WARN] Provider exited with auth error (code 78) — token expired or revoked."
            rm -f "$JWT_FILE"
            if [ -n "$USER_AUTH" ] && [ -n "$PASSWORD" ]; then
                log "[INFO] Re-authenticating with USER_AUTH/PASSWORD..."
                if "$PROVIDER_BIN" auth --user_auth="$USER_AUTH" --password="$PASSWORD" -f && [ -s "$JWT_FILE" ]; then
                    log "[INFO] Re-authentication successful. Restarting provider..."
                    func_sleep 5
                    continue
                else
                    log "[ERROR] Re-authentication failed."
                fi
            fi
            log "[CRITICAL] Token expired/revoked and credentials unavailable. Check USER_AUTH/PASSWORD and restart."
            func_sleep 30
            exit 78
        fi
        if [ "$code" -eq 75 ]; then
            log "[INFO] UrNetwork requested planned restart (exit 75); restarting in 5s."
            failures=0
            func_sleep 5
            continue
        fi
        failures=$((failures+1))
        log "[WARN] UrNetwork crashed (#$failures; code=$code)"
        if [ "$failures" -ge 3 ]; then
            log "[ERROR] Too many crashes; clearing JWT and reauthenticating"
            rm -f "$JWT_FILE" || true
            func_do_login
            failures=0
        fi
        log "[INFO] Waiting 60s before retry"
        func_sleep 60
    done
}

func_start_provider_nightly(){
    failures=0
    while :; do
        func_exit_if_stopping
        log "[INFO] Starting UrNetwork (attempt #$((failures+1)))"
        PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_${PROVIDER_FLAVOR}"
		BIN_VER="$($PROVIDER_BIN --version)"
		log "[INFO] Running UrNetwork build ${BIN_VER}"
        # Capture the real exit code (set -e safe; bare `provide` would abort the loop on crash)
        resolve_provider_env_args
        # shellcheck disable=SC2086  # PROVIDER_ENV_ARGS is a deliberately split flag list
        func_run_provider "$PROVIDER_BIN" provide $PROVIDER_ENV_ARGS
        if [ "$code" -eq 0 ]; then
            if [ -f "$HOME/.urnetwork/update-pending" ]; then
                rm -f "$HOME/.urnetwork/update-pending"
                log "[INFO] Binary updated — restarting provider."
                failures=0
                continue
            fi
            if [ -f "$HOME/.urnetwork/hotswap-pending" ] || pgrep -f "urnetwork.*provide" >/dev/null 2>&1; then
                rm -f "$HOME/.urnetwork/hotswap-pending"
                log "[INFO] UrNetwork hotswap successor active; continuing supervision."
                failures=0
                while pgrep -f "urnetwork.*provide" >/dev/null 2>&1; do
                    func_sleep 2
                done
                func_exit_if_stopping
                continue
            fi
            log " [INFO] UrNetwork exited cleanly."
            break
        fi
        if [ "$code" -eq 78 ]; then
            log "[WARN] Provider exited with auth error (code 78) — token expired or revoked."
            rm -f "$JWT_FILE"
            if [ -n "$USER_AUTH" ] && [ -n "$PASSWORD" ]; then
                log "[INFO] Re-authenticating with USER_AUTH/PASSWORD..."
                if "$PROVIDER_BIN" auth --user_auth="$USER_AUTH" --password="$PASSWORD" -f && [ -s "$JWT_FILE" ]; then
                    log "[INFO] Re-authentication successful. Restarting provider..."
                    func_sleep 5
                    continue
                else
                    log "[ERROR] Re-authentication failed."
                fi
            fi
            log "[CRITICAL] Token expired/revoked and credentials unavailable. Check USER_AUTH/PASSWORD and restart."
            func_sleep 30
            exit 78
        fi
        if [ "$code" -eq 75 ]; then
            log "[INFO] UrNetwork requested planned restart (exit 75); restarting in 5s."
            failures=0
            func_sleep 5
            continue
        fi
        failures=$((failures+1))
        log "[WARN] UrNetwork crashed (#$failures; code=$code)"
        if [ "$failures" -ge 3 ]; then
            log "[ERROR] Too many crashes; clearing JWT and reauthenticating"
            rm -f "$JWT_FILE" || true
            func_check_credentials
            failures=0
        fi
        log "[INFO] Waiting 60s before retry"
        func_sleep 60
    done
}

func_start_provider_jwt(){
    JWT_TOKEN="${1:-}"
    PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_${PROVIDER_FLAVOR}"
    BIN_VER="$($PROVIDER_BIN --version 2>/dev/null || echo "dev")"
    log "[INFO] Running UrNetwork build ${BIN_VER}"

    # Priority 1: Existing session file (Shared Volume / Watchtower Path)
    if [ -s "$JWT_FILE" ]; then
        if command -v validate_jwt_file >/dev/null 2>&1; then
            if ! validate_jwt_file "$JWT_FILE"; then
                log "[WARN] Stored JWT expired or invalid - removing stale token"
                rm -f "$JWT_FILE"
            fi
        fi
    fi

    if [ -s "$JWT_FILE" ]; then
        log "[INFO] Existing session found at $JWT_FILE - skipping auth"

    # Priority 2: Authentication via Environment Variable (Safe for dash-prefixed tokens)
    elif [ -n "$URNETWORK_AUTH_CODE" ]; then
        log "[INFO] Starting UrNetwork with provided auth code (environment)..."
        # We use -f to force overwrite and skip prompts.
        if "$PROVIDER_BIN" auth "$URNETWORK_AUTH_CODE" -f; then
            log "[INFO] UrNetwork authenticated successfully."
        else
            code=$?
            log "[ERROR] UrNetwork authentication failed with code=$code"
            exit $code
        fi
        # Verify result
        [ -s "$JWT_FILE" ] || { log "[ERROR] JWT file not written to $JWT_FILE"; exit 1; }

    # Priority 3: Authentication via Positional Argument (Backward Compatibility)
    elif [ "$#" -eq 1 ]; then
        JWT_TOKEN="$1"
        log "[INFO] Starting UrNetwork with provided auth code (argument) ..."
        # Shell quoting handles tokens starting with dashes safely.
        if "$PROVIDER_BIN" auth "$JWT_TOKEN" -f; then
            log "[INFO] UrNetwork authenticated successfully."
        else
            code=$?
            log "[ERROR] UrNetwork authentication failed with code=$code"
            exit $code
        fi
        [ -s "$JWT_FILE" ] || { log "[ERROR] JWT failed; code=$code"; exit 1; }

    # Failure: No session and no auth code provided
    else
        log "[ERROR] No session found and no URNETWORK_AUTH_CODE provided."
        log "[ERROR] On first run, provide your code via environment or argument."
        exit 1
    fi

    # Start loop: Provider is now authenticated, keep it running
    failures=0
    reauth_attempts=0
    while :; do
        func_exit_if_stopping
        log "[INFO] Starting UrNetwork (attempt #$((failures+1)))"
        # Capture the real exit code. Reading $? after the `if ... fi` gave
        # the compound's status, 0 whenever provide failed: every crash logged
        # code=0 and the code 78 re-auth branch below could never run.
        resolve_provider_env_args
        # shellcheck disable=SC2086  # PROVIDER_ENV_ARGS is a deliberately split flag list
        func_run_provider "$PROVIDER_BIN" provide $PROVIDER_ENV_ARGS
        if [ "$code" -eq 0 ]; then
            if [ -f "$HOME/.urnetwork/update-pending" ]; then
                rm -f "$HOME/.urnetwork/update-pending"
                log "[INFO] Binary updated — restarting provider."
                failures=0
                reauth_attempts=0
                continue
            fi
            if [ -f "$HOME/.urnetwork/hotswap-pending" ] || pgrep -f "urnetwork.*provide" >/dev/null 2>&1; then
                rm -f "$HOME/.urnetwork/hotswap-pending"
                log "[INFO] UrNetwork hotswap successor active; continuing supervision."
                failures=0
                reauth_attempts=0
                while pgrep -f "urnetwork.*provide" >/dev/null 2>&1; do
                    func_sleep 2
                done
                func_exit_if_stopping
                continue
            fi
            log "[INFO] UrNetwork exited cleanly."
            break
        fi

        # Exit code 78 = token invalid/expired. Delete the stale JWT and re-authenticate
        # if URNETWORK_AUTH_CODE is available. This is the only case where we delete the JWT.
        if [ "$code" -eq 78 ]; then
            log "[WARN] UrNetwork exited with auth error (code 78) — token expired or revoked."
            rm -f "$JWT_FILE"
            reauth_attempts=$((reauth_attempts+1))
            if [ "$reauth_attempts" -ge 3 ]; then
                log "[CRITICAL] Re-auth attempted $reauth_attempts times without recovery. Exiting."
                exit 78
            fi
            if [ -n "$URNETWORK_AUTH_CODE" ]; then
                log "[INFO] Re-authenticating with URNETWORK_AUTH_CODE (attempt $reauth_attempts)..."
                if "$PROVIDER_BIN" auth "$URNETWORK_AUTH_CODE" -f && [ -s "$JWT_FILE" ]; then
                    log "[INFO] Re-authentication successful. Restarting provider..."
                    failures=0
                    reauth_attempts=0
                    func_sleep 5
                    continue
                else
                    log "[ERROR] Re-authentication failed."
                fi
            elif [ -n "$JWT_TOKEN" ]; then
                log "[INFO] Re-authenticating with positional token (attempt $reauth_attempts)..."
                if "$PROVIDER_BIN" auth "$JWT_TOKEN" -f && [ -s "$JWT_FILE" ]; then
                    log "[INFO] Re-authentication successful. Restarting provider..."
                    failures=0
                    reauth_attempts=0
                    func_sleep 5
                    continue
                else
                    log "[ERROR] Re-authentication failed."
                fi
            fi
            log "[CRITICAL] Token expired/revoked and re-auth unavailable. Set URNETWORK_AUTH_CODE and restart."
            func_sleep 30
            exit 78
        fi

        if [ "$code" -eq 75 ]; then
            log "[INFO] UrNetwork requested planned restart (exit 75); restarting in 5s."
            failures=0
            reauth_attempts=0
            func_sleep 5
            continue
        fi

        failures=$((failures+1))
        log "[WARN] UrNetwork crashed (#$failures; code=$code)"

        # Shared-volume safety: never delete the JWT for generic crashes. In the 3-in-1
        # shared config model that would deauth the whole stack, and the single-use auth
        # code is already consumed. After repeated crashes, exit and let Docker restart.
        if [ "$failures" -ge 5 ]; then
            log "[ERROR] Too many consecutive crashes ($failures); exiting for Docker to restart. Session preserved."
            exit 1
        fi

        log "[INFO] Waiting 60s before retry"
        func_sleep 60
    done
}

# === Mode Dispatchers ===
func_do_login() {
    case "$PROVIDER_MODE" in
      nightly) func_do_login_nightly ;;
      *)       func_do_login_stable ;;
    esac
}

func_start_provider() {
    case "$PROVIDER_MODE" in
      stable)  func_start_provider_stable ;;
      nightly) func_start_provider_nightly ;;
      jwt)     func_start_provider_jwt "$@" ;;
      *)
        log "[ERROR] Unknown PROVIDER_MODE '$PROVIDER_MODE'" >&2
        exit 1
        ;;
    esac
}

# === Bootstrap Sequence ===
func_bootstrap() {
    case "$PROVIDER_MODE" in
      stable)
        # sh /app/urnetwork_ipinfo.sh
        func_get_architecture
        func_choose_network
        func_check_dir
        func_check_credentials
        func_check_proxy
        func_report_identity
        func_ip_checker
        func_start_vnstat
        # Pass host's actual hostname (if provided) so provider reports correctly on dashboard
        export HOST_HOSTNAME="${HOST_HOSTNAME:-}"
        func_do_login
        func_start_provider
        ;;
      nightly)
        # sh /app/urnetwork_ipinfo.sh
        func_get_architecture
        func_check_dir
        func_check_credentials
        func_check_proxy
        func_report_identity
        func_ip_checker
        func_start_vnstat
        func_check_update
        # choose_network runs after func_check_update: on a fresh nightly
        # container the urnetwork_${A_SYS_ARCH}_${PROVIDER_FLAVOR} binary doesn't exist
        # until the update check downloads it.
        func_choose_network
        # Pass host's actual hostname (if provided) so provider reports correctly on dashboard
        export HOST_HOSTNAME="${HOST_HOSTNAME:-}"
        (
          while :; do
            NOW="$(TZ='America/Tijuana' date +%H:%M)"
            if [ "$NOW" = "$UPDATE_TIME" ]; then
                log "Watcher: hit $UPDATE_TIME, updating"
                func_check_update
                if ! ps aux | grep -q "[u]rnetwork_${A_SYS_ARCH}_"; then
                    log "UrNetwork not running; launching now"
                    func_do_login
                    func_start_provider
                else
                    log "UrNetwork already running; skipping restart"
                fi
                sleep 60
            fi
            sleep 30
          done
        ) &
        WATCHER_PID=$!
        log "Time‐watcher PID is $WATCHER_PID"
        func_do_login
        func_start_provider
        ;;
      jwt)
        func_get_architecture
        func_choose_network
        func_check_dir
        func_check_proxy
        func_report_identity
        func_ip_checker
        func_start_vnstat
        # Pass host's actual hostname (if provided) so provider reports correctly on dashboard
        export HOST_HOSTNAME="${HOST_HOSTNAME:-}"
        func_start_provider "$@"
        ;;
      *)
        log "[ERROR] Unknown PROVIDER_MODE '$PROVIDER_MODE'" >&2
        exit 1
        ;;
    esac
}

# === Main Entrypoint ===
main() {
    func_install_stop_trap
    func_bootstrap "$@"
}
