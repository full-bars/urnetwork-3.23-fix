# URNetwork 3.23-fix Fork — Custom Changes

This document tracks all modifications made to the upstream URNetwork v3.23 codebase in this fork. Use this as a reference when rebasing to newer upstream versions.

**Fork Based On**: urnetwork/connect v3.23  
**Repository**: github.com/full-bars/urnetwork-3.23-fix  
**Current Version**: v3.23.0-fix.15.5-dev

---

## 1. Enhanced Logging — `[net][s]select` Visibility

**Purpose**: Make provider throughput observable in logs without `-v` (debug mode). Critical for real-time monitoring and testing.

**Files Modified**: `net_http.go`

**Change**:
- Log level: `[net][s]select` serial-select messages promoted from `Debug(2)` → `Info()`
- **Effect**: One log line per successful connection, visible in standard log output
- **Impact**: Enables real-time traffic observation (critical for warmup testing, outage monitoring)

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
  - `ContractTransferByteSeqScale`: 4 → 2 (reaches full contract size in 2 contracts)
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
- **`URNETWORK_PROFILE=auto`**: Opt-in profile that selects one of three tiers:
  - **Tier 1 (Low, <1.2GB)**: 128KB contracts, 32 seq buffers, 128KB TCP window, 512KB WebRTC.
  - **Tier 2 (Balanced, 1.2-3GB)**: 256KB contracts, 128 seq buffers, 512KB TCP window, 1MB WebRTC.
  - **Tier 3 (Perf, >3GB)**: 2MB contracts, 256 seq buffers, 1MB TCP window, 4MB WebRTC.
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

**Last Updated**: 2026-06-02  
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
