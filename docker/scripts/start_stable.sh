#!/bin/sh
# URNetwork Provider Entrypoint Script
# ------------------------------------
# This script bootstraps the URNetwork provider inside a container.
# Responsibilities:
#   - Validate environment and credentials
#   - Configure proxy if provided
#   - Detect system architecture
#   - Optionally check public IP
#   - Start vnStat monitoring and lightweight HTTP server
#   - Authenticate and obtain JWT
#   - Manage provider lifecycle (restart on crash)

# Exit immediately if any command fails
set -e

# === Configuration Variables ===
export TZ="America/Tijuana"
APP_DIR="/app"
JWT_FILE="/root/.urnetwork/jwt"
ENABLE_VNSTAT="${ENABLE_VNSTAT:-true}"
ENABLE_IP_CHECKER="${ENABLE_IP_CHECKER:-false}"
IP_CHECKER_URL="https://raw.githubusercontent.com/techroy23/IP-Checker/refs/heads/main/app.sh"

# === Logging Helper ===
log() {
  echo "$(date '+%Y-%m-%d %H:%M:%S') >>> UrNetwork >>> $*"
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
		PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_stable"
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

# === Client Identity Reporting ===
# Fetches the public IP used to build this node's dashboard identity label
# (node name @ redacted-IP [version]). Always on; no configuration required.
# Distinct from func_ip_checker below, which is an opt-in diagnostic.
func_report_identity() {
  # Use curl ip.me -4 with a 5s timeout to avoid hanging startup
  export URNETWORK_PUBLIC_IP="$(curl -s --max-time 5 --retry 0 ip.me -4 || echo "")"
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

# === vnStat Monitoring Setup ===
func_start_vnstat() {
    VNSTAT_LC="$(printf '%s' "$ENABLE_VNSTAT" | tr '[:upper:]' '[:lower:]')"
    if [ "$VNSTAT_LC" = "true" ]; then
        if [ -f /var/lib/vnstat/vnstat.db ]; then
            log "[INFO] vnStat DB already exists (SQLite backend)"
        elif [ -f /var/lib/vnstat/.config ]; then
            log "[INFO] vnStat DB already exists (binary backend)"
        else
            log "[INFO] Initializing vnStat database"
            vnstatd --initdb
        fi
        vnstatd -d --alwaysadd >/dev/null 2>&1
        log "[INFO] vnstatd started"
        httpd -f -p 8080 -h /app &
        log "[INFO] HTTP server started on container port 8080"
    else
        log "[INFO] VNSTAT disabled ..."
    fi
}

# === Authentication (JWT) ===
func_do_login() {
    PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_stable"

    # If a JWT already exists (e.g. persisted via a Docker volume), skip auth entirely.
    # This is the Watchtower-safe path: container restarts after an image update reuse
    # the existing token rather than hitting the API again with a spent auth code.
    if [ -s "$JWT_FILE" ]; then
        log "[INFO] Existing JWT found at $JWT_FILE — skipping authentication"
        return 0
    fi

    log "[INFO] No JWT found, starting authentication flow..."

    # Retry loop for authentication
    while true; do
        log "[INFO] Sleeping 15s before obtaining new JWT..."
        sleep 15
        
        log "[INFO] Attempting authentication..."
        
        # Capture auth command output for parsing
        AUTH_OUTPUT=$("$PROVIDER_BIN" auth --user_auth="$USER_AUTH" --password="$PASSWORD" -f 2>&1) || true
        AUTH_EXIT_CODE=$?
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
            sleep 5
            
            # Verify JWT file exists as backup check
            if [ -s "$JWT_FILE" ]; then
                log "[INFO] JWT file verified at $JWT_FILE"
                sleep 5
                return 0
            else
                log "[WARN] Success message found but JWT file missing - retrying"
                log "[INFO] Will retry authentication in 1 minutes (60 seconds)..."
                sleep 60
            fi
        else
            # Authentication failed - output exit code and auth output
            log "[ERROR] Authentication failed (exit code: $AUTH_EXIT_CODE)" >&2
			log "[ERROR] $(echo "$PANIC_LINE" | tr '[:lower:]' '[:upper:]')" >&2
            log "[INFO] Will retry authentication in 5 minutes (300 seconds)..."
            sleep 300
        fi
    done
}

# === Provider Lifecycle Management ===
func_start_provider(){
    failures=0
    while :; do
        log "[INFO] Starting UrNetwork (attempt #$((failures+1)))"
        PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_stable"
		BIN_VER="$($PROVIDER_BIN --version)"
		log "[INFO] Running UrNetwork build v${BIN_VER}"
        # Capture the real exit code (set -e safe; avoids the `|| true` that pinned code to 0)
        if "$PROVIDER_BIN" provide; then code=0; else code=$?; fi
        if [ "$code" -eq 0 ]; then
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
                    sleep 5
                    continue
                else
                    log "[ERROR] Re-authentication failed."
                fi
            fi
            log "[CRITICAL] Token expired/revoked and credentials unavailable. Check USER_AUTH/PASSWORD and restart."
            sleep 30
            exit 78
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
        sleep 60
    done
}

# === Bootstrap Sequence ===
func_bootstrap() {
    # sh /app/urnetwork_ipinfo.sh
	func_get_architecture
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
}

# === Main Entrypoint ===
main() {
    func_bootstrap
}

main "$@"
