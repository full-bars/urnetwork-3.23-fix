
#!/bin/sh
# URNetwork Provider Entrypoint Script
# ------------------------------------
# This script bootstraps the URNetwork provider inside a container.
# Responsibilities:
#   - Configure proxy if provided
#   - Detect system architecture
#   - Optionally check public IP
#   - Start vnStat monitoring and lightweight HTTP server

# Exit immediately if any command fails
set -e

# === Configuration Variables ===
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

# === Public IP Checker ===
func_get_ip() {
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

# === Provider Lifecycle Management ===
func_start_provider(){
    PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_stable"
    BIN_VER="$($PROVIDER_BIN --version)"
    log "[INFO] Running UrNetwork build v${BIN_VER}"

    # If a session JWT already exists in the mounted volume, skip re-auth and
    # run provide directly. This is the Watchtower-safe path — container restarts
    # and image updates reuse the existing session rather than consuming the
    # (single-use) auth code again.
    if [ -s "$JWT_FILE" ] && [ "$#" -eq 0 ]; then
        log "[INFO] Existing session found at $JWT_FILE — skipping auth"
    elif [ "$#" -eq 0 ]; then
        log "[ERROR] jwt mode requires a JWT token argument on first run"
        log "[ERROR] Usage: docker run ... IMAGE <JWT_TOKEN>"
        log "[ERROR] After first run the session is persisted to the volume and no argument is needed."
        exit 1
    elif [ "$#" -ne 1 ]; then
        log "[ERROR] Expected exactly 1 JWT token argument, got $#"
        exit 1
    else
        JWT_TOKEN="$1"
        log "[INFO] Starting UrNetwork with provided JWT token ..."
        "$PROVIDER_BIN" auth-provide "$JWT_TOKEN"
        code=$?
        if [ "$code" -eq 0 ]; then
            log "[INFO] UrNetwork exited cleanly."
        else
            log "[ERROR] UrNetwork exited with code=$code"
        fi
        return $code
    fi

    # Session exists — restart loop mirrors start_stable.sh behaviour
    failures=0
    while :; do
        log "[INFO] Starting UrNetwork (attempt #$((failures+1)))"
        "$PROVIDER_BIN" provide
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
            log "[ERROR] Session cleared. Restart the container with a fresh JWT token."
            exit 1
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
    func_check_proxy
    # func_get_ip
    func_start_vnstat
    func_start_provider "$@"
}

# === Main Entrypoint ===
main() {
    func_bootstrap "$@"
}

main "$@"
