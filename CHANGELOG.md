# Changelog

All notable changes to this project are documented here.

---

## [Unreleased]

### Fixed
- **URL-sourced proxy give-up backoff is now enforced at launch** (PR #133): the escalating backoff (PR #129) computed a 15m→24h retry delay and scheduled a one-shot `time.AfterFunc` reload, but nothing gated relaunch, `reload()` re-added every desired-but-not-running proxy immediately (checking only `isDraining`). Since every give-up and URL refresh triggers a reload, dead proxies were relaunched every few minutes regardless of their schedule (live data: proxies reaching give-up 9 within 44 minutes of uptime, 70% of log volume). `proxyFailureHistory` now records a per-address next-eligible time on each give-up, `reload()` skips addresses whose window has not elapsed and reports `N deferred (backoff)`, and `Reset`/`Prune` clear the window alongside the failure/give-up counters. Frees auth-rate-limiter slots for live proxies in addition to cutting churn.
- **`urnet-tools -v` reports running vs on-disk version** (PR #132): `show_version` ran the on-disk binary's `--version`, so right after a plain (non-forced) `update` it reported the freshly-installed binary while the old image kept serving traffic, with no hint a restart was pending. It now resolves the running version through `/proc/<pid>/exe` (accurate even after the file is renamed to `.old` or deleted), prints separate **Running version** and **Installed on disk** lines, and warns with the restart command when they drift, including when the running binary predates the `--version` flag (detected via a `(deleted)`/`.old` exe link).

---

## [v3.23.0-fix.24.10] — 2026-06-24

### Added
- **Per-minute earning windows** (PR #121): New `runEarningWindows` goroutine emits `[earn] billable_1m=X billable_5m=Y billable_15m=Z billable_60m=W active=yes|no` every 60 seconds, decoupled from the ~5-minute health heartbeat. Operators see earning changes within 1 minute instead of waiting for the next heartbeat tick. Rolling 60-minute ring buffer with counter-reset guard for proxy restarts. Partial windows displayed during warmup.
- **URL-sourced proxy give-up backoff and eviction** (PR #129): Replaces the flat 15-minute give-up-to-retry delay with an escalating per-address backoff (15m→30m→1h→2h→4h→8h→16h→24h, +20% jitter). Permanently evicts addresses after 10 give-up cycles via a persisted blacklist in `proxy_url.json`, so a hopeless proxy can never re-enter the auth-rate-limiter lottery across restarts. Fixes a concurrent bug where `Prune` silently wiped give-up/failure counters during the wait window by using the full desired address set (file/internal + URL cache) instead of the live health registry.

### Fixed
- **`paceMonitor` goroutine now exits after warmup completes** (PR #122): The `✓ warmup … done` message was being re-emitted every 30 seconds indefinitely because the ticker loop had no terminal state. Added `return` after the done log line — the goroutine now exits as soon as `pct > 90 && connectingN < 5`, printing the completion line exactly once.
- **File-based proxies now start before URL-sourced proxies on boot** (PR #127): `runProxyURLFetcher` and `runProxyURLCleanup` goroutines were launched at line 1583, before the proxy-file reloader was ready — URL proxies would race ahead of operator-curated file proxies. Moved both goroutine launches to line 2056, after `reloader.StartWatcher(ctx)` ensures file proxies are loaded first.

---

## [v3.23.0-fix.24.7] — 2026-06-23

### Fixed
- **Operator-curated proxies never give up on auth** (PR #119): File, internal, and direct proxies that exhaust `maxAuthFailures` now switch to a slow capped retry (5m → 10m → 15m with jitter) instead of going permanently offline. URL-sourced proxies retain the short leash — they give up and the `time.AfterFunc` requeue brings them back after 15 minutes.
- **URL requeue decoupled from reload engine** (PR #120): Replaced the per-proxy `scheduleGiveUpRequeue` goroutine (15m sleep + cooldown map + trigger write + 30s recheck) with a single `time.AfterFunc` per proxy. Deleted the entire `proxyGiveUpCooldown` map/mutex/functions (~110 lines). `reload()` now acquires `proxy.lock` before proceeding, serializing hot-reloads with cross-process operations like `proxy remove-dead` and URL source fetches.

---

## [v3.23.0-fix.24.5] — 2026-06-22

### Fixed
- **SOCKS5 probe no longer runs on file/internal proxy lists**: The pre-auth SOCKS5 probe was running on every proxy, including operator-curated paid lists from proxy files. File-based and internal proxies now skip the probe and go straight to auth. URL-sourced proxies added via hot-reload are still probed.

---

### Performance
- **Raised default throughput ceilings** (`transport.go`, `ip.go`, `tuning.go`, `transfer_contract_manager.go`):
  - TransportBufferSize 1 → 16 (removes in-flight message bottleneck between framer and WebSocket writer)
  - TCP/UDP MaxWindowSize 1 MiB → 4 MiB (removes ~160 Mbps per-connection throughput ceiling at 50ms RTT)
  - applyTier3 now sets actual performance values for `URNETWORK_PROFILE=auto` on 4GiB+ nodes (4 MiB TCP window, 256-depth IP buffers, 4 MiB transfer queues)
  - ContractTransferByteSeqScale 4 → 2 (reaches 128 MiB standard contract in 2 sequences instead of 4)
- **Relaxed client-side auth rate limiter** (`provider/auth_rate_limiter.go`):
  - Default min 1 → 20 req/s, max 10 → 200 req/s, burst 3 → 50
  - Added `URNETWORK_AUTH_UNLIMITED=true` env var to bypass the limiter entirely
  - Server-side ConnectionRateLimit already caps auth connections; the client limiter was serializing fleet warmup unnecessarily
- **CPU-scaled MultiRaceClientCount** (`ip_remote_multi_client.go`):
  - Races more providers per connection based on available CPU cores (4-12 instead of fixed 2)
  - 1-2 cores → 4, 3-4 cores → 6, 5-8 cores → 8, 9+ cores → 12
- **Dynamic ContractFillFraction based on RTT** (`transfer.go`, `transfer_rtt.go`):
  - Fill fraction adapts to observed RTT: 0.85 at low RTT (≤100ms) → 0.50 at high RTT (≥1000ms)
  - Prevents pipeline stalls on high-latency links while filling closer to capacity on fast links
- **Sharded packet dispatch** (`ip.go`):
  - Single-goroutine dispatch loop replaced with N shard goroutines (one per CPU, capped at 16)
  - Packets routed via deterministic FNV-1a flow hash of IP 4-tuple for per-flow affinity
  - Independent buffer instances per shard eliminate dispatch CPU bottleneck on multi-core nodes

### Added
- 11 new unit tests covering MeanRtt, computeFillFraction, flowHash, pickShard, auth unlimited mode, and CPU-scaled race count

### Changed
- Updated FORK_CHANGES.md with sections 43-47
- Updated progress.md with comprehensive codebase analysis findings

---

## [v3.23.0-fix.24.1] — 2026-06-21

### Fixed
- **MultiRaceClientCount set to 16 unconditionally**: Replaces the CPU-based tier system with a simple constant — 16 racers on all platforms. The race is double-bounded at runtime by actual healthy provider count and per-flow packet budgets, so the value acts only as a ceiling. Higher values on single-core nodes were previously held back by a conservative CPU heuristic that had no basis in the actual resource cost (goroutines are I/O-bound, not CPU-bound).

### CI
- **Docker build depends on test success**: `build-and-push` now `needs: test-and-lint`, saving ~3 min of Docker build compute on every failing PR.
- **Go module cache restored**: Re-added `~/go/pkg/mod` cache alongside existing build cache. Saves ~30s per run.
- **Test order optimized**: Go tests (with `-race`) now run before shell installer tests for faster failure feedback.

---

## [v3.23.0-fix.23] — 2026-06-20

### Added
- **SOCKS5 handshake probe**: The per-proxy TCP connect probe now performs a full SOCKS5 handshake (greeting + response) instead of a bare TCP connect, eliminating false positives from hosts that accept TCP but aren't functioning SOCKS5 proxies.
- **Bounded auth concurrency**: In-flight auth attempts are now limited to 5 via a concurrency semaphore, preventing resource exhaustion on large proxy lists.
- **Rate limiter heartbeat**: When the adaptive auth rate limiter is pinned at its floor (1 req/s), a `[proxy][authrate]` heartbeat line is logged every 60 seconds so operators can see the limiter is actively engaged.
- **Contract logs no longer rate-limited**: Contract lifecycle events (`[contract] acquired`, `[contract] denied`, `[contract] oob`) are now logged unconditionally for complete auditability.
- **`[traffic]` total with earning signal**: The `[traffic] total` line now includes `earning=yes|no` and `billable_today=X` fields for immediate visibility into whether a proxy is generating earnings.
- **Failure count resets on successful auth**: Per-proxy failure counters are reset to zero when a proxy successfully authenticates, preventing historical failures from skewing health tracking.
- **Non-429 retry delay scales with attempt**: Transient errors (non-429) now also scale their retry delay with attempt number (capped at 60s), instead of using a flat jitter.
- **Unproven overload streak decays gracefully**: The overload streak counter for unproven proxies now decays over time rather than clearing fully, preventing rapid re-triggering of backoff.
- **Give-up requeue on reload**: Proxies that gave up due to exhaustion of auth retries are re-queued for retry if a hot-reload cycle doesn't pick up their address (via file or URL source change).
- **RAMLOG ring buffer**: The in-memory log buffer now uses a ring-buffer design that keeps the newest ~3.3 MB and discards the oldest ~1.7 MB when full, rather than a simple truncation.
- **ControlPingTimeout enabled (30s keepalive)**: Active keepalive pings on control-plane connections detect silent drops within 30 seconds.
- **Stale proxy.lock detection**: On startup, the provider detects and cleans up stale `proxy.lock` files left behind after a crash.
- **Overlapping fetch cycles prevented**: Concurrent `PROXY_URL` fetch cycles for the same URL are now skipped with a log warning.
- **CLI commands now lock properly**: All CLI management commands (`proxy refresh`, `proxy remove-dead`, etc.) acquire the proxy lock to prevent concurrent operations from corrupting state.
- **Memory leak fix (failure history + proven set pruning)**: Internal failure history and proven-set data structures are periodically pruned of stale entries, preventing unbounded heap growth.
- **Custom HTTP client for URL fetches**: URL fetch subsystem uses a dedicated HTTP client with 30s connect / 60s response timeouts, isolated from the provider's control-plane transport.
- **Admission gate slot leak fixed**: All error return paths in the admission gate now properly release acquired slots, preventing gradual exhaustion of the admission budget.
- **Proxy URL Sources**: Point the provider at a live proxy list URL (`--proxy_url` / `PROXY_URL`, comma-separated for multiple sources) instead of, or alongside, a static `proxy.txt`. Fetches on an interval (default 15m), merges new entries into the existing hot-reload pipeline without disturbing already-warmed-up proxies, and supports scoped automatic dead-proxy cleanup (`--proxy_dead_cleanup_scope=url|all|none`) so a noisy public list can self-prune without touching hand-curated entries. See [Proxy URL Sources](docs/Proxy-URL-Sources.md).
- **Hot-reload added-proxy visibility**: When a hot-reload adds proxies, the provider now prints the same `Using N proxy servers:` + per-proxy listing used at startup, instead of only logging removals. Makes it trivial to confirm a `--proxy_file` or `--proxy_url` reload actually picked up new entries.
- **Global adaptive auth rate limiter**: All proxy auth attempts (first tries and retries alike) now funnel through a single shared, self-tuning rate limiter instead of relying on uncoordinated per-proxy backoff. The limiter starts at the believed safe ceiling, halves on a 429 from the auth API, and creeps back up after a sustained run of clean attempts — bounding aggregate load on a fragile upstream API without serializing startup of large proxy fleets.

### Resilience
- **Proactive JWT refresh**: JWT tokens now refresh automatically every 7 days (regardless of expiry), ensuring tokens never expire under normal operation. Includes 48-hour expiry fallback as safety net if periodic refresh fails. Startup jitter (0-9 minutes) desynchronizes fleet refresh attempts. Works across all auth modes (`BUILD=jwt`, `BUILD=stable`, `BUILD=nightly`) via JWT-to-JWT renewal. Last refresh timestamp persisted to disk to survive provider restarts.

### Security
- **QUIC Memory Exhaustion vulnerability**: Bumped `quic-go` to `v0.59.1` to resolve a vulnerability where an unauthenticated remote attacker could cause excessive memory allocation during the handshake.

### Telemetry
- **Increased RAMLOGS size**: Capacity expanded from 1MB to 5MB for larger diagnostic windows on high-volume nodes.
- **Enhanced `logs` command**: `urnet-tools logs` now supports `all`/`full` (stream entire buffer) and `dump` (save current buffer to `~/urlogs.txt`).
- **Docker CLI parity**: `urnet-tools` is now natively available inside the container, allowing operators to use the same management commands across all deployment types.

### Fixed
- **Proxy refresh status check failure**: Fixed a bug where `urnet-tools proxy refresh` failed with `FATAL [exit 51]: provider does not appear to be running` on first startup with 0 proxies. The provider now unconditionally writes the `proxy.state` file at startup and heals zero timestamps during heartbeat execution.
- **HTML error pages logged verbatim**: Auth failures that returned an HTML error body (e.g. a 429 from a rate-limiting proxy in front of the API) used to dump the entire page into the log. Now collapsed to `<html error page, N bytes>`.
- **429 auth retries used the same flat backoff as ordinary errors**: A rate-limited auth attempt now waits proportionally longer on each subsequent retry (capped at 60s) instead of the same flat 0.5–10.5s jitter used for transient network errors — so a batch of proxies hitting 429s together backs off instead of immediately re-hammering the API.
- **Sub-hour retry durations logged as `0h Nm`**: Duration formatting now omits the redundant hours segment when there are none, e.g. `15m` instead of `0h 15m`.
- **URL-sourced proxies stuck after exhausting auth retries**: A proxy whose addresses came from `--proxy_url` now automatically retries 15 minutes after giving up, instead of waiting for an hourly pulse that only file/manually-added proxies receive.
- **Installer/update JSON parsing broke under `dash`**: `Provider_Install_Linux.sh` passed raw GitHub API responses to `jq`/`python3` via `echo`, which interprets backslashes natively under `dash` (Debian/Ubuntu default `/bin/sh`) and silently corrupted JSON containing escape sequences. Switched to `printf "%s"`, added a silent-failure fallback to the script's web-scraping path, and removed bashisms (`[[ ... ]]`) from the test suite so it runs cleanly under strict POSIX `sh`.

### Documentation
- **Production-ready Docker guide**: Added recommended deployment patterns to README for persistent telemetry and auto-tuning.

---

## [v3.23.0-fix.21.2] — 2026-06-16

### Fixed
- **Message pool Share/Return race**: When a buffer was returned to the pool while another goroutine was concurrently sharing it, there was a narrow window where the buffer could be handed out to a third goroutine before the share completed — silently corrupting in-flight packet data. Metadata reset is now performed under the lock.
- **Orphaned buffer leak in proto serialization**: `ProtoMarshalWithTag` grabbed a pool buffer based on an estimated size. If protobuf's actual output exceeded the estimate, the library allocated a fresh backing slice and abandoned the pool buffer without returning it. The call site now detects the reallocation and explicitly returns the orphaned buffer.
- **Hub report failures were silent**: Non-2xx responses from the hub now log `[report] hub rejected report: <status>` instead of silently succeeding.
- **`TestMessagePoolShare` was failing**: The assertion was checking against the old maximum bucket size, predating the pool's larger bucket additions (16 KiB, 32 KiB, 64 KiB). Fixed to reflect current pool structure.

### Added
- **CI full test suite**: Replaced hardcoded `-run` allowlist with `go test -short -race ./...`. New tests are discovered automatically and Go's race detector runs on every build.
- **Sibling-fork drift monitor**: Daily CI job checks `full-bars/connect` for new commits to critical files and posts a Discord alert.
- **Bandwidth reporter startup jitter**: Reporter now waits a random duration (up to one full interval) before first post, preventing thundering-herd on fleet restart.

---

## [v3.23.0-fix.21.1] — 2026-06-16

### Performance
- **O(1) proxy health lookup**: Rewrote `proxy_health.go` tracker to use an address-based pointer index map, eliminating global mutex contention and `O(N)` array scans on every bandwidth update.
- **Dial latency metrics**: Injected `dur=Xms` field into all `[net][s]select` logs for real-time per-strategy latency visibility.
- **`[earn]` utilization log**: Added `[earn] proxies_up=N serving=M idle=K clients=C` summary to the periodic heartbeat.

### Fixed
- **TLS session deadlock**: Resolved a lock-ordering inversion in `EncryptionSessionManager` where idle-reaping and concurrent TLS handshakes could deadlock the provider permanently. Refactored to a push-model architecture.
- **`[traffic]` unit suffix**: Fixed formatting bug where `[traffic] total` printed `MB/s/s` instead of `MB/s`.
- **Docker tag consistency**: CI now preserves the full git release tag string; `docker pull` with exact semver no longer triggers `manifest unknown`.
- **Atomic binary replacement**: `urnet-tools update` now uses an atomic `mv` to bypass "Text file busy" on live binary replacement.
- **[contract] acquired/denied log**: Added log signal for contract lifecycle visibility.

---

## [v3.23.0-fix.21] — 2026-06-16

### Added
- **`urnet-tools hub` commands**: `hub set <url>`, `hub off`, `hub install` — configure bandwidth hub reporting without manual systemd edits.
- **Hub dashboard polish**: Fixed UI state tracking on auto-refresh, added natural sort for all column types, persistent sort directions, billable traffic highlighting, delta-time division guard.
- **Per-proxy failure diagnostics**: Dead/degraded proxies in `[health][proxies]` now include inline failure breakdown (`auth:N`, `timeout:N`, `drops:N`, `last_err`).

### Fixed
- **Auth error message clarity**: Proxy timeouts now log `"network error reaching API"` instead of suggesting JWT expiry.

### Internal
- **Logger interface migration**: All 357 `glog.*` call sites across 21 core files migrated to the `Logger` interface, eliminating merge conflicts on upstream rebases.

---

## [v3.23.0-fix.20.1] — 2026-06-14

### Fixed
- **`urnet-tools update` resilience**: Gracefully falls back to version string comparison when API response parsing fails; no longer aborts with a misleading error.
- **Parser optimization** (upstream PR#185): Fast-path protocol buffer parser and streamlined 120-second contract expiry cleanup adopted from upstream.

---

## [v3.23.0-fix.20] — 2026-06-12

### Fixed
- **HMAC Contract Verification Format Migration**: Implemented dual-format HMAC verification to support the upstream platform's contract signing format migration on July 1, 2026. Providers now verify both legacy (pre-July 1) and standard (post-July 1) HMAC formats seamlessly, ensuring continuous operation through the platform cutover with zero performance impact.
- **Write Error Log Suppression (QUIC)**: Fixed write error log flooding in the QUIC (runH3) transport path. Both WebSocket (runH1) and QUIC (runH3) write errors are now rate-limited to one log message per minute globally, with suppression counts reported to reduce noise during backend outages.

---

## [v3.23.0-fix.18.4] — unreleased

### Added
- **Proactive JWT Renewal**: The provider now checks the JWT expiry once per hour. When the token is within 48 hours of its `exp` claim, it proactively calls the auth API for a replacement and writes it to disk — no restart, no exit-78 blip. The check runs immediately at startup and every hour after. If the API is temporarily unreachable, it retries on the next cycle.
- **shmLogFatal**: All fatal error paths now write a `FATAL [exit <code>]: ...` line directly to the ramlog file before terminating (bypasses the pipe goroutine, so the message is never lost to a race on exit). Also writes to stderr for Docker logs. Works regardless of whether ramlogs are enabled.
- **Unique Exit Codes**: Every failure path now has a documented exit code so operators can triage from the exit code alone. See `FORK_CHANGES.md#exit-code-reference` for the full table.

### Fixed
- **Ramlog Race on Fatal Exit**: When the provider exited with code 78 (expired JWT), the error message was written to the stdout/stderr pipe but the process often died before the ramlog goroutine could flush it to `/dev/shm/urnetwork.log`. `shmLogFatal` writes directly to the file, sidestepping the race entirely.

---

## [v3.23.0-fix.18] — 2026-06-07

### Added
- **Unified Proxy Telemetry**: Completely overhauled the proxy tracking system. "Total Tx/Rx" now reflects the exact raw bytes on the wire for ALL traffic types (H1, H3, and NAT), providing 100% accurate billing vs wire-usage transparency.
- **Dialer Session Tracking**: The `CLIENTS` counter and `clients=N` log field now track active connections at the dialer level. This ensures that internal provider heartbeats and platform transports are correctly reflected in the load metrics, resolving the "clients=0" confusion on idle/health-check nodes.
- **Proxy Session Timers**: Added tracking for connection longevity. A new "MAX AGE" column in the traffic report and an `age=...` field in logs help identify zombie or stuck connections across the fleet.
- **Docker Built-in Aliases**: Added `proxy-traffic` and `logs` commands directly to the container. Operators can now use `docker exec -it <name> logs` to instantly tail RAMLOGS (resolving the empty `docker logs` issue) and `proxy-traffic` for quick load checks.
- **JWT Smart Refresh**: Implemented local `exp` claim validation and self-healing shell logic. Providers now detect expired tokens before network calls, exiting with code 78 to trigger automatic re-authentication in entrypoint scripts.

### Fixed
- **NAT Bandwidth Visibility**: Fixed a bug where NAT session bandwidth was not being included in the "TOTAL (TX/RX)" columns of the traffic report.
- **Counter Double-Counting**: Removed redundant atomic increments across `ip.go` and `transport.go`, consolidating all session and wire-traffic tracking into a single atomic source of truth in the dialer layer.
- **Auth Panic Guard**: Replaced multiple `panic` calls in `provideAuth` with structured error returns. Added nil-guards for `authClientResult` to prevent crashes on malformed API responses.

---

## [v3.23.0-fix.17] — 2026-06-05

### Added
- **Proxy Hot-Reload**: Live add/remove proxies via `urnet-tools proxy refresh` and `urnet-tools proxy remove-dead` without restart. Proxy slots are now stable across reloads.
- **Provider Logs Command**: Added `urnet-tools logs` to stream current RAMLOGS buffer and tail live logs automatically.
- **Per-Proxy Bandwidth Tracking**: Tracks cumulative billable and total bandwidth per proxy, visible via `urnet-tools proxy traffic`. Survives RAMLOGS rotation.
- **Active NAT Session Tracking**: The `[net][s]select` logs and traffic report now include a `clients=N` field to track active NAT sessions multiplexed through each proxy.
- **E2E Post-Quantum Encryption**: Ported upstream PR #183 adding ML-KEM/Kyber hybrid encryption and hardened `CloseContract` delivery (disabled by default in this release).
- **Global Tool Versioning**: `urnet-tools` subcommands now accept a `-t <tag>` flag for pinning to specific versions.

### Fixed
- **[net][s]select Error Spam**: Implemented rate-limiting for `[net][s]select:` error logs during backend outages. Errors are now suppressed to one log line per minute with a suppression count.
- **Clients Counter Bug**: Fixed a regression from the rc.1 pre-release where the clients counter was not being copied into bandwidth snapshots, causing it to display as 0.

---

## [v3.23.0-fix.16] — 2026-06-02

### Added
- **Dead-Proxy Health Report**: The `[health]` heartbeat now emits `[health][proxies]` lines listing `dead` (never authenticated) and `degraded` (worked before, down now) proxies, plus `recovered`/`lost` and `lifetime_recovered`/`lifetime_lost` counters that make the hourly retry pulse's effectiveness visible. A `[pulse]` marker logs each retry sweep. Full dead/degraded lists and a transition history are mirrored to `proxy_health.state` and `proxy_health.log` on the config volume (survives RAMLOGS), readable via `urnet-tools proxy health` (host) or `proxy-health` (Docker).
- **Per-Proxy Health Tracking**: The `[net][s]select` log now includes the proxy index and IP address when running a proxy list (e.g., `proxy[42] (1.2.3.4:1081) [fragment] success=100 error=2`). This allows operators to easily identify and remove failing or "black hole" proxies from their deployment.
- **Active Connection Counter**: Added `connections=N` to the `[health]` heartbeat log. This provides real-time visibility into the number of active TCP and UDP proxy sessions directly from the standard output.
- **Active Proxy Counter**: Added `proxies=N` to the `[health]` heartbeat log, counting authenticated proxy transports currently live on the platform. Unlike `connections` (end-user NAT sessions), this reflects how many proxies from your list are actually working, so a node with no users still reports a non-zero value (e.g. `connections=0 proxies=1188`).

---

## [v3.23.0-fix.15.4] — 2026-05-29

### Added
- **Force Update Flag**: `urnet-tools update -f` (or `--force`) now bypasses the version check and re-downloads/reinstalls even if the installed version matches the available version. Useful when a release tag is re-tagged with updated binaries or for manual recovery.

---

## [v3.23.0-fix.15.3] — 2026-05-29

### Fixed
- **Log Spam**: Removed redundant "Reporting to dashboard" log that was emitted on every auth retry, causing noise during startup or API errors. The `client_id` and `instance_id` logs already signal successful provider startup.
- **[r]drop Rate-Limiting**: Implemented rate-limiting for `[r]drop` errors, now suppressed to 1 per minute globally with suppression count. Prevents log flood during backend timeouts (similar to existing `[t]auth` and `[contract]oob` suppression).

### Documentation
- **README Restructure**: Major overhaul of the README for better clarity and organization. Moved detailed technical guides (Installation, Docker, Scaling, Tuning, Configuration) into a dedicated `/docs` directory to keep the main page focused on essential information.

---

## [v3.23.0-fix.15.2] — 2026-05-28

### Documentation
- **README Standardization**: Overhauled all `docker run` and `docker compose` examples to include optimized sysctls and automatic hostname detection by default.
- **Improved Clarity**: Standardized container names to `urfix` and volumes to `${NAME:-urfix}` for safer copy-pasting. Corrected the environment variables table to reflect refined identity logic.

### Fixed
- **Dashboard Reporting**: Optimized identity logic to avoid redundant `IP @ IP` strings. If no name is provided, only the redacted public IP is reported.

---

## [v3.23.0-fix.15] — 2026-05-28

### Added
- **Installer Root Guard**: The Linux installer now detects if it is being run as root and offers an interactive menu to create a dedicated service user (`urnet`) with the correct permissions. This prevents "Failed to connect to bus" errors caused by root's lack of a user session bus.
- **Assisted User Setup**: Automatically handles user creation, admin group detection (`wheel` or `sudo`), and systemd lingering enablement across diverse Linux distributions.
- **Hardened User Hand-off**: Implemented a robust `runuser` mechanism that handles SELinux-enforcing environments (like openSUSE and AlmaLinux) by ensuring correct environment propagation (`XDG_RUNTIME_DIR`, `DBUS_SESSION_BUS_ADDRESS`) and directory transitions.
- **Automatic Server Hostname Reporting**: Containers can now automatically report the host's actual server name via the `HOST_HOSTNAME` environment variable (e.g. `-e HOST_HOSTNAME=$(hostname)`).

### Fixed
- **Dashboard Identity Format**: Refined the identity reporting format to strictly follow `Name @ IP [Version]`. Version strings are now consistently enclosed in brackets, and random 12-char hex container IDs are automatically hidden and replaced with "provider" to keep the dashboard clean.
- **Timezone Consistency**: Standardized the default timezone to `America/Tijuana` across all entrypoint scripts (`start_jwt.sh`, `start_stable.sh`, `start_nightly.sh`) for consistent log timestamps and update watcher timings.
- **Proxy Command Syntax**: Fixed a bug in `urnet-tools proxy add` where an extra argument shift caused the file path to be misidentified as a command.
- **Auto-Tune Log Spam**: `[tune] auto-profile` was logged once per proxy server on startup instead of once per process. With large proxy lists this produced thousands of identical lines. The log is now emitted exactly once; per-proxy settings application is unchanged.
- **Eco Monitor Duplication**: The eco memory monitor goroutine was started inside the per-proxy loop, spawning one monitor per proxy. Under memory pressure, all copies would log the same `[eco]` line and call `runtime.GC()` simultaneously. The monitor now starts exactly once per process regardless of proxy count.

### Documentation
- **User-Level Service Guide**: Updated README with instructions on the new recommended non-privileged deployment path.
- **Generic Server Naming**: Standardized documentation to use generic node references for privacy.

---

## [v3.23.0-fix.14.4] — 2026-05-28

### Documentation
- **Streamlined Multi-Container Scaling**: Documented the "Shared JWT" method for running three nodes in a single `docker-compose.yml` with one auth code and shared storage.
- **Improved RAM Logging Guide**: Added a comprehensive `docker run` sample command with all common flags.
- **Expanded Outage Alerting Guide**: Added a detailed `docker run` example for setting up Discord/Slack/ntfy webhooks.

### Added
- **Environment Variable Authentication**: Added support for `URNETWORK_AUTH_CODE`. This allows providing auth tokens (especially those starting with dashes) without command-line parsing issues in Docker.
- **Dashboard Identity Reporting**: All provider builds (JWT, Stable, Nightly, and Pelican) now automatically detect their public IP (via `ip.me`, with a 5s timeout) and report `NodeName @ redacted-IP [Version]` to the backend for easier identification. The IP is redacted to `first.x.x.last`. This is always on and requires no configuration; it is distinct from the opt-in `ENABLE_IP_CHECKER` diagnostic, which logs the full IP locally.

### Fixed
- **Shared-volume crash safety (jwt build)**: The provider restart loop no longer deletes the JWT after repeated crashes. In the shared-config multi-node model that would have deauthenticated the entire stack with no automatic recovery (the auth code is single-use). After repeated crashes the container now exits cleanly for Docker's restart policy to cycle it, leaving the session intact.
- **Restart loop reliability (jwt build)**: Fixed a `provide || true` pattern that made the crash counter always read success, so the in-script restart/backoff never engaged on a real crash.
- **Multi-node startup race**: The 3-in-1 scaling guide now uses a healthcheck on the first node plus `depends_on` so secondary nodes wait for the shared JWT instead of crash-looping on first boot.
- **Graceful ZRAM handling**: Systems with kernels that don't include zram support (e.g., Oracle Linux UEK) now complete `optimize` successfully. ZRAM is skipped with a simple warning; other OS optimizations (sysctl, ulimits) continue normally. Users on Ubuntu can optionally install Zabbly kernel to gain zram support.

## [v3.23.0-fix.14.2] — 2026-05-28

### Fixed
- **Update command reliability**: Fixed silent update failures on systems missing `jq` or `python3`. Update now fails loudly with clear instructions to install missing JSON parsing tools instead of reporting "up-to-date" when it couldn't verify version dates.

## [v3.23.0-fix.14.1] — 2026-05-28

### Fixed
- **Ubuntu 20.04→24.04 upgrade compatibility**: Hardened `setup_zram_manual()` fallback for systems where the zramswap service fails after distro upgrades. Now tries dynamic module allocation first, implements module reload recovery on disksize config failure, and adds lz4 compression fallback if zstd is unavailable. Adds sysfs permission checks and timing delays to prevent race conditions.

## [v3.23.0-fix.14] — 2026-05-27

### Added
- **Auto-Tune Performance Profile**: New `URNETWORK_PROFILE=auto` dynamically selects buffer sizes and contract floors based on available RAM (Low/Balanced/Performance tiers). Automatically enables Eco Mode on RAM-constrained systems and enables RAM Logging if slow disk I/O is detected. Managed via `urnet-tools auto on/off`.
- **System Optimizer**: New `urnet-tools optimize` command (requires root) to apply "Golden Fleet" network tuning to the host:
  - **Auto-Installation**: Automatically installs `conntrack-tools` and `zram` on supported distros.
  - **Interactive Protection**: Detects pre-optimized states and asks for confirmation before overriding (skip with `-f`).
  - **Boot Persistence**: Configures `/etc/modules-load.d` to ensure `nf_conntrack` loads early.
  - Ulimit bumped to 1,048,576.
  - Conntrack max raised to 2,097,152.
  - TCP established timeout reduced to 1 hour (from 5 days).
  - Enabled **BBR** congestion control and **Fair Queuing (fq)** for improved network throughput.
  - Expanded local port range and enabled TCP port reuse.
- **System Auditor**: Provider now checks OS limits (ulimit, conntrack) and performs a dynamic Disk I/O test on startup. Logs high-signal warnings for suboptimal host limits or low disk space.
- **Message pool auto-sizing**: `InitialMessagePoolByteCount` now scales to RAM/32 at startup (floor 8 MiB, cap 256 MiB) instead of the hardcoded 1 MiB default. The 1 MiB default is far too small for large proxy list deployments — almost every packet above the pool cap fell back to a fresh GC allocation, adding unnecessary GC pressure. Skipped for `lowmem` profile and when `--max-memory` is set explicitly. Logs `[pool] message pool NMiB (RAM=NMiB)` once at startup.
- **Health heartbeat**: logs `[health] uptime=X profile=Y heap=ZMiB sys=WMiB` every 5 minutes. Passive liveness confirmation and heap trend visibility without external tooling. Interval configurable via `URNETWORK_HEALTH_INTERVAL`.

### Changed
- **Outage detection and alerting**: a background watcher polls `IsBackendDegraded()` every 30 seconds and logs `[outage] backend degraded` / `[outage] backend recovered` on state transitions. Runs always, no configuration required.
  - Requires 10 consecutive failures (5 minutes) before firing `outage_start` to eliminate false positives from brief network blips.
  - If `URNETWORK_ALERT_WEBHOOK` is set, POSTs a JSON payload on each transition. Compatible with Slack, Discord, ntfy, etc. Webhook delivery is now non-blocking and handles Discord/Slack payload formatting automatically.
  - Requires two consecutive clean polls before firing `outage_clear` to avoid premature all-clears during brief mid-outage lulls.
  - Per-event 5-minute cooldown prevents webhook spam when the backend flickers at the recovery boundary.
- **RAM Log Tail Depth**: `urnet-tools logs` now displays the last 250 lines of history (up from 10) when in RAM logging mode.

### Fixed
- Fixed a potential panic in the health monitor when reading metrics on certain Go versions.
- `[turbo]` startup log now fires once at provider startup instead of once per proxy goroutine.
- `provide()` was missing a `ResizeMessagePools` call when `--max-memory` was set.
- **Watchtower Persistence**: `start_jwt.sh` now correctly reuses existing sessions, preventing "Invalid auth code" panics after image updates.
- **Installer date parsing**: Fixed Python 3.10 fromisoformat() error when parsing GitHub release dates with ISO 8601 `Z` timezone suffix.
- **Root installation handling**: Installer no longer exits on systemd enable failure when running as root in Docker containers (no user session bus). Warns gracefully instead.
- **Systemd lingering**: `urnet-tools optimize` now auto-enables lingering (`loginctl enable-linger`) for the detected user, ensuring systemd --user services persist after logout. The installer defers this to optimize (which already prompts for sudo if needed) to keep the install step zero-privilege.
- **Installer Robustness**: Fixed "No download URL" errors by implementing a robust `latest` tag resolution fallback (using GitHub redirects) and direct URL construction. This bypasses issues where the GitHub API returns malformed JSON.
- **Proxy Management**: Added `urnet-tools proxy add <file>` and `urnet-tools proxy clear` commands to simplify bulk proxy operations.
- **Robust ZRAM Deployment**: Implemented a "Universal Manual Fallback" for ZRAM that uses direct kernel commands (`zramctl`, `swapon`) if the distro-specific systemd service fails. Ensures ZRAM works reliably across all environments.

---

## [v3.23.0-fix.13] — 2026-05-27

### Fixed
- JWT is no longer deleted on every container start in `start_stable.sh`. The startup script now checks for an existing JWT at `/root/.urnetwork/jwt` and skips authentication entirely if one is found. This makes container restarts and Watchtower image updates seamless — the provider starts immediately without re-hitting the auth API. A persistent volume at `/root/.urnetwork` is required for this to survive container recreation.
- Auth failures in the provider binary no longer `panic` (which produced unreadable stack traces in Docker logs). They now print a clean error message to stderr and exit with code 1, allowing the shell restart loop to handle retries. Auth code failures include a hint about volume persistence.
- Provider binary now exits cleanly after 10 consecutive auth API rejections (expired or revoked JWT) so the shell restart loop can delete the JWT and re-authenticate. Previously the binary looped internally forever, making recovery impossible without manual intervention.
- Crash loop in `start_stable.sh` now calls `func_do_login` (not `func_check_credentials`) after clearing a bad JWT, so re-authentication actually runs.
- `urnet-tools logs` now correctly routes to `/dev/shm` when eco mode is active. Previously it checked for `lowmem` only, so eco users were tailed against journald (empty) instead of the RAM log.
- Auth error paths in `provider/main.go` (`os.UserHomeDir`, `os.MkdirAll`) converted from `panic` to clean `os.Exit(1)` with descriptive stderr messages.
- Auto-update timer no longer silently dead after install or reinstall. The install script was using `systemctl --user enable` instead of `enable --now`, so the timer was registered but never started. On long-running servers that hadn't rebooted, it never fired.

### Added
- Turbo mode (`URNETWORK_PROFILE=turbo-v4` / `turbo-v8`): raises the TCP Accordion window ceiling from 1 MiB to 4 MiB (V4) or 8 MiB (V8), removing the mathematical per-connection limit that existed because throughput is bounded by window/RTT.
  - Significantly higher theoretical ceilings for low-latency paths.
  - Transfer-layer resend and receive queues scale with the window (8 MiB for V4, 16 MiB for V8) so they don't become the new bottleneck.
  - IP and transfer goroutine buffer depths doubled (512 and 64 respectively).
  - WebRTC DataChannel buffer set to 2× window size per peer.
  - Contract ramp accelerated: `ContractTransferByteSeqScale` 4 → 2 (full speed in 2 contracts instead of 4).
  - GOGC raised to 200 with no GOMEMLIMIT — lets the heap breathe on RAM-rich boxes.
- `urnet-tools turbo <v4|v8|off>`: toggles turbo mode on the systemd provider service. Bare `urnet-tools turbo` prints current state.
- Docker `TURBO=v4` / `TURBO=v8` environment variable: single env var support for containers. The entrypoint translates it to `URNETWORK_PROFILE` before exec; GOGC is handled internally by the binary.

---

## [v3.23.0-fix.12] — 2026-05-26

### Fixed
- Raised `lowmem` mode initial contract size from 16 KiB to 256 KiB. The 16 KiB floor forced constant contract renegotiation that hurt throughput and earnings without meaningfully reducing RAM usage.

### Added
- Eco mode (`URNETWORK_PROFILE=eco`): GC-tuned memory profile for providers on RAM-constrained systems. Sets GOMEMLIMIT to 75% of detected RAM (cgroup-aware; reads cgroup v2/v1 limits so Docker `--memory` containers get the correct ceiling), enables `GOGC=50`, and leaves all buffers and contract sizes untouched so throughput and earnings are unaffected.
- `runEcoMemoryMonitor` goroutine: watches available memory every 30 seconds and dynamically tightens GC pressure when RAM is low — `GOGC=25` under pressure (<300 MiB available), `GOGC=10` at critical (<150 MiB) — then relaxes when it recovers. Hysteresis prevents oscillation. Inside Docker containers, uses cgroup headroom rather than host `MemAvailable` so pressure detection fires correctly.
- `urnet-tools eco <on|off>`: toggles eco mode on the systemd provider service. Merges eco-specific env vars into the existing override.conf rather than overwriting it, preserving other settings such as ramlogs.

---

## [v3.23.0-fix.11] — 2026-05-22

Productionizes the experimental work from the `v3.23.0-beta.1` pre-release (automated proxy recovery pulse and smart exponential backoff), folding it into main.

### Added
- Autonomous proxy recovery via an hourly global pulse (`pulse.go`). A pulse fired every 60 minutes wakes all goroutines blocked on `Pulse()`, so proxies stuck in exponential backoff recover without a provider restart.
- Exponential backoff (5s up to 1h cap) for parallel route selection; on pulse, failure counts and dialer health reset so blacklisted routes get a fair retry.
- Pulse integration and matching backoff for the P2P reconnect loop, which fix.10 did not cover.

---

## [v3.23.0-fix.10] — 2026-05-22

### Fixed
- Prevented bandwidth leak during backend API outages by gating retries when the backend is degraded:
  - Contract retry storm: `CreateContract` no longer launches API goroutines every 5s when degraded.
  - Transport reconnect storm: H1/H3 auth-error loops use exponential backoff (5s to 60s cap) instead of a near-instant retry timer.
  - Added `lastBackendFailNano` for accurate degraded-state detection without log-rate-limit flicker.
  - Degraded state clears immediately on successful reconnect rather than after a 60s timeout.

---

## [v3.23.0-fix.9] — 2026-05-21

### Fixed
- Limited resend amplification during backend outages (`MaxResendCount=16`, `MultiRaceClientCount=2`) so the resend queue can't grow unbounded when the API is unreachable.

### Added
- `urnet-tools update` now self-updates from GitHub instead of reinstalling the current version.
- Auto-update default interval changed from daily to weekly (Sunday midnight UTC).
- `urnet-tools update` no longer stops a running provider; the binary is updated on disk and you're prompted to restart when convenient.
- Documented `URNETWORK_RAMLOGS` and `URNETWORK_PROFILE=lowmem` in the Docker README, plus log rotation defaults and a RAM Logging section.

---

## [v3.23.0-fix.8] — 2026-05-20

### Fixed
- Rate-limited `[t]auth error` and `[contract]oob err` log spam during backend outages to one line per minute globally across all proxy instances
- A suppressed-count suffix (e.g. `(3,952 suppressed)`) is appended when the outage clears so no errors are silently dropped

---

## [v3.23.0-fix.7] — 2026-05-08

### Added
- Lowmode and RAM logging documentation added to README

---

## [v3.23.0-fix.6] — 2026-05-08

### Added
- `URNETWORK_RAMLOGS=1` environment variable — redirects provider logs to `/dev/shm/urnetwork.log` (RAM disk, 1MB cap) to eliminate disk I/O overhead
- `URNETWORK_PROFILE=lowmem` environment variable — enables RAM logging plus reduced buffer sizes and dynamic `GOMEMLIMIT` (85% of system RAM) for memory-constrained nodes
- `urnet-tools ramlogs on/off` command — toggles RAM logging independently of lowmode
- `urnet-tools lowmode on/off` command — toggles the full lowmem profile
- Dynamic `GOMEMLIMIT` calculation in lowmode (was previously a fixed value)

### Fixed
- ARM64 build failure caused by missing architecture-specific `dup2` implementation
- Release workflow and Dockerfile updated to build the full provider package correctly

---

## [v3.23.0-fix.5] — 2026-05-07

### Added
- One-liner install/uninstall commands in README
- Installation summary now shows technical improvements on first install
- Guidance for enabling systemd lingering so provider survives logout
- Auto-update timer cleanup on uninstall

### Fixed
- Auth error logging improved — errors are now visible without requiring verbose flags
- Panic on `proxy add` command prevented
- Various installer output and formatting improvements

---

## [v3.23.0-fix.4] — 2026-05-07

### Fixed
- Context cancellation return type corrected in provider
- Installer lingering instructions cleaned up

---

## [v3.23.0-fix.3] — 2026-05-04

### Added
- `CreateContractTimeout` increased from 30s to 60s to prevent stream drops during signaling spikes
- `ContractFillFraction` tuned from 0.8 to 0.7 for more headroom before contract exhaustion
- Custom installation script (`Provider_Install_Linux.sh`) with `urnet-tools` management suite
- Release workflow to build and publish provider binaries as GitHub release assets

### Fixed
- Docker image version now correctly reflects the build tag (was hardcoded to fix.1 in all images)
- Git tags are now fetched during CI builds so version extraction works correctly

---

## [v3.23.0-fix.2] — 2026-05-04

### Added
- Dynamic TCP window scaling (Accordion logic) — windows start at 4KB on idle connections and double up to 1MB under active throughput, then shrink back after 30s of inactivity

---

## [v3.23.0-fix.1] — 2026-04-30

### Added
- `InitialContractTransferByteCount` increased from 16 KiB to 256 KiB, eliminating the 13,107-byte effective capacity ceiling that caused excessive contract renegotiation and reduced earnings
- Expanded internal message pools (16KB, 32KB, 64KB) to reduce garbage collector pressure during high-throughput transfers
- IP buffer depth increased from 64 to 256 to absorb burst traffic without packet drops
- `[net][s]select` serial-select log promoted from Debug level 2 to INFO — one line per successful connection, visible without `-v`
- Multi-architecture Docker image (AMD64 + ARM64) with vnStat traffic monitoring integration
- CI/CD pipeline via GitHub Actions pushing to GHCR

---

## [v3.23-stock] — 2026-04-30

Baseline snapshot of the upstream URnetwork v3.23 provider before any modifications.
