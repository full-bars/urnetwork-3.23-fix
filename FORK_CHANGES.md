# URNetwork 3.23-fix Fork — Custom Changes

This document tracks all modifications made to the upstream URNetwork v3.23 codebase in this fork. Use this as a reference when rebasing to newer upstream versions.

**Fork Based On**: urnetwork/connect v3.23  
**Repository**: github.com/full-bars/urnetwork-3.23-fix  
**Current Version**: v3.23.0-fix.30.4

---

## 1. Enhanced Logging — `[net][s]select` Visibility

**Purpose**: Make the provider's control-plane connectivity observable in logs without `-v` (debug mode). Critical for warmup monitoring, proxy health checks, and outage detection.

**Files Modified**: `net_http.go`

**Change**:
- Log level: `[net][s]select` serial-select messages promoted from `Debug(2)` → `Info()`
- **Effect**: One log line per successful **backend (control-plane) dial** — i.e. the provider's own API/WebSocket connection to the URnetwork platform (e.g. `api.bringyour.com/connect/control`), visible in standard log output
- **Impact**: Makes per-proxy control-plane connectivity observable (critical for warmup testing and outage monitoring)

> [!IMPORTANT]
> `[net][s]select` measures the **provider's own control-plane traffic**, NOT end-user relay throughput. `success=N` counts successful backend dials; it does not mean bytes are flowing for users. The `clients=N` field and the separate `[traffic]` log line are the actual data-plane / earnings signals — a proxy can show `success=5000 clients=0` and be relaying zero bytes. See `LOG_REFERENCE.md`.

**How to Identify in New Upstream**:
- Search for `[net][s]select` log statements in `net_http.go`
- Look for debug-level assignments; promote to info-level if they exist
- Verify logs show success counts (e.g., `success=4474 error=193`)

**Status**: ✅ Shipped in all releases; no upstream PR (too specific to fork needs)

---

## 2. Increased Contract Byte Limit

**Purpose**: Faster throughput ramp-up for providers. Higher initial contract limit allows more bytes to transfer before renegotiating, reducing overhead on cold starts.

**Files Modified**: `transfer_contract_manager.go`

**Change**:
```
InitialContractTransferByteCount: 16 KiB → 2 MiB
```
- **Old**: 16384 bytes per contract
- **New**: 2097152 bytes per contract (mib(2))
- **Ratio**: 128x increase

**Effect**: Reduces contract renegotiation overhead during traffic ramp-up; faster throughput scaling.

**How to Identify in New Upstream**:
- Search for `InitialContractTransferByteCount` in `transfer_contract_manager.go`
- Current value is `mib(2)` (in bytes)

**Status**: ✅ Shipped in all releases; could be upstreamed if performance gains are universal

---

## 3. Log Spam Reduction — Rate-Limited Errors

**Purpose**: Prevent log explosion during outages or high-error conditions. Reduces noise while preserving diagnostics.

**Files Modified**: 
- Error logging in transport/auth layers (exact files depend on error type)

**Changes**:
- `[t]auth error` — Rate-limited (suppressed repeated occurrences)
- `[contract]oob error` — Rate-limited
- `[r]drop` — Rate-limited (Added in v3.23.0-fix.15.3)

**Example**: When auth fails repeatedly, logs show "X suppressed" instead of identical message spam.

**How to Identify in New Upstream**:
- Search for `[t]auth error`, `[contract]oob error`, and `[r]drop` log patterns
- Look for "suppressed" or "rate" patterns in logging calls
- Check if glog has rate-limiting wrappers (e.g., `Infof` vs `Infof_Limited`)

**Status**: ✅ Fully shipped in v3.23.0-fix.15.3.

---

## 4. Docker Configuration & Multi-Arch Build

**Purpose**: Production-ready containerization with traffic monitoring and multi-architecture support (amd64, arm64).

**Files Modified**:
- `Dockerfile` — Alpine base, multi-stage build, vnStat integration
- `provider/Makefile` — Multi-arch build targets
- `.github/workflows/build.yml` — CI/CD for Docker image publishing
- Entrypoint scripts: `start_jwt.sh`, `start_stable.sh`, `start_nightly.sh`

**Changes**:
- **Base Image**: Alpine Linux (minimal footprint)
- **Traffic Monitoring**: vnStat listening on port 8080
- **Build Variants**: JWT, stable, nightly startup modes
- **Environment Variables**:
  - `BUILD`: selects startup script
  - `USER_AUTH`, `PASSWORD`: credential-based auth
  - `ENABLE_VNSTAT`, `ENABLE_IP_CHECKER`: optional monitoring
- **Multi-arch Push**: Builds and pushes `ghcr.io/full-bars/urnetwork-3.23-fix:latest` for both amd64 and arm64

**How to Identify in New Upstream**:
- Check if upstream provides `Dockerfile` (unlikely — 3.23 may not have it)
- If upstream adds Docker support, compare entry points and environment handling
- Ensure `BUILD` env var routing still works correctly
- Test multi-arch builds with: `docker buildx build --platform linux/amd64,linux/arm64 -t IMAGE:TAG .`

**Status**: ✅ Custom to this fork (not in upstream). Needs manual maintenance per upstream upgrade, but logic is isolated.

---

## 5. Build Flags & Optimization

**Purpose**: Reduce binary size and enable low-memory mode for providers.

**Files Modified**: `provider/Makefile`, build commands

**Changes**:
- **GOEXPERIMENT=greenteagc**: Enabled for reduced memory overhead
- **Strip symbols**: `-ldflags "-w -s"` (reduces binary size)
- **Version injection**: `-X main.Version=...` (custom versioning)
- **CLI flag**: `max-memory` — applies soft memory limit

**How to Identify in New Upstream**:
- Check `provider/Makefile` for build flags
- Verify `greenteagc` experiment is still viable in newer Go versions
- Confirm `-ldflags` pattern is preserved

**Status**: ✅ Shipped; unlikely to conflict with upstream unless build system changes significantly

---

## 6. Provider CLI Customizations

**Purpose**: Support custom auth backends and proxy management.

**Files Modified**: `provider/main.go`

**Known Customizations**:
- Auth backends: JWT token or user/password via `https://api.bringyour.com`
- Proxy management: `provider proxy add|remove` commands
- Docopt-based CLI

**How to Identify in New Upstream**:
- If upstream changes auth flow or CLI structure, review for conflicts
- Check if provider CLI is still docopt-based
- Verify proxy management commands still exist

**Status**: Likely stable; main risk is if upstream refactors CLI structure

---

## 7. Turbo Mode (V4 / V8)

**Purpose**: Remove the per-connection throughput ceiling on RAM-rich servers. The ceiling exists because per-connection bandwidth is bounded by `MaxWindowSize / RTT`. Turbo raises the window to 4 or 8 MiB and scales all dependent buffers accordingly.

**Files Modified**: `provider/main.go`, `scripts/Provider_Install_Linux.sh`, `docker/scripts/entrypoint.sh`

**Changes**:
- `applyTurboSettings()` in `provider/main.go` — reads `URNETWORK_PROFILE=turbo-v4` or `turbo-v8` and applies:
  - `MaxWindowSize`: 1 MiB → 4 MiB (V4) / 8 MiB (V8) for both TCP and UDP
  - `ResendQueueMaxByteCount` / `ReceiveQueueMaxByteCount`: scaled to 2× window (8/16 MiB)
  - IP-layer `SequenceBufferSize`: 256 → 512
  - Transfer-layer `SequenceBufferSize`: 16 → 64
  - WebRTC `ReceiveBufferSize`: 2× window per peer
  - `ContractTransferByteSeqScale`: 4 → 3 (reaches full contract size in 3 contracts)
  - `GOGC`: 200, no GOMEMLIMIT
- `toggle_turbomode()` in `Provider_Install_Linux.sh` — `urnet-tools turbo <v4|v8|off>` command
- `entrypoint.sh` — translates Docker `TURBO=v4/v8` env var to `URNETWORK_PROFILE` before exec

**Impact**:
- Significantly higher theoretical throughput ceilings for low-latency paths.
- Removes the mathematical cap inherent in the upstream window defaults.

**How to Identify in New Upstream**:
- If upstream changes `TcpBufferSettings`, `SendBufferSettings`, or `ReceiveBufferSettings` struct fields, verify `applyTurboSettings` still sets valid fields
- If upstream changes `WebRtcSettings`, verify `ReceiveBufferSize` field still exists
- `ContractTransferByteSeqScale` lives in `ContractManagerSettings` — verify path if contract manager is refactored

**Status**: ✅ Shipped in fix.13. Custom to this fork. Needs netem/Detroit testing before tuning values further.

---

## 8. Message Pool Auto-Sizing

**Purpose**: The message pool free-list caps at 1 MiB by default regardless of available RAM. With large proxy lists, this pool is exhausted almost immediately under any real load — every packet above the cap falls back to a `make([]byte, ...)` GC allocation, adding constant allocation churn. Auto-sizing scales the cap to available RAM so the pool actually serves its purpose.

**Files Modified**: `provider/main.go`

**Changes**:
- `applyPoolAutoSize()` in `provider/main.go` — called at startup from `provide()`:
  - Detects effective RAM via the existing cgroup-aware `detectEffectiveRAMLimitBytes()`
  - Calls `connect.ResizeMessagePools(RAM / 32)` with floor 8 MiB and cap 256 MiB
  - Skipped when `URNETWORK_PROFILE=lowmem` (lowmem manages its own footprint)
  - Skipped when `--max-memory` is set (that path already calls `ResizeMessagePools(maxMemory/8)`)
  - Logs `[pool] message pool NMiB (RAM=NMiB)` once at startup

**Per-server effect (approximate)**:
- ATL (1.9 GiB RAM): ~61 MiB (was 1 MiB)
- ATL2 (4.7 GiB RAM): ~150 MiB (was 1 MiB)
- honk (23 GiB RAM): 256 MiB capped (was 1 MiB)

**How to Identify in New Upstream**:
- Search for `InitialMessagePoolByteCount` in `message_pool.go`
- Search for `ResizeMessagePools` call sites — verify `provide()` still calls it at startup
- If upstream changes the pool size class structure (2KB, 4KB, 16KB, 32KB, 64KB tiers), verify `Resize` still accepts a byte-count argument and divides by pool size internally

**Status**: ✅ Shipped in fix.14. Custom to this fork.

---

## 9. Outage Webhook and Health Heartbeat

**Purpose**: Operators managing fleets of providers have no push signal when a backend outage starts or clears. The only indicator was log spam (rate-limited auth errors). These two features give active and passive observability: push alerts on outage events, and a regular heartbeat line for liveness and memory trend monitoring.

**Files Modified**: `provider/main.go`, `transport.go`

**Changes**:

`transport.go`:
- Exported `IsBackendDegraded()` — wrapper around `isBackendDegraded()`. Degraded is reported only when **both** conditions hold: a consecutive-failure counter (`consecutiveBackendFails`) has crossed `backendDegradedFailThreshold` (3), and the last failure was within `backendDegradedWindow` (2 min). The counter is incremented at every auth/connect failure (H1 and H3 paths) and every contract OOB error, and reset to 0 on every successful connect **and** every successful OOB result. Because any one success resets the count, isolated transient timeouts never accumulate — only broad, sustained failure (a real outage) drives the counter past the threshold.

`provider/main.go`:
- `runOutageWatcher(ctx, nodeName, webhookURL)` — background goroutine, always runs:
  - Polls `connect.IsBackendDegraded()` every 30 seconds
  - Logs `[outage] backend degraded` / `[outage] backend recovered` on state transitions
  - Requires `startConfirm` (10) consecutive degraded polls — 5 minutes of continuous degradation — before firing `outage_start`. Any healthy poll in between resets the count. This is the primary false-alarm guard: detection latency is traded (~5 min) for a near-zero false-positive rate.
  - Requires 2 consecutive healthy polls before firing `outage_clear` (prevents false clears mid-outage)
  - If `URNETWORK_ALERT_WEBHOOK` is set: POSTs JSON `{event, node, timestamp, message}` via a shared `webhookClient` (5s timeout); webhook calls are in goroutines so delivery never blocks the poll loop
  - Per-event 5-minute cooldown on webhook POSTs to prevent spam at the recovery boundary
- `fireWebhook(url, nodeName, event, message)` — HTTP POST helper; drains response body before closing to avoid leaving server sockets in CLOSE_WAIT
- `runHealthHeartbeat(ctx, startTime, profile)` — background goroutine, always runs:
  - Logs `[health] uptime=X profile=Y heap=ZMiB sys=WMiB connections=N` on a configurable interval (default 5 minutes)
  - Provides real-time visibility into active TCP/UDP proxy sessions (instrumented via `ip.go`)
  - Uses `runtime/metrics` (lock-free, no stop-the-world) rather than `runtime.ReadMemStats`
  - Interval set via `URNETWORK_HEALTH_INTERVAL` (Go duration string, min 1 minute)

**Node identity**: `URNETWORK_NODE_NAME` sets the node label in payloads. Auto-fallback: detects Docker via `/.dockerenv` and appends `(docker)` or `(binary)` to the hostname, so alerts from containers and bare binaries on the same host are distinguishable without configuration. Newlines stripped from env var to prevent log injection.

**How to Identify in New Upstream**:
- If upstream renames `lastBackendFailNano` / `consecutiveBackendFails` or refactors the degraded-state machinery, update the `IsBackendDegraded()` export in `transport.go`
- If upstream moves auth error handling out of `transport.go` (e.g., into a dedicated health module), check that both `lastBackendFailNano` and `consecutiveBackendFails` are still written at every auth and OOB failure path, and that the counter is reset to 0 at every success path (H1/H3 connect success and OOB success) — a missing reset would make the counter climb monotonically and produce false outages
- `runOutageWatcher` and `runHealthHeartbeat` launch sites are in `provide()` — if the provide function is refactored, ensure these still launch with the correct `ctx`

**Status**: ✅ Shipped in fix.14. Custom to this fork.

---

## 10. System Optimizer & Auditor

**Purpose**: Maximize system-level throughput and stability by automatically tuning kernel limits (ulimit, conntrack) for high-volume traffic.

**Files Added**: `audit.go`
**Files Modified**: `provider/main.go`, `scripts/Provider_Install_Linux.sh`

**Changes**:
- **System Auditor**: Runs on provider startup; passively checks host `ulimit -n`, `nf_conntrack_max`, and `tcp_timeout_established`. Logs `[audit]` warnings if host settings are suboptimal. Docker-aware hint tells users to run the optimizer on the host machine.
- **`urnet-tools optimize`**: New management command (requires root) that applies "Golden Fleet" settings:
  - **Auto-Install**: Installs `conntrack` on Arch, Debian, and RHEL distros.
  - **Boot Persistence**: Writes `nf_conntrack` to `/etc/modules-load.d/urnetwork.conf` to solve the systemd race condition where sysctl applies before the module loads.
  - `ulimit -n`: 1,048,576
  - `nf_conntrack_max`: 2,097,152 (standard across all RAM sizes based on fleet observations)
  - `nf_conntrack_tcp_timeout_established`: 3,600s (1h)
  - `net.ipv4.tcp_fin_timeout`: 10s
  - `net.ipv4.ip_local_port_range`: 1024 65535
  - `net.ipv4.tcp_tw_reuse`: 1
- **Persistence**: Writes settings to `/etc/sysctl.d/99-urnetwork.conf` and systemd service overrides.

**Status**: ✅ Shipped in fix.14 (unreleased).

---

## 11. Auto-Tune Performance Profile

**Purpose**: Dynamically scale internal buffer and contract settings based on detected system RAM. Replaces the "binary" choice between `lowmem` and `default` with a smart `auto` profile.

**Files Added**: `tuning.go`
**Files Modified**: `provider/main.go`, `util.go`

**Changes**:
- **`URNETWORK_PROFILE=auto`**: Opt-in profile that selects one of four tiers:
  - **Tier 1 (Low, <1.2GB)**: 128KB contracts, 32 seq buffers, 128KB TCP window, 512KB WebRTC.
  - **Tier 2 (Balanced, 1.2-3GB)**: 256KB contracts, 128 seq buffers, 512KB TCP window, 1MB WebRTC.
  - **Tier 3 (Perf, 3-8GB)**: 2MB contracts, 256 seq buffers, 4MB TCP window, 4MB WebRTC.
  - **Tier 4 (Extreme, >=8GB)**: 2MB contracts, 512 seq buffers, 8MB TCP window, 16MB WebRTC, GOGC 200, contract ramp scale 3.
- **Cgroup Awareness**: `DetectEffectiveRAMLimitBytes()` (moved to `util.go`) correctly reads limits in Docker/K8s environments.

**Status**: ✅ Shipped in fix.14 (unreleased).

---

## 12. Installer Robustness & Systemd Integration

**Purpose**: Make the install and optimize experience seamless across different execution contexts (Docker, root, regular user, etc.).

**Files Modified**: `scripts/Provider_Install_Linux.sh`

**Changes**:
- **Date Parsing Fix**: Python 3 `datetime.fromisoformat()` fails on ISO 8601 strings with `Z` suffix (Python < 3.11). Converted to `+00:00` before parsing. Fixes crashes when installer queries GitHub release metadata.
- **Root Installation Handling**: When running as root in a Docker container (no user session bus), installer gracefully warns instead of exiting on `systemctl --user enable` failure. Docker users can still proceed without systemd --user services; the provider binary works standalone.
- **Systemd Lingering Auto-Enable**: `urnet-tools optimize` now automatically enables lingering (`loginctl enable-linger <user>`) so systemd --user services persist after logout. This was previously a manual step users had to remember. Kept in `optimize` (not `install`) to defer root/sudo prompts to a single explicit optimization step.
- **Robust Tag Resolution**: Added a fallback to resolve the `latest` tag using GitHub HTTP redirects when the JSON API returns malformed data.
- **Direct URL Construction**: Fixed the download URL pattern to correctly include the `v` prefix in filenames and added `-f` to `curl` to prevent downloading 404 pages.
- **Robust ZRAM Manual Fallback**: The `optimize` command now includes a universal fallback for ZRAM enablement. If the distro-specific systemd service fails (common in restricted environments), the script manually initializes a ZRAM device via `zramctl` and `swapon`.
- **Simplified Proxy Management**: Added `proxy add` and `proxy clear` wrappers to `urnet-tools` to simplify bulk proxy operations without requiring long `provider` command arguments.

**How to Identify in New Upstream**:
- Search for GitHub release metadata fetching in the install script; if new date parsing logic appears, apply the `Z` → `+00:00` conversion.
- Verify `systemctl --user enable` failures are handled gracefully (warn, don't exit) for root/Docker contexts.
- Check if `loginctl enable-linger` is called somewhere; if not, add it to the optimize command for consistency.

**Status**: ✅ Shipped in fix.14. Purely operational improvements (no provider code changes).

---

## Porting Checklist for Future Upstream Versions

When merging a new upstream version (e.g., v3.24, v4.0):

### Pre-Merge
- [ ] Clone new upstream tag into a temporary branch: `git fetch upstream v<NEW> && git checkout -b upstream-new upstream/v<NEW>`
- [ ] Create new branch for porting: `git checkout -b upgrade-to-v<NEW>`

### Code Changes
- [ ] **Logging**: Verify `[net][s]select` is still at debug level; promote to info if needed (net_http.go)
- [ ] **Contract limit**: Update `InitialContractTransferByteCount` to 256 KiB if reset to 16 KiB (transfer_contract_manager.go)
- [ ] **Error logging**: Check for new error paths; apply rate-limiting if spam appears ([t]auth, [contract]oob, [r]drop)
- [ ] **Docker**: If upstream adds Dockerfile, review and merge; preserve BUILD env var routing and multi-arch build
- [ ] **Makefile**: Preserve greenteagc, strip flags, version injection
- [ ] **Turbo mode**: Verify `TcpBufferSettings`, `SendBufferSettings`, `ReceiveBufferSettings`, and `WebRtcSettings` struct fields used in `applyTurboSettings` still exist; re-check field names if contract manager or IP stack is refactored
- [ ] **Per-proxy loop spam**: Scan any new functions or goroutine starts added inside the per-proxy provide loop (`provideWithProxy`). If a function logs an identical line or starts a monitor goroutine on every call, apply a `sync/atomic` once-guard (see Section 14 pattern)

### Testing
- [ ] Build for current platform: `go build -ldflags "-X main.Version=dev" -o provider_bin ./provider/main.go`
- [ ] Build multi-arch Docker: `docker buildx build --platform linux/amd64,linux/arm64 -t test:v<NEW> .`
- [ ] Run unit tests: `./test.sh`
- [ ] Smoke test with container: Start container, verify logs show `[net][s]select` at INFO level
- [ ] Verify contract behavior: Check logs for contract sizes (~256 KiB batches)

### Post-Merge
- [ ] Update version tag: `git tag v3.23.0-fix.<N>`
- [ ] Update `FORK_CHANGES.md` if any changes modified or removed
- [ ] Push to GitHub: `git push origin upgrade-to-v<NEW>` → Create PR for review
- [ ] Document any upstream changes that affected our modifications in this file

---

## Files Safe to Skip During Upstream Merges

These files are fully custom or unlikely to change:
- `Dockerfile`
- `.github/workflows/build.yml`
- `provider/start_*.sh` (startup scripts)
- `FORK_CHANGES.md` (this file)

**Caution**: If upstream restructures directories (e.g., moves provider CLI or protocol buffers), review all custom files for import path changes.

---

## 15. Dead-Proxy Health Report

**Purpose**: Provide a pure-observability per-heartbeat report of which proxies are dead vs degraded, a record of how many the fix.11 retry pulse recovers, and durable on-disk files so the picture survives RAMLOGS.

**Files Modified**: `connect/proxy_health.go`, `connect/transport.go`, `provider/main.go`, `provider/proxy_health_log.go`, `scripts/Provider_Install_Linux.sh`, `docker/scripts/proxy-health.sh`

**Changes**:
- `[health][proxies]` lines listing `dead` (never authenticated) and `degraded` (worked before, down now) proxies, plus transition counters.
- A `[pulse]` marker logs each retry sweep.
- Persistent state and event logs in `proxy_health.state` and `proxy_health.log` (default `~/.urnetwork`).
- Access commands: host `urnet-tools proxy health` and Docker `proxy-health`.

**Status**: ✅ Shipped in v3.23.0-fix.16.

---

## Known Upstream Additions to Monitor

These features from upstream should be reviewed before merging to ensure compatibility:
- **Log spam fixes** (upstream PR#180): Compare with our rate-limiting approach
- **Contract behavior changes**: If upstream ever increases contract sizes, reconsider our 256 KiB choice
- **Outage handling** (v3.23.0-fix.9 validates this): Monitor for upstream improvements to outage detection/retry logic
- **Docker support**: If upstream adds official Docker build, evaluate vs. our custom Dockerfile

---

## Questions for Future Merges?

If a new upstream version introduces changes to files in the "Modified" list above, follow this process:
1. Generate diff: `git diff upstream/v3.23 HEAD -- [file]` to see exact change
2. Apply diff manually to new upstream version
3. Test the change in isolation (see Testing section)
4. Document any conflicts or assumptions in this file

---

**Last Updated**: 2026-06-20  
**Maintained By**: @full-bars  
**Contact**: Reference GitHub issues in urnetwork-3.23-fix repo

---

## 13. Root Guard & Assisted User Setup

**Purpose**: Guide users away from running the provider as root on systemd hosts. Prevents "Failed to connect to bus" errors and improves fleet security by ensuring services run in a proper user session.

**Files Modified**: `scripts/Provider_Install_Linux.sh`

**Changes**:
- **`func_root_guard`**: Detects root execution on systemd hosts and provides an interactive menu to correct the deployment path.
- **Assisted Setup**: Automatically creates a dedicated `urnet` service user, detects the correct admin group (`wheel` or `sudo`) based on the Linux distribution, and enables systemd lingering.
- **`func_run_as_user`**: A hardened hand-off mechanism using `runuser`. It is **SELinux-aware** and correctly handles `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS` propagation to ensure the user-level systemd bus is reachable even from a direct-root SSH session.

**Impact**:
- Verified stable on **Arch, AlmaLinux (RHEL), Debian, and openSUSE**.
- Eliminates "Permission Denied" errors during installation on SELinux-enforcing systems.
- Ensures all new fleet deployments follow security best practices.

**Status**: ✅ Shipped in v3.23.0-fix.15.

---

## 14. Startup Log Spam — Once-Per-Process Guards

**Purpose**: Prevent identical log lines from repeating once per proxy server on startup. With large proxy lists (~3000 proxies), functions that run inside the per-proxy `provideWithProxy` closure but produce global-state side effects were firing thousands of times.

**Root cause pattern**: Functions that belong logically at startup were placed inside the per-proxy closure. Settings mutation is correct per-proxy (each proxy gets its own `ClientSettings`/`LocalUserNatSettings`), but logging and goroutine starts are process-wide and must happen once.

**Files Modified**: `tuning.go`, `provider/main.go`

**Changes**:

- **`ApplyAutoTuning` (`tuning.go`)**: The `[tune] auto-profile` log line was emitted once per proxy. Added `autoTuneLogged atomic.Bool` and `autoTuneLogf` test seam. The log now fires exactly once per process via `CompareAndSwap`; per-proxy settings application (contract floors, buffer depths, GOGC, GOMEMLIMIT) is unchanged.

- **Eco memory monitor (`provider/main.go`)**: `go runEcoMemoryMonitor(ctx)` was called inside `provideWithProxy`, spawning one goroutine per proxy. Each goroutine polled independently on a 30-second ticker. Under memory pressure, all copies logged the same `[eco]` line and called `runtime.GC()` simultaneously — a log-spam and GC-storm bug. Added `ecoMonitorStarted atomic.Bool`, `startEcoMonitor` test seam, and `startEcoMonitorOnce()` wrapper. Both call sites (top-level eco profile check and per-proxy closure) now go through this wrapper so exactly one monitor goroutine starts per process.

**How to Identify in New Upstream**:
- When merging a new upstream, scan the per-proxy loop for any log calls or goroutine starts that produce global/identical output. The pattern to watch: a function called inside a proxy or connection loop whose log message would be identical across all iterations.
- Look for new monitoring goroutines added to `provideWithProxy` or equivalent — they should always be guarded.

**Status**: ✅ Shipped in v3.23.0-fix.15. Tests added: `TestApplyAutoTuningLogsOncePerProcess`, `TestApplyAutoTuningSkippedWhenProfileNotAuto`, `TestSelectTierThresholds`, `TestEcoMonitorStartsOnce`.

---

## 16. Dialer Selection Error Suppression

**Purpose**: Reduce log spam during backend outages. When the backend is unreachable, `[net][s]select:` error logs fire hundreds per second across dialer variants (fragment, direct, reorder, etc.), making log analysis impossible. Rate-limit these errors to one per minute with suppression counts.

**Files Modified**: `transport.go`, `net_http.go`

**Changes**:

- **New atomic counters** (`transport.go`, lines 31-32):
  - `var lastSelectErrLogNano atomic.Int64`
  - `var suppressedSelectErrCount atomic.Int64`

- **New rate-limiting function** (`transport.go`, lines 83-96):
  - `func shouldLogSelectErr() (bool, int64)` — Exact mirror of existing `shouldLogAuthErr()` with only variable names changed. Same 1-minute window, same atomic CAS pattern for thread-safety, same suppression count swap.

- **Error log wrapper** (`net_http.go`, around lines 679-686):
  - Original: `self.log.Infof("[net][s]select: %s = %s\n", dialer.String(), result.err)` (was `glog.Infof(...)` before the glog→Logger refactor — see Section on logger de-globalization)
  - Now wrapped: `if ok, suppressed := shouldLogSelectErr(); ok { ... }`
  - Format: `[net][s]select: {variant} = {error} (N suppressed)` when suppressed count > 0, otherwise `[net][s]select: {variant} = {error}` for first error in window
  - The success log (the `success=N error=N` line) remains untouched (visible on every successful selection)

**Impact**:
- Normal operation: Success logs (`[net][s]select: {variant}`) appear regularly; error logs appear at normal rate
- Backend unreachable: Error logs suppressed to one per minute with count of suppressed attempts shown
- Log volume reduction: ~99% reduction during extended outages (hundreds/second → one/minute)

**How to Identify in New Upstream**:
- If upstream modifies the `[net][s]select` logging in `net_http.go` (the `self.log.Infof("[net][s]select: ...")` calls), ensure the error log line still exists
- If upstream adds new error logging in the serial-select path, consider applying the same rate-limiting pattern
- Verify `shouldLogAuthErr()` still exists and uses the same atomic-counter/CAS pattern (reference for this feature)

**Status**: ✅ Shipped in v3.23.0-fix.17. Follows established rate-limiting pattern used for `[t]auth error`, `[contract]oob error`, and `[r]drop` errors.

---

## 17. Proxy Hot-Reload Engine

**Purpose**: Allow adding and removing proxies from a running provider without incurring the massive 8-hour warmup penalty associated with a full process restart. Proxy slot assignments (`proxy[N]`) are address-stable across reloads.

**Files Modified**: `provider/main.go`, `proxy_health.go`, `provider/proxy_reload.go`, `provider/proxy_id.go`

**Changes**:
- **Stable IDs**: `proxy_id.go` assigns monotonic stable IDs based on IP/Port, saving state to `proxy.state`.
- **Watcher**: `proxy_reload.go` watches a `.reload` trigger file to stagger additions/deletions.
- **Signal Map**: A per-proxy context map allows cancelling individual proxies without touching healthy connections on others.

**Status**: ✅ Shipped in v3.23.0-fix.17.

---

## 19. JWT Smart Refresh (Self-Healing Auth)

**Purpose**: Reduce manual intervention and API load by validating JWT expiry locally and providing a "self-healing" mechanism for entrypoint scripts to refresh tokens automatically.

**Files Modified**: `provider/main.go`, `docker/scripts/start_jwt.sh`, `docker/scripts/start_stable.sh`, `docker/scripts/start_nightly.sh`, `provider/jwt_test.go` (added)

**Changes**:

- **Local Expiry Validation**: `provider/main.go` now parses the JWT locally using `validateJWTExpiry(token)` before any network call. If the token's `exp` claim is in the past (with a 30s leeway for clock skew), it returns `ErrTokenInvalid` immediately.
- **Exit Code 78**: The provider now exits with **exit code 78** specifically when an authentication token is invalid or expired. This distinguishes auth failures from transient network errors (exit 1) or clean shutdowns (exit 0).
- **Self-Healing Shell Logic**: All startup scripts trap exit code 78. When detected:
  - The stale `jwt` file is deleted.
  - The script attempts to re-authenticate using available credentials (`USER_AUTH`/`PASSWORD` or positional tokens).
  - Includes a re-auth attempt cap (3) and backoff logic to prevent API hammering if credentials themselves are invalid.
- **Auth Panic Guard**: Replaced 4 `panic` calls in the `provideAuth` path with structured error returns and added nil-guards for API response fields.

**How to Identify in New Upstream**:
- Search for `provideAuth` in `provider/main.go`; ensure it doesn't panic on API errors.
- Check if upstream adds its own JWT expiry check.
- If upstream changes the `AuthNetworkClientResult` structure, verify the nil-guards in `provideAuth`.

**Status**: ✅ Shipped in v3.23.0-fix.17.

---

---

## 20. Per-Proxy Failure Reason Tracking

**Purpose**: Track the reason each proxy fails (auth errors, transport drops, contract failures) via atomic counters on `proxyHealth`, so operators can distinguish recurring auth errors from transient timeouts without grepping logs.

**Files Modified**: `proxy_health.go`, `transport.go`, `provider/main.go`

**Changes**:
- New `ProxyFailureCounters` struct with atomic.Int64 fields: `AuthFailures`, `TransportDrops`, `ContractFailures`, `TimeoutFailures`
- `RecordProxyAuthFailure(index)` at H1/PT auth failure sites in `transport.go`
- `RecordProxyTransportDrop(index)` alongside `markProxyDown` in both transport defer blocks
- Counters exposed via `ProxyHealthStatus` as `AuthFailures`, `TransportDrops`, `TimeoutFails`, `ContractFails`

**Status**: ✅ Shipped in v3.23.0-fix.18.4.

---

## 21. Graceful Drain on Proxy Removal

**Purpose**: When a proxy is removed via hot-reload, wait for all active sessions (`ProxyBandwidth.Clients`) to finish before tearing down, instead of cancelling the context immediately. Zero billable traffic is interrupted.

**Files Modified**: `proxy_health.go`, `provider/proxy_reload.go`, `provider/main.go`

**Changes**:
- `ProxyBandwidthByAddress(addr)` lookup in `proxy_health.go`
- `drainingProxies` tracking map on `ProxyReloader` with `isDraining()` guard
- Removal loop in `reload()`: if clients > 0, spawns drain goroutine that polls until 0, then cancels context
- Re-add of same address during drain is safely skipped with a log line
- `reload()` returns immediately — drain runs in background, other adds/removes not blocked
- No timeout — process stays alive until all drains complete

**Status**: ✅ Shipped in v3.23.0-fix.18.4.

---

## 22. Proxy Benchmarking (Opt-In)

**Purpose**: Periodically measure per-proxy latency with staggered, opt-in probes. Two probe types: TCP connect time (raw network RTT to proxy SOCKS5 port) and SOCKS5 CONNECT RTT (end-to-end latency through the proxy to a configurable target).

**Files Added**: `provider/proxy_benchmark.go`
**Files Modified**: `proxy_health.go`, `provider/main.go`

**Changes**:
- `LatencyNs` / `SocksLatencyNs` atomic.Int64 fields on `ProxyBandwidth`
- TCP connect probe every 5 min (~400 B/probe) — measures raw network RTT
- SOCKS5 CONNECT probe every 15 min (~800 B/probe) — measures end-to-end proxy latency
- Random startup jitter (0–5 min) prevents thundering herd at the benchmark endpoint
- Results exposed in `ProxyHealthStatus` as `LatencyMs` / `SocksLatencyMs`

**Configuration**:
- `URNETWORK_PROXY_BENCHMARK=true` — enables benchmarking (off by default)
- `URNETWORK_PROXY_BENCHMARK_ENDPOINT=connect.bringyour.com:443` — SOCKS5 CONNECT target

**Bandwidth estimate at scale (both probes, 10k proxies)**:
- TCP connect only: ~35 GB/month
- SOCKS5 CONNECT only: ~69 GB/month
- Total: ~104 GB/month

**Status**: ✅ Shipped in v3.23.0-fix.18.4.

---

## 23. Bandwidth Hub Dashboard

**Purpose**: Live fleet monitoring dashboard that aggregates bandwidth reports from all provider nodes. Shows per-node traffic rates (Mbps), billable vs total traffic, per-proxy drilldown with status/age/bytes, sortable columns, and auto-refresh.

**Files Modified**: `hub/main.go` (new), `provider/bandwidth_reporter.go`

**Changes**:
- New `hub/` directory with standalone dashboard server
- **Rate tracking**: Delta between consecutive reports computes RX/TX Mbps per node
- **Billable distinction**: `proxyReport` struct carries both `TotalRX/TX` and `BillRX/TX`; dashboard displays both
- **Per-proxy drilldown**: Click any node row to expand its full proxy list with individual metrics
- **Auto-refresh**: 30s countdown with toggle; full page reload on expiry
- **Sortable columns**: 12 sortable columns with numeric-aware sorting and direction indicators
- **Status badges**: Color-coded up/connecting/degraded proxy counts per node
- **Heartbeat health**: Green/yellow/red dot based on report freshness
- **JSON API**: `/api/nodes` returns full node state for external tooling
- **Bandwidth reporter**: Posts to `/api/report` hub endpoint, skips empty reports, includes connecting count

**Running the hub**:
```sh
./hub -addr :8080 -data /path/to/data
```

**Configuring provider reporting**:
```sh
URNETWORK_REPORT_URL=http://HUB_IP:8080
```

**Status**: ✅ Shipped in v3.23.0-fix.19.

---

## 24. Proxy Startup Pacing & Pace Monitor

**Purpose**: Prevent thundering-herd WebSocket dials when starting a provider with large proxy lists (500+). A jittered stagger spreads initial connections across a configurable window, and a pace monitor logs warmup progress every 30s until the fleet is up.

**Files Modified**: `provider/main.go`, `proxy_health.go`, `proxy_health_test.go`

**Changes**:
- `backoffPacer(n)` — waits `n × stagger_ms ± 50% jitter` before dialing
- `paceMonitor(ctx)` — background goroutine logs warmup progress at 30s intervals, then exits once warmup is complete
- Warns (⚠) when <50% up with >10 connecting; marks done (✓) when >90% up with <5 connecting, then returns
- Pulse log now includes connecting count from health snapshot
- `ProxyHealthSnapshot()` extended to return `connecting []string` (5th return value)
- `proxyIndex()` for native direct transports now correctly returns `index=0, ok=true`

**Configuration**:
- `URNETWORK_PROXY_STAGGER_MS=1000` — base stagger per proxy (default 1000, min 10)

**Log examples**:
```
[pace] ⚠ warmup: 47/200 up (24%), 150 connecting, 3 done
[pace] warmup: 142/200 up (71%), 55 connecting
[pace] ✓ warmup: 196/200 up (98%), 4 connecting — done
```
Once the `✓ done` line is logged, `paceMonitor` exits. No further `[pace]` output is produced.

**Breaking**: `ProxyHealthSnapshot()` now returns 5 values. Update any custom callers.

**Status**: ✅ Initial pacing shipped in v3.23.0-fix.19. Goroutine exit fix shipped in PR #122.

---

## 27. Message Pool Race Fix & Orphaned Buffer Leak

**Purpose**: Close two silent correctness bugs in the memory recycling subsystem found during a scheduled code review.

**Files Modified**: `message_pool.go`

**Changes**:

*Bug 1 — Share/Return race*: When `MessagePoolReturn` and `MessagePoolShare` ran concurrently on the same buffer, `Return` could reset the metadata (tag, flags, count to zero) and call `pool.Put()` while `Share` had already read count=1 and was about to increment it. The returning goroutine's `pool.Put()` would race a `pool.Get()` in a third goroutine, handing out the buffer before `Share` finished. Fixed by moving the metadata reset (`tag=0, flags=0, count=0`) inside the `stateLock` closure, so any concurrent `Share` that reads the count under the same lock sees count=0 and returns `false` before the buffer reaches the freelist.

*Bug 2 — Orphaned buffer leak in `ProtoMarshalWithTag`*: The function called `proto.Size` to estimate serialized size, grabbed a pool buffer of that size, then passed it to `proto.MarshalAppend`. If the estimate was too small, `MarshalAppend` allocated a new backing slice and returned it — abandoning the pool buffer. The orphaned buffer was never returned, accumulating as a steady GC-allocation leak. Fixed by comparing `cap(out) != cap(buf)` after the marshal; a cap change indicates reallocation, and the original buffer is explicitly returned to the pool.

**How to Identify in New Upstream**:
- Look for `MessagePoolReturn` in `message_pool.go` — verify metadata reset happens inside `stateLock`
- Look for `ProtoMarshalWithTag` — verify a cap-change guard returns the original buffer on reallocation

**Status**: ✅ Shipped in v3.23.0-fix.21.2 (PR #78).

---

## 28. CI Full Test Suite

**Purpose**: Replace a hand-picked test allowlist with auto-discovery so new tests are never silently skipped and Go's race detector catches concurrency bugs (like the one in §27) automatically.

**Files Modified**: `.github/workflows/build.yml`, `message_pool_test.go`

**Changes**:
- CI test step replaced: `go test -run TestFoo|TestBar` → `go test -short -race -timeout 600s ./...`
- `TestMessagePoolShare` fixed: assertion was checking against the old maximum bucket size (4 KiB) before the pool gained larger buckets (16 KiB, 32 KiB, 64 KiB). Updated to use `pools[len(pools)-1].size` dynamically.
- Added daily drift monitor job (`monitor-sibling-drift`) in `.github/workflows/upstream_monitor.yml` that checks `full-bars/connect` for new commits to critical files and posts a Discord "port check" alert.

**Status**: ✅ Shipped in v3.23.0-fix.21.2 (PR #79, #81).

---

## 29. Hub Report Visibility & Reporter Startup Jitter

**Purpose**: Surface silent hub reporting failures in logs, and prevent thundering-herd on fleet restart.

**Files Modified**: `provider/bandwidth_reporter.go`

**Changes**:
- `runBandwidthReporter`: non-2xx HTTP responses now log `[report] hub rejected report: <status>` instead of silently moving on.
- Added random startup jitter (0 to one full interval) before the first report POST. Without this, all providers that restart together (e.g., after a fleet update) post on the same wall-clock boundary, spiking the hub. Mirrors the existing jitter pattern in `proxy_benchmark.go`.

**Status**: ✅ Shipped in v3.23.0-fix.21.2 (PR #80, #82).

---

## Exit Code Reference

All non‑zero exit codes write a `FATAL [exit <code>]: ...` line to both stderr (Docker logs) and the ramlog file (`/dev/shm/urnetwork.log`) via `shmLogFatal` before terminating.

| Code | Meaning |
|------|---------|
| 0 | Clean shutdown |
| 10 | `auth`: home directory not found |
| 11 | `auth`: login request failed (network) |
| 12 | `auth`: API rejected the login credentials |
| 13 | `auth`: account requires additional verification via the app |
| 14 | `auth`: auth code request failed (network) |
| 15 | `auth`: auth code rejected (expired or single‑use code reused without persistent volume) |
| 16 | `auth`: could not create `~/.urnetwork` directory for JWT storage |
| 20 | `provide`: proxy file cannot be read |
| 21 | `provide`: proxy file contains no valid entries |
| 40 | `logs`: ramlog file not found (is `URNETWORK_RAMLOGS=1` set?) |
| 50 | `proxy refresh`: proxy state file not found |
| 51 | `proxy refresh`: provider is not currently running |
| 52 | `proxy refresh`: provider has not reached the 8‑hour warmup threshold (use `--force` to override) |
| 53 | `proxy refresh`: could not acquire the proxy lock (another operation in progress) |
| 54 | `proxy refresh`: could not read the proxy source file |
| 55 | `proxy refresh`: could not determine the reload trigger path |
| 56 | `proxy refresh`: could not write the reload trigger |
| 60 | `proxy remove-dead`: provider is not currently running |
| 61 | `proxy remove-dead`: provider has not reached the 65‑minute dead‑confirmation threshold |
| 62 | `proxy remove-dead`: could not update the proxy source file |
| 63 | `proxy remove-dead`: could not acquire the proxy lock |
| 64 | `proxy remove-dead`: could not write the reload trigger |
| 78 | `provide`: JWT expired or invalid — startup scripts intercept this code to delete the stale JWT and re‑authenticate |

**Status**: ✅ Shipped in v3.23.0-fix.19.

---

## 25. Zero-Contention Proxy Health Tracking (O(1) Lookup)

**Purpose**: Eliminate CPU spikes and lock contention during mass proxy reconnects and hot-reloads. The provider used to perform an `O(N)` scan across all proxies under a global mutex lock for every bandwidth update or health query.

**Files Modified**: `proxy_health.go`, `provider/proxy_reload.go`, `transport.go`

**Changes**:
- Replaced the global `O(N)` array scan with an address-based pointer index map (`map[string]*proxyHealth`) inside `proxyHealthRegistry`.
- `ProxyBandwidthByAddress` and `ProxyHealthByAddress` now perform instant `O(1)` direct memory lookups.
- Reduced the scope of `proxyHealthMu` to only protect structural changes (adds/removes) rather than read-only bandwidth queries.

**Status**: ✅ Shipped in v3.23.0-fix.21.1.

---

## 26. TLS Session Lock-Ordering Deadlock Fix

**Purpose**: Fix a chronic, silent deadlock that could permanently freeze live nodes and CI tests. The `EncryptionSessionManager` and `peerEncryptionSession` locks were occasionally acquired in inverted order between the idle-reaping background timer and active TLS handshakes.

**Files Modified**: `transfer_encrypt.go`

**Status**: ✅ Shipped in v3.23.0-fix.21.1.

---

## 30. Expanded RAMLOGS & Enhanced Log Triage

**Purpose**: Provide a larger diagnostic window for high-volume nodes and simplify log extraction for troubleshooting without requiring full journald access.

**Files Modified**: `provider/shmlog_linux.go`, `scripts/Provider_Install_Linux.sh`

**Changes**:
- `shmLogMaxSize`: Increased the in-memory log buffer from 1 MiB to 5 MiB. This allows the provider to retain more history in RAM (critical for high-throughput nodes with frequent log events) while still avoiding disk I/O.
- `urnet-tools logs`: Added subcommands to control output:
    - `all` / `full`: Streams the entire buffer from the beginning (useful for capturing startup sequences after the buffer has rolled over in normal tail mode).
    - `dump`: Copies the current RAM buffer to `~/urlogs.txt` and exits. Simplifies log gathering for operators on remote systems.

**Status**: 🛠 [Unreleased]

---

## 31. Security: QUIC Handshake Memory Exhaustion Fix

**Purpose**: Protect providers against a vulnerability where an unauthenticated remote attacker could cause the provider to allocate excessive memory during the QUIC handshake, potentially leading to an OOM crash.

**Files Modified**: `go.mod`, `go.sum`

**Changes**:
- Bumped `github.com/quic-go/quic-go` from `v0.59.0` to `v0.59.1`.
- This version includes protection against handshakes that attempt to stall or leak memory before authentication is complete.

**Status**: 🛠 [Unreleased]

---

## 32. Proactive Periodic JWT Refresh

**Purpose**: Ensure JWT tokens never expire under normal operation by proactively refreshing every 7 days, with a 48-hour expiry fallback as safety net.

**Files Modified**: `provider/main.go`

**Changes**:
- Tracks last successful refresh timestamp on disk (`~/.urnetwork/jwt_last_refresh`)
- **Primary mechanism**: Refreshes every 7 days regardless of token expiry, ensuring continuous service
- **Secondary fallback**: Also refreshes if token gets within 48h of expiry (catches failures in primary mechanism)
- **Startup jitter**: 0-9 minute random delay before first check to desynchronize fleet

**Benefit**: Eliminates the risk of tokens expiring unexpectedly. Multi-day outages don't cause exit-78 failures because the 48h expiry buffer provides recovery time even if weekly refresh is missed. All auth modes benefit equally via JWT-to-JWT mechanism.

**Status**: 🛠 [Unreleased]

---

## 33. Proxy URL Sources

**Purpose**: Let the provider track a live, rotating proxy list (e.g. a free SOCKS5 feed) without manual re-downloading, re-importing, or duplicate-checking. Previously the only input was a static `proxy.txt`/`proxy add`.

**Files Modified**: `provider/proxy_url.go`, `provider/proxy_url_source.go`, `provider/main.go`, `provider/proxy_reload.go`, `provider/proxy_state.go`

**Changes**:
- `--proxy_url=<url>` / `PROXY_URL` (comma-separated for multiple sources) — fetches on an interval (`--proxy_url_refresh`, default `15m`) and merges new entries into the same hot-reload desired-set pipeline used by `--proxy_file`. Already-running proxies (by address) are never disturbed.
- `--proxy_url_max=<n>` / `PROXY_URL_MAX` — caps total URL-sourced proxies; once hit, new entries are skipped rather than evicting existing ones.
- `--proxy_dead_cleanup_scope=url|all|none` (default `none`) / `--proxy_dead_cleanup_interval` (default `24h`) — automatic dead-proxy cleanup, scoped by where a proxy was added from (`url`, `file`, `internal`) so cleanup of a noisy public list never touches hand-curated entries unless explicitly widened to `all`.
- `urnet-tools proxy add-source <url>` / `remove-source <url>` — manage sources at runtime; `add-source` triggers an immediate fetch and persists the URL across restarts.
- v1 list format: plain text, one proxy per line, optional `socks5://` prefix; blank lines and `#` comments ignored; non-SOCKS5 prefixed lines skipped with a warning rather than failing the whole fetch.
- Every proxy is tagged with its source (`url`, `file`, `internal`) in `proxy.state`, which is what cleanup scoping and dedup keys off of.

**Fixed during live deployment hardening** (discovered running a 1600+ proxy free list against this feature in production):
- `reload()` checked for an empty desired set *before* merging in the URL-sourced cache, so a URL-only configuration (no `--proxy_file`) was treated as "remove everything" on every reload cycle.
- The added-proxy stagger used a fixed `100ms × i` delay instead of the existing jittered `backoffPacer`, so a large URL-sourced batch landed on the auth API in a near-simultaneous burst instead of spread out.
- HTML error-page bodies from the auth API (e.g. a 429 from an upstream rate limiter) were logged verbatim instead of collapsed — see #35.
- A proxy whose addresses came only from a URL source had no path to retry after exhausting `maxAuthFailures`, since the existing hourly-pulse retry only covered file/manually-added proxies; it now auto-retries 15 minutes after giving up.

**Docs**: [Proxy URL Sources](docs/Proxy-URL-Sources.md), [design doc](docs/design/proxy-url-source-design.md).

**Status**: ✅ Shipped, pending next tagged release.

---

## 34. Proxy State File Not Written Until First Reload

**Purpose**: Fix `urnet-tools proxy refresh` failing with `FATAL [exit 51]: provider does not appear to be running` on a brand-new provider with 0 proxies — the status check read `proxy.state`, which previously wasn't written until the first hot-reload occurred.

**Files Modified**: `provider/main.go`, `provider/proxy_state.go`

**Changes**:
- The provider now unconditionally writes `proxy.state` at startup, even with zero proxies configured.
- A zero/missing `started_at` timestamp is healed during normal heartbeat execution instead of only at hot-reload time.

**Status**: ✅ Shipped, pending next tagged release.

---

## 35. Hot-Reload Added-Proxy Visibility

**Purpose**: A hot-reload that *adds* proxies (editing `--proxy_file`, or a URL source landing new entries) printed nothing beyond per-removal lines, making it hard to confirm the reload actually picked up new proxies without cross-checking `proxy.state` by hand.

**Files Modified**: `provider/proxy_reload.go`

**Changes**:
- Hot-reload now prints `[proxy] reload: adding N proxies:` followed by the same per-proxy `proxy[%d] addr (user/pass)` line format used at startup, for every proxy the reload adds.

**Status**: ✅ Shipped, pending next tagged release.

---

## 36. 429-Aware Auth Retry Backoff & Global Adaptive Rate Limiter

**Purpose**: Two related problems surfaced testing the Proxy URL Sources feature against a large, low-quality free proxy list: (1) a rate-limited (429) auth attempt retried on the same flat jitter as an ordinary timeout, so a proxy that got 429'd kept re-hitting the API at the same rate that triggered the 429; (2) even with per-attempt backoff scaled to the 429, hundreds of proxies starting or retrying concurrently still hammer the API in aggregate, since no single proxy's backoff has visibility into how many siblings are doing the same thing at the same time.

**Files Modified**: `provider/main.go`, `provider/auth_rate_limiter.go` (new)

**Changes**:
- `proxyAuthRetryDelay(err, attempt)`: a 429/"Too Many Requests" error now waits `attempt × 5s + jitter`, capped at 60s, instead of the flat `0.5–10.5s` jitter used for every other error.
- `formatDuration`: omits the redundant hours segment when zero (`15m` instead of `0h 15m`) — surfaced by the above logging the new scaled delays.
- `authRateLimiter` (`golang.org/x/time/rate`-backed): a single process-wide token bucket that every auth attempt — first try and retry alike — waits on (`Wait(ctx)`) before calling the API, instead of relying on uncoordinated per-proxy backoff to bound aggregate request rate.
  - AIMD-adaptive: starts at the believed ceiling (10 req/s, burst 15, so an initial batch of proxies isn't artificially slow-ramped), halves on any 429 (floor 1 req/s), and creeps back up by 1 req/s after 20 consecutive non-429 results (ceiling 10 req/s).
  - A 2-second cooldown between adjustments prevents a burst of already-in-flight 429s (issued before the first cut takes effect) from each triggering their own additional cut.
  - Logs `[proxy][authrate] 429 received — cutting auth rate X -> Y req/s` and the equivalent on recovery, so the adaptation is visible in normal operation.

**Status**: ✅ Shipped, pending next tagged release.

---

## 37. v3.23.0-fix.23 — Various Enhancements & Fixes

### SOCKS5 Handshake Probe (Was TCP-Only)

The TCP connect probe now performs a full SOCKS5 handshake (`0x05 0x01 0x00` greeting + response) instead of a bare TCP connect. This verifies the proxy actually speaks SOCKS5 before marking it as reachable, eliminating false positives from hosts that accept TCP connections but aren't functioning SOCKS5 proxies.

**Files Modified**: `provider/proxy_benchmark.go`

**Status**: ✅ Shipped in v3.23.0-fix.23.

---

## 38. Bounded Auth Concurrency

**Purpose**: Prevent resource exhaustion when many proxies attempt authentication simultaneously.

**Files Modified**: `provider/main.go`

**Change**:
- Introduced a concurrency semaphore limiting in-flight auth attempts to 5.
- When the limit is reached, additional auth attempts block until a slot opens, ensuring auth API calls remain bounded regardless of proxy list size.

**Status**: ✅ Shipped in v3.23.0-fix.23.

---

## 39. Contract Logging — No Longer Rate-Limited

**Purpose**: Ensure every contract event is visible in logs for debugging and earnings verification.

**Files Modified**: `transfer.go`

**Change**:
- Contract lifecycle events (`[contract] acquired`, `[contract] denied`, `[contract] oob`) are now logged unconditionally — no rate-limiting applied.
- Previously, contract errors were suppressed during high-churn periods. Now every event is recorded for complete auditability.

**Status**: ✅ Shipped in v3.23.0-fix.23.

---

## 40. ControlPingTimeout Enabled (30s Keepalive)

**Purpose**: Detect silent connection drops to the control plane faster.

**Files Modified**: `transport.go`

**Change**:
- `ControlPingTimeout` set to 30 seconds, enabling active keepalive pings on control-plane connections.
- Previously disabled, this ensures the provider detects a dead control connection within 30 seconds rather than waiting for the next application-level message or TCP timeout.

**Status**: ✅ Shipped in v3.23.0-fix.23.

---

## 41. Stale `proxy.lock` Detection After Crash

**Purpose**: Automatically detect and clean up stale lock files left behind after a provider crash, preventing "another operation in progress" errors on restart.

**Files Modified**: `provider/proxy_state.go`

**Change**:
- On startup, the provider checks if `proxy.lock` exists and whether the process that created it is still alive.
- If the owning process is gone, the lock file is removed and a warning is logged (`[proxy] cleaned stale lock from PID <N>`).
- Prevents manual intervention after a crash.

**Status**: ✅ Shipped in v3.23.0-fix.23.

---

## 42. Admission Gate Slot Leak Fixed

**Purpose**: Close a resource leak where admission gate slots were not released on certain error paths, causing the provider to gradually exhaust its admission budget and reject new connections.

**Files Modified**: `admission.go`

**Change**:
- All error return paths in the admission gate now properly release the acquired slot via `defer` or explicit release calls.
- Previously, a subset of early-exit paths skipped the release, leaking one slot per occurrence until the gate capacity was exhausted.

**Status**: ✅ Shipped in v3.23.0-fix.23.

---

## 43. Raised Default Throughput Ceilings

**Purpose**: Remove conservative defaults that artificially capped throughput on medium-to-large nodes. The fork's previous defaults were designed for memory-constrained environments, leaving significant bandwidth unused on production hardware.

**Files Modified**: `transport.go`, `ip.go`, `tuning.go`, `transfer_contract_manager.go`

**Changes**:
- **TransportBufferSize**: 1 → 16. Only 1 message was buffered in-flight between the protocol framer and WebSocket writer. This serialized all upstream traffic regardless of available bandwidth. Now matches the transfer buffer depth.
- **TCP/UDP MaxWindowSize**: 1 MiB → 4 MiB. Removes the ~160 Mbps per-connection throughput ceiling at 50ms RTT. UDP window raised to match.
- **applyTier3 sets actual performance values**: Previously `URNETWORK_PROFILE=auto` on 4GiB+ boxes left all settings at defaults. Now applies 2 MiB initial contracts, 256-depth IP buffers, 4 MiB TCP window, and 4 MiB transfer queues.
- **Tier 4 Extreme added for >= 8 GiB**: New `applyTier4` applies turbo-v8-equivalent settings (8 MiB windows, 16 MiB queues, 512 seq buf, GOGC 200) with a contract ramp scale of 3.
- **ContractTransferByteSeqScale**: 4 → 2. Reaches the 128 MiB standard contract in 2 sequences instead of 4, halving cold-start ramp time. Previously only turbo profiles got this.

**Status**: ✅ Shipped in v3.23.0-fix.24.

---

## 44. Relaxed Client-Side Auth Rate Limiter

**Purpose**: The server-side ConnectionRateLimit already caps auth connections per client IP hash (~200 conns/60s). The client-side limiter at 1 req/s (burst 3) was unnecessarily serializing fleet warmup on top of the server's own limits.

**Files Modified**: `provider/auth_rate_limiter.go`, `provider/auth_rate_limiter_test.go`

**Changes**:
- Default min: 1 → 20 req/s, max: 10 → 200 req/s, burst: 3 → 50
- Added `URNETWORK_AUTH_UNLIMITED=true` env var to bypass the limiter entirely
- The limiter is preserved (not removed) because it still provides adaptive 429 backoff protection

**Status**: ✅ Shipped in v3.23.0-fix.24.

---

## 45. CPU-Scaled MultiRaceClientCount

**Purpose**: Replace the hardcoded MultiRaceClientCount (2) with a value that scales with available CPU cores. More parallel sends at connection-establishment time means higher chance of winning the first-packet race.

**Files Modified**: `ip_remote_multi_client.go`, `ip_remote_multi_client_test.go`

**Changes**:
- Added `defaultMultiRaceClientCount()` function: 1-2 cores → 4, 3-4 cores → 6, 5-8 cores → 8, 9+ cores → 12
- The race cost is purely transient (parallel sends only at connection-establishment, not per-packet)

**Status**: ✅ Shipped in v3.23.0-fix.24.

---

## 46. Dynamic ContractFillFraction Based on RTT

**Purpose**: Replace the static ContractFillFraction (0.7) with a value that adapts to observed round-trip time. On high-latency links, contract bytes drain faster relative to the API round-trip, so more headroom prevents pipeline stalls. On low-latency links, we can fill closer to capacity.

**Files Modified**: `transfer.go`, `transfer_rtt.go`, `transfer_rtt_test.go`

**Changes**:
- Added `MeanRtt()` public method to `RttWindow`
- Added `computeFillFraction(meanRtt, fallback)`: RTT ≤ 100ms → 0.85, ≥ 1000ms → 0.50, linear interpolation between
- `SendSequence.contractFillFraction()` now delegates to `computeFillFraction`, falling back to the static settings value when no RTT data is available
- Added 3 unit tests for MeanRtt and fill fraction computation

**Status**: ✅ Shipped in v3.23.0-fix.24.

---

## 47. Sharded Packet Dispatch

**Purpose**: Replace the single-goroutine packet dispatch loop in LocalUserNat with N shard goroutines (one per CPU, capped at 16). Each shard has its own buffer instances and processes packets independently.

**Files Modified**: `ip.go`, `ip_test.go`

**Changes**:
- Packets are routed to shards via a deterministic FNV-1a flow hash of the IP 4-tuple (source/dest IP + ports), ensuring per-flow affinity
- Each shard runs its own `select` loop with independent UDP/TCP buffer instances
- `CallbackList` is already mutex-protected, so concurrent receives across shards are safe
- Added 7 unit tests for flowHash and pickShard

**Status**: ✅ Shipped in v3.23.0-fix.24.

---

## 48. MultiRaceClientCount to 16 (Unconditional)

**Purpose**: The CPU-based tier system (10-14-16) was unnecessarily conservative. All race goroutines are I/O-bound (block on network response), so single-core nodes benefit just as much as multicore ones. The runtime actually races `min(16, len(healthyProviders), packetBudget)`, so the value is just a ceiling — no downside to setting it high.

**Files Modified**: `ip_remote_multi_client.go`, `ip_remote_multi_client_test.go`

**Change**: `MultiRaceClientCount` = 16 on all platforms, replacing the CPU-tier function.

**Status**: ✅ Shipped in v3.23.0-fix.24.1.

---

## 49. CI Pipeline Improvements

**Purpose**: Faster feedback and less wasted compute during PR checks.

**Files Modified**: `.github/workflows/build.yml`, `.github/workflows/release.yml`

**Changes**:
- `build-and-push` now `needs: test-and-lint` — skips Docker build on failing PRs (~3 min saved)
- Re-added Go module cache (`~/go/pkg/mod`) alongside existing build cache
- Go tests (with `-race`) run before shell installer tests
- Fixed release.yml `checkout@v4` → `v6`, added caching

**Status**: ✅ Shipped in v3.23.0-fix.24.1.

---

## 50. Per-Minute Earning Windows (Independent Goroutine)

**Purpose**: Give operators real-time earning visibility without waiting for the ~5-minute health heartbeat. A separate goroutine polls `ProxyHealthSnapshot()` for cumulative billable across all proxies every 60 seconds and emits rolling windows.

**Files Modified**: `provider/main.go`

**Change**:
- Added `runEarningWindows(ctx)` — standalone goroutine with a 1-minute ticker
- Tracks cumulative billable (BillableRx + BillableTx) across all proxies each tick
- 60-sample ring buffer stores per-minute deltas with counter-reset guard
- Computes rolling sums: 1m, 5m, 15m, 60m
- Emits: `[earn] billable_1m=X billable_5m=Y billable_15m=Z billable_60m=W active=yes|no`
- `active=yes` when billable_1m > 0, `active=no` when idle
- Partial windows during warmup (no silent gaps before 60 minutes of data)
- Silent when `ProxyHealthCount() == 0` (non-proxy mode)
- Launched in `provide()` alongside other goroutines

**Status**: ✅ Shipped in v3.23.0-fix.24.8 (PR #121).

---

## 51. `urnet-tools update -f` Stops, Updates, and Restarts Automatically

**Purpose**: `urnet-tools update -f`/`--force` previously only swapped the binary on disk and printed "Restart the service when convenient" — the running provider was never touched, so unattended/scripted updates left the old binary running until a human intervened.

**Files Modified**: `urnet-tools` (or equivalent installer/update script)

**Change**:
- `stop_systemd_units()` now distinguishes a plain update from a force-update
- With `-f`/`--force`: stop the running provider (no confirmation prompt) → replace binary → restart the service automatically
- Plain `urnet-tools update` (no `-f`) is unchanged: swap binary in place, leave the service running, so auto-update timers are unaffected

**Status**: ✅ Shipped in v3.23.0-fix.24.9 (PR #125).

---

## 52. Hub Dashboard Per-Proxy Earning Column

**Purpose**: Make it visible at a glance which proxies (and nodes) are actively carrying billable traffic versus sitting up but idle, without digging through provider logs node by node.

**Files Modified**: `hub/main.go`

**Change**:
- New in-memory tracking on `store` (`prevBillable`, `earning` maps, nodeID → proxyID) computed in `upsert()` from the billable-bytes delta against the previous report
- `earning=yes` when a proxy's billable bytes (`BillRX+BillTX`) grew since the previous report **and** it currently has active clients — same criteria as the provider's own `[traffic]` log line
- Rendered in three places: per-proxy detail table (`Yes`/`No` badge), per-node summary row (`X/Y` earning count), and the top fleet summary bar (fleet-wide total)
- No wire format or SQLite schema change — purely a hub-side computed/rendered signal

**Status**: ✅ Shipped in v3.23.0-fix.24.9 (PR #124).

---

## 53. `[profit]` Heartbeat and `[contract]` Close Utilization Logging

**Purpose**: The 5-minute `[health]` heartbeat and 1-minute `[earn]` rolling windows are too coarse to answer "is billable traffic moving right now," and `[contract] acquired` only shows a contract was granted, never how much of it actually got used.

**Files Modified**: `provider/main.go`, `transfer_contract_manager.go`

**Change**:
- Added `runProfitHeartbeat(ctx)` — standalone goroutine with a 15-second ticker, using `ProxyHealthSnapshot()` (safe — doesn't disturb the health heartbeat's dead/recovered baseline)
- Sums billable bytes (`BillableRx+BillableTx`) and `Clients` across all proxies each tick; `earning=yes` when billable bytes grew since the last tick **and** clients > 0
- Emits: `[profit] earning=yes|no clients=N rate=X` (rate via existing `fmtRate` helper, e.g. `4.5 MB/s`)
- Log throttling to avoid flooding quiet/warmup periods: `earning=yes` logs every 15s tick; `earning=no` logs only on the first occurrence, on the immediate yes→no transition (so the exact stop time is visible), or after ≥5 minutes since the last log
- Added a `[contract] closed acked=X allotted=Y util=Z% destination=W` line in `CloseContractWithCheckpoint`, pairing with the existing `[contract] acquired size=X destination=Y` line — shows how much of a granted contract actually got used before it closed

**Status**: ✅ Shipped in v3.23.0-fix.24.9 (PR #126).

---

## 54. File Proxies Start Before URL-Sourced Proxies on Boot

**Purpose**: Operator-curated paid proxy lists loaded via `--proxy_file` should start authenticating before URL-scraped free proxies. The URL fetcher was racing ahead during startup, causing file-based proxies to queue behind URL proxy probes.

**Files Modified**: `provider/main.go`

**Change**:
- Moved `go runProxyURLFetcher(ctx)` and `go runProxyURLCleanup(ctx)` from line 1583 to line 2056 — after `reloader.StartWatcher(ctx)` ensures file proxies are fully loaded first
- No new env vars or CLI flags (YAGNI) — file-before-URL is the correct default by design
- No behavioral change after startup: URL refresh/cleanup tickers and hot-reload behavior are identical

**Status**: ✅ Shipped in v3.23.0-fix.24.9 (PR #127).

---

## 55. URL-Sourced Proxy Give-Up Backoff and Eviction

**Purpose**: Replace the flat 15-minute give-up-to-retry cycle for URL-sourced proxies with an escalating per-address backoff, and permanently evict addresses that prove hopeless after enough cycles. Fix a discovered bug where lifetime failure/give-up counters were silently wiped during the wait window between cycles.

**Files Modified**:
- `provider/proxy_failure_history.go` — added `giveUps` map, `RecordGiveUp`/`GiveUpCount` methods, extended `Reset`/`Prune` to cover new counter
- `provider/main.go` — added `proxyURLGiveUpRetryDelay` (15m→30m→1h→2h→4h→8h→16h→24h, +20% jitter), `proxyURLGiveUpEvictAfterCycles=10`; rewrote give-up site to use escalating delay and eviction; rewrote Prune call site to use `currentDesiredProxyAddresses` instead of live health registry
- `provider/proxy_url.go` — added `Blacklist map[string]time.Time` to `ProxyURLState`; `mergeProxyURLEntries` now skips blacklisted addresses
- `provider/proxy_url_source.go` — added `evictProxyURLAddress` (cache remove + blacklist + reload trigger) and `currentDesiredProxyAddresses` helper (file/internal + URL cache, independent of health registration)

**Change**:
- New `proxyURLGiveUpRetryDelay(giveUpCount)` computes escalating delay: cycle 1=15m, 2=30m, 3=1h, 4=2h, 5=4h, 6=8h, 7=16h, 8+=24h (capped), with up to 20% jitter
- `proxyFailureHistory` gains `giveUps` map tracking lifetime give-up cycles per address (not per-attempt), with same `Reset`/`Prune` lifecycle as `failures`
- `ProxyURLState.Blacklist` persisted to `proxy_url.json`; `mergeProxyURLEntries` skips any address present in the blacklist, enforcing permanent eviction at the only add path
- `evictProxyURLAddress` removes from cache, writes to blacklist, triggers hot-reload — called at cycle 10+ instead of scheduling another retry
- `currentDesiredProxyAddresses()` returns all addresses from file/internal + URL cache, used by both `globalProxyFailureHistory.Prune` and `globalProvenProxies.Prune` — fixes the bug where `keepAddrs` was built from live health registry `report.Bandwidth`, which drops give-up'd proxies during their wait window

**11 new unit tests** covering: give-up counter, Reset/Prune for giveUps, delay schedule (monotonic increase + cap + jitter bounds), blacklist round-trip, blacklist enforcement on merge, eviction (removal + blacklist + reload trigger), blacklist surviving a fetch cycle, desired-address-set helper (file merge, internal config fallback, URL-cache-only address survives health absence).

**Status**: ✅ Implemented (pending PR).

---

## 52. `urnet-tools update` Self-Update Fix

**Purpose**: Fix `urnet-tools update` to actually fetch the latest `urnet-tools` script from GitHub when updating, and bundle the script in the provider release tarball for offline-capable installation.

**Files Modified**:
- `scripts/Provider_Install_Linux.sh` — priority: tarball-bundled `urnet-tools` > GitHub fetch > `cat "$0"`; removed `[ -n "$URNET_INSTALL_URL" ]` guard that blocked GitHub fetch during normal `update` because the env var is only set for dev/testing overrides
- `.github/workflows/release.yml` — copies `scripts/Provider_Install_Linux.sh` into release tarball as `urnet-tools`

**Change**:
- New three-tier priority for script source: bundled tarball (highest) → GitHub raw fetch → current script on disk (fallback)
- Removed the `&& [ -n "$URNET_INSTALL_URL" ]` condition from the update path — this was the bug causing `urnet-tools update` to never fetch the latest script
- Bundling script in tarball ensures `urnet-tools update` works even when GitHub is unreachable (the tarball contains the matching script for the release)
- Nested `if` structure (instead of `{ }` grouping) for POSIX sh / dash compatibility

**1 dash-compatible test** (`test_fallback_logic.sh`) passing.

**Status**: ✅ Merged (PR #136, v3.23.0-fix.24.12). **Note**: v24.12 has chicken-and-egg bootstrap issue — the fix can't propagate from old scripts. v24.13 fixes the bootstrap by checking `$workdir/urnet-tools` in the common script-writing section.

---

## 56. `proxy remove --all` Clears URL Cache and Source URLs

**Purpose**: `proxy clear` was clearing the internal config and `proxy.state`, but leaving `proxy_url.json` untouched. The cached URL proxies (previously fetched from `--proxy_url` sources) survived the clear and were re-loaded on restart, and the configured source URLs caused the background fetcher to re-add free proxies within minutes — defeating the clear entirely.

**Files Modified**:
- `provider/main.go` — `proxyRemove()` now also wipes `urlState.Cache` and resets `urlState.Sources` to nil in `proxy_url.json`, alongside the existing proxy.state reset

**Change**:
- `proxy remove --all` now reads `proxy_url.json`, clears both `Cache` and `Sources`, and writes back
- Source URLs must be re-added via `urnet-tools proxy add-source` if URL fetches are desired again
- Comment updated to reflect that both cache and sources are cleared

**Status**: ✅ Merged (PR #139, v3.23.0-fix.24.16).

---

## 57. Proxy Launch Order Sorted for File-Before-URL Priority

**Purpose**: The proxy launch order at startup was non-deterministic (Go random map iteration over `proxyDesiredSet`), so file-based and URL-sourced proxies were interleaved in the launch sequence. File proxies (paid, operator-curated) should start connecting before URL proxies (free, scraped).

**Files Modified**:
- `provider/main.go` — `provide()` now sorts `allProxySettings` by source after building the slice

**Change**:
- Added `sort.SliceStable` call after building `allProxySettings` from the desired set, ordering by source: `file`/`internal` before `url`
- `backoffPacer` uses the slice index for startup delay (`n * staggerMs`), so file proxies get a head start of ~len(file proxies) × 1s before URL proxies begin
- Added `"sort"` import

**Status**: ✅ Merged (PR #139, v3.23.0-fix.24.16).

---

## 58. Hot-Path Allocation Optimizations (Upstream d474f36b Port)

**Purpose**: Eliminate per-packet heap allocations on hot send/receive/ack/forward/teardown paths. Ported from upstream d474f36b.

**Files Modified**: `transfer.go`, `ip.go`, `ip_remote_multi_client.go`

**`transfer.go`** (PR #150):
- `safeAck()` standalone function — eliminates per-send closure alloc on every ack callback.
- `Snapshot()` returns by value + `clear()` map reuse — eliminates pointer alloc per ack window read.
- `time.After()` → `time.NewTimer().Reset()` in 6 hot loops — eliminates per-iteration timer+channel alloc.

**`ip.go`** (PR #149):
- `StreamState.IpPath()` lazy-cached — eliminates IpPath struct alloc per UDP datagram.
- `singleDataPacket [1][]byte` reuse — eliminates slice-header alloc for unfragmented case.
- `ParseIpPathWithPayload` shared `ipBacking` slice — eliminates 2× `make(net.IP)` per packet.

**`ip_remote_multi_client.go`** (PR #148):
- `waitForIdle`/`rst` closures hoisted to methods — eliminates per-packet closure alloc in teardown.
- `ipPacketToProviderFrame` helper — avoids per-packet proto wrapper struct allocs on v2+ path.

**Status**: ✅ Merged `main` (2026-06-26). PRs #148–#150.

---

## 59. Dual-Stage SOCKS5 + API Reachability Probe for URL Proxies

**Purpose**: Free public proxy lists are mostly dead entries that waste auth-rate-limiter slots and generate log noise. The existing SOCKS5 greeting probe filtered out non-SOCKS5 endpoints, but proxies that passed the greeting could still fail to route traffic to `api.bringyour.com` through the tunnel — resulting in infinite retry loops (51+ attempts observed in production).

**Files Modified**: `proxy_probe.go`, `proxy_url.go`, `proxy_url_source.go`, `provider/main.go`, `proxy_probe_test.go`, `proxy_url_source_test.go`

### Changes

**Unified dual-stage probe** (`proxy_probe.go`):
- `probeProxy()` performs both the SOCKS5 greeting AND a SOCKS5 CONNECT to `api.bringyour.com:443` on a single TCP connection
- Returns one of three results: `probeDead` (not SOCKS5), `probeSocks5Only` (SOCKS5 but can't reach API), `probeAPIReachable` (fully verified)
- API destination IP resolved once via `resolveAPIProbeAddr()` and cached across all probes — no DNS storm from 50 parallel CONNECTs
- 100ms random stagger before each probe dial spreads the concurrent burst across ~5s
- `probeAndFilterProxyURLLines()` replaces `filterReachableProxyURLLines()`, returning API-reachable and socks5-only addresses in separate buckets
- SOCKS5-only addresses are cached with `ProbeOK=false` for background retry by the reaper; API-reachable addresses are cached with `ProbeOK=true` and enter the auth queue immediately

**Proxy URL entry tracking** (`proxy_url.go`):
- `ProxyURLEntry` gains three new fields: `ProbeOK` (passed API probe), `ProbeFails` (consecutive failure count), `LastProbe` (last probe timestamp)
- Existing `proxy_url.json` files without these fields work correctly — zero values default to `false`/`0`/zero time, triggering re-probe on the next reaper cycle

**Background reaper** (`proxy_url_source.go`):
- `runURLProxyReaper()` scans the URL cache every 5 minutes for entries with `ProbeOK=false`
- Re-probes each entry with the dual-stage check
- After 3 consecutive `probeSocks5Only` or `probeDead` results, the address is moved to the persistent `Blacklist`
- Launched at provider startup alongside the URL fetcher

**Blacklist pruner** (`proxy_url_source.go`):
- `pruneURLProxyBlacklist()` removes blacklist entries older than 24 hours every 30 minutes
- Gives previously-dead addresses a second chance: on the next fetch cycle, `mergeProxyURLEntries` no longer skips them, and they're re-probed from scratch

**Auth-time probe** (`provider/main.go`):
- `probeProxySocks5()` preserved as a thin wrapper for the pre-auth SOCKS5 gate on URL-sourced proxies (doesn't test API reachability — that's handled by the fetch pipeline and reaper)

### Status
✅ Merged `main` (2026-06-27). PR #152.

---

## 60. IP Security DPI Refactor — Layered Packet Inspection (Upstream ac91c55)

**Purpose**: Replace the monolithic `ip_security.go` (~66K lines, mostly a `map[[4]byte]bool` blocklist) with a layered deep-packet-inspection pipeline that separates static endpoint reputation, BitTorrent signature detection, and web-standard protocol recognition. Provides payload-level BitTorrent detection instead of port-only heuristics, and adds IPv6 blocklist support.

**Files Added**:
- `ip_security_cfaa.go` — Static endpoint-reputation detector (blocked IP ranges + port/protocol policy). Three-way verdict: drop/allow/pass-to-DPI.
- `ip_security_cfaa_block.go` — Packed binary-search IP blocklist (44225 IPv4 ranges + 513 IPv6 ranges as of 2026-08-19 sync). Replaces 66K-line `map[[4]byte]bool`.
- `ip_security_dmca.go` — Stateful deep-packet inspection: BitTorrent signature detection (BEP 3/5/15/29), entropy-based encrypted-flow heuristic, 16-shard LRU flow table.
- `ip_security_webstandard.go` — Stateless TLS/DTLS/QUIC/STUN byte-signature matcher. Exempts legitimate encrypted flows from the DMCA entropy heuristic.

**Files Modified**:
- `ip_security.go` — `SecurityPolicy.Inspect()` interface gains `payload []byte` parameter. Egress rewritten as CFAA → DMCA two-layer pipeline. Ingress uses CFAA source-endpoint check. Exported `NewEgressSecurityPolicy()`, `NewIngressSecurityPolicy()` constructors.
- `ip.go` — `ClientReceive` and `SendPacket` use `ParseIpPathWithPayload` and pass payload to `Inspect`.
- `ip_remote_multi_client.go` — Both `Inspect` call sites updated to pass payload/nil.
- `net_tls.go` — Added `TlsContentType` type and constants (0x14–0x18) for web-standard byte matchers.

**Key Behavior Changes**:
- **Payload-level DMCA detection**: BitTorrent handshake signatures (BEP 3 peer wire, BEP 3 HTTP tracker, BEP 5 DHT KRPC, BEP 15 UDP tracker, BEP 29 uTP) now detected from L7 content, not just port heuristics.
- **IPv6 blocked subnets**: `cfaaBlockedPrefix6Data` introduces 214 IPv6 prefix ranges from Spamhaus DROPv6 and other feeds. Previously, IPv6 was unchecked (`// FIXME`).
- **Blocklist format change**: 66K-line `map[[4]byte]bool` replaced by ~8K-line packed string + binary search. Same feeds, zero-allocation lookup.
- **Port policy refined**: Three-way verdict (drop/allow/pass-to-DPI) instead of binary allow/drop. Known-safe protocols (NTP, IKE, DNS/UDP) skip DPI entirely.

**Fork Adaptation**: Dropped `Ip6Path.ServerName` reference in `ip_security_dmca.go` (fork's `Ip6Path` is pure 5-tuple; upstream uses `ServerName` for flow affinity which is unused here).

**How to Identify in New Upstream**:
- The monolithic `ip_security.go` with `var blockIp4s` is gone; replaced by `ip_security_cfaa*.go`, `ip_security_dmca.go`, `ip_security_webstandard.go`
- Search for `cfaaDetector`, `dmcaDetector`, `webStandardDetector` types
- `SecurityPolicy.Inspect(provideMode, ipPath, payload)` signature with 3 params

**Status**: ✅ Merged `main` (2026-06-28). PR #160.

---

## 61. Fix `urnet-tools` No-Args Fallback to Install

**Purpose**: Running `urnet-tools` with no arguments was triggering a full install (fetching release tarball, prompting for restart) instead of showing the help menu. This broke the UX for 5 releases.

**Root Cause**: Commit `c29facf` (v24.18) added a `cp "$workdir/urnet-tools" "$install_path/bin/urnet-tools"` that overwrites the installed script with the tarball-bundled copy, stripping the injected `URNETWORK_TOOLS_MODE=1` env var. Without that flag, the empty-argument fallback defaults to `operation="install"`.

**Fix**: Replaced the env-var injection approach with a direct `$0` path check. When the script's path ends with `urnet-tools`, it shows help on no args instead of defaulting to install. Removed the now-dead `URNETWORK_TOOLS_MODE` injection code.

**Affected Releases**: v3.23.0-fix.24.18 through v3.23.0-fix.24.22. Escape hatch: `urnet-tools update` still works on broken versions (the bug only triggers on empty args) and will download the fixed script.

**How to Identify in New Upstream**: Search for `URNETWORK_TOOLS_MODE` — if it still exists, the old injection approach is in use. The fix is the `case "$0" in *"/urnet-tools")` check in the no-ops fallback block.

**Status**: ✅ Merged `main` (2026-06-28). PR #161.

---

## 62. Codebase Audit Fixes — Error Logging, DoH Pinning, Dead Config Cleanup

**Purpose**: Address findings from a systematic codebase review of the provider and connect packages. Four PRs fixing silent error discards, a security gap, operational hazards, and dead code.

### 62a. Error Propagation for Reload/State Writes (PR #163) — H1, H2

**Problem**: `writeReloadTrigger()` and `writeProxyState()` silently discarded errors with `_ =`. If a write failed (disk full, permissions), hot-reloads silently stopped working and proxy.state went stale — no log, no alert.

**Fix**: All 6 production call sites now log a `tlog` warning on failure. No change to the success path.

**Files**: `provider/main.go` (4 sites), `provider/proxy_url_source.go` (2 sites)

### 62b. DoH Certificate Pinning (PR #164) — H6

**Problem**: The DNS-over-HTTPS resolver built its own `http.Transport` with no TLS config. The `TLSClientConfig` field was commented out with a `// FIXME`. An attacker who could intercept DNS traffic could MITM DoH responses — no cert pinning.

**Fix**: DoH now uses `DefaultTlsConfig()` which pins the ISRG Root X1/X2 certs, same as every other TLS connection the provider makes. Removed stale FIXME comments.

**Files**: `net_http_doh.go`

### 62c. Reload Watcher and Proxy Probe Error Handling (PR #165) — M3, M1

**Problem**: Two issues:
- `readReloadSeq` errors in the reload watcher were merged into the "no change" branch (`if err != nil || seq == lastSeq`), making transient FS read failures spuriously trigger a full proxy reload (auth storm).
- `conn.SetDeadline()` return values were discarded at both probe stages. Stage 2 had no context timeout backup — if `SetDeadline` failed the probe could hang indefinitely.

**Fix**:
- Split the error check from the sequence comparison. Read errors now log a warning and skip the poll cycle instead of spurring a reload.
- Stage 1 deadline errors log a warning (context timeout is backup). Stage 2 deadline errors log and return `probeSocks5Only` so the probe doesn't hang.

**Files**: `provider/proxy_reload.go`, `provider/proxy_probe.go`

### 62d. Remove Dead Config Fields (PR #166) — H5

**Problem**: `ContractManagerSettings` had two config fields (`LegacyCreateContract`, `TrackUsedContracts`) that were always `false`, never set to `true` anywhere, and marked `// TODO remove`. The code paths guarding them were dead.

**Fix**: Removed both fields from the struct, defaults, and all referencing code paths. Cleaned up test files (4 test files, 6 reference removals).

**Investigated, not changed**: `MultiRouteSelector.Read` returning `nil, nil` on timeout (H3). The FIXME expressed uncertainty but the nil return is the correct signal — `transfer_stream_manager.go:420` checks `if transferFrameBytes == nil` to trigger stream idle-close. Changing it would break stream teardown.

**Files**: `transfer_contract_manager.go`, `transfer_contract_manager_test.go`, `transfer_encrypt_contract_test.go`, `transfer_test.go`

### How to Identify in New Upstream
- Search for `_ = writeReloadTrigger` or `_ = writeProxyState` in the provider directory — if any remain, the fix hasn't been fully ported.
- Search for `// FIXME DoH` in `net_http_doh.go` — if found, DoH cert pinning hasn't been ported.
- Search for `LegacyCreateContract` or `TrackUsedContracts` — if found, the dead config cleanup hasn't been ported.

**Status**: ✅ Merged `main` (2026-06-28). PRs #162, #163, #164, #165, #166.

---

## 63. DoH System Cert Pool + `isHttpRequest` Detection (Upstream b6ee955 Port)

**Purpose**: Fix two issues found post-v24.24: (1) applying the narrow LE-only cert pool to DoH broke all four DoH providers (Cloudflare, Google, Quad9, OpenDNS), (2) port upstream's `isHttpRequest` check to fix false positives in DPI.

### 63a. DoH Uses System Cert Pool (PR #167)

**Problem**: `DefaultTlsConfig()` only pins ISRG Root X1/X2 — correct for `api.bringyour.com` but none of the four DoH providers use Let's Encrypt. Every TLS handshake failed silently, falling back to plain UDP DNS.

**Fix**: DoH now leaves `TlsConfig` nil, letting Go's `net/http` use the system cert pool automatically. Restores working encrypted DNS and matches upstream behavior.

**Files Modified**: `net_http_doh.go`

### 63b. `isHttpRequest` Detection (Upstream b6ee955)

**Purpose**: Upstream commit `b6ee955` added plaintext HTTP/1.x request line detection so radio/media streaming on non-standard ports isn't falsely flagged by the encrypted-traffic entropy heuristic. The check fires after BitTorrent signatures so HTTP-tracker GET is still classified correctly.

**Files Added**: 36-line `isHttpRequest` function in `ip_security_dmca.go`

**Files Modified**: `net_http_doh.go` — major DoH restructure (dnsmessage parsing, MinCacheTtl, MaxConcurrentResolutions, 4 DoH servers with wire-format support, local DoH) with fork's cert pinning applied via `DefaultTlsConfig()` injected into `DefaultDnsResolverSettings()`.

**Status**: ✅ Merged `main` (2026-06-29). PR #167.

---

## 64. Hub TLS, Live Heartbeat, SSE Dashboard Push, Dashboard Polish (PR #186, #188)

**Purpose**: Enable encrypted provider-to-hub reporting with trust-on-first-use cert pinning, add a lightweight in-memory heartbeat endpoint for sub-minute dashboard freshness, push real-time updates to browser tabs via Server-Sent Events, and visually polish the dashboard.

### 64a. Hub TLS with Cert Pinning (PR #186)

**Problem**: Provider bandwidth reports were sent over plain HTTP. An attacker on the same network could read or modify report data. The upstream hub has no TLS support.

**Solution**: The hub binary accepts a `-tls-addr` flag (`URNETWORK_HUB_TLS_ADDR` env, default `:8443`). On first boot with TLS enabled, the hub auto-generates a self-signed ECDSA P-256 certificate and starts an HTTPS listener in addition to the existing HTTP listener. A `/api/cert` endpoint exposes the SHA-256 fingerprint for trust-on-first-use pinning.

**Provider side**:
- `bandwidth_reporter.go` uses `newClientForURL()` which detects HTTPS URLs and creates an HTTP client with a `VerifyConnection` callback that checks the peer cert's SHA-256 fingerprint against `~/.urnetwork/hub.pin`
- Cert mismatch: connection refused with a descriptive error + debug info written to `/tmp/hub-tls-debug.txt`
- The dashboard shows a green padlock icon next to nodes that reported via TLS

**`urnet-tools hub` subcommands**:

| Command | What it does |
|---|---|
| `hub init` | Enables TLS via `URNETWORK_HUB_TLS_ADDR=:8443`, restarts, waits for cert gen, prints fingerprint + firewall hint |
| `hub link https://host:8443` | Fetches fingerprint from `/api/cert`, prompts for confirmation, pins to `hub.pin` and sets `report_url` |
| `hub unlink` | Removes pin, rewrites report URL to plain `http://:8080` |
| `hub test [url]` | Connects via openssl to verify the TLS cert fingerprint matches the pinned value |

**Files Added**: `hub/tls.go` (cert generation / fingerprint / API endpoint / TLS config applied in `main()`)

**Files Modified**: `hub/main.go`, `hub/store_db.go`, `provider/bandwidth_reporter.go`, `urnet-tools` (hub subcommand methods)

### 64b. Transactional Hub Update (PR #186)

**Problem**: Updating the hub binary was a manual, error-prone process with no rollback safety.

**Solution**: `hub update` is atomic and rollback-capable:

**Sequence**: stop service → backup `hub.db` → download to same-fs temp file → verify `--version` → copy old binary to `.old` → `rename()` new in → create systemd unit if missing → start service → verify it came up.

**Rollback**: any failure restores the old binary + DB backup + restarts the old service with a descriptive error. After success, keeps `hub.db.bak` as safety net and removes `.old`.

**Idempotent**: exits 0 with "Nothing to do" if already at the target version (unless `--force`).

**Testing**: 40 test cases in `scripts/test_hub_update.sh` covering tag resolution, idempotency, rollback states, systemd templating, E2E, Docker wrapper.

**Files Added**: `scripts/test_hub_update.sh`

**Files Modified**: `urnet-tools` (hub update subcommand methods)

### 64c. Live Hub Heartbeat + SSE Push (PR #187, #188)

**Problem**: Full reports arrive every 5-15 minutes, so dashboard Mbps, client counts, and contract rates can be minutes stale. Increasing the report cadence would hammer the hub DB.

**Solution**: Two layered additions:

**`/api/heartbeat`** — lightweight POST endpoint (15s default cadence, configurable via `HEARTBEAT_INTERVAL` env):
- Carries `node_id`, `mbps_rx`, `mbps_tx`, `clients`, `conns`, `heap_mib`, `sys_mib`, `uptime`, `contracts_acquired`, `contracts_denied`
- Per-proxy status array included only for proxies whose status or contract counters changed since the last tick (`filterChangedProxies`)
- Zero DB writes — merges into in-memory node state only
- One `http.Client` reused per reporter instance (no TCP+TLS handshake per tick)
- Consecutive failures back off exponentially (capped at 5m)

**`GET /api/events`** (SSE) — pushes a bare `data: refresh` to connected dashboard tabs the instant a heartbeat or report lands:
- Implemented via an in-process `broadcaster` (non-blocking, nil-safe)
- Dashboard subscribes via `EventSource` and re-fetches node metadata on push
- Existing 30s poll stays as backstop for environments where SSE is buffered/stripped

**Blocker**: `api.go:552` was not reachable via `make all` + `go get` flow because `golang.org/x/net/context` is a deprecated alias. The fix replaces `x/net/context` with `context` from the stdlib.

**Files Modified**: `hub/main.go`, `provider/bandwidth_reporter.go`, `api.go`

### 64d. Dashboard Visual Polish (PR #186)

**Problem**: The node table had a grouped-header layout that wasted space, no source-IP visibility for multi-node fleets, no TLS indicator, and columns were not sortable.

**Changes**:
- **Inline IP tags**: Each node row shows its source IP as a color-coded badge. Same-NAT boxes (same IP) share a color from a 10-color palette so they cluster visually.
- **TLS padlock**: A green lock icon appears next to nodes that reported via HTTPS.
- **Sortable columns**: Click any column header to sort ascending/descending. Active sort column shows ▼/▲ indicator.
- Removed group-header rows in favor of a simpler flat layout.
- `fmtBytes` defensive against `undefined` input.

**Files Modified**: `hub/main.go` (template + static JS/CSS), `hub/node_info.go`

**Status**: ✅ Merged `main` (2026-07-02). PRs #186, #187, #188.

### 64e. DNS Cache, Dial Timeout, Connecting-State Cleanup (PR #190)

**Purpose**: Fix proxy warmup death spiral on nodes with large proxy pools (2000+). Three compounding issues caused 100% CPU, thousands of goroutines stuck in "connecting" state, and swap thrashing during warmup.

**Changes**:

1. **DNS cache** (`net.go`): The `net.DefaultResolver.LookupIP` call added in PR #189 was hitting the system resolver on every SOCKS5 dial. With 2000+ concurrent warmup goroutines resolving the same hostnames simultaneously, the resolver became a bottleneck. Fix: cache hostname-to-IPv4 lookups behind `sync.Mutex` with a 60s TTL. Falls back to stale entries on transient resolver failures.

2. **Dial timeout** (`net.go`): The startup warmup creates proxy goroutines with `context.WithCancel` (no deadline). A SOCKS5 proxy that accepts TCP but never responds to the handshake could pin a goroutine indefinitely. Fix: apply a 30s timeout to the SOCKS5 dial when the caller's context has no deadline. Paths with existing deadlines (e.g., `serialEval` with 15s `RequestTimeout`) are unaffected.

3. **Connecting-state fix** (`proxy_health.go`): `markProxyDown` now clears `h.connecting`. Previously, only `markProxyUp` cleared it, so a proxy whose initial connection attempt failed stayed in "connecting" state forever, making the health snapshot show thousands of "connecting" proxies that were actually stuck.

**Files Modified**: `net.go`, `proxy_health.go`

**Status**: ✅ Merged `main` (2026-07-03). PR #190. v3.23.0-fix.24.32.

### 64f. Transport Mode Monitor Self-Wake Fix (PR #191)

**Severity**: High — caused 100% CPU on all deployments since first shipped in v3.23.0-fix.24.27 (June 30, 2026).

**Root Cause**: Commit `1f64686` (PR #188, live heartbeat SSE push) added `modeMonitor.NotifyAll()` to `PlatformTransport.setActiveMode()` so that goroutines waiting on transport mode changes would be woken immediately. However, `PlatformTransport.run()` calls `setActiveMode()` on every loop iteration unconditionally — even when the selected mode is already active. `NotifyAll()` closes the notification channel that `run()` literally just captured half an iteration earlier via `modesAvailable()`. The `select { case <-notify: }` fires instantly, creating a self-wake feedback loop that runs the loop body (map clone, extract keys, sort, mutex operations) at ~8,000 Hz per CPU core.

Each `NotifyAll` also wakes 4+ mode-waiting goroutines (H1/H3 read/write loops), which check the mode, find nothing changed, and re-park — creating a futex storm and saturating both cores.

**Fix**: Gate `NotifyAll()` on actual state change — only fire the wake signal when the mode or availability genuinely transitions. `run()` now blocks normally on its select, waiting for real mode changes from `setModeAvailable` or external triggers. Same guard applied to `setModeAvailable()`.

**Affected releases**: v3.23.0-fix.24.27 through .32. First shipped June 30, 2026.

**Diagnostic** (all shipped in v3.23.0-fix.24.33):
- `setActiveMode` now logs a rate-limited warning when called with the mode already active

**Follow-up diagnostics shipped in v3.23.0-fix.24.34**:
- `[health]` log line now includes `goroutines=N` for spotting goroutine leaks
- Provider logs `[startup] provider version=...` exactly once at process startup (before proxy setup)

**Files Modified**: `transport.go`, `provider/main.go`

**Status**: ✅ Merged `main` (2026-07-03). PR #191. Core fix shipped in v3.23.0-fix.24.33; diagnostics above shipped in v3.23.0-fix.24.34.

### 64g. Remove Redundant WARP_VERSION Env Var

**Purpose**: The `WARP_VERSION` environment variable was a legacy override for the binary version string. It duplicated the linked-in `main.Version` (set via `-ldflags`) and was only set in Docker to the same value. If someone ran `urnet-tools update` inside a Docker container, the version string would freeze at the image's build version instead of showing the actual running binary version.

**Changes**:
- `RequireVersion()` now returns `Version` directly instead of checking `WARP_VERSION` first
- Removed `ENV WARP_VERSION=${VERSION}` from the Dockerfile
- Added a single `[startup] provider version=...` log line at the top of `provide()` so every deployment shows the running binary version once at process startup

**Files Modified**: `provider/main.go`, `Dockerfile`

**Status**: ✅ Merged `main` (2026-07-03). v3.23.0-fix.24.34.

---

## 65. Core Networking Audit Fixes (PR #200–#206)

**Purpose**: Fix 7 correctness and performance bugs found in a July 4, 2026 deep-dive audit of the fork's transfer/contract/route-manager/message-pool code. Five correctness bugs (one HIGH, three MEDIUM, one LOW), one throughput config change, and one tuning change.

### 65a. HIGH — Park resend-capped items instead of dropping (transfer.go)

When `sendCount >= MaxResendCount` (16), the old code continued without re-adding the item to the resend queue. The item was already removed at line 2125, so it stayed orphaned in `sendItems`. When a cumulative ack later reached that item, the implicit-ack loop called `resendQueue.RemoveByMessageId`, got nil, and hit `panic("Missing item")`. HandleError would recover but the entire send sequence was torn down — pending sends erred, contracts flushed.

Reachable under ordinary sustained loss (16 resends at ≥2s `ScaledRtt` fit inside 60s `AckTimeout`), worst at outage recovery.

**Fix**: Park instead of drop — set `resendTime = sendTime + AckTimeout` and re-`Add` to the queue. Retransmission stops but ack/teardown bookkeeping stays consistent. The ack-timeout check at line 2107 uses `sendTime` (never updated on resend), so once `sendTime + AckTimeout` elapses, the loop closes the entire sequence gracefully.

**Status**: ✅ Merged `main` (2026-07-04). PR #201. v3.23.0-fix.25.

---

### 65b. MEDIUM — Dispatch ContractStatus on Trust/Invalid errors (transfer_contract_manager.go)

`HandleControlFrame` constructed `ContractStatus` structs in both error branches (Trust on `addContract` failure, Invalid on `ProtoUnmarshal` failure) but `return err` fired before `self.contractStatus()` dispatch. Only the success branch dispatched. Platform denials (the `contractErrors` loop) did dispatch, so hub metrics were mostly correct, but locally-rejected and malformed contracts were invisible to `registerContractCallback` (provider contract metrics) and multi-client penalty logic.

**Fix**: Call `self.contractStatus(contractStatus)` before the `return err` in both error branches.

**Status**: ✅ Merged `main` (2026-07-04). PR #202. v3.23.0-fix.25.

---

### 65c. MEDIUM — Pooled buffer leak on write timeout (transfer_route_manager.go, transfer_stream_manager.go)

`writeMaybeWrappedBytes` calls `MessagePoolShareReadOnly` to increment the pool message refcount before passing to `Write`/`WriteDetailed`. When `WriteDetailed` failed (timeout, ctx-cancelled, done channel), the shared ref was never returned — the `MessagePoolReturn` calls were commented out. Each timed-out write (WriteTimeout=15s) permanently stranded one refcount. `reflect.Select` guarantees the send case didn't fire on those branches, so returning is safe.

The caller-side `StreamSequence` at `transfer_stream_manager.go:429` passes a raw (unshared) buffer from `Read()` and returns it itself on failure. After restoring `WriteDetailed`'s returns, both callee and caller returned the same buffer — creating a double-free that could reassign a live buffer to another goroutine (silent cross-sequence corruption under production concurrency).

**Fix (v2)**: Restore `MessagePoolReturn` in `WriteDetailed`'s contextDoneIndex/doneIndex/timeoutIndex branches. Remove the two redundant `MessagePoolReturn` calls in `transfer_stream_manager.go`'s err/!success branches — `WriteDetailed` handles the return on all failure paths; on success the route owns the buffer.

**Status**: ✅ Merged `main` (2026-07-04). PRs #203, #206. v3.23.0-fix.25.

---

### 65d. LOW — MinRtt returns garbage, setActive ignores param (transfer_rtt.go, transfer_route_manager.go)

Two latent bugs with no current impact:

- `rttHeap.MinRtt()` returned `items[n-1]` — an arbitrary heap leaf, neither min nor max. Min is `items[0]`. Unused today; fixed before anyone builds on it.
- `MultiRouteSelector.setActive(route, active)` always set `routeActive[route] = false`, ignoring the `active` param. Both current callers pass `false` so behavior is correct; fixed to remove the footgun.

**Status**: ✅ Merged `main` (2026-07-04). PR #200. v3.23.0-fix.25.

---

### 65e. Performance — Default resend queue 2→4 MiB (transfer.go)

Commit `c3cefc7` raised TCP/UDP `MaxWindowSize` to 4 MiB, but `DefaultSendBufferSettings.ResendQueueMaxByteCount` stayed at `mib(2)`. Transfer-layer in-flight is capped at `min(window, resend_queue)`, so the effective per-sequence ceiling was 2 MiB/RTT (~160 Mbps at 100ms) — half the window ceiling.

**Fix**: Raise to `mib(4)`. Tier3 auto and turbo already set 4 MiB queues, so the value is fleet-proven. Memory cost is +2 MiB per active send sequence. Profiles that set their own queue size explicitly (tier1/lowmem/tier2/tier3/turbo) are unaffected.

**Status**: ✅ Merged `main` (2026-07-04). PR #204. v3.23.0-fix.25.

---

### 65f. Tuning — RTT fill-fraction floor 0.5→0.7 (transfer.go)

`computeFillFraction` linearly interpolates fill fraction from 0.85 (RTT ≤100ms) to a floor (RTT ≥1s). The floor was 0.5, meaning only ~64 MiB of a 128 MiB standard contract was consumed before renegotiation on ≥1s-RTT clients — doubling contract-negotiation frequency for the slowest links (mobile, satellite, remote).

**Fix**: Raise the floor to 0.7 (~90 MiB consumed). Halves contract churn on high-RTT paths while the 0.85 ceiling still provides a 0.15 head start on renegotiation vs the hot path. 60s AckTimeout + 15s WriteTimeout provide ample slop if renegotiation is slower than expected.

**Status**: ✅ Merged `main` (2026-07-04). PR #205. v3.23.0-fix.25.

---

### 65g. Cleanup — gofmt

Tab indentation introduced by PRs #201, #202, #203 in `transfer.go`, `transfer_route_manager.go`, and `transfer_contract_manager.go` was cleaned up with `gofmt -w`.

**Status**: ✅ Merged `main` (2026-07-04). PR #206. v3.23.0-fix.25.

---

## 66. Message Pool N-Way Mutex Sharding (PR #207)

**Purpose**: Eliminate per-size-class lock contention — the dominant synchronization bottleneck on the provider hot path. At fleet-node packet rates (hundreds of K pps), every ~1500B packet's Get/Put/Return/ShareReadOnly previously serialized on a single `stateLock` per size class, capping per-node throughput.

### 66a. Internal sharding

Each `messagePool` now holds N `poolShard` structs (N=16 default), each with its own freelist, mutex, nextId counter, and per-tag accounting arrays. No buffer ever migrates across shards — a buffer created in shard K stays in shard K for life.

**Shard selection**: Per-pool atomic `shardNext` counter, incremented on every Get. `shardIndex = counter & (shardCount-1)`. Round-robin; guaranteed even distribution.

**Shard routing on Return**: The shard index is stored in buffer metadata at `size+12` at creation time. `MessagePoolReturn`, `MessagePoolShareReadOnly`, and `MessagePoolCheck` all read this byte and lock only the designated shard. Every read path has a bounds check — a hypothetically corrupted byte fails safe (drops buffer) rather than panicking or accessing wrong shard.

### 66b. Metadata layout change

`MessagePoolMetaByteCount` bumped 12→13. Byte `size+8` remains the tag byte (used by `MessagePoolGetDetailedWithTag` for per-caller accounting). The new byte at `size+12` is the shard index.

```
[size : size+8]      — nextId (uint64, 8 bytes)
[size+8]             — tag (uint8, 0-254=active, 255=untagged)
[size+9]             — flags (uint8, MessagePoolFlagShared)
[size+10 : size+12]  — refcount (uint16, 2 bytes)
[size+12]            — shard index (uint8, NEW)
```

All `make([]byte, size+MessagePoolMetaByteCount)` call sites automatically use the new size via the constant. No migration needed — on restart, old 12-byte-metadata buffers are GC'd; new allocations use 13 bytes.

### 66c. Rollback lever

Env var `URNETWORK_MESSAGE_POOL_SHARD_COUNT` overrides the shard count (must be power of two, 1–256). Set to 1 for functionally identical pre-sharding behavior — all Gets route to shard 0, one mutex behind the scenes.

### 66d. Tag accounting

`takenTags`, `returnedTags`, `createdTags` are per-shard (`[256]uint64` on each `poolShard`). `poolStats` and `MessagePoolStats` iterate all shards, summing each tag column. `ResetMessagePoolStats` zeros all shard tag arrays.

### 66e. Per-shard freelist capacity

Pool budget is divided evenly: `maxCount / shardCount`. Per-shard capacity floors at 1 buffer. At shipped defaults this distributes correctly (e.g. lowmem's 1 MiB budget → 512 pool entries / 16 shards = 32 per shard). If shard count is raised significantly without raising the pool budget, this floor inflates memory — see comment in `newMessagePool`.

**Files Modified**: `message_pool.go`, `message_pool_test.go`

**Tests**: 5 new shard-specific tests + power-of-two validation test, all passing under `-race`:
- `TestMessagePoolShardRouting` — buffer routes to correct shard, Return increments correct freelist
- `TestMessagePoolShardWithTag` — tag byte (size+8) and shard byte (size+12) don't collide; per-shard tag accounting correct
- `TestMessagePoolShardRoundRobin` — 1600 Gets distributes evenly across 16 shards (100 each)
- `TestMessagePoolShardContention` — 32 goroutines × 1000 iterations concurrent Get/Return, `-race` clean
- `TestMessagePoolShardTagConcurrent` — 16 goroutines with distinct tags, taken=returned for all tags, `-race` clean
- `TestMessagePoolShardPowerOfTwo` — validates bitmask routing constraint
- All existing `TestMessagePool` / `TestMessagePoolShare` / transfer / send-receive tests continue passing

**Status**: ✅ Merged `main` (2026-07-04). PR #207. v3.23.0-fix.25.

---

## 67. Contract Ramp-Up Scale 3

**Purpose**: Increase `ContractTransferByteSeqScale` from 2 to 3, adding an intermediate ramp step between the 2 MiB initial and 128 MiB standard contract sizes. Reduces the proportion of unused contract allocations on short-lived connections (probes, quick disconnects, unreachable targets).

**Files Modified**: `transfer_contract_manager.go`, `provider/main.go`, `tuning.go`

**Changes**:
- Fork default `ContractTransferByteSeqScale`: 2 → 3 (`transfer_contract_manager.go`)
- Turbo V8 `ContractTransferByteSeqScale`: 2 → 3 (`provider/main.go`, `applyTurboSettings`)
- Auto Extreme (Tier 4) uses scale 3 (`tuning.go`, `applyTier4`)

**Ramp progression**:

| Step | Scale=2 (before) | Scale=3 (after) |
| :--- | :--- | :--- |
| 0 (initial) | 2 MiB | 2 MiB |
| 1 | ~65 MiB | ~44 MiB |
| 2 | 128 MiB | ~86 MiB |
| 3 | — | 128 MiB |

**Tradeoff**: One additional contract negotiation per session for more granular sizing that better matches actual usage. Connections that complete in 1-2 contracts now allocate ~44 MiB instead of ~65 MiB, reducing waste.

**Status**: ✅ Merged `main` (2026-07-05). PR #209. v3.23.0-fix.25.

---

## 68. P2P Transport Async Startup + DNS Fragment Buffer Leaks (PR #211)

**Purpose**: Fix two correctness bugs found in a July 5, 2026 deep-dive audit — both introduced when the fork first branched from stock v3.23 and never ported from upstream's later fixes.

### 68a. HIGH — P2P transport setup blocked forever, dropping one direction of every bidirectional relay stream (transport_p2p.go, transfer_stream_manager.go)

`NewP2pTransport()` started its connection-negotiation loop with `HandleError(p2pTransport.run)` — no `go`. Upstream runs this as `go HandleError(p2pTransport.run, cancel)`, fire-and-forget. `run()` is a `for {}` loop that only returns on `ctx.Done()`, so the synchronous call never returned until the stream tore down.

`StreamSequence.Run()` needs two P2P transports for a bidirectional relay stream — one "to destination", one "to source" — calling `NewP2pTransport` twice back to back. Because the first call blocked for the stream's entire lifetime, the second call was unreachable code. Every bidirectional P2P stream ended up with only one direction's WebRTC transport ever negotiated; the other direction silently fell back to the lowest-priority gateway transport (relayed through the platform's own servers instead of a direct peer hop). No error, no log — this degraded routing performance and understated relayed-bandwidth on affected streams for the entire life of the fork.

**Fix**: Restore `go HandleError(p2pTransport.run, cancel)`. Added INFO-level logging (`[p2p]` transport start/stop, `[sm]` both-transports-created) so a regression of this kind shows up as a missing log line instead of failing silently.

**Status**: ✅ Merged `main` (2026-07-05). PR #211. v3.23.0-fix.25.1.

---

### 68b. MEDIUM — Message pool buffer leak in DNS fragment reassembly (transport_pt_queue.go, transport_pt.go)

Three leak paths in `combineQueue`/`decodeDns`, all fixed upstream but absent here:

- `RemoveOlder`: when a fragment-reassembly item timed out before all fragments arrived, its already-received fragment buffers (`MessagePoolGet`-backed) were dropped without a `MessagePoolReturn`.
- `Combine`: a duplicate/retransmitted fragment index overwrote `item.packets[i]` without returning the buffer it replaced.
- `decodeDns`'s goroutine had no shutdown drain — buffers still queued in `dnsCombineQueue` or `readPipeline` at teardown were never returned.

Bounded by `DnsMaxCombine`/`DnsMaxCombinePerAddress` so not unbounded, but a steady, avoidable drain on the pool under sustained fragmented-DNS or retransmit traffic.

**Fix**: Return buffers in all three paths. Added `TestCombineRemoveOlderReturnsPooledBuffers` and `TestCombineDuplicateIndexReturnsPooledBuffer` regression tests.

**Status**: ✅ Merged `main` (2026-07-05). PR #211. v3.23.0-fix.25.1.

---

## 69. Systemd Restart Drop-In Self-Heal + Log File:Line Fix

**Purpose**: Two fixes from a live fleet incident response on 2026-07-05.

### 69a. Self-heal invalid `Restart=` systemd drop-in (scripts/Provider_Install_Linux.sh)

A provider node's `urnetwork.service` was found `inactive (dead)` after an update. Root cause: a `urnetwork.service.d/restart-override.conf` drop-in set `Restart=yes` — not a valid systemd value (the directive is an enum: `no`/`always`/`on-success`/`on-failure`/`on-abnormal`/`on-watchdog`/`on-abort`, not a boolean). systemd silently rejected that one line on every `daemon-reload` (logged as a parse warning, easy to miss) and fell back to the base unit's `Restart=no`, leaving the node with zero crash-restart protection since at least 2026-07-01.

A full history search of this script (all commits, all branches) found no reference to `restart-override.conf` or `Restart=yes` ever — urnet-tools has never generated this file. Origin is unknown; most likely a manual `systemctl --user edit` at some point, possibly by analogy to other tools' boolean restart flags. A fleet sweep afterward found the same rogue drop-in (with varying values, `yes` on 2 nodes, the correct `on-failure` on 1) on 3 of 31 reachable nodes, confirming it wasn't isolated to a single node.

**Fix**: `install_systemd_units` now runs `sanitize_restart_dropins`, which scans all `urnetwork.service.d/*.conf` files (not just the ones urnet-tools manages) on every install/update/reinstall and rewrites `Restart=yes|true|1` to `Restart=on-failure`.

**Status**: ✅ Merged `main` (2026-07-05, direct commit — hotfix). v3.23.0-fix.25.2.

---

### 69b. Restore correct file:line in log output (log.go)

The glog→Logger interface migration (#65, PR #69, 2026-06-15) added a wrapper frame between call sites and the actual `glog` calls. The `V(n)` verbose path was updated to account for the extra frame (`glog.VDepth`, `InfoDepth`), but the plain `Info`/`Infof`/`Warningf`/`Errorf` methods on `glogLogger` called glog's non-depth-aware functions directly — so every plain-level log line in the codebase (the majority of INFO output, including the message-pool stats line) has reported `log.go`'s own line instead of the real caller since that PR merged.

**Fix**: Switched to glog's depth-aware variants (`InfoDepth`/`InfoDepthf`/`WarningDepthf`/`ErrorDepthf`) with `depth=1` to skip the wrapper frame, matching the verbose path's existing convention. Verified via `TestCombine`: log output now shows `message_pool.go:231` instead of `log.go:100`.

**Status**: ✅ Merged `main` (2026-07-05). PR #213. v3.23.0-fix.25.2.

---

## 70. Code Review Findings — Reaper Lock, Heartbeat, Hub Regressions (PR #225)

**Purpose**: Fixes for critical bugs found in a comprehensive code review audit conducted by Opus. Covers provider reliability, data integrity, and hub infrastructure.

### 70a. Reaper Lock Fix (proxy_url_source.go)

The reaper was holding the proxy file lock across serial HTTP probes (up to 8s per dead entry, ~40 candidates on a typical public list). This caused concurrent reloads or fetches to race on `proxy_url.json`, losing blacklist entries and resurrecting dead proxies.

**Fix**: Candidates are now collected under the lock, probed serially outside it, then results applied atomically under re-acquired lock.

### 70b. Heartbeat Correctness (bandwidth_reporter.go)

- **Delta baseline**: Now advances only on POST success — failed sends no longer permanently drop status deltas during hub outages
- **Body cap**: Raised from 64 KiB to 256 KiB so first heartbeat after restart (with every proxy marked "changed") doesn't 400 on fleets above ~600 proxies
- **HTTP client**: Cached across ticks (same as heartbeat reporter), eliminating fresh TCP+TLS handshake per report cycle
- **Data race**: `loggedLegacyPinDeprecation` swapped from `bool` to `atomic.Bool`
- **Certificate validation**: `verifyHubChain` now takes DNSName from the URL host; IP literals skip DNSName

### 70c. Drain Re-Trigger (proxy_reload.go)

Proxies removed-then-re-added while still draining were staying dead indefinitely. `reload()` skipped draining entries, and drain-complete only called `cancelFn()` with no re-trigger.

**Fix**: Drain-complete now checks if the address is back in the desired set and fires a reload trigger if so.

### 70d. io.Reader Contract (message_pool.go)

`MessagePoolReadAllWithTag` was dropping trailing data on `(n>0, io.EOF)` and treating `(0, nil)` as EOF.

**Fix**: Switched to standard pattern: process bytes first, check EOF/error after.

### 70e. Hub Regressions (hub/main.go)

- **SSE/gzip**: `gzipMiddleware` was wrapping `/api/events` but `gzipResponseWriter` doesn't implement `http.Flusher` — every browser EventSource hit got a 500. Exempted like the rate limiter already does.
- **Signal shutdown**: `signal.NotifyContext` was capturing SIGTERM/SIGINT but ctx was never wired to the servers. First signal was silently swallowed, `docker stop` waited the full grace period then SIGKILLed. Both TLS and plain-HTTP servers now shut down cleanly.

### 70f. Other Fixes

- `fetchAndMergeProxyURLs` only wrote LastProbe stamps when new proxies were found — on steady-state refreshes, stamps were lost. Tracked a dirty flag instead.
- `RecordProxyAuthFailure` switched from substring-matching "timeout" to `errors.Is(DeadlineExceeded)` and `net.Error.Timeout()`
- Snapshot copies use `proxyBandwidthSnapshot` helper instead of `AddSession("snapshot", ...)` hack — latency fields no longer silently zeroed
- `test_bin` untracked from git and added to gitignore

**Status**: ✅ Merged `main` (2026-07-07). PR #225. v3.23.0-fix.25.4.

---

## 71. Tactical Emoji + Log Visibility Improvements (PR #226)

**Purpose**: Make provider logs scannable at a glance by adding tactical emoji to key log lines, plus additional visibility into contracts, traffic, and JWT health.

### 71a. Tactical Emoji (Phase 1)

14 log lines now carry emoji prefixes for visual scanning:

| Tag | Emoji | Rationale |
|-----|-------|-----------|
| `[outage]` | 🚨 | Critical state change |
| `[eco]` | 🌿🔴🟡🟢 | Memory state transitions (leaf + severity color) |
| `[proxy] reloaded` | 🔄 | Fleet change event |
| `[contract] denied` | ⛔ | Contrasts with 🤝 acquired |
| `[net][s]select` | 🌐 | Proxy control plane comms |
| `[jwt]` | 🔑 | Auth reliability signal |
| `[webhook]` | 📡 | Alert infrastucture failure |
| `[pool]` | 📦 | Startup sizing confirmation |
| `[traffic]` (per-proxy detail) | 📈 | Traffic detail lines |

### 71b. Contract Aggregates (Phase 2)

Atomic counters in `transfer_contract_manager.go` track contracts acquired, denied, and rolling average utilization. Surfaced on the `[profit]` heartbeat:

```
💰 [profit] earning=yes rate=2.1 MB/s contracts=8 denied=2 avg_util=72%
```

Fields only appear when there's data — zero clutter when idle.

### 71c. WebRTC Peer Lifecycle (Phase 3)

`🔗 [signal] peer connected` and `🔗 [signal] peer disconnected` events in `transport_p2p_webrtc.go`. One per P2P session (low frequency).

### 71d. DNS Visibility (Phase 4)

- DNS failure counter (`dns_failures=N`) on `[health]` heartbeat
- Rate-limited `[doh]` warnings capped at 1 per 5 minutes globally
- Escalation to 🚨 when failures exceed 100 in a window

### 71e. Traffic Velocity Alerts (Phase 5)

Velocity detection fires when total rate changes 3x+ between health heartbeat ticks (5 minutes):

```
📈 [traffic] velocity: 3.2x → rx=12.3 MB/s tx=8.7 MB/s (was rx=3.8 MB/s tx=2.1 MB/s)
📈 [traffic] velocity: 0.3x → rx=1.2 MB/s tx=0.8 MB/s — traffic dropping
```

Added peak tracking (`peak_rx`/`peak_tx`) to `[traffic]` total line. Client flight markers (`✈️` when transitions 0→>0, `🛬` when >0→0).

### 71f. JWT Visibility (Phase 6)

Startup and periodic JWT health logging:

```
🔑 [jwt] expires in 12 days
🔑 [jwt] EXPIRED 3 days ago — refresh needed
🔑 [jwt] ⚠ expires in 18h — refresh triggered (12 suppressed)
```

**Files changed**: `provider/main.go`, `provider/proxy_reload.go`, `transfer_contract_manager.go`, `transport_p2p_webrtc.go`, `net_http.go`, `net_http_doh.go`, `audit.go`, `log_throttle.go` (exported NewLogThrottle)

**Status**: ✅ Merged `main` (2026-07-07). PR #226. v3.23.0-fix.25.4.

---

## 72. JWT Refresh Rewrite — Fix Token Species Corruption (PR #227)

**Purpose**: Fix the never-verified JWT self-refresh feature that was silently corrupting on-disk tokens and creating orphan client/device rows on every 7-day cycle.

### 72a. Bug Confirmed (Code Audit)

Old `refreshJWT()` (`provider/main.go:1504-1535`) called `/network/auth-client` with no `ClientId` in the request body. Server (`server/model/network_client_model.go:140`) unconditionally minted a new client_id + device row on every call (no session-based fallback exists). Each 7-day refresh cycle:

1. Created one orphan client+device row per node
2. Overwrote the on-disk network JWT with a client JWT (different token species)
3. Zero live impact on running proxies (proxies independently mint their own client_ids on reconnect)

### 72b. Fix

Rewrite `refreshJWT()` to use `/auth/code-create → /auth/code-login` — the same flow the provider `auth` command uses at initial login. Returns a same-species **network JWT** with zero side effects.

### 72c. Protections

- **`jwtContainsClientId()` regression guard**: Refuses to return a JWT that contains a `client_id` claim. Catches future regressions where the server might return a client JWT instead of a network JWT.
- **Verification step**: Before returning, hits `GET /transfer/stats` with the new token to verify it's accepted. Caller never overwrites the on-disk JWT with a dead or rejected token.
- **Verbose logging**: Each step (code-create → code-login → verification) is logged with step N/3 markers for operator visibility.

### 72d. Files Changed

- `api.go` — Added `AuthCodeCreate` types and methods (previously only `AuthCodeLogin` existed in the client)
- `provider/main.go` — Rewrote `refreshJWT()`, added `jwtContainsClientId()`
- `provider/jwt_test.go` — Added `TestJWTContainsClientId` (4 cases), added `createFakeJWTWithClaims` helper

## 73. Persisted Custom Network Selection (`choose_network`, PR #288)

**Purpose**: Let operators running their own API/connect backend (test networks, private infrastructure) point the provider at it without a custom-built binary or repeating `--api_url`/`--connect_url` on every invocation. Ported from `urfoundation/sn` PR #1 (`miner choose_network`), adapted to this fork's `provider` CLI.

### 73a. CLI

- `provider choose_network <api_url> <connect_url>` — validates (`<api_url>` must be `http`/`https`, `<connect_url>` must be `ws`/`wss`) and saves to `~/.urnetwork/network.json`.
- `provider choose_network --reset` — clears the saved network, reverting to the hardcoded main-network defaults.
- Resolution order for `auth`, `provide`, `wallet set`, `claim`: `--api_url`/`--connect_url` flag > saved `network.json` > hardcoded default. Unchanged from upstream if no network is ever chosen.

### 73b. Docker

`UR_API_URL` / `UR_CONNECT_URL` env vars, wired into all three entrypoints (`start_stable.sh`, `start_jwt.sh`, `start_nightly.sh`). Both must be set together — either alone fails the container fast rather than silently running against the wrong backend. Calls `choose_network` once at boot; `nightly` runs it after the update-check step since that build's binary doesn't exist on disk until downloaded. Also wired into `docker/scripts/urnet-tools.sh` (`urnet-tools choose_network ...` via `docker exec`) and `scripts/urnet-tools.ps1` for Windows Docker installs.

### 73c. Files Changed

- `provider/network.go`, `provider/network_cmd.go` (new) — config I/O, URL validation, precedence resolution. Reuses the existing `providerStatePath` helper rather than a new path helper.
- `provider/main.go`, `provider/sn.go` — `auth`/`provide`/`wallet set`/`claim` migrated to the new resolvers.
- `provider/network_test.go` (new) — validation, round-trip, precedence, reset, corrupt-config, file-permission, and partial-write-prevention tests.
- `docker/scripts/start_stable.sh`, `start_jwt.sh`, `start_nightly.sh`, `urnet-tools.sh` — `UR_API_URL`/`UR_CONNECT_URL` wiring.
- `scripts/urnet-tools.ps1` — Windows Docker wiring.
- `docs/Configuration.md`, `README.md`, `AI.md` — documented the new command and env vars.

**Status**: ✅ Merged `main` (2026-07-17). PR #288. v3.23.0-fix.26.3.

---

## 74. Disk-Based Critical Event Log + RAM-Log Restart Persistence (PR #242)

**Purpose**: `/dev/shm` ramlogs are wiped on every restart, so a crash that took the process down with it left zero forensic trail. Needed a log that survives the event that necessitated looking at it.

**Change**:
- New `~/.urnetwork/events.log` — 1MB capped, auto-rotating, lives on disk (not RAM). Captures `STARTUP`, `SIGNAL`, `PROVIDER EXIT`, `PANIC`, and `FATAL` events specifically — not general chatter.
- `connect.CritLogger`/`connect.LogCritical()` (new, in the root `connect` package) lets any networking goroutine write a recovered panic to this disk-based log, not just `provider/`.
- `shmLogPath`/`shmImportantLogPath` (the RAM logs) switched from `O_TRUNC` to `O_APPEND` — a restart no longer wipes the previous run's tail. A `--- provider restarted at ... ---` separator marks the boundary, which is also what `shmlog_linux.go`'s current ghost-fix-adjacent notice printing (see entry #92) builds on.

**Files Changed**: `critlogger.go`, `provider/critlog.go`, `provider/main.go`, `provider/shmlog_linux.go`, `trace.go`

**Status**: ✅ Merged `main` (2026-07-08). PR #242. v3.23.0-fix.26.

---

## 75. Help Text Reorganization + No-Arg Status Toggles (PR #245, #247)

**Purpose**: `Provider_Install_Linux.sh`'s `--help` output hadn't been touched since the script was much smaller; hub commands weren't documented at all, and running a toggle command (`eco`, `ramlogs`, `lowmode`, `hot-restart`) with no argument errored instead of reporting current status.

**Change**: `show_help()` rewritten with full descriptions and a `urnet-tools set help` sub-menu; hub commands documented for the first time. `eco`/`ramlogs`/`lowmode`/`hot-restart` invoked with no args now print current status; space-separated aliases (`hot restart`) work the same as the hyphenated form.

**Files Changed**: `scripts/Provider_Install_Linux.sh`, `scripts/test_help_text.sh`

**Status**: ✅ Merged `main` (2026-07-09). PR #245, #247. v3.23.0-fix.26.

---

## 76. IPv6 STUN Auto-Detect (PR #246)

**Purpose**: On hosts with no real IPv6 connectivity (common on cheap VPS/NAT setups), ICE candidate gathering wasted time and log noise trying IPv6 STUN reachability on every connection attempt.

**Change**: Provider probes IPv6 STUN reachability once at startup via a `sync.Once`-guarded 100ms UDP dial. If unreachable, ICE candidates are restricted to `NetworkTypeUDP4`/`NetworkTypeTCP4` for the process lifetime, eliminating repeated "network is unreachable" STUN noise.

**Files Changed**: `transport_p2p_webrtc.go`, `transport_p2p_webrtc_pc.go`

**Status**: ✅ Merged `main` (2026-07-10). PR #246. v3.23.0-fix.26.

---

## 77. Transport Self-Wake CPU Loop — Structural Fix (PR #248)

**Purpose**: Follow-up to the 100% CPU bug from PR #191. That fix addressed the symptom; this addresses the actual structural hazard so no future code change inside the mode-selection loop can reintroduce it.

**Root Cause**: `run()` in `transport.go` captured the mode-change notify channel *before* calling `setActiveMode()`. If `setActiveMode()`'s `NotifyAll()` fired (which it always does on a mode change, and could on redundant calls), it closed the very channel `select` was about to block on — an already-closed channel wakes `select` instantly, so the loop spun at 100% CPU with zero actual work to do.

**Fix**:
```go
// Before: notify captured early, self-triggered close causes spin
available, notify := self.modesAvailable()
self.setActiveMode(mode)
select { case <-notify: }

// After: channel captured AFTER mode-selection work, redundant calls skipped
available, _ := self.modesAvailable()
if bestMode != lastMode {
    self.setActiveMode(bestMode)
    lastMode = bestMode
}
_, notify := self.activeMode()
select { case <-notify: }
```
Zero added latency (still wakes on the next real notification) and structurally eliminates the self-wake regardless of what future code runs inside the loop. Diagnostic log reworded from "spurious call... likely self-wake loop" to "redundant setActiveMode(h1) — if frequent, check for 100% CPU (self-wake loop)" for operator clarity.

**Files Changed**: `transport.go`

**Status**: ✅ Merged `main` (2026-07-09). PR #248. v3.23.0-fix.26.

---

## 78. Session Save/Load — Cross-Platform Identity Transfer (PR #250, #251, #252, #253, #254)

**Purpose**: Let an operator move a provider's full identity + proxy configuration (JWT, keys, cert, proxy list) to a different machine without re-authenticating (auth codes are single-use).

**Change**:
- **Core** (`provider/main.go`, PR #250): `urnet-tools session save <file>` / `session load <file>` — bundles all 7 identity/proxy-list files from `~/.urnetwork/`, encrypted via `openssl aes-256-cbc -pbkdf2` with a prompted passphrase. `provider print-network-id <file>` (new hidden CLI subcommand) extracts the bundle's `network_id` JWT claim so `session load` can refuse to import a bundle from a different account. 6 file paths converted from `os.WriteFile` to atomic `os.CreateTemp`+`os.Rename` so `session save` is safe to run against a live, traffic-serving provider. `applyStagedSession()` runs early in `provide()`, checking for a `.session-pending` marker and atomically swapping staged files into the live directory — this is what lets `session load` work without stopping the provider first.
- **macOS** (`scripts/Provider_Install_Mac.sh`, PR #252): New native installer — `install`, `start/stop/restart/status` via `launchctl`, `hot-restart on|off`, `session save|load`, `proxy`, `hub`, `auth`, `logs`. Installs to `~/.local/share/urnetwork-provider/`, launchd plist with `KeepAlive`, strips the quarantine xattr. `release.yml` expanded to build/publish `darwin/amd64` and `darwin/arm64`.
- **Windows** (`scripts/urnet-tools.ps1`, PR #251): `hot-restart on|off` toggle with process-level `$env:` propagation so an immediate restart inherits the setting without a fresh shell.
- **Docker** (`docker/scripts/urnet-tools.sh`, PR #253): `session save|load` added, with an interactive-TTY guard that prompts if `-it` was omitted from `docker exec`. Restart is via `pkill`, relying on the start script's crash-loop restart.
- **Security hardening** (PR #254, same-week follow-up): `openssl enc -pass "pass:$var"` (visible in `ps` output) replaced with `-pass "file:$_pf"` using a temp file cleaned up immediately after. macOS installer's `cp "$0"` (fails silently under `curl | sh`, since `$0` isn't a real file path in that invocation) replaced with a GitHub raw URL download. `atomicWriteFile`'s fixed `path + ".tmp"` changed to `os.CreateTemp(dir, name+".*.tmp")` to avoid collisions under concurrent `.provider.key`/`.cert` writers. `session load --force` parsing fixed to scan all args after the file path instead of only the immediate next one. Bundle load now hard-fails if `print-network-id` returns empty (corrupt bundle JWT) instead of silently bypassing the network-ID safety gate.

**Files Changed**: `provider/main.go`, `provider/main_test.go`, `scripts/Provider_Install_Linux.sh`, `scripts/Provider_Install_Mac.sh`, `scripts/urnet-tools.ps1`, `docker/scripts/urnet-tools.sh`

**Status**: ✅ Merged `main` (2026-07-09 – 2026-07-10). PR #250, #251, #252, #253, #254. v3.23.0-fix.26.

---

## 79. Docker In-Place Updates (PR #255)

**Purpose**: Updating a Docker-deployed provider previously meant pulling a new image and recreating the container (via Watchtower or manually). `urnet-tools update` inside the running container allows an in-place binary replacement without touching the image.

**Files Changed**: `docker/scripts/urnet-tools.sh`

**Status**: ✅ Merged `main` (2026-07-10). PR #255. v3.23.0-fix.26.1.

---

## 80. Hot-Reload Lock Race + Shutdown Diagnostics (PR #260)

**Purpose**: Investigation of a live incident on a fleet node where `provider proxy remove --match=decodo --yes` caused the running provider to die with exit code 0, and separately, where config changes from the CLI were sometimes silently never picked up.

**Bug #1 — lock-file race silently dropped hot-reloads (confirmed root cause)**: `removeDeadProxies()` and `evictProxyURLAddress()` in `proxy_url_source.go` acquire `~/.urnetwork/proxy.lock`, modify config, and write the reload trigger — all before their deferred `release()` runs (Go's `defer` fires after `return` evaluates, so the trigger was written *while the lock was still held*). The running provider's own `reload()` tries to acquire that same lock; if the CLI still holds it, `acquireProxyLock()` errors and `reload()` bails out early. Since the trigger's sequence number was already bumped, the watcher's next poll sees no change and never retries — the config change is lost silently, with no error surfaced anywhere. **Fix**: call `release()` explicitly before `writeReloadTrigger()` in both functions.

**Bug #2 — provider exit during hot-reload, root cause undetermined, diagnostics added**: Log analysis showed *all* ~2000 proxies received `context canceled` during the incident, not just the ~200 targeted by `--match` — including the native/direct proxy, whose context (`nativeCtx`) is explicitly wired to be immune to hot-reload deletions. That's only possible if the **main context** itself was cancelled, not individual proxy contexts, but the expected `[provider] shutting down: main context cancelled` / `[signal] received` log lines were never seen (likely never flushed before exit). Added `debug.Stack()` capture on `ctx.Done()` in `main.go` and PID in the `[signal]` log in `util.go` so a repeat occurrence is diagnosable. Root cause of *why* the main context was cancelled remains open.

**Also added**: `🔄 [proxy] reload trigger: seq N → M` log line at trigger-detection time in the watcher goroutine — previously there was no log signal for when a reload was triggered vs. when it completed.

**Files Changed**: `provider/proxy_url_source.go`, `provider/proxy_reload.go`, `provider/main.go`, `util.go`

**Status**: ✅ Merged `main` (2026-07-13). PR #260. v3.23.0-fix.26.1.

---

## 81. Self-Healing Proxy Resource Management (PR #259)

**Purpose**: A production meltdown where thousands of dead-but-still-desired proxies caused a huge goroutine pile-up and days of sustained 100% CPU. Two layers: always-on correctness fixes (apply regardless of any toggle) and an opt-in closed-loop pressure-response system.

**Always-on fixes** (independent of any toggle):
- `proxy_url_max` now defaults to 500 (was unlimited).
- Cleanup `cleanup_scope` defaults to `"url"` (was `"none"`), base interval 6h (was 24h), with an uptime guard so proxies still warming up (report "dead" before their first successful auth) are never mass-evicted at startup.
- Dead URL-sourced proxies now give up and get evicted after 4 backoff cycles (~4h) instead of 10 (~6 days).
- The reaper now re-probes stale `ProbeOK=true` entries after 3h, demoting once-good-now-dead proxies into the existing 3-strikes blacklist path — previously a proxy that went good→dead after its last successful probe could sit in the cache indefinitely, invisible to cleanup.

**Opt-in pressure system** (`URNETWORK_SELF_HEAL=1` / `urnet-tools self-heal on`, **default off**): A monitor goroutine samples every 30s and publishes a smoothed [0,1] pressure score from `/proc/pressure/{memory,cpu}` (self-normalizing across core counts), `MemAvailable/MemTotal`, loadavg fallback, and self-signals (goroutine count, heap vs. `max-memory`). Actuators respond proportionally instead of via binary gates: URL fetch interval stretches 1×–8× with pressure (floor: a cache under 50 entries is never stretched), probe concurrency scales 50→1 workers, and cleanup/reaper cadence *shrinks* under pressure (they shed load, so overload is exactly when they should run harder). An AIMD-controlled `TargetPoolSize` (persisted to `proxy_url.json`) grows +25 while calm and cuts ×0.7 after sustained high pressure (floor 50, ceiling `proxy_url_max`); shrinks shed worst-first (dead → degraded tiers → healthy by ascending traffic) through the normal cache-removal path with a 1h re-admission backoff — shed proxies are never blacklisted, so they re-enter through a normal fetch+probe once the box recovers. The old static `proxy_load_threshold` gate and its skip-counter are removed entirely, replaced by this system.

**Files Changed**: `docker/scripts/urnet-tools.sh`, `docs/Configuration.md`, `provider/bandwidth_reporter.go`, `provider/main.go`, `provider/proxy_admission_gate.go`, `provider/proxy_failure_history.go`, `provider/proxy_probe.go`, `provider/proxy_reload.go`, `provider/proxy_url.go`, `provider/proxy_url_source.go`, `provider/resource_pressure.go` (new), `scripts/Provider_Install_Linux.sh`, `scripts/Provider_Install_Mac.sh`, `scripts/urnet-tools.ps1`

**Status**: ✅ Merged `main` (2026-07-14). PR #259. v3.23.0-fix.26.2.

---

## 82. Upstream Infrastructure Ports — Egress, Memory Budget, Contract Stats, Peer API, 532ee20c (PR #261, #262, #265, #266)

**Purpose**: Pull forward a batch of upstream `urnetwork/connect` infrastructure changes: new egress-interface abstraction, per-connection memory budgets, contract statistics tracking, a `ReceiveFunction` type change + pause-behavior fix in the Peer API, and a further upstream commit (`532ee20c`) fixing a transport mode-election bug, a hot-spin CPU issue, and a connection-eviction bug.

**Files Changed**: `egress.go`, `egress_net.go`, `egress_other.go`, `egress_windows.go`, `ip_assoc.go`, `ip_remote_multi_client.go`, `memory_budget.go`, `transfer.go`, `transfer_contract_manager.go`, `transfer_contract_stats.go`, `transfer_memory_budget.go`, `transfer_peer_manager.go`, `transport_p2p_webrtc.go`, `ip_block_action.go`, `ip_security_cfaa.go`, `transfer_control.go`, `transfer_encrypt.go`, `transport.go`, `provider/main.go`, `net_resilient.go`, `transfer_route_manager.go`, plus generated `protocol/*.pb.go` and matching test files.

**Status**: ✅ Merged `main` (2026-07-12 – 2026-07-13). PR #261, #262, #265, #266. v3.23.0-fix.26.1.

---

## 83. Go 1.26 Toolchain Bump + Repository Templates (PR #267, #268, #269)

**Purpose**: Housekeeping. Compiler bumped 1.25.x → 1.26.4 (`Dockerfile`, `go.mod`/`go.sum`, both CI workflows, `hub/Dockerfile`); core deps bumped (`pion/webrtc`, `quic-go`, `golang.org/x/*`). Standardized `.github/PULL_REQUEST_TEMPLATE.md` and `.github/RELEASE_TEMPLATE.md` added for consistent PR/release documentation going forward — the templates this very document's release notes now follow.

**Status**: ✅ Merged `main` (2026-07-13). PR #267, #268, #269. v3.23.0-fix.26.1.

---

## 84. Subnet (`sn`) Integration — Phase 1–3, Dormant (PR #272)

**Purpose**: Prep work for eventual Bittensor subnet integration. Pins `urfoundation/sn` crypto/chain packages and backports upstream `Sn*Sync` API methods. Inert — no CLI wiring lands in this PR, so behavior is unchanged until a future PR actually calls into it.

**Files Changed**: `api_verify.go`, `go.mod`, `go.sum`, `sn_deps.go` (new)

**Status**: ✅ Merged `main` (2026-07-13). PR #272. v3.23.0-fix.26.2.

---

## 85. proxy_url Cache Merge ProbeOK Regression + go-ethereum Bump (PR #274, #275)

**Purpose**: `mergeProxyURLEntries` was unconditionally setting `ProbeOK=false` on merge, even for addresses that had just passed the dual-stage API-reachability probe — discarding that result before it ever reached the cache. Net effect: the background reaper could blacklist proxies that had just been proven live, since it only sees `ProbeOK` from the cache, not the fresher in-memory probe result. Fixed to preserve the probe result through the merge. Separately, `github.com/ethereum/go-ethereum` bumped v1.16.7 → v1.17.4, clearing 5 Dependabot alerts.

**Files Changed**: `provider/proxy_url.go`, `provider/proxy_url_source.go`, `provider/proxy_url_test.go`, `go.mod`, `go.sum`

**Status**: ✅ Merged `main` (2026-07-14). PR #274, #275. v3.23.0-fix.26.2.

---

## 86. Hub Off/Set Live Reload (PR #276)

**Purpose**: `hub off`/`hub set` previously required a provider restart to take effect. All four hub commands (`link`/`unlink`/`set`/`off`) now write the same override file the provider already polls every report tick, so the change is picked up live.

**Files Changed**: `provider/bandwidth_reporter.go`, `provider/bandwidth_reporter_test.go`, `scripts/Provider_Install_Linux.sh`

**Status**: ✅ Merged `main` (2026-07-14). PR #276. v3.23.0-fix.26.2.

---

## 87. Hot-Restart Status Display Fix + Docker-Backed Hub Install (Mac/Windows) (PR #277, #278)

**Purpose**:
- `urnet-tools restart` always printed "cold restart required" regardless of actual hot-restart status — deduped the `hotRestartEnabled()` check into a shared cross-platform helper so the message reflects reality. The GitHub-rate-limit-resilient worker-download fallback (previously Linux-only) was also ported to macOS (`Provider_Install_Mac.sh`) and Windows (`urnet-tools.ps1`). Also fixed: a persisted `--tag` config silently overriding an explicitly-passed `--tag` flag on `hub update` (should be the reverse), and `hub update` on Docker resolving the *provider* image instead of the `-hub` image.
- `urnet-tools hub install`/`hub update` on macOS and Windows now deploy the hub via Docker (`docker pull`/`run` against `ghcr.io/full-bars/urnetwork-3.23-fix-hub`) since neither platform has a native hub binary. Linux gets this as an opt-in `--docker` flag (native systemd remains the Linux default). All platforms share the same `urnetwork-hub` container name and `urnetwork-hubdata` volume.

**Files Changed**: `hub/onboard.go`, `scripts/Provider_Install_Linux.sh`, `scripts/Provider_Install_Mac.sh`, `scripts/Provider_Install_Win32.ps1`, `scripts/urnet-tools.ps1`, `docker/scripts/urnet-tools.sh`, `docs/Hub-Setup.md`

**Status**: ✅ Merged `main` (2026-07-14 – 2026-07-15). PR #277, #278. v3.23.0-fix.26.3.

---

## 88. Hub CA Cert Auto-Bootstrap + Live Reload + Dashboard Basic Auth (PR #279, #281, #282)

**Purpose**:
- `hub init` now checks `URNETWORK_HUB_TOKEN`/`URNETWORK_HUB_TOKEN_FILE`/`URNETWORK_HUB_TOKEN_STDIN` on startup and, if present, fetches the CA cert from `$HUB/ca-cert?token=...` automatically before doing anything else — removes a manual bootstrap step for new hub deployments.
- The hub now watches `hub_ca.pem` via file poll and reloads the CA certificate on change without a restart, enabling live CA rotation.
- New `URNETWORK_HUB_DASHBOARD_PASS` env var gates the dashboard (`/`) and read-only API endpoints behind HTTP Basic Auth. Independent of `URNETWORK_HUB_TOKEN` (which still protects the write endpoints) — see [docs/Hub-Setup.md](docs/Hub-Setup.md#locking-down-the-dashboard).

**Files Changed**: `hub/main.go`, `hub/onboard.go`, `hub/onboard_test.go`, `hub/main_test.go`, `provider/bandwidth_reporter.go`, `provider/bandwidth_reporter_ca_test.go`, `docs/Hub-Setup.md`

**Status**: ✅ Merged `main` (2026-07-15). PR #279, #281, #282. v3.23.0-fix.26.3.

---

## 89. Auto Tier 4 (Extreme) Profile (PR #280)

**Purpose**: On hosts with ≥8 GiB RAM, the provider now auto-selects a Tier 4 "extreme" performance profile matching `turbo-v8` settings, instead of requiring an operator to discover and manually opt into `tier set 4`. Manual override remains available.

**Files Changed**: `provider/main.go`, `tuning.go`

**Status**: ✅ Merged `main` (2026-07-15). PR #280. v3.23.0-fix.26.3.

---

## 90. Outage Memory Safety — GOMEMLIMIT + Degraded-Proxy Reaper (PR #293)

**Purpose**: During a sustained backend/auth outage, every proxy degrades and holds onto its ~14 goroutines and turbo-v8 buffer allocations indefinitely (the slow auth retry loop never exits) — observed at 4,001 degraded proxies × ~375KB ≈ 1.5 GiB baseline heap, with no ceiling to stop further growth.

**Fix 1 — GOMEMLIMIT for turbo profiles**: `turbo-v4`/`turbo-v8` previously set `GOGC=200` with no `GOMEMLIMIT` at all — the GC wouldn't act until the heap hit 2× the live set, and `resource_pressure.go`'s heap-pressure sensor was blind without a limit to measure against. Now turbo profiles set `GOMEMLIMIT` to 80% of available RAM whenever the operator hasn't explicitly set `--max-memory`/`GOMEMLIMIT`, matching the safety net `eco` mode already had.

**Fix 2 — degraded-proxy timeout reaper**: A background reaper runs every 3 minutes, ranks degraded proxies by lifetime contribution (`TotalRx+TotalTx` from `ProxyBandwidth` plus contracts won from `globalContractMetrics`, ascending), and cancels the `proxyCtx` of the bottom 50% if they've been degraded for more than 30 minutes — ceil-rounded so 3 proxies keeps 2, not 1. Cancelling `proxyCtx` triggers the full cleanup path (`connectClient.Close()`, buffer release, goroutine exit). Runs unconditionally (not gated on the self-heal toggle) as a structural safety floor; the 30-minute grace period lets transient blips self-heal before being killed, and ranking by contribution rather than raw downtime means the best-performing degraded proxies are always the ones kept retrying.

**Files Changed**: `provider/main.go`, `proxy_health.go`, `provider/degraded_reaper_test.go`, `proxy_health_test.go`

**Status**: ✅ Merged `main` (2026-07-18). PR #293. v3.23.0-fix.26.4.

---

## 91. Download Reliability, TOCTOU Fix, and Misc v26.4 Correctness Fixes (PR #291, #292, #294, #295, #296)

**Purpose**: A batch of smaller fixes shipped alongside the outage-safety work in entry #90.

**Change**:
- **Download reliability** (#291): Provider updates route through `dl.fullbars.xyz` first with automatic GitHub fallback; `-y`/`-f` flags added to skip the interactive restart confirmation.
- **TOCTOU tmpfile vulnerability** (#292): Update downloads switched from a hardcoded `/tmp/urnetwork-update.tar.gz` to an `mktemp`-generated unpredictable path, closing a symlink-race (CWE-377) that could let a local user redirect the extraction to overwrite an arbitrary file. Binary installation now stages and atomically replaces only on full success.
- **Dashboard contract feed typo** (#294): "Recent Contracts" matched `"[contract] acquired"` twice instead of also matching `"[contract] denied"` — every denial was invisible on the dashboard.
- **JWT NetworkId claim key** (#295): `ParseByJwtUnverified` read `claims["network_name"]` for both `NetworkName` and `NetworkId` (copy-paste bug) — `NetworkId` now correctly reads `claims["network_id"]`.
- **authBytes pool leak** (#295): `runH3` in `transport.go` was missing the `defer MessagePoolReturn(authBytes)` that `runH1` already had, leaking one pool buffer per H3 auth handshake.
- **Division-by-zero guard, logThrottle consolidation, .gitignore hardening** (#296): `contractByteCount()` guards against `ContractTransferByteSeqScale=0`; 6 hand-rolled rate-limiters in `transfer.go` consolidated into 3 `logThrottle` instances; `hub/hub_bin`, `hub.db`, `prs.json` added to `.gitignore`.

**Files Changed**: `docker/scripts/urnet-tools.sh`, `scripts/Provider_Install_Linux.sh`, `scripts/Provider_Install_Mac.sh`, `scripts/Provider_Install_Win32.ps1`, `provider/main.go`, `jwt.go`, `jwt_test.go`, `transport.go`, `.gitignore`, `transfer.go`, `transfer_contract_manager.go`

**Status**: ✅ Merged `main` (2026-07-18). PR #291, #292, #294, #295, #296. v3.23.0-fix.26.4.

---

## 92. proxy.state Ghost-Entry Pruning (PR #305)

**Purpose**: Fix a production incident where `proxy.state` accumulated entries for proxies that had been removed from the config/source but never got pruned from state, growing without bound and causing `proxy remove-dead` to re-report the same removals forever.

### 92a. Root Cause

`ProxyReloader.reload()` in `provider/proxy_reload.go` computed `removed` as `running ∖ desiredSet` — the set of addresses that were both currently running (present in the live `cancelMap`) and no longer desired. Only those addresses had their `state.Proxies` entry deleted. A dead/offline proxy's goroutine has usually already exited by the time an operator runs `proxy remove-dead` against it, so it was never in `running` to begin with — its ghost entry in `proxy.state` was never reachable by the existing prune logic, regardless of how many times `remove-dead` "removed" it.

Confirmed on a production node (v3.23.0-fix.26.4): `remove-dead` correctly shrank the config from ~1020 to 298 servers and live auth-failure churn stopped, but `proxy.state` retained 857 entries, 711 of which existed in neither the config nor the running set.

### 92b. Fix

After `desiredSet` is fully computed (config/file source merged with the URL cache), `reload()` now prunes `state.Proxies` to exactly that set, in addition to the existing running-diff removal loop (which still handles draining active sessions gracefully). Safe for URL-sourced proxies mid give-up-backoff, since `mergeProxyURLCache` keeps them in the URL cache — and therefore `desiredSet` — for their whole backoff window; only explicit eviction/blacklisting removes them.

A follow-up review finding was addressed before merge: if `proxy_url.json` fails to read for a given reload cycle (corrupt file, transient I/O — not the normal "no URL sources configured" case, which returns an empty cache with no error), `desiredSet` would silently exclude every URL-sourced address for that cycle. The prune pass now tracks whether the URL cache loaded successfully and skips pruning entirely if it didn't, so a transient read failure can't wipe state for still-desired URL proxies.

### 92c. Files Changed

- `provider/proxy_reload.go` — desired-set-wide prune pass, `urlCacheLoaded` guard, `pruned` count in the reload summary log line.
- `provider/proxy_reload_test.go` — `TestReload_PrunesGhostStateEntries_NotRunningNotDesired`, `TestReload_PreservesBackoffURLProxyState`, `TestReload_SkipsPruneOnURLCacheReadFailure`.

**Status**: ✅ Merged `main` (2026-08-02). PR #305. v3.23.0-fix.26.5. Validated on a live test deployment (500 URL-sourced proxies): an explicit `remove-dead` run pruned exactly 14/14 with zero left behind, and a spontaneous give-up/eviction cycle was separately observed pruning 7 stale entries on its own before `remove-dead` was ever invoked.

---

## 93. Ramlog Redirect Scoped to `provide`/`auth-provide` (PR #306)

**Purpose**: Found while validating entry #92 on a live Detroit test container. `URNETWORK_RAMLOGS=1` does a process-wide file-descriptor `dup2` of stdout/stderr into `/dev/shm/urnetwork.log` — but it applied to *every* invocation of the binary, not just the long-running `provide` process. Running a one-shot CLI subcommand via `docker exec` (`proxy remove-dead --preview`, `proxy summary`, `--version`, etc.) against a container with ramlogs enabled produced zero visible output: the results silently went into the ramlog file instead of the caller's terminal.

**Fix**: New `isLongRunningSubcommand()` in `provider/main.go` checks `os.Args[1]` for `provide`/`auth-provide`, excluding `-h`/`--help`/`--version` (this check runs from `init()`, before `docopt.ParseArgs` gets a chance to handle those flags and exit — without the exclusion, `provide --help` would have had its usage text redirected too). Gates all three redirect call sites: `initGlog()`'s direct path, the auto-profile slow-disk-benchmark path (which also skipped running `RunStartupAudit()` for one-shot commands as an unrelated latency win), and the auto-detected handover path. `shmlog_linux.go`'s `initSHMLogger()` also now prints a short "[ramlogs] output redirected to ..." notice to the pre-redirect stdout, before the `dup2` — visible on `docker logs`/journald for the `provide` process itself, or the caller's terminal for a one-shot command in the rare case ramlogs got enabled some other way.

**Files Changed**: `provider/main.go`, `provider/main_test.go` (`TestIsLongRunningSubcommand`), `provider/shmlog_linux.go`

**Status**: ✅ Merged `main` (2026-08-02). PR #306. v3.23.0-fix.26.5.

---

## 94. Hourly Reload Reconciler — Self-Heal Safety Net (PR #309)

**Purpose**: `ProxyReloader.reload()` only ever runs on an explicit trigger (add-source, remove-dead, proxy refresh, URL fetch merge, reaper change). A mass-failure event (e.g. a transient backend outage) can leave a batch of still-desired proxies stuck out of the running set with no future event scheduled to bring them back.

**Root Cause**: Confirmed live on ATL2 via `~/.urnetwork/proxy_health.log` (a persistent transition log, unaffected by ramlog rotation): a mass degrade event at 06:37 UTC caused ~2,500 proxies to flip to `DEGRADED` simultaneously (direct count from the health log at that timestamp). The vast majority recovered within ~40 minutes on their own. A persistent subset then logged **zero further activity for the next ~22 hours** — no new degrade, no recovery — until an unrelated operator action (adding 3 URL sources) forced a reload the following morning, at which point `reload()`'s own log line reported `+3341 added` in a single cycle. (The ~3300 figure in the original incident report was an independent estimate from a live `proxy summary` snapshot taken during triage, not derived from `proxy_health.log` — the ~2,500 degrade count, the reload's `+3341`, and that ~3300 estimate are three different snapshots/methodologies and shouldn't be read as the same measurement, though they're consistent with one large persistent cluster.) The exact mechanism by which those specific goroutines left the running set wasn't conclusively identified (operator-curated/internal proxies are coded to never give up on auth failure and retry in-place instead — see `main.go:2566-2569` — so this isn't the same path as the URL-sourced give-up/eviction flow), but the *outcome* — `running` silently drifting below `desired` with nothing to reconcile it — is independent of the exact cause and needed a fix regardless.

**Fix**: New `runReloadReconciler` (`provider/proxy_reload.go`) — a background goroutine that calls `writeReloadTrigger` once an hour, unconditionally. It goes through the same trigger-file path every other caller uses (`StartWatcher` polls and calls `reload()` on a sequence-number change) rather than calling `reload()` directly, relying on `writeReloadTrigger`'s existing serialization/debounce/coalescing — it doesn't add a new locking mechanism, and a concurrent caller can still produce a trailing duplicate trigger the same way any two existing callers could, which is harmless (an extra `reload()` cycle is a no-op when nothing changed). Deliberately cheap when nothing is wrong: if `running` already matches `desired`, `reload()` just logs `+0 added, -0 removed` and returns. Not gated behind `URNETWORK_SELF_HEAL` (unlike the pressure-proportional fetch/cleanup-interval scaling from entry #81) — same tier as `runDegradedProxyReaper`, which is also unconditional by design: safety nets in this codebase are built to not throttle themselves during exactly the conditions they exist to catch.

**Files Changed**: `provider/proxy_reload.go`, `provider/proxy_reload_test.go` (`TestRunReloadReconciler_FiresOnInterval`), `provider/main.go` (wired into `provide()`'s startup goroutine list)

**Status**: ✅ Merged `main` (2026-08-02). PR #309. v3.23.0-fix.26.5.

## 95. IPv4 Fragment-Drop Guard in `parseIpv4` (PR #311)

**Purpose**: Reject fragmented IPv4 packets before transport parsing. A non-first fragment has no transport header and a first fragment has a truncated payload — both were previously misparsed as if a transport header were present, reading garbage bytes as ports/flags. The fork had zero fragment handling before this.

**Fix**: One 16-bit load in `parseIpv4` (`ip.go`): if the MF bit or fragment-offset field (`0x3fff`) is non-zero, return without parsing. DF and the reserved bit pass. Ported from upstream `e05ecee0` (guard only — the ICMP echo path from the same commit was not ported; a SOCKS-relay provider has no use for ICMP).

**Tests**: 4 new (`ip_fragment_drop_test.go`): MF-set, non-zero offset, DF-only (must still parse), unfragmented.

**Status**: ✅ Merged `main` (2026-08-03). PR #311. v3.23.0-fix.26.6.

---

## 96. SCTP/WebRTC Tuning: ReceiveMtu, CwndCAStep, Progress Watchdog (PR #312)

**Purpose**: Three SCTP/WebRTC reliability corrections ported from upstream `aee94774` (settings only — the Android netlink/SDP changes from the same commit are not applicable).

**Changes**:
- `ReceiveMtu` 4 KiB → 1500: it's a per-packet demux buffer, not the SCTP receive window; Pion's SCTP path MTU stays under 1500 regardless, so the old value only inflated scratch buffers.
- `SctpCwndCAStep` = 4×1200: Pion's default adds one ~1.2 KiB MTU only after a complete cwnd is acknowledged; four MTUs makes recovery from independent loss competitive on higher-latency paths. Verified `SetSCTPCwndCAStep` exists at the fork's vendored `pion/webrtc` v4.2.15 — no dependency bump.
- SCTP progress watchdog (`SctpNoProgressTimeout` 10 s): lazy goroutine that starts after the first successful write and detects a blackholed association where ICE consent stays healthy but the data plane is dead, cancelling the connection. Ported without upstream's `requestImmediateReconnect()` (doesn't exist here) — uses the fork's existing `self.cancel()`-only teardown pattern.

**Files Changed**: `transport_p2p_webrtc.go`, `transport_p2p_webrtc_pc.go` (`webRtcSctpProgress`), `util.go` (`resetOrCreateTimer`), new `transport_p2p_webrtc_sctp_settings_test.go`.

**Status**: ✅ Merged `main` (2026-08-03). PR #312. v3.23.0-fix.26.6.

---

## 97. `sender_generation_id` Proto Field + Proto Source Drift Repair (PR #313)

**Purpose**: Add `sender_generation_id` to `ExchangeSignals` (disambiguates a delayed initial `WaitingForSdpOffer` from a newly restarted passive association). Inert until the peer-connection generation-reset logic lands; additive and wire-compatible with peers that don't set it. Ported from upstream `45357960` (field only).

**Also fixes proto source drift**: commit `ccada52` (Jul 11) regenerated `transfer.pb.go`/`frame.pb.go` from a different upstream `.proto` during an earlier port without landing the matching source changes. Running `make build` in `protocol/` against the stale sources would have silently deleted `NetworkPeer`, `NetworkPeersReset`, `NetworkPeersUpdate`, and the two `TransferNetworkPeers*` enum values — all live types used by `transfer_peer_manager.go`. The missing definitions were reconstructed from the already-shipping generated code and restored to `transfer.proto`/`frame.proto`; `protocol/`'s `make build` is safe to run again.

**Tests**: Marshal/unmarshal round-trips for the new field (with/without it set) and for the reconciled `NetworkPeer`/`NetworkPeersUpdate` types — proving wire serialization (protobuf-go serializes via the compiled descriptor, not struct tags alone), plus enum value pinning.

**Status**: ✅ Merged `main` (2026-08-03). PR #313. v3.23.0-fix.26.6.

---

## 98. Bound `SecurityPolicyStatsCollector` Destination Cardinality (PR #314)

**Purpose**: `resultDestinationCounts` (diagnostics map in `ip_security.go`) had no cap — on a long-running provider relaying to arbitrarily many distinct destinations, every unique (protocol, ip, port) tuple got a permanent map entry: unbounded memory growth on the diagnostics path.

**Fix**: Cap at `securityPolicyStatsMaxDestinationsPerResult = 1024` per result, final slot reserved as an overflow bucket ("other destinations"); unrecognized result values share one unknown-result bucket; zero-count updates ignored. Ported from upstream `45357960` (same commit as #313, different file). Actual diff is 67 lines — diagnostics bookkeeping only, no blocklist data touched.

**Tests**: 4 new — bound holds at 1050 distinct destinations fed in, unknown results share one bucket, zero-count no-op, overflow destination `String()`.

**Status**: ✅ Merged `main` (2026-08-03). PR #314. v3.23.0-fix.26.6.

---

## 99. Root Package Dependency Surface Reduction — `sn_deps.go` Deleted (PR #315)

**Purpose**: Remove the go-ethereum/AWS/Azure/zkVM tree from the root `connect` library package. `sn_deps.go` was four blank imports of `urfoundation/sn` packages predating real callers — its own comment said to remove it once callers imported them directly, and `provider/sn.go` (lines 38-41) now does exactly that. While the blank-import shim lived, `go mod why github.com/ethereum/go-ethereum` routed through the root `connect` package, dragging ~50 heavy dependencies (ethereum, AWS SDK, Azure SDK, gnark, c-kzg, ZK zkVM, DataDog zstd) into every consumer of the library (extender, connectctl, the Rust VPN client bindings).

**Fix**: `git rm sn_deps.go`. Nothing else — provider still has its real imports.

**Effect**: Root package 471 → 370 transitive deps; heavy deps 50 → 0; `go mod why go-ethereum` now routes via `connect/provider`, not `connect`. Provider binary size unchanged (27,016,488 B before/after — expected; the win is the library graph, not the binary). `go.mod`/`go.sum` byte-identical.

**Files Modified**: `sn_deps.go` (deleted)

**Status**: ✅ Merged `main` (2026-08-04). PR #315. v3.23.0-fix.26.7.

---

## 100. Connecting Guard in All Degraded Predicates + Dead Counters Removed (PR #316)

**Purpose**: Close the one-predicate gap left by the `IsDegraded()` fix. `RegisterProxy` reuses the existing `*proxyHealth` struct on re-registration and sets `connecting = true` without resetting the predecessor's `everUp`/`downSince` — so a freshly respawned instance inherits its dead predecessor's state. `IsDegraded()` carried the `!connecting` guard (10-line rationale comment); `DegradedProxies()` and `ProxyHealthByAddress()` had the identical predicate without it.

**Impact of the gap**: `DegradedProxies()` (feeds the URL-reaper at `provider/main.go:2179`) counted a mid-connect instance as degraded, skewing the keep/reap split (mitigated only because `reapProxies` re-verifies). `ProxyHealthByAddress()` was unmitigated — it writes `proxy.state`'s `Health` field (`provider/main.go:1749`) and the URL-source reaper's `isLive` (`provider/proxy_url_source.go:597`); a respawning proxy read as a degraded tier from the stale predecessor `downSince`, and the `isLive` check could demote or evict a proxy that was simply reconnecting.

**Fix**:
- `DegradedProxies()`: added `&& !h.connecting` to match `IsDegraded()`.
- `ProxyHealthByAddress()`: converted to a switch with `connecting` checked before `everUp`, mirroring the `IsDegraded()` guard.
- `ProxyHealthSnapshot()`: deleted dead `total`/`bwCount` counters (incremented, never read).

Deliberately NOT resetting `everUp`/`downSince` in `RegisterProxy` — that would wipe lifetime recovery stats and is a larger behavioral change.

**Tests**: 3 new (`TestDegradedProxiesExcludesConnecting`, `TestProxyHealthByAddressReportsConnectingOnRespawn`, `TestProxyHealthByAddressUpWinsOverConnecting`) + 5 follow-ups locking in dead/connecting/degraded tiers.

**Files Modified**: `proxy_health.go`, `proxy_health_test.go`

**Status**: ✅ Merged `main` (2026-08-04). PR #316. v3.23.0-fix.26.7.

---

## 101. Connecting-State Bound at One Pulse Cycle (PR #320, #322)

**Purpose**: Bound the `connecting` state, which #316 made visible but left unbounded. `connecting` is cleared only by `markProxyUp`/`markProxyDown` (`transport.go:743-752`), and neither fires on a failed dial — both are tied to an established WebSocket. So a proxy that respawned and never reconnected reported `"connecting"` indefinitely: you could not distinguish "respawned 2s ago" from "respawned 6h ago and never came back" by reading the state file.

**Fix**:
- `RegisterProxy` stamps `connectingSince = time.Now()`.
- New `connectingActive(now)` helper — `connecting` is treated as active only within `connectingStaleAfter`; zero-value (pre-bound or `RegisterProxyBandwidth`-init) counts as active so the bound doesn't retroactively break existing behavior.
- All six call sites (snapshot, heartbeat, `ProxyHealthByAddress`, `DegradedProxies`, `IsDegraded`, `NewlyDead`) use `connectingActive()`; a stale-connecting proxy falls back to a degraded tier computed from the stale `downSince` when it was previously up (`everUp`), and to `dead` when it never connected at all.
- `connectingStaleAfter` = **65 minutes** — one hourly retry pulse plus margin, explicitly coupled to the provider's `deadConfirmDelay` (`provider/main.go`, same value, comments cross-reference each other so one cannot change without the other). The original 15-minute value (in #320) was rejected as too aggressive: it made never-connected proxies read `dead` well inside the ~1h staging window that `docs/Proxy-Management.md` promises operators. Both clocks now agree. Note the two timers are independent: `deadConfirmDelay` runs from provider start and gates only the `NewlyDead` log row; `connectingStaleAfter` runs from each `RegisterProxy` call and gates state classification.
- Heartbeat and snapshot switches reordered so a fresh `connectingActive` state is evaluated **before** the inherited `everUp` (matching `ProxyHealthByAddress()`, which #316 already fixed): a previously connected proxy mid-respawn no longer counts as degraded in the health report — or as dead in the snapshot when the inherited `downSince` is older than 7 days.

**Behavioral note (verified)**: `NewlyDead` was latently dead code before #320 — its clause read `!h.connecting`, which was never false for never-up proxies, so the path could not fire. An API-equivalent test fails at pre-#320 `cb6794b` (`expected 1 NewlyDead event, got 0`) and passes at main. Now that `connectingActive()` goes false at 65m and `confirmDead` gates at 65m, it fires once per proxy that genuinely never connected within a full pulse cycle. Blast radius: one `DEAD` row in `proxy_health.log` (`provider/proxy_health_log.go:161`); nothing alerts on it.

**Tests**: 2 new — `TestNeverUpProxyReadsDeadAfterConnectingStale`, `TestNewlyDeadFiresForNeverUpProxyAfterConnectingStale` (backdate `connectingSince` symbolically, no sleeps). Plus the 3 from #320 (`TestConnectingStateExpiresToDegraded`, `TestDegradedProxiesIncludesStaleConnecting`, `TestConnectingStateResetsOnUpAndDown`).

**Files Modified**: `proxy_health.go`, `proxy_health_test.go`

**Status**: ✅ Merged `main` (2026-08-04). PR #320, #322. v3.23.0-fix.26.7.

---

## 102. Status Server Timeouts + `hub-join` Client Hardening (PR #317, #321)

**Purpose**: Two timeout/robustness gaps in the provider's HTTP surface. (1) The provider's status server was constructed with only `Addr`/`Handler` — no `ReadHeaderTimeout`, so a Slowloris-style dribbled-header client could hold connections open indefinitely on an exposed port. (2) Both `hub-join` PAKE round-trips used bare `http.Post(...)` — `http.DefaultClient`, no timeout, no context — so a blackholed hub wedged the CLI forever with no output; the same defect class already fixed once (2026-07-07 `refreshJWT` hang).

**Fix**:
- Status server: `ReadHeaderTimeout: 10s`, `IdleTimeout: 120s`, matching the hub's configuration (`hub/main.go`); `WriteTimeout` deliberately unset so the SSE stream is not killed.
- `hub-join`: one shared `http.Client{Timeout: 30s}`, both POSTs replaced, `signal.NotifyContext` so Ctrl-C aborts a wedged join; the previously-discarded KE2 response decode error is now checked and reported as a parse error instead of a confusing hex failure two lines later.
- #321 (same-night hotfix): CodeRabbit test-gen landed after #317 merged and generated `pake_handlers_test.go` against the pre-#317 `doHubJoin(hubURL)` signature, breaking the hub test package build on `main`; all call sites updated to the context signature.

**Files Modified**: `provider/main.go`, `hub/pake_handlers.go`, `hub/pake_handlers_test.go`

**Status**: ✅ Merged `main` (2026-08-04). PR #317, #321. v3.23.0-fix.26.7.

---

## 103. `go vet` Cleanliness — Unreachable Returns in `connectctl` (PR #318)

**Purpose**: `go vet ./...` reported 12 `unreachable code` findings, all the same pattern in `connectctl/main.go`: `panic(err); return` — the `return` after `panic` is dead. A permanently-dirty vet means a genuinely new finding in real code gets lost in the noise.

**Fix**: Deleted the 12 bare `return` statements following `panic(err)`. The `panic` calls themselves are untouched — `connectctl` is a developer CLI where panicking is intended behavior; converting to returned errors is a separate decision.

**Result**: `go vet ./...` fully clean — first time in the fork's history (0 findings repo-wide).

**Files Modified**: `connectctl/main.go`

**Status**: ✅ Merged `main` (2026-08-04). PR #318. v3.23.0-fix.26.7.

---

## 104. Stage-1 Table-Probe Quality Gate for URL-Source Proxy Admission (PR #342)

**Purpose**: A URL-source proxy only had to survive stage-0 admission (a SOCKS5 greeting plus an API `CONNECT`) to be trusted with real traffic. That proves the proxy is alive; it does not prove the proxy can actually reach the destinations providers need to reach. Bad or geo-restricted URL-source proxies were being admitted on aliveness alone and only shown to be useless once real client traffic started failing through them.

**Files Modified**: `ip_probe_targets.go`, `ip_probe_targets_api.go`, `ip_probe_targets_api_test.go`, `provider/proxy_probe.go`, `provider/proxy_table_probe.go`, `provider/proxy_table_probe_test.go`, `provider/proxy_table_probe_integration_test.go`, `provider/proxy_table_probe_review_test.go`, `provider/proxy_url.go`, `provider/proxy_url_source.go`, `provider/main.go`

**Change**:
- After the existing stage-0 handshake probe, each surviving proxy is graded against a sampled block of the backend's destination table (~127 health hosts, default 12 per pass), dialed through the proxy itself at `:443` with a 4s per-target timeout.
- Only proxies scoring `>= pass_bar` (default 0.6) are admitted to the auth queue; a `preferred_bar` (default 0.9) marks a higher tier used by downstream consumers.
- Follows upstream `ip_remote_multi_client_probe.go`'s design: positive evidence only (a SynAck proves the proxy's own upstream dial worked; silence never convicts, since an unreachable target for unrelated reasons shouldn't fail the proxy), resolution outside the probed channel (the box's own DNS, not the proxy's, so a proxy with broken DNS never fails a TCP probe it should pass), deterministic disjoint-block rotation across passes, and a viability abort so an aborted pass is always a decided verdict rather than an ambiguous one.
- A grade only overwrites the previous one when the pass is DECIDABLE (quorum of the requested sample answered, context not cancelled); an empty, cancelled, or resolver-gutted pass leaves the prior grade untouched — the same "the box's own DNS can never convict a proxy" guarantee applied to the grading loop itself.
- RFC 1929 auth (`host:port:user:pass`) now carries through both probe stages, so credentialed (usually paid) URL entries are graded on the same footing as free ones instead of failing auth mid-probe.
- Score/Graded/Failed persist in `proxy_url.json` cache entries, matching the backend's own data model.
- Kill switch: `~/.urnetwork/proxy_probe.json` with `{"enabled": false}` restores pre-feature behavior end-to-end. `sample_width`, `timeout_ms`, `pass_bar`, `preferred_bar`, and `enabled` are all runtime-tunable via the same file.

**Effect**: Proxies that grade below the bar never spawn, and the auth gate reports them distinctly from proxies that are simply dead — operators can now tell "alive but can't reach anything useful" apart from "dead." Needs a fleet deploy: this changes provider binary behavior and introduces a new operator-facing config file.

**How to Identify in New Upstream**:
- Search for `ip_remote_multi_client_probe.go` or destination-table probing logic in `urnetwork/connect`'s proxy admission path.
- Look for a second post-handshake admission stage gating URL-source proxies, or scoring fields (`Score`, `Graded`, `Failed`) on proxy cache entries.
- Check whether upstream's probe design still follows positive-evidence-only semantics before porting; a stricter (silence-convicts) upstream rewrite would need reconciling with this fork's DECIDABLE-only grade-overwrite rule.

**Status**: ✅ Merged `main` (2026-08-09). PR #342. v3.23.0-fix.26.8.

---

## 105. A-F Proxy Grade Tiers, Admission Funnel, and Best-Overall Eviction (PR #343)

**Purpose**: The stage-1 scores from #342 (entry 104) are a single pass/fail bar. They don't let the fleet preferentially keep its best proxies when the cache is full, or compare quality across sources when admitting new ones — a 0.61-scoring proxy and a 0.98-scoring proxy are treated identically once both clear `pass_bar`.

**Files Modified**: `proxy_probe.go`, `proxy_url.go`, `proxy_reload.go`, `provider/main.go`

**Change**:
- Letter-grade tiers layered on top of the existing stage-1 scores: A `>= 0.9`, B `>= 0.8`, C `>= 0.7`, D `>= 0.6`, F `< 0.6`.
- Best-overall cache eviction: when the proxy cache is full, eviction compares candidates across all sources by tier rather than per-source, so a full cache keeps the fleet's highest-tier proxies regardless of which source they came from.
- Admission funnel: candidates from all sources are pooled and added best-first up to the cap, instead of admitting per-source in whatever order sources happen to be processed.
- Per-cycle A-F probe grade breakdown logged, and probe-grade lines routed to both the important and disk logs.
- Fetch cycles probe only newly-seen addresses; the reaper's existing stale sweep is reused to refresh grades on already-cached proxies instead of re-probing everything every cycle.
- Cross-source duplicates are table-probed once per cycle (a live `probed` skip set), and the eviction tie-break uses the grade score.

**Effect**: Shipped. Changes which proxies survive cache pressure (best-overall rather than per-source) and adds new log lines (`admitted by tier`, `probe grade breakdown`, `cap eviction`, `reaper: refreshed grade`) that downstream log tooling and docs need to account for.

**How to Identify in New Upstream**: N/A — this is fork-native logic built on top of entry 104's stage-1 gate, which itself has no direct upstream equivalent yet. If upstream adds its own quality-tiering on top of `ip_remote_multi_client_probe.go`, compare tier thresholds and eviction policy before reconciling.

**Status**: ✅ Shipped in v3.23.0-fix.27.0 (merged 2026-08-09).

## 106. Read-Only Grading for Paid/File-List Proxies (PR #344)

**Purpose**: Proxies from `--proxy_file` / the internal config bypass the URL admission gate by construction, so the quality system (stage-1 scores, A-F tiers) had no opinion on them. Operators running paid lists had no signal on what their paid lists actually deliver.

**Files Modified**: `provider/proxy_state.go`, `provider/proxy_grade_paid.go` (new), `provider/main.go`, `provider/proxy_grade_paid_test.go` (new)

**Change**:
- Background sweep (`runPaidProxyGrader`) rides the reaper ticker and, on the same 1-3h pressure-scaled stale cadence, table-probes every tracked non-URL proxy the box serves (creds carried through RFC 1929).
- Grade persists into `proxy.state` ProxyEntry: Score/Graded/Failed/LastGraded (omitempty — same field shape as the URL store's cache entries).
- Read-only by construction: only the grade fields are written; admission, eviction, give-up, and cleanup never read them, so a graded F keeps serving exactly as it did before. Proxies without a tracker entry are skipped at collect AND apply (no ghost entries).
- Kill switch: `proxy_probe.json enabled=false` is a full skip, mirroring the fetch/reaper invariant.

**Effect**: Shipped. Every proxy the box serves now carries an A-F grade, not just URL-sourced ones. `proxy.state` grows the four grade fields per graded entry.

**How to Identify in New Upstream**: N/A — fork-native. If upstream adds non-URL grading, compare cadence and the read-only guarantee.

**Status**: ✅ Shipped in v3.23.0-fix.27.0 (merged 2026-08-09).

## 107. Provider-Aware `urnet-tools` Rewrite in Go (PR #345)

**Purpose**: The legacy `urnet-tools` (POSIX shell `Provider_Install_Linux.sh` ~4000 lines, plus a separate `urnet-tools.ps1` for Windows) resolved its target from a hardcoded path with zero awareness that other providers exist on the box — the root cause of the 08-08 pool-wipe and the 08-09 half-update. Two parallel implementations that drift (proven: box copies differ from repo copy).

**Files Modified**: `cmd/urnet-tools/main.go` (new), `cmd/urnet-docker/main.go` (new), `internal/urnettools/*` (new: cli, target, discover, update, proxy, legacy_cmds, lifecycle_cmds, release, docker, select_multi, provider, io_util, docker_actions + tests)

**Change**:
- Single Go codebase: `urnet-tools` (process/systemd variant) + `urnet-docker` (container variant). One source, cross-compiled — kills the shell↔PowerShell drift.
- Provider discovery, not path guessing: process scan + systemd units; identity is JWT-derived (network_name/network_id); paths only locate state.
- Targeting: `--unit` / `--user` / `--network` / `--network-id` / `--state-dir`. Multi-provider + no target = REFUSAL with inventory table. Conflicting selectors error. `-f` only skips confirm prompts, never picks providers.
- `--help` always prints help, never executes (the legacy `--help`-executes-clear bug class dies).
- `update`: interactive-first, latest-release fetch, sha256 digest MANDATORY (resolved from release API on `--tag`), private per-update `MkdirTemp` staging dir, atomic binary swap (dst.new + rename), restarts THE unit that is actually running (user vs system), timestamped backups, batch continues on per-provider failure.
- `optimize` platform-aware: Linux adds ephemeral-port pool + TIME_WAIT sysctls; Windows netsh dynamicport + TcpTimedWaitDelay.
- All 25 legacy subcommands dispatch (verified parity).

**Effect**: Shipped. The tool refuses to operate on an ambiguous target — operators on multi-provider boxes must specify a target. Migration is drop-in at the same path; `.ps1` and docker shell variant retired in Phase 2.

**How to Identify in New Upstream**: N/A — fork-native tooling. If upstream ships its own Go ops tool, compare targeting/discovery semantics before reconciling.

**Status**: ✅ Phase 1 shipped in v3.23.0-fix.27.0 (PR #345, merged 2026-08-09). Phase 2 (retire `.ps1` + docker shell variant) and Phase 3 (installer in Go) tracked in URN-TOOLS-GO-DESIGN.md §7.


## 108. In-Process Per-Proxy Client-JWT Renewal (PR #356)

**Purpose**: The backend (beta first, then mainnet's new token format) switched to 24-hour JWTs (exp−iat=86400, standard claims). The fork minted each proxy's client JWT once at process start and never renewed it in-process — after ~24h of uptime every proxy's token expired, audit/contract-OOB/transport auths all 401'd, and the provider silently became a black hole (registered, proxies up, no contracts, earnings decay to zero).

**Files Modified**: `provider/main.go` (`newProviderAuthClientArgsForRenewal`, `renewClientJWT`, watcher wiring), `provider/renewal_watcher.go` (new), `transfer_oob_control.go` (`SetByJwt` rotation hook + atomic 401 counter), `provider/client_jwt_store.go` (SetByJwt persistence), tests.

**Change**:
- Per-proxy watcher: renews when the client JWT's `exp` is within 12h (hourly retry) or immediately on a 401 fast-path; startup check renews an already-expired reused token.
- Renewal calls `/network/auth-client` WITH the existing `ClientId`, so the server UPDATEs the same network_client row and re-signs the same client_id/device_id — reputation preserved (a nil ClientId would mint a throwaway identity).
- Process-wide `renewalMutex` + shared auth rate limiter serialize auth-client calls on 50-60 proxy boxes; failed renewals keep the old token and retry.
- EXP-DRIVEN, not interval-driven: a no-op on backends still issuing long-lived tokens, so mainnet is unaffected.
- Revocation-watcher coordination: successful renewal stands down the pre-existing revocation watcher for that identity.

**Effect**: Shipped in v3.23.0-fix.28.0. Fixes the recurring beta 401 storms / black holes. Needs a fleet deploy to take effect on boxes.

**How to Identify in New Upstream**: Upstream has the `ApiOutOfBandControl.SetByJwt` hook + `PlatformTransport.SetAuth` but no provider dir and no renewal loop; the fork's watcher is fork-native.

**Status**: ✅ Shipped in v3.23.0-fix.28.0 (PR #356, merged 2026-08-11). Canary verified.

## 109. EncryptionMode Tri-State + Bounded TLS Establishment (PR #350, #353)

**Purpose**: Port upstream's EncryptionMode (Off/Opportunistic/Required) with fail-closed gates, and bound per-peer TLS establishment to a 60s handshake timeout (was unbounded) so departed peers cannot retain workers/goroutines.

**Files Modified**: `transfer_encrypt.go` (EncryptionMode enum, Required-mode send/receive gates, EncryptionEvent/PeerEncryptionState callbacks, RequiredCipherPollInterval), `transport.go`, `transfer.go` (`SendSequence.packMutex` → RWMutex so a parked Required-mode send cannot deadlock the handshake), tests.

**Change**: Tri-state encryption mode with a poll-driven fail-closed gate in Required mode; handshake establishment bounded by default (60s). The fork kept its own TlsTimeout default (fork deviation; upstream's parent value differs).

**Effect**: Shipped in v3.23.0-fix.28.0. Requires a fleet deploy.

**How to Identify in New Upstream**: The EncryptionMode port maps to upstream d2553e06; compare the packMutex RWMutex shape and the fork's TlsTimeout default before reconciling.

**Status**: ✅ Shipped in v3.23.0-fix.28.0 (PR #350, #353). Scope 3 (post-quantum key exchange hardening) tracked separately.

## 110. urnet-docker exec Flag Forwarding + ramlogs Container-Name Resolution (PR #349, #352)

**Purpose**: `urnet-docker exec` silently dropped unknown leading flags (an inner `-f`/`--verbose` before the executable vanished); the ramlogs hint printed a literal `<container>` placeholder that was not copy-pasteable inside a container.

**Files Modified**: `internal/urnettools/cli_docker.go` (splitExecArgs), `provider/shmlog_linux.go` (ramlogsTailHint).

**Change**:
- `exec` uses a `--` separator: everything after it forwards verbatim to the container command; unknown leading flags ERROR with a hint (never silently swallowed); a trailing target flag missing its value errors "requires a value" instead of panicking.
- ramlogs hint resolves the real container name: `URNETWORK_CONTAINER_NAME` env (opt-in), else the container ID via `os.Hostname()` when inside Docker, else a plain tail path on bare metal.

**Effect**: Shipped in v3.23.0-fix.28.0.

**How to Identify in New Upstream**: N/A — fork-native tooling.

**Status**: ✅ Shipped in v3.23.0-fix.28.0 (PR #349, #352).

## 111. Paid/Free Probe Divergence — Earn-Skip, Wider Paid Stale Window, URL Startup Cooldown (PR #357)

**Purpose**: Paid/file proxies cost the operator real money per stage-1 table probe; in steady state a paid proxy that is actively relaying traffic was still re-probed on the same cadence as free URL proxies. Divergence makes paid probe spend proportional to suspicion, and a startup cooldown closes a probe-amplification loop for crash-looping boxes.

**Files Modified**: `provider/earn_tracker.go` (new per-address delta tracker), `provider/proxy_grade_paid.go` (earn-skip, never-graded force-probe, paid stale window), `provider/resource_pressure.go` (paidStaleCalm/Hot), `provider/proxy_probe.go` + `provider/proxy_url_source.go` (probeStartupCooldown), `provider/proxy_grade_summary.go` (per-source windows + desired-set ownership), `provider/main.go` (tracker feed, empty-health-set prune), tests.

**Change**:
- Paid/file stale window 6h calm / 3h hot (vs URL 3h/1h), pressure-ramped.
- Earn-skip: a paid proxy with a positive billable delta within 15m is not re-probed; the signal is the per-address DELTA (never the raw cumulative counter — a proxy that earned once then died must not look "earning" forever); a hard 24h force-probe ceiling and never-graded-always-probe keep fail-fast honest.
- The tracker normalizes `ProxyHealthSnapshot`'s formatted `proxy[N] (addr)` keys to raw addresses (previously the keys never matched the grader's lookups — earn-skip was dead code in production); maps are pruned to the live proxy set and cleared when the health set empties.
- URL startup cooldown: first fetch + probe deferred 20s after process start; a crash-looping box (5s restarts) never re-probes.
- Grade summary: per-source freshness windows (URL vs paid) and desired-set ownership resolution (file ownership overrides a stale "url" provenance tag).

**Effect**: Shipped in v3.23.0-fix.28.1. Requires a fleet deploy — probe cadence and probe spend change on every box at redeploy time.

**How to Identify in New Upstream**: N/A — fork-native design; upstream has no per-proxy earn tracking.

**Status**: ✅ v3.23.0-fix.28.1 (PR #357).

## 112. Tool Distribution — Release Assets, Installers, and Self-Update (PR #362)

**Purpose**: The Go tool from #345 was merged but never shipped: `release.yml` built only provider+hub, the release tarballs still carried the legacy shell script as `urnet-tools`, the Docker image baked the shell variant, and the systemd installer self-copied the shell script. Docker-only users (half the user base) had no supported way to get `urnet-docker` at all. This PR wires up the distribution path the design doc (§8, §10, §11) specified but never landed.

**Files Modified**: `.github/workflows/release.yml`, `scripts/Provider_Install_Linux.sh`, `scripts/Provider_Install_Mac.sh`, `scripts/install-urnet-docker.sh` (new), `cmd/urnet-tools/main.go`, `cmd/urnet-docker/main.go`, `internal/urnettools/update.go`, `internal/urnettools/release.go`, `internal/urnettools/legacy_cmds.go`, `internal/urnettools/cli.go`, `internal/urnettools/cli_docker.go`, `docs/urnet-tools-go.md`, `docs/Docker-Deployment.md`, `CHANGELOG.md`

**Change**:
- `release.yml` now builds `cmd/urnet-tools` and `cmd/urnet-docker` for the full matrix and attaches them as standalone release assets named `urnet-tools-<os>-<arch>` / `urnet-docker-<os>-<arch>` (e.g. `urnet-tools-linux-amd64`, bare — no `.exe` even on Windows). GitHub's release API publishes a sha256 `digest` per asset, which the installers and the tool's own self-update verify.
- `Provider_Install_Linux.sh`: fresh installs and `update`/`reinstall` now fetch the Go `urnet-tools-linux-<arch>` binary, verify its sha256 against the release API digest, and install it at `bin/urnet-tools` — a one-time handoff from the shell wrapper to the Go tool. Falls back to the legacy self-copy shell script only for releases that predate the Go asset (or 386 hosts).
- `Provider_Install_Mac.sh`: same swap for `urnet-tools-darwin-<arch>` (verified via `shasum -a 256`), legacy wrapper fallback.
- `scripts/install-urnet-docker.sh` (new): standalone host-side installer for docker-only users — `curl ... | sh` detects os/arch, resolves the latest release, downloads the tool asset, verifies sha256, installs to `/usr/local/bin` (or `~/.local/bin` when not root). Same script can install `urnet-tools` with `sh -s -- urnet-tools`.
- **Tool self-update** (`update.go`): `urnet-tools update` now also refreshes the tool binary itself from the same release (digest-verified, structural check, timestamped backup, atomic rename) — a failure there is reported but does not fail the provider updates. New `self-update`/`selfupdate` subcommand updates ONLY the tool (works on boxes with zero providers). `urnet-docker update` = tool self-update (containers update by image pull).
- **Platform-aware binary checks** (`legacy_cmds.go`): the structural sanity check on downloaded binaries now accepts ELF (linux), Mach-O (darwin), and PE (windows) instead of ELF only — the old check rejected every macOS/Windows self-update, and the same latent defect existed in the provider tarball and hub download paths. The darwin magic bytes were verified against real cross-compiled binaries.
- `releaseInfo` carries the full asset list so the tool's own asset digest resolves from the same release JSON; the provider tarball digest field renamed `ProviderDigest` for clarity.

**Effect**: Every install path hands off to the Go tool where the release carries it (v3.23.0-fix.28+); older releases and 32-bit x86 hosts retain the legacy shell fallback. The Go tool keeps itself current. Docker-only users: `curl -fSsL .../install-urnet-docker.sh | sh`, then `urnet-docker update` going forward.

**How to Identify in New Upstream**: N/A — fork-native tooling.

**Status**: ✅ PR #362. Needs a fleet deploy for the installer change; new tool assets ship with the next tagged release.

## 113. Hub A-F Grade Surfacing — Report Payload, proxy_grades Store, Dashboard (PR #360)

**Purpose**: The provider boxes compute A-F grades for every proxy they serve (stage-1 table probe; paid/file-list proxies graded on the same cadence), but the hub never received them — the fleet dashboard was blind to the whole grading system, and its existing "Score" column is a different traffic-composite metric (`win% × ln(1+traffic)`). This closes the gap end-to-end.

**Files Modified**: `provider/` (report payload grade fields + `proxyGradeFor` helper + URL-store `LastGraded`), `hub/` (proxy_grades store, ingest sanitization, dashboard Grade column), tests.

**Change**:
- `proxyReport` gains `score/graded/failed/tier/last_graded` (omitempty — legacy payload shape preserved for ungraded proxies and older hubs). Grades resolved by a pure `proxyGradeFor(addr, paid, url)` helper: the paid/file store (`proxy.state`) wins over the URL store when both are graded.
- URL-store grade timestamps are honest: a real `LastGraded` field, stamped only when a genuine stage-1 grade lands; liveness-only `LastProbe` bumps never advance it.
- Hub ingests into a new `proxy_grades` table keyed `(node_id, proxy_id, hour)` — latest report in an hour overwrites (latest-wins), earlier hours stay as history; 7-day retention keeps the best-proxies join bounded.
- Ingest hardening: grade tiers sanitized to exactly A-F before the in-memory store or the DB (the field is attacker-influenced — any authenticated node's report body — and rendered into dashboard HTML); both render sites additionally HTML-escape.
- Dashboard: color-coded A-F Grade column (badges) in the Best Proxies table (sortable) and the node drawer; the existing Score column is untouched and stays labeled.

**Effect**: Shipped. The dashboard's Grade column is a NEW metric — distinct from the traffic-composite Score column; never compare them directly.

**How to Identify in New Upstream**: N/A — the fork's report/schema/hub are not shared with `urnetwork/connect`.

**Status**: ✅ v3.23.0-fix.28.1 (PR #360). Needs a fleet deploy — provider binary + hub image both change.
## 114. Resource-Leak Hunt — Per-Bucket Source-Count Refcount Fix (PR #367)

**Purpose**: A monotonic map-growth leak in the multi-client window. `addSource` incremented `sourceCount[source]` per PACKET while the bucket path set is deduped, and eviction decremented once per (bucket, path) — so a source sending N packets in one bucket left a phantom N-1 count that never reached zero, and the (destination, source) entry was never pruned. At thousands-of-proxies scale this grew `ip4DestinationSourceCount`/`ip6DestinationSourceCount` monotonically per pair, inflating window-resize sizing and ulimit warnings.

**Files Modified**: `ip_remote_multi_client.go`, `ip_remote_multi_client_test.go`.

**Change**: `addSource` now increments the source-count refcount only when the path is first added to the bucket — exactly matching what eviction subtracts. The count is a pure liveness refcount (readers only use `len()` of the inner maps), so no consumed value changes; pruning is restored.

**Effect**: Fixed. The first finding of a full leak hunt (see #116-#120); every fix in the hunt is mutation-proven (removing the guard fails the regression test).

**How to Identify in New Upstream**: The pattern is fork-native to the multi-client window; check `addSource`/`coalesceEventBuckets` for increment/decrement symmetry.

**Status**: ✅ v3.23.0-fix.28.1 (PR #367). Needs a fleet deploy.

## 115. Resource-Leak Hunt — runHandshake Worker Freed on TlsTimeout (PR #369)

**Purpose**: The handshake timeout watchdog fired `completeHandshake(timeoutErr)` at TlsTimeout but never cancelled the epoch's ctx, so `runHandshake` stayed parked in `sequenceTlsTransport.Read` (no deadline) until a rebuild or session close — a stranded goroutine per timed-out handshake against silent/departed peers.

**Files Modified**: `transfer_encrypt.go`, `transfer_encrypt_restart_test.go`.

**Change**: The watchdog now cancels the epoch ctx only when the timeout actually recorded a failure (`handshakeErr` set). `completeHandshake` can no-op through its done() gate if the handshake finished concurrently — the epoch is then established and its ctx must stay alive to serve the live cipher, so the cancel is gated on the recorded failure (race sibling). Complements the rebuild-cancel from #353 (covers the churn case where a new send arrives); this closes the silent-peer case.

**Effect**: Fixed. Stranded worker per timed-out handshake eliminated.

**How to Identify in New Upstream**: `handshakeTimeoutWatcher` in `transfer_encrypt.go` — check whether the timeout branch cancels the epoch ctx.

**Status**: ✅ v3.23.0-fix.28.1 (PR #369). Needs a fleet deploy.

## 116. Resource-Leak Hunt — update ctx Cancelled on Client Removal (PR #370 + #373)

**Purpose**: `removeClient` nulled `update.client` but never cancelled `update.ctx`, so the update's per-flow teardown goroutine stayed parked in `waitForIdleUpdate` (`time.After` up to SequenceIdleTimeout, default 120s) and could not observe the client removal until the idle timer fired. Every client removal stranded one goroutine + one timer for the full idle timeout.

**Files Modified**: `ip_remote_multi_client.go`, `ip_remote_multi_client_test.go`.

**Change**: `removeClient` — the single choke point wired as `clientRemoveCallback` for every window — now cancels each affected update's ctx, covering all removal paths (window resize, error removal, replacedClient, shuffle, batch removeClients). `waitForIdleUpdate` returns on ctx.Done and the teardown loop's updateDone check tears the flow down cleanly; the goroutine's own `defer update.cancel()` remains idempotent-safe. Follow-up #373 added a supersede guard: the teardown closure now verifies it is still the registered update for its path before committing the delete and the affinity strip (both ip4/ip6 branches), so a successor registered at the same path after cancellation is never orphaned — otherwise packets would split across exit clients and break flows.

**Effect**: Fixed. Stranded teardown goroutines/timers eliminated; successor-update clobbering (found by independent audit of #370) closed.

**How to Identify in New Upstream**: `removeClient` in `ip_remote_multi_client.go` — check whether it cancels per-update ctxs and whether the teardown delete is guarded by path-map ownership.

**Status**: ✅ v3.23.0-fix.28.1 (PR #370, #373). Needs a fleet deploy.

## 117. Resource-Leak Hunt — Zombie refs==0 Sessions under IdleTimeout==0 (PR #371)

**Purpose**: The PQE/Required path replaced a nil `EncryptionSettings` with `DefaultEncryptionSettings()`, which ships `IdleTimeout == 0`. With that value, `Release()` defers deletion while a handshake is in flight (the session must stay registered so the next send reuses it), but `Run()` only arms its `CancelIfIdle` reap loop when `0 < IdleTimeout` — so once the handshake settles (success or failure), a refs==0 session stayed registered forever: a zombie that grew monotonically per departed peer.

**Files Modified**: `ip_remote_multi_client.go`, `ip_remote_multi_client_test.go`.

**Change**: The PQE path now derives the same sequence-bounded idle horizon `DefaultClientSettings` uses (max of send/receive buffer idle timeouts) instead of adopting the 0 default, so the reap loop is armed and the session outlives the sequences that ref-hold it without leaking indefinitely. A reap-recheck-after-handshake was deliberately not chosen: it would tear down a just-established cipher and churn a fresh ClientHello on the next send, which the keep-alive comment explicitly warns against.

**Effect**: Fixed. Zombie refs==0 sessions under the PQE path eliminated.

**How to Identify in New Upstream**: `DefaultEncryptionSettings` in `transfer_encrypt.go` ships IdleTimeout 0; check every consumer that adopts it wholesale for an armed reap loop.

**Status**: ✅ v3.23.0-fix.28.1 (PR #371). Needs a fleet deploy.

## 118. Resource-Leak Hunt — Pooled Buffer Returned on SendEncryptedControl Cancel (PR #372)

**Purpose**: The `SendEncryptedControl` retry loop exited via ctx.Done (caller ctx or send-buffer ctx) without returning `ecBytes` — a message-pool buffer from `ProtoMarshal`. `Pack` only takes ownership of `Frame.MessageBytes` on success (the send sequence returns it after transmission); on every failed Pack the caller retains ownership, so each cancel-path exit leaked one pooled buffer per teardown race.

**Files Modified**: `transfer.go`, `transfer_test.go`.

**Change**: Both ctx.Done exits in the retry loop now return `ecBytes` to the pool. `MessagePoolReturn` is safe if `MarshalAppend` reallocated (cap no longer matches a pool bucket -> no-op), so the realloc corner is covered.

**Effect**: Fixed. One pooled buffer per teardown race no longer leaked.

**How to Identify in New Upstream**: `SendEncryptedControl` in `transfer.go` — check the retry loop's exit paths return the marshaled bytes.

**Status**: ✅ v3.23.0-fix.28.1 (PR #372). Needs a fleet deploy.

## 119. Resource-Leak Hunt — Cancel Unsubscribes Receive Callback (PR #374)

**Purpose**: `multiClientChannel.Cancel()` called `self.cancel()` and `client.Cancel()` but never `clientReceiveUnsub()`, unlike `Close()`. A Cancel-only path (client eviction, shuffle, replacedClient) left the channel's receive callback registered on the client, retaining the dead channel's callback chain until the next resize — a bounded but steady retention per cancelled client.

**Files Modified**: `ip_remote_multi_client.go`, `ip_remote_multi_client_test.go`.

**Change**: `Cancel()` now calls `clientReceiveUnsub()` like `Close()`. `CallbackList.Remove` is idempotent (BinarySearch miss returns silently), so a later Close unsubscribing again is a no-op.

**Effect**: Fixed. Dead clients release their callback chains immediately.

**How to Identify in New Upstream**: `multiClientChannel.Cancel` vs `Close` — check both unsubscribe the receive callback.

**Status**: ✅ v3.23.0-fix.28.1 (PR #374). Needs a fleet deploy.

## 120. Resource-Leak Hunt — Flaky Test Root Causes Closed (PR #364, #368)

**Purpose**: Two CI flakes traced to deterministic bugs rather than load, and fixed at the root so every future PR stops hitting them.

**Files Modified**: `ip_remote_multi_client_test.go`, `provider/proxy_grade_paid_test.go`.

**Change**:
- PR #364: the window-stats `-short` timeout was a fixed 300ms that only happened to span the 3-bucket minimum; the assertion silently became a coin flip under `-race` load. The timeout is now derived from `StatsWindowBucketDuration` (4x = one spare bucket over the minimum), and deterministic regression tests (synthetic buckets, no wall-clock dependency) were folded in.
- PR #368: `TestPaidProxyGrader_UndecidableKeepsPriorGrade` failed intermittently because the probe host table contains literal IPs (1.1.1.1/8.8.8.8/9.9.9.9) that `resolveProbeTarget` short-circuits via `net.ParseIP` BEFORE consulting the DNS-fail cache — a seeded fail can never make a literal IP unresolvable, so any pass whose sampled block includes one always dials it (~4.7% of passes). The test now pins the pass counter to a hostname-only block and seeds exactly that.

**Effect**: Fixed. CI is deterministic; the flake class (timing-sensitive assertions, literal-IP fixtures) documented in the audit skill.

**How to Identify in New Upstream**: Any test asserting "all sampled targets unresolvable" against a table containing literal IPs is unfixable-by-seeding; pin the pass to a hostname-only block.

**Status**: ✅ v3.23.0-fix.28.1 (PR #364, #368). No fleet deploy — test-only.

## 121. Go-Tool Gauntlet — 8 Field-Tested Fixes for the Rewritten urnet-tools/urnet-docker (PR #376)

**Purpose**: The Go tool rewrite from #345 was merged and shipped, but its delegation assumptions and CLI surface had never been exercised against a live provider. A field gauntlet on a fresh droplet against the live beta network found 8 real bugs — several of them silently breaking core fleet operations (URL proxy management, hub reporting, provider restarts).

**Files Modified**: `cmd/urnet-tools/main.go`, `cmd/urnet-docker/main.go`, `internal/urnettools/cli.go`, `internal/urnettools/cli_docker.go`, `internal/urnettools/proxy.go`, `internal/urnettools/legacy_cmds.go`, tests.

**Change**:
- **version command**: `version`/`--version`/`-v` now print the stamped tool version (previously errored "unknown command" even though main.Version is stamped via ldflags). ToolVersion package var wired from main.Version in both cmd mains.
- **report**: previously delegated to `<provider> report`, which does not exist in the provider CLI (auth/provide/proxy/wallet/claim/logs/summary) — printed auth usage, did nothing. Now writes `~/.urnetwork/report_url` in the provider's state dir, the documented override the bandwidth reporter re-reads every tick (no restart). Written 0644 so a provider running as a different user than the tool can read it (root tool + urnetwork-beta service is the fleet norm).
- **hot-restart**: previously delegated to `<provider> hot-restart` (does not exist). Now restarts the provider's systemd unit via unitCommand, behind the same confirm gate as cmdRestart (`--force`/dry-run supported through the dispatcher's global flags).
- **systemctl argv**: `unitCommandArgs` built `systemctl <action>` WITHOUT the unit name — start/stop/restart/hot-restart all failed with "Too few arguments". The unit is now always the final argument (system: `systemctl <action> <unit>`; user: `systemctl --user -M <user>@ <action> <unit>`).
- **summary**: delegated as `<provider> summary` but the provider nests it under proxy (`provider proxy summary`). cmdSimpleDelegation now builds `["proxy", sub]`.
- **proxy add-source / remove-source**: URL proxy sources (a core fleet feature) were entirely unmanageable through the Go tool. Both are now single-target delegations to `provider proxy add-source <url>` / `remove-source <url>`.
- **proxy refresh --force**: the dispatcher's parseGlobalFlags consumed `-f`/`--force` as the global force flag before cmdProxy ran, so the provider never received it — the warmup gate (exit 52) was never bypassed. cmdProxy now re-adds `--force` to the refresh opArgs when the global force flag was set.
- **proxy help**: `-h`/`--help` at ANY position in proxy args shows proxy-specific help and never executes. Previously showed root usage, was rejected as an unknown flag, or — for `proxy refresh --force -h` — reached the interactive picker and blocked on EOF.

**Effect**: Shipped. Every fix was live-verified on the droplet (version prints, report writes the file, hot-restart restarts the unit with identity reuse, summary prints the real proxy summary, add-source fetches + probes, refresh --force bypasses warmup, help never hangs). The full `update -f` flow (download → sha256 verify → backup → install → restart) also verified end-to-end. Docker container deploy (BUILD=jwt), urnet-docker exec/restart/logs verified.

**How to Identify in New Upstream**: N/A — the Go tooling is fork-native (upstream still ships the shell tool).

**Status**: ✅ v3.23.0-fix.29.0 (PR #376). Needs a fleet deploy — the Go tool binaries ship as release assets with this release.

---

## 122. TCP Stack Port — RFC 7323 Timestamps, MSS Negotiation, Initial-Window Warmup (PR #380)

**Purpose**: The provider's TCP option handling was window-scale-only. Upstream perfvar had since added RFC 7323 timestamps, MSS-aware segmentation, and an initial-window warmup path, none of which this fork carried. Without timestamps and correct MSS clamping, the stack under-negotiates against modern peers and loses the round-trip-time and PAWS protections RFC 7323 provides.

**Files Modified**: TCP option parsing and segmentation paths (unified MSS/window-scale/timestamp option parser), DataPackets segmentation, ReadBufferByteCount accounting, InitialWindowSize setting, plus new tests covering window-scale table cases, MSS+timestamp extraction, malformed-tail parsing, SynAck layouts, timestamp bytes, PureAck TSopt, and DataPackets timestamp segmentation.

**Change**:
- Unified TCP option parser reads MSS, window scale, and timestamp together, replacing the old window-scale-only parser.
- RFC 7323 timestamps negotiate from the peer's SYN and emit on every non-RST segment (SYN-ACK, data, pure ACK, FIN-ACK), backed by a monotonic clock and a reorder guard.
- Peer-MSS-aware segmentation: DataPackets clamp payload to the peer's advertised MSS (RFC 879/6691 data-only semantics), with a 536-byte floor.
- New InitialWindowSize setting: memory-budget scaled, power-of-two, clamped to 4 KiB-128 KiB. The SYN advertises the literal unscaled window per RFC 7323; the post-handshake ACK then jumps to the warmup window.
- The fork keeps its existing 4 KiB/4 MiB low-RAM window profile rather than following upstream's move to 64 KiB/16 MiB.
- ReadBufferByteCount now accounts for the timestamp option, so full IPv6 reads no longer split into a runt tail segment.

**Effect**: Shipped. This is the only provider binary code change in v3.23.0-fix.29.1. Multiple independent review passes covered the port; the existing test suite plus new tests (window-scale table across 9 cases, MSS+timestamp extraction, malformed-tail parsing, SynAck layouts, timestamp bytes, PureAck TSopt, DataPackets timestamp segmentation) all pass.

**How to Identify in New Upstream**: Compare the fork's unified TCP option parser and timestamp/MSS handling against upstream perfvar's current state. If upstream changes its low-RAM window profile again, reconcile against this fork's deliberately-retained 4 KiB/4 MiB profile before porting further.

**Status**: ✅ v3.23.0-fix.29.1 (PR #380). Needs a fleet deploy — operators must redeploy provider nodes for this change to take effect.

---

## 123. Release Tooling and Pre-Release Shakedown (PR #381-391)

**Purpose**: Release binaries shipped with no automated malware scanning, the DoH test suite could hang indefinitely on live server lookups, the gauntlet workflow (the pre-release droplet test) needed further hardening before it could be trusted as a release gate, and the shakedown's runner-to-droplet lifecycle leaked ssh sessions and swallowed the exit code.

**Files Modified**: CI workflow files for release scanning, the shakedown workflow (formerly gauntlet), a new sweeper workflow + script, DoH test files (hermetic httptest server replacing live server queries).

**Change**:
- VirusTotal scan of release binaries (#381): two-tier gate, 0-2 detections pass, 3-10 ship flagged for review, 11+ blocks the release; writes a scan report receipt with per-artifact PASS/REVIEW/FAIL table and permalinks. Fails on tag runs when the API key secret is missing.
- DoH test fix (#382): TestDohQuery/TestDohCache now use a local httptest server serving canned Google-style JSON DoH responses instead of querying live public DoH servers for a hostname that does not resolve, so the tests fail fast instead of hanging on a retry loop.
- ClamAV scan of release binaries (#383): EICAR self-test plus clamscan over all 20 release binaries, run after the VirusTotal scan as a quota-free second opinion.
- Pre-release shakedown workflow build-out (#384-390): the workflow itself on a disposable 1 CPU/1 GB DigitalOcean droplet with guaranteed cleanup; apt-get update before docker install; skip URL checks when auth failed; report review step; Discord webhook verdict posting; preflight connectivity gate; public IPv4 requirement before SSH; full-journal auth-check window fix; Wacatac.C!ml false-positive annotation; exit-status-first assertions and a self-test phase; hub coverage for systemd and docker; rename from gauntlet to shakedown.
- Shakedown lifecycle bulletproofing (#391): the test runs DETACHED via systemd-run --no-block so the ssh session returns immediately (backgrounded children previously held the pipe, the watchdog killed the session, and the exit code was swallowed showing green). The exit code survives through an rc sentinel written by atomic rename. A deterministic gate maps exit 0/1/75/124 to PASS / RELEASE_BLOCKED / ENV_BLOCKER / HARNESS_CRASH; a crash before any FAIL line cannot false-green. A new shakedown-sweeper workflow (separate file, 15-minute schedule) destroys shakedown-ci droplets older than 3 hours and reaps stale SSH keys, the backstop for runner eviction or force-kill. The droplet is tagged at create; cleanup retries delete-by-id and delete-by-tag, verifies the list is empty, and the trap covers EXIT INT TERM HUP. Polling is bounded, the report streams on every poll, a heartbeat detects a stalled script, and a concurrency group queues runs instead of cancelling. Six independent design and verification review rounds were run; every finding applied. The release pipeline also gained a syntax-check step that runs before droplet creation.
- The shakedown covers a fresh install, auth, the Go tool, the full proxy lifecycle, URL sources against real free proxies, the admission pipeline, docker, the hub under systemd and docker, hot-restart identity, update --tag, self-update, a long observation phase with resource sampling, and clean shutdown. Tier-1 failures exit non-zero and fail the job. A full run takes about 2 hours, bounded by a watchdog and job timeout.

**Effect**: Shipped. Release binaries now get two independent malware scans before publishing. The DoH test suite no longer hangs in CI. The shakedown workflow is a trustworthy pre-release gate that exercises a full fresh-install path against a live droplet before a tag ships, and the runner-to-droplet lifecycle can no longer leak a droplet or false-green a broken release.

**How to Identify in New Upstream**: N/A. This is fork-native release tooling with no upstream equivalent.

**Status**: ✅ v3.23.0-fix.29.1 (PR #381-391). CI-only, no fleet deploy needed for the pipeline itself. Entry 122's TCP port above is the deploy item in this release.

---

## 124. Provider HOME-Robustness Fix + Release Pipeline Follow-Ups (post-merge direct commits)

**Purpose**: The pre-release shakedown for v3.23.0-fix.29.1 found a provider startup panic when the HOME environment variable was unset, blocking the first release attempt. These direct commits fix that panic and also land three release-pipeline changes made after PR #381-391 merged: a scan/publish split so malware scanning never blocks publication, an adaptive shakedown wait tied to the release workflow's actual conclusion, and the v29.1 re-cut itself.

**Files Modified**: Provider client-JWT store and startup path (HOME resolution), auth and proxy-config write paths, the release workflow (scan/publish split), the shakedown workflow (adaptive asset wait).

**Change**:
- Provider HOME-robustness fix: the provider panicked with "$HOME is not defined" on every invocation when HOME was unset, breaking `--version`, one-shot commands, and the `update --tag` path in bare environments (for example, a root shell with no HOME set). HOME resolution is now safe at startup. When HOME is unavailable, the client-JWT store degrades to in-memory-only mode, so identity reuse is lost for that process but the command still runs. The auth and proxy-config write paths now return errors instead of panicking.
- Release scan/publish split: `create-release` now publishes the release immediately, without waiting on malware scans. VirusTotal and ClamAV run afterward in a separate, non-blocking scan job that appends its verdict to the release body once done. Scanning can no longer delay or gate publication.
- Adaptive shakedown asset-wait: the shakedown workflow used to wait a fixed amount of time for release assets to appear. It now waits for the release workflow's own conclusion instead, so it does not start early against a partially-published release or sit idle after the release is already ready.
- v3.23.0-fix.29.1 re-cut: the first release attempt was blocked by the HOME panic above. Once the fix landed, the release was re-cut under the same version number and shipped clean.

**Effect**: Shipped. The provider no longer panics on missing HOME. Release publication is decoupled from scan completion time. The shakedown starts exactly when a release is ready instead of guessing with a timer.

**How to Identify in New Upstream**: N/A. This is fork-native robustness and release-tooling work with no upstream equivalent.

**Status**: ✅ v3.23.0-fix.29.1 (post-merge direct commits, after PR #381-391). Needs a fleet deploy for the provider HOME-robustness fix; the release/shakedown workflow changes are CI-only.


## 125. Cross-Platform Lifecycle, urnet-docker Proxy Commands, Runner-Based Testing (v3.23.0-fix.30)

**Purpose**: Complete the cross-platform story for the Go management tools. Auto-start and auto-update now work on Windows through Task Scheduler and on macOS through launchd. The Docker tool gains host-side proxy commands. The test droplet is replaced by runner-based functional testing.

**Windows**:
- Auto-start/auto-update use Task Scheduler (weekly = Sunday midnight default, monthly = day 1). Replaces the legacy startup-folder shortcut.
- Uninstall removes scheduled tasks and deletes the JWT properly.
- State directory corrected to `%USERPROFILE%\.urnetwork` (matches the provider's actual data location).
- `safeRemoveTarget` now accepts absolute Windows paths and rejects volume roots. Previously it rejected ALL Windows paths, so uninstall could never delete anything.
- Scheduled-task names are per-provider; two providers no longer overwrite each other's tasks.
- The legacy startup-folder `.lnk` is removed when the schtasks path is taken.

**macOS** (new):
- Auto-start/auto-update use launchd agents (weekly = Sunday midnight default).
- Headless fallback: `user/<uid>` domain when no GUI session exists.
- Discovery uses pgrep because macOS has no /proc.

**Linux**:
- Auto-update timer is disabled on uninstall (was orphaned).
- cgroup-derived unit names are filtered by `isProviderUnit`.

**urnet-docker proxy commands** (host-side, Design 2):
- `proxy add/clear/remove/add-source/remove-source/refresh/remove-dead` run from the host; exec plumbing hidden.
- Multi-container targeting: interactive picker (TTY) or flags; untargeted multi-container refuses.
- Wrapper now supports add-source/remove-source; clear maps to remove-all.

**Testing**:
- Runner-based functional smokes (real auth, fail loudly) replace the droplet.
- 3-hour soaks for native + docker, 25-proxy cap, restart cycles, client-ID reuse. Both green.
- Multi-container tests: turbo-v8/auto/lowmem modes, targeting refusal.
- CI: setup-go native cache + retry; gofmt enforced; auth verification in tests.

**Image tags**: `latest` = last tagged release; `main` = current code; CI pulls `main`.

## 126. Post-Quantum Encryption Interop + urnet-tools Usage Text (v3.23.0-fix.30.2, PR #400 + #401 + #402)

**Purpose**: Make post-quantum encryption work end to end between an app and a provider. This closes a gap where an app with post-quantum encryption turned on could never complete a handshake to a fork provider and stalled at the 60-second timeout.

**PR #400 — provider enables encrypted sessions** (`provider/main.go`):
- The serving client now sets `EncryptionModeOpportunistic` instead of leaving the mode unset (`EncryptionModeOff`).
- Before, the session layer never started because the mode was off. A TLS ClientHello from an encrypted app connection was never answered.
- The mode is Opportunistic, not Required, so plaintext consumers are still served. This mirrors the stock provider build (`sdk/device_local_provider.go`).
- Helper `enableProviderEncryption` nil-checks and creates the settings, preserves cert/key fields, and never sets Required. Tests cover the mode switch, a live session layer (`TestEnableProviderEncryptionSessionManager`, mutation-verified), idempotency, and field preservation.

**PR #401 — control channel routes by handshake generation** (`transfer_encrypt.go`, `protocol/transfer.proto`):
- Added `optional bytes epoch_id = 5` to `EncryptedControl` (regenerated `transfer.pb.go`). Unset keeps legacy behavior.
- `tlsHandshakeEpoch` gains an `epochId` (a ULID minted by the TLS-client role). `epochIdOf`/`adoptEpochId` bind the responder to the initiator's generation.
- Outbound handshake and identity-proof controls are stamped with the epoch id.
- Inbound routing: `deliverHandshake(payload, epochId)` and `receivePeerIdentityProofForEpoch(payload, epochId)`. Older generations are ignored. A newer generation resets the handshake onto the newer one. A malformed nonempty epoch id is rejected outright (never downgraded to legacy).
- This ports the epoch-generation mechanism upstream `urnetwork/connect` already has (`epochId`, `adoptEpochId`, `epochIdOf`, `deliverHandshake(payload, epochId)`, `receivePeerIdentityProofForEpoch`). The fork previously had none of these.
- Scope: the port carries the epoch identity and inbound routing. It intentionally does not port upstream's `identityFailedTerminal` safeguard or the `UnknownWrapNack` subsystem, which live in a different layer. Peer controllers using the stock app SDK are wire-compatible.
- Tests: ~28 epoch/identity tests (adopt, stale/newer routing, legacy skip, malformed rejection, proto round-trips).

**Also in this release** (PR #402): `urnet-tools usage()` now lists the full command surface, grouped into core, performance, proxy, hub, and maintenance sections. Before, it showed only a few core commands.

**Verified live**: an app with post-quantum encryption turned on connected to a fork provider running both fixes. The handshake completed in milliseconds. Traffic flowed and became billable. No 60-second timeout.

**How to Identify in New Upstream**:
- Search for `epochId` in `transfer_encrypt.go` (upstream has them; the fork did not before this work).
- The provider's serving client sets `EncryptionModeOpportunistic`.

---

## 127. Busybox-Safe In-Container Updates + Nightly Self-Update Hardening (v3.23.0-fix.30.3, PR #406 + follow-up)

**Purpose**: The provider container is Alpine-based, so `mktemp` is busybox `mktemp`, which requires `XXXXXX` to be the LAST characters of the template. The in-container `urnet-tools update` used `mktemp /tmp/urnetwork-update-XXXXXX.tar.gz` — the `.tar.gz` suffix after the `X`s made busybox fail with `Invalid argument`, so every in-container update aborted before downloading. The same failure class existed in the nightly startup script's self-update path.

**PR #406 — busybox-safe staging in `urnet-tools update`** (`docker/scripts/urnet-tools.sh`):
- Replaced the broken `mktemp /tmp/urnetwork-update-XXXXXX.tar.gz` file template with one busybox-safe temp DIR: `mktemp -d /tmp/urnetwork-update-XXXXXX`, tarball placed inside as `update.tar.gz`.
- One cleanup path (`rm -rf "$tmpdir"`) removes everything on success or failure. Previously `rm -f "$tarball"` on download failure left the dir; now every path cleans up.
- `update` and `idle-update` both route through `do_update`, so both commands are covered.
- Test harness (`scripts/test_docker_update_tarball.sh`) hardened with a busybox-enforcing `mktemp` stub that rejects any template not ending in `XXXXXX` (GNU host mktemp accepts a suffix, so a regressed non-tarball template previously passed silently). New coverage: zero-byte tarballs, partial downloads overwritten by mirror, `update-pending` marker lifecycle (created on success, removed when the final `mv` fails, respects custom HOME), architecture mapping, busybox stub regression guard.

**Nightly self-update path hardening** (`docker/scripts/start_nightly.sh` `func_check_update`):
- The nightly path had the same update-failure class: reused a shared `/tmp/urn_update` dir (no per-attempt isolation), downloaded with `curl -sL` (no `-f`, so a bad HTTP response was written as an archive and mistaken for success), had no cleanup trap, and touched `update-pending` BEFORE the download/extraction succeeded — a failed update left the restart loop believing a new binary was installed.
- Now stages the whole update inside one busybox-safe `mktemp -d /tmp/urnetwork-update-XXXXXX` dir; `curl -sfL` fails on any HTTP error; every failure path (download, extract, missing binary, install) cleans the temp dir, logs `Update aborted; existing provider left untouched`, and returns without touching `update-pending`.
- The marker is written only after the binary swap and version write succeed. The `pkill` that restarts the provider runs only after the new binary is in place (previously it killed the provider before the download even started).
- Dead `TMP_DIR="/tmp/urn_update"` variable removed.

**Verified**: end-to-end in a real Alpine container. The broken template reproduces `mktemp: : Invalid argument`; the fixed `urnet-tools update` downloads the real release, swaps the provider binary, sets the marker, and reports the new version. The nightly function was exercised with a mock curl/tar across four cases (success, download fail, extract fail, metadata fail): success swaps binary + sets marker with zero temp-dir leftovers; every failure leaves the old binary and no marker, with no leaked temp dir.

**How to Identify in New Upstream**:
- Search for `mktemp /tmp/urnetwork-update` in `docker/scripts/urnet-tools.sh` and `start_nightly.sh`. The busybox-safe form is `mktemp -d /tmp/urnetwork-update-XXXXXX`.
- The nightly `func_check_update` should touch `update-pending` only after `mv` of the new binary succeeds.

## 128. Loopback-Only Diagnostics + Observability Metrics (PR #423, #424, #425)

**Purpose**: Give operators a single loopback-only place to inspect a running provider -- Go pprof CPU/heap profiles and process metrics -- without exposing anything on the public status port. Two earlier PRs (pool metrics, error tracking) were reviewed as a batch and consolidated into one PR (#425) after a big-picture review found their first form put internal metrics on the unauthenticated public status server.

**PR #423 -- loopback-only diagnostics server** (`profiling.go`, `provider/main.go`):
- `connect.EnableProfiling(addr)` starts an HTTP server that refuses any non-loopback bind (validates the host is a literal loopback IP via `net.ParseIP(...).IsLoopback()`).
- Serves `/debug/pprof/*` (index, cmdline, profile, symbol, trace).
- Opt-in, off by default: enabled only when the `URNETWORK_PPROF` env var is set to a `host:port`. Documented in `docs/Configuration.md` under Monitoring & Telemetry.

**PR #425 -- consolidated pool metrics + error tracking** (`message_pool.go`, `error_tracking.go`, `profiling.go`, `provider/main.go`):
- Message pool metrics: `PoolMetrics` atomics for hits/misses/returns/active buffers and a per-size distribution. The size-distribution map is created lazily so an unknown pool size cannot nil-deref on first use. `ActiveBuffers` is decremented on every `Put` (pooled or discarded) so the gauge does not drift upward.
- GC pauses are read via `runtime/metrics` (`/gc/cycles/total`), not `runtime.ReadMemStats`, matching the fork's existing lock-free, no-stop-the-world metric style, and read synchronously on pull so no background goroutine runs for binaries that never query metrics.
- `EnhancedMetrics()` returns a JSON snapshot: hits, misses, returns, active buffers, live pooled capacity, GC cycles, size distribution, last reset time.
- Error tracking: `RecordError(category, msg)` records a rate-limited, categorized recent-error buffer (transport/ip/proxy/webrtc) with a truncated stack capture. Rate limiting reuses the fork's existing `logThrottle` (lock-free, O(1)) rather than a bespoke limiter. The trim path is bounded by length (`len()`), fixing a cap()/len() panic the original implementation had. `ErrorMetrics()` returns copies of the buffer, so concurrent records cannot race with serialization.
- Both `/metrics/pool` and `/metrics/errors` are served on the loopback-only diagnostics listener from PR #423, alongside `/debug/pprof/*`. They are NOT on the public status server.

**PR #424 -- Docker build layer caching** (`Dockerfile`, `.dockerignore`):
- Builder stage copies `go.mod`/`go.sum` and runs `go mod download` first as a separate cacheable layer, so dependency downloads are only invalidated when the module manifests change, not on every source edit.
- Expanded `.dockerignore` to keep docs, res, and scratch files out of the build context.

**Verified**: unit tests pin the metric shape (including unknown-size pools), error buffer, rate limiting, and trim path; race-clean under `-race`; full CI (test-and-lint, build-and-push, CodeRabbit) green.

**How to Identify in New Upstream**: `profiling.go` (loopback diagnostics) does not exist upstream. `message_pool.go`'s `EnhancedMetrics`/`globalPoolMetrics` and `error_tracking.go` are fork-only. The `URNETWORK_PPROF` env var and the `/metrics/pool` + `/metrics/errors` routes on the loopback listener are fork additions.


## 129. Adaptive GC Consolidation (PR #428)

**Purpose**: Remove the separate runtime eco memory monitor and fold its host available RAM signal into one consolidated adaptive GC governor that lives in the pressure monitor. This makes the Go GC percentage knob single-writer for the whole process.

**Files Modified**: `provider/resource_pressure.go`, `provider/main.go`

**Change**:
- `runEcoMemoryMonitor` was deleted from `provider/main.go`. Its host available RAM signal now feeds the consolidated `gcGovernor` inside `runPressureMonitor` in `provider/resource_pressure.go`.
- The governor is the only writer to `debug.SetGCPercent` for the process. The old two-writers hazard (eco monitor plus static profile tuning) is gone, and the mode-split that restricted the eco monitor to small/eco boxes is removed. The governor applies to all profiles (baseline no-profile, auto Tier 1-4, turbo, eco).
- It merges process heap fraction and host available RAM and takes the tighter of the two. It only ever lowers GOGC below the captured baseline; it never raises it. Levels: `min(baseline, 50)` at heap >= 0.70, `min(baseline, 25)` at >= 0.80, and `min(baseline, 10)` plus `FreeOSMemory` at >= 0.92. Host available RAM: pressure <= 300 MiB, critical <= 150 MiB.
- A 10s heap subtick (`/gc/heap/live:bytes`) reacts to heap spikes faster than the 30s sweep, which merges the heap and host-RAM signals.
- Kill switch: `URNETWORK_ADAPTIVE_GC` set to `0`, `false`, `off`, or `no` disables the governor. It is on by default. If the operator sets `GOGC`, the governor backs off entirely and never touches the knob.
- The `~/.urnetwork/pressure_status` file now also reports `gc_state` and `heap_frac`.
- The `eco` profile still exists and still applies its static startup tuning (`applyEcoSettings` sets baseline GOGC 50 and 75% RAM GOMEMLIMIT). Only the separate runtime eco-monitor loop is retired. The consolidated governor may tighten the eco baseline further at runtime.
- Retired log lines: the `[eco] memory pressure` lines are gone, replaced by `[proxy][pressure] gcGovernor ...` lines.

**How to Identify in New Upstream**: `runEcoMemoryMonitor` no longer exists in `provider/main.go`. The adaptive GC logic lives in `provider/resource_pressure.go` as `gcGovernor`, `gcGovernorState`, and `runPressureMonitor`. The `URNETWORK_ADAPTIVE_GC` env var and the `gc_state`/`heap_frac` fields in the pressure status file are fork additions.

**Status**: Part of PR #428 on the `feat/adaptive-gc-consolidation` branch. Not yet shipped to a release. Tests cover the single-writer governor and the kill switch.

## 84. Go 1.27 Toolchain Bump

**Purpose**: Move the compiler from Go 1.26.4 to 1.27.0.

**Files Modified**: `go.mod`, all `.github/workflows/*.yml` (`go-version` pins), `Dockerfile`, `hub/Dockerfile` (builder bases), `README.md`, `docs/Project-Structure.md`.

**Change**:
- `go.mod` `go` directive changed from `1.26.4` to `1.27.0`.
- CI `go-version` pins floated from `1.26` to `1.27` across all workflows.
- Docker builder base `golang:1.26-alpine` updated to `golang:1.27-alpine`.

**Status**: Validated on stock Go 1.27.0. Build, vet, the full `-short -race` test suite, the cross-compile matrix, and a functional smoke are all green. `go mod tidy` produced no dependency changes. The dev-only custom `greenteagc`/`nodwarf5` toolchain is not shipped because `release.yml` builds stock Go.
