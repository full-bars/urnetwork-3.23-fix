# ⛓ UrNetwork v3.23 Fix

A high-performance, high-visibility fork of the **UrNetwork Connect** provider, based on the stable **v3.23** engine. Tuned for professional providers managing large proxy lists, high throughput, and production-grade operations.

## 🆚 What this fork changes vs upstream

| | Upstream | This fork |
| :--- | :--- | :--- |
| Control-plane dial visibility | Debug level 2 (silent) | INFO — one line per successful backend dial (`[net][s]select`, control-plane not relay traffic) |
| Initial contract size | 16 KiB | Min 256 KiB (lowmem), 2 MiB (performance), tunable per profile |
| Proxy startup | All at once | Jittered stagger with live `[pace]` warmup |
| Proxy changes | Restart required | Hot-reload via trigger file, zero downtime |
| Error noise | Auth/contract errors spam logs | Rate-limited with suppressed counts |
| Fleet visibility | None | Hub dashboard — live Mbps, billable traffic, per-proxy drilldown |
| Performance profiles | None | Auto / Turbo V4 / Turbo V8 / Eco / Lowmem |

---

## 🗺 Start Here

| If you want to... | Go here |
| :--- | :--- |
| Install on a Linux host as a user-level service | [Installation Guide](docs/Installation.md) |
| Run one Docker container | [Docker Deployment](docs/Docker-Deployment.md) |
| Run multiple containers on one host | [Multi-Container Scaling](docs/Multi-Container-Scaling.md) |
| Choose profiles, turbo mode, or host tuning | [Performance Tuning](docs/High-Volume-Performance-Tuning.md) |
| Understand environment variables | [Configuration Reference](docs/Configuration.md) |
| Interpret provider logs | [Log Message Reference](../../wiki/Log-Message-Reference) |
| Monitor your fleet with the bandwidth hub dashboard | [Hub Dashboard](docs/Hub-Dashboard.md) |

---

## ⚡ Quick Start

### 🐧 Linux Service

Run as your normal non-root user:

```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/main/scripts/Provider_Install_Linux.sh | sh
```

Uninstall:

```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/main/scripts/Provider_Uninstall_Linux.sh | sh
```

After installation, source your shell profile and authenticate:

```bash
source ~/.bashrc
urnetwork auth
```

Then load your proxy list and start providing:

```bash
urnet-tools proxy add ~/proxies.txt
urnet-tools proxy refresh
urnet-tools auto on
urnet-tools proxy health
urnet-tools logs
```

### 🐋 Docker (Quick Start)

```bash
docker run -d \
  --name=urnetwork \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -e BUILD='jwt' \
  -v /path/to/your/proxy.txt:/app/proxy.txt \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest YOUR_AUTH_CODE_HERE
```

### 🐋 Docker (Production-Ready)

Recommended for real deployments — includes auto-tuning, in-memory logs, persistent config, and bandwidth monitoring:

```bash
docker run -d \
  --name=urnetwork-provider \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -e BUILD=jwt \
  -e URNETWORK_PROFILE=auto \
  -e URNETWORK_RAMLOGS=1 \
  -e ENABLE_VNSTAT=true \
  -e HOST_HOSTNAME=$(hostname) \
  -v urnetwork_config:/root/.urnetwork \
  -v urnetwork_vnstat:/var/lib/vnstat \
  -v /path/to/proxy.txt:/app/proxy.txt \
  -p 8080:8080 \
  -e URNETWORK_AUTH_CODE='YOUR_AUTH_CODE_HERE' \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

**Key env vars:**
- `URNETWORK_PROFILE=auto` — Auto-tunes based on available RAM (balanced, lowmem, etc.)
- `URNETWORK_RAMLOGS=1` — In-memory logging for fast diagnostics (view with `docker exec urnetwork-provider logs`)
- `URNETWORK_AUTH_CODE` — Your JWT token (single-use on first run; saved to volume)

See [Docker Deployment](docs/Docker-Deployment.md) for Docker Compose, email/password auth, Watchtower, multi-container, and advanced options.

> [!NOTE]
> **Docker shortcuts** — `urnet-tools` commands work via `docker exec` (same as bare-metal):
> - `docker exec -it urfix urnet-tools proxy health`
> - `docker exec -it urfix urnet-tools logs`
> - `docker exec -it urfix urnet-tools status`

---

## 🔧 Common Operations

| Command | Use this when... |
| :--- | :--- |
| `urnetwork auth` | You need to log in or refresh your identity manually |
| `urnet-tools proxy traffic` | You want to see active clients, bandwidth, and **Max Age** per proxy |
| `urnet-tools proxy health` | You need to see which proxies are `DEAD` vs `DEGRADED` vs `UP` |
| `urnet-tools logs` | You want to stream the current RAMLOGS buffer |
| `urnet-tools optimize` | You just added many proxies and need to tune kernel `ulimits` |
| `urnet-tools proxy refresh` | You updated your proxy list and want the node to reload live |

> [!TIP]
> `~/proxies.txt` and `/home/user/proxies.txt` are both valid path formats.

---

## 📡 Fleet Dashboard

Monitor your entire fleet in real time. The hub aggregates bandwidth reports from all nodes and renders a live HTML dashboard with traffic rates, billable accounting, per-proxy drilldown, and auto-refresh.

![Hub Dashboard Preview](docs/hub-dashboard-preview.png)

```bash
# Run the hub
./hub -addr :8080 -data /var/hub-data

# Point each provider at it
URNETWORK_REPORT_URL=http://HUB_IP:8080
```

See [Hub Dashboard](docs/Hub-Dashboard.md) for full deployment details.

---

## 💡 Recommended Defaults

- Use the **Linux installer** for a host-managed systemd service
- Use **Docker** if you prefer containers — both are fully documented
- Leave profile on **`auto`** unless you have a specific reason to override
- Mount `/root/.urnetwork` as a persistent volume in Docker deployments
- Run `urnet-tools optimize` after adding a large proxy list, or when the System Auditor flags kernel limits

---

## 📚 Documentation

**In-repo:**

- [Installation](docs/Installation.md)
- [Docker Deployment](docs/Docker-Deployment.md)
- [Multi-Container Scaling](docs/Multi-Container-Scaling.md)
- [Configuration Reference](docs/Configuration.md)
- [Proxy Management & Hot-Reload](docs/Proxy-Management.md)
- [High-Volume Performance Tuning](docs/High-Volume-Performance-Tuning.md)
- [Hub Dashboard](docs/Hub-Dashboard.md)
- [Changelog](CHANGELOG.md)

**Wiki:**

- [Log Message Reference](../../wiki/Log-Message-Reference)
- [Proxy Hot-Reload internals](../../wiki/Proxy-Hot-Reload)
- [CI and Release Process](../../wiki/CI-and-Release-Process)
- [Project Structure](../../wiki/Project-Structure)

---

## 🏗 Build Info

- **Base engine:** UrNetwork v3.23
- **Language:** Go 1.25, compiled on Alpine
- **Images:** Multi-arch `linux/amd64` + `linux/arm64` via GitHub Actions → GHCR
- **Bridge-friendly:** runs on standard Docker bridge networks, no `--network host` required

---

> [!WARNING]
> This is a private, custom modification for professional provider use. Not affiliated with the official UrNetwork project.
