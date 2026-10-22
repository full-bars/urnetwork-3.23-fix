# Project Structure

This is a map of the 3.23-fix repository. It is written for people who want to find their way around the code, not for people who only run the provider. If you only run it, start with the [README](https://github.com/full-bars/urnetwork-3.23-fix#readme).

3.23-fix is a fork of the UrNetwork Connect provider with a Go management tool (`urnet-tools`), proxy health and grading, a DNS-over-HTTPS resolver, auto-tuning, a live control socket and the Bittensor Subnet 25 tooling. The Go module is `github.com/urnetwork/connect` (Go 1.27.3, see `go.mod`); the name is kept from upstream so merges stay easy.

## Top-level directories

| Directory | What is in it |
| :--- | :--- |
| `provider/` | The provider program, the relay node you run. |
| `cmd/` | The three small programs built from this repo: `urnet-tools`, `urnet-docker` and `fake-provider` (a test double). |
| `internal/urnettools/` | The code behind `urnet-tools` and `urnet-docker`. |
| `internal/` (others) | Small shared helpers: `tui`, `promtext`, `verdictfeed`, `durablepath`, `durablehead`, `durablefixture`. |
| `./*.go` (repo root) | The core network stack: transports, contracts, IP relay and security, DNS-over-HTTPS. Details below. |
| `extender/` | The extender transport. |
| `protocol/` | Protocol Buffer definitions. Generated Go code is not edited by hand. |
| `api/` | API endpoint definitions. |
| `blocker/`, `security/` | Generators for the built-in blocklist data. |
| `durablesys/`, `durablevolume/`, `hotkeywallet/` | Storage and wallet helpers used by the Bittensor claim tools. |
| `third_party/` | Vendored dependencies we patch (see `THIRD_PARTY_LICENSES.md`). |
| `connectctl/`, `emoji/` | Upstream tools copied in as they are. |
| `docker/scripts/` | Start-up and helper scripts baked into the Docker image. |
| `scripts/` | Installers and uninstallers for Linux, FreeBSD, macOS and Windows, plus test and release-check scripts. |
| `monitoring/` | The Prometheus and Grafana bundle. See [Monitoring](/full-bars/urnetwork-3.23-fix/wiki/Monitoring). |
| `pelican/` | The Pelican game-server panel egg. |
| `workers/` | Cloudflare Worker sources for the download and install web addresses. |
| `docs/` | The documentation. The wiki-sync workflow copies `docs/*.md` and `LOG_REFERENCE.md` to the GitHub wiki on pushes to `main` that change `docs/**` or `LOG_REFERENCE.md` (plus manual dispatch). `docs/guides/` and `docs/design/` are not copied. |
| `releases/` | One release-notes file per tag (`v3.23.0-fix.*`). |
| `.github/workflows/` | CI, release, installer tests, shakedown tests, blocklist sync and wiki sync. |

## The provider (`provider/`)

The `provider` binary authenticates with the platform, starts one connection per proxy, relays traffic and keeps itself healthy. `main.go` holds the entry point and the command line (`auth`, `provide`, `proxy`, `claim`, `fleet`, `wallet`, `sn-status` and friends). The other files are grouped here by what they do. File names mostly start with the topic.

| Group | Files | What it does |
| :--- | :--- | :--- |
| Proxy lists and hot reload | `proxy_url*.go`, `proxy_reload.go`, `proxy_state.go`, `proxy_id.go`, `proxy_paste.go`, `proxy_match.go` | Read proxy files and live URL sources, reload them without a restart, remember state, and hand out stable proxy IDs. |
| Proxy admission and health | `proxy_admission_gate.go`, `proxy_failure_history.go`, `proxy_auth_history.go`, `proxy_slow_retry.go`, `proxy_probe.go`, `proxy_table_probe.go`, `proxy_spread_sampler.go`, `proxy_benchmark.go`, `proxy_health_log.go`, `proxy_warmth.go`, `proxy_udp_check.go` | Decide which proxies start, retry dead ones slowly, probe them and log their health. |
| Proxy grading and trimming | `proxy_grade_*.go`, `proxy_audit*.go`, `proxy_scout.go`, `proxy_trim.go`, `proxy_direct_grade.go`, `proxy_earnings_store.go` | A to F reachability grades, the audit that parks bad proxies, the cap that sheds the worst first, and earnings per proxy. |
| Login rate limiting | `auth_rate_limiter.go` | One shared limit on how fast proxies sign in. |
| Identity and sessions | `client_jwt_store.go`, `jwt_store_lock_*.go`, `renewal_watcher.go`, `network*.go`, `node_internals.go`, `startup_banner.go` | Client identity reuse across restarts, the saved custom network, and the start-up banner. |
| Control socket and settings | `control_socket.go`, `control_state.go`, `pending_overrides*.go`, `peercred_*.go`, `restrict_socket_*.go`, `startup_env_seed.go`, `action_ledger.go`, `audit_ring.go` | The live settings socket, queued overrides, who is allowed to talk to it, and the history of changes. |
| Hot swap and restarts | `hotswap*.go`, `restart_reason.go`, `systemd_status.go`, `dup_linux_*.go` | Replace the running binary without a gap, and record why a restart happened. |
| Self-protection | `thrash_*.go`, `mempressure*.go`, `memory_headroom.go`, `oom_cap.go`, `resource_*.go`, `liveness_*.go`, `loop_supervisor.go`, `incident_capture.go`, `client_limit_hold.go`, `pool_health.go` | Watch memory and swap, shrink the proxy count before the box falls over, exit cleanly for a supervised restart, and capture evidence when something stalls. |
| Transport choices | `h3*.go`, `h1plus_metrics.go`, `family_transport.go`, `smart_dialer_probe.go`, `socks5_udp.go`, `ssrf_guard.go`, `direct.go` | Pick HTTP/3 or HTTP/1, the IPv4 and IPv6 families, and direct (no proxy) providing. |
| Logs, metrics and status | `shmlog_*.go`, `critlog.go`, `important_log*.go`, `tlog.go`, `metrics_*.go`, `lifetime_metrics.go`, `node_snapshot*.go`, `baseline.go`, `enforcement_log.go`, `hourly_pulse.go`, `earn_tracker.go`, `verdict_feed.go` | RAM logs, the critical event log, the Prometheus endpoint, the status snapshot that `urnet-tools status` and `top` read, and the local behaviour record. |
| DNS and disk | `doh_cache.go`, `disk_bench_metrics.go` | Persistent DNS-over-HTTPS cache and disk speed checks. |
| Bittensor Subnet 25 | `sn*.go`, `snclaim_cmd.go`, `bt_*.go`, `claim_*.go`, `fleet*.go`, `hotkey_wallet.go`, `wallet_*`, `operator_clientauth.go` | Network wallet consent, claims and claim daemons, fleet registration, hotkey delegation and multi-operator provide. The operator guides are the `docs/Bittensor-*.md` pages. |

`provider/README.md` has more about running the binary directly.

## urnet-tools (`cmd/urnet-tools` and `internal/urnettools/`)

`urnet-tools` is the operator's management program, a single Go binary on Linux, FreeBSD, macOS and Windows. `urnet-docker` is the same code aimed at containers. It finds every provider on the machine and refuses to act when the target is ambiguous.

| Group | Files | What it does |
| :--- | :--- | :--- |
| Command line | `cobra*.go`, `cli*.go`, `legacy_cmds.go`, `target.go`, `default_provider.go` | The commands, their help text and how a target provider is chosen. |
| Finding providers | `discover*.go`, `docker*.go`, `namespace_guard.go`, `state_dir_*.go` | Look for providers in systemd, OpenRC, launchd, FreeBSD rc.d, Windows tasks and containers. |
| Start, stop, restart | `lifecycle_*.go`, `openrc*.go`, `provider_recover*.go`, `restart_*.go`, `idle_update.go` | Per-OS service control. |
| Updating | `update.go`, `release.go`, `hotswap*.go`, `self_heal.go`, `tool_path_*.go` | Download a release, check its digest, swap the binary, and update the tool itself. |
| Settings and proxies | `proxy*.go`, `config.go`, `session_cmds.go`, `control_client.go`, `pending_overrides_lock_*.go`, `ramlogs_detect.go`, `ip_detect.go` | Add and remove proxies, show every setting and where it came from, save and load a session. |
| Status and history | `top_*.go`, `snapshot*.go`, `usage*.go`, `metrics_status.go`, `autopilot.go`, `baseline.go`, `enforcement.go`, `internals.go`, `log_stream.go` | `top`, `status`, traffic accounting, and the logs and history views. |
| Subnet 25 | `wallet.go`, `sn_status.go` | Wallet consent and `sn-status`. |

The tests (`*_test.go`) sit next to the code they check.

## The core network stack (repo root)

The Go files in the repo root are one package. Names start with the topic.

| Prefix | What it does |
| :--- | :--- |
| `transfer*.go` | Contracts, the send and receive queues, encryption, routing and round-trip timing. |
| `transport*.go` | Connecting to the platform: HTTP/1, HTTP/1+ (`transport_h1plus.go`), HTTP/3 and its datagram lane (`transport_h3_*.go`), WebRTC peer-to-peer, and the pluggable tunnel (`transport_pt*.go`). `transport_family*.go` runs separate IPv4 and IPv6 transports. |
| `reconnect_*.go`, `govern_*.go`, `governor_*.go` | Limits that stop reconnect storms and record what the governors enforced. |
| `ip*.go` | Packet relay, fragments, ICMP, UDP polling, and the security policy (`ip_security*.go`: blocklists and traffic classification). |
| `net*.go` | HTTP, DNS-over-HTTPS (`net_http_doh*.go`), the smart dialer and resilient connections. |
| `proxy_health.go`, `proxy_scout_score.go`, `proxy_target_family.go` | Proxy health scoring shared with the provider. |
| `routing_class*.go`, `socket_bytes.go`, `platform_*.go`, `stun_*.go` | Traffic classes, byte accounting, platform limits and STUN checks. |
| `tuning.go`, `audit*.go`, `sysmem_*.go`, `memory_budget.go`, `message_pool.go` | Auto-tuning by RAM, the start-up system auditor and buffer pooling. |
| `metrics_prometheus.go`, `log*.go`, `trace.go`, `profiling.go` | Metrics and logging helpers. |
| `jwt.go`, `api*.go`, `connect.go` | Sign-in tokens, the platform API client and the top-level client entry point. |

## Docs, wiki and releases

Everything in `docs/*.md` is copied to the [GitHub wiki](https://github.com/full-bars/urnetwork-3.23-fix/wiki) by `.github/workflows/wiki-sync.yml` when it changes on `main`. The README, this file, `CHANGELOG.md`, `FORK_CHANGES.md` and `LOG_REFERENCE.md` live in the repo root and are not copied. See also [FORK_CHANGES](https://github.com/full-bars/urnetwork-3.23-fix/blob/main/FORK_CHANGES.md) for the detailed history of every change, and [LOG_REFERENCE](https://github.com/full-bars/urnetwork-3.23-fix/blob/main/LOG_REFERENCE.md) for what each log line means.

## The hub (removed)

The fleet-hub dashboard (the `hub/` package and the `urnet-tools hub` commands) was removed in v32.0 (FORK_CHANGES section 165). Fleet visibility is now the Prometheus `/metrics` endpoint (`urnet-tools metrics on`) with the [Monitoring bundle](/full-bars/urnetwork-3.23-fix/wiki/Monitoring).
