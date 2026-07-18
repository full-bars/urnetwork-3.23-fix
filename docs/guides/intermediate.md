# 🧭 Intermediate — Custom Setup

This guide walks you through a complete provider setup with explanations at each step. You'll choose between a systemd service (bare-metal) or Docker, configure your own proxy lists, and learn the daily commands to monitor your node.

---

## 📋 Before you start

You need:
- A Linux server with `sudo` access (2 GiB RAM minimum, 4+ GiB recommended)
- An **auth code** from the URnetwork team
- Optional: a list of SOCKS5 proxies you want the provider to manage

### Which install method should you choose?

| Method | Best for | Restart behavior |
|--------|----------|-----------------|
| **Systemd** (Linux native) | Dedicated servers, maximum performance | Automatic on crash, manual for config changes |
| **Docker** | Containers, easy migration, isolated environment | Automatic with `--restart unless-stopped` |

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
urnetwork auth <your-auth-code>
```

The auth code is a one-time token. Your provider JWT is saved to `~/.urnetwork/jwt` and is valid for ~30 days. Hot-restart is enabled by default, so JWT reuse across restarts happens automatically.

### 3. Add proxies

The provider needs proxies to manage. Create a text file with one proxy per line:

```
104.207.36.213:1081:user1:pass1
216.26.233.32:1081:user2:pass2
```

> **File format:** `address:port:username:password` or just `address:port` for no-auth proxies.

Then load it:

```sh
urnet-tools proxy add ~/proxies.txt
```

### 4. Start and verify

```sh
urnet-tools restart
urnet-tools proxy summary
```

The summary shows proxy health, clients, and earnings.

---

## 🐋 Option B: Docker Install

### 1. Run the container

```sh
docker pull ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

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

Then load the proxy list:

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

These work for both systemd and Docker (prepend `docker exec urnetwork` for Docker).

| Command | What it does |
|---------|-------------|
| `urnet-tools proxy summary` | Health overview — proxy counts, traffic, earnings |
| `urnet-tools proxy traffic` | Live proxy traffic snapshot |
| `urnet-tools status` | Provider process status and uptime |
| `urnet-tools proxy health` | Per-proxy up/degraded/dead status |

---

## 🔄 Hot-reload (changing proxies without restart)

Instead of restarting the provider to apply proxy changes, add or remove proxies and then refresh:

```sh
urnet-tools proxy add ~/proxies.txt
urnet-tools proxy refresh
```

This diffs against the currently running proxy set and applies add/remove changes without interrupting active connections.

---

## ⚙️ Key environment variables

| Variable | Purpose | Example |
|----------|---------|---------|
| `URNETWORK_PROFILE` | Performance profile | `auto`, `turbo-v4`, `turbo-v8`, `eco` |
| `URNETWORK_HOT_RESTART` | JWT reuse on restart | `1` (default), `0` to disable |
| `PROXY_URL` | Auto-fetch proxies from a URL | `https://example.com/proxies.txt` |
| `URNETWORK_SELF_HEAL` | Enable pressure-based pool management | `1` to enable (default off) |

---

## 🔍 Checking proxy health

```sh
urnet-tools proxy health
```

Shows how many proxies are up, degraded, or dead, with lifetime recovery/loss counts.

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
- Systemd: `journalctl -u urnetwork -n 100 -f`
- Docker: `docker logs -f urnetwork`
- RAM logs (survive restarts): `/dev/shm/urnetwork.log`
- Events (persist across restarts): `~/.urnetwork/events.log`
