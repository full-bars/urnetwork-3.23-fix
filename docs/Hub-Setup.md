# Hub Setup Guide

The Hub is a standalone dashboard binary that aggregates bandwidth and health reports from every provider node in your fleet into one live view. This guide covers installing it, pointing providers at it, securing it, and keeping it running.

> [!TIP]
> For a full feature tour (dashboard columns, SSE, JSON API, report format), see [Hub-Dashboard.md](Hub-Dashboard.md). This page is the setup walkthrough.

## Prerequisites

- One machine to host the hub (any fleet node works fine — it doesn't need to be a dedicated box)
- `urnet-tools` installed on that machine (comes bundled with a native `urnet-tools` install)
- Provider nodes that can reach the hub's port over the network (LAN, Tailscale, or public internet)

## 1. Install the Hub

On the machine that will host the dashboard:

```sh
urnet-tools hub install
```

This downloads the hub binary, installs it as a **systemd user service** (`urnetwork-hub.service`), and enables it to start on boot. By default it listens on `:8080` (HTTP) and persists data to `~/.local/share/urnetwork-hub/hub.db`.

> [!WARNING]
> `hub install` uses `systemctl --user enable --now`, which does **not** restart an already-running hub. If you're re-running `hub install` after a config change (e.g. pinning `URNETWORK_HUB_TAG`), follow up with:
> ```sh
> systemctl --user restart urnetwork-hub.service
> ```

Verify it's up:

```sh
systemctl --user status urnetwork-hub.service
journalctl --user -fu urnetwork-hub.service   # stream logs
```

Open `http://HUB_IP:8080` in a browser — you should see an empty dashboard (no nodes reporting yet).

### Pinning a version

By default `hub install` and `hub update` resolve the latest release tag. To pin a specific version:

```sh
URNETWORK_HUB_TAG=vX.Y.Z urnet-tools hub install
```

## 2. Point Providers at the Hub

On **every provider node** you want to appear on the dashboard:

```sh
urnet-tools hub set http://HUB_IP:8080
```

This writes `~/.urnetwork/report_url`, which the provider re-reads on its next report tick (default every 5 minutes) — no restart needed. Use the same hub URL across your entire fleet; all nodes report to one instance.

To stop a node from reporting without touching anything else:

```sh
urnet-tools hub off
```

Check what a node is currently pointed at:

```sh
urnet-tools report
```

> [!NOTE]
> Docker deployments use the same commands via `docker exec` under the hood — the `urnet-tools.ps1` PowerShell wrapper and the in-container `urnet-tools.sh` both expose `hub set` / `hub off` / `report`.

## 3. Secure the Connection (Recommended)

Reports contain fleet bandwidth and proxy details — don't send them over plaintext HTTP across the open internet. Pick one:

### Option A: Built-in TLS (best for a trusted network / VPN)

The hub has a TLS listener on `:8443` with an auto-generated self-signed ECDSA P-256 cert, using trust-on-first-use pinning (no CA, no reverse proxy needed).

```sh
# On the hub machine: turn on TLS (restarts the hub with HTTPS on :8443)
urnet-tools hub init

# Sanity check from anywhere:
urnet-tools hub test https://HUB_IP:8443

# On each provider: pin the hub's cert fingerprint and switch reporting to HTTPS
urnet-tools hub link https://HUB_IP:8443
```

The fingerprint is stored at `~/.urnetwork/hub.pin` on each provider and re-verified on every connection. A mismatch (e.g. the hub's cert was regenerated) refuses the connection and logs debug info to `/tmp/hub-tls-debug.txt` — re-run `hub link` to re-pin.

To roll back to plain reporting: `urnet-tools hub unlink`.

### Option B: Reverse proxy with a real domain (best for public-facing hubs)

If your fleet spans different networks, put a reverse proxy like [Caddy](https://caddyserver.com/) in front for automatic HTTPS and a clean hostname:

```caddyfile
hub.yourdomain.com {
    reverse_proxy localhost:8080
}
```

Then point providers at the domain instead of an IP:

```sh
urnet-tools hub set https://hub.yourdomain.com
```

## 4. Confirm It's Working

- Dashboard shows a row per node within one report cycle (up to 5 min) or one heartbeat (15s) after `hub set`.
- Green heartbeat dot = reporting normally; red = no report in 5+ minutes; check `URNETWORK_REPORT_URL` / `hub set` on that node if it stays red.
- TLS padlock icon appears next to nodes reporting over HTTPS — if some nodes show it and others don't, those nodes still need `hub link`.

## Running the Hub in Docker (Windows / Mac / any host)

`urnet-tools hub install` sets the hub up as a systemd user service, which only works on Linux. For Windows and macOS hosts — or any Linux box where you'd rather not manage a systemd service — build and run the hub as a container instead using `hub/Dockerfile`:

```sh
# From the repo root
docker build -f hub/Dockerfile -t urnetwork-hub .

docker run -d \
  --name urnetwork-hub \
  -p 8080:8080 \
  -v hubdata:/data \
  -e URNETWORK_HUB_TOKEN=YOUR_SHARED_SECRET \
  urnetwork-hub
```

- `-v hubdata:/data` — named volume holding `hub.db` (and `tls.crt`/`tls.key` if TLS is enabled). Persists across container recreation/updates; back it up with `docker volume` or `docker cp`.
- `URNETWORK_HUB_TOKEN` — sets the shared secret required on `/api/report` and `/api/nodes/remove`. Without it the hub starts but logs a warning and accepts unauthenticated reports — set this for anything beyond local testing.
- The image builds `CGO_ENABLED=0` since the hub's SQLite driver (`modernc.org/sqlite`) is pure Go — no gcc/musl needed, keeping the final image just the binary + `ca-certificates` on Alpine.

To enable the built-in TLS listener in the container, publish `8443` and set `URNETWORK_HUB_TLS_ADDR`:

```sh
docker run -d \
  --name urnetwork-hub \
  -p 8080:8080 -p 8443:8443 \
  -v hubdata:/data \
  -e URNETWORK_HUB_TOKEN=YOUR_SHARED_SECRET \
  -e URNETWORK_HUB_TLS_ADDR=:8443 \
  urnetwork-hub
```

The cert/key are generated into `/data` on first boot and persist in the volume, same as the native `hub init` flow. Get the fingerprint from the container logs on first start (`docker logs urnetwork-hub`) or via `/api/cert`, then `urnet-tools hub link https://HUB_IP:8443` on each provider as usual.

### Prebuilt Image

CI publishes multi-arch (amd64/arm64) images on every change under `hub/`, tagged independently from the provider's `v3.23.0-fix.X.Y` scheme — the hub uses its own `vX.Y.Z` versions starting at `v0.1.0`, cut via `hub-vX.Y.Z` git tags:

```sh
docker pull ghcr.io/full-bars/urnetwork-3.23-fix-hub:latest
# or
docker pull 3cape/urnetwork-hub:latest

docker run -d --name urnetwork-hub -p 8080:8080 -v hubdata:/data \
  -e URNETWORK_HUB_TOKEN=YOUR_SHARED_SECRET \
  ghcr.io/full-bars/urnetwork-3.23-fix-hub:latest
```

## Manual / Non-`urnet-tools` Setup

If you're not using `urnet-tools` (e.g. building from source):

```sh
cd hub && go build -o hub .
./hub -addr :9090 -data /var/hub-data   # custom port + data dir; defaults to :8080 and cwd
```

Point providers using the environment variable instead of `urnet-tools hub set`:

```sh
# Docker
docker run -d --name=urfix \
  -e URNETWORK_AUTH_CODE=YOUR_CODE \
  -e URNETWORK_REPORT_URL=http://HUB_IP:8080 \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest

# Native binary
URNETWORK_REPORT_URL=http://HUB_IP:8080 ./ur-provider
```

## Updating & Uninstalling

```sh
urnet-tools hub update             # pull latest (or $URNETWORK_HUB_TAG) hub binary, restarts service
systemctl --user stop urnetwork-hub.service     # stop without removing
systemctl --user disable urnetwork-hub.service  # remove from boot
```

## Troubleshooting

| Symptom | Likely Cause |
|---|---|
| Node never appears | `hub set`/`URNETWORK_REPORT_URL` not set on that node, or it can't reach `HUB_IP:8080` (firewall/NAT) |
| Node shows red heartbeat dot | No report in 5+ minutes — provider may be down or misconfigured |
| TLS padlock missing for some nodes | Those nodes are still on HTTP — run `urnet-tools hub link https://HUB_IP:8443` on them |
| `hub link` fails with "fingerprint mismatch" | Hub cert was regenerated (e.g. after data dir loss). Run `hub test` for the new fingerprint, then `hub link` again |
| `hub install` doesn't pick up new tag | Re-run with `URNETWORK_HUB_TAG=vX.Y.Z`, then `systemctl --user restart urnetwork-hub.service` |

For dashboard usage, report format, and the JSON/SSE API, see [Hub-Dashboard.md](Hub-Dashboard.md).
