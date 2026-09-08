### **Unreleased (v3.23.0-fix.31.0 Draft)**

Full draft release notes: [releases/v3.23.0-fix.31.0.md](v3.23.0-fix.31.0.md)

#### Added
- **Provider Daemon Architecture (Flagship)**: Decoupled provider runtime into a standalone background daemon with local control socket IPC (`provider.sock` / Windows named pipes) and `urnet-tools` client support.
- **Offline Pending Configuration Queue**: Queues `urnet-tools set` updates to `~/.urnetwork/pending_overrides.json` when the provider daemon is offline, merging and applying settings one-shot on startup.
- **Zero-Downtime HotSwap (In Flight)**: In-place binary upgrades with socket inheritance and graceful connection drain protocol, eliminating 20–60s upgrade downtime down to 0s downtime.
- **Public IP Autodetection & Dashboard Rename (PR #534)**: Automatically discovers public IPv4 address with caching, 60s TTL, and concurrent request deduplication; added `urnet-tools rename` to modify dashboard display names dynamically.
- **JWT Auto-Refresh Transfer Stats Telemetry**: Step 3/3 of JWT rotation reads `paid_bytes_provided` and `unpaid_bytes_provided` from `GET /transfer/stats`, reporting account balances inline in human-readable units (`unpaid: X, paid: Y`).
- **ICE IPv6 Host Candidate Egress Guard**: Gated synthetic IPv6 host candidates behind `egressIPv6Usable()` send probes to prevent dead cellular routes on Android from blackholing WebRTC traffic.
- **Systemd Key Convergence**: Migrates `URNETWORK_PROFILE`, `URNETWORK_RAMLOGS`, `GOMEMLIMIT`, and `GOGC` into control-socket runtime state.
- **Automated CFAA Blocklist Synchronizations (PR #540, #542)**: Synchronized Computer Fraud and Abuse Act IP blocklists with upstream definitions.
