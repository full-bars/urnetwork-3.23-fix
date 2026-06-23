# 📜 URnetwork Provider — Log Message Reference

A plain-language guide to every log line you'll regularly see running urnetwork providers, whether binary or Docker. Examples are drawn from real production deployments.

---

## 📊 System Auditor & Smart Auto

```
[audit] Running system checks...
[audit] Conntrack Max: 65536 (Suboptimal! Target: 2097152)
[audit] Hint: System is not optimized for high volume. Run 'urnet-tools optimize' to fix.
[audit] Disk write speed: 22.4 MB/s (1024MB sync test)
[audit] Auto-enabling RAM logs due to slow disk I/O.
[tune] auto-profile: detected 1969 MiB RAM; applying 'Balanced' settings
```

Fires **once per process** at startup when `URNETWORK_PROFILE=auto` is set, regardless of how many proxy servers are loaded. (Prior to fix.15, this line fired once per proxy server, producing thousands of identical lines on large proxy lists.)

| Message | Meaning |
|---|---|
| `[audit] Suboptimal...` | The host OS has low limits (default ulimit or conntrack). This will throttle connections under heavy load. |
| `[audit] Disk write speed...` | Result of the 1GB cache-busting stress test. |
| `[audit] Auto-enabling RAM logs...` | The provider decided your disk is too slow and moved logs to `/dev/shm` to protect network performance. |
| `[tune] auto-profile...` | Confirms which performance tier (Low/Balanced/Perf) was selected based on detected RAM. |

---

## 🍃 Eco Memory Monitor

```
[eco] memory pressure detected (available=287MiB), GOGC=25
[eco] memory critical (available=134MiB), GOGC=10
[eco] memory pressure eased (available=462MiB), GOGC=50
```

Fires on state transitions when `URNETWORK_PROFILE=eco` or the `auto` profile selects a tier that enables eco mode (Low or Balanced tiers). The monitor checks available memory every 30 seconds and adjusts GOGC dynamically.

Exactly **one monitor runs per process** regardless of proxy count. (Prior to fix.15, one monitor goroutine was started per proxy server, meaning all copies would log the same line and call `runtime.GC()` simultaneously under pressure.)

| Message | Meaning |
|---|---|
| `memory pressure detected` | Available RAM dropped below 300 MiB; GOGC lowered to 25 to collect more aggressively. |
| `memory critical` | Available RAM below 150 MiB; GOGC at minimum (10). A forced GC cycle runs each tick. |
| `memory pressure eased` | Available RAM recovered above 450 MiB; GOGC restored to normal (50). |

These only appear when memory actually changes tier — a stable system shows none of them.

---

## 🏊 Buffer Pool Health

```
pool[2048] tag=0 [] r=1616413/t=1617695/c=20087 = 99.92% return / 98.76% reuse
```

Fires every 60 seconds. This is the provider's internal memory health check.

| Field | Meaning |
|---|---|
| `pool[2048]` | Buffer size in bytes. The provider pools fixed-size byte slices to avoid constant GC pressure. |
| `tag=0 []` | Internal tag used to categorize allocations. Usually `0` with an empty caller name in production. |
| `r=` | **Returned** — total buffers handed back to the pool (cumulative lifetime count). |
| `t=` | **Taken** — total buffers checked out from the pool (cumulative lifetime count). |
| `c=` | **Created** — how many times `Get()` found the pool empty and had to allocate a fresh buffer instead of reusing one. |
| `return %` | `r / t` — what fraction of taken buffers came back. Should be ~100%. A leak shows here. |
| `reuse %` | `(t - c) / t` — what fraction of checkouts found an existing buffer ready in the pool. High is good. |

**What to watch for:**
- `return %` dropping below 99% — buffers are being leaked somewhere
- `reuse %` below 95% — the pool is undersized for the load; GC pressure is higher than ideal
- `c=` growing rapidly between checks — pool is being depleted under load

**Examples from the fleet:**
- Detroit test server (1000 proxies, early): `c=320`, `99.99% reuse` — pool nearly perfectly sized
- Production server (long-running): `c=20087`, `98.76% reuse` — higher allocation pressure, still healthy
- Another production server: `c=7195`, `99.28% reuse` — moderate, normal for busy deployments

---

## 🚫 Transport Auth Error

```
[t]auth error 019e2d83-3118-5186-995f-aabe3b2dcf0b = Timeout. (34 suppressed)
```

The provider failed to authenticate a transport connection to the URnetwork platform. Each transport ID (the UUID) represents one proxy or connection attempt.

- The error is usually `Timeout.` — the platform didn't respond in time
- `(N suppressed)` tells you how many additional transports also failed since the last log line was emitted. The rate limiter allows at most one log per minute globally across all transports.
- Without the suppressed count, the first failure of a new session logs cleanly: `[t]auth error <id> = Timeout.`
- This is normal during platform outages or high load. The provider retries automatically.
- Seeing this occasionally is expected. Seeing it continuously for many minutes indicates a platform-side issue.

---

## ⏳ OOB Contract Backoff

```
[contract]oob err = Timeout.; backing off create contract OOB requests for 1m0s
```

The provider tried to request a contract via the out-of-band (OOB) control channel and got a timeout. It will stop sending OOB contract requests for 60 seconds before retrying.

- Fires at most once per minute (rate-limited)
- Sustained appearances over many minutes = platform OOB service degraded
- Does not affect already-established sessions, only new contract negotiations
- The provider continues running and retrying throughout

---

## 🚪 Session Exit — Could Not Create Contract

```
[s]019e0f4d-b48e-45e3-33e6-d7228666f41e->[]...019e2f50-4c42-571c-6adb-5c9a990d99e9 s(00000000-0000-0000-0000-000000000000) exit could not create contract.
```

A session between two clients failed because no contract could be allocated. The format is:

```
[s]<source-client-id>->[]...<destination-client-id> s(<contract-id>) exit <reason>
```

- `s(00000000-...)` — the nil contract ID means no contract was ever assigned
- This fires when traffic is being attempted but the platform can't issue contracts (OOB down, rate limited, etc.)
- Seeing these during an OOB backoff period is expected — they're proof that clients are trying to use this provider
- The session will retry

---

## ⚠️ Debit Contract Near Capacity

```
[s]debit contract 019e2c16-80c4-ef1d-edc7-47d788752706 failed +1420->13750 (12330/13107 total 94.1% full)
```

A contract was allocated and is filling up. The provider tried to debit bytes from it but it's near its limit.

- `+1420->13750` — tried to debit 1420 bytes, bringing the total to 13750
- `12330/13107 total 94.1% full` — the contract has used 94.1% of its byte allowance
- When a contract fills up a new one is negotiated automatically
- This line being present means data is actually flowing through the provider — it's a sign of real traffic

---

## 🚨 Outage Watcher

```
[outage] watcher active node=my-server (docker) webhook=configured
[outage] backend degraded
[outage] backend recovered
```

Monitors backend connectivity. It is designed to be conservative to avoid false alarms.

| Message | Meaning |
|---|---|
| `watcher active` | Confirms the background monitor is running and identifies the node. |
| `backend degraded` | The provider has failed several consecutive connection attempts to the platform. New connections are likely to fail. |
| `backend recovered` | Connectivity has been restored. The provider will resume normal operations. |

> [!NOTE]
> An outage is only declared after **5 minutes** of continuous failure. Alerts via webhook (if configured) fire on these transitions.

---

## 🗑️ Packet Drop Rate-Limiting

```
[r]drop: write error: connection reset by peer (1,420 suppressed)
```

The `[r]drop` message indicates the provider dropped a packet because it couldn't be delivered to the final destination (e.g., target website or proxy).

- These are **rate-limited to 1 per minute** globally to prevent log flooding during network instability.
- The `(N suppressed)` suffix shows how many other drops occurred since the last log line.
- High drop counts are normal during global outages or if a specific proxy server goes down.

---

## 💓 Health Heartbeat

```
[health] uptime=15m0s profile=auto heap=80MiB sys=255MiB connections=998 proxies=1150
```

Fires every 5 minutes (default). Provides passive liveness confirmation and resource utilization trends.

| Field | Meaning |
|---|---|
| `uptime` | How long the provider process has been running. |
| `profile` | The active performance profile (e.g., `auto`, `turbo-v4`, `lowmem`). |
| `heap` | RAM currently used by live Go objects. |
| `sys` | Total RAM reserved from the OS (includes stack, heap, and unused reservations). |
| `connections` | Total number of **active end-user NAT sessions** (TCP/UDP) currently routing through the provider. |
| `proxies` | Total number of **authenticated, working proxy links** to the platform (how many proxies from your list are online). |

**What to watch for:**
- `connections` staying at 0 — the provider is running but no traffic is being routed (normal if `proxies` is also 0, otherwise indicates lack of users).
- `proxies` much lower than your `proxy.txt` count — indicates many proxies are failing auth or networking (check `[net][s]select` logs).
- `heap` growing continuously over hours/days — potential memory leak.
- `heap` vs `connections` — if heap grows while connections stay flat, memory is being consumed by something other than traffic (e.g. large proxy list storage).

### 💀 Dead-Proxy Health Report

In addition to the main `[health]` line, when running with a proxy list the provider emits proxy health lines:

```
[health][proxies] up=1193 down=7 dead=4 degraded=3 recovered=5 lost=0 lifetime_recovered=51 lifetime_lost=39
[health][proxies] dead: proxy[112] (45.3.32.184:1081), proxy[266] (104.207.45.110:1081), ... (+2 more)
[health][proxies] degraded: proxy[49] (209.50.167.49:1081), proxy[1037] (209.50.169.110:1081), proxy[660] (98.76.54.32:1081)
```

| Field | Meaning |
|---|---|
| `up` / `down` | Current proxy state (`up` agrees with `proxies=N`). |
| `dead` | Proxies that have never successfully authenticated (trustworthy after ~1h). |
| `degraded` | Proxies that worked before but are currently down. |
| `recovered` / `lost` | Down->up and up->down transitions since the last heartbeat. |
| `lifetime_recovered` / `lifetime_lost` | Cumulative transition counts since process start. |

- The detail lines are capped at 50 entries in stdout (shows `... (+N more)` when truncated).
- A complete, uncapped history is mirrored to `proxy_health.state` and `proxy_health.log` (default `~/.urnetwork`).
- A real-time bandwidth and concurrent session load tracker is mirrored to `proxy_traffic.state` (default `~/.urnetwork`).

### ⏱️ Hourly Pulse Marker

```
[pulse] waking stalled transports: down=12 dead=3 degraded=9
```

An hourly retry sweep is performed to wake stalled transports. This marker logs the pre-pulse state, so you can track how many of the `down` proxies are `recovered` in the next heartbeat.

---

## 🔀 Outbound Connection Health (3.23-fix variant)

```
[net][s]select: proxy[42] (1.2.3.4:1081) [fragment] success=6086 error=192 clients=0
[net][s]select: proxy[13] (5.6.7.8:1081) [direct] success=2221 error=223
[net][s]select: direct success=171 error=3
```

Logged at INFO level in the 3.23-fix fork (promoted from debug level 2). Each line fires when the **provider itself** makes an outbound API call or WebSocket dial to the URnetwork platform (e.g. `api.bringyour.com/connect/control`) and records which route was used. This is the provider's own control-plane traffic, **not** end-user relay traffic.

> [!IMPORTANT]
> `success` and `clients` measure completely different things and do not correlate. A proxy with `success=5000 clients=0` is healthy and talking to the platform — it just has no users assigned to it right now. The platform decides which providers serve which clients.

| Field | Meaning |
|---|---|
| `proxy[N] (ip:port)` | The SOCKS5 proxy used to reach the platform. Absent when using the direct path. |
| `[fragment]` / `[reorder]` / `[fragment+reorder]` / `[direct]` | DPI bypass strategy used for this outbound call (see below). |
| `success=N` | Cumulative provider API/WebSocket calls that succeeded through this route since last reset. |
| `error=N` | Cumulative failures. A healthy error rate is under ~10% of successes. |
| `clients=N` | **Independent metric.** Number of end-user relay sessions currently routing through this proxy via LocalUserNat. Zero is normal when no users are assigned. |
| `age=Xs` | How long the oldest current user session has been continuously present on this proxy. Only shown when `clients > 0`. |

### Connection strategies

The provider tries multiple strategies for its outbound connections to avoid DPI and firewall interference. These are techniques applied to the TLS handshake, not to user traffic:

| Mode | Meaning |
|---|---|
| `direct` | Standard TLS with no modifications — the default path when no proxy is configured. |
| `fragment` | Splits the TLS ClientHello across multiple TCP segments so stateful DPI cannot read the SNI hostname. Highest priority; no throughput cost. |
| `reorder` | Sends TLS fragments out of order to confuse stateless DPI inspectors. |
| `fragment+reorder` | Both techniques combined. |

The selector tracks per-strategy success rates and prefers whichever is most reliable. When errors accumulate on one strategy, it rotates to the next.

**What to watch for:**
- `error` growing faster than `success` on a specific proxy — that proxy's outbound path to the platform is degraded. Consider removing it from `proxy.txt`.
- Repeated strategy rotations on the same proxy (log shows `[fragment]` → `[fragment+reorder]` → `[direct]` in quick succession) — the proxy has inconsistent connectivity to the platform.
- `clients=N` staying at 0 across all proxies for extended periods is normal when the platform hasn't assigned users to this provider. It is not related to `success` counts.

---

## 📡 Relay Traffic Rates

```
[traffic] total rx=2.3 MB/s tx=0.8 MB/s clients=1 active_proxies=2
[traffic] proxy[124] (216.26.228.3:1081) rx=2.3 MB/s tx=0.8 MB/s clients=1 age=5m12s billable_today=1.2 GB
[traffic] proxy[230] (45.3.48.195:1081) rx=0.4 MB/s tx=0.1 MB/s clients=0 billable_today=340 MB
```

Fires on every health heartbeat tick (same cadence as `[health]`). Measures **actual end-user relay traffic** — bytes flowing through the provider's IP relay stack (LocalUserNat) on behalf of connected clients. This is what earns you platform credit.

| Field | Meaning |
|---|---|
| `rx` / `tx` | Bytes per second relayed since the previous heartbeat tick. |
| `clients` | Number of end-user relay sessions active on this proxy right now. |
| `age` | How long the current client session has been continuously present (shown when `clients > 0`). |
| `billable_today` | Cumulative billable bytes relayed through this proxy since midnight (local time). Resets at midnight. |
| `active_proxies` | How many proxies moved any bytes since the last tick (summary line only). |

The per-proxy lines only appear for proxies that moved bytes since the last tick — proxies with zero traffic are omitted. The `total` summary line always appears so you have one line to grep even when nothing is flowing (`rx=0 B/s tx=0 B/s clients=0`).

> [!NOTE]
> `[traffic]` and `[net][s]select` measure different things. `[net][s]select success=N` is the provider's own API calls to the platform. `[traffic] rx=X` is actual bytes your provider relayed for end-users. Both can be high or low independently.

---

## ⏱️ TCP Write Timeout (transport stream)

```
[ts]019e28a3-76dd-1fd5-08a3-342775fdfa7b-> error = write tcp 172.17.0.2:58902->216.26.233.197:1081: i/o timeout
```

A TCP write to a proxy server timed out at the transport stream layer. This appears when network conditions are degraded (high latency, packet loss).

- `172.17.0.2` — the container's internal IP
- `216.26.233.197:1081` — the proxy server that stopped responding
- Followed shortly by a `[t]auth error` for the same transport ID
- Common during netem stress testing or real network degradation

---

## 😱 Startup — Proxy Auth Panic (handled)

```
W0516 trace.go:47] Unexpected error: {"error":"*errors.errorString=Timeout.","stack":[...,"main.provideAuth",...]}
```

During startup with a large proxy pool, many proxies attempt to authenticate simultaneously. Some time out and `provideAuth` panics with the timeout error. The `HandleError` wrapper catches the panic and logs it as JSON instead of crashing.

- This is benign — the proxy goroutine restarts and retries
- Expected on startup with 200+ proxies
- Goes away once the initial auth rush settles (usually within 2-3 minutes)
- Only the provider binary startup path triggers this, not the ongoing connection phase

---

## ℹ️ Startup — Provider Info

```
Provider e442be5 started
client_id: 019e2d67-5a52-b4f0-a00f-0bb97281dfe0
instance_id: 019e2d67-5a73-4bb3-6661-df9b5c595003
```

- `Provider <version>` — the git commit hash or version tag the binary was built from
- `client_id` — the provider's permanent identity on the URnetwork platform
- `instance_id` — unique ID for this specific run, changes on restart

---

## 🔄 Startup — Proxy Loading

```
[INFO] proxy.txt found; adding proxy
added server 65.111.10.67:1081 (91***rn/cf***9m)
Using 1000 proxy servers:
  proxy[0] 216.26.225.158:1081 (91***rn/cf***9m)
  proxy[1] 45.3.34.215:1081 (91***rn/cf***9m)
  ...
```

- Each `added server` line confirms a proxy was registered successfully
- Credentials are partially redacted in logs (`***`)
- `Using N proxy servers:` summarizes the loaded pool with index assignments

---

## 📈 Reading Pool Stats Across Time

The pool stat fires every minute, so you can derive buffer throughput by subtracting consecutive `r=` values:

```
r=5601295  (05:25)
r=5607261  (05:26)
```
→ 5,966 buffers returned in 1 minute = active traffic flowing

A flat `r=` counter that doesn't grow means no sessions are active. A rapidly growing counter means heavy throughput.

---

## 🔑 JWT Auto-Refresh

```
[jwt] refreshing token — expires in 10h 0m (less than 48h 0m threshold)
[jwt] refresh failed: api error: ... (will retry in 1h)
[jwt] token refreshed successfully (next check in 1h)
```

Fires at most once per month per provider. The provider checks the JWT expiry every hour. When the token is within 48 hours of its `exp` claim (the API issues 30-day tokens), the provider proactively calls the auth API for a replacement and writes it to disk — no restart, no downtime.

| Message | Meaning |
|---|---|
| `refreshing token` | JWT is close to expiry; initiating a refresh call to the auth API. |
| `refresh failed` | The API call or disk write failed. Will retry on the next hourly check. |
| `token refreshed successfully` | A new JWT was obtained and written to `~/.urnetwork/jwt`. The old token remains valid until its original expiry, so there is no disruption. |

---

## 🐌 Proxy Startup Pace Monitor

```
[pace] ⚠ warmup: 47/200 up (24%), 150 connecting, 3 done
[pace] warmup: 142/200 up (71%), 55 connecting
[pace] ✓ warmup: 196/200 up (98%), 4 connecting — done
```

Fires every **30 seconds** during provider startup when the proxy fleet is warming up. Shows real-time progress of proxy authentication and connection. The pace monitor is a passive observer — it does not influence the stagger rate.

Once the `✓ done` line is logged, the `paceMonitor` goroutine exits. No further `[pace]` output is produced — silence after that line is expected and correct.

| Message | Meaning |
|---|---|
| `⚠ warmup: X/Y up (Z%), N connecting` | Fewer than 50% of proxies are up and more than 10 are still connecting — slow warmup. |
| `warmup: X/Y up (Z%), N connecting` | Normal warmup progress. |
| `✓ warmup: X/Y up (Z%), N connecting — done` | More than 90% of proxies are up and fewer than 5 are still connecting. Logged once, then the goroutine exits. |
