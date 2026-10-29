#!/bin/sh
set -e

APP_DIR="/app"
JWT_FILE="$HOME/.urnetwork/jwt"
ENABLE_VNSTAT="false"

# Accept URNETWORK_AUTH_CODE as an alias for AUTHCODE if unset
if [ -z "${AUTHCODE:-}" ] && [ -n "${URNETWORK_AUTH_CODE:-}" ]; then
  AUTHCODE="$URNETWORK_AUTH_CODE"
fi

# === Logging Helper ===
log() {
  echo "$(date '+%Y-%m-%d %H:%M:%S') >>> UrNetwork >>> $*"
}

# === Stop Forwarding ===
# Same helpers as docker/scripts/provider_common.sh, kept inline because this
# script is standalone under the Pelican egg (and busybox ash cannot rely on
# parallel edits in another file). `docker stop` / panel STOP signals PID 1,
# which is this shell; with no default action for SIGTERM the provider never
# hears it and Docker SIGKILLs the container after the timeout, skipping the
# provider's clean shutdown.
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

# === Authentication (JWT) ===
func_do_login() {
    rm -f "$JWT_FILE" || true
    log "[INFO] Removed existing JWT (if any)"
    
    PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_stable"
    
    # Retry loop for authentication
    while true; do
        func_exit_if_stopping
        log "[INFO] Sleeping 15s before obtaining new JWT..."
        func_sleep 15
        
        log "[INFO] Attempting authentication..."
        
        # Capture the auth command's real exit status: `|| true` followed by
        # $? always recorded 0, hiding every failure code (same fix as the
        # shared login loop in provider_common.sh).
        AUTH_EXIT_CODE=0
        AUTH_OUTPUT=$("$PROVIDER_BIN" auth --user_auth="$USER_AUTH" --password="$PASSWORD" -f 2>&1) || AUTH_EXIT_CODE=$?
        PANIC_LINE=$(echo "$AUTH_OUTPUT" | grep -E '^panic:' || true)
        
        if [ "${DEBUG:-false}" = "true" ]; then
            if [ -n "$USER_AUTH" ]; then user_auth_set=yes; else user_auth_set=no; fi
            if [ -n "$PASSWORD" ]; then password_set=yes; else password_set=no; fi
            echo "DEBUG: USER_AUTH set=$user_auth_set length=${#USER_AUTH}"
            echo "DEBUG: PASSWORD set=$password_set length=${#PASSWORD}"
            echo "DEBUG: AUTH_EXIT_CODE=$AUTH_EXIT_CODE"
            # Preserve the output line without exposing echoed credentials.
            case "$AUTH_OUTPUT" in
                *"$USER_AUTH"*|*"$PASSWORD"*) echo "DEBUG: AUTH_OUTPUT=[redacted]" ;;
                *) echo "DEBUG: AUTH_OUTPUT=$AUTH_OUTPUT" ;;
            esac
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

# === Client Identity Reporting ===
# Fetches the public IP used to build this node's dashboard identity label
# (node name @ redacted-IP [version]). Always on; no configuration required.
func_report_identity() {
  # Use curl ip.me -4 with a 5s timeout to avoid hanging startup
  export URNETWORK_PUBLIC_IP="$(curl -s --max-time 5 --retry 0 ip.me -4 || echo "")"
  if [ -n "$URNETWORK_PUBLIC_IP" ]; then
    log "[INFO] Public IP detected: $URNETWORK_PUBLIC_IP"
  else
    log "[WARN] Could not detect public IP (timeout or service unreachable)"
  fi
}

# === Proxy Setup ===
# Detect proxy configuration for the panel provider. URL-sourced proxies
# (PROXY_URL) are handled natively by the provider from environment.
# File-based proxies (PROXY_FILE) must be a mounted volume; the provider
# reads them via the env var fallback added in v30.8.
func_check_proxy() {
    # URL-sourced: provider reads PROXY_URL from environment automatically
    if [ -n "${PROXY_URL:-}" ]; then
        log "[INFO] PROXY_URL set; provider will fetch proxy lists from URL(s)"
    fi

    # File-based: validate the path exists and is non-empty before provider tries to load it
    if [ -n "${PROXY_FILE:-}" ]; then
        if [ -s "$PROXY_FILE" ]; then
            log "[INFO] PROXY_FILE=$PROXY_FILE found; provider will load proxies from file"
        elif [ -f "$PROXY_FILE" ]; then
            log "[ERROR] PROXY_FILE=$PROXY_FILE exists but is empty (need one ip:port:user:pass per line)"
            return 1
        else
            log "[ERROR] PROXY_FILE=$PROXY_FILE set but file not found"
            return 1
        fi
    elif [ -z "${PROXY_URL:-}" ]; then
        log "[WARN] No PROXY_URL or PROXY_FILE set — running with direct connection only"
    fi
}

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

func_start_provider(){
    failures=0
    while :; do
        func_exit_if_stopping
        log "[INFO] Starting UrNetwork (attempt #$((failures+1)))"
        PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_stable"
		BIN_VER="$($PROVIDER_BIN --version)"
		log "[INFO] Running UrNetwork build ${BIN_VER}"
        func_run_provider "$PROVIDER_BIN" provide
        if [ "$code" -eq 0 ]; then
            log " [INFO] UrNetwork exited cleanly."
            break
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
    log "[INFO] Starting UrNetwork ..."
    PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_stable"
    BIN_VER="$($PROVIDER_BIN --version)"
    log "[INFO] Running UrNetwork build ${BIN_VER}"

    func_run_provider "$PROVIDER_BIN" auth-provide "$AUTHCODE" -f
    if [ "$code" -eq 0 ]; then
        log "[INFO] UrNetwork exited cleanly."
    else
        log "[ERROR] UrNetwork exited with code=$code"
    fi
}

# Main
func_install_stop_trap
if [ "$BUILD" = "jwt" ]; then
  func_get_architecture
  func_report_identity
  func_check_proxy || exit 1
  func_start_provider_jwt
else
  func_get_architecture
  func_report_identity
  func_check_credentials
  func_check_proxy || exit 1
  func_do_login
  func_start_provider
fi
