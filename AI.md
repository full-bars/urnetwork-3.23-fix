# AI.md — Provider & Hub Deployment Guide for AI Agents

This file is designed to be read by an AI agent (or a human) to quickly understand how to install, configure, and manage the URnetwork provider and hub dashboard. Give this file to your AI and it will be able to deploy everything end-to-end.

## Quick Start: Install the Provider

The provider routes traffic through proxies and earns money. It reports to a hub (optional) for fleet monitoring.

```bash
# Fresh install (non-Docker, systemd)
curl -fsSL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/Provider_Install_Linux.sh | sh -s -- install

# The same script becomes the `urnet-tools` CLI after installation.
# Location: ~/.local/share/urnetwork-provider/bin/urnet-tools
```

### Authentication

Before the provider can serve traffic, it needs to authenticate:

```bash
urnet-tools auth <email> <password>
# Or set env vars for automated deployments:
# URNETWORK_USER_AUTH=<email> URNETWORK_PASSWORD=<password>
```

### Proxy Configuration

The provider needs proxy addresses to serve. Add them with:

```bash
# From a text file (one ip:port per line)
urnet-tools proxy add file /path/to/proxies.txt

# From a URL (socks5 list, refreshed periodically)
urnet-tools proxy add url https://example.com/proxies.txt

# View current proxy pool
urnet-tools proxy summary
```

### Docker Deployment

```bash
docker pull ghcr.io/full-bars/urnetwork-3.23-fix:latest
docker run -d \
  -e USER_AUTH=email@example.com \
  -e PASSWORD=secret \
  -v /path/to/proxies.txt:/app/proxies.txt:ro \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

## Hub Dashboard

The hub collects reports from providers and shows fleet-wide metrics (proxies, contracts, traffic, earnings). One hub serves many providers.

### Install the Hub

```bash
urnet-tools hub install
# Or with a specific tag:
urnet-tools hub install -t v3.23.0-fix.25.9
```

### Update the Hub

```bash
urnet-tools hub update
# Or to a specific version:
urnet-tools hub update -t v3.23.0-fix.25.9
```

The update stops the hub, backs up the database, downloads the new binary, atomically swaps it, and restarts. Takes ~10-15 seconds.

### Hub Endpoints

| Port | Protocol | Purpose |
|------|----------|---------|
| 8080 | HTTP | Provider reports, dashboard, onboard script |
| 8443 | HTTPS | Encrypted provider reports, admin (when TLS configured) |

Dashboard: `http://<hub-ip>:8080`

### Hub Data

The hub stores data in `~/.local/share/urnetwork-hub/hub.db` (SQLite, WAL mode). The database is backed up before every update (`hub.db.bak`).

## Enabling TLS (Encrypted Provider Reports)

TLS encrypts reports between providers and the hub using a CA certificate. All providers trust the same CA, and the hub certificate (signed by the CA) changes on restarts — but CA trust survives restarts.

### Onboard Providers to TLS

**Method 1: `curl | sh` one-liner (recommended)**

On the hub, mint a 15-minute token:
```bash
urnet-tools hub onboard-cmd
```

Copy the `curl | sh` command and run it on each provider. Example:
```bash
curl -fsSL http://<hub-ip>:8080/onboard.sh | sh -s -- <token>
```

This does NOT restart the provider. It writes two files:
- `~/.urnetwork/hub_ca.pem` — the CA certificate
- `~/.urnetwork/report_url` — set to `https://<hub-ip>:8443`

The provider picks up the change on its next report tick (every ~5 minutes, no restart needed).

**Method 2: `urnet-tools hub link`**

```bash
urnet-tools hub link https://<hub-ip>:8443 --token <token>
```

Note: requires a recent `urnet-tools` version. The `curl | sh` method works on any version.

### Verify TLS Status

In the hub dashboard, TLS-enabled providers show a padlock icon. Or check the API:

```bash
curl -s http://<hub>:8080/api/nodes/ | jq '.[] | {node_id, tls}'
```

### Legacy Pin Warning

The non-token `urnet-tools hub link` flow (without `--token`) uses certificate fingerprint pinning (TOFU). This breaks on every hub restart because the hub certificate regenerates. Always use the token-based CA cert flow.

## Common urnet-tools Commands

```bash
# Provider management
urnet-tools auth <email> <password>     # Authenticate
urnet-tools proxy add file <path>       # Add proxies from file
urnet-tools proxy add url <url>         # Add proxies from URL (auto-refreshes)
urnet-tools proxy remove <ip:port>      # Remove a proxy
urnet-tools proxy summary               # Fleet-wide proxy overview
urnet-tools update                       # Update provider binary
urnet-tools report <url>                 # Set hub report URL at runtime

# Hub management
urnet-tools hub install                 # Install hub + systemd service
urnet-tools hub update                  # Update hub binary
urnet-tools hub link <url> [--token]    # Configure TLS for this provider
urnet-tools hub onboard-cmd             # Mint a 15-min onboard token
urnet-tools hub test                    # Test connectivity to hub
urnet-tools hub show-password           # Show the hub.password (one-time secret)
urnet-tools hub set <key> <value>       # Configure hub settings
urnet-tools hub off                     # Stop and disable hub service
urnet-tools hub unlink                  # Remove TLS config, fall back to HTTP

# Experimental
urnet-tools hot-restart on|off          # Enable/disable client JWT reuse (experimental)
```

## Release & Versioning

- **Provider releases**: tagged `v3.23.0-fix.X.Y` — includes provider tarball + hub binary. `urnet-tools update` and `urnet-tools hub update` both resolve "latest" to the newest `v*` tag.
- **Hub Docker-only tags**: tagged `hub-docker-vX.Y.Z` — Docker images only, no binaries.
- **Release notes**: in `releases/` directory alongside each tag.

## Fleet SSH Access

Server details are in FLEET.md (public IPs, Tailscale IPs, usernames). SSH key: `~/.ssh/id_ed25519`.

Most servers use `user@<public-ip>`. Exceptions:
- Chicago, Phoenix, Multivortex, AMD, AMD2: `ubuntu@`
- Vegas: port 24842, `user@`

## Key Files & Directories

| Path | Purpose |
|------|---------|
| `~/.local/share/urnetwork-provider/` | Provider binary, urnet-tools, config |
| `~/.local/share/urnetwork-provider/bin/urnetwork` | Provider binary |
| `~/.local/share/urnetwork-provider/bin/urnet-tools` | CLI tool |
| `~/.local/share/urnetwork-hub/hub.db` | Hub SQLite database |
| `~/.local/share/urnetwork-hub/hub.db.bak` | Database backup (pre-update) |
| `~/.urnetwork/hub_ca.pem` | CA certificate for TLS reporting |
| `~/.urnetwork/report_url` | Hub URL this provider reports to |
| `~/.urnetwork/hub.pin` | Legacy certificate fingerprint pin (remove if using CA) |
| `~/.config/systemd/user/urnetwork.service` | Provider systemd unit |
| `~/.config/systemd/user/urnetwork-hub.service` | Hub systemd unit |
| `~/.config/systemd/user/urnetwork.service.d/override.conf` | Provider env overrides |
| `/dev/shm/urnetwork.log` | Provider ramdisk log (ramlogs) |
| `~/.urnetwork/.client_jwts.json` | Client JWT persistence store (experimental) |

### Important Systemd Commands

```bash
# Provider
systemctl --user status urnetwork.service
systemctl --user restart urnetwork.service
journalctl --user -u urnetwork.service -f

# Hub
systemctl --user status urnetwork-hub.service
systemctl --user restart urnetwork-hub.service
journalctl --user -u urnetwork-hub.service -f
```

## Troubleshooting

### Provider not reporting

1. Check ramlog: `tail -100 /dev/shm/urnetwork.log | grep report`
2. Check report_url: `cat ~/.urnetwork/report_url`
3. Test connectivity: `curl http://<hub>:8080/api/nodes/`
4. Check provider status: `systemctl --user status urnetwork.service`

### TLS errors / padlock missing

1. Verify CA cert exists: `ls -la ~/.urnetwork/hub_ca.pem`
2. Verify no legacy pin: `ls ~/.urnetwork/hub.pin` (should not exist)
3. Re-onboard: run `curl | sh` onboard script again
4. Report URL should be HTTPS: `cat ~/.urnetwork/report_url`

### Hub dashboard slow or errors

1. Check hub status: `systemctl --user status urnetwork-hub.service`
2. Check hub logs: `journalctl --user -u urnetwork-hub.service -n 50`
3. Restart hub: `urnet-tools hub update` (or `systemctl --user restart urnetwork-hub.service`)
4. Database size: `ls -lh ~/.local/share/urnetwork-hub/hub.db`

### Hub update fails / "already at version"

Use `--force`: `urnet-tools hub update -f -t <tag>`

### Database very large

The hub SQLite database grows with fleet activity. A 28-node fleet generates ~5.5GB in ~6 days. The `hub.db.bak` file doubles disk usage during updates. Pruning policies (hourly retention, daily rollup) keep growth bounded but the current retention window is 90 days for hourly data.
