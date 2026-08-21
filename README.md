# ⛓ UrNetwork v3.23 Fix

[![CodeRabbit Pull Request Reviews](https://img.shields.io/badge/CodeRabbit_Reviews-AI_PRs-FF570A?labelColor=171717&link=https%3A%2F%2Fcoderabbit.ai)](https://coderabbit.ai)
[![CI](https://github.com/full-bars/urnetwork-3.23-fix/actions/workflows/build.yml/badge.svg)](https://github.com/full-bars/urnetwork-3.23-fix/actions)
![Go Version](https://img.shields.io/github/go-mod/go-version/full-bars/urnetwork-3.23-fix?labelColor=171717&color=FF570A)
![Release](https://img.shields.io/github/v/release/full-bars/urnetwork-3.23-fix?labelColor=171717&color=FF570A)
![Language](https://img.shields.io/github/languages/top/full-bars/urnetwork-3.23-fix?labelColor=171717&color=FF570A)
![Activity](https://img.shields.io/github/commit-activity/m/full-bars/urnetwork-3.23-fix?labelColor=171717&color=FF570A)

A high-performance, high-visibility fork of the **UrNetwork Connect** provider, based on the stable **v3.23** engine. Tuned for professional providers managing large proxy lists, high throughput, and production-grade operations.

## 🆚 What this fork changes vs upstream

| | Upstream | This fork |
| :--- | :--- | :--- |
| Control-plane dial visibility | Debug level 2 (silent) | INFO — one line per successful backend dial (`[net][s]select`, control-plane not relay traffic) |
| Initial contract size | 16 KiB | Min 256 KiB (lowmem), 2 MiB (performance), tunable per profile |
| Proxy startup | All at once | Jittered stagger with live `[pace]` warmup, plus a shared adaptive rate limiter that bounds aggregate auth load on the API |
| Proxy changes | Restart required | Hot-reload via trigger file, zero downtime, with full added-proxy listing |
| Proxy source | Static file only | File and/or live URL feed, with scoped auto-cleanup |
| Error noise | Auth/contract errors spam logs | Rate-limited with suppressed counts |
| Fleet visibility | None | Hub dashboard — live Mbps, billable traffic, per-proxy drilldown |
| Performance profiles | None | Auto / Turbo V4 / Turbo V8 / Eco / Lowmem |
| Crash diagnostics | Journal-only, logs lost on restart | Disk-based critical event log + preserved RAM logs, panic hooks |
| Custom API/connect backend | One-off `--api_url`/`--connect_url` flags only, re-passed on every invocation | `choose_network` persists the URLs to disk; flags still override per-call |

---

## 🗺 Start Here

| If you want to... | Go here |
| :--- | :--- |
| **Start here — pick your skill level** | [🐣 Beginner](docs/guides/beginner.md) · [🧭 Intermediate](docs/guides/intermediate.md) · [🚀 Advanced](docs/guides/advanced.md) |
| Install on a Linux host as a user-level service | [Installation Guide](docs/Installation.md) |
| Run one Docker container | [Docker Deployment](docs/Docker-Deployment.md) |
| Run multiple containers on one host | [Multi-Container Scaling](docs/Multi-Container-Scaling.md) |
| Choose profiles, turbo mode, or host tuning | [Performance Tuning](docs/High-Volume-Performance-Tuning.md) |
| Understand environment variables | [Configuration Reference](docs/Configuration.md) |
| Interpret provider logs | [Log Message Reference](LOG_REFERENCE.md) |
| Monitor your fleet with the bandwidth hub dashboard | [Hub Dashboard](docs/Hub-Dashboard.md) |
| Feed the provider a live proxy list URL | [Proxy URL Sources](docs/Proxy-URL-Sources.md) |

---

## ⚡ Quick Start

Choose your platform:

| Platform | Install | Uninstall |
|----------|---------|-----------|
| 🐧 Linux (systemd) | [`curl -fSsL https://dl.fullbars.xyz/install.sh \| sh`](https://dl.fullbars.xyz/install.sh) | [`curl -fSsL https://dl.fullbars.xyz/uninstall.sh \| sh`](https://dl.fullbars.xyz/uninstall.sh) |
| 🍎 macOS (launchd) | [`curl -fSsL https://dl.fullbars.xyz/install-mac.sh \| sh`](https://dl.fullbars.xyz/install-mac.sh) | manual — see [docs/Installation.md](docs/Installation.md) |
| 🪟 Windows (PowerShell) | [`irm https://dl.fullbars.xyz/install-win.ps1 \| iex`](https://dl.fullbars.xyz/install-win.ps1) | [`irm https://dl.fullbars.xyz/uninstall-win.ps1 \| iex`](https://dl.fullbars.xyz/uninstall-win.ps1) |
| 🐋 Docker | `docker pull ghcr.io/full-bars/urnetwork-3.23-fix:latest` | `docker rm -f <container> && docker rmi ghcr.io/full-bars/urnetwork-3.23-fix:latest` |
| 🐋 Docker (manage) | [`curl -fSsL https://dl.fullbars.xyz/urnet-docker.sh \| sh`](https://dl.fullbars.xyz/urnet-docker.sh) | `rm /usr/local/bin/urnet-docker` (root) or `rm ~/.local/bin/urnet-docker` (non-root) |

After installation, authenticate and start providing:

```bash
# Linux / macOS / Windows (PowerShell) — one Go binary on every platform
urnetwork auth
urnet-tools proxy add ~/proxies.txt
urnet-tools proxy refresh
urnet-tools auto on
```

> [!NOTE]
> Since v3.23.0-fix.27.0, `urnet-tools` is a provider-aware Go binary (the legacy POSIX shell + PowerShell variants are retired). It discovers every provider on the box and **refuses to act on an ambiguous target** — on multi-provider machines, pass `--unit` / `--user` / `--network` / `--network-id` / `--state-dir`. See [docs/urnet-tools-go.md](docs/urnet-tools-go.md).
>
> Docker-only deployments: the provider runs in a container, but the management tool (`urnet-docker`) runs **on the docker host, outside the container**. Install it with the one-liner above (use `curl -fSsL https://dl.fullbars.xyz/urnet-docker.sh | sh -s -- urnet-tools` for the systemd variant; GitHub fallback: `curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/install-urnet-docker.sh | sh` (use `| sh -s -- urnet-tools` for the systemd variant)). The tool self-updates afterward (`urnet-docker update`).

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
  -e PROXY_URL='https://example.com/your-proxy-list.txt' \
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
- `PROXY_URL` — Optional live proxy list URL (comma-separated for multiple), additive with the mounted `proxy.txt`. See [Proxy URL Sources](docs/Proxy-URL-Sources.md).
- `UR_API_URL` / `UR_CONNECT_URL` — Point at a custom API + connect backend instead of `bringyour.com`. Must be set together; saved to the `~/.urnetwork` volume so it survives restarts. See [Configuration Reference](docs/Configuration.md).

See [Docker Deployment](docs/Docker-Deployment.md) for Docker Compose, email/password auth, Watchtower, multi-container, and advanced options.

> [!NOTE]
> **Docker shortcuts** — `urnet-tools` commands work via `docker exec` (same as bare-metal):
> - `docker exec -it urfix urnet-tools proxy health`
> - `docker exec -it urfix urnet-tools logs`
> - `docker exec -it urfix urnet-tools status`
> - `docker exec -it urfix urnet-tools session save /root/.urnetwork/backup.urnsession`

---

## 🔧 Common Operations

| Command | Use this when... |
| :--- | :--- |
| `urnetwork auth` | You need to log in or refresh your identity manually |
| `urnet-tools proxy traffic` | You want to see active clients, bandwidth, and **Max Age** per proxy |
| `urnet-tools proxy health` | You need to see which proxies are `DEAD` vs `DEGRADED` vs `UP` |
| `urnet-tools logs` | You want to stream the current RAMLOGS buffer |
| `urnet-tools optimize` | You just added many proxies and need to tune kernel `ulimits` |
| `urnet-tools proxy summary` | You want a single-pane fleet overview -- sources, health, URL cache status |
| `urnet-tools proxy refresh` | You updated your proxy list and want the node to reload live |
| `urnet-tools hot-restart on/off` | Toggle client JWT reuse across restarts (on by default; `off` sets `URNETWORK_HOT_RESTART=0`) |
| `urnet-tools session save <file>` | Export identity+proxy state as encrypted bundle (cross-machine transfer) |
| `urnet-tools session load <file>` | Import identity+proxy state, then restart |
| `urnet-tools report <url>` | You want to set or change the hub report URL without restarting |
| `urnet-tools report` | You want to check which URL the provider is currently reporting to |
| `urnetwork choose_network <api_url> <connect_url>` | You run your own API/connect backend and want the provider to default to it |
| `urnetwork choose_network --reset` | You want to clear a saved custom network and revert to the main network |

> [!TIP]
> `~/proxies.txt` and `/home/user/proxies.txt` are both valid path formats.

---

## 📡 Fleet Dashboard

Monitor your entire fleet in real time. The hub aggregates bandwidth reports from all nodes and renders a live HTML dashboard with traffic rates, billable accounting, per-proxy drilldown, and auto-refresh.

![Hub Dashboard Preview](docs/hub-dashboard-preview.png)

```bash
# Run the hub (Linux, via urnet-tools — recommended)
urnet-tools hub install

# Or run it in Docker (Windows / Mac / any host)
docker build -f hub/Dockerfile -t urnetwork-hub .
docker run -d --name urnetwork-hub -p 8080:8080 -v hubdata:/data urnetwork-hub

# Point each provider at it
URNETWORK_REPORT_URL=http://HUB_IP:8080
```

See [Hub Setup](docs/Hub-Setup.md) for installation and [Hub Dashboard](docs/Hub-Dashboard.md) for full feature details.

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
- [Hub Setup](docs/Hub-Setup.md)
- [Hub Dashboard](docs/Hub-Dashboard.md)
- [Project Structure](docs/Project-Structure.md)
- [Log Message Reference](LOG_REFERENCE.md)
- [Go urnet-tools Reference](docs/urnet-tools-go.md)
- [Changelog](CHANGELOG.md)

**Wiki:**

- [Online GitHub Wiki](https://github.com/full-bars/urnetwork-3.23-fix/wiki)
- [CI and Release Process](https://github.com/full-bars/urnetwork-3.23-fix/wiki/CI-and-Release-Process)

---

## 🏗 Build Info

- **Base engine:** UrNetwork v3.23
- **Language:** Go 1.25, compiled on Alpine
- **Images:** Multi-arch `linux/amd64` + `linux/arm64`, `darwin/amd64` + `darwin/arm64` via GitHub Actions → GHCR
- **Bridge-friendly:** runs on standard Docker bridge networks, no `--network host` required

---

> [!WARNING]
> This is a private, custom modification for professional provider use. Not affiliated with the official UrNetwork project.
