# URnetwork Provider — Log Message Reference

A plain-language guide to every log line you'll regularly see running urnetwork providers, whether binary or Docker. Examples are drawn from real production deployments.

---

## Buffer Pool Health

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

## Transport Auth Error

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

## OOB Contract Backoff

```
[contract]oob err = Timeout.; backing off create contract OOB requests for 1m0s
```

The provider tried to request a contract via the out-of-band (OOB) control channel and got a timeout. It will stop sending OOB contract requests for 60 seconds before retrying.

- Fires at most once per minute (rate-limited)
- Sustained appearances over many minutes = platform OOB service degraded
- Does not affect already-established sessions, only new contract negotiations
- The provider continues running and retrying throughout

---

## Session Exit — Could Not Create Contract

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

## Debit Contract Near Capacity

```
[s]debit contract 019e2c16-80c4-ef1d-edc7-47d788752706 failed +1420->13750 (12330/13107 total 94.1% full)
```

A contract was allocated and is filling up. The provider tried to debit bytes from it but it's near its limit.

- `+1420->13750` — tried to debit 1420 bytes, bringing the total to 13750
- `12330/13107 total 94.1% full` — the contract has used 94.1% of its byte allowance
- When a contract fills up a new one is negotiated automatically
- This line being present means data is actually flowing through the provider — it's a sign of real traffic

---

## Connection Selection (3.23-fix variant)

```
[net][s]select: fragment success=6086 error=192
[net][s]select: reorder success=1114 error=140
[net][s]select: normal success=2221 error=223
[net][s]select: fragment+reorder success=3727 error=172
```

Logged at INFO level in the 3.23-fix fork (promoted from debug level 2). Each line represents the provider selecting a routing strategy for a client session.

| Mode | Meaning |
|---|---|
| `normal` | Standard direct routing |
| `fragment` | Packet fragmentation applied to work around path MTU issues |
| `reorder` | Packets reordered to improve delivery on lossy paths |
| `fragment+reorder` | Both applied |

- `success=N` — cumulative successful connections using this strategy
- `error=N` — cumulative failed attempts
- A healthy error rate is under ~10% of successes
- High error counts on a specific mode suggest that strategy isn't working well on this server's network path

---

## TCP Write Timeout (transport stream)

```
[ts]019e28a3-76dd-1fd5-08a3-342775fdfa7b-> error = write tcp 172.17.0.2:58902->216.26.233.197:1081: i/o timeout
```

A TCP write to a proxy server timed out at the transport stream layer. This appears when network conditions are degraded (high latency, packet loss).

- `172.17.0.2` — the container's internal IP
- `216.26.233.197:1081` — the proxy server that stopped responding
- Followed shortly by a `[t]auth error` for the same transport ID
- Common during netem stress testing or real network degradation

---

## Startup — Proxy Auth Panic (handled)

```
W0516 trace.go:47] Unexpected error: {"error":"*errors.errorString=Timeout.","stack":[...,"main.provideAuth",...]}
```

During startup with a large proxy pool, many proxies attempt to authenticate simultaneously. Some time out and `provideAuth` panics with the timeout error. The `HandleError` wrapper catches the panic and logs it as JSON instead of crashing.

- This is benign — the proxy goroutine restarts and retries
- Expected on startup with 200+ proxies
- Goes away once the initial auth rush settles (usually within 2-3 minutes)
- Only the provider binary startup path triggers this, not the ongoing connection phase

---

## Startup — Provider Info

```
Provider e442be5 started
client_id: 019e2d67-5a52-b4f0-a00f-0bb97281dfe0
instance_id: 019e2d67-5a73-4bb3-6661-df9b5c595003
```

- `Provider <version>` — the git commit hash or version tag the binary was built from
- `client_id` — the provider's permanent identity on the URnetwork platform
- `instance_id` — unique ID for this specific run, changes on restart

---

## Startup — Proxy Loading

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

## Reading Pool Stats Across Time

The pool stat fires every minute, so you can derive buffer throughput by subtracting consecutive `r=` values:

```
r=5601295  (05:25)
r=5607261  (05:26)
```
→ 5,966 buffers returned in 1 minute = active traffic flowing

A flat `r=` counter that doesn't grow means no sessions are active. A rapidly growing counter means heavy throughput.
