# URNetwork 3.23-fix Fork — Custom Changes

This document tracks all modifications made to the upstream URNetwork v3.23 codebase in this fork. Use this as a reference when rebasing to newer upstream versions.

**Fork Based On**: urnetwork/connect v3.23  
**Repository**: github.com/full-bars/urnetwork-3.23-fix  
**Current Version**: v3.23.0-fix.13

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
InitialContractTransferByteCount: 16 KiB → 256 KiB
```
- **Old**: 16384 bytes per contract
- **New**: 262144 bytes per contract
- **Ratio**: 16x increase

**Effect**: Reduces contract renegotiation overhead during traffic ramp-up; faster throughput scaling.

**How to Identify in New Upstream**:
- Search for `InitialContractTransferByteCount` in `transfer_contract_manager.go`
- Look for constant or variable assignment (likely near contract initialization)
- Current value is `256 * 1024` (in bytes)

**Status**: ✅ Shipped in all releases; could be upstreamed if performance gains are universal

---

## 3. Log Spam Reduction — Rate-Limited Errors

**Purpose**: Prevent log explosion during outages or high-error conditions. Reduces noise while preserving diagnostics.

**Files Modified**: 
- Error logging in transport/auth layers (exact files depend on error type)

**Changes**:
- `[t]auth error` — Rate-limited (suppressed repeated occurrences)
- `[contract]oob error` — Rate-limited

**Example**: When auth fails repeatedly, logs show "X suppressed" instead of identical message spam.

**How to Identify in New Upstream**:
- Search for `[t]auth error` and `[contract]oob error` log patterns
- Look for "suppressed" or "rate" patterns in logging calls
- Check if glog has rate-limiting wrappers (e.g., `Infof` vs `Infof_Limited`)

**Status**: ⚠️ Partially shipped in v3.23.0-fix.8; upstream PR#180 covers some of this. Check upstream for completeness before re-applying.

**Outstanding**: `[r]drop` error spam still unaddressed in this fork — may want to add rate-limiting to it.

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

**Purpose**: Remove the ~100–150 Mbps per-connection throughput ceiling on RAM-rich servers. The ceiling exists because per-connection bandwidth is bounded by `MaxWindowSize / RTT` — at the 1 MiB default and a 10ms RTT that's ~100 Mbps. Turbo raises the window to 4 or 8 MiB and scales all dependent buffers accordingly.

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

**Theoretical speed ceiling**:
- V4 at 10ms RTT: ~400 Mbps / V8: ~800 Mbps (vs ~100 Mbps default)

**How to Identify in New Upstream**:
- If upstream changes `TcpBufferSettings`, `SendBufferSettings`, or `ReceiveBufferSettings` struct fields, verify `applyTurboSettings` still sets valid fields
- If upstream changes `WebRtcSettings`, verify `ReceiveBufferSize` field still exists
- `ContractTransferByteSeqScale` lives in `ContractManagerSettings` — verify path if contract manager is refactored

**Status**: ✅ Shipped in fix.13. Custom to this fork. Needs netem/Detroit testing before tuning values further.

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

**Last Updated**: 2026-05-27  
**Maintained By**: @full-bars  
**Contact**: Reference GitHub issues in urnetwork-3.23-fix repo
