#!/bin/sh
# urnet-tools: URnetwork provider manager (FreeBSD)
# Author: full-bars (GitHub), onlyinthe707 / "mesocyclone" (Discord)
# Based on: Ar Rakin, Ryan Mello (original)
# https://github.com/full-bars/urnetwork-3.23-fix

me="$(basename "$0")"

show_help() {
    echo "Usage: $me [options] <command>"
    echo ""
    echo "Core Commands:"
    echo "  install [<version>]      Download and install the provider + rc.d service"
    echo "  start                     Start the provider"
    echo "  stop                      Stop the provider"
    echo "  restart                   Restart the provider"
    echo "  status                    Show provider service status"
    echo "  update [<version>]        Upgrade to latest (or specified version)"
    echo "  version                   Show installed version"
    echo ""
    echo "Performance & Tuning:"
    echo "  hot-restart <on|off>      Reuse client JWT identities across restarts"
    echo "  self-heal [on|off]        Auto-regulate proxies (load gate + cleanup) (default: off)"
    echo ""
    echo "Session:"
    echo "  session save <file>       Export identity+proxy state (encrypted)"
    echo "  session load <file>       Import identity+proxy state, then restart"
    echo ""
    echo "Proxy Management:"
    echo "  proxy refresh             Re-read configs and hot-reload proxies"
    echo "  proxy remove-dead         Interactively prune dead/degraded/failing"
    echo "  proxy summary             Fleet summary (sources, health, counts)"
    echo ""
    echo "Options:"
    echo "  -h, --help                Show this help"
    echo "  -v, --version             Show version"
    echo "  -f, --force               Skip confirmation prompts"
    echo ""
    echo "https://github.com/full-bars/urnetwork-3.23-fix"
}

# --- helpers ---

pr_err() {
    fmt="$1"; shift
    printf "%s: $fmt\n" "$me" "$@" >&2
}

pr_info() {
    fmt="$1"; shift
    printf "%s: $fmt\n" "$me" "$@"
}

pr_warn() {
    fmt="$1"; shift
    printf "%s: $fmt\n" "$me" "$@" >&2
}

# --- paths ---
#
# The binary lives in the SAME location the Go tool's discovery fallback looks
# (bsdProviderCandidates in internal/urnettools/discover_freebsd.go), so a
# stopped install is still discoverable and lifecycle commands have a target.
# Keep the two in step.

install_path="$HOME/.local/share/urnetwork-provider"
provider_bin="$install_path/bin/urnetwork"
service_name="urnetwork"
rc_script="/usr/local/etc/rc.d/$service_name"
state_dir="$HOME/.urnetwork"
github_api="https://api.github.com/repos/full-bars/urnetwork-3.23-fix"
github_raw="https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main"

# --- rc.d ---

load_service() {
    service "$service_name" start 2>/dev/null
}

unload_service() {
    service "$service_name" stop 2>/dev/null
}

restart_service() {
    service "$service_name" restart 2>/dev/null
}

service_running() {
    service "$service_name" onestatus >/dev/null 2>&1
}

enable_service() {
    # sysrc is the supported editor for rc.conf: it rewrites the file
    # atomically and preserves formatting, which every other service on the
    # box also depends on. A direct append to /etc/rc.conf cannot.
    sysrc "$service_name"_enable=YES 2>/dev/null
}

# --- rc.d script ---
#
# One template, emitted on stdout by write_rc_script_body so the root and
# non-root paths in do_install cannot drift apart. write_rc_script just wraps it
# into the file.

write_rc_script() {
    write_rc_script_body > "$rc_script"
    chmod 755 "$rc_script"
}

# write_rc_script_body emits the rc.d script on stdout so it can be piped to
# `sudo tee` without a second copy of the template drifting.
write_rc_script_body() {
    cat <<RCSCRIPT
#!/bin/sh
#
# PROVIDE: $service_name
# REQUIRE: LOGIN NETWORKING
# KEYWORD: shutdown
#
# rc.conf knobs:
#   ${service_name}_enable="YES"   start at boot (set by urnet-tools auto-start)
#   ${service_name}_user="$USER"   run as this user
#   ${service_name}_flags=""       extra flags for the provider

. /etc/rc.subr

name="$service_name"
rcvar=${service_name}_enable

: \$${service_name}_enable="NO"
: \$${service_name}_user="$USER"
: \$${service_name}_flags=""

load_rc_config \$name

: \${${service_name}_enable:="NO"}
: \${${service_name}_user:="$USER"}
: \${${service_name}_flags:=""}

start_cmd="${service_name}_start"
stop_cmd="${service_name}_stop"

${service_name}_start()
{
    cd $HOME || return 1
    echo "Starting $service_name."
    su -m \${${service_name}_user} -c "$provider_bin provide \$${service_name}_flags"
}

${service_name}_stop()
{
    echo "Stopping $service_name."
    pkill -TERM -f $provider_bin
}

run_rc_command "\$1"
RCSCRIPT
}

do_install_service() {
    if [ "$(id -u)" != "0" ]; then
        pr_err "install-service needs root: sudo %s install-service" "$me"
        exit 1
    fi
    mkdir -p "$(dirname "$rc_script")"
    write_rc_script
    sysrc "$service_name"_enable=YES
    sysrc "$service_name"_user="$USER"
    pr_info "Service installed at %s" "$rc_script"
}

# --- runtime commands ---

do_start() {
    if service_running; then
        pr_info "Provider is already running"
        return
    fi
    load_service
    pr_info "Provider started"
}

do_stop() {
    unload_service
    pr_info "Provider stopped"
}

do_restart() {
    if [ "$FORCE" != "1" ]; then
        printf "Restart the provider? [y/N]: "
        read -r yn < /dev/tty
        case "$yn" in
            [Yy]*) ;;
            *) pr_info "Aborted."; exit 0 ;;
        esac
    fi
    restart_service
    pr_info "Provider restarted"
}

do_status() {
    if service_running; then
        pr_info "Provider is running"
    else
        pr_info "Provider is stopped"
    fi
}

do_version() {
    if [ -f "$install_path/version" ]; then
        cat "$install_path/version"
    else
        pr_err "Provider not installed"
        exit 1
    fi
}

do_update() {
    version="${1:-latest}"
    do_install "$version"
    pr_info "Update complete."
}

do_hot_restart() {
    mode="${1:-}"
    file="$state_dir/hot_restart"
    case "$mode" in
        on)    mkdir -p "$state_dir"; printf '%s\n' "on"  > "$file"; pr_info "hot-restart enabled" ;;
        off)   mkdir -p "$state_dir"; printf '%s\n' "off" > "$file"; pr_info "hot-restart disabled" ;;
        status|"")
            if [ -f "$file" ] && [ "$(cat "$file" 2>/dev/null)" = "on" ]; then
                pr_info "hot-restart: on"
            else
                pr_info "hot-restart: off"
            fi
            ;;
        *) pr_err "Usage: urnet-tools hot-restart <on|off|status>"; exit 1 ;;
    esac
}

do_session() {
    action="${1:-}"
    shift 2>/dev/null || true
    case "$action" in
        save)
            [ -n "${1:-}" ] || { pr_err "Usage: urnet-tools session save <file>"; exit 1; }
            mkdir -p "$state_dir"
            "$provider_bin" session-save "$1" || { pr_err "session save failed"; exit 1; }
            pr_info "Session saved to %s" "$1"
            ;;
        load)
            [ -n "${1:-}" ] || { pr_err "Usage: urnet-tools session load <file>"; exit 1; }
            "$provider_bin" session-load "$1" || { pr_err "session load failed"; exit 1; }
            pr_info "Session loaded; restarting"
            restart_service
            ;;
        *) pr_err "Usage: urnet-tools session <save|load> <file>"; exit 1 ;;
    esac
}

do_proxy() {
    action="${1:-}"
    case "$action" in
        refresh)     "$provider_bin" proxy-refresh ;;
        remove-dead) "$provider_bin" proxy-remove-dead ;;
        summary)     "$provider_bin" proxy-summary ;;
        *)           pr_err "Usage: urnet-tools proxy <refresh|remove-dead|summary>"; exit 1 ;;
    esac
}

# --- arg parsing ---

FORCE=0
while [ $# -gt 0 ]; do
    case "$1" in
        -h|--help)    show_help; exit 0 ;;
        -v|--version) do_version; exit 0 ;;
        -f|--force)   FORCE=1; shift ;;
        *)            break ;;
    esac
done

operation="${1:-}"
[ -n "$operation" ] && shift

case "$operation" in
    install)         do_install "$@" ;;
    install-service) do_install_service ;;
    update)          do_update "$@" ;;
    start)           do_start ;;
    stop)            do_stop ;;
    restart)         do_restart ;;
    status)          do_status ;;
    version)         do_version ;;
    hot-restart)     do_hot_restart "$@" ;;
    self-heal)
        file="$state_dir/proxy_self_heal"
        case "${1:-}" in
            on) mkdir -p "$state_dir"; printf '%s\n' "on" > "$file"; pr_info "Self-heal enabled" ;;
            off) mkdir -p "$state_dir"; printf '%s\n' "off" > "$file"; pr_info "Self-heal disabled" ;;
            status|"")
                if [ -f "$file" ] && [ "$(cat "$file" 2>/dev/null)" = "on" ]; then
                    pr_info "self-heal: on"
                else
                    pr_info "self-heal: off"
                fi
                if [ -f "$state_dir/pressure_status" ]; then
                    if command -v jq >/dev/null 2>&1; then
                        pr_info "$(jq -r '"pressure: \(.score) (target_pool=\(.target_pool), updated=\(.updated))"' \
                            "$state_dir/pressure_status" 2>/dev/null)"
                    else
                        cat "$state_dir/pressure_status"
                    fi
                fi
                ;;
            *) pr_err "Usage: urnet-tools self-heal [on|off|status]"; exit 1 ;;
        esac
        ;;
    session)         do_session "$@" ;;
    proxy)           do_proxy "$@" ;;
    auth)            "$provider_bin" auth "$@" ;;
    logs)            "$provider_bin" logs "$@" ;;
    "")
        show_help
        exit 0
        ;;
    *)
        pr_err "Unknown command: %s" "$operation"
        echo "Run '$me --help' for usage."
        exit 1
        ;;
esac