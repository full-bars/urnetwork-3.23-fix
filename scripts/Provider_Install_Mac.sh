#!/bin/sh
# urnet-tools: URnetwork provider manager (macOS)
# Author: full-bars (GitHub), onlyinthe707 / "mesocyclone" (Discord)
# Based on: Ar Rakin, Ryan Mello (original)
# https://github.com/full-bars/urnetwork-3.23-fix

me="$(basename "$0")"

show_help() {
    echo "Usage: $me [options] <command>"
    echo ""
    echo "Core Commands:"
    echo "  install [<version>]      Download and install the provider + launchd service"
    echo "  start                     Start the provider"
    echo "  stop                      Stop the provider"
    echo "  restart                   Restart the provider"
    echo "  status                    Show provider service status"
    echo "  update [<version>]        Upgrade to latest (or specified version)"
    echo "  version                   Show installed version"
    echo ""
    echo "Performance & Tuning:"
    echo "  hot-restart <on|off>      Reuse client JWT identities across restarts"
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
    echo "Hub Management:"
    echo "  hub link <url> [--token]  Link to a hub (fetch CA cert)"
    echo "  hub unlink                Revert to HTTP"
    echo "  hub onboard-cmd           Mint 15-min join token, print curl|sh line"
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

# --- paths ---

install_path="$HOME/.local/share/urnetwork-provider"
provider_bin="$install_path/bin/urnetwork"
plist_path="$HOME/Library/LaunchAgents/com.urnetwork.provider.plist"
label="com.urnetwork.provider"
state_dir="$HOME/.urnetwork"
log_dir="$HOME/Library/Logs/$label"
github_api="https://api.github.com/repos/full-bars/urnetwork-3.23-fix"
github_raw="https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main"

# --- launchd ---

load_service() {
    launchctl bootstrap "gui/$UID" "$plist_path" 2>/dev/null
}

unload_service() {
    launchctl bootout "gui/$UID/$label" 2>/dev/null
}

restart_service() {
    unload_service
    sleep 1
    load_service
}

service_running() {
    launchctl print "gui/$UID/$label" >/dev/null 2>&1
}

# --- plist ---

write_plist() {
    cat > "$plist_path" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$label</string>
    <key>ProgramArguments</key>
    <array>
        <string>$provider_bin</string>
        <string>provide</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$log_dir/stdout.log</string>
    <key>StandardErrorPath</key>
    <string>$log_dir/stderr.log</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key>
        <string>$HOME</string>
    </dict>
    <key>WorkingDirectory</key>
    <string>$HOME</string>
</dict>
</plist>
PLIST
}

plist_set_env() {
    _key="$1"
    _val="$2"
    /usr/libexec/PlistBuddy -c "Set :EnvironmentVariables:$_key $_val" "$plist_path" 2>/dev/null || \
    /usr/libexec/PlistBuddy -c "Add :EnvironmentVariables:$_key string $_val" "$plist_path" 2>/dev/null
}

plist_rm_env() {
    _key="$1"
    /usr/libexec/PlistBuddy -c "Delete :EnvironmentVariables:$_key" "$plist_path" 2>/dev/null
}

# --- install ---

do_install() {
    version="${1:-latest}"

    if [ "$version" = "latest" ]; then
        release_url="$github_api/releases/latest"
    else
        release_url="$github_api/releases/tags/$version"
    fi

    pr_info "Fetching release info..."
    release_json="$(curl -fsSL "$release_url" 2>/dev/null)"
    if [ -z "$release_json" ]; then
        pr_err "Could not fetch release info from %s" "$release_url"
        exit 1
    fi

    tag="$(echo "$release_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["tag_name"])' 2>/dev/null)"
    if [ -z "$tag" ]; then
        pr_err "Could not parse release tag"
        exit 1
    fi

    # Determine arch
    arch="$(uname -m)"
    case "$arch" in
        arm64|aarch64) goarch="arm64" ;;
        x86_64|amd64)   goarch="amd64" ;;
        *)              pr_err "Unsupported architecture: %s" "$arch"; exit 1 ;;
    esac

    tarball_url="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/$tag/urnetwork-provider-$tag.tar.gz"
    pr_info "Downloading %s..." "$tarball_url"

    tmpdir="$(mktemp -d)"
    curl -fsSL "$tarball_url" -o "$tmpdir/provider.tar.gz" || {
        pr_err "Download failed"
        rm -rf "$tmpdir"
        exit 1
    }

    tar -xzf "$tmpdir/provider.tar.gz" -C "$tmpdir" || {
        pr_err "Extract failed"
        rm -rf "$tmpdir"
        exit 1
    }

    # Find the darwin binary for our arch
    provider_src="$tmpdir/darwin/$goarch/provider"
    if [ ! -f "$provider_src" ]; then
        pr_err "Darwin/%s binary not found in release tarball" "$goarch"
        rm -rf "$tmpdir"
        exit 1
    fi

    # Install binary
    mkdir -p "$install_path/bin"
    cp "$provider_src" "$provider_bin" || { pr_err "Failed to install binary"; rm -rf "$tmpdir"; exit 1; }
    chmod 755 "$provider_bin"

    # Write version file
    echo "$tag" > "$install_path/version"

    # Remove quarantine attribute
    xattr -d com.apple.quarantine "$provider_bin" 2>/dev/null || true

    # Install the tools wrapper (download from GitHub — curl|sh means $0 isn't a file path)
    curl -fsSL "$github_raw/scripts/Provider_Install_Mac.sh" -o "$install_path/bin/urnet-tools" 2>/dev/null || true
    chmod 755 "$install_path/bin/urnet-tools" 2>/dev/null || true

    # Create launchd plist
    mkdir -p "$(dirname "$plist_path")"
    mkdir -p "$log_dir"
    write_plist

    # Load the service
    unload_service
    load_service

    # Add to PATH if not already present
    if ! echo "$PATH" | tr ':' '\n' | grep -qxF "$install_path/bin"; then
        shell_rc=""
        case "$SHELL" in
            */zsh)  shell_rc="$HOME/.zshrc" ;;
            */bash) shell_rc="$HOME/.bash_profile" ;;
        esac
        if [ -n "$shell_rc" ]; then
            echo "export PATH=\"$install_path/bin:\$PATH\"" >> "$shell_rc"
            pr_info "Added %s to PATH in %s" "$install_path/bin" "$shell_rc"
        fi
    fi

    # Clean up
    rm -rf "$tmpdir"

    pr_info "URnetwork provider %s installed" "$tag"
    pr_info "  Binary:  %s" "$provider_bin"
    pr_info "  Config:  %s" "$plist_path"
    pr_info "  Data:    %s" "$state_dir"
    pr_info ""
    pr_info "Commands:  urnet-tools start|stop|restart|status|hot-restart|session|proxy|hub"
    pr_info "Restart your terminal or run 'hash -r' for urnet-tools to be found"
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
        pr_info "Not installed"
    fi
}

do_update() {
    version="${1:-latest}"
    do_install "$version"
}

# --- hot-restart ---

do_hot_restart() {
    case "$1" in
        on)
            pr_info "Enabling hot-restart (client JWT reuse across restarts)"
            plist_set_env "URNETWORK_HOT_RESTART" "1"
            if [ "$FORCE" != "1" ]; then
                printf "Restart provider to apply? [y/N]: "
                read -r yn < /dev/tty
                case "$yn" in
                    [Yy]*) restart_service; pr_info "Provider restarted with hot-restart enabled" ;;
                    *) pr_info "Change applied. Run 'urnet-tools restart' when ready." ;;
                esac
            else
                restart_service
                pr_info "Provider restarted with hot-restart enabled"
            fi
            ;;
        off)
            pr_info "Disabling hot-restart"
            plist_rm_env "URNETWORK_HOT_RESTART"
            if [ "$FORCE" != "1" ]; then
                printf "Restart provider to apply? [y/N]: "
                read -r yn < /dev/tty
                case "$yn" in
                    [Yy]*) restart_service; pr_info "Provider restarted with hot-restart disabled" ;;
                    *) pr_info "Change applied. Run 'urnet-tools restart' when ready." ;;
                esac
            else
                restart_service
                pr_info "Provider restarted with hot-restart disabled"
            fi
            ;;
        "")
            if /usr/libexec/PlistBuddy -c "Print :EnvironmentVariables:URNETWORK_HOT_RESTART" "$plist_path" 2>/dev/null | grep -q "1"; then
                pr_info "Hot-restart is enabled"
            else
                pr_info "Hot-restart is off"
            fi
            ;;
        *)
            pr_err "Usage: urnet-tools hot-restart <on|off>"
            exit 1
            ;;
    esac
}

# --- session save/load ---

do_session() {
    action="$1"
    file="$2"
    staging_dir="$state_dir/.session-staging"

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

            # Scan remaining args for --force/-f
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
                        pr_info "Restarting provider..."
                        restart_service
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

# --- proxy ---

do_proxy() {
    if [ ! -x "$provider_bin" ]; then
        pr_err "Provider binary not found. Is it installed?"
        exit 1
    fi

    case "$1" in
        refresh)    "$provider_bin" proxy refresh;;
        remove-dead) "$provider_bin" proxy remove-dead;;
        summary)    "$provider_bin" proxy summary;;
        *)
            pr_err "Usage: urnet-tools proxy <refresh|remove-dead|summary>"
            exit 1
            ;;
    esac
}

# --- hub ---

do_hub() {
    if [ ! -x "$provider_bin" ]; then
        pr_err "Provider binary not found. Is it installed?"
        exit 1
    fi

    case "$1" in
        link)
            shift
            url="$1"
            shift 2>/dev/null || true
            token=""
            while [ $# -gt 0 ]; do
                case "$1" in
                    --token) token="$2"; shift 2 ;;
                    *) shift ;;
                esac
            done
            if [ -z "$url" ]; then
                pr_err "Usage: urnet-tools hub link <url> [--token <token>]"
                exit 1
            fi

            mkdir -p "$state_dir"
            if [ -n "$token" ]; then
                resp="$(curl -fsSL "$url/api/ca-cert?token=$token" 2>/dev/null)"
            else
                resp="$(curl -fsSL "$url/api/cert" 2>/dev/null)"
            fi

            if [ -z "$resp" ]; then
                pr_err "Could not reach hub at %s" "$url"
                exit 1
            fi

            ca_pem="$(echo "$resp" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("ca_pem",""))' 2>/dev/null)"
            if [ -n "$ca_pem" ]; then
                echo "$ca_pem" | python3 -c 'import sys; sys.stdout.write(sys.stdin.read().replace("\\n","\n"))' > "$state_dir/hub_ca.pem"
                rm -f "$state_dir/hub.pin"
                pr_info "CA certificate saved to %s/hub_ca.pem" "$state_dir"
            else
                fingerprint="$(echo "$resp" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("fingerprint",""))' 2>/dev/null)"
                if [ -n "$fingerprint" ]; then
                    echo "$fingerprint" > "$state_dir/hub.pin"
                    pr_info "Fingerprint pinned to %s/hub.pin" "$state_dir"
                else
                    pr_err "Could not extract CA or fingerprint from hub response"
                    exit 1
                fi
            fi

            echo "$url" > "$state_dir/report_url"
            pr_info "Report URL set to %s" "$url"
            ;;

        unlink)
            rm -f "$state_dir/hub_ca.pem" "$state_dir/hub.pin"
            if [ -f "$state_dir/report_url" ]; then
                current="$(cat "$state_dir/report_url")"
                if echo "$current" | grep -q "^https://"; then
                    hostport="${current#https://}"
                    host="${hostport%%:*}"
                    echo "http://${host}:8080" > "$state_dir/report_url"
                fi
            fi
            pr_info "Unlinked from hub"
            ;;

        onboard-cmd)
            if [ ! -x "$provider_bin" ]; then
                pr_err "Provider binary not found"
                exit 1
            fi
            "$provider_bin" proxy auth add
            ;;

        *)
            pr_err "Usage: urnet-tools hub <link|unlink|onboard-cmd>"
            exit 1
            ;;
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
    install)       do_install "$@" ;;
    update)        do_update "$@" ;;
    start)         do_start ;;
    stop)          do_stop ;;
    restart)       do_restart ;;
    status)        do_status ;;
    version)       do_version ;;
    hot-restart)   do_hot_restart "$@" ;;
    session)       do_session "$@" ;;
    proxy)         do_proxy "$@" ;;
    hub)           do_hub "$@" ;;
    auth)          "$provider_bin" auth "$@" ;;
    logs)          "$provider_bin" logs "$@" ;;
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