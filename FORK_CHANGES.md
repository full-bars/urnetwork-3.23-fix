# URNetwork 3.23-fix Fork — Custom Changes

This document tracks all modifications made to the upstream URNetwork v3.23 codebase in this fork. Use this as a reference when rebasing to newer upstream versions.

**Fork Based On**: urnetwork/connect v3.23  
**Repository**: github.com/full-bars/urnetwork-3.23-fix  
**Current Version**: v3.23.0-fix.26.5

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
- `ip_security_cfaa_block.go` — Packed binary-search IP blocklist (64131 IPv4 ranges + 214 IPv6 ranges). Replaces 66K-line `map[[4]byte]bool`.
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

**Status**: Open in PR #288 (branch `feat/choose-network`), not yet merged.

**Status**: ✅ Merged `main` (2026-07-07). PR #227. v3.23.0-fix.25.4. Startup-triggered refresh (fires on first invocation, since no `jwt_last_refresh` file exists yet) confirmed working in production. The 7-day periodic path has not yet been separately observed completing a cycle.

---

## 74. proxy.state Ghost-Entry Pruning (PR #305)

**Purpose**: Fix a production incident where `proxy.state` accumulated entries for proxies that had been removed from the config/source but never got pruned from state, growing without bound and causing `proxy remove-dead` to re-report the same removals forever.

### 74a. Root Cause

`ProxyReloader.reload()` in `provider/proxy_reload.go` computed `removed` as `running ∖ desiredSet` — the set of addresses that were both currently running (present in the live `cancelMap`) and no longer desired. Only those addresses had their `state.Proxies` entry deleted. A dead/offline proxy's goroutine has usually already exited by the time an operator runs `proxy remove-dead` against it, so it was never in `running` to begin with — its ghost entry in `proxy.state` was never reachable by the existing prune logic, regardless of how many times `remove-dead` "removed" it.

Confirmed on a production node (v3.23.0-fix.26.4): `remove-dead` correctly shrank the config from ~1020 to 298 servers and live auth-failure churn stopped, but `proxy.state` retained 857 entries, 711 of which existed in neither the config nor the running set.

### 74b. Fix

After `desiredSet` is fully computed (config/file source merged with the URL cache), `reload()` now prunes `state.Proxies` to exactly that set, in addition to the existing running-diff removal loop (which still handles draining active sessions gracefully). Safe for URL-sourced proxies mid give-up-backoff, since `mergeProxyURLCache` keeps them in the URL cache — and therefore `desiredSet` — for their whole backoff window; only explicit eviction/blacklisting removes them.

A follow-up review finding was addressed before merge: if `proxy_url.json` fails to read for a given reload cycle (corrupt file, transient I/O — not the normal "no URL sources configured" case, which returns an empty cache with no error), `desiredSet` would silently exclude every URL-sourced address for that cycle. The prune pass now tracks whether the URL cache loaded successfully and skips pruning entirely if it didn't, so a transient read failure can't wipe state for still-desired URL proxies.

### 74c. Files Changed

- `provider/proxy_reload.go` — desired-set-wide prune pass, `urlCacheLoaded` guard, `pruned` count in the reload summary log line.
- `provider/proxy_reload_test.go` — `TestReload_PrunesGhostStateEntries_NotRunningNotDesired`, `TestReload_PreservesBackoffURLProxyState`, `TestReload_SkipsPruneOnURLCacheReadFailure`.

**Status**: ✅ Merged `main` (2026-08-02). PR #305. v3.23.0-fix.26.5. Validated on a live test deployment (500 URL-sourced proxies): an explicit `remove-dead` run pruned exactly 14/14 with zero left behind, and a spontaneous give-up/eviction cycle was separately observed pruning 7 stale entries on its own before `remove-dead` was ever invoked.
