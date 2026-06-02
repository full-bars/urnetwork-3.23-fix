# Changelog

All notable changes to this project are documented here.

---

## [Unreleased]

### Added
- **Active Connection Counter**: Added `connections=N` to the `[health]` heartbeat log. This provides real-time visibility into the number of active TCP and UDP proxy sessions directly from the standard output.

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
