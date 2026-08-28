# ⚙️ Configuration Reference

## 🌍 Environment Variables

Quick jump:
- [Authentication & Identity](#-authentication--identity)
- [Performance & Hardware Profiles](#-performance--hardware-profiles)
- [Proxy Feeds & Reaping](#-proxy-feeds--reaping)
- [Self-Healing & Resource Pressure](#-self-healing--resource-pressure)
- [Monitoring & Telemetry](#-monitoring--telemetry)

### 🔑 Authentication & Identity

| Variable | Default | Description |
| :--- | :--- | :--- |
| `BUILD` | `stable` | Set to `jwt` for auth code login, or `stable` for email/password auth. |
| `USER_AUTH` | - | Your email. Required if `BUILD=stable`. Also used for **self-healing** in `BUILD=jwt` mode to refresh expired tokens. |
| `PASSWORD` | - | Your password. Required if `BUILD=stable`. Also used for **self-healing** in `BUILD=jwt` mode to refresh expired tokens. |
| `URNETWORK_AUTH_CODE` | - | First-run auth code for `BUILD=jwt`. Use this instead of passing the code as a trailing command argument. Ignored once a JWT exists in the volume. |
| `UR_API_URL` | `https://api.bringyour.com` | Custom API URL, for operators running their own backend. Must be set together with `UR_CONNECT_URL`. Applied once at startup via `provider choose_network` and persisted to `~/.urnetwork/network.json`, so it survives restarts if that directory is on a volume, same as the JWT. |
| `UR_CONNECT_URL` | `wss://connect.bringyour.com` | Custom connect (WebSocket signaling) URL, for operators running their own backend. Must be set together with `UR_API_URL`. See `UR_API_URL`. |
| `URNETWORK_NODE_NAME` | hostname / redacted IP | Friendly label for dashboard identity and webhook alerts. |
| `HOST_HOSTNAME` | - | Pass the host server name into the container. Use `-e HOST_HOSTNAME=$(hostname)` with `docker run` or `HOST_HOSTNAME=${HOSTNAME}` in Compose. |

### ⚡ Performance & Hardware Profiles

| Variable | Default | Description |
| :--- | :--- | :--- |
| `URNETWORK_PROFILE` | - | Advanced provider profile: `auto`, `lowmem`, `eco`, `turbo-v4`, or `turbo-v8`. For turbo, prefer `TURBO`. |
| `TURBO` | - | Set to `v4` or `v8` to enable turbo mode. Prefer this variable for Docker turbo mode. |
| `URNETWORK_RAMLOGS` | `0` | Set to `1` to redirect provider logs to RAM instead of stdout. Cannot be used with Docker `--log-opt`. |
| `URNETWORK_MESSAGE_POOL_SHARD_COUNT` | `16` | Number of internal mutex shards per message-pool size class. Higher values reduce lock contention at high packet rates. Must be a power of two, 1–256. Set to `1` to disable sharding (pre-v24.35 behavior). Sane values: `8` (moderate), `16` (default), `32` (high-pps tier3+). |
| `URNETWORK_SKIP_AUDIT` | `0` | Set to `1` to skip the startup system audit (disk speed benchmark, ulimit, conntrack checks). Useful in Docker where host sysctls aren't visible. |
| `GOTRACEBACK` | - | Set to `crash` to produce full goroutine stack traces on Go runtime crashes. Add `Environment="GOTRACEBACK=crash"` to the systemd override.conf. |

### 🌐 Proxy Feeds & Reaping

| Variable | Default | Description |
| :--- | :--- | :--- |
| `PROXY_URL` | - | Live proxy list URL, fetched and merged on an interval. Comma-separate for multiple sources. See [Proxy URL Sources](Proxy-URL-Sources.md). |
| `PROXY_URL_REFRESH` | `1h` | How often `PROXY_URL` is re-fetched to add new entries. |
| `PROXY_URL_MAX` | `500` | Caps total proxies sourced from `PROXY_URL`. `0` = unlimited. |
| `PROXY_DEAD_CLEANUP_SCOPE` | `url` | `none`, `url`, or `all`: which proxies the automatic dead-proxy cleanup may touch. Default `url` means only URL-sourced dead proxies are cleaned up automatically. |
| `PROXY_DEAD_CLEANUP_INTERVAL` | `6h` | Base cadence of the automatic cleanup job, when scope is not `none` (shrinks under pressure when self-heal is on). The reaper runs this often to sweep dead proxies from the allowed sources. |

### 🩹 Self-Healing & Resource Pressure

| Variable | Default | Description |
| :--- | :--- | :--- |
| `URNETWORK_SELF_HEAL` | `0` (off) | Set to `1` to enable the pressure-based self-heal system: proportional URL-fetch pacing, probe concurrency scaling, pressure-scaled cleanup/reaper cadence, and AIMD proxy-pool sizing. Off by default: with self-heal off, every actuator behaves exactly as it did before this system existed. Toggle at runtime with `urnet-tools self-heal on`, `urnet-tools self-heal off`, or `urnet-tools self-heal status` (no restart required; the monitor starts sensing within ~30s). |
| `URNETWORK_ADAPTIVE_GC` | on | Consolidated adaptive GC governor in the pressure monitor. On by default for every profile. It tightens GOGC below the profile baseline under memory pressure: the tighter of process heap fraction and host available RAM wins. Set to `0`, `false`, `off`, or `no` to disable it. If the operator sets `GOGC` directly, the governor backs off entirely and never touches the knob. |

### 📊 Monitoring & Telemetry

| Variable | Default | Description |
| :--- | :--- | :--- |
| `ENABLE_VNSTAT` | `true` | Enables the traffic monitor on port 8080. |
| `ENABLE_IP_CHECKER` | `false` | Diagnostic only. Prints your full public IP to container logs on startup via an external script. Distinct from dashboard identity reporting, which sends only a redacted IP. |
| `URNETWORK_HEALTH_INTERVAL` | `5m` | How often to emit a `[health]` heartbeat log line. Includes uptime, RAM stats, and active connection count. Accepts Go duration strings such as `10m` or `1h`. Minimum `1m`. |
| `URNETWORK_PPROF` | - | Set to a `host:port` to enable the loopback-only diagnostics server (e.g. `127.0.0.1:6060`). Off by default. Serves `/debug/pprof/*`, `/metrics/pool`, and `/metrics/errors`; only literal loopback IPs are accepted (hostnames are rejected). Pull profiles via an SSH tunnel, e.g. `ssh -L 6060:127.0.0.1:6060 host` then `go tool pprof http://127.0.0.1:6060/debug/pprof/profile`. |
| `URNETWORK_PROXY_BENCHMARK` | - | Set to `true` to enable per-proxy latency monitoring. Off by default. Probes: TCP connect every 5 min (raw RTT to proxy port), SOCKS5 CONNECT every 15 min (end-to-end through proxy). Staggered startup jitter prevents thundering herd. ~104 GB/month at 10k proxies. |
| `URNETWORK_PROXY_BENCHMARK_ENDPOINT` | `connect.bringyour.com:443` | Target for the SOCKS5 CONNECT latency probe. Measured end-to-end through each proxy. |
| `URNETWORK_REPORT_URL` | - | HTTP URL of a bandwidth hub server. When set, the provider POSTs a JSON report with per-proxy metrics (Clients, TotalRx/Tx, BillableRx/Tx). See `hub/main.go` for the server. Can be changed at runtime without restart by writing to `~/.urnetwork/report_url` (or using `urnet-tools report <url>`). |
| `URNETWORK_REPORT_INTERVAL` | `5m` | How often bandwidth reports are posted to `URNETWORK_REPORT_URL`. Accepts Go duration strings such as `30s` or `2m`. Minimum `10s`. The `5m` default keeps the hub's historical SQLite write volume modest across a large fleet; lower it where a more live dashboard matters. |
| `URNETWORK_HEARTBEAT_INTERVAL` | `15s` | Provider heartbeat cadence to the hub (minimum `5s`). Shortening it fleet-wide can thundering-herd the hub. |
| `URNETWORK_ALERT_WEBHOOK` | - | HTTP POST endpoint for outage alerts. Fires on outage start and recovery. |
| `URNETWORK_AUTH_UNLIMITED` | `false` | Bypass the auth rate limiter; every auth attempt fires immediately. Equivalent to creating `~/.urnetwork/fast_auth`. Only for trusted or benchmark environments. |
| `URNETWORK_PUBLIC_IP` | `<detected>` | Override the public IP shown in the dashboard identity label. Display only; does not change the actual egress IP. Auto-set by Docker startup scripts. |
| `URNETWORK_SHM_LOG` | `/dev/shm/urnetwork.log` | Path for the RAM log. |
| `URNETWORK_PROXY_HEALTH_DIR` | `<home>/.urnetwork` | Directory for persistent `proxy_health.state` and `proxy_traffic.state` files (Docker: `/root/.urnetwork`). |
| `URNETWORK_CONTAINER_NAME` | `<container-id>` | Container name used in copy-paste `docker exec <name> tail -f` hints for RAM logs. |
| `WARP_HOST` | `<hostname>` | Override the host string reported by the warp status endpoint. Diagnostic only. |

## 📄 `proxy_probe.json` (Stage-1 Gate)

Location: `~/.urnetwork/proxy_probe.json`. Controls the stage-1 table-probe
quality gate for URL-source proxy admission (see
[Proxy URL Sources](Proxy-URL-Sources.md#-stage-1-quality-gate)). All
fields are optional; omitted fields keep their default. The file is
re-read on a short cache (a few seconds), so changes take effect without
a restart.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `enabled` | bool | `true` | Kill switch. `false` disables stage-1 entirely; proxies are admitted on stage-0 alone. |
| `sample_width` | int | `12` | Intended sample base width, in destination-table hosts. |
| `min_sample_width` | int | `0` | The small width a probe starts at. The paid grader forces 6. A clean verdict settles here and spends almost no probe bandwidth. |
| `max_sample_width` | int | `36` | Upper bound adaptive sample growth may reach for a borderline proxy. |
| `timeout_ms` | int | `4000` | Per-target dial timeout, in milliseconds. |
| `pass_bar` | float | `0.6` | Minimum score (fraction of successful dials) required for admission to the auth queue. |
| `preferred_bar` | float | `0.9` | Score threshold above which a proxy is marked preferred tier. |
| `border_line_band` | float | `0.15` | Half-width around the pass bar that counts a proxy as borderline. A borderline score grows the sample toward `max_sample_width`; a score farther away is a decisive verdict and stops at the base width. |
| `max_paid_probes_per_tick` | int | `200` | Cap on how many paid/file proxies one 5-minute scoring sweep probes. |
| `stage0_liveness` | bool | `false` | One-dial SOCKS5 and API reachability gate before a sample block. The paid grader forces true. |

Example, disabling the gate entirely:

```json
{"enabled": false}
```

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

> [!NOTE]
> The status server (served on the provider's `--port`) sets `ReadHeaderTimeout: 10s` and `IdleTimeout: 120s`, matching the hub, so dribbled-header (Slowloris-style) clients cannot hold connections open indefinitely; `WriteTimeout` is deliberately unset so live streams are not killed.

## 🩹 Pressure system (self-heal)

`URNETWORK_SELF_HEAL=1` (or `urnet-tools self-heal on` at runtime) turns on a resource-pressure monitor that scales several actuators proportionally instead of gating them on/off. It's off by default.

**Sensors** (sampled every 30s, worst-of-N combined, then smoothed with an asymmetric EWMA — fast to react, slow to relax):
- `/proc/pressure/memory` and `/proc/pressure/cpu` (PSI `some avg60`), where available
- `MemAvailable / MemTotal` from `/proc/meminfo`
- `loadavg1` per core (fallback where PSI is unavailable)
- Self-signals: goroutine count and heap fraction of the configured `max-memory` soft limit

These combine into a single smoothed pressure score in `[0, 1]`. A self-inflicted blowout (heap ≥90% of the soft limit, or ≥25,000 goroutines) pins the score to `1.0` immediately, bypassing smoothing.

**Actuators**, all driven off that one score:
- URL-fetch pacing stretches from 1× to 8× the configured interval as pressure rises (replaces the old binary skip-at-threshold gate)
- Proxy probe concurrency scales down toward a floor of 1 worker
- The dead-proxy cleanup job and the reaper's stale re-probe window both run *more* often under pressure (6h → 1h and 3h → 1h respectively) — cleanup and the reaper shed load, so pressure is exactly when they should run harder, not less
- An AIMD pool controller adjusts a persisted `TargetPoolSize` (stored in `proxy_url.json`) every 5 minutes: +25 proxies when calm, ×0.7 after two consecutive high-pressure samples (floor 50, capped by `PROXY_URL_MAX`). Shrinks evict the worst URL-sourced proxies first (dead, then degraded tiers, then healthy ones by ascending traffic) with a 1h re-admission backoff. This learned target only caps admission while self-heal is enabled.

Check current state with `urnet-tools self-heal status`, which prints the on/off toggle plus the live score, per-component breakdown, and target pool size from `~/.urnetwork/pressure_status`. The status file also reports `gc_state` and `heap_frac`, the adaptive GC governor's current level and live heap fraction. The same score is included as `pressure` in bandwidth hub reports.

> [!NOTE]
> The ramp anchors (PSI 10%/60%, MemAvailable 25%/5%, load 1.0/3.0 per core, etc.) are properties of what each metric means — e.g. "a box stalled on memory 60% of the time is exhausted" holds regardless of core count or RAM size. They are not per-server capacity tuning knobs.
