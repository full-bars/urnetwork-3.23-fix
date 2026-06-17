#!/bin/bash
# urnet-tools -- Docker wrapper for URNetwork provider management
set -eu

operation="${1:-}"
[ -z "$operation" ] && { echo "Usage: urnet-tools <command> [args]"; exit 1; }
shift

case "$operation" in
    proxy)
        subcmd="${1:-}"
        shift || true
        case "$subcmd" in
            health)  exec /usr/local/bin/proxy-health ;;
            traffic) exec /usr/local/bin/proxy-traffic ;;
            add|clear|refresh|remove-dead)
                exec /usr/local/bin/provider proxy "$subcmd" "$@"
                ;;
            *)
                echo "Unknown proxy command: $subcmd (Try 'health', 'traffic', 'add', 'clear', 'refresh', or 'remove-dead')"
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
        exec /usr/local/bin/provider hub "$@"
        ;;
    *)
        echo "Operation '$operation' is not supported in Docker or should be handled via 'docker' commands (start/stop/restart)."
        exit 1
        ;;
esac
