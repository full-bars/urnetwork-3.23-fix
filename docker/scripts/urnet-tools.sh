#!/bin/bash
# urnet-tools -- Docker wrapper for URNetwork provider management
set -eu

operation="${1:-}"
[ -z "$operation" ] && { echo "Usage: urnet-tools <command> [args]"; exit 1; }
shift

hub_link() {
    url="${1%/}"
    case "$url" in
        https://*) ;;
        *) echo "Hub link URL must start with https://"; echo "Usage: urnet-tools hub link https://<hub-host>:8443"; exit 1 ;;
    esac

    hub_dir="$HOME/.urnetwork"
    pin_file="$hub_dir/hub.pin"
    report_file="$hub_dir/report_url"

    echo "Fetching hub certificate from $url/api/cert ..."
    cert_json=""
    if command -v curl > /dev/null; then
        cert_json="$(curl -k --connect-timeout 10 -fSsL "$url/api/cert" 2>/dev/null)" || true
    elif command -v wget > /dev/null; then
        cert_json="$(wget -q --no-check-certificate --timeout=10 -O - "$url/api/cert" 2>/dev/null)" || true
    fi
    if [ -z "$cert_json" ]; then
        echo "Could not reach hub at $url. Is the hub running and reachable?"
        exit 1
    fi

    fingerprint="$(printf '%s' "$cert_json" | sed -n 's/.*"fingerprint" *: *"\([^"]*\)".*/\1/p')"
    if [ -z "$fingerprint" ]; then
        echo "Could not extract fingerprint from hub response."
        echo "Response: $cert_json"
        exit 1
    fi

    echo ""
    echo "Hub certificate fingerprint:"
    echo "  $fingerprint"
    echo ""

    if [ "${HUB_LINK_YES:-0}" != "1" ]; then
        printf "Accept this fingerprint? (y/n) "
        read -r answer
        case "$answer" in
            [Yy]|[Yy][Ee][Ss]) ;;
            *) echo "Aborted."; exit 1 ;;
        esac
    fi

    mkdir -p "$hub_dir"
    printf '%s\n' "$fingerprint" > "$pin_file.tmp" && mv "$pin_file.tmp" "$pin_file"
    echo "Fingerprint pinned to $pin_file"
    printf '%s\n' "$url" > "$report_file.tmp" && mv "$report_file.tmp" "$report_file"
    echo "Report URL set to $url"
    echo ""
    echo "Success. The provider will now send encrypted reports to $url."
    echo "The change takes effect on the next report tick (no restart needed)."
}

hub_unlink() {
    hub_dir="$HOME/.urnetwork"
    pin_file="$hub_dir/hub.pin"
    report_file="$hub_dir/report_url"

    rm -f "$pin_file"
    echo "Removed $pin_file"

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
            update|install|init)
                echo "In Docker, update the hub by pulling a new image:"
                echo "  docker pull ghcr.io/full-bars/urnetwork-3.23-fix:latest"
                echo "Or re-create the container with the updated image."
                exit 1
                ;;
            *) echo "Unknown hub command: $subcmd (try 'link <url>' or 'unlink')"; exit 1 ;;
        esac
        ;;
    *)
        echo "Operation '$operation' is not supported in Docker or should be handled via 'docker' commands (start/stop/restart)."
        exit 1
        ;;
esac
