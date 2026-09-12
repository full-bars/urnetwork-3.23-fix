# ⛓ UrNetwork v3.23 Fix

[![CodeRabbit Pull Request Reviews](https://img.shields.io/coderabbit/prs/github/full-bars/urnetwork-3.23-fix?utm_source=oss&utm_medium=github&utm_campaign=full-bars%2Furnetwork-3.23-fix&labelColor=171717&color=FF570A&link=https%3A%2F%2Fcoderabbit.ai&label=CodeRabbit+Reviews)](https://coderabbit.ai)
[![CI](https://github.com/full-bars/urnetwork-3.23-fix/actions/workflows/build.yml/badge.svg)](https://github.com/full-bars/urnetwork-3.23-fix/actions)
![Go Version](https://img.shields.io/github/go-mod/go-version/full-bars/urnetwork-3.23-fix?labelColor=171717&color=FF570A)
![Release](https://img.shields.io/github/v/release/full-bars/urnetwork-3.23-fix?labelColor=171717&color=FF570A)
![Language](https://img.shields.io/github/languages/top/full-bars/urnetwork-3.23-fix?labelColor=171717&color=FF570A)
![Activity](https://img.shields.io/github/commit-activity/m/full-bars/urnetwork-3.23-fix?labelColor=171717&color=FF570A)

A high-performance, high-visibility fork of the **UrNetwork Connect** provider, based on the stable **v3.23** engine. Tuned for professional providers managing large proxy lists, high throughput, and production-grade operations.

## 🆚 What this fork changes vs upstream

| | Upstream | This fork |
| :--- | :--- | :--- |
| Control-plane dial visibility | Debug-level glog (silent by default) | INFO — one line per successful backend dial (`[net][s]select`, control-plane not relay traffic) |
| Contract sizing | Fixed 1 MiB initial, 4-contract ramp to 128 MiB standard | Profile-tuned initial size (256 KiB lowmem/balanced, 1 MiB default) with a faster 3-contract ramp |
| Proxy startup | All at once | Jittered stagger with live `[pace]` warmup, plus a shared adaptive rate limiter that bounds aggregate auth load on the API |
| Proxy changes | Restart required | Hot-reload via trigger file, zero downtime, with full added-proxy listing |
| Dead proxy handling | Retry forever (15 min loop, no ceiling) | 24 h daily retry, 14-day drop (file) or 65 min cleanup (URL), persisted state |
| Proxy source | Static file only | File and/or live URL feed, with scoped auto-cleanup |
| Error noise | Log-level throttle (suppresses repeated lines) | Shared auth rate limiter reduces the error source itself — fewer API calls hit the failure path |
| Proxy health grading | None | A–F reachability grade per proxy with continuous re-probing (`proxy health`, `proxy trim`) |
| Fleet visibility & accounting | None | Built-in CLI accounting (`usage`, `proxy traffic`), persistent byte splits, and optional telemetry (`dev/hub`) |
| Performance profiles | None | Auto / Turbo V4 / Turbo V8 / Eco / Lowmem — memory, window, and GC tuned per profile |
| Crash diagnostics | Journal-only, logs lost on restart | Shared-memory RAM logs (`shmlog`) + disk-based critical event log, panic hooks |
| Custom API/connect backend | One-off `--api_url`/`--connect_url` flags only, re-passed on every invocation | `choose_network` persists the URLs to disk; flags still override per-call |
| Runtime settings | Edit systemd drop-ins by hand, then restart | Live control socket (`~/.urnetwork/provider.sock`); changes apply without a restart and are queued in `pending_overrides.json` when the provider is stopped |
| Binary upgrade | Stop, swap, start (20–60 s of downtime) | Zero-downtime HotSwap handoff to a verified candidate, with automatic rollback (requires a `Type=notify` unit, see the release notes) |
| Node identity on the dashboard | Hostname only | `rename` sets the display label and `show-ip` appends the public IP, both without a restart |
| Multi-provider boxes | One provider per host, no targeting | One provider per OS user, with `providers` / `providers --all` inventory and cross-user `sudo` self-elevation |
| Session migration | None | `session save` / `session load` exports identity + proxy state as an encrypted bundle for cross-machine transfer |
| Subnet 25 telemetry | None | `sn-status` command with STSubnet operations guide, wallet registration, and head-fleet tiering docs |

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
| Track traffic usage (billable vs control overhead) | [Docker Deployment](docs/Docker-Deployment.md) · [CLI Reference](docs/urnet-tools-go.md) |
| Load a proxy file into the provider (per-OS) | [Adding Proxies](docs/Adding-Proxies.md) |
| Feed the provider a live proxy list URL | [Proxy URL Sources](docs/Proxy-URL-Sources.md) |
| Import into a Pelican game-server panel | [Pelican Panel](docs/Docker-Deployment.md#-pelican-panel) · [Egg README](pelican/README.md) |

---

## ⚡ Quick Start

### Install

**🐧 Linux (systemd)**

```sh
curl -fSsL https://dl.fullbars.xyz/install.sh | sh
```

**🍎 macOS (launchd)**

```sh
curl -fSsL https://dl.fullbars.xyz/install-mac.sh | sh
```

**🪟 Windows (PowerShell)**

```powershell
irm https://dl.fullbars.xyz/install-win.ps1 | iex
```

**🐋 Docker**

```sh
docker pull ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

**🐋 Docker (management wrapper)**

```sh
curl -fSsL https://dl.fullbars.xyz/urnet-docker.sh | sh
```

### Uninstall

**🐧 Linux**

```sh
curl -fSsL https://dl.fullbars.xyz/uninstall.sh | sh
```

**🍎 macOS**

Manual — see [docs/Installation.md](docs/Installation.md).

**🪟 Windows (PowerShell)**

```powershell
irm https://dl.fullbars.xyz/uninstall-win.ps1 | iex
```

**🐋 Docker**

```sh
docker rm -f <container> && docker rmi ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

**🐋 Docker (management wrapper)**

```sh
rm /usr/local/bin/urnet-docker   # root install
rm ~/.local/bin/urnet-docker     # non-root install
```

After installation, authenticate and start providing:

```bash
# Linux / macOS: one Go binary on every platform
urnetwork auth
urnet-tools proxy add ~/proxies.txt
urnet-tools proxy refresh
urnet-tools auto on
```
On Windows, run the same commands in PowerShell but use a Windows path, e.g.
`urnet-tools proxy add "$env:USERPROFILE\Downloads\proxies.txt"`. Full per-OS
walkthrough, including the `.txt.txt` extension trap: [Adding Proxies](docs/Adding-Proxies.md).

> [!NOTE]
> Since v3.23.0-fix.27.0, `urnet-tools` is a provider-aware Go binary (the legacy POSIX shell + PowerShell variants are retired). It discovers every provider on the box and **refuses to act on an ambiguous target** — on multi-provider machines, pass `--unit` / `--user` / `--network` / `--network-id` / `--state-dir`. See [docs/urnet-tools-go.md](docs/urnet-tools-go.md).
>
> Docker-only deployments: the provider runs in a container, but the management tool (`urnet-docker`) runs **on the docker host, outside the container**. Install it with the one-liner above (use `curl -fSsL https://dl.fullbars.xyz/urnet-docker.sh | sh -s -- urnet-tools` for the systemd variant; GitHub fallback: `curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/install-urnet-docker.sh | sh` (use `| sh -s -- urnet-tools` for the systemd variant)). The tool self-updates afterward (`urnet-docker update`).

### 🐋 Docker (Production-Ready)

Recommended for real deployments — includes auto-tuning, in-memory logs, and persistent config with zero listening ports:

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
  -e HOST_HOSTNAME=$(hostname) \
  -e PROXY_URL='https://example.com/your-proxy-list.txt' \
  -v urnetwork_config:/root/.urnetwork \
  -v /path/to/proxy.txt:/app/proxy.txt \
  -e URNETWORK_AUTH_CODE='YOUR_AUTH_CODE_HERE' \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

**Key env vars:**
- `URNETWORK_PROFILE=auto` — Auto-tunes based on available RAM (balanced, lowmem, etc.)
- `URNETWORK_RAMLOGS=1` — In-memory logging for fast diagnostics (view with `urnet-docker logs`)
- `URNETWORK_AUTH_CODE` — Auth code is exchanged for your JWT token, obtained from https://ur.io (single-use on first run; saved to volume)
- `PROXY_URL` — Optional live proxy list URL (comma-separated for multiple), additive with the mounted `proxy.txt`. See [Proxy URL Sources](docs/Proxy-URL-Sources.md).
- `UR_API_URL` / `UR_CONNECT_URL` — Point at a custom API + connect backend instead of `bringyour.com`. Must be set together; saved to the `~/.urnetwork` volume so it survives restarts. See [Configuration Reference](docs/Configuration.md).

> [!TIP]
> Providers do not require any listening ports. Outbound connections handle all traffic. If you wish to enable the optional vnstat web monitor, see [Running with vnstat](docs/Docker-Deployment.md#-optional-running-with-vnstat-bandwidth-monitor).

See [Docker Deployment](docs/Docker-Deployment.md) for Docker Compose, email/password auth, Watchtower, multi-container, and advanced options.

> [!NOTE]
> **Docker Management** — Use `urnet-docker` directly on the host (no `docker exec` wrapping needed):
> - `urnet-docker status`
> - `urnet-docker direct status` (or `urnet-docker direct off` for proxy-only mode)
> - `urnet-docker usage` (or `urnet-docker usage graphs` for time-series history)
> - `urnet-docker proxy add ~/proxies.txt`
> - `cat proxies.txt | urnet-docker proxy paste`
> - `urnet-docker proxy traffic`

---

## 🔧 Common Operations

| Command | Use this when... |
| :--- | :--- |
| `urnetwork auth` | You need to log in or refresh your identity manually |
| `urnet-tools direct [on\|off\|status]` | You want to check or toggle native direct IP providing (`off` = proxy-only stealth) |
| `urnet-tools usage [graphs]` | You want to view persistent traffic accounting (billable relay vs control plane split) |
| `urnet-tools proxy add <path\|url>` | You want to add proxies from a file (straight path or `--file=`) or URL source |
| `urnet-tools proxy paste < file` | You want to stream raw proxies from stdin or a pipe into the provider |
| `urnet-tools proxy trim <count>` | You want to enforce a hard cap, shedding worst A-F reachability graded proxies first |
| `urnet-tools proxy traffic` | You want to see active clients, bandwidth, and **Max Age** per proxy |
| `urnet-tools proxy health` | You need to see which proxies are `DEAD` vs `DEGRADED` vs `UP` |
| `urnet-tools logs` | You want to stream the current RAMLOGS buffer |
| `urnet-tools optimize` | You just added many proxies and need to tune kernel `ulimits` |
| `urnet-tools summary` | You want a single-pane fleet overview -- sources, health, URL cache status |
| `urnet-tools proxy refresh` | You updated your proxy list and want the node to reload live |
| `urnet-tools hot-restart on/off` | Toggle client JWT reuse across restarts (on by default; `off` sets `URNETWORK_HOT_RESTART=0`) |
| `urnet-tools set [<key> [<value>]]` | Show or change a runtime tuning override live, without editing a drop-in or restarting |
| `urnet-tools hotswap` | Swap to an updated binary with no downtime (needs a `Type=notify` unit; otherwise `update` falls back to a restart) |
| `urnet-tools config [--json]` | Show every provider setting with the source it came from, so you can see which writer won |
| `urnet-tools history [limit]` | Read the provider's command audit trail |
| `urnet-tools dashboard` | Terminal status panel: state, active settings, proxy sources, restart warnings |
| `urnet-tools metrics on/off` | Toggle the Prometheus `/metrics` endpoint live, no restart |
| `urnet-tools profile [name]` | Show or set the memory and GC tuning profile |
| `urnet-tools proxy ids` | Show the `client_id` the platform assigned to each proxy, including `direct` |
| `urnet-tools rename <name>` | Set the dashboard display label without touching the hostname |
| `urnet-tools show-ip [on\|off\|status]` | Control whether the public IP is appended to that dashboard label (was `ip-detect`) |
| `urnet-tools providers [--all]` | List the providers on this box; `--all` (as root) covers every OS user |
| `urnet-tools session save <file>` | Export identity+proxy state as encrypted bundle (cross-machine transfer) |
| `urnet-tools session load <file>` | Import identity+proxy state, then restart |
| `urnetwork choose_network <api_url> <connect_url>` | You run your own API/connect backend and want the provider to default to it |
| `urnetwork choose_network --reset` | You want to clear a saved custom network and revert to the main network |

> [!TIP]
> `~/proxies.txt` and `/home/user/proxies.txt` are both valid straight path formats across all tools.

---

## 📡 Telemetry & Legacy Fleet Dashboard

UrNetwork Connect provides rich standalone metrics directly via `urnet-tools usage` and `urnet-docker usage` (billable vs control plane accounting with hour/day/month historical graphs).

A Prometheus text-format endpoint is also available. `urnet-tools metrics on`
enables it on a running provider without a restart. It exposes eleven `urnet_*`
series covering uptime, active connections, proxy pool size by status,
per-proxy bytes and clients, errors by category, contracts by result, and Go
runtime memory and goroutine counts. It binds to loopback by default;
`URNETWORK_METRICS` binds it on the Tailscale address for remote scraping.

> [!NOTE]
> **Legacy Central Dashboard:** The multi-node aggregation hub dashboard has been transitioned to an optional add-on. Development and maintenance are tracked on the [`dev/hub`](https://github.com/full-bars/urnetwork-3.23-fix/tree/dev/hub) branch of `urnetwork-3.23-fix` and the [`dev/hub`](https://github.com/full-bars/meso-miner/tree/dev/hub) branch of `meso-miner`. For setup details, see [Hub Setup](docs/Hub-Setup.md).

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
- [Node Identity & Dashboard Label](docs/Node-Identity.md)
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
- **Language:** Go 1.27, compiled on Alpine
- **Images:** Multi-arch `linux/amd64` + `linux/arm64`, `darwin/amd64` + `darwin/arm64` via GitHub Actions → GHCR
- **Bridge-friendly:** runs on standard Docker bridge networks, no `--network host` required

---

> [!WARNING]
> This is a private, custom modification for professional provider use. Not affiliated with the official UrNetwork project.
