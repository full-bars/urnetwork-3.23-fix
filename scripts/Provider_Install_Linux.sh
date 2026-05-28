#!/bin/sh
# Credits: Ar Rakin, Ryan Mello
# urnet-tools -- URnetwork manager script (also acts as an installation script)
# GitHub: <https://github.com/full-bars/urnetwork-3.23-fix>

me="$0"
script_rundir="$(pwd)"

if [ "$me" = "sh" ] || [ "$me" = "bash" ] || [ "$me" = "zsh" ] || [ "$URNETWORK_TOOLS_MODE" = "1" ]; then
    me="urnet-tools"
fi

show_help ()
{
    echo "Usage: "
    
    if [ -n "$URNETWORK_TOOLS_MODE" ]; then
        echo "  $me [options] start"
        echo "  $me [options] stop"
        echo "  $me [options] update"
        echo "  $me [options] status"
        echo "  $me [options] reinstall [-t=TAG] [-B]"
        echo "  $me [options] uninstall [-B]"
        echo "  $me [options] auto-update [on | off] [--interval=INTERVAL]"
        echo "  $me [options] auto-start [on | off]"
        echo "  $me [-h] [-v]"
    else
	    echo "  $me [options] [-t=TAG] [-B]"
    fi
    
    echo ""

    if [ -z "$URNETWORK_TOOLS_MODE" ]; then    
        echo "Installs URnetwork locally for the current user."
    else
	    echo "Manages URnetwork installation."
    fi
    
    if [ -n "$URNETWORK_TOOLS_MODE" ]; then
        echo ""
        echo "Operational modes:"
        echo "  start                   Start URnetwork provider"
        echo "  stop                    Stop URnetwork provider"
        echo "  update                  Upgrade URnetwork, if updates are available"
        echo "  status                  Show the status of URnetwork provider service"
        echo "  logs                    Stream the provider logs (RAM or journald)"
        echo "  eco <on|off>            🌿 Toggle eco mode (GC-tuned for low-RAM systems, full throughput)"
        echo "  lowmode <on|off>        Toggle low-memory mode (reduced buffers, max RAM savings)"
        echo "  $me [options] turbo <v4|v8|off>       🚀 Turbo mode: raise throughput limits for RAM-rich boxes"
        echo "                          v4=4MiB window, v8=8MiB window (higher ceilings for low-RTT paths)"
        echo "  $me [options] auto <on|off>           🧠 Auto-Tune: detect hardware and pick best performance profile"
        echo "  $me [options] optimize          ⚡ Optimize OS limits (ulimit, conntrack) for high volume"
        echo "  $me [options] ramlogs <on|off>        Toggle RAM-disk logging (Zero Disk I/O)"

        echo "  reinstall               Reinstall URnetwork"
        echo "  uninstall               Uninstall URnetwork"
        echo "  auto-update             Manage auto update settings.  If no argument is"
        echo "                          specified, it will print the current auto update state."
		echo "  auto-start              Turn auto-start of URnetwork provider on login on or off"
        echo ""
        echo "Options for reinstall:"
        echo "  -t, --tag=TAG           Reinstall a specific version of URnetwork."
        echo "  -B, --no-modify-bashrc  Do not modify ~/.bashrc"
        echo ""
        echo "Options for uninstall:"
        echo "  -B, --no-modify-bashrc  Do not modify ~/.bashrc"
        echo ""
        echo "Options for auto-update:"
        echo "  --interval=INTERVAL     Auto update interval.  Values can be:"
        echo "                          daily, weekly, monthly.  Defaults to daily."
    fi

    echo ""
    echo "Options for install:"
    echo "  -t, --tag=TAG           Reinstall a specific version of URnetwork."
    echo "  -B, --no-modify-bashrc  Do not modify ~/.bashrc"
    
    echo ""
    echo "Global Options:"
    echo "  -h, --help              Show this help and exit"
    echo "  -v, --version           Show the version of URnetwork that's installed"
    echo "  -i, --install=[PATH]    Installation path"
    echo "  -4, --ipv4              Force IPv4 for network requests (fixes IPv6 hangs)"
    echo "  -f, --force             Force optimization / skip confirmation prompts"
    echo ""
    echo "Refer to the online documentation for more help."
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
update_timer_oncalendar="Sun *-*-* 00:00:00 UTC"

api_base="https://api.github.com/repos/full-bars/urnetwork-3.23-fix"

install_path="$HOME/.local/share/urnetwork-provider"
version_file="$install_path/.version"

if [ -z "$URNETWORK_TOOLS_MODE" ]; then
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

    # shellcheck disable=SC2068
    # shellcheck disable=SC2059
    printf "$argv0: $fmt\n" $@ >&2
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

    # shellcheck disable=SC2068
    # shellcheck disable=SC2059
    printf "$argv0: $fmt\n" $@
}

pr_warn ()
{
    argv0="\033[1;33m$me\033[0m"
    fmt="$1"
    shift

    if [ -n "$operation" ]; then
        argv0="$argv0: $operation"
    fi

    # shellcheck disable=SC2068
    # shellcheck disable=SC2059
    printf "$argv0: $fmt\n" $@
}

opt_requires_arg ()
{
    pr_err "Option '%s' requires an argument" "$1"
    pr_err "Try '$me --help' for more information"
}

get_version_from_api_response () 
{    
    if command -v jq > /dev/null; then
        latest_version="$(echo "$1" | tr -d '\000-\037' | jq -r '.tag_name' 2>/dev/null)"
    elif command -v python3 > /dev/null; then
        latest_version="$(echo "$1" | tr -d '\000-\037' | python3 -c 'import sys, json;
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
        date="$(echo "$1" | tr -d '\000-\037' | jq -r '.published_at | fromdateiso8601' 2>/dev/null)"
    elif command -v python3 > /dev/null; then
        date="$(echo "$1" | tr -d '\000-\037' | python3 -c 'import sys, json;
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
    if command -v curl > /dev/null; then
        if [ "$FORCE_IPV4" -eq 1 ]; then
            curl -4 --connect-timeout 10 -fSsL "$1"
        else
            curl --connect-timeout 10 -fSsL "$1"
        fi
        return $?
    elif command -v wget > /dev/null; then
        if [ "$FORCE_IPV4" -eq 1 ]; then
            wget -4 --connect-timeout=10 -qO- "$1"
        else
            wget --connect-timeout=10 -qO- "$1"
        fi
        return $?
    else
        pr_err "Neither curl nor wget is available"
        exit 1
    fi
}

show_version () 
{
    if [ ! -f "$version_file" ]; then
        pr_err "version file '$version_file' could not be found"
        exit 1
    fi

    version="$(cat "$version_file")"

    echo "Current version: $version"
    
    api_url="$api_base/releases/latest"
    release="$(network_fetch "$api_url")"
    latest_version="$(get_version_from_api_response "$release")"

    if [ -z "$latest_version" ]; then
        pr_err "Could not fetch any information about the latest release"
        exit 1
    fi

    if [ "$latest_version" != "$version" ]; then
        echo "Latest version (Update available): $latest_version"
    fi
}

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

        --)
            shift
            break
            ;;

        -*)
            pr_err "Invalid option '%s'" "$1"
            exit 1
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
            if [ "$operation" = "update" ]; then
                pr_info "urnetwork.service is running — binary will be updated on disk."
                pr_info "Restart the service when convenient to apply the update: systemctl --user restart urnetwork.service"
                systemd_units_stopped=0

                systemctl --user disable --now urnetwork-update.timer || {
                    pr_err "Failed to disable urnetwork-update.timer before update; continuing anyway"
                }
                return
            fi

            pr_err "warning: urnetwork.service is running, it will be stopped to perform a reinstall"
            pr_err "warning: It will be started again automatically, once the reinstall finishes"
            pr_err "warning: You will need to restart this service after this reinstall if auto start fails"
            systemd_units_stopped=1
        fi

        systemctl --user disable --now urnetwork.service || {
            pr_err "Failed to disable urnetwork.service early before reinstall; continuing anyway"
        }

        systemctl --user disable --now urnetwork-update.timer || {
            pr_err "Failed to disable urnetwork-update.timer before reinstall; continuing anyway"
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

do_install ()
{
    tag="latest"
    no_modify_bashrc=0

    case "$operation" in
        install)
            if [ -n "$URNETWORK_TOOLS_MODE" ]; then
                pr_err "Invalid operation '%s'" "$operation"
                exit 1
            fi

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

            while [ $# -gt 0 ]; do
                case "$1" in
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

    if ! release="$(network_fetch "$api_url" 2>/dev/null)"; then
        pr_err "Failed to fetch release information for tag: %s" "$tag"
        exit 1
    fi

    version_to_install="$(get_version_from_api_response "$release" 2>/dev/null)"
    release_date="$(get_release_date_from_api_response "$release" 2>/dev/null)"

    if [ "$operation" = "update" ] && [ -f "$install_path/.date" ] && [ -f "$install_path/.version" ]; then
        install_release_date="$(cat "$install_path/.date")"
        installed_version="$(cat "$install_path/.version")"

        # If GitHub API parsing failed, skip update check and proceed
        if [ -z "$release_date" ] || [ -z "$version_to_install" ]; then
            pr_info "Installed version is up-to-date"
            exit 0
        fi

        if [ "$install_release_date" -lt "$release_date" ]; then
            pr_info "Version %s is newer than the installed version %s" "$version_to_install" "$installed_version"
            pr_info "Continuing upgrade"
        else
            pr_info "Installed version is up-to-date"
            exit 0
        fi
    fi

    dl_url=""

    if command -v jq > /dev/null; then
        asset="$(echo "$release" | tr -d '\000-\037' | jq -r '.assets[] | select(.name | startswith("urnetwork-provider-"))' 2>/dev/null)"

        if [ -n "$asset" ]; then
            dl_url="$(echo "$asset" | jq -r '.browser_download_url' 2>/dev/null)"
        fi
    fi

    # Fall back to Python if jq failed or is not available
    if [ -z "$dl_url" ] && command -v python3 > /dev/null; then
        dl_url="$(echo "$release" | tr -d '\000-\037' | python3 -c 'import sys, json
try:
    data = json.load(sys.stdin)
    assets = data["assets"]
    asset = next((a for a in assets if a["name"].startswith("urnetwork-provider")), None)
    print(asset["browser_download_url"] if asset else "")
except (json.JSONDecodeError, KeyError, StopIteration):
    print("")
' 2>/dev/null)"
    fi

    if [ -z "$dl_url" ]; then
        pr_err "No download URL could be found for tag: %s (GitHub API may be unreachable or returning invalid data)" "$tag"
        exit 1
    fi
    
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
        if command -v curl > /dev/null; then
            if ! curl --progress-bar -L "$dl_url" -o "$tarball"; then
                pr_err "Failed to download $dl_url"
                exit 1
            fi
        elif command -v wget > /dev/null; then
            if ! wget -O "$tarball" "$dl_url"; then 
                pr_err "Failed to download $dl_url"
                exit 1
            fi
        else
            pr_err "Neither curl nor wget is available"
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
        cp "$bin_program" "$install_path/bin/urnetwork" || { pr_err "Failed to install provider binary"; exit 1; }
        chmod 755 "$install_path/bin/urnetwork" || { pr_err "Failed to install provider binary"; exit 1; }
    fi

    cd "$script_rundir" || exit 1

    if [ "$operation" = "update" ] || [ "$operation" = "reinstall" ] || [ -z "$(cat "$0" 2>/dev/null)" ]; then
        pr_info "Fetching latest urnet-tools from GitHub..."

        if ! script="$(network_fetch https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/Provider_Install_Linux.sh)"; then
            pr_err "Failed to fetch latest urnet-tools from GitHub, using current version"
            script="$(cat "$0" 2>/dev/null)"
        fi
    else
        script="$(cat "$0" 2>/dev/null)"
    fi

    cd "$workdir" || exit 1
    
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

show_status ()
{
	systemctl --user status urnetwork.service
}

show_logs ()
{
    override_file="$HOME/.config/systemd/user/urnetwork.service.d/override.conf"
    is_ramlog=0
    if [ -f "$override_file" ]; then
        if grep -q "URNETWORK_PROFILE=lowmem" "$override_file" || grep -q "URNETWORK_PROFILE=eco" "$override_file" || grep -q "URNETWORK_RAMLOGS=1" "$override_file"; then
            is_ramlog=1
        fi
    fi

    if [ "$is_ramlog" -eq 1 ]; then
        pr_info "Streaming from RAM disk (/dev/shm/urnetwork.log)"
        if [ ! -f "/dev/shm/urnetwork.log" ]; then
            pr_err "Log file not found. Is the provider running?"
            exit 1
        fi
        tail -n 250 -f /dev/shm/urnetwork.log
    else
        pr_info "Streaming from journald"
        journalctl --user -fu urnetwork.service
    fi
}

toggle_ramlogs ()
{
    mode="$1"
    override_dir="$HOME/.config/systemd/user/urnetwork.service.d"
    override_file="$override_dir/override.conf"

    case "$mode" in
        on)
            pr_info "Enabling RAM logging..."
            mkdir -p "$override_dir"
            if [ -f "$override_file" ]; then
                if ! grep -q "URNETWORK_RAMLOGS=1" "$override_file"; then
                    # Append to existing [Service] block or add it
                    if grep -q "\[Service\]" "$override_file"; then
                        sed -i '/\[Service\]/a Environment="URNETWORK_RAMLOGS=1"' "$override_file"
                    else
                        echo -e "\n[Service]\nEnvironment=\"URNETWORK_RAMLOGS=1\"" >> "$override_file"
                    fi
                fi
            else
                cat > "$override_file" <<EOF
[Service]
Environment="URNETWORK_RAMLOGS=1"
EOF
            fi
            systemctl --user daemon-reload
            systemctl --user restart urnetwork.service
            pr_info "RAM logging enabled and service restarted."
            ;;
        off)
            pr_info "Disabling RAM logging..."
            if [ -f "$override_file" ]; then
                sed -i '/URNETWORK_RAMLOGS=1/d' "$override_file"
                # If file is empty or only has [Service], remove it
                if [ ! -s "$override_file" ] || [ "$(grep -v "^\[" "$override_file" | grep -v "^$" | wc -l)" -eq 0 ]; then
                    rm -f "$override_file"
                    # If directory is empty, remove it
                    rmdir "$override_dir" 2>/dev/null || true
                fi
                systemctl --user daemon-reload
                systemctl --user restart urnetwork.service
            fi
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
    override_dir="$HOME/.config/systemd/user/urnetwork.service.d"
    override_file="$override_dir/override.conf"

    case "$mode" in
        on)
            pr_info "Enabling lowmode..."
            ram_mib=$(detect_mem_limit_mib)
            gomem_mib=$(( ram_mib * 85 / 100 ))
            pr_info "Dynamic GOMEMLIMIT set to ${gomem_mib}MiB (85%% of ${ram_mib}MiB detected RAM)"

            mkdir -p "$override_dir"
            cat > "$override_file" <<EOF
[Service]
Environment="URNETWORK_PROFILE=lowmem"
Environment="GOMEMLIMIT=${gomem_mib}MiB"
Environment="GOGC=50"
EOF
            systemctl --user daemon-reload
            systemctl --user restart urnetwork.service
            pr_info "Lowmode enabled and service restarted."
            ;;
        off)
            pr_info "Disabling lowmode..."
            rm -rf "$override_dir"
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
    override_dir="$HOME/.config/systemd/user/urnetwork.service.d"
    override_file="$override_dir/override.conf"

    case "$mode" in
        on)
            pr_info "Enabling eco mode..."
            ram_mib=$(detect_mem_limit_mib)
            gomem_mib=$(( ram_mib * 75 / 100 ))
            pr_info "Dynamic GOMEMLIMIT set to ${gomem_mib}MiB (75%% of ${ram_mib}MiB detected RAM)"

            mkdir -p "$override_dir"
            # Preserve unrelated settings (e.g. URNETWORK_RAMLOGS) while
            # writing the eco-specific vars. Strip any prior profile/GC lines
            # then append the new ones under an existing [Service] header or
            # create the file fresh.
            if [ -f "$override_file" ]; then
                sed -i '/URNETWORK_PROFILE\|GOMEMLIMIT\|GOGC/d' "$override_file"
            else
                printf '[Service]\n' > "$override_file"
            fi
            printf 'Environment="URNETWORK_PROFILE=eco"\n' >> "$override_file"
            printf 'Environment="GOMEMLIMIT=%sMiB"\n' "$gomem_mib" >> "$override_file"
            printf 'Environment="GOGC=50"\n' >> "$override_file"
            systemctl --user daemon-reload
            systemctl --user restart urnetwork.service
            pr_info "Eco mode enabled and service restarted."
            ;;
        off)
            pr_info "Disabling eco mode..."
            if [ -f "$override_file" ]; then
                sed -i '/URNETWORK_PROFILE\|GOMEMLIMIT\|GOGC/d' "$override_file"
                # Remove the file entirely if nothing meaningful remains
                if ! grep -q 'Environment=' "$override_file" 2>/dev/null; then
                    rm -rf "$override_dir"
                fi
            fi
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
            pr_info "Enabling auto-tune profile..."
            mkdir -p "$override_dir"
            if [ -f "$override_file" ]; then
                sed -i '/URNETWORK_PROFILE\|GOMEMLIMIT\|GOGC/d' "$override_file"
                if ! grep -q '^\[Service\]' "$override_file" 2>/dev/null; then
                    sed -i '1i[Service]' "$override_file"
                fi
            else
                printf '[Service]\n' > "$override_file"
            fi
            printf 'Environment="URNETWORK_PROFILE=auto"\n' >> "$override_file"
            systemctl --user daemon-reload
            systemctl --user restart urnetwork.service
            pr_info "Auto-tune enabled and service restarted."
            ;;
        off)
            pr_info "Disabling auto-tune profile..."
            if [ -f "$override_file" ]; then
                sed -i '/URNETWORK_PROFILE=auto/d' "$override_file"
                if ! grep -q 'Environment=' "$override_file" 2>/dev/null; then
                    rm -rf "$override_dir"
                fi
            fi
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
            pr_info "Enabling turbo %s..." "$mode"
            mkdir -p "$override_dir"
            if [ -f "$override_file" ]; then
                sed -i '/URNETWORK_PROFILE\|GOMEMLIMIT\|GOGC/d' "$override_file"
                if ! grep -q '^\[Service\]' "$override_file" 2>/dev/null; then
                    sed -i '1i[Service]' "$override_file"
                fi
            else
                printf '[Service]\n' > "$override_file"
            fi
            printf 'Environment="URNETWORK_PROFILE=turbo-%s"\n' "$mode" >> "$override_file"
            systemctl --user daemon-reload
            systemctl --user restart urnetwork.service
            pr_info "Turbo %s enabled and service restarted." "$mode"
            ;;
        off)
            pr_info "Disabling turbo mode..."
            if [ -f "$override_file" ]; then
                sed -i '/URNETWORK_PROFILE\|GOMEMLIMIT\|GOGC/d' "$override_file"
                if ! grep -q 'Environment=' "$override_file" 2>/dev/null; then
                    rm -rf "$override_dir"
                fi
            fi
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

do_optimize ()
{
    if [ "$(id -u)" -ne 0 ]; then
        pr_info "Elevation required. Re-running with sudo..."
        exec sudo "$0" "$operation" "$@"
    fi

    pr_info "⚡ Starting System Optimizer..."

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
    
    if [ ! -d "/proc/sys/net/netfilter" ]; then
        pr_info "Conntrack kernel module not found. Attempting to install utilities..."
        if [ -f /etc/os-release ]; then
            . /etc/os-release
            case "$ID" in
                arch)
                    pacman -Sy --noconfirm conntrack-tools
                    ;;
                debian|ubuntu|linuxmint)
                    apt-get update && apt-get install -y conntrack
                    ;;
                fedora|rhel|centos|rocky|almalinux|amzn)
                    dnf install -y conntrack-tools
                    ;;
                alpine)
                    apk add conntrack-tools
                    ;;
                opensuse*|sles)
                    zypper install -y conntrack-tools
                    ;;
                *)
                    pr_warn "Unsupported distro ID: $ID. Please install 'conntrack' manually."
                    ;;
            esac
        else
            # Fallback for older systems
            if [ -f /etc/arch-release ]; then
                pacman -Sy --noconfirm conntrack-tools
            elif [ -f /etc/debian_version ]; then
                apt-get update && apt-get install -y conntrack
            elif [ -f /etc/redhat-release ]; then
                dnf install -y conntrack-tools
            fi
        fi
        modprobe nf_conntrack || pr_err "Failed to load nf_conntrack. Please check your kernel support."
    fi

    # Persistence for module (solves race condition on reboot)
    pr_info "Configuring early module loading..."
    mkdir -p /etc/modules-load.d
    echo "nf_conntrack" > /etc/modules-load.d/urnetwork.conf

    # 2. ZRAM Optimization
    pr_info "Checking for ZRAM (Compressed RAM Swap)..."
    skip_zram=0
    if swapon --show | grep -q "zram"; then
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
                    apt-get update && apt-get install -y zram-tools
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
        swapon --show | grep -q "zram" && pr_info "ZRAM enabled successfully." || pr_warn "ZRAM could not be auto-enabled."
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
        pr_info "Pre-optimized state detected (conntrack_max is already $current_max)."
        if ! confirm "System already has high limits. Apply URNetwork's specific sysctl overrides anyway?"; then
            pr_info "Skipping sysctl configuration (respecting existing tuning)."
            skip_sysctl=1
        fi
    fi

    if [ "$skip_sysctl" -eq 0 ]; then
        pr_info "Writing sysctl config to $sysctl_conf..."
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
EOF
        sysctl --system >/dev/null 2>&1 || pr_err "Warning: some sysctl settings could not be applied."
    fi

    # 4. Apply Ulimits (Systemd)
    # When running via sudo, we need to find the original user's home
    actual_user=$(logname 2>/dev/null || echo "$SUDO_USER" || echo "$USER")
    actual_home=$(getent passwd "$actual_user" | cut -d: -f6)
    
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
            
            if [ -f "$override_file" ]; then
                sed -i '/URNETWORK_RAMLOGS/d' "$override_file"
            else
                printf "[Service]\n" > "$override_file"
            fi
            printf 'Environment="URNETWORK_RAMLOGS=1"\n' >> "$override_file"
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

	status)
		show_status
		exit 0
		;;

    logs)
        show_logs
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

    *)
        pr_err "Invalid operation '%s'" "$operation"
        exit 1
        ;;
esac
