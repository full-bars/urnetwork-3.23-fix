#!/bin/sh
# urnet-tools: URnetwork provider manager
# Author: full-bars (GitHub), onlyinthe707 / "mesocyclone" (Discord)
# Based on: Ar Rakin, Ryan Mello (original)
# https://github.com/full-bars/urnetwork-3.23-fix

me="$0"
script_rundir="$(pwd)"

if [ "$me" = "sh" ] || [ "$me" = "bash" ] || [ "$me" = "zsh" ]; then
    me="urnet-tools"
fi

show_help ()
{
    echo "Usage: $me [options] <command>"
    echo ""
    echo "Core Commands:"
    echo "  start                   Start the provider"
    echo "  stop                    Stop the provider"
    echo "  restart [-y|-f]         Restart the provider (-y/-f to skip confirmation)"
    echo "  update                  Upgrade to the latest version"
    echo "  status                  Show provider service status"
    echo "  logs [all|dump|-i]      Stream logs (all=from start, dump=save to ~/urlogs.txt, -i=important only)"
    echo ""
    echo "Performance & Tuning:"
    echo "  turbo <v4|v8|off>       🚀  RAISE throughput limits for RAM-rich boxes"
    echo "  auto <on|off>            🧠  AUTO-TUNE detect hardware and pick best profile"
    echo "  eco <on|off>             🌿  ECO MODE GC-tuned for low-RAM systems"
    echo "  lowmode <on|off>         🧊  LOW-MEMORY reduced buffers for max RAM savings"
    echo "  ramlogs <on|off>         📝  RAM LOGS zero disk I/O logging"
    echo "  hot-restart <on|off>     ♻   reuse client_ids across restarts"
    echo "  optimize                 ⚡  Apply Golden Fleet OS/kernel limits"
    echo ""
    echo "Session:"
    echo "  session save <file>      💾  Export identity+proxy state (encrypted)"
    echo "  session load <file>      📥  Import identity+proxy state, then restart"
    echo ""
    echo "Proxy Management:"
    echo "  auth [<code>]            🔑  Authenticate (omit for interactive paste)"
    echo "  proxy add <file>         🌐  Bulk add proxies from a text file"
    echo "  proxy clear              🗑   Remove all configured proxies"
    echo "  proxy health             ❤   Show dead/degraded proxies + live event log"
    echo "  proxy traffic            📈  Real-time bandwidth & client session load"
    echo "  proxy refresh            🔄  Re-read configs and hot-reload proxies"
    echo "  proxy summary            📊  Fleet summary (sources, health, counts)"
    echo "  proxy remove-dead        💀  Interactively prune dead/degraded/failing"
    echo "  report [<url>|off]       📡  Show or set hub report URL"
    echo "  fast-auth [on|off]       ⚡  Bypass auth rate limiter without restart"
    echo "  self-heal [on|off]       🏥  Auto-regulate proxies (load gate + cleanup) (default: off)"
    echo "  set [<k> [<v>|off]]      ⚙   Show or change runtime tuning overrides"
    echo ""
    echo "Hub Management:"
    echo "  hub init                        🔐  Initialize hub and generate CA certificate"
    echo "  hub link <url> [--token]        🔗  Fetch CA cert and pin the hub's identity"
    echo "  hub unlink                      ✂   Revert to HTTP (remove pin + CA cert)"
    echo "  hub test [<url>]                🔍  Probe TLS connection, verify certificate"
    echo "  hub set <host:port>             📡  Configure legacy HTTP hub report URL"
    echo "  hub off                         📴  Stop reporting to hub (no restart)"
    echo "  hub onboard-cmd                 🎫  Mint 15-min join token, print curl|sh line"
    echo "  hub show-password               🔑  Show CA password (printed once after init)"
    echo "  hub open-port <port>            🔓  Open port in firewall"
    echo "  hub install [--docker]          📦  Install hub as systemd service (or Docker container with --docker)"
    echo "  hub update [--docker]           ⬆  Update hub to latest version"
    echo ""
    echo "Maintenance:"
    echo "  reinstall               Reinstall provider"
    echo "  uninstall               Uninstall provider"
    echo "  auto-update             Manage auto-update schedule (on/off, --interval daily|weekly|monthly)"
    echo "  auto-start              Toggle auto-start on login"
    echo ""
    echo "Global Options:"
    echo "  -h, --help              Show this help"
    echo "  -v, --version           Show running and installed versions"
    echo "  -t, --tag=TAG           Use a specific version for (re)install"
    echo "  -i, --install=[PATH]    Custom install path"
    echo "  -4, --ipv4              Force IPv4 for network requests"
    echo "  -f, --force             Skip confirmation prompts"
    echo "  -B, --no-modify-bashrc  Do not modify ~/.bashrc"
    echo ""
    echo "Need help? Email support@fullbars.xyz or visit <https://github.com/full-bars/urnetwork-3.23-fix>"
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

# If no operation is specified and running as a one-off pipe (curl | sh),
# default to install. The installed urnet-tools binary is handled later
# (after arg parsing) by the empty-operation check.
if [ -z "$operation" ]; then
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

# get_asset_digest_from_api_response extracts the sha256 digest (hex,
# "sha256:" prefix stripped) for a named release asset from the release
# JSON. Empty when the asset is absent — callers treat that as "release
# predates this asset" and fall back.
get_asset_digest_from_api_response ()
{
    asset="$2"
    if command -v jq > /dev/null; then
        digest="$(printf "%s" "$1" | tr -d '\000-\037' | jq -r --arg a "$asset" '.assets[] | select(.name == $a) | .digest // ""' 2>/dev/null)"
    elif command -v python3 > /dev/null; then
        digest="$(printf "%s" "$1" | tr -d '\000-\037' | python3 -c 'import sys, json;
try:
    data = json.load(sys.stdin)
    asset = sys.argv[1]
    for a in data.get("assets", []):
        if a.get("name") == asset:
            print(a.get("digest") or "")
            break
except (json.JSONDecodeError, KeyError):
    print("")
' "$asset" 2>/dev/null)"
    else
        pr_err "Neither python3 nor jq is available"
        exit 1
    fi

    # The release API digest field is "sha256:<hex>"; strip the prefix so
    # sha256sum comparison works. A JSON null digest (assets uploaded before
    # digest support) yields the literal "null"/"None" — normalize those to
    # empty so callers take the documented pre-digest fallback instead of
    # downloading and hitting a sha256 mismatch (verified 2026-08-12 review).
    case "$digest" in
        null|None) digest="" ;;
    esac
    echo "$digest" | sed 's/^sha256://'
}

# verify_sha256_file checks a file's sha256 against the expected hex digest.
# Returns 0 on match, 1 on mismatch/missing tools.
verify_sha256_file ()
{
    file="$1"
    expected="$2"
    if [ -z "$expected" ] || [ ! -f "$file" ]; then
        return 1
    fi
    if command -v sha256sum > /dev/null; then
        actual="$(sha256sum "$file" | awk '{print $1}')"
    elif command -v openssl > /dev/null; then
        actual="$(openssl dgst -sha256 "$file" | awk '{print $2}')"
    else
        pr_err "Neither sha256sum nor openssl is available; cannot verify downloaded tool"
        return 1
    fi
    [ "$actual" = "$expected" ]
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

# tls_fetch URL — like network_fetch but tolerates self-signed certificates
# (-k for curl, --no-check-certificate for wget). Used during hub link TOFU
# provisioning when the TLS cert hasn't been pinned yet.
tls_fetch () {
    url="$1"
    if command -v curl > /dev/null; then
        curl -k --connect-timeout 10 -fSsL "$url" 2>/dev/null
    elif command -v wget > /dev/null; then
        wget -q --no-check-certificate --timeout=10 -O - "$url" 2>/dev/null
    else
        return 1
    fi
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
        if latest_version="$(network_fetch "https://dl.fullbars.xyz/latest-version" 2>/dev/null)"; then
            latest_version="$(printf "%s" "$latest_version" | tr -d '[:space:]')"
        fi
    fi

    if [ -z "$latest_version" ]; then
        echo "Latest version: (could not check for updates)"
        return
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

# If the default "install" was set above but this is the installed
# urnet-tools binary with no actual subcommand, show help instead.
if [ "$operation" = "install" ]; then
    case "$0" in
        *"/urnet-tools"|*"/bin/urnet-tools")
            operation=""
            ;;
    esac
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

# sanitize_restart_dropins
# Repairs any urnetwork.service.d/*.conf drop-in with an invalid `Restart=`
# value. systemd only accepts no/always/on-success/on-failure/on-abnormal/
# on-watchdog/on-abort for this directive -- not a bare "yes"/"true"/"1"
# (a common mistake by analogy to other tools' boolean restart flags). An
# invalid value isn't an error systemd surfaces loudly: it logs a parse
# warning on every daemon-reload/start and silently ignores that one line,
# falling back to the base unit's `Restart=no`, which leaves the service
# with zero crash-restart protection. urnet-tools has never written a
# drop-in like this itself, but a stray/manually-created one persists
# across installs and updates since nothing else in this script scans
# urnetwork.service.d for foreign files -- only override.conf is managed
# by override_set_env/override_rm_env.
sanitize_restart_dropins ()
{
    dropin_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/urnetwork.service.d"
    [ -d "$dropin_dir" ] || return 0

    for f in "$dropin_dir"/*.conf; do
        [ -f "$f" ] || continue
        if grep -Eq '^Restart=(yes|true|1)[[:space:]]*$' "$f"; then
            pr_warn "Repairing invalid 'Restart=' value in %s (systemd requires no/always/on-failure/etc, not yes/true/1)" "$f"
            sed -i -E 's/^Restart=(yes|true|1)[[:space:]]*$/Restart=on-failure/' "$f"
        fi
    done
}

install_systemd_units ()
{
    start="$systemd_units_stopped"

    sanitize_restart_dropins

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
            case "$0" in
                *"/urnet-tools"|*"/bin/urnet-tools")
                    pr_err "Invalid operation '%s'" "$operation"
                    exit 1
                    ;;
            esac

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

    # Fallback: try dl.fullbars.xyz latest-version endpoint
    if [ -z "$version_to_install" ]; then
        if worker_version="$(network_fetch "https://dl.fullbars.xyz/latest-version" 2>/dev/null)"; then
            version_to_install="$(printf "%s" "$worker_version" | tr -d '[:space:]')"
        fi
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
    dl_url="https://dl.fullbars.xyz/releases/download/$tag/urnetwork-provider-$tag.tar.gz"
    mirror_url="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/$tag/urnetwork-provider-$tag.tar.gz"
    
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
            pr_warn "Primary download failed, trying GitHub mirror..."
            if ! download_asset "$mirror_url" "$tarball"; then
                pr_err "Failed to download from both primary and mirror"
                exit 1
            fi
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

    # --- urnet-tools install ---
    # The tool is now a Go binary shipped as a standalone release asset
    # (urnet-tools-<os>-<arch>, v3.23.0-fix.28+). Prefer it — digest-verified
    # against the release API — and fall back to the legacy self-copy shell
    # script only for releases that predate the Go asset. The Go tool is
    # self-updating (`urnet-tools update` refreshes its own binary), so this
    # shell path is a one-time handoff.
    tool_installed=0
    if [ -n "$tag" ] && [ "$tag" != "latest" ]; then
        tool_asset="urnet-tools-linux-$arch"
        tool_dl_url="https://dl.fullbars.xyz/releases/download/$tag/$tool_asset"
        tool_mirror_url="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/$tag/$tool_asset"

        # Resolve the digest from the release API. Empty digest = the release
        # predates tool assets (or the asset is missing) → fall back to shell.
        tool_digest=""
        if release_json="$(network_fetch "$api_base/releases/tags/$tag" 2>/dev/null)"; then
            tool_digest="$(get_asset_digest_from_api_response "$release_json" "$tool_asset")"
        fi

        if [ -n "$tool_digest" ]; then
            pr_info "Installing Go urnet-tools binary (%s)..." "$tool_asset"
            if download_asset "$tool_dl_url" "$workdir/$tool_asset"; then
                if verify_sha256_file "$workdir/$tool_asset" "$tool_digest"; then
                    # Bypass 'Text file busy' like the provider swap above.
                    mv -f "$install_path/bin/urnet-tools" "$install_path/bin/urnet-tools.old" 2>/dev/null || true
                    cp "$workdir/$tool_asset" "$install_path/bin/urnet-tools" || { pr_err "Failed to install urnet-tools binary"; exit 1; }
                    chmod 755 "$install_path/bin/urnet-tools" || { pr_err "Failed to install urnet-tools binary"; exit 1; }
                    rm -f "$install_path/bin/urnet-tools.old" 2>/dev/null || true
                    tool_installed=1
                else
                    pr_warn "urnet-tools sha256 mismatch, falling back to shell script"
                fi
            else
                pr_warn "Primary tool download failed, trying GitHub mirror..."
                if download_asset "$tool_mirror_url" "$workdir/$tool_asset"; then
                    if verify_sha256_file "$workdir/$tool_asset" "$tool_digest"; then
                        mv -f "$install_path/bin/urnet-tools" "$install_path/bin/urnet-tools.old" 2>/dev/null || true
                        cp "$workdir/$tool_asset" "$install_path/bin/urnet-tools" || { pr_err "Failed to install urnet-tools binary"; exit 1; }
                        chmod 755 "$install_path/bin/urnet-tools" || { pr_err "Failed to install urnet-tools binary"; exit 1; }
                        rm -f "$install_path/bin/urnet-tools.old" 2>/dev/null || true
                        tool_installed=1
                    else
                        pr_warn "urnet-tools sha256 mismatch (mirror), falling back to shell script"
                    fi
                else
                    pr_warn "Mirror tool download failed, falling back to shell script"
                fi
            fi
        fi
    fi

    if [ "$tool_installed" -ne 1 ]; then
        # Legacy fallback: self-copy the shell script (releases that predate
        # the Go tool asset, or 386 hosts with no Go asset).
        # Priority: tarball-bundled script > GitHub fetch > running script
        script=""
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
        printf "%s\n" "$script" > "$install_path/bin/urnet-tools"
        chmod 755 "$install_path/bin/urnet-tools" || { pr_err "Failed to install urnet-tools"; exit 1; }

        # Note: the GitHub-fetched script above (from main branch) is the canonical
        # version. The tarball copy is NOT used here to avoid overwriting with stale
        # bundled content that may lack the latest fix.
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

    if hot_restart_is_enabled; then
        printf "\n\e[1;33m🔄 WARNING: A Hot Restart Will Be Attempted 🔄\e[0m\n"
        printf "\e[1m%s\n" "$action"
        printf "Hot-restart is enabled — client JWT identities and warmup state *may* be preserved, but this is not guaranteed (provider binary upgrades, internal state resets, or other factors can force a cold restart).\e[0m\n\n"
    else
        printf "\n\e[1;31m🛑 WARNING: This Command Triggers a Cold Restart 🛑\e[0m\n"
        printf "\e[1m%s\n" "$action"
        printf "This will instantly drop all active connections and reset your proxy warmup state (which typically requires a minimum of 8-12 hours).\e[0m\n\n"
    fi
    
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

# hot_restart_is_enabled
# Reports whether URNETWORK_HOT_RESTART=0 is NOT set in the override.conf —
# matches provider/main.go's hotRestartEnabled() (on by default unless
# explicitly disabled). Single source of truth so confirm_restart() and
# toggle_hotrestart() can't drift.
hot_restart_is_enabled() {
    local override_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/urnetwork.service.d"
    local override_file="$override_dir/override.conf"
    if [ -f "$override_file" ] && grep -q 'URNETWORK_HOT_RESTART=0' "$override_file" 2>/dev/null; then
        return 1
    fi
    return 0
}

# firewall_hint PORT [PROTO]
# Detects the local firewall and prints the command to open the port.
# Does NOT execute anything — the operator runs it with sudo.
firewall_hint() {
    port="$1"
    proto="${2:-tcp}"

    if command -v firewall-cmd > /dev/null && firewall-cmd --state 2>/dev/null | grep -q running; then
        printf '  sudo firewall-cmd --add-port=%s/%s --permanent && sudo firewall-cmd --reload\n' "$port" "$proto"
        return 0
    fi

    if command -v ufw > /dev/null; then
        printf '  sudo ufw allow %s/%s\n' "$port" "$proto"
        return 0
    fi

    if command -v iptables > /dev/null; then
        printf '  sudo iptables -I INPUT -p %s --dport %s -j ACCEPT\n' "$proto" "$port"
        return 0
    fi

    if command -v nft > /dev/null && nft list tables 2>/dev/null | grep -q .; then
        printf '  sudo nft add rule inet filter input %s dport %s accept\n' "$proto" "$port"
        return 0
    fi

    return 1
}

# override_set_env_for_hub KEY VALUE
# Same as override_set_env but targets urnetwork-hub.service (the hub's systemd
# unit) instead of urnetwork.service (the provider's unit).
override_set_env_for_hub() {
    local key="$1" value="$2"
    local override_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/urnetwork-hub.service.d"
    local override_file="$override_dir/override.conf"
    mkdir -p "$override_dir"
    if [ ! -f "$override_file" ]; then
        printf '[Service]\n' > "$override_file"
    fi
    sed -i '/^Environment="'"$key"'=/d' "$override_file"
    printf 'Environment="%s=%s"\n' "$key" "$value" >> "$override_file"
}

# override_rm_env_for_hub KEY
# Same as override_rm_env but targets urnetwork-hub.service.
override_rm_env_for_hub() {
    local key="$1"
    local override_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/urnetwork-hub.service.d"
    local override_file="$override_dir/override.conf"
    if [ -f "$override_file" ]; then
        sed -i '/^Environment="'"$key"'=/d' "$override_file"
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
        "")
            if [ -f "$override_file" ] && grep -q 'URNETWORK_RAMLOGS=1' "$override_file" 2>/dev/null; then
                pr_info "RAM logging is enabled."
            else
                pr_info "RAM logging is off."
            fi
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
    override_dir="$HOME/.config/systemd/user/urnetwork.service.d"
    override_file="$override_dir/override.conf"

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
        "")
            if [ -f "$override_file" ] && grep -q 'URNETWORK_PROFILE=lowmem' "$override_file" 2>/dev/null; then
                gomem=$(grep 'GOMEMLIMIT=' "$override_file" 2>/dev/null | sed 's/.*GOMEMLIMIT=\([^"]*\).*/\1/')
                gogc=$(grep 'GOGC=' "$override_file" 2>/dev/null | sed 's/.*GOGC=\([^"]*\).*/\1/')
                pr_info "Lowmode is enabled. (GOMEMLIMIT=${gomem:-?}, GOGC=${gogc:-?})"
            else
                pr_info "Lowmode is off."
            fi
            ;;
        *)
            pr_err "Usage: urnet-tools lowmode <on|off>"
            exit 1
            ;;
    esac
}

toggle_hotrestart ()
{
    mode="$1"

    case "$mode" in
        on)
            confirm_restart "Enabling hot-restart requires restarting the URNetwork provider."
            pr_info "Enabling hot-restart..."
            override_rm_env "URNETWORK_HOT_RESTART"
            systemctl --user daemon-reload
            systemctl --user restart urnetwork.service
            pr_info "Hot-restart enabled and service restarted."
            ;;
        off)
            confirm_restart "Disabling hot-restart requires restarting the URNetwork provider."
            pr_info "Disabling hot-restart..."
            override_set_env "URNETWORK_HOT_RESTART" "0"
            systemctl --user daemon-reload
            systemctl --user restart urnetwork.service
            pr_info "Hot-restart disabled and service restarted."
            ;;
        "")
            if hot_restart_is_enabled; then
                pr_info "Hot-restart is enabled."
            else
                pr_info "Hot-restart is off."
            fi
            ;;
        *)
            pr_err "Usage: urnet-tools hot-restart <on|off>"
            exit 1
            ;;
    esac
}

toggle_ecomode ()
{
    mode="$1"
    override_dir="$HOME/.config/systemd/user/urnetwork.service.d"
    override_file="$override_dir/override.conf"

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
        "")
            if [ -f "$override_file" ] && grep -q 'URNETWORK_PROFILE=eco' "$override_file" 2>/dev/null; then
                gomem=$(grep 'GOMEMLIMIT=' "$override_file" 2>/dev/null | sed 's/.*GOMEMLIMIT=\([^"]*\).*/\1/')
                gogc=$(grep 'GOGC=' "$override_file" 2>/dev/null | sed 's/.*GOGC=\([^"]*\).*/\1/')
                pr_info "Eco mode is enabled. (GOMEMLIMIT=${gomem:-?}, GOGC=${gogc:-?})"
            else
                pr_info "Eco mode is off."
            fi
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

do_self_heal ()
{
    file="$HOME/.urnetwork/proxy_self_heal"
    case "${1:-}" in
        on)
            mkdir -p "$HOME/.urnetwork"
            printf '%s\n' "on" > "$file"
            pr_info "Self-heal enabled (load gate + auto cleanup active)"
            ;;
        off)
            mkdir -p "$HOME/.urnetwork"
            printf '%s\n' "off" > "$file"
            pr_info "Self-heal disabled (load gate + auto cleanup turned off)"
            ;;
        status|"")
            if [ -f "$file" ] && [ "$(cat "$file" 2>/dev/null)" = "on" ]; then
                pr_info "self-heal: on"
            elif [ -f "$file" ]; then
                pr_info "self-heal: off"
            else
                pr_info "self-heal: off (default; enable with 'urnet-tools self-heal on' or URNETWORK_SELF_HEAL=1)"
            fi
            if [ -f "$HOME/.urnetwork/pressure_status" ]; then
                if command -v jq >/dev/null 2>&1; then
                    pr_info "$(jq -r '"pressure: \(.score) (target_pool=\(.target_pool), updated=\(.updated))"' \
                        "$HOME/.urnetwork/pressure_status" 2>/dev/null)"
                else
                    cat "$HOME/.urnetwork/pressure_status"
                fi
            fi
            ;;
        *)
            pr_err "Usage: urnet-tools self-heal [on|off|status]"
            exit 1
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

    if [ "$key" = "help" ]; then
        echo "Usage: urnet-tools set <key> [<value>|off]"
        echo ""
        echo "Runtime overrides — files read by the provider at runtime from ~/.urnetwork/."
        echo "Changes take effect on the next provider tick (no restart needed)."
        echo "Set a value:  urnet-tools set <key> <value>"
        echo "Show current: urnet-tools set <key>"
        echo "Clear it:     urnet-tools set <key> off"
        echo ""
        echo "Available keys:"
        echo ""
        echo "  node-name           <string>    Node name reported to the fleet hub (default: hostname)"
        echo "  report-interval     <duration>  Bandwidth report cadence (default: 5m, min: 10s)"
        echo "  proxy-url-max       <int>       Max proxies from URL feeds, 0 = unlimited (default: 0)"
        echo "  proxy-url-refresh   <duration>  URL proxy list refresh interval (default: 1h, min: 10s)"
        echo "  cleanup-scope       none|url|all  Dead proxy auto-cleanup scope (default: none)"
        echo "  cleanup-interval    <duration>  Dead proxy cleanup interval (default: 24h, min: 1m)"
        echo "  fast-auth           on|off      Bypass auth rate limiter (marker file)"
        echo ""
        echo "Duration format: Go-style, e.g. 30s, 5m, 1h, 24h."
        return
    fi

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

# --- hub (Docker, opt-in via --docker) ---
#
# 'hub install'/'hub update' default to a native systemd binary install
# (below). --docker runs the hub as a container instead, for operators who'd
# rather not manage a systemd service — the same containerized path Mac and
# Windows always use (they have no native hub binary at all).

hub_docker_image="ghcr.io/full-bars/urnetwork-3.23-fix-hub"
hub_docker_container="urnetwork-hub"
hub_docker_volume="urnetwork-hubdata"
hub_docker_conf="$HOME/.urnetwork/hub-docker.conf"

hub_docker_require() {
    if ! command -v docker > /dev/null; then
        pr_err "Docker is required for --docker mode."
        pr_err "Install: https://docs.docker.com/engine/install/"
        exit 1
    fi
    if ! docker info > /dev/null 2>&1; then
        pr_err "Docker is installed but not running (or this user lacks permission)."
        exit 1
    fi
}

# hub_docker_run TAG PORT TOKEN: (re)creates the hub container. Assumes any
# previous container with the same name has already been removed by the
# caller (install refuses to overwrite; update stops+removes first).
hub_docker_run() {
    _tag="$1" _port="$2" _token="$3"
    _run_args="-d --name $hub_docker_container --restart unless-stopped -p ${_port}:8080 -v ${hub_docker_volume}:/data"
    if [ -n "$_token" ]; then
        _run_args="$_run_args -e URNETWORK_HUB_TOKEN=$_token"
    fi
    # shellcheck disable=SC2086
    docker run $_run_args "$hub_docker_image:$_tag"
}

do_hub_docker_install() {
    tag="latest"
    port="8080"
    token=""
    while [ $# -gt 0 ]; do
        case "$1" in
            --tag) tag="$2"; shift 2 ;;
            --port) port="$2"; shift 2 ;;
            --token) token="$2"; shift 2 ;;
            *) pr_err "Unknown argument: %s" "$1"; exit 1 ;;
        esac
    done

    hub_docker_require

    if docker ps -a --format '{{.Names}}' | grep -qx "$hub_docker_container"; then
        pr_err "Hub container '%s' already exists. Use 'urnet-tools hub update --docker' to upgrade it," "$hub_docker_container"
        pr_err "or 'docker rm -f %s' to remove it first." "$hub_docker_container"
        exit 1
    fi

    pr_info "Pulling %s:%s..." "$hub_docker_image" "$tag"
    docker pull "$hub_docker_image:$tag" || { pr_err "docker pull failed"; exit 1; }

    if ! hub_docker_run "$tag" "$port" "$token"; then
        pr_err "Failed to start hub container"
        exit 1
    fi

    mkdir -p "$HOME/.urnetwork"
    { printf 'tag=%s\n' "$tag"; printf 'port=%s\n' "$port"; printf 'token=%s\n' "$token"; } > "$hub_docker_conf"

    pr_info "Hub container started."
    pr_info "  Dashboard: http://localhost:%s" "$port"
    pr_info "  Data:      docker volume '%s' (persists across updates)" "$hub_docker_volume"
    pr_info ""
    pr_info "Next steps:"
    pr_info "  urnet-tools hub set http://<this-host>:%s   # point your providers at the hub" "$port"
    pr_info "  docker logs -f %s                            # stream hub logs" "$hub_docker_container"
}

do_hub_docker_update() {
    tag=""
    force=0
    while [ $# -gt 0 ]; do
        case "$1" in
            --tag) tag="$2"; shift 2 ;;
            -f|--force) force=1; shift ;;
            *) pr_err "Unknown argument: %s" "$1"; exit 1 ;;
        esac
    done

    hub_docker_require

    if ! docker ps -a --format '{{.Names}}' | grep -qx "$hub_docker_container"; then
        pr_err "No hub container found. Run 'urnet-tools hub install --docker' first."
        exit 1
    fi

    # An explicit --tag must win over the persisted conf, but the conf file
    # is just 'tag=...' shell assignments — sourcing it would silently
    # clobber $tag if we didn't save the flag value first.
    _explicit_tag="$tag"
    port="8080"
    token=""
    if [ -f "$hub_docker_conf" ]; then
        # shellcheck disable=SC1090
        . "$hub_docker_conf"
    fi
    if [ -n "$_explicit_tag" ]; then
        tag="$_explicit_tag"
    elif [ -z "$tag" ]; then
        tag="latest"
    fi

    pr_info "Pulling %s:%s..." "$hub_docker_image" "$tag"
    docker pull "$hub_docker_image:$tag" || { pr_err "docker pull failed"; exit 1; }

    if [ "$force" != "1" ]; then
        running_image="$(docker inspect --format '{{.Image}}' "$hub_docker_container" 2>/dev/null)"
        pulled_image="$(docker inspect --format '{{.Id}}' "$hub_docker_image:$tag" 2>/dev/null)"
        if [ -n "$running_image" ] && [ "$running_image" = "$pulled_image" ]; then
            pr_info "Hub is already running %s:%s. Nothing to do. Use --force to recreate anyway." "$hub_docker_image" "$tag"
            return
        fi
    fi

    pr_info "Recreating hub container (data volume '%s' is preserved)..." "$hub_docker_volume"
    docker stop "$hub_docker_container" > /dev/null 2>&1
    docker rm "$hub_docker_container" > /dev/null 2>&1

    if ! hub_docker_run "$tag" "$port" "$token"; then
        pr_err "Failed to start hub container"
        exit 1
    fi

    { printf 'tag=%s\n' "$tag"; printf 'port=%s\n' "$port"; printf 'token=%s\n' "$token"; } > "$hub_docker_conf"
    pr_info "Hub updated and running %s:%s." "$hub_docker_image" "$tag"
}

do_hub () {
    cmd="$1"
    shift || true

    # --docker opt-in: scanned here (rather than in each case arm's own
    # parsing) so the flag can appear anywhere in the remaining args.
    if [ "$cmd" = "install" ] || [ "$cmd" = "update" ]; then
        _hub_docker_mode=0
        _hub_docker_rest=""
        for _hub_arg in "$@"; do
            case "$_hub_arg" in
                --docker) _hub_docker_mode=1 ;;
                *) _hub_docker_rest="$_hub_docker_rest $_hub_arg" ;;
            esac
        done
        if [ "$_hub_docker_mode" -eq 1 ]; then
            if [ "$cmd" = "install" ]; then
                # shellcheck disable=SC2086
                do_hub_docker_install $_hub_docker_rest
            else
                # shellcheck disable=SC2086
                do_hub_docker_update $_hub_docker_rest
            fi
            return
        fi
    fi

    override_dir="$HOME/.config/systemd/user/urnetwork.service.d"
    hub_conf="$override_dir/hub.conf"

    hub_service_dir="$HOME/.config/systemd/user"
    hub_service="$hub_service_dir/urnetwork-hub.service"
    hub_bin="$install_path/bin/urnetwork-hub"

    case "$cmd" in
        init)
            do_hub_init
            ;;

        link)
            if [ $# -eq 0 ]; then
                pr_err "Usage: urnet-tools hub link <https://hub-host:port> [--token <onboard-token>]"
                pr_err "Fetches the hub's CA certificate and configures TLS trust so all"
                pr_err "future reports are encrypted and verified."
                exit 1
            fi
            do_hub_link "$@"
            ;;

        test)
            do_hub_test "$@"
            ;;

        show-password)
            do_hub_show_password
            ;;

        onboard-cmd)
            do_hub_onboard_cmd
            ;;

        open-port)
            port="$1"
            if [ -z "$port" ]; then
                pr_err "Usage: urnet-tools hub open-port <port>"
                exit 1
            fi
            if ! firewall_hint "$port"; then
                pr_err "No supported firewall detected."
                pr_err "Open port %s manually in your firewall." "$port"
                exit 1
            fi
            ;;

        unlink)
            do_hub_unlink
            ;;

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

            # Same ~/.urnetwork/report_url override the provider polls every
            # report tick (see resolveReportURL in bandwidth_reporter.go) —
            # matches hub link/unlink, so no restart is needed.
            report_file="$HOME/.urnetwork/report_url"
            mkdir -p "$HOME/.urnetwork"
            printf '%s\n' "$url" > "$report_file.tmp"
            mv "$report_file.tmp" "$report_file"
            pr_info "Report URL set to %s" "$url"

            # Clean up any legacy systemd env override from an older
            # urnet-tools version so a future restart doesn't reintroduce a
            # stale URL that would otherwise take precedence over a blank
            # override file.
            if [ "$has_systemd" -eq 1 ] && [ -f "$hub_conf" ]; then
                rm -f "$hub_conf"
                rmdir "$override_dir" 2>/dev/null || true
                systemctl --user daemon-reload 2>/dev/null || true
            fi

            pr_info "The change takes effect on the next report tick (no restart needed)."
            ;;

        off)
            # "off" is a sentinel resolveReportURL recognizes as force-disabled,
            # distinct from a blank/missing file (which is a no-op that falls
            # back to URNETWORK_REPORT_URL from startup, if any was set).
            report_file="$HOME/.urnetwork/report_url"
            mkdir -p "$HOME/.urnetwork"
            printf '%s\n' "off" > "$report_file.tmp"
            mv "$report_file.tmp" "$report_file"

            if [ "$has_systemd" -eq 1 ] && [ -f "$hub_conf" ]; then
                pr_info "Removing legacy hub override: %s" "$hub_conf"
                rm -f "$hub_conf"
                rmdir "$override_dir" 2>/dev/null || true
                systemctl --user daemon-reload 2>/dev/null || true
            fi

            pr_info "Hub reporting disabled. Takes effect on the next report tick (no restart needed)."
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
                    if worker_tag="$(network_fetch "https://dl.fullbars.xyz/latest-version" 2>/dev/null)"; then
                        hub_tag="$(printf "%s" "$worker_tag" | tr -d '[:space:]')"
                    fi
                fi

                if [ -z "$hub_tag" ]; then
                    pr_err "Could not resolve the latest release tag. Try: URNETWORK_HUB_TAG=vX.Y.Z urnet-tools hub install"
                    exit 1
                fi
            fi

            hub_dl_url="https://dl.fullbars.xyz/releases/download/${hub_tag}/urnetwork-hub-${hub_tag}-linux-${arch}"
            hub_mirror_url="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/${hub_tag}/urnetwork-hub-${hub_tag}-linux-${arch}"
            pr_info "Downloading hub binary from: %s" "$hub_dl_url"

            mkdir -p "$install_path/bin"
            tmp_hub="$(mktemp)"
            trap 'rm -f "$tmp_hub"' EXIT

            if ! download_asset "$hub_dl_url" "$tmp_hub"; then
                pr_warn "Primary download failed, trying GitHub mirror..."
                if ! download_asset "$hub_mirror_url" "$tmp_hub"; then
                    pr_err "Failed to download hub binary from both primary and mirror"
                    pr_err "Make sure this release includes a hub binary asset."
                    exit 1
                fi
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

        update)
            force=0
            hub_tag_arg=""

            while [ $# -gt 0 ]; do
                case "$1" in
                    -f|--force)
                        force=1
                        shift
                        ;;
                    -t|--tag)
                        if [ -z "$2" ]; then
                            opt_requires_arg "$1"
                            exit 1
                        fi
                        hub_tag_arg="$2"
                        if [ "$hub_tag_arg" != "latest" ] && [ "$(printf '%s' "$hub_tag_arg" | cut -c -1)" != "v" ]; then
                            hub_tag_arg="v$hub_tag_arg"
                        fi
                        shift 2
                        ;;
                    -*)
                        pr_warn "Ignoring unknown option '%s'" "$1"
                        shift
                        ;;
                    *)
                        pr_err "Unexpected argument '%s' (try 'urnet-tools hub update' without extra args)" "$1"
                        exit 1
                        ;;
                esac
            done

            do_hub_update "$hub_tag_arg" "$force"
            ;;

        "")
            pr_err "Usage: urnet-tools hub <init|link|unlink|set|off|install|update|test|show-password|onboard-cmd>"
            exit 1
            ;;

        *)
            pr_err "Unknown hub command: %s (try 'init', 'link', 'unlink', 'set', 'off', 'install', 'update', 'test', 'show-password', or 'onboard-cmd')" "$cmd"
            exit 1
            ;;
    esac
}

do_hub_test () {
    url="$1"
    ca_file="$HOME/.urnetwork/hub_ca.pem"
    pin_file="$HOME/.urnetwork/hub.pin"
    report_file="$HOME/.urnetwork/report_url"

    if [ -z "$url" ]; then
        if [ -f "$report_file" ]; then
            url="$(cat "$report_file" | tr -d '\n')"
        fi
    fi
    if [ -z "$url" ]; then
        pr_err "No hub URL configured. Specify one or run 'urnet-tools hub link https://...' first."
        exit 1
    fi

    url="${url%/}"
    case "$url" in
        https://*) ;;
        *) pr_err "URL must use https:// for TLS verification (got: %s)" "$url"; exit 1 ;;
    esac

    host="${url#https://}"
    host="${host%%:*}"
    port_tmp="${url#https://}"
    port_tmp="${port_tmp#*:}"
    if [ "$port_tmp" = "${url#https://}" ]; then port=443; else port="$port_tmp"; fi

    pr_info "Testing TLS to %s:%s ..." "$host" "$port"

    # CA-based verification path (preferred)
    if [ -f "$ca_file" ]; then
        if [ ! -s "$ca_file" ]; then
            pr_err "CA certificate file is empty — re-run 'urnet-tools hub link' to re-fetch."
            exit 1
        fi
        if ! command -v openssl > /dev/null; then
            pr_err "openssl is required for CA-based verification."
            exit 1
        fi
        result=$(echo "" | openssl s_client -connect "${host}:${port}" -servername "$host" -CAfile "$ca_file" 2>&1)
        if echo "$result" | grep -q "Verify return code: 0"; then
            pr_info "TLS OK — CA chain verification passed."
            return 0
        else
            pr_err "TLS FAILED — CA chain verification error."
            echo "$result" | grep "Verify return code:"
            exit 1
        fi
    fi

    # Legacy fingerprint-based verification path (hub.pin)
    expected=""
    if [ -f "$pin_file" ]; then
        expected="$(cat "$pin_file" | tr -d ' \n')"
        case "$expected" in
            SHA256:*) ;;
            *) expected="" ;;
        esac
    fi

    if [ -n "$expected" ]; then
        pr_info "Pinned fingerprint: %s" "$expected"
    fi

    if command -v openssl > /dev/null; then
        actual_hex=$(echo "" | openssl s_client -connect "${host}:${port}" -servername "$host" 2>/dev/null | openssl x509 -noout -fingerprint -sha256 2>/dev/null | cut -d= -f2 | tr -d ':' | tr '[:upper:]' '[:lower:]')
        if [ -z "$actual_hex" ]; then
            pr_err "Could not connect to %s:%s or retrieve certificate." "$host" "$port"
            pr_err ""
            pr_err "Check:"
            pr_err "  1. Is the hub running?"
            pr_err "     systemctl --user status urnetwork-hub.service"
            pr_err "  2. Is port %s open?" "$port"
            firewall_hint "$port"
            exit 1
        fi
        actual="SHA256:${actual_hex}"
        pr_info "Hub certificate:  %s" "$actual"

        if [ -n "$expected" ]; then
            if [ "$expected" = "$actual" ]; then
                pr_info "TLS OK — fingerprint matches."
                return 0
            else
                pr_err "TLS FAILED — fingerprint MISMATCH!"
                pr_err "Expected:  %s" "$expected"
                pr_err "Got:       %s" "$actual"
                pr_err "To re-pin: urnet-tools hub link %s" "$url"
                exit 1
            fi
        else
            pr_info "TLS OK — connected. Run 'urnet-tools hub link %s' to pin." "$url"
        fi
    elif command -v curl > /dev/null; then
        pr_info "openssl not found, using curl fallback..."
        cert_json=$(tls_fetch "$url/api/cert" 2>/dev/null)
        if [ -z "$cert_json" ]; then
            pr_err "Could not reach hub at %s." "$url"
            exit 1
        fi
        fp=$(printf '%s' "$cert_json" | sed -n 's/.*"fingerprint" *: *"\([^"]*\)".*/\1/p')
        if [ -z "$fp" ]; then
            pr_err "Hub responded but did not return a fingerprint."
            exit 1
        fi
        pr_info "Hub fingerprint: %s" "$fp"
        if [ -n "$expected" ]; then
            if [ "$expected" = "$fp" ]; then
                pr_info "TLS OK — fingerprint matches."
            else
                pr_err "TLS FAILED — fingerprint MISMATCH!"
                exit 1
            fi
        else
            pr_info "TLS OK — connected. Run 'urnet-tools hub link %s' to pin." "$url"
        fi
    else
        pr_err "Neither openssl nor curl found."
        exit 1
    fi
}

do_hub_update () {
    hub_tag_arg="$1"
    force="$2"
    hub_data_dir="$HOME/.local/share/urnetwork-hub"
    hub_version_file="$hub_data_dir/.hub_version"

    if [ "$has_systemd" -eq 0 ]; then
        pr_err "systemd is not available on this system"
        exit 1
    fi

    # Resolve which tag to download
    hub_tag="$hub_tag_arg"
    if [ -z "$hub_tag" ]; then
        hub_tag="${URNETWORK_HUB_TAG:-}"
    fi
    if [ -z "$hub_tag" ] && [ -f "$hub_version_file" ]; then
        hub_tag="$(cat "$hub_version_file")"
    fi
    if [ -z "$hub_tag" ]; then
        hub_tag=latest
    fi

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
            if worker_tag="$(network_fetch "https://dl.fullbars.xyz/latest-version" 2>/dev/null)"; then
                hub_tag="$(printf "%s" "$worker_tag" | tr -d '[:space:]')"
            fi
        fi

        if [ -z "$hub_tag" ]; then
            pr_err "Could not resolve the latest release tag. Try: URNETWORK_HUB_TAG=vX.Y.Z urnet-tools hub update"
            exit 1
        fi
    fi

    pr_info "Target version: %s" "$hub_tag"

    # Idempotency check: skip if already at this version and not forced
    if [ "$force" != "1" ] && [ -f "$hub_version_file" ]; then
        current="$(cat "$hub_version_file")"
        if [ "$current" = "$hub_tag" ]; then
            if [ -x "$hub_bin" ]; then
                pr_info "Hub binary is already at version %s. Nothing to do." "$hub_tag"
                pr_info "Use --force to re-download and reinstall."
                return
            fi
        fi
    fi

    # State tracking for transactional rollback
    _service_was_active=false
    _db_was_backed_up=false
    _binary_was_backed_up=false
    _binary_was_swapped=false

    _restore_and_abort() {
        local fail_step="$1"
        local fail_msg="$2"
        pr_err "%s" "$fail_msg"

        if [ "$_binary_was_swapped" = true ]; then
            pr_warn "Rolling back: restoring previous binary and database..."
            if [ -f "${hub_bin}.old" ]; then
                mv "${hub_bin}.old" "$hub_bin" || pr_warn "Could not restore old binary from ${hub_bin}.old"
            fi
            if [ "$_db_was_backed_up" = true ] && [ -f "${hub_data_dir}/hub.db.bak" ]; then
                cp "${hub_data_dir}/hub.db.bak" "${hub_data_dir}/hub.db" || pr_warn "Could not restore database backup"
                pr_info "Database restored from backup."
            fi
        elif [ "$_binary_was_backed_up" = true ]; then
            pr_warn "Rolling back: restoring previous binary..."
            if [ -f "${hub_bin}.old" ]; then
                mv "${hub_bin}.old" "$hub_bin" || pr_warn "Could not restore old binary"
            fi
        fi

        if [ "$_service_was_active" = true ]; then
            pr_info "Starting previous hub service..."
            systemctl --user start urnetwork-hub.service 2>/dev/null || true
            sleep 1
            if systemctl --user is-active --quiet urnetwork-hub.service 2>/dev/null; then
                pr_info "Hub service restarted with the previous binary."
            else
                pr_warn "Could not restart hub service. Check: journalctl --user -u urnetwork-hub.service -n 30"
            fi
        fi

        pr_err "Hub update failed at: %s" "$fail_step"
        exit 1
    }

    # Step 1: Stop hub service if running
    pr_info "Checking hub service state..."
    if systemctl --user is-active --quiet urnetwork-hub.service 2>/dev/null; then
        _service_was_active=true
        pr_info "Stopping hub service..."
        systemctl --user stop urnetwork-hub.service || _restore_and_abort "stop-service" "Failed to stop hub service"
        pr_info "Hub service stopped."
    else
        pr_info "Hub service is not running."
    fi

    # Step 2: Back up database
    if [ -f "${hub_data_dir}/hub.db" ]; then
        pr_info "Backing up database to hub.db.bak..."
        cp "${hub_data_dir}/hub.db" "${hub_data_dir}/hub.db.bak" || _restore_and_abort "backup-db" "Failed to back up database"
        _db_was_backed_up=true
        pr_info "Database backed up."
    else
        pr_info "No database found — nothing to back up."
    fi

    # Step 3: Download new binary to a temp file on the same filesystem
    # so that the final mv is an atomic rename, not a cross-filesystem copy.
    hub_dl_url="https://dl.fullbars.xyz/releases/download/${hub_tag}/urnetwork-hub-${hub_tag}-linux-${arch}"
    hub_mirror_url="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/${hub_tag}/urnetwork-hub-${hub_tag}-linux-${arch}"
    pr_info "Downloading hub binary from: %s" "$hub_dl_url"

    mkdir -p "$install_path/bin"
    _tmp_hub="${hub_bin}.new"

    if ! download_asset "$hub_dl_url" "$_tmp_hub"; then
        pr_warn "Primary download failed, trying GitHub mirror..."
        rm -f "$_tmp_hub"
        if ! download_asset "$hub_mirror_url" "$_tmp_hub"; then
            rm -f "$_tmp_hub"
            _restore_and_abort "download" "Failed to download hub binary from both primary and mirror"
        fi
    fi
    pr_info "Download complete."

    # Step 4: Verify downloaded binary
    chmod 755 "$_tmp_hub"
    if ! _tmp_version="$("$_tmp_hub" --version 2>/dev/null)"; then
        rm -f "$_tmp_hub"
        _restore_and_abort "verify-binary" "Downloaded binary is not executable or does not report a version"
    fi
    pr_info "Downloaded binary version: %s" "$(printf '%s' "$_tmp_version" | head -n 1)"

    # Step 5: Back up current binary (copy, not move, so the binary is
    # never missing even if the user hits Ctrl+C mid-update).
    if [ -f "$hub_bin" ]; then
        pr_info "Backing up current binary to ${hub_bin}.old..."
        cp "$hub_bin" "${hub_bin}.old" || _restore_and_abort "backup-binary" "Failed to back up current binary"
        _binary_was_backed_up=true
        pr_info "Binary backed up."
    fi

    # Step 6: Atomic swap (same-filesystem rename — either succeeds or
    # leaves the old binary untouched).
    pr_info "Installing new binary..."
    if ! mv "$_tmp_hub" "$hub_bin"; then
        _restore_and_abort "swap-binary" "Failed to install new binary at $hub_bin"
    fi
    _binary_was_swapped=true
    chmod 755 "$hub_bin"
    pr_info "New binary installed at: %s" "$hub_bin"

    # Step 7: Ensure systemd unit exists
    if [ ! -f "$hub_service" ]; then
        pr_info "Systemd unit not found — installing hub service unit..."
        mkdir -p "$hub_service_dir"
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
        systemctl --user daemon-reload || _restore_and_abort "daemon-reload" "Failed to reload systemd after creating unit"
        pr_info "Hub service unit installed."
    fi

    # Step 8: daemon-reload (in case unit changed or binary path updated)
    systemctl --user daemon-reload || _restore_and_abort "daemon-reload" "Failed to reload systemd"

    # Step 9: Start hub service
    if [ "$_service_was_active" = true ]; then
        pr_info "Starting hub service..."
        systemctl --user start urnetwork-hub.service || _restore_and_abort "start-service" "Failed to start hub service after update"
        sleep 2
        if ! systemctl --user is-active --quiet urnetwork-hub.service 2>/dev/null; then
            _restore_and_abort "verify-service" "Hub service started but is not active. Check: journalctl --user -u urnetwork-hub.service -n 30"
        fi
        pr_info "Hub service started successfully."
    else
        pr_info "Hub service was not previously running — leaving it stopped."
        pr_info "Start it with: systemctl --user start urnetwork-hub.service"
    fi

    # Step 10: Write version file
    printf '%s\n' "$hub_tag" > "$hub_version_file"
    pr_info "Version recorded: %s" "$hub_tag"

    # Cleanup binary backup
    if [ -f "${hub_bin}.old" ]; then
        rm -f "${hub_bin}.old"
        pr_info "Binary backup removed."
    fi

    pr_info ""
    pr_info "Hub updated successfully to %s." "$hub_tag"
    if [ -f "${hub_data_dir}/hub.db.bak" ]; then
        pr_info "Database backup preserved at: ${hub_data_dir}/hub.db.bak"
    fi
    if [ "$_service_was_active" = true ]; then
        pr_info "Dashboard: http://localhost:8080"
        if [ -f "${hub_data_dir}/tls.crt" ]; then
            pr_info "TLS dashboard: https://localhost:8443"
        fi
    fi
}

do_hub_init () {
    hub_data_dir="$HOME/.local/share/urnetwork-hub"
    ca_cert="$hub_data_dir/ca.crt"
    password=""

    while [ $# -gt 0 ]; do
        case "$1" in
            --password|-p)
                if [ -z "$2" ]; then
                    pr_err "Option --password requires a value."
                    exit 1
                fi
                password="$2"
                shift 2
                ;;
            *)
                pr_err "Unknown argument: %s (usage: init [--password <pw>])" "$1"
                exit 1
                ;;
        esac
    done

    if [ "$has_systemd" -eq 0 ]; then
        pr_err "systemd is not available on this system"
        exit 1
    fi

    if [ -f "$ca_cert" ]; then
        if command -v openssl > /dev/null; then
            fp=$(openssl x509 -in "$ca_cert" -noout -fingerprint -sha256 2>/dev/null | cut -d= -f2 | tr -d ':' | tr '[:upper:]' '[:lower:]')
            fp="SHA256:${fp}"
        else
            fp="(openssl not found)"
        fi
        pr_info "Hub CA is already initialized."
        pr_info "CA fingerprint: %s" "$fp"
        pr_info ""
        pr_info "On each provider, run:"
        pr_info "  urnet-tools hub link https://<this-host>:8443"
        pr_info ""
        pr_info "Or use the onboard token for zero-touch provisioning:"
        pr_info "  urnet-tools hub onboard-cmd"
        return
    fi

    if [ -f "$hub_data_dir/hub.password" ] && [ ! -f "$ca_cert" ]; then
        pr_warn "hub.password exists but ca.crt is missing — a previous init may have"
        pr_warn "been interrupted. Re-running init will overwrite the existing password."
        pr_warn "If you continue, you may need to re-link all providers."
        pr_warn ""
        case "$(printf '%s' "${HUB_LINK_YES:-0}" | tr '[:upper:]' '[:lower:]')" in
            1|yes|true|y) ;;
            *)
                printf "Proceed? (y/n) "
                read -r answer
                case "$answer" in
                    [Yy]|[Yy][Ee][Ss]) ;;
                    *) pr_err "Aborted by user."; exit 1 ;;
                esac
                ;;
        esac
    fi

    # Write password to hub.data dir before start so hub derives CA from it
    if [ -n "$password" ]; then
        mkdir -p "$hub_data_dir"
        printf '%s' "$password" > "$hub_data_dir/hub.password.tmp"
        mv "$hub_data_dir/hub.password.tmp" "$hub_data_dir/hub.password"
        chmod 600 "$hub_data_dir/hub.password"
        pr_info "Password written to hub data directory."
    else
        pr_info "No password provided — hub will auto-generate one."
        pr_info "After init, run 'urnet-tools hub show-password' to retrieve it."
    fi

    # Enable TLS on the hub by writing URNETWORK_HUB_TLS_ADDR into its
    # systemd drop-in. The hub binary reads this env var and starts an
    # HTTPS listener on the given address.
    override_set_env_for_hub "URNETWORK_HUB_TLS_ADDR" ":8443"
    systemctl --user daemon-reload || { pr_err "daemon-reload failed"; exit 1; }

    if systemctl --user is-active --quiet urnetwork-hub.service 2>/dev/null; then
        systemctl --user restart urnetwork-hub.service || { pr_err "Failed to restart hub"; exit 1; }
    else
        if systemctl --user is-enabled urnetwork-hub.service 2>/dev/null | grep -q 'masked'; then
            pr_err "urnetwork-hub.service is masked — unmask it first: systemctl --user unmask urnetwork-hub.service"
            exit 1
        fi
        systemctl --user start urnetwork-hub.service || { pr_err "Failed to start hub"; exit 1; }
    fi

    pr_info "Hub started with TLS. Waiting for CA certificate..."
    sleep 5

    if [ ! -f "$ca_cert" ]; then
        pr_err "CA certificate not generated. Check hub logs:"
        pr_err "  journalctl --user -u urnetwork-hub.service --no-pager -n 30"
        exit 1
    fi

    if command -v openssl > /dev/null; then
        fingerprint=$(openssl x509 -in "$ca_cert" -noout -fingerprint -sha256 2>/dev/null | cut -d= -f2 | tr -d ':' | tr '[:upper:]' '[:lower:]')
        fingerprint="SHA256:${fingerprint}"
    else
        fingerprint="(openssl not found)"
    fi

    pr_info ""
    pr_info "Ensure port 8443 is open in your firewall so providers can reach the hub:"
    firewall_hint 8443 || pr_info "  (open port 8443/tcp in your firewall)"

    pr_info "Hub TLS is ready."
    pr_info "CA fingerprint: %s" "$fingerprint"
    pr_info ""
    pr_info "On each provider, run:"
    pr_info "  urnet-tools hub link https://<this-host>:8443"
    pr_info ""
    pr_info "Or mint an onboard token for the whole fleet at once:"
    pr_info "  urnet-tools hub onboard-cmd"
}

do_hub_show_password () {
    hub_data_dir="$HOME/.local/share/urnetwork-hub"
    if [ ! -x "$hub_bin" ]; then
        pr_err "Hub binary not executable at %s" "$hub_bin"
        exit 1
    fi
    pr_info "Hub password (keep this secret, do not paste it anywhere public):"
    pr_info ""
    "$hub_bin" -show-password -data "$hub_data_dir" || exit 1
}

do_hub_onboard_cmd () {
    hub_data_dir="$HOME/.local/share/urnetwork-hub"
    if [ ! -x "$hub_bin" ]; then
        pr_err "Hub binary not executable at %s" "$hub_bin"
        exit 1
    fi

    output=$("$hub_bin" -mint-onboard-token -data "$hub_data_dir") || {
        pr_err "Failed to mint onboard token:"
        pr_err "%s" "$output"
        exit 1
    }

    token=$(printf '%s' "$output" | sed -n 's/^Token: *//p')
    expires=$(printf '%s' "$output" | sed -n 's/^Expires: *//p')
    ca_fp=$(printf '%s' "$output" | sed -n 's/^CA fingerprint: *//p')

    if [ -z "$token" ] || [ -z "$expires" ]; then
        pr_err "Unexpected output from hub:"
        printf '%s\n' "$output"
        exit 1
    fi

    # Best-effort local IP for the one-liner
    local_ip=""
    if command -v hostname > /dev/null; then
        # Prefer the first IPv4 address — IPv6 needs brackets in URLs
        local_ip=$(hostname -I 2>/dev/null | tr ' ' '\n' | grep -m1 '\.')
    fi

    pr_info "Token:      %s" "$token"
    pr_info "Expires:    %s" "$expires"
    pr_info "CA fingerprint: %s" "$ca_fp"
    pr_info ""
    pr_info "On each provider, run this one-liner:"
    pr_info ""

    if [ -n "$local_ip" ]; then
        pr_info "  curl -fsSL http://%s:8080/onboard.sh | sh -s -- %s" "$local_ip" "$token"
        pr_info ""
        pr_info "  (if %s is not the address providers reach, substitute the correct host)" "$local_ip"
    else
        pr_info "  curl -fsSL http://<this-host>:8080/onboard.sh | sh -s -- %s" "$token"
    fi

    pr_info ""
    pr_info "The token is reusable for 15 minutes — paste once and onboard the whole fleet."
}

do_hub_link () {
    url=""
    token=""

    while [ $# -gt 0 ]; do
        case "$1" in
            --token)
                if [ -z "$2" ]; then
                    pr_err "Option --token requires a value."
                    exit 1
                fi
                token="$2"
                shift 2
                ;;
            *)
                if [ -z "$url" ]; then
                    url="$1"
                    shift
                else
                    pr_err "Unexpected argument: %s" "$1"
                    exit 1
                fi
                ;;
        esac
    done

    if [ -z "$url" ]; then
        pr_err "Usage: urnet-tools hub link <https://hub-host:port> [--token <onboard-token>]"
        pr_err "Fetches the hub's CA certificate and configures TLS trust so all"
        pr_err "future reports are encrypted and verified."
        exit 1
    fi

    case "$url" in
        https://*) ;;
        *)
            pr_err "Hub link URL must start with https://"
            exit 1
            ;;
    esac

    url="${url%/}"
    hub_dir="$HOME/.urnetwork"
    ca_file="$hub_dir/hub_ca.pem"
    pin_file="$hub_dir/hub.pin"
    report_file="$hub_dir/report_url"

    # Warn if linking would reconfigure a provider tied to a different host
    extract_host() {
        h="$1"
        h="${h#https://}"
        h="${h#http://}"
        h="${h%%:*}"
        printf '%s' "$h" | tr '[:upper:]' '[:lower:]'
    }
    if [ -f "$report_file" ]; then
        old_host=$(extract_host "$(cat "$report_file" | tr -d '\n')")
        new_host=$(extract_host "$url")
        if [ "$old_host" != "$new_host" ]; then
            pr_warn "This provider directory is already linked to a different hub host."
            pr_warn "  Current: %s" "$old_host"
            pr_warn "  New:     %s" "$new_host"
            pr_warn ""
            pr_warn "Linking to a different host will reconfigure all providers sharing"
            pr_warn "this directory — containers with bind mounts, native installs on"
            pr_warn "the same user, etc."
            pr_warn ""
        case "$(printf '%s' "${HUB_LINK_YES:-0}" | tr '[:upper:]' '[:lower:]')" in
            1|yes|true|y) ;;
            *)
                printf "Proceed? (y/n) "
                read -r answer
                case "$answer" in
                    [Yy]|[Yy][Ee][Ss]) ;;
                    *) pr_err "Aborted by user."; exit 1 ;;
                esac
                ;;
        esac
        fi
    fi

    # Token-based flow: fetch CA cert from the onboarding endpoint
    if [ -n "$token" ]; then
        pr_info "Fetching hub CA certificate via onboard token..."
        encoded_token=$(printf '%s' "$token" | sed 's/+/%2B/g; s/=/%3D/g; s/\//%2F/g')
        cert_json=$(tls_fetch "${url}/api/ca-cert?token=${encoded_token}") || {
            pr_err "Could not reach hub at %s with the given token." "$url"
            pr_err "Is the hub running and is the token still valid (15 min TTL)?"
            exit 1
        }

        ca_pem=$(printf '%s' "$cert_json" | sed -n 's/.*"ca_pem" *: *"\([^"]*\)".*/\1/p')
        ca_fingerprint=$(printf '%s' "$cert_json" | sed -n 's/.*"ca_fingerprint" *: *"\([^"]*\)".*/\1/p')

        if [ -z "$ca_pem" ]; then
            pr_err "Hub responded but did not return a CA certificate (may be running an older version)."
            pr_err "Response: %s" "$cert_json"
            exit 1
        fi

        pr_info ""
        pr_info "Hub CA fingerprint: %s" "$ca_fingerprint"
        pr_info ""

        # Write CA cert: decode JSON-escaped PEM (embedded \n) to real newlines
        mkdir -p "$hub_dir"
        printf '%s' "$ca_pem" | sed 's/\\n/\n/g' > "$ca_file.tmp"
        mv "$ca_file.tmp" "$ca_file"
        chmod 600 "$ca_file"
        rm -f "$pin_file"
        pr_info "CA certificate saved to %s" "$ca_file"
    else
        # Legacy/TOFU flow: fetch /api/cert (which now returns ca_pem too)
        pr_info "Fetching hub certificate from %s/api/cert ..." "$url"
        cert_json=$(tls_fetch "${url}/api/cert") || {
            pr_err "Could not reach hub at %s. Is the hub running and reachable?" "$url"
            exit 1
        }

        ca_pem=$(printf '%s' "$cert_json" | sed -n 's/.*"ca_pem" *: *"\([^"]*\)".*/\1/p')
        ca_fingerprint=$(printf '%s' "$cert_json" | sed -n 's/.*"ca_fingerprint" *: *"\([^"]*\)".*/\1/p')
        # Also try legacy fingerprint field (old hubs without CA support)
        legacy_fp=$(printf '%s' "$cert_json" | sed -n 's/.*"fingerprint" *: *"\([^"]*\)".*/\1/p')

        if [ -n "$ca_pem" ]; then
            # New CA-capable hub
            pr_info ""
            pr_info "Hub CA fingerprint:"
            pr_info "  %s" "$ca_fingerprint"
            pr_info ""

            case "$(printf '%s' "${HUB_LINK_YES:-0}" | tr '[:upper:]' '[:lower:]')" in
                1|yes|true|y) ;;
                *)
                    printf "Accept this fingerprint? (y/n) "
                    read -r answer
                    case "$answer" in
                        [Yy]|[Yy][Ee][Ss]) ;;
                        *) pr_err "Aborted by user."; exit 1 ;;
                    esac
                    ;;
            esac

            mkdir -p "$hub_dir"
            printf '%s' "$ca_pem" | sed 's/\\n/\n/g' > "$ca_file.tmp"
            mv "$ca_file.tmp" "$ca_file"
            chmod 600 "$ca_file"
            rm -f "$pin_file"
            pr_info "CA certificate saved to %s" "$ca_file"
        elif [ -n "$legacy_fp" ]; then
            # Legacy hub (old TOFU flow)
            pr_warn "Hub does not support CA-based trust (missing ca_pem in response)."
            pr_warn "Falling back to legacy fingerprint pinning."

            pr_info ""
            pr_info "Hub certificate fingerprint:"
            pr_info "  %s" "$legacy_fp"
            pr_info ""

            case "$(printf '%s' "${HUB_LINK_YES:-0}" | tr '[:upper:]' '[:lower:]')" in
                1|yes|true|y) ;;
                *)
                    printf "Accept this fingerprint? (y/n) "
                    read -r answer
                    case "$answer" in
                        [Yy]|[Yy][Ee][Ss]) ;;
                        *) pr_err "Aborted by user."; exit 1 ;;
                    esac
                    ;;
            esac

            mkdir -p "$hub_dir"
            printf '%s\n' "$legacy_fp" > "$pin_file.tmp"
            mv "$pin_file.tmp" "$pin_file"
            pr_info "Fingerprint pinned to %s" "$pin_file"
        else
            pr_err "Could not extract CA certificate or fingerprint from hub response."
            pr_err "Response: %s" "$cert_json"
            exit 1
        fi
    fi

    printf '%s\n' "$url" > "$report_file.tmp"
    mv "$report_file.tmp" "$report_file"
    pr_info "Report URL set to %s" "$url"

    # Clean up stale systemd env overrides that could confuse the report URL
    # after an older urnet-tools version wrote them. The provider reads the
    # file above at runtime (resolveReportURL), but the env var in the
    # override file would still take effect on next restart if left behind.
    if [ "$has_systemd" -eq 1 ]; then
        override_rm_env "URNETWORK_REPORT_URL"
        if [ -f "$hub_conf" ]; then
            rm -f "$hub_conf"
            rmdir "$override_dir" 2>/dev/null || true
        fi
        systemctl --user daemon-reload 2>/dev/null || true
    fi

    pr_info ""
    pr_info "Success. The provider will now send encrypted reports to %s." "$url"
    pr_info "The change takes effect on the next report tick (no restart needed)."
}

do_hub_unlink () {
    hub_dir="$HOME/.urnetwork"
    pin_file="$hub_dir/hub.pin"
    ca_file="$hub_dir/hub_ca.pem"
    report_file="$hub_dir/report_url"

    rm -f "$pin_file"
    pr_info "Removed %s" "$pin_file"
    if [ -f "$ca_file" ]; then
        rm -f "$ca_file"
        pr_info "Removed %s" "$ca_file"
    fi

    # Rewrite the report URL from https:// to http:// on the same host, port 8080,
    # if it currently points to an HTTPS URL.
    if [ -f "$report_file" ]; then
        current="$(cat "$report_file")"
        case "$current" in
            https://*)
                # Extract host from https://host:port → http://host:8080
                host_port="${current#https://}"
                host="${host_port%%:*}"
                new_url="http://${host}:8080"
                printf '%s\n' "$new_url" > "$report_file.tmp"
                mv "$report_file.tmp" "$report_file"
                pr_info "Report URL changed to %s (insecure)" "$new_url"
                ;;
            *)
                pr_info "Report URL is %s (not HTTPS, left unchanged)" "$current"
                ;;
        esac
    fi

    # Also clean up any stale URNETWORK_REPORT_URL in systemd overrides so
    # a future restart doesn't re-enable a URL the operator just unlinked.
    if [ "$has_systemd" -eq 1 ]; then
        override_rm_env "URNETWORK_REPORT_URL"
        systemctl --user daemon-reload 2>/dev/null || true
    fi

    pr_info ""
    pr_info "Unlinked. Reports are no longer encrypted."
    pr_info "To re-link, run: urnet-tools hub link https://<hub-host>:8443"
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

do_session ()
{
    action="$1"
    file="$2"
    state_dir="$HOME/.urnetwork"
    staging_dir="$state_dir/.session-staging"
    provider_bin="$install_path/bin/urnetwork"

    case "$action" in
        save)
            if [ -z "$file" ]; then
                pr_err "Usage: urnet-tools session save <file>"
                exit 1
            fi

            pr_info "WARNING: This bundle contains full identity and reputation"
            pr_info "credentials for this provider's fleet. Treat it like a password."
            printf "\n"

            printf "Enter encryption passphrase (will NOT echo): "
            stty -echo
            read -r pass1 < /dev/tty
            stty echo
            printf "\n"
            printf "Confirm passphrase: "
            stty -echo
            read -r pass2 < /dev/tty
            stty echo
            printf "\n"
            if [ "$pass1" != "$pass2" ]; then
                pr_err "Passphrases do not match."
                exit 1
            fi

            files=""
            for f in .client_jwts.json jwt jwt_last_refresh .provider.key .provider.cert proxy proxy_url.json proxy.state; do
                if [ -f "$state_dir/$f" ]; then
                    files="$files $f"
                fi
            done

            _pf="$(mktemp /tmp/urnsession-XXXXXX)"
            printf '%s' "$pass1" > "$_pf"
            chmod 600 "$_pf"
            set -o pipefail
            if ! tar -czf - -C "$state_dir" $files 2>/dev/null | openssl enc -aes-256-cbc -pbkdf2 -salt -pass "file:$_pf" -out "$file"; then
                rm -f "$_pf"
                pr_err "Failed to create session bundle."
                exit 1
            fi
            set +o pipefail
            rm -f "$_pf"

            chmod 600 "$file" 2>/dev/null
            pr_info "Session saved to %s" "$file"
            ;;

        load)
            if [ -z "$file" ]; then
                pr_err "Usage: urnet-tools session load <file>"
                exit 1
            fi
            if [ ! -f "$file" ]; then
                pr_err "Session file '%s' not found." "$file"
                exit 1
            fi

            if [ ! -x "$provider_bin" ]; then
                pr_err "Provider binary not found at %s. Is it installed?" "$provider_bin"
                exit 1
            fi

            # Scan trailing args for --force/-f (global --force is consumed
            # before the operation word; the session subcommand sees $3+).
            shift 2
            for arg in "$@"; do
                if [ "$arg" = "--force" ] || [ "$arg" = "-f" ]; then
                    FORCE=1
                fi
            done

            printf "Enter passphrase: "
            stty -echo
            read -r pass < /dev/tty
            stty echo
            printf "\n"

            tmpdir="$state_dir/.session-tmp-$$"
            mkdir -p "$tmpdir"

            _pf="$(mktemp /tmp/urnsession-XXXXXX)"
            printf '%s' "$pass" > "$_pf"
            chmod 600 "$_pf"
            set -o pipefail
            if ! openssl enc -d -aes-256-cbc -pbkdf2 -pass "file:$_pf" -in "$file" | tar -xzf - -C "$tmpdir"; then
                rm -f "$_pf"
                rm -rf "$tmpdir"
                pr_err "Failed to decrypt session bundle (wrong passphrase or corrupt file)."
                exit 1
            fi
            set +o pipefail
            rm -f "$_pf"

            if [ ! -f "$tmpdir/jwt" ]; then
                pr_err "Session bundle is missing 'jwt' file. Is this a valid session bundle?"
                rm -rf "$tmpdir"
                exit 1
            fi

            current_id=""
            if [ -f "$state_dir/jwt" ]; then
                current_id="$("$provider_bin" print-network-id "$state_dir/jwt" 2>/dev/null)"
            fi
            new_id="$("$provider_bin" print-network-id "$tmpdir/jwt" 2>/dev/null)"

            if [ -z "$new_id" ]; then
                pr_err "Could not extract network_id from the session bundle's JWT."
                pr_err "The bundle may be corrupted or the JWT is invalid."
                rm -rf "$tmpdir"
                exit 1
            fi

            if [ -n "$current_id" ] && [ "$new_id" != "$current_id" ]; then
                pr_err "Network ID mismatch."
                pr_err "  Current account: %s" "$current_id"
                pr_err "  Session account: %s" "$new_id"
                pr_err "Session bundles can only be loaded under the same URnetwork account."
                if [ "$FORCE" != "1" ]; then
                    rm -rf "$tmpdir"
                    exit 1
                fi
                pr_info "Proceeding anyway (--force)."
            fi

            backup_dir="$state_dir/.session-backup-$(date +%Y%m%d-%H%M%S)"
            mkdir -p "$backup_dir"
            for f in .client_jwts.json jwt jwt_last_refresh .provider.key .provider.cert proxy proxy_url.json proxy.state; do
                if [ -f "$state_dir/$f" ]; then
                    cp "$state_dir/$f" "$backup_dir/$f"
                fi
            done
            pr_info "Backed up current session to %s" "$backup_dir"

            rm -rf "$staging_dir"
            mkdir -p "$staging_dir"
            for f in .client_jwts.json jwt jwt_last_refresh .provider.key .provider.cert proxy proxy_url.json proxy.state; do
                if [ -f "$tmpdir/$f" ]; then
                    mv "$tmpdir/$f" "$staging_dir/$f"
                fi
            done
            rm -rf "$tmpdir"

            touch "$state_dir/.session-pending"

            # Warn about entries near the 30-day stale cutoff
            if [ -f "$staging_dir/.client_jwts.json" ]; then
                mint_count=$(grep -c '"minted_at"' "$staging_dir/.client_jwts.json" 2>/dev/null || echo 0)
                pr_info "Session contains %s client JWT entries." "$mint_count"
                pr_info "Note: entries older than 30 days will be automatically pruned"
                pr_info "on next startup."
            fi

            printf "\n"
            while true; do
                printf "Restart provider now to apply loaded session? (Y/n): "
                read -r yn < /dev/tty
                case $yn in
                    [Nn]*) 
                        pr_info "Session staged. Run 'urnet-tools restart' when ready."
                        break
                        ;;
                    [Yy]* | "")
                        pr_info "Restarting urnetwork.service..."
                        systemctl --user restart urnetwork.service || { pr_err "Failed to restart urnetwork.service"; exit 1; }
                        pr_info "Service restarted with new session."
                        break
                        ;;
                    *) printf "Please answer yes or no.\n" ;;
                esac
            done
            ;;
    
        *)
            pr_err "Usage: urnet-tools session <save|load> <file>"
            exit 1
            ;;
    esac
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
        # Parse -y/-f/--yes/--force flags after the subcommand
        _restart_force=0
        while [ $# -gt 0 ]; do
            case "$1" in
                -y|--yes|-f|--force)
                    _restart_force=1
                    shift
                    ;;
                *)
                    pr_err "Unknown option: %s" "$1"
                    exit 1
                    ;;
            esac
        done
        if [ "$_restart_force" = "1" ]; then
            FORCE=1
        fi
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

    hot)
        shift
        case "$1" in
            restart)
                shift
                toggle_hotrestart "$@"
                exit 0
                ;;
            *)
                pr_err "Usage: urnet-tools hot restart <on|off>"
                exit 1
                ;;
        esac
        ;;
    hot-restart)
        toggle_hotrestart "$@"
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

    fast)
        shift
        case "$1" in
            auth)
                shift
                do_fast_auth "$@"
                exit 0
                ;;
            *)
                pr_err "Usage: urnet-tools fast auth [on|off]"
                exit 1
                ;;
        esac
        ;;
    fast-auth)
        do_fast_auth "$@"
        exit 0
        ;;

    self-heal)
        do_self_heal "$@"
        exit 0
        ;;

    set)
        do_set "$@"
        exit 0
        ;;

    session)
        do_session "$@"
        exit 0
        ;;

    *)
        pr_err "Invalid operation '%s'" "$operation"
        exit 1
        ;;
esac
