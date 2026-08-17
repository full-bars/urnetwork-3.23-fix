# 🧭 Intermediate: Custom Setup & Proxy Management

> **Navigation:** [Guides Index](README.md) · [🐣 Beginner](beginner.md) · **🧭 Intermediate** · [🚀 Advanced](advanced.md)

This guide walks you through a complete provider setup with explanations at each step. You will choose an install method for your OS (systemd on Linux, launchd on macOS, a native Windows service, or Docker), configure your own proxy lists, and learn the daily commands to monitor your node.

---

## 📋 Before you start

You need:
- A Linux, macOS, or Windows machine with a **wired ethernet connection** — the provider relays traffic in real time, so connection quality matters more than raw CPU/RAM. Avoid wireless backhaul, VPNs on the provider machine, carrier-grade NAT, or anything that adds latency or jitter between the provider and the peers it serves.
- An **auth code** (generate one from the [web dashboard](https://app.ur.network), the [ur.io site](https://ur.io/), or the URnetwork mobile app)
- Optional: a list of SOCKS5 proxies you want the provider to manage. Resource needs scale with proxy count — for very large proxy pools, allocate more RAM accordingly.

> [!TIP]
> **Common latency sources to eliminate:** Home routers doing double-NAT, Wi-Fi extenders, ISP-level CGNAT, oversubscribed shared bandwidth. If you can plug directly into a publicly-routed port on a decent ISP, that's ideal.

### Which install method should you choose?

| Method | Best for | Restart behavior |
|--------|----------|-----------------|
| **Systemd** (Linux native) | Dedicated servers, maximum performance | Automatic on crash, manual for config changes |
| **launchd** (macOS native) | Mac desktops/servers | Automatic on crash via `KeepAlive`; no auto-update yet |
| **Native Windows service** | Windows desktops/servers | Starts at login (Startup entry); auto-update on by default |
| **Docker** | Containers, easy migration, isolated environment, any OS with Docker | Automatic with `--restart unless-stopped` |

---

## 🐧 Option A: Systemd Install

### 1. Install

```sh
curl -fSsL https://dl.fullbars.xyz/install.sh | sh
```

This creates:
- The provider binary at `~/.local/share/urnetwork-provider/bin/urnetwork`
- A systemd service called `urnetwork.service`
- Configuration directory at `~/.urnetwork/`

### 2. Authenticate

```sh
urnetwork auth
```

You can also pass the auth code directly:

```sh
urnetwork auth <your-auth-code>
```

The auth code is a one-time token. Your provider JWT is saved to `~/.urnetwork/jwt` and is valid for ~30 days. Hot-restart is enabled by default, so JWT reuse across restarts happens automatically.

### 3. Add proxies

The provider needs proxies to manage. Create a text file with one proxy per line:

```
192.0.2.45:1081:proxyuser:proxypass
198.51.100.78:1081:anotheruser:anotherpass
```

> **File format:** `address:port:username:password` or just `address:port` for no-auth proxies.

Then load it:

```sh
urnet-tools proxy add ~/proxies.txt
```

### 4. Start and verify

```sh
urnet-tools start
urnet-tools proxy summary
```

The summary displays proxy source breakdown (file/URL/internal), health state counts (Up/Connecting/Degraded/Dead), and URL feed cache status.

---

## 🍎 Option B: macOS Native Install

### 1. Install

```sh
curl -fSsL https://dl.fullbars.xyz/install-mac.sh | sh
```

This is the same installer as Linux but uses `launchd` instead of `systemd`. It creates:
- The provider binary at `~/.local/share/urnetwork-provider/bin/urnetwork`
- A launchd agent at `~/Library/LaunchAgents/com.urnetwork.provider.plist` (starts on login, restarts on crash)
- Configuration directory at `~/.urnetwork/` (same as Linux)

### 2. Authenticate

```sh
urnetwork auth
```

You can also pass the auth code directly:

```sh
urnetwork auth <your-auth-code>
```

Same behavior as Linux — JWT saved to `~/.urnetwork/jwt`, hot-restart on by default.

### 3. Add proxies

Same file format as the systemd method above. Since v3.23.0-fix.27.0, `urnet-tools` is the provider-aware Go binary on every platform — macOS, Linux, and Windows share one codebase with the full command surface (no more platform-specific wrapper limitations):

```sh
urnet-tools proxy add ~/proxies.txt
```

### 4. Start and verify

```sh
urnet-tools start
urnet-tools proxy summary
```

Logs live at `~/Library/Logs/com.urnetwork.provider/stdout.log` and `stderr.log` instead of `journalctl`.

---

## 🪟 Option C: Windows Native Install

### 1. Install

```powershell
powershell -c "irm https://dl.fullbars.xyz/install-win.ps1 | iex"
```

No admin rights required. This installs:
- The provider binary at `%LOCALAPPDATA%\urnetwork\provider\urnetwork.exe`
- The `urnet-tools` Go management binary (v3.23.0-fix.27.0+; the legacy `urnet-tools.ps1` wrapper is deprecated) and `urnetwork-updater.ps1` alongside it
- A Startup shortcut so the provider launches on login
- Configuration directory at `%USERPROFILE%\.urnetwork\`

### 2. Authenticate

```powershell
urnetwork auth
```

You can also pass the auth code directly:

```powershell
urnetwork auth <your-auth-code>
```

### 3. Add proxies

Same file format as the systemd method above, using a Windows path:

```powershell
urnet-tools proxy add C:\Users\You\proxies.txt
```

### 4. Start and verify

```powershell
urnet-tools start
urnet-tools proxy summary
```

Auto-update is enabled by default on install (`urnet-tools auto-update-enable` / `auto-update-disable` to control it). Stream logs with `urnet-tools logs`.

---

## 🐋 Option D: Docker Install

Works the same way on Linux, macOS, and Windows — anywhere Docker runs.

### 1. Run the container

```sh
docker pull ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

**Linux/macOS:**

Create a directory for your config:

```sh
mkdir -p ~/.urnetwork
```

Create `~/proxies.txt` with your proxy list (same format as the systemd method above), then run the container with `BUILD=jwt` and your auth code:

```sh
docker run -d \
  --name urnetwork \
  --restart unless-stopped \
  -v ~/.urnetwork:/root/.urnetwork \
  -v ~/proxies.txt:/app/proxies.txt \
  -e BUILD=jwt \
  -e URNETWORK_AUTH_CODE="<your-auth-code>" \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

**Windows (PowerShell):**

Create a directory for your config:

```powershell
New-Item -ItemType Directory -Force -Path "$env:USERPROFILE\.urnetwork"
```

Create `$env:USERPROFILE\proxies.txt` with your proxy list (same format as the systemd method above), then run the container with `BUILD=jwt` and your auth code — PowerShell needs backtick line continuations instead of `\`, and Windows-style volume paths:

```powershell
docker run -d `
  --name urnetwork `
  --restart unless-stopped `
  -v "$env:USERPROFILE\.urnetwork:/root/.urnetwork" `
  -v "$env:USERPROFILE\proxies.txt:/app/proxies.txt" `
  -e BUILD=jwt `
  -e URNETWORK_AUTH_CODE="<your-auth-code>" `
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

The provider will find and load the proxy file automatically on startup. If you need to add proxies after the container is already running, use:

```sh
docker exec urnetwork urnet-tools proxy add /app/proxies.txt
```

### 2. Check status

```sh
docker logs -f urnetwork
```

### 3. Open a shell in the container

```sh
docker exec -it urnetwork /bin/sh
```

From inside the container, `urnet-tools` is on `PATH` (symlinked to `/usr/local/bin/urnet-tools`).

---

## 📊 Daily commands

These work across all platforms via the Go `urnet-tools` binary (or on Docker via `urnet-docker` or `docker exec <container> urnet-tools`).

| Command | What it does |
|---------|-------------|
| `urnet-tools proxy summary` | Fleet summary: source breakdown (file/URL/internal), health state, URL feed cache |
| `urnet-tools proxy traffic` | Live proxy traffic snapshot and max age |
| `urnet-tools status` | Provider process status and uptime |
| `urnet-tools proxy health` | Per-proxy up/degraded/dead status |

---

## 🔄 Hot-reload (changing proxies without restart)

Instead of restarting the provider to apply proxy changes, add or remove proxies and then refresh (on macOS, use the direct binary invocation from step 3 for `add`):

```sh
urnet-tools proxy add ~/proxies.txt
urnet-tools proxy refresh
```

`refresh` writes the reload trigger; the running provider is the one that diffs against the current proxy set, applies the add/remove changes, and revalidates health — without interrupting active connections.

---

## ⚙️ Key environment variables

Set these before starting the provider — `export VAR=value` on Linux/macOS, `$env:VAR = "value"` in PowerShell, or `-e VAR=value` on `docker run`. On Linux, `urnet-tools` also has toggle commands for some of these (`turbo`, `eco`, `self-heal`) that write the value to a systemd override so it survives restarts without re-exporting.

| Variable | Purpose | Example |
|----------|---------|---------|
| `URNETWORK_PROFILE` | Performance profile | `auto`, `turbo-v4`, `turbo-v8`, `eco` |
| `URNETWORK_HOT_RESTART` | JWT reuse on restart | `1` (default), `0` to disable |
| `PROXY_URL` | Auto-fetch proxies from a URL | `https://example.com/proxies.txt` |
| `URNETWORK_SELF_HEAL` | Enable pressure-based pool management | `1` to enable (default off) |
| `URNETWORK_SKIP_AUDIT` | Skip startup system audit (disk speed, ulimit, conntrack) — useful in Docker | `1` to skip (default off) |

---

## 🔍 Checking proxy health

```sh
urnet-tools proxy health         # Linux/Docker
urnet-tools proxy health     # all platforms
```

Shows how many proxies are up, degraded, or dead, with lifetime recovery/loss counts. Not available through the macOS wrapper — see the note in step 3 of Option B.

---

## ❓ Common questions

**How do I update?**
```sh
urnet-tools update
```

**How do I stop the provider?**
```sh
urnet-tools stop
```

**Where are the logs?**
- Systemd (Linux): `journalctl -u urnetwork -n 100 -f`
- launchd (macOS): `~/Library/Logs/com.urnetwork.provider/stdout.log` + `stderr.log`
- Windows: `urnet-tools logs`
- Docker: `docker logs -f urnetwork`
- RAM logs, Linux/Docker only (survive process restarts, not host reboots: `/dev/shm` is tmpfs): `/dev/shm/urnetwork.log`
- Events (persist across restarts): `~/.urnetwork/events.log`

---

> Navigation: [← 🐣 Beginner](beginner.md) | [🚀 Advanced Guide: Fleet & Performance →](advanced.md)
