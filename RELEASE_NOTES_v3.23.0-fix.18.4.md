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

---

### New: Proactive JWT Renewal

The provider now checks the JWT expiry every hour and proactively refreshes the token when within 48 hours of its `exp` claim — no restart, no exit-78 blip.

JWTs are issued with a 30-day lifetime. The 48-hour renewal window gives ~46 hours of retry cushion. If the API is unreachable during a refresh attempt, the next hourly check retries automatically. The exit-78 path remains as a last resort.

Existing connections continue working throughout — the JWT on disk is only read at startup or during re-auth; live sessions have their own auth state.

No configuration required. The feature is always enabled with zero overhead between monthly refresh cycles.

#### New log lines

```
[jwt] refreshing token — expires in 10h 0m (less than 48h 0m threshold)
[jwt] refresh failed: api error: ... (will retry in 1h)
[jwt] token refreshed successfully (next check in 1h)
```

### Changes
- `provider/main.go` — added `runJWTRefresher` goroutine, `refreshJWT` and `parseJWTExpiryTime` helpers
- `LOG_REFERENCE.md` — documented `[jwt]` log lines

---

### New: shmLogFatal & Unique Exit Codes

Every fatal error path now writes a `FATAL [exit <code>]: ...` line to both stderr and the ramlog file before terminating. The message is guaranteed to be on disk because `shmLogFatal` writes directly to `/dev/shm/urnetwork.log` (bypassing the pipe goroutine) before calling `os.Exit`.

Each failure path has a documented exit code so operators can triage from the exit code alone. See `FORK_CHANGES.md#exit-code-reference` for the full table.

### Changes
- `provider/shmlog_linux.go` — added `shmLogFatal` (direct ramlog write + stderr + exit)
- `provider/main.go` — replaced all `fmt.Fprintf` + `os.Exit(n)` with `shmLogFatal(code, ...)`
- `docs/Troubleshooting.md` — full troubleshooting guide with exit code reference
- `FORK_CHANGES.md` — exit code reference table
- GitHub Wiki Troubleshooting Guide — same content
