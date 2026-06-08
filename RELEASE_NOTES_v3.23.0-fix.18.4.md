# Release Notes Draft

## v3.23.0-fix.18.4

### New: Bandwidth Hub Reporting

Adds an opt-in system to report per-proxy metrics to a central hub for fleet-wide monitoring.

#### How to use

**On each provider**, set the `URNETWORK_REPORT_URL` environment variable to your hub server URL:

```bash
URNETWORK_REPORT_URL=http://your-hub:8080 /path/to/provider_bin provide ...
```

The provider will POST a JSON report every 60 seconds with per-proxy Clients, throughput (TotalRx/TotalTx, BillableRx/BillableTx), and system info (version, uptime, proxy count).

**Run the hub server** anywhere reachable by your providers:

```bash
go run ./hub/main.go
```

Or build and deploy:

```bash
go build -o hub ./hub/main.go
./hub
```

The hub serves:
- `POST /api/report` — receives reports from providers
- `GET /api/nodes` — returns all node/proxy data as JSON
- `GET /` — HTML dashboard (auto-refresh every 30s)

Data is persisted atomically to `hub.json` in the working directory.

### Changes
- `provider/bandwidth_reporter.go` — new reporting goroutine
- `hub/main.go` — new standalone hub server (stdlib only, zero deps)
- `provider/main.go` — wiring to start reporter when env var is set

---

### New: Accurate Proxy Client Count & MaxAge

`ProxyBandwidth.Clients` and `MaxAge()` now reflect user traffic sources, not SOCKS5 connection count. The persistent platform WebSocket no longer inflates these metrics.

- `ip.go` — session tracking moved from SOCKS5 dialer to NAT buffer layer; source tracking uses the existing `sourceSequences` map (which was previously dead code)
- `net.go` — removed spurious Clients/session counting from the SOCKS5 proxy dialer; byte counting remains
