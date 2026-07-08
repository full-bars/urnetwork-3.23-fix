# AI Guide — URnetwork Provider & Hub

This file is designed for an AI agent to read so it can help users install, configure, and manage the provider and hub. It references the project's existing documentation rather than duplicating it. Give this file to your AI alongside the linked docs below.

## Project Layout

See [PROJECT_STRUCTURE.md](PROJECT_STRUCTURE.md) for the full directory layout. Quick orientation:

- `provider/` — the traffic-relay binary that serves proxies and earns money
- `hub/` — standalone dashboard server that collects fleet metrics
- `scripts/Provider_Install_Linux.sh` — the installer that doubles as the `urnet-tools` CLI
- `docs/` — user-facing documentation (Installation, Configuration, Proxies, Hub, etc.)
- `Dockerfile` — provider Docker image
- `hub/Dockerfile` — hub Docker image

## Quick Start: Install the Provider

```bash
curl -fsSL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/Provider_Install_Linux.sh | sh -s -- install
```

The same script becomes the `urnet-tools` CLI after installation. Full install docs: [docs/Installation.md](docs/Installation.md).

### Authenticate

You need an **auth code** from the URnetwork website. The provider exchanges it for a JWT.

```bash
urnet-tools auth                    # interactive — paste auth code at prompt
urnet-tools auth <auth-code>        # non-interactive
urnet-tools auth <jwt>              # or paste a JWT directly
```

Docker deployments can use email/password instead (see [Docker Deployment](docs/Docker-Deployment.md)), which the container exchanges for a JWT internally.

### Add Proxies

```bash
urnet-tools proxy add file proxies.txt     # one ip:port per line
urnet-tools proxy add url https://...      # auto-refreshing URL source
urnet-tools proxy summary                  # fleet-wide proxy overview
```

Full proxy docs: [docs/Proxy-Management.md](docs/Proxy-Management.md), [docs/Proxy-URL-Sources.md](docs/Proxy-URL-Sources.md).

## Docker Deployment

Full guide: [docs/Docker-Deployment.md](docs/Docker-Deployment.md).

```bash
docker pull ghcr.io/full-bars/urnetwork-3.23-fix:latest
docker run -d \
  -e USER_AUTH=email@example.com \
  -e PASSWORD=secret \
  -v /path/to/proxies.txt:/app/proxies.txt:ro \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

## Performance Tuning

Full guide: [docs/High-Volume-Performance-Tuning.md](docs/High-Volume-Performance-Tuning.md).

```bash
urnet-tools turbo v8          # high-throughput profile
urnet-tools auto on           # auto-tune based on detected RAM
urnet-tools eco on            # low-memory GC tuning
urnet-tools optimize          # apply OS kernel limits (ulimit, conntrack, etc.)
urnet-tools ramlogs on        # redirect logs to /dev/shm (RAM disk)
```

## Hub Dashboard

Full guides: [docs/Hub-Setup.md](docs/Hub-Setup.md), [docs/Hub-Dashboard.md](docs/Hub-Dashboard.md).

The hub collects reports from providers and shows a web dashboard with fleet metrics (proxies, contracts, traffic, earnings). One hub serves many providers.

### Install

```bash
urnet-tools hub install
```

### Update

```bash
urnet-tools hub update
urnet-tools hub update -t v3.23.0-fix.25.9    # specific version
```

The update stops the hub, backs up the database, downloads the new binary, atomically swaps it, and restarts. ~10-15 seconds. Database: `~/.local/share/urnetwork-hub/hub.db` (SQLite, WAL mode).

### Dashboard URLs

- Dashboard: `http://<hub-ip>:8080`
- TLS dashboard: `https://<hub-ip>:8443`

Pages: Overview (#overview), Servers (#servers), Proxies (#proxies), Contracts (#contracts), Best Proxies (#best). Each has a unique hash URL that can be bookmarked.

## TLS Encryption (Provider → Hub)

Full guide: [docs/Hub-Setup.md](docs/Hub-Setup.md) (TLS section).

TLS encrypts reports between providers and the hub using a long-lived CA certificate. The hub certificate (signed by the CA) changes on restarts, but CA trust survives restarts — so providers only need to onboard once.

### Onboard Providers

On the hub, mint a 15-minute token:
```bash
urnet-tools hub onboard-cmd
```

Run the printed `curl | sh` command on each provider:
```bash
curl -fsSL http://<hub-ip>:8080/onboard.sh | sh -s -- <token>
```

This does NOT restart the provider. It writes two files:
- `~/.urnetwork/hub_ca.pem` — the CA certificate (trust anchor)
- `~/.urnetwork/report_url` — set to `https://<hub-ip>:8443`

The provider picks up the change on its next report tick (~5 minutes, no restart needed).

### Verify TLS

In the hub dashboard, TLS-enabled providers show a padlock icon. Or check the API:
```bash
curl -s http://<hub>:8080/api/nodes/ | jq '.[] | {node_id, tls}'
```

### Legacy Pin Warning

The non-token `urnet-tools hub link` flow (without `--token`) uses certificate fingerprint pinning. This breaks on every hub restart because the hub certificate regenerates. Always use the token-based CA cert flow (`curl | sh` onboard).

## Reference: All urnet-tools Commands

```bash
# Auth & Setup
urnet-tools auth <email> <password>
urnet-tools update                 # update provider binary
urnet-tools reinstall              # full reinstall

# Proxy Management
urnet-tools proxy add file <path>  # bulk add from file
urnet-tools proxy add url <url>    # auto-refreshing URL source
urnet-tools proxy remove <addr>    # remove specific proxy
urnet-tools proxy remove --match <pattern>  # pattern-based removal
urnet-tools proxy remove-dead      # interactive dead proxy cleanup
urnet-tools proxy refresh          # hot-reload proxies (no restart)
urnet-tools proxy clear            # remove all proxies
urnet-tools proxy summary          # fleet-wide proxy overview
urnet-tools proxy health           # dead/degraded proxy list
urnet-tools proxy traffic          # real-time bandwidth & clients

# Performance
urnet-tools turbo v4|v8|off        # throughput profile
urnet-tools auto on|off            # auto-tune profile
urnet-tools eco on|off             # low-memory GC mode
urnet-tools lowmode on|off         # reduced buffer allocations
urnet-tools optimize               # apply Golden Fleet OS limits
urnet-tools ramlogs on|off         # RAM-disk logging

# Hub
urnet-tools hub install            # install hub + systemd service
urnet-tools hub update [-f] [-t <tag>]  # update hub binary
urnet-tools hub link <url> [--token]    # configure TLS for this provider
urnet-tools hub onboard-cmd        # mint 15-min onboarding token
urnet-tools hub test               # test connectivity to hub
urnet-tools hub show-password      # show hub CA password (one-time secret)
urnet-tools hub set <key> <value>  # configure hub settings
urnet-tools hub off                # stop and disable hub
urnet-tools hub unlink             # remove TLS config, fall back to HTTP

# Reporting
urnet-tools report [<url>|off]     # set/show/disable hub report URL

# Maintenance
urnet-tools start|stop|restart|status  # service management
urnet-tools logs [all|dump|-i]     # stream logs (-i = important only)
urnet-tools uninstall              # full removal

# Experimental
urnet-tools hot-restart on|off     # client JWT reuse (default off)
urnet-tools fast-auth on|off       # bypass auth rate limiter
```

## Environment Variables

Full reference: [docs/Configuration.md](docs/Configuration.md).

Key variables:
| Variable | Purpose |
|----------|---------|
| `USER_AUTH` / `PASSWORD` | Email/password auth |
| `URNETWORK_AUTH_CODE` | JWT auth code (first-run) |
| `ENABLE_VNSTAT` | Traffic monitor on port 8080 |
| `URNETWORK_PROFILE` | `auto`, `lowmem`, `eco`, `turbo-v4`, `turbo-v8` |
| `URNETWORK_REPORT_URL` | Hub URL for bandwidth reports |
| `URNETWORK_REPORT_INTERVAL` | Report interval (default 5m) |
| `PROXY_URL` | Live proxy URL feed (comma-separated) |
| `URNETWORK_HEALTH_INTERVAL` | Health heartbeat interval |
| `URNETWORK_RAMLOGS` | `1` = log to /dev/shm |
| `URNETWORK_HOT_RESTART` | `1` = experimental client JWT reuse |

## Hub API Endpoints

| Endpoint | Description |
|----------|-------------|
| `GET /` | Full HTML dashboard |
| `POST /api/report` | Bandwidth report from providers |
| `POST /api/heartbeat` | Lightweight heartbeat |
| `GET /api/nodes/` | All node summaries (JSON) |
| `GET /api/nodes/<id>/proxies` | Single node's proxy detail |
| `POST /api/nodes/remove` | Remove node from dashboard |
| `GET /api/nodes/contracts` | Per-node contract history |
| `GET /api/proxies/top` | Top proxies leaderboard |
| `GET /api/proxies/best` | Best proxies (composite score) |
| `GET /api/proxies/history` | Per-proxy time series |
| `GET /api/history` | Fleet-wide hourly rollups |
| `GET /api/events` | SSE live-update stream |
| `GET /api/cert` | TLS certificate + fingerprint |
| `GET /api/ca-cert?token=` | CA cert for onboarding |
| `GET /onboard.sh` | Onboarding shell script |

## Release & Versioning

- **Provider releases** tagged `v3.23.0-fix.X.Y` — includes provider tarball + hub binary
- **Hub Docker tags** tagged `hub-docker-vX.Y.Z` — Docker images only, no binaries
- `urnet-tools update` and `urnet-tools hub update` resolve "latest" to the newest `v*` tag
- Release notes in `releases/` directory

## Key Files

| Path | Purpose |
|------|---------|
| `~/.local/share/urnetwork-provider/bin/urnetwork` | Provider binary |
| `~/.local/share/urnetwork-provider/bin/urnet-tools` | CLI tool |
| `~/.local/share/urnetwork-hub/hub.db` | Hub SQLite database |
| `~/.urnetwork/hub_ca.pem` | CA cert for TLS reporting |
| `~/.urnetwork/report_url` | Hub URL this provider reports to |
| `~/.urnetwork/hub.pin` | Legacy fingerprint pin (remove if using CA) |
| `~/.config/systemd/user/urnetwork.service` | Provider systemd unit |
| `~/.config/systemd/user/urnetwork-hub.service` | Hub systemd unit |
| `~/.config/systemd/user/urnetwork.service.d/override.conf` | Provider env overrides |
| `/dev/shm/urnetwork.log` | Provider ramdisk log (when ramlogs enabled) |

## Troubleshooting

### Provider not reporting
1. Check ramlog (if ramlogs enabled): `tail -100 /dev/shm/urnetwork.log | grep report`
2. Check report URL: `cat ~/.urnetwork/report_url`
3. Test hub connectivity: `curl http://<hub>:8080/api/nodes/`
4. Provider logs: `urnet-tools logs -i`

### TLS errors / padlock missing
1. Verify CA cert: `ls -la ~/.urnetwork/hub_ca.pem`
2. Remove legacy pin: `rm -f ~/.urnetwork/hub.pin`
3. Verify report URL is HTTPS: `cat ~/.urnetwork/report_url`
4. Re-onboard: run the `curl | sh` onboard script

### Hub dashboard slow
1. Restart hub: `urnet-tools hub update` (or `systemctl --user restart urnetwork-hub.service`)
2. Database grows with fleet activity (~5GB in ~6 days for a 28-node fleet). The `hub.db.bak` backup doubles usage during updates. Retention policies bound growth (hourly data kept 90 days).

### Hub update stuck / "already at version"
Use force: `urnet-tools hub update -f -t <tag>`

## Documentation Index

| Doc | Covers |
|-----|--------|
| [Installation](docs/Installation.md) | Bare-metal install, post-install setup |
| [Docker Deployment](docs/Docker-Deployment.md) | Docker run, Compose, Watchtower, scaling |
| [Configuration](docs/Configuration.md) | Complete env var reference, profiles |
| [Proxy Management](docs/Proxy-Management.md) | Hot-reload, stable slots, dead cleanup |
| [Proxy URL Sources](docs/Proxy-URL-Sources.md) | Live proxy feeds, dedup, cleanup |
| [Hub Setup](docs/Hub-Setup.md) | Hub install, TLS, Caddy reverse proxy |
| [Hub Dashboard](docs/Hub-Dashboard.md) | Dashboard tour, API, data model |
| [Performance Tuning](docs/High-Volume-Performance-Tuning.md) | Profiles, optimizer, parameters |
| [Troubleshooting](docs/Troubleshooting.md) | Exit codes, errors, resource issues |
| [Project Structure](PROJECT_STRUCTURE.md) | Directory layout, architecture |
| [Log Reference](LOG_REFERENCE.md) | Every log line documented |
| [Fork Changes](FORK_CHANGES.md) | All ~66 modifications from upstream |
