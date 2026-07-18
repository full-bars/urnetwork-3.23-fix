# 🚀 Advanced — Production Fleet

This guide covers multi-server fleet management, performance tuning, the hub dashboard, hot-reload, memory management, and troubleshooting production issues. It assumes you already have providers running and want to optimize, monitor, and scale.

---

## 📋 Contents

- [Performance Profiles](#-performance-profiles)
- [Fleet Management](#-fleet-management)
- [Hub Dashboard](#-hub-dashboard)
- [Hot-Reload & Proxy Management](#-hot-reload--proxy-management)
- [Memory & GC Tuning](#-memory--gc-tuning)
- [Logging & Forensics](#-logging--forensics)
- [Troubleshooting](#-troubleshooting)

---

## 🎛️ Performance Profiles

Set via `URNETWORK_PROFILE` or `urnet-tools turbo <v4|v8|off>`.

### Profile reference

| Profile | RAM Recommended | GOGC | GOMEMLIMIT | Best for |
|---------|----------------|------|------------|----------|
| auto | Any | varies (50-200) | varies (tiered) | Recommended default, adapts to RAM |
| turbo-v8 | 16 GiB+ | 200 | 80% RAM | Dedicated servers, maximum throughput |
| turbo-v4 | 4-16 GiB | 200 | 80% RAM | Well-provisioned VPS |
| eco | 1-2 GiB | 50 (dynamic) | 75% RAM | RAM-constrained boxes |
| lowmem | < 1 GiB | 50 | 85% RAM | Minimum footprint, RAM logs on |

### Turbo mode details

Turbo raises per-connection throughput limits by scaling buffers:

| Parameter | Default | Turbo V4 | Turbo V8 |
|-----------|---------|----------|----------|
| TCP MaxWindowSize | 4 MiB | 4 MiB | 8 MiB |
| ResendQueueMaxByteCount | 2 MiB | 8 MiB | 16 MiB |
| WebRTC ReceiveBufferSize | 4 MiB | 8 MiB | 16 MiB |
| GOGC | 100 | 200 | 200 |
| GOMEMLIMIT | unset | 80% RAM | 80% RAM |

> **Choosing V4 vs V8:** V4 is a good starting point for 4-16 GiB boxes. V8 is for 16 GiB+ servers where the extra 4 MiB per-connection window is affordable. Check RSS under real load before rolling V8 fleet-wide.

---

## 🏢 Fleet Management

### Multi-node setup

Each server runs its own provider instance. Standard deployment:

```sh
# Per server — same steps:
curl -fSsL https://dl.fullbars.xyz/install.sh | sh
urnetwork auth <auth-code>
```

### Hot-reload across the fleet

Proxy changes propagate without restart:

```sh
urnet-tools proxy add ~/proxies.txt   # or: proxy remove --match=<pattern>
urnet-tools proxy refresh             # triggers reload on the current node
```

`refresh` writes to the `~/.urnetwork/proxy.reload` trigger file, which the running provider watches and uses to apply add/remove diffs against the current proxy set. Active connections are not interrupted.

### Automated proxy sources

Instead of a static file, point the provider at a live URL:

```sh
export PROXY_URL=https://example.com/proxies.txt
export PROXY_URL_REFRESH=30m    # refresh every 30 minutes (Go duration format)
export PROXY_URL_MAX=500        # max proxies to keep
```

The provider fetches, caches, and cleans up dead proxies automatically.

### Self-healing pool management (opt-in)

```sh
urnet-tools self-heal on
```

When enabled, the provider monitors PSI pressure, memory availability, load average, and goroutine counts. Under sustained pressure it:
- Stretches proxy fetch/probe intervals
- Shrinks cleanup cadence
- Sheds dead and degraded proxies first, then healthy ones by lowest traffic

The emergency goroutine pin at >= 25000 goroutines provides an extra safety net.

---

## 📊 Hub Dashboard

Set up a hub server for fleet-wide visibility:

```sh
urnet-tools hub install
urnet-tools hub init
urnet-tools hub onboard-cmd    # mints a one-time onboard token
```

Then on each provider node:

```sh
urnet-tools hub link <https://hub-host:port> --token <onboard-token>
```

The hub dashboard (port 8080) shows:
- Live Mbps throughput per node
- Billable traffic (hourly/daily/monthly)
- Contract win rates
- Per-proxy drilldown by address
- Active client sessions

> 💡 Dashboard is accessible at `http://<hub-ip>:8080/`

---

## 🔄 Hot-Reload & Proxy Management

### Proxy file management

File format: one proxy per line
```
ip:port:user:pass
ip:port          # no-auth proxy
```

### Proxy URL sources (live feed)

```sh
export PROXY_URL=https://example.com/proxies.txt
urnet-tools proxy refresh   # force immediate fetch
```

### Adding/removing proxies

```sh
urnet-tools proxy add ~/proxies.txt
urnet-tools proxy remove --match=<pattern>
urnet-tools proxy refresh
```

The provider diffs against the current running set and applies only the changes — no restart, no connection drops.

---

## 🧠 Memory & GC Tuning

### Profiles and memory limits

Since v3.23.0-fix.26.4, all profiles have a GOMEMLIMIT safety ceiling except the default (unset) profile. Turbo profiles (v4/v8) set 80% RAM. Eco sets 75% RAM. Lowmem sets 85% RAM.

This prevents the unbounded heap growth that could occur during sustained outages with thousands of degraded proxies.

### Manual memory limit

Override the profile's memory limit with:

```sh
urnetwork provide --max-memory 2GiB
```

Or in Docker, set the standard Go `GOMEMLIMIT` env var directly:

```yaml
# docker-compose.yml
environment:
  - GOMEMLIMIT=2GiB
```

### Eco memory monitor

When `URNETWORK_PROFILE=eco` is set, the provider runs a dynamic GC pressure monitor that adjusts GOGC based on available memory:

| Available RAM | GOGC |
|---------------|------|
| > 450 MiB | 50 (normal) |
| 150-300 MiB | 25 (pressure) |
| < 150 MiB | 10 (critical) |

Recovers with hysteresis when pressure clears.

### Message pool sizing

The message pool free-list is auto-sized to RAM/32 at startup, capped at 256 MiB. This prevents nearly every packet from falling through to a GC allocation when managing thousands of proxies.

---

## 📝 Logging & Forensics

### Log locations

| Source | Location | Persists restarts? |
|--------|----------|-------------------|
| Health logs | `/dev/shm/urnetwork.log` | Yes (O_APPEND mode) |
| Important events | `/dev/shm/urnetwork-important.log` | Yes |
| Critical events | `~/.urnetwork/events.log` | Yes (1MB capped, auto-rotated) |
| System journal | `journalctl -u urnetwork` | Yes |

### Log levels

- `[health][proxies]` — per-cycle proxy counts (up, down, degraded, recovered)
- `[profit]` — earnings heartbeat (reason, clients, rate)
- `[traffic]` — bandwidth summary (rx, tx, active proxies)
- `[c]ping` — contract pings (suppressed when healthy)
- `[t]auth` — transport auth events
- `[net][s]select` — control-plane dial results

### Watch logs live

```sh
tail -f /dev/shm/urnetwork.log
```

---

## 🔧 Troubleshooting

### Proxy problems

| Symptom | Likely cause | Check |
|---------|-------------|-------|
| `up=0` for all proxies | API/auth unreachable | `curl https://api.bringyour.com/hello` |
| Proxies stuck "degraded" | Transport connections failing | `[t]auth` log entries, network/firewall |
| Some proxies showing "auth still failing" | Those proxy IPs can't reach the API | Test from the proxy's network |
| High error count in `[net][s]select` | Proxy endpoint unreachable or slow | Probe the proxy directly: `curl -x socks5://ip:port https://example.com` |

### Memory / OOM

| Symptom | Likely cause | Fix |
|---------|-------------|-----|
| Heap growing during outage | All proxies hold buffers while retrying | Fixed in 26.4: GOMEMLIMIT + reaper |
| Process using 100% swap | Heap exceeds physical RAM | Lower profile (`eco`), or set `--max-memory` |
| High goroutine count | Many proxies each with ~14 goroutines | The reaper (26.4) kills worst-performing degraded proxies |

### Network issues

| Symptom | Check |
|---------|-------|
| Provider won't start | `journalctl -u urnetwork -n 50` or `docker logs urnetwork` |
| Auth fails | `cat ~/.urnetwork/jwt` — is it present and non-empty? |
| No traffic flowing | `urnet-tools proxy traffic` — are any proxies active? |
| Proxies cycling up/down | Look for `[t]auth error` in the RAM log |

### Fleet-wide checks

```sh
# Check the hub dashboard
curl http://<hub-ip>:8080/

# Check a specific node
ssh user@<node-ip> "urnet-tools proxy summary"
```

---

## 🔁 Release upgrade notes

| Version | Impact |
|---------|--------|
| 26.4 | GOMEMLIMIT added for turbo profiles. Degraded-proxy reaper runs automatically. No config change needed. |
| 26.3 | Hub PAKE auth, choose_network, auto Tier 4 for 8 GiB+ RAM. |
| 26.2 | Hot-reload for hub commands, self-heal pool management (opt-in). |
| 26.1 | Docker in-place updates, Go 1.26 toolchain, dl.fullbars.xyz URLs. |
