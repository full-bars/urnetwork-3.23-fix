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

## 2. Principles

1. ONE supervisor loop owns sensing, state, and escalation. Components expose actions;
   they do not each grow their own timer and policy.
2. The core must be lock-free relative to the pathologies it responds to: it may not
   share the reload mutex, the proxy lock, or any lock a wedge can hold. The responder
   must survive the disease.
3. IP-preserving by default: more running proxies means more earnings. Tiers a-c below
   lose zero identities; shedding is bounded, floored, worst-first, and reversible.
4. Every action is recorded with reason and outcome (reuse the existing action-ledger /
   audit-ring pattern), and every action is bounded by global budgets and hysteresis.
5. Off means off: with self-heal disabled, behavior is exactly as today.

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
  become this rung's budget inputs instead of parallel cap systems.
- e. COMPONENT RESTART: reload-watchdog escalation, hot-restart of subsystems.
- f. ESCAPE (heaviest): hotswap/restart the provider; remember the condition (a "thrash
  cap" in the oom-cap pattern: start leaner next time, relax after clean windows).

## 5. Status and config surfaces

- `urnet-tools heal status` (new): state, score and components, active budgets, parked
  count, shed history, hotswap availability, recent ledger entries. `self-heal status`
  remains as an alias until deprecated.
- One config block for budgets/floors; existing keys keep working. Audit/trim/OOM-cap
  keys become ladder parameters rather than separate engines.

## 6. Migration path

1. Ship the sensors + `heal status` (read-only, zero behavior change).
2. Fold the reload watchdog and the thrash responder in as the component-restart rung;
   their loops become the supervisor core's lock-free spine.
3. Wrap audit/pacing/AIMD/trim as ladder rungs one at a time, each behind a flag, each
   with its own tests.
4. Only then consider changing defaults (still opt-in).

## 7. Open questions

- H3-carrier narrowing: worth measuring as a memory rung? (canary test)
- Should update-verify failures feed the healing state or stay separate?
- Should audit's acting mode depend on the supervisor instead of the hot-restart toggle
  directly?
