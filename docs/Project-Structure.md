# Project Structure

```
urnetwork-3.23-fix/
├── *.go                          # Core network stack (root package)
├── go.mod / go.sum               # Module: github.com/urnetwork/connect (Go 1.26)
├── Dockerfile                    # Alpine multi-arch provider image
├── CHANGELOG.md                  # Version history with per-release notes
├── FORK_CHANGES.md               # Delta summary vs upstream urnetwork/connect
├── CODESTYLE.md                  # Go conventions for this codebase
│
├── provider/                     # Provider CLI binary
│   ├── main.go                   # Entrypoint: auth / provide / auth-provide commands
│   ├── proxy_health_log.go       # Per-proxy health state machine and dead-proxy detection
│   ├── proxy_reload.go           # SIGHUP-triggered hot-reload of proxy list
│   ├── proxy_state.go            # In-memory proxy registry with startup stagger
│   ├── proxy_benchmark.go        # Optional per-proxy SOCKS5 latency probes
│   ├── bandwidth_reporter.go     # Posts bandwidth metrics to hub dashboard
│   ├── proxy_id.go               # Stable proxy identity across reloads
│   ├── shmlog_linux.go           # Linux shared-memory log ring buffer
│   ├── shmlog_fallback.go        # Fallback for non-Linux builds
│   ├── dup_linux_arm64.go        # ARM64-specific fd dup shim
│   ├── dup_linux_generic.go      # Generic Linux fd dup shim
│   └── Makefile                  # Cross-compile targets (amd64, arm64, darwin)
│
├── hub/                          # Fleet bandwidth dashboard server
│   ├── main.go                   # HTTP server: /api/report ingress, dashboard render
│   ├── main_test.go              # 18 unit tests for rate calculation and state logic
│   └── Dockerfile                # Containerized hub (Windows/Mac/any host, no systemd needed)
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
│   ├── legacy_cmds.go           # Parity commands (lifecycle, tuning, hub, optimize)
│   └── ...                      # + tests (~73)
│
├── scripts/                      # Installer and test scripts (installer stays shell)
│   ├── Provider_Install_Linux.sh # One-command provider install for Linux (installs the Go urnet-tools)
│   ├── Provider_Install_Win32.ps1
│   ├── Provider_Uninstall_Linux.sh
│   ├── Provider_Uninstall_Win32.ps1
│   ├── urnet-tools.ps1           # Windows helper — DEPRECATED, retired in Phase 2 (use the Go binary)
│   ├── urnetwork-updater.ps1     # Windows auto-updater
│   ├── test_provider_install.sh  # CI: validates installer script logic
│   └── test_fallback_logic.sh    # CI: validates fallback behavior
│
├── docs/                         # Operational documentation
│   ├── Configuration.md
│   ├── Docker-Deployment.md
│   ├── Installation.md
│   ├── Hub-Setup.md
│   ├── Hub-Dashboard.md
│   ├── High-Volume-Performance-Tuning.md
│   ├── Multi-Container-Scaling.md
│   ├── Proxy-Management.md
│   ├── Proxy-URL-Sources.md
│   ├── Traffic-Amplification.md
│   ├── Troubleshooting.md
│   └── design/                   # Internal design docs (proxy health, hot-reload, bandwidth)
│
├── releases/                     # Per-version release notes (v3.23.0-fix.*)
│
├── .github/workflows/
│   ├── build.yml                 # CI: parallel test-and-lint + build-and-push Docker (multi-arch)
│   ├── release.yml               # Tags a new release and uploads provider binaries
│   ├── codeql.yml                # Weekly scheduled CodeQL security scan
│   └── upstream_monitor.yml      # Twice-daily: watch urnetwork/connect PRs and commits, Discord alerts
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
    ├── transport_p2p_webrtc.go   # WebRTC P2P transport
    ├── transport_pt.go           # Pluggable tunnel transport
    ├── net_http.go               # HTTP proxy with chunked encoding
    ├── net_http_doh.go           # DNS-over-HTTPS resolver
    ├── net_resilient.go          # Platform-aware resilient connections
    ├── message_pool.go           # GC-reducing byte buffer pool
    ├── proxy_health.go           # Proxy health scoring
    ├── tuning.go                 # Auto-tuning (turbo/lowmem modes)
    ├── log.go                    # Structured logging helpers
    └── connect.go                # Top-level client/server entrypoint
```
