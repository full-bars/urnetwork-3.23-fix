# Monitoring with Prometheus and Grafana

Every provider can serve Prometheus metrics at `/metrics`. The `monitoring/` bundle runs Prometheus and Grafana with a ready-made dashboard, so you can watch a whole fleet from one page.

It takes two steps: turn metrics on at each provider, then start the bundle on any machine that can reach them.

## 1. Turn on metrics on each provider

```bash
urnet-tools metrics on
```

The command prints where the endpoint listens and what to give Prometheus:

```
Metrics: on
  listening  http://127.0.0.1:9100/metrics
  listening  http://100.64.0.10:9100/metrics
Prometheus target: 100.64.0.10:9100
```

The setting survives restarts and updates. Run `urnet-tools metrics status` at any time to see it again.

### Where the endpoint listens

| Situation | Listens on |
|---|---|
| Bare metal or VM (default) | `127.0.0.1` plus this machine's Tailscale address, if it has one |
| Docker container (default) | Every interface inside the container |
| `urnet-tools metrics listen <ip:port>` | Exactly that address |
| `URNETWORK_METRICS=<ip:port>` set in the environment | Exactly that address (overrides everything above) |

The default port is 9100. If another program already uses it (Prometheus's `node_exporter` does), the provider takes the next free port up to 9103. Use whatever `metrics status` prints.

> [!TIP]
> Tailscale is the easiest way to connect Prometheus to providers on different networks. If Tailscale starts after the provider, the provider starts serving on the Tailscale address within 30 seconds. No restart is needed.

**Without Tailscale**, pick an address Prometheus can reach, such as a private LAN or VPN address:

```bash
urnet-tools metrics listen 10.0.0.5:9100
urnet-tools metrics listen auto   # back to the default
```

> [!WARNING]
> The metrics include every proxy's address and traffic. Never expose the endpoint on a public address. `metrics listen 0.0.0.0:9100` on an internet-facing server is readable by anyone who can reach that port, unless a firewall blocks it.

### Docker

Publish the port on an address Prometheus can reach, then turn metrics on inside the container:

```bash
docker run -d --name urnetwork -p 100.64.0.10:9100:9100 ... ghcr.io/full-bars/urnetwork-3.23-fix:latest
docker exec urnetwork urnet-tools metrics on
```

Here `100.64.0.10` is the host's Tailscale address, and the Prometheus target is `100.64.0.10:9100`. For several containers on one host, publish each on its own host port (`-p 100.64.0.10:9101:9100`) and use that port as the target.

> [!WARNING]
> With `--network host`, the container shares the host's interfaces, so the default binds every host interface, including public ones. Use `urnet-tools metrics listen <tailscale-ip>:9100` there.

## 2. Run the monitoring stack

You need Docker with Compose v2 on the monitoring machine. Download `urnetwork-monitoring-<version>.tar.gz` from the [latest release](https://github.com/full-bars/urnetwork-3.23-fix/releases/latest), then:

```bash
tar xzf urnetwork-monitoring-*.tar.gz
cd monitoring
./setup.sh 100.64.0.10:9100=node-1 100.64.0.11:9101=node-2
docker compose up -d
```

Each `address=name` argument adds one provider. Use the address from `metrics status`; the name is what the dashboard shows. On first run, `setup.sh` also creates `.env` with a random Grafana admin password.

Open `http://<monitoring machine>:3000` and sign in as `admin` with the password from `.env`. The **URnetwork Providers** dashboard is the home page.

To add providers later, run `./setup.sh` again with the new ones. Prometheus picks them up within a minute, with no restart.

> [!IMPORTANT]
> Grafana listens on port 3000 on every interface, protected by the admin password. To keep it private, set `GRAFANA_BIND` in `.env` to `127.0.0.1` or the machine's Tailscale address, then run `docker compose up -d` again.

### Settings

Add any of these to `.env`, then run `docker compose up -d`:

| Variable | Default | Meaning |
|---|---|---|
| `GRAFANA_BIND` | `0.0.0.0` | Address Grafana listens on |
| `GRAFANA_PORT` | `3000` | Grafana port |
| `PROMETHEUS_PORT` | `9090` | Prometheus port, always on `127.0.0.1` |
| `PROMETHEUS_RETENTION` | `30d` | How long Prometheus keeps data |

## What the dashboard shows

- **Fleet:** nodes up and down, billable throughput, active clients, and proxies up or failing. A table lists each node's version, uptime, clients, proxies, memory, and resource pressure.
- **Traffic:** billable and total bytes per second, clients per node, and the top 15 proxies by billable traffic.
- **Proxies:** pool status, grades, recoveries and losses, and contract outcomes.
- **Node health:** errors by category, resource pressure, memory, goroutines, sessions, and DNS-over-HTTPS failures. Two panels plot memory and file descriptors against their limits (the limit is a dashed line).
- **Lifecycle:** restarts by reason, uptime per node, HotSwap outcomes per hour, and version skew (each version in the fleet and how many nodes run it).

Use the **Node** selector at the top to narrow the view to specific providers.

The memory, descriptor and restart-reason panels need a provider that exports `urnet_mem_limit_bytes`, `urnet_rss_bytes`, `urnet_open_fds`, `urnet_fd_limit` and `urnet_restart_reason`. On older providers those panels stay empty and the rest of the dashboard works as before. Descriptor metrics are Linux only.

## Alerts

The bundle ships alert rules in `monitoring/prometheus/rules/urnetwork.yml`. Prometheus loads them at startup, and `docker compose up -d` mounts the `rules` directory for you. Firing alerts show at `http://127.0.0.1:9090/alerts`.

> [!NOTE]
> The bundle has no Alertmanager. Alerts are visible in Prometheus and as the `ALERTS` series, but nothing is emailed or posted until you add an Alertmanager and point Prometheus at it.

### Enabled by default

| Alert | Fires when | Does not detect |
|---|---|---|
| `UrnetworkNodeDown` | A node's `/metrics` endpoint has not answered for 5 minutes (`up == 0`). | A stopped provider looks the same as metrics switched off, a blocked port or a Tailscale outage. A node removed from `targets/providers.yml` never fires. It says nothing about whether a reachable node is earning. |
| `UrnetworkRestartLoop` | More than 3 restarts of one node in an hour. | A restart that hides between two scrapes, and the cause of a restart. Check `urnet_restart_reason` and the node's logs. |

`UrnetworkRestartLoop` counts drops of `urnet_uptime_seconds` with `resets()`. It does not use `urnet_startup_restarted`, because that gauge keeps one fixed value for the life of a process, so it cannot be counted with `increase()`.

### Optional rules

These are in the same file, commented out, because they need thresholds that suit your fleet.

| Alert | Fires when | Does not detect |
|---|---|---|
| `UrnetworkOldVersion` | A node runs a different version from most of the fleet for 7 days. | Which version is newer. During a slow rollout it can flag the early upgraders. It does not fire on a fleet that is uniformly out of date. |
| `UrnetworkMemoryNearLimit` | `urnet_mem_sys_bytes` is above 90% of `urnet_mem_limit_bytes` for 15 minutes. | Resident memory, other processes on the machine, and a container limit lower than the Go limit. Nodes with no limit set are skipped. |
| `UrnetworkFileDescriptorsNearLimit` | `urnet_open_fds` is above 85% of `urnet_fd_limit` for 10 minutes. | Which kind of descriptor leaks, and a spike between two scrapes. |
| `UrnetworkNoBillableTraffic` | A node is up, has been up for over 30 minutes, and billed no traffic for 30 minutes. | A broken node versus a quiet one with no demand. It can be noisy on idle nodes. |

To enable one, open `monitoring/prometheus/rules/urnetwork.yml`, read the note above the rule, adjust the threshold, and remove the leading `# ` from every line of that block (the block ends at the next blank line). Then reload Prometheus:

```bash
curl -X POST http://127.0.0.1:9090/-/reload
```

Nodes whose provider does not export a metric never match its rule, so a fleet with mixed versions is safe.

### Checking your changes

If you have `promtool` (it ships with Prometheus), run these from the `monitoring/` directory after editing the rules:

```bash
promtool check rules prometheus/rules/urnetwork.yml
promtool test rules prometheus/rules/urnetwork.test.yml
```

The test file covers the two enabled rules. `prometheus.yml` lists `urnetwork.yml` by name, so the test file is never loaded as rules.

## Troubleshooting

**A node shows as down.** On that node, run `urnet-tools metrics status` and check that the address matches the target in `prometheus/targets/providers.yml`. From the monitoring machine, `curl http://<target>/metrics | head` should print metrics. `connection refused` means nothing listens on that address. A timeout usually means a firewall or Tailscale ACL is blocking the port.

**The target in Prometheus shows an error.** Open `http://127.0.0.1:9090/targets` on the monitoring machine. The error column says why the last scrape failed.

**Panels are empty right after starting.** Rates need two scrapes, so allow about a minute.

## Metrics reference

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `urnet_info` | gauge | `version` | Always 1; carries the provider version |
| `urnet_uptime_seconds` | gauge | | Provider uptime |
| `urnet_billable_bytes_total` | counter | `direction` | Billable bytes, all proxies |
| `urnet_bytes_total` | counter | `direction` | All bytes, all proxies |
| `urnet_clients_active` | gauge | | Active clients, all proxies |
| `urnet_connections_active` | gauge | | Active connections |
| `urnet_proxy_pool_size` | gauge | `status` | Proxies by status: up, connecting, degraded, dead |
| `urnet_proxy_bytes_total` | counter | `proxy`, `direction` | Bytes per proxy |
| `urnet_proxy_billable_bytes_total` | counter | `proxy`, `direction` | Billable bytes per proxy |
| `urnet_proxy_clients` | gauge | `proxy` | Active clients per proxy |
| `urnet_proxy_session_age_seconds` | gauge | `proxy` | Length of the current client presence window per proxy |
| `urnet_proxies_known` | gauge | | Proxies in the provider's proxy state |
| `urnet_proxy_grades` | gauge | `tier` | Proxies by grade |
| `urnet_proxy_health` | gauge | `status` | Proxies by recorded health |
| `urnet_proxy_graded_recent` | gauge | | Proxies graded in the last hour |
| `urnet_proxy_graded_stale` | gauge | | Proxies with an older grade |
| `urnet_proxy_auth_failures` | gauge | | Auth failures summed over current proxies |
| `urnet_url_proxy_grades` | gauge | `tier` | URL-sourced proxies by grade |
| `urnet_url_proxy_ungraded` | gauge | | URL-sourced proxies not yet graded |
| `urnet_contracts_total` | counter | `result` | Contract outcomes since start |
| `urnet_errors_total` | counter | `category` | Errors since start |
| `urnet_doh_failures_total` | counter | | DNS-over-HTTPS failures |
| `urnet_sessions_pqe` | gauge | | Active post-quantum sessions |
| `urnet_sessions_classical` | gauge | | Active classical sessions |
| `urnet_sessions_opened_total` | counter | `encryption` | Sessions opened, lifetime |
| `urnet_sessions_opened_recent` | gauge | `window`, `encryption` | Sessions opened in the last hour, day, or week |
| `urnet_lifetime_sessions_total` | counter | `encryption` | Sessions, persisted across restarts |
| `urnet_lifetime_contracts_total` | counter | `result` | Contract outcomes, persisted across restarts |
| `urnet_lifetime_proxies_total` | counter | `event` | Proxy recoveries and losses, persisted across restarts |
| `urnet_lifetime_billable_bytes_total` | counter | | Billable bytes, persisted across restarts |
| `urnet_lifetime_errors_total` | counter | `category` | Errors, persisted across restarts |
| `urnet_hotswap_outcomes_total` | counter | `reason` | HotSwap attempt outcomes |
| `urnet_control_commands_total` | counter | `cmd` | Control socket commands |
| `urnet_startup_clean_shutdown` | gauge | | 1 if the previous run shut down cleanly |
| `urnet_startup_restarted` | gauge | | 1 if the previous run did not shut down cleanly |
| `urnet_startup_upgraded` | gauge | | 1 if the version changed since the previous run |
| `urnet_startup_previous_version` | gauge | `version` | Always 1; the version before the last upgrade |
| `urnet_pressure_score` | gauge | | Resource pressure, 0 (fine) to 1 (emergency) |
| `urnet_mem_heap_bytes` | gauge | | Go heap in use |
| `urnet_mem_sys_bytes` | gauge | | Memory obtained from the OS |
| `urnet_mem_limit_bytes` | gauge | | The Go memory limit in effect |
| `urnet_rss_bytes` | gauge | | Resident set size (Linux) |
| `urnet_open_fds` | gauge | | Open file descriptors (Linux) |
| `urnet_fd_limit` | gauge | | File descriptor limit (Linux) |
| `urnet_restart_reason` | gauge | `reason` | Value 1 for the reason the current process started; absent for other reasons |
| `urnet_gc_cycles_total` | counter | | Garbage collection cycles |
| `urnet_goroutines` | gauge | | Goroutines |
| `urnet_pool_latency_ms` | gauge | | Message pool average latency |
