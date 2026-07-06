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
        cert_json="$(fetch_json "${url}/api/ca-cert?token=${token}")" || true
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
            if [ "${HUB_LINK_YES:-0}" != "1" ]; then
                printf "Accept this fingerprint? (y/n) "
                read -r answer
                case "$answer" in [Yy]|[Yy][Ee][Ss]) ;; *) echo "Aborted."; exit 1 ;; esac
            fi
            mkdir -p "$hub_dir"
            printf '%s' "$ca_pem" | sed 's/\\n/\n/g' > "$ca_file.tmp" && mv "$ca_file.tmp" "$ca_file"
            chmod 600 "$ca_file"
            echo "CA certificate saved to $ca_file"
        elif [ -n "$legacy_fp" ]; then
            echo "WARNING: Hub does not support CA-based trust. Falling back to legacy fingerprint pinning."
            echo ""
            echo "Hub certificate fingerprint: $legacy_fp"
            echo ""
            if [ "${HUB_LINK_YES:-0}" != "1" ]; then
                printf "Accept this fingerprint? (y/n) "
                read -r answer
                case "$answer" in [Yy]|[Yy][Ee][Ss]) ;; *) echo "Aborted."; exit 1 ;; esac
            fi
            mkdir -p "$hub_dir"
            printf '%s\n' "$legacy_fp" > "$pin_file.tmp" && mv "$pin_file.tmp" "$pin_file"
            echo "Fingerprint pinned to $pin_file"
        else
            echo "Could not extract CA certificate or fingerprint from hub response."
            exit 1
        fi
    fi

    rm -f "$pin_file"
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

case "$operation" in
    proxy)
        subcmd="${1:-}"
        shift || true
        case "$subcmd" in
            health)  exec /usr/local/bin/proxy-health ;;
            traffic) exec /usr/local/bin/proxy-traffic ;;
            add|clear|refresh|remove-dead|remove|exclude)
                exec /usr/local/bin/provider proxy "$subcmd" "$@"
                ;;
            *)
                echo "Unknown proxy command: $subcmd (Try 'health', 'traffic', 'add', 'clear', 'refresh', 'remove-dead', 'remove --match=<pat>', or 'exclude')"
                exit 1
                ;;
        esac
        ;;
    logs)
        exec /usr/local/bin/logs "$@"
        ;;
    status)
        echo "URNetwork Provider (Docker)"
        /usr/local/bin/provider -v
        echo "Status: Running"
        ;;
    -v|version)
        exec /usr/local/bin/provider -v
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
                echo "  docker pull ghcr.io/full-bars/urnetwork-3.23-fix:latest"
                echo "Or re-create the container with the updated image."
                exit 1
                ;;
            *) echo "Unknown hub command: $subcmd (try 'link', 'unlink', 'onboard-cmd', 'show-password')"; exit 1 ;;
        esac
        ;;
    *)
        echo "Operation '$operation' is not supported in Docker or should be handled via 'docker' commands (start/stop/restart)."
        exit 1
        ;;
esac
