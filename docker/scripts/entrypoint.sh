#!/bin/sh
set -e
# Entrypoint script for selecting and starting the build of UrNetwork.
# 
# Usage:
#   BUILD=<stable|nightly|jwt> ./entrypoint.sh
#
# Environment Variables:
#   BUILD  - Determines which startup script to run. Defaults to "stable".
#              Accepted values: "stable", "nightly".
#
# Behavior:
#   - Logs timestamped messages for visibility.
#   - Normalizes BUILD to lowercase.
#   - Executes the appropriate startup script based on BUILD.
#   - Exits with error if BUILD is invalid.

# === Logging Helper ===
log() {
  echo "$(date '+%Y-%m-%d %H:%M:%S') >>> UrNetwork >>> $*"
}

# Sourcing JWT verification helper
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [ -f "$SCRIPT_DIR/jwt_verify.sh" ]; then
  . "$SCRIPT_DIR/jwt_verify.sh"
elif [ -f "/app/jwt_verify.sh" ]; then
  . "/app/jwt_verify.sh"
fi

# Accept AUTHCODE as an alias for URNETWORK_AUTH_CODE
if [ -z "${URNETWORK_AUTH_CODE:-}" ] && [ -n "${AUTHCODE:-}" ]; then
  export URNETWORK_AUTH_CODE="$AUTHCODE"
fi

ORIG_BUILD="${BUILD:-}"
AUTH_RAW="${AUTH:-}"
AUTH_LC="$(printf '%s' "$AUTH_RAW" | tr '[:upper:]' '[:lower:]')"
AUTH_DERIVED_BUILD=""

if [ -n "$AUTH_LC" ]; then
  case "$AUTH_LC" in
    userpass)
      AUTH_DERIVED_BUILD="stable"
      ;;
    authcode)
      AUTH_DERIVED_BUILD="jwt"
      ;;
    *)
      log "ERROR: Invalid AUTH '$AUTH_RAW'. Valid options are: userpass, authcode"
      exit 1
      ;;
  esac
fi

# Precedence & Deprecation Hint:
# Explicit BUILD wins. If BUILD is unset and AUTH is set, derive from AUTH.
# If both set and they conflict, log a clear WARNING and let BUILD win.
if [ -n "$ORIG_BUILD" ]; then
  BUILD="$(printf '%s' "$ORIG_BUILD" | tr '[:upper:]' '[:lower:]')"
  if [ -z "$AUTH_LC" ]; then
    log "BUILD is deprecated for auth selection, use AUTH=userpass|authcode"
  else
    if [ "$BUILD" != "$AUTH_DERIVED_BUILD" ]; then
      log "WARNING: Conflicting AUTH='$AUTH_RAW' and BUILD='$ORIG_BUILD' specified; BUILD takes precedence"
    fi
  fi
else
  if [ -n "$AUTH_LC" ]; then
    BUILD="$AUTH_DERIVED_BUILD"
  else
    # Both unset: keep today's auto-selection
    if [ "$#" -gt 0 ] || [ -n "${URNETWORK_AUTH_CODE:-}" ]; then
      BUILD="jwt"
    else
      BUILD="stable"
    fi
  fi
fi

# Validate stored JWT if present: decode locally and verify exp/nbf
JWT_FILE="${HOME:-/root}/.urnetwork/jwt"
if [ -s "$JWT_FILE" ]; then
  if command -v validate_jwt_file >/dev/null 2>&1; then
    if ! validate_jwt_file "$JWT_FILE"; then
      log "Stored JWT expired or invalid - removing stale token"
      rm -f "$JWT_FILE"
    fi
  fi
fi

# Validate credentials for AUTH modes:
# AUTH=userpass requires USER_AUTH and PASSWORD.
# AUTH=authcode requires an auth code (or an existing stored JWT).
if [ "$AUTH_LC" = "userpass" ]; then
  if [ -z "${USER_AUTH:-}" ] || [ -z "${PASSWORD:-}" ]; then
    missing=""
    [ -z "${USER_AUTH:-}" ] && missing="USER_AUTH"
    [ -z "${PASSWORD:-}" ] && missing="${missing:+$missing and }PASSWORD"
    log "ERROR: AUTH=userpass requires $missing to be set"
    exit 1
  fi
elif [ "$AUTH_LC" = "authcode" ]; then
  if [ -z "${URNETWORK_AUTH_CODE:-}" ] && [ "$#" -eq 0 ] && [ ! -s "$JWT_FILE" ]; then
    log "ERROR: AUTH=authcode requires AUTHCODE (or an existing session volume at ${HOME:-/root}/.urnetwork)"
    exit 1
  fi
fi

# === Startup Safety Messages ===
STATE_DIR="${HOME:-/root}/.urnetwork"
[ -d "$STATE_DIR" ] || STATE_DIR="/root/.urnetwork"

is_mounted() {
  target="$1"
  if command -v mountpoint >/dev/null 2>&1; then
    mountpoint -q "$target" 2>/dev/null && return 0
  fi
  if [ -r /proc/self/mountinfo ]; then
    awk -v d="$target" '$5 == d { found=1 } END { exit !found }' /proc/self/mountinfo 2>/dev/null && return 0
  fi
  return 1
}

has_identity() {
  target="$1"
  [ -s "$target/jwt" ] || [ -f "$target/identity.json" ] || [ -f "$target/private_key.pem" ] || [ -f "$target/network.json" ]
}

is_empty_dir() {
  target="$1"
  [ ! -d "$target" ] || [ -z "$(ls -A "$target" 2>/dev/null)" ]
}

if ! is_mounted "$STATE_DIR" && ! is_mounted "/root/.urnetwork" || ( ! has_identity "$STATE_DIR" && is_empty_dir "$STATE_DIR" ); then
  log "WARNING: Without a persistent volume mounted at /root/.urnetwork every container restart creates a NEW provider identity."
fi

log "INFO: A payout wallet and consent must be set in the URnetwork app or website (or with the provider wallet commands in docs/Bittensor-Operations.md) for the provider to be paid."

# Translate TURBO=v4|v8 into URNETWORK_PROFILE so the binary picks it up.
# GOGC is handled internally by the binary when turbo is active.
TURBO="$(echo "${TURBO:-}" | tr '[:upper:]' '[:lower:]')"
case "$TURBO" in
  v4|v8)
    export URNETWORK_PROFILE="turbo-${TURBO}"
    log "Turbo mode: ${TURBO} (window=$([ "$TURBO" = "v4" ] && echo 4 || echo 8)MiB)"
    ;;
  "")
    ;;
  *)
    log "WARNING: unknown TURBO value '$TURBO' — ignoring (valid: v4, v8)"
    ;;
esac

log "Script version: v3.23.2026"
#log "Starting with"
#log "*** *** *** *** *** *** *** *** *** ***"
#log "USER_AUTH = $USER_AUTH"
#log "PASSWORD  = $PASSWORD"
#log "AUTH-CODE = $AUTHCODE $JWT_TOKEN"
#log "BUILD     = $BUILD"
#log "PELICAN   = $PELICAN"
#log "*** *** *** *** *** *** *** *** *** ***"

# === Helper to run as pelican if requested ===
run_as_pelican() {
  log "Running Pelican Panel mode..."
  exec /app/pelican_panel.sh
}

run_normal() {
  case "$BUILD" in
    stable)
      exec /app/start_stable.sh
      ;;
    nightly)
      exec /app/start_nightly.sh
      ;;
    jwt)
      if [ "$#" -eq 0 ] && [ -s "$HOME/.urnetwork/jwt" ]; then
        log "Existing session found — skipping auth"
        exec /app/start_jwt.sh
      elif [ "$#" -eq 0 ] && [ -n "$URNETWORK_AUTH_CODE" ]; then
        log "Authentication requested via environment variable"
        exec /app/start_jwt.sh
      elif [ "$#" -ne 1 ]; then
        log "ERROR: jwt mode requires a JWT token on first run"
        log "ERROR: Usage: docker run ... IMAGE <JWT_TOKEN>"
        log "ERROR: On subsequent starts the session is read from the $HOME/.urnetwork volume automatically."
        exit 1
      fi
      log "Entrypoint received $# arguments"
      JWT_TOKEN="$1"
      exec /app/start_jwt.sh "$JWT_TOKEN"
      ;;
    *)
      log "Invalid build: $BUILD"
      log "Valid options are: stable, nightly, jwt"
      exit 1
      ;;
  esac
}

# Route based on PELICAN setting
if [ "$PELICAN" = "yes" ]; then
  run_as_pelican
else
  run_normal "$@"
fi
