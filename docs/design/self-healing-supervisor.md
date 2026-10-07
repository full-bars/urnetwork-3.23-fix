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
| Reload watchdog (in flight) | reload stuck past a bound | escalation into hot-restart machinery | risks becoming another silo unless folded in |
| Thrash responder (proposed) | swap thrash (PSI full + pswpout + refaults) | freeze growth -> shed -> escape+remember | same risk: another loop unless folded in |
| Backend outage watcher | backend degraded probes | logs + webhook (observer) | observer only; fine as-is |

Since this doc was written, phase 2a shipped: the thrash watchdog merged together with the readable-log pass, using this doc's state names and the shared action ledger. Reality check against principle 1: the pressure monitor, the thrash watchdog and the reload watchdog currently run three independent tickers with overlapping /proc and cgroup reads. "One supervisor loop" therefore means collapsing three live loops into one, not relocating code - the single biggest cost of the fold-in.

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
5. Off means off for actions: with self-heal disabled, no actuator fires and behavior is
   exactly as today. Sensing, logging and status stay visible (the shipped thrash
   watchdog works this way), so an operator can watch the system without arming it.

## 3. Sensors (one sample, shared)

Extend the existing 30s pressure sample (`resource_pressure.go`) with:

- PSI memory `full avg60` (all tasks stalled) in addition to `some` — the thrash signal.
- Swap ACTIVITY: `/proc/vmstat` pswpin/pswpout rates, cgroup `memory.stat` pswpout, and
  `workingset_refault_anon` (our own pages being re-fetched is direct thrash evidence).
  Swap usage alone is NOT thrash and must not trigger actions.
- Event inputs: kernel OOM kill observed, hotswap decline recorded, reload stall
  detected.

Keep worst-component-wins, EWMA, and emergency pins as today.

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
  become this rung's budget inputs instead of parallel cap systems. There is no code
  precedent for this rung today - it is greenfield, unlike rung c, which wraps the
  audit's existing park action.
- e. COMPONENT RESTART: restart the wedged component without the whole process (the
  target to build toward; today both watchdogs go straight to a full restart).
- f. ESCAPE (heaviest): hotswap/restart the provider; remember the condition (a "thrash
  cap" in the oom-cap pattern: start leaner next time, relax after clean windows).

Until a component-restart actuator narrower than a process restart exists, e and f are
the same action with different callers; treat them as one rung.

## 5. Status and config surfaces

- `urnet-tools self-heal status` (extended): state, score and components, active budgets,
  parked count, shed history, hotswap availability, recent ledger entries. The shipped
  surface is already `self-heal` (CLI and status file), so the name stays; `heal` was the
  working title and no rename is planned. `urnet-tools status` also prints the current
  reading as one sentence.
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
   all; its state and ledger support must be built, not moved. Split the work accordingly.
2. Collapse the loops: "one supervisor loop" means merging the pressure monitor, the
   thrash watchdog and the reload watchdog - three independent tickers reading
   overlapping /proc and cgroup sources - into one. Size this as a refactor, not a
   relocation; it is the biggest single lift in the fold-in.
3. Wrap audit as the park rung: the concrete first task is the acting gate. Audit's
   acting mode depends on `hotRestartEnabled()` today; moving it under the supervisor is
   this step's whole content.
4. Wrap pacing/AIMD/trim as ladder rungs one at a time, each behind a flag, each with
   its own tests.
5. Only then consider changing defaults (still opt-in).

Invariants the fold-in must preserve (easy to drop in a refactor):

- the hotswap-concurrency guard (`thrashHotSwapBusy`): never restart while a hot-swap is
  draining or mid-handoff;
- the exit-75 restart contract: the restart actuator exits 75, and the unit's restart
  policy must restart on it (`Restart=on-failure` or `always`; the installer warns about
  drop-ins that weaken this);
- attribution: never restart for swap owned by another process;
- off-means-off for actions (sensing and logging stay visible).

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
