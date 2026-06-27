#!/bin/sh
# Credits: Ar Rakin, Ryan Mello
# v3.23-fix fork & customizations: full-bars (GitHub), mesocyclone (Discord)
# urnet-tools -- URnetwork manager script (also acts as an installation script)
# GitHub: <https://github.com/full-bars/urnetwork-3.23-fix>

me="$0"
script_rundir="$(pwd)"

if [ "$me" = "sh" ] || [ "$me" = "bash" ] || [ "$me" = "zsh" ] || [ "$URNETWORK_TOOLS_MODE" = "1" ]; then
    me="urnet-tools"
fi

show_help ()
{
    echo "Usage: $me [options] <command>"
    echo ""
    echo "Core Commands:"
    echo "  start                   Start URnetwork provider"
    echo "  stop                    Stop URnetwork provider"
    echo "  restart                 Restart URnetwork provider"
    echo "  status                  Show the status of URnetwork provider service"
    echo "  update                  Upgrade URnetwork to the latest version"
    echo "  logs [all|dump|-i]      Stream provider logs ('all' full RAM, 'dump' to ~/urlogs.txt, '-i'/'--important' high-value lines only)"
    echo ""
    echo "Performance & Tuning:"
    echo "  turbo <v4|v8|off>       🚀 RAISE throughput limits for RAM-rich boxes"
    echo "  auto <on|off>           🧠 AUTO-TUNE: detect hardware and pick best profile"
    echo "  eco <on|off>            🌿 ECO MODE: GC-tuned for low-RAM systems"
    echo "  lowmode <on|off>        LOW-MEMORY: reduced buffers for max RAM savings"
    echo "  ramlogs <on|off>        RAM LOGS: zero disk I/O logging"
    echo "  optimize                ⚡ OPTIMIZE: apply Golden Fleet OS/kernel limits"
    echo ""
    echo "Proxy Management:"
    echo "  auth [<code>]           🔑 Authenticate with an auth code (omit for interactive paste)"
    echo "  proxy add <file>        🌐 ADD: bulk add proxies from a text file"
    echo "  proxy clear             🗑️  CLEAR: remove all configured proxies"
    echo "  proxy health            ❤️  HEALTH: show dead/degraded proxies + live event log"
    echo "  proxy traffic           📈 TRAFFIC: show real-time bandwidth & client session load"
    echo "  proxy refresh           🔄 REFRESH: gracefully drop all connections and force a proxy reload"
    echo "  proxy summary           📊 Show proxy fleet summary (sources, health, counts)"
    echo "  proxy remove-dead       💀 CLEANUP: interactively remove dead proxies from your config"
    echo "  report [<url>|off]      📡 Show or set hub report URL ('report off' to disable)"
    echo "  fast-auth [on|off]      ⚡ Bypass auth rate limiter without restart (takes effect immediately)"
    echo "  set [<key> [<val>|off]] ⚙️  Show or change runtime tuning overrides (no restart needed)"
    echo "  hub set <http://host:port>  Configure this node to report to a hub (writes systemd override)"
    echo "  hub off                 Stop reporting to hub (removes override, restarts provider)"
    echo "  hub install             Download and install the hub binary as a systemd user service"
    echo ""
    echo "Maintenance:"
    echo "  reinstall               Reinstall URnetwork"
    echo "  uninstall               Uninstall URnetwork"
    echo "  auto-update             Manage auto update settings (daily/weekly/monthly)"
    echo "  auto-start              Toggle auto-start on login"
    echo ""
    echo "Global Options:"
    echo "  -h, --help              Show this help and exit"
    echo "  -v, --version           Show running and installed versions"
    echo "  -t, --tag=TAG           Use a specific version for (re)install"
    echo "  -i, --install=[PATH]    Specify custom installation path"
    echo "  -4, --ipv4              Force IPv4 for network requests"
    echo "  -f, --force             Force optimization / skip confirmation prompts"
    echo "  -B, --no-modify-bashrc  Do not modify ~/.bashrc during install"
    echo ""
    echo "Refer to <https://github.com/full-bars/urnetwork-3.23-fix> for more help."
}

get_arch ()
{
    if command -v arch > /dev/null; then
        arch="$(arch)"
    else
        arch="$(uname -m)"
    fi

    case "$arch" in
        i386|i686)
            arch=386
            ;;

        x86_64)
            arch=amd64
            ;;

        aarch64)
            arch=arm64
            ;;
    esac

    echo "$arch"
}

operation=""
arch="$(get_arch)"
has_systemd=0
no_modify_bashrc=0
update_timer_oncalendar="Sun *-*-* 00:00:00 UTC"

api_base="https://api.github.com/repos/full-bars/urnetwork-3.23-fix"

install_path="$HOME/.local/share/urnetwork-provider"
version_file="$install_path/.version"

# Canonical URL for re-running this installer in a freshly created user's context.
# Overridable via URNET_INSTALL_URL (e.g. to test a branch before it lands on main).
urnet_install_url="${URNET_INSTALL_URL:-https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/Provider_Install_Linux.sh}"

# If no operation is specified:
# - Default to 'install' if running as a one-off installer (curl | sh)
# - Show help if running as the installed 'urnet-tools' command
if [ -z "$operation" ] && [ -z "$URNETWORK_TOOLS_MODE" ]; then
    operation="install"
fi

if command -v systemctl > /dev/null; then
    has_systemd=1
fi

pr_err ()
{
    argv0="$me"
    fmt="$1"
    shift

    if [ -t 2 ]; then
        argv0="\033[1m$me\033[0m"
    fi

    if [ -n "$operation" ]; then
        argv0="$argv0: $operation"
    fi

    # shellcheck disable=SC2059
    if [ $# -eq 0 ]; then
        printf "%b: %s\n" "$argv0" "$fmt" >&2
    else
        printf "%b: $fmt\n" "$argv0" "$@" >&2
    fi
}

pr_info ()
{
    argv0="$me"
    fmt="$1"
    shift

    if [ -t 1 ]; then
        argv0="\033[1m$me\033[0m"
    fi

    if [ -n "$operation" ]; then
        argv0="$argv0: $operation"
    fi

    # shellcheck disable=SC2059
    if [ $# -eq 0 ]; then
        printf "%b: %s\n" "$argv0" "$fmt"
    else
        printf "%b: $fmt\n" "$argv0" "$@"
    fi
}

pr_warn ()
{
    argv0="\033[1;33m$me\033[0m"
    fmt="$1"
    shift

    if [ -n "$operation" ]; then
        argv0="$argv0: $operation"
    fi

    # shellcheck disable=SC2059
    if [ $# -eq 0 ]; then
        printf "%b: %s\n" "$argv0" "$fmt"
    else
        printf "%b: $fmt\n" "$argv0" "$@"
    fi
}

opt_requires_arg ()
{
    pr_err "Option '%s' requires an argument" "$1"
    pr_err "Try '$me --help' for more information"
}

get_version_from_api_response () 
{    
    if command -v jq > /dev/null; then
        latest_version="$(printf "%s" "$1" | tr -d '\000-\037' | jq -r '.tag_name' 2>/dev/null)"
    elif command -v python3 > /dev/null; then
        latest_version="$(printf "%s" "$1" | tr -d '\000-\037' | python3 -c 'import sys, json;
try:
    data = json.load(sys.stdin)
    print(data["tag_name"])
except (json.JSONDecodeError, KeyError):
    print("")
' 2>/dev/null)"
    else
        pr_err "Neither python3 nor jq is available"
        exit 1
    fi

    echo "$latest_version"
}

# shellcheck disable=SC2317
get_release_date_from_api_response () 
{   
    if command -v jq > /dev/null; then
        date="$(printf "%s" "$1" | tr -d '\000-\037' | jq -r '.published_at | fromdateiso8601' 2>/dev/null)"
    elif command -v python3 > /dev/null; then
        date="$(printf "%s" "$1" | tr -d '\000-\037' | python3 -c 'import sys, json;
from datetime import datetime, timezone
try:
    data = json.load(sys.stdin)
    iso_str = data["published_at"].replace("Z", "+00:00")
    print(int(datetime.fromisoformat(iso_str).timestamp() * 1000))
except (json.JSONDecodeError, KeyError, ValueError):
    print("")
' 2>/dev/null)"
    else
        pr_err "Neither python3 nor jq is available"
        exit 1
    fi

    echo "$date"
}

# shellcheck disable=SC2317
get_current_date () 
{
    if command -v jq > /dev/null; then
        date="$(jq -n 'now * 1000 | floor')"
    elif command -v python3 > /dev/null; then
        date="$(python3 -c 'from datetime import datetime, timezone; now = datetime.now(timezone.utc); print(int(now.timestamp() * 1000));')"
    else
        pr_err "Neither python3 nor jq is available"
        exit 1
    fi

    echo "$date"
}

FORCE_IPV4=0
FORCE=0

network_fetch ()
{
    url="$1"
    
    # Try curl first
    if command -v curl > /dev/null; then
        # --retry 3: retry on transient errors (500, 502, 503, 504, 408)
        # --retry-connrefused: retry even if connection is refused
        # --retry-delay 2: wait 2 seconds between retries
        opts="--connect-timeout 10 --retry 3 --retry-delay 2 --retry-connrefused -fSsL"
        if [ "$FORCE_IPV4" -eq 1 ]; then
            opts="-4 $opts"
        fi
        
        # We use a temporary file to capture output because $(...) can strip trailing newlines
        # and we want to separate stdout from curl's exit code clearly.
        if result=$(curl $opts "$url" 2>/dev/null); then
            printf "%s" "$result"
            return 0
        fi
    fi

    # Try wget fallback
    if command -v wget > /dev/null; then
        # --tries=3: retry up to 3 times
        # --waitretry=2: wait 2 seconds between retries
        # --retry-connrefused: retry even if connection is refused
        opts="--connect-timeout=10 --tries=3 --waitretry=2 --retry-connrefused -qO-"
        if [ "$FORCE_IPV4" -eq 1 ]; then
            opts="-4 $opts"
        fi
        
        if result=$(wget $opts "$url" 2>/dev/null); then
            printf "%s" "$result"
            return 0
        fi
    fi

    return 1
}

show_version ()
{
    # Report two facts that can legitimately differ, and used to be conflated:
    #   1. the version installed on disk ($install_path/bin/urnetwork), and
    #   2. the version of the provider process actually running right now.
    # A plain (non-forced) `update` swaps the on-disk binary but deliberately
    # leaves the running provider untouched (the restart is left to the
    # operator), so the two drift until the service is restarted. Reading only
    # the on-disk binary — as this used to — reports the *replacement*, never
    # what is serving traffic, which is actively misleading right after an
    # update. The running version is resolved through /proc/<pid>/exe, the
    # kernel's record of the exact image a process is executing, which stays
    # accurate even after the on-disk file is renamed (to .old) or deleted.
    provider_bin="$install_path/bin/urnetwork"
    file_version=""
    disk_version=""

    if [ -f "$version_file" ]; then
        file_version="$(cat "$version_file" 2>/dev/null)"
    fi

    if [ -x "$provider_bin" ]; then
        disk_version="$("$provider_bin" --version 2>/dev/null | head -n 1)"
    fi

    # Resolve the running provider's PID: prefer the systemd unit (user, then
    # system), and fall back to matching the launch path — argv keeps the
    # original "$provider_bin provide" string even after the file is renamed.
    running_pid="$(systemctl --user show -p MainPID --value urnetwork.service 2>/dev/null)"
    if [ -z "$running_pid" ] || [ "$running_pid" = "0" ]; then
        running_pid="$(systemctl show -p MainPID --value urnetwork.service 2>/dev/null)"
    fi
    if [ -z "$running_pid" ] || [ "$running_pid" = "0" ]; then
        running_pid="$(pgrep -f "$provider_bin provide" 2>/dev/null | head -n 1)"
    fi

    run_version=""
    run_exe=""
    if [ -n "$running_pid" ] && [ "$running_pid" != "0" ] && [ -e "/proc/$running_pid/exe" ]; then
        run_exe="$(readlink "/proc/$running_pid/exe" 2>/dev/null)"
        run_version="$("/proc/$running_pid/exe" --version 2>/dev/null | head -n 1)"
    fi

    # Self-heal a stale .version file from whatever the on-disk binary reports,
    # so the installer's own upgrade comparisons (which read .version) stay
    # accurate. This tracks the on-disk binary, not the running one.
    if [ -n "$disk_version" ] && [ -n "$file_version" ] && [ "$file_version" != "$disk_version" ]; then
        if echo "$disk_version" > "$version_file" 2>/dev/null; then
            pr_info "Corrected stale version file: %s -> %s" "$file_version" "$disk_version"
        else
            pr_warn "Version file is stale (%s, should be %s) but could not be updated" "$file_version" "$disk_version"
        fi
    fi

    # On-disk version for display + the latest-release comparison below, with a
    # version-file fallback if the binary itself is not runnable.
    version="$disk_version"
    disk_note=""
    if [ -z "$version" ] && [ -n "$file_version" ]; then
        version="$file_version"
        disk_note=" (from version file; binary not queryable, may be stale)"
    fi
    if [ -z "$version" ] && [ -z "$run_version" ]; then
        pr_err "Could not determine installed version: binary '%s' is not runnable and no version file exists" "$provider_bin"
        exit 1
    fi

    # Flag drift: the running image was replaced on disk (renamed/deleted), or
    # the two reported versions simply disagree.
    stale=0
    case "$run_exe" in
        *"(deleted)"*|*.old) stale=1 ;;
    esac
    if [ -n "$run_version" ] && [ -n "$version" ] && [ "$run_version" != "$version" ]; then
        stale=1
    fi

    if [ -n "$run_version" ]; then
        echo "Running version:   $run_version (PID $running_pid)"
    elif [ -n "$running_pid" ] && [ "$running_pid" != "0" ]; then
        echo "Running version:   unknown (PID $running_pid; binary not queryable)"
    else
        echo "Running version:   provider is not running"
    fi
    echo "Installed on disk: $version$disk_note"

    if [ "$stale" -eq 1 ]; then
        pr_warn "A newer/different binary is installed on disk than the one currently running."
        pr_warn "Restart the service to apply it: systemctl --user restart urnetwork.service"
    fi

    api_url="$api_base/releases/latest"
    release="$(network_fetch "$api_url" 2>/dev/null || true)"
    latest_version="$(get_version_from_api_response "$release" 2>/dev/null)"

    if [ -z "$latest_version" ]; then
        if command -v curl > /dev/null; then
            tag_url=$(curl -Ls -o /dev/null -w %{url_effective} "https://github.com/full-bars/urnetwork-3.23-fix/releases/latest")
            if [ -n "$tag_url" ] && [ "$tag_url" != "https://github.com/full-bars/urnetwork-3.23-fix/releases/latest" ]; then
                latest_version="${tag_url##*/}"
            fi
        fi
    fi

    if [ -z "$latest_version" ]; then
        pr_err "Could not fetch any information about the latest release"
        exit 1
    fi

    if [ "$latest_version" != "$version" ]; then
        echo "Latest version (Update available): $latest_version"
    fi
}

tag="latest"

while [ $# -gt 0 ]; do
    case "$1" in
        -h|--help)
            show_help
            exit 0
            ;;

        -v|--version)
            show_version
            exit 0
            ;;

        -t|--tag)
            if [ -z "$2" ]; then
                opt_requires_arg "$1"
                exit 1
            fi

            tag="$2"

            if [ "$tag" != "latest" ] && [ "$(echo "$tag" | cut -c -1)" != "v" ]; then
                tag="v$tag"
            fi

            shift 2
            ;;

        -i|--install)
            if [ -z "$2" ]; then
                opt_requires_arg "$1"
                exit 1
            fi

            install_path="$2"
            shift 2
            ;;

        -f|--force)
            FORCE=1
            shift
            ;;

        -4|--ipv4)
            FORCE_IPV4=1
            shift
            ;;

        -B|--no-modify-bashrc)
            no_modify_bashrc=1
            shift
            ;;

        --)
            shift
            break
            ;;

        -*)
            pr_warn "Ignoring unknown option '%s'" "$1"
            shift
            ;;

        *)
            break
            ;;
    esac
done

if [ -n "$1" ]; then
    operation="$1"
    shift
fi

if [ -z "$operation" ]; then
    show_help >&2
    exit 1
fi

systemd_userdir="$HOME/.config/systemd/user"
systemd_service="$systemd_userdir/urnetwork.service"
systemd_update_service="$systemd_userdir/urnetwork-update.service"
systemd_update_timer="$systemd_userdir/urnetwork-update.timer"
systemd_units_stopped=0

stop_systemd_units ()
{
    if [ -f "$systemd_service" ]; then
        if [ "$(systemctl --user is-active urnetwork.service)" = "active" ]; then
            if [ "$operation" = "update" ] && [ "$FORCE" != "1" ]; then
                pr_info "urnetwork.service is running — binary will be updated on disk."
                pr_info "Restart the service when convenient to apply the update: systemctl --user restart urnetwork.service"
                systemd_units_stopped=0

                systemctl --user disable --now urnetwork-update.timer || {
                    pr_err "Failed to disable urnetwork-update.timer before update; continuing anyway"
                }
                return
            fi

            if [ "$FORCE" != "1" ]; then
                confirm_restart "Upgrading or reinstalling requires temporarily stopping the URNetwork provider to safely swap the core binary."
            fi

            pr_info "urnetwork.service is running — it will be stopped for the update and restarted automatically once finished."
            systemd_units_stopped=1
        fi

        systemctl --user disable --now urnetwork.service || {
            pr_err "Failed to disable urnetwork.service; continuing anyway"
        }

        systemctl --user disable --now urnetwork-update.timer || {
            pr_err "Failed to disable urnetwork-update.timer; continuing anyway"
        }
    fi
}

install_systemd_units ()
{
    start="$systemd_units_stopped"

    pr_info "Installing urnetwork.service in %s" "$systemd_service"
    mkdir -p "$systemd_userdir"
    
    cat > "$systemd_service" <<EOF
[Unit]
Description=URnetwork Provider

[Service]
Environment="HOST_HOSTNAME=$(hostname)"
ExecStart=$install_path/bin/urnetwork provide
Restart=no

[Install]
WantedBy=default.target
EOF
    
    pr_info "Installing urnetwork-update.service in %s" "$systemd_update_service"
    cat > "$systemd_update_service" <<EOF
[Unit]
Description=URnetwork Update

[Service]
Type=oneshot
ExecStart=$install_path/bin/urnet-tools update
EOF
    
    pr_info "Installing urnetwork-update.timer in %s" "$systemd_update_timer"
    cat > "$systemd_update_timer" <<EOF
[Unit]
Description=Run URnetwork Update

[Timer]
OnCalendar=$update_timer_oncalendar
Persistent=true

[Install]
WantedBy=default.target
EOF

    if ! systemctl --user enable urnetwork.service 2>/dev/null; then
        if [ "$(id -u)" -eq 0 ]; then
            pr_warn "Running as root: user systemd service skipped (requires user session bus). Use Docker 'restart: unless-stopped' or manually run the provider binary."
        else
            pr_err "Could not enable the newly installed systemd service"
            exit 1
        fi
    fi

    if ! systemctl --user enable --now urnetwork-update.timer 2>/dev/null; then
        if [ "$(id -u)" -ne 0 ]; then
            pr_err "Could not enable the newly installed update timer"
            exit 1
        fi
    fi

    if [ "$start" -eq 1 ]; then
        systemctl --user daemon-reload
        
        if ! systemctl --user start urnetwork.service; then
            pr_err "warning: Unable to restart urnetwork.service after update; please manually start it"
        fi
    fi
}

# Run a command string as another user with HOME and the per-user systemd bus
# reachable. Prefers 'runuser', which works on SELinux-enforcing hosts where a
# root 'su' is denied ("failed to execute shell: Permission denied"); falls back
# to 'su' only when runuser is unavailable. Usage: func_run_as_user USER "CMD".
func_run_as_user ()
{
    _ru_user="$1"; shift
    _ru_home="$(getent passwd "$_ru_user" | cut -d: -f6)"; [ -n "$_ru_home" ] || _ru_home="/home/$_ru_user"
    _ru_uid="$(id -u "$_ru_user")"; _ru_rt="/run/user/$_ru_uid"
    if command -v runuser >/dev/null 2>&1; then
        # runuser preserves the caller's cwd (often /root, which the target user
        # cannot enter), so move to the user's home first.
        runuser -u "$_ru_user" -- env HOME="$_ru_home" USER="$_ru_user" LOGNAME="$_ru_user" \
            XDG_RUNTIME_DIR="$_ru_rt" DBUS_SESSION_BUS_ADDRESS="unix:path=$_ru_rt/bus" \
            sh -c "cd \"$_ru_home\" 2>/dev/null || cd /tmp; $*"
    else
        su - "$_ru_user" -c "export XDG_RUNTIME_DIR='$_ru_rt'; export DBUS_SESSION_BUS_ADDRESS='unix:path=$_ru_rt/bus'; $*"
    fi
}

# Distro-agnostic creation of a dedicated unprivileged user, then finish the
# install in that user's systemd context. Called only from the interactive
# root path; exits the script.
func_assisted_user_setup ()
{
    printf "Username to create [urnet]: "
    read -r newuser < /dev/tty
    [ -n "$newuser" ] || newuser="urnet"
    case "$newuser" in
        *[!a-z0-9_-]*) pr_err "Invalid username '%s' (use lowercase letters, digits, '-' or '_')." "$newuser"; exit 1 ;;
    esac

    if id "$newuser" >/dev/null 2>&1; then
        pr_info "User '%s' already exists; reusing it." "$newuser"
    elif command -v useradd >/dev/null 2>&1; then
        useradd -m -s /bin/sh "$newuser" || { pr_err "useradd failed for '%s'." "$newuser"; exit 1; }
        pr_info "Created user '%s'." "$newuser"
    elif command -v adduser >/dev/null 2>&1; then
        adduser -D "$newuser" 2>/dev/null || adduser "$newuser" || { pr_err "adduser failed for '%s'." "$newuser"; exit 1; }
        pr_info "Created user '%s'." "$newuser"
    else
        pr_err "No 'useradd' or 'adduser' found. Create a user manually and re-run as them."
        exit 1
    fi

    # Grant admin group membership so the user can later run 'urnet-tools optimize'.
    for grp in sudo wheel; do
        if getent group "$grp" >/dev/null 2>&1; then
            if command -v usermod >/dev/null 2>&1; then
                usermod -aG "$grp" "$newuser" 2>/dev/null && pr_info "Added '%s' to group '%s'." "$newuser" "$grp"
            elif command -v addgroup >/dev/null 2>&1; then
                addgroup "$newuser" "$grp" 2>/dev/null && pr_info "Added '%s' to group '%s'." "$newuser" "$grp"
            fi
            break
        fi
    done

    uid="$(id -u "$newuser")"
    runtime="/run/user/$uid"

    # Lingering starts the user's systemd + D-Bus without a login session.
    if command -v loginctl >/dev/null 2>&1; then
        loginctl enable-linger "$newuser" 2>/dev/null || pr_warn "enable-linger failed; the service may stop on logout."
    else
        pr_warn "'loginctl' not found; cannot enable lingering automatically."
    fi

    # Wait for the per-user bus socket to appear (logind creates it asynchronously).
    i=0
    while [ ! -S "$runtime/bus" ] && [ "$i" -lt 15 ]; do
        sleep 1
        i=$((i + 1))
    done

    if [ -S "$runtime/bus" ]; then
        pr_info "Finishing the install as '%s'..." "$newuser"
        func_run_as_user "$newuser" "curl -fSsL '$urnet_install_url' | sh" || true

        if func_run_as_user "$newuser" "systemctl --user is-enabled urnetwork.service >/dev/null 2>&1"; then
            pr_info "Done. The provider is installed as a user service under '%s'." "$newuser"
            exit 0
        fi
        pr_warn "Could not confirm the user service from this session."
    else
        pr_warn "The user session bus for '%s' did not come up in time." "$newuser"
    fi

    # Bulletproof fallback: a real login session always sets up the bus correctly.
    pr_info "Log in as '%s' (a fresh SSH or console session) and run:" "$newuser"
    pr_info "    curl -fSsL %s | sh" "$urnet_install_url"
    exit 0
}

# When run as root, the user-level systemd service cannot be set up (root has no
# user session bus). Guide the operator rather than silently leaving a broken
# install. Non-systemd hosts (e.g. containers) are unaffected and proceed.
func_root_guard ()
{
    [ "$(id -u)" -eq 0 ] || return 0
    [ "$has_systemd" -eq 1 ] || return 0

    pr_warn "You are running the installer as root."
    pr_info "The provider runs as a user-level systemd service (systemctl --user), which"
    pr_info "needs a session bus that root lacks. As root the service cannot be enabled"
    pr_info "and you will hit 'Failed to connect to bus' errors."

    # Non-interactive (curl | sh with no terminal): cannot prompt. Preserve the
    # documented root tools-only path, but warn loudly and show the fix.
    if [ ! -r /dev/tty ]; then
        pr_warn "No interactive terminal; continuing with tools only (no service)."
        pr_info "To run the provider service, create a user and install as them:"
        pr_info "    useradd -m -s /bin/sh urnet && loginctl enable-linger urnet"
        pr_info "    su - urnet -c 'curl -fSsL %s | sh'" "$urnet_install_url"
        return 0
    fi

    printf "\n  1) Create a dedicated user now and finish as them (recommended)\n"
    printf "  2) Show the manual steps and exit\n"
    printf "  3) Continue as root (install tools only, no service)\n"
    printf "Choose [1/2/3]: "
    read -r choice < /dev/tty
    printf "\n"

    case "$choice" in
        1) func_assisted_user_setup ;;
        2)
            pr_info "Run these as your normal (non-root) user:"
            pr_info "    sudo useradd -m -s /bin/sh urnet && sudo loginctl enable-linger urnet"
            pr_info "    su - urnet -c 'curl -fSsL %s | sh'" "$urnet_install_url"
            exit 0
            ;;
        3) pr_warn "Continuing as root: tools only, the user service will be skipped." ;;
        *) pr_err "Invalid choice '%s'." "$choice"; exit 1 ;;
    esac
}

download_asset ()
{
    url="$1"
    output="$2"
    
    # Try curl first
    if command -v curl > /dev/null; then
        # --retry 3: retry on transient errors (500, 502, 503, 504, 408)
        # --retry-connrefused: retry even if connection is refused
        # --retry-delay 2: wait 2 seconds between retries
        opts="--progress-bar --connect-timeout 10 --retry 3 --retry-delay 2 --retry-connrefused -fL"
        if [ "$FORCE_IPV4" -eq 1 ]; then
            opts="-4 $opts"
        fi
        
        if curl $opts "$url" -o "$output"; then
            return 0
        fi
        pr_warn "curl failed to download asset, trying wget fallback..."
    fi

    # Try wget fallback
    if command -v wget > /dev/null; then
        # --tries=3: retry up to 3 times
        # --waitretry=2: wait 2 seconds between retries
        # --retry-connrefused: retry even if connection is refused on modern wget
        opts="--connect-timeout=10 --tries=3 --waitretry=2"
        if [ "$FORCE_IPV4" -eq 1 ]; then
            opts="-4 $opts"
        fi
        
        # Check if wget supports --retry-connrefused
        if wget --help | grep -q "retry-connrefused"; then
            opts="$opts --retry-connrefused"
        fi

        if wget $opts -O "$output" "$url"; then
            return 0
        fi
    fi

    return 1
}

do_install ()
{
    : "${tag:=latest}"
    : "${no_modify_bashrc:=0}"
    original_operation="$operation"

    # Dependency Check
    if ! command -v curl > /dev/null && ! command -v wget > /dev/null; then
        pr_err "Neither 'curl' nor 'wget' is available. One of these is required for downloads."
        exit 1
    fi

    if ! command -v jq > /dev/null && ! command -v python3 > /dev/null; then
        pr_err "Neither 'jq' nor 'python3' is available. One of these is required for JSON parsing."
        exit 1
    fi

    case "$operation" in
        install)
            if [ -n "$URNETWORK_TOOLS_MODE" ]; then
                pr_err "Invalid operation '%s'" "$operation"
                exit 1
            fi

            func_root_guard

            while [ $# -gt 0 ]; do
                case "$1" in
                    -t|--tag)
                        if [ -z "$2" ]; then
                            opt_requires_arg "$1"
                            exit 1
                        fi

                        tag="$2"

                        if [ "$tag" != "latest" ] && [ "$(echo "$tag" | cut -c -1)" != "v" ]; then
                            tag="v$tag"
                        fi 

                        shift 2
                        ;;

                    -4|--ipv4)
                        FORCE_IPV4=1
                        shift
                        ;;

                    -B|--no_modify_bashrc)
                        no_modify_bashrc=1
                        shift
                        ;;

                    -*)
                        pr_err "Invalid option '%s'" "$1"
                        exit 1
                        ;;

                    *)
                        pr_err "Invalid argument '%s'" "$1"
                        exit 1
                        ;;
                esac
            done

            ;;

        update)
            no_modify_bashrc=1
            force_update=0

            while [ $# -gt 0 ]; do
                case "$1" in
                    -f|--force)
                        force_update=1
                        FORCE=1
                        shift
                        ;;

                    -t|--tag)
                        if [ -z "$2" ]; then
                            opt_requires_arg "$1"
                            exit 1
                        fi

                        tag="$2"

                        if [ "$tag" != "latest" ] && [ "$(echo "$tag" | cut -c -1)" != "v" ]; then
                            tag="v$tag"
                        fi

                        shift 2
                        ;;

                    -*)
                        pr_warn "Ignoring unknown option '%s'" "$1"
                        shift
                        ;;

                    *)
                        pr_warn "Ignoring unknown argument '%s'" "$1"
                        shift
                        ;;
                esac
            done

            ;;

        reinstall)
            if [ ! -f "$version_file" ]; then
                pr_err "Could not determine the currently installed version"
                exit 1
            fi

	   		tag="$(cat "$version_file")"

            while [ $# -gt 0 ]; do
                case "$1" in
                    -t|--tag)
                        if [ -z "$2" ]; then
                            opt_requires_arg "$1"
                            exit 1
                        fi

                        tag="$2"

                        if [ "$tag" != "latest" ] && [ "$(echo "$tag" | cut -c -1)" != "v" ]; then
                            tag="v$tag"
                        fi 

                        shift 2
                        ;;

                    -4|--ipv4)
                        FORCE_IPV4=1
                        shift
                        ;;

                    -B|--no_modify_bashrc)
                        no_modify_bashrc=1
                        shift
                        ;;

                    -*)
                        pr_err "Invalid option '%s'" "$1"
                        exit 1
                        ;;

                    *)
                        pr_err "Invalid argument '%s'" "$1"
                        exit 1
                        ;;
                esac
            done
            ;;
    esac

    api_url=""

    if [ "$tag" = "latest" ] || [ -z "$tag" ]; then
        tag=latest
        api_url="$api_base/releases/latest"
    else
        api_url="$api_base/releases/tags/$tag"
    fi

    pr_info "Fetching release information for tag: %s" "$tag"

    release="$(network_fetch "$api_url" 2>/dev/null || true)"

    version_to_install="$(get_version_from_api_response "$release" 2>&1)"
    release_date="$(get_release_date_from_api_response "$release" 2>&1)"

    # If tag was "latest" and API failed to provide a version, try the redirect trick
    if [ "$tag" = "latest" ] && [ -z "$version_to_install" ]; then
        if command -v curl > /dev/null; then
            tag_url=$(curl -Ls -o /dev/null -w %{url_effective} "https://github.com/full-bars/urnetwork-3.23-fix/releases/latest")
            if [ -n "$tag_url" ] && [ "$tag_url" != "https://github.com/full-bars/urnetwork-3.23-fix/releases/latest" ]; then
                version_to_install="${tag_url##*/}"
            fi
        fi
    fi

    # If API failed and a specific tag was requested, just use the requested tag
    if [ "$tag" != "latest" ] && [ -z "$version_to_install" ]; then
        version_to_install="$tag"
    fi

    if [ -z "$version_to_install" ]; then
        pr_err "Failed to fetch release information for tag: %s" "$tag"
        exit 1
    fi

    # Resolve tag to the actual version for subsequent asset URL logic
    if [ "$tag" = "latest" ] && [ -n "$version_to_install" ]; then
        tag="$version_to_install"
    fi

    if [ "$operation" = "update" ] && [ -f "$install_path/.date" ] && [ -f "$install_path/.version" ]; then
        install_release_date="$(cat "$install_path/.date")"
        installed_version="$(cat "$install_path/.version")"

        # If release_date could not be parsed, fall back to version string comparison
        if [ -z "$release_date" ]; then
            pr_info "Unable to parse release date from API; using version string comparison"
            if [ "$force_update" = "1" ]; then
                pr_info "Force flag enabled, reinstalling version %s" "$version_to_install"
            elif [ "$version_to_install" != "$installed_version" ]; then
                pr_info "Version %s differs from installed version %s, continuing upgrade" "$version_to_install" "$installed_version"
            else
                pr_info "Installed version %s is already up-to-date" "$installed_version"
                exit 0
            fi
        else
            # Validate dates are numeric before comparison
            if ! printf '%s' "$install_release_date" | grep -qE '^[0-9]+$' || ! printf '%s' "$release_date" | grep -qE '^[0-9]+$'; then
                pr_info "Invalid date format from API; using version string comparison"
                if [ "$force_update" = "1" ]; then
                    pr_info "Force flag enabled, reinstalling version %s" "$version_to_install"
                elif [ "$version_to_install" != "$installed_version" ]; then
                    pr_info "Version %s differs from installed version %s, continuing upgrade" "$version_to_install" "$installed_version"
                else
                    pr_info "Installed version %s is already up-to-date" "$installed_version"
                    exit 0
                fi
            else
                if [ "$force_update" = "1" ]; then
                    pr_info "Force flag enabled, reinstalling version %s" "$version_to_install"
                elif [ "$install_release_date" -lt "$release_date" ]; then
                    pr_info "Version %s is newer than the installed version %s" "$version_to_install" "$installed_version"
                    pr_info "Continuing upgrade"
                else
                    pr_info "Installed version is up-to-date"
                    exit 0
                fi
            fi
        fi
    fi

    # Construct download URL directly from GitHub release pattern
    # instead of parsing potentially malformed API JSON.
    # We MUST have a real tag name here; "latest" is not a valid download tag.
    if [ "$tag" = "latest" ]; then
        pr_err "Could not resolve 'latest' tag to a specific version. GitHub API might be unreachable."
        exit 1
    fi
    dl_url="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/$tag/urnetwork-provider-$tag.tar.gz"
    
    pr_info "Downloading: %s" "$dl_url"
    
    if ! workdir="$(mktemp -d)"; then
        pr_err "Failed to create working directory"
        exit 1
    fi
    
    cd "$workdir" || exit 1

    tarball="$workdir/urnetwork.tar.gz"
    bindir="$workdir/linux/$arch"
    bin_program="$bindir/provider"

    trap 'rm -r "$workdir"' EXIT 
    trap 'exit 1' INT TERM

    if [ -z "$URNETWORK_NO_DOWNLOAD_TARBALL" ]; then
        if ! download_asset "$dl_url" "$tarball"; then
            pr_err "Failed to download $dl_url after multiple attempts and fallbacks"
            exit 1
        fi

        if ! tar -xf "$tarball" 2>/dev/null; then
            pr_err "Failed to extract tarball: %s" "$tarball"
            exit 1
        fi

        if [ ! -f "$bin_program" ]; then
            pr_err "Provider binary was not found in the tarball!"
            pr_err "This indicates an issue with the tarball that was downloaded."
            exit 1
        fi
    fi
	
    if [ "$has_systemd" -eq 1 ]; then
        stop_systemd_units
    fi

    if [ -d "$install_path" ] && [ "$operation" = "install" ]; then
        pr_info "Found existing installation in $install_path, updating instead"
        operation=update
        no_modify_bashrc=1
    else
        if [ ! -d "$install_path" ]; then
            pr_info "Creating directory '%s'" "$install_path"

            if ! mkdir -p "$install_path"; then
                pr_err "Failed to create directory '%s'" "$install_path"
                exit 1
            fi
        fi

        if ! mkdir -p "$install_path/bin"; then
            pr_err "Failed to create directory '%s'" "$install_path/bin"
            exit 1
        fi
    fi

    if [ -z "$URNETWORK_NO_DOWNLOAD_TARBALL" ]; then
        # Bypass 'Text file busy' locks on running binaries by moving the active inode out of the way first
        mv -f "$install_path/bin/urnetwork" "$install_path/bin/urnetwork.old" 2>/dev/null || true
        cp "$bin_program" "$install_path/bin/urnetwork" || { pr_err "Failed to install provider binary"; exit 1; }
        chmod 755 "$install_path/bin/urnetwork" || { pr_err "Failed to install provider binary"; exit 1; }
        rm -f "$install_path/bin/urnetwork.old" 2>/dev/null || true
    fi

    cd "$script_rundir" || exit 1

    # Priority: tarball-bundled script > GitHub fetch > running script
    if [ -f "$workdir/urnet-tools" ]; then
        script="$(cat "$workdir/urnet-tools" 2>/dev/null)"
    fi

    if [ -z "$script" ]; then
        if [ "$original_operation" = "update" ] || [ "$original_operation" = "reinstall" ]; then
            pr_info "Fetching latest urnet-tools from GitHub..."
            if ! script="$(network_fetch "$urnet_install_url")"; then
                pr_err "Failed to fetch latest urnet-tools from GitHub, using current version"
                script="$(cat "$0" 2>/dev/null)"
            fi
        fi
    fi

    if [ -z "$script" ]; then
        script="$(cat "$0" 2>/dev/null)"
        if [ -z "$script" ]; then
            pr_info "Fetching urnet-tools from GitHub..."
            script="$(network_fetch "$urnet_install_url")"
        fi
    fi

    cd "$workdir" || exit 1

    if [ -f "urnet-tools" ]; then
        script_override="$(cat "urnet-tools" 2>/dev/null)"
        if [ -n "$script_override" ]; then
            script="$script_override"
        fi
    fi

    if [ -z "$script" ]; then
        pr_err "Invalid script contents"
        exit 1
    fi

    rm -f "$install_path/bin/urnet-tools"
    printf "%s\n" "$script" | head -n1 > "$install_path/bin/urnet-tools"

    {
        echo "URNETWORK_TOOLS_MODE=1"; 
    } >> "$install_path/bin/urnet-tools"

    printf "%s\n" "$script" | tail -n +2 >> "$install_path/bin/urnet-tools"
    chmod 755 "$install_path/bin/urnet-tools" || { pr_err "Failed to install urnet-tools"; exit 1; }

    # Overwrite with the tarball-bundled script if available, so even an old
    # script that just wrote itself via cat "$0" gets replaced by the latest.
    if [ -f "$workdir/urnet-tools" ]; then
        cp "$workdir/urnet-tools" "$install_path/bin/urnet-tools" 2>/dev/null
    fi

    echo "$version_to_install" > "$install_path/.version"
    echo "$release_date" > "$install_path/.date"

    if [ "$has_systemd" -eq 1 ]; then
        install_systemd_units
    fi

    if [ "$no_modify_bashrc" -eq 0 ]; then
	if awk '/^[[:space:]]*# == urnetwork-provider start[[:space:]]*$/ { code=1; } END { exit code; }' "$HOME/.bashrc"; then
	    pr_info "Adding '%s' to ~/.bashrc" "$install_path/bin"
            cat >> "$HOME/.bashrc" <<EOF

# == urnetwork-provider start
export URNETWORK_PROVIDER_INSTALL="$install_path"
export PATH="\$PATH:\$URNETWORK_PROVIDER_INSTALL/bin"
# == urnetwork-provider end
EOF
	else
	    pr_info "~/.bashrc is up-to-date"
	fi
    fi

    case "$operation" in
        install)
            pr_info "Installation complete (Systemd check: %s)" "$has_systemd"
            printf "\n"
            printf "\e[1;32mCustom Build Improvements:\e[0m\n"
            printf " - Logs: [net][s]select promoted to INFO (High-signal monitoring).\n"
            printf " - Throughput: InitialContractTransferByteCount increased to 256 KiB.\n"
            printf " - Stability: IP Buffer Depth quadrupled to 256 for burst protection.\n"
            printf " - Scaling: Accordion TCP window scaling (4KB idle -> 1MB active).\n"
            printf "\n"
            printf "Reload shell:          \e[1msource ~/.bashrc\e[0m           # or restart your terminal\n"
            printf "First run:             \e[1murnetwork auth\e[0m             # auth code can be found at <https://ur.io>\n"
            printf "Start:                 \e[1murnetwork provide\e[0m          # in foreground\n"

            if [ "$has_systemd" -eq 1 ]; then
                printf "Start service:         \e[1msystemctl --user start urnetwork\e[0m\n"
                printf "Disable service:       \e[1msystemctl --user disable urnetwork\e[0m\n"
                printf "Disable auto-updates:  \e[1msystemctl --user disable urnetwork-update.timer\e[0m\n"
                
                
                printf "\n"
                printf "\e[1;33mNote:\e[0m Run \e[1murnet-tools optimize\e[0m to enable systemd lingering and apply OS-level optimizations.\n"
                printf "This ensures the provider keeps running in the background after you log out.\n"
                printf "\n"
                printf "\e[1mRefer to <https://docs.ur.io/provider#linux-and-macos> for more detailed instructions.\e[0m\n"
            fi
            ;;

        reinstall)
            pr_info "Reinstallation successful"
            ;;

        update)
            pr_info "Updated successfully"
            ;;
    esac
}

do_uninstall ()
{
    no_modify_bashrc=0

    while [ $# -gt 0 ]; do
        case "$1" in
            -B|--no_modify_bashrc)
                no_modify_bashrc=1
                shift
                ;;

            -*)
                pr_err "Invalid option '%s'" "$1"
                exit 1
                ;;

            *)
                pr_err "Invalid argument '%s'" "$1"
                exit 1
                ;;
        esac
    done

    if [ ! -d "$install_path" ]; then
        pr_err "Directory '%s' could not be found, are you sure you have URnetwork installed?" "$install_path"
        exit 1
    fi

    pr_info "Removing: %s" "$install_path"
    
    if ! rm -r "$install_path"; then
        pr_err "Failed to completely remove '%s'" "$install_path"
        exit 1
    fi

    pr_info "Removing: %s" "$HOME/.urnetwork"
    rm -rf "$HOME/.urnetwork"

    if [ "$has_systemd" -eq 1 ]; then
        pr_info "Removing systemd unit files"
        systemctl --user disable --now urnetwork.service
        systemctl --user disable --now urnetwork-update.timer
        rm -f "$HOME/.config/systemd/user/urnetwork.service"
        rm -f "$HOME/.config/systemd/user/urnetwork-update.service"
        rm -f "$HOME/.config/systemd/user/urnetwork-update.timer"
    fi

    if [ "$no_modify_bashrc" -eq 0 ]; then
        if command -v awk > /dev/null; then
            pr_info "Removing PATH exports from ~/.bashrc"
            cp "$HOME/.bashrc" "$HOME/.bashrc.backup.old"
            awk '/# == urnetwork-provider start/ { pr=1 } pr == 0 { print } /# == urnetwork-provider end/ { pr=0 }' "$HOME/.bashrc" > "$HOME/.bashrc.new"
            mv "$HOME/.bashrc.new" "$HOME/.bashrc"
        else
            pr_err "warning: awk not found, cannot update ~/.bashrc"
            pr_err "Please manually remove PATH exports from your ~/.bashrc"
        fi
    fi

    pr_info "Uninstallation successful"
}

change_auto_update_prefs ()
{
    mode=""
    interval="weekly"

    while [ $# -gt 0 ]; do
        case "$1" in
            --interval)
                if [ -z "$2" ]; then
                    opt_requires_arg "$1"
                    exit 1
                fi

                if [ "$2" != "daily" ] && [ "$2" != "weekly" ] && [ "$2" != "monthly" ]; then
                    pr_err "Invalid update interval '%s': Must be one of these: daily, weekly, monthly" "$1"
                    exit 1
                fi
                
                interval="$2"
                shift 2
                ;;

            -*)
                pr_err "Invalid option '%s'" "$1"
                exit 1
                ;;

            *)
                if [ -n "$mode" ]; then
                    pr_err "Unexpected argument '%s'" "$1"
                    exit 1
                fi

                if [ "$1" != "on" ] && [ "$1" != "off" ]; then
                    pr_err "Invalid argument '%s': Must be either 'on' or 'off'" "$1"
                    exit 1
                fi

                mode="$1"
                shift
                ;;
        esac
    done

    if [ "$has_systemd" -eq 0 ]; then
        pr_err "This system doesn't seem to have systemd"
        exit 1
    fi

    state="$(systemctl --user is-enabled urnetwork-update.timer)"

    if [ -z "$mode" ]; then
        pr_info "Auto update state: $state"
        exit 0
    fi

    case "$mode" in
        on)
            pr_info "Updating systemd unit files"

            case "$interval" in
                daily)   new_calendar="daily" ;;
                weekly)  new_calendar="Sun *-*-* 00:00:00 UTC" ;;
                monthly) new_calendar="monthly" ;;
            esac

            if ! sed -e "s|^OnCalendar=.*|OnCalendar=$new_calendar|" -i "$HOME/.config/systemd/user/urnetwork-update.timer"; then
                pr_err "Failed to update auto update interval: sed substitution failed"
                exit 1
            fi

            pr_info "Executing \`systemctl --user daemon-reload'"

            if ! systemctl --user daemon-reload; then
                pr_err "Failed to turn on auto updates: systemctl daemon reload failed"
                exit 1
            fi

            pr_info "Executing \`systemctl --user enable --now urnetwork-update.timer'"

            if ! systemctl --user enable --now urnetwork-update.timer; then
                pr_err "Failed to turn on auto updates: systemctl command failed"
                exit 1
            fi
            ;;

        off)
            pr_info "Executing \`systemctl --user disable --now urnetwork-update.timer'"

            if ! systemctl --user disable --now urnetwork-update.timer; then
                pr_err "Failed to turn off auto updates: systemctl command failed"
                exit 1
            fi
            ;;
    esac
}

toggle_auto_start ()
{
	if test -z "$1"; then
		pr_err "Must provide an argument: Either 'on' or 'off'"
		exit 1
	fi

	if test "$1" != on && test "$1" != off; then
		pr_err "Invalid value: %s, must be either on or off" "$1"
		exit 1
	fi

	if test "$1" = on; then
		if systemctl --user is-enabled --quiet urnetwork.service; then
			pr_info "urnetwork.service is already enabled on login"
			exit 0
	    else
			pr_info "Enabling urnetwork.service (on login)"
			systemctl --user enable urnetwork.service
	    fi
	else
		if ! systemctl --user is-enabled --quiet urnetwork.service; then
			pr_info "urnetwork.service is already disabled"
			exit 0
	    else
			pr_info "Disabling urnetwork.service"
			systemctl --user disable urnetwork.service
	    fi
	fi
}

do_start ()
{
    if ! systemctl --user is-active --quiet urnetwork.service; then
		pr_info "Starting urnetwork.service"
		systemctl --user start urnetwork.service || { pr_err "Failed to start urnetwork.service"; exit 1; }
    else
		pr_info "Service urnetwork.service is already active"
		exit 1
    fi
}

do_stop ()
{
    if systemctl --user is-active --quiet urnetwork.service; then
		pr_info "Stopping urnetwork.service"
		systemctl --user stop urnetwork.service || { pr_err "Failed to stop urnetwork.service"; exit 1; }
    else
		pr_info "Service urnetwork.service is not active"
		exit 1
    fi
}

confirm_restart ()
{
    action="${1:-Executing this command will trigger a full restart of the URNetwork provider.}"
    if [ "$FORCE" = "1" ]; then
        return 0
    fi
    printf "\n\e[1;31m🛑 WARNING: This Command Triggers a Cold Restart 🛑\e[0m\n"
    printf "\e[1m%s\n" "$action"
    printf "This will instantly drop all active connections and reset your proxy warmup state (which typically requires a minimum of 8-12 hours).\e[0m\n\n"
    
    while true; do
        printf "\e[1mAre you absolutely sure you want to proceed and restart the provider? (y/N): \e[0m"
        read -r yn < /dev/tty
        case $yn in
            [Yy]* ) return 0;;
            [Nn]* | "" ) pr_info "Command aborted. Provider was NOT restarted and your warmup is safe."; exit 1;;
            * ) echo "Please answer yes or no.";;
        esac
    done
}

do_restart ()
{
    confirm_restart
    pr_info "Restarting urnetwork.service..."
    systemctl --user restart urnetwork.service || { pr_err "Failed to restart urnetwork.service"; exit 1; }
    pr_info "Service successfully restarted."
}

show_status ()
{
	systemctl --user status urnetwork.service
}

show_logs ()
{
    mode="$1"

    # The "important" buffer is a separate small /dev/shm file holding only
    # high-value lines (profit/earn/health/outage/evictions), so the earnings
    # signal survives for hours even when the main ramlog floods.
    if [ "$mode" = "important" ] || [ "$mode" = "--important" ] || [ "$mode" = "-i" ]; then
        if [ ! -f "/dev/shm/urnetwork-important.log" ]; then
            pr_err "Important log not found. Is the provider running with RAM logs (urnet-tools ramlogs on)?"
            exit 1
        fi
        pr_info "Streaming high-value lines (/dev/shm/urnetwork-important.log)"
        tail -n 1000 -f /dev/shm/urnetwork-important.log
        return
    fi

    override_dir="$HOME/.config/systemd/user/urnetwork.service.d"
    is_ramlog=0
    if [ -d "$override_dir" ]; then
        if grep -q -s "URNETWORK_PROFILE=lowmem" "$override_dir"/*.conf || grep -q -s "URNETWORK_PROFILE=eco" "$override_dir"/*.conf || grep -q -s "URNETWORK_RAMLOGS=1" "$override_dir"/*.conf; then
            is_ramlog=1
        fi
    fi

    if [ "$is_ramlog" -eq 1 ]; then
        pr_info "Streaming from RAM disk (/dev/shm/urnetwork.log)"
        if [ ! -f "/dev/shm/urnetwork.log" ]; then
            pr_err "Log file not found. Is the provider running?"
            exit 1
        fi
        if [ "$mode" = "full" ] || [ "$mode" = "all" ]; then
            tail -n +1 -f /dev/shm/urnetwork.log
        elif [ "$mode" = "dump" ]; then
            cp /dev/shm/urnetwork.log "$HOME/urlogs.txt"
            pr_info "Logs successfully dumped to $HOME/urlogs.txt"
            exit 0
        else
            tail -n 1000 -f /dev/shm/urnetwork.log
        fi
    else
        if [ "$mode" = "dump" ]; then
            journalctl --user -u urnetwork.service > "$HOME/urlogs.txt"
            pr_info "Logs successfully dumped to $HOME/urlogs.txt"
            exit 0
        fi
        pr_info "Streaming from journald"
        journalctl --user -fu urnetwork.service
    fi
}

# override_set_env KEY VALUE
# Idempotently sets Environment="KEY=VALUE" in the systemd override.conf.
# Removes any existing line with the same KEY before appending, so running
# this multiple times never creates duplicates. Creates the file with a
# [Service] header if it doesn't exist.
override_set_env() {
    local key="$1" value="$2"
    local override_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/urnetwork.service.d"
    local override_file="$override_dir/override.conf"
    mkdir -p "$override_dir"
    if [ ! -f "$override_file" ]; then
        printf '[Service]\n' > "$override_file"
    fi
    # Remove any existing line with this key (matches any value after =)
    sed -i '/^Environment="'"$key"'=/d' "$override_file"
    printf 'Environment="%s=%s"\n' "$key" "$value" >> "$override_file"
}

# override_rm_env KEY
# Removes Environment="KEY=..." from override.conf. Cleans up empty files.
override_rm_env() {
    local key="$1"
    local override_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/urnetwork.service.d"
    local override_file="$override_dir/override.conf"
    if [ -f "$override_file" ]; then
        sed -i '/^Environment="'"$key"'=/d' "$override_file"
        # If file only has [Service] header, remove it entirely
        if [ "$(grep -cvE '(^\[)|(^$)' "$override_file")" -eq 0 ]; then
            rm -f "$override_file"
            rmdir "$override_dir" 2>/dev/null || true
        fi
    fi
}

toggle_ramlogs ()
{
    mode="$1"
    override_dir="$HOME/.config/systemd/user/urnetwork.service.d"
    override_file="$override_dir/override.conf"

    case "$mode" in
        on)
            confirm_restart "Enabling RAM logging requires restarting the URNetwork provider."
            pr_info "Enabling RAM logging..."
            override_set_env "URNETWORK_RAMLOGS" "1"
            systemctl --user daemon-reload
            systemctl --user restart urnetwork.service
            pr_info "RAM logging enabled and service restarted."
            ;;
        off)
            confirm_restart "Disabling RAM logging requires restarting the URNetwork provider."
            pr_info "Disabling RAM logging..."
            override_rm_env "URNETWORK_RAMLOGS"
            systemctl --user daemon-reload
            systemctl --user restart urnetwork.service
            pr_info "RAM logging disabled and service restarted."
            ;;
        *)
            pr_err "Usage: urnet-tools ramlogs <on|off>"
            exit 1
            ;;
    esac
}

# Returns the effective RAM ceiling in MiB.
# Checks cgroup v2, cgroup v1, then /proc/meminfo MemTotal.
detect_mem_limit_mib ()
{
    # cgroup v2 (Docker with --memory, modern systemd slices)
    if [ -f /sys/fs/cgroup/memory.max ]; then
        cg_val=$(cat /sys/fs/cgroup/memory.max 2>/dev/null)
        if [ -n "$cg_val" ] && [ "$cg_val" != "max" ]; then
            echo $(( cg_val / 1024 / 1024 ))
            return
        fi
    fi
    # cgroup v1 — sentinel for "no limit" is near max int64; ignore anything >= 1 TiB
    if [ -f /sys/fs/cgroup/memory/memory.limit_in_bytes ]; then
        cg_val=$(cat /sys/fs/cgroup/memory/memory.limit_in_bytes 2>/dev/null)
        if [ -n "$cg_val" ] && [ "$cg_val" -lt 1099511627776 ] 2>/dev/null; then
            echo $(( cg_val / 1024 / 1024 ))
            return
        fi
    fi
    # Fall back to MemTotal
    total_ram_kb=$(awk '/MemTotal/ {print $2}' /proc/meminfo 2>/dev/null)
    if [ -n "$total_ram_kb" ]; then
        echo $(( total_ram_kb / 1024 ))
        return
    fi
    echo 850
}

toggle_lowmode ()
{
    mode="$1"
    case "$mode" in
        on)
            confirm_restart "Enabling lowmode requires restarting the URNetwork provider."
            pr_info "Enabling lowmode..."
            ram_mib=$(detect_mem_limit_mib)
            gomem_mib=$(( ram_mib * 85 / 100 ))
            pr_info "Dynamic GOMEMLIMIT set to ${gomem_mib}MiB (85%% of ${ram_mib}MiB detected RAM)"
            override_set_env "URNETWORK_PROFILE" "lowmem"
            override_set_env "GOMEMLIMIT" "${gomem_mib}MiB"
            override_set_env "GOGC" "50"
            systemctl --user daemon-reload
            systemctl --user restart urnetwork.service
            pr_info "Lowmode enabled and service restarted."
            ;;
        off)
            confirm_restart "Disabling lowmode requires restarting the URNetwork provider."
            pr_info "Disabling lowmode..."
            override_rm_env "URNETWORK_PROFILE"
            override_rm_env "GOMEMLIMIT"
            override_rm_env "GOGC"
            systemctl --user daemon-reload
            systemctl --user restart urnetwork.service
            pr_info "Lowmode disabled and service restarted."
            ;;
        *)
            pr_err "Usage: urnet-tools lowmode <on|off>"
            exit 1
            ;;
    esac
}

toggle_ecomode ()
{
    mode="$1"
    case "$mode" in
        on)
            confirm_restart "Enabling eco mode requires restarting the URNetwork provider."
            pr_info "Enabling eco mode..."
            ram_mib=$(detect_mem_limit_mib)
            gomem_mib=$(( ram_mib * 75 / 100 ))
            pr_info "Dynamic GOMEMLIMIT set to ${gomem_mib}MiB (75%% of ${ram_mib}MiB detected RAM)"
            override_set_env "URNETWORK_PROFILE" "eco"
            override_set_env "GOMEMLIMIT" "${gomem_mib}MiB"
            override_set_env "GOGC" "50"
            systemctl --user daemon-reload
            systemctl --user restart urnetwork.service
            pr_info "Eco mode enabled and service restarted."
            ;;
        off)
            confirm_restart "Disabling eco mode requires restarting the URNetwork provider."
            pr_info "Disabling eco mode..."
            override_rm_env "URNETWORK_PROFILE"
            override_rm_env "GOMEMLIMIT"
            override_rm_env "GOGC"
            systemctl --user daemon-reload
            systemctl --user restart urnetwork.service
            pr_info "Eco mode disabled and service restarted."
            ;;
        *)
            pr_err "Usage: urnet-tools eco <on|off>"
            exit 1
            ;;
    esac
}

toggle_automode ()
{
    mode="$1"
    override_dir="$HOME/.config/systemd/user/urnetwork.service.d"
    override_file="$override_dir/override.conf"

    case "$mode" in
        on)
            confirm_restart "Enabling auto-tune profile requires restarting the URNetwork provider."
            pr_info "Enabling auto-tune profile..."
            override_set_env "URNETWORK_PROFILE" "auto"
            systemctl --user daemon-reload
            systemctl --user restart urnetwork.service
            pr_info "Auto-tune enabled and service restarted."
            ;;
        off)
            confirm_restart "Disabling auto-tune profile requires restarting the URNetwork provider."
            pr_info "Disabling auto-tune profile..."
            override_rm_env "URNETWORK_PROFILE"
            systemctl --user daemon-reload
            systemctl --user restart urnetwork.service
            pr_info "Auto-tune disabled and service restarted."
            ;;
        "")
            if [ -f "$override_file" ] && grep -q 'URNETWORK_PROFILE=auto' "$override_file" 2>/dev/null; then
                pr_info "Auto-tune is currently enabled."
            else
                pr_info "Auto-tune is currently off."
            fi
            ;;
        *)
            pr_err "Usage: urnet-tools auto <on|off>"
            exit 1
            ;;
    esac
}

toggle_turbomode ()
{
    mode="$1"
    override_dir="$HOME/.config/systemd/user/urnetwork.service.d"
    override_file="$override_dir/override.conf"

    case "$mode" in
        v4|v8)
            confirm_restart "Enabling turbo mode requires restarting the URNetwork provider."
            pr_info "Enabling turbo %s..." "$mode"
            override_set_env "URNETWORK_PROFILE" "turbo-${mode}"
            systemctl --user daemon-reload
            systemctl --user restart urnetwork.service
            pr_info "Turbo %s enabled and service restarted." "$mode"
            ;;
        off)
            confirm_restart "Disabling turbo mode requires restarting the URNetwork provider."
            pr_info "Disabling turbo mode..."
            override_rm_env "URNETWORK_PROFILE"
            systemctl --user daemon-reload
            systemctl --user restart urnetwork.service
            pr_info "Turbo mode disabled and service restarted."
            ;;
        "")
            if [ -f "$override_file" ] && grep -q 'URNETWORK_PROFILE=turbo-' "$override_file" 2>/dev/null; then
                level=$(grep 'URNETWORK_PROFILE=turbo-' "$override_file" | sed 's/.*turbo-\([^"]*\).*/\1/')
                pr_info "Turbo mode is enabled: %s" "$level"
            else
                pr_info "Turbo mode is off."
            fi
            ;;
        *)
            pr_err "Usage: urnet-tools turbo <v4|v8|off>"
            exit 1
            ;;
    esac
}

setup_zram_manual () {
    pr_info "Attempting manual ZRAM setup (kernel direct)..."

    # First, ensure zram module is loaded with enough devices
    # For upgraded systems, try num_devices=0 (dynamic) first, then static
    modprobe -r zram 2>/dev/null || true
    sleep 0.5

    if ! modprobe zram num_devices=0 2>/dev/null; then
        # Fallback to static device if dynamic fails
        if ! modprobe zram num_devices=1 2>/dev/null; then
            pr_warn "Failed to load 'zram' kernel module. ZRAM is likely not supported by your kernel or restricted by the environment."
            return 1
        fi
    fi

    sleep 1

    # Find or create zram device
    zdev=""
    for d in /dev/zram0 /dev/zram1; do
        if [ -b "$d" ]; then zdev="$d"; break; fi
    done

    if [ -z "$zdev" ]; then
        # Try to create it via zram-control (newer systems)
        if [ -w /sys/class/zram-control/hot_add ]; then
            echo 0 > /sys/class/zram-control/hot_add 2>/dev/null
            sleep 0.5
            [ -b /dev/zram0 ] && zdev="/dev/zram0"
        fi
    fi

    if [ -z "$zdev" ] || [ ! -b "$zdev" ]; then
        pr_warn "Could not find or create a ZRAM device (/dev/zram*). Your environment may lack device-creation permissions."
        return 1
    fi

    # Determine size (80% of total RAM)
    ram_kb=$(grep MemTotal /proc/meminfo | awk '{print $2}')
    zram_bytes=$(( ram_kb * 8 / 10 * 1024 ))
    zdev_name=$(basename "$zdev")

    # Apply configuration with proper error handling for upgraded systems
    pr_info "Configuring $zdev (${zram_bytes} bytes)..."

    swapoff "$zdev" 2>/dev/null || true
    sleep 0.2

    # Reset device (if it has residual config)
    if [ -w "/sys/block/$zdev_name/reset" ]; then
        echo 1 > "/sys/block/$zdev_name/reset" 2>/dev/null || true
        sleep 0.2
    fi

    # Set compression algorithm (zstd preferred, lz4 fallback)
    if [ -w "/sys/block/$zdev_name/comp_algorithm" ]; then
        echo zstd > "/sys/block/$zdev_name/comp_algorithm" 2>/dev/null || \
        echo lz4 > "/sys/block/$zdev_name/comp_algorithm" 2>/dev/null || true
    fi

    # Set disksize — this may fail if device is already in use
    if ! echo "$zram_bytes" > "/sys/block/$zdev_name/disksize" 2>/dev/null; then
        pr_warn "Failed to set ZRAM disksize. Device may be in use or already configured."
        # Try unloading and reloading the module to force a clean state
        modprobe -r zram 2>/dev/null || true
        sleep 1
        if ! modprobe zram num_devices=1 2>/dev/null; then
            return 1
        fi
        sleep 0.5
        if ! echo "$zram_bytes" > "/sys/block/$zdev_name/disksize" 2>/dev/null; then
            pr_warn "Still unable to set disksize after module reload."
            return 1
        fi
    fi

    sleep 0.2

    # Format and enable swap
    if ! mkswap "$zdev" >/dev/null 2>&1; then
        pr_warn "Failed to mkswap on $zdev. Device may not be properly initialized."
        return 1
    fi

    if ! swapon -p 100 "$zdev" >/dev/null 2>&1; then
        pr_warn "Failed to swapon $zdev."
        return 1
    fi

    sleep 0.5

    if swapon --show | grep -q "$zdev_name"; then
        pr_info "ZRAM device $zdev is now active"
        return 0
    fi
    return 1
}

do_report ()
{
    mode="$1"
    override_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/urnetwork.service.d"
    override_file="$override_dir/override.conf"

    if [ -z "$mode" ]; then
        # Show current setting
        if [ -f "$override_file" ]; then
            url=$(grep '^Environment="URNETWORK_REPORT_URL=' "$override_file" | sed 's/^Environment="URNETWORK_REPORT_URL=//; s/"$//')
            if [ -n "$url" ]; then
                pr_info "Report URL: %s" "$url"
            else
                pr_info "Report URL: not configured"
            fi
        else
            pr_info "Report URL: not configured"
        fi
        return
    fi

    case "$mode" in
        off)
            pr_info "Removing report URL (takes effect on next provider restart)..."
            override_rm_env "URNETWORK_REPORT_URL"
            systemctl --user daemon-reload
            pr_info "Report URL removed. Restart provider to apply: systemctl --user restart urnetwork.service"
            ;;
        *)
            pr_info "Setting report URL to %s (takes effect on next provider restart)..." "$mode"
            override_set_env "URNETWORK_REPORT_URL" "$mode"
            systemctl --user daemon-reload
            pr_info "Report URL set to %s. Restart provider to apply: systemctl --user restart urnetwork.service" "$mode"
            ;;
    esac
}

do_fast_auth ()
{
    file="$HOME/.urnetwork/fast_auth"
    case "${1:-}" in
        on)
            mkdir -p "$HOME/.urnetwork"
            touch "$file"
            pr_info "Auth rate limiter bypassed. Takes effect immediately (same as URNETWORK_AUTH_UNLIMITED=true)."
            ;;
        off)
            rm -f "$file"
            pr_info "Auth rate limiter re-enabled. Takes effect immediately."
            ;;
        "")
            if [ -f "$file" ]; then
                pr_info "fast-auth: on (rate limiter bypassed)"
            else
                pr_info "fast-auth: off (rate limiter active)"
            fi
            ;;
        *)
            pr_err "Usage: urnet-tools fast-auth [on|off]"
            exit 1
            ;;
    esac
}

# _set_key_to_file maps a user-facing key name to its ~/.urnetwork/ filename.
# Prints the filename on success, nothing on unknown key.
_set_key_to_file ()
{
    case "$1" in
        node-name)         echo "node_name" ;;
        report-interval)   echo "report_interval" ;;
        proxy-url-max)     echo "proxy_url_max" ;;
        proxy-url-refresh) echo "proxy_url_refresh" ;;
        cleanup-scope)     echo "proxy_dead_cleanup_scope" ;;
        cleanup-interval)  echo "proxy_dead_cleanup_interval" ;;
        fast-auth)         echo "fast_auth" ;;
        *)                 ;;
    esac
}

do_set ()
{
    base_dir="$HOME/.urnetwork"
    key="${1:-}"
    value="${2:-}"

    if [ -z "$key" ]; then
        # Show all active overrides
        pr_info "Runtime overrides (%s/):" "$base_dir"
        found=0
        for name in node_name report_interval proxy_url_max proxy_url_refresh \
                    proxy_dead_cleanup_scope proxy_dead_cleanup_interval fast_auth; do
            file="$base_dir/$name"
            [ -f "$file" ] || continue
            found=$((found + 1))
            if [ "$name" = "fast_auth" ]; then
                printf "  %-32s %s\n" "$name" "on"
            else
                printf "  %-32s %s\n" "$name" "$(cat "$file")"
            fi
        done
        [ "$found" -eq 0 ] && pr_info "No runtime overrides set (all using startup defaults)."
        return
    fi

    filename=$(_set_key_to_file "$key")
    if [ -z "$filename" ]; then
        pr_err "Unknown key: %s" "$key"
        pr_err "Valid keys: node-name, report-interval, proxy-url-max, proxy-url-refresh,"
        pr_err "            cleanup-scope, cleanup-interval, fast-auth"
        exit 1
    fi

    file="$base_dir/$filename"

    # No value: show current
    if [ -z "$value" ]; then
        if [ "$filename" = "fast_auth" ]; then
            [ -f "$file" ] && pr_info "fast-auth: on" || pr_info "fast-auth: off (not set)"
        elif [ -f "$file" ]; then
            pr_info "%s: %s" "$key" "$(cat "$file")"
        else
            pr_info "%s: not set (using startup default)" "$key"
        fi
        return
    fi

    # off: clear the override
    if [ "$value" = "off" ]; then
        rm -f "$file"
        pr_info "%s cleared — reverts to startup default on next tick." "$key"
        return
    fi

    # fast-auth is existence-based; route through do_fast_auth for consistent messaging
    if [ "$filename" = "fast_auth" ]; then
        do_fast_auth "$value"
        return
    fi

    mkdir -p "$base_dir"
    printf '%s' "$value" > "$file"
    pr_info "%s set to %s — takes effect on next provider tick." "$key" "$value"
}

do_hub () {
    cmd="$1"
    shift || true

    override_dir="$HOME/.config/systemd/user/urnetwork.service.d"
    hub_conf="$override_dir/hub.conf"

    hub_service_dir="$HOME/.config/systemd/user"
    hub_service="$hub_service_dir/urnetwork-hub.service"
    hub_bin="$install_path/bin/urnetwork-hub"

    case "$cmd" in
        set)
            url="$1"
            if [ -z "$url" ]; then
                pr_err "Usage: urnet-tools hub set <http://host:port>"
                pr_err "Example: urnet-tools hub set http://192.0.2.10:8080"
                pr_err "Note: https:// also works when the hub is behind a reverse proxy like Caddy"
                exit 1
            fi

            # Basic sanity check: must start with http:// or https://
            case "$url" in
                http://*|https://*) ;;
                *)
                    pr_err "Invalid URL '%s': must begin with http:// or https://" "$url"
                    exit 1
                    ;;
            esac

            if [ "$has_systemd" -eq 0 ]; then
                pr_err "systemd is not available on this system"
                exit 1
            fi

            pr_info "Configuring provider to report to hub: %s" "$url"
            mkdir -p "$override_dir"

            cat > "$hub_conf" <<EOF
[Service]
Environment="URNETWORK_REPORT_URL=$url"
EOF
            pr_info "Wrote %s" "$hub_conf"

            systemctl --user daemon-reload || { pr_err "daemon-reload failed"; exit 1; }

            if systemctl --user is-active --quiet urnetwork.service 2>/dev/null; then
                pr_info "Restarting urnetwork.service to apply hub URL..."
                systemctl --user restart urnetwork.service || { pr_err "Failed to restart urnetwork.service"; exit 1; }
                pr_info "Provider restarted. It will now report to: %s" "$url"
            else
                pr_info "urnetwork.service is not running; hub URL will take effect on next start."
            fi
            ;;

        off)
            if [ "$has_systemd" -eq 0 ]; then
                pr_err "systemd is not available on this system"
                exit 1
            fi

            if [ ! -f "$hub_conf" ]; then
                pr_info "Hub reporting is not configured (no override found at %s)." "$hub_conf"
                exit 0
            fi

            pr_info "Removing hub override: %s" "$hub_conf"
            rm -f "$hub_conf"

            # Clean up empty drop-in dir if nothing else lives there
            rmdir "$override_dir" 2>/dev/null || true

            systemctl --user daemon-reload || { pr_err "daemon-reload failed"; exit 1; }

            if systemctl --user is-active --quiet urnetwork.service 2>/dev/null; then
                pr_info "Restarting urnetwork.service to remove hub URL..."
                systemctl --user restart urnetwork.service || { pr_err "Failed to restart urnetwork.service"; exit 1; }
                pr_info "Hub reporting disabled. Provider restarted."
            else
                pr_info "Hub reporting disabled. Change takes effect on next provider start."
            fi
            ;;

        install)
            if [ "$has_systemd" -eq 0 ]; then
                pr_err "systemd is not available on this system"
                exit 1
            fi

            # Resolve which tag to download the hub binary from
            hub_tag="${URNETWORK_HUB_TAG:-}"
            if [ -z "$hub_tag" ] && [ -f "$version_file" ]; then
                hub_tag="$(cat "$version_file")"
            fi
            if [ -z "$hub_tag" ]; then
                hub_tag=latest
            fi

            # Resolve 'latest' to a real tag
            if [ "$hub_tag" = "latest" ]; then
                pr_info "Resolving latest release tag..."
                api_url="$api_base/releases/latest"
                release="$(network_fetch "$api_url" 2>/dev/null || true)"
                hub_tag="$(get_version_from_api_response "$release" 2>/dev/null)"

                if [ -z "$hub_tag" ] && command -v curl > /dev/null; then
                    tag_url=$(curl -Ls -o /dev/null -w %{url_effective} \
                        "https://github.com/full-bars/urnetwork-3.23-fix/releases/latest")
                    if [ -n "$tag_url" ] && [ "$tag_url" != "https://github.com/full-bars/urnetwork-3.23-fix/releases/latest" ]; then
                        hub_tag="${tag_url##*/}"
                    fi
                fi

                if [ -z "$hub_tag" ]; then
                    pr_err "Could not resolve the latest release tag. Try: URNETWORK_HUB_TAG=vX.Y.Z urnet-tools hub install"
                    exit 1
                fi
            fi

            hub_dl_url="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/${hub_tag}/urnetwork-hub-${hub_tag}-linux-${arch}"
            pr_info "Downloading hub binary from: %s" "$hub_dl_url"

            mkdir -p "$install_path/bin"
            tmp_hub="$(mktemp)"
            trap 'rm -f "$tmp_hub"' EXIT

            if ! download_asset "$hub_dl_url" "$tmp_hub"; then
                pr_err "Failed to download hub binary from: %s" "$hub_dl_url"
                pr_err "Make sure this release includes a hub binary asset."
                exit 1
            fi

            chmod 755 "$tmp_hub"
            mv "$tmp_hub" "$hub_bin"
            trap - EXIT
            pr_info "Hub binary installed at: %s" "$hub_bin"

            # Create the hub data directory
            hub_data_dir="$HOME/.local/share/urnetwork-hub"
            mkdir -p "$hub_data_dir"

            pr_info "Installing urnetwork-hub.service..."
            cat > "$hub_service" <<EOF
[Unit]
Description=URnetwork Hub Dashboard

[Service]
ExecStart=$hub_bin -addr :8080 -data $hub_data_dir
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=default.target
EOF

            systemctl --user daemon-reload || { pr_err "daemon-reload failed"; exit 1; }

            if systemctl --user enable --now urnetwork-hub.service; then
                pr_info "Hub service enabled and started."
                pr_info "Dashboard available at: http://localhost:8080"
                pr_info ""
                pr_info "Next steps:"
                pr_info "  urnet-tools hub set http://<this-host>:8080   # point your providers at the hub"
                pr_info "  journalctl --user -fu urnetwork-hub.service    # stream hub logs"
                pr_info ""
                pr_info "Tip: https:// also works when the hub is behind a reverse proxy like Caddy"
                pr_info "     e.g. urnet-tools hub set https://hub.example.com"
            else
                pr_err "Failed to enable or start urnetwork-hub.service"
                pr_err "Try: journalctl --user -xe | grep hub"
                exit 1
            fi
            ;;

        "")
            pr_err "Usage: urnet-tools hub <set <http://host:port>|off|install>"
            exit 1
            ;;

        *)
            pr_err "Unknown hub command: %s (try 'set', 'off', or 'install')" "$cmd"
            exit 1
            ;;
    esac
}

do_proxy () {
    cmd="$1"
    shift
    
    provider_bin="$install_path/bin/urnetwork"
    if [ ! -f "$provider_bin" ]; then
        pr_err "URnetwork binary not found at %s. Is it installed?" "$provider_bin"
        exit 1
    fi

    case "$cmd" in
        add)
            proxy_file="$1"
            if [ -z "$proxy_file" ]; then
                pr_err "Usage: urnet-tools proxy add <path_to_proxies.txt>"
                exit 1
            fi
            if [ ! -f "$proxy_file" ]; then
                pr_err "Proxy file not found: %s" "$proxy_file"
                exit 1
            fi
            pr_info "Adding proxies from %s..." "$proxy_file"
            "$provider_bin" proxy add --proxy_file="$proxy_file" -f
            ;;
        clear)
            pr_info "Clearing all proxies..."
            "$provider_bin" proxy remove --all
            # Also strip PROXY_URL from systemd override drop-ins so the env var
            # doesn't re-populate URL sources on next restart.
            sd_override_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/urnetwork.service.d"
            if [ -d "$sd_override_dir" ]; then
                for conf in "$sd_override_dir"/*.conf; do
                    [ -f "$conf" ] || continue
                    if grep -q '^Environment="PROXY_URL=' "$conf" 2>/dev/null; then
                        grep -v '^Environment="PROXY_URL=' "$conf" > "${conf}.tmp" && mv "${conf}.tmp" "$conf"
                        pr_info "Removed PROXY_URL from $(basename "$conf")"
                    fi
                done
                systemctl --user daemon-reload 2>/dev/null || true
            fi
            ;;
        health)
            health_dir="${URNETWORK_PROXY_HEALTH_DIR:-$HOME/.urnetwork}"
            state_file="$health_dir/proxy_health.state"
            log_file="$health_dir/proxy_health.log"
            if [ -f "$state_file" ]; then
                pr_info "Current proxy health ($state_file):"
                cat "$state_file"
            else
                pr_warn "No snapshot yet at %s (waiting for first heartbeat?)." "$state_file"
            fi
            if [ -f "$log_file" ]; then
                echo
                pr_info "Streaming proxy health events ($log_file). Ctrl-C to stop."
                tail -n 20 -f "$log_file"
            else
                pr_warn "No event log yet at %s." "$log_file"
            fi
            ;;
        traffic)
            health_dir="${URNETWORK_PROXY_HEALTH_DIR:-$HOME/.urnetwork}"
            state_file="$health_dir/proxy_traffic.state"
            if [ -f "$state_file" ]; then
                pr_info "Current proxy traffic ($state_file):"
                cat "$state_file"
            else
                pr_warn "No traffic snapshot yet at %s." "$state_file"
            fi
            ;;
        refresh)
            "$provider_bin" proxy refresh "$@"
            ;;
        remove-dead)
            "$provider_bin" proxy remove-dead "$@"
            ;;
        summary)
            "$provider_bin" proxy summary
            ;;
        *)
            pr_err "Unknown proxy command: %s (Try 'add', 'clear', 'health', 'traffic', 'refresh', 'remove-dead', or 'summary')" "$cmd"
            exit 1
            ;;
    esac
}

do_auth ()
{
    provider_bin="$install_path/bin/urnetwork"
    if [ ! -f "$provider_bin" ]; then
        pr_err "URnetwork binary not found at %s. Is it installed?" "$provider_bin"
        exit 1
    fi

    if [ "$#" -ge 1 ]; then
        auth_code="$1"
        "$provider_bin" auth "$auth_code" -f
    else
        "$provider_bin" auth -f
    fi
    exit_code=$?
    if [ "$exit_code" -eq 0 ]; then
        pr_info "Authentication successful."
    else
        pr_err "Authentication failed with code $exit_code."
    fi
    exit "$exit_code"
}

do_optimize ()
{
    if [ "$(id -u)" -ne 0 ]; then
        pr_info "Elevation required. Re-running with sudo..."
        exec sudo "$0" "$operation" "$@"
    fi

    # Detect if we are in a container
    is_container=0
    if [ -f "/.dockerenv" ] || grep -qa container= /proc/1/environ 2>/dev/null; then
        is_container=1
    fi

    # 0. Immediate Pre-Optimization (prevents failures during the optimization itself)
    # Increase ulimit and inotify limits immediately for the session to prevent "Too many open files"
    ulimit -n 1048576 2>/dev/null || true
    sysctl -w fs.inotify.max_user_watches=524288 >/dev/null 2>&1 || true
    sysctl -w fs.inotify.max_user_instances=512 >/dev/null 2>&1 || true
    sysctl -w fs.file-max=2097152 >/dev/null 2>&1 || true

    # When running via sudo, we need to find the original user's home
    actual_user=$(logname 2>/dev/null || echo "$SUDO_USER" || echo "$USER")
    actual_home=$(getent passwd "$actual_user" | cut -d: -f6)

    # Check if service is active and warn about restart before doing any work
    if sudo -u "$actual_user" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$(id -u $actual_user)/bus" systemctl --user is-active --quiet urnetwork.service 2>/dev/null; then
        confirm_restart "Applying kernel limits and OS optimizations requires restarting the URNetwork provider to bind the new system ulimits."
    fi

    pr_info "⚡ Starting System Optimizer..."

    if [ "$is_container" -eq 1 ]; then
        pr_warn "Container environment detected. OS-level kernel tuning (ZRAM, Sysctl) should be run on the HOST machine for best results."
    fi

    # Helper for interactive confirmation
    confirm () {
        if [ "$FORCE" = "1" ]; then return 0; fi
        printf "  [?] " && printf '%s [y/N]: ' "$1"
        read -r response
        case "$response" in
            [yY][eE][sS]|[yY]) return 0 ;;
            *) return 1 ;;
        esac
    }

    # 1. Dependency Check & Module Loading
    pr_info "Ensuring kernel modules are loaded..."
    modprobe nf_conntrack >/dev/null 2>&1

    if [ ! -d "/proc/sys/net/netfilter" ] || ! command -v jq >/dev/null; then
        pr_info "System utilities missing (conntrack/jq). Attempting to install..."
        if [ -f /etc/os-release ]; then
            . /etc/os-release
            case "$ID" in
                arch)
                    pacman -Sy --noconfirm conntrack-tools jq
                    ;;
                debian|ubuntu|linuxmint)
                    apt-get update && apt-get install -y conntrack jq
                    ;;
                fedora|rhel|centos|rocky|almalinux|amzn)
                    dnf install -y conntrack-tools jq
                    ;;
                alpine)
                    apk add conntrack-tools jq
                    ;;
                opensuse*|sles)
                    zypper install -y conntrack-tools jq
                    ;;
                *)
                    pr_warn "Unsupported distro ID: %s. Please install 'conntrack' and 'jq' manually." "$ID"
                    ;;
            esac
        else
            # Fallback for older systems
            if [ -f /etc/arch-release ]; then
                pacman -Sy --noconfirm conntrack-tools jq
            elif [ -f /etc/debian_version ]; then
                apt-get update && apt-get install -y conntrack jq
            elif [ -f /etc/redhat-release ]; then
                dnf install -y conntrack-tools jq
            fi
        fi
        modprobe nf_conntrack >/dev/null 2>&1 || pr_warn "Warning: Failed to load nf_conntrack module."
    fi

    # Persistence for module (solves race condition on reboot)
    pr_info "Configuring early module loading..."
    mkdir -p /etc/modules-load.d
    echo "nf_conntrack" > /etc/modules-load.d/urnetwork.conf

    # 2. ZRAM Optimization
    pr_info "Checking for ZRAM (Compressed RAM Swap)..."
    skip_zram=0

    # Check if kernel supports zram module
    if ! modprobe -n zram >/dev/null 2>&1; then
        pr_warn "Your kernel does not support ZRAM. Skipping compressed memory optimization."
        skip_zram=1
    elif swapon --show | grep -q "zram"; then
        pr_info "ZRAM is already active."
        if ! confirm "ZRAM is already configured. Re-apply URNetwork's 80% zstd optimization?"; then
            pr_info "Skipping ZRAM configuration (respecting existing setup)."
            skip_zram=1
        fi
    fi

    if [ "$skip_zram" -eq 0 ]; then
        pr_info "Applying ZRAM configuration (80% RAM, zstd)..."
        if [ -f /etc/os-release ]; then
            . /etc/os-release
            case "$ID" in
                arch)
                    pacman -Sy --noconfirm zram-generator
                    printf "[zram0]\nzram-size = ram * 0.8\ncompression-algorithm = zstd\n" > /etc/systemd/zram-generator.conf
                    systemctl daemon-reload 2>/dev/null || true
                    systemctl start /dev/zram0 2>/dev/null || true
                    ;;
                debian|ubuntu|linuxmint)
                    apt-get update && apt-get install -y zram-tools 2>/dev/null || true
                    ram_kb=$(grep MemTotal /proc/meminfo | awk '{print $2}')
                    zram_mb=$(( ram_kb * 8 / 10 / 1024 ))
                    echo "ZRAM_SIZE=$zram_mb" > /etc/default/zramswap
                    echo "ZRAM_ALGORITHM=zstd" >> /etc/default/zramswap
                    systemctl restart zramswap 2>/dev/null || true
                    ;;
                fedora|rhel|centos|rocky|almalinux|amzn)
                    dnf install -y zram-generator
                    printf "[zram0]\nzram-size = ram * 0.8\ncompression-algorithm = zstd\n" > /etc/systemd/zram-generator.conf
                    systemctl daemon-reload 2>/dev/null || true
                    systemctl start /dev/zram0 2>/dev/null || true
                    ;;
            esac
        fi

        # Verify ZRAM activation, with manual fallback
        if swapon --show | grep -q "zram"; then
            pr_info "ZRAM enabled successfully."
        else
            if setup_zram_manual; then
                pr_info "ZRAM enabled via manual fallback."
            else
                pr_warn "ZRAM could not be auto-enabled. You may need to restart the zram service manually."
            fi
        fi
    fi

    # 3. Sysctl Optimization
    ram_mib=$(detect_mem_limit_mib)
    ct_max=2097152
    ct_buckets=$(( ct_max / 4 ))
    timeout=3600
    ulimit_val=1048576

    sysctl_conf="/etc/sysctl.d/99-urnetwork.conf"
    skip_sysctl=0

    # Check if a non-URNetwork file already has high conntrack settings
    current_max=$(cat /proc/sys/net/netfilter/nf_conntrack_max 2>/dev/null || echo 0)
    if [ "$current_max" -ge "$ct_max" ] && [ ! -f "$sysctl_conf" ]; then
        pr_info "Pre-optimized state detected (conntrack_max is already %s)." "$current_max"
        if ! confirm "System already has high limits. Apply URNetwork's specific sysctl overrides anyway?"; then
            pr_info "Skipping sysctl configuration (respecting existing tuning)."
            skip_sysctl=1
        fi
    fi

    if [ "$skip_sysctl" -eq 0 ]; then
        pr_info "Writing sysctl config to %s..." "$sysctl_conf"
        cat > "$sysctl_conf" <<EOF
# URNetwork Optimized Network Settings
net.netfilter.nf_conntrack_max = $ct_max
net.netfilter.nf_conntrack_buckets = $ct_buckets
net.netfilter.nf_conntrack_tcp_timeout_established = $timeout
net.ipv4.tcp_fin_timeout = 10
net.ipv4.ip_local_port_range = 1024 65535
net.ipv4.tcp_tw_reuse = 1
net.core.default_qdisc = fq
net.ipv4.tcp_congestion_control = bbr
fs.file-max = 2097152
fs.inotify.max_user_watches = 524288
fs.inotify.max_user_instances = 512
EOF
        sysctl --system >/dev/null 2>&1 || pr_warn "Warning: some sysctl settings could not be applied."
    fi

    # 4. Apply Ulimits (Systemd)

    # Ensure everything in the home folder touched by root is restored to user ownership
    if [ -d "$actual_home/.urnetwork" ]; then
        chown -R "$actual_user":"$actual_user" "$actual_home/.urnetwork"
    fi

    override_dir="$actual_home/.config/systemd/user/urnetwork.service.d"
    mkdir -p "$override_dir"
    chown -R "$actual_user":"$actual_user" "$actual_home/.config/systemd" 2>/dev/null
    override_file="$override_dir/override.conf"

    # 5. Disk Benchmark
    pr_info "Running disk benchmark (1GB sync test)..."
    test_file="/tmp/.io-test-optimize"
    res=$(dd if=/dev/zero of="$test_file" bs=1M count=1024 oflag=dsync 2>&1)
    speed_mb=$(echo "$res" | grep -oE '[0-9.]+[[:space:]]+MB/s' | awk '{print int($1)}')
    rm -f "$test_file"

    if [ -n "$speed_mb" ]; then
        pr_info "Disk write speed: ${speed_mb} MB/s"
        if [ "$speed_mb" -lt 50 ]; then
            pr_info "Slow disk detected (< 50 MB/s). High-volume logs will bottleneck your server."
            pr_info "Automatically enabling permanent RAM logging for performance..."
            
            override_set_env "URNETWORK_RAMLOGS" "1"
        fi
    fi

    # Update ulimits in override
    if [ ! -f "$override_file" ]; then
        printf "[Service]\n" > "$override_file"
    fi

    if ! grep -q "LimitNOFILE=" "$override_file"; then
        sed -i "/\[Service\]/a LimitNOFILE=$ulimit_val" "$override_file"
    else
        sed -i "s|LimitNOFILE=.*|LimitNOFILE=$ulimit_val|" "$override_file"
    fi
    chown "$actual_user":"$actual_user" "$override_file"

    pr_info "Optimization applied successfully."
    pr_info "Restarting URnetwork service to apply ulimits..."

    # Run as the actual user to access their systemd bus
    sudo -u "$actual_user" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$(id -u $actual_user)/bus" systemctl --user daemon-reload
    sudo -u "$actual_user" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$(id -u $actual_user)/bus" systemctl --user restart urnetwork.service || pr_info "Note: Service not running; ulimits will apply on next start."

    # Enable lingering for the user so services persist after logout
    if command -v loginctl > /dev/null; then
        loginctl enable-linger "$actual_user" 2>/dev/null && pr_info "✓ Systemd lingering enabled for '$actual_user' (provider will persist after logout)"
    fi
}

case "$operation" in
    install|update|reinstall)
        do_install "$@"
        exit 0
        ;;

    uninstall)
        do_uninstall "$@"
        exit 0
        ;;

    auto-update)
        change_auto_update_prefs "$@"
        exit 0
        ;;

    auto-start)
		toggle_auto_start "$@"
		exit 0
		;;

    start)
		do_start
		exit 0
		;;

    stop)
		do_stop
		exit 0
		;;

    restart)
		do_restart
		exit 0
		;;

	status)
		show_status
		exit 0
		;;

    logs)
        show_logs "$@"
        exit 0
        ;;

    ramlogs)
        toggle_ramlogs "$@"
        exit 0
        ;;

    eco)
        toggle_ecomode "$@"
        exit 0
        ;;

    lowmode)
        toggle_lowmode "$@"
        exit 0
        ;;

    turbo)
        toggle_turbomode "$@"
        exit 0
        ;;

    auto)
        toggle_automode "$@"
        exit 0
        ;;

    optimize)
        do_optimize
        exit 0
        ;;

    auth)
        do_auth "$@"
        exit 0
        ;;

    proxy)
        do_proxy "$@"
        exit 0
        ;;

    hub)
        do_hub "$@"
        exit 0
        ;;

    report)
        do_report "$@"
        exit 0
        ;;

    fast-auth)
        do_fast_auth "$@"
        exit 0
        ;;

    set)
        do_set "$@"
        exit 0
        ;;

    *)
        pr_err "Invalid operation '%s'" "$operation"
        exit 1
        ;;
esac
