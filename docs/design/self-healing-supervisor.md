# Unified Self-Healing Supervisor — design

## 1. Why

The provider has accumulated several healing-ish systems that each work in isolation but
share no brain, no ledger, and no status surface. When a canary box wedged for 13h (a
proxy reload stuck holding its in-process mutex), every one of them either did nothing
or could not see the problem:

| System | Trigger | Actuator | Gap |
|---|---|---|---|
| Pressure self-heal (`resource_pressure.go`) | one composite score: PSI mem/cpu/io `some avg60`, MemAvailable, load/core, goroutines (per-proxy), heap fraction of soft limit, FD fraction — worst component wins, asymmetric EWMA, emergency pins | URL pacing 1x-8x, probe concurrency down to 1, cleanup+reaper cadence 6h->1h, AIMD pool sizing, adaptive GC (single owner) | does not drive audit; cannot restart anything; blind to swap I/O and to a wedged component |
| Proxy audit | grade <= 0.4 on two consecutive passes | parks junk proxies (reversible, backoff 6h->7d) | silently depends on hot-restart being armed; observe-only otherwise |
| Cleanup job + reaper | cadence 6h (`cleanup-scope`) | sweeps dead proxies, refreshes stale grades | separate loop, no shared budget |
| Trim cap + OOM cap | manual trim; kernel OOM kill | caps the pool ("tighter wins") | two cap systems reconciling by min(); no relation to the pressure score |
| Hot-restart / HotSwap | explicit update, or manual | zero-downtime restart (SIGUSR2, NOTIFY_SOCKET-gated, 30s drain) | nothing else coordinates with it; audit depends on it |
| Unit-type convergence | every update | rewrites unit Type= to match the binary (config-drift self-heal) | one more independent reconciler |
| Reload watchdog (in flight) | reload stuck past a bound | escalation into hot-restart machinery (zero-downtime hot-swap, gated on `hotRestartEnabled()`, default on) | risks becoming another silo unless folded in |
| Thrash responder (proposed) | swap thrash (PSI full + pswpout + refaults) | freeze growth -> shed -> escape+remember | same risk: another loop unless folded in |
| Backend outage watcher | backend degraded probes | logs + webhook (observer) | observer only; fine as-is |

Since this doc was written, phase 2a shipped: the thrash watchdog merged together with the readable-log pass, using this doc's state names and the shared action ledger. Reality check against principle 1: the pressure monitor, the thrash watchdog and the reload watchdog currently run three independent tickers; the pressure monitor and the thrash watchdog share /proc and cgroup sources, while the reload watchdog only reads in-memory atomics. "One supervisor loop" therefore means collapsing three live loops into one, not relocating code - the single biggest cost of the fold-in. Gate reality check: the reload watchdog's escalation fires whenever hot-restart is armed (default on), entirely independent of the self-heal switch - see principle 5.

## 2. Principles

1. ONE supervisor loop owns sensing, state, and escalation. Components expose actions;
   they do not each grow their own timer and policy.
2. The core must be lock-free relative to the pathologies it responds to: it may not
   share the reload mutex, the proxy lock, or any lock a wedge can hold. The responder
   must survive the disease. It must also preserve the hotswap-concurrency guard the
   thrash watchdog already carries (`thrashHotSwapBusy`: never restart while a hot-swap
   is draining or mid-handoff) - easy to drop in a refactor.
3. IP-preserving by default: more running proxies means more earnings. Tiers a-c below
   lose zero identities; shedding is bounded, floored, worst-first, and reversible.
4. Every action is recorded with reason and outcome (reuse the existing action-ledger /
   audit-ring pattern), and every action is bounded by global budgets and hysteresis.
5. Gate separations stay as shipped. Resource-pressure and thrash-mitigation actuators
   are gated on the opt-in self-heal switch: off means off for actions, while sensing,
   logging and status stay visible (the shipped thrash watchdog works this way), so an
   operator can watch the system without arming it. Process-wedge watchdogs that defend
   availability are a separate domain: the reload watchdog today fires whenever
   `hotRestartEnabled()` is true (default on), and folding it under a default-off
   self-heal switch would silently disarm stuck-reload protection on every default node.
   The fold-in must keep these two gate domains distinct and document which rungs belong
   to which (the reload-watchdog escalation stays armed when hot-restart is armed).

## 3. Sensors (one sample, shared)

Extend the existing 30s pressure sample (`resource_pressure.go`) with:

- PSI memory `full`: the cumulative `total=` microseconds computed over the ACTUAL
  elapsed wall time between samples, as `thrashRates` does - NOT `avg60`. A
  swapped-out ticker can slip 90s+ between samples, and any fixed-window average then
  distorts; the delta-over-wall-clock math survives that. `full` is the thrash signal;
  `some` remains the pressure system's.
- Swap ACTIVITY: `/proc/vmstat` pswpin/pswpout rates, cgroup `memory.stat` pswpout, and
  `workingset_refault_anon` (our own pages being re-fetched is direct thrash evidence).
  Swap usage alone is NOT thrash and must not trigger actions.
- Event inputs: kernel OOM kill observed, hotswap decline recorded, reload stall
  detected.

Keep worst-component-wins, EWMA, and emergency pins as today.

Platforms: the pressure monitor and the reload watchdog already compile and run on
Linux, Windows and macOS (memory notification objects on Windows,
`kern.memorystatus_vm_pressure_level` on macOS); only the thrash detection (`/proc` and
cgroup reads) is Linux-only. Structure the supervisor as a portable core - ticker,
state transitions, budgets, action dispatch - with OS-specific sensor drivers (the
existing `thrash_watchdog_stub.go` pattern), so Windows and macOS keep pressure
management and reload-watchdog monitoring. `thrashing` and `critical` are Linux states;
`under-pressure` and `calm` are portable.

## 4. State and escalation ladder (ordered by "who loses an IP")

States: calm -> under-pressure -> thrashing -> critical (hysteresis: rise fast, relax
only after sustained calm).

- a. FREEZE (zero IP loss): pause admissions and URL probing; stop pool growth.
- b. SHRINK OURSELVES (zero IP loss): URL pacing, probe concurrency, GC tightening,
  background work pauses; consider temporarily narrowing H3 carriers (a canary running
  h3=all held ~500 carriers, the largest single memory consumer; backing it off should
  free memory while keeping every identity — measure before adding as a rung).
- c. PARK (zero IP loss, reversible): the audit's park action (worst-first, backoff
  ladder, 24h budget) becomes the standard "stop wasting on junk" rung, no longer gated
  behind a separate hot-restart precondition decision.
- d. SHED (bounded IP loss): the last resort for sustained heavy pressure only: the
  smallest step that relieves, worst-first (dead/degraded earn nothing), with a floor
  (never below N proxies), hysteresis, and reversible re-admission. Trim cap and OOM cap
  become this rung's budget inputs instead of parallel cap systems. The shed machinery
  already exists in the pool controller (`selectURLProxiesToShed` worst-first: dead ->
  degraded -> lowest earnings; `shedPoolToTarget` with floor bounds; `applyShedBackoff`
  1h re-admission; `runPoolController` hysteresis); this rung integrates those parts
  into the ladder rather than building new logic.
- e. COMPONENT RESTART: restart the wedged component without the whole process (the
  target to build toward). The closest shipped actuator is the reload watchdog's
  escalation: a zero-downtime hot-swap (`runHotSwapParentHandoff`, 30s connection drain,
  NOTIFY_SOCKET-gated), which works on systemd, Docker (in-place `execve`) and Windows
  (named-pipe handoff) — existing proxies and client traffic stay live and earning.
- f. ESCAPE (heaviest): hotswap/restart the provider; remember the condition (a "thrash
  cap" in the oom-cap pattern: start leaner next time, relax after clean windows). The
  thrash responder's form of this rung is a COLD, service-manager-restarted exit with
  status 75 (systemd-only). That is a different action from rung e by design: severe
  swap thrash is exactly when spawning a child next to the parent is unsafe, so it
  cannot hand off - it exits, and the unit restarts it. e preserves live traffic; f
  accepts the drop because memory exhaustion leaves no safe zero-downtime option. Keep
  them distinct and never rebrand the reload watchdog's escalation as an exit-75
  restart.

## 5. Status and config surfaces

- `urnet-tools self-heal status` (extended): state, score and components, active budgets,
  parked count, shed history, hotswap availability, recent ledger entries. The CLI
  command is `self-heal`; on disk the state lives in `pressure_status` and
  `thrash_status` (no `self_heal`-named status file exists; `proxy_self_heal` is an
  unrelated marker) - so the name stays, `heal` was the working title and no rename is
  planned. A fold-in task: consolidate the two status files into one supervisor status
  document. `urnet-tools status` already prints the persisted sentence.
- One config block for budgets/floors; existing keys keep working. Audit/trim/OOM-cap
  keys become ladder parameters rather than separate engines.

## 6. Migration path

Steps 1 and 2 already happened, fused: the swap-thrash watchdog (phase 2a) shipped the
real sensor set, a state machine using this doc's state names, ledger writes, and a
supervised-restart actuator gated on self-heal - plus a readable-log pass. There is no
standalone sensors-only checkpoint, so the restart actuator cannot be pulled without the
sensors; the only axis is the existing self-heal toggle.

What remains, concretely:

1. Fold the thrash responder in: close to a relocation - it already speaks the
   supervisor's state names and ledger format. The reload watchdog is the larger half of
   this step: its own ticker, no `thrashStateT`-style states, and no ledger writes at
   all; its state and ledger support must be built, not moved. It also stays on its own
   gate (hot-restart armed), per principle 5 - do not subordinate it to self-heal.
   Split the work accordingly.
2. Collapse the loops: "one supervisor loop" means merging the pressure monitor, the
   thrash watchdog and the reload watchdog - three independent tickers, of which the
   first two read overlapping /proc and cgroup sources - into one. Size this as a
   refactor, not a relocation; it is the biggest single lift in the fold-in. Build the
   loop as a portable core with Linux-only thrash sensors behind a build-tagged driver
   (the `thrash_watchdog_stub.go` pattern).
3. Wrap audit as the park rung: the concrete first task is the acting gate. Audit's
   acting mode depends on `hotRestartEnabled()` today; moving it under the supervisor is
   this step's whole content.
4. Wrap pacing/AIMD/trim as ladder rungs one at a time, each behind a flag, each with
   its own tests.
5. Only then consider changing defaults (still opt-in).

Invariants the fold-in must preserve (easy to drop in a refactor):

- the hotswap-concurrency guard (`thrashHotSwapBusy`): never restart while a hot-swap is
  draining or mid-handoff;
- the exit-75 restart contract (thrash rung only): the restart actuator exits 75, and
  the unit's restart policy must restart on it (`Restart=on-failure` or `always`; the
  installer warns about drop-ins that weaken this);
- gate separation: reload-watchdog escalation stays gated on hot-restart (default on),
  never folded under the opt-in self-heal switch (principle 5);
- attribution: never restart for swap owned by another process;
- off-means-off for ACTIONS in the self-heal domain (sensing and logging stay visible);
- durable anti-loop persistence: never restart if writing the escalation record
  (`thrash_cap.json`) fails or times out - alert instead (`persist-failed`); enforce the
  24h restart ceiling (3 escapes) and the growing backoff so the service cannot trip
  systemd's restart burst limits.

## 7. Open questions

- H3-carrier narrowing as a memory rung: measured candidate (~0.55 MiB and ~13 goroutines
  per carrier on the canary); confirm on a canary before adding the rung.
- Should update-verify failures feed the healing state or stay separate?
- Should the supervisor's critical-state transitions also fire a webhook, or is polling
  `self-heal status` the intended operator workflow?
- Two rungs can engage at once from different causes today (e.g. audit parks while thrash
  freeze-growth holds); a shared budget arbitrates this only once the rungs merge -
  confirm the arbitration order then.
- The "who loses an IP" ordering assumes each proxy maps to an independently
  shed/restartable IP; confirm that for every provider topology (shared uplink,
  multi-identity-per-process).
- OpenRC and Docker deployments have no systemd exit-75 restart; the restart rung is
  inert there by construction - document the actuator's per-init behavior when the
  supervisor ships.
- The cold-escape exit-75 actuator and the zero-downtime hot-swap both exist; confirm
  the supervisor's arbitration prefers hot-swap whenever the failure mode is a wedge
  (not memory exhaustion), so escapes do not needlessly drop traffic.