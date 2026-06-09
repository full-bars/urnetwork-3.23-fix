# 🚀 UrNetwork v3.23 Fix

A high-performance, high-visibility fork of the **UrNetwork Connect** provider, based on the stable **v3.23** engine. This build is tuned for professional providers managing larger proxy lists, higher throughput, and more visible operations.

## 🏁 Start Here

| If you want to... | Go here |
| :--- | :--- |
| Install on a Linux host as a user-level service | [Installation Guide](docs/Installation.md) |
| Run one Docker container | [Docker Deployment](docs/Docker-Deployment.md) |
| Run multiple containers on one host | [Multi-Container Scaling](docs/Multi-Container-Scaling.md) |
| Choose profiles, turbo mode, or host tuning | [Performance Tuning](docs/High-Volume-Performance-Tuning.md) |
| Understand environment variables | [Configuration Reference](docs/Configuration.md) |
| Interpret provider logs | [Log Message Reference](LOG_REFERENCE.md) |
| Monitor your fleet with the bandwidth hub dashboard | [Hub Dashboard](docs/Hub-Dashboard.md) |

## ⚡ Quick Start

### 🐧 Linux Service

Run this as your normal non-root user:

```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/main/scripts/Provider_Install_Linux.sh | sh
```

Uninstall:

```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/main/scripts/Provider_Uninstall_Linux.sh | sh
```

After installation, source your terminal profile so the new commands are available, and authenticate the provider:

```bash
source ~/.bashrc
urnetwork auth
```

Then you can add your proxy list and monitor the node:

```bash
urnet-tools proxy add ~/proxies.txt
urnet-tools proxy refresh
urnet-tools auto on
urnet-tools proxy health
urnet-tools logs
```

## 🛠️ Common Operations

| Command | Use this when... |
| :--- | :--- |
| `urnetwork auth` | You need to log in or refresh your identity manually (Native). |
| `urnet-tools proxy traffic` | You want to see active clients, bandwidth, and **Max Age** per proxy. |
| `urnet-tools proxy health` | You need to see which proxies are `DEAD` vs `DEGRADED` vs `UP`. |
| `urnet-tools logs` | You want to stream the current RAMLOGS buffer (Native/Docker-Alias). |
| `urnet-tools optimize` | You just added many proxies and need to tune kernel `ulimits` for them. |
| `urnet-tools proxy refresh` | You updated your proxy list and want the node to load changes live. |

> [!TIP]
> **Path Formatting**  
> You can use either `~/proxies.txt` or `/home/user/proxies.txt` — both syntaxes work.

### 📊 Bandwidth Hub Dashboard

Monitor your entire fleet in real time. The hub aggregates bandwidth reports from all provider nodes and renders an HTML dashboard with traffic rates, billable accounting, per-proxy drilldown, and auto-refresh.

![Hub Dashboard Preview](docs/hub-dashboard-preview.png)

```sh
# Run the hub (standalone binary, built from hub/main.go)
./hub -addr :8080 -data /var/hub-data

# Each provider reports to the hub via env
URNETWORK_REPORT_URL=http://HUB_IP:8080
```

See [Hub Dashboard](docs/Hub-Dashboard.md) for deployment and configuration details.

### 🐳 Docker

See [Docker Deployment](docs/Docker-Deployment.md) for full copy-paste examples covering `docker run`, Docker Compose, JWT auth, email/password auth, GHCR, Docker Hub, RAM logs, outage alerts, and Watchtower updates.

> [!NOTE]
> **Docker "Power User" Shortcuts**
> Most `urnet-tools` commands have direct Docker aliases. Use `docker exec -it <container_name> <cmd>`:
> *   **Health**: `docker exec -it urfix proxy-health`
> *   **Traffic**: `docker exec -it urfix proxy-traffic`
> *   **Live Logs**: `docker exec -it urfix logs`

## ✨ Key Improvements

- **Deep proxy telemetry:** Real-time NAT session multiplexing tracks exactly how many concurrent clients and bandwidth each individual proxy handles, exported live to `proxy_traffic.state`.
- **High-signal monitoring:** successful serial connection selection logs are promoted to normal INFO output, while noisy parallel-selection logs stay quiet.
- **Outage noise reduction:** repeated backend auth and contract errors are rate-limited and summarized with suppressed counts.
- **Higher throughput ceiling:** larger contract ramp-up, longer contract timeout, dynamic TCP accordion windows, deeper packet buffers, and expanded message pools.
- **Performance profiles:** Auto, Turbo V4, Turbo V8, Eco, and Lowmem profiles cover hosts from small VPS instances to high-RAM dedicated servers.
- **Lean native binary & extensive container support:** Built primarily for maximum efficiency as a native binary app, alongside comprehensive support for Docker deployments (**JWT Smart Refresh**, Watchtower compatibility, multi-container patterns).
- **Self-healing auth:** Automatic detection and recovery from expired or revoked JWT tokens without manual intervention.

## 💡 Recommended Defaults

For most providers:

- Use the Linux installer if you want a host-managed service.
- Use either `docker run` or Docker Compose if you prefer containers; both are fully documented.
- Enable `auto` profile unless you already know you need a specific profile.
- Keep a persistent config volume mounted at `/root/.urnetwork` for Docker deployments.
- Run `urnet-tools optimize` when deploying many proxies, or whenever the System Auditor reports suboptimal kernel limits.

## 📚 Documentation

- [Installation Guide](docs/Installation.md)
- [Docker Deployment](docs/Docker-Deployment.md)
- [Multi-Container Scaling](docs/Multi-Container-Scaling.md)
- [Configuration Reference](docs/Configuration.md)
- [Proxy Management & Hot-Reloading](docs/Proxy-Management.md)
- [High-Volume Performance Tuning](docs/High-Volume-Performance-Tuning.md)
- [Log Message Reference](LOG_REFERENCE.md)
- [Changelog](CHANGELOG.md)

## 🏗️ Architecture & Build

This repository is designed to be **standalone**.

- **Base Engine:** UrNetwork v3.23
- **Builder:** Go 1.25 on Alpine
- **CI/CD:** GitHub Actions builds and pushes multi-arch images to GHCR
- **Bridge-Friendly:** optimized for standard Docker bridge networks without requiring `--network host`

## ⚠️ Disclaimer

> [!WARNING]
> This is a private, custom modification intended for testing and professional provider use. It is not affiliated with the official UrNetwork project.
