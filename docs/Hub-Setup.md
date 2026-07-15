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

> [!TIP]
> On a Linux box you'd rather not manage a systemd service on, `urnet-tools hub install --docker` runs the hub as a Docker container instead — same command, containerized. See [Running the Hub in Docker](#running-the-hub-in-docker-windows--mac--any-host) below. macOS and Windows always use this path (`urnet-tools hub install`, no flag needed) since neither has a native hub binary.

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

### Option A: Password-derived CA (recommended for any fleet)

The hub derives an Ed25519 CA from a password, signs an ephemeral ECDSA P-256 leaf cert (rotated every 48h), and serves TLS on `:8443`. Providers verify the hub's leaf cert against the CA certificate — no TOFU, no fingerprint pinning, no reverse proxy.

```sh
# On the hub machine: set a password and turn on TLS
urnet-tools hub init --password "your-password-here"

# Or let the hub auto-generate one, then retrieve it:
urnet-tools hub init
urnet-tools hub show-password   # prints the auto-generated password

# Sanity check from anywhere:
urnet-tools hub test https://HUB_IP:8443

# On each provider: fetch the CA cert and verify future connections
urnet-tools hub link https://HUB_IP:8443
```

**Password is only needed if you re-deploy the hub on a new machine** — the same password always derives the same CA. Providers never see the password; they only need the CA public cert (`~/.urnetwork/hub_ca.pem`). If the hub's leaf cert rotates (every 48h), providers trust the new cert automatically via CA chain verification.

To roll back to plain HTTP reporting: `urnet-tools hub unlink`.

#### Zero-touch onboard for new providers

Mint a 15-minute join token that any provider can use to fetch the CA cert and link itself — no SSH to each box:

```sh
urnet-tools hub onboard-cmd
# Prints: curl -fsSL http://<this-host>:8080/onboard.sh | sh -s -- <token>
```

Paste that one-liner on each provider. The same token works for the entire fleet within the 15-minute window.

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

### Locking Down the Dashboard

By default, the fleet dashboard (`/`) and all read-only API endpoints (`/api/nodes/*`, `/api/proxies/*`, `/api/history`, `/api/events`) are open to anyone who can reach the hub's address — no auth required. Set `URNETWORK_HUB_DASHBOARD_PASS` to gate them behind HTTP Basic Auth:

```sh
-e URNETWORK_HUB_DASHBOARD_PASS=your-dashboard-password
```

Any username is accepted — only the password is checked. This is independent of `URNETWORK_HUB_TOKEN`, which protects the provider write endpoints (`/api/report`, `/api/heartbeat`, `/api/nodes/remove`) and is unaffected by this setting. Browsers cache the Basic Auth credential per origin after the first prompt, so the dashboard's own JS (`fetch`, `EventSource`) doesn't need any changes to keep working once you've logged in.

## 4. Confirm It's Working

- Dashboard shows a row per node within one report cycle (up to 5 min) or one heartbeat (15s) after `hub set`.
- Green heartbeat dot = reporting normally; red = no report in 5+ minutes; check `URNETWORK_REPORT_URL` / `hub set` on that node if it stays red.
- TLS padlock icon appears next to nodes reporting over HTTPS — if some nodes show it and others don't, those nodes still need `hub link`.

## Running the Hub in Docker (Windows / Mac / any host)

`urnet-tools hub install` sets the hub up as a **systemd user service**, which only works on Linux. On Windows and macOS — which have no native hub binary at all — and optionally on Linux too, `urnet-tools hub install`/`hub update` run the hub as a **Docker container** instead:

```sh
# Windows / macOS: always Docker (no --docker flag needed)
urnet-tools hub install

# Linux: opt-in, native systemd is still the default
urnet-tools hub install --docker
```

This pulls the prebuilt multi-arch image (`ghcr.io/full-bars/urnetwork-3.23-fix-hub`), runs it as a container named `urnetwork-hub` with a `-p 8080:8080` port mapping and a named `urnetwork-hubdata` volume for `/data`, and writes the chosen tag/port/token to `~/.urnetwork/hub-docker.conf` so `hub update [--docker]` can recreate it without re-specifying flags:

```sh
urnet-tools hub install [--tag <tag>] [--port <port>] [--token <token>]
urnet-tools hub update  [--tag <tag>] [-f|--force]
```

- `--token` sets `URNETWORK_HUB_TOKEN`, the shared secret required on `/api/report` and `/api/nodes/remove`. Without it the hub starts but logs a warning and accepts unauthenticated reports — set this for anything beyond local testing.
- `hub update` pulls the target image and, unless it's already what's running, stops + removes the old container and recreates it — the named volume (and everything in it) survives.
- The CA cert and password are generated into the volume on first boot; get the fingerprint via `/api/cert` or mint an onboard token with `docker exec urnetwork-hub /hub -mint-onboard-token -data /data`.
- This flow doesn't wire up the TLS listener (`URNETWORK_HUB_TLS_ADDR`) or a `docker build` from local source — for either of those, or to build from `hub/Dockerfile` yourself, use the manual commands below.

### Manual Docker commands

If you'd rather run Docker by hand — for TLS, a custom build, or just to see exactly what `urnet-tools hub install --docker` does under the hood:

```sh
# From the repo root, to build from source instead of pulling
docker build -f hub/Dockerfile -t urnetwork-hub .

docker run -d \
  --name urnetwork-hub \
  -p 8080:8080 \
  -v hubdata:/data \
  -e URNETWORK_HUB_TOKEN=YOUR_SHARED_SECRET \
  urnetwork-hub
```

- `-v hubdata:/data` — named volume holding `hub.db` (and `tls.crt`/`tls.key` if TLS is enabled). Persists across container recreation/updates; back it up with `docker volume` or `docker cp`.
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

Then `urnet-tools hub link https://HUB_IP:8443` on each provider as usual.

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

To use encrypted, CA-verified reporting (Option A above) without `urnet-tools hub link`, fetch the CA cert by hand and point the provider at `https://` instead:

```sh
# One-shot join token, minted on the hub:
#   ./hub -mint-onboard-token -data /var/hub-data
mkdir -p ~/.urnetwork
curl -fsSk "https://HUB_IP:8443/api/ca-cert?token=YOUR_TOKEN" \
  | sed -n 's/.*"ca_pem" *: *"\([^"]*\)".*/\1/p' \
  | sed 's/\\n/\n/g' > ~/.urnetwork/hub_ca.pem

URNETWORK_REPORT_URL=https://HUB_IP:8443 ./ur-provider
```

The provider reads the CA cert from the fixed path `~/.urnetwork/hub_ca.pem` — no env var to point it elsewhere. If the file isn't there, it falls back to the legacy `~/.urnetwork/hub.pin` fingerprint (if present) and otherwise fails closed rather than connecting unverified.

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
| `hub link` fails with "fingerprint mismatch" | Hub is running an old version without CA support — fallback to legacy pinning failed. Update the hub binary |
| `hub install` doesn't pick up new tag | Re-run with `URNETWORK_HUB_TAG=vX.Y.Z`, then `systemctl --user restart urnetwork-hub.service` |

For dashboard usage, report format, and the JSON/SSE API, see [Hub-Dashboard.md](Hub-Dashboard.md).
