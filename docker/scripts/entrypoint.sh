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

# Default to "stable" if BUILD is not set
BUILD="${BUILD:-stable}"
BUILD="$(echo "$BUILD" | tr '[:upper:]' '[:lower:]')"

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
      if [ "$#" -eq 0 ] && [ -s "/root/.urnetwork/jwt" ]; then
        log "Existing session found — skipping auth"
        exec /app/start_jwt.sh
      elif [ "$#" -eq 0 ] && [ -n "$URNETWORK_AUTH_CODE" ]; then
        log "Authentication requested via environment variable"
        exec /app/start_jwt.sh
      elif [ "$#" -ne 1 ]; then
        log "ERROR: jwt mode requires a JWT token on first run"
        log "ERROR: Usage: docker run ... IMAGE <JWT_TOKEN>"
        log "ERROR: On subsequent starts the session is read from the /root/.urnetwork volume automatically."
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
