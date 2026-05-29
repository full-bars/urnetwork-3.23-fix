#!/bin/sh
set -e

APP_DIR="/app"
JWT_FILE="$HOME/.urnetwork/jwt"
ENABLE_VNSTAT="false"

# === Logging Helper ===
log() {
  echo "$(date '+%Y-%m-%d %H:%M:%S') >>> UrNetwork >>> $*"
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
        log "[INFO] Starting UrNetwork (attempt #$((failures+1)))"
        PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_stable"
		BIN_VER="$($PROVIDER_BIN --version)"
		log "[INFO] Running UrNetwork build v${BIN_VER}"
        # Capture the real exit code (set -e safe; bare `provide` would abort the loop on crash)
        if "$PROVIDER_BIN" provide; then code=0; else code=$?; fi
        if [ "$code" -eq 0 ]; then
            log " [INFO] UrNetwork exited cleanly."
            break
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
        sleep 60
    done
}

func_start_provider_jwt(){
    log "[INFO] Starting UrNetwork ..."
    PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_stable"
    BIN_VER="$($PROVIDER_BIN --version)"
    log "[INFO] Running UrNetwork build v${BIN_VER}"

    # set -e safe: bare command + code=$? would abort before the error branch
    if "$PROVIDER_BIN" auth-provide "$AUTHCODE" -f; then
        log "[INFO] UrNetwork exited cleanly."
    else
        code=$?
        log "[ERROR] UrNetwork exited with code=$code"
    fi
}

# Main
if [ "$BUILD" = "jwt" ]; then
  func_get_architecture
  func_report_identity
  func_start_provider_jwt
else
  func_get_architecture
  func_report_identity
  func_check_credentials
  func_do_login
  func_start_provider
fi
