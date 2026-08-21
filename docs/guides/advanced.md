# 🚀 Advanced: Production Fleet & Optimization

> **Navigation:** [Guides Index](README.md) · [🐣 Beginner](beginner.md) · [🧭 Intermediate](intermediate.md) · **🚀 Advanced**

This guide covers multi-server fleet management, performance tuning, the hub dashboard, hot-reload, memory management, and troubleshooting production issues. It assumes you already have providers running and want to optimize, monitor, and scale.

> [!NOTE]
> This guide is Linux/Docker-focused because some commands are Linux-specific for hardware reasons:
> - **`optimize`** is Linux-kernel-specific (sysctl tuning, conntrack kernel module, ZRAM, distro package installs) and has no realistic macOS/Windows equivalent.
> - **`ramlogs`** depends on `/dev/shm` (Linux's tmpfs convention). No built-in equivalent on macOS or Windows today.
> - **`turbo`/`eco`** are *not* kernel-dependent. The Go `urnet-tools` binary supports `turbo` and `eco` on all three platforms as of v3.23.0-fix.27.0. On Linux/macOS they write a persistent environment override; on Windows they set the equivalent registry/env value. You can also set `URNETWORK_PROFILE` directly in any environment.
>
> `self-heal`, `proxy *`, `status`, `logs`, `summary`, and `report` work identically on all three platforms.

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
| eco | 1-2 GiB | 50 (static) | 75% RAM | RAM-constrained boxes |
| lowmem | < 1 GiB | 50 | 85% RAM | Minimum footprint, RAM logs on |

### Turbo mode details

Turbo raises per-connection throughput limits by scaling buffers:

| Parameter | Default | Turbo V4 | Turbo V8 |
|-----------|---------|----------|----------|
| TCP MaxWindowSize | 4 MiB | 4 MiB | 8 MiB |
| ResendQueueMaxByteCount | 4 MiB | 8 MiB | 16 MiB |
| WebRTC ReceiveBufferSize | 4 MiB | 8 MiB | 16 MiB |
| GOGC | 100 | 200 | 200 |
| GOMEMLIMIT | unset | 80% RAM | 80% RAM |

> **Choosing V4 vs V8:** V4 is a good starting point for 4-16 GiB boxes. V8 is for 16 GiB+ servers where the extra 4 MiB per-connection window is affordable. Check RSS under real load before rolling V8 fleet-wide.

---

## 🏢 Fleet Management

### Multi-node setup

Each server runs its own provider instance. Standard deployment (Linux shown; see [intermediate.md](intermediate.md) for the macOS/Windows/Docker installers):

```sh
# Per server — same steps:
curl -fSsL https://dl.fullbars.xyz/install.sh | sh
urnetwork auth                       # interactive, prompts for code
# or: urnetwork auth <your-auth-code>
```

> [!TIP]
> **In Docker** the host's sysctls aren't visible inside the container, so the startup disk benchmark and conntrack checks may produce warnings. Set `URNETWORK_SKIP_AUDIT=1` to skip them.

### Hot-reload across the fleet

Proxy changes propagate without restart:

```sh
urnet-tools proxy add ~/proxies.txt   # or: proxy clear to remove all
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

> [!WARNING]
> **Running the hub on the same host as a provider?** The hub defaults to port `:8080`, and a provider's `ENABLE_VNSTAT=true` stats page (on by default) also binds container port `8080`. Colocated on the same machine, whichever starts second fails to bind. Remap one of them — Docker hub: `hub install --docker --port 8081`; Docker provider: publish vnstat on a different host port or set `ENABLE_VNSTAT=false`; bare-metal hub: use a `systemctl --user edit urnetwork-hub.service` drop-in to override `-addr :8080` rather than editing the generated unit directly (a future `hub install` will regenerate it). See [Hub-Dashboard.md](../Hub-Dashboard.md) for the exact drop-in. Separate hosts are unaffected.

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
urnet-tools proxy clear               # removes all proxies
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

### Adaptive GC governor

Memory pressure is handled by a single consolidated adaptive GC governor in the pressure monitor. It replaces the separate eco-memory-monitor runtime loop. The former host available RAM signal is folded into this one controller, along with the process heap signal, and the tighter of the two wins.

The governor applies to all profiles (baseline no-profile, auto Tier 1-4, turbo, and eco). There is exactly one writer to the Go GC percentage knob, which removes the old two-writers hazard. It only ever lowers GOGC below the profile baseline; it never raises it.

The governor acts on the process live heap fraction (with a fast 10s subtick via the `/gc/heap/live:bytes` metric) and on host available RAM (merged on the 30s sweep):

| Signal | Level | Effect |
|--------|-------|--------|
| Heap >= 0.70 | Tighten | GOGC to `min(baseline, 50)` |
| Heap >= 0.80 | Hard | GOGC to `min(baseline, 25)` |
| Heap >= 0.92 | Critical | GOGC to `min(baseline, 10)` plus `FreeOSMemory` |
| Host RAM <= 300 MiB | Pressure | Tightens GOGC |
| Host RAM <= 150 MiB | Critical | Hard-tightens GOGC and frees memory |

The 10s heap subtick reacts to heap spikes faster than the 30s sweep. Release back toward baseline happens one level at a time after several consecutive calm samples, so the governor does not oscillate.

The governor is on by default. Operators can disable it with `URNETWORK_ADAPTIVE_GC=0`. If the operator sets `GOGC` directly, the governor backs off entirely and never touches the knob.

### Message pool sizing

The message pool free-list is auto-sized to RAM/32 at startup, capped at 256 MiB. This prevents nearly every packet from falling through to a GC allocation when managing thousands of proxies.

---

## 📝 Logging & Forensics

### Log locations

| Source | Location | Persists restarts? |
|--------|----------|-------------------|
| Health logs | `/dev/shm/urnetwork.log` | Survives process restarts, not host reboots (RAM-backed tmpfs) |
| Important events | `/dev/shm/urnetwork-important.log` | Survives process restarts, not host reboots (RAM-backed tmpfs) |
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
| `up=0` for all proxies | API/auth unreachable | `curl https://api.bringyour.com/hello` — is the API reachable? |
| Proxies stuck "degraded" | Transport connections failing | `[t]auth` log entries, network/firewall |
| Some proxies showing "auth still failing" | Those proxy IPs can't reach the API | Test from the proxy's network |
| High error count in `[net][s]select` | Proxy endpoint unreachable or slow | Probe the proxy directly: `curl -x socks5://ip:port https://example.com` |

### Memory / OOM

| Symptom | Likely cause | Fix |
|---------|-------------|-----|
| Heap growing during outage | All proxies hold buffers while retrying | Fixed in 26.4: GOMEMLIMIT + reaper. If still OOMing, you have too many proxies for the box — deploy fewer |
| Process using 100% swap | Heap exceeds physical RAM | Lower profile (`eco`), set `--max-memory`, or deploy fewer proxies |
| High goroutine count | Many proxies each with ~14 goroutines | The reaper (26.4) kills worst-performing degraded proxies. If still overloaded, deploy fewer proxies |

### Network issues

| Symptom | Check |
|---------|-------|
| Provider won't start | `journalctl -u urnetwork -n 50` or `docker logs urnetwork` |
| Auth fails | `test -s ~/.urnetwork/jwt && echo present` — is it present and non-empty? (don't `cat` it — it's a credential) |
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
| 30.0 | Cross-platform parity (Windows schtasks, macOS launchd), urnet-docker Design 2 host-side proxy ops. |
| 29.0 | Go urnet-tools provider discovery, report URL runtime config, hot-restart subcommands. |
| 28.0 | Standalone `urnet-docker` CLI for host-side Docker container discovery and delegation. |
| 27.0 | Initial Go rewrite of urnet-tools suite with targeting refusal safety model. |
| 26.4 | GOMEMLIMIT added for turbo profiles. Degraded-proxy reaper runs automatically. No config change needed. |
| 26.3 | `URNETWORK_SKIP_AUDIT` env var to skip startup system audit. Hub PAKE auth, choose_network, auto Tier 4 for 8 GiB+ RAM. |
| 26.2 | Hot-reload for hub commands, self-heal pool management (opt-in). |
| 26.1 | Docker in-place updates, Go 1.26 toolchain, dl.fullbars.xyz URLs. |

---

> Navigation: [← 🐣 Beginner](beginner.md) | [← 🧭 Intermediate](intermediate.md) | [📚 Configuration Reference](../Configuration.md)
