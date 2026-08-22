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
the operator has no way to know which proxies the pulse keeps failing to recover,
or whether it ever resurrects formerly-dead ones.

This feature adds the missing **visibility layer** on top of fix.11's blind retry:
a per-heartbeat report of which proxies are down (split by whether they have ever
worked) plus a record of how many the retry mechanism recovers.

## Goals

- At each `[health]` heartbeat, emit a companion report of proxies that are not
  currently live, labeled `dead` vs `degraded`.
- Make the fix.11 retry mechanism **auditable**: surface when the hourly pulse
  fires, how many proxies it recovers each interval, and a **lifetime recovery
  total** so the operator can judge both immediate and long-term effectiveness
  rather than inferring it.
- Stay **pure observability** — no change to the pulse/backoff retry logic, no
  auto-removal of proxies.
- Be safe at scale (1200+ proxies): bounded log output, no hot-path cost.

## Future work

- **Per-proxy bandwidth metrics** (tracked in issue #32). The per-proxy `proxyHealth`
  registry introduced here is the natural home for per-proxy byte counters: add
  `bytesSent` / `bytesRecv` atomics keyed by proxy index and surface them in the
  same `[health][proxies]` lines and `proxy_health.*` files. Attribution is free
  (each proxy already has its own `Client` / `RemoteUserNatProvider`); the
  measurement point (platform-transport bytes vs true relayed bytes via a
  `TransferStats(reset bool)` accessor mirroring `SecurityPolicyStats`) is the open
  design question. Deferred to its own release because byte counting is on the packet
  hot path and needs dedicated throughput validation, unlike this cold-path feature.

## Non-Goals (YAGNI)

- Surfacing per-proxy backoff/retry state (couples to `ClientStrategy` internals).
- Auto-removing dead proxies from rotation (changes runtime behavior; separate
  release).
- Per-proxy timestamps in the report (the `[net][s]select` line already carries
  `success=/error=` detail for deep dives).

## Definition of "not working"

A proxy is **down** if its platform transport is not currently live (this is the
exact inverse of the `proxies=N` count). Down proxies split into two labels via a
single per-proxy bit, `everUp`:

| Label | Condition | Meaning | Operator action |
|---|---|---|---|
| `dead` | `everUp == false` | Transport has never authenticated since process start. | Likely bad proxy/credentials — remove from list. |
| `degraded` | `everUp == true`, currently down | Authenticated at least once, down now (or parked in backoff). | Transient — may recover on next pulse. |

### Interaction with the fix.11 retry cadence

Heartbeat fires every ~5 min; the pulse fires every 60 min. Implications baked
into the framing:

- `degraded` means *"down or parked in backoff,"* **not** "actively failing this
  instant." A proxy that blipped can sit in backoff for up to an hour before the
  pulse retries it, appearing `degraded` the whole time.
- `dead` is **most trustworthy after uptime > ~1h** (one full pulse cycle).
  Before that, a never-connected proxy may simply not have been pulse-retried yet.
  A proxy that remains `dead` across multiple hourly pulses is a confirmed
  dead endpoint.
- `everUp` is **monotonic for the process lifetime** and is *not* reset by the
  pulse (which only resets `failureCount`/dialer health). So a proxy that worked
  once stays `degraded` through every pulse/backoff cycle and is never wrongly
  relabeled `dead`. The report and the retry machinery do not fight.

### Startup Ramp & Staggered Deployment

> [!WARNING]
> **False "Dead" Labels During Startup — How Staggered Deployment Works**
>
> When you deploy a large proxy list, the backend API cannot accept all authentication requests simultaneously. To prevent overwhelming the API, proxies are started with a staggered delay:
>
> **Each proxy waits `(position_in_list × 100 milliseconds)` before attempting its first connection.**
>
> Examples:
> - 1,000 proxies: startup completes in ~100 seconds
> - 5,000 proxies: startup completes in ~500 seconds (~8 minutes)
> - 10,000 proxies: startup completes in ~1000 seconds (~17 minutes)
>
> **Timeline after startup:**
> 1. **0 to completion of stagger sequence**: Proxies are being deployed in sequence; early proxies may already be connected while later ones haven't started yet
> 2. **Stagger completion to ~1 hour**: All proxies have attempted connection, but the hourly retry pulse has not yet fired. Never-connected proxies show as `dead`, but may not have received their first retry attempt yet
> 3. **1+ hours**: The hourly pulse has fired at least once for all proxies. A proxy still labeled `dead` across multiple pulse cycles is a confirmed problem (bad credentials, unreachable address, etc.)
>
> **Key guarantee:** Once a proxy successfully authenticates (marked `everUp=true`), it is permanently marked and can only be `degraded` (not `dead`) if it drops later. This prevents false relabeling while the retry mechanism works.
>
> **Bottom line:** Do not remove or investigate `dead` proxies in the first ~1 hour of startup. Wait for the hourly pulse to give them a fair chance.

## Architecture

### Component 1: per-proxy registry (`proxy_health.go`, package `connect`)

A new file holding a process-global registry:

```
type proxyHealth struct {
    address     string
    currentlyUp bool
    everUp      bool
    downSince   time.Time  // when currentlyUp last went false (for recovery latency)
    lastSeenUp  bool       // currentlyUp as of the previous heartbeat (baseline)
    deadLogged  bool       // a confirmed-dead event has been written for this proxy
}

type proxyEvent struct {
    index   int
    address string
    after   time.Duration  // for recovered events: how long it was down
}

// ProxyHealthReport is the full per-heartbeat result: current state, the
// transitions since the last heartbeat, and lifetime counters.
type ProxyHealthReport struct {
    Up       int
    Dead     []string  // formatted "proxy[idx] (addr)", index-sorted
    Degraded []string

    Recovered     []proxyEvent  // down->up since last heartbeat (incl. first-ever connect)
    NewlyDegraded []proxyEvent  // up->down since last heartbeat
    NewlyDead     []proxyEvent  // never-up proxies newly confirmed dead, logged once
                                // (gated behind a confirm delay so startup ramp is
                                //  not logged as dead)

    LifetimeRecovered int
    LifetimeLost      int
}

var (
    proxyHealthMu sync.Mutex
    proxyHealthByIndex map[int]*proxyHealth

    // lifetime transition counters (since process start)
    lifetimeRecovered int  // down->up events
    lifetimeLost      int  // up->down events
    baselineSet       bool
)
```

Exported API:

- `RegisterProxy(index int, address string)` — idempotent; creates an entry with
  `currentlyUp=false, everUp=false` if absent. Called eagerly at startup for every
  proxy in the list.
- `markProxyUp(index int)` — sets `currentlyUp=true, everUp=true`.
- `markProxyDown(index int)` — sets `currentlyUp=false`; stamps `downSince=now` if
  it was previously up.
- `ProxyHealthSnapshot() (up int, dead []string, degraded []string)` — **read-only**
  walk; returns the current up count plus the two formatted lists
  (`proxy[idx] (address)` strings, index-sorted). Does **not** advance the
  baseline, so it is safe to call from the pulse-fire marker.
- `ProxyHealthHeartbeat(confirmDead bool) ProxyHealthReport` — the single
  per-heartbeat call. Walks the registry once: builds the current `Dead`/`Degraded`
  lists; compares each entry's `currentlyUp` against `lastSeenUp` to populate
  `Recovered` (down->up, with latency from `downSince` when known) and
  `NewlyDegraded` (up->down), folding those into the lifetime counters; then advances
  the baseline (`lastSeenUp = currentlyUp` for all). **Called exactly once per
  heartbeat** so interval deltas mean "since the last heartbeat." On the first call
  it sets the baseline and returns empty `Recovered`/`NewlyDegraded` lists / zero
  counters, to avoid reporting the entire startup ramp as transitions.
  `NewlyDead` is populated only when `confirmDead` is true (the caller passes
  `uptime >= deadConfirmDelay`, ~65 min, just past one pulse cycle): each never-up
  proxy not yet logged is emitted once and flagged, so the durable event log records
  confirmed-dead endpoints without flagging the startup ramp. `lost` counts only
  up->down transitions, not dead confirmations.

Eager registration is required so that a proxy which **never connects at all**
(never reaching `markProxyUp`) still shows up as `dead`. Lazy registration would
silently omit exactly the proxies the feature exists to surface.

**Recovery-event semantics:** `lifetimeRecovered` counts down->up *transitions*
over the process lifetime, not distinct proxies. A chronically flapping proxy that
cycles down->up repeatedly contributes once per recovery. This is intended — the
counter measures how much work the retry machinery is doing, and a large
`lifetime_recovered` relative to the proxy count is itself a signal of an unstable
fleet.

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
[health][proxies] up=1193 down=7 dead=4 degraded=3 recovered=5 lost=0 lifetime_recovered=51 lifetime_lost=39
[health][proxies] dead: proxy[112] (45.3.32.184:1081), proxy[266] (104.207.45.110:1081), proxy[497] (209.50.170.87:1081), proxy[902] (12.34.56.78:1081)
[health][proxies] degraded: proxy[49] (209.50.167.49:1081), proxy[1037] (209.50.169.110:1081), proxy[660] (98.76.54.32:1081)
```

Summary line fields:

- `up` / `down` — current state (`up` agrees with the `proxies=N` count by
  construction; `up + down == list size`).
- `dead` / `degraded` — the two down labels (`dead + degraded == down`).
- `recovered` / `lost` — down->up and up->down transitions **since the last
  heartbeat** (`len(Recovered)` / `len(NewlyDead)+len(NewlyDegraded)` from
  `ProxyHealthHeartbeat`). `recovered` spiking right after the hourly pulse is the
  direct evidence the retry worked.
- `lifetime_recovered` / `lifetime_lost` — cumulative transition counts since
  process start, for judging long-term retry effectiveness.

Rules:

- The **summary line is always emitted** in proxy mode, including when `down=0`
  (positive liveness confirmation, one line per ~5 min).
- A **detail line is emitted only when its label has members.**
- Each detail list is **capped** at a constant `proxyHealthListCap` (50) entries,
  with `... (+N more)` when truncated. Summary counts remain exact. This bounds log
  volume during a mass outage of a 1200-proxy fleet, consistent with the fork's
  log-spam discipline. Example when truncated:
  `[health][proxies] dead: proxy[3] (1.2.3.4:1081), ... (+217 more)`
  **This cap applies to the stdout/combined-log lines only.** The persistent files
  (Component 5) are never truncated and never show `(+N more)` — they always carry
  the complete dead/degraded list.
- On the **first** heartbeat the transition fields read
  `recovered=0 lost=0 lifetime_recovered=0 lifetime_lost=0` (baseline
  establishment), then populate from the second heartbeat onward.

Whether the provider is in proxy mode is already known in `main.go` (the
`allProxySettings` slice from `readProxySettings()`); pass that signal into
`runHealthHeartbeat` (e.g. a `proxyMode bool` parameter).

### Component 4: pulse-fire marker (`provider/main.go`, hourly pulse goroutine)

The existing hourly goroutine (main.go:854-863) calls `connect.TriggerPulse()`.
Wrap that call so it logs the pre-pulse state each time it fires:

```
[pulse] waking stalled transports: down=12 dead=3 degraded=9
```

Implementation notes:

- The log line is built from a `ProxyHealthSnapshot()` read taken just before
  `TriggerPulse()`. Because the snapshot is read-only it does not disturb the
  heartbeat's transition baseline.
- The line lives in `main.go` (operator-facing logging), keeping `pulse.go` a pure
  wakeup primitive with no logging dependency.
- One line per hour — negligible volume. Recovery shows up afterward as `recovered`
  climbing in the next heartbeat(s).
- Emitted only in proxy mode (skip when there are no registered proxies).

### Component 5: persistent records (`provider/main.go`, written from the heartbeat)

RAMLOGS write to volatile `/dev/shm`, which rotates and is lost on restart, so the
dead/degraded picture cannot be checked retroactively. This component mirrors the
state to the host-backed config volume (`/root/.urnetwork` in Docker).

Two files, both written from the heartbeat tick (so they inherit
`URNETWORK_HEALTH_INTERVAL`):

**(a) Current-state snapshot — `proxy_health.state`, rewritten atomically each
heartbeat.** Answers "what is broken right now?" at any time without watching live.

```
# updated 2026-06-02T16:05:11Z  up=1193 down=7 dead=4 degraded=3
# lifetime_recovered=51 lifetime_lost=39
DEAD     proxy[112] (45.3.32.184:1081)
DEAD     proxy[266] (104.207.45.110:1081)
DEAD     proxy[497] (209.50.170.87:1081)
DEAD     proxy[902] (12.34.56.78:1081)
DEGRADED proxy[49] (209.50.167.49:1081)
DEGRADED proxy[660] (98.76.54.32:1081)
DEGRADED proxy[1037] (209.50.169.110:1081)
```
Written via temp-file + rename so a reader never sees a half-written file. **Never
truncated and no `(+N more)`** — the 50-entry display cap is a stdout-only concern;
this file always lists *every* dead and degraded proxy (size is bounded by the
proxy count). The whole point is a complete, durable record.

**(b) Durable event log — `proxy_health.log`, append-only.** Answers "which proxies
have been problematic, and when?" One line per transition, sourced from
`ProxyHealthHeartbeat`'s `NewlyDead` / `NewlyDegraded` / `Recovered` lists:

```
2026-06-02T15:10:03Z DEAD      proxy[112] (45.3.32.184:1081)
2026-06-02T15:10:03Z DEGRADED  proxy[49] (209.50.167.49:1081)
2026-06-02T16:05:11Z RECOVERED proxy[49] (209.50.167.49:1081) after=55m8s
```
Every transition is written in full — the event log is never list-truncated and
never shows `(+N more)`. Rotation: when the file exceeds **20 MB**, rename it to
`proxy_health.log.1` (replacing any previous `.1`) and start fresh — one generation
retained. (Rotation only ages out old history; it never drops entries from a current
write.)

Mechanics:

- Location is `URNETWORK_PROXY_HEALTH_DIR`, defaulting to the provider config dir
  (`/root/.urnetwork`). Both files live there.
- If the directory is unwritable, log **one** warning to stdout and disable
  persistence for the rest of the run. A log-file problem must never crash or stall
  the provider.
- All writes happen on the heartbeat goroutine, off the packet hot path.
- `after=` on `RECOVERED` is `now - downSince`, accurate to heartbeat granularity on
  the recovery side (the down side is stamped at the exact teardown moment).

### Component 6: access commands (host + Docker)

Operators need a one-step way to read the persistent record. There is no `follow`
flag — `-f` already means *force* elsewhere in urnet-tools (`update -f`,
`proxy add ... -f`), so overloading it would conflict. The single command both
prints the current state and then tails the event log automatically.

**Host / systemd install — `urnet-tools proxy health`**
Add a `health` case to the existing `do_proxy()` dispatcher
(`scripts/Provider_Install_Linux.sh`, alongside `add` / `clear`):

1. print the current-state file (`proxy_health.state`),
2. then `tail -f` the event log (`proxy_health.log`) so new
   dead/degraded/recovered events stream live until the operator quits.

Path resolution mirrors `URNETWORK_PROXY_HEALTH_DIR`, defaulting to the provider
config dir. Update the `usage()` help text to list `proxy health`.

**Docker — `proxy-health` helper baked into the image**
`urnet-tools` is the host installer/manager and is not present in the container, so
Docker needs its own entry point. Add a tiny `docker/scripts/proxy-health.sh`,
installed to `/usr/local/bin/proxy-health` in the image, that does the same
print-state-then-`tail -f`-log against `/root/.urnetwork/`. Operators run:

```
docker exec -it <container> proxy-health
```

A no-tooling fallback is also documented for anyone who prefers raw access:

```
docker exec -it <container> sh -c 'cat /root/.urnetwork/proxy_health.state; echo; tail -f /root/.urnetwork/proxy_health.log'
```

**RAMLOGS independence (the key property):** the `proxy_health.*` files live on disk
unconditionally (config volume), so both `urnet-tools proxy health` and
`proxy-health` work identically whether RAMLOGS is on or off. This is deliberate —
it is exactly the retroactive-access gap RAMLOGS creates. Only *live-tailing the
main combined log* differs by RAMLOGS, which the docs spell out (see Documentation).

## Data flow

```
startup: readProxySettings() --> for each: connect.RegisterProxy(idx, addr)
                                              (currentlyUp=false, everUp=false)

runH1/runH3 transport up   --> markProxyUp(idx)   (currentlyUp=true, everUp=true)
runH1/runH3 teardown defer --> markProxyDown(idx) (currentlyUp=false)
                              [same points as activeProxyConnections inc/dec]

heartbeat tick (URNETWORK_HEALTH_INTERVAL, default 5m)
    --> ProxyHealthHeartbeat()  (current lists + transitions + lifetime,
                                 advances baseline; called exactly once)
    --> stdout:  [health][proxies] summary + capped dead/degraded detail lines
    --> file:    proxy_health.state  (rewrite, atomic, uncapped)
    --> file:    proxy_health.log    (append NewlyDead/NewlyDegraded/Recovered;
                                      rotate at 20 MB)

pulse tick (~60m) --> ProxyHealthSnapshot() (read-only) --> log [pulse] line
                  --> connect.TriggerPulse()
```

## Error handling & edge cases

- **Non-proxy mode:** `markProxyUp/Down` are no-ops when `ProxySettings == nil`;
  the heartbeat skips the `[health][proxies]` lines entirely. No behavior change to
  the existing single-transport path.
- **Duplicate indices:** `RegisterProxy` is idempotent; re-registration keeps the
  existing entry (preserves `everUp` across any re-read of the list).
- **Concurrency:** all registry access is under `proxyHealthMu`. The snapshot and
  transition walks run every ~5 min, off the hot path. The `markProxy*` calls fire
  once per transport up/down event, not per packet.
- **Single transitions caller:** `ProxyHealthTransitions()` advances the baseline,
  so it must be called from exactly one site (the heartbeat). The pulse marker uses
  the read-only `ProxyHealthSnapshot()` to avoid double-counting transitions.
- **Startup ramp:** at the first few heartbeats most proxies are still connecting,
  so `dead` will be high then drain. This is honest and self-correcting; the
  "trustworthy after ~1h" framing is documented rather than suppressed in code.

## Testing

- **Unit (`proxy_health_test.go`):**
  - `RegisterProxy` then `ProxyHealthSnapshot` -> all `dead`.
  - `markProxyUp` -> moves to up; `markProxyDown` after up -> `degraded` (not
    `dead`).
  - never `markProxyUp` then `markProxyDown` -> stays `dead`.
  - idempotent `RegisterProxy` preserves `everUp`.
  - list cap: register > 50 down proxies -> formatted list truncated with
    `(+N more)`, counts exact.
  - transitions: first `ProxyHealthTransitions` call returns all zeros and sets the
    baseline; after a `markProxyUp` the next call returns `recovered=1` and
    `lifetimeRecovered=1`; a subsequent `markProxyDown` returns `lost=1`,
    `lifetimeLost=1`, `lifetimeRecovered` unchanged.
  - flapping: up->down->up across calls increments `lifetimeRecovered` twice (event
    semantics).
  - `ProxyHealthSnapshot` does not advance the baseline: two snapshots with no
    `mark*` in between, followed by a transitions call, still report the real delta.
- **Live (Detroit `urtest`, ps1200 list):** deploy, wait for heartbeat, confirm
  `up + down == list size`, `dead + degraded == down`, and that the formatted
  lines render with real indices/IPs. Watch across an hourly pulse: confirm the
  `[pulse]` line fires, then `recovered` climbs in the following heartbeat and
  `lifetime_recovered` accumulates. Re-check after >1h to confirm `dead`
  stabilizes.

## Documentation

- `LOG_REFERENCE.md`: add the `[health][proxies]` lines (including the
  `recovered` / `lost` / `lifetime_recovered` / `lifetime_lost` fields), the
  `[pulse]` marker, the `dead` / `degraded` label definitions, the persistent
  files (`proxy_health.state` / `proxy_health.log`), and the pulse-cadence framing
  ("trustworthy after ~1h").
- **The `proxy health` / `proxy-health` commands must be documented everywhere the
  other ops commands are**, so operators discover them:
  - `README.md` — add to the command/usage overview alongside `urnet-tools logs`,
    `proxy add`, etc.
  - `docs/Configuration.md` and `docs/Docker-Deployment.md` — the full "Viewing
    proxy health" subsection below.
  - `scripts/Provider_Install_Linux.sh` `usage()` help text — list `proxy health`.
  - GitHub **wiki** — the `docs/*.md` pages mirror wiki pages; update the
    corresponding wiki page(s) (Configuration, Docker Deployment, and any
    Commands/Logging page) to match.
  - `LOG_REFERENCE.md` and `FORK_CHANGES.md` as noted above.
- **Viewing proxy health (new docs subsection):**
  - Persistent record (RAMLOGS-independent, always works):
    - Host: `urnet-tools proxy health`
    - Docker: `docker exec -it <container> proxy-health`
  - Live-tailing the combined log **does** depend on RAMLOGS:
    - RAMLOGS on (logs in RAM at `/dev/shm/urnetwork.log`):
      `docker exec -it <c> sh -c "tail -f /dev/shm/urnetwork.log | grep -E '\[health\]\[proxies\]|\[pulse\]'"`
    - RAMLOGS off (logs to container stdout / journald):
      `docker logs -f <c> 2>&1 | grep -E '\[health\]\[proxies\]|\[pulse\]'`
  - Spell out the distinction: the `proxy_health.*` files are the durable record and
    are read the same way regardless of RAMLOGS; only the live combined-log stream
    changes location when RAMLOGS is enabled.
- `CHANGELOG.md`: `[Unreleased]` entry under Added.
- `FORK_CHANGES.md`: note the new observability surface and the access commands.
- `Dockerfile`: install `docker/scripts/proxy-health.sh` to
  `/usr/local/bin/proxy-health` (the existing `COPY docker/scripts/*.sh /app/` +
  `chmod` step covers it; add a symlink or copy into `PATH`).
