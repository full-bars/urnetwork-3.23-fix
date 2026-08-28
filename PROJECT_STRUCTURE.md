# Project Structure

This document outlines the high-level architecture and directory structure of the **URNetwork 3.23-fix** fork. It highlights the major subsystems and the custom components introduced in this fork (e.g., the Hub, proxy health tracking, tuning, and installer scripts).

## Directory Layout

```
urnetwork-3.23-fix/
├── provider/
│   ├── main.go                    # Provider entrypoint, settings parsing, and graceful shutdown
│   ├── auth_rate_limiter.go       # Global adaptive auth rate limiter (AIMD: 20-200 req/s)
│   ├── proxy_admission_gate.go    # Weighted-lottery admission gate for auth slots
│   ├── proxy_failure_history.go   # Persistent per-proxy failure count (across requeues)
│   ├── proxy_auth_history.go      # Proven-proxy set for rate limiter gating
│   ├── proxy_probe.go             # Dual-stage SOCKS5 probe (TCP + API CONNECT)
│   ├── proxy_url.go               # Proxy URL state persistence (proxy_url.json)
│   ├── proxy_url_source.go        # URL fetcher, merge, periodic refresh, reaper, blacklist
│   ├── proxy_reload.go            # Hot-reload engine via .reload trigger files + give-up cooldown
│   ├── proxy_state.go             # On-disk proxy state management (proxy.state)
│   ├── proxy_id.go                # Stable monotonic proxy ID assignment (e.g., proxy[0])
│   ├── proxy_health_log.go        # Durable state persistence for proxy health (disk writer)
│   ├── proxy_slow_retry.go        # Slow-retry state: 24h daily gate, 14-day drop ceiling, persistence
│   ├── proxy_benchmark.go         # Opt-in staggered latency probing (TCP and SOCKS5)
│   ├── proxy_match.go             # Pattern-based proxy removal (proxy remove --match)
│   ├── contract_metrics.go        # Fleet-wide per-proxy contract history tracking
│   ├── bandwidth_reporter.go      # Pushes periodic JSON telemetry to the Hub
│   ├── important_log.go           # Important-event log (/dev/shm/urnetwork-important.log)
│   ├── tlog.go                    # Thread-safe timestamped logging helpers
│   ├── shmlog.go                  # Rolling ring-buffer RAM log (/dev/shm/urnetwork.log)
│   ├── shmlog_fallback.go         # Fallback RAM logger for non-Linux
│   ├── dup_linux_arm64.go         # ARM64 platform-specific stubs
│   ├── dup_linux_generic.go       # Generic Linux platform-specific stubs
│
├── hub/
│   ├── main.go                    # Standalone Bandwidth Hub server; provides :8080 JSON API and HTML dashboard
│   ├── broadcaster.go             # SSE event broadcaster for live dashboard updates
│   ├── proxy_api.go               # /api/proxies/* leaderboard and history endpoints
│   ├── proxy_delta.go             # Delta-based proxy bandwidth ingestion and rollups
│   └── store_db.go                # SQLite persistence for node/proxy history and snapshots
│
├── scripts/
│   └── Provider_Install_Linux.sh  # Bare-metal installer & `urnet-tools` CLI (hub, logs, optimize, proxy cmds)
│
├── docker/
│   └── scripts/                   # Docker-specific helper scripts (entrypoint.sh, start_*.sh, urnet-tools.sh, proxy-health.sh, proxy-traffic.sh, logs.sh)
│
├── Dockerfile                     # Alpine-based, multi-stage, multi-arch build with vnStat integration
├── FORK_CHANGES.md                # Comprehensive documentation of all modifications made vs upstream
├── progress.md                    # Active development tracker
│
# Core Library Components (Root)
├── connect.go                     # Core types: TransferPath, Id (16-byte ULID), ByteCount
├── net.go                         # TCP/TLS dialing with SOCKS5 proxy support (trackedConn)
├── net_http.go                    # Control-plane dialing & ClientStrategy (Normal/Resilient/Extender)
├── net_http_doh.go                # DNS-over-HTTPS resolver with caching
├── transport.go                   # PlatformTransport: WebSocket (H1) + QUIC/H3 + DNS PT
├── transport_p2p.go               # P2P WebRTC transport
├── transport_p2p_webrtc.go        # WebRTC peer connection management
├── transport_pt.go                # Pluggable transport: DNS packet translation
├── transport_pt_codec.go          # DNS encode/decode for packet translation
├── transport_pt_queue.go          # Fragment reassembly and pump queue
├── proxy_health.go                # O(1) indexed tracker for proxy bandwidth, status, failures
├── transfer.go                    # Client state machine: sequenced frames, contracts, encryption
├── transfer_contract_manager.go   # Contract creation, taking, size ramping, provide modes
├── transfer_route_manager.go      # Transport interface + RouteManager for weighted multi-transport
├── transfer_control.go            # In-band control sync (retry loop)
├── transfer_control_oob.go        # Out-of-band control sync (registration messages)
├── transfer_encrypt.go            # TLS encryption session manager
├── transfer_encrypt_contract.go   # Contract-scoped encryption
├── transfer_key.go                # Ed25519 client key management
├── transfer_queue.go              # Ordered delivery with gap detection
├── transfer_rtt.go                # Round-trip time tracking for adaptive retransmit
├── transfer_stream_manager.go     # Multi-hop stream management
├── transfer_oob_control.go        # OOB control abstractions
├── tuning.go                      # System auto-profiling (Tier1/Tier2/Tier3) based on cgroup RAM
├── ip.go                          # IP-layer NAT, security policy, RemoteUserNatProvider
├── ip_security.go                 # Egress/ingress security policy (port rules, IP blocklist)
├── ip_security_dmca.go            # DPI-based BitTorrent/media fingerprint detection
├── ip_security_cfaa.go            # DMCA/CFAA-style traffic classification pipeline
├── ip_remote_multi_client.go      # Multi-client relay for remote user NAT
├── ip_packet.go                   # Raw IP packet framing
├── audit.go                       # Passive host kernel setting validator (conntrack, ulimit, disk)
├── util.go                        # Utility functions, including Docker/cgroup RAM detection
├── message_pool.go                # Dynamic allocation pool for relay payloads
├── frame.go                       # Protocol Buffer frame encoding/decoding
├── jwt.go                         # JWT auth token management
├── log.go                         # Logger interface + glog adapter
├── log_throttle.go                # Shared rate-limiting helpers for log-spam reduction
├── pulse.go                       # Periodic transport wake-up for stalled recovery
├── wakeup_schedule.go             # Delayed wake-up scheduler
└── api.go                         # Platform API client (auth, OOB control)
```
```

## Architectural Concepts

### The Provider Node (`provider/`)
The main worker. Binds to `api.bringyour.com` to authenticate and fetch a list of proxies. Our fork extends it with auto-tuning, health snapshots, outage webhooks, and the ability to hot-reload proxies without a full restart.

### The Hub (`hub/`)
A custom, zero-dependency Go binary built to solve fleet observability without requiring a full Prometheus/Grafana stack. Providers push their `[earn]` and bandwidth telemetry to the Hub, which aggregates and serves an HTML dashboard to operators.

### Installer & Tooling (`scripts/`)
`Provider_Install_Linux.sh` doubles as the `urnet-tools` CLI. It manages systemd drop-ins, applies kernel optimizations (`urnet-tools optimize`), and bridges legacy `urnetwork` commands with modern enhancements.
