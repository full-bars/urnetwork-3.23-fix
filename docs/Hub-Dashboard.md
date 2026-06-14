# 📊 Bandwidth Hub Dashboard

![Hub Dashboard Preview](hub-dashboard-preview.png)

A live fleet monitoring dashboard that aggregates bandwidth reports from all provider nodes. The hub runs as a standalone binary, accepts periodic POSTs to `/api/report`, and renders an HTML dashboard at the root path.

## Architecture

```
┌─────────┐   POST /api/report (every 15s)   ┌─────────┐
│ Provider│ ────────────────────────────────▶ │  Hub    │
│  (node) │                                   │ :8080   │
└─────────┘                                   └─────────┘
                                                    │
                                                    ▼
                                             ┌──────────────┐
                                             │  hub.json     │
                                             │  (persistent  │
                                             │   state)      │
                                             └──────────────┘
```

Each provider sends a bandwidth report containing per-proxy traffic counters, health status, and system metrics. The hub stores the latest state from each node in `hub.json` and computes delta-based traffic rates between consecutive reports.

## Quick Start

### Option A: urnet-tools (native Linux installs, recommended)

If you installed via `urnet-tools`, the hub binary is bundled in the same release. One command installs and starts it as a systemd user service:

```sh
# Install hub as a systemd user service (downloads binary, enables on startup)
urnet-tools hub install

# Point the provider on this machine at the hub
urnet-tools hub set http://localhost:8080

# Point providers on other machines at this hub
urnet-tools hub set http://192.0.2.10:8080
# or with a domain name / HTTPS reverse proxy:
urnet-tools hub set https://hub.yourdomain.com
```

To stop hub reporting without uninstalling anything:

```sh
urnet-tools hub off
```

Stream the hub's own logs:

```sh
journalctl --user -fu urnetwork-hub.service
```

> [!NOTE]
> `hub install` writes a systemd drop-in that persists across reboots. The hub data file lives at `~/.local/share/urnetwork-hub/hub.json`.

---

### Option B: Build & Run Manually

```sh
# Build from source
cd hub && go build -o hub .

# Run (default :8080, data in current dir)
./hub

# Custom port and data path
./hub -addr :9090 -data /var/hub-data
```

### Configure Providers to Report (Manual / Docker)

Set the `URNETWORK_REPORT_URL` environment variable on each provider:

```sh
# Docker
docker run -d \
  --name=urfix \
  -e URNETWORK_AUTH_CODE=YOUR_CODE \
  -e URNETWORK_REPORT_URL=http://HUB_IP:8080 \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest

# Native binary (without urnet-tools)
URNETWORK_REPORT_URL=http://HUB_IP:8080 ./ur-provider
```

> [!TIP]
> Use a single hub instance for your entire fleet. All nodes report to the same URL.

## Secure Deployment (Reverse Proxy)

> [!TIP]
> If your nodes are distributed across different networks, it is highly recommended to deploy the Hub behind a reverse proxy like [Caddy](https://caddyserver.com/). This provides automatic HTTPS (SSL/TLS) and allows you to use a clean Domain or DDNS URL.

**1. Caddyfile Configuration**
Create or update your `Caddyfile` to route your domain to the local Hub instance (running on port 8080 by default):

```caddyfile
hub.yourdomain.com {
    reverse_proxy localhost:8080
}
```

**2. Node Configuration**
Configure your provider nodes to report to your new secure URL instead of the raw IP:

```sh
# Docker setup
-e URNETWORK_REPORT_URL=https://hub.yourdomain.com \

# Native binary setup
URNETWORK_REPORT_URL=https://hub.yourdomain.com ./ur-provider
```

## Dashboard Features

### Summary Bar

- **Nodes** — total reporting nodes
- **Proxies** — aggregate up / connecting / degraded / dead counts
- **Clients** — total active client sessions
- **RX / TX** — total traffic with billable breakdown (`1.3 GB · 1.2 GB billable`)

### Node Table

| Column | Description |
|--------|-------------|
| Node | Node ID (name) |
| Host | Hostname and version |
| Heartbeat | Time since last report, with green/yellow/red health dot |
| Uptime | Provider uptime |
| Proxies | Color-coded status badges (up/connecting/degraded) |
| Clients | Active client sessions on this node |
| RX | Total received bytes / billable received (smaller text) |
| TX | Total transmitted bytes / billable transmitted |
| In Mbps | Inbound traffic rate (delta-based) |
| Out Mbps | Outbound traffic rate (delta-based) |
| Heap | Go heap memory (MiB) |
| Conns | Active network connections |

- **Sortable**: Click any column header to sort ascending/descending
- **Sort indicator**: ▼ (descending) or ▲ (ascending) arrow in the active sort column
- **Default sort**: By proxy count (highest first)

### Per-Proxy Drilldown

Click any node row to expand its full proxy list:

- **ID** — proxy identifier and address
- **Address** — IP:port
- **Status** — up / connecting / degraded
- **Clients** — active sessions on this proxy
- **Max Age** — longest-running connection
- **RX** — total received bytes
- **TX** — total transmitted bytes
- **Bill RX** — billable received bytes
- **Bill TX** — billable transmitted bytes

Click the expanded row again to collapse.

### Auto-Refresh

The dashboard reloads every **30 seconds**. A countdown shows time until the next refresh. Toggle auto-refresh off with the checkbox to pause updates.

### JSON API

For external tooling:

```sh
curl http://HUB_IP:8080/api/nodes
```

Returns the full node state as JSON, including all proxy details and system metrics.

## Report Format

Providers POST to `/api/report` with this structure:

```json
{
  "node_id": "vegas",
  "host": "vegas.example.com",
  "version": "v3.23.0-fix.19",
  "uptime": 3600.0,
  "proxies": [
    {
      "id": "proxy[0] (192.168.1.1:1080)",
      "addr": "192.168.1.1:1080",
      "status": "up",
      "rx": 1024000,
      "tx": 512000,
      "bill_rx": 900000,
      "bill_tx": 400000,
      "clients": 3,
      "max_age_s": 120
    }
  ],
  "sys": {
    "heap_mib": 64,
    "sys_mib": 128,
    "conns": 42
  }
}
```

## Rate Tracking

The hub computes per-node traffic rates as:

```
mbps = (current_bytes - previous_bytes) / elapsed_seconds × 8 / 1_000_000
```

Rates are shown in the **In Mbps** and **Out Mbps** columns. A node needs at least two reports at least one second apart to show a rate.

## Billable vs Total Traffic

Each proxy report carries both `rx`/`tx` (total wire bytes) and `bill_rx`/`bill_tx` (billable bytes). The dashboard distinguishes them:

- **Summary bar**: `RX 1.3 GB · 1.2 GB billable`
- **Node rows**: Total on top, billable below in muted text with `b` suffix
- **Proxy detail**: Separate columns for Bill RX and Bill TX

This lets you see at a glance how much traffic is revenue-generating vs. total wire overhead.

## Troubleshooting

| Symptom | Likely Cause |
|---------|-------------|
| Node shows "—" for rates | Only one report received; wait for the next report cycle |
| Node shows red dot | No report received in >5 minutes |
| Dashboard shows stale data | Provider process may be down or `URNETWORK_REPORT_URL` is misconfigured |
| No proxies in drilldown | Provider sent empty proxy list; check provider logs |
| "b" shows 0 B for billable | Proxy did not report billable bytes (may be in warmup state) |

## Data Storage

The hub persists node state to `hub.json` (in the configured `-data` directory). This file can be inspected or backed up.

```sh
# View the raw state
cat /var/hub-data/hub.json

# Count reporting nodes
jq '.nodes | length' /var/hub-data/hub.json
```
