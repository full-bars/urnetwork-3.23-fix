# ⚙️ Configuration Reference

## 🌍 Environment Variables

| Variable | Default | Description |
| :--- | :--- | :--- |
| `BUILD` | `stable` | Set to `jwt` for auth code login, or `stable` for email/password auth. |
| `USER_AUTH` | - | Your email. Required if `BUILD=stable`. Also used for **self-healing** in `BUILD=jwt` mode to refresh expired tokens. |
| `PASSWORD` | - | Your password. Required if `BUILD=stable`. Also used for **self-healing** in `BUILD=jwt` mode to refresh expired tokens. |
| `URNETWORK_AUTH_CODE` | - | First-run auth code for `BUILD=jwt`. Use this instead of passing the code as a trailing command argument. Ignored once a JWT exists in the volume. |
| `UR_API_URL` | `https://api.bringyour.com` | Custom API URL, for operators running their own backend. Must be set together with `UR_CONNECT_URL`. Applied once at startup via `provider choose_network` and persisted to `~/.urnetwork/network.json`, so it survives restarts if that directory is on a volume — same as the JWT. |
| `UR_CONNECT_URL` | `wss://connect.bringyour.com` | Custom connect (WebSocket signaling) URL, for operators running their own backend. Must be set together with `UR_API_URL`. See `UR_API_URL`. |
| `ENABLE_VNSTAT` | `true` | Enables the traffic monitor on port 8080. |
| `ENABLE_IP_CHECKER` | `false` | Diagnostic only. Prints your full public IP to container logs on startup via an external script. Distinct from dashboard identity reporting, which sends only a redacted IP. |
| `TURBO` | - | Set to `v4` or `v8` to enable turbo mode. Prefer this variable for Docker turbo mode. |
| `URNETWORK_RAMLOGS` | `0` | Set to `1` to redirect provider logs to RAM instead of stdout. Cannot be used with Docker `--log-opt`. |
| `URNETWORK_SKIP_AUDIT` | `0` | Set to `1` to skip the startup system audit (disk speed benchmark, ulimit, conntrack checks). Useful in Docker where host sysctls aren't visible. |
| `URNETWORK_PROFILE` | - | Advanced provider profile: `auto`, `lowmem`, `eco`, `turbo-v4`, or `turbo-v8`. For turbo, prefer `TURBO`. |
| `URNETWORK_ALERT_WEBHOOK` | - | HTTP POST endpoint for outage alerts. Fires on outage start and recovery. |
| `URNETWORK_NODE_NAME` | hostname / redacted IP | Friendly label for dashboard identity and webhook alerts. |
| `HOST_HOSTNAME` | - | Pass the host server name into the container. Use `-e HOST_HOSTNAME=$(hostname)` with `docker run` or `HOST_HOSTNAME=${HOSTNAME}` in Compose. |
| `URNETWORK_HEALTH_INTERVAL` | `5m` | How often to emit a `[health]` heartbeat log line. Includes uptime, RAM stats, and active connection count. Accepts Go duration strings such as `10m` or `1h`. Minimum `1m`. |
| `URNETWORK_PROXY_BENCHMARK` | - | Set to `true` to enable per-proxy latency monitoring. Off by default. Probes: TCP connect every 5 min (raw RTT to proxy port), SOCKS5 CONNECT every 15 min (end-to-end through proxy). Staggered startup jitter prevents thundering herd. ~104 GB/month at 10k proxies. |
| `URNETWORK_PROXY_BENCHMARK_ENDPOINT` | `connect.bringyour.com:443` | Target for the SOCKS5 CONNECT latency probe. Measured end-to-end through each proxy. |
| `URNETWORK_REPORT_URL` | - | HTTP URL of a bandwidth hub server. When set, the provider POSTs a JSON report with per-proxy metrics (Clients, TotalRx/Tx, BillableRx/Tx). See `hub/main.go` for the server. Can be changed at runtime without restart by writing to `~/.urnetwork/report_url` (or using `urnet-tools report <url>`). |
| `URNETWORK_REPORT_INTERVAL` | `5m` | How often bandwidth reports are posted to `URNETWORK_REPORT_URL`. Accepts Go duration strings such as `30s` or `2m`. Minimum `10s`. The `5m` default keeps the hub's historical SQLite write volume modest across a large fleet; lower it where a more live dashboard matters. |
| `URNETWORK_MESSAGE_POOL_SHARD_COUNT` | `16` | Number of internal mutex shards per message-pool size class. Higher values reduce lock contention at high packet rates. Must be a power of two, 1–256. Set to `1` to disable sharding (pre-v24.35 behavior). Sane values: `8` (moderate), `16` (default), `32` (high-pps tier3+). |
| `PROXY_URL` | - | Live proxy list URL, fetched and merged on an interval. Comma-separate for multiple sources. See [Proxy URL Sources](Proxy-URL-Sources.md). |
| `PROXY_URL_REFRESH` | `15m` | How often `PROXY_URL` is re-fetched to add new entries. |
| `PROXY_URL_MAX` | `500` | Caps total proxies sourced from `PROXY_URL`. `0` = unlimited. |
| `PROXY_DEAD_CLEANUP_SCOPE` | `url` | `none`, `url`, or `all` — which sources the automatic dead-proxy cleanup may touch. |
| `PROXY_DEAD_CLEANUP_INTERVAL` | `6h` | Base cadence of the automatic cleanup job, when scope isn't `none` (shrinks under pressure unconditionally — no longer gated by self-heal). |
| `URNETWORK_SELF_HEAL` | `0` (off) | Set to `1` to let the AIMD pool controller shed (remove) currently-healthy proxies under sustained pressure. Off by default. Pressure sampling, URL-fetch pacing, probe concurrency scaling, cleanup/reaper cadence, and AIMD pool *growth* are always on regardless of this flag — none of those can remove a healthy proxy, so there's nothing to opt into. Toggle at runtime with `urnet-tools self-heal on\|off\|status`. |
| `GOTRACEBACK` | - | Set to `crash` to produce full goroutine stack traces on Go runtime crashes. Add `Environment="GOTRACEBACK=crash"` to the systemd override.conf. |

## 📝 Critical Event Log

Since v3.23.0-fix.25.14, the provider writes a per-process event log to `~/.urnetwork/events.log` (on disk, not RAM — survives restarts). It records STARTUP, SIGNAL, PROVIDER EXIT, PANIC, and FATAL events. Capped at 1 MiB with automatic rotation.

```bash
cat ~/.urnetwork/events.log
```

## 🎛️ Profile Selection

| Profile | Docker Value | Best For | RAM |
| :--- | :--- | :--- | :--- |
| Auto | `URNETWORK_PROFILE=auto` | Recommended zero-config mode (auto-selects Low/Balanced/Perf/Extreme by RAM) | Any |
| Turbo V8 | `TURBO=v8` or `URNETWORK_PROFILE=turbo-v8` | Maximum throughput, dedicated servers | 16 GiB+ |
| Turbo V4 | `TURBO=v4` or `URNETWORK_PROFILE=turbo-v4` | High throughput, well-provisioned VPS | 4-16 GiB |
| Default | unset | General use | 2-4 GiB |
| Eco | `URNETWORK_PROFILE=eco` | RAM-constrained, full throughput | 1-2 GiB |
| Lowmem | `URNETWORK_PROFILE=lowmem` | Minimum RAM, reduced throughput | < 1 GiB |

See [High-Volume Performance Tuning](High-Volume-Performance-Tuning.md) for the detailed profile behavior and parameter tables.

## 🩺 Viewing proxy health

You can view the full list of dead and degraded proxies, as well as a live event log of proxy state transitions:

*   **Host**: Run `urnet-tools proxy health`.
*   **Docker**: See [Docker Deployment](Docker-Deployment.md) for the `proxy-health` command.

> [!NOTE]
> The proxy health files are stored in `URNETWORK_PROXY_HEALTH_DIR` (defaults to `<home>/.urnetwork` or `/root/.urnetwork` in Docker). Heartbeat intervals are tied to `URNETWORK_HEALTH_INTERVAL` (defaults to 5m).

## 🩹 Pressure system (self-heal)

A resource-pressure monitor is always running — it scales several actuators
proportionally instead of gating them on/off, and none of that requires
`URNETWORK_SELF_HEAL`. The flag only controls one thing: whether the AIMD
pool controller is allowed to shed (remove) currently-healthy proxies when
pressure stays high. It's off by default.

**Sensors** (sampled every 30s, worst-of-N combined, then smoothed with an
asymmetric EWMA — fast to react, slow to relax):
- `/proc/pressure/memory` and `/proc/pressure/cpu` (PSI `some avg60`), where available
- `MemAvailable / MemTotal` from `/proc/meminfo`
- `loadavg1` per core (fallback where PSI is unavailable)
- Self-signals: goroutine count and heap fraction of the configured `max-memory` soft limit

These combine into a single smoothed pressure score in `[0, 1]`. A self-inflicted
blowout (heap ≥90% of the soft limit, or ≥25,000 goroutines) pins the score to
`1.0` immediately, bypassing smoothing.

A second signal, `churn`, tracks how large a fraction of tracked proxies the
degraded-proxy reaper cut on its most recent 3-minute tick, smoothed the same
way as pressure but sampled on the reaper's own cadence rather than the 30s
pressure loop — a fast-decaying host-resource signal and a slower fleet-quality
signal are kept separate rather than blended into one number.

**Actuators, always on regardless of `URNETWORK_SELF_HEAL`:**
- URL-fetch pacing stretches from 1× to 8× the configured interval as pressure
  rises (replaces the old binary skip-at-threshold gate)
- Proxy probe concurrency scales down toward a floor of 1 worker
- The dead-proxy cleanup job and the reaper's stale re-probe window both run
  *more* often under pressure (6h → 1h and 3h → 1h respectively) — cleanup
  and the reaper shed load, so pressure is exactly when they should run harder,
  not less
- An AIMD pool controller adjusts a persisted `TargetPoolSize` (stored in
  `proxy_url.json`) every 5 minutes: +25 proxies when calm and churn is low
  (or the churn signal hasn't warmed up yet — a fresh restart isn't treated
  as "high churn"). A calm box won't grow the pool while the reaper is actively
  cutting a meaningful slice of the fleet every tick, since new arrivals would
  just get fed to the same grinder. This growth cap, and the fetch-admission
  cap it feeds, apply unconditionally.

**The one actuator gated by `URNETWORK_SELF_HEAL`:** after two consecutive
high-pressure samples (10 min sustained), the pool controller shrinks
`TargetPoolSize` (×0.7, floor 50, capped by `PROXY_URL_MAX`) and evicts the
worst URL-sourced proxies to match — dead, then degraded tiers, then healthy
ones by ascending traffic, with a 1h re-admission backoff. This is the only
actuator that removes a currently-healthy proxy, which is why it stays opt-in
behind the flag while everything above it doesn't.

Check current state with `urnet-tools self-heal status`, which prints the
on/off toggle plus the live pressure score, churn score, per-component
breakdown, and target pool size from `~/.urnetwork/pressure_status`. The
pressure score is included as `pressure` in bandwidth hub reports.

> [!NOTE]
> The ramp anchors (PSI 10%/60%, MemAvailable 25%/5%, load 1.0/3.0 per core,
> etc.) are properties of what each metric means — e.g. "a box stalled on
> memory 60% of the time is exhausted" holds regardless of core count or RAM
> size. They are not per-server capacity tuning knobs.
