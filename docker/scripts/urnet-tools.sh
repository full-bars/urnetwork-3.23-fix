#!/bin/bash
# urnet-tools -- Docker wrapper for URNetwork provider management
set -eu

operation="${1:-}"
[ -z "$operation" ] && { echo "Usage: urnet-tools <command> [args]"; exit 1; }
shift

hub_link() {
    url=""
    token=""

    while [ $# -gt 0 ]; do
        case "$1" in
            --token)
                if [ -z "$2" ]; then echo "Option --token requires a value."; exit 1; fi
                token="$2"; shift 2 ;;
            *)
                if [ -z "$url" ]; then url="$1"; shift
                else echo "Unexpected argument: $1"; exit 1; fi ;;
        esac
    done

    if [ -z "$url" ]; then
        echo "Usage: urnet-tools hub link <https://hub-host:port> [--token <onboard-token>]"
        exit 1
    fi

    case "$url" in https://*) ;; *)
        echo "Hub link URL must start with https://"; exit 1 ;;
    esac

    url="${url%/}"
    hub_dir="$HOME/.urnetwork"
    ca_file="$hub_dir/hub_ca.pem"
    pin_file="$hub_dir/hub.pin"
    report_file="$hub_dir/report_url"

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
            echo "WARNING: This provider directory is already linked to a different hub host."
            echo "  Current: $old_host"
            echo "  New:     $new_host"
            echo ""
            echo "Linking to a different host will reconfigure all providers sharing"
            echo "this directory — containers with bind mounts, native installs on"
            echo "the same user, etc."
            echo ""
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

    fetch_json() {
        local u="$1"
        if command -v curl > /dev/null; then
            curl -k --connect-timeout 10 -fSsL "$u" 2>/dev/null
        elif command -v wget > /dev/null; then
            wget -q --no-check-certificate --timeout=10 -O - "$u" 2>/dev/null
        fi
    }

    extract_pem() {
        printf '%s' "$1" | sed -n 's/.*"ca_pem" *: *"\([^"]*\)".*/\1/p'
    }
    extract_fp() {
        printf '%s' "$1" | sed -n 's/.*"ca_fingerprint" *: *"\([^"]*\)".*/\1/p'
    }
    extract_legacy_fp() {
        printf '%s' "$1" | sed -n 's/.*"fingerprint" *: *"\([^"]*\)".*/\1/p'
    }

    if [ -n "$token" ]; then
        echo "Fetching hub CA certificate via onboard token..."
        encoded_token=$(printf '%s' "$token" | sed 's/+/%2B/g; s/=/%3D/g; s/\//%2F/g')
        cert_json="$(fetch_json "${url}/api/ca-cert?token=${encoded_token}")" || true
        if [ -z "$cert_json" ]; then
            echo "Could not reach hub at $url with the given token."
            exit 1
        fi
        ca_pem="$(extract_pem "$cert_json")"
        ca_fp="$(extract_fp "$cert_json")"
        if [ -z "$ca_pem" ]; then
            echo "Hub responded but did not return a CA certificate (may be running an older version)."
            exit 1
        fi
        echo ""
        echo "Hub CA fingerprint: $ca_fp"
        echo ""
        mkdir -p "$hub_dir"
        printf '%s' "$ca_pem" | sed 's/\\n/\n/g' > "$ca_file.tmp" && mv "$ca_file.tmp" "$ca_file"
        chmod 600 "$ca_file"
        rm -f "$pin_file"
        echo "CA certificate saved to $ca_file"
    else
        echo "Fetching hub certificate from $url/api/cert ..."
        cert_json="$(fetch_json "$url/api/cert")" || true
        if [ -z "$cert_json" ]; then
            echo "Could not reach hub at $url."
            exit 1
        fi
        ca_pem="$(extract_pem "$cert_json")"
        ca_fp="$(extract_fp "$cert_json")"
        legacy_fp="$(extract_legacy_fp "$cert_json")"

        if [ -n "$ca_pem" ]; then
            echo ""
            echo "Hub CA fingerprint: $ca_fp"
            echo ""
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
            printf '%s' "$ca_pem" | sed 's/\\n/\n/g' > "$ca_file.tmp" && mv "$ca_file.tmp" "$ca_file"
            chmod 600 "$ca_file"
            rm -f "$pin_file"
            echo "CA certificate saved to $ca_file"
        elif [ -n "$legacy_fp" ]; then
            echo "WARNING: Hub does not support CA-based trust. Falling back to legacy fingerprint pinning."
            echo ""
            echo "Hub certificate fingerprint: $legacy_fp"
            echo ""
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
            printf '%s\n' "$legacy_fp" > "$pin_file.tmp" && mv "$pin_file.tmp" "$pin_file"
            echo "Fingerprint pinned to $pin_file"
        else
            echo "Could not extract CA certificate or fingerprint from hub response."
            exit 1
        fi
    fi

    printf '%s\n' "$url" > "$report_file.tmp" && mv "$report_file.tmp" "$report_file"
    echo "Report URL set to $url"
    echo ""
    echo "Success. The provider will now send encrypted reports to $url."
    echo "The change takes effect on the next report tick (no restart needed)."
}

hub_unlink() {
    hub_dir="$HOME/.urnetwork"
    pin_file="$hub_dir/hub.pin"
    ca_file="$hub_dir/hub_ca.pem"
    report_file="$hub_dir/report_url"

    rm -f "$pin_file"
    echo "Removed $pin_file"
    if [ -f "$ca_file" ]; then
        rm -f "$ca_file"
        echo "Removed $ca_file"
    fi

    if [ -f "$report_file" ]; then
        current="$(cat "$report_file")"
        case "$current" in
            https://*)
                host_port="${current#https://}"
                host="${host_port%%:*}"
                new_url="http://${host}:8080"
                printf '%s\n' "$new_url" > "$report_file.tmp" && mv "$report_file.tmp" "$report_file"
                echo "Report URL changed to $new_url (insecure)"
                ;;
            *)
                echo "Report URL is $current (not HTTPS, left unchanged)"
                ;;
        esac
    fi

    echo ""
    echo "Unlinked. Reports are no longer encrypted."
    echo "To re-link, run: urnet-tools hub link https://<hub-host>:8443"
}

# === Update Logic ===
do_update() {
    arch="$(uname -m)"
    case "$arch" in
        x86_64) arch="amd64" ;;
        aarch64) arch="arm64" ;;
        *) echo "ERROR: unsupported architecture $arch"; exit 1 ;;
    esac

    provider_bin="/app/urnetwork_${arch}_stable"

    echo "Checking for provider updates..."

    release_json="$(curl -s --connect-timeout 10 "https://api.github.com/repos/full-bars/urnetwork-3.23-fix/releases/latest")" || {
        echo "ERROR: could not reach GitHub API."
        exit 1
    }

    version="$(echo "$release_json" | jq -r '.tag_name // empty')"
    [ -n "$version" ] || { echo "ERROR: could not parse release info."; exit 1; }

    download_url="$(echo "$release_json" | jq -r '.assets[] | select((.name | contains(".tar.gz")) and (.name | contains("linux-'"$arch"'"))) | .browser_download_url // empty' | head -n1)"
    [ -n "$download_url" ] || { echo "ERROR: no download found for linux-$arch in release $version"; exit 1; }

    primary_url="$(echo "$download_url" | sed 's|https://github.com/full-bars/urnetwork-3.23-fix/releases/download/|https://dl.fullbars.xyz/releases/download/|')"

    current_version="unknown"
    if [ -x "$provider_bin" ]; then
        current_version="$($provider_bin --version 2>/dev/null || echo "unknown")"
    fi
    echo "Current version: $current_version"
    echo "Latest version: $version"

    if [ "$current_version" = "$version" ]; then
        echo "Already at latest version. Nothing to update."
        exit 0
    fi

    echo "Downloading $version..."
    tarball="$(mktemp /tmp/urnetwork-update-XXXXXX.tar.gz)"
    if ! curl -fL --connect-timeout 30 -o "$tarball" "$primary_url"; then
        echo "Primary download failed, trying GitHub mirror..."
        curl -fL --connect-timeout 30 -o "$tarball" "$download_url" || {
            echo "ERROR: download failed."
            rm -f "$tarball"
            exit 1
        }
    fi

    tmpdir="$(mktemp -d /tmp/urnetwork-update-XXXXXX)"
    tar -xzf "$tarball" -C "$tmpdir" || {
        echo "ERROR: failed to extract tarball."
        rm -rf "$tmpdir" "$tarball"
        exit 1
    }

    if [ ! -f "$tmpdir/provider" ]; then
        echo "ERROR: provider binary not found in tarball."
        ls -la "$tmpdir/" 2>/dev/null || true
        rm -rf "$tmpdir" "$tarball"
        exit 1
    fi

    staged_provider="$(mktemp "${provider_bin}.XXXXXX")" || {
        rm -rf "$tmpdir" "$tarball"
        exit 1
    }
    if ! cp "$tmpdir/provider" "$staged_provider" || ! chmod +x "$staged_provider"; then
        rm -rf "$tmpdir" "$tarball" "$staged_provider"
        exit 1
    fi

    marker_dir="$HOME/.urnetwork"
    mkdir -p "$marker_dir" || { echo "ERROR: could not create $marker_dir"; rm -rf "$tmpdir" "$tarball" "$staged_provider"; exit 1; }
    touch "$marker_dir/update-pending" || { echo "ERROR: could not write update-pending marker"; rm -rf "$tmpdir" "$tarball" "$staged_provider"; exit 1; }

    if ! mv -f "$staged_provider" "$provider_bin"; then
        rm -f "$marker_dir/update-pending"
        rm -rf "$tmpdir" "$tarball" "$staged_provider"
        exit 1
    fi

    rm -rf "$tmpdir" "$tarball"
    echo "Provider binary updated to $version."

    rc=0
    pkill -f "^/app/urnetwork_${arch}_stable provide" 2>/dev/null || rc=$?
    case $rc in
        0) echo "Provider process terminated." ;;
        1) echo "No running provider process found — nothing to terminate." ;;
        *) echo "WARNING: pkill returned exit code $rc — provider may still be running." ;;
    esac

    shutdown_timeout=15
    waited=0
    while pgrep -f "^/app/urnetwork_${arch}_stable provide" >/dev/null 2>&1; do
        if [ "$waited" -ge "$shutdown_timeout" ]; then
            echo "ERROR: provider process still running ${shutdown_timeout}s after termination attempt."
            rm -f "$marker_dir/update-pending"
            exit 1
        fi
        sleep 1
        waited=$((waited + 1))
    done

    echo "Startup loop will respawn provider with the new binary."
    exit 0
}

case "$operation" in
    proxy)
        subcmd="${1:-}"
        shift || true
        case "$subcmd" in
            health)  [ -x /usr/local/bin/proxy-health ] && exec /usr/local/bin/proxy-health || { echo "proxy-health not found"; exit 1; } ;;
            traffic) [ -x /usr/local/bin/proxy-traffic ] && exec /usr/local/bin/proxy-traffic || { echo "proxy-traffic not found"; exit 1; } ;;
            add|clear|refresh|remove-dead|remove|exclude)
                [ -x /usr/local/bin/provider ] || { echo "provider binary not found"; exit 1; }
                exec /usr/local/bin/provider proxy "$subcmd" "$@"
                ;;
            *)
                echo "Unknown proxy command: $subcmd (Try 'health', 'traffic', 'add', 'clear', 'refresh', 'remove-dead', 'remove --match=<pat>', or 'exclude')"
                exit 1
                ;;
        esac
        ;;
    choose_network)
        [ -x /usr/local/bin/provider ] || { echo "provider binary not found"; exit 1; }
        exec /usr/local/bin/provider choose_network "$@"
        ;;
    logs)
        [ -x /usr/local/bin/logs ] && exec /usr/local/bin/logs "$@" || { echo "logs tool not found"; exit 1; }
        ;;
    status)
        echo "URNetwork Provider (Docker)"
        [ -x /usr/local/bin/provider ] && /usr/local/bin/provider -v || echo "provider binary not found"
        echo "Status: Running"
        ;;
    self-heal)
        file="$HOME/.urnetwork/proxy_self_heal"
        case "${1:-}" in
            on) mkdir -p "$HOME/.urnetwork"; printf '%s\n' "on" > "$file"; echo "Self-heal enabled" ;;
            off) mkdir -p "$HOME/.urnetwork"; printf '%s\n' "off" > "$file"; echo "Self-heal disabled" ;;
            status|"")
                if [ -f "$file" ] && [ "$(cat "$file" 2>/dev/null)" = "on" ]; then
                    echo "self-heal: on"
                elif [ -f "$file" ]; then
                    echo "self-heal: off"
                else
                    echo "self-heal: off (default; enable with 'urnet-tools self-heal on' or URNETWORK_SELF_HEAL=1)"
                fi
                if [ -f "$HOME/.urnetwork/pressure_status" ]; then
                    if command -v jq >/dev/null 2>&1; then
                        jq -r '"pressure: \(.score) (target_pool=\(.target_pool), updated=\(.updated))"' \
                            "$HOME/.urnetwork/pressure_status" 2>/dev/null
                    else
                        cat "$HOME/.urnetwork/pressure_status"
                    fi
                fi
                ;;
            *) echo "Usage: urnet-tools self-heal [on|off|status]"; exit 1 ;;
        esac
        ;;
    -v|version)
        [ -x /usr/local/bin/provider ] && exec /usr/local/bin/provider -v || { echo "provider binary not found"; exit 1; }
        ;;
    optimize)
        echo "Optimization is mostly handled by Docker runtime/host settings."
        echo "Ensure you run the container with --cap-add=NET_ADMIN --cap-add=NET_RAW."
        ;;
    hub)
        subcmd="${1:-}"
        case "$subcmd" in
            link) shift; hub_link "$@" ;;
            unlink) hub_unlink ;;
            onboard-cmd|show-password|init)
                echo "Hub-side commands (onboard-cmd, show-password, init) run inside the hub container:"
                echo ""
                echo "  docker exec <hub-container> /hub -mint-onboard-token -data /data"
                echo "  docker exec <hub-container> /hub -show-password -data /data"
                echo ""
                echo "For full hub setup, exec into the container or use docker compose exec."
                exit 1
                ;;
            update|install)
                echo "In Docker, update the hub by pulling a new image:"
                echo "  docker pull ghcr.io/full-bars/urnetwork-3.23-fix-hub:latest"
                echo "Or re-create the container with the updated image."
                exit 1
                ;;
            *) echo "Unknown hub command: $subcmd (try 'link', 'unlink', 'onboard-cmd', 'show-password')"; exit 1 ;;
        esac
        ;;
    session)
        subcmd="${1:-}"; shift || true
        state_dir="/root/.urnetwork"
        staging_dir="$state_dir/.session-staging"
        provider_bin="/usr/local/bin/provider"

        case "$subcmd" in
            save)
                file="${1:-}"
                [ -n "${file:-}" ] || { echo "Usage: urnet-tools session save <file>"; exit 1; }
                [ -t 0 ] || { echo "ERROR: session save requires an interactive TTY (use 'docker exec -it')."; exit 1; }

                echo "WARNING: This bundle contains full identity and reputation"
                echo "credentials for this provider's fleet. Treat it like a password."
                echo ""

                printf "Enter encryption passphrase (will NOT echo): "
                stty -echo 2>/dev/null || true
                read -r pass1 < /dev/tty
                stty echo 2>/dev/null || true
                echo ""
                printf "Confirm passphrase: "
                stty -echo 2>/dev/null || true
                read -r pass2 < /dev/tty
                stty echo 2>/dev/null || true
                echo ""
                [ "$pass1" = "$pass2" ] || { echo "ERROR: Passphrases do not match."; exit 1; }

                files=""
                for f in .client_jwts.json jwt jwt_last_refresh .provider.key .provider.cert proxy proxy_url.json proxy.state; do
                    [ -f "$state_dir/$f" ] && files="$files $f"
                done

                _pf="$(mktemp /tmp/urnsession-XXXXXX)"
                printf '%s' "$pass1" > "$_pf"
                chmod 600 "$_pf"
                set -o pipefail
                tar -czf - -C "$state_dir" $files 2>/dev/null | \
                    openssl enc -aes-256-cbc -pbkdf2 -salt -pass "file:$_pf" -out "$file" || { rm -f "$_pf"; echo "ERROR: Failed to create session bundle."; exit 1; }
                set +o pipefail
                rm -f "$_pf"
                chmod 600 "$file" 2>/dev/null || true
                echo "Session saved to $file"
                echo "Retrieve with: docker cp <container>:$file ."
                ;;

            load)
                file="${1:-}"
                [ -n "${file:-}" ] || { echo "Usage: urnet-tools session load <file> [--force]"; exit 1; }
                shift || true
                _force=0
                for _a in "$@"; do
                    [ "$_a" = "--force" ] || [ "$_a" = "-f" ] && _force=1
                done
                [ -f "$file" ] || { echo "ERROR: Session file '$file' not found."; exit 1; }
                [ -x "$provider_bin" ] || { echo "ERROR: Provider binary not found at $provider_bin."; exit 1; }
                [ -t 0 ] || { echo "ERROR: session load requires an interactive TTY (use 'docker exec -it')."; exit 1; }

                printf "Enter passphrase: "
                stty -echo 2>/dev/null || true
                read -r pass < /dev/tty
                stty echo 2>/dev/null || true
                echo ""

                tmpdir="$state_dir/.session-tmp-$$"
                mkdir -p "$tmpdir"

                _pf="$(mktemp /tmp/urnsession-XXXXXX)"
                printf '%s' "$pass" > "$_pf"
                chmod 600 "$_pf"
                set -o pipefail
                openssl enc -d -aes-256-cbc -pbkdf2 -pass "file:$_pf" -in "$file" | \
                    tar -xzf - -C "$tmpdir" || { rm -f "$_pf"; rm -rf "$tmpdir"; echo "ERROR: Failed to decrypt (wrong passphrase or corrupt file)."; exit 1; }
                set +o pipefail
                rm -f "$_pf"

                [ -f "$tmpdir/jwt" ] || { echo "ERROR: Bundle is missing 'jwt' file. Not a valid session bundle."; rm -rf "$tmpdir"; exit 1; }

                current_id="$("$provider_bin" print-network-id "$state_dir/jwt" 2>/dev/null || true)"
                new_id="$("$provider_bin" print-network-id "$tmpdir/jwt" 2>/dev/null || true)"

                [ -n "$new_id" ] || { echo "ERROR: Could not extract network_id from bundle JWT. Bundle may be corrupt."; rm -rf "$tmpdir"; exit 1; }

                if [ -n "$current_id" ] && [ "$new_id" != "$current_id" ]; then
                    echo "ERROR: Network ID mismatch."
                    echo "  Current account: $current_id"
                    echo "  Session account: $new_id"
                    echo "Session bundles can only be loaded under the same URnetwork account."
                    [ "$_force" = "1" ] || { rm -rf "$tmpdir"; exit 1; }
                    echo "Proceeding anyway (--force)."
                fi

                backup_dir="$state_dir/.session-backup-$(date +%Y%m%d-%H%M%S)"
                mkdir -p "$backup_dir"
                for f in .client_jwts.json jwt jwt_last_refresh .provider.key .provider.cert proxy proxy_url.json proxy.state; do
                    [ -f "$state_dir/$f" ] && cp "$state_dir/$f" "$backup_dir/$f"
                done
                echo "Backed up current session to $backup_dir"

                rm -rf "$staging_dir"
                mkdir -p "$staging_dir"
                for f in .client_jwts.json jwt jwt_last_refresh .provider.key .provider.cert proxy proxy_url.json proxy.state; do
                    [ -f "$tmpdir/$f" ] && mv "$tmpdir/$f" "$staging_dir/$f"
                done
                rm -rf "$tmpdir"
                touch "$state_dir/.session-pending"

                if [ -f "$staging_dir/.client_jwts.json" ]; then
                    _cnt="$(grep -c '"minted_at"' "$staging_dir/.client_jwts.json" 2>/dev/null || echo 0)"
                    echo "Session contains $_cnt client JWT entries."
                    echo "Note: entries older than 30 days are auto-pruned on startup."
                fi

                echo ""
                printf "Restart provider now to apply loaded session? (Y/n): "
                read -r yn < /dev/tty
                case "$yn" in
                    [Nn]*) echo "Session staged. Restart the container to apply." ;;
                    *)
                        echo "Killing provider to trigger restart..."
                        pkill -f "urnetwork.*provide" 2>/dev/null || true
                        echo "Provider restarted with new session."
                        ;;
        esac
        ;;

            *)
                echo "Usage: urnet-tools session <save|load> <file>"
                echo "  save <file>              Encrypt and export identity+proxy state"
                echo "  load <file> [--force]    Decrypt and import, then restart"
                echo ""
                echo "Save: docker exec -it <container> urnet-tools session save /root/.urnetwork/name.urnsession"
                echo "      docker cp <container>:/root/.urnetwork/name.urnsession ."
                echo "Load: docker cp file.urnsession <container>:/root/.urnetwork/"
                echo "      docker exec -it <container> urnet-tools session load /root/.urnetwork/file.urnsession"
                exit 1
                ;;
        esac
        ;;
    idle-update)
        threshold=5120
        window=300
        while [ $# -gt 0 ]; do
            case "$1" in
                --threshold) threshold="$2"; shift 2 ;;
                --window) window="$2"; shift 2 ;;
                *) echo "Unknown option: $1"; exit 1 ;;
            esac
        done

        case "$threshold" in ''|*[!0-9]*) echo "ERROR: --threshold must be a non-negative integer"; exit 1 ;; esac
        case "$window" in ''|*[!0-9]*) echo "ERROR: --window must be a non-negative integer"; exit 1 ;; esac

        # Bash's [ -gt / -le ] only handle signed 64-bit integers; billable_rate
        # is a Go uint64 and could exceed that range.
        in_int64_range() {
            v="$1"
            len=${#v}
            if [ "$len" -gt 19 ]; then
                return 1
            elif [ "$len" -eq 19 ] && [ "$v" \> "9223372036854775807" ]; then
                return 1
            fi
            return 0
        }
        if ! in_int64_range "$threshold"; then
            echo "ERROR: --threshold is out of range"
            exit 1
        fi
        if ! in_int64_range "$window"; then
            echo "ERROR: --window is out of range"
            exit 1
        fi

        health_dir="${URNETWORK_PROXY_HEALTH_DIR:-$HOME/.urnetwork}"
        rate_file="$health_dir/billable_rate"

        echo "Waiting for billable traffic to drop below ${threshold} B/s for ${window}s..."
        echo "  Polling ${rate_file} every 10s..."
        quiet=0
        while true; do
            rate=0
            rate_known=1
            if [ -f "$rate_file" ]; then
                content="$(cat "$rate_file" 2>/dev/null || echo "0")"
                case "$content" in
                    ''|*[!0-9]*) rate_known=0 ;;
                    *) if in_int64_range "$content"; then rate="$content"; else rate_known=0; fi ;;
                esac
            else
                rate_known=0
                echo "  billable_rate not found — running provider predates idle-update; treating as traffic detected, not idle"
            fi

            if { [ "$rate_known" -eq 1 ] && [ "$rate" -le "$threshold" ]; } || [ "$window" -eq 0 ]; then
                quiet=$((quiet + 10))
                echo "  rate=${rate} B/s — ${quiet}s of quiet (need ${window}s)"
                if [ "$quiet" -ge "$window" ]; then
                    echo ""
                    echo "=== Idle threshold met ==="
                    echo "  Final rate: ${rate} B/s"
                    echo "  Quiet window: ${window}s"
                    if [ "$window" -eq 0 ]; then
                        echo "  Skipping verification (window=0 — immediate update requested)."
                        verify_failed=0
                    else
                        echo "  Verifying sustained quiet (1s polling for 10s)..."
                        verify_failed=0
                        fresh_seen=0
                        last_mtime=""
                        if [ -f "$rate_file" ]; then
                            last_mtime="$(stat -c %Y "$rate_file" 2>/dev/null)"
                        fi
                        for i in 1 2 3 4 5 6 7 8 9 10; do
                            sleep 1
                            vrate=0
                            vrate_known=1
                            cur_mtime=""
                            if [ -f "$rate_file" ]; then
                                cur_mtime="$(stat -c %Y "$rate_file" 2>/dev/null)"
                                content="$(cat "$rate_file" 2>/dev/null)" || vrate_known=0
                                case "$content" in
                                    ''|*[!0-9]*) vrate_known=0 ;;
                                    *) if in_int64_range "$content"; then vrate="$content"; else vrate_known=0; fi ;;
                                esac
                            else
                                vrate_known=0
                            fi
                            if [ "$vrate_known" -eq 0 ] || [ "$vrate" -gt "$threshold" ]; then
                                if [ "$vrate_known" -eq 0 ]; then
                                    echo "  billable_rate disappeared during verification — going back to 10s polling"
                                else
                                    echo "  Traffic resumed (${vrate} B/s) during verification — going back to 10s polling"
                                fi
                                verify_failed=1
                                break
                            fi
                            if [ -n "$cur_mtime" ] && [ "$cur_mtime" != "$last_mtime" ]; then
                                fresh_seen=1
                                last_mtime="$cur_mtime"
                            fi
                        done
                        if [ "$verify_failed" -eq 0 ] && [ "$fresh_seen" -eq 0 ]; then
                            echo "  No fresh billable_rate sample observed during verification window — going back to 10s polling"
                            verify_failed=1
                        fi
                    fi
                    if [ "$verify_failed" -eq 0 ]; then
                        echo "  Verification passed — proceeding with update..."
                        echo ""
                        do_update
                        break
                    else
                        quiet=0
                    fi
                fi
            else
                quiet=0
                if [ "$rate_known" -eq 1 ]; then
                    echo "  rate=${rate} B/s — traffic detected, resetting quiet timer"
                fi
            fi

            sleep 10
        done
        ;;

    update)
        do_update
        ;;

    *)
        echo "Operation '$operation' is not supported in Docker or should be handled via 'docker' commands (start/stop/restart)."
        exit 1
        ;;
esac
