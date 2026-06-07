#!/bin/sh
# URNetwork Provider Entrypoint Script
# ------------------------------------
# This script bootstraps the URNetwork provider inside a container.

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

# === Provider Lifecycle Management ===
func_start_provider(){
    JWT_TOKEN="${1:-}"
    PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_stable"
    BIN_VER="$($PROVIDER_BIN --version 2>/dev/null || echo "dev")"
    log "[INFO] Running UrNetwork build v${BIN_VER}"

    # Priority 1: Existing session file (Shared Volume / Watchtower Path)
    if [ -s "$JWT_FILE" ]; then
        log "[INFO] Existing session found at $JWT_FILE — skipping auth"
    
    # Priority 2: Authentication via Environment Variable (Safe for dash-prefixed tokens)
    elif [ -n "$URNETWORK_AUTH_CODE" ]; then
        log "[INFO] Starting UrNetwork with provided auth code (environment)..."
        # We use -f to force overwrite and skip prompts.
        if "$PROVIDER_BIN" auth "" -f; then
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
        log "[INFO] Starting UrNetwork (attempt #$((failures+1)))"
        if "$PROVIDER_BIN" provide; then
            log "[INFO] UrNetwork exited cleanly."
            break
        fi
        code=$?

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
                if "$PROVIDER_BIN" auth "" -f && [ -s "$JWT_FILE" ]; then
                    log "[INFO] Re-authentication successful. Restarting provider..."
                    failures=0
                    reauth_attempts=0
                    sleep 5
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
                    sleep 5
                    continue
                else
                    log "[ERROR] Re-authentication failed."
                fi
            fi
            log "[CRITICAL] Token expired/revoked and re-auth unavailable. Set URNETWORK_AUTH_CODE and restart."
            sleep 30
            exit 78
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
        sleep 60
    done
}

# === Bootstrap Sequence ===
func_bootstrap() {
    func_get_architecture
    func_check_dir
    func_check_proxy
    func_report_identity
    func_ip_checker
    func_start_vnstat
    # Pass host's actual hostname (if provided) so provider reports correctly on dashboard
    export HOST_HOSTNAME="${HOST_HOSTNAME:-}"
    func_start_provider "$@"
}

# === Main Entrypoint ===
main() {
    func_bootstrap "$@"
}

main "$@"
