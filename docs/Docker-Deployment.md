# 🐳 Docker Deployment

This page keeps the copy-paste Docker examples from the README in one place. Use either `docker run` or Docker Compose, depending on how you prefer to manage containers.

## 🛠️ Managing Containers with `urnet-docker`

> [!IMPORTANT]
> **Host-Side vs. In-Container Tooling:**
> - **`urnet-docker`** runs **on the Docker host** (outside the containers). It discovers provider containers, reads their in-container JWTs, and dispatches management tasks directly.
> - **`urnet-tools`** runs **inside the container** (accessible via `docker exec -it <container> urnet-tools <command>`).

Install `urnet-docker` once on the host (SHA-256 verified against the release API):

```sh
curl -fSsL https://dl.fullbars.xyz/urnet-docker.sh | sh
# installs /usr/local/bin/urnet-docker (or ~/.local/bin when not root)
# GitHub fallback: curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/install-urnet-docker.sh | sh
```

The tool is self-updating afterwards:

```sh
urnet-docker update                  # update the tool binary itself
urnet-docker update --unit urfix     # update a provider container in place (no recreate)
urnet-tools self-update              # same, for the process/systemd tool
```

Common host-side commands:

```sh
urnet-docker providers                          # list provider containers
urnet-docker status --unit urfix                # status of one container
urnet-docker proxy add --unit urfix ~/p.txt     # add proxies from host
urnet-docker proxy trim --unit urfix 500        # hold running proxies at cap (A-F worst first)
urnet-docker proxy refresh --unit urfix         # reload proxies without restart
urnet-docker restart --unit urfix               # restart a container
urnet-docker logs --unit urfix 100              # stream logs (RAMLOGS-aware)
```

> [!NOTE]
> `urnet-docker update` with a target flag (such as `--unit`) updates a provider container in place. The container ID stays the same. Plain `urnet-docker update` with no target updates only the host tool binary. Containers can also be updated by pulling a new image and recreating the container (e.g. via Docker Compose or Watchtower).

## 🗄️ Image Registries

Primary image:

```text
ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

Docker Hub mirror:

```text
3cape/urnetwork-3.23-fix:latest
```

Use the Docker Hub mirror if GHCR returns `denied` errors or rate-limiting.

## 🔑 Persistent JWT

The JWT is stored inside the container at `/root/.urnetwork/jwt`. Without a persistent volume, every container restart wipes it and forces re-authentication using the original auth code, which is single-use and will fail on the second attempt.

All examples below mount a config volume at `/root/.urnetwork`. With this volume in place, the startup script detects the existing JWT and skips authentication on later starts. Auth codes are only consumed once: on first run or after manually removing the config volume.

## 🏃 Docker Run - GHCR

The examples below use `urfix` as the container name.

#### With proxy benchmarking and bandwidth hub:

```bash
docker run -d --name urfix \
  -v ~/.urnetwork:/root/.urnetwork \
  -v /path/to/proxy.txt:/app/proxy.txt \
  -e PROXY_URL='https://example.com/your-proxy-list.txt' \
  -e URNETWORK_PROXY_BENCHMARK=true \
  -e URNETWORK_PROXY_BENCHMARK_ENDPOINT=connect.bringyour.com:443 \
  -e URNETWORK_REPORT_URL=http://hub-server:8080 \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

| Env var | Purpose |
|---|---|
| `PROXY_URL` | Live proxy list URL, fetched and merged on interval (see [Proxy URL Sources](Proxy-URL-Sources.md)) |
| `URNETWORK_PROXY_BENCHMARK=true` | Enables per-proxy latency probes (TCP connect every 5m, SOCKS5 every 15m) |
| `URNETWORK_PROXY_BENCHMARK_ENDPOINT` | Target for SOCKS5 CONNECT probe (default `connect.bringyour.com:443`) |
| `URNETWORK_REPORT_URL` | URL for bandwidth hub reporting. Can be changed at runtime via `urnet-tools report <url>` (writes `~/.urnetwork/report_url` via docker exec). |

> [!TIP]
> `hub-server` above needs to be running the hub itself somewhere. If that host doesn't have systemd (Windows, macOS, or you just prefer containers), the hub can also run in Docker — see [Hub Setup](Hub-Setup.md#running-the-hub-in-docker-windows--mac--any-host).

For additional containers, change the container name and volumes together.

### 🔐 JWT Auth

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  --log-driver=json-file \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  -e BUILD=jwt \
  -e ENABLE_VNSTAT=true \
  -e HOST_HOSTNAME=$(hostname) \
  -v urfix_config:/root/.urnetwork \
  -v urfix_vnstat:/var/lib/vnstat \
  -v /path/to/proxy.txt:/app/proxy.txt \
  -p 9001:8080 \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest AUTH_CODE_HERE
```

Replace `AUTH_CODE_HERE` with your token from [ur.io](https://ur.io). Auth codes are single-use; the token is saved to the `urfix_config` volume on first run and reused on later starts.

Alternative method:

```bash
-e URNETWORK_AUTH_CODE=YOUR_CODE
```

To label this server in webhook alerts and health logs, add:

```bash
-e URNETWORK_NODE_NAME=your-server-name
```

### ✉️ Email/Password Auth

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  --log-driver=json-file \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  -e BUILD=stable \
  -e USER_AUTH=you@example.com \
  -e PASSWORD=yourpassword \
  -e ENABLE_VNSTAT=true \
  -e HOST_HOSTNAME=$(hostname) \
  -v urfix_config:/root/.urnetwork \
  -v urfix_vnstat:/var/lib/vnstat \
  -v /path/to/proxy.txt:/app/proxy.txt \
  -p 9001:8080 \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

## 🏃 Docker Run - Docker Hub

The commands are identical to GHCR except for the image name.

### 🔐 JWT Auth

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  --log-driver=json-file \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  -e BUILD=jwt \
  -e ENABLE_VNSTAT=true \
  -e HOST_HOSTNAME=$(hostname) \
  -v urfix_config:/root/.urnetwork \
  -v urfix_vnstat:/var/lib/vnstat \
  -v /path/to/proxy.txt:/app/proxy.txt \
  -p 9001:8080 \
  3cape/urnetwork-3.23-fix:latest AUTH_CODE_HERE
```

Alternative method:

```bash
-e URNETWORK_AUTH_CODE=YOUR_CODE
```

### ✉️ Email/Password Auth

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  --log-driver=json-file \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  -e BUILD=stable \
  -e USER_AUTH=you@example.com \
  -e PASSWORD=yourpassword \
  -e ENABLE_VNSTAT=true \
  -e HOST_HOSTNAME=$(hostname) \
  -v urfix_config:/root/.urnetwork \
  -v urfix_vnstat:/var/lib/vnstat \
  -v /path/to/proxy.txt:/app/proxy.txt \
  -p 9001:8080 \
  3cape/urnetwork-3.23-fix:latest
```

## 🐙 Docker Compose

For another single-container deployment on the same host, copy your `docker-compose.yml` to a new folder and replace the `urfix` prefix with a unique name in `container_name`, `volumes`, and the top-level `volumes:` section.

For 3, 5, or 10 nodes in one Compose file, use the [Multi-Container Scaling](Multi-Container-Scaling.md) guide.

### 🔐 JWT Auth

```yaml
services:
  urnetwork:
    image: ghcr.io/full-bars/urnetwork-3.23-fix:latest
    container_name: urfix
    restart: unless-stopped
    pull_policy: always
    cap_add:
      - NET_ADMIN
      - NET_RAW
    sysctls:
      - net.ipv4.ip_forward=1
    environment:
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
    volumes:
      - urfix_config:/root/.urnetwork
      - urfix_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9001:8080"
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

volumes:
  urfix_config:
  urfix_vnstat:
```

On first start, provide your auth code using one of these methods:

1. Trailing argument: `docker compose run --rm urnetwork AUTH_CODE_HERE`
2. Environment variable: add `URNETWORK_AUTH_CODE=AUTH_CODE_HERE` to the `environment:` section, then run `docker compose up -d`

After the JWT is saved to the volume, subsequent starts need no auth code:

```bash
docker compose up -d
```

### ✉️ Email/Password Auth

```yaml
services:
  urnetwork:
    image: ghcr.io/full-bars/urnetwork-3.23-fix:latest
    container_name: urfix
    restart: unless-stopped
    pull_policy: always
    cap_add:
      - NET_ADMIN
      - NET_RAW
    sysctls:
      - net.ipv4.ip_forward=1
    environment:
      - BUILD=stable
      - USER_AUTH=you@example.com
      - PASSWORD=yourpassword
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
    volumes:
      - urfix_config:/root/.urnetwork
      - urfix_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9001:8080"
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

volumes:
  urfix_config:
  urfix_vnstat:
```


## 💾 RAM Logging

Setting `URNETWORK_RAMLOGS=1` redirects provider logs to `/dev/shm/urnetwork.log` inside the container, a RAM-backed filesystem, instead of stdout. This keeps log I/O entirely off disk.

> [!TIP]
> **Live Monitoring (RAMLOGS)**
> If you have `URNETWORK_RAMLOGS=1` enabled, your logs are stored in high-speed memory instead of standard output. Use this command to live-tail them:
> ```bash
> docker exec -it <container_name> tail -f /dev/shm/urnetwork.log
> ```

> [!NOTE]
> `URNETWORK_RAMLOGS=1` and Docker `--log-opt` are mutually exclusive. When RAM logging is active, nothing is written to stdout, so Docker's log driver has nothing to capture. Remove `--log-driver` and `--log-opt` if you enable this.

Example Docker Run:

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -e URNETWORK_RAMLOGS=1 \
  -e URNETWORK_NODE_NAME=urfix \
  -e BUILD=jwt \
  -e ENABLE_VNSTAT=true \
  -e HOST_HOSTNAME=$(hostname) \
  -e PROXY_URL='https://example.com/your-proxy-list.txt' \
  -v urfix_config:/root/.urnetwork \
  -v urfix_vnstat:/var/lib/vnstat \
  -v /path/to/proxy.txt:/app/proxy.txt \
  -p 9001:8080 \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest YOUR_AUTH_CODE
```

View logs live:

```bash
docker exec -it urfix tail -f /dev/shm/urnetwork.log
```

RAM logs are capped at 1MB with automatic rotation and are lost when the container restarts.

## 📊 Proxy Summary

View a single-pane fleet overview showing proxy counts by source (file vs URL), health breakdown, and URL cache status:

```sh
docker exec -it <container> provider proxy summary
```

## 📡 Report URL

Set or check the hub report URL at runtime without restarting. Uses `~/.urnetwork/report_url` inside the container:

```sh
# Set report URL
docker exec -it <container> sh -c 'echo "http://HUB_IP:8080" > "$HOME/.urnetwork/report_url"'

# Check current URL
docker exec -it <container> sh -c 'cat "$HOME/.urnetwork/report_url" 2>/dev/null || echo "not set"'

# Disable
docker exec -it <container> sh -c 'rm -f "$HOME/.urnetwork/report_url"'
```

Or use the Go `urnet-docker` binary (v3.23.0-fix.27.0+) which handles `docker exec` transparently — it discovers provider containers and delegates commands into them:
```sh
urnet-docker report http://HUB_IP:8080
urnet-docker report
urnet-docker report off
```
(The legacy PowerShell wrapper `urnet-tools.ps1` is deprecated; the Go binary replaces it on every platform.)

## ♻️ Hot-Restart

Client JWTs are reused across restarts by default (no env var needed since v3.23.0-fix.26). The write path has been un-gated since v25.15, so JWTs were already being saved on every auth cycle — now the read path follows suit.

**Disabling:**
```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -e BUILD=jwt \
  -e URNETWORK_HOT_RESTART=0 \
  -v urfix_config:/root/.urnetwork \
  -v urfix_vnstat:/var/lib/vnstat \
  -v /path/to/proxy.txt:/app/proxy.txt \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest YOUR_AUTH_CODE
```

**Status check:**
```bash
docker exec urfix sh -c 'echo ${URNETWORK_HOT_RESTART:-1}'
```

> [!NOTE]
> To opt out, set `-e URNETWORK_HOT_RESTART=0` at container creation. If you don't set anything, hot-restart is active by default.

## 💾 Session Save/Load

Export a provider's full identity state (client JWTs, account JWT, signing keys, proxy lists) as an encrypted, portable bundle for cross-machine transfer.

**Save** (two steps: save inside container, then copy out):
```bash
docker exec -it urfix urnet-tools session save /root/.urnetwork/nyc.urnsession
docker cp urfix:/root/.urnetwork/nyc.urnsession .
```

**Load** (two steps: copy in, then load inside container):
```bash
docker cp atlanta.urnsession urfix:/root/.urnetwork/
docker exec -it urfix urnet-tools session load /root/.urnetwork/atlanta.urnsession
```

The load will prompt for the passphrase, check the network_id against the current account, backup existing files, stage the new session, and ask "Restart now? (Y/n)". If yes, it kills the provider process — the container's start script crash loop picks it up with the new identity.

> [!IMPORTANT]
> Save and load require an interactive TTY (`docker exec -it`, not just `docker exec`). The script will fail with a clear error if `-it` is omitted.

## 🩺 Viewing Proxy Health

You can view the full list of dead and degraded proxies, as well as a live event log of proxy state transitions. These files persist on the config volume and survive container restarts, even if RAM logging is active.

- Persistent (always): `docker exec -it <container> proxy-health`
- Live-tail RAMLOGS on: `docker exec -it <container> sh -c "tail -f /dev/shm/urnetwork.log | grep -E '\[health\]\[proxies\]|\[pulse\]'"`
- Live-tail RAMLOGS off: `docker logs -f <container> 2>&1 | grep -E '\[health\]\[proxies\]|\[pulse\]'`

## 🏎️ Auto-Tune Performance

The Auto-Tune feature (`URNETWORK_PROFILE=auto`) automatically optimizes the provider based on server hardware.

It detects total system RAM, assigns buffer sizes, enables Eco mode on very low-RAM machines, and benchmarks disk speed on startup. If storage is too slow, it can automatically enable RAM logging to prevent disk I/O from bottlenecking the network stack.

Example Docker Run:

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -e BUILD=jwt \
  -e URNETWORK_PROFILE=auto \
  -e URNETWORK_NODE_NAME=urfix \
  -e ENABLE_VNSTAT=true \
  -e HOST_HOSTNAME=$(hostname) \
  -e PROXY_URL='https://example.com/your-proxy-list.txt' \
  -v urfix_config:/root/.urnetwork \
  -v urfix_vnstat:/var/lib/vnstat \
  -v /path/to/proxy.txt:/app/proxy.txt \
  -p 9001:8080 \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest YOUR_AUTH_CODE
```

> [!NOTE]
> Because Auto-Tune may enable RAM logging if it detects a slow disk, omit `--log-driver` and `--log-opt` from this command to avoid conflicts.

## 🔄 Automatic Updates

[Watchtower](https://containrrr.dev/watchtower/) can automatically pull new image versions and restart your container when an update is published. Add it to your `docker-compose.yml` alongside the `urnetwork` service:

```yaml
  watchtower:
    image: containrrr/watchtower
    container_name: watchtower
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    command: --cleanup --interval 3600 urfix
```

> [!IMPORTANT]
> The `urfix_config` volume is required when using Watchtower. Without it, Watchtower will pull a new image, recreate the container, and the auth code will be consumed again, which fails because auth codes are single-use. With the volume mounted, the existing JWT is reused.
> 
> **JWT Smart Refresh**: As of `v3.23.0-fix.17`, the container includes "smart refresh" logic. If the JWT stored in your volume expires, the provider will automatically detect this, delete the stale file, and attempt to re-authenticate using the `USER_AUTH` and `PASSWORD` environment variables if provided. This ensures your nodes stay online even if a JWT is revoked or corrupted during an update.

> [!NOTE]
> **Pelican Panel deployments**: When the image runs under a [Pelican panel](#-pelican-panel) with `PELICAN=yes`, the self-update scripts are disabled. The panel manages updates by re-pulling the published image. Runtime fetches are blocked to prevent silently replacing the audited fork binary mid-flight.

## ⏳ Idle Update

As of `v3.23.0-fix.26.5`, `urnet-tools idle-update` lets you apply a pending provider update without interrupting active client sessions — it waits for a quiet traffic window before swapping the binary, instead of updating immediately and cutting off whatever's in flight:

```sh
docker exec -it <container> urnet-tools idle-update
```

It polls `billable_rate` every 10s and waits until traffic stays at or below a threshold for a sustained window, then double-checks with 1s polling for 10s before actually applying the update — so a brief lull doesn't trigger an update while traffic is still fluctuating.

| Flag | Default | Meaning |
|---|---|---|
| `--threshold <bytes/sec>` | `5120` (5 KiB/s) | Traffic at or below this is considered "quiet". |
| `--window <seconds>` | `300` (5 min) | How long traffic must stay quiet before updating. |

```sh
# Wait for a 10-minute quiet window under 10 KiB/s
docker exec -it <container> urnet-tools idle-update --window 600 --threshold 10240

# Skip waiting entirely and update immediately
docker exec -it <container> urnet-tools idle-update --window 0
```

> [!NOTE]
> If `billable_rate` isn't available yet (provider predates this feature, or hasn't written its first sample), `idle-update` treats the node as **not idle** rather than assuming it's safe to update — it fails closed.

For multiple containers, give each deployment a different container name, config volume, vnStat volume, and host port.

## 🐦 Pelican Panel

As of `v3.23.0-fix.30.8`, the provider image is importable into the [Pelican game-server panel](https://github.com/pelican-dev/panel) as a one-click egg. The egg ships with the audit-preferred defaults pinned — vnStat off, IP checker off, and runtime self-update disabled.

### Importing the egg

1. Download the egg JSON from the repo: `pelican/egg-urnetwork-323fix.json`
2. In Pelican admin, go to **Nests**, select or create a nest, and use **Import Egg** to upload the JSON.
3. The egg pulls `ghcr.io/full-bars/urnetwork-3.23-fix:latest` (multi-arch amd64/arm64).

### Configuration variables

| Variable | Editable | Description |
|---|---|---|
| `BUILD` | Yes | `stable`, `nightly`, or `jwt` |
| `USER_AUTH` | Yes | Email/phone for password-based auth. Required for `stable`/`nightly`, ignored for `jwt` |
| `PASSWORD` | Admin-only | Password for password-based auth. Required for `stable`/`nightly` |
| `AUTHCODE` | Admin-only | One-time auth code from [ur.io](https://ur.io). Required for `jwt` |
| `PELICAN` | Hidden | Always `yes` — set by the egg, not editable |
| `ENABLE_VNSTAT` | Hidden | Always `false` in the egg |
| `ENABLE_IP_CHECKER` | Hidden | Always `false` in the egg |

### Authentication modes

- **`BUILD=jwt`**: Uses `AUTHCODE` (one-time auth code from [ur.io](https://ur.io)). `pelican_panel.sh` calls `auth-provide "$AUTHCODE" -f` once; it does not retry on failure.
- **`BUILD=stable`** or **`BUILD=nightly`**: Uses `USER_AUTH` + `PASSWORD` for password-based authentication, with retry-on-failure and automatic JWT re-auth after 3 crashes.

> [!NOTE]
> Under Pelican, `BUILD=nightly` currently behaves identically to `BUILD=stable`: the panel always routes through `pelican_panel.sh`, which runs the stable provider binary regardless of `BUILD`. The nightly-vs-stable binary split only applies outside Pelican mode. Don't rely on `BUILD=nightly` to get nightly binary behavior when `PELICAN=yes`.

### Resource requirements

The provider needs raw packet access on the node running the egg:

- Container capability `NET_ADMIN` (and typically `NET_RAW`)
- IP forwarding enabled on the host
- Outbound UDP allowed, for WebRTC P2P transport

No inbound ports need to be forwarded — the provider dials out.

### Update behavior

Under Pelican (`PELICAN=yes`), the self-update checks in `start_nightly.sh` and `urnet-tools update` are **disabled**. The published image is the single source of truth; to update, re-pull the image via the panel (**Settings → Reinstall**) or `docker pull`.

### Testing

The egg ships with automated CI checks (`docker/scripts/test_pelican_gates.sh` for egg JSON structure, variable contracts, and PELICAN-gate behavior; `docker/scripts/test_pelican_smoke.sh` for a full boot smoke test against a fake provider binary). For a real-panel import walkthrough and log output to expect, see `pelican/README.md`.
