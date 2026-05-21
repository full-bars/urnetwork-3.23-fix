# Changelog

All notable changes to this project are documented here.

---

## [Unreleased]

- `urnet-tools update` now self-updates from GitHub instead of reinstalling the current version
- Auto-update default interval changed from daily to weekly (Sunday midnight UTC)
- `urnet-tools update` no longer stops a running provider — binary is updated on disk and the user is prompted to restart when convenient
- Added `URNETWORK_RAMLOGS` environment variable to Docker documentation
- Added log rotation defaults (`--log-opt max-size=10m --log-opt max-file=3`) to sample Docker run command
- Added RAM Logging section to README explaining mutual exclusivity with `--log-opt`

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
