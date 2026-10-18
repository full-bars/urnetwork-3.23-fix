#!/bin/sh
# provider_env.sh: map the v33 operator environment variables onto
# `provider provide` flags for the Docker start scripts.
#
# Sourced by start_stable.sh, start_nightly.sh and start_jwt.sh. Every
# variable is optional. With none of them set, provider_env_flags prints
# nothing, so the provide command line is exactly the legacy `provide`.
#
# The flags are spelled as in the v33 usage (provider/main.go):
#   --all-operators, --auto-register, --hotkey_seed_file=<file>,
#   --allow-client-registration, --max-memory=<mem>, --port=<port>, -v/-vv.
#
#   ALL_OPERATORS=Y|N|1|0|true|false|yes|no|on|off
#       provide --all-operators: one child provider per listed operator.
#   AUTO_REGISTER=Y|N|1|0|true|false|yes|no|on|off
#       With ALL_OPERATORS only: sign in to each operator with the hotkey.
#       Needs HOTKEY_SEED_FILE.
#   HOTKEY_SEED_FILE=<path>
#       With ALL_OPERATORS only: the 32-byte hotkey seed the supervisor signs
#       in with. The path must not contain whitespace.
#   ALLOW_CLIENT_REGISTRATION=auto|Y|N
#       Y always adds --allow-client-registration, N never does, and auto
#       adds it only when the state directory has neither .provider.key nor
#       an operators/ directory (a fresh identity).
#   MAX_MEMORY=<mem>
#       --max-memory=<mem>, for example 2gib. Must not contain whitespace.
#   VERBOSE=0|1|2
#       --verbosity for provide: 0 adds nothing, 1 adds -v, 2 adds -vv
#       (the binary counts -v...). Accepts the techroy23 spellings.
#   PROVIDER_PORT=<port>
#       --port=<port>, the status server port. 0 adds nothing, which is the
#       binary's own default (status server disabled).
#   PROVIDER_ARGS="<args>"
#       Extra arguments appended to `provide`, split on whitespace. Use this
#       for flags that have no dedicated variable, for example
#       --operators-url=... or --operators-refresh=30m.
#
# techroy23/Docker-UrNetwork compatibility aliases (see
# docs/Docker-Deployment.md): API_URL/CONNECT_URL alias onto
# UR_API_URL/UR_CONNECT_URL for the start scripts' func_choose_network, and
# VERBOSE/PROVIDER_PORT map onto provide flags below. The UR_ names win when
# both are set. API_URL/CONNECT_URL are ignored under ALL_OPERATORS, like the
# source image, because each operator uses its own endpoints.

# Print 1 for a true value, 0 for a false or empty value, and return 2 for
# anything else so callers can fail on a typo instead of silently changing
# the command line. Accepts the techroy23 spellings (Y/N) as well as the
# v33 ones (1/0, true/false, yes/no, on/off).
provider_env_bool() {
  case "$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')" in
    ""|n|no|false|0|off) printf '0\n' ;;
    y|yes|true|1|on)     printf '1\n' ;;
    *) return 2 ;;
  esac
}

# True when ALL_OPERATORS asks for provide --all-operators.
provider_env_all_operators() {
  [ "$(provider_env_bool "${ALL_OPERATORS:-}")" = "1" ]
}

# True when ALLOW_CLIENT_REGISTRATION=auto should add the flag: the state
# directory has no existing identity (.provider.key) and no operators/ tree.
provider_env_auto_allow_client_registration() {
  _pe_state_dir="${1:-${HOME:-/root}/.urnetwork}"
  [ ! -f "$_pe_state_dir/.provider.key" ] && [ ! -d "$_pe_state_dir/operators" ]
}

_provider_env_error() {
  if command -v log >/dev/null 2>&1; then
    log "[ERROR] $*" >&2
  else
    printf 'ERROR: %s\n' "$*" >&2
  fi
}

# Print the extra `provider provide` arguments, one per line. Returns non-zero
# with an ERROR line on stderr when the environment is inconsistent.
provider_env_flags() {
  _pe_state_dir="${1:-${HOME:-/root}/.urnetwork}"

  if ! _pe_all="$(provider_env_bool "${ALL_OPERATORS:-}")"; then
    _provider_env_error "ALL_OPERATORS must be Y/N, 1/0, true/false, yes/no or on/off (got '${ALL_OPERATORS}')"
    return 1
  fi
  if ! _pe_auto="$(provider_env_bool "${AUTO_REGISTER:-}")"; then
    _provider_env_error "AUTO_REGISTER must be Y/N, 1/0, true/false, yes/no or on/off (got '${AUTO_REGISTER}')"
    return 1
  fi

  _pe_hotkey="${HOTKEY_SEED_FILE:-}"
  case "$_pe_hotkey" in
    *[[:space:]]*)
      _provider_env_error "HOTKEY_SEED_FILE must not contain whitespace (got '$_pe_hotkey')"
      return 1
      ;;
  esac

  # --hotkey_seed_file and --auto-register exist only in the
  # `provide --all-operators` usage. Refuse them on plain provide rather
  # than emit flags the binary would reject.
  if [ "$_pe_all" != "1" ]; then
    if [ -n "$_pe_hotkey" ]; then
      _provider_env_error "HOTKEY_SEED_FILE requires ALL_OPERATORS=1 (the flag exists only in provide --all-operators)"
      return 1
    fi
    if [ "$_pe_auto" = "1" ]; then
      _provider_env_error "AUTO_REGISTER requires ALL_OPERATORS=1 (the flag exists only in provide --all-operators)"
      return 1
    fi
  fi

  if [ "$_pe_all" = "1" ]; then
    printf '%s\n' --all-operators
    if [ "$_pe_auto" = "1" ]; then
      if [ -z "$_pe_hotkey" ]; then
        _provider_env_error "AUTO_REGISTER needs HOTKEY_SEED_FILE"
        return 1
      fi
      printf '%s\n' --auto-register
    fi
    [ -z "$_pe_hotkey" ] || printf '%s\n' "--hotkey_seed_file=$_pe_hotkey"
  fi

  _pe_max_memory="${MAX_MEMORY:-}"
  case "$_pe_max_memory" in
    *[[:space:]]*)
      _provider_env_error "MAX_MEMORY must not contain whitespace (got '$_pe_max_memory')"
      return 1
      ;;
  esac
  [ -z "$_pe_max_memory" ] || printf '%s\n' "--max-memory=$_pe_max_memory"

  _pe_verbose="$(printf '%s' "${VERBOSE:-}" | tr '[:upper:]' '[:lower:]')"
  case "$_pe_verbose" in
    ""|0|no|false|off)
      ;;
    1|y|yes|true|on|v|-v)
      printf '%s\n' -v
      ;;
    2|vv|-vv)
      printf '%s\n' -vv
      ;;
    *)
      _provider_env_error "VERBOSE must be 0 (off), 1 (-v) or 2 (-vv) (got '${VERBOSE}')"
      return 1
      ;;
  esac

  _pe_port="${PROVIDER_PORT:-}"
  case "$_pe_port" in
    "")
      ;;
    *[!0-9]*)
      _provider_env_error "PROVIDER_PORT must be a number between 0 and 65535 (got '$_pe_port')"
      return 1
      ;;
    *)
      _pe_port_num="$_pe_port"
      while [ "${#_pe_port_num}" -gt 1 ] && [ "${_pe_port_num#0}" != "$_pe_port_num" ]; do
        _pe_port_num="${_pe_port_num#0}"
      done
      if [ "${#_pe_port_num}" -gt 5 ] || [ "$_pe_port_num" -gt 65535 ]; then
        _provider_env_error "PROVIDER_PORT must be a number between 0 and 65535 (got '$_pe_port')"
        return 1
      fi
      # 0 means the status server stays off, which is also the binary default.
      [ "$_pe_port_num" = "0" ] || printf '%s\n' "--port=$_pe_port_num"
      ;;
  esac

  _pe_allow="$(printf '%s' "${ALLOW_CLIENT_REGISTRATION:-}" | tr '[:upper:]' '[:lower:]')"
  case "$_pe_allow" in
    ""|n|no|false|0)
      ;;
    y|yes|true|1)
      printf '%s\n' --allow-client-registration
      ;;
    auto)
      provider_env_auto_allow_client_registration "$_pe_state_dir" \
        && printf '%s\n' --allow-client-registration
      ;;
    *)
      _provider_env_error "ALLOW_CLIENT_REGISTRATION must be auto, Y or N (got '${ALLOW_CLIENT_REGISTRATION}')"
      return 1
      ;;
  esac

  if [ -n "${PROVIDER_ARGS:-}" ]; then
    case $- in *f*) _pe_glob_off=1 ;; *) _pe_glob_off=0 ;; esac
    set -f
    # shellcheck disable=SC2086  # PROVIDER_ARGS is deliberately split on whitespace
    printf '%s\n' ${PROVIDER_ARGS}
    [ "$_pe_glob_off" = "1" ] || set +f
  fi
}

# --- techroy23/Docker-UrNetwork aliases (applied at source time) ------------
# Their image names the custom endpoints API_URL/CONNECT_URL and passes
# --api_url/--connect_url to each command; ours chooses the network once via
# `provider choose_network` from UR_API_URL/UR_CONNECT_URL. Alias the pair
# here, before the start scripts run func_choose_network (which reads the UR_
# names), so their both-or-neither validation still applies. The UR_ names
# win when both are set, and the pair is ignored when ALL_OPERATORS is on,
# exactly like the source image.
provider_env_warn() {
  if command -v log >/dev/null 2>&1; then
    log "[WARN] $*" >&2
  else
    printf 'WARN: %s\n' "$*" >&2
  fi
}

if [ -n "${API_URL:-}" ] || [ -n "${CONNECT_URL:-}" ]; then
  if [ "$(provider_env_bool "${ALL_OPERATORS:-}")" = "1" ]; then
    provider_env_warn "ALL_OPERATORS is on: ignoring API_URL/CONNECT_URL, each operator uses its own endpoints"
  else
    if [ -z "${UR_API_URL:-}" ] && [ -n "${API_URL:-}" ]; then
      UR_API_URL="$API_URL"
      export UR_API_URL
    fi
    if [ -z "${UR_CONNECT_URL:-}" ] && [ -n "${CONNECT_URL:-}" ]; then
      UR_CONNECT_URL="$CONNECT_URL"
      export UR_CONNECT_URL
    fi
  fi
fi
