# Design: Per-Heartbeat Dead-Proxy Report

**Date:** 2026-06-02
**Status:** Approved (pending spec review)
**Target release:** v3.23.0-fix.16
**Branch:** `feature/dead-proxy-health-report`

## Problem

The `[health]` heartbeat now reports `proxies=N` (authenticated proxy transports
currently live on the platform, shipped in PR #31). When `N` is lower than the
proxy-list size, the operator can see *that* some proxies are down but not *which*
ones, nor whether a given proxy never worked or worked before and dropped.

Separately, fix.11 (`13372f2`) added an **autonomous retry mechanism**: an hourly
`TriggerPulse()` wakes all stalled transports, and per-proxy exponential backoff
(5s -> 1h cap) parks dead proxies between pulses. That retry runs **silently** —
the operator has no way to know which proxies the pulse keeps failing to recover.

This feature adds the missing **visibility layer** on top of fix.11's blind retry:
a per-heartbeat report of which proxies are down, split by whether they have ever
worked.

## Goals

- At each `[health]` heartbeat, emit a companion report of proxies that are not
  currently live, categorized as `never_up` vs `flapped`.
- Stay **pure observability** — no change to the pulse/backoff retry logic, no
  auto-removal of proxies.
- Be safe at scale (1200+ proxies): bounded log output, no hot-path cost.

## Non-Goals (YAGNI)

- Surfacing per-proxy backoff/retry state (couples to `ClientStrategy` internals).
- Auto-removing dead proxies from rotation (changes runtime behavior; separate
  release).
- Per-proxy timestamps in the report (the `[net][s]select` line already carries
  `success=/error=` detail for deep dives).

## Definition of "not working"

A proxy is **down** if its platform transport is not currently live (this is the
exact inverse of the `proxies=N` count). Down proxies split into two buckets via a
single per-proxy bit, `everUp`:

| Bucket | Condition | Meaning | Operator action |
|---|---|---|---|
| `never_up` | `everUp == false` | Transport has never authenticated since process start. | Likely bad proxy/credentials — remove from list. |
| `flapped` | `everUp == true`, currently down | Authenticated at least once, down now (or parked in backoff). | Transient/degraded — may recover on next pulse. |

### Interaction with the fix.11 retry cadence

Heartbeat fires every ~5 min; the pulse fires every 60 min. Implications baked
into the framing:

- `flapped` means *"down or parked in backoff,"* **not** "actively failing this
  instant." A proxy that blipped can sit in backoff for up to an hour before the
  pulse retries it, appearing `flapped` the whole time.
- `never_up` is **most trustworthy after uptime > ~1h** (one full pulse cycle).
  Before that, a never-connected proxy may simply not have been pulse-retried yet.
  A proxy that remains `never_up` across multiple hourly pulses is a confirmed
  dead endpoint.
- `everUp` is **monotonic for the process lifetime** and is *not* reset by the
  pulse (which only resets `failureCount`/dialer health). So a proxy that worked
  once stays in `flapped` through every pulse/backoff cycle and is never wrongly
  demoted to `never_up`. The report and the retry machinery do not fight.

## Architecture

### Component 1: per-proxy registry (`proxy_health.go`, package `connect`)

A new file holding a process-global registry:

```
type proxyHealth struct {
    address     string
    currentlyUp bool
    everUp      bool
}

var (
    proxyHealthMu sync.Mutex
    proxyHealthByIndex map[int]*proxyHealth
)
```

Exported API:

- `RegisterProxy(index int, address string)` — idempotent; creates an entry with
  `currentlyUp=false, everUp=false` if absent. Called eagerly at startup for every
  proxy in the list.
- `markProxyUp(index int)` — sets `currentlyUp=true, everUp=true`.
- `markProxyDown(index int)` — sets `currentlyUp=false`.
- `ProxyHealthSnapshot() (up int, neverUp []string, flapped []string)` — walks the
  registry under the lock and returns the summary count plus the two formatted
  lists (`proxy[idx] (address)` strings, index-sorted). Called once per heartbeat.

Eager registration is required so that a proxy which **never connects at all**
(never reaching `markProxyUp`) still appears as `never_up`. Lazy registration would
silently omit exactly the proxies the feature exists to surface.

### Component 2: instrumentation (reuse PR #31's two points)

In `transport.go`, `runH1` and `runH3`, at the same spots where PR #31 added the
`activeProxyConnections` atomic inc/dec:

- On transport up (after `routeManager.UpdateTransport`):
  `markProxyUp(self.proxyIndex())`
- In the teardown `defer`: `markProxyDown(self.proxyIndex())`

`self.proxyIndex()` resolves via `self.clientStrategy.settings.ProxySettings`
(same package; `ProxySettings` carries `.Index` and `.Address`). When
`ProxySettings == nil` (non-proxy / direct mode) the calls are no-ops.

The existing `activeProxyConnections` atomic is **kept unchanged** as the source
for `proxies=N` — it is cheap, works in non-proxy mode, and the registry's
`currentlyUp` count agrees with it by construction.

### Component 3: heartbeat output (`provider/main.go`, `runHealthHeartbeat`)

After the existing `[health]` line, **in proxy mode only** (proxy list non-empty),
emit:

```
[health][proxies] up=1188 down=12 never_up=3 flapped=9
[health][proxies] never_up: proxy[112] (45.3.32.184:1081), proxy[266] (104.207.45.110:1081), proxy[497] (209.50.170.87:1081)
[health][proxies] flapped: proxy[49] (209.50.167.49:1081), proxy[1037] (209.50.169.110:1081), ... (+7 more)
```

Rules:

- The **summary line is always emitted** in proxy mode, including when `down=0`
  (positive liveness confirmation, one line per ~5 min).
- A **detail line is emitted only when its bucket is non-empty.**
- Each detail list is **capped** at a constant `proxyHealthListCap` (proposed: 20)
  entries, with `... (+N more)` when truncated. Summary counts remain exact. This
  bounds log volume during a mass outage of a 1200-proxy fleet, consistent with the
  fork's log-spam discipline.

Whether the provider is in proxy mode is already known in `main.go` (the
`allProxySettings` slice from `readProxySettings()`); pass that signal into
`runHealthHeartbeat` (e.g. a `proxyMode bool` parameter).

## Data flow

```
startup: readProxySettings() --> for each: connect.RegisterProxy(idx, addr)
                                              (currentlyUp=false, everUp=false)

runH1/runH3 transport up   --> markProxyUp(idx)   (currentlyUp=true, everUp=true)
runH1/runH3 teardown defer --> markProxyDown(idx) (currentlyUp=false)
                              [same points as activeProxyConnections inc/dec]

heartbeat tick (~5m) --> ProxyHealthSnapshot() --> format + cap --> glog/stdout
```

## Error handling & edge cases

- **Non-proxy mode:** `markProxyUp/Down` are no-ops when `ProxySettings == nil`;
  the heartbeat skips the `[health][proxies]` lines entirely. No behavior change to
  the existing single-transport path.
- **Duplicate indices:** `RegisterProxy` is idempotent; re-registration keeps the
  existing entry (preserves `everUp` across any re-read of the list).
- **Concurrency:** all registry access is under `proxyHealthMu`. The snapshot is a
  read-only walk taken every ~5 min, off the hot path. The `markProxy*` calls fire
  once per transport up/down event, not per packet.
- **Startup ramp:** at the first few heartbeats most proxies are still connecting,
  so `never_up` will be high then drain. This is honest and self-correcting; the
  "trustworthy after ~1h" framing is documented rather than suppressed in code.

## Testing

- **Unit (`proxy_health_test.go`):**
  - `RegisterProxy` then `ProxyHealthSnapshot` -> all `never_up`.
  - `markProxyUp` -> moves to up; `markProxyDown` after up -> `flapped` (not
    `never_up`).
  - never `markProxyUp` then `markProxyDown` -> stays `never_up`.
  - idempotent `RegisterProxy` preserves `everUp`.
  - list cap: register > cap down proxies -> formatted list truncated with
    `(+N more)`, counts exact.
- **Live (Detroit `urtest`, ps1200 list):** deploy, wait for heartbeat, confirm
  `up + down == list size`, `never_up + flapped == down`, and that the formatted
  lines render with real indices/IPs. Re-check after >1h to confirm `never_up`
  stabilizes as the pulse retries.

## Documentation

- `LOG_REFERENCE.md`: add the `[health][proxies]` lines, the bucket definitions,
  and the pulse-cadence framing ("trustworthy after ~1h").
- `CHANGELOG.md`: `[Unreleased]` entry under Added.
- `FORK_CHANGES.md`: note the new observability surface.
