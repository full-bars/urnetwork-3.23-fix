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
| `[tune] auto-profile...` | Confirms which performance tier (Low/Balanced/Perf/Extreme) was selected based on detected RAM. |

---

## 🚀 Provider Startup

```
❤️ [startup] provider version=v3.23.0-fix.24.34
```

Emitted exactly **once per provider process**, early in the startup sequence before any proxy work begins. This line confirms the exact binary version that is running.

| Field | Meaning |
|---|---|
| `version` | The provider binary version (from `-ldflags -X main.Version=...`). In Docker this matches the image tag; after `urnet-tools update` it reflects the updated binary. |

> [!NOTE]
> Docker deployments also log `[INFO] Running UrNetwork build v...` from the startup script; the `[startup]` line makes the same information available in bare-metal/binary installs and is written to RAMLOGS when enabled.

The existing `client_id` and `instance_id` lines are printed separately, once per proxy, and are unchanged by this log line.

---

## 🧠 Adaptive GC Governor (pressure monitor)

```
[proxy][pressure] gcGovernor armed (baseline GOGC=100)
[proxy][pressure] gcGovernor tighten_gogc50_heap0.72 (heap=0.72 go=50)
[proxy][pressure] gcGovernor hard_gogc25_heap0.82 (heap=0.82 go=25)
[proxy][pressure] gcGovernor critical_gogc10_heap0.94 (heap=0.94 go=10)
```

The consolidated adaptive GC governor lives in the pressure monitor and is the single writer to the Go GC percentage knob for the whole process. It is fed by both the process heap fraction and host available RAM, and it takes the tighter of the two. It applies to every profile and is on by default. Operators can disable it with `URNETWORK_ADAPTIVE_GC=0`.

| Message | Meaning |
|---|---|
| `gcGovernor armed` | Startup message that shows the captured baseline GOGC. |
| `gcGovernor tighten` | Governor lowered GOGC to `min(baseline, 50)` (heap >= 0.70 or host RAM <= 300 MiB). |
| `gcGovernor hard` | Governor lowered GOGC to `min(baseline, 25)` (heap >= 0.80 or host RAM <= 150 MiB). |
| `gcGovernor critical` | Governor lowered GOGC to `min(baseline, 10)` and called `FreeOSMemory` (heap >= 0.92 or host RAM <= 150 MiB). |

The former `[eco]` memory monitor lines are retired. Their host available RAM signal now flows through this governor, so small memory-fragile boxes keep the same protection.

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
[health] uptime=15m0s profile=auto heap=80MiB sys=255MiB goroutines=2156 connections=998 proxies=1150
```

Fires every 5 minutes (default). Provides passive liveness confirmation and resource utilization trends.

| Field | Meaning |
|---|---|
| `uptime` | How long the provider process has been running. |
| `profile` | The active performance profile (e.g., `auto`, `turbo-v4`, `lowmem`). |
| `heap` | RAM currently used by live Go objects. |
| `sys` | Total RAM reserved from the OS (includes stack, heap, and unused reservations). |
| `goroutines` | Number of live goroutines. Useful for spotting leaks or runaway growth (e.g., the self-wake loop fixed in v3.23.0-fix.24.33). |
| `connections` | Total number of **active end-user NAT sessions** (TCP/UDP) currently routing through the provider. |
| `proxies` | Total number of **authenticated, working proxy links** to the platform (how many proxies from your list are online). |

**What to watch for:**
- `connections` staying at 0 — the provider is running but no traffic is being routed (normal if `proxies` is also 0, otherwise indicates lack of users).
- `proxies` much lower than your `proxy.txt` count — indicates many proxies are failing auth or networking (check `[net][s]select` logs).
- `heap` growing continuously over hours/days — potential memory leak.
- `heap` vs `connections` — if heap grows while connections stay flat, memory is being consumed by something other than traffic (e.g. large proxy list storage).
- `goroutines` climbing steadily while load is flat — likely a goroutine leak (watch for repeated logs that should fire once per process, such as `[tune] auto-profile`).

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

### `proxy_health.log` row format

`proxy_health.log` receives one append-line per proxy state transition (complete, uncapped; rotated to `proxy_health.log.1` at 20 MB, one generation kept):

```
| 2026-08-04T00:12:03Z | RECOVERED | proxy[47]  | 1.2.3.4:8080     | after=3m12s |
| 2026-08-04T00:12:03Z | DEGRADED  | proxy[49]  | 5.6.7.8:1081     |             |
| 2026-08-04T00:12:03Z | DEAD      | proxy[112] | 45.3.32.184:1081 |             |
```

| Field | Meaning |
|---|---|
| Timestamp | RFC3339 UTC. |
| `RECOVERED` | A proxy that was down came back up. `after=` shows how long it was down (only when a `downSince` was recorded). |
| `DEGRADED` | A proxy that was up went down (worked before, now not). |
| `DEAD` | A proxy that never connected within a full pulse cycle. Emitted **once per proxy** (the `deadLogged` latch) prevents repeat rows for the same proxy. |

> [!IMPORTANT]
> `DEAD` rows were unreachable before the connecting-state bound shipped (the `!connecting` gate could never pass for a never-up proxy, so the path was latently dead). A fleet that has never seen `DEAD` rows will start seeing them for proxies that genuinely never connected within 65 minutes. This is a fixed latent bug; the rows are diagnostics only and nothing alerts on them.

### ⏱️ Hourly Pulse Marker

```
[pulse] waking stalled transports: down=12 dead=3 degraded=9 connecting=4
```

An hourly retry sweep is performed to wake stalled transports. This marker logs the pre-pulse state, so you can track how many of the `down` proxies are `recovered` in the next heartbeat.

| Field | Meaning |
|---|---|
| `down` | Sum of `dead` + `degraded` proxies. |
| `dead` | Proxies that have never successfully authenticated (trustworthy after ~1h). |
| `degraded` | Proxies that worked before but are currently down. |
| `connecting` | Proxies registered and still establishing their first WebSocket. A never-connected proxy counts as `connecting` only until its `connectingStaleAfter` window expires (65 minutes, one hourly pulse interval plus margin); past that it falls to `dead`. |

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
[traffic] total rx=2.3 MB/s tx=0.8 MB/s clients=1 active_proxies=2 billable_today=1.5 GB earning=yes
[traffic] proxy[124] (216.26.228.3:1081) rx=2.3 MB/s tx=0.8 MB/s clients=1 age=5m12s billable_today=1.2 GB
[traffic] proxy[230] (45.3.48.195:1081) rx=0.4 MB/s tx=0.1 MB/s clients=0 billable_today=340 MB
```

Fires on every health heartbeat tick (same cadence as `[health]`). Measures **actual end-user relay traffic** — bytes flowing through the provider's IP relay stack (LocalUserNat) on behalf of connected clients. This is what earns you platform credit.

| Field | Meaning |
|---|---|
| `rx` / `tx` | Bytes per second relayed since the previous heartbeat tick. |
| `clients` | Number of end-user relay sessions active on this proxy right now. |
| `age` | How long the current client session has been continuously present (shown when `clients > 0`). |
| `billable_today` | Cumulative billable bytes relayed through this proxy since midnight (local time). Resets at midnight. On the `total` line it is the fleet-wide sum. |
| `active_proxies` | How many proxies moved any bytes since the last tick (summary line only). |
| `earning` | `yes` if any billable bytes moved this tick, else `no` (summary line only) — a quick grep for "is this node earning at all". |

The per-proxy lines only appear for proxies that moved bytes since the last tick — proxies with zero traffic are omitted. The `total` summary line always appears so you have one line to grep even when nothing is flowing (`rx=0 B/s tx=0 B/s clients=0`).

> [!NOTE]
> `[traffic]` and `[net][s]select` measure different things. `[net][s]select success=N` is the provider's own API calls to the platform. `[traffic] rx=X` is actual bytes your provider relayed for end-users. Both can be high or low independently.

---

## 💰 Profit Heartbeat (3.23-fix)

```
[profit] earning=yes reason=- clients=4 rate=2.1 MB/s proxies_up=12 serving=3 idle=9
[profit] earning=no reason=idle clients=0 rate=0 B/s proxies_up=12 serving=0 idle=12
```

A fast, focused answer to **"are we earning right now, and if not, why?"**, emitted by `runProfitHeartbeat` every **15 seconds** — independent of the 5-minute `[health]`/`[traffic]` heartbeat. It uses `ProxyHealthSnapshot`, so it never disturbs the health heartbeat's dead/recovered baseline. It folds the headline earning signal into one greppable line so it survives even a tiny in-RAM log window.

| Field | Meaning |
|---|---|
| `earning` | `yes` if billable bytes moved in the last interval, else `no`. |
| `reason` | Why not earning (`-` while earning): `warmup` (still ramping up), `no_proxies` (none up), `idle` (proxies up but no clients matched), `no_traffic` (clients present but no billable bytes moved). |
| `clients` | End-user relay sessions active across all proxies right now. |
| `rate` | Aggregate billable throughput since the previous tick. |
| `proxies_up` | Proxies whose platform transport is currently live. |
| `serving` | Of those, how many are carrying at least one client. |
| `idle` | Up proxies carrying no clients (`proxies_up - serving`). |

To keep quiet periods from flooding the log, `earning=no` lines throttle to **once every 5 minutes** — except an `earning=no` line always fires **immediately on the `yes -> no` transition**, so the exact moment traffic stopped is visible. `earning=yes` lines print every tick.

---

## 📈 Earning Windows & Utilization (3.23-fix)

```
[earn] billable_1m=4.2 MB billable_5m=31 MB billable_15m=88 MB billable_60m=402 MB active=yes
[earn] proxies_up=12 serving=3 idle=9 clients=4
```

Two distinct `[earn]` lines surface **how much** and **how well** the node is earning:

**Rolling windows** (`billable_1m`/`5m`/`15m`/`60m`) — emitted per minute by `runEarningWindows`. Cumulative billable bytes over the trailing 1/5/15/60-minute windows, so you can see the trend at a glance. `active=yes` when any billable bytes moved in the last minute.

**Proxy utilization** (`proxies_up`/`serving`/`idle`/`clients`) — emitted on the 5-minute health tick. Shows how many up proxies are actually carrying users (`serving`) versus sitting `idle`. Sustained high `idle` with `proxies_up > 0` means the platform is **not assigning users** to this node — an earning signal distinct from `[traffic]` (bytes) and `[contract]` (assignments).

---

## 💾 Billable Rate Writer

```
[billable_rate] writer started (interval=10s)
[billable_rate] warn: write failed: open /root/.urnetwork/billable_rate: permission denied
[billable_rate] writer stopped
```

Persists the current billable transfer rate to `~/.urnetwork/billable_rate` on an interval so one-shot tools can read it without polling the live counters. This file is what `urnet-tools idle-update`'s threshold check reads. If you are debugging why `idle-update` never fires, this is the file (and these lines) to look at.

| Message | Meaning |
|---|---|
| `writer started (interval=...)` | The writer loop began; interval is the persistence cadence. |
| `warn: write failed: ...` | The rate file could not be written (permissions, disk). The rate itself is unaffected; the file is a mirror. |
| `writer stopped` | The writer loop exited (provider shutdown or context cancel). |

---

## 📑 Contract Lifecycle (3.23-fix)

```
[contract] acquired size=256 KiB destination=0142...c3a9
[contract] denied = insufficient allowance destination=0142...c3a9
[contract] closed acked=198 KiB allotted=256 KiB util=77% destination=0142...c3a9
```

A contract is the platform's bandwidth grant for relaying a client's traffic. These lines bracket a contract's life:

| Field | Meaning |
|---|---|
| `size` (acquired) | Bytes granted by this contract. |
| `denied` | The platform rejected a contract request; the message after `=` is the reason (e.g. `insufficient allowance`). A denied contract carries no `size`/`acked`. Frequent denials alongside low `util` means contracts are being refused, not just unused. |
| `acked` (closed) | Bytes actually acknowledged/relayed before the contract closed. |
| `allotted` (closed) | Bytes the contract granted (same basis as `size`). |
| `util` (closed) | `acked / allotted` as a percentage — actual revenue-generating usage, not just the grant. |
| `destination` | The client/destination the contract served. |

Low `util` across many `[contract] closed` lines means contracts are being acquired but barely used (clients connecting then leaving, or short transfers) — distinct from not acquiring contracts at all.

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

### `proxy.state` reconciliation

```
[proxy] pruned 711 stale proxy.state entries (no longer desired)
[proxy] skipping state prune this cycle: proxy_url.json unavailable
```

Emitted by the reload reconciler when it reconciles `~/.urnetwork/proxy.state` against the desired proxy set (config/file + URL cache). Entries for proxies that are no longer desired are pruned so `remove-dead` doesn't re-report ghosts forever.

| Message | Meaning |
|---|---|
| `pruned N stale proxy.state entries (no longer desired)` | `N` state entries were deleted because their proxies are gone from every source. On the first run after upgrading, this can be a large number: the accumulated ghost backlog being cleaned, not an outage. |
| `skipping state prune this cycle: proxy_url.json unavailable` | The URL cache could not be read, so the prune pass was skipped rather than risking deletion of state for still-desired URL proxies over a transient error. Nothing was removed this cycle; the next reload retries. |

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
[jwt] refreshing token — 7-day periodic refresh due (last refresh 168h0m ago)
🔑 [jwt] refresh → step 1/3: requesting auth code...
🔑 [jwt] refresh → step 1/3 ok: auth code received (684 chars)
🔑 [jwt] refresh → step 2/3: exchanging auth code for network JWT...
🔑 [jwt] refresh → step 2/3 ok: network JWT received (512 chars)
🔑 [jwt] refresh → step 3/3: verifying new token against https://api.bringyour.com/transfer/stats...
🔑 [jwt] refresh → step 3/3 ok: verification passed (HTTP 200)
🔑 [jwt] refresh OK — network JWT written to /root/.urnetwork/jwt (512 bytes, next refresh in 168h0m)
```

Two triggers (OR logic — either fires a refresh):
1. **Periodic (7-day)**: Has it been ≥7 days since the last successful refresh? Primary mechanism. Guarantees the token is rotated on a fixed cadence.
2. **Expiry fallback (48h)**: Is the token within 48 hours of expiring? Safety net if the periodic refresh failed repeatedly.

The refresher uses `/auth/code-create → /auth/code-login` (same flow as initial login). Before overwriting the on-disk JWT, it verifies the new token against `GET /transfer/stats`. A regression guard rejects any response containing a `client_id` claim (catches future regressions).

| Message | Meaning |
|---|---|
| `step 1/3` | Requesting an auth code from the API. |
| `step 2/3` | Exchanging the auth code for a fresh network JWT. |
| `step 3/3` | Verifying the new token works via a read-only stats endpoint. |
| `refresh OK` | New network JWT written to disk successfully. |
| `refresh FAILED: ... — keeping existing JWT` | Refresh failed at any step. The existing JWT is preserved. Will retry in 1h. |

## 🔑 JWT Startup Health

```
🔑 [jwt] expires in 12 days
🔑 [jwt] EXPIRED 3 days ago — refresh needed
```

Emitted once at startup. Shows the current JWT's health status.

---

## 🤝 Hub Bootstrap & CA Cert

```
[hub] bootstrapping CA cert from https://hub-server:8443/api/ca-cert
[hub] CA cert installed from hub token bootstrap
[hub] WARNING: verified CA cert fetch failed (expected for a direct password-derived CA hub) — falling back to an unverified fetch. The hub token will be sent before the hub's identity is confirmed. Only safe if hub and provider share a trusted network at boot; run 'urnet-tools hub link <url>' manually instead if you can't accept that.
[hub] bootstrap URL error: failed to parse report URL
```

Fires once at startup when `URNETWORK_REPORT_URL` starts with `https://` and the hub has a CA. The provider fetches the hub's CA certificate over HTTPS so subsequent report/heartbeat POSTs can verify the hub's identity instead of trusting it blindly. Plain-`http://` report URLs skip this bootstrap entirely (there is nothing to verify).

| Message | Meaning |
|---|---|
| `bootstrapping CA cert from <url>` | The provider is fetching the hub's cert from `<hub>/api/ca-cert`. |
| `CA cert installed from hub token bootstrap` | The fetched cert was written to the hub CA store; future hub traffic is verified. |
| `WARNING: verified CA cert fetch failed ...` | **Security-relevant.** The verified fetch failed (normal for a direct password-derived CA hub that hasn't been linked yet) and the provider fell back to an *unverified* TLS fetch, so the hub token will be sent before the hub's identity is confirmed. Only acceptable when the hub and provider share a trusted network at boot. If you can't accept that, run `urnet-tools hub link <url>` manually instead. |
| `bootstrap URL error` / `bootstrap request error` / `CA cert write error` | The bootstrap attempt failed at the named step; the provider continues without a verified hub cert. |

---

## 🌐 WebRTC Peer Lifecycle

```
🔗 [signal] peer connected client_id=abc... type=webrtc
🔗 [signal] peer disconnected client_id=abc... type=webrtc reason=timeout
```

Fires once per P2P session creation/destruction. Low frequency — one event per peer connection, not per packet.

| Message | Meaning |
|---|---|
| `peer connected` | A new WebRTC peer connection was established. |
| `peer disconnected` | A peer connection was closed or timed out. |

### SCTP progress watchdog

```
[peerconn]SCTP no progress for 10s with 524288 bytes buffered; reconnecting
```

Logged by the lazy SCTP progress watchdog (added in the WebRTC tuning pass). It starts only after the first successful write, and fires when the data plane has made no progress for the watchdog window (10s) while bytes are still buffered, i.e. ICE consent looks healthy but the association is blackholed. The connection is torn down and re-established.

> [!NOTE]
> This line looks alarming but is **correct, expected behaviour**: it is the teardown path for an association that stopped moving data, not a transport failure report. Seeing it occasionally on lossy links is normal; seeing it constantly for the same destination warrants investigation.

---

## 📈 Traffic Velocity & Peaks

```
📈 [traffic] velocity: 3.2x → rx=12.3 MB/s tx=8.7 MB/s (was rx=3.8 MB/s tx=2.1 MB/s)
📈 [traffic] velocity: 0.3x → rx=1.2 MB/s tx=0.8 MB/s — traffic dropping
📈 [traffic] total rx=6.3 MB/s tx=3.9 MB/s peak_rx=18.4 MB/s peak_tx=7.8 MB/s clients=16
```

Velocity detection fires when total rate changes 3x+ between 5-minute health heartbeat ticks. Peak tracking records the maximum observed rates since startup.

| Message | Meaning |
|---|---|
| `velocity: N.Mx →` | Aggregate rate changed by N.Mx since last tick. Greater than 1 = increase. |
| `traffic dropping` | Rate decreased below 0.5x of previous — notable decline. |
| `peak_rx` / `peak_tx` | Highest observed receive/transmit rates in this session. |

---

## ✈️ Client Flight Markers

```
✈️ [traffic] clients 0→4 (first connect in 5h)
🛬 [traffic] clients 4→0 (last disconnect in 3m)
```

Emitted when aggregate client count transitions between zero and non-zero (and vice versa).

---

## 🚨 DNS Health

```
[doh] ⚠ 5 failures in last 5m
🚨 [doh] 120 failures in last 5m — possible DNS outage
```

Rate-limited to 1 per 5 minutes globally. Escalates to 🚨 when failures exceed 100 in a window. Failures also tracked as `dns_failures=N` in the `[health]` heartbeat.

| Message | Meaning |
|---|---|
| `⚠ N failures in last 5m` | Moderate DoH resolution failures — investigate if persistent. |
| `🚨 N failures` | Over 100 failures in 5 minutes — likely DNS outage. |

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
