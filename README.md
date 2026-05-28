# UrNetwork v3.23 Fix (Custom Build)

This is a high-performance, high-visibility fork of the **UrNetwork Connect** provider, based on the stable **v3.23** engine. It combines the latest protocol optimizations with surgical improvements for professional providers managing large proxy lists.

---

## Table of Contents

- [Key Improvements](#-key-improvements)
- [Quick Start (Linux)](#-quick-start-linux)
- [Usage](#-usage)
  - [Environment Variables](#environment-variables)
  - [Docker Run — GHCR](#docker-run--github-registry-ghcr)
  - [Docker Run — Docker Hub](#docker-run--docker-hub-alternative)
  - [Docker Compose](#docker-compose)
  - [Persistent JWT](#persistent-jwt)
  - [Outage Alerting](#outage-alerting-optional)
  - [RAM Logging](#ram-logging-optional)
  - [Automatic Updates (Watchtower)](#automatic-updates-watchtower)
- [Architecture & Build](#-architecture--build)
- [Disclaimer](#%EF%B8%8F-disclaimer)

---

## 🚀 Key Improvements

### 1. High-Signal Monitoring (Promoted Logs)
In standard builds, connection handshake logs are hidden behind debug flags, leading to "silent" nodes. In this version:
*   **[net][s]select (Serial Select)**: Promoted from Debug Level 2 to **Standard INFO level**. You will see exactly one clean line every time a proxy connection is successfully established.
*   **Noise Reduction**: Parallel selection logs ([net][p]) remain silenced, ensuring that even with high-scale proxy lists, your logs stay readable and useful.
*   **Log Spam Reduction**: During backend outages, `[t]auth error` and `[contract]oob err` are rate-limited to one line per minute globally across all proxy instances. A suppressed-count suffix (e.g. `(3,952 suppressed)`) is appended when the outage clears so no errors are silently dropped.

### 2. Throughput & Scalability (Unlocked Engine)
The default UrNetwork engine is often bottlenecked for high-bandwidth providers, leading to capacity caps and micro-stutters.
*   **Contract Cap**: Boosted `InitialContractTransferByteCount` from 16 KiB to **2 MiB** for faster connection ramp-up.
*   **High-Scale Stability**: Increased `CreateContractTimeout` to **60s** and tuned `ContractFillFraction` to **0.7** to prevent connection drops during massive signaling spikes.
*   **Accordion Scaling**: Implemented dynamic TCP window scaling. Windows start small (**4KB**) to save RAM on idle connections and grow on demand (up to **1MB**) for active throughput. Windows automatically shrink back to 4KB after 30s of inactivity.
*   **Zero-Allocation Path**: Expanded internal Message Pools (16KB, 32KB, 64KB) to eliminate Garbage Collector CPU spikes during high-throughput transfers. Pool capacity now auto-scales to RAM/32 at startup (floor 8 MiB, cap 256 MiB) so the pool isn't exhausted on large proxy list deployments.
*   **Burst Protection**: Quadrupled IP Buffer Depth to **256** to absorb network volatility without dropping packets.

### 3. Professional Docker Integration
This image integrates the excellent wrapper scripts from the community-maintained `techroy23/Docker-UrNetwork` project.
*   **JWT & User/Pass Support**: Full support for `BUILD=jwt` or standard email/password authentication.
*   **vnStat Integration**: Real-time traffic monitoring built-in (accessible via port 8080).
*   **Multi-Arch**: Native builds for both **AMD64** (Intel/AMD) and **ARM64** (Oracle Cloud, Raspberry Pi, Graviton).

### 4. Performance Profiles

Choose the profile that matches your server's available RAM:

| Profile | Best For | RAM |
| :--- | :--- | :--- |
| **Auto** | **Recommended (Zero-Config)** | Any |
| **Turbo V8** | Maximum throughput, dedicated servers | 16 GiB+ |
| **Turbo V4** | High throughput, well-provisioned VPS | 4–16 GiB |
| *(default)* | General use | 2–4 GiB |
| **Eco** | RAM-constrained, full throughput | 1–2 GiB |
| **Lowmode** | Minimum RAM, reduced throughput | < 1 GiB |

*   **Auto**: Intelligent hardware detection. Automatically selects the best Tier for contracts/buffers, enables Eco mode on low-RAM boxes, and automatically moves logs to RAM (`/dev/shm`) if your disk is too slow to keep up with high volume.
*   **Turbo V4 / V8**: Raises the TCP Accordion window from 1 MiB to 4 or 8 MiB, removing the ~100–150 Mbps per-connection ceiling. 
*   **Eco**: GC-tuned for RAM-constrained systems. 
*   **Lowmode**: Reduces buffer sizes for < 1GB environments.
*   **RAM Logging**: Redirects logs to `/dev/shm` (Linux RAM disk). Eliminates disk I/O bottlenecks.

---

## ⚡ Quick Start (Linux)

Install the optimized provider directly as a background service:

**Install:**
```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/main/scripts/Provider_Install_Linux.sh | sh
```

**Uninstall:**
```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/main/scripts/Provider_Uninstall_Linux.sh | sh
```

### 🛠 Post-Install Commands
The installation includes the `urnet-tools` suite for easy management:

| Command | Description |
| :--- | :--- |
| `urnet-tools status` | Check service health and uptime. |
| `urnet-tools logs` | Stream logs (automatically detects RAM vs Disk). |
| `urnet-tools auto on` | **Enable Smart Auto (Recommended).** |
| `urnet-tools optimize` | **Supercharge host kernel limits (ulimit, conntrack).** |
| `urnet-tools turbo v4` | Enable Turbo V4 mode (~400 Mbps ceiling). |
| `urnet-tools turbo v8` | Enable Turbo V8 mode (~800 Mbps ceiling). |
| `urnet-tools eco on/off` | Toggle Eco mode (GC-tuned). |
| `urnet-tools ramlogs on/off` | Toggle RAM-disk logging independently. |
| `urnet-tools update` | Upgrade to the latest version. |

---

## 🛠 Usage

### Environment Variables
| Variable | Default | Description |
| :--- | :--- | :--- |
| `BUILD` | `stable` | Set to `jwt` for auth code login, or `stable` for email/pass. |
| `USER_AUTH` | - | Your email (required if `BUILD=stable`). |
| `PASSWORD` | - | Your password (required if `BUILD=stable`). |
| `ENABLE_VNSTAT` | `true` | Enables the traffic monitor on port 8080. |
| `ENABLE_IP_CHECKER` | `false` | Prints your public IP to the logs on startup. |
| `TURBO` | - | Set to `v4` or `v8` to enable turbo mode. Raises the TCP window ceiling from 1 MiB to 4 or 8 MiB, removing the ~100–150 Mbps per-connection limit. Use `v4` on 4–16 GiB boxes, `v8` on 16 GiB+. |
| `URNETWORK_RAMLOGS` | `0` | Set to `1` to redirect provider logs to RAM instead of stdout. Cannot be used with `--log-opt`. See [RAM Logging](#ram-logging-optional). |
| `URNETWORK_PROFILE` | - | Advanced: directly sets the provider profile (`lowmem`, `eco`, `turbo-v4`, `turbo-v8`). For turbo, prefer the `TURBO` variable above. `lowmem` reduces buffer sizes and sets GOMEMLIMIT=85% RAM. Cannot be combined with `--log-opt`. |
| `URNETWORK_ALERT_WEBHOOK` | - | HTTP POST endpoint for outage alerts. Fires a JSON payload when the backend becomes unreachable and again when it recovers. See [Outage Alerting](#outage-alerting-optional). |
| `URNETWORK_NODE_NAME` | hostname (docker/binary) | Label included in webhook payloads and log lines to identify which server the alert came from. Defaults to the system hostname suffixed with `(docker)` or `(binary)`. |
| `URNETWORK_HEALTH_INTERVAL` | `5m` | How often to emit a `[health]` heartbeat log line. Accepts Go duration strings (`10m`, `1h`). Minimum `1m`. |

---

### Docker Run — GitHub Registry (GHCR)

The primary image is hosted on the GitHub Container Registry.

Set `NAME` once before the command. The container name, JWT storage volume, and vnStat volume are all derived from it — so running the same command twice with different `NAME` values produces two fully isolated containers with no manual volume renaming. Docker also enforces that no two containers share the same name, so conflicts are caught immediately.

**JWT auth (auth code login):**

```bash
NAME=urfix   # change this per container — volumes are named from it

docker run -d \
  --name=$NAME \
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
  -v ${NAME}_config:/root/.urnetwork \
  -v ${NAME}_vnstat:/var/lib/vnstat \
  -v /path/to/your/proxy.txt:/app/proxy.txt \
  -p 9001:8080 \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest AUTH_CODE_HERE
```

> Replace `AUTH_CODE_HERE` with your token from [ur.io](https://ur.io). Auth codes are single-use — the token is saved to the `${NAME}_config` volume on first run and reused on all subsequent starts.
>
> To label this server in webhook alerts and health logs, add `-e URNETWORK_NODE_NAME=your-server-name`. If omitted, the provider auto-generates a name from the hostname (e.g. `vps-123 (docker)`).

**Email/password auth:**

```bash
NAME=urfix   # change this per container

docker run -d \
  --name=$NAME \
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
  -v ${NAME}_config:/root/.urnetwork \
  -v ${NAME}_vnstat:/var/lib/vnstat \
  -v /path/to/your/proxy.txt:/app/proxy.txt \
  -p 9001:8080 \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

---

### Docker Run — Docker Hub (Alternative)

If you experience `denied` errors or rate-limiting on GHCR, use the Docker Hub mirror. The commands are identical — only the image name changes.

**JWT auth:**

```bash
NAME=urfix

docker run -d \
  --name=$NAME \
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
  -v ${NAME}_config:/root/.urnetwork \
  -v ${NAME}_vnstat:/var/lib/vnstat \
  -v /path/to/your/proxy.txt:/app/proxy.txt \
  -p 9001:8080 \
  3cape/urnetwork-3.23-fix:latest AUTH_CODE_HERE
```

**Email/password auth:**

```bash
NAME=urfix

docker run -d \
  --name=$NAME \
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
  -v ${NAME}_config:/root/.urnetwork \
  -v ${NAME}_vnstat:/var/lib/vnstat \
  -v /path/to/your/proxy.txt:/app/proxy.txt \
  -p 9001:8080 \
  3cape/urnetwork-3.23-fix:latest
```

---

### Docker Compose

Volume names are derived from a single `NAME` variable so containers on the same host can never accidentally share storage. Create a `.env` file in the same folder as your `docker-compose.yml`:

```bash
# .env
NAME=urfix
```

Docker Compose reads `.env` automatically and substitutes `${NAME:-urfix}` everywhere — into the container name, the JWT volume, and the vnStat volume. To run a second container on the same host, copy the folder, set a different `NAME` in its `.env`, and run `docker compose up -d` from there. Each instance gets completely isolated storage with no manual renaming required.

**JWT auth:**
```yaml
services:
  urnetwork:
    image: ghcr.io/full-bars/urnetwork-3.23-fix:latest
    container_name: ${NAME:-urfix}
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
      # - URNETWORK_NODE_NAME=my-server-name   # optional: auto-names from hostname if omitted
      # - TURBO=v4                             # optional: v4 (4-16 GiB RAM) or v8 (16 GiB+ RAM)
      # - URNETWORK_ALERT_WEBHOOK=https://your-webhook
    volumes:
      - ${NAME:-urfix}_config:/root/.urnetwork
      - ${NAME:-urfix}_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9001:8080"
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

volumes:
  ${NAME:-urfix}_config:
  ${NAME:-urfix}_vnstat:
```

Pass your auth code on first start: `docker compose run --rm urnetwork AUTH_CODE_HERE`

After the JWT is saved to the volume, subsequent starts need no argument: `docker compose up -d`

**Email/password auth:**
```yaml
services:
  urnetwork:
    image: ghcr.io/full-bars/urnetwork-3.23-fix:latest
    container_name: ${NAME:-urfix}
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
      # - URNETWORK_NODE_NAME=my-server-name   # optional: auto-names from hostname if omitted
      # - TURBO=v4                             # optional: v4 (4-16 GiB RAM) or v8 (16 GiB+ RAM)
      # - URNETWORK_ALERT_WEBHOOK=https://your-webhook
    volumes:
      - ${NAME:-urfix}_config:/root/.urnetwork
      - ${NAME:-urfix}_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9001:8080"
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

volumes:
  ${NAME:-urfix}_config:
  ${NAME:-urfix}_vnstat:
```

---

### Persistent JWT

The JWT is stored inside the container at `/root/.urnetwork/jwt`. Without a persistent volume, every container restart wipes it and forces re-authentication using the original auth code — which is single-use and will fail on the second attempt.

All examples above already include `-v urnetwork_config:/root/.urnetwork`. With this volume in place, the startup script detects the existing JWT and skips authentication entirely on all subsequent starts. Auth codes are only consumed once — on first run or after a manual `docker volume rm urnetwork_config`.

### Outage Alerting (Optional)

Set `URNETWORK_ALERT_WEBHOOK` to receive a push notification when the provider loses contact with the URnetwork backend and when it recovers. The provider posts a JSON payload:

```json
{
  "event": "outage_start",
  "node": "my-server-name (docker)",
  "timestamp": "2026-05-27T23:48:34Z",
  "message": "Backend unreachable — provider holding existing connections but not accepting new ones."
}
```

`event` is either `outage_start` or `outage_clear`. The provider logs `[outage]` state transitions to stdout regardless of whether a webhook URL is set, so they are always visible in `docker logs`.

**Example: Discord**
```
URNETWORK_ALERT_WEBHOOK=https://discord.com/api/webhooks/YOUR_ID/YOUR_TOKEN
```

**Example: Slack**
```
URNETWORK_ALERT_WEBHOOK=https://hooks.slack.com/services/T.../B.../...
```

**Example: ntfy (self-hosted push notifications)**
```
URNETWORK_ALERT_WEBHOOK=https://ntfy.sh/your-topic
```

**Startup log:** On every start the provider logs:
```
[outage] watcher active node=my-server-name (docker) webhook=configured
```
This confirms the node name and webhook status without waiting for an outage to fire.

### RAM Logging (Optional)
Setting `URNETWORK_RAMLOGS=1` redirects provider logs to `/dev/shm/urnetwork.log` inside the container — a RAM-backed filesystem — instead of stdout. This keeps log I/O entirely off disk, which can help on weak cloud instances with slow storage.

> **Note:** `URNETWORK_RAMLOGS=1` and `--log-opt` are mutually exclusive. When RAM logging is active, nothing is written to stdout so Docker's log driver has nothing to capture. Remove the `--log-driver` and `--log-opt` flags if you enable this.
>
> **Note:** `URNETWORK_PROFILE=lowmem` also enables RAM logging unconditionally. If you use lowmem mode, remove `--log-driver` and `--log-opt` from your docker run command and use `docker exec` to view logs as shown below.

To view logs live (replace `urfix` with your container name if different):
```bash
docker exec -it urfix tail -f /dev/shm/urnetwork.log
```

RAM logs are capped at 1MB with automatic rotation and are lost when the container restarts.

---

### Automatic Updates (Watchtower)

[Watchtower](https://containrrr.dev/watchtower/) can automatically pull new image versions and restart your container when an update is published. Add it to your `docker-compose.yml` alongside the `urnetwork` service:

```yaml
  watchtower:
    image: containrrr/watchtower
    container_name: watchtower
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    command: --cleanup --interval 3600 urfix   # check for updates hourly
```

> **Important:** The `${NAME:-urfix}_config` volume is required when using Watchtower. Without it, Watchtower will pull a new image, recreate the container, and the auth code will be consumed again — which fails because auth codes are single-use. With the volume mounted, the existing JWT is reused and the restart is seamless.
>
> **Multiple containers:** Because volumes are named from `NAME` in `.env`, each container folder automatically gets its own isolated volumes — no manual renaming needed. Just give each folder a different `NAME` value.

---

## 📦 Architecture & Build

This repository is designed to be **standalone**.
*   **Base Engine**: UrNetwork v3.23.
*   **Builder**: Go 1.25 (Alpine).
*   **CI/CD**: GitHub Actions automatically builds and pushes multi-arch images to GHCR.
*   **Bridge-Friendly**: Optimized to work within standard Docker bridge networks without requiring `--network host` (though NET_ADMIN capabilities are still recommended).

## ⚠️ Disclaimer
This is a private, custom modification intended for testing and professional provider use. It is not affiliated with the official UrNetwork project.
