#!/bin/sh
# URNetwork Provider Entrypoint Script
# ------------------------------------
# This script bootstraps the URNetwork provider inside a container.

# Exit immediately if any command fails
set -e

# === Configuration Variables ===
APP_DIR="/app"
JWT_FILE="/root/.urnetwork/jwt"
ENABLE_VNSTAT="${ENABLE_VNSTAT:-true}"

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

# === Proxy Setup ===
func_check_proxy() {
    log "[INFO] Checking proxy configuration"
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

# === Public IP Fetching ===
func_get_ip() {
  # Use curl ip.me -4 as suggested for simplicity and IPv4 focus
  export URNETWORK_PUBLIC_IP="$(curl -s ip.me -4 || echo "")"
  if [ -n "$URNETWORK_PUBLIC_IP" ]; then
    log "[INFO] Public IP detected: $URNETWORK_PUBLIC_IP"
  else
    log "[WARN] Could not detect public IP"
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

# === Provider Lifecycle Management ===
func_start_provider(){
    PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_stable"
    BIN_VER="$($PROVIDER_BIN --version 2>/dev/null || echo "dev")"
    log "[INFO] Running UrNetwork build v${BIN_VER}"

    # Priority 1: Existing session file (Shared Volume / Watchtower Path)
    if [ -s "$JWT_FILE" ]; then
        log "[INFO] Existing session found at $JWT_FILE — skipping auth"
    
    # Priority 2: Authentication via Environment Variable (Safe for dash-prefixed tokens)
    elif [ -n "$URNETWORK_AUTH_CODE" ]; then
        log "[INFO] Starting UrNetwork with provided auth code (environment)..."
        # We use -f to force overwrite and skip prompts
        "$PROVIDER_BIN" auth-provide -f || true
        code=$?
        if [ "$code" -eq 0 ]; then
            log "[INFO] UrNetwork exited cleanly after authentication."
        else
            log "[ERROR] UrNetwork authentication failed with code=$code"
            exit $code
        fi
        # If it successfully authed but exited (standard behavior), it's now Priority 1
        [ -s "$JWT_FILE" ] || { log "[ERROR] JWT file not written to $JWT_FILE"; exit 1; }

    # Priority 3: Authentication via Positional Argument (Backward Compatibility)
    elif [ "$#" -eq 1 ]; then
        JWT_TOKEN="$1"
        log "[INFO] Starting UrNetwork with provided auth code (argument) ..."
        # Use -- to handle tokens starting with dashes
        "$PROVIDER_BIN" auth-provide -f -- "$JWT_TOKEN" || true
        code=$?
        [ -s "$JWT_FILE" ] || { log "[ERROR] JWT failed; code=$code"; exit 1; }

    # Failure: No session and no auth code provided
    else
        log "[ERROR] No session found and no URNETWORK_AUTH_CODE provided."
        log "[ERROR] On first run, provide your code via environment or argument."
        exit 1
    fi

    # Start loop: Provider is now authenticated, keep it running
    failures=0
    while :; do
        log "[INFO] Starting UrNetwork (attempt #$((failures+1)))"
        "$PROVIDER_BIN" provide || true
        code=$?
        if [ "$code" -eq 0 ]; then
            log "[INFO] UrNetwork exited cleanly."
            break
        fi
        failures=$((failures+1))
        log "[WARN] UrNetwork crashed (#$failures; code=$code)"
        if [ "$failures" -ge 3 ]; then
            log "[ERROR] Too many crashes; clearing session and requiring re-auth"
            rm -f "$JWT_FILE" || true
            exit 1
        fi
        log "[INFO] Waiting 60s before retry"
        sleep 60
    done
}

# === Bootstrap Sequence ===
func_bootstrap() {
    func_get_architecture
    func_check_dir
    func_check_proxy
    func_get_ip
    func_start_vnstat
    func_start_provider "$@"
}

# === Main Entrypoint ===
main() {
    func_bootstrap "$@"
}

main "$@"
