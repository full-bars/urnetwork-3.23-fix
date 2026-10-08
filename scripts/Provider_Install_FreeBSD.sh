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

# --- service account resolution ---
#
# resolve_service_user prints the account the provider must run as. Order:
# an explicit override, then SUDO_USER (who actually invoked sudo; $USER is
# "root" there), then logname (the login session owner, which survives a
# plain `su -`), then the current uid. $USER alone is NOT trusted: it is
# unset under cron and is "root" for a plain root login.
resolve_service_user() {
    if [ -n "${SERVICE_USER_OVERRIDE:-}" ]; then
        printf '%s\n' "$SERVICE_USER_OVERRIDE"
        return
    fi
    if [ -n "${SUDO_USER:-}" ]; then
        printf '%s\n' "$SUDO_USER"
        return
    fi
    _lu="$(logname 2>/dev/null || true)"
    if [ -n "$_lu" ] && [ "$_lu" != "root" ]; then
        printf '%s\n' "$_lu"
        return
    fi
    id -un
}

# user_home prints a user's home directory from the passwd database. $HOME is
# not usable when running under sudo (it may still be root's), and the service
# user must be able to read the install and state directories.
user_home() {
    _h="$(getent passwd "$1" 2>/dev/null | cut -d: -f6)"
    if [ -z "$_h" ]; then
        _h="$(pw usershow "$1" 2>/dev/null | cut -d: -f9)"
    fi
    printf '%s\n' "${_h:-$HOME}"
}

# --- paths ---
#
# The binary lives in the SAME location the Go tool's discovery fallback looks
# (bsdProviderCandidates in internal/urnettools/discover_freebsd.go), so a
# stopped install is still discoverable and lifecycle commands have a target.
# Keep the two in step.

service_name="urnetwork"
rc_script="/usr/local/etc/rc.d/$service_name"

# Paths come from the SERVICE user's home, not $HOME. Under sudo $HOME may
# still be root's, and an install under /root would be unreadable by the
# account the provider actually runs as.
service_user="$(resolve_service_user)"
service_home="$(user_home "$service_user")"
install_path="$service_home/.local/share/urnetwork-provider"
provider_bin="$install_path/bin/urnetwork"
state_dir="$service_home/.urnetwork"
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

# install_wrapper_tool downloads this script as the urnet-tools stand-in.
# Staged to a temp file and renamed so a running tool is never truncated in
# place (ETXTBSY would otherwise break `update` while the tool is executing).
install_wrapper_tool() {
    _td="$1"
    curl -fsSL "$github_raw/scripts/Provider_Install_FreeBSD.sh" -o "$_td/urnet-tools.wrapper" 2>/dev/null || return 1
    chmod 755 "$_td/urnet-tools.wrapper"
    mv -f "$_td/urnet-tools.wrapper" "$install_path/bin/urnet-tools"
}

# --- rc.d script ---
#
# One template, emitted on stdout by write_rc_script_body so the root and
# non-root paths in do_install cannot drift apart. write_rc_script just wraps it
# into the file.

write_rc_script() {
    write_rc_script_body "${1:-$(resolve_service_user)}" > "$rc_script"
    chmod 755 "$rc_script"
}

# write_rc_script_body [user] emits the rc.d script on stdout so it can be piped
# to `sudo tee` without a second copy of the template drifting.
#
# $1 is the account the provider runs as. It defaults to the invoking user, but
# the caller must pass the resolved SUDO_USER-derived name: under sudo, $USER is
# commonly "root", and a service that runs the internet-facing relay as root is
# exactly what this must not produce.
#
# Paths are quoted in every command that interpolates them, because an install
# under a home directory containing a space would otherwise break `cd` and
# `su -c` at boot.
write_rc_script_body() {
    run_user="${1:-$(resolve_service_user)}"
    cat <<RCSCRIPT
#!/bin/sh
#
# PROVIDE: $service_name
# REQUIRE: LOGIN NETWORKING
# KEYWORD: shutdown
#
# rc.conf knobs:
#   ${service_name}_enable="YES"   start at boot (set by urnet-tools auto-start)
#   ${service_name}_user="$run_user"   run as this user
#   ${service_name}_flags=""       extra flags for the provider

. /etc/rc.subr

name="$service_name"
rcvar=${service_name}_enable

: \$${service_name}_enable="NO"
: \$${service_name}_user="$run_user"
: \$${service_name}_flags=""

load_rc_config \$name

: \${${service_name}_enable:="NO"}
: \${${service_name}_user:="$run_user"}
: \${${service_name}_flags:=""}

start_cmd="${service_name}_start"
stop_cmd="${service_name}_stop"

# The provider is long-lived, so start it through daemon(8). Running it in the
# foreground would make "service $service_name start" never return AND stall the
# boot sequence, because rc runs start synchronously.
#
# daemon(8) also settles two things a bare "su -m ... &" would not:
#   - -u sets HOME, USER and SHELL for the target user, so the provider's state
#     dir lands in that user's home rather than root's.
#   - -p writes the child pid through pidfile(3), which is what gives
#     "service $service_name onestatus" something real to check. Without it
#     status always reports "not running" and every start launches a duplicate.
daemon="/usr/sbin/daemon"
pidfile="/var/run/$service_name.pid"
logdir="/var/log/$service_name"

${service_name}_start()
{
    mkdir -p "\$logdir"
    chown \${${service_name}_user} "\$logdir"
    echo "Starting $service_name."
    \$daemon -f -t $service_name -p "\$pidfile" -o "\$logdir/stdout.log" -m 0 \
        -u \${${service_name}_user} \
        "$provider_bin" provide \$${service_name}_flags
}

${service_name}_stop()
{
    echo "Stopping $service_name."
    # Signal the CHILD, whose pid the pidfile holds. A "-f" pattern on the
    # binary path would also match urnet-tools, cron and anything else that
    # merely mentions the path, so the pidfile is the precise handle.
    if [ -f "\$pidfile" ]; then
        pid="\$(cat "\$pidfile")"
        kill "\$pid" 2>/dev/null || true
        for _ in 1 2 3 4 5 6 7 8 9 10; do
            [ -f "\$pidfile" ] || break
            sleep 1
        done
    fi
    pkill -TERM -x "$(basename "$provider_bin")" 2>/dev/null || true
    rm -f "\$pidfile"
}

run_rc_command "\$1"
RCSCRIPT
}

do_install() {
    version="${1:-latest}"

    # Refuse BEFORE downloading: an install that runs the internet-facing
    # relay as root is a privilege escalation the operator never asked for.
    # resolve_service_user only settles on root when the caller is root with
    # no sudo parent and names no override, so this never blocks a normal
    # `sudo install` (SUDO_USER is used) - only a genuine root login.
    if [ "$service_user" = "root" ]; then
        pr_err "refusing to install a service that runs as root"
        pr_err "re-run as a normal user (sudo is fine: SUDO_USER is used),"
        pr_err "or name an account: SERVICE_USER_OVERRIDE=<user> %s install" "$me"
        exit 1
    fi

    if [ "$version" = "latest" ]; then
        release_url="$github_api/releases/latest"
    else
        release_url="$github_api/releases/tags/$version"
    fi

    pr_info "Fetching release info..."
    release_json="$(curl -fsSL "$release_url" 2>/dev/null)"
    tag="$(echo "$release_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["tag_name"])' 2>/dev/null)"

    # GitHub API failed or rate-limited: for an explicit version we can trust
    # the caller's tag directly; for "latest" fall back to the dl.fullbars.xyz
    # Worker, which mirrors GitHub's latest-release tag at the edge.
    if [ -z "$tag" ]; then
        if [ "$version" != "latest" ]; then
            tag="$version"
        else
            pr_warn "Trying dl.fullbars.xyz fallback..."
            tag="$(curl -fsSL "https://dl.fullbars.xyz/latest-version" 2>/dev/null | tr -d '[:space:]')"
        fi
    fi

    if [ -z "$tag" ]; then
        pr_err "Could not fetch release info from %s" "$release_url"
        exit 1
    fi

    arch="$(uname -m)"
    case "$arch" in
        amd64|x86_64)  goarch="amd64" ;;
        arm64|aarch64) goarch="arm64" ;;
        *)             pr_err "Unsupported architecture: %s" "$arch"; exit 1 ;;
    esac

    tarball_url="https://dl.fullbars.xyz/releases/download/$tag/urnetwork-provider-$tag.tar.gz"
    mirror_url="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/$tag/urnetwork-provider-$tag.tar.gz"
    pr_info "Downloading %s..." "$tarball_url"

    tmpdir="$(mktemp -d)"
    if ! curl -fsSL "$tarball_url" -o "$tmpdir/provider.tar.gz"; then
        pr_warn "Primary download failed, trying GitHub mirror..."
        if ! curl -fsSL "$mirror_url" -o "$tmpdir/provider.tar.gz"; then
            pr_err "Failed to download from both primary and mirror"
            rm -rf "$tmpdir"
            exit 1
        fi
    fi

    tar -xzf "$tmpdir/provider.tar.gz" -C "$tmpdir" || {
        pr_err "Extract failed"
        rm -rf "$tmpdir"
        exit 1
    }

    provider_src="$tmpdir/freebsd/$goarch/provider"
    if [ ! -f "$provider_src" ]; then
        pr_err "FreeBSD/%s binary not found in release tarball" "$goarch"
        rm -rf "$tmpdir"
        exit 1
    fi

    mkdir -p "$install_path/bin"
    # `cp` onto a running binary fails with ETXTBSY ("Text file busy"), which
    # is exactly the `update` case: the provider is still executing the file
    # being replaced. Staging next to the target and renaming is atomic, is
    # allowed over a running image, and never leaves a half-written binary.
    staged="$provider_bin.new.$$"
    cp "$provider_src" "$staged" || { pr_err "Failed to stage binary"; rm -f "$staged"; rm -rf "$tmpdir"; exit 1; }
    chmod 755 "$staged"
    mv -f "$staged" "$provider_bin" || { pr_err "Failed to install binary"; rm -f "$staged"; rm -rf "$tmpdir"; exit 1; }

    echo "$tag" > "$install_path/version"

    # Install the tool: the Go urnet-tools binary shipped as a release asset,
    # digest-verified; fall back to this shell script for releases that
    # predate the Go asset.
    tool_asset="urnet-tools-freebsd-$goarch"
    # A release asset with no digest must read as ABSENT, not as the four
    # characters "null". jq prints the string null for a missing or null field,
    # which passed the -n test below and then failed the comparison, so every
    # install silently fell back to the shell wrapper while reporting success.
    # Select the asset first and require a real hex digest, so an unknown
    # digest means "cannot verify" and takes the same deliberate path.
    tool_digest=""
    release_json="$(curl -fsSL "$github_api/releases/tags/$tag" 2>/dev/null)"
    if command -v jq > /dev/null 2>&1; then
        tool_digest="$(printf '%s' "$release_json" | jq -r --arg a "$tool_asset" '
            (.assets[]? | select(.name == $a) | .digest)
            | select(type == "string")
            | sub("^sha256:"; "")
            | select(test("^[0-9a-fA-F]{64}$"))
        ' 2>/dev/null || true)"
    elif command -v python3 > /dev/null 2>&1; then
        tool_digest="$(printf '%s' "$release_json" | python3 -c '
import sys, json, re
try:
    doc = json.load(sys.stdin)
except Exception:
    sys.exit(0)
val = ""
for asset in doc.get("assets", []):
    if asset.get("name") == sys.argv[1]:
        val = asset.get("digest") or ""
        break
val = val.replace("sha256:", "")
print(val if re.fullmatch(r"[0-9a-fA-F]{64}", val) else "")
' "$tool_asset" 2>/dev/null || true)"
    fi

    if [ -n "$tool_digest" ]; then
        if curl -fsSL "https://github.com/full-bars/urnetwork-3.23-fix/releases/download/$tag/$tool_asset" -o "$tmpdir/$tool_asset" 2>/dev/null; then
            # FreeBSD ships sha256(1); fall back to openssl where it is absent.
            if command -v sha256 > /dev/null 2>&1; then
                actual="$(sha256 -q "$tmpdir/$tool_asset")"
            elif command -v openssl > /dev/null 2>&1; then
                actual="$(openssl dgst -sha256 "$tmpdir/$tool_asset" | awk '{print $NF}')"
            else
                actual=""
            fi
            if [ -n "$actual" ] && [ "$actual" = "$tool_digest" ]; then
                mv -f "$tmpdir/$tool_asset" "$install_path/bin/urnet-tools"
                chmod 755 "$install_path/bin/urnet-tools"
            else
                pr_warn "urnet-tools sha256 mismatch, falling back to shell wrapper"
                install_wrapper_tool "$tmpdir" || true
            fi
        else
            pr_warn "urnet-tools download failed, falling back to shell wrapper"
            install_wrapper_tool "$tmpdir" || true
        fi
    else
        install_wrapper_tool "$tmpdir" || true
    fi

    # The rc.d script and rc.conf are ROOT-owned; the files the SERVICE user
    # must read are not. Under sudo the download and copy ran as root, so hand
    # the install and state directories to the service user or the provider
    # starts as an account that cannot read its own binary.
    if [ "$(id -u)" = "0" ] && [ "$service_user" != "root" ]; then
        chown -R "$service_user" "$install_path" 2>/dev/null || true
        mkdir -p "$state_dir"
        chown -R "$service_user" "$state_dir" 2>/dev/null || true
    fi

    if [ "$(id -u)" != "0" ]; then
        pr_info "Installing the rc.d service requires root; re-running that step with sudo..."
        if command -v sudo > /dev/null 2>&1; then
            sudo mkdir -p "$(dirname "$rc_script")"
            sudo tee "$rc_script" > /dev/null <<RCSCRIPT
$(write_rc_script_body "$service_user")
RCSCRIPT
            sudo chmod 755 "$rc_script"
            sudo sysrc "$service_name"_enable=YES
            sudo sysrc "$service_name"_user="$service_user"
        else
            pr_warn "sudo not found. Install the service manually:"
            pr_warn "  sudo mkdir -p /usr/local/etc/rc.d"
            pr_warn "  sudo %s install-service" "$me"
        fi
    else
        mkdir -p "$(dirname "$rc_script")"
        write_rc_script_body "$service_user" > "$rc_script"
        chmod 755 "$rc_script"
        sysrc "$service_name"_enable=YES
        sysrc "$service_name"_user="$service_user"
    fi

    unload_service
    load_service

    if ! echo "$PATH" | tr ':' '\n' | grep -qxF "$install_path/bin"; then
        shell_rc="$HOME/.profile"
        case "$SHELL" in
            */zsh)  shell_rc="$HOME/.zshrc" ;;
            */bash) shell_rc="$HOME/.bash_profile" ;;
            */tcsh) shell_rc="$HOME/.cshrc" ;;
        esac
        if [ -n "$shell_rc" ]; then
            case "$SHELL" in
                */tcsh) echo "setenv PATH $install_path/bin:\$PATH" >> "$shell_rc" ;;
                *)      echo "export PATH=\"$install_path/bin:\$PATH\"" >> "$shell_rc" ;;
            esac
            pr_info "Added %s to PATH in %s" "$install_path/bin" "$shell_rc"
        fi
    fi

    rm -rf "$tmpdir"

    pr_info "URnetwork provider %s installed" "$tag"
    pr_info "  Binary:  %s" "$provider_bin"
    pr_info "  Service: %s" "$rc_script"
    pr_info "  Data:    %s" "$state_dir"
    pr_info ""
    pr_info "Commands:  urnet-tools start|stop|restart|status|hot-restart|session|proxy"
    pr_info "Restart your terminal or run 'hash -r' for urnet-tools to be found"
}

do_install_service() {
    if [ "$(id -u)" != "0" ]; then
        pr_err "install-service needs root: sudo %s install-service" "$me"
        exit 1
    fi
    # Running the provider as root because a bootstrap was run with sudo, or
    # because the box was logged into as root, would be a privilege
    # escalation the operator never asked for: refuse and say what to do.
    service_user="$(resolve_service_user)"
    if [ "$service_user" = "root" ]; then
        pr_err "refusing to install a service that runs as root"
        pr_err "re-run as a normal user, or set SERVICE_USER_OVERRIDE=<user>"
        exit 1
    fi
    mkdir -p "$(dirname "$rc_script")"
    write_rc_script "$service_user"
    sysrc "$service_name"_enable=YES
    sysrc "$service_name"_user="$service_user"
    pr_info "Service installed at %s (runs as %s)" "$rc_script" "$service_user"
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