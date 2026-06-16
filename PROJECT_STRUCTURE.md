# Project Structure

This document outlines the high-level architecture and directory structure of the **URNetwork 3.23-fix** fork. It highlights the major subsystems and the custom components introduced in this fork (e.g., the Hub, proxy health tracking, tuning, and installer scripts).

## Directory Layout

```
urnetwork-3.23-fix/
├── provider/
│   ├── main.go                    # Provider entrypoint, settings parsing, and graceful shutdown
│   ├── proxy_health_log.go        # Durable state persistence for proxy health (disk writer)
│   ├── proxy_reload.go            # Hot-reload engine via .reload trigger files
│   ├── proxy_id.go                # Stable monotonic proxy ID assignment (e.g., proxy[0])
│   ├── proxy_benchmark.go         # Opt-in staggered latency probing (TCP and SOCKS5)
│   ├── bandwidth_reporter.go      # Pushes periodic JSON telemetry to the Hub
│   ├── start_stable.sh            # Docker entrypoint for stable configuration
│   ├── start_jwt.sh               # Docker entrypoint with JWT auto-refresh healing
│   └── Makefile                   # Multi-architecture (amd64/arm64) build targets
│
├── hub/
│   └── main.go                    # Standalone Bandwidth Hub server; provides :8080 JSON API and HTML dashboard
│
├── scripts/
│   └── Provider_Install_Linux.sh  # Bare-metal installer & `urnet-tools` CLI (hub, logs, optimize, proxy cmds)
│
├── docker/
│   └── scripts/                   # Docker-specific helper scripts (e.g., proxy-health.sh)
│
├── Dockerfile                     # Alpine-based, multi-stage, multi-arch build with vnStat integration
├── FORK_CHANGES.md                # Comprehensive documentation of all modifications made vs upstream
├── PROGRESS.md                    # Active development tracker
│
# Core Library Components (Root)
├── net_http.go                    # Control-plane dialing logic & dialer selection (EWMA / Priority)
├── transport.go                   # High-level data plane, auth/OOB failure handling, outage detection, rate-limiting
├── proxy_health.go                # Core O(1) indexed tracker for proxy bandwidth, status, and failure counters
├── transfer.go                    # Data plane sequencer and message loop
├── transfer_encrypt.go            # TLS and encryption session manager (deadlock-free push architecture)
├── transfer_contract_manager.go   # Contract size limits and renegotiation logic
├── transport_pt.go                # Packet translation layers (DNS, HTTPS)
├── transport_pt_test.go           # Hardened test suite with timeout bounds
├── transfer_test.go               # Hardened encrypted transfer test suite
├── tuning.go                      # System auto-profiling (Low/Balanced/Perf) based on cgroup RAM limits
├── audit.go                       # Passive host kernel setting validator (conntrack, ulimit)
├── util.go                        # Utility functions, including Docker/cgroup RAM detection
├── message_pool.go                # Dynamic allocation pool for relay payloads
└── ip.go                          # IP-layer statistics and security policy counters
```

## Architectural Concepts

### The Provider Node (`provider/`)
The main worker. Binds to `api.bringyour.com` to authenticate and fetch a list of proxies. Our fork extends it with auto-tuning, health snapshots, outage webhooks, and the ability to hot-reload proxies without a full restart.

### The Hub (`hub/`)
A custom, zero-dependency Go binary built to solve fleet observability without requiring a full Prometheus/Grafana stack. Providers push their `[earn]` and bandwidth telemetry to the Hub, which aggregates and serves an HTML dashboard to operators.

### Installer & Tooling (`scripts/`)
`Provider_Install_Linux.sh` doubles as the `urnet-tools` CLI. It manages systemd drop-ins, applies kernel optimizations (`urnet-tools optimize`), and bridges legacy `urnetwork` commands with modern enhancements.
