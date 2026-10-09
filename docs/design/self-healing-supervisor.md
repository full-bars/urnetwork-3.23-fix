# Unified Self-Healing Supervisor: design

## 1. Why

The provider has accumulated several healing-ish systems that each work in isolation but
share no brain, no ledger, and no status surface. When a canary box wedged for 13h (a
proxy reload stuck holding its in-process mutex), every one of them either did nothing
or could not see the problem:

| System | Trigger | Actuator | Gap |
|---|---|---|---|
| Pressure self-heal (`resource_pressure.go`) | one composite score: PSI mem/cpu/io `some avg60`, MemAvailable, load/core, goroutines (per-proxy), heap fraction of soft limit, FD fraction; worst component wins, asymmetric EWMA, emergency pins | URL pacing 1x-8x, probe concurrency down to 1, cleanup+reaper cadence 6h->1h, AIMD pool sizing, adaptive GC (single owner) | does not drive audit; cannot restart anything; blind to swap I/O and to a wedged component |
| Proxy audit | grade <= 0.4 on two consecutive passes | parks junk proxies (reversible, backoff 6h->7d) | silently depends on hot-restart being armed; observe-only otherwise |
| Cleanup job + reaper | cadence 6h (`cleanup-interval`; `cleanup-scope` selects what it cleans: none/url/all) | sweeps dead proxies, refreshes stale grades | separate loop, no shared budget |
| Trim cap + OOM cap | manual trim; kernel OOM kill | caps the pool ("tighter wins") | two cap systems reconciling by min(); no relation to the pressure score |
| Hot-restart / HotSwap | explicit update, or manual | zero-downtime restart (SIGUSR2, NOTIFY_SOCKET-gated, 30s drain) | nothing else coordinates with it; audit depends on it |
| Unit-type convergence | every update | rewrites unit Type= to match the binary (config-drift self-heal) | one more independent reconciler |
| Reload watchdog (in flight) | reload stuck past a bound | escalation into hot-restart machinery (zero-downtime hot-swap, gated on `hotRestartEnabled()`, default on) | risks becoming another silo unless folded in |
| Thrash responder (phase 2a, shipped) | swap thrash (PSI full + pswpout + refaults) | freeze growth -> escape+remember (it sheds nothing itself; rung d owns shedding) | another loop unless folded in |
| Backend outage watcher | backend degraded probes | logs + webhook (observer) | observer only; fine as-is |

Since this doc was written, phase 2a shipped: the thrash watchdog merged together with the readable-log pass, using this doc's state names and the shared action ledger.

Loop architecture reality check: the pressure monitor, the thrash watchdog, and the reload watchdog initially ran three independent tickers. The pressure monitor and thrash watchdog share /proc and cgroup sources. The reload watchdog only reads in-memory atomics. Collapsing pressure and thrash into one supervisor loop unifies memory sensing and escalation. However, the reload-wedge detector must stay in its own supervised goroutine. Folding reload detection into the memory sampling loop would put stuck-reload recovery in the same failure domain as slow disk, flock stalls, or sensor panics.

Gate reality check: the reload watchdog's escalation fires whenever hot-restart is armed (default on), independent of the self-heal switch. The fold-in keeps these gate domains distinct.

## 2. Principles

1. Pure decision step and isolated reload watchdog. The supervisor decision engine does no I/O. Sensors run in a dedicated goroutine and publish timestamped immutable snapshots. The reload-wedge detector stays its own supervised goroutine. It checks in-memory atomics only. This isolates reload stall detection from sensor panics, disk stalls, or blocking lock operations.
2. Lock freedom and two-way mutual exclusion. The decision loop never takes the reload mutex, the proxy lock, or any lock a wedge can hold. Status file, critLog, and ledger writes are asynchronous and bounded by timeouts so disk stalls never block decision loops. Two-way mutual exclusion protects hot-swap (rung e) and cold escape (rung f): rung e holds `hotSwapLock` throughout handoff, and rung f holds `hotSwapLock` until process exit. If `hotSwapLock` is already held, duplicate triggers return a distinct error instead of nil. The core loop never iterates proxies: proxy operations remain O(1) in the supervisor core.
3. IP-preserving by default. More running proxies means more earnings. Rungs a to c2 lose zero identities. Shedding (rung d) is bounded, floored, worst-first, and reversible. Shedding requires two independent signals rather than acting on a composite pressure score alone.
4. Durable bounded accounting. Every action is recorded in the ledger with reason and outcome. Every action is bounded by global budgets and hysteresis. Rung e has its own persisted daily budget file and backoff schedule, preventing infinite hot-swap churn.
5. Gate separations and observed versus actuating split. Off means actuating score is zero. The supervisor continuously computes an observed score for status and telemetry. When self-heal is disabled, the actuating score published to consumers is strictly 0. Consumers read only the actuating score. Process-wedge defense remains independent: reload-watchdog escalation is gated on `hotRestartEnabled()` (default on).
6. Fail-neutral and fail-open failure policy. If sensor streams stop or become stale, state decays to unknown. Costly rungs (shed, park, restart) stop at once; cheap reversible rungs (freeze growth, slow probes, tighter GC) hold for a bounded time and then lift (tiered fail-open). If the supervisor loop crashes, recovery hooks reset actuators to neutral (actuating score 0, memory budget full, baseline GOGC, thrashFreeze false). A stall delta above 1.05 is flagged implausible: the shipped tracker still clamps the stored fraction to 1.0 (pinned by `TestThrashTrackerClampsFraction`), and the flag makes the machine take a neutral tick instead of counting the sample toward severe thrash. A restart never rests on one sensor: rung f needs two independent signals to agree, and its persisted cap bounds any restart loop.
7. Unified clock seam. All supervisor loops, state machines, and backoff timers use one mockable clock seam (`nowFn`).

## 3. Sensors and signals

The supervisor unifies memory and host pressure sensing while keeping stuck-reload detection isolated.

### 3.1 Signal table

| Signal | Source | Cadence | Owner | Behavior when absent |
|---|---|---|---|---|
| Host memory pressure (`some avg60`) | `/proc/pressure/memory` | 30s | Pressure monitor | Component omitted from composite score; fallback to memory headroom |
| Cgroup memory pressure (`full total` deltas) | Unit cgroup `memory.pressure` cumulative `total=` microseconds | 30s | Thrash detector | Neutral tick; blind for 90s: state decays to unknown, costly rungs stop, cheap rungs hold up to 10m (tiered fail-open) |
| Swap activity | `/proc/vmstat` (`pswpin`/`pswpout`) and unit `memory.stat` (`pswpout`, `workingset_refault_anon`) | 30s | Thrash detector | Absent: host `MemAvailable` below the headroom low threshold becomes the second signal for rung f. With neither, rung f does not fire. (The shipped code accepts a stricter PSI-only bar; the supervisor does not for restarts.) |
| Live heap fraction | Go runtime `ReadMemStats` vs soft limit | 10s GC subtick, 30s main tick | GC governor | Maintain baseline GOGC; do not tighten |
| Proxy pool size | In-memory atomic `lastRunningProxyCount` | 5m pool controller, on demand | Pool controller, Thrash cap | Fall back to configured floor |
| Reload duration | In-process atomic timestamp `reloadStartedAt` | 30s | Reload watchdog goroutine | No action; reload treated as healthy |
| Memory headroom | Host `MemAvailable` vs RAM low threshold | 30s | Headroom watcher | Fail-open; do not shed proxies |

### 3.2 Observed versus actuating score split

Merging sensing into the supervisor must not break the rule that self-heal off means off. When self-heal is disabled, operators still require live status and telemetry, but actuators must receive zero:

- Observed score: continuously computed from live sensors. Written to `pressure_status`, metrics, and CLI status displays. Reflects true host conditions even when self-heal is off.
- Actuating score: published to internal consumers. When self-heal is enabled, actuating equals observed. When self-heal is disabled, actuating is clamped strictly to 0.0.
- Consumers: `proxy_url_source.go` (URL fetch pacing), `proxy_probe.go` (probe concurrency), `proxy_table_probe.go` (table probe concurrency), `proxy_grade_paid.go` (paid probe concurrency), and the pool controller (AIMD sizing), which reads both the score and `currentPressureNoCPU()` (the score without its CPU component, so a CPU stall cannot block pool growth). All consumers read only the actuating score, and the split applies to both reads.

### 3.3 Sensor isolation and pure decision step

Reading /proc, cgroup files, and runtime memory stats takes file system and kernel locks. If the disk stalls or a cgroup path hangs, the supervisor loop must not stall reload detection.

Sensors run in a dedicated polling goroutine. They publish timestamped, immutable snapshots. The supervisor decision step runs purely in memory against the latest snapshot. It executes zero I/O and acquires no file or mutex locks.

Status writes (`pressure_status`), critical events logging (`critLog`), and ledger writes (`ledgerRecord`) run asynchronously with strict timeouts (5s persist timeout, 2s ledger timeout). The decision step never waits synchronously on flock or fsync.

### 3.4 Sensor staleness decay

If the sensor goroutine crashes, hangs, or stops producing snapshots, the supervisor must not freeze on old state. In the shipped thrash machine, a missing PSI reading causes an early return that holds existing state indefinitely. If the state was `thrashing`, `thrashFreeze` remains true forever.

The supervisor enforces a staleness bound of 90 seconds (3 sample ticks). If no fresh snapshot arrives within 90 seconds:
- The supervisor decays state to `unknown` and logs a single warning line.
- Costly rungs stop immediately: park, recycle, shed, and both restart rungs (e and f). Without data there is no justification for losing an IP or restarting.
- Cheap reversible rungs (freeze growth, slow probes, tighter GC) keep holding for up to 10 more minutes (`sensorBlindCheapHold`), then lift. This avoids both a stuck brake and dropping all protection the moment one sensor blinks.
- `thrashFreeze` follows the cheap-rung timer, so it can no longer stay true forever.

### 3.5 Plausibility checking and corroboration

Corrupt counter reads or kernel counter wraps can produce impossible delta fractions. Clamping these values to 1.0 turns sensor corruption into a severe thrash condition. Severe thrash restarts the node after 90 seconds. A sensor bug could restart the entire opted-in fleet.

The supervisor implements three safeguards instead of a post-deploy grace period:
- Plausibility threshold: any raw delta fraction above 1.05 is treated as sensor corruption. The tracker still clamps the stored fraction to 1.0, as it does today, but it also reports the sample as implausible, and the machine executes a neutral tick instead of declaring severe thrash. The clamp and the flag are separate outputs, so the existing clamp test keeps passing.
- Corroboration: rung f (exit 75) requires two independent signals to agree: severe memory stall (PSI full) AND either a sustained swap-out or refault rate, or host `MemAvailable` below the headroom low threshold. One misreading sensor cannot trigger a restart.
- Persisted cap: the restart counter (`thrash_cap.json`) survives restarts, so a restart loop is bounded to 3 per rolling 24 hours with backoff regardless of cause. After the cap, the supervisor alerts instead of restarting.

Thrash detection already requires sustained stall (3 minutes, or 90 seconds if severe), which covers the normal startup memory ramp, so no separate startup grace window is needed.

### 3.6 Single clock seam

Different components historically read different clocks (`time.Now()`, `thrashNowFn`, `gcNow`). The supervisor unifies all timers, sustain clocks, and backoff calculations on one mockable clock seam (`nowFn`). Parity tests inject deterministic time steps.

### 3.7 Platforms

The pressure monitor and reload watchdog run on Linux, Windows, and macOS (memory notification objects on Windows, `kern.memorystatus_vm_pressure_level` on macOS). Thrash detection (/proc and cgroup) is Linux-only. The supervisor uses a portable core with OS-specific sensor drivers (the `thrash_watchdog_stub.go` pattern). `thrashing` and `critical` are Linux states. `under-pressure`, `calm`, and `unknown` are portable. FreeBSD is a supported platform with its own pressure sensor (`mempressure_freebsd.go`), so the portable core needs a stub for it as well, and it runs under rc.d with no restart policy for exit 75, so rung f is disabled there as on Windows until a marker and a policy exist.

## 4. State and escalation ladder (ordered by "who loses an IP")

States: calm -> under-pressure -> thrashing -> critical -> unknown (hysteresis: rise fast, relax only after sustained calm).

### 4.1 Ladder rungs

- a. FREEZE (zero IP loss): pause admissions and URL probing; stop pool growth. Engaged from under-pressure upward.
- b. SHRINK OURSELVES (zero IP loss): URL pacing, probe concurrency down to 1, adaptive GC tightening, and background work pauses. The single GC governor retains its 10s subtick to respond rapidly to heap spikes. Narrowing H3 carriers remains an experimental candidate for future memory relief.
- c. PARK (zero IP loss, reversible): proxy audit park action (worst-first, backoff ladder, 24h budget). Moves under the supervisor. Detection requires `proxy_audit` enabled. Acting requires both `proxy_audit` and `self_heal` enabled. This decouples parking from `hotRestartEnabled()`.
- c2. RECYCLE (brief blip, IP preserved): restart the one subsystem attributed as the runaway, without restarting the process. Each subsystem registers a `Recycle()` that cancels its own context and rebuilds it, plus a `Cost()` estimate. Identities served by that subsystem blip briefly and keep their IPs; everything else is untouched. First candidate: the H3 carriers (confirmed memory hog). The reload goroutine is not a candidate: it wedges waiting on a `sync.Mutex` (`r.mu` in `proxy_reload.go`), and a goroutine blocked on a mutex cannot be cancelled, so a wedged reload goes straight to the hot-swap handoff (rung e) while memory is healthy. A subsystem may register only if it owns a cancellable context and releases its goroutines on cancel (Go cannot kill a goroutine). After a recycle the supervisor checks, within `recycleVerifyWindow`, that the offending metric fell by at least `recycleMinImprovement`. If not, it records a miss and escalates to the next rung that applies to the cause: shed (d) when the cause is memory pressure, or the hot-swap handoff (e, subject to its veto and budget) when the cause is a wedged component. It never recycles the same subsystem more than `recycleMaxPerHour` times. Recycle does not shrink a heap that has already grown; only a new process returns that memory to the OS, so a new process is the backstop: rung e (hot-swap) while the node is below `thrashing`, and rung f (cold escape) once it is thrashing or critical, because rung e is vetoed there (see below).
- d. SHED (bounded IP loss): last resort under sustained heavy pressure. Smallest step that relieves, worst-first (dead, degraded, lowest earnings), with a floor (never below configured minimum), hysteresis, and reversible re-admission. Trim cap and OOM cap become budget inputs to this rung.
  - Two independent signals rule: shedding loses earning identities and must not rely on the composite pressure score alone. The composite score can read 1.00 on a healthy node or 0.00 during swap storms. Shedding requires a concrete signal (host `MemAvailable` below headroom low threshold) PLUS a second independent confirming signal (elevated PSI full or sustained refault and swap rate).
- e. HOT-SWAP HANDOFF (zero-downtime): starts a fresh copy of the whole process, hands live connections to it, and retires the old one. It is a whole-process handoff, not a component restart (that is rung c2). Calls the hot-swap machinery (`runHotSwapParentHandoff`, 30s connection drain).
  - Thrash veto rule: the hot-swap handoff requires thrash state strictly below `thrashing`. Spawning a child process beside a heavily swapped parent causes memory exhaustion. If the node is thrashing or critical, rung e is vetoed. If vetoed while self-heal is off, an alert is logged.
  - Persisted daily budget with backoff: only supervisor-initiated handoffs count against it. An operator-driven `urnet-tools update` hot-swap is never refused by the budget, but it is written to the ledger like every other action. Rung e has its own persisted budget file (`hotswap_cap.json`), mirroring `thrash_cap.json`. It permits at most 3 restarts per rolling 24 hours with escalating backoff (30m after first, 2h after second). Every attempt is written to the action ledger. If the budget is exhausted, an alert is logged. Rung f takes over only when memory thrashing is confirmed by two signals (see 3.5); otherwise an alert is logged.
  - Two-way mutual exclusion: rung e holds `hotSwapLock` throughout handoff. Duplicate triggers return a distinct error rather than nil, ensuring callers never log a false success. Rung f cannot start while rung e holds `hotSwapLock`.
- f. ESCAPE (cold restart exit 75): cold escape for severe memory thrashing. The provider exits with status 75, and the service supervisor restarts it.
  - Requires thrash state `thrashing` or `critical`, confirmed by two independent signals (see 3.5).
  - Holds `hotSwapLock` until process exit, ensuring no concurrent hot-swap can start during persist or ledger writes.
  - Never waits for the lock: it acquires `hotSwapLock` without blocking (`TryLock`). If rung e holds it, a handoff is in flight, and that handoff is itself bounded (20s pre-flight, 60s acknowledgement, 30s drain). Rung f then logs a `hotswap` alert, waits one `thrashRetryInterval` and tries again, as the shipped escalation already does when it sees a draining or mid-handoff process. The decision loop never blocks on the lock.
  - Persisted daily budget (`thrash_cap.json`): hard ceiling of 3 escapes per rolling 24 hours, backoff schedule of 30m then 2h (a 6h step is defined in `thrash_cap.go` but unreachable at a cap of 3).

### 4.2 Init system detection and policy

The supervisor detects the active init system and adapts restart policies accordingly:

| Init system | Detection | Rung e (hot-swap) policy | Rung f (exit 75) policy |
|---|---|---|---|
| systemd | `INVOCATION_ID != ""` or `NOTIFY_SOCKET != ""` | Fully supported via sd_notify MainPID handoff | Fully supported; exits 75; service restarts under `Restart=on-failure` or `Restart=always` |
| OpenRC | Today nothing marks the process: `INVOCATION_ID`, `NOTIFY_SOCKET` and `RC_SVCNAME` are all empty in a supervise-daemon child (tested on Alpine). The init script can `export` a marker such as `URNETWORK_INIT=openrc`, which the child does see | Unavailable: supervise-daemon has no sd_notify MainPID handoff (stated in the installer) | Inert today for lack of a marker. With a marker it works mechanically: supervise-daemon respawns on ANY exit including 0 and 75, up to `respawn_max=10` per `respawn_period=3600`, after which the service silently stays `stopped` |
| Docker | `/.dockerenv` exists or `URNETWORK_CONTAINER=1` | To confirm: hot-swap inside a container | Opt-in: a container counts as a supervisor only when `URNETWORK_EXIT75_OK=1` is set and the state directory holding `thrash_cap.json` can be written and read back (otherwise the escalation is refused as `no-supervisor` or `persist-failed`). The start scripts treat exit 75 as a planned restart: a 5 second restart that does not count toward the three-crash JWT clear |
| Windows | `runtime.GOOS == "windows"` | Supported via named-pipe handoff | Disabled; rung f is Linux-only; Task Scheduler handles auto-start on logon |

Notes on OpenRC and Docker:
- OpenRC: the shipped OpenRC service file configures `supervise-daemon` with `respawn_max=10` per 3600s. It respawns the binary on any exit; exit codes 0, 1 and 75 were measured to behave identically. The shipped check in `thrash_watchdog.go` checked only `INVOCATION_ID` and `NOTIFY_SOCKET`, making rung f inert on OpenRC. The supervisor explicitly supports OpenRC service detection.
- Docker: sensing works (the watchdog reads the container's own cgroup pressure on cgroup v2 hosts). The exit-75 escape is an opt-in as of the container exit-75 change: the gate recognizes a container (`/.dockerenv` or `URNETWORK_CONTAINER=1`) only with `URNETWORK_EXIT75_OK=1` and a writable state directory, and `start_stable.sh`, `start_nightly.sh`, `start_jwt.sh` and the provider loop of `pelican_panel.sh` treat exit 75 as a planned restart. The image default stays unset, so nothing changes until an operator opts in. The same scripts exist in meso-miner and sn.

### 4.3 Failure policy

When the supervisor loop panics or terminates unexpectedly, it must fail neutral and fail open. A sensor that merely goes quiet is not a loop failure: it follows the tiered decay in 3.4 (costly rungs stop at once, cheap rungs hold up to `sensorBlindCheapHold`).
- Actuating score is immediately reset to 0.0.
- Connection memory budget is reset to full (0).
- Adaptive GC governor is reset to baseline GOGC (for example 100), releasing GC tightening.
- `thrashFreeze` is cleared to false, allowing pool growth.
- Both status files (`pressure_status` and `thrash_status`) are updated with neutral or unknown state.
- Loop supervision restarts the loop with backoff (1s base, 5m max).
- After a loop crash all ladder rungs fail open: the supervisor never leaves a node pinned in a degraded, parked, or shed state while its own loop is down.

### 4.4 Constants and tunables

| Parameter | Value | Scope | Description |
|---|---|---|---|
| `pressureSampleInterval` | 30s | Pressure sensing | Main pressure sampling cadence |
| `thrashSampleInterval` | 30s | Thrash sensing | Thrash counter delta cadence |
| `thrashSustain` | 3m | Escalation | Sustained mild thrash required for thrashing state |
| `thrashSevereSustain` | 90s | Escalation | Sustained severe thrash (>= 25% full, `thrashSevereFrac`) for thrashing state |
| `thrashCalmRelax` | 5m | State recovery | Sustained calm required to step down one state |
| `thrashCriticalAfter` | 15m | Escalation | Continuous thrashing before declaring critical state |
| `thrashRetryInterval` | 10m | Escalation | Minimum interval between denied restart attempts |
| `thrashMaxRestarts24h` | 3 | Rung f budget | Maximum exit-75 escapes per rolling 24 hours |
| `thrashRestartBackoff` | 30m, 2h, 6h | Rung f budget | Escalating re-arm delays between escapes |
| `reloadWatchdogInterval` | 30s | Reload detection | Cadence for stuck-reload checks |
| `reloadHardLimit` | 20m | Reload detection | Maximum allowed duration for a single reload |
| `reloadWatchdogReFire` | 5m | Reload detection | Minimum cooldown between reload watchdog escalations |
| `hotswapMaxRestarts24h` | 3 | Rung e budget | Maximum hot-swap handoffs per rolling 24 hours |
| `hotswapRestartBackoff` | 30m, 2h | Rung e budget | Escalating delays between hot-swap handoffs |
| `gcSubtickInterval` | 10s | GC governor | Rapid heap-spike sampling interval |
| `gcFreeOSMemoryMinInterval` | 5m | GC governor | Minimum cooldown between FreeOSMemory calls |
| `loopBackoffBase` | 1s | Loop supervision | Initial backoff after a supervised loop failure |
| `loopBackoffMax` | 5m | Loop supervision | Maximum backoff cap for supervised loop restarts |
| `loopHealthyRun` | 10m | Loop supervision | Run duration required to reset supervisor backoff |
| `sensorStalenessTimeout` | 90s | Fault detection | Missing sensor updates before decaying to unknown and stopping costly rungs |
| `sensorBlindCheapHold` | 10m | Fault detection | How long cheap reversible rungs keep holding after sensors go blind |
| `recycleVerifyWindow` | 5m | Rung c2 | Time after a recycle to confirm the offending metric fell |
| `recycleMinImprovement` | 20% | Rung c2 | Minimum drop that counts as a successful recycle |
| `recycleMaxPerHour` | 2 | Rung c2 | Cap per subsystem so a bad recycle cannot loop |
| `sensorPlausibilityMax` | 1.05 | Sensor validation | Raw delta fraction ceiling above which a sample is flagged implausible (the stored fraction still clamps to 1.0) |
| `thrashPersistTimeout` | 5s | I/O bound | Bounded timeout for anti-loop state persistence |
| `thrashLedgerTimeout` | 2s | I/O bound | Bounded timeout for action ledger writes |

Proposals, not yet in code: `hotswapMaxRestarts24h`, `hotswapRestartBackoff`, `sensorStalenessTimeout`, `sensorBlindCheapHold`, `sensorPlausibilityMax`, `recycleVerifyWindow`, `recycleMinImprovement`, `recycleMaxPerHour`. The names `nowFn`, `hotswap_cap.json`, `Recycle()`, `Cost()`, `subsys` table and the `urnet_subsystem_*` and `urnet_heal_*` metrics also do not exist yet. Every other constant above is the shipped value and is pinned by the parity tests.

### 4.5 Attribution: which subsystem is the runaway

The supervisor names the offender with data, using the existing metrics system as its only source (`provider/node_internals.go` and the Prometheus registry behind `/metrics`). It adds no second sampler.

1. Totals: the cheap `internals` read (runtime/metrics, about 18us at 95k goroutines, cached 100ms). This is the same read `urnet-tools top` uses, so the supervisor and operators see identical numbers.
2. Goroutines per subsystem: the cached goroutine profile grouped by function (`goroutineGroupsTTL` 5s, about 80ms of CPU and a brief pause at 95k goroutines). A maintained table maps function name prefixes to subsystem names (for example `github.com/urnetwork/connect.(*PlatformTransport).runH3` to `h3`; profile frames carry the full import path, so a prefix without it never matches). No change to where goroutines are started. The supervisor only asks for a fresh profile when pressure is rising, and shares the cache with `top`, so it never adds a second profile. The cached profile keeps only the largest groups (`goroutineGroupsMax`, 12), so a subsystem outside the top 12 would be invisible to attribution: the supervisor's read must aggregate by prefix before the truncation, or the cap must be lifted for its reads.
3. Resources per subsystem: counters the code already keeps (live connections, H3 attempts and connects, buffer and queue bytes, sockets) read from the same registry that `/metrics` exports. New counters are added to that registry, never to a private structure.
4. Heap per subsystem: one on-demand heap profile when pressure is rising, cached like the goroutine profile, grouped by the same function table and diffed against a baseline stored when the box was last calm.
5. Counterfactual: every recycle writes before and after values to the action ledger. Over time the supervisor learns which subsystems' recycles actually help and prefers those.

The offender is the subsystem whose goroutines, resources, or heap grew the most relative to the calm baseline, normalized by its share of the total. A subsystem missing from the function table or without counters is reported as unattributed and never recycled.

One source of truth: the per-subsystem values the supervisor acts on are exported as Prometheus gauges (`urnet_subsystem_goroutines{subsystem}`, `urnet_subsystem_heap_bytes{subsystem}`, `urnet_subsystem_connections{subsystem}`) and shown in `self-heal status` and `urnet-tools top`. Grafana shows exactly what the supervisor saw. Counters for recycles and misses (`urnet_heal_recycles_total{subsystem,result}`) go in the same registry.

### 4.6 Operator-facing logs (plain English)

Every action and every refusal to act is explained in one plain English line, tagged `[heal]` (H3 sizing uses `[h3]`). Rules: log on a state change or an action, never on every tick; each line says what was seen, what was done, what it affects, and what the operator can do; rate-limited per cause; important level so it reaches disk and the ramlog. Wording is fixed so operators can search for it, and each line is listed in LOG_REFERENCE.md and pinned by a test.

Examples of the required lines:

- `[heal] Memory is under pressure (score 0.71, 2.1 GiB of 8 GiB available). Pausing pool growth and slowing proxy checks. No proxies are affected.`
- `[heal] The h3 carriers look like the cause: 61% of goroutines, up from 38% when the box was calm. Restarting only them. Your proxies keep their IPs and may reconnect for a few seconds.`
- `[heal] Restarted the h3 carriers. Goroutines fell 41% within 5 minutes, so that worked.`
- `[heal] Restarted the h3 carriers but goroutines only fell 4% (needed 20%). That did not help. Escalating to a hot-swap if memory allows.`
- `[heal] Memory looks like it is thrashing, but only one signal says so (memory stall). Not restarting until a second signal confirms.`
- `[heal] Restart budget used (3 of 3 in 24 hours). Not restarting again. This box needs more memory or fewer proxies; see self-heal status.`
- `[heal] The memory sensors have gone quiet for 90 seconds. Stopping any action that costs an IP or restarts the process. Pool growth stays paused for up to 10 more minutes, then resumes.`
- `[heal] A reading of 140% memory stall is not possible, so it was ignored (sensor glitch).`
- (planned with the H3 Stage B work, not normative until then) `[h3] H3 was requested for 3400 proxies but only 610 fit. Measured cost is 118 KiB per connection on this box and the memory headroom floor was reached. Running 610. Lower the setting or add memory to run more.`
- (planned with the H3 Stage B work, not normative until then) `[h3] Memory is tight, so H3 is being dropped for 80 proxies first. They keep working over H1. H3 will return slowly once memory has been calm for 15 minutes.`

## 5. Status and config surfaces

- `urnet-tools self-heal status` (extended): state, observed and actuating scores, component breakdowns, active budgets, parked count, shed history, hotswap availability, and recent ledger entries. Consolidates `pressure_status` and `thrash_status` into one supervisor document. Surfaces the human summary sentence.
- `~/.urnetwork/proxy_self_heal`: config toggle marker for self-heal on/off.
- Budgets and ceilings: unified configuration block for ladder thresholds, floors, and 24h restart budgets.

## 6. Migration path

Phase 2a shipped the real sensor set, state names, ledger writes, and supervised restart gated on self-heal.

What remains, concretely:

1. Fold the thrash responder in: relocate thrash sensing and states into the supervisor core. Keep the reload watchdog in its own goroutine with pure decision functions and in-memory atomics.
2. Collapse loops: merge pressure monitor and thrash watchdog tickers into one sensing and decision loop. Build the loop as a portable core with Linux-only thrash sensors behind build tags.
3. Wrap audit as the park rung: move audit acting mode under self-heal, retaining detection under `proxy_audit`.
4. Wrap pacing, AIMD, and trim as ladder rungs one at a time behind individual flags.
5. Add the recycle rung (c2) once the attribution counters exist, starting with the H3 carriers and the reload goroutine.
6. Only then consider changing defaults (still opt-in).

Invariants the fold-in must preserve:

- Two-way mutual exclusion: rung e and rung f exclude each other via `hotSwapLock`. The lock is held until exit or handoff completion.
- Thrash veto on hot-swap: rung e is strictly vetoed if thrash state is `thrashing` or `critical`.
- Rung e persisted budget: hot-swap handoffs enforce a 24h budget and backoff schedule.
- Pure decision step: the supervisor core executes zero I/O and never takes locks that can wedge.
- Observed vs actuating split: self-heal off publishes 0 to actuators while keeping observed metrics live.
- Attribution: never restart for swap owned by another process.
- Durable anti-loop persistence: never restart if writing the escalation record fails or times out.
- Exit-75 restart contract: rung f exits 75, requiring service supervisor restart support.

## 7. Open questions

- H3-carrier narrowing as a memory rung: candidate measured at ~0.55 MiB and ~13 goroutines per carrier on the canary; confirm on a canary before adding the rung.
- Should update-verify failures feed the healing state or stay separate?
- Should the supervisor's critical-state transitions also fire a webhook, or is polling `self-heal status` the intended operator workflow?
- Two rungs can engage at once from different causes today (for example audit parks while thrash freeze-growth holds); a shared budget arbitrates this once rungs merge; confirm the arbitration order then.
- The "who loses an IP" ordering assumes each proxy maps to an independently shed/restartable IP; confirm that for every provider topology (shared uplink, multi-identity-per-process).
- Hot-swap handoff (rung e) versus cold escape (rung f) arbitration: the handoff is used for a wedged process when memory is healthy (thrash state below thrashing). If thrash state is thrashing or critical, the handoff is strictly vetoed because spawning a child process under memory exhaustion accelerates memory failure. If vetoed while self-heal is off, an alert is logged. Under memory exhaustion, cold escape (rung f, exit 75) is the sole restart mechanism. Rung e and rung f enforce two-way mutual exclusion via `hotSwapLock`.

## 8. Implementation order

The implementation proceeds in 10 sequential steps:

1. Amend design doc for H1-H4 and M1-M3 (done by this document).
2. Add behavior-parity tests for existing loops and thresholds without refactoring.
3. Extract pure decision functions, keeping three separate goroutines.
4. Implement observed versus actuating score split, shared state, and ledger writes.
5. Implement rung e persisted daily budget, thrash state veto, and exclusive actuation token.
6. Merge pressure and thrash sensing and decision logic into the supervisor core, keeping reload detector in its own supervised goroutine.
7. Implement tiered sensor staleness decay, plausibility checks, and rung f corroboration.
8. Add the function-prefix to subsystem table over the existing goroutine profile, the per-subsystem gauges in the existing Prometheus registry, and the recycle interface; register the H3 carriers first, then verify others one at a time.
9. Move proxy audit acting gate under self-heal with release note callout.
10. Fold in the shrink, park, recycle, and shed rungs one at a time behind individual flags, canaried sequentially.

## 9. Required tests

Existing test suites that must continue passing:
- `thrash_watchdog_test.go`
- `reload_watchdog_test.go`
- `loop_supervisor_test.go`
- `resource_pressure_test.go`
- Installer exit-75 drop-in assertions (`scripts/test_provider_install.sh` lines 851-952)

New tests required for the implementation pull request:
- Self-heal off produces an actuating score of 0 while the observed score remains non-zero.
- Blocked flock or slow critLog delays reload stall detection by at most one tick.
- Sensor parser panic leaves the reload watchdog running and firing.
- Rung e hot-swap handoff is vetoed when thrash state is `thrashing` or `critical`.
- Rung e daily budget persists across process lifetimes and enforces escalating backoff.
- Hot-swap (rung e) and exit-75 (rung f) exclude each other in both directions, including the branch where `hotSwapLock` is held.
- Unavailable or dropped sensor stream decays to `unknown`: costly rungs stop at once, cheap rungs lift after `sensorBlindCheapHold`.
- A raw stall fraction above 1.05 is flagged implausible and yields a neutral tick, while the tracker still clamps the stored fraction to 1.0 (`TestThrashTrackerClampsFraction` is unchanged).
- Rung f does not fire on a single signal: severe PSI full alone is not enough, and a corrupt reading plus a healthy second signal produces no restart.
- The persisted cap bounds restarts across process lifetimes even when conditions persist after a restart.
- A recycled subsystem releases its goroutines (live count returns to baseline) and keeps its identities' IPs.
- A recycle that does not improve the metric is recorded as a miss and escalates; `recycleMaxPerHour` stops a loop.
- A subsystem missing from the function table or without counters is reported as unattributed and never recycled.
- Every `[heal]` line in section 4.6 is emitted exactly once per state change with the documented wording, appears in LOG_REFERENCE.md, and is not repeated on every tick.
- The per-subsystem gauges in `/metrics` match the values the supervisor acted on.
- Failed or panicked merged loop resets all actuators to neutral (actuating score 0, memory budget full, baseline GOGC, thrashFreeze false).
- Stub build compiles and behaves sanely on non-Linux platforms (Darwin, Windows, FreeBSD).