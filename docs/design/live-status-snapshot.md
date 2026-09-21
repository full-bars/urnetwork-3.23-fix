# Live status snapshot (design proposal)

> [!NOTE]
> **Status: Implemented.** The typed snapshot contract, provider collector, `urnet-tools status` live block/`--json`, Prometheus metrics, alert rules, and Grafana dashboard panels are implemented. Initial default tuning values remain open for ongoing refinement against fleet telemetry. Companion feature: [`urnet-tools top`](urnet-tools-top.md).

## Problem

Three questions about a node are hard to answer today without reading logs or a file:

1. **Is it earning right now?** `urnet-tools status` on Linux prints `systemctl status` and nothing about traffic. Live throughput is only visible as the `billable_rate` file (read by `update --idle`) or as a Prometheus counter that needs Grafana to turn into a rate.
2. **Why did it restart, and what changed?** The provider records whether the last shutdown was clean and what version it replaced (`detectStartup` in `provider/metrics_provider.go`), but nothing says *why* it restarted, and none of it is shown to an operator.
3. **Is it near a limit?** Memory limit, open file descriptors and RSS are not exported.

Fleet-wide visibility already exists as Prometheus plus the Monitoring bundle, which has a 25-panel dashboard and **no alert rules**. That bundle needs the provider's `/metrics` endpoint, which is optional and can be switched off, so `urnet-tools` cannot depend on it.

## Goals

- One place in the provider that builds a typed picture of the node, read by every consumer, so a new metric is added once.
- A live section in `urnet-tools status`, and machine-readable output for scripts.
- The few missing gauges, a first set of alert rules, and dashboard panels for what is exported but not shown.
- Nothing breaks in a mixed-version fleet, and nothing existing changes shape.

## Non-goals

- A live full-screen view. That is [`urnet-tools top`](urnet-tools-top.md).
- A restart guard ("the node is busy, are you sure?"). Deferred; it reuses the `busy` signal defined here.
- Changing the `billable_rate` file, its location or its format. `update --idle` keeps reading it exactly as today.
- Any hub or report-URL work. The hub was removed in v31.3.

## Design

### The node snapshot

A new file, `provider/node_snapshot.go`, defines `NodeSnapshot` and a collector with one call, `Get()`. Results are cached for about one second so several readers in the same second do not recompute anything.

| Group | Fields |
|---|---|
| Identity | schema version, provider version, previous version, start time, uptime |
| Earning | billable rate now, 1 minute average, 5 minute average, active clients, active sessions (classical and PQE) |
| Lifecycle | restart reason, whether the last shutdown was clean |
| Health | proxies by status (up, degraded, connecting, dead), recent auth failures, pressure score |
| Resources | heap in use, memory limit, RSS, goroutines, open file descriptors, descriptor limit |
| Restart safety | `busy` (derived from the 1 minute rate against the idle threshold) and whether config changes are waiting for a restart |

Fields the platform cannot supply (RSS and descriptor counts outside Linux) are omitted, not zeroed, so a reader can tell "unknown" from "zero".

**Rate.** The rate is computed the same way `runBillableRateWriter` does today: the sum of per-proxy billable receive and transmit bytes from `connect.ProxyHealthSnapshot()`, differenced over time. The collector samples once per second into a fixed ring of 600 values (10 minutes, about 5 KB) that also feeds the sparklines and the 1 and 5 minute averages. The `billable_rate` writer is left alone.

**Open question for the plan:** the count of *active contracts*. `/metrics` exports contract counters (`urnet_contracts_total`) but not a live count. Confirm whether one is available cheaply; if not, the field is left out of the first version.

### Restart reason

The provider already writes `.clean-shutdown` on exit and reads it at the next start. This adds a sibling marker, `.restart-reason`, written by `urnet-tools` immediately before it restarts a provider and consumed (read, then deleted) by `detectStartup`, the same pattern.

| Reason | How it is decided |
|---|---|
| `update` | marker written by `urnet-tools update` |
| `hotswap` | marker written by the hotswap parent before handover |
| `manual` | marker written by `urnet-tools restart` |
| `clean` | clean-shutdown marker present, no reason marker (for example a plain `systemctl restart` or a reboot) |
| `unclean` | no clean-shutdown marker: crash, OOM kill, `SIGKILL` or power loss |
| `first-start` | no version file yet |

A crash and an OOM kill look identical from inside the process, so both are `unclean`. Telling them apart needs systemd's recorded exit status; that is out of scope here.

### Control socket

A new `snapshot` request on the existing control socket returns the snapshot as one JSON object.

- The object carries a schema version. Fields are only ever added, never renamed or removed.
- A newer `urnet-tools` talking to an older provider gets "unknown command" and simply omits the live section. An older tool never sends the request.
- The existing `status`, `version`, `get`, `set`, `clear` and `history` commands are untouched.

### `urnet-tools status`

On every platform the existing output stays, and a live block is added underneath when the provider answers. Illustration (numbers made up):

```
Live (provider v3.23.0-fix.32.1)     FLOWING
  throughput  38.2 MiB/s   1m 41.0   5m 35.4   ▂▃▅▆▇▇▆▅▆▇█▇
  clients     212 sessions
  proxies     58 up, 2 degraded, 0 connecting, 0 dead
  uptime      3d 4h        last restart: update (v3.23.0-fix.32.0 to v3.23.0-fix.32.1)
  memory      1.8 GiB heap of 4.0 GiB limit, RSS 2.1 GiB, 1204 goroutines, 310 of 65536 fds
  pressure    0.21
```

- **State** is one of `STOPPED`, `DEGRADED`, `IDLE` or `FLOWING`, checked in that order. `RESTART PENDING` is shown as a flag beside the state, not as a state, because a node can be flowing and still have a config change waiting.
- **`DEGRADED`** initial defaults: more than half the pool dead or degraded, or pressure score at or above 0.8.
- **`IDLE`** means the 1 minute rate is below the same threshold `update --idle` uses (5 KiB/s).
- **`--json`** prints the raw snapshot for scripts.
- **Several providers on one host** (`Discover()` already finds them): plain `status` shows one compact row per provider (state, rate, clients, uptime, version), and `status <name>` shows the detail block above.
- **No provider running or reachable:** the live block is skipped and nothing else changes.
- Colour and sparklines respect `NO_COLOR` and fall back to plain characters on a dumb terminal.

### "Why is it idle?"

When the state is `IDLE`, a hint line is chosen by the first rule that matches:

1. no proxies are configured;
2. every proxy is dead or still connecting;
3. auth failures increased in the last 10 minutes;
4. proxies are up but no contracts were acquired in the last 10 minutes;
5. otherwise, "no traffic offered".

These use signals the provider already tracks. Rule order and windows are initial defaults.

### Metrics

`urnet_uptime_seconds` already exists and is not repeated. New gauges, all read from the snapshot at scrape time so nothing is counted twice, and **no existing metric name changes**:

| Metric | Meaning |
|---|---|
| `urnet_restart_reason{reason="..."}` | value 1 for the current reason, absent otherwise |
| `urnet_mem_limit_bytes` | the Go memory limit in effect |
| `urnet_rss_bytes` | resident set size (Linux) |
| `urnet_open_fds` | open descriptors (Linux) |
| `urnet_fd_limit` | descriptor limit (Linux) |

### Monitoring bundle

- **Panels:** restarts and reasons over time, uptime per node, hotswap outcomes (`urnet_hotswap_outcomes_total`), version skew across nodes, memory against limit, descriptors against limit.
- **Alert rules:** a new `monitoring/prometheus/rules/urnetwork.yml`, referenced from `prometheus.yml`. Initial candidates: node down, restart loop (more than N restarts in an hour), node on an old version for more than a week, memory near limit, descriptors near limit, and zero billable traffic while up. Thresholds are initial defaults and every rule is documented with what it does not detect.

## Compatibility

- Mixed-version fleets are expected. Old provider with new tool, and new provider with old tool, both degrade to today's behaviour.
- Existing metric names, the `billable_rate` file and every existing control command are unchanged.
- Windows and macOS get the live block through the existing status panel; fields they cannot supply are omitted.

## Testing

- **Collector:** an injectable clock and rate source, tests for the cache window, ring wrap, the 1 and 5 minute averages, and the counter-reset case (a proxy removed mid-run must not produce a negative or huge rate).
- **Restart reason:** a table test over marker combinations, including a stale marker left by an interrupted restart.
- **Control socket:** a round trip, an unknown-command reply from an old provider, and schema-version tolerance (extra unknown fields ignored).
- **Status rendering:** golden files for each state, `NO_COLOR`, and a narrow terminal.
- **Metrics:** the existing `promtext` linter over the new gauges, plus a test that no existing metric name changed.
- **Alert rules:** `promtool test rules` where available.

## Rollout

Built in `urnetwork-3.23-fix` first, then ported to `meso-miner` and `sn` in the same way as earlier fixes. `sn` lays out its provider code differently, so its port adapts rather than cherry-picks. Ships as an ordinary point release, not a release candidate. Adds no new dependency.

## Open questions

- Active-contract count (see above).
- Whether the alert thresholds should ship enabled or as commented examples. Leaning enabled for node down and restart loop, commented for the rest.
- Whether the sparkline ring should be exposed over the control socket for tools other than `top`, or only through `snapshot`.
