# Changelog

All notable changes to this project are documented here.

---

## [Unreleased]

## [v3.23.0-fix.26.5] — 2026-08-02

### Added

**`urnet-tools idle-update`** (#301): Waits for billable traffic to drop below a threshold (default 5 KiB/s, `--threshold`) and stay there for a sustained window (default 5 min, `--window`) — verified with a second, tighter polling pass — before applying a pending provider update, instead of updating immediately and cutting off active sessions. `--window 0` updates immediately. Fails closed if `billable_rate` isn't available yet, and rejects `--threshold`/`--window`/rate values outside Bash's signed-int64 range.

**Cloudflare Worker sources versioned in-repo** (#304): `dl`, `dl-fullbars`, `geo`, and `provider-redirect` — previously dashboard-only or partially untracked — now live under `workers/` with their `wrangler.jsonc` configs and a `workers/README.md`. Deploys remain manual (`wrangler deploy`); nothing is wired into CI.

**`URNETWORK_HUB_TRUSTED_PROXIES`** (#302): New hub env var. `X-Forwarded-For`/`X-Real-IP` are only honored from configured trusted proxy IPs/CIDRs — previously ignored entirely (direct TCP peer was always used), which collapsed PAKE join rate-limiting into one shared bucket when the hub sat behind a reverse proxy.

**Upstream a2b144c port** (#298): `WindowType`/`OverrideAllowDirect`/PQE/peer-identity support ported from upstream `urnetwork/connect`.

**Tiered setup guides** (#297): Beginner/intermediate/advanced installation documentation.

### Fixed

**`proxy.state` ghost-entry accumulation** (#305): `ProxyReloader.reload()` in `proxy_reload.go` only pruned `proxy.state` for addresses that were both currently running and no longer desired. A dead/offline proxy's goroutine has usually already exited by the time it's removed (e.g. via `proxy remove-dead`), so it was never in the "running" set and its state entry was never deleted — accumulating forever and getting re-reported as removable on every subsequent `remove-dead` run. Confirmed on a production node: 722 proxies correctly removed from config/live fleet, but `proxy.state` retained 857 entries, 711 of which existed nowhere else. `reload()` now reconciles `proxy.state` against the full desired set (config/file + URL cache) instead. Guards against a related edge case: if `proxy_url.json` fails to read mid-cycle, the prune pass is skipped rather than deleting state for still-desired URL proxies over a transient error.

**Pooled-buffer leaks on backpressure/error** (#303): `tcpSend` could double-return a pooled packet on certain nil-sequence paths in `ip.go`. UDP backpressure drops and RST-buffer sharing are now leak-free under load; GCM seal count is capped per key.

**Node-bound hub credentials** (#302): v2 node credentials are now bound to their owning node — a valid credential for one node can no longer act on another node's data via `/api/report`, `/api/heartbeat`, or `/api/nodes/remove`. `onboard.sh`'s Host header is validated before being interpolated into the generated install script.

**Docker in-place update process handling** (#299): The update path now kills the correct process and respawns it properly.

**Cloudflare Worker hardening** (#304): `dl-fullbars`'s GitHub proxy now allowlists outbound headers instead of forwarding everything from the client; the `curl | sh` install dispatcher fails the install if the script download fails instead of silently running an empty script; `geo` worker's RTT fields use explicit null checks and the response is marked `Cache-Control: no-store`.

## [v3.23.0-fix.26.4] — 2026-07-18

### Added

**Degraded-proxy reaper** (#293): Background reaper runs every 3 minutes during sustained backend outages, ranks degraded proxies by lifetime contribution (traffic + contracts won), and cancels the worst-contributing half every cycle — while always keeping at least half alive so the fleet can detect recovery immediately. `direct`/native mode is permanently exempt. Re-verifies a proxy is still actually degraded immediately before cancelling it, since scoring/sorting thousands of candidates takes real time and the target could have reconnected (or been replaced by hot-reload) in that window.

**GOMEMLIMIT for turbo profiles** (#293): `turbo-v4`/`turbo-v8` now set a memory ceiling (80% of available RAM) when the operator hasn't set `--max-memory`/`GOMEMLIMIT` explicitly — previously `GOGC=200` with no ceiling at all, so nothing capped growth during an outage. Matches the safety net `eco` mode already had.

**`connect.IsDegraded(address)`**: New exported health check used by the reaper's stale-decision recheck; also usable standalone.

### Fixed

**TOCTOU tmpfile vulnerability** (#292): Provider update downloads now use `mktemp`-generated unpredictable paths instead of a hardcoded `/tmp/urnetwork-update.tar.gz`, closing a symlink-race (CWE-377) that could let a local user redirect the extraction to overwrite an arbitrary file. Provider binary installation is now staged and atomically replaced only on full success, with cleanup on any failure path.

**Download reliability** (#291): Provider updates route through `dl.fullbars.xyz` first with automatic GitHub fallback, plus two follow-up fixes in the same window — `curl` now fails on HTTP error responses (a missing `-f` flag meant the GitHub fallback never actually triggered on a bad primary response), and the Windows primary/mirror URL selection logic was corrected. Added `-y`/`-f` flags to skip the interactive confirmation on restart.

**Dashboard contract feed** (#294): Fixed a copy-paste bug where the "Recent Contracts" dashboard feed matched `"[contract] acquired"` twice instead of also matching `"[contract] denied"` — every contract denial was silently invisible in the dashboard. Removed a dead, never-displayed `activeProxies` capture left over from an unfinished feature.

**BusyBox httpd docroot**: Restricted to `/app/www` via symlink across all Docker startup scripts, closing unauthenticated access to the full `/app/` directory on port 8080.

**CI flake** (#290): Fixed a test still using a hardcoded 45s deadline that PR #286 had bumped everywhere else.

**JWT NetworkId claim key** (#295): `ParseByJwtUnverified` in `jwt.go` previously read `claims["network_name"]` for both `NetworkName` and `NetworkId` (copy-paste bug). `NetworkId` now correctly reads `claims["network_id"]`. 3 new tests cover valid, missing, and malformed JWT claims.

**authBytes message-pool leak** (#295): `runH3` in `transport.go` was missing the `defer MessagePoolReturn(authBytes)` that `runH1` has, leaking one pool buffer per H3 auth handshake.

**logThrottle consolidation** (#296): Replaced 6 hand-rolled atomic rate-limiters in `transfer.go` with 3 `logThrottle` instances, removing ~50 lines of duplicated `CompareAndSwap`/`Swap` boilerplate. All three call sites (`[c]ping`, `[c]ping err`, `[r]drop`) behave identically.

**Division-by-zero guard** (#296): `contractByteCount()` in `transfer_contract_manager.go` now guards against `ContractTransferByteSeqScale=0` (bad profile override), returning `max(StandardContractTransferByteCount, minByteCount)` instead of panicking.

**Explicit GOGC=100 for tier3** (#296): `applyTier3` in `tuning.go` now calls `debug.SetGCPercent(100)` to match the other three tiers, which already set it explicitly. No behavioral change (100 is Go's default).

**SequencePeerAudit error logging** (#296): `SequencePeerAudit.Complete()` no longer silently swallows `SendControl` errors — the empty `func(...){}` callback was replaced with an `Errorf` that logs the error. Test confirms the log message is emitted without panicking.

**.gitignore hardening** (#296): Added `hub/hub_bin`, `hub.db`, and `prs.json` to prevent accidental commits of build artifacts and local state.

### Known gaps not yet in this release

No open gaps currently tracked for the next release.

---

## [v3.23.0-fix.26.3] — 2026-07-16

### Added

**Hub CA cert auto-bootstrap** (#281): On startup, `hub init` now checks `URNETWORK_HUB_TOKEN`, `URNETWORK_HUB_TOKEN_FILE`, and `URNETWORK_HUB_TOKEN_STDIN` for a hub token. If present, it fetches the CA cert from `$HUB/ca-cert?token=...` using the token before doing anything else.

**PAKE-based hub join** (`hub -hub-join <url>`): New command that runs the OPAQUE handshake against a hub's join endpoints, reads password from stdin, and saves a per-node credential to `~/.urnetwork/hub.credential`. The credential is accepted by `requireAuth` alongside `URNETWORK_HUB_TOKEN`. Credentials are stored hashed (SHA-256), revoked on `/api/nodes/remove`, and re-registration follows `hub.password` rotation. Pending handshakes expire after 2 minutes to bound server memory. The CLI runs the same tested OPAQUE helpers as the unit test suite rather than a separate implementation.

**Auto Tier 4 Extreme** (#280): On hosts with >= 8 GiB RAM, the provider now auto-selects the Tier 4 (extreme) performance profile matching turbo-v8 settings. Manual `tier set 4` overrides remain.

**Docker-backed hub install/update** (#278): `urnet-tools hub install` and `hub update` on macOS and Windows now deploy the hub via Docker (`docker pull`/`run` against `ghcr.io/full-bars/urnetwork-3.23-fix-hub`). Linux supports `--docker` opt-in. All platforms share the same `urnetwork-hub` container name and `urnetwork-hubdata` named volume.

**HTTP Basic Auth for hub dashboard** (#282): The hub dashboard and read-only API endpoints now accept HTTP Basic Auth via `URNETWORK_HUB_DASHBOARD_PASS`. Separate from `URNETWORK_HUB_TOKEN` (used for write endpoints). Unset = unauthenticated.

**Persisted custom network selection** (#288): New `provider choose_network <api_url> <connect_url>` / `provider choose_network --reset` commands let operators running their own API/connect backend save it to `~/.urnetwork/network.json` instead of repeating `--api_url`/`--connect_url` on every invocation. Resolution order: flag > saved config > hardcoded default. Docker: `UR_API_URL`/`UR_CONNECT_URL` env vars (must be set together), also reachable via `urnet-tools choose_network` (Linux/Docker exec and Windows). Ported from `urfoundation/sn` PR #1.

### Fixed

**CA cert live reload** (#279): The hub now watches `hub_ca.pem` via file poll and reloads the CA certificate on change without restarting. Allows CA cert rotation on live hubs.

**Hot-restart warning fix** (#277): `urnet-tools restart` no longer always shows "cold restart required" — it now reflects actual hot-restart status. Deduped `hotRestartEnabled()` check into a cross-platform helper. Worker download fallback ported from Linux to macOS (`Provider_Install_Mac.sh`) and Windows (`urnet-tools.ps1`) so all three platforms have GitHub rate-limit resilience for binary downloads.

**`--tag` not honored on hub update** (#277): When a persisted config had a tag and `--tag` was passed, the persisted tag took precedence. Fixed so `--tag` always wins.

**Docker hub update pointed at wrong image** (#277): `hub update` was pulling the provider image instead of the hub image. Fixed to resolve the correct `-hub` image.

**Go vet fix** (#277): Escaped `%` in printf-style comment.

### Known remaining

- **Fingerprint pinning (`hub.pin`)**: Deprecated. To be removed in the next release.

---

## [v3.23.0-fix.26.2] — 2026-07-14

Release notes: `releases/v3.23.0-fix.26.2.md`

### Added

**Self-Healing Proxy Resource Management** (#259): Two-layer system — always-on fixes (default `proxy_url_max=500`, cleanup scope `"url"`, faster dead give-up, stale re-probe) plus opt-in closed-loop pressure system (`URNETWORK_SELF_HEAL=1` / `urnet-tools self-heal on`). AIMD pool controller grows under calm, sheds worst-first under pressure. Off by default.

**Hub Off/Set live reload** (#276): All four hub commands (`link`/`unlink`/`set`/`off`) now write the same override file the provider polls every report tick — `hub set`/`off` no longer require a restart.

**Subnet Integration Prep (Phases 1–2)** (#272): Pins `urfoundation/sn` crypto/chain packages; backports upstream `Sn*Sync` API methods. Inert until CLI wiring lands.

### Fixed

**ProbeOK Lost on Cache Merge** (#274): Fixed `mergeProxyURLEntries` discarding API-reachability results before caching, letting the reaper blacklist proven-live proxies.

**go-ethereum Dependency Bump** (#275): `github.com/ethereum/go-ethereum` v1.16.7 → v1.17.4, clearing 5 Dependabot alerts.

**Installer Links** (#271): Replaced `raw.githubusercontent.com` installer links with `dl.fullbars.xyz` shortcuts.

---

## [v3.23.0-fix.26.1] — 2026-07-13

Release notes: `releases/v3.23.0-fix.26.1.md`

### Added

**Docker: In-Place Updates** (#255): `urnet-tools update` inside the Docker container allows in-place binary replacements without pulling a new image.

**Official `dl.fullbars.xyz` URLs** (#258): All installation scripts and docs updated to use the new shortened domain.

**Go 1.26 Toolchain & Dependencies** (#267): Compiler bumped from 1.25.x to 1.26.4; core deps bumped (`pion/webrtc`, `quic-go`, `golang.org/x/*`).

**Standardized Repository Templates** (#268, #269): Structured PR and Release templates for consistent technical documentation.

### Fixed

**Upstream Infrastructure Port** (#261): Ported upstream commits `ee7a476` + `83dc999` — Egress interfaces, Memory Budgets, Contract Stats, Peer Management, `ip_assoc.go`.

**Peer API Refactor & Pause Fix** (#262): Ported upstream Peer API changes — `ReceiveFunction` type change and pause behavior fix.

**Upstream 532ee20c Fixes** (#265): Mode election fix, hot-spin CPU fix, connection eviction fix, `MonitorValue` constructs.

**Core Stability & Leak Cleanup** (#266): Fixed memory leaks in transfer pool and route manager (unbound map growth), resolved WebRTC stream ID panic, prevented infinite CPU spin loop during warmup, handled auth context cancellations.

**Hot-Reload Lock Race & Shutdown Diagnostics** (#260): Idempotent lock releases; cancel-time stack trace diagnostics for easier debugging of exit hangs.

---

## [v3.23.0-fix.26] — 2026-07-09

### Hot-Restart Enabled by Default

`hotRestartEnabled()` now returns `true` unless `URNETWORK_HOT_RESTART=0` is set. Client JWTs are reused across restarts without any opt-in flag — the write path was already un-gated (v25.15), now the read path follows. Set `=0` to disable.

Platform toggles updated:
- **Linux**: `hot-restart off` sets `URNETWORK_HOT_RESTART=0` in override.conf
- **macOS**: `hot-restart off` sets `=0` in launchd plist  
- **Windows**: `hot-restart off` sets `=0` in user env
- `hot-restart on` removes the flag (falls back to Go default = true)

### Added

**Session save/load** (#250): New `urnet-tools session save <file>` / `session load <file>` commands. Encrypted via `openssl aes-256-cbc -pbkdf2`, bundles all 7 identity and proxy-list files from `~/.urnetwork/`, with password-prompted passphrase. Network ID check on load prevents cross-account transfer.

**`provider print-network-id <file>`** (#250): New hidden CLI subcommand that extracts the `network_id` JWT claim using the existing `gojwt` parser, used by the session load safety gate.

**Atomic file writes** (#250): 6 file paths (`jwt`, `jwt_last_refresh`, `.provider.key`, `.provider.cert`, `proxy`) converted from `os.WriteFile` to `os.CreateTemp` + `os.Rename`. Enables `session save` against a live, traffic-serving provider without torn-file risk.

**`applyStagedSession()`** (#250): New Go function called early in `provide()`. Checks for `~/.urnetwork/.session-pending` marker and atomically swaps files from `.session-staging/` → live directory. This is the mechanism that lets `session load` run safely without stopping the provider.

**macOS native installer** (#252): New `Provider_Install_Mac.sh` — supports `install`, `start/stop/restart/status` (via launchctl), `hot-restart on|off`, `session save|load`, `proxy`, `hub`, `auth`, `logs`. Installs binary to `~/.local/share/urnetwork-provider/`, creates launchd plist with KeepAlive, strips quarantine xattr.

**macOS CI builds** (#252): `release.yml` expanded to build and publish `darwin/amd64` and `darwin/arm64` binaries alongside linux. Release tarball includes `darwin/` directory.

**Windows hot-restart toggle** (#251): `urnet-tools.ps1 hot-restart on|off`. Sets/removes `URNETWORK_HOT_RESTART` as user-level env var, with process-level `$env:URNETWORK_HOT_RESTART` propagation so immediate `Start-Process` restart inherits it. Help text reorganized matching Linux PR #245.

**Docker session save/load** (#253): `session save|load` commands added to `docker/scripts/urnet-tools.sh`. Interactive TTY guard prompts if `-it` is omitted. Restart via `pkill` triggers start script crash loop.

**Help text reorganization** (#245): `show_help()` in `Provider_Install_Linux.sh` rewritten with full descriptions, hub commands listed for the first time, `urnet-tools set help` sub-menu, header attribution updated.

**IPv6 STUN auto-detect** (#246): Provider probes IPv6 STUN reachability on startup via `sync.Once`-guarded 100ms UDP dial. If unreachable, restricts ICE candidates to `NetworkTypeUDP4`/`NetworkTypeTCP4` — eliminates "network is unreachable" STUN noise on IPv6-unreachable hosts.

**Transport self-wake loop fix** (#248): Restructured `run()` loop in `transport.go` — `activeMode()` notify channel captured AFTER `setActiveMode()` call, guarded with `lastMode` dedup to skip redundant calls. Eliminates the structural vulnerability that caused a 100% CPU self-wake loop.

**No-arg status toggles** (#247): `eco`/`ramlogs`/`lowmode`/`hot-restart` with no arguments now report current status instead of erroring. Space-separated aliases (`hot restart`) work correctly.

### Fixed

**Passphrase cmdline leak** (#254): All `openssl enc -pass "pass:$var"` invocations changed to `-pass "file:$_pf"` with temp file and immediate cleanup. Passphrase no longer visible in `ps` output.

**Mac installer `$0` fix** (#254): `cp "$0"` replaced with GitHub raw URL download — `cp "$0"` fails silently when invoked via `curl | sh`.

**Atomic writes safety** (#254): `atomicWriteFile` changed from fixed `path + ".tmp"` to `os.CreateTemp(dir, name+".*.tmp")` to avoid temp file collision under concurrent `.provider.key`/`.cert` writers.

**`--force` parsing** (#254): Session load now scans `$3+` for `--force/-f` — previously silently dropped when placed after the file path.

**Bundle JWT validation** (#254): Hard-fail if `print-network-id` returns empty (corrupt bundle JWT) — previously silently bypassed the network_id gate.

### Security

- Session bundles: `openssl aes-256-cbc -pbkdf2`, password-required
- Network ID safety gate on load prevents cross-account identity transfer
- Temp files `0600`, cleaned up on both success and error paths

### Added

**Critical event log** (#242): New `~/.urnetwork/events.log` on disk (not RAM) — 1MB capped, auto-rotating file that survives restarts. Captures STARTUP, SIGNAL, PROVIDER EXIT, PANIC, and FATAL events. Provides a permanent forensic trail even when `/dev/shm` logs are wiped by a restart.

**RAM logs survive restarts** (#242): `shmLogPath` and `shmImportantLogPath` now open with `O_APPEND` instead of `O_TRUNC`. Previous run logs are preserved with a `--- provider restarted at ...` separator.

**Panic hook in `connect` package** (#242): `connect.CritLogger` / `connect.LogCritical()` writes recovered panics from any networking goroutine to the disk-based events.log.

**Release notes backlog** (#243): Release notes for v3.23.0-fix.25.6 through 25.13 are now written and committed.

---

## [v3.23.0-fix.25.13]

### Fixed

**Best Proxies fresher last-seen**: UNION scan of `proxy_node_hourly` alongside `proxy_fleet_daily` for today-accurate timestamps.

### Changed

**x/crypto v0.51.0 → v0.52.0**: Resolves 7 critical CVEs.

---

## [v3.23.0-fix.25.12]

### Added

**Sortable dashboard columns**: Best Proxies and Proxies tables support click-to-sort on all columns.

---

## [v3.23.0-fix.25.11]

### Added

**`proxy remove-dead --auth-failures=N`**: Per-address auth failure threshold with 250/day rate limit. Self-healing skip of currently-up proxies.

---

## [v3.23.0-fix.25.10]

### Added

**Hub dashboard charts**: Added Mbps throughput, Billable %, and Contract Win Rate charts. Fixed chart overflow.

**AI.md operations guide**: Comprehensive guide for provider + hub operations.

---

## [v3.23.0-fix.25.9]

### Fixed

**Dash shell compatibility**: Removed `set -e` and added `|| true` guards on `curl` assignments in `onboard.sh`.

---

## [v3.23.0-fix.25.8]

### Added

**Hash-based URL routing**: Dashboard sections now bookmarkable (`#overview`, `#servers`, `#proxies`, `#contracts`, `#best`).

### Fixed

**Onboard script**: HTTP fallback double-port bug, added direct `/api/cert` fallback.

---

## [v3.23.0-fix.25.7]

### Fixed

**SQLite index hint**: Forced `INDEXED BY idx_pnh_hour_proxy` on live-tail scan to prevent query planner from picking the wrong index.

---

## [v3.23.0-fix.25.6]

### Fixed

**Hub dashboard**: SSE debounce to 5s, sort state survives table rebuilds, composite index on `proxy_node_hourly(hour, proxy_id)`, rate limiter removed (Caddy handles it).

### Changed

**CI**: Renamed `hub-v*` Docker tags to `hub-docker-v*` to clarify they are Docker-only.

---

## [v3.23.0-fix.25.2] — 2026-07-05

### Summary
Two more fixes found while responding to a live fleet incident: a self-healing repair for a rogue systemd drop-in that left the provider with no crash-restart protection, and a logging regression that's been misattributing every plain-level log line to `log.go` since mid-June.

### Fixed

**Self-heal invalid `Restart=` systemd drop-in**: a `restart-override.conf` drop-in with `Restart=yes` (not a valid systemd value — the directive only accepts `no`/`always`/`on-failure`/etc.) was found on a fleet node, silently rejected by systemd on every reload and falling back to the base unit's `Restart=no`. This left the node with zero auto-restart protection since at least July 1. Not something urnet-tools ever wrote — a full history search turned up no commit that ever generated this file or value — so origin is unknown (likely a manual edit). `install_systemd_units` now scans `urnetwork.service.d/*.conf` on every install/update/reinstall and repairs `Restart=yes|true|1` to `Restart=on-failure`.

**Restore correct file:line in log output**: the glog→Logger interface migration (PR #69, 2026-06-15) added a wrapper frame between call sites and the actual `glog` calls. Only the `V(n)` verbose path accounted for the extra frame; the plain `Info`/`Infof`/`Warningf`/`Errorf` methods called glog's non-depth-aware functions directly, so every plain-level log line in the codebase has reported `log.go`'s own line instead of the real caller since that PR merged. Switched to glog's depth-aware variants (`InfoDepth`/`InfoDepthf`/`WarningDepthf`/`ErrorDepthf`) to match the verbose path's existing convention.

---

## [v3.23.0-fix.25.1] — 2026-07-05

### Summary
Two correctness fixes found during a deep code audit, both introduced when the fork branched from stock v3.23 and never ported from upstream.

### Fixed

**P2P transport setup blocked forever, silently dropping one direction of every bidirectional relay stream**: `NewP2pTransport()` started its connection-negotiation loop synchronously (`HandleError(p2pTransport.run)` with no `go`). Since that loop only returns when the stream's context is cancelled, the first call never returned — so `StreamSequence.Run()`'s second `NewP2pTransport()` call (needed for the opposite direction) was unreachable. Every bidirectional P2P stream ended up with only one direction's WebRTC transport ever negotiated, forcing silent fallback to lower-priority transports. Fix: restore `go HandleError(p2pTransport.run, cancel)` to match upstream. Added INFO-level logging (`[p2p]` transport start/stop, `[sm]` both-transports-created) so this class of bug is visible in logs going forward instead of failing silently.

**Message pool buffer leak in DNS fragment reassembly**: `combineQueue.RemoveOlder` dropped timed-out fragment sets without returning their pooled buffers via `MessagePoolReturn`; `combineQueue.Combine` overwrote a fragment slot on a duplicate/retransmitted index without returning the buffer it replaced; and `packetTranslation.decodeDns`'s goroutine had no shutdown drain for its `dnsCombineQueue` or `readPipeline`, leaking any buffers still in flight at teardown. All three now return buffers to the pool.

---

## [v3.23.0-fix.25] — 2026-07-05

### Summary
Minor version bump covering ~50 PRs across core networking stability, performance optimization, proxy lifecycle management, a fully self-hosted hub dashboard with live SSE push, audited error propagation, and tightened security. Includes all sub-patch releases from v3.23.0-fix.24.1 through v3.23.0-fix.24.34 plus new fixes and tuning.

---

### Core Networking

**SendSequence no longer panics at resend cap** (PR #201): When a packet hit the 16-resend limit, it was removed from the resend queue but left orphaned in `sendItems`. The next cumulative ack would hit `panic("Missing item")`, tearing down the entire send sequence and flushing pending contracts. Fix: park the item at `AckTimeout` horizon instead of dropping it.

**Contract rejection statuses now visible to callbacks** (PR #202): `HandleControlFrame` constructed `ContractStatus` on Trust/Invalid errors but returned before dispatching. Locally-rejected and malformed contracts were invisible to `registerContractCallback` (provider metrics) and penalty logic. Fix: dispatch before the early return.

**Pool buffer leak on write timeout — v2** (PRs #203, #206): `WriteDetailed` timeout/ctx-done/done branches had `MessagePoolReturn` commented out, stranding one shared pool ref per timed-out write. PR #206 removes the double-return regression from the first fix and applies `gofmt -w`.

**SOCKS5 proxy DNS death spiral** (PR #189): Resolve target hostnames locally before passing to SOCKS5 dialer, converting FQDN→IPv4. Prevents proxy infrastructure DNS from being blacklisted by upstream resolvers.

**Proxy warmup pileup on 2000+ proxy pools** (PR #190): Three compounding fixes — DNS lookup cache (60s TTL), 30s SOCKS5 dial timeout, and `markProxyDown` now clears `h.connecting` state to unblock warmup progress tracking.

**Self-wake 100% CPU feedback loop** (PR #191): `modeMonitor.NotifyAll()` gated on actual mode changes. Affected v24.27–v24.32.

**Error propagation for reload/state writes** (PRs #163, #165): Silently discarded errors in reload trigger and proxy probe paths now log warnings. Split error check from sequence comparison to prevent transient FS failures from spuriously triggering reloads.

**Port bounds check** (PR #168): Fix CodeQL alert #11 — `strconv.Atoi` could produce values outside 0–65535 port range before `uint16` cast.

**Dead code cleanup** (PR #200): `rttHeap.MinRtt()` returned arbitrary heap leaf instead of actual min. `MultiRouteSelector.setActive()` ignored its parameter. Both fixed.

---

### Performance

**Message pool N-way mutex sharding** (PR #207): Each size-class freelist (2048–65536) split into 16 internal shards with independent mutexes, eliminating cross-proxy lock contention on the hottest allocation path. Rollback lever: `URNETWORK_MESSAGE_POOL_SHARD_COUNT=1`. ~60–80% contention reduction expected at fleet packet rates.

**Default resend queue 2→4 MiB** (PR #204): Raised `ResendQueueMaxByteCount` to match the 4 MiB send window. ~2× per-client throughput ceiling on default-profile nodes.

**Contract ramp-up scale 2→3** (PR #209): Adds intermediate ramp step (2 MiB → ~44 MiB → ~86 MiB → 128 MiB vs 2 MiB → ~65 MiB → 128 MiB). Reduces unused contract allocation on short-lived connections by ~30%.

**RTT fill-fraction floor 0.5→0.7** (PR #205): At ≥1s RTT, 90 MiB consumed per 128 MiB contract before renegotiation (was 64 MiB). Halves contract churn on high-latency paths.

**Hot-path allocation optimizations** (PRs #137, #138, #148–#150): Zero-alloc marshal via `frame_protobuf.go`, `ulid.Make()` replaced with allocation-free `NewId()`, closure hoisting in `ip_remote_multi_client.go`, `ip.go` IpPath caching + timer reuse, `transfer.go` safeAck by-value + timer reuse.

**Message pool optimized for write pipeline** (PRs #148–#150): Pooled buffers drained from write pipeline on cancellation. `SendBuffer.Ack` tries all candidate sequences instead of only the first.

---

### Hub Dashboard & Self-Hosting

**Self-hosted hub dashboard** — a full-featured fleet management UI:
- SQLite-backed historical storage with 5m report interval (PR #123)
- Multiple sortable data views: Overview, Servers, Proxies, Contracts (PRs #184, #185)
- uPlot charts: healthy proxies, traffic, billable bytes, clients (PR #158)
- Summary cards, lazy proxy loading, filter/search, slide-out drawer (PRs #155–#157)
- Per-proxy earning column (`earning=yes/no`) (PR #124)
- Per-proxy contract metrics with 15m/1h/24h rolling windows (PR #182)
- Fleet-wide per-proxy analytics: hourly 90d, daily 13mo, fleet_daily forever (PR #184)

**Live updates via SSE** (PR #188): Server-sent events push `data: refresh` to dashboard tabs the moment a heartbeat or report arrives. No polling.

**TLS hub reports with cert pinning** (PR #186): Auto-generated ECDSA P-256 cert, TOFU-style pinning, `hub init`/`hub link`/`hub unlink` commands, transactional `hub update` with rollback.

**Live heartbeat endpoint** (PR #187): `/api/heartbeat` at 15s cadence with connection reuse and exponential failure backoff (capped at 5m).

**Performance** (PR #186): Gzip compression, per-IP rate limiting (60 req/min), stale node eviction (15 min), nodeSummary API.

**Operational commands**: `urnet-tools report <url>` for runtime hub URL config (PR #154), `urnet-tools set report-interval 60s` for runtime report cadence (PR #159).

**Hub Docker & CI** (PRs #193–#195, #197): First-class Docker image with buildx caching, CI pipeline building on `hub/**` changes, reduced build context.

---

### Proxy URL Sources & Lifecycle

**Dual-stage SOCKS5 + API reachability probe** (PR #152): Tests SOCKS5 compliance AND API reachability through the proxy on a single TCP connection. Background reaper with 5-min retry, 3-failure eviction to 24h persistent blacklist.

**Escalating give-up backoff** (PRs #129, #133): 15m → 30m → 1h → 2h → 4h → 8h → 16h → 24h, permanent eviction after 10 cycles via persisted `proxy_url.json` blacklist.

**File-before-URL launch priority** (PRs #139, #140): File-based proxies launch before URL-sourced ones (sorted by source). URL fetcher waits for warmup completion.

**Pattern-based proxy management** (PR #183): `proxy remove --match=<substring>` with host-substring matching, persisted exclusion patterns, `proxy exclude` subcommand.

**Multi-tier degraded proxy cleanup** (PRs #145, #185): Runtime-configurable auto-cleanup with source filtering. Dead proxy reclassification after 7 days down.

**Operator-curated proxy resilience** (PRs #119, #120): Auth retry with slow capped backoff (5m→10m→15m). URL requeue decoupled from reload engine via `time.AfterFunc`.

---

### Observability

**Per-proxy contract metrics** (PRs #182, #184): Lock-free atomic counters for acquired/denied contracts. Contract Win Rate card, contracts/hr chart, sortable Contracts column in node table.

**Per-minute earning windows** (PR #121): `[earn] billable_1m/5m/15m/60m` every 60 seconds, decoupled from health heartbeat.

**`[profit]` heartbeat + contract utilization** (PR #126): 15s ticker showing `earning=yes/no clients=N rate=X`. Contract close logging with `acked=X allotted=Y util=Z%`.

**Important log buffer** (PR #135): 1 MiB `/dev/shm/urnetwork-important.log` — high-value lines only (`[profit]`, `[health]`, `[outage]`). Survives main ramlog flood.

**Log spam suppression** (PRs #131, #135): Unified `logThrottle` replaces 4 identical rate-limiters. `[c]ping err` suppressed to 1 line per 5 minutes with `(N suppressed)` count.

**Version awareness** (PR #132): `urnet-tools -v` shows running vs on-disk version separately. Provider version logged at startup. Goroutine count in health heartbeat.

---

### Security

**DoH certificate pinning** (PR #164): Uses `DefaultTlsConfig()` with ISRG Root X1/X2 pinning instead of insecure nil TLS config for all four DoH providers.

**System cert pool for DoH** (PR #167): Restored Go's system cert pool after LE-only pool broke Cloudflare, Google, Quad9, and OpenDNS.

**Hub bearer token authentication** (PR #170): `URNETWORK_HUB_TOKEN` required for `/api/report` and `/api/nodes/remove` write endpoints.

**Cap unbounded reads** (PRs #180, #181): `io.LimitReader` (8 MiB HTTP / 64 KiB DoH) on all response body reads. JWT write failure now triggers `log.Fatal`.

**IP Security DPI refactor** (PR #160): Replaced monolithic `ip_security.go` with layered DPI pipeline. Packed binary-search blocklist (64,131 IPv4 + 214 IPv6 ranges). BitTorrent signature detection (BEP 3/5/15/29).

---

### CI/CD & Operations

- **`urnet-tools update -f`** now stops, updates, and restarts non-interactively (PR #125)
- **`urnet-tools update`** fetches latest script from GitHub; bundled in tarball (PR #136)
- **Idempotent `override.conf` helpers**: `override_set_env`/`override_rm_env` eliminate duplicate systemd entries (PR #153)
- **Runtime tunables**: `urnet-tools set node-name`, `report-interval`, `proxy-url-max`, `proxy-url-refresh`, `fast-auth` (PR #159)
- **Provider version resolution** scoped to its own tag scheme (PR #199)
- **Remove dead config fields**: `LegacyCreateContract` and `TrackUsedContracts` (PR #166)
- **`urnet-tools` no-args** shows help instead of triggering install (PR #161)
- **Dash/bash compatibility**: `echo -e` replaced with `printf` across scripts; lint extended to deps/uninstall (PR #117)
- **Build release monitor**: Discord notifications on `urnetwork/build` release tags (PR #162)
- **CI**: Docker build depends on test success, module cache, skip registry logins on PR builds (PRs #112, #198)
- **Deps**: Bump `golang.org/x/net` (PR #196)

---

## [v3.23.0-fix.24.34] — 2026-07-03

### Added
- **Provider version startup log**: The binary now logs `[startup] provider version=...` once at process startup (inside `provide()` before proxy setup), matching the version line Docker already printed via startup scripts. Existing `client_id`/`instance_id` output is unchanged.
- **Goroutine count in health heartbeat**: The `[health]` log line now includes `goroutines=N` for spotting leaks and runaway goroutine growth.

### Changed
- **Removed legacy `WARP_VERSION` env var**: `RequireVersion()` now returns the linked-in `Version` directly. The Dockerfile no longer sets `WARP_VERSION`, so in-container `urnet-tools update` no longer pins the reported version to the original image version.

---

## [v3.23.0-fix.24.33] — 2026-07-03

### Fixed
- **100% CPU self-wake feedback loop** (PR #191): Commit `1f64686` (v24.27) added `modeMonitor.NotifyAll()` to `PlatformTransport.setActiveMode()` so mode-waiting goroutines would wake on changes. However `run()` calls `setActiveMode()` unconditionally — even when the selected mode is already active — causing `NotifyAll()` to close the notification channel `run()` literally just captured, forming a self-wake loop that spins ~8,000 Hz per CPU core. Fix: gate `NotifyAll()` on actual mode change. Same guard applied to `setModeAvailable()`. Regression tests added.

### Added
- **Self-wake diagnostic log**: `setActiveMode` now logs a rate-limited warning when called with the mode already active — surfaces the feedback loop pattern in logs before it silently consumes 100% CPU.
- **Regression tests**: `TestSetModeAvailableNoSpuriousWake` and `TestSetActiveModeNoSpuriousWake` verify that neither setter fires `NotifyAll` when the value hasn't changed, but both still fire on real transitions.

---

## [v3.23.0-fix.24.32] — 2026-07-03

### Fixed
- **Proxy warmup goroutine pileup** (PR #190): Three fixes for warmup on large proxy pools (2000+): DNS lookup cache (60s TTL) so 2000+ concurrent goroutines don't hammer the system resolver; 30s SOCKS5 dial timeout for the warmup path (no-op for paths with existing context deadlines); `markProxyDown` now clears `h.connecting` so proxies whose initial connection fails aren't stuck in "connecting" state forever.

---

## [v3.23.0-fix.24.31] — 2026-07-02

### Fixed
- **SOCKS5 proxy DNS death spiral** (PR #189): Broken DNS resolvers on paid SOCKS5 proxies caused `golang.org/x/net/proxy` to fail TLS handshakes. Fix resolves target hostnames locally before passing them to the SOCKS5 dialer, converting FQDN → IPv4. Downgrade-safe.

---

## [v3.23.0-fix.24.30] — 2026-07-02

### Added
- **Hub TLS with cert pinning (TOFU)** (PR #186): Hub binary accepts `-tls-addr` and auto-generates a self-signed ECDSA P-256 cert on first boot. `/api/cert` exposes the SHA-256 fingerprint for trust-on-first-use pinning; providers verify it via `~/.urnetwork/hub.pin` before reporting. New `urnet-tools hub` subcommands:

  | Command | What it does |
  |---|---|
  | `hub init` | Enables TLS via `URNETWORK_HUB_TLS_ADDR=:8443`, restarts, prints fingerprint + firewall hint |
  | `hub link https://host:8443` | Fetches and confirms the fingerprint, pins it, sets `report_url` |
  | `hub unlink` | Removes the pin, reverts to plain HTTP |
  | `hub test [url]` | Verifies the pinned fingerprint still matches (openssl, curl fallback) |
  | `hub open-port <port>` | Detects firewalld/ufw/iptables/nftables, prints the exact command to open it |
  | `hub update [-f] [-t tag]` | Transactional binary update with automatic rollback on failure |

- **Transactional hub update** (PR #186): `hub update` is atomic — stops the service, backs up `hub.db`, downloads and verifies the new binary, swaps it in, restarts, and verifies it came up. Any failure restores the old binary + DB and restarts the previous version. Idempotent (no-ops at the target version unless `--force`). 40 test cases cover tag resolution, rollback states, and systemd templating.
- **Live hub heartbeat** (PR #187, #188): New `/api/heartbeat` endpoint gives the dashboard a 15s-cadence liveness signal, separate from the full `/api/report` (5-15m) — Mbps rate, last-seen, uptime, and now per-proxy status/contract counts, all updated in-memory only (no DB writes). The provider only sends proxies whose status or contract counters actually changed since the last tick, so payload size scales with fleet activity, not fleet size. One `http.Client` is reused per reporter instance instead of a fresh TCP+TLS handshake every tick, and consecutive failures back off exponentially (capped at 5m) so a flaky link to the hub doesn't turn into a retry storm.
- **Live dashboard updates via SSE** (PR #188): New `GET /api/events` endpoint pushes a "something changed" signal to connected browser tabs the instant a heartbeat or report lands, instead of waiting on the dashboard's 30s poll. The poll stays as a backstop for links where SSE gets buffered or stripped.
- **Dashboard visual polish** (PR #186): Inline color-coded IP tags per node row (same-NAT boxes cluster visually), a green TLS padlock icon on nodes reporting over HTTPS, and sortable Servers-page columns (replacing the old grouped-header layout).

### Fixed
- **Dead proxy reclassification** (PR #185): Proxies down for ≥7 days are now classified `dead` instead of `degraded` in `ProxyHealthSnapshot()`, matching the existing `inactive` tier.
- **Fleet chart initial-spike glitch** (PR #185): `loadFleetChart()` now seeds cumulative deltas from the first hour's data instead of 0, preventing a flattened chart after the first spike.
- **Window pill ordering** (PR #185): Proxies/Contracts page time-window pills now read ascending (1h → 24h → 7d → 30d → 1y).
- **NULL contract columns crash** (PR #185): `COALESCE(contracts_acquired,0)` / `COALESCE(contracts_denied,0)` guards added to three queries that could hit a NULL scan error on older rows.
- **`/api/nodes` missing traffic fields** (PR #186): Response now includes `rx`/`tx`/`bill_rx`/`bill_tx`/`clients` aggregated from proxy data — these were missing and caused `undefined` errors in the dashboard JS.

---

## [v3.23.0-fix.24.28] — 2026-07-01

### Added
- **Per-proxy contract metrics** — Each proxy goroutine now tracks contract acquisition and denial counts with lock-free atomic counters. Rolling time windows (15m, 1h, 24h) let operators spot short-term performance changes. Two new visibility layers:

  **Provider CLI**: `urnet-tools proxy summary` now shows:
  
    --- Contract Stats ---
      Acquired: 262
      Denied:   1012
      Win rate: 20.5%
      15m:  42 acquired / 181 denied
      1h:   262 acquired / 1012 denied  
      24h:  262 acquired / 1012 denied

  **Hub dashboard**:
  - **Contract Win Rate card** — Fleet-wide view of raw acquired vs denied totals and computed win percentage
  - **Contracts/hr chart** — uPlot chart showing acquired (green) and denied (red) contracts per hour, letting you see patterns across the day
  - **Contracts column** in the fleet node table — Sortable column showing which nodes win contracts
  - **Won/Lost in proxy drawer** — Per-proxy breakdown of successful vs denied contracts, sortable by either column

  **SQLite storage**: `node_hourly.contracts_acquired` and `node_hourly.contracts_denied` store contract history for 1-year retention, queryable via `/api/history?hours=168` for weekly rollups.

---

## [v3.23.0-fix.24.27] — 2026-07-01

### Fixed
- **Hub bearer token authentication**: `/api/report` and `/api/nodes/remove` now require `URNETWORK_HUB_TOKEN` (PR #170). Constant-time comparison, backward-compatible (unset = unauthenticated). Report body capped at 1MB.
- **RouteManager concurrent map crash**: Added mutex lock in `DowngradeReceiverConnection` to prevent "concurrent map iteration and write" crash under transport churn (PR #171).
- **NACK accounting leak**: `removeEventBucket` now subtracts `sendNackCount`/`sendNackByteCount` on bucket eviction, preventing permanent bandwidth accounting drift (PR #172).
- **SendBuffer.Ack wrong sequence handling**: Moved `break` inside success branch so all candidate sequences are tried before giving up (PR #173).
- **Mode monitor waiters never wake**: Added `NotifyAll()` calls after `setModeAvailable`/`setActiveMode`, allowing H1/H3 mode-wait loops to wake on state change instead of timing out (PR #174).
- **Write-pipeline buffer leak on cancel**: UDP and TCP write goroutines now drain `writePayloads` on exit; additional drain in `Run()` catches items re-queued after the goroutine drain (PR #175).
- **MultiRouteSelector.Read ignored caller context**: Changed to use passed-in `ctx` instead of `self.ctx` (PR #176).
- **Checkpoint contracts closed incorrectly**: `CloseContractWithCheckpoint` now guards cleanup behind `!checkpoint` (PR #177).
- **Client replace cleanup bypassed removeClient**: `expand()` now calls `clientRemoveCallback` instead of bare `Cancel`, properly cleaning up `clientUpdates` entries (PR #178).
- **PeerConn data channel not closed on teardown**: Added `conn.Close()` to deferred cleanup in `peerConn.Run()` (PR #179).
- **Unbounded HTTP/DoH reads**: Added `io.LimitReader` caps (8 MiB / 64 KiB) on all response body reads (PR #180).
- **JWT write error silently ignored**: Added error check + fatal log for `os.WriteFile` in JWT persist (PR #181).

---

## [v3.23.0-fix.24.21] — 2026-06-27

### Added
- **`urnet-tools fast-auth [on|off]`**: Toggle the auth rate limiter bypass without restarting the provider. `fast-auth on` takes effect immediately (same as `URNETWORK_AUTH_UNLIMITED=true`). `fast-auth` with no args shows the current state.
- **`urnet-tools set [<key> [<value>|off]]`**: Unified interface for all runtime tuning overrides — no restart needed, changes take effect on the next provider tick. `urnet-tools set` with no args shows all active overrides. `urnet-tools set <key> off` clears the override and reverts to the startup default.

  | Key | Example | What it changes |
  |-----|---------|-----------------|
  | `node-name` | `urnet-tools set node-name ny1-box3` | Identity label shown in hub dashboard |
  | `report-interval` | `urnet-tools set report-interval 60s` | How often bandwidth stats are posted to the hub (min 10s) |
  | `proxy-url-max` | `urnet-tools set proxy-url-max 500` | Caps URL-sourced proxies loaded per fetch cycle |
  | `proxy-url-refresh` | `urnet-tools set proxy-url-refresh 15m` | How often the URL proxy list is re-fetched (min 10s) |
  | `cleanup-scope` | `urnet-tools set cleanup-scope degraded` | Which proxies the cleanup job targets (`dead`, `degraded`, `all`) |
  | `cleanup-interval` | `urnet-tools set cleanup-interval 6h` | How often the dead-proxy cleanup job runs (min 1m) |
  | `fast-auth` | `urnet-tools fast-auth on` | Auth rate limiter bypass (also settable via `set`) |

  Backed by `~/.urnetwork/<name>` files introduced in PR #159; `urnet-tools set` is the supported interface for all of them.

### Fixed
- **Hub fleet charts rewritten** (PR #158): The sparkline was plotting cumulative totals (monotonically increasing, unreadable). Now shows 4 charts in a 2×2 flex grid: healthy proxies + active clients over time; hourly traffic deltas (RX/TX per hour window); billable traffic per hour; peak clients + reporting nodes.
- **History chart aggregates correctly in multi-node view** (PR #158): Was interleaving every node's data as separate line segments at the same X positions. Now aggregates by hour and sums RX/TX across all nodes. Negative deltas from reporting gaps clamped to zero.
- **Earning metric seeds correctly on restart** (PR #158): `prevBillable` was initialized to zero on restart, so the first report cycle always showed 0 earnings. Now seeded from per-proxy IDs stored in the DB — earning is accurate from the first report after a restart.
- **Proxy drawer routing fixed** (PR #158): `/api/nodes/<id>/proxies` was unreachable due to a routing conflict — the drawer silently failed to load per-node proxy lists. Drawer now defaults to sort by active clients descending (then billable RX descending), with sortable columns and 90vw width.
- **`/api/nodes` response includes earning field** (PR #158): Summary cards now reflect live earning state without a separate request.

---

## [v3.23.0-fix.24.22] — 2026-06-28

### Added
- **IP Security DPI Refactor** (PR #160): Replaced the monolithic `ip_security.go` (~66K lines) with a layered deep-packet-inspection pipeline. Payload-level BitTorrent signature detection (BEP 3/5/15/29, entropy-based encrypted-flow heuristic) instead of port-only heuristics. 214 IPv6 prefix ranges now blocked (previously unchecked). 66K-line `map[[4]byte]bool` blocklist replaced by ~8K-line packed binary-search for zero-allocation lookups. Known-safe protocols (NTP, IKE, DNS/UDP) skip DPI entirely via three-way verdict (drop/allow/pass-to-DPI).

---

## [v3.23.0-fix.24.23] — 2026-06-28

### Fixed
- **`urnet-tools` no-args shows help instead of triggering install** (PR #161): Running the script with no arguments was triggering a full install instead of showing the help menu. Fixed via `$0` path check instead of the broken `URNETWORK_TOOLS_MODE` env var injection approach.

### Added
- **Build release monitor** (PR #162): New GitHub Actions job monitors `urnetwork/build` repository for release tags, posts commit summaries with file changes to Discord. Expands critical files list and shows actual filenames in notifications.

---

## [v3.23.0-fix.24.24] — 2026-06-29

### Fixed
- **Error propagation for reload/state writes** (PR #163): `writeReloadTrigger()` and `writeProxyState()` now log warnings on failure instead of silently discarding errors. Prevents hot-reloads from silently breaking and stale state files when disk writes fail.
- **DoH certificate pinning** (PR #164): DNS-over-HTTPS resolver now uses `DefaultTlsConfig()` with ISRG Root X1/X2 pinning instead of insecure `// FIXME` nil TLS config. Prevents potential MITM of DoH responses.
- **Reload watcher and proxy probe error handling** (PR #165): Split error check from sequence comparison in `readReloadSeq` to prevent transient FS read failures from spuriously triggering proxy reloads. Added proper timeout handling for `SetDeadline` errors in probe stages.
- **Removed dead config fields** (PR #166): Eliminated `LegacyCreateContract` and `TrackUsedContracts` fields from `ContractManagerSettings` that were always `false` and had no active code paths.

---

## [v3.23.0-fix.24.29] — 2026-07-02

### Added
- **Pattern-based proxy removal** (PR #183): `proxy remove --match=<substring>` removes all proxies whose host matches a case-insensitive substring — no provider restart needed. Matching operates on host only (never port/user/pass). Persists exclusion patterns to `proxy_url.json` so URL source refreshes don't re-add matching proxies. `proxy exclude <pat>` / `proxy exclude --remove <pat>` manages the pattern list; `proxy summary` shows active patterns.
- **Fleet-wide per-proxy analytics** (PR #184): Three-tier proxy history in the hub database with delta-based ingestion, automatic rollups, and time-based retention:
  - **Tier 1 — proxy_node_hourly (90d)**: Sparse per-node per-proxy hourly deltas from cumulative provider counters. A delta tracker suppresses the first report after provider restart (counter going backwards = new baseline, no spike). Only proxies with actual activity get rows.
  - **Tier 2 — proxy_node_daily (13mo)**: Rolled up from hourly by a background goroutine using a high-water mark to prevent double-counting.
  - **Tier 3 — proxy_fleet_daily (forever)**: Fleet-wide daily aggregates with per-proxy node count.
  - Retention windows configurable via `URNETWORK_HUB_RETAIN_HOURLY_DAYS` (default 90) and `URNETWORK_HUB_RETAIN_DAILY_MONTHS` (default 13).
- **New hub read APIs** (PR #184): Three GET endpoints consumed by the new UI:
  - `/api/proxies/top?window=&sort=&node=&limit=` — proxy leaderboard with traffic/contracts/denied sorting
  - `/api/proxies/history?addr=&window=&split=node` — per-proxy time series with optional per-node comparison
  - `/api/nodes/contracts?node=&window=` — per-server won/denied contract series from node_hourly
- **Hub UI overhaul** (PR #184): Sidebar navigation with four pages:
  - **Overview**: summary cards + 5 fleet mini-charts (traffic, billable, clients, nodes, contracts/hr)
  - **Servers**: sortable/filterable node table with per-node proxy drawer
  - **Proxies**: leaderboard from `/api/proxies/top` with window pills, sort dropdown, node filter, color-coded win%, and collapsible idle section
  - **Contracts**: fleet won-vs-denied uPlot chart + per-server table sorted by win rate with green/red split bars; click a server opens its proxy drawer

### Fixed
- **Hub test CI timeout**: `TestRollupAndPrune` was processing 17000+ empty rollup hours on every run, timing out at 600s in CI. Mocked `nowFunc` to limit the rollup window to only the 3 test hours (0.06s → 0.06s, was 15s without race).

---

## [v3.23.0-fix.24.26] — 2026-06-30

### Fixed
- **`isHttpRequest` actually shipped** (upstream b6ee955): The v3.23.0-fix.24.25 entry documented this port, but the code was never added to `ip_security_dmca.go`. This change delivers it for real — plaintext HTTP/1.x request lines are detected in the DPI flow and return `dmcaAllow` before the encrypted-traffic entropy heuristic fires, preventing false-positive drops of radio/media streaming over non-standard ports.
- **Privileged-port DPI skip** (upstream 2144e33): `dmcaDetector.classify()` now allows destination ports `<1024` without payload inspection or flow tracking. A privileged port can't host a peer-to-peer/BitTorrent hole (peers, DHT, uTP, plaintext trackers all run on ephemeral/high ports), so skipping it lets legitimate non-web-standard encrypted services on privileged ports (e.g. Telegram MTProto on 443) through. Peer traffic on high ports is still inspected.
- **DoH rewrite actually shipped** (upstream b6ee955): The v3.23.0-fix.24.25 entry documented this port too, but it was also lost in the same history rewrite. Restored the full DoH refactor: dnsmessage wire-format support for Quad9/OpenDNS, parallel queries to 4 providers (Cloudflare, Google, Quad9, OpenDNS), `MinCacheTtl`/`MaxConcurrentResolutions` settings, single-flight query coalescing, local DoH support, and `TlsConfig` field for cert pinning. Merge conflict resolved by applying LE pinning to remote DoH (tunneled) while preserving system pool for local DoH (host-dialed).
- **Miss flag lost in DoH merge timeout paths** (PR #169): `dohQueryWithClientResult`'s three early-return paths (deadline, ctx cancel, timeout) were creating a new result struct with only `AddrTtls`, silently dropping a merged `Miss=true` from an already-received NXDOMAIN response. This caused the cache layer to skip caching, doubling DoH requests on resolution timeout.

### Added
- **DPI test coverage**: Ported upstream `ip_security_dmca_test.go` wholesale (13 tests). The fork's earlier DPI refactor (`f79e3c5`) brought `ip_security_dmca.go` without its test file, leaving the detector untested; this restores full upstream parity so future audits diff cleanly.

---

## [v3.23.0-fix.24.25] — 2026-06-29

### Fixed
- **DoH certificate pinning** (PR #164): DNS-over-HTTPS resolver now uses `DefaultTlsConfig()` with ISRG Root X1/X2 pinning instead of insecure `// FIXME` nil TLS config. Prevents potential MITM of DoH responses. (This was already live; PR #167's DoH rewrite was lost.)
- **`isHttpRequest` detection** (PR #167): Ported from upstream b6ee955. Detects plaintext HTTP/1.x request lines in DPI flow and returns `dmcaAllow` before the encrypted-traffic entropy heuristic fires, preventing false positives on radio/media streaming over non-standard ports.
- **Port bounds check** (PR #168): Fixed CodeQL alert #11 where `strconv.Atoi` return values on 64-bit systems could hold values outside the 0–65535 port range. Added bounds guard so invalid ports fall back to `defaultAPIPort` instead of silently wrapping.

---

## [v3.23.0-fix.24.19] — 2026-06-26

### Performance
- **Proxy summary command** (PR #151): New `proxy summary` command shows a fleet-wide overview — total proxies, up/connecting/degraded/dead counts, source breakdown (file vs URL vs internal), URL source URLs with cached/blacklisted counts, provider start time, and file paths. Reads from on-disk `proxy_health.state` and `proxy_url.json` — no provider restart required. Usage: `urnet-tools proxy summary`.
- **Hub dashboard performance overhaul** (PR #155, #156, #157): The hub dashboard now serves gzip-compressed responses, lazy-loads proxy details on demand (instead of embedding 45k inline rows), and auto-refreshes via lightweight JSON API calls instead of full HTML page fetches. Fleet-wide traffic sparkline chart and historical time series charts via uPlot.
- **Hub server-side optimizations** (PR #155, #157): Added per-IP rate limiting (60 req/min), stale node auto-eviction (15 min), gzip compression middleware, and N+1 query fix on startup (2 queries instead of 1+N).

### Added
- **Idempotent override.conf helpers** (PR #153): New `override_set_env` and `override_rm_env` shell functions ensure systemd Environment= lines in `override.conf` never duplicate regardless of how many times a toggle command is run. Fixes the bug where `toggle_lowmode off` used `rm -rf` on the entire override directory, silently wiping unrelated settings (RAMLOGS, REPORT_URL, etc.).
- **`urnet-tools report` command** (PR #154): New `urnet-tools report <url>` sets the hub report URL at runtime via `~/.urnetwork/report_url` without restarting the provider. `urnet-tools report` shows the current URL. `urnet-tools report off` disables reporting. Works for both native binary and Docker (PowerShell wrapper).
- **Hub UI overhaul** (PR #156): Completely redesigned hub dashboard with summary cards (Total Proxies, Healthy, Degraded, Earning, Active Clients), tab navigation (Nodes/History), filter bar with text search and status dropdown, fleet-wide aggregate RX/TX sparkline chart, historical time series charts with 24h/3d/7d ranges, and a slide-out drawer for per-node proxy details.
- **Dual-stage SOCKS5 + API reachability probe for URL proxies** (PR #152): The URL proxy fetch pipeline now tests both SOCKS5 protocol compliance AND API reachability through each proxy — on a single TCP connection, with DNS resolved once and cached. Proxies that can't reach `api.bringyour.com` through the SOCKS5 tunnel are caught at fetch time (within seconds) instead of wasting auth-rate-limiter slots and generating log noise.
  - Stage 1 (3s): TCP connect + SOCKS5 greeting
  - Stage 2 (5s): SOCKS5 CONNECT to api.bringyour.com:443 through the proxy
  - 100ms random stagger before each probe dial spreads 50 concurrent probes across ~5s
- **Background URL proxy reaper** (PR #152): A background goroutine re-probes cached URL-sourced proxies that weren't fully verified every 5 minutes. After 3 consecutive API reachability failures, the proxy is moved to a persistent blacklist.
- **Blacklist pruner** (PR #152): Blacklisted addresses automatically expire after 24 hours and are removed, giving them a chance to re-enter on the next URL fetch cycle. Pruner runs every 30 minutes.

### Fixed
- **URL-sourced proxies that pass SOCKS5 but can't reach the API** are now filtered at fetch time or within 15 minutes by the reaper. Previously these proxies would consume auth rate-limiter slots indefinitely and accumulate retry attempts (up to 51+ seen in production) with "network error reaching API" errors.

---

## [v3.23.0-fix.24.16] — 2026-06-25

### Fixed
- **`proxy clear` now wipes URL cache, source URLs, and systemd PROXY_URL** (PR #139, #140): `proxy remove --all` was clearing the internal config and `proxy.state`, but leaving `proxy_url.json` and systemd `Environment=PROXY_URL` untouched. The cached URL proxies and env var would re-populate sources on restart — the clear had no lasting effect. Now `proxy_url.json` is wiped entirely (cache, blacklist, source URLs) and `PROXY_URL` is stripped from any systemd override drop-in in `~/.config/systemd/user/urnetwork.service.d/`, with a systemd daemon-reload.
- **File-sourced proxies launch before URL-sourced ones** (PR #139): The proxy launch order at startup was non-deterministic (Go random map iteration), so file-based and URL-sourced proxies were interleaved — no priority for paid proxies. The proxy list is now sorted so file-sourced proxies get lower indices in the launch sequence, giving them a head start via `backoffPacer` before any URL-sourced proxies begin connecting.
- **URL fetcher waits for file-proxy warmup before first fetch** (PR #140): The URL fetcher was triggering 5-15 minutes after startup, before file proxy warmup completed (~17 min for 1000 proxies at 1s stagger). URL-sourced proxies started authing mid-warmup, competing with file proxies for auth rate-limiter slots. The URL fetcher now waits for warmup to reach `>90% up with <5 connecting`, with a 60-minute timeout fallback so a few slow file proxies don't block URL proxies forever.

## [v3.23.0-fix.24.18] — 2026-06-26

### Added
- **Multi-tier degraded proxy removal** (PR #145): `proxy remove-dead` now supports `--degraded[=<duration>]` to remove degraded proxies offline past a threshold, `--source=<url|file|internal>` to filter by source, `--yes` for non-interactive use, and `--preview` for dry-run. Previously only `dead` (never authed) and `inactive` (7+ days) were removable — the `degraded` category (authed once, now offline) was a blind spot that let zombie proxies clog the pool indefinitely.
- **Proxy activity dashboard** (PR #145): `proxy activity` opens a real-time terminal dashboard showing active proxies carrying client traffic, per-proxy bandwidth rates, client counts, and recent contract acquisition events. Auto-refreshes every second. Press `q` or Ctrl+C to exit.

### Tests added (pending next release)
- **HMAC dual-format verification test** — regression test confirming `ContractManager.Verify()` accepts both legacy (pre-July 1) and standard (post-July 1) HMAC formats, ensuring the platform cutover doesn't break provider contract verification.
- **Startup sort order test** — verifies file/internal proxies sort before URL-sourced ones.
- **backoffPacer edge case tests** — zero stagger returns immediately, context cancellation aborts non-zero stagger waits.
- **Warmup gate lifecycle test** — URL proxies deferred during warmup are launched once warmup completes.

## [v3.23.0-fix.24.14] — 2026-06-25

### Fixed
- **`urnet-tools update -f` now reliably restarts the provider** (bootstrap + FORCE fix): Two bugs fixed: (1) The `-f` flag after a positional arg (`urnet-tools update -f`) was parsed by the subcommand handler which set `force_update=1` (lowercase) for version comparison bypass, but never set `FORCE=1` (uppercase) — so `stop_systemd_units` always took the non-force path and never stopped/restarted the service. (2) The bootstrap chicken-and-egg fix ensures the tarball-bundled `urnet-tools` is picked up even by old scripts via a check in the common script-writing section.

## [v3.23.0-fix.24.13] — 2026-06-25

### Fixed
- **`urnet-tools update` bootstrap self-update fix**: The self-update code previously had a chicken-and-egg problem — the fix for fetching the latest script was in the script determination code, but the old script (which runs the update) never reached that code path because it just did `cat "$0"` (read itself from disk). Now, **even the old script** will pick up the bundled `urnet-tools` from the freshly-extracted tarball immediately before writing, because the check lives in the common script-writing section after `cd "$workdir"`. This means: (1) the script self-update works on the very first update from any previous version, (2) `-f` restart-logic propagates correctly because the new script gets installed, and (3) future updates from v24.13+ work without any bootstrap issues.
- **`urnet-tools update -f` now reliably restarts the provider**: The `stop_systemd_units` function correctly checks `$FORCE` (set by `-f`) and stops the service, and `install_systemd_units` starts it back. This was already correct in the script logic but was blocked by the same chicken-and-egg — the old script's behavior was running, not the fixed one.

## [v3.23.0-fix.24.12] — 2026-06-25

### Fixed
- **`urnet-tools update` now fetches latest script from GitHub** (PR #136): Removed the `[ -n "$URNET_INSTALL_URL" ]` guard that blocked the GitHub fetch during normal `urnet-tools update` — that env var was only set for dev/testing overrides, so updates silently reinstalled the old script from disk. Added tarball-bundled script as highest-priority source. The script is now bundled in the provider tarball (`release.yml`), enabling offline-capable update.

**Note**: v3.23.0-fix.24.12 has a chicken-and-egg bootstrap issue — the fix can't propagate to existing installs because the old script running the update never reaches the new code paths. Use v3.23.0-fix.24.13+ which fixes the bootstrap by checking `$workdir/urnet-tools` in the common script-writing section.

## [v3.23.0-fix.24.11] — 2026-06-25

### Added
- **Important log buffer** (PR #135): New 1 MB `/dev/shm/urnetwork-important.log` secondary RAM buffer holding only high-value lines (`[profit]`, `[earn]`, `[health]`, `[outage]`, `client_id`, `instance_id`, `Permanently removed`, `[proxy][authrate]`) so the earnings/health status survives for hours even when the main 5 MB ramlog floods in ~84s. High-volume lines (per-proxy enumeration, per-attempt auth, `give-up`, `[net][s]select`) are deliberately excluded. `urnet-tools logs -i` / `--important` streams it. `isImportantLogLine` is a tested pure function.
- **Consolidated `[profit]` line** (PR #134): Single greppable line printed on each health heartbeat: `[profit] earning=yes|no reason=-|warmup|no_proxies|idle|no_traffic clients=N rate=X MB/s proxies_up=N serving=M idle=K`. Reason tells operators *why* when not earning without cross-referencing other lines. `[earn]` and `[traffic]` line formats left unchanged.

### Changed
- **Unified `logThrottle`** (PR #135): Four byte-identical `shouldLogX` rate-limiters (auth/select/write in `transport.go`, oob in `transfer_contract_manager.go`) collapsed into one reusable `logThrottle` type (one-line-per-interval + suppressed counter). `shouldLogX` stay as thin wrappers; call sites are unchanged.
- **Dropped per-proxy enumeration on reload** (PR #134): `reload()` no longer prints one line per added proxy address. The `+N added, -M removed` summary and the cold-start `Using N proxy servers:` roster are kept.

### Fixed
- **URL-sourced proxy give-up backoff is now enforced at launch** (PR #133): The escalating backoff (PR #129) computed a 15m→24h retry delay and scheduled a one-shot `time.AfterFunc` reload, but nothing gated relaunch — `reload()` re-added every desired-but-not-running proxy immediately (checking only `isDraining`), so dead proxies were relaunched every few minutes regardless of their schedule (proxies reached give-up 9 within 44 minutes of uptime; 70% of log volume was churn). `proxyFailureHistory` now records a per-address next-eligible time on each give-up, `reload()` skips addresses whose window has not elapsed and reports `N deferred (backoff)`, and `Reset`/`Prune` clear the window alongside existing counters. Frees auth-rate-limiter slots for live proxies in addition to cutting churn.
- **`urnet-tools -v` reports running vs on-disk version separately** (PR #132): `show_version` ran the on-disk binary's `--version`, so right after a plain (non-forced) `update` it reported the freshly-installed binary while the old image kept serving traffic, with no hint a restart was pending. It now resolves the running version through `/proc/<pid>/exe` (accurate even after the file is renamed to `.old` or deleted), prints separate **Running version** and **Installed on disk** lines, and warns with the restart command when they drift. Also catches pre-`--version` binaries via `(deleted)`/`.old` exe link detection.
- **`[c]ping err` log spam suppressed** (PR #131): Repeated identical `[c]ping err = Send sequence closed` messages during network outages now aggregate into one line per 5 minutes with `(N suppressed)` count. During the Detroit Hostodo transit outage this was producing 1,992+ identical lines over 2 days.

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
  - Tier 4 Extreme auto profile added for >= 8 GiB nodes (8 MiB windows, 16 MiB queues, 512 seq buf, GOGC 200, contract ramp scale 3)
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
