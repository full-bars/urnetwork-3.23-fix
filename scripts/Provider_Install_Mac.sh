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
    echo "Hub Management:"
    echo "  hub install [--tag] [--port] [--token]"
    echo "                            Deploy the hub as a Docker container"
    echo "  hub update [--tag] [-f]   Pull latest hub image and recreate container"
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

pr_warn() {
    fmt="$1"; shift
    printf "%s: $fmt\n" "$me" "$@" >&2
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

    # Determine arch
    arch="$(uname -m)"
    case "$arch" in
        arm64|aarch64) goarch="arm64" ;;
        x86_64|amd64)   goarch="amd64" ;;
        *)              pr_err "Unsupported architecture: %s" "$arch"; exit 1 ;;
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

    # Install the tool: the Go urnet-tools binary (v3.23.0-fix.28+) shipped
    # as a release asset, digest-verified; fall back to the legacy shell
    # wrapper for releases that predate the Go asset.
    tool_asset="urnet-tools-darwin-$goarch"
    tool_digest=""
    if command -v jq > /dev/null 2>&1; then
        tool_digest="$(curl -fsSL "$github_api/releases/tags/$tag" 2>/dev/null | jq -r --arg a "$tool_asset" '.assets[] | select(.name == $a) | .digest' 2>/dev/null | sed 's/^sha256://' || true)"
    elif command -v python3 > /dev/null 2>&1; then
        tool_digest="$(curl -fsSL "$github_api/releases/tags/$tag" 2>/dev/null | python3 -c 'import sys,json; d=json.load(sys.stdin); print(next((a.get("digest","").replace("sha256:","") for a in d.get("assets",[]) if a.get("name")==sys.argv[1]), ""))' "$tool_asset" 2>/dev/null || true)"
    fi

    if [ -n "$tool_digest" ]; then
        if curl -fsSL "https://github.com/full-bars/urnetwork-3.23-fix/releases/download/$tag/$tool_asset" -o "$tmpdir/$tool_asset" 2>/dev/null; then
            if command -v shasum > /dev/null 2>&1; then
                actual="$(shasum -a 256 "$tmpdir/$tool_asset" | awk '{print $1}')"
            elif command -v openssl > /dev/null 2>&1; then
                actual="$(openssl dgst -sha256 "$tmpdir/$tool_asset" | awk '{print $2}')"
            else
                actual=""
            fi
            if [ -n "$actual" ] && [ "$actual" = "$tool_digest" ]; then
                mv -f "$tmpdir/$tool_asset" "$install_path/bin/urnet-tools"
                chmod 755 "$install_path/bin/urnet-tools"
                xattr -d com.apple.quarantine "$install_path/bin/urnet-tools" 2>/dev/null || true
            else
                pr_warn "urnet-tools sha256 mismatch, falling back to shell wrapper"
                curl -fsSL "$github_raw/scripts/Provider_Install_Mac.sh" -o "$install_path/bin/urnet-tools" 2>/dev/null || true
                chmod 755 "$install_path/bin/urnet-tools" 2>/dev/null || true
            fi
        else
            pr_warn "urnet-tools download failed, falling back to shell wrapper"
            curl -fsSL "$github_raw/scripts/Provider_Install_Mac.sh" -o "$install_path/bin/urnet-tools" 2>/dev/null || true
            chmod 755 "$install_path/bin/urnet-tools" 2>/dev/null || true
        fi
    else
        # Release predates the Go tool asset — legacy wrapper.
        curl -fsSL "$github_raw/scripts/Provider_Install_Mac.sh" -o "$install_path/bin/urnet-tools" 2>/dev/null || true
        chmod 755 "$install_path/bin/urnet-tools" 2>/dev/null || true
    fi

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
            plist_rm_env "URNETWORK_HOT_RESTART"
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
            plist_set_env "URNETWORK_HOT_RESTART" "0"
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
            if /usr/libexec/PlistBuddy -c "Print :EnvironmentVariables:URNETWORK_HOT_RESTART" "$plist_path" 2>/dev/null | grep -q "0"; then
                pr_info "Hot-restart is off"
            else
                pr_info "Hot-restart is enabled"
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

hub_image="ghcr.io/full-bars/urnetwork-3.23-fix-hub"
hub_container="urnetwork-hub"
hub_volume="urnetwork-hubdata"
hub_docker_conf="$state_dir/hub-docker.conf"

# hub_docker_require: there's no native macOS hub binary (the hub only
# ships Linux binaries + a multi-arch Docker image — see docs/Hub-Setup.md),
# so 'hub install'/'hub update' here always run the hub as a container.
hub_docker_require() {
    if ! command -v docker > /dev/null; then
        pr_err "Docker is required to run the hub on macOS (no native binary exists)."
        pr_err "Install Docker Desktop: https://www.docker.com/products/docker-desktop"
        exit 1
    fi
    if ! docker info > /dev/null 2>&1; then
        pr_err "Docker is installed but not running. Start Docker Desktop and try again."
        exit 1
    fi
}

# hub_docker_run TAG PORT TOKEN: (re)creates the hub container. Assumes any
# previous container with the same name has already been removed by the
# caller (install refuses to overwrite; update stops+removes first).
hub_docker_run() {
    _tag="$1" _port="$2" _token="$3"
    _run_args="-d --name $hub_container --restart unless-stopped -p ${_port}:8080 -v ${hub_volume}:/data"
    if [ -n "$_token" ]; then
        _run_args="$_run_args -e URNETWORK_HUB_TOKEN=$_token"
    fi
    # shellcheck disable=SC2086
    docker run $_run_args "$hub_image:$_tag"
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

    if docker ps -a --format '{{.Names}}' | grep -qx "$hub_container"; then
        pr_err "Hub container '%s' already exists. Use 'urnet-tools hub update' to upgrade it," "$hub_container"
        pr_err "or 'docker rm -f %s' to remove it first." "$hub_container"
        exit 1
    fi

    pr_info "Pulling %s:%s..." "$hub_image" "$tag"
    docker pull "$hub_image:$tag" || { pr_err "docker pull failed"; exit 1; }

    if ! hub_docker_run "$tag" "$port" "$token"; then
        pr_err "Failed to start hub container"
        exit 1
    fi

    mkdir -p "$state_dir"
    { printf 'tag=%s\n' "$tag"; printf 'port=%s\n' "$port"; printf 'token=%s\n' "$token"; } > "$hub_docker_conf"

    pr_info "Hub container started."
    pr_info "  Dashboard: http://localhost:%s" "$port"
    pr_info "  Data:      docker volume '%s' (persists across updates)" "$hub_volume"
    pr_info ""
    pr_info "Next steps:"
    pr_info "  urnet-tools hub link http://<this-host>:%s   # point your providers at the hub" "$port"
    pr_info "  docker logs -f %s                             # stream hub logs" "$hub_container"
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

    if ! docker ps -a --format '{{.Names}}' | grep -qx "$hub_container"; then
        pr_err "No hub container found. Run 'urnet-tools hub install' first."
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

    pr_info "Pulling %s:%s..." "$hub_image" "$tag"
    docker pull "$hub_image:$tag" || { pr_err "docker pull failed"; exit 1; }

    if [ "$force" != "1" ]; then
        running_image="$(docker inspect --format '{{.Image}}' "$hub_container" 2>/dev/null)"
        pulled_image="$(docker inspect --format '{{.Id}}' "$hub_image:$tag" 2>/dev/null)"
        if [ -n "$running_image" ] && [ "$running_image" = "$pulled_image" ]; then
            pr_info "Hub is already running %s:%s. Nothing to do. Use --force to recreate anyway." "$hub_image" "$tag"
            return
        fi
    fi

    pr_info "Recreating hub container (data volume '%s' is preserved)..." "$hub_volume"
    docker stop "$hub_container" > /dev/null 2>&1
    docker rm "$hub_container" > /dev/null 2>&1

    if ! hub_docker_run "$tag" "$port" "$token"; then
        pr_err "Failed to start hub container"
        exit 1
    fi

    { printf 'tag=%s\n' "$tag"; printf 'port=%s\n' "$port"; printf 'token=%s\n' "$token"; } > "$hub_docker_conf"
    pr_info "Hub updated and running %s:%s." "$hub_image" "$tag"
}

do_hub() {
    case "$1" in
        install|update)
            ;;
        *)
            if [ ! -x "$provider_bin" ]; then
                pr_err "Provider binary not found. Is it installed?"
                exit 1
            fi
            ;;
    esac

    case "$1" in
        install)
            shift
            do_hub_docker_install "$@"
            ;;

        update)
            shift
            do_hub_docker_update "$@"
            ;;

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
            pr_err "Usage: urnet-tools hub <install|update|link|unlink|onboard-cmd>"
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
    self-heal)
        file="$HOME/.urnetwork/proxy_self_heal"
        case "${1:-}" in
            on) mkdir -p "$HOME/.urnetwork"; printf '%s\n' "on" > "$file"; pr_info "Self-heal enabled" ;;
            off) mkdir -p "$HOME/.urnetwork"; printf '%s\n' "off" > "$file"; pr_info "Self-heal disabled" ;;
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
            *) pr_err "Usage: urnet-tools self-heal [on|off|status]"; exit 1 ;;
        esac
        ;;
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