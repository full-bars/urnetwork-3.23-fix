# Project Structure

```
urnetwork-3.23-fix/
├── *.go                          # Core network stack (root package)
├── go.mod / go.sum               # Module: github.com/urnetwork/connect (Go 1.27)
├── Dockerfile                    # Alpine multi-arch provider image
├── CHANGELOG.md                  # Version history with per-release notes
├── FORK_CHANGES.md               # Delta summary vs upstream urnetwork/connect
├── CODESTYLE.md                  # Go conventions for this codebase
│
├── provider/                     # Provider CLI binary
│   ├── main.go                   # Entrypoint: auth / provide / auth-provide commands
│   ├── proxy_health_log.go       # Per-proxy health state machine and dead-proxy detection
│   ├── proxy_slow_retry.go       # Slow-retry state: 24h daily gate, 14-day drop ceiling, persistence
│   ├── proxy_reload.go           # SIGHUP-triggered hot-reload of proxy list
│   ├── proxy_state.go            # In-memory proxy registry with startup stagger
│   ├── proxy_benchmark.go        # Optional per-proxy SOCKS5 latency probes
│   ├── bandwidth_reporter.go     # Posts bandwidth metrics to configured fleet target
│   ├── proxy_id.go               # Stable proxy identity across reloads
│   ├── shmlog_linux.go           # Linux shared-memory log ring buffer
│   ├── shmlog_fallback.go        # Fallback for non-Linux builds
│   ├── direct.go                 # Dynamic direct native connection controller
│   ├── proxy_paste.go            # Bulk proxy ingestion and format normalization
│   ├── ssrf_guard.go             # SSRF guard for proxy source URL fetching
│   ├── doh_cache.go              # Persistent DNS-over-HTTPS resolver cache
│   ├── client_jwt_hotrestart.go  # Identity snapshotting across hot restarts
│   ├── read_fd_frac_unix.go      # Linux/Unix file descriptor pressure reader
│   ├── read_fd_frac_windows.go   # Windows file descriptor pressure stub
│   ├── dup_linux_arm64.go        # ARM64-specific fd dup shim
│   ├── dup_linux_generic.go      # Generic Linux fd dup shim
│   ├── hotswap_windows.go        # Windows HotSwap via named pipes
│   ├── hotswap_unix.go           # Linux HotSwap via Unix sockets
│   ├── hotswap.go                # HotSwap handoff coordination (spawn, takeover, drain)
│   ├── control_socket.go         # Control socket server (hotswap, shutdown, status commands)
│   ├── audit_ring.go             # 1000-entry command audit ring, persisted to audit.json
│   ├── systemd_status.go         # Proxy-liveness counters and sliding STATUS= severity
│   ├── control_state.go          # Persistent provider state management
│   ├── restrict_socket_windows.go # Windows DACL socket permission hardening
│   ├── metrics_listen.go         # Prometheus /metrics HTTP listener
│   ├── metrics_provider.go       # Provider-specific metrics registration
│   ├── contract_metrics.go       # Per-contract Prometheus counters
│   ├── lifetime_metrics.go       # Provider lifetime Prometheus gauges
│   ├── oom_cap.go                # OOM-aware start cap (shadow by default) and kill switch
│   ├── action_ledger.go          # Audit ledger of capacity decisions (autopilot.jsonl)
│   ├── baseline.go               # Local behaviour record (baseline.jsonl) for upgrade comparison
│   ├── h3_direct.go              # H3 settings and eligibility for the direct identity
│   ├── node_internals.go         # Internals snapshot (goroutines, heap, transport split) served to `top`
│   ├── memory_headroom.go        # Low free-memory watcher (host or cgroup headroom log)
│   ├── resource_config_warn.go   # Startup warning when memory limits are short for the pool
│   ├── smart_dialer_probe.go     # Background connect probes for the smart dialer
│   ├── proxy_spread_sampler.go   # Content-keyed spread sampler for the stage-1 table probe
│   ├── proxy_direct_grade.go     # Read-only direct-path health grade (~/.urnetwork/direct_grade.json)
│   └── Makefile                  # Cross-compile targets (amd64, arm64, darwin)
│
├── protocol/                     # Protobuf definitions and generated Go code
│   ├── *.proto                   # Source definitions (ip, transfer, frame, extender, audit)
│   ├── *.pb.go                   # Generated — do not edit directly
│   └── Makefile                  # Runs protoc to regenerate pb.go files
│
├── extender/                     # Extender transport implementation
│   ├── extender.go               # Client-side extender connection handler
│   └── extender_test.go
│
├── connectctl/                   # connectctl CLI (upstream tool, unmodified)
│
├── api/                          # API client bindings
│   ├── bringyour.yml             # API endpoint definitions
│   └── test.sh
│
├── docker/                       # Docker startup scripts
│   ├── scripts/                  # start_jwt.sh, start_stable.sh, start_nightly.sh, urnet-tools.sh
│   └── ...                       # Selected by BUILD env var at container start
│
├── pelican/                      # Pelican game-server panel egg
│   ├── egg-urnetwork-323fix.json # PLCN_v3 egg definition (BUILD, USER_AUTH, PASSWORD, AUTHCODE)
│   └── README.md                 # Panel deployment guide, env vars, PELICAN-gated updates
│
├── workers/                      # Cloudflare Worker sources (dl.fullbars.xyz + friends)
│   ├── dl/                       # Script proxy + install.fullbars.xyz smart dispatcher/landing page
│   ├── dl-fullbars/               # latest-version + releases/download GitHub release mirror
│   ├── geo/                       # geo.fullbars.xyz — client geo/RTT info endpoint
│   ├── provider-redirect/         # provider.fullbars.xyz -> GitHub 301 redirect
│   └── README.md                  # Routes, deploy notes, wrangler usage
│
├── cmd/                         # Go binaries (v3.23.0-fix.27.0+)
│   ├── urnet-tools/             # Provider-aware fleet ops tool (process/systemd)
│   └── urnet-docker/            # Container variant (docker exec delegation)
│
├── internal/urnettools/         # Shared Go core for the urnet-tools binaries
│   ├── cli.go                   # Dispatch, flag parsing, confirm gates
│   ├── target.go                # Targeting (--unit/--user/--network/--network-id/--state-dir)
│   ├── discover.go              # Provider discovery (/proc + systemd units)
│   ├── update.go                # Interactive-first update, digest verify, atomic swap
│   ├── legacy_cmds.go           # Reporting config, lifecycle, tuning, and proxy commands
│   ├── autopilot.go             # autopilot log: timeline of capacity decisions (OOM cap, trim)
│   ├── baseline.go              # baseline show/mark/compare: this box's own record
│   ├── internals.go             # Internals snapshot for the `top` panel (transport split)
│   ├── lifecycle_start_windows.go   # Windows provider start (schtasks/detached)
│   ├── lifecycle_stop_windows.go    # Windows provider stop (socket shutdown + TerminateProcess)
│   ├── lifecycle_restart_windows.go # Windows provider restart (HotSwap fallback)
│   ├── lifecycle_windows.go         # Windows lifecycle helpers (schtasks, task naming)
│   ├── lifecycle_stubs_notwindows.go # Stubs for non-Windows builds
│   ├── hotswap_windows.go           # Windows HotSwap candidate launch
│   └── ...                      # + tests (~73)
│
├── scripts/                      # Installer and test scripts (installer stays shell)
│   ├── Provider_Install_Linux.sh # One-command provider install for Linux (installs the Go urnet-tools)
│   ├── Provider_Install_Win32.ps1
│   ├── Provider_Uninstall_Linux.sh
│   ├── Provider_Uninstall_Win32.ps1
│   ├── test_provider_install.sh  # CI: validates installer script logic
│   └── test_fallback_logic.sh    # CI: validates fallback behavior
│
├── docs/                         # Operational documentation
│   ├── Configuration.md
│   ├── Docker-Deployment.md
│   ├── Installation.md
│   ├── High-Volume-Performance-Tuning.md
│   ├── Multi-Container-Scaling.md
│   ├── Proxy-Management.md
│   ├── Proxy-URL-Sources.md
│   ├── Traffic-Amplification.md
│   ├── Adding-Proxies.md
│   ├── Bittensor-Operations.md
│   ├── Troubleshooting.md
│   └── design/                   # Internal design docs (proxy health, hot-reload, bandwidth)
│
├── monitoring/                   # Prometheus + Grafana observability stack
│   ├── docker-compose.yml        # Prometheus + Grafana stack
│   ├── grafana/                  # Grafana dashboards and provisioning
│   ├── prometheus/               # Prometheus scrape configuration
│   └── setup.sh                  # Monitoring stack setup script
│
├── releases/                     # Per-version release notes (v3.23.0-fix.*)
│
├── .github/workflows/
│   ├── build.yml                 # CI: parallel test-and-lint + build-and-push Docker (multi-arch)
│   ├── release.yml               # Tag push: builds provider binaries (multi-GOOS), scans (VirusTotal + ClamAV), publishes the release
│   ├── shakedown.yml             # Pre-release shakedown: fresh-droplet install + proxy + URL + docker test on v3.23.0-fix.* tags
│   ├── docker-shakedown.yml      # Docker shakedown on v3.23.0-fix.* tags
│   ├── codeql.yml                # Weekly scheduled CodeQL security scan
│   ├── dash-compat.yml           # Dash/POSIX compatibility check
│   ├── unix-lifecycle.yml        # Unix lifecycle verification (push to main + PR)
│   ├── windows-lifecycle.yml     # Windows lifecycle verification (push to main + PR)
│   ├── lifecycle-stress.yml      # Lifecycle stress test (PRs touching provider/, daily, manual)
│   ├── docker-multi-container.yml # Multi-container scaling test (fix/* pushes + manual)
│   ├── tool-functional-smoke.yml # urnet-tools functional smoke (main + PR, manual)
│   ├── functional-soak.yml       # Three-hour functional soak (manual only)
│   ├── cfaa-blocklist-sync.yml   # Twice-daily CFAA blocklist data refresh from upstream
│   ├── upstream_monitor.yml      # Twice-daily: watch urnetwork/connect PRs and commits, Discord alerts
│   ├── wiki-sync.yml             # On docs/** push: copy docs/*.md into the GitHub wiki
│   └── pr-labeler.yml            # Label pull requests
│
└── Root package files (core network stack):
    ├── ip.go                     # IP packet relay core (81 KB)
    ├── ip_remote_multi_client.go # Multi-client relay coordinator
    ├── ip_security.go            # Pre-compiled IP blocklist (2.3 MB)
    ├── transfer.go               # Contract-based bandwidth allocation
    ├── transfer_contract_manager.go  # Per-proxy contract lifecycle
    ├── transfer_route_manager.go     # Adaptive routing with RTT tracking
    ├── transfer_rtt.go           # RTT window measurement
    ├── transfer_encrypt.go       # Per-contract encryption
    ├── transfer_queue.go         # Backpressure queue
    ├── transport.go              # Transport abstraction layer
    ├── transport_h3_gate.go      # Runtime on/off gate for the H3 transport
    ├── transport_h3_counted.go   # Byte-counting wrapper for the H3 UDP socket
    ├── transport_h3_datagram.go  # QUIC DATAGRAM message layer and receive route
    ├── transport_h3_datagram_lane.go  # Bounded send lane for outgoing datagrams
    ├── transport_h3_datagram_state.go # DATAGRAM counters shown on the [health] line
    ├── transport_h3_memory.go    # H3 memory bound
    ├── transport_mode_stats.go   # Payload frames and bytes per transport mode
    ├── transport_p2p_webrtc.go   # WebRTC P2P transport
    ├── transport_pt.go           # Pluggable tunnel transport
    ├── net_http.go               # HTTP proxy with chunked encoding
    ├── net_http_doh.go           # DNS-over-HTTPS resolver
    ├── net_http_smart_dialer.go  # Smart dialer: measured-cost transport preference (off by default)
    ├── emoji/                    # Network tag validation and suggestion (copied from upstream)
    ├── net_resilient.go          # Platform-aware resilient connections
    ├── message_pool.go           # GC-reducing byte buffer pool
    ├── proxy_health.go           # Proxy health scoring
    ├── tuning.go                 # Auto-tuning (turbo/lowmem modes)
    ├── log.go                    # Structured logging helpers
    ├── metrics_prometheus.go     # Prometheus metrics registry and collector
    └── connect.go                # Top-level client/server entrypoint
```
