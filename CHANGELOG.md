# Changelog

## [Unreleased]

### Added

- **A FreeBSD host installs without curl** ([#1002](https://github.com/full-bars/urnetwork-3.23-fix/pull/1002)): FreeBSD ships `fetch(1)` in the base system and curl only as a package, so the dispatcher and the installer both failed on a host that had never installed one. Both now try `curl`, then `fetch`, then `wget`, and each stays nonzero on an HTTP error status, so a caller that treats a nonzero status as "no such asset" still works (the status value differs between the tools; only nonzero is preserved). This removes the downloader requirement only: the installer still needs `jq` or `python3` to read the `urnet-tools` digest, which the FreeBSD base system does not ship, so a bare host does not install (see the verification entry below).

### Fixed

- **The FreeBSD installer refuses an unverifiable `urnet-tools` and a damaged install tree** ([#1010](https://github.com/full-bars/urnetwork-3.23-fix/pull/1010), 91cbed7c1, 117754fda): the earlier installer could not read the release digest without `jq` or `python3`, printed a warning and installed the shell `urnet-tools` fetched from the branch instead. That fallback is gone on FreeBSD. The `urnet-tools-freebsd-<arch>` asset is installed only when its SHA-256 matches the digest the release publishes (checked with `sha256(1)`, then `openssl`); a missing digest, a failed download or a mismatch stops the install with an error, and nothing is renamed over the live binaries first. Because the base system has neither `jq` nor `python3`, a bare FreeBSD host now has to install one of them; the earlier claim that it does not is withdrawn. Before the rename the installer also makes the install tree root-owned without group or other write access and checks every file in it. A symlink inside the tree is not repaired or removed: the install stops with `symlink in install tree; refusing to repair`. The provider tarball itself is not digest-checked by this installer. Read from `scripts/Provider_Install_FreeBSD.sh`; not run on a FreeBSD host.
- **FreeBSD discovery requires an execute permission the invoking user holds** ([#1009](https://github.com/full-bars/urnetwork-3.23-fix/pull/1009)): discovery accepted any file with an execute bit set. It now asks the kernel with `access(2)` `X_OK`, and the root-owned `libexec` binary must also carry the other-execute bit because the service user is not its owner. The per-user execute test is skipped when run as root. The `FreeBSD verification` workflow runs the execute test as `nobody` on a FreeBSD 14.5 VM and it passed on `main` at 22c43f7f5 (run 41061967540); the stat test is covered by the same job. Neither has run on a physical FreeBSD host.
- **Every macOS update staged the Linux binary and then refused it** ([#1018](https://github.com/full-bars/urnetwork-3.23-fix/pull/1018)): `tarRelPath` in `internal/urnettools/update.go` had a case for `windows` and one for `freebsd` but none for `darwin`, so it fell through to `linux/<arch>/provider`. The update downloaded the universal tarball correctly, extracted the Linux ELF, and the Mach-O guard refused it, so no macOS update could finish. `darwin` now maps to `darwin/<arch>/provider`, which `.github/workflows/release.yml` packs for both architectures, and a test pins both. Unit-tested only; not run on a Mac.
- **A FreeBSD update no longer hands the root-owned tree to the service user, and root's auto-update runs from the staged copy** ([#1019](https://github.com/full-bars/urnetwork-3.23-fix/pull/1019), 3bbd80b03): `installBinary` chowned a binary swapped inside `/usr/local/libexec/urnetwork-provider` to the service user, which put code root later executes in an unprivileged account's hands, and root's crontab ran whichever `urnet-tools` invoked `auto-update` and logged into a user home. The chown is skipped inside that tree. Root's cron line now runs `/usr/local/libexec/urnetwork-provider/bin/urnet-tools` and logs to `/var/log/urnetwork/auto-update.log`; it is refused when that copy is missing, not a regular file, not owned by root or writable by group or other. A non-root user who asks to schedule an update for a binary in the tree is told to use `sudo urnet-tools auto-update <interval>`. Read from `internal/urnettools/lifecycle_freebsd.go` and `lifecycle_bsd_cron.go`; not run on a FreeBSD host.
- **The FreeBSD rc script's fallback process scan looks only at the service user's processes** ([#1019](https://github.com/full-bars/urnetwork-3.23-fix/pull/1019), 5a4b30e30): it listed every process (`ps -axo`) and matched on the binary path, so a copy at the same path run by another account could be reported as, or stopped as, the service. It now uses `ps -U <service user>`, in both the installer's rc template and the one `urnet-tools` renders.
- **The FreeBSD installer changes into the state directory before it chowns** ([#1019](https://github.com/full-bars/urnetwork-3.23-fix/pull/1019), 9de22a787): the `cd` ran in its own subshell and the recursive `chown -R -h` ran in the installer's working directory as root, so it re-owned whatever directory the operator launched the script from. The `cd`, a `pwd -P` check against the service home and a non-recursive `chown` now run in one subshell, and files the service user does not own are warned about, not followed.
- **The provider's proxy state is written under one lock, in one order** (SN release port, [#1013](https://github.com/full-bars/urnetwork-3.23-fix/pull/1013)): startup read and rewrote `proxy.state` without the cross-process proxy lock, so a concurrent clear or reload could be overwritten by the older snapshot; it now holds the lock across the read-modify-write and waits for a held lock instead of aborting the launch, after clearing the self-lock an in-place hot swap inherits. `proxy remove --match` writes the exclusion first and removes under the same lock, so a URL fetch cannot merge matching proxies back, and it aborts when the URL state is unreadable. `proxy remove --all` holds the lock across the config write, state reset and URL cache reset, reports an unreadable `proxy.state`, and signals a reload after every clear attempt. Shutdown signals the reloader to stop before it waits, with the stop channel initialised under a lock, so a queued reload no longer holds up exit until its timeout. `proxy add` to a source file keeps that file's mode and ownership, and proxy addresses with malformed or percent-encoded parts are redacted in logs.
- **Docker startup keeps a valid proxy configuration and redacts echoed credentials** ([#1014](https://github.com/full-bars/urnetwork-3.23-fix/pull/1014)): the start script ran `rm -f ~/.urnetwork/proxy` on every start before it looked for `/app/proxy.txt`, so a container started without the file lost the proxies saved on its volume. With no `/app/proxy.txt` the saved configuration is left alone. A mounted file is imported into a staging directory first and replaces the saved configuration only if the import produced valid servers; a failed import keeps a usable saved configuration and the container carries on with it, and if no usable configuration remains the container stops with `Proxy import failed and no configuration is saved; refusing to start without the mounted proxy`. With `DEBUG=true` the login output is replaced with `[redacted]` when it echoes the configured username or password (the match ignores case), and the debug lines print whether each is set and its length instead of the values, in the shared login loop and in the Pelican login. With `ENABLE_IP_CHECKER=true` the checker is downloaded to a temporary file and run only if the download succeeded; a failed download logs `IP checker fetch failed` instead of the previous false `IP checker script ran successfully`. Verified by reading `docker/scripts/provider_common.sh` and `pelican_panel.sh`; the container scripts were not run here.
- **The probe asks every address before it convicts a route** ([#996](https://github.com/full-bars/urnetwork-3.23-fix/pull/996)): each probe loop read only the first address a host resolved to, so a host whose first address failed was recorded as a proxy failure even when its second answered, and a v6-only target reached the v4 encoder, where a nil `to4` left the frame at atyp ipv4 with an all-zero destination and the proxy answered for `0.0.0.0`. The SOCKS5 frame now uses a family-aware encoder and carries the real 16-byte destination, every resolved address is asked once per loop and the host counted once, and a round where the limiter cut the address set short reports no evidence unless something positive was seen. Two dead probe hosts are replaced with reachable ones, and the reputation-class hosts stay out of the health table.
- **A stopped FreeBSD provider installed under libexec is found again** ([#1005](https://github.com/full-bars/urnetwork-3.23-fix/pull/1005)): discovery built its candidate list from the home directory, so a provider installed at `/usr/local/libexec/urnetwork-provider` was invisible while it was stopped. The root-owned path is first now and needs no home, and the old per-user paths stay as a fallback for installs made before the move.
- **A FreeBSD restart no longer erases the service log** ([#1007](https://github.com/full-bars/urnetwork-3.23-fix/pull/1007)): the `rc.d` start ran `install` over `/var/log/urnetwork/stdout.log` on every start, which truncates it, and the guard refused only a symlink, so a fifo passed and blocked the `daemon(8)` open while a hard link let the log write through to the aliased file. The log is created only when missing, a failed creation stops the start instead of being ignored, and the file is kept only when `stat -f '%HT %l %Su'` says it is a regular file with one link owned by the service user. A fifo, a file with more than one link or a file owned by someone else is removed and recreated before `daemon(8)` opens it as root. A symlink is not removed: the start stops with `Refusing to start: <log> is a symlink (privilege escalation risk)` and the operator has to remove it by hand. Read from the `rc.d` script the installer writes; not run on FreeBSD.
- **The paste prompt names the key that ends input on Windows** ([#1006](https://github.com/full-bars/urnetwork-3.23-fix/pull/1006)): the hint hardcoded Ctrl+D, which does nothing in a native Windows console where stdin ends on Ctrl+Z then Enter. It names both on Windows, because a pty there (Git Bash, MSYS) still ends on Ctrl+D and Ctrl+Z only suspends the process, and a unit test pins both branches. The prompt is also printed in a Git Bash or MSYS pty: Go hands a Windows pty to the process as a pipe, so the old character-device test hid the prompt and left the read blocking with no hint. A pipe counts as interactive only on Windows and only with `TERM` set, so a scripted run on Linux or macOS stays silent.
- **The RAM log trim stays off the writer's lock, and the descriptor swap is retried** ([#1003](https://github.com/full-bars/urnetwork-3.23-fix/pull/1003)): a failed reopen left the reader on an unlinked file that nothing would trim, and the fixed temp name could be blocked in a shared `/dev/shm`. The swap is retried every pass, the temp file is unique, and the tail hints use `-F` so they follow the name after a rename.
- **The URL fetch failure reason stays in the important log** ([#997](https://github.com/full-bars/urnetwork-3.23-fix/pull/997)): the cycle summary says `N failed`, but the cause — timeout, DNS, status code — is on the per-source `fetch failed for` line, which the important log did not keep and the ring rolled out within minutes.
- **An extender source that asks again at once is answered** (bc24ec5c): the in-flight slot was freed after the answer was written, so a source that answered and asked again immediately was refused as already having a query in flight. The slot is freed before the answer goes on the wire, the deferred release stays for early returns, and a test observes the slot at write time.
- **The Docker auth-code path passes the code to the binary** (044280bc4): both initial authentication and re-authentication now pass `URNETWORK_AUTH_CODE` to `auth`.

### Changed

- **The contract log line separates the verb from the proxy** (7fa8f52f8): the proxy tag was joined to the verb with a single space, so the line read as one phrase. `[contract] acquired`, `denied`, `closed` and `checkpointed` now read `[contract] <verb> | proxy[<index>] (<address>)`, for example `[contract] acquired | proxy[3] (203.0.113.22:1081) size=256 KiB destination=...`. The direct slot reads `proxy[0] (direct)`. A script that matched the old `acquired proxy[` text needs updating; `[contract] acquired` and `[contract] closed` still match as prefixes, as the provider's own check does. Documented in `LOG_REFERENCE.md`.
- **`--i-am-on-my-own-workstation` is removed** ([#1017](https://github.com/full-bars/urnetwork-3.23-fix/pull/1017)): the override is gone from `urnet-tools wallet`, and passing it now fails with a message that says it was removed, so an old script stops instead of being silently accepted. On a box that looks like a fleet node, the `--coldkey_seed_file` form asks a human at the terminal and refuses when nothing can answer. `wallet sign` is no longer gated, because the seed stays in the browser extension. The help text and the Bittensor operator docs no longer mention the flag.
- **The gossip vantage stream test is bounded and synchronized** (SN release port, [#1013](https://github.com/full-bars/urnetwork-3.23-fix/pull/1013)): `TestGossipFeedStreamIsBoundToTheClientVantage` preloads 32 records with a pinned partition secret and subscribes two foreign prefixes dealt different partitions, and every batch is checked per client against the directory's own sample in lockstep with the revocation marker. Test only.
- **Every image reports a dated `app_version`, not only a tag ref** ([#999](https://github.com/full-bars/urnetwork-3.23-fix/pull/999)): only tag refs computed the upstream-style version, so a `:main` or `:sha` image sent its raw tag to the backend, which cannot bucket it by date. The dated version is computed for those builds too, and a ref outside the fix scheme still falls back to the plain tag exactly as before.
- **The CFAA and IP blocker tables refreshed** ([#1000](https://github.com/full-bars/urnetwork-3.23-fix/pull/1000), [#1004](https://github.com/full-bars/urnetwork-3.23-fix/pull/1004), [#1008](https://github.com/full-bars/urnetwork-3.23-fix/pull/1008)): three upstream syncs since v3.23.0-fix.35.1. The counts below are read from `ip_security_cfaa_block.go` and `ip_blocker_block.go`: the CFAA table has 48,764 IPv4 ranges (47,632 at the tag) and 539 IPv6 ranges (619 at the tag), and the IP blocker retains 164,781 blocked-host records (164,225 at the tag). The generated files carry each feed's digest and parse counts in their provenance comment. Data only, and it takes effect when a node runs this release.
- **The install docs say what each platform's installer needs** ([#1001](https://github.com/full-bars/urnetwork-3.23-fix/pull/1001)): the Quick Start names the installer the one-liner detects, the FreeBSD example uses `fetch` instead of a `curl` pipe, and the host-requirements table lists what FreeBSD needs: `fetch` is in the base system, and `jq` or `python3` has to be installed for the `urnet-tools` digest.
- **The FreeBSD page and the Installation page agree on the hosted installer** (cbc27d01): `docs/FreeBSD.md` said FreeBSD had no hosted one-line installer and then documented `https://install.fullbars.xyz`. It now says the dispatcher detects FreeBSD and runs `install-freebsd.sh`, so the hosted one-liner and the mirror script install the same thing — the form `docs/Installation.md` already used. Both pages name an existing non-root account on the root-shell path, because a plain root login resolves the service user to root and the installer refuses that before downloading and never creates the account, and the FreeBSD page's prerequisites now match the installer: `fetch` is in the base system, `curl` and `wget` are optional, and nothing is installed for you.
- **The FreeBSD rc.d stand-in keeps the provider argv** ([#998](https://github.com/full-bars/urnetwork-3.23-fix/pull/998)): the stand-in CI uses in place of `rc.d` ran the provider without the argv the real script passes, so a test could pass against a start line the service never produces. It now starts the provider the way the script does.

## [v3.23.0-fix.35.1]

_Patch release on v3.23.0-fix.35.0: reliability and installer work on top of the extender release, led by the proxied H3 outage-egress fix, the degraded reaper holding off when the scores cannot rank the pool, a planned thrash restart no longer logged as a rejected sign-in, and a FreeBSD installer with its hardening. No operator action beyond the normal update and restart, and the extender, the Chrome hello and the blocker stay off unless set. Notes: `releases/v3.23.0-fix.35.1.md`._

### Added

- **A FreeBSD provider installer** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/979>): `docs/FreeBSD.md` told FreeBSD users to fetch `scripts/Provider_Install_FreeBSD.sh` from `main`, and that file did not exist, so the documented command returned a 404. The script now exists, adapted to this repo: it installs the per-architecture FreeBSD tarball the release publishes (`urnetwork-provider-<tag>-freebsd-<arch>.tar.gz`) and takes `urnet-tools` from the same release. The tag resolves through the GitHub API and falls back to this fork's mirror, the same pattern the Linux, macOS and Windows installers use. Provider files go to a system-wide location while service state stays in the service user's home, the service user can be overridden explicitly, installed files get restricted ownership and permissions, and the service refuses to start if its output log is a symbolic link.
- **FreeBSD is served from the universal autoinstaller** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/981>): the universal autoinstaller sent any `uname` it did not recognise to Unsupported OS, which included FreeBSD. The `dl` worker now routes FreeBSD to `install-freebsd.sh`, with the matching wrangler route and alias, and its landing text names Linux, macOS and FreeBSD and says the Linux path covers OpenRC. The `dl` worker deploys separately from the provider.

### Fixed

- **A silent platform edge cost proxied H3 nodes about 16 Mbit/s that the governor could not see** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/994>): with `h3` set to `all`, a count or `auto`, every proxy identity's H3, H3-over-dns and dns-pump carriers redial on their own timer, and a dial the edge never answers runs the whole QUIC retransmit ladder, about 33 KB. The governor counted no QUIC bytes and dropped canceled dials, and H3 never waited on the attempt budget, so the node stayed at `verdict=ok` while it spent a flat 15.5 to 16.4 Mbit/s for about 15 hours. H3 attempts now carry their handshake bytes, a canceled attempt's bytes count toward the failed-byte breaker without counting as an attempt, and a proxied identity's H3 waits on the shared budget like its H1. Nodes on the default `h3 direct` were not affected.
- **The degraded reaper culled half the pool when the scores could not rank it** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/986>): `scoreDegradedProxies` ranks by lifetime bytes and contracts, and when the platform is unreachable neither moves, so every candidate tied and the reaper still dropped half the pool (249 of 498 proxies on one node in the 2026-10-23 outage). A round where every candidate scored the same, or there are too few to rank, now holds off instead of culling.
- **A planned thrash restart was reported as a rejected sign-in** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/988>): exit code 75 has two producers, a failed sign-in that quarantines the child and the thrash watchdog's planned restart, and the supervisor only knew the first, so a thrash restart was logged as a rejected network sign-in. Every planned exit 75 (the thrash watchdog and the liveness self-exit) now writes a one-shot `thrash_exit.<pid>` marker beside `thrash_cap.json` just before it exits, and the supervisor consumes the marker for the pid it reaped. A genuine quarantine is still reported as one, including after a mid-life `thrash_cap.json` write. Only the operator log line changes: both branches make the same state changes, the exit code is unchanged so the systemd units, container start scripts and OpenRC keep restarting on it, and `docs/Troubleshooting.md` now lists both meanings of 75.
- **The memory-headroom warning fired too late on a host whose RAM it could not read** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/990>): `EffectiveRAMLimit()` returns 850 MiB with the source `fallback default, host memory unreadable` when it cannot read host memory, which the headroom threshold turned into its 150 MiB floor, so the warning fired far too late on such a host. The limit is now classified by its source, never its value: an unreadable host gets the 400 MiB cap, a real 850 MiB host keeps the floor, and the note explaining the threshold goes to the persisted log once.
- **A failed OOM-cap state write is reported, not discarded** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/993>): the startup OOM-cap decision wrote its state with the result discarded. That state carries the cap and the clean-window clock, so a lost write could let the next start read a stale window and relax the cap straight after a fresh OOM kill, and the applied path could ledger a cap that nothing enforces. Both branches now report the failure in the warnings, and the applied path leaves the timeline to the last cap that really was persisted.
- **`psi_some_avg60` never appeared in the baseline host sample** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/983>): the reader asked for a pressure resource named after the line (`some`), which does not exist under `/proc/pressure`, so the read failed and the omission rule dropped the field from every sample on every node. The call now asks for the memory resource, and a package-level seam pins the resource name so a rename cannot reintroduce a path that does not exist.
- **A hotkey wallet write failed on Windows** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/985>): after the rename the write flushed the parent directory, and Windows has no directory flush (`FlushFileBuffers` answers access denied), so the write was reported failed with the file already in place. The flush now goes through `internal/durablesync`, which absorbs only the errnos that mean the platform has no directory flush; every other failure still reaches the caller, and on Unix it is the same fsync as before. Verified on a Windows host, and the Windows job now runs the helper's tests.
- **Five extender review findings are acted on** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/980>): the feed read deadline is set before the cancellation hook is registered, so a canceled read cannot be blocked by a later deadline; a zero connect timeout stays unbounded on the http3 carrier, matching the tcp carrier, and an unbounded connect aborts when the context ends; issued-key pruning walks the table only when the epoch changes; service and refusal counts of retired servers fold into the base counters after the server has drained, once, so cumulative metrics keep their totals; and VLESS response-header parsing keeps the bytes already consumed, completes the header before reporting a read error, and resumes after a timeout once the caller refreshes the deadline. A started cancel hook is awaited before the feed stream is reused, so a late callback cannot arm an immediate deadline on the next read.
- **The installer refuses a symlinked service path, an unusable binary, and no longer needs python3** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/984>): `mkdir -p` follows a link and the `chmod` has no `-h`, so a symlinked home or state directory sent root somewhere else; the guard runs first in `do_install`, before the download, and covers a symlinked parent. A binary that is not a non-empty executable is refused before it replaces a working one. The release tag is read with `jq`, then `python3`, then a `sed` extraction, because python3 is not in the FreeBSD base system and a default latest install failed on a clean host.
- **The FreeBSD installer no longer follows a symlinked shell rc** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/987>): the installer runs as root, appends a PATH line to the service user's rc file and changes its owner, and both steps followed a symlink, so a link planted at `.profile` sent root's write to a file of the user's choosing. A symlinked rc is now refused with a warning (the install carries on, since the provider is already in place). For a real file the new content is built in a `mktemp` file beside it, owned, given the original mode and renamed over the target. `scripts/test_provider_install_shellrc_symlink.sh` lifts the block, checks the fix, and requires the unfixed version from `origin/main` to still write through the link, so it cannot pass vacuously.

### Changed

- **The disk benchmark samples by time, not a fixed 16 MiB** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/992>): the benchmark stopped at a fixed 16 MiB, which an NVMe writes in about 20 ms, so the rate reported at startup and in `urnet_disk_*` was a burst figure that moved between runs. The 3 s budget now governs, up to 256 MiB: microSD is unchanged at about 20 MiB, eMMC samples about 240 MiB, NVMe the 256 MiB ceiling in about 85 ms. The result carries its duration, so the log reads `900.0 MB/s (256 MB written in 85 ms)`. The container guard, IO-pressure skip, space cap, per-device cache and hung-write watchdog are unchanged.
- **The smart dialer costs nothing while it is off, and is now restart-required** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/989>): the idle preference's off state still cost a goroutine, a context and a timer per proxy, so a node with thousands of proxies carried thousands of idle goroutines. The start function is gated, so while the preference is off there is none. Because the loop now only exists when the preference was on at start, `smart_dialer` is reclassified restart-required: `set smart_dialer on` is persisted and takes effect at the next start, the CLI says so, and `set` now prints the same "takes effect at the next start" wording for `profile` and `ramlogs`, which never applied live. The default is unchanged: off.
- **The installer PATH-hint test runs in CI** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/991>): `scripts/test_provider_install_shellrc_symlink.sh` proves the installer's PATH hint never writes through a name the service user controls, and it never ran in CI. It now runs with full history, and the script fails, not skips, when its falsification pin is missing under `REQUIRE_FALSIFICATION`, so a shallow checkout cannot turn it into a silent pass.
- **The extender relay counter test bounds read counts** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/982>): the test pinned one read per write, but how the transport splits a write into reads is not the relay's to control, and the same session reported four ingress reads on one run and three on another. Byte counts stay exact and read counts are bounded between one per chunk and one per byte. Test only; it fixes a shard 2 flake.

## [v3.23.0-fix.35.0]

_Feature release on v3.23.0-fix.34.3: the provider can serve as an entry relay (extender v2) and dial through a VLESS/Reality carrier, the TLS hello can be switched to a Chrome-shaped one, the egress path consults the ad and tracker blocker, and a group of dataplane, reliability and observability changes come with it. The extender, the Chrome hello, egress TTL realism and the blocker are off by default._

### Added

- **The provider can serve as an entry relay (extender v2)** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/971>): the extender library and server with signed records, a trust chain, a directory and feed client, and tcp, quic and dns carriers, plus a feed and gossip composition layer and `host_sleep`. The serving role is opt-in through `URNETWORK_EXTENDER`, runs on a direct slot only, binds tcp 443 before quic 443 and dns 4053, keeps its own identity seed at `.provider.extender.key`, and exports `urnet_extender_*` metrics. Off by default.
- **A VLESS/Reality client carrier** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/971>): VLESS over tcp, TLS, Reality, WebSocket and HTTP upgrade, with share-link import and export, connection, request and admission limits, and relay statistics.
- **Lag tickets and measurement ids** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/975>): the protocol gains the lag ticket and report messages, and extenders are named by measurement id.
- **API additions** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/975>): opt-in auth request observations through the request context, wallet mapping consent calls, and a control-result error reply treated as a refusal.
- **Egress TTL realism and DoH dial observation** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/976>): the egress TTL can be applied or shadowed on the socket with counters for both modes (ships inactive behind the zero mode), what the resolver dial observed is recorded behind the dial context settings, and the H3 datagram auth negotiator is now actually called, so the connection negotiates the authentication shape the peer accepts.
- **Contract discovery reports outcomes and refusal reasons** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/977>): the contract metrics gain outcome and refusal-reason labels (`urnet_contract_attempts_total`, `urnet_contract_denied_total`), and a rate-limited log line names the reason a data path did not appear, instead of only acquired and denied counts.
- **The TLS ClientHello is a runtime switch** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/972>): `tls_hello`: `urnet-tools set tls-hello on|off` makes the normal and resilient dialers present Chrome's TLS ClientHello instead of Go's on new connections, live and without a restart, with `URNETWORK_TLS_HELLO` as the startup default. It affects new connections only: an open session keeps the handshake it made and follows the setting the next time it dials. The Chrome hello is **off by default** while it soaks, so a box that never sets the key keeps Go's hello exactly as before; a persisted `off` beats `URNETWORK_TLS_HELLO=on`, and clearing the key hands the decision back to the environment. The provider logs `⚙️ [control] applied tls_hello=<v> (was <old>)` on each change.
- **`urnet-tools proxy trim` reports the trim cap** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/965>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/966>): with no argument the command reports the effective cap and its source (operator, OOM or thrash), the configured pool size before the cap, how many proxies the cap holds back, and the live counts. `urnet-tools status` carries a `trim` row whenever a cap applies. The provider's effective cap wins over the `proxy_trim` file, and trim state is reported only from a complete pool, so an unreadable `proxy_url.json` cannot understate the pool.

### Fixed

- **A redirect could leak its headers and method into a later attempt** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/976>): the redirect policy is scoped to one request now.
- **URL proxies were held back for an hour behind proxies that never authenticate** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/973>): the warmup gate released on a ratio over every registered proxy, so a node with a dead proxy tail never crossed the gate and only a 60-minute backstop ended the hold, while the URL fetcher stayed parked behind the same flag. The hold is now bounded to five minutes from the start in both the reload gate and the fetcher, and the reload fires when the grace expires.

### Changed

- **The lifetime counters say when they began** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/967>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/969>): the store persists a start stamp written once, the `all-time` lines carry it, a pre-existing store reports an approximate start instead of inventing a date, and the scrape gains `urnet_lifetime_start_timestamp_seconds` and `urnet_lifetime_start_approximate`.
- **The CFAA and IP blocker tables refreshed** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/959>): blocked hosts 164,212 to 164,225, CFAA prefixes 46,733 to 47,632 IPv4 and 597 to 619 IPv6. Data only.
- **The live-numbers job signs its commits** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/968>): the scheduled cost-table refresh refuses to push an unsigned commit, runs twice an hour off-peak and on pushes to `main`, and the wiki mirror runs every time.
- **The client-id reuse checks restart the provider and fail on an empty store** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/955>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/957>): the smoke and soak checks stop and start the provider and diff the full stored client-id list instead of comparing one decoded JWT.
- **The documentation links at the wiki and drops the phantom commands** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/960>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/961>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/963>): wiki links replace raw markdown paths, the SN25 table shows dollar figures, and `urnet-tools auto-update daily|weekly|monthly|off` replaces the auto-update commands that never existed.

### Security

- **The packet path consults the ad and tracker blocker** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/974>): a destination in the blocker's ranges, or whose observed server names are in its host set, is dropped before the local-route, SMTP-guard and policy decisions, and the host lookup runs only when the ip range misses. The blocker is installed through `SetBlocker` by the device and shared with the dns path; nothing installs one on this fork yet, so traffic is unaffected until that wiring lands. The shipped tables are unchanged.
- **The blocklist generator is a Go program** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/975>): it replaces the shell pipeline, preserves CIDR masks, validates octets, parses the abuse.ch csv feeds, enforces per-feed minimum counts and subtracts reserved space. It runs only through `go:generate`, is never invoked by the build, and refuses a target without a do-not-edit marker unless forced.

## [v3.23.0-fix.34.3]

_Patch release on v3.23.0-fix.34.2: the pqe handshake stages and negotiated curves land with dashboard panels, `docker stop` reaches the shutdown path inside a container, lifetime session counters survive a proxy exit, the scheduled blocklist sync covers both tables, the SN25 cost table publishes from the chain, plus the Bittensor doc corrections. Notes: `releases/v3.23.0-fix.34.3.md`._

### Added
- **PQE handshake stages and negotiated curves** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/944>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/946>): the `[pqe]` line reports `attempts`, `tls_ok`, `verified` and `identity_failed`, plus a per-curve histogram. `/metrics` carries the same data as `urnet_sessions_handshake_total{stage=...}` and `urnet_sessions_curve_total{curve=...}`, and two dashboard panels show both rates. A zero classical count is now explainable instead of ambiguous.

### Fixed

- **A `docker stop` skipped the shutdown path** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/940>): a shell as PID 1 has no default action for SIGTERM, so the provider never heard the stop and Docker SIGKILLed the container at the grace timeout. The supervisor forwards the signal and waits for the successor.
- **The nightly updater could pick another platform's tarball** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/940>): the asset is now matched against this architecture.
- **A completed handshake could go uncounted** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/944>): an epoch that was superseded before verification returned early, so the attempt looked like a TLS failure. A failed identity proof could also be counted by every concurrent verifier. Both count once now.
- **`urnet-tools` could pick a release it cannot install** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/949>): a stable release that carries only the fat tarball was selected and then had no `linux-<arch>` asset, so an untagged update exited without installing anything. Releases without a per-arch asset are skipped when the caller needs one.
- **An api failure read as a missing tag** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/949>): `curl -s` returns success with an error body on 403 or 500, and the empty `tag_name` was reported as a tag that does not exist. The http status is checked now and the real failure is named.
- **A `docker stop` could exit before a hotswap successor drained** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/949>): the stop branch returned as soon as the tracked child was reaped, so PID 1 could exit while a successor was still draining and Docker SIGKILLed it. The exit hands over to the successor wait, which is bounded below the stop timeout.
- **Lifetime session counters dropped on proxy exit** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/946>): a `_total` could decrease and `rate()` read it as a reset. A departing manager's counts fold into a provider-wide accumulator first.

### Changed

- **The stop harness cannot signal a provider on the host** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/949>): it runs against a private app path, and the stale deny-list guard matches the old inline list as a fixed string.
- **The IP blocker table syncs with CFAA** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/947>): the scheduled job pulled one file, so the blocker table drifted unseen. Both files sync now, and a check fails on drift.
- **SN25 costs are published from the chain** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/941>): the head-tier cost table is regenerated on a schedule instead of promising a manual check.
- **Four Bittensor claims are corrected** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/945>): the v1 and v2 claim triggers, the quoted challenge message, the stopped-daemon caveat for unsigned v1 claims, and the consent scope.

### Security

- **The CFAA and IP blocker tables refreshed** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/951>): 41,202 to 46,733 IPv4 entries and 593 to 597 IPv6 entries, with blocked hosts moving from 163,972 to 164,212. Data only.

## [v3.23.0-fix.34.2]

_Patch release on v3.23.0-fix.34.1: the provider reports an upstream-style version to the backend, its metrics counters survive a restart, the build moves to Go 1.27.3 for the compromised-certificate block (CVE-2026-111943), the retired hub report commands are removed, plus the Linux installer distro fixes. No operator action beyond the normal update and restart is required unless a script or cron job still calls a retired report command or sets a retired `URNETWORK_REPORT_*` variable; remove those calls and settings. Notes: `releases/v3.23.0-fix.34.2.md`._

### Added

- **The provider reports an upstream-style `app_version` to the backend**: the connect auth now carries `YYYY.M.D-<code>-fix.N.M` (for example `2026.10.20-1076955500-fix.34.1`) instead of the git tag, which the platform filed under `other`. The date is the tagged commit's UTC date, the code is the house build code (`(seconds since 2023-05-23) x 10`), and `fix.N.M` is the release. Logs, metrics, hotswap, the dashboard label and the update checks keep the git tag. A build without the `main.AuthVersion` flag reports its plain version.
- **Cumulative metrics counters survive a restart**: the counters behind `/metrics` (`urnet_*_total`) are stored in `~/.urnetwork/metrics_counters.json` and restored at start, are written every 45 seconds and on a clean shutdown, and the HotSwap parent flushes them before it exits, so a restart, reboot or upgrade no longer resets a 24-hour attribution window. A crash or hard kill loses at most the counts since the last write.

### Security

- **The CFAA endpoint blocklist is refreshed from upstream**: 40,924 to 41,202 IPv4 entries and 546 to 593 IPv6 entries, across two syncs. Data only.
- **The build moves to Go 1.27.3** (`CVE-2026-111943`): Go 1.27.3 and 1.26.10 block eight compromised eMudhra intermediate CA keys in the certificate verifier. `go.mod`, the Dockerfile builder and every `setup-go` pin Go 1.27.3; only binaries built with it carry the block, so update to this release.

### Fixed

- **`urnet_doh_failures_total` no longer drops to zero on every scrape**: the exported counter was fed by a read that swaps the count to zero for the health heartbeat, so a scrape saw the series fall back to zero. A cumulative counter now backs it.
- **A restored total no longer shows up as a tick of traffic**: per-proxy rate and profit deltas skip a key's first sample, so a record that arrives carrying its persisted totals cannot appear at the lifetime rate.
- **The installer installs missing curl and JSON dependencies** through the host's package manager (apt, dnf, yum, zypper, pacman, apk), honors `URNET_NO_AUTOINSTALL=1` on every path, uses `yum` when `dnf` is absent (Amazon Linux 2), and no longer prompts under a non-interactive run.

### Changed

- **The VirusTotal release scan covers every artifact**: hash lookup before upload, one scan per identical file, `Retry-After` handling, large files through the upload URL, and `INCOMPLETE` reported when anything was not scanned.
- **CI runs the real installer on Debian 12 and 13, the RHEL family and Alpine**, and release publication waits for that matrix (report-only until two clean tag runs).
- **Examples and comments use placeholder hosts** instead of real hosts and home directories.
- **The Bittensor guides are rewritten** (Quick Start, Head Tier, Hotkey Delegation, Operations, Fleet, Claim Daemons, Glossary) and re-checked against current upstream: every `wallet status` result, the settlement v2 schedule, the v1 claim window, the mainnet flag pairs and the commitment oracle status.

### Removed

- **The retired fleet hub's report commands and settings**: `urnet-tools report`, `urnet-docker report`, the `report-url` and `report-interval` settings, `URNETWORK_REPORT_URL`, `URNETWORK_REPORT_INTERVAL` and `URNETWORK_HEARTBEAT_INTERVAL` and the reporters behind them are gone. Nothing received the reports after the hub was removed in v32.0. A saved `report_url` is ignored.

## [v3.23.0-fix.34.1]

_Patch release on v3.23.0-fix.34.0: the v34 review follow-ups plus a CFAA blocklist refresh. No operator action beyond the normal update and restart. Notes: `releases/v3.23.0-fix.34.1.md`._

### Fixed

- **Plain-DNS resolver rotation is per lookup, not per process**: the fallback resolver picked its server from a counter shared by the whole cache and advanced once per attempt, so a concurrent lookup could move it between another lookup's two attempts and both could land on the same address family. Each lookup now seeds its own attempt counter from the shared one, which advances once per lookup, so one lookup's attempts walk consecutive entries of the operator-paired v4/v6 list while lookups still spread across the servers. DoH stays primary and only the plain-DNS fallback changes.
- **A network-refresh 401 is terminal**: the hourly renewal no longer falls back to the legacy code-create/code-login flow on HTTP 401, because that call would present the same dead token, fail the same way and mask the real error. The refresh fails, the current JWT stays in place and the next hourly pass retries. A 403, the other 4xx refusals except 408, 425 and 429, and the missing-route statuses 404, 405 and 501 still fall back.
- **The UDP shared-socket poller returns the rest of the batch when a drain is canceled**: a refused enqueue returned only its own packet, so the packets behind it in the same batch, one pooled buffer per fragment of an oversized datagram, never went back to the message pool. The drain now returns the remainder of the slice. The poller stays inert: no shipped setting sets `SocketReadShardCount`. The head-of-line coupling of the shard's single drain loop and the zero-length-datagram end-of-drain rule are documented.
- **Docker public-IP detection no longer masks curl's exit status**: `docker/scripts/provider_common.sh` split the declaration and the assignment of `URNETWORK_PUBLIC_IP` before the export, so `export` no longer swallows the command's status (shellcheck SC2155). The detected address and the log line are unchanged.

### Changed

- **The reliable handoff timeout default is 0, not -1**: the field is carried for a later wiring decision and has no effect yet, because no production caller reads the selector and the receive path still selects its handoff wait with `ClientSettings.BufferTimeout`. The comment now says a negative value must not become the default until the receive path is wired to it deliberately. No behavior change.
- **Windows egress fail-closed behavior is documented** (comment only): a v4 egress index with `index6 == 0` fails IPv6 dials on purpose, since there is no v6 index to pin to, and the early return after the first successful option is safe for IPv4 destinations.
- **The normal strategy dial is pinned to the same egress control** (test only): the dial reaches the network through `ConnectSettings.DialContext`, whose plain branch builds `NetDialer()` and installs `DialControl`, so the forced-interface egress control still runs on the socket. The new test fails the build if `NetDialer`'s `Control` field is dropped. No production change.
- **The CFAA egress blocklist is refreshed** to 40,924 IPv4 ranges (from 41,513) and 546 IPv6 ranges (from 575), data only.

## [v3.23.0-fix.34.0]

_Final release: everything in rc2 plus the post-rc2 fixes below. Notes: `releases/v3.23.0-fix.34.0.md`._

### Added

- **Contract lifecycle logs include the proxy identity**: every contract lifecycle line names the proxy that carried it, so a close, an OOM, or a denied contract is attributable to a slot without cross-referencing ids.

### Fixed

- **The network switch notice only appears when the effective network changes**: `choose_network main` or `--reset` with no saved network, and re-saving the same URL pair, no longer print the "jwt was issued by the previous network" warning. The notice compares the effective `api_url` before and after, so a real switch still warns and a single-use auth code is not spent on a false one.

### Changed

- **OpenRC smoke CI declares read-only token permissions** and disables checkout credential persistence.
- **The CFAA egress blocklist is refreshed** to 41,513 IPv4 ranges and 575 IPv6 ranges (data-only).
- **The host and Docker shakedowns can run together**: per-workflow concurrency groups plus a 0-30s jittered and retried JWT mint, so a tag push no longer cancels one of them.

## [v3.23.0-fix.34.0-rc2]

_Release candidate superseding rc1 with the two changes that merged after rc1 was tagged. Notes: `releases/v3.23.0-fix.34.0-rc2.md`._

### Fixed

- **A proxied TLS handshake never strikes or retries a family**: `recordControlFamilyStrike` rejected a `proxyDialError`, but a handshake over a proxy fails with a plain timeout, so a proxied stall still demoted the proxy socket's family and then retried through the same proxy. The dial now checks `dialUsesProxy` at every decision: no family strike and no family retry for a proxied handshake, with two regression tests.

### Changed

- **CI enforces the modern-provider gate on every build**: `build.yml` runs `modern_provider_gate_test.go`, so a release cannot lose the PQ/window capability evidence without the build failing.

## [v3.23.0-fix.34.0-rc1]

_Release candidate: the v2026 engine transplant plus the post-slice fixes. Notes: `releases/v3.23.0-fix.34.0-rc1.md`._

### Fixed

- **The governor restores its learned baseline in shadow mode**: `InitGovernState` returned early unless the mode was enforce, so a shadow process started every restart with no persisted baseline and judged windows against the plain 20% threshold. Shadow now restores the baseline and keeps learning it, so its `baseline` and `threshold` fields match the enforce decision. The outage restart marker stays enforce-only because only enforce acts on it.
- **A local teardown is never governor evidence**: the H1 and H3 loops now skip `recordAttempt` when the attempt's local context has ended, matching the existing dial-failure carve-outs, so a cancelled dial is not counted as an early death.
- **First-send counters require a successful write**: `recordTransferFirstSend` moved inside the success branch, so a write that never reached the wire no longer counts as a first send; the resend path already behaved this way.

### Changed

- **The standalone transfer budget is lock-free, and `Reserve` charges a hierarchy instead of panicking**: standalone reserve/release no longer take the root admission lock, an exact-equality hierarchy now charges its full amount rather than underflowing into a panic, the waiter grant scratch is reused across releases, and capacity waiters wake in FIFO order.

## [v3.23.0-fix.33.2]

_Docker-only patch: the v33.1 image could not be built; the provider and urnet-tools binaries are identical to v33.1. Notes: `releases/v3.23.0-fix.33.2.md`._

### Fixed

- **The v33.1 Docker image can be built and published**: the nightly Pelican egg check reads `provider_common.sh` again after the entrypoint consolidation, and the provider package timeout is raised past its roughly 446-second race run. Both are CI-only changes; no provider binary changed.

## [v3.23.0-fix.33.1]

_Release: the v33.1 batch. Notes: `releases/v3.23.0-fix.33.1.md`._

### Added

- **`h3 auto` sizes the fallback transports from memory, off by default**: a new value for the `h3` control key (`urnet-tools set h3 auto`). It applies to the fallback set for proxied identities; the direct identity keeps the `direct` policy and the beside-H1 auxiliary that `on` means is never enabled. Capacity is one proxy slot per 64 MiB of the effective memory limit, zero below a 2 GiB limit, capped at 64. The target grows 4 proxies per 30 s pressure sample below 0.30 smoothed memory pressure, holds from 0.30 to 0.50, sheds to 16 at 0.50, and drops to direct-only at 0.75. With the key unset the controller returns before reading a sensor and behavior is unchanged. See `docs/H3-Auto.md`.
- **Network JWT renewal at `POST /auth/network-refresh`**: the hourly renewal tries the one-call route first. A 404, 405, 501 or a non-transient 4xx refusal falls back to the existing code-create and code-login flow. A 408, 425, 429, a 5xx or a transport error stays an error and leaves the current token in place. A response carrying a client JWT is rejected, and both success paths verify the new token against `/transfer/stats` before writing it.
- **Ad and tracker Blocker data with a new pepper, no caller**: the `connect` package gains the upstream Blocker subsystem and a generated table of 163,972 host records (about 0.94 MiB of read-only data) hashed with a new 32-byte pepper. `NewBlockerWithDefaults` starts disabled and nothing in the provider constructs or consults a Blocker, so runtime classification is unchanged. The CFAA table moves to 40,288 IPv4 ranges (from 38,753 in v33.0), a live classification change.

### Changed

- **The Docker start scripts share `provider_common.sh`**: `start_stable.sh`, `start_jwt.sh` and `start_nightly.sh` become thin wrappers that select the mode and binary flavor, and the shared bootstrap, login and supervision logic lives in one file. `BUILD`, `AUTH`, credentials, `TURBO` and `PELICAN` are byte-compatible on the offline equivalence matrix, and the exit 75 and 78 loops keep their existing behavior. The v33 operator variables still map through `provider_env.sh`, and with every one unset the container runs exactly `provide`.
- **The updater pages the release listing and skips releases it cannot install**: `urnet-tools update` and the Docker update paths walk `GET /releases` newest first, one hundred per page up to ten pages, and select the first release that is not a draft, not flagged as a pre-release, not named like a pre-release (`rc`, `alpha`, `beta`, `pre`, `preview`, `nightly`, `snapshot` or `dev` as a tag component), and publishes `urnetwork-provider-<tag>.tar.gz` with a sha256 digest. An explicit `--tag` remains an operator override and is not filtered this way.

### Fixed

- **sn durable-head publication (sn fork only)**: the sn fork keeps prepared bytes across a cancelled publication, anchors the pending checkpoint before the bytes, and abandons only a zero-byte pending temporary in `Reconcile`. The create marker is transient head metadata during restore and rebind, never a signed member. This fix is on the sn fork's `fix/durablehead-cancel-window` branch and is not part of this repository's provider binaries.

### Design only

- **Stage B `--adopt-legacy-provider-key`**: design note only. There is no flag, no gate and no change to `provide` or `auth-provide` in this batch. Plain `provide` and `auth-provide` keep their current identity behavior.

## [v3.23.0-fix.33.0]

### Added

- **Resizable transfer windows advertised**: every Ack the receiver sends carries `receive_window_byte_count`, the out-of-order hold capacity it enforces, from the first Ack on. The platform's capability probe reads this, together with post-quantum end-to-end encryption, to class a provider as modern for the Quality and Speed tiers. Receive memory is unchanged. Stable releases up to 32.10 do not advertise it.
- **`provider fleet publish` writes the tagged `urfleet1` commitment**: the tag followed by the 32-byte manifest hash, the only fleet format the platform's commitment oracle mirrors. An unresolved publication recorded in the older untagged format is closed from its original bytes before the tagged write. A fleet whose commitment was mirrored before the tagged format must republish with this release before about block 9,360,673 or its bindings stop.
- **`provider fleet publish` refuses validator and subnet-owner hotkeys**: the signing hotkey is checked at the finalized block it signs against, on the pinned publish, the recoverable publish and recovery replay. A read error refuses, and there is no override.
- **`provider snclaim`**: the settlement-vault submission tool that sn ships as a separate `snclaim` binary, reachable as a subcommand (`submit`, `status`, `withdraw`, `v2 status|claim|check`). `withdraw` retries a v1 claim credit whose payment was deferred; the payment always lands on the coldkey. Everything after `snclaim` is passed to it unchanged.
- **`provider choose_network --show`** prints the network in effect, the saved one or the main network. `choose_network` now warns when a saved URL is unencrypted (`http://` or `ws://`) and when a jwt from the previously selected network is still in the state directory, `network.json` is written atomically, and an incomplete saved file is a clear error instead of an empty base URL.
- **The Bittensor mainnet commands** (`provider claim`, `claim-daemon`, `claim-daemon-v2`, `wallet hotkey challenge|set|status`, `fleet manifest|register|publish|bind|sign|status|revoke`, `sn-status`, `operators` and `provide --all-operators`) are described in the release notes and the Bittensor wiki pages.
- **Fragments from tunnel clients are reassembled at the provider**: a fragmented UDP datagram (IPv4 or IPv6) is reassembled before the parse, the security policy runs once on the whole datagram, and the NAT receives the complete datagram, which is sharded by its real flow. Before this the parser rejected every fragment and the datagram was dropped without a trace. Only UDP is accepted; fragmented TCP is still dropped. The provider keeps at most 16 KiB of in-flight fragments per family.
- **ICMP echo through the provider, on by default**: a tunnel client can ping through a provider. The echo request is parsed, goes through the same security and CFAA policy as UDP and TCP (private, loopback, link-local and special-use destinations are refused), and is sent from an unprivileged ICMP socket; the reply returns to the source, never larger than the request. Flows are capped across the whole NAT (64 per client, 128 overall), idle ones are reaped after 60 s, and a failed or oversized send drops that packet and keeps the flow. `URNETWORK_ICMP=off` turns it off. A kernel that refuses unprivileged ICMP sockets (`net.ipv4.ping_group_range` excluding the service group) turns it off for that address family with one log line and affects no other traffic. The client send path stays off.
- **Client dial-failure gates**: the multi-client consumes the provider's dial-failure answers (RST for refused, ICMP unreachable for fast failures). A provider that keeps failing dials is warned and the window moves on, but only when the strikes span at least two distinct destinations, so one dead address cannot get healthy providers warned. The forwarded unreachable is built over the flow's SYN sequence so a Linux or Android stack accepts it. A provider's own RST is not counted as a successful connect.
- **An IPv6 flow keeps its shard behind an extension header**: an unfragmented extension chain hashes the ports at the transport header, like a plain packet.

- **Network wallet consent (`provider wallet challenge`, `provider wallet set`)**: the coldkey now signs the canonical prospective network consent, the statement release settlement pays, instead of the generic sign-in challenge (which is also a wallet login and is never paid). One signature covers every provider client of the network, current and future, and carries the earning-epoch interval (`--wallet-from-epoch`, `--wallet-through-epoch`; the default is the next epoch through 65,536 epochs). Every field of the statement is checked against the saved login, the epoch route and the coldkey before the key is loaded, the consent is retained before it is sent so a lost acknowledgement is answered by the identical original, and the server's receipt must name the same consent. Sign on your own machine; a fleet node needs nothing.

- **`provider wallet status`**: shows what the server holds for the network, with a consent (provider, network or hotkey scope, its epochs, whether it is in effect or starts in N epochs, and its receipt) apart from a login-proof wallet, which release settlement does not pay.
- **`provider wallet sign <coldkey>`**: serves one local page (127.0.0.1 only, session token, 15 minute lifetime, visible countdowns, URnetwork dark style) where a browser wallet extension (Talisman, SubWallet or any Polkadot `signRaw` extension) signs the verified consent statement. The seed phrase never leaves the extension, the page cannot make the program store a different message, and the result and receipt are shown on the page. See `scripts/wallet-consent/README.md`.
- **Docker auth onboarding (`AUTH=userpass|authcode`)**: container entrypoint accepts `AUTH=userpass|authcode` with `AUTHCODE` as an alias for `URNETWORK_AUTH_CODE`. Added local base64url decoding and JWT claim verification for `exp` and `nbf` (with 5-minute clock skew) before reusing stored volume tokens: expired or malformed tokens are deleted and re-authenticated when an auth code is available. Added startup warnings when `/root/.urnetwork` is unmounted or empty, and a payout wallet reminder for provider earnings.

### Changed

- **Docker onboarding documentation**: quick start rewritten around `AUTH=userpass|authcode` with named volume `urnetwork_data:/root/.urnetwork`. `BUILD=stable|jwt|nightly` is retained as a fully backward-compatible legacy selector.

### Removed

- **Retired vnStat and CGI stats from Docker image**: removed `vnstat`, `busybox-extras`, the stats CGI endpoint, and container port 8080. Provider containers run with zero listening ports by default. `ENABLE_VNSTAT` remains accepted as a compatibility no-op with a warning.

### Fixed

- **A stop during a claim-queue write no longer strands the queue**: the durable head marks a publication before it starts and writes the pending checkpoint only after the bytes are complete and synced, and a reconcile clears a crashed marker pair. A file no marker names is still refused, so unknown bytes are never admitted.
- **`provider wallet hotkey` acts on every authenticated operator**: targets come from the operator list snapshot, one failure never stops the others, and the command exits non-zero keyed off failed operators, not awaiting ones. A pre-operator install with no `operators/` directory keeps the single-network behavior.
- The client send path (`RemoteUserNatClient` and the multi-client) forwards packets the SMTP guard does not govern. It treated the guard's "not applicable" verdict as a refusal and dropped all ordinary client traffic, which the `-short` CI never saw because the end-to-end echo tests skip there. Provider nodes were not affected: they use the receive-side path.
- The seed file is read through the descriptor that was checked, closing a check-then-read swap window.
- The receive path releases its scratch references after a committed-prefix scan.

## [v3.23.0-fix.33.0-rc6]

_Sixth and final pre-release of the v33 line. This is a **test-only** release: the provider code is identical to rc5, built from `e9dd5b70d`; the only changes are in the release shakedown harnesses. It exists so the repaired checks run against a tag before the stable release. Pre-release, invisible to `urnet-tools update`. Notes: `releases/v3.23.0-fix.33.0-rc6.md`._

## [v3.23.0-fix.33.0-rc5]

_Fifth pre-release of the v33 line: rc4 plus the operator-facing completion work. The Docker container gains the `AUTH` selector with local JWT validation, the operator env passthrough and `ALL_OPERATORS`; the provider ports sn's all-operators surface with the typed 401 and sign-in quarantine, adds `provider snclaim` and `choose_network --show`, and fixes the durable-head publication in three places (the claim queue's cancel window among them); and the CFAA blocklist is refreshed for the v33 tag. The provider code here is the v33 release candidate (`e9dd5b70d`). Pre-release, invisible to `urnet-tools update`. Notes: `releases/v3.23.0-fix.33.0-rc5.md`._

## [v3.23.0-fix.33.0-rc4]

_Fourth pre-release of the v33 line: rc3 plus the Bittensor mainnet operator surface and a provider hardening pass. It adds the operators, fleet, claim-daemon and wallet-hotkey command groups with the Bittensor operations docs, serves `/metrics` by default (never on a public interface), and lands the TLS, stream and p2p accounting together with the transport, DoH, WebRTC, DMCA and wallet-journal fixes. Pre-release, invisible to `urnet-tools update`. Notes: `releases/v3.23.0-fix.33.0-rc4.md`._

## [v3.23.0-fix.33.0-rc3]

_Third pre-release of the v33 line: test-only fixes for the ICMP test isolation and the cancelled-sequence stats test on top of rc2; the runtime is unchanged from rc2. See the rc2 entry for that release's features. Pre-release, invisible to `urnet-tools update`. Notes: `releases/v3.23.0-fix.33.0-rc3.md`._

## [v3.23.0-fix.33.0-rc2]

_Second pre-release of the v33 line. It adds the receive-window advertisement the platform's modern-provider gate looks for, ICMP echo, the client dial-failure gates, provider-side fragment reassembly, and the network wallet consent commands. Pre-release, invisible to `urnet-tools update`. Notes: `releases/v3.23.0-fix.33.0-rc2.md`._

## [v3.23.0-fix.33.0-rc1]

> Pre-release of the v33 line, invisible to `urnet-tools update`. Try it on one box first.

### Added

- **Dual-stack platform transport**: the direct identity connects to the platform over IPv4 and IPv6 at once (one pinned transport per family plus a parked family-agnostic fallback that dials only if neither pin connects), on hosts with a usable global IPv6 address. A proxied identity stays on the single legacy transport. Switch it live with `urnet-tools set ip-version dual|ipv4|ipv6|legacy` (control key `ip_version`, default `dual`). Per-family `urnet_dualstack_*` metrics and `[ipv6]` log lines show each family's connects, drops and session lifetimes, and a pinned transport that connects over the other family is counted as a mismatch. See the Configuration and Monitoring pages.
- **Upstream's platform transport is swapped in**: the transport files are taken from upstream, with the fork's defaults and datagram safety kept. DNS and DNS-pump carriers (upstream's packet translation) are automatic fallbacks on the direct identity, and a SOCKS5 UDP relay plus a per-proxy path check carry QUIC for proxied identities.
- **The `h3` setting is `off|direct|on`, a proxy count, or `all`**: `direct` (default) runs the fallback transports on the direct identity only; a number or `all` widens it to the best N or every proxy. Widening relaunches the proxies it adds and each redial costs the platform's speed tests, so prefer a small N. `urnet-tools set h3 off|direct|on|N|all`.
- **H1+ is on by default again, with a live switch**: the authenticated custom carrier upgrade is tried on each H1 dial; `urnet-tools set h1-plus on|off` (control key `h1_plus`) turns it off for new connections. Losing H1 dials are closed, the outage guard is kept, a 429 or 5xx does not fall back to a plain websocket, and the framed-upgrade rejection cache is scoped to the route it was seen on. Counters and the carrier appear in the log and under `urnet_h1plus_*`.
- **QUIC datagrams are on by default, as upstream**: `h3_datagram` and `h3_datagram_send` default to `on` (`off` turns each off). The minimum message length limit is 16 KiB, as upstream, so the per-peer TLS handshake flights no longer end a session with `read limit exceeded`.
- **Credential renewal**: the provider renews its credential when the platform refuses it, and a transport holds its dials on a credential the platform definitively refused (a 401 or 403, or a revoked, expired or rejected close) instead of redialing it every few seconds.
- **An IPv6 extension-header walker**: the parse now walks the extension chain (hop-by-hop, routing, fragment, destination options) to find the transport header, so a datagram carrying extension headers is parsed instead of being dropped as an unsupported protocol. `ip_ipv6_ext.go`.
- **Cross-OS CI**: a build and vet matrix over the supported operating systems, native Windows and macOS runs of the ip and wallet tests, and the same tests on the FreeBSD kernel. `.github/workflows/portable-core.yml`.
- **An optional client key publication opt-out** (`ClientSettings.SkipClientKeyPublication`, library setting, default off): a delivery-only client can keep its identity key local. It is refused with a logged error whenever the per-peer session layer needs the published key.

> [!NOTE]
> Not in this release: fragments arriving at a provider are not reassembled (reassembly runs in the local NATs only), and ICMP echo is not supported.

- **The NAT reassembles fragmented IPv4 and IPv6 datagrams before parsing them**: a datagram split by the link was dropped whole, because a non-first fragment carries no transport identity and the parsers refuse a lone fragment. Each NAT send shard now reassembles only the datagrams pinned to it (by the IPv4 identification or the IPv6 fragment identification), retains at most 64 KiB of fragment pool capacity per family per shard (about 33 KiB of payload), and drops a datagram still incomplete after the reassembly timeout (15 s). A lone fragment is still refused. `ip.go:646`, `ip_nat_fragment.go:9`, `ip_fragment.go:28`, `ip_ipv6_fragment.go:35`.
- **A provider answers a source whose upstream dial failed, instead of going silent**: a refused TCP dial is answered RST+ACK, any other immediate failure with an ICMP destination-unreachable, and a timeout, a failure later than 3 s or a parent cancellation with nothing at all. The source learns in milliseconds instead of waiting out its own timeout. `ip_dial_failure.go:18`, `ip_dial_failure.go:73`.
- **Signed wallet consent (`provider wallet challenge`, `provider wallet set`)**: the coldkey proof no longer has to hand the provider a key. Sign the server's sign-in challenge here from `--coldkey_seed_file=<path>` (a 32-byte seed in a regular file the caller owns, mode 600, not a symlink), or print the challenge with `provider wallet challenge <coldkey_ss58>` and sign it elsewhere (`--message`/`--signature`). The provider refuses to sign anything that is not the documented three-line sign-in challenge and requires the text it signs to carry the challenge the server issued. `provider/wallet_consent.go`, `provider/wallet_consent_owner_unix.go:12`.

- **Backend outage protection (`URNETWORK_GOVERN`)**: when the backend is up but failing, every proxy used to retry the platform transport on its own, producing a flood of control traffic with nothing billable behind it. A process-wide governor now classifies every connect attempt, charges it its real dials and bytes, and decides over a 60 s window whether the backend is degraded (only evidence that the platform answered counts; dead proxies and silent timeouts never trip it). In `enforce` mode connects run under a shared attempt budget (open at 200/s, cut 4x per 5 s to a 2/s floor, doubled back as attempts connect), a failed dial gives up after one round, a byte breaker cuts the connect rate when failed attempts burn more than 256 KiB/s (the rate that would be about 22 GB/day; it does not cap bytes), and a restart during an outage starts throttled. It is **on by default** (`URNETWORK_GOVERN=enforce`); `shadow` and `off` remain as emergency overrides. See the Configuration page.
- **Automatic proxy removal waits for the backend**: the URL proxy reaper, the give-up eviction and the URL cleanup pass all held a proxy's failure record against it, and in an outage every proxy fails its API probe and its authentication, so a 24h blacklist could empty the pool just when it was needed. All three now hold off while the backend is degraded, and the reaper ignores a proxy that is still connecting or parked. New throttled `[proxy][outage]` line.
- **The outage verdict is judged against the host's own normal**: a chronically poor proxy list no longer reads as an outage. The usual share of good attempts is learned from healthy windows and kept across restarts (`~/.urnetwork/govern_baseline`); the budget reopens at that normal instead of a fixed 50%, so a poor-list host cannot sit at the floor for ever. An explicit 401/403 or client-limit close is a new `refused` class with no weight. The incident's own shape, the platform accepting the websocket then dropping it, now counts, but only for proxies that held a stable session before. A reconnect burst cannot teach the baseline that a high ratio is normal.
- **A start is never throttled by the outage protection**: for the first 90 seconds of the process the verdict is not trusted and the budget does not cut. A cold start or hot swap of a thousand proxies is a stampede whose failures are the host's own congestion, and the protection throttled one for about a minute on a healthy backend. The learned baseline is also seeded from three windows (the lowest) so a start burst cannot leave it far above the host's normal.
- **The H3 loop has the flap guard**, and a sole H3 transport's connects feed the governor, through one end-of-session helper shared with H1.
- **Better outage logging**: `[govern] budget cut ... reason=verdict|bytes` and `budget reopened` lines with the evidence, memory-pressure start/end lines, a found-restart-marker line, `baseline`/`threshold`/`refused` in the `[govern]` window line, speed test counts in `[health][govern]`, and a `LOG_REFERENCE.md` section for every outage-protection line.
- **Prometheus alert rules** for the outage protection: `UrnetworkBackendDegraded`, `UrnetworkFleetBackendDegraded`, `UrnetworkReconnectBudgetHeld` and `UrnetworkResendAmplification` (enabled, with unit tests), and an optional `UrnetworkSpeedTestStorm`.
- **Platform speed tests are counted**: every new transport is speed-tested by the platform (512 KiB flooded in twice and echoed straight back, about 2 MiB each way per connect on the proxy, no billable payload). New `urnet_speedtest_total` and `urnet_speedtest_echo_bytes_total` metrics make the cost visible. The echo is deliberately never dropped: a test that does not come back is retried by the server.
- **Reconnects are shed under memory pressure**: with under 7% of host memory available, or the Go heap within 8% of its soft limit, the connect budget sheds regardless of the backend verdict.
- **Recovery scouts are ranked**: while the budget is throttled the proxies with the best earnings history (file-listed and operator-configured ahead of URL-scraped) are released first, with every third slot random.
- **`[health][govern]` and `urnet_govern_*` metrics** show the mode, verdict, budget rate and waiting proxies.
- **A failed platform dial says why**: the bare `Timeout.` now carries how many dials failed over how many rounds and the last real error, for example `Timeout. 4 dial(s) failed over 2 round(s) in 15s, last: websocket: bad handshake (http 200)`.

- **A per-host attempt cap for failures the outage verdict cannot weigh**: the verdict only counts evidence that the platform answered, so a storm of SOCKS failures, silent timeouts, TLS errors, unclassified failures or refusals never cut the connect budget, and a large node kept making about one full connect attempt per proxy per minute for as long as it lasted. When more than 80% of the attempts in each of 2 consecutive minutes fail (any class, at least 20 attempts a minute), connects are now held to one attempt per second per 600 proxies, never below 2/s, until a 10 s window shows at least 20% of attempts connecting or a minute is no longer a failure storm. Nodes with fewer than 60 proxies are never capped, the cap never applies inside the 90 s startup grace, and like the memory-pressure hold it is a host hold: it does not count as a backend outage, so the reapers keep culling dead proxies. New `[govern] attempt cap on:` and `attempt cap off:` lines, and a `cap=` field on the `[govern]` window line while it is on. A large node whose proxy list is mostly dead can meet the 80% condition on a healthy backend; the best-history proxies are released first, so working proxies are the first to reconnect while it holds.
- **A crash loop no longer restarts into an unthrottled stampede every time**: the 90 s startup grace let a provider that died sooner than that run every start ungoverned, and since the budget never cut, it never left the outage marker either. Each start now appends a timestamp to `~/.urnetwork/govern_starts` (beside `govern_outage` and `govern_baseline`), and the third start within 10 minutes skips the startup grace and begins at the 2/s floor, ramping as attempts connect, with a `[govern] crash loop:` line. Hot-swap successors are not counted. Three deliberate restarts within 10 minutes (for example several `urnet-tools` toggles in a row) also count, and cost that start about half a minute of ramp on a healthy backend. Old starts age out, a damaged file is ignored, and an unreadable or read-only state directory only means no history. Enforce mode only.
- **The provider captures goroutine and heap profiles on the way into a stall**: a provider that runs away on memory stops answering within minutes, so there was never any evidence of what had piled up. It now samples its goroutine count and its heap against the soft limit every 10 seconds and, on a build-up (1.5 times the 30-minute low and at least 20,000 goroutines, heap at 90%, heap at 120%), writes `summary.txt` (the biggest goroutine stacks), the full goroutine profile, a heap profile and the numbers to `~/.urnetwork/incidents/`. At most 6 a day, newest 8 kept, disk only. `URNETWORK_INCIDENT_CAPTURE=0` turns it off.
- **The combined TCP-segment + TLS-record fragmentation dialer** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/792>): a new `fragment+segment` dialer cuts every fragmented ClientHello record a second time, across two TCP segments at an interior byte, so a middlebox that reassembles either layer alone cannot stitch the hello. The segment cut is best effort. It needs no raw sockets, so it works where the ttl reorder technique cannot (non-root Android, iOS network extension, non-Linux). It registers at the resilient tier (priority 50) beside `fragment` / `reorder` / `fragment+reorder` for direct connections only (not behind a SOCKS proxy), fails closed exactly like the fragment path, and changes no existing dialer's behavior. Port of upstream `3fd67304`.
- **A swap-thrash watchdog with a supervised restart** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/788>): memory pressure alone does not say "thrashing". The watchdog senses PSI memory `full`, swap activity, page refaults and direct reclaim, and tracks one state: `calm`, `under-pressure`, `thrashing`, `critical`. It first freezes pool growth; when the condition persists it restarts the provider by exiting with status 75, which the shipped unit (`Restart=on-failure`) turns into a supervised restart. The restart is capped at 3 per 24 hours with a growing backoff, is skipped while a hot-swap is in flight, and refuses to act when the swap belongs to another process. A restart leaves a thrash cap in `~/.urnetwork/thrash_cap.json` so the next start begins leaner — about 60% of the previous running count, at least one proxy, and no new cap when nothing was running before the restart. It rides the existing self-heal switch; with self-heal off it senses and logs but never restarts anything.
- **`urnet-tools status` prints the pressure summary sentence**: the live block's pressure row shows the provider's one-sentence summary (the `summary` the provider persists to `~/.urnetwork/pressure_status`), instead of only the score. Older providers keep the bare score.
- **The installer warns when a restart-policy override weakens the watchdog**: a drop-in with `Restart=no`, `Restart=on-success`, `Restart=on-abnormal`, `Restart=on-watchdog` or `Restart=on-abort`, a `RestartPreventExitStatus` listing 75, or a `SuccessExitStatus` marking 75 a success under `Restart=on-failure` would leave a detected thrash unrecovered; the installer now says so. The policy itself is never rewritten.
- **A container can use the thrash watchdog's exit 75 restart** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/803>): the watchdog's supervised restart used to be allowed only under systemd, so a container always declined it. A container (`/.dockerenv` or `URNETWORK_CONTAINER=1`) now counts as a supervisor like systemd and OpenRC, so self-heal is the one switch. Before exiting it proves the state directory holding `thrash_cap.json` can be written and read back; when it cannot, the escalation is refused as `persist-failed`, so a self-exit cannot loop without its throttle record. The write probe is bounded by the same timeout as the cap write, so a hung volume cannot stall the watchdog. The start scripts treat exit 75 as a planned restart.

- **The log reference covers the operator lines it was missing, and the wiki page is generated from it**: new sections for the swap-thrash watchdog (`[memory]`, `[proxy][thrash]`, `[thrash]`), hot restart and the identity store, hot-swap abort and heal lines, payload inspection (`[security][dpi]`), proxy audit, the sign-in retry and rate lines, and the maintenance lines. The wiki's `Log-Message-Reference` page had been a separate 305-line copy that lacked all of these; the wiki-sync workflow now publishes `LOG_REFERENCE.md` as that page and rewrites the links to wiki page names, and `../LOG_REFERENCE.md` links in the docs no longer break on the wiki.

- **A stalled provider is now recovered wherever a supervisor restarts it on exit 75**: on systemd a unit that sets `WatchdogSec=` is restarted when the provider stops pinging, but OpenRC's `supervise-daemon` and a container's start script have no such watchdog, so a provider that was alive and making no progress (a heap far over its limit, every loop stuck) was never restarted there. The same detection now runs in both with a different action: after 10 minutes without a tick the provider exits with status 75 itself, which the supervisor turns into a restart, leaving the same lean start cap and counting against the same 3 per 24 hours restart ring. Same self-heal switch and 30 minute startup grace: with self-heal off it logs what it would have done and keeps running. In a container the exit is refused when the state directory holding `thrash_cap.json` is not writable, so a self-exit cannot loop without its throttle record.

### Fixed

- **An oversized IPv4 UDP reply is one fragmented datagram again**: it was cut into several independent datagrams, which corrupts application framing wherever the payload is a single message (QUIC, DNS among others). IPv6 already fragmented at the ip layer; IPv4 now does the same, and a payload past the ipv4 packet limit is refused instead of split. `ip.go:1626`.

- **A slow backend no longer deletes saved client identities**: the revocation watcher counted a plain timeout as an auth failure, so during a backend episode it evicted valid identities (and their history) by the dozens per node and re-minted them. An identity is now evicted only after 5 explicit refusals (HTTP 401 or 403) from a healthy backend, never on a timeout or during an outage. A failed dial also no longer feeds the JWT renewal fast path during an outage.
- **Sessions that die seconds after connecting no longer reconnect in a tight loop**: a connect that succeeds resets the backoff, so a backend that accepts the websocket and then drops it made every proxy reconnect immediately. A session shorter than 60 s now holds the next connect with an escalating, jittered wait.
- **The degraded-proxy reaper no longer culls proxies that are down because of a backend outage**: it holds off during an outage and for 30 minutes after it.
- **`urnet-tools status` surfaces a stopped OpenRC service instead of hiding it**: on an OpenRC host the status view ran `rc-service urnetwork status` and threw its exit code away, so a service that had exited and either failed to restart or exhausted supervise-daemon's respawn limit (`respawn_max=10`) printed the same bare "stopped" as one the operator had stopped on purpose. A service that is stopped but is still registered in the default runlevel is now reported loudly with the likely cause and the next step, and a stopped service that is not enabled at boot gets a plain one-line hint instead. The `rc-service` output is still shown either way.
- **The incident capture no longer fires on the startup ramp**: after a restart the goroutine count climbs from about 14k to 21k over 40 minutes as the proxies authenticate, which is 1.5 times the window's lowest count, so the first real capture on the canary was only the ramp. The goroutine-growth trigger now waits 45 minutes after the start; the heap triggers are unchanged.
- **A provider that is alive but stalled is now restarted** (found on a 2 GiB single-core canary that sat wedged for hours, 90 percent CPU, no log output, self-heal reading "calm"): the garbage collector, running against a heap far over its soft limit with part of it swapped out, left no goroutine able to make progress, so none of the in-process responders ran and systemd saw a healthy unit. Two changes. A unit that sets `WatchdogSec=` is now fed `WATCHDOG=1` only while the pressure monitor keeps ticking; after 10 quiet minutes the ping stops, a lean start cap is recorded (60% of the running proxies, in the thrash restart ring), and systemd restarts the provider. And the thrash watchdog counts a heap at least 1.4 times its soft limit on a box short of free RAM as severe on its own, so the supervised restart can fire in the minutes before the stall, while PSI still reads calm. The feed starts at READY with a 30 minute startup grace, follows the self-heal switch and the thrash restart budget (3 per 24 hours), keeps a standing cap instead of compounding it, and takes the cap back out if the stall resolves itself. A heap-runaway episode on a box without PSI now relaxes when the heap recovers. The watchdog is off unless the unit sets `WatchdogSec=`; see the Configuration page.
- **A failed renewal replaced a client identity** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/802>): when a stored client token had expired and renewing it failed, `provideAuth` minted a new client and abandoned the old client id and the history attached to it, even when the failure was only a timeout or a dropped connection. A renewal that fails without a platform verdict (a transport error, a 5xx, a 408 or a 429, a cancelled context) is now transient: the identity is kept and the caller's backoff retries. A definite answer (the client does not exist, a mismatched client id, a permanent 4xx) still mints a fresh client as before.
- **`provider claim` hid the platform's refusal** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/802>): the platform answers a refused claim as HTTP 200 with an error object, and the client had no field for it, so the message was dropped and the command ended in `claim has no no_id`. It now prints the platform's own message, for example `No claimable epoch.`.
- **`urnet-tools` did not run on Windows** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/795>): the log views ran `tail -n 20 -f`, which Windows does not have, `urnet-tools start` set the provider's working directory before the state directory existed so Windows refused to launch it, and the sudo checks had no Windows branch. The commands now run natively. A native Go follower prints the last lines and then streams new output, and it is the fallback wherever `tail` is missing. It opens the log with delete sharing, so a rotation, a clear or an update is no longer blocked by a sharing violation. If the path starts naming a different file, the follower finishes the old one and follows the new one from its start, and a truncated log is followed from its start. It also opens paths longer than `MAX_PATH`, and a 64 KiB tail window that starts exactly on a line boundary keeps its first line.

- **Three `provider claim` rough edges** (follow-ups to the claim credential change): `--legacy-coldkey` now refuses an expired network token locally instead of sending it and reading an opaque server refusal; `--store-client=<address>` now finds the identity at that address even when the store key carries a proxy user (it refuses, without echoing usernames, when several accounts share the address); and a platform refusal prints `claim: the platform refused: <message>` and exits 1 instead of panicking with a stack trace.

- **A bytes or verdict cut no longer doubles straight back open**: while the budget was throttled, any 5 s interval in which fewer than 3 attempts reached the platform doubled the rate ("probe harder"). In an outage the classifier cannot see (silent timeouts, TLS or SOCKS failures) that is every interval, so a bytes cut reopened within seconds and the connect rate oscillated around the byte ceiling. For 5 minutes after a cut for `reason=bytes` or `reason=verdict` the no-evidence reopen now adds 2/s once every 30 s instead of doubling. Attempts that reach the platform and connect still reopen the budget at the usual pace, and with no recent cut nothing changes.

- **A restart into a backend that accepts and then drops every connection is now governed**: an early death counted against the backend only for a proxy that had held a stable session in the same process, so a freshly started provider whose every session died within seconds never had a single one count, and nothing throttled its reconnects. Early deaths of proxies not yet known stable now count as hard failures too once they are more than 30% of the attempts in each of 2 consecutive minutes after the startup grace. It needs a learned baseline (`~/.urnetwork/govern_baseline`, kept across restarts), so a host whose poor proxy list always dies young is judged against its own normal, and a brand-new host with no baseline behaves as before. The proven-proxy path is unchanged.

### Changed

- The `github.com/urfoundation/sn` pin is bumped to `v0.0.0-20261014223308-23d0bf9d92a4` (`go.mod`).
- Fork hook kept deliberately: `ip_ipv6_fragment.go` draws the IPv6 fragment identification from `crypto/rand` instead of upstream's process-wide sequential counter, which an off-path observer can predict and walk. The hook is re-applied at every sync of that file. `ip_ipv6_fragment.go:401`.

- **The provider's operator logs are readable sentences** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/788>): the pressure regime, pool target, trim receipt and apply, reload summary, health verdict, auth limiter and GC governor lines say what happened in plain words, with machine counters kept in a trailing `(...)` for grep. The new `[health] Verdict:` line summarizes state in one sentence. Dialer and connect lines are unchanged.
- **`provider claim` needs an explicit client credential** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/802>): the platform looks a claim up by the client in the token, and serves a network token only for an explicit legacy read of an epoch with no provider artifact. Claim takes exactly one of `--store-client=<key>` (a client token from the identity store), `--provider-jwt=<path>` (a client token in a file) or `--legacy-coldkey=<ss58>` (the network token plus a coldkey). With none it refuses and says why, because a node holds hundreds of identities and only one that served traffic has a payout. A token without a client id, and an expired one, is refused locally. Tokens and proxy keys are never printed. Scripts that called `provider claim` without options must now choose a credential.
- **`provider wallet set` and `provide --wallet` refuse the unsigned network wallet request** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/804>): the platform is moving wallet binding to a signed consent, and whether it still accepts the unsigned request is a server-side policy the binary cannot see. Both now refuse unless `--legacy-network-wallet` is given, and the message names the app or web account route. `provide --wallet` still starts providing after printing the refusal. A running provider that does not pass `--wallet` is unaffected.
- **The container start scripts treat exit 75 as a planned restart** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/803>): `start_stable.sh`, `start_nightly.sh`, `start_jwt.sh` and the provider loop of `pelican_panel.sh` counted 75 as a crash, which counted toward the three-crash JWT clear and slept 60 seconds. It now restarts after 5 seconds and leaves the JWT alone. Every other status is unchanged.
- **The hourly reconnect pulse is skipped during a backend outage**: once an hour every stalled transport had its backoff reset, which in an outage sent every failing proxy back into it within the same half minute. The pulse is now skipped, with one `[pulse] skipped hourly reset (backend degraded)` line, while the backend is degraded or the connect budget was throttled within the last 10 minutes, and the next hour tries again. When the backend is healthy, or with `URNETWORK_GOVERN=off`, nothing changes.

## [v3.23.0-fix.32.11-rc1]

_Pre-release tag, not a stable release._

### Added

- **Backend-outage protection is enforced by default** ([#824](https://github.com/full-bars/urnetwork-3.23-fix/pull/824), [#825](https://github.com/full-bars/urnetwork-3.23-fix/pull/825), [#826](https://github.com/full-bars/urnetwork-3.23-fix/pull/826), [#834](https://github.com/full-bars/urnetwork-3.23-fix/pull/834)): short-lived sessions back off, a shared attempt budget limits reconnects during a degraded backend, and failed dials stop after one round. The verdict uses the host's learned baseline; recovery scouts favor proxies with a stronger earnings history. `shadow` and `off` remain available as overrides.
- **Resend amplification is measured and bounded** ([#836](https://github.com/full-bars/urnetwork-3.23-fix/pull/836), [#837](https://github.com/full-bars/urnetwork-3.23-fix/pull/837), [#838](https://github.com/full-bars/urnetwork-3.23-fix/pull/838)): first sends and resends have separate counters, and a token budget defers excess resends without charging a send that did not happen.
- **Stalled providers recover under OpenRC and containers** ([#815](https://github.com/full-bars/urnetwork-3.23-fix/pull/815), [#816](https://github.com/full-bars/urnetwork-3.23-fix/pull/816), [#817](https://github.com/full-bars/urnetwork-3.23-fix/pull/817)): where the supervisor restarts exit 75, a stalled provider records its restart cap before exiting; containers must prove the state write is durable to avoid an unthrottled restart loop.
- **Outage status is observable** ([#830](https://github.com/full-bars/urnetwork-3.23-fix/pull/830), [#839](https://github.com/full-bars/urnetwork-3.23-fix/pull/839), [#844](https://github.com/full-bars/urnetwork-3.23-fix/pull/844), [#846](https://github.com/full-bars/urnetwork-3.23-fix/pull/846)): health metrics, budget and memory-pressure logs, speed-test accounting and alert rules expose the protection's decisions and reconnect cost.

### Fixed

- **Automatic proxy removal holds during backend outages** ([#827](https://github.com/full-bars/urnetwork-3.23-fix/pull/827), [#842](https://github.com/full-bars/urnetwork-3.23-fix/pull/842)): reaping, give-up eviction and URL cleanup stand down while the backend is degraded. Saved client identities are not removed for timeouts; automatic identity eviction is off unless explicitly enabled and requires repeated platform refusals while healthy ([#829](https://github.com/full-bars/urnetwork-3.23-fix/pull/829), [#838](https://github.com/full-bars/urnetwork-3.23-fix/pull/838)).
- **Startup failures no longer throttle a healthy recovery** ([#847](https://github.com/full-bars/urnetwork-3.23-fix/pull/847)): the first 90 seconds are excluded from the outage verdict and budget cuts, and startup samples cannot seed the learned baseline.

### Security

- **The CFAA blocklist refreshed** ([#848](https://github.com/full-bars/urnetwork-3.23-fix/pull/848)): 40,588 to 38,548 IPv4 entries and 569 to 562 IPv6 entries. Data only.

## [v3.23.0-fix.32.10]

### Added

- **A read-only grade for the direct path** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/776>): the provider grades its own local-IP egress, so an operator can tell a bad proxy from a bad box. It samples the same destination table as the proxy probe, with the same sampler. It dials each host at port 443 from the box's own address. The grade lands in `~/.urnetwork/direct_grade.json` and never in `proxy.state`, so no existing consumer can act on it. It runs on the reaper tick and needs the table probe and the direct transport enabled. It logs `[proxy][grade] direct:` on a tier change.

- **Providers declare a provide intent and hold their dials after a client-limit close**: every provider connection now says it is a provider (an `X-UR-Provide-Intent: 1` header on the HTTP/1 upgrade and a `provide_intent` field on the auth frame), and when the platform closes a connection because the network is over its client limit (WebSocket close 4001 or a QUIC application close 4001, preceded on HTTP/1 by a control message) the transport stops dialing for 15 to 20 minutes instead of reconnecting into the same refusal. All of a client's transports and its live sibling connections stop together, and the hold is saved to `~/.urnetwork/client_limit_hold.json` so a restart or a hot swap does not start another kick. An older platform ignores the declaration and never sends the close, so nothing changes against it. `URNETWORK_PROVIDE_INTENT=0` stops declaring; the hold still applies to a kick.

### Fixed

- **A failing auxiliary H3 no longer retries in lockstep or spins**: H3 keyed its accounting on the static gate and no longer takes part in the hourly global Pulse, but its failure backoff was plain doubling, so a fleet that failed together (a network that filters UDP, a fleet-wide start) still retried in synchronized waves. The wait is now spread by 20% either way, has a one second floor so an unset reconnect timeout cannot make a tight dial loop, and is cut short when the H3 gate is switched, so turning H3 off parks the transport at once instead of after a sleep of up to ten minutes.
- **OpenRC: the thrash restart and the stop schedule now work**: supervise-daemon sets neither `INVOCATION_ID` nor `NOTIFY_SOCKET`, so the thrash watchdog refused its supervised restart (exit 75) on every OpenRC host; the generated init script now exports `URNETWORK_INIT=openrc` and the provider treats it as a supervisor. The script also had no `retry=` stop schedule, so `rc-service stop` reported success while a hung or SIGTERM-ignoring provider kept running and the next start ran a second one with the same identity; it now sends TERM, waits up to 40s, then KILL. `urnet-tools` also checks, after an OpenRC restart, that the previous provider pid is really gone and fails loudly naming it otherwise. Found by running the generated script under a real OpenRC in Alpine. `urnet-tools update` now warns when the installed init script still lacks the marker or the stop schedule, since a binary update does not rewrite `/etc/init.d/urnetwork`; rerun the installer to refresh it. A process started by hand with `URNETWORK_INIT=openrc` in its environment is treated as supervised, so do not set it outside the init script.
- **The stage-1 probe condemned proxies on a clustered sample** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/775>): the probe drew a contiguous block of the destination table, which is grouped by theme. A block landed on one or two adjacent themes. One refused region then failed the whole pass. The score swung between A and F as the rotation moved between regions, although the proxy never changed. The probe now samples a fixed, content-keyed permutation of the table. The dial count is identical, and `use_spread_order=false` restores the old sampling.
- **A thin pass could still convict a proxy** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/775>): a new `min_confirm_dials` floor gates the early abort and the final verdict. A pass below the floor keeps dialing. A pass that runs out of block below the floor keeps the previous grade. The paid grader uses 6 by default. The URL admission path and the reaper keep 0, so their dial profile is unchanged. `-1` forces the floor off on the paid path.
- **The proxy-health down bucket reused the word degraded** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/769>): the live status and the `top` view called the was-up-now-down bucket `degraded`. The whole-pool figure uses that word for a different state. The bucket is `dropped` now, matching the proxy health report, so `down` has one meaning. Labels and wording only; the reaper inputs are unchanged.

### Changed

- **The sn pin is advanced and the head binding model is updated** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/771>): `bind-head` and `unbind-head` now use the many-to-one dual-signed fleet binding. The commands keep their names and their offline-print and on-chain submit shape. The flags follow the new model (`--manifest`, `--hotkey_seed_file`, the epoch flags, `--client_id`, `--client_seed_file`).
- **The metrics and profile documentation is corrected** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/772>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/773>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/774>): the metrics port is `9100`, not `9091`. The endpoint takes the next free port up to `9103`. The endpoint is off until `urnet-tools metrics on` turns it on. `URNETWORK_METRICS` sets a bind address, not a switch. The docs now describe the `monitoring/` bundle that ships, not a compose file that does not exist.

## [v3.23.0-fix.32.9]

### Added

- **H3 (QUIC) beside H1 for the direct identity, beta and off by default** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/744>): the engine already contained a working H3 platform transport. `Auto` mode never launched it. With `URNETWORK_H3=on` the direct (non-proxied) identity starts it beside H1. A proxied identity never does, because `runH3` opens a host UDP socket. H3 is auxiliary: H1 remains the health signal. An H3 connect failure is "mode unavailable". It backs off quietly up to 10 minutes. It is not a backend failure or a proxy auth failure. The sole H3 target mode keeps the full accounting. An H3 connect or drop does not mark the identity up, down or dropped. Both transports stay connected and neither idle-drains.
- **A runtime gate for H3** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/753>): `urnet-tools set h3 on|off` switches H3 live with no restart. On lets the idle transport dial at once. Off closes a live connection and stops further dials, and that is not counted as an H3 drop. The key is persisted and replayed at startup, so a persisted off beats `URNETWORK_H3=on`. Clearing the key hands the decision back to the environment variable.
- **H3 socket bytes count into total traffic** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/746>): the H3 UDP socket now credits the identity's total, so `top`, `status` and the metrics include it. Billable is counted at the IP layer and is unaffected.
- **A QUIC DATAGRAM lane, receive then send, both off by default** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/761>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/762>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/763>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/765>): `h3_datagram` offers DATAGRAM on the H3 connection. A server that accepts then sends its small frames as datagrams. `h3_datagram_send` lets this provider send small frames the same way. Larger frames stay on the reliable stream. Datagrams are used only while H1 is up. The blackhole guard drops the lane when datagrams go out and none come back. The H3 memory is bounded like upstream and scales with the provider's budget.
- **Per-mode transport counters** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/753>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/755>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/759>): payload frames and bytes are counted for H1, H3 and the two DNS modes. The figures show up in `[health]` and in `/metrics` as the `urnet_transport_*` families. They also land in the baseline record, the Grafana dashboard and a new `top` panel. The H3 share uses the direct identity's H1 as its denominator. That is the fair comparison. A box that never tries H3 sees no new fields.
- **A low-noise STUN success summary** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/749>): STUN reports one `[stun] ok=N fail=M` line per interval. The interval is one minute under load and five minutes otherwise. Success and failure are counted in the fork's own code. A Cloudflare STUN endpoint is added as a probe only. No STUN or ICE behaviour changes.
- **A report id on close frames** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/750>): `CloseContract` frames now carry a `ReportId`. It is logged, so a retry or out-of-band dedup mismatch can be triaged from the client side. The close report is built in one place now.
- **`urnet-tools set` documents the new keys** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/764>): the help rows now cover `h3`, `h3-datagram`, `h3-datagram-send`, `baseline` and `proxy-audit`. Each row gives its domain, default and effect. The `metrics-listen` row is documented too.

### Fixed

- **An H3 reconnect leaked a socket** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/751>): each reconnect leaked one UDP file descriptor, its read goroutine and the QUIC transport state. The transport and socket close in order now.
- **An idle connection never drained** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/751>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/752>): the inactive-drain check counted keepalives as use. The peer pings on a timer, so the check never saw an idle connection. It counts payload only now, and the closed line still reports the total per direction.
- **H3 bytes went uncounted on a direct box** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/757>): the bandwidth record was registered after the transport started. The first dial then found nothing and kept the raw socket, leaving every H3 byte uncounted. The record is registered before the transport now.
- **A path-size report inflated the send-error metric** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/765>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/763>): quic-go rejects an oversized DATAGRAM before it is queued. That is path discovery, not a failed send, and the async lane no longer counts it. A report that cannot lower the limit is still counted.
- **A datagram reader could close a live channel** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/761>): the reader slot was reserved after the receive closer had started waiting. That is a wait group misuse. Every reader slot is reserved before the closer waits now.
- **Stale datagram fragments held memory** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/761>): expiry ran only on arrival, so an incomplete message on a quiet connection held its allocation until teardown. A timer expires fragments now. Payload is counted only after a route accepts a message.
- **The STUN probe misread its own servers and results** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/749>): it read the compiled defaults. A client with its own ICE servers was judged against servers it never used. It also counted some failures as success and held a lock across each round trip. It reads the active settings and bounds the response body now. It uses one success test and stops on shutdown.
- **Log markers were restored and standardized** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/748>): the ready, hotswap and lifetime markers are back to their documented form. The contract close tokens are intact again. Provider lines use one emoji per namespace now.
- **A source that went empty kept its proxies dialing** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/756>): those proxies are cancelled now. They drain first when they still have live clients. A drained proxy whose source returns wakes a reload at once. Pool reuse and growth are judged over the current interval. The no-source reason reads honestly now.
- **A corrupt `proxy_url.json` quarantine lost the configuration** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/758>): the quarantine restarted from an empty state. The sources, the permanent blacklist, the exclude patterns and the cleanup threshold went with the cache. The replacement now starts from the last configuration that parsed.
- **The download worker no longer needs an authenticated GitHub call** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/768>): `/latest-version` resolved the latest release tag through the GitHub API. That API allows 60 anonymous requests per hour per source IP. Cloudflare Workers egress from shared addresses, so the budget stayed spent and the endpoint answered a 502 `failed` for every caller, including the clients it exists to rescue. It reads the tag from the `/releases/latest` redirect now. That path needs no token and has no such limit. The GitHub API stays as a fallback that uses `GITHUB_TOKEN` when the secret is set.

### Changed

- **CI smoke runs are serialized** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/760>): six open pull requests authenticated against the live API at once. The shared account hit its sign-in limit. One smoke run now runs at a time. A queued run waits instead of racing.

## [v3.23.0-fix.32.8]

### Fixed

- **Trim and pool-shed protected proxies by total bytes, not by what they earned**: the shed rankings read each running proxy's total traffic, which on a measured provider was about 17x its billable traffic, so a proxy that moved a lot of non-billable traffic and earned nothing ranked as a protected earner while a small real earner could be shed ahead of it. They now rank by billable bytes, taking the larger of this run's billable bytes and the decayed earnings score, so a proxy that earned heavily before the last restart is still a proven earner. This includes a proxy that is still launching and has no bandwidth record yet, which would otherwise read as earning nothing. `docs/Proxy-Management.md` already described the billable tiebreaker.
- **The OOM kill switch never turned the feature off** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/706>): `set oom-cap off` was treated as a clear, which re-applies the default (`shadow`). It now disables the feature, forgets a standing cap, and takes effect at once. The marker heartbeat runs while it is off.
- **An upgraded box reported itself as a crash** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/706>): `status` now reports `update` after an in-place upgrade. A crash still reports `unclean`.
- **A crash could be reported as clean** (<https://github.com/full-bars/urnetwork-3.23-fix/commit/855339684>): the clean-shutdown marker is consumed on read again. A plain `systemctl restart` therefore reads `unclean` (tracked in <https://github.com/full-bars/urnetwork-3.23-fix/issues/707>).
- **An unreadable `proxy_url.json` could destroy the pool state** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/706>): an unreadable file is skipped, not treated as empty, so grades and the blacklist survive one bad read.
- **Retention events were dropped on a slow disk** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/706>, <https://github.com/full-bars/urnetwork-3.23-fix/commit/2465f9e2f>): the writer drains in batches and rotates before the write that would cross the limit.
- **The action ledger never trimmed an oversized newest line** (<https://github.com/full-bars/urnetwork-3.23-fix/commit/bd6cdfad2>).
- **Trim kept no state for the proxies it shed** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/698>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/702>): shed proxies keep their ID, health and grade, and a finished drain no longer triggers a wasted reload.
- **The trim cap is written atomically and read honestly** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/705>): a garbage value is reported once and an unreadable file no longer blocks the automatic cap.
- **The status denominator ignored the trim cap** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/698>) and **`proxy trim --preview` always answered "nothing to shed"**: the running provider now computes the preview.
- **The trim cap is honored before launching at startup** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/699>): a restart no longer opens every desired proxy and then sheds.
- **The goroutine pressure sensor judges per proxy** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/699>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/702>): a healthy pool above about 1,000 proxies no longer sits at pressure 1.00.
- **RAM detection honors the unit's own cgroup limit** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/698>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/701>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/702>): `MemoryMax=` and `MemoryHigh=` on a systemd unit are now seen, and reclaimable page cache is not counted as used.
- **The OOM cap blamed this provider for other workloads** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/705>): attribution is scoped to the provider's own cgroup subtree, and a kill after long uptime is still attributed.
- **GC settings were reverted or ratcheted** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/698>): the profile GOGC is applied once per process, and the governor can release with self-heal off.
- **Concurrent log writers could lose lines, and unreadable state was silently dropped** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/705>).
- **A slow give-up could mislabel a later genuine failure** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/704>): deadline-cut attempts are reported as slow and no longer move the shared auth rate limiter.
- **Content-filtering blocklist refreshed** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/697>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/703>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/717>): the blocked-destination table is refreshed from upstream.
- **A hotswap could leave the audit trail without the new process's start entry** (<https://github.com/full-bars/urnetwork-3.23-fix/issues/670>): the old process persists its own audit ring again at the end of its drain, which can land after the new process's merged persist and leave a ring without the new process's entries on disk until the next control command or a clean shutdown. The new process now waits out the drain, merges again and persists, so a crash soon after a hotswap no longer loses it. Audit trail only, traffic was never affected.
- **The pool controller sawtoothed on boxes with self-heal on** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/721>): after a pressure cut it regrew by 25 every five minutes into the same wall. It now caps regrowth at 90% of the pool size where pressure first appeared, relaxing over 24 hours, ranks sheds by the persisted earnings score, and clamps the URL target to the running-proxy cap minus running paid proxies.
- **The GC governor chased its own tail** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/721>): the sweep measured memory by the heap goal, which moves with the GOGC the governor writes, and a box at its soft limit read as an emergency. The sweep and the 10 second subtick now share one measure (live heap plus stacks), the subtick honours the CPU veto, and entering the critical level forces a full GC at most once per five minutes.
- **A draining proxy could recreate its own registry entry** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/723>): a late log line or dial after unregister created an entry nothing removes, inflating the proxy count and the figures derived from it.
- **The audit ring reconcile evicted and re-added entries, and persists could land out of order** ([#727](https://github.com/full-bars/urnetwork-3.23-fix/pull/727)): after a hotswap the reconcile appended missing disk entries one at a time, so on a full ring each older parent entry evicted a newer one and the evicted entries then looked missing and were added again. It now builds a deduplicated union ordered by timestamp, keeps the newest 1000, and persists only when the kept window gained entries from disk. Every persist now takes its snapshot and writes under one lock, so an older snapshot can no longer be written after a newer one and drop an imported entry.
- **The slow-retry semaphore was skipped after a genuine give-up** ([#728](https://github.com/full-bars/urnetwork-3.23-fix/pull/728)): it caps how many failing paid or file proxies sit in the auth pipeline at once, but a genuine give-up left it gated off, so every later ramp and daily attempt skipped it and a box with hundreds of failing proxies could send them all at the auth API together. It now also counts genuine retry cycles.
- **Source URL tokens reached the log** ([#729](https://github.com/full-bars/urnetwork-3.23-fix/pull/729)): a source URL that failed to parse repeated the whole URL, query token included, in the fetch error, the log and the operator warning, two summary lines printed the raw URL, and the SSRF guard's errors embedded a redirect target. They now use the redacted source label and the guard no longer names the URL. Refusal behaviour is unchanged.
- **`urnet-tools top` could leave its settings directory owned by root** ([#730](https://github.com/full-bars/urnetwork-3.23-fix/pull/730)): under sudo with HOME preserved, the first settings save created `~/.config/urnet-tools` as root and the invoking user's next save failed. Every directory the save creates is now handed to the owner of the nearest existing ancestor.

### Added

- **An OOM-aware start cap, shadow by default, with a kill switch** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/700>): after a kernel OOM kill the next start runs a reduced, self-relaxing cap. `urnet-tools set oom-cap on|off|shadow` or `URNETWORK_OOM_CAP`. `shadow` decides and logs and enforces nothing.
- **An audit ledger and `urnet-tools autopilot log [limit]`** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/700>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/701>): every capacity decision is appended to `~/.urnetwork/autopilot.jsonl`.
- **A low memory headroom log and startup memory warnings** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/699>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/700>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/705>): advisory only, the provider never changes the limits. A new `effective RAM ceiling` line names the limit in force.
- **Trim receipts in the log** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/698>): `[proxy][trim] received` and `applied` lines.
- **`urnet-tools smart-dialer [status|on|off]`** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/704>, <https://github.com/full-bars/urnetwork-3.23-fix/pull/709>): a measured-cost transport preference in place of the hardcoded 500 ms baseline, with background probes so every transport gets measured. Off by default, with the plan to make it the default in a later release after more testing.
- **An `emoji` package** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/708>): network tag validation and suggestion, copied from upstream. No provider behavior changes.
- **A built-in baseline recorder** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/712>, `~/.urnetwork/baseline.jsonl`, `urnet-tools baseline show|mark|compare`): every provider records a small local time series of its own behaviour, on by default and with no setup, so an upgrade can be judged against what the box did before it rather than against a baseline somebody remembered to capture. Counts and totals only, never proxy addresses, usernames or passwords; an unmeasured field is omitted rather than written as zero. The comparison derives its rate from the lifetime counter's delta, excludes the post-restart ramp from both sides, and warns when capacity changed so a trimmed box is not misread as a regression. `set baseline off` stops recording without a restart and keeps the existing file.
- **Supervised control loops** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/722>): the pressure monitor, pool controller and degraded-proxy reaper restart with backoff after a panic, and the pressure monitor resets its score, memory budget and GOGC to neutral whenever it exits. `urnet_loop_restarts_total` and `urnet_loop_up` are exported per loop. A shutdown during the backoff does not count a restart that never happened, and the pressure monitor removes its `pressure_status` file on exit so a reader never sees a stale reading ([#732](https://github.com/full-bars/urnetwork-3.23-fix/pull/732)).
- **GC state and socket-table signals** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/724>): `baseline.jsonl` and `/metrics` carry GOGC in force, whether the governor is tightening, the GC CPU share, conntrack fill, TIME_WAIT and orphaned sockets. Observation only. A TCP line that reports only one of the two socket counters leaves the other out instead of exporting zero ([#732](https://github.com/full-bars/urnetwork-3.23-fix/pull/732)).
- **Host memory is read on Windows and macOS** ([#731](https://github.com/full-bars/urnetwork-3.23-fix/pull/731)): RAM detection and the available-memory reader only knew `/proc/meminfo`, so on Windows and macOS they fell back to a fixed 850 MiB. That sized the memory limit whatever the real RAM, and the host-RAM GC levels and the headroom log had no input. Linux keeps its parse, macOS reads `hw.memsize` and approximates available memory from the free, speculative, purgeable and file-backed page counters, and Windows uses `GlobalMemoryStatusEx`. Other platforms report unavailable and keep the fallback. Linux behaviour is unchanged and cgroup ceilings still win. The macOS figure is an approximation that has not been run on real hardware.
- **A memory pressure signal on Windows and macOS** ([#733](https://github.com/full-bars/urnetwork-3.23-fix/pull/733)): the `psi_mem` pressure component came only from `/proc/pressure/memory`. When that cannot be read, macOS now reads the kernel's memory-pressure level and Windows reads its low-memory notification, translated onto the same percent scale (macOS warn lands mid-ramp and critical at the top, Windows low at the top). A failed Windows handle creation is retried on the next sample. Linux is unchanged. The level mappings are judgement calls that have not been exercised on real hardware.

### Changed

- **A hotswap is declined when the box cannot hold two providers** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/698>): the update falls back to a plain restart when available memory is under 1.1 times the running provider's RSS.
- **The per-proxy memory estimate matches running nodes** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/705>): the startup warning now fires on RAM-tight hosts with large pools.
- **Capacity decisions reach the important buffer and the disk event log** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/701>).

---

## [v3.23.0-fix.32.7]

### Fixed

- **"why idle: auth failing" fired on normal noise** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/688>): the idle hint blamed auth if the failure counter rose at all in 10 minutes, which a large paid pool always does. Auth is now blamed only for a failure wave or a mostly-down pool, and the hint shows the numbers behind it.
- **A node stuck in startup read IDLE** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/688>): the state now follows the real startup phase, reads `degraded` if it stalls, and a new `state_reason` says why a node is starting or degraded.
- **The `top` graph shimmered instead of scrolling** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/690>): buckets counted from the oldest sample re-averaged every column each second. They are now aligned to the provider's own sample count (a new `history_seq` field), so a completed column never changes.
- **Total traffic read zero on a node that makes only direct connections** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/693>): direct sockets are now counted as total traffic over TCP and UDP, as proxy connections already were, so the total rate, total graph, bytes moved and the Prometheus total byte series carry real values on a direct-only node. Needs the provider updated.
- **`top` called a slow provider lost** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/690>): a provider that answers late stays connected with a `SLOW` marker and is called lost only after 20 seconds of silence; a refused connection is still reported at once. `top` also backs off while a provider struggles instead of adding load.
- **Content-filtering blocklist refreshed** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/692>): the blocked-destination table is refreshed from upstream.

### Added

- **Live rates at up to 100ms in `top`, and billable and total traffic** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/690>): a light `traffic` control command feeds btop-style live rates, and the snapshot, `status` and `top` show billable and total rates and bytes moved.
- **Clear logs when URL-sourced proxies are added** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/690>): every URL fetch cycle ends with one headline saying what happened to the pool (new, already known, rejected, pool size), including when nothing changed. Reloads announce URL-sourced launches and attribute additions to their source.
- **A summary line after every message pool dump** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/689>): judges the trend of outstanding buffers, so a plateau is not mistaken for a leak.
- **Zoom, a runtime Internals panel and a goroutine list in `top`** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/694>): `w` zooms the graphs to the last 15 seconds at the refresh rate; a panel shows goroutines with a trend, heap, GC and scheduler figures live; `g` lists where goroutines are parked. Two new light provider control commands, `internals` and `goroutines`, feed them; both are dropped first when the provider struggles.
- **A menu for color themes and graph styles in `top`** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/694>): `m` picks one of eight themes and braille, block or tty graphs, saved to `top.conf`. The y-axis now stays steady, and the layout stops reserving room a quiet or direct-only node does not use.

### Changed

- **`-` refreshes `top` faster and `+` slower**, like btop; they were the other way round.

---

## [v3.23.0-fix.32.6]

### Fixed

- **Two accounts at one gateway address collapsed into one proxy** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/684>): proxy state, health, earnings, grading, audit, trim, removal and refresh were keyed by bare address, so two credentialed accounts behind one host:port shared a single entry and the winning account flipped on each reload, rotating the proxy in a loop. A proxy is now identified by address plus username, and existing state and earnings history migrate on start. Unauthenticated proxies are unchanged.
- **Saved client logins keyed by identity** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/685>): the hot-restart login store had the same bare-address keying, so accounts sharing a gateway shared one saved login. It is now keyed by identity and existing entries are adopted on start and on reload. When accounts share an address, one inherits the saved login and the rest mint fresh once. The migration is one way but idempotent.

- **Startup no longer stalls on a large saved-login store** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/687>): adoption rewrote the whole login store once per login, holding every proxy back for minutes to hours on a node with thousands of saved logins. It now moves every login with a single write.

### Maintenance

- **Flaky reload test** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/686>): test-only, no behavior change.

---

## [v3.23.0-fix.32.5]

### Fixed

- **HotSwap silently stranded by a re-run installer** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/682>): `update` only reconciled a unit's systemd `Type=` after installing a new binary, so a unit clobbered by a re-run of the installer (`urnet-tools reinstall`, or the documented `curl -fsSL https://install.fullbars.xyz | sh` one-liner — which always writes `Type=simple`, unaware of any prior HotSwap migration) stayed clobbered until a new release shipped. `update`'s "already on `<tag>`" no-op path now also reconciles the unit against the binary on disk, restarting the provider exactly when a migration to `Type=notify` needs the running process to pick up `NOTIFY_SOCKET`. No installer changes, no new CLI surface — every `update`, including the unattended weekly timer, is now sufficient on its own to converge a stranded unit.

---

## [v3.23.0-fix.32.4]

### Fixed

- **Perpetual credential rotation on paid proxies** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/680>): the reload cycle recorded a running proxy's auth baseline as the same pointer handed to its own goroutine, which the proxy runtime mutates during its auth flow. The baseline drifted within milliseconds of launch, so every ~30s reload saw a phantom credential change and rotated — relaunching (and re-mutating) the proxy, forever. Only credentialed (paid, file-sourced) proxies were affected; nil-auth proxies always compared equal. The baseline is now a deep copy, isolated from the object the runtime mutates. A genuine credential change (re-paste) still rotates exactly once.

---

## [v3.23.0-fix.32.3]

### Fixed

- **Message-pool buffer leak on connect failure** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/677>): the dial-failure branch released the pooled packet without returning its buffer to the pool. It now returns the buffer exactly once, with a regression test. Restart a provider to reclaim memory the leak already took.

### Maintenance

- **Deterministic test suite** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/675>).
- **Security blocklist sync** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/676>).

---

## [v3.23.0-fix.32.2]

### Added

- **Audit ring now records lifecycle events** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/668>): `urnet-tools history` previously showed only `set` and `clear` config changes. It now also records process start (with the version), the hotswap handoff, and control-socket shutdown. After an update the sequence reads `hotswap` on the retired process, and the successor's ring shows `start` plus the merged `hotswap` (`start` then `hotswap` on spawned candidates, `hotswap` then `start` on the Docker execve path).
- **Audit entries survive hotswap** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/668>): the parent flushes the audit ring at the handoff commit point, before the takeover message, on every handover path (systemd, Windows, Docker). The successor merges the ring from disk after takeover, with timezone-safe deduplication, so the last control-socket commands before an update are not lost. The parent quiesces its control socket at the same commit point, so a command accepted after the flush cannot vanish in the parent's memory; it falls back to the pending-overrides queue, which the successor merges.
- **Live node snapshot and a live block in `urnet-tools status`** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/664>): the provider keeps a snapshot of the node, cached for about a second, and serves it over the control socket as `snapshot`. It carries the billable rate now and as 1 and 5 minute averages with a 10 minute history, active clients and sessions, the proxy pool by status, pressure, memory, descriptors and goroutines, the restart reason, and a flowing, idle, degraded or starting verdict with a hint when idle. `urnet-tools status` shows it as a live block, `urnet-tools status --json` prints it for scripts, and a host with several providers gets a one-line summary of each.
- **Restart reason** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/664>): `urnet-tools update`, `hotswap` and `restart` record why the provider is about to restart, and the provider reports it after it starts (`update`, `hotswap`, `manual`, `clean`, `unclean` or `first-start`) in the snapshot and as `urnet_restart_reason`.
- **Resource metrics and alert rules** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/664>): `urnet_mem_limit_bytes`, `urnet_rss_bytes`, `urnet_open_fds` and `urnet_fd_limit` (the last three on Linux); `UrnetworkNodeDown` and `UrnetworkRestartLoop` Prometheus alerts on by default in the Monitoring bundle, plus four commented-out fleet-specific ones. See `docs/Monitoring.md`.
- **`urnet-tools top`, a full-screen live provider view** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/665>).
- **Automated proxy audit and quality enforcement** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/666>): `urnet-tools proxy audit on|off|status|release`, parking proxies that grade as junk; status reports parked and paused honestly.
- **Control unitless providers** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/669>): lifecycle and settings commands work against bare or containerized providers without a systemd unit, state-dir rows are deduplicated, and `urnet-tools get` reads one setting.
- **File-backed proxy add and paste fixed** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/663>): comments and blank lines survive, lock-guarded deduplication, keyed credentials preserved.
- **Security blocklist sync** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/667>): refreshed the content-filtering blocklist from upstream — a net reduction of about 6,400 IPv4 ranges and 32 IPv6 prefixes after upstream pruning. The shipped file is the sync-time upstream snapshot; upstream has since moved.

### Changed

- **Provider status uses a sliding severity scale** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/668>): `active` at 90% or more of configured proxies live, `partial` at 70-89%, `degraded` at 50-69%, `critical` below 50% (including zero), with the exact percentage always rendered and clamped at 100. A healthy node with a small dead tail of proxies no longer reads as `partial` forever.
- **Start entries persist immediately on normal boots** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/668>): only a hotswap successor defers its start entry until the takeover merge; a regular boot keeps writing to disk at once.

---

---

## [v3.23.0-fix.32.1]

### Added

- **HotSwap how-to and measured costs** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/659>): new `docs/HotSwap.md` covering requirements, the update flow, what you will see, and measured costs (connection ramp, memory, drain).
- **Design proposal: per-client make-before-break HotSwap handover** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/661>): `docs/design/hotswap-make-before-break.md` plans a handover that keeps every client connected throughout. A proposal only, no code; it needs research before development.

### Security

- **Container-discovery ghost hardening, round 2** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/653>): containerized providers no longer appear as host providers (cgroup classification when the mount namespace is unreadable); provider state is read and written through descriptor-pinned handles that walk from the kernel-attributed owner home without following symlinks; state-dir arguments are validated against the owner captured in the same process scan; `docker cp` output is decoded from the tar stream and size-capped; Docker commands have deadlines and the exec fallback rejects option injection; session load and save, the pending-overrides lock, self-heal, hotswap counters, the direct toggle, the reload trigger and the unit backup and replace paths no longer re-resolve a user-controlled pathname as root; a recovered provider binary is chmod'ed and chown'ed on the open file descriptor.

### Fixed

- **HotSwap unit migration was a silent no-op** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/659>): the installer writes a unit with no `Type=` line and `update` only rewrote an explicit `Type=simple`, so nodes set up by the current installer never reached a hotswap. A unit with no `Type=` now gets `Type=notify` and `NotifyAccess=all`, and the update says so when a unit cannot be migrated.
- **HotSwap declines up front when the running provider has no notify socket** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/659>): a migrated unit whose provider has not restarted no longer aborts after SIGUSR2 and rolls back; new decline label `needs_restart`.
- **pprof diagnostics disappeared on every other hotswap** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/659>): a candidate retries the diagnostics bind until its parent releases the port.
- **`urnet-tools` not found** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/659>) from non-interactive shells, zsh and root: the installer links `urnet-tools` and `urnetwork` into `~/.local/bin` and `/usr/local/bin` and writes the PATH block to `~/.bashrc`, `~/.profile` and `~/.zshenv`; `urnet-tools update` repairs older installs.
- **Proxy credential rotation on re-paste** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/655>): pasting an address that already exists with different credentials now rotates the running proxy instead of silently keeping the old credentials; all duplicate entries for an address are scanned before an add is skipped.
- **Docker idle-update poll is bounded** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/653>): a hung Docker daemon can no longer stall the idle wait past its own timeout.
- **Message-pool leak attribution** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/655>): per-call-site leak tags are on by default, name the acquiring call site, and are safe under the race detector.

### Changed

- **HotSwap is no longer described as zero-downtime.** Measured on a live node, a hotswap removes the 2 to 3 second window with no provider process, but the old process drops its proxy connections at the handover and the new one rebuilds them over about 30 s, the same ramp as a restart. Earlier entries that say "zero-downtime" describe the process handover only. See `docs/HotSwap.md`.

### Maintenance

- **Security blocklist sync** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/657>): refreshed the content filtering blocklist.

---

## [v3.23.0-fix.32.0]

### Removed
- **Hub subsystem (PR #634)**: the entire hub fleet-dashboard subsystem is removed — the `hub/` package (24 files, ~9,700 lines), all `urnet-tools hub *` CLI commands, hub CI build/packaging/scan steps, the hub CA bootstrap and TLS fingerprint pinning from `bandwidth_reporter.go`, hub test scripts and shakedown sections, and the hub documentation. Prometheus `/metrics`, `urnet-tools metrics on|off`, the generic `report` command, and the Grafana monitoring bundle are preserved and remain the supported path for fleet visibility.

### Fixed
- **Selective-ack ordering and provable-hole ack wake (PR #643)**: selective acks were written in map iteration order, so a partial batch could prove the neighbours of one real hole lost and trigger needless resends; selective acks now leave in ascending sequence order. The ack compression wait also ends early when a hole becomes provable to the sender or when the head ack advances past selectively acked items. With `AckCompressTimeout=0` (the default) acks remain per-packet and the in-order rate is unchanged.
- **Docker shakedown checks aligned with current release state (PR #644)**: the proxy-cap check reads proxy state from `proxy.state` (with a `proxy_url.json` fallback), the eco/lowmem profile checks read `/dev/shm` ramlogs, and the compose down/up check handles ramlog client_id capture with adaptive deadline polling and one retry on initial timeout.
- **Docker entrypoint jwt mode autodetect**: passing a JWT auth code as the positional argument (or setting `URNETWORK_AUTH_CODE`) now selects `BUILD=jwt` automatically instead of demanding `USER_AUTH`/`PASSWORD`; an explicit `BUILD=` still overrides.

---

## [v3.23.0-fix.31.9-rc1]

_Pre-release tag._

### Added

- **The heartbeat identifies the build and reports message-pool health** ([#592](https://github.com/full-bars/urnetwork-3.23-fix/pull/592), [#595](https://github.com/full-bars/urnetwork-3.23-fix/pull/595)): `[health][build]` leads each heartbeat, and the pool line reports whether it is warming, healthy or accumulating buffers that may indicate a leak.
- **`urnet-tools proxy ids` lists client ids by proxy** ([#593](https://github.com/full-bars/urnetwork-3.23-fix/pull/593)).

### Fixed

- **RAM logs retain their history and `urnet-tools logs` follows the active buffer** ([#596](https://github.com/full-bars/urnetwork-3.23-fix/pull/596), [#597](https://github.com/full-bars/urnetwork-3.23-fix/pull/597)): the trim now reads the file it rewrites instead of replacing retained content with zero bytes, and log discovery recognizes RAM logging enabled through the provider environment.
- **Repeated transport errors no longer flood the RAM log** ([#598](https://github.com/full-bars/urnetwork-3.23-fix/pull/598)): TLS handshake timeouts and audit-send errors are rate-limited by failure class.
- **Provider discovery rejects unrelated sibling services** ([#599](https://github.com/full-bars/urnetwork-3.23-fix/pull/599)): when `ExecStart` is unreadable, a matching service name alone is no longer enough; a provider state directory is also required.
- **Status retrieval reports when both backing calls fail** ([#590](https://github.com/full-bars/urnetwork-3.23-fix/pull/590)); a failed reload or empty proxy source also appears in service status ([#591](https://github.com/full-bars/urnetwork-3.23-fix/pull/591)).

## [v3.23.0-fix.31.8-rc1]

_Pre-release tag._

### Added

- **Provider version detection survives trimmed build metadata** ([#579](https://github.com/full-bars/urnetwork-3.23-fix/pull/579), [#580](https://github.com/full-bars/urnetwork-3.23-fix/pull/580), [#584](https://github.com/full-bars/urnetwork-3.23-fix/pull/584), [#585](https://github.com/full-bars/urnetwork-3.23-fix/pull/585)): release builds carry a version stamp, the control socket reports the running version, and unattended update prompts record the current version with a binary fallback when needed.

### Fixed

- **An unreliable transfer lane no longer stalls the whole send sequence** ([#586](https://github.com/full-bars/urnetwork-3.23-fix/pull/586)): when its resend flight is full, traffic can use an available reliable lane; unreliable acknowledgements no longer skew the RTT estimate, and ICE UDP sockets receive a larger buffer.
- **Go pseudo-versions with build suffixes no longer mask the provider version stamp** ([#587](https://github.com/full-bars/urnetwork-3.23-fix/pull/587)).
- **The unattended update timer states its non-interactive intent** ([#583](https://github.com/full-bars/urnetwork-3.23-fix/pull/583)): the unit passes `-y` to `urnet-tools update`.

## [v3.23.0-fix.31.6-rc1] — 2026-09-10

_Pre-release tag._

### Added

- **A unified v2 configuration schema** ([#575](https://github.com/full-bars/urnetwork-3.23-fix/pull/575)): the provider becomes the single source of truth for its settings. A v2 envelope, backward compatible with v1, records where each value came from (socket, environment, pending queue, legacy file, default), a `status` socket command returns all of it with that provenance, and `urnet-tools config` renders it as a table or JSON. Persistence syncs the file and its parent directory.
- **Socket hardening: peer credentials, size limits and version negotiation** ([#573](https://github.com/full-bars/urnetwork-3.23-fix/pull/573)): `SO_PEERCRED` checks the connecting UID against the provider's on Linux, a five-second per-line read deadline closes the slowloris shape, requests are capped at 64 KiB and values at 4 KiB, and a client speaking a newer protocol version is rejected instead of parsed hopefully.
- **A phased startup banner with one ready summary** ([#571](https://github.com/full-bars/urnetwork-3.23-fix/pull/571)): startup prints in phases and closes with a single ready line instead of the scattered lines that made a slow start hard to read.
- **`optimize` sizes the conntrack table from host RAM** ([#569](https://github.com/full-bars/urnetwork-3.23-fix/pull/569)): the connection-tracking table is scaled to the host instead of left at the distribution default.

### Fixed

- **The control socket validates values at the server** ([#572](https://github.com/full-bars/urnetwork-3.23-fix/pull/572)): a raw socket client can no longer persist a value that breaks the next start.
- **`version` reported non-provider units**: a sentinel or similar unit was answered as the provider's version; only provider units are considered now.
- **The running provider's version is read through `/proc/<pid>/exe`** ([#578](https://github.com/full-bars/urnetwork-3.23-fix/pull/578)): the resolved binary is used instead of the unit's start path, which can be a wrapper.
- **Startup log duplication and idle spam** ([#577](https://github.com/full-bars/urnetwork-3.23-fix/pull/577)): the ready summary no longer repeats lines already printed, and idle lines stop filling the buffer.

### Changed

- **CFAA blocklist refreshed twice** ([#570](https://github.com/full-bars/urnetwork-3.23-fix/pull/570), [#574](https://github.com/full-bars/urnetwork-3.23-fix/pull/574)): 44,672 to 43,261 and then to 44,007 IPv4 entries; 522 to 569 and then to 591 IPv6 entries. Data only.

## [v3.23.0-fix.31.5-rc1] — 2026-09-10

_Pre-release tag._

### Added

- **A Prometheus `/metrics` endpoint with no new dependencies** ([#565](https://github.com/full-bars/urnetwork-3.23-fix/pull/565)): eleven `urnet_*` metrics covering uptime, connections, the proxy pool by status, per-proxy bytes and clients, errors by category, contracts by result, and Go runtime memory and goroutines. The text format is generated directly, and the endpoint mounts on the loopback diagnostics server.
- **`history`, `metrics`, `profile` and `dashboard` commands** ([#568](https://github.com/full-bars/urnetwork-3.23-fix/pull/568)): the provider's 1000-entry command audit ring, a live toggle for the metrics endpoint without a restart, the tuning profile, and a terminal status panel.
- **A hardened control socket with a persisted audit trail** ([#566](https://github.com/full-bars/urnetwork-3.23-fix/pull/566)): server-side validation for every key with an auto-computed `needs_restart`, an atomic error flush with checksum recovery, and a bounded audit ring served by a `history` command.
- **Durable v2 persistence and a `status` socket command**: writes are synced and the parent directory fsynced, so a recorded setting survives a host crash rather than only a clean exit.

### Fixed

- **`ramlogs`, `turbo` and `eco` with no arguments showed nothing** ([#564](https://github.com/full-bars/urnetwork-3.23-fix/pull/564)): they print the current status instead of failing or prompting.
- **Dead `IncrBytesTransferred` removed and label escaping fixed**: the unused counter is gone and metric labels are escaped, so a proxy address or error class cannot break the exposition format.
- **Metrics error handling, naming and shutdown** ([#565](https://github.com/full-bars/urnetwork-3.23-fix/pull/565)): review findings on the new endpoint, including a clean stop when the provider exits.

## [v3.23.0-fix.31.4]

### Fixed
- **Flight window no longer grows back on a resend timeout (PR #640)**: a timeout halved the unreliable-flight window, but the old release path handed the timed-out item to the acknowledge path, which grew the window back on the same event. Timeouts now only shrink the window; delivery evidence is the only thing that grows it. On lossy routes the congestion reduction sticks and recovery is consistent.
- **The receiver no longer acknowledges items it can still evict (PR #641)**: held items were acknowledged on arrival and then quietly evicted when a full hold had to admit an earlier arrival. The sender learned of the withdrawal only after two acknowledgement-tail probes and the full selective-ack lease. The receive hold now commits a prefix that no later arrival can evict; tentative items above the boundary are held unacknowledged and evicted at zero cost, so a removal costs the sender a resend instead of a stalled lease.

### Changed
- **CFAA blocklist refreshed (PR #637)**: content-filtering ranges synced from upstream.
- **Hub deprecation notice (PR #635, #636)**: release notes carry a CAUTION callout. The hub is removed in the next release series.

### Other
- **H3 + miner migration plan (PR #639)**: added `MIGRATION_PLAN.md` and `scripts/h3-workspace.sh`. No behavior changes.

## [v3.23.0-fix.31.4-rc1] — 2026-09-09

_Pre-release tag._

### Fixed

- **Shakedown failures: Docker arch, `--tag`, restart verification and hotswap** ([#558](https://github.com/full-bars/urnetwork-3.23-fix/pull/558)): the checks now run against the architecture and tag they are given and verify a restart after a handover.
- **An unbalanced brace in the `supportsHotSwap` gate** ([#558](https://github.com/full-bars/urnetwork-3.23-fix/pull/558)): the tool's capability gate compiled to the wrong branch; it is balanced now.
- **The HotSwap decline reason was lost on the update path** ([#560](https://github.com/full-bars/urnetwork-3.23-fix/pull/560)): `urnet-tools update` reports why a handover was declined instead of proceeding silently.
- **An unreadable unit `Type=` was reported as a plain mismatch** ([#560](https://github.com/full-bars/urnetwork-3.23-fix/pull/560)): it is reported as a query failure, so an unreadable unit is not mistaken for `Type=simple`.
- **Provider units are corroborated against `ExecStart`** ([#562](https://github.com/full-bars/urnetwork-3.23-fix/pull/562)): a matching unit name alone no longer identifies a provider.
- **The discovery fallback test raced its child** ([#559](https://github.com/full-bars/urnetwork-3.23-fix/pull/559)): the test polls for the process instead of assuming it exists at the next statement.
- **The auto-update test mutated the host** ([#563](https://github.com/full-bars/urnetwork-3.23-fix/pull/563)): it runs against a fixture, not the machine running CI.

### Changed

- **CI shakedown review findings** ([#557](https://github.com/full-bars/urnetwork-3.23-fix/pull/557)): the workflow inputs and quoting were corrected.
- **README fork-changes section refreshed** ([#556](https://github.com/full-bars/urnetwork-3.23-fix/pull/556)): docs only.
- **CFAA blocklist refreshed** ([#561](https://github.com/full-bars/urnetwork-3.23-fix/pull/561)): 44,533 to 44,672 IPv4 entries and 506 to 522 IPv6 entries. Data only.

## [v3.23.0-fix.31.3]

### Fixed
- **Pooled buffers returned on every drop path (PR #633)**: `SendPacketWithTimeout` and the multi-client send paths dropped packets without returning the pooled byte buffer on channel-full, timeout, and cancellation paths. A busy relay leaked buffers into the message pool until it pinned hundreds of megabytes. All drop and backpressure paths now return the buffer exactly once, with regression tests covering the ownership contracts.

### Changed
- **CFAA blocklist refreshed**: content-filtering ranges synced from upstream.
- **Hub deprecation notice (PR #635)**: release notes now carry the initial deprecation callout.

## [v3.23.0-fix.31.3-rc1] — 2026-09-08

_Pre-release tag._

### Added

- **`urnet-tools show-ip`, and settings a provider receives are logged** ([#553](https://github.com/full-bars/urnetwork-3.23-fix/pull/553)): `ip-detect` is renamed to `show-ip`, and a setting applied over the control socket now leaves a line in the provider's own log instead of being visible only in the CLI that set it.

### Fixed

- **The Docker shakedown pulled the image before it existed** ([#555](https://github.com/full-bars/urnetwork-3.23-fix/pull/555)): the job now waits for the build to publish before pulling.

### Changed

- **The v31 surface reached the release notes, wiki, README and changelog** ([#554](https://github.com/full-bars/urnetwork-3.23-fix/pull/554)): docs only; before this the control socket and HotSwap were documented only in a release-notes draft.
- **CFAA blocklist refreshed** ([#549](https://github.com/full-bars/urnetwork-3.23-fix/pull/549)): 43,809 to 44,533 IPv4 entries and 469 to 506 IPv6 entries. Data only.

## [v3.23.0-fix.31.2]

### Added
- **Zero-Downtime HotSwap on Windows (PR #613, #614)**: the in-place binary handover now works on Windows with SIGUSR2-equivalent process signaling and `execve` replacement; Docker containers can hot-swap the provider binary without restarting the container, with `/metrics` state carried across the handover.
- **Windows lifecycle management (PR #616)**: `urnet-tools` now supports `start`, `stop`, `restart`, and control-socket shutdown on Windows via Task Scheduler and named-pipe control, bringing feature parity with the Linux systemd lifecycle.
- **Go `urnet-tools` in Windows release tarball (PR #617)**: the Windows tarball now ships the compiled Go `urnet-tools` binary alongside the provider; legacy `urnet-tools.ps1` updater removed in favor of the Go-based self-update path.
- **Automated WDSI submission bundle (PR #618)**: every release now produces a ready-to-submit WDSI (Windows Driver Signing Infrastructure) bundle automatically, removing manual preparation steps.
- **Metrics reachable out of the box (PR #620)**: `/metrics` endpoint is now enabled by default and reachable without requiring manual `URNETWORK_PPROF` opt-in, so Prometheus scraping works immediately after install.
- **Prometheus and Grafana monitoring bundle (PR #621)**: a ready-to-deploy monitoring stack ships with the release, including a Prometheus configuration that scrapes the provider's `/metrics` endpoint and a Grafana dashboard with pre-built panels for provider health, transfer throughput, proxy tiers, and contract metrics.

### Fixed
- **`/metrics` Prometheus parse errors (PR #619)**: fixed metric name and label formatting so the `/metrics` endpoint parses cleanly under Prometheus's text exposition format without scrape errors.
- **Root allowed through control-socket peer check (PR #622)**: root callers are no longer rejected by the Unix socket peer credential check; non-root rejections now send a structured JSON error response instead of a bare message.
- **`config` command discovery path (PR #623)**: `urnet-tools config` now resolves providers using the same discovery logic as `status`, eliminating mismatches where `status` found a provider but `config` could not.
- **Docker nightly staging path and update settle delay (PR #624)**: corrected the tarball extraction path in the nightly Docker build and increased the post-update verification settle delay so slow-starting providers are not falsely flagged as failed.
- **Throttled errors counted in Prometheus counter (PR #626)**: errors suppressed by the rate limiter were not incrementing the Prometheus error counter, so dashboards under-reported error rates during sustained failure storms.
- **Three shakedown root-cause fixes (PR #627)**: addressed three distinct failure modes discovered during automated shakedown runs — incorrect script exit handling, missing prerequisite checks, and stale state between shakedown sections.
- **`urnet-tools` provider deduplication by state dir (PR #628)**: when multiple unit files pointed at the same provider state directory, `urnet-tools` listed the same provider multiple times; units are now deduped by canonical state directory path.
- **Docker nightly tarball extraction regression (PR #629)**: added a regression test and fixed the extraction path handling for nightly tarballs inside Docker containers.
- **Smarter update verification loop with PID tracking (PR #630)**: the post-update verification now tracks the new process by PID instead of relying on a fixed settle delay, so it detects both success and failure faster and avoids false negatives on slow starts.
- **HotSwap Docker execve arg sanitization (PR #614)**: Docker `execve` arguments for in-container HotSwap are now sanitized, preventing injection of untrusted values during the binary handover.
- **`gofmt` compliance for `connect.go` (PR #612)**: formatted `connect.go` to pass the `gofmt` CI gate.

### Changed
- **Release engineering hardening (PR #617, #618, #629)**: Windows packaging cleaned of legacy updater remnants; WDSI bundle automated; Docker nightly extraction regression-tested to prevent silent packaging regressions.

---

## [v3.23.0-fix.31.2-rc1] — 2026-09-08

_Pre-release tag._

### Added

- **Shakedown sections Q to Z for the v31 daemon surface** ([#544](https://github.com/full-bars/urnetwork-3.23-fix/pull/544)): 980 lines covering the parts of the daemon work the existing script never reached. The unit shapes it exercises are taken from a live sweep of 24 production nodes, every one of which runs `Type=simple`.
- **A Docker shakedown workflow** ([#545](https://github.com/full-bars/urnetwork-3.23-fix/pull/545)): a separate workflow and script for the container path, because HotSwap and the lifecycle commands have their own implementations there rather than being the same code deployed twice.
- **A manual dispatch can target a specific release** ([#548](https://github.com/full-bars/urnetwork-3.23-fix/pull/548)): the dispatch no longer resolves `/releases/latest`, which excludes prereleases and silently tested 30.9.

### Fixed

- **`Type=notify` wedged every start and restart** ([#546](https://github.com/full-bars/urnetwork-3.23-fix/pull/546)): the unit shipped `Type=notify` with `TimeoutStartSec=0`, so a start that never received `READY=1` blocked forever.
- **Provider version resolution on `-trimpath` release builds** ([#547](https://github.com/full-bars/urnetwork-3.23-fix/pull/547)): `-trimpath` strips the `-ldflags` build setting, so `main.Version` is unreadable from a release binary; the existing `--version` fallback was gated behind a check that never ran.
- **Shakedown section L aborted the suite on an unset variable** ([#550](https://github.com/full-bars/urnetwork-3.23-fix/pull/550)): `$CID_FRESH` is never assigned and the script runs under `set -u`, so every section after L had never executed in any run.
- **A childless session read as a dead HotSwap candidate** ([#552](https://github.com/full-bars/urnetwork-3.23-fix/pull/552)): `Wait` returns immediately when no child backs the session, so the drain-liveness select fired at once and the handover was abandoned.

### Changed

- **CI shards the race suite and splits lint out** ([#551](https://github.com/full-bars/urnetwork-3.23-fix/pull/551)): `test-and-lint` was the whole critical path at 408 seconds while every other check finished inside 3.5 minutes.

## [v3.23.0-fix.31.1]

### Added
- **`urnet-tools history` command**: exposes the provider's 1000-entry command audit ring via the control socket. Supports `--limit` and `--cursor` for pagination. Fixed wire-format mismatch (`int64` vs `time.Time` serialization) that broke every invocation.
- **HotSwap unit migration (PR #611)**: `urnet-tools update` rewrites a `Type=simple` unit to `Type=notify` when the binary it installs sends `READY=1` at startup (v31.0+), so the following update can hot swap; the migrating update itself uses a normal restart. The unit follows the installed binary: installing anything older, or a binary whose version cannot be read, demotes it back to `Type=simple`, as does a HotSwap rollback to such a binary. A `Type=notify` unit is therefore never left paired with a binary that cannot signal readiness, the skew #546 kept the installer on `Type=simple` to avoid. Unit, temp and backup files are written with `O_NOFOLLOW`. Non-fatal: a failed migration logs a note and falls back to restart.
- **HotSwap operator-facing decline messages (PR #611)**: every HotSwap decline point now tells the operator what went wrong in plain language and what to do about it, instead of technical jargon. Covers version too old, Type=simple, Windows, and missing NOTIFY_SOCKET.
- **HotSwap decline metrics (PR #611)**: new `urnet_hotswap_outcomes_total{reason="..."}` Prometheus counter tracking HotSwap success and decline outcomes by reason (version_old, unit_not_notify, windows, takeover_failed, etc.). Written atomically by `urnet-tools` and scraped by the provider's `/metrics` endpoint.
- **`ParseByteCount` human suffixes (PR #610)**: `urnet-tools set gomemlimit` now accepts common formats (`1536M`, `2G`, `512MB`, `1g`, `2tib`) in addition to the original lowercase iB variants. Spaces stripped, longest-suffix-first matching.
- **`urnet-tools set metrics on|off` (PR #611)**: when `URNETWORK_METRICS` is unset, the listener starts on the first free loopback port in `127.0.0.1:9100-9103`. Set `URNETWORK_METRICS` to serve it on another interface. A persisted `on` is applied at boot.
- **Proxy earnings priority ranking (PR #611, from #601)**: proxy launch order now considers earnings history — high-earning URL proxies are promoted into the trusted launch group alongside file proxies. Sort order: warmth first, then provenance, then earnings. Exploration quota interleaves 1 unproven proxy per 5 trusted cold proxies to prevent starvation.

### Fixed
- **`urnet-tools history` wire-format mismatch**: `AuditEntry.Timestamp` was `int64` on the client but the provider sends `time.Time` (RFC3339 string), causing JSON unmarshal failure on every invocation.
- **`urnet-tools set metrics on` no-op**: `applyMetricsLive` required `URNETWORK_METRICS` env var at boot — if unset, the server never started and `set metrics on` silently did nothing. Now auto-starts the metrics server on a free loopback port.
- **HotSwap declined on every systemd node**: installs run `Type=simple`, so HotSwap always declined. The unit is now migrated during update when the installed binary supports it. The migration runs in the `urnet-tools` performing the update, so on a 31.0 node run `urnet-tools self-update` before `urnet-tools update` to migrate on the upgrade to 31.1.
- **Proxy earnings priority inversion**: cold promoted URL proxies could jump ahead of warm unpromoted URL proxies because provenance was checked before warmth. Sort order corrected to warmth-first.
- **Proxy earnings starvation**: unproven URL proxies could be permanently starved behind cumulative cold-proxy ramp delays. Added exploration quota.
- **Contract retry wait**: the wait is recomputed from its baseline before every retry, so it follows denials that async `CreateContract` callbacks record between retries and drops back once a contract succeeds or the denial state expires.
- **Adaptive proxy `ParallelBlockSize`**: `getAdaptiveBlockSize()` now guards against zero/negative values.
- **`consecutiveErrors` overflow**: capped at 20 to prevent unbounded growth.
- **`writeStateFile` double close**: the fd was closed directly and again by the `*os.File` wrapping it, whose finalizer later closed whatever descriptor had reused the number. Surfaced as intermittent "bad file descriptor" test failures.
- **HotSwap counter file symlink write**: `urnet-tools` (root) wrote `.hotswap_declines.json.tmp` into the provider-owned state dir with `os.WriteFile`, which follows symlinks. Now written with `O_NOFOLLOW`.
- **Contract denial backoff cycled instead of climbing**: denial state expired 2 minutes after the last denial, shorter than the 120s+ tiers, so the count reset before reaching the cap. State now survives backoff + 2 minutes. A frame carrying several errors counts as one denial.
- **Adaptive probe batch shared across proxies**: the success window was process-wide, so one proxy's failing path resized every proxy's batch. Now one window per client strategy; the batch halves when healthy and never exceeds the configured `ParallelBlockSize`.
- **`gogc` "off" could disable GC**: a set carrying `OFF` (any casing the CLI did not rewrite to a clear) called `SetGCPercent(-1)`. Only `disabled` turns collection off; the CLI clear match is case-insensitive.
- **Persisted `metrics on` ignored at boot**: without `URNETWORK_METRICS`, a saved `on` was never re-applied at startup, so `set metrics on` did not survive a restart. The auto-selected listener is now held open from probe to serve.
- **Denial backoff overflow**: a very large denial count overflowed the shift and returned no backoff.
- **Serial dialer ordering race**: dialer health is snapshotted before sorting, with one comparator shared by both evaluation paths.
- **Unit migration hardening**: a symlinked unit file is refused, and files root writes into a user's unit directory are handed to that directory's owner.
- **Test harness global mutation**: `withGlobalEarningsStore` now uses `t.Cleanup` to restore state.

### Changed
- **CFAA blocklist sync (PR #609)**: refreshed Computer Fraud and Abuse Act IP blocklists from upstream definitions.
- **HotSwap decline messages**: all 5 decline points now include operator-facing guidance with actionable next steps instead of technical error strings.
- **Proxy warmth test expectations**: updated to reflect warmth-first sort order.

### Test Coverage
- 62 new test functions covering denial backoff, proxy selection and ordering, adaptive probe batching, HotSwap unit migration, counters and declines, the control socket (`gogc`, metrics at boot), earnings launch ranking, `ParseByteCount`, the audit wire format, and the `writeStateFile` descriptor fix.

---

## [v3.23.0-fix.31.1-rc1] — 2026-09-08

_Pre-release tag. This tag points at the same commit as `v3.23.0-fix.31.0-rc1`; it re-cuts that build and carries no code change of its own._

## [v3.23.0-fix.31.0]

### Added
- **Provider Control Plane & Daemonization (Flagship)**: Decoupled provider service execution from interactive CLI sessions; added a background supervisor, a local Unix domain socket control plane (`~/.urnetwork/provider.sock`, `provider/control_socket.go`) and client protocol in `urnet-tools` for live runtime configuration management without restarts or brittle shared-file locks. Atomic set/clear, single-owner `provider_state.json`, and cross-process flocking.
- **Offline Pending Configuration Queue**: Seamless fallback queue (`~/.urnetwork/pending_overrides.json`) for `urnet-tools set` invocations when the provider is offline, atomically merged and applied one-shot on startup.
- **Zero-Downtime HotSwap (Docker only in practice)**: In-place binary handover with control-socket/listener transfer, mandatory TAKEOVER-ACK-then-yield, graceful drain, SIGUSR2/`urnet-tools hotswap` + Docker PID-1 in-place `execve` and systemd `Type=notify` handoff, and automatic rollback if the candidate fails to take over.
- **Public IP Autodetection & Dashboard Rename (PR #534)**: Automatic detection of public IPv4 address with caching, 60s TTL, concurrent request deduplication, and opt-out (`~/.urnetwork/disable_ip_autodetect` / `urnet-tools ip-detect off`); added `urnet-tools rename` to set custom dashboard display labels dynamically.
- **Systemd Environment Key Migration**: Unified `URNETWORK_PROFILE`, `URNETWORK_RAMLOGS`, `GOMEMLIMIT`, and `GOGC` under control-socket configuration, eliminating manual `override.conf` requirements.
- **urnet-tools Multi-Provider UX (one provider per OS user)**: implicit current-user scope on ordinary commands; `providers` / `providers --all` inventory; cross-user `--user`/`--unit` commands self-elevate under `sudo <full binary path>`; `-y`/`--yes` aliases `--force`; `optimize` self-elevates and applies atomically with rollback; actionable manage-time refusals; control-socket liveness in `status`.
- **JWT Refresh Transfer Stats Logging**: Step 3/3 of JWT auto-refresh parses `paid_bytes_provided` and `unpaid_bytes_provided` from `GET /transfer/stats` and logs account balances in human-readable units inline (`unpaid: X, paid: Y`).
- **CFAA Blocklist Synchronizations (PR #540, #542)**: Automated synchronization of Computer Fraud and Abuse Act IP blocklists with upstream definitions.
- **Unified configuration, v2 schema (PR #575)**: the provider becomes the single source of truth for every setting. A v2 envelope, backward compatible with v1, records where each setting came from (socket, env, pending, legacy, default); a `status` socket command returns all of it with that metadata, and `urnet-tools config` renders it as a table or JSON. Mutation goes through one API so values and their metadata cannot drift, and persistence syncs the file and its parent directory.
- **Prometheus `/metrics` endpoint (PR #565)**: eleven `urnet_*` metrics covering uptime, connections, proxy pool by status, per-proxy bytes and clients, errors by category, contracts by result, and Go runtime memory and goroutines. No new dependencies; the text format is generated directly. Mounts on the loopback diagnostics server, with `URNETWORK_METRICS` binding it on the Tailscale address for remote scraping.
- **`urnet-tools config`, `history`, `profile`, `metrics`, `dashboard` (PR #568)**: settings with their sources, the provider's 1000-entry command audit ring, the tuning profile, a live toggle for the metrics endpoint without a restart, and a terminal status panel. `set` now warns when a key needs a restart to take effect.
- **Control socket hardening (PR #573)**: `SO_PEERCRED` checks the connecting UID against the provider's on Linux, a five-second per-line read deadline closes the slowloris shape, requests are capped at 64 KiB and values at 4 KiB, and protocol version negotiation rejects anything newer than it understands rather than parsing it hopefully.
- **Server-side control validation and restart flagging (PR #566, #572)**: all fourteen keys are validated at the server, so a raw socket client cannot persist a value that breaks the next start. Each response carries a `needs_restart` flag computed from whether the key has a live side effect, so it cannot go stale. `v4` and `v8` are accepted as turbo aliases.
- **Phased startup banner and Ready summary (PR #571, #577)**: boot messages are collected into phases with per-phase results and timings over 100 ms, closed by a single `✅ Ready` line carrying version, profile and proxy count. Suppressed under systemd and by `URNETWORK_STARTUP_BANNER=0`.
- **Version stamp for `-trimpath` builds (PR #579)**: `-trimpath` strips `-ldflags` out of Go build info, so `main.Version` cannot be read back out of a release binary. The release workflow now embeds `URNET_VERSION_STAMP=<version>` as program data, which survives, and the tool scans for it without executing the file. Works on a stopped provider, a cross-architecture binary, and any release build, and is what lets a v30.9 tool verify an upgrade to v31.
- **Provider version over the control socket (PR #578, #580)**: a running provider answers a `version` request with its own build string, the only source reporting what the process is rather than inferring it from the filesystem. Falls back to `/proc/<pid>/exe` and then on-disk build info, which matters because `update` swaps the binary before the HotSwap preflight runs and a rename over a running binary leaves that path naming a deleted file.
- **Conntrack tuning in `optimize` (PR #569)**: both settings are host-wide and persist via /etc/sysctl.d. The one-hour established timeout is deliberate: a proxy node churns connections, and at the kernel default of five days the table fills with entries for peers that are long gone. Idle connections elsewhere on the box that do not send keepalives can be reaped after an hour. `nf_conntrack_max` was never set, leaving the 65,536-entry kernel default, which is far too low for a proxy box. It is now scaled to host RAM across six brackets, from 131,072 under 1 GB to 4,194,304 above 32 GB, with `nf_conntrack_tcp_timeout_established` cut to an hour. Skipped cleanly when the module is not loaded, persisted to `/etc/sysctl.d/99-urnetwork.conf` with rollback.
- **Heartbeat build line (PR #595)**: `[health][build]` leads every five-minute heartbeat tick with version, profile and node name, so a log tail can be tied to a build after the startup banner has scrolled out of the RAM log window.
- **Message-pool health verdict (PR #592, #595)**: an aggregated pool line in the heartbeat reporting `ok`, `warming`, `watch` or `leak` in plain language, keyed on the outstanding low-water mark and all-time allocation count rather than a return percentage, which cannot detect a leak proportional to throughput. Needs an hour of uptime before the floor tracking is meaningful, which `warming` states with a countdown. Documented with worked examples in `LOG_REFERENCE.md`.
- **`urnet-tools proxy ids` (PR #593)**: shows the `client_id` the platform assigned to each proxy, including the `direct` transport. Bearer tokens in the client-JWT store are never printed.
- **Proxy resolution state in systemd `STATUS=` (PR #591)**: reloads publish the configured proxy count and a resolution state; a failed or empty fetch publishes the failure with its reason and emits a rate-limited operator warning. Operator-facing only, and `READY` stays decoupled from health.

### Fixed
- **ICE IPv6 Host Candidate Guard**: Gated synthetic IPv6 host candidates behind `egressIPv6Usable()` send probes to prevent blackholed cellular routes on Android from stalling ICE connection negotiation.
- **Memory Target & GC Tuning at Startup**: Persisted `gomemlimit` and `gogc` applied at bootstrap and runtime via debug APIs.
- **systemd `Type=notify` start/restart wedge (PR #543, #546)**: the provider signalled `READY=1` only once a proxy had authenticated, so a `Type=notify` unit waited on a readiness signal that arrived late or never and `systemctl start`/`restart` hung. READY is now sent when the provider is self-managing.
- **Provider version on `-trimpath` release builds (PR #547)**: `urnet-tools` could not resolve the version from a stripped release binary, so `update --tag` reported the wrong version and refused to align an install to a requested release.
- **HotSwap drain misreporting a successful handoff (PR #552)**: the liveness monitor armed even when no child process backed the session, where `Wait` returns immediately; that case and `ctx.Done()` were both ready and Go chose at random, so a successful handoff could exit non-zero as a dead candidate.
- **Profile defaults clobbering persisted `gomemlimit`/`gogc`**: applying a profile overwrote values an operator had set explicitly.
- **Installer `json_escape` emitting unquoted JSON**: corrupted `pending_overrides.json`; queueing an override under `sudo` also left the file owned by root.
- **The weekly auto-update timer had never completed an update (PR #581, #583)**: `ExecStart` was a bare `urnet-tools update`, systemd hands a oneshot unit `/dev/null` on stdin, and the version prompt was gated only on `!force`, so every run exited 1 on the refused read. The two confirmation gates ignored the interactive decision the target pickers already used, so one run could skip the picker as non-interactive and then demand a prompt. A single `unattendedUpdate()` decision now drives all of them: force, a systemd `INVOCATION_ID`, or stdin on `/dev/null` skip prompts, while other non-interactive stdin still refuses, so a pipe or a pty-less SSH session cannot silently auto-confirm. Concurrent updates of one binary are serialised by an flock. Not a v31 regression; the install script has written that `ExecStart` since before v3.23.0-fix.30.9.
- **HotSwap had never engaged on a release build (PR #578)**: `update` swaps the binary before the preflight runs, and a rename over a running binary leaves `/proc/<pid>/exe` naming a deleted file, so the preflight read an empty version and declined with a reason naming a version that was never the problem, returning before the unit-type check ran.
- **A sentinel-update unit appeared as a provider (PR #576)** and **discovery now corroborates units against `ExecStart` (PR #562)**.
- **The HotSwap decline reason was lost on the update path (PR #560)**, so a fallback to restart reported the fallback without saying why.
- **Profile commands with no arguments print current status (PR #564)** rather than erroring on the empty argument.
- **HotSwap handed off to the binary it replaced (PR #588, #594)**: the handoff resolved its image with `os.Executable()`, which follows the inode rather than the path, so once the updater moved the running binary to a backup it spawned the backup. The old build came up, announced READY, the parent drained, and the operator saw a clean zero-downtime swap that changed nothing. `updateProvider` skips the unit restart when the trigger succeeds, so the fallback that would have worked was bypassed precisely because the handoff "worked". Not a regression. The install path is now captured at init and the candidate is validated with `syscall.Access(X_OK)` as a regular executable file. A provider on an earlier build cannot be HotSwap-upgraded onto this one; restart the unit once.
- **Version resolution defeated by `+dirty` pseudo-versions (PR #587)**: Go 1.27 with `-trimpath` emits `v0.0.0-<ts>-<hash>+dirty` in build info. The filter rejected `+incompatible` but not `+dirty`, so the pseudo-version passed as real, short-circuited the version-stamp scan, and `hotSwapVersionOK` declined a valid candidate.
- **Empty version in the batch confirmation gate (PR #584, #585)**: `confirmGateMulti` now records the version running before an unattended swap, and falls back to reading the binary when the in-memory provider struct carries no version.
- **RAM log trim replaced kept history with NUL padding (PR #596)**: the five-second trimmer read with `ReadAt` on a write-only descriptor, so the read always failed with `EBADF`; the error was discarded and the zero-filled buffer was written back as the preserved history. Both the main and the important buffer were affected, the latter 97% padding. Measured on a live node, 3.5 MB of NUL bytes preceded the first real line. Not a regression, and never covered by a test.
- **`urnet-tools logs` followed stored state, not the provider (PR #597)**: the RAM-log decision came from the control-state chain alone, while the provider decides from `URNETWORK_RAMLOGS` in its environment. The two disagreed whenever the variable arrived via a unit body, a drop-in, an `EnvironmentFile=`, or `docker -e`, streaming an almost-empty journal while the provider wrote to `/dev/shm` twice a second.
- **Two unthrottled error lines (PR #598)**: `completeHandshake failed` (487 lines in 6m25s on a node with 1160 proxies) is now throttled per reason class rather than per client, since a per-client throttle on a busy box is no throttle at all; `[c]audit send error` is throttled globally. Both carry an `(N suppressed)` count.
- **Lossy carriers stalling the reliable lane (PR #586)**: ports upstream's WebRTC lane-gating fix. A full unreliable resend queue stalled the whole send sequence while a reliable relay lane sat idle. Unreliable flight limits are decoupled from the global resend-queue gate, acknowledgement RTT samples from unreliable transports are excluded from the RTT window, and ICE UDP connections get a 4 MB socket buffer. Running the port live surfaced three further defects fixed here: a flight reserve stranded on zero-byte acknowledgements, a message-pool double return in the multi-route write helpers, and a nil-map panic from a shadowed declaration in route selection that this change made reachable.
- **Phantom providers from non-provider siblings (PR #599)**: discovery fell back to matching any `urnetwork-*` unit name when `ExecStart` could not be read, so an unrelated unit appeared as a second provider and made every command on a single-provider box ambiguous. A name-only match now also requires a `.urnetwork` state directory.

### Changed
- **`urnet-tools ip-detect` is now `urnet-tools show-ip`**: the old name described the mechanism rather than the effect and read as a diagnostic that would report the IP, and sat awkwardly beside `direct`, which is about the address the provider serves on. `ip-detect` and `ipdetect` remain as aliases.
- **Settings changes are logged at the provider**: `set`, `clear`, the startup merge of queued overrides, and dashboard-label changes now emit log lines, so an operator can confirm from the node's own log that a change registered with the daemon. `get` stays silent because `status` polls it on every invocation.
- **`urnet-tools help` lists `hotswap`**: registered since PR #533 but never shown in the usage text.
- **Tuning help text corrected**: `turbo`, `auto`, `eco` and `lowmode` described writing a systemd drop-in; they have written the control-socket key since the v31 migration.

### Release Engineering
- **Pre-release shakedown sections Q-Z (PR #544)** and a **Docker Shakedown workflow (PR #545)** covering the v31 daemon and container surfaces.
- **Shakedown runs to completion (PR #550)**: section L read an unassigned variable, which under `set -u` aborted the suite silently, so every section after it had never executed. Both shakedown workflows now surface the script's raw output on failure and warn on a report with no SUMMARY.
- **CI wall clock roughly halved (PR #551)**: race suite sharded across three runners, lint parallel to the tests, retry no longer masking genuine failures, and concurrency groups on the three workflows that lacked them.
- **Manual shakedown dispatch can target a release (PR #548)**.
- **Shakedown failures across four surfaces (PR #558)**: Docker architecture selection, `--tag`, restart verification and hotswap. Plus a Docker shakedown that raced image publication (PR #555) and review findings on both workflows (PR #557).
- **Two tests fixed (PR #559, #563)**: an exec race in the discovery fallback test, and an auto-update test that mutated the host it ran on.
- **A test that passed only in CI (PR #589)**: `TestCurrentProviderNetworkID` redirected `HOME` but not the package-level client-JWT store, which is initialised at load time from the real home directory, so it failed on any machine that had ever run a provider.
- **`sn-status` error contract and flake (PR #590)**: `FetchSnStatus` always returned a nil error, so a total API outage surfaced as zero-value assertion mismatches. It now errors when both independent calls fail while preserving partial results, and each call gets its own timeout budget instead of sharing one across three.
- **Update timer unit passes `-y` (PR #583)**: makes the non-interactive intent explicit in the file an operator reads.

---

## [v3.23.0-fix.31.0-rc1] — 2026-09-08

_Pre-release tag._

### Added

- **Zero-downtime HotSwap** ([#533](https://github.com/full-bars/urnetwork-3.23-fix/pull/533)): a running provider hands its listener, its coordinator session and its clients to a replacement binary instead of restarting. The handover is verified before the old process yields, rolls back if the candidate fails to take over, and works for a container's PID 1 as an in-place `execve`.
- **A control socket for live settings** ([#536](https://github.com/full-bars/urnetwork-3.23-fix/pull/536), [#537](https://github.com/full-bars/urnetwork-3.23-fix/pull/537), [#538](https://github.com/full-bars/urnetwork-3.23-fix/pull/538), consolidated in [#539](https://github.com/full-bars/urnetwork-3.23-fix/pull/539)): `~/.urnetwork/provider.sock` replaces the nine settings files two processes used to share. Settings that need a restart are queued in `pending_overrides.json` and applied once on the next start.
- **Public IP autodetection and a dashboard display name** ([#534](https://github.com/full-bars/urnetwork-3.23-fix/pull/534)): a native or systemd install detects its public IPv4 the way the Docker entrypoint already did, with `urnet-tools ip-detect on|off|status` to opt out, and `urnet-tools rename` sets the dashboard label.
- **Multi-provider `urnet-tools` UX** ([#539](https://github.com/full-bars/urnetwork-3.23-fix/pull/539)): ordinary commands scope to the current user's provider, `providers` lists what is running, a cross-user target self-elevates under the full binary path, and a manage-time refusal now says how to narrow it.
- **JWT refresh logs its transfer stats**: step 3 of the refresh parses `paid_bytes_provided` and `unpaid_bytes_provided` and prints them in human units.

### Fixed

- **systemd READY was sent before the provider could serve** ([#543](https://github.com/full-bars/urnetwork-3.23-fix/pull/543)): the provider reports ready when it is self-managing, not when a proxy authenticates.
- **`json_escape` emitted unquoted JSON**, corrupting `pending_overrides.json`; and a queued override written under `sudo` left the file owned by root.
- **Profile defaults clobbered persisted `gomemlimit`/`gogc`**, and those values were applied only live rather than at startup.
- **HotSwap was offered on a unit that cannot support it**: the zero-downtime handoff is now gated on `Type=notify` rather than on version alone.
- **The darwin build was broken by `SOCK_CLOEXEC`**, and a Linux-only root gate check failed on other platforms.

### Changed

- **CFAA blocklist refreshed twice** ([#540](https://github.com/full-bars/urnetwork-3.23-fix/pull/540), [#542](https://github.com/full-bars/urnetwork-3.23-fix/pull/542)): 48,427 to 43,479 and then to 43,809 IPv4 entries; 453 to 438 and then to 469 IPv6 entries. Data only.
- **CI no longer triggers every smoke workflow twice for a `fix/*` branch**, and never publishes `:latest` for a pre-release tag.

## [v3.23.0-fix.30.9]

### Added
- **TCP-return retention across ack timeouts (PR #506, #521)**: in-flight packets on TCP return paths are retained past ack deadlines instead of being prematurely dropped, protecting relay session continuity and operator billing credit during transient network stalls. Retained items are bounded by `MaxRetainedBytes` and cleanly drained on flow resets (`rstFlow`).
- **Retention health telemetry (PR #506, #521)**: structured `retained_ack` and `retained_drop` events written to the persistent health log (`proxy_health_log.go`) via a buffered, thread-safe writer.
- **Bulk proxy ingestion via `proxy paste` (PR #502, #520)**: operator command accepting raw proxy lines from stdin, files, or HTTP(S) URLs. Auto-detects and normalizes `host:port`, `socks5://`, `user:pass@host:port`, and CSV formats.
- **Signal-safe proxy-paste temp cleanup (PR #520)**: unlinks plaintext temporary proxy-paste files on `SIGINT`/`SIGTERM` with cross-platform process signaling (`os.Process.Signal`) and restores default signal disposition.
- **SSRF guard on proxy sources (PR #522)**: dial-time and redirect-time verification blocking all operator-supplied URLs from reaching loopback, link-local metadata (`169.254.169.254`), private RFC1918, IPv6 ULA, and multicast addresses.
- **Dynamic direct transport toggle (`provider direct`) (PR #510, #514, #516, #517)**: CLI command (`provider direct on|off`) to enable or disable native direct IP transport on demand without restarting SOCKS5 proxies. State is persisted atomically to `~/.urnetwork/direct_override`.
- **Usage & traffic accounting (`urnet-tools usage`) (PR #507, #521)**: aggregate traffic accounting distinguishing billable relay bytes from control-plane protocol overhead, with rolling time-series summaries (`24h`, `7d`, `30d`, `lifetime`).
- **`choose_network` presets (PR #504)**: fast target switching using `main` and `beta` network presets.
- **Persistent DoH cache (PR #496)**: cached DNS resolutions and per-server scores persist across restarts, so a reboot no longer cold-starts the resolver.
- **Upstream production DoH optimizations P1-P3 (PR #495)**: server scoring, serve-stale (RFC 8767), staggered launch, single-flight coalescing, memory budget, TLS resumption, and warm-up on resolver failure.
- **Client JWT hot-restart v2 (PR #494)**: expired client JWTs are renewed on startup and identities are snapshotted before restart, gated on network compatibility. A provider restarts with a working identity instead of re-authenticating from scratch.
- **Warm proxy priority on startup (PR #526)**: evaluates local client JWT store on boot and prioritizes warm, previously-authenticated proxies during startup selection to minimize validator probe latency.
- **Accelerated startup stagger for warm proxies (PR #526)**: reduces connection launch stagger from 2s to 100ms for known-warm proxies, slashing cold-start ramp times by up to 95%.
- **Network ID conflict guard (PR #526)**: hardens `AnyNetworkID` against mismatched network identity state, ensuring strict network identity consistency.
- **Subnet 25 telemetry & status dashboard (`sn-status`) (PR #528)**: live Bittensor Subnet 25 miner and node telemetry displaying global ranking, top-200 tier eligibility, net bandwidth provided (MiB/GiB), registered coldkey (SS58 & Hex), subnet epoch lifecycle, and finalized epoch merkle pool payout share (basis points & percentage). Available across `urnet-tools sn-status`, `urnet-docker sn-status`, and `provider sn-status`.
- **JSON machine telemetry output (`--json`) (PR #528)**: machine-readable JSON output for `sn-status` across all CLI tools for automated monitoring, scraping, and alerting fleets.
- **Subnet 25 ranking API client bindings (PR #528)**: Go SDK methods `NetworkGetRankingSync` (`GET /network/ranking`) and `StatsLeaderboardSync` (`POST /stats/leaderboard`) on `BringYourApi`.
- **Bittensor operations guide (`docs/Bittensor-Operations.md`) (PR #528)**: operator guide covering Subnet 25 mining mechanics, coldkey registration, epoch lifecycle, top 200 cutoff requirements, reward claims, and CLI monitoring workflows.
- **Go-native `idle-update` (PR #525)**: polls billable throughput rate before updating in place, ensuring active client sessions aren't severed, with configurable `--timeout` ceiling and `--threshold`.
- **Docker CLI parity (PR #525)**: `urnet-docker direct`, `urnet-docker usage`, and `urnet-docker proxy paste` delegating to in-container commands.
- **Tilde home path expansion (PR #525)**: expands `~` in `--file` and positional arguments across CLI tools and shells.
- **CFAA blocklist sync (PR #527, #530, #532)**: refreshed Computer Fraud and Abuse Act IP blocklists from upstream definitions (IPv4: 48,427 entries, IPv6: 453 entries).
- **Automated CFAA blocklist sync CI/CD (PR #529, #531)**: daily automated GitHub Actions cron workflow and repository dispatch trigger that monitors upstream `urnetwork/connect` for blocklist updates, verifies lookup table integrity, and opens automated pull requests with Discord webhook notifications.
- **Provider version in `urnet-tools status` (PR #497)**: each provider's actual running version is shown.
- **Subprocess timeouts (PR #499)**: every discovery and systemctl call is bounded (5s/10s) so a hung process cannot wedge the tool.

### Changed
- **`pathTable` LRU eviction & bounds (PR #513)**: multi-hop route table in `ip.go` guarded with mutex locking, bounded size cap, and LRU pruning to eliminate unbounded memory growth under peer churn.
- **DNS negative-cache bounded pruning (PR #513)**: replaced unbounded negative DNS caching with periodic sort-based eviction.
- **Message pool metrics & refcount safety (PR #505, #521)**: converted pool size distribution mutex (`sizeDistMu`) to `sync.RWMutex` and added bounds checking against buffer counter underflows/overflows.
- **urnet-tools security audit remediation (PR #499, #505, #509)**: version strings are read from Go build info when recorded; discovered binaries are still executed as a 3s-timeout `--version` fallback when build info lacks a version (e.g. `-trimpath` builds); `set`/`fast-auth`/`self-heal` chown created state files to the provider user instead of leaving them root-owned; secure hub install verification via release digest and exclusive temp download; `runtime.GOARCH` arch detection; atomic drop-in writes with `%%` escaping.
- **Reliable provider updates (PR #497, #503)**: post-restart verification reads running process image via `/proc/<pid>/exe`; redundant reinstall for stale on-disk binaries.
- **Direct reloader lifecycle (PR #514, #517, #521)**: compare-and-delete CAS tracking and clean `cancelMap` cleanup prevent orphaned cancellation tokens or race conditions during rapid reload cycles.
- **Bounded DNS resolution (upstream)**: DNS lookups capped at 3s and abort on the winning server.
- **Source comments cleanup (PR #518, #519)**: removed stale review-process references and AI attributions for clean codebase hygiene.

### Fixed
- **Contract billing debit rounding (PR #521)**: fixed `unack()` debit rounding to ensure packets discarded at the retention backstop ceiling are never erroneously credited as acknowledged billable transfer.
- **Cross-platform build compatibility (PR #520)**: replaced POSIX-only `syscall.Kill` with standard `os.Process.Signal` for clean Windows (`GOOS=windows`), macOS (`GOOS=darwin`), and Linux compilation.
- **SSRF test fixture loopback isolation (PR #522)**: added unexported `ssrfAllowLoopback` toggle to allow local `httptest.NewServer` mock fixtures during test runs without relaxing strict production guards.
- **Direct toggle `<state>` argument parsing (PR #517)**: fixed docopt command parsing to properly recognize positional state arguments.
- **Root PATH sanitization (PR #517)**: preserved accessible executable directories in system search paths while preventing ancestor directory traversal.
- **Uninstall path guard (PR #499)**: `uninstall` refuses to delete well-known system directories; no longer removes arbitrary absolute paths.
- **Update confirm no longer hangs (PR #499)**: the non-TTY confirm path returns an error instead of blocking on stdin forever (cron/CI-safe).
- **Chown of state dir (PR #499)**: `chownLikeStateOwner` no longer a self-referential no-op; the provider's state dir is correctly owned.
- **DoH concurrent-map race (PR #499 / upstream)**: `stateLock` restored around the stale-cache read.
- **14 parity findings resolved (PR #523)**: fixed independently confirmed findings from the meso-miner parity port audit across provider core, memory allocation, and connection management.

### Removed
- **Unreliable outage watcher**: the alert-webhook-based outage signaling feature was removed (parity with meso-miner). It produced false outage signals.

---

## [v3.23.0-fix.30.8]

### Added
- **Pelican panel egg support (PR #480)**: the hardened provider image is now importable into the Pelican game-server panel as a one-click egg (`pelican/egg-urnetwork-323fix.json`, PLCN_v3). `BUILD` (stable/nightly/jwt) and `USER_AUTH` are user-editable; `PASSWORD` and `AUTHCODE` are admin-only. `PELICAN=yes`, `ENABLE_VNSTAT=false`, and `ENABLE_IP_CHECKER=false` are hidden and non-editable in the egg, pinning the audit-preferred defaults.
- **Pelican boot smoke (PR #480)**: `docker/scripts/test_pelican_smoke.sh` drives `pelican_panel.sh` against a fake provider binary. Empty credentials fail fast, jwt mode routes to `auth-provide` with the code, stable mode routes through the auth loop to `provide`. Runs in `test-and-lint`.
- **Pelican behavioral gate tests (PR #480)**: `docker/scripts/test_pelican_gates.sh` adds 15 mutation-checked tests covering the PELICAN update gate in `start_nightly.sh`, the `urnet-tools update` refusal, the `$HOME`-based state-dir resolution in `proxy-health.sh` / `proxy-traffic.sh`, and the egg-JSON invariants (non-empty rules arrays, known validator tokens, unique env_variable and sort values). Wired into CI as its own step.

### Changed
- **Pelican-gated runtime update policy (PR #480)**: under `PELICAN=yes`, `start_nightly.sh` short-circuits the update check (bootstrap and daily watcher) and `urnet-tools update` exits 1 with a refusal message. The published image is the single source of truth under a panel; a runtime fetch would silently swap the audited fork binary for whatever the release API serves mid-flight. Non-Pelican Docker deployments keep the v3.23.0-fix.30.6 behavior unchanged.

### Fixed
- **Docker state-dir split-brain (PR #480)**: all `docker/scripts/*.sh` now resolve provider state at `$HOME/.urnetwork` instead of the hardcoded `/root/.urnetwork`. Update-pending markers already used `$HOME`; the auth path was the outlier. Any container run with `HOME != /root` (e.g. Pelican eggs) would have silently split auth and marker state across two directories. Default-Docker behavior is unchanged (the image still ships with HOME=/root); the fix is a no-op for `docker run` and a correctness fix for panel/egg deployments.
- **Pelican egg vnStat note (PR #480)**: reworded the vnStat description in `pelican/README.md` and the egg JSON. The off-by-default posture in the egg is an operator-preference default, not a current-exposure claim; vnStat was patched long ago.

## [v3.23.0-fix.30.7]

### Added
- **Paid and file-list proxy grading (PR #458)**: the provider now table-probes every tracked paid and file-list proxy and assigns an A-F reachability grade. Before this release those proxies were never graded. Trimming and the proxy summary could not see their health. A per-sweep summary line reports how many proxies were graded and how many are pending. Proxies are only demoted on positive evidence, never on silence.
- **Adaptive sample sampling (PR #458)**: a probe starts small (min_sample_width) and grows toward max_sample_width only while a proxy's score stays borderline (within pass_bar plus or minus border_line_band). Clearly-good and clearly-dead proxies settle at the small width and spend almost no probe bandwidth.
- **Honest pending status (PR #458)**: a proxy the probe reaches but cannot decide (too few of the sampled hosts resolvable from the box) is marked pending instead of being assigned a fabricated grade.
- **Per-tick probe budget (PR #458)**: one 5-minute scoring sweep probes at most max_paid_probes_per_tick proxies (default 200). Oldest-stale probes are scored first.
- **Stage-0 liveness gate (PR #458)**: paid proxies get a one-dial SOCKS5 and API reachability check before a sample block.
- **Restored emoji and sectioned help (PR #459)**: `urnet-tools` and `urnet-docker` restore the approved emoji and sectioned per-command help.

### Changed
- **Probe configuration JSON (PR #458)**: proxy probe settings now read from `~/.urnetwork/proxy_probe.json` with keys `enabled`, `sample_width`, `min_sample_width`, `max_sample_width`, `timeout_ms`, `pass_bar`, `preferred_bar`, `border_line_band`, `max_paid_probes_per_tick`, and `stage0_liveness`.
- **Restart targeting for user-owned units (PR #459)**: a fix corrects restart targeting when a provider unit is owned by another user. The restart runs against the right unit instead of the current user's scope.
- **Suite test hardening (PR #460)**: the provider test suite replaces a fixed-wait flake with an adaptive wait and makes a paid-grader test hermetic. No production behavior change.

---

## [v3.23.0-fix.30.6]

### Added
- **Real per-command help (PR #448, #453)**: `urnet-tools` and `urnet-docker` now route through the Cobra CLI framework. Every command prints real per-command help with `-h` or `--help`. Each help page has a usage line, a short description, and copy-paste examples. The `urnet-docker proxy` command is a parent with 12 subcommand help pages. `urnet-docker exec -h` renders the in-container command's own help. `--help` never executes an action.
- **In-place container update (PR #453, #455)**: `urnet-docker update <target>` updates a running provider container in place. A target flag (`--unit`, `--user`, `--network`, or `--state-dir`) or a bare container name selects the container, then runs the in-container `urnet-tools update` self-update without recreating the container, behind the confirm gate. In-place update now works on any container image. A host-side repair fixes an in-container update script that older images ship broken (a busybox `mktemp` bug and a `pkill` name-truncation issue), then swaps the provider binary. If the swap stops the container (older images stop instead of relaunching), the command auto-restarts the same container so the provider comes back on the new binary. Plain `urnet-docker update` still self-updates the host binary only. The `self-update` and `selfupdate` aliases always update the host binary only.
- **Post-quantum session visibility (PR #455)**: the provider detects whether an end-to-end session uses post-quantum or classical TLS from the negotiated curve in the TLS connection state. The post-quantum hybrid curves `X25519MLKEM768`, `SecP256r1MLKEM768`, `SecP384r1MLKEM1024`, and `MLKEM1024` tag session-up and session-close log lines. A periodic `[pqe]` provider log line reports live post-quantum and classical session counts plus opens over 1h, 24h, 7d, and lifetime.

### Changed
- **Go 1.27.0 toolchain bump (PR #449)**: Compiler bumped from Go 1.26.4 to 1.27.0. Updates the `go` directive in `go.mod` to `1.27.0`, floats the CI `go-version` pins to 1.27 across all workflows, and moves the Docker builder base to `golang:1.27-alpine`. Validated under 1.27.0 with a clean `go build`, `go vet`, the full `-short -race` test suite, the complete cross-compile matrix (including 386 and mips/mipsle/mips64/mips64le), and a functional smoke. `go mod tidy` required no dependency changes.
- **CI skips heavy jobs on docs-only PRs (PR #452)**: the build, dash-compat, tool-functional-smoke, unix-lifecycle, and windows-lifecycle workflows skip their heavy jobs on pull requests that touch only `**/*.md`, `docs/**`, or `releases/**`. The labeler always runs. VirusTotal upload, lookup, and analysis timeouts are now non-fatal. A timed-out artifact records UNKNOWN in the summary instead of failing the scan job. Only a genuine malicious hit over the threshold fails the job.

---

## [v3.23.0-fix.30.5] — 2026-08-22

### Added
- **`urnet-tools` and `urnet-docker` dispatch migrated to Cobra** (PR #448): the 2,000-plus lines of hand-written switch logic became a structured command tree, so `-h` on a subcommand prints that subcommand's description and flags instead of the root usage list.
- **`urnet-docker update <target>` updates a container in place** (PR #453): with a target the in-container self-update runs behind the confirm gate without recreating the container; without one it falls through to the host tool's self-update. Verified on a 26.4 container updating to 30.4 with the container ID unchanged.
- **Go 1.27.0 toolchain** (PR #449): the builder images moved with it.

### Fixed
- **`update <target>` matched only the bare flag form**: `--flag=value` fell through the target gate; the help wrap was corrected with it.
- **Opus review findings on the in-place update** (PR #453): the container update path was remediated, and the shim test is POSIX-only so the Windows runner skips it.

### Changed
- **CI skips heavy jobs on docs-only pull requests and treats a VirusTotal timeout as non-blocking** (PR #452).
- **Greenteagc is recorded as viable on Go 1.27** (PR #450), and the 30.4 Windows detections are recorded as Microsoft-confirmed false positives (PR #451).
- **Changelog, fork-changes and Docker docs brought current for this release** (PR #454).

## [v3.23.0-fix.30.4] — 2026-08-22

### Added
- **Restored urnet-tools command suite (PRs #438, #439, #440, #441)**:
  - **`auth <code> [-f]`**: restored provider authentication delegation to provider binary `auth-provide`/`auth`. Bypasses global flag parsing so `-f` forces JWT overwrite; drops privileges to run as the target user when invoked by root. (PR #439)
  - **`choose-network <api> <connect>` (or `choose_network` / `--reset`)**: restored custom backend network endpoint switching. Persists to `~/.urnetwork/network.json`. (PR #439)
  - **`fast-auth [on|off|status]`**: restored management of the `~/.urnetwork/fast_auth` rate-limiter bypass marker with input validation and confirm gating. (PR #439)
  - **`set [help | <key> <val> | <key> off | <key>]`**: restored runtime tuning overrides in the provider state dir for the 7 keys read live (`node-name`, `report-interval`, `proxy-url-max`, `proxy-url-refresh`, `cleanup-scope`, `cleanup-interval`, `fast-auth`). Confirm-gated state writes with value validation. (PR #439)
  - **`session save <file>` & `session load <file>`**: restored encrypted provider identity export and import (AES-256-CBC bundle with PBKDF2/SHA-256). Features non-interactive password refusal/prompting, no-clobber protection, automated timestamped pre-load state backup (`~/.urnetwork.bak.<timestamp>`), same-account verification with `--allow-different-account` override, and state file chowning to the target owner. (PR #440)
  - **Hub command family (`hub init`, `link`, `unlink`, `test`, `onboard-cmd`, `show-password`, `open-port`, `update`)**: fully restored in pure Go. `hub init` sets up the hub service with password/salt validation; `hub link <url>` pairs providers with interactive CA verification and SHA-256 fingerprint pinning (TOFU with InsecureSkipVerify on initial fetch, verified against pinned CA/fingerprint) and confirm-gating on identity change; `hub test` verifies TLS chains against `hub_ca.pem` or pinned fingerprints; `hub onboard-cmd` generates URL-escaped one-liners; `hub open-port` manages firewall rules with confirm gates; `hub unlink` and `hub update` manage pairing and lifecycle. (PR #441)
  - **`self-heal [on|off|status]`**: restored CLI management for the `~/.urnetwork/proxy_self_heal` pressure-monitor marker. (PR #438)
  - **Linux `systemctl status` view**: Linux `status` command now renders live `systemctl status` (user or system unit) for systemd-managed providers, with graceful table fallback for bare processes. (PR #438)
- **Persistent proxy trim (`urnet-tools proxy trim <N>`) (PR #442)**: provider-side hard cap holding running proxies to `<N>`. Sorts all proxies by the A-F reachability grade (`score/graded/failed/tier/last_graded` across both file and URL cache sources) and sheds worst-graded proxies first (dead -> never-graded -> F -> D -> C -> B -> A), using billable traffic as a secondary tiebreaker to preserve earning proxies. Persists cap to `~/.urnetwork/proxy_trim` across restarts and reloads; clamps AIMD controller `TargetPoolSize` to prevent controller fighting; blocks over-budget additions during fetch/reload while preserving history. Supports `--preview` / `-n` / `--dry-run` and reset (`proxy trim off` or raising limit). (PR #442)
- **Persisted default provider target (`urnet-tools default`) (PR #436)**: added `default set <target>`, `default show`, and `default clear` commands to persist a preferred provider selector in `os.UserConfigDir()/urnet-tools/default`. Automatically resolves multi-provider invocations without repetitive targeting flags while logging a visible notice to stderr. Explicit flags (`--unit`, `--network`, etc.) and `--all` always take precedence. (PR #436)
- **Loopback-only diagnostics and profiling (`URNETWORK_PPROF`) (PR #423)**: opt-in diagnostics listener on loopback IP (e.g. `127.0.0.1:6060`) serving Go standard pprof profiles (`/debug/pprof/*`). Non-loopback IPs and hostnames are rejected. (PR #423)
- **Message pool metrics & categorized error tracking (PR #425, #426)**: loopback endpoints `/metrics/pool` and `/metrics/errors` exposed when `URNETWORK_PPROF` is active. Pool metrics track hits, misses, returns, active buffers, capacity, GC cycles, and per-size distribution. Error tracking captures rate-limited recent errors with truncated stack traces across transport, IP, proxy, and WebRTC layers. (PR #425, #426)
- **Adding Proxies documentation (PR #430)**: comprehensive per-OS guide covering proxy loading, formatting (`host:port` vs `host:port:user:pass`), Windows PowerShell traps (hidden `.txt.txt` extensions, quoting), and Docker host-side (`urnet-docker proxy add`) vs in-container (`--proxy_file`) usage. (PR #430)
- **urnet-docker command suite expansion & feature parity (PR #445)**: expanded host-side `urnet-docker` to full parity with `urnet-tools`. Adds first-class container lifecycle (`start`, `stop`, `restart`, `logs` with RAMLOGS `/dev/shm` streaming), authentication (`auth`, `fast-auth`, `choose-network`), runtime configuration (`set`, `report`, `self-heal`), observability (`summary`), session export/import (`session save/load`), hub management (`hub`), and proxy controls (`proxy add`, `clear`, `remove`, `refresh`, `add-source`, `remove-source`, `remove-dead`, `health`, `traffic`, `trim <N>`, `exclude`). Expands discovery to recognize upcoming `meso-miner` container naming. (PR #445)
- **Emoji and sectioned help menu styling (PR #437)**: restructured help output into clean, emoji-decorated sections: Core Commands, Session & defaults, Performance & Tuning, Proxy Management, Hub Management, Maintenance, Targeting rules, and Force options. (PR #437)
- **Windows and macOS status panel styling (PR #409)**: styled status UI rendering on Windows and macOS matching native terminal aesthetics. (PR #409)

### Changed
- **Adaptive GC governor consolidation (PR #428, #429)**: retired the separate eco runtime monitor loop and consolidated all dynamic GC tuning into a single `gcGovernor` in `resource_pressure.go` active across all profiles. The governor is the single writer to Go runtime `GOGC`, tightening GOGC below profile baseline based on the tighter of process heap fraction and host available RAM. Includes a 10s live-heap subtick to catch spikes fast. Controlled by `URNETWORK_ADAPTIVE_GC` (on by default); backs off if `GOGC` is explicitly set by operator. Pressure status reports `gc_state` and `heap_frac`. Eco profile retains static startup baseline (GOGC 50, 75% RAM GOMEMLIMIT). (PR #428, #429)
- **Universal finite memory limit (PR #427)**: guarantees a finite memory limit across every provider execution path and profile (tier 1/2 `--max-memory`, cgroup limits, host available RAM boundaries), eliminating unbounded memory paths on constrained hosts. (PR #427)
- **Flag targeting syntax (`--flag=value`) (PR #433)**: `urnet-tools` and `urnet-docker` now accept both `--flag=value` and `--flag value` across all targeting selectors (`--unit`, `--user`, `--network`, `--network-id`, `--state-dir`, `--match`). (PR #433)
- **Docker installer routing and PATH handling (PR #432)**: served standalone installer at `dl.fullbars.xyz/urnet-docker.sh` with GitHub raw fallback; automatically appends non-root install directory to `~/.bashrc` PATH; warns on unrecognized flags or extra positional arguments. (PR #432)
- **Docker build layer caching (PR #424)**: optimized Dockerfile builder stage by isolating `go.mod`/`go.sum` download into a dedicated cacheable layer and slimming the build context. (PR #424)

### Fixed
- **Proxy trim earning protection and F-tier eviction (PR #447)**: restructured proxy trim eviction hierarchy to protect active billable client traffic (`traffic > 0`) against idle proxies (`traffic == 0`). Confirmed failing F-tier nodes are evicted before untested/probationary ungraded proxies, and smaller earners shed before larger earners to preserve maximum operator revenue. (PR #447)
- **Same-user privilege rejection in journalctl, systemctl, and optimize (PR #446)**: resolved `Using the --machine= switch requires root privileges / Operation not permitted` on Linux non-root operations (`logs`, `restart`, `status`, `update`, `uninstall`). Centralized user-session dispatch in `systemctlUserArgs()` and `journalctlArgs()` to omit `-M` when targeting units owned by the current user. Decoupled `optimize` from provider discovery so host kernel sysctls can be applied on unconfigured nodes; auto-escalates via sudo when available. (PR #446)
- **Windows CI test pipe buffer deadlocks (PR #446)**: fixed 10-minute timeout hangs on Windows runners by draining `os.Pipe()` buffers concurrently across all test capture helpers. (PR #446)
- **Proxy remove data loss fix (PR #434)**: fixed a critical bug where `proxy remove` without `--all` previously forwarded an unconditional wipe; now correctly passes target addresses and `--match` patterns. `proxy add-source`, `remove-source`, and `remove-dead` now properly honor `--dry-run`. (PR #434)
- **urnet-tools reinstall recursion (PR #434)**: fixed `urnet-tools reinstall` self-recursion by delegating cleanly to `updateProvider`/latest. (PR #434)
- **Provider binary discovery on Windows and macOS (PR #435)**: fixed case-sensitive binary matching on Windows and enabled multi-path probing for `provider_beta` and `urnetwork` binaries across macOS and Windows; standardized `isPrivileged` execution seam. (PR #435)
- **Deleted binary handling on discovery and update (PR #433)**: stripped `(deleted)` suffix from `/proc/exe` discovery to prevent phantom binary paths; skipped backup staging when the running provider binary was already unlinked. (PR #433)
- **Windows installer architecture selection and uninstall (PR #408)**: corrected single per-arch asset selection on Windows; handled flat tarball extraction layout; fixed uninstaller path filter to match exact installed binary. (PR #408)
- **Connect NAT shard buffer test race (PR #443)**: fixed test-side data race in `ip_smtp_seam_test.go` by retaining pooled seam packets until asynchronous NAT shard processing completes. (PR #443)
- **Upstream monitor compact diff representation (PR #410, #431)**: replaced 60KB raw diff truncation with structured GitHub API deltas and introduced a fork-aware manifest to eliminate false-positive porting alerts. (PR #410, #431)
- **CI review workflow stability (PRs #411, #413-#417, #419, #420)**: repaired PR head SHA resolution, OAuth action triggers, and YAML indentation across review workflows before pruning non-functional triggers. (PRs #411, #413-#417, #419, #420)

### Security
- **CFAA IP security blocklist sync (PR #412)**: synchronized provider egress firewall tables with upstream feed (commit 6813e788), expanding IPv4 prefix coverage from 43,299 to 44,225 and IPv6 prefix coverage from 431 to 513. (PR #412)
- **Cross-user state and privilege isolation (PRs #439, #440, #441)**: delegated provider commands drop privileges to match target user; session save/load and hub link enforce strict file permissions (0700 state dir, 0644 trust files) and chown state assets to the unit owner; single-stdin password reader avoids TTY leakage. (PRs #439, #440, #441)

## [v3.23.0-fix.30.3] — 2026-08-19
### Added
- **Busybox-safe mktemp for in-container updates**: `urnet-tools update` now uses `mktemp -d /tmp/urnetwork-update-XXXXXX` (directory template, busybox-compatible) instead of the broken `.tar.gz` suffix template that failed with `Invalid argument` on Alpine images. One `rm -rf` cleans up. Test harness hardened with a busybox-enforcing `mktemp` stub that rejects any template not ending in `XXXXXX`. New coverage: zero-byte tarballs, partial downloads overwritten by mirror, update-pending marker lifecycle, architecture mapping, busybox stub regression guard. (PR #406)
- **Nightly self-update path hardened**: `start_nightly.sh` `func_check_update` stages the update in a busybox-safe temp dir, uses `curl -sfL` (fails on HTTP errors), cleans up on every failure path, and writes `update-pending` only after the binary swap + version write succeed. Previously it reused a shared `/tmp/urn_update`, downloaded with `curl -sL`, and set the marker before download/extraction completed; a failed update left the restart loop believing a new binary was installed. The metadata fetch is also guarded so a transient API or network failure cannot trip the script's `set -e` and kill the provider supervision loop; the check skips that attempt. The new binary is staged on the target filesystem and atomically renamed into place, and the version-file and restart-marker writes are checked so a partial install cannot falsely signal a restart.
- **Full semver docker tag on release pushes**: CI workflow preserves the full semver tag (e.g., `v3.23.0-fix.30.3`) instead of truncating to the minor segment. Operators can pull the exact release they deployed. (PR #405)

### Changed
- **Default-provider selection completed**: `urnet-tools` now detects the current OS user on Linux, Darwin, and Windows (`currentUserName` cross-platform), discovers the provider running under that user, and auto-selects it for read-only commands (`logs`, `status`, `summary`). Gated by a real privilege check: root callers never auto-default; unprivileged callers only auto-default when exactly one reachable provider belongs to them. Surfaces selection reason (e.g., `auto-selected: sole provider for user alice (pid 1234)`). Falls back to explicit `sudo <binary>` hint when an unprivileged user has `sudo` available but the target provider is owned by another account. Test seams cover privilege guard, fallback message, and cross-platform username resolution. (PR #404)

## [v3.23.0-fix.30.2] — 2026-08-18
### Added
- **Post-quantum encryption now works end to end**: the provider enables its encrypted session layer on the serving client (PR #400), and the control channel routes encrypted handshakes by generation id (PR #401). An app with post-quantum encryption turned on completes the handshake instead of stalling at the 60-second timeout. The provider stays Opportunistic, so plaintext apps are unaffected.

- **urnet-tools usage text**: the help output now lists the full command surface (performance, proxy, hub, and maintenance commands) instead of only the core commands. (PR #402)

## [v3.23.0-fix.30.1] — 2026-08-17
### Added
- **SMTP policy layer**: port 25 stays local-only; ports 465 and 587 require TLS. Owner-scoped eviction, one RST per rejected flow, and hostname-shaped EHLO/HELO arguments (PR #392).
- **CFAA IP blocklist sync**: tables updated from upstream's feed sources (PR #395).
- **Go tools fixes**: stopped-provider discovery restored, auto-update timers create/remove correctly, bare container-name targets accepted, cross-user provider discovery, and false-positive dashboard units excluded (PRs #393, #396, #399).
### Fixed
- **Smoke-test harness**: proxy refresh waits for started_at; gh API 5xx retries in the labeler (PRs #397, #398).
### Changed
- **Documentation restructure**: configuration tables, guide navigation, troubleshooting matrix (PR #394).

## [v3.23.0-fix.30] — 2026-08-16
### Added
- **urnet-docker host-side proxy commands**: `proxy add`, `clear`, `remove`, `add-source`, `remove-source`, `refresh`, and `remove-dead` now run from the host. The exec plumbing is hidden. Example: `urnet-docker proxy add ~/proxies.txt`.
- **macOS lifecycle support**: auto-start and auto-update use launchd agents. A headless fallback covers servers without a login session. Discovery uses pgrep because macOS has no /proc.
- **Runner-based functional testing**: smoke tests run real auth and real providers on free Linux runners, replacing the test droplet for tool testing.
- **Soak tests**: native and containerized providers each run a three-hour soak with a capped proxy pool, restart cycles, and client-ID reuse checks.
### Fixed
- **Windows uninstall could never delete anything**: the safety guard rejected all Windows paths. It now accepts absolute drive paths and rejects volume roots.
- **Windows auto-start/auto-update now use Task Scheduler** instead of the legacy startup-folder shortcut. Uninstall removes the scheduled tasks.
- **Windows state directory corrected**: now matches where the provider actually stores data.
- **Proxy clear worked only in the shell wrapper**: the tool now maps clear to remove-all.
- **Container wrapper supported only some proxy commands**: add-source and remove-source are now accepted.
- **Linux uninstall orphaned the auto-update timer**: the timer is now disabled on uninstall.
- **Build cache flake stalled image builds**: replaced with setup-go native cache and a retry.
### Changed
- **Docker image tag model is explicit**: `latest` means the last tagged release. `main` tracks the current code. CI tests pull `main`.
- **CI no longer false-passes**: tests verify provider authentication and fail loudly instead of swallowing errors.
## [v3.23.0-fix.29.1] — 2026-08-14

### Added

**RFC 7323 TCP timestamps, MSS negotiation, and initial-window warmup** (#380): the provider's TCP stack ports upstream perfvar's option handling. A unified option parser now reads MSS, window scale, and timestamp together, replacing the old window-scale-only parser. Timestamps negotiate from the peer's SYN and emit on every non-RST segment (SYN-ACK, data, pure ACK, FIN-ACK), backed by a monotonic clock and a reorder guard. DataPackets now clamp payload to the peer's advertised MSS (RFC 879/6691 data-only semantics, 536 floor). A new InitialWindowSize setting scales with the memory budget, is power-of-two, and clamps between 4 KiB and 128 KiB; the SYN advertises the literal unscaled window per RFC 7323, then the post-handshake ACK jumps to the warmup window. The fork keeps its existing 4 KiB/4 MiB low-RAM window profile rather than following upstream's move to 64 KiB/16 MiB. ReadBufferByteCount now accounts for the timestamp option so full IPv6 reads no longer split into a runt tail segment. Multiple independent reviews covered the port; new tests span the window-scale table (9 cases), MSS+timestamp extraction, malformed-tail parsing, SynAck layouts, timestamp bytes, PureAck TSopt, and DataPackets timestamp segmentation. Needs a fleet deploy. This is the only provider binary change in this release.

**VirusTotal scan of release binaries** (#381): release binaries are scanned before publishing with a two-tier gate. Zero to two detections pass (the known stripped-Go false-positive band), three to ten ship but are flagged for review, eleven or more block the release. The scan writes a report receipt with a per-artifact PASS/REVIEW/FAIL table and VirusTotal permalinks.

**ClamAV scan of release binaries** (#383): an EICAR self-test plus a clamscan pass over all 20 release binaries, run after the VirusTotal scan as a quota-free second opinion.

**Pre-release shakedown workflow** (#384-391): the gauntlet workflow is rebuilt and renamed to shakedown. It runs on a disposable 1 CPU/1 GB DigitalOcean droplet with guaranteed cleanup (trap plus job timeout), and covers a fresh install, auth, the Go tool, the full proxy lifecycle, URL sources against real free proxies, the admission pipeline, docker, the hub under systemd and docker, hot-restart identity, update --tag, self-update, a long observation phase with resource sampling, and clean shutdown. Tier-1 failures exit non-zero and fail the job; tag-triggered runs test the exact release being cut. The range along the way adds an apt-get update before the docker install, skips URL checks when auth failed, adds a report review step, posts the run verdict to a Discord webhook, adds a preflight connectivity gate, requires a public IPv4 before SSH, fixes the auth-check window to read the full journal, annotates Wacatac.C!ml as a confirmed false positive, and adds hub coverage for both systemd and docker. PR #391 bulletproofs the runner-to-droplet lifecycle: the test runs detached via systemd-run, the exit code survives through an atomic-rename sentinel, a deterministic gate maps 0/1/75/124 to PASS/RELEASE_BLOCKED/ENV_BLOCKER/HARNESS_CRASH, a separate 15-minute sweeper workflow destroys orphaned droplets older than 3 hours and reaps stale SSH keys, polling is bounded, the report streams each poll, a heartbeat detects a stalled script, and a concurrency group queues runs instead of cancelling. The lifecycle went through six rounds of independent review. A full run takes about 2 hours, bounded by a watchdog and job timeout.

### Fixed

**Provider panicked when HOME was unset**: the pre-release shakedown found a startup panic, "$HOME is not defined," on every invocation when the HOME environment variable was missing. This broke `--version`, one-shot commands, and the `update --tag` path in bare environments, such as a root shell with no HOME set. The client-JWT store now degrades to in-memory-only mode when HOME is unavailable; identity reuse is lost for that process, but the command still runs. The auth and proxy-config write paths now return errors instead of panicking. This fix blocked the first v3.23.0-fix.29.1 release attempt; the release was re-cut once the fix landed.

**DoH tests hung on live server lookups** (#382): TestDohQuery and TestDohCache queried live public DoH servers for a hostname that does not resolve, so the retry loop hung. A local httptest server now serves canned Google-style JSON DoH responses, and the tests fail fast instead of retrying forever.

## [v3.23.0-fix.29.0] — 2026-08-14

### Added

**Hub A-F grade surfacing** (#360): the provider's report payload now carries each proxy's grade (`score/graded/failed/tier/last_graded`, omitempty), the hub ingests it into a new `proxy_grades` store (latest-wins per node+proxy per hour, 7-day retention), and the dashboard renders a color-coded A-F **Grade** column — distinct from the existing traffic-composite **Score** column. URL-store grade timestamps are honest: a real `LastGraded` field stamped only on genuine stage-1 grades, never on liveness-only probes. Grade tiers are sanitized to exactly A-F at ingest plus HTML-escaping at both render sites (stored-XSS hardening). Needs a fleet deploy — provider binary + hub image both change.

**urnet-tools/urnet-docker tool distribution + self-update** (#362): The Go tool rewrite from #345 is now actually shippable. `release.yml` builds `cmd/urnet-tools` and `cmd/urnet-docker` for the full matrix and attaches them as release assets (`urnet-tools-<os>-<arch>` / `urnet-docker-<os>-<arch>`, bare — no `.exe` even on Windows); `Provider_Install_Linux.sh` and `Provider_Install_Mac.sh` install the Go binary (sha256-verified against the release API digest) instead of self-copying the shell script, with a legacy fallback for releases that predate the asset; docker-only users get a standalone host-side installer (`scripts/install-urnet-docker.sh`, `curl ... | sh`, digest-verified). `urnet-tools update` now also refreshes the tool binary itself; new `self-update`/`selfupdate` subcommands update only the tool (works on boxes with zero providers); `urnet-docker update` self-updates the tool. The structural check on downloaded binaries is now platform-aware (ELF/Mach-O/PE) so self-update works on macOS and Windows, not just Linux. Needs a fleet deploy for the installer change; the new assets ship with the next tagged release.

**Paid/free probe divergence + earn-skip** (#357): stage-1 table probing splits into two cadences. Paid/file proxies move to a wider stale window (6h calm / 3h hot vs the URL 3h/1h) and are skipped entirely when they show a positive billable delta within the last 15 minutes — earn-skip — with a hard 24h force-probe ceiling and never-graded proxies always probed, so fail-fast can never be starved. The first URL fetch + probe pass is deferred 20s after process start, so a crash-looping box can never re-fetch/re-probe on every restart. The grade summary's stale ratio picks its freshness window per entry source. Needs a fleet deploy — probe cadence and probe spend change on every box at redeploy time.

### Fixed

**Earn-skip dead in production** (#357): the earn tracker stored per-address earning state under `ProxyHealthSnapshot`'s formatted `proxy[N] (addr)` keys while the paid grader looked up raw addresses — keys never matched, so earn-skip never fired and every paid proxy was always probed. Keys are now normalized on ingest and lookup, the tests drive the tracker through the real snapshot API, and per-address state is pruned to the live proxy set (and cleared when the health set empties).

**Resource-leak hunt — six findings + one audit follow-up fixed** (#367, #369, #370, #371, #372, #373, #374): a full pass over the multi-client window and encryption layer closed every confirmed leak. Source-count refcount now increments once per unique (bucket, path), pruning phantom (destination, source) map entries that grew monotonically at thousands-of-proxies scale (#367). The handshake timeout watchdog cancels the epoch ctx when the timeout records a failure, freeing the parked `runHandshake` worker against silent peers (#369). `removeClient` cancels each affected update's ctx so teardown goroutines wake immediately instead of stranding on the 120s idle timer — with a follow-up supersede guard so a successor update registered after cancellation is never clobbered by the stale teardown delete (#370, #373). The PQE path derives a sane encryption IdleTimeout (max of send/receive buffer idle) instead of adopting the 0 default, arming the reap loop and eliminating zombie refs==0 sessions (#371). `SendEncryptedControl` returns its pooled buffer on both cancel-path exits instead of leaking one per teardown race (#372). `Cancel()` now unsubscribes the receive callback like `Close()`, releasing dead clients' callback chains immediately (#374). Every fix is mutation-proven and confirmed by independent review passes. Needs a fleet deploy — provider binary behavior changes.

**CI flake root causes closed** (#364, #368): the window-stats `-short` timeout is now derived from the bucket duration (was a fixed 300ms coin flip under `-race` load), and the paid-grade undecidable test pins the pass counter to a hostname-only probe block (literal-IP targets in the host table bypass the DNS-fail cache by design, so ~4.7% of passes always dialed one). CI is deterministic; test-only, no fleet deploy.

**Go-tool gauntlet — 8 bugs found + fixed by live field testing** (#376): the rewritten Go `urnet-tools`/`urnet-docker` was gauntlet-tested on a fresh droplet against the live beta network. Found and fixed: no version command (added `version`/`--version`/`-v`); `report`/`hot-restart` delegated to non-existent provider subcommands (now write `~/.urnetwork/report_url` / restart the unit, with a confirm gate + `--force` on hot-restart); systemctl argv omitted the unit name (start/stop/restart/hot-restart failed with "Too few arguments"); `summary` delegated to the wrong flat form (now the provider's nested `proxy summary`); `proxy add-source`/`remove-source` missing entirely (URL sources now manageable); `proxy refresh --force` didn't forward `--force` (warmup gate never bypassed); proxy `--help` at any position showed root help or hung (now proxy-specific help, never executes). The report_url override is written 0644 so a provider running as a different user can read it. Needs a fleet deploy — the tools ship as release assets with this release.

## [v3.23.0-fix.28.0] — 2026-08-12

### Added

**In-process per-proxy client-JWT renewal** (#356): the beta backend (and mainnet's new token format) mints 24-hour JWTs; previously each proxy's client JWT was minted once at startup and never renewed in-process, so after ~24h of uptime every proxy's token expired, all auths 401'd, and providers silently became black holes. The provider now renews each proxy's client JWT 12h before expiry (hourly retry, immediate on 401), re-signing the SAME client_id through `/network/auth-client` so server-side reputation is preserved; a process-wide mutex + shared rate limiter prevent API stampedes; a no-op on backends still issuing long-lived tokens.

**EncryptionMode tri-state port (scope 2)** (#350): Off/Opportunistic/Required mode with fail-closed gates; bounded per-peer TLS establishment (60s handshake timeout default, was unbounded) (#353).

**urnet-docker exec flag forwarding** (#349, #352): a `--` separator forwards inner flags verbatim; unknown leading flags error instead of being silently dropped; the ramlogs hint resolves the real container name; `urnet-docker logs` follows the right provider.

### Fixed

**window-stats test flake under -short** (#354): deterministic bucket span.

## [v3.23.0-fix.27.0] — 2026-08-09

### Added

**A-F proxy quality tiers + best-overall cache eviction** (#343): every URL-source proxy that survives the stage-1 gate is assigned a letter grade (A >= 0.9, B >= 0.8, C >= 0.7, D >= 0.6, F < 0.6); all sources' candidates pool into a single best-first admission funnel up to the cap; the cache evicts lowest-grade entries when full; the fetch cycle probes only new addresses while the reaper's stale sweep refreshes cached grades.

**Read-only grading for paid/file-list proxies** (#344): a background sweep grades every non-URL proxy the box serves with the same stage-1 table probe on the same 1-3h stale cadence, persisting Score/Graded/Failed/LastGraded into `proxy.state`. Read-only by construction — grades never gate admission, evict, or feed give-up/cleanup.

**Periodic A-F grade summary + grades.log history** (#346): every 5 minutes (configurable via `proxy_grades.json interval_sec`) the provider logs a running tier snapshot — per-source A-F breakdown, changes vs the previous round, median/p95/min score stats, next-probe countdown — and per-proxy tier changes emit delta lines into the important buffer and a durable per-day history at `~/.urnetwork/grades/` (retention default 7 days).

**Provider-aware `urnet-tools` rewritten in Go** (#345): the fleet ops tool is now a single Go codebase (`urnet-tools` for process/systemd, `urnet-docker` for containers) that discovers real running providers, identifies each by its JWT network identity, refuses ambiguous targets with an inventory table, and gates destructive ops behind a confirm prompt. All 25 legacy subcommands dispatch with verified parity.

### Breaking Changes

- **`urnet-tools` is now a Go binary** (#345): drop-in at the same path; operators on multi-provider boxes must now specify a target (`--unit` / `--user` / `--network` / `--network-id` / `--state-dir`) — the tool refuses to guess.


### Added

**A-F letter-grade proxy quality tiers** (#343): Every URL-source proxy that survives the stage-1 gate is assigned a letter grade (A >= 0.9, B >= 0.8, C >= 0.7, D >= 0.6, F < 0.6). Best-overall cache eviction keeps the highest-tier proxies when the cache is full; all sources' candidates pool into a single A-to-F admission funnel (best-first up to the cap); the fetch cycle probes only new addresses while the reaper's stale sweep refreshes cached grades; a per-cycle A-F grade breakdown is logged. Cross-source duplicates are probed once per cycle; the eviction tie-break uses the grade score. Seven review passes (two external review rounds plus independent passes) closed the fetch-logging phantom lines, cross-source duplicate probing, reaper-refresh herd, and eviction-tie-break gaps. Needs a fleet deploy.

**Read-only grading for paid/file-list proxies** (#344): Proxies from `--proxy_file` / the internal config bypass the URL admission gate by construction and were previously invisible to the quality system. A background sweep now grades every non-URL proxy the box serves with the same stage-1 table probe on the same 1-3h stale cadence, persisting Score/Graded/Failed/LastGraded into `proxy.state` (omitempty — same field shape as the URL store). Read-only by construction: only the grade fields are written; admission, eviction, give-up, and cleanup never read them, so a graded F keeps serving exactly as it did before. `proxy_probe.json enabled=false` skips the sweep entirely. Needs a fleet deploy.

### Changed

**Provider-aware `urnet-tools` rewritten in Go** (#345): The fleet ops tool is now a single Go codebase (`urnet-tools` for process/systemd, `urnet-docker` for containers) instead of the ~4000-line POSIX shell script plus a separate PowerShell variant. Discovers actual running providers (process scan + systemd units), identifies each by its JWT network identity, refuses ambiguous targets with an inventory table, and gates destructive ops behind a confirm prompt (`-f` for machines, never for provider selection). All 25 legacy subcommands dispatch with verified parity. Update is interactive-first with mandatory SHA-256 digest verification; the staging dir is a private per-update `MkdirTemp`; the hub install sanity-checks ELF magic instead of executing the downloaded binary; `optimize` is platform-aware (Linux ephemeral-port pool + TIME_WAIT sysctls, Windows netsh/reg equivalents). **Breaking for multi-provider boxes**: the tool now refuses to guess — scripts must specify `--unit`/`--user`/`--network`/`--network-id`/`--state-dir` when the box is ambiguous. Needs a fleet deploy.

## [v3.23.0-fix.26.8] — 2026-08-09

### Added

**Stage-1 table-probe quality gate for URL-source proxy admission** (#342): A URL-source proxy only had to survive stage-0 (a SOCKS5 greeting plus an API `CONNECT`) to be admitted, which proves the proxy is alive but not that it can actually reach the destinations providers need to reach. Every surviving proxy is now dialed at `:443` against a sampled block of the backend's ~127 health hosts (default 12 per pass, 4s per-target timeout), and only proxies scoring `>= pass_bar` (default 0.6, with a 0.9 "preferred" tier) go on to the auth queue. Design follows upstream `ip_remote_multi_client_probe.go`: positive evidence only (a SynAck proves the proxy's own upstream dial; silence never convicts), resolution outside the probed channel (the box's own DNS, so a proxy with broken DNS never fails a TCP probe), deterministic disjoint-block rotation, and a viability abort so an aborted pass is always a decided verdict. A grade persists only for DECIDABLE passes (quorum of the sample answered, context not cancelled) — an empty, cancelled, or resolver-gutted pass leaves the prior grade untouched, so the box's own DNS can never convict a proxy. RFC 1929 auth (`host:port:user:pass`) now carries through both probe stages so credentialed, usually paid, URL entries are graded on the same footing as free ones. Proxies graded below the bar never spawn, and the auth gate reports them distinctly from proxies that are simply dead. A kill switch (`~/.urnetwork/proxy_probe.json` with `{"enabled": false}`) restores pre-feature behavior end-to-end; `sample_width`/`timeout_ms`/`pass_bar`/`preferred_bar`/`enabled` are all runtime-tunable. Score/Graded/Failed now persist in `proxy_url.json` cache entries, matching the backend's own data model. Two rounds of external review plus an independent review pass closed a kill-switch accounting bug, an inert regression test (now mutation-verified), a cache-growth bound, and credentialed-proxy admission handling. Needs a fleet deploy — this changes provider binary behavior and introduces a new operator-facing config file.

### Fixed

**TTL fix for the inert reorder dialer** (#340): The reorder technique alternates a connection's socket TTL between 0 and its native value to force retransmits and out-of-order arrival at the TLS ClientHello. `IP_TTL` only accepts 1-255; Linux rejects 0 with `EINVAL`, and `SetSocketTtl` discarded that error, so on Linux the TTL never moved and the reorder/fragment+reorder dialers delivered plain fragmentation while reporting themselves as reorder dialers. Every fleet node runs Linux, so the technique has been inert in production for the fork's entire life. The low TTL is now set to 1 (in range, still dropped at the first L3 hop, the loss the technique needs), and `SetSocketTtl`'s error is now returned so a future out-of-range value fails loudly instead of silently. Two previously-vacuous test assertions are now real; 8 tests ported from upstream; 34 resilient tests pass, race clean. Activates a mechanism that has cost nothing on Linux for the fork's whole life and now costs something real — deliberately dropped segments in the ClientHello flight, quantified upstream at 12 dropped blocks of 24 on the reorder-only path. Needs a fleet deploy; worth staging on one node before rolling fleet-wide.

### CI

**Fixed truncated AI summaries, bound model calls, made the portability verdict fork-aware** (#341): Tier-1 summaries were piped through `tail -1`, keeping only the final chunk of the model's streamed JSON output — measured at 6.6% retention (143 of 2153 characters) on a representative run, so every summary that shipped was cut down to whatever fragment happened to land in the last chunk. The accept guard that was supposed to catch bad summaries couldn't distinguish a real summary from a fragment; it now requires at least 400 characters and a parseable verdict tag, applied at 12 sites. A wedged model call could hang the job for up to 6 hours with nothing timing it out; all four call sites are now wrapped in `timeout 120`. The portability rubric was a closed allowlist with no way to say "the fork shares this code"; `[MUST PORT]` now has to lead with a shared-code citation, and `[NO ACTION]` has to give a positive reason instead of defaulting to it. The truncation, guard, and timeout fixes are deterministic and verifiably correct; the rubric fix is a prompt change, measured at improving one diff's verdict accuracy from 2-of-4 to 5-of-5 correct on a single test case, and is deliberately biased toward false positives (flagging something as portable when it isn't) rather than false negatives. No deploy implications — this only touches the CI monitoring pipeline, not the provider binary.

## [v3.23.0-fix.26.7] — 2026-08-06

### Fixed

**Resilient TLS leaked fds, stranded the socket TTL, and could corrupt a stream** (#339): Both TCP reorder branches in `net_resilient.go` acquired a descriptor via `tcpConn.File()` and never closed it, so every failed write leaked one. The socket TTL those branches lower to force retransmits was restored only on the success path, leaving a failed connection at the lowered value for the rest of its life. A mid-fragment write failure returned `0, err` while the buffer still held the full record and earlier fragments were already on the wire, so a resend re-entered `Write`, re-fragmented from the start, and duplicated bytes into the TLS stream; `Off()` separately discarded any partially buffered record instead of draining it. The TTL is now read and restored through `SyscallConn` rather than a dup'd descriptor (no fd to leak at all), each restore is scoped to the branch that lowered it, an indeterminate write drops the buffer and disables the layer so a retry goes direct, `Off()` drains first, and the extender connection is closed on failures after the mode switch. Carries upstream `connect`'s fail-closed resilient TLS implementation into the fork. 1,138 lines of new tests including an fd-growth assertion over repeated failed writes.

**A canceled dial is no longer counted as a backend failure** (#332): Both auth sites counted every `connect()` error as a backend failure, including context cancellation from this process's own teardown. Closing a multi-client window cancels many transports mid-dial at once, and that burst tripped the degraded threshold with fresh timestamps, so the next session started already gated and skipped its first `CreateContract` per sequence. This fork keeps the window-expansion gate upstream removed, so a teardown-manufactured outage also blocked the next window from expanding. `RecordProxyAuthFailure` sat on the same error, marking healthy proxies as auth-failed and feeding them to the dead-proxy reaper. Both now sit behind a `ctx.Err()` carve-out. The open-coded bookkeeping is replaced with `noteBackendFailure`/`noteBackendSuccess` under a mutex, which also fixes two latent defects: the timestamp and counter were separate stores, so a concurrent success landing between them left a positive count with a zero timestamp that reads as healthy; and an aged-out failure streak was extended rather than discarded, so one new failure could read as a full outage on the strength of two events an hour old. Ports the review fixes from urnetwork/connect#192.

**Contract metrics: unbounded retention, corrupt windows, and a 160-minute "24h"** (#328): `provider/contract_metrics.go` had no test coverage and three compounding defects. Every proxy index kept a ~15.4 KB bucket ring forever with nothing to free it, and indices are monotonic per unique address, so churn-heavy boxes grew without bound. The ring advanced one slot per epoch change regardless of gap size, so an idle proxy's stale counts read as recent. And the `24h:` status row covered 960 × 10s, clamped, which is 160 minutes. Now: `retire()` on proxy teardown nils the rings while keeping the registry entry and lifetime atomics (deleting would zero a returning proxy's totals and make a proven-good proxy a reaper candidate) and invokes the previously discarded `AddContractStatusCallback` unsubscribe; buckets carry their own epoch so `window()` stops at the first out-of-range bucket; and two rings (fine 10s × 420, coarse 10min × 144) make the 24h row truthful while cutting per-live-proxy footprint from 15.4 KB to 6.7 KB.

**Hub rollup test flake** (direct to main): the hourly-rollup tests used timestamps that could straddle an epoch-hour boundary and failed at the top of the hour. Pinned mid-hour.

**Root package dependency surface reduced** (#315): `sn_deps.go` deleted. The file was four blank imports of `urfoundation/sn` packages whose own comment said to remove them once real callers existed — `provider/sn.go` now imports all four directly. While it lived, it forced the go-ethereum/cloud-SDK tree into the root `connect` library package: every consumer (extender, connectctl, bindings) compiled and linked the entire Ethereum + AWS/Azure + ZK toolchain. After deletion the root package drops from 471 to 370 transitive dependencies, and `go mod why go-ethereum` routes via `provider` instead of `connect`; library consumers no longer pay for what only the provider binary uses.

**`connecting` guard missing from two of three degraded predicates** (#316): `IsDegraded()` had the guard, `DegradedProxies()` and `ProxyHealthByAddress()` did not. `RegisterProxy` reuses the existing health struct on respawn, setting `connecting = true` without resetting the predecessor's `everUp`/`downSince`, so a proxy that was simply reconnecting read as a degraded tier — and the URL-source reaper's `isLive` check could demote or evict it mid-respawn. All three predicates now agree on the guard, and the dead `total`/`bwCount` counters in `ProxyHealthSnapshot()` were removed.

**Stale `connecting` state now bounded at one pulse cycle** (#320, #322): `connecting` is cleared only by `markProxyUp`/`markProxyDown`, neither of which fires on a failed dial, so a proxy that respawned and never reconnected reported `"connecting"` indefinitely. `RegisterProxy` now stamps `connectingSince`, and past the bound a previously connected proxy falls back to the degraded tier (a never-connected one to `dead`), so a hung respawn reads differently from a fresh one. The bound is 65 minutes, one hourly retry pulse plus margin, explicitly coupled to the provider's `deadConfirmDelay` and the ~1h staging window the docs promise operators — a never-connected proxy only reads as `dead` after a full pulse cycle, not during normal deployment ramp. The heartbeat report and snapshot now also evaluate the fresh `connecting` state before the inherited `everUp` (as `ProxyHealthByAddress()` already did), so a previously connected proxy mid-respawn no longer counts as degraded or dead while its new connection attempt is running. This also switched on a latently dead path: `NewlyDead` previously could never fire for never-up proxies (its `!connecting` clause was never false), and now logs one `DEAD` row to `proxy_health.log` per proxy that genuinely never connected within a pulse cycle; nothing alerts on that row.

**Status server timeouts** (#317): The provider's status HTTP server had no timeouts; a dribbled-header client could hold connections open indefinitely on the exposed port. `ReadHeaderTimeout: 10s` and `IdleTimeout: 120s` now match the hub's configuration, with `WriteTimeout` deliberately unset so the SSE stream is not killed.

**`hub-join` client hardening** (#317): Both PAKE join round-trips used `http.DefaultClient` — no timeout, no context — so a blackholed hub wedged the CLI forever with no output. They now share a 30s-timeout client with contexts, so Ctrl-C aborts a wedged join and a hung hub fails cleanly; the discarded KE2 response decode error is now checked and reported. A same-night hotfix (#321) repaired the test call sites after the PAKE signature churn.

**`go vet ./...` fully clean** (#318): 12 unreachable `return` statements after `panic(err)` removed from `connectctl/main.go` (connectctl is a developer CLI where panicking is intended; the panic calls themselves are unchanged). Vet now reports zero findings repo-wide, the first time in the fork's history, so genuinely new findings can no longer hide in the noise.

### Performance

**Hot-path verbose logging no longer allocates** (#329): The provider ships with `-v=0`, but an unguarded `V(n).Infof()` still constructs and heap-boxes its arguments on every call. Measured against the glog-backed logger at a disabled level, the per-inbound-message site in `Client.run` cost 128 B and 5 allocations per message; the per-packet sites in `ip.go` cost 32 B and 1 allocation. A central fix in `log.go` is impossible, since arguments escape at the call site and `Verbose` is an interface, so the only remedy is per-call-site guards. 35 Tier-1 sites now carry inline `if v := log.V(n); v.Enabled()` blocks (17 in `ip.go`, 16 in `transfer.go`, 2 in `ip_remote_multi_client.go`) and benchmark at 0 B, 0 allocations. Tier-2 per-connection sites were deliberately left alone. No log level, message text, or format string changed; output is byte-identical at any verbosity.

**Capped multiplicative resend backoff** (#331): The fork's resend path backed off only while `isBackendDegraded()` and shifted uncapped as `ScaledRtt() << min(sendCount, 6)`, reaching 64s at `sendCount=6` with a 1s RTT despite a defined-but-ignored `MaxResendInterval` of 8s. Adopts upstream's formula, which backs off on every repeated resend rather than only during outages, caps at `MaxResendInterval`, and offsets from the first resend so the first transmission stays at plain `ScaledRtt()`. The unconditional form matters when acks are delayed by queueing rather than lost: a flat timeout re-sends the whole in-flight window each interval, and the duplicates feed the congestion that delayed the acks. Extracted into a pure `resendBackoff()` helper so the formula is unit-testable; `isBackendDegraded()` and its other gating points are untouched. This is a backport, not a port; the fork's version would be a regression upstream.

### Documentation

**Contract docstrings across the transfer stack** (#333, #334, #335, #336, #338): Roughly 400 lines documenting invariants that were previously only discoverable by reading the implementation. Covers the transfer data plane (client, send sequence, receive sequence), the contract manager lifecycle, queue and provide-key contracts, the route manager, multi-route selector and transport weights, the RTT window, stream manager and control sync contracts, and the net/io message pool, dialing and framing contracts. No behavior change. The review of #333 surfaced the three `net_resilient.go` defects fixed in #339.

**`LOG_REFERENCE` refresh** (#327): Documents the pulse `connecting` field, hub bootstrap lines, and `proxy_health.log` rows.

### CI

**PR auto-labeling** (#326, #337): Every PR is labeled along two axes, area from changed paths via `actions/labeler`, and type from the conventional-commit prefix already used in PR titles, plus `upstream-port` when the title mentions upstream. Porting from `urnetwork/connect` is a recurring category in this fork with no way to filter for it before now. `needs-fleet-deploy` and `security` remain manual because they are judgement calls not derivable from a diff. #337 orders the type labeler after the area labeler to close a race where the two could overwrite each other. The workflow triggers on `pull_request` rather than `pull_request_target`, so the write-scoped token is never exposed to an untrusted head ref, and the PR title reaches the script through `env:` rather than `${{ }}` interpolation, so an attacker-controlled title cannot be spliced into the shell.

**gofmt sweep and gate** (#324): 29 drifted files formatted, pure alignment across var blocks, struct literals and spacing, 92 insertions and 94 deletions with no semantic change. A new `Check gofmt` step in `test-and-lint` fails the build if `gofmt -l .` prints anything, so formatting cannot silently drift back.

**Verbose-log gate blind spots closed** (#330): The gate protecting #329 had two holes. A format string shared between a guarded site and an unguarded allowlisted site meant un-guarding the guarded one would pass CI silently; the gate now enforces disjointness between the allowlist and the guarded sites, making the whole class self-policing. And the extractor's `(?m)^[^/]*` matched across newlines, mis-attributing lines and leaving 15 sanctioned formats unchecked entirely, so the allowlist held 41 entries against a true 55. Both fixed, with the gate watched in the passing state and both failing states.
## [v3.23.0-fix.26.6] — 2026-08-03

### Fixed

**Fragmented IPv4 packets dropped in `parseIpv4`** (#311): A non-first fragment has no transport header and a first fragment has a truncated payload; both were previously misparsed as if a transport header were present, reading garbage bytes as ports/flags. Any packet with the MF bit set or a non-zero fragment offset is now rejected before transport parsing. DF-only and unfragmented packets are unaffected. Ported from upstream `e05ecee0`.

**SCTP/WebRTC reliability tuning** (#312): `ReceiveMtu` corrected from 4 KiB to 1500 (it's a per-packet demux buffer, not the SCTP receive window); `SctpCwndCAStep` set to 4 MTUs for faster recovery from independent loss on higher-latency paths; new lazy SCTP progress watchdog tears down blackholed associations where ICE consent stays healthy but the data plane is dead. Ported from upstream `aee94774` (settings only). No pion dependency bump needed (`SetSCTPCwndCAStep` verified at vendored v4.2.15).

**`SecurityPolicyStatsCollector` destination cardinality bound** (#314): `resultDestinationCounts` was unbounded — every unique (protocol, ip, port) tuple got a permanent map entry on long-running providers. Now capped at 1024 destinations per result with the final slot as an overflow bucket ("other destinations"); unrecognized result values share one unknown-result bucket; zero-count updates ignored. Ported from upstream `45357960`.

### Added

**`sender_generation_id` proto field** (#313): Added to `ExchangeSignals` to disambiguate a delayed initial `WaitingForSdpOffer` from a newly restarted passive association. Inert until the peer-connection generation-reset logic lands; additive and wire-compatible with peers that don't set it. Ported from upstream `45357960` (field only).

**Proto source drift repaired** (#313): Commit `ccada52` (Jul 11) regenerated `transfer.pb.go`/`frame.pb.go` from a different upstream `.proto` without landing matching source changes; a `make build` regen would have silently deleted `NetworkPeer`, `NetworkPeersReset`, `NetworkPeersUpdate`, and the `TransferNetworkPeers*` enum values (all live, used by `transfer_peer_manager.go`). Definitions reconstructed from the shipping generated code and restored to the `.proto` sources; `protocol/` `make build` is safe to run again.

### Out of scope (deferred)

- ICMP echo path (upstream `e05ecee0`): client-facing, provider relays TCP via SOCKS.
- Android/cellular P2P (`759a5a7d`, `aee94774` Android bits): mobile-gated; fork lacks `ProviderStreamPolicy`/`DegradedMode` prerequisites.
- Lifecycle/budget overhaul (`45357960` remainder, `fe8dee32` full perf split): needs a dedicated diff session against the fork's divergent `message_pool.go`/`memory_budget.go`.

## [v3.23.0-fix.26.5] — 2026-08-02

### Added

**`urnet-tools idle-update`** (#301): Waits for billable traffic to drop below a threshold (default 5 KiB/s, `--threshold`) and stay there for a sustained window (default 5 min, `--window`) — verified with a second, tighter polling pass — before applying a pending provider update, instead of updating immediately and cutting off active sessions. `--window 0` updates immediately. Fails closed if `billable_rate` isn't available yet, and rejects `--threshold`/`--window`/rate values outside Bash's signed-int64 range.

**Cloudflare Worker sources versioned in-repo** (#304): `dl`, `dl-fullbars`, `geo`, and `provider-redirect` — previously dashboard-only or partially untracked — now live under `workers/` with their `wrangler.jsonc` configs and a `workers/README.md`. Deploys remain manual (`wrangler deploy`); nothing is wired into CI.

**`URNETWORK_HUB_TRUSTED_PROXIES`** (#302): New hub env var. `X-Forwarded-For`/`X-Real-IP` are now only honored from configured trusted proxy IPs/CIDRs — previously trusted unconditionally from any client, letting an attacker spoof their apparent IP and evade PAKE join rate-limiting entirely. Without this env var set, a hub behind a reverse proxy now sees the proxy's own IP for every request (collapsing rate-limiting into one shared bucket) instead of trusting an unverified header — set it to the proxy's address to restore correct per-client behavior.

**Upstream a2b144c port** (#298): `WindowType`/`OverrideAllowDirect`/PQE/peer-identity support ported from upstream `urnetwork/connect`.

**Tiered setup guides** (#297): Beginner/intermediate/advanced installation documentation.

### Fixed

**`proxy.state` ghost-entry accumulation** (#305): `ProxyReloader.reload()` in `proxy_reload.go` only pruned `proxy.state` for addresses that were both currently running and no longer desired. A dead/offline proxy's goroutine has usually already exited by the time it's removed (e.g. via `proxy remove-dead`), so it was never in the "running" set and its state entry was never deleted — accumulating forever and getting re-reported as removable on every subsequent `remove-dead` run. Confirmed on a production node: 722 proxies correctly removed from config/live fleet, but `proxy.state` retained 857 entries, 711 of which existed nowhere else. `reload()` now reconciles `proxy.state` against the full desired set (config/file + URL cache) instead. Guards against a related edge case: if `proxy_url.json` fails to read mid-cycle, the prune pass is skipped rather than deleting state for still-desired URL proxies over a transient error.

**Pooled-buffer leaks on backpressure/error** (#303): `tcpSend` could double-return a pooled packet on certain nil-sequence paths in `ip.go`. UDP backpressure drops and RST-buffer sharing are now leak-free under load; GCM seal count is capped per key.

**Node-bound hub credentials** (#302): v2 node credentials are now bound to their owning node — a valid credential for one node can no longer act on another node's data via `/api/report`, `/api/heartbeat`, or `/api/nodes/remove`. `onboard.sh`'s Host header is validated before being interpolated into the generated install script.

**Docker in-place update process handling** (#299): The update path now kills the correct process and respawns it properly.

**Cloudflare Worker hardening** (#304): `dl-fullbars`'s GitHub proxy now allowlists outbound headers instead of forwarding everything from the client; the `curl | sh` install dispatcher fails the install if the script download fails instead of silently running an empty script; `geo` worker's RTT fields use explicit null checks and the response is marked `Cache-Control: no-store`.

**Ramlog redirect scoped to `provide`/`auth-provide`** (#306): `URNETWORK_RAMLOGS=1` redirected stdout/stderr into `/dev/shm/urnetwork.log` for every invocation of the binary, not just the long-running `provide` process — one-shot CLI subcommands run via `docker exec` (`proxy remove-dead --preview`, `proxy summary`, etc.) produced no visible output at all. Found while validating the `proxy.state` fix on a live test container. Now scoped to `provide`/`auth-provide` only.

**Hourly reload reconciler** (#309): `reload()` in `provider/proxy_reload.go` only ran on an explicit trigger (add-source, remove-dead, proxy refresh, URL fetch merge, reaper change). A mass-failure event could leave still-desired proxies stuck out of the running set with no future trigger to bring them back. Confirmed on a production node via `proxy_health.log`: a mass degrade event mostly self-recovered within an hour, but a persistent subset (~3300 proxies) sat idle for ~22 hours until an unrelated `add-source` call forced a reload. New `runReloadReconciler` fires a reload trigger every hour unconditionally as a safety net — cheap no-op when nothing is actually wrong, not gated behind `URNETWORK_SELF_HEAL`.

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

## [v3.23.0-fix.25.14] — 2026-07-08

### Added

**Disk-based critical event log** (#242): `~/.urnetwork/events.log` records `STARTUP` (version, PID, hostname), `SIGNAL`, and fatal exits, capped at 1 MB with rotation. It exists because the provider periodically exited 0 on some nodes with no log at all: RAMLOG lives in `/dev/shm` and `initSHMLogger` opens it `O_TRUNC`, so a restart wiped the only evidence. The event log survives the restart.

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

## [v3.23.0-fix.25.5] — 2026-07-08

### Fixed

**The `proxy_fleet_hourly` backfill migration raced the first report** (#239): the backfill now runs only when the source table actually has history, instead of rebuilding from an empty source.

### Changed

**Hub commands moved into their own help category** (#238): `urnet-tools` help lists the hub commands as a group rather than mixing them with the provider's.

---

## [v3.23.0-fix.25.4] — 2026-07-08

### Added

**Best Proxies hall-of-fame leaderboard** (#235): a ranked board over the fleet's proxy history with a win percentage, so a consistently good proxy can be told from a lucky one.

**Per-proxy client JWT persisted across restarts** (#233), gated behind an opt-in flag that is off by default (#237): a restart reuses the same client token instead of minting a new one, so the server-side reputation attached to that client is not thrown away. The log distinguishes a reused token from a fresh mint.

**Tactical log visibility** (#226): emoji and layout changes on the lines an operator actually watches.

### Fixed

**Best Proxies navigation and scaling** (#236): a duplicate nav item and a `win_pct` scale bug.

**Profit-heartbeat metrics were wrong** (#228): corrected against #226's definitions.

**URL proxies were probed inside the file lock**: the reaper held `proxy.lock` across the probe, so every other proxy operation waited on a network round trip. Probing now happens outside the lock.

**Heartbeat deltas, body cap and drain re-trigger**: the delta baseline was recomputed on every pass, the response body had no bound, and a drain did not re-trigger when it should have. The delta is also chunked now.

**TLS and stream correctness**: the `tlsSrv` pointer race, client reuse, `DNSName` verification, a non-atomic flag, an `io.Reader` contract violation, and snapshot copies that shared their backing array.

**SSE responses were gzipped**: server-sent events now bypass compression, and signal shutdown is wired to the servers.

**JWT refresh rewritten onto `code-create`/`code-login`** (#227): `AuthNetworkClient` was not the right endpoint for a refresh. The verification client also had no timeout, and now has 30 seconds.

**Checkpointed contracts were counted in `contractUtilSum`**, inflating utilization.

**Upstream correctness ports** (#231, #234): `MessagePoolReadAllWithTag`'s defensive guards, and the partial read/write fixes from upstream `7f0835a`.

**Cleanups** (#229): JWT expiry formatting, a dead throttle removed, the peak rate frozen rather than recomputed, and a WebRTC rate limit.

### Changed

**Hub dashboard**: reset zoom, request sequencing so a slow response cannot overwrite a newer one, and the fleet-hourly query made cheaper.

---

## [v3.23.0-fix.25.3] — 2026-07-06

### Added

**A password-derived hub CA** (#216): the hub's random self-signed certificate is replaced by an Ed25519 CA derived from the operator's password with Argon2id. The same password and salt produce a byte-identical certificate, so providers do not have to re-pin after a hub restart.

**The provider prefers CA verification and fails closed** (#217): when no trust material exists — no `hub_ca.pem` and no `hub.pin` — the provider now refuses the connection with a clear error instead of accepting any certificate.

**Token-based onboarding** (#218): the operator mints a 15-minute token and pastes a one-liner into each provider. The script is self-contained and works the same for native and Docker providers.

**An operator CLI for the CA trust system** (direct to main): `urnet-tools hub link` writes the CA material, and the legacy fingerprint fallback is kept for a hub that has not been migrated (#222). The manual CA certificate path is documented for operators who cannot run the one-liner (#223).

### Fixed

**Pre-ship audit, P0 and P1** (#224): CA material could be lost on a failed write, `--token` was not forwarded, the password was not validated, the systemd path had no guard, IPv6 addresses were not bracketed, and `ca.crt` rotation rewrote more than it should.

**Pre-ship audit, remaining P2 items** (#224): thirty-one further edge cases across error handling, resilience and UX, plus token URL-encoding and JSON parsing fallbacks.

**`onboard.sh` corruption and a port-443 edge case**: both fixed, with test coverage.

**The proxy reload trigger fired in rapid-fire loops** (#221): the trigger is debounced, so one configuration change no longer starts a reload storm.

**Shutdown logged no signal name** (#215): the signal that triggered `Event.SetOnSignals` is named in the log.

**The PowerShell hub link ran in the wrong place** (#222): `hub link` and `unlink` route through `docker exec` when the provider is containerized, detect Docker versus native correctly, match the container name exactly, and warn before an operation would contaminate the host.

### Changed

**Release notes for this release were finalized** (#223): migration order and the legacy `hub.pin` caveat are documented, including the deprecation tlog for a legacy pin.

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

## [v3.23.0-fix.24.24] — 2026-06-29

### Fixed
- **Error propagation for reload/state writes** (PR #163): `writeReloadTrigger()` and `writeProxyState()` now log warnings on failure instead of silently discarding errors. Prevents hot-reloads from silently breaking and stale state files when disk writes fail.
- **DoH certificate pinning** (PR #164): DNS-over-HTTPS resolver now uses `DefaultTlsConfig()` with ISRG Root X1/X2 pinning instead of insecure `// FIXME` nil TLS config. Prevents potential MITM of DoH responses.
- **Reload watcher and proxy probe error handling** (PR #165): Split error check from sequence comparison in `readReloadSeq` to prevent transient FS read failures from spuriously triggering proxy reloads. Added proper timeout handling for `SetDeadline` errors in probe stages.
- **Removed dead config fields** (PR #166): Eliminated `LegacyCreateContract` and `TrackUsedContracts` fields from `ContractManagerSettings` that were always `false` and had no active code paths.

---

## [v3.23.0-fix.24.23] — 2026-06-28

### Fixed
- **`urnet-tools` no-args shows help instead of triggering install** (PR #161): Running the script with no arguments was triggering a full install instead of showing the help menu. Fixed via `$0` path check instead of the broken `URNETWORK_TOOLS_MODE` env var injection approach.

### Added
- **Build release monitor** (PR #162): New GitHub Actions job monitors `urnetwork/build` repository for release tags, posts commit summaries with file changes to Discord. Expands critical files list and shows actual filenames in notifications.

---

## [v3.23.0-fix.24.22] — 2026-06-28

### Added
- **IP Security DPI Refactor** (PR #160): Replaced the monolithic `ip_security.go` (~66K lines) with a layered deep-packet-inspection pipeline. Payload-level BitTorrent signature detection (BEP 3/5/15/29, entropy-based encrypted-flow heuristic) instead of port-only heuristics. 214 IPv6 prefix ranges now blocked (previously unchecked). 66K-line `map[[4]byte]bool` blocklist replaced by ~8K-line packed binary-search for zero-allocation lookups. Known-safe protocols (NTP, IKE, DNS/UDP) skip DPI entirely via three-way verdict (drop/allow/pass-to-DPI).

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

## [v3.23.0-fix.24.20] — 2026-06-27

### Added
- **Hub UI overhaul** (direct to main): summary cards, a fleet chart, history charts, a filter, and a per-node drawer.
- **Hub server-side performance** (direct to main): gzip, request rate limiting, stale-node eviction, and a `nodeSummary` API so the dashboard stops fetching every node to draw a summary.
- **Idempotent `override.conf` helpers** (PR #153): six inline `sed`/`printf` patterns across the toggle functions became two shared helpers. Running a toggle twice can no longer produce duplicate `Environment=` lines or a duplicate `[Service]` section.
- **`urnet-tools report`** (PR #154): prints, sets or clears `REPORT_URL` through those helpers, so the hub report URL is no longer edited by hand.

### Fixed
- **Bug fixes for the proxy summary and reachability-probe work** (PR #151, #152, #153): the reported failures in those three changes.
- **PowerShell log switch syntax and Docker exec sanitization** (direct to main): `') {'` became `' {'`, and the values passed into `docker exec` are sanitized.
- **`REPORT_URL` extraction and help text** (direct to main): the `sed` pattern matched the wrong form, and the help text described the old behavior.

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

## [v3.23.0-fix.24.18] — 2026-06-26

### Added
- **Multi-tier degraded proxy removal** (PR #145): `proxy remove-dead` now supports `--degraded[=<duration>]` to remove degraded proxies offline past a threshold, `--source=<url|file|internal>` to filter by source, `--yes` for non-interactive use, and `--preview` for dry-run. Previously only `dead` (never authed) and `inactive` (7+ days) were removable — the `degraded` category (authed once, now offline) was a blind spot that let zombie proxies clog the pool indefinitely.
- **Proxy activity dashboard** (PR #145): `proxy activity` opens a real-time terminal dashboard showing active proxies carrying client traffic, per-proxy bandwidth rates, client counts, and recent contract acquisition events. Auto-refreshes every second. Press `q` or Ctrl+C to exit.

### Tests added (pending next release)
- **HMAC dual-format verification test** — regression test confirming `ContractManager.Verify()` accepts both legacy (pre-July 1) and standard (post-July 1) HMAC formats, ensuring the platform cutover doesn't break provider contract verification.
- **Startup sort order test** — verifies file/internal proxies sort before URL-sourced ones.
- **backoffPacer edge case tests** — zero stagger returns immediately, context cancellation aborts non-zero stagger waits.
- **Warmup gate lifecycle test** — URL proxies deferred during warmup are launched once warmup completes.

## [v3.23.0-fix.24.17] — 2026-06-25

### Added
- **Emoji prefixes on the key log lines** (direct to main): the lines an operator scans for — ready, contract, proxy auth, traffic — are prefixed so they can be found in a busy log.

### Fixed
- **The reload tests hung on the warmup gate** (direct to main): the tests now set `proxyWarmupDone`, which the gate requires, instead of waiting for a warmup that never completes in a test.

---

## [v3.23.0-fix.24.16] — 2026-06-25

### Fixed
- **`proxy clear` now wipes URL cache, source URLs, and systemd PROXY_URL** (PR #139, #140): `proxy remove --all` was clearing the internal config and `proxy.state`, but leaving `proxy_url.json` and systemd `Environment=PROXY_URL` untouched. The cached URL proxies and env var would re-populate sources on restart — the clear had no lasting effect. Now `proxy_url.json` is wiped entirely (cache, blacklist, source URLs) and `PROXY_URL` is stripped from any systemd override drop-in in `~/.config/systemd/user/urnetwork.service.d/`, with a systemd daemon-reload.
- **File-sourced proxies launch before URL-sourced ones** (PR #139): The proxy launch order at startup was non-deterministic (Go random map iteration), so file-based and URL-sourced proxies were interleaved — no priority for paid proxies. The proxy list is now sorted so file-sourced proxies get lower indices in the launch sequence, giving them a head start via `backoffPacer` before any URL-sourced proxies begin connecting.
- **URL fetcher waits for file-proxy warmup before first fetch** (PR #140): The URL fetcher was triggering 5-15 minutes after startup, before file proxy warmup completed (~17 min for 1000 proxies at 1s stagger). URL-sourced proxies started authing mid-warmup, competing with file proxies for auth rate-limiter slots. The URL fetcher now waits for warmup to reach `>90% up with <5 connecting`, with a 60-minute timeout fallback so a few slow file proxies don't block URL proxies forever.

## [v3.23.0-fix.24.15] — 2026-06-25

### Fixed
- **`ulid.Make()` allocated on the per-packet hot path** (PR #137): every sequence, message and transport id went through the external library, which heap-allocates on each call. It is replaced by an inline monotonic generator that writes into a stack-allocated `[16]byte`, ported from upstream `d474f36`.
- **Frame marshal allocated per frame** (PR #138): `frame_protobuf.go` is ported so the send and receive hot path marshals without allocating.

---

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

## [v3.23.0-fix.24.9] — 2026-06-24

### Added
- **`earning=yes|no` column on the hub dashboard** (direct to main): the proxy table says whether a proxy has earned, rather than leaving it to be inferred from a billable figure.
- **`urnet-tools update -f` stops, updates and restarts non-interactively** (PR #125): the unattended path no longer needs a second command to bring the provider back.
- **`[profit]` heartbeat and `[contract]` close utilization logging** (PR #126): a periodic profit line and a utilization figure on each contract close.

### Changed
- **HMAC migration deadline disclaimer** (direct to main): the releases that skipped the migration now carry it.

---

## [v3.23.0-fix.24.8] — 2026-06-23

### Added
- **Per-minute earning windows** (direct to main): `billable_1m`, `5m`, `15m` and `60m` are tracked separately, so a rate can be read without differencing two cumulative totals.
- **Hub SQLite historical storage** (PR #123): the flat `hub.json`, rewritten in full on every report from every node, became a WAL-mode SQLite database. Each node's proxy list is stored per report as a compressed blob with seven-day retention.
- **A five-minute report interval and timestamped status lines** (PR #123).

### Fixed
- **`paceMonitor` outlived warmup** (PR #122): the goroutine kept running after the warmup it was pacing had completed.

---

## [v3.23.0-fix.24.7] — 2026-06-23

### Fixed
- **Operator-curated proxies never give up on auth** (PR #119): File, internal, and direct proxies that exhaust `maxAuthFailures` now switch to a slow capped retry (5m → 10m → 15m with jitter) instead of going permanently offline. URL-sourced proxies retain the short leash — they give up and the `time.AfterFunc` requeue brings them back after 15 minutes.
- **URL requeue decoupled from reload engine** (PR #120): Replaced the per-proxy `scheduleGiveUpRequeue` goroutine (15m sleep + cooldown map + trigger write + 30s recheck) with a single `time.AfterFunc` per proxy. Deleted the entire `proxyGiveUpCooldown` map/mutex/functions (~110 lines). `reload()` now acquires `proxy.lock` before proceeding, serializing hot-reloads with cross-process operations like `proxy remove-dead` and URL source fetches.

---

## [v3.23.0-fix.24.6] — 2026-06-23

### Fixed
- **`urnet-tools -v` reported the installer's version, not the binary's** (PR #117), and the dash-compatibility cleanup replaced `echo -e` with `printf` in the ramlogs override. The dash bashism lint now covers the deps and uninstall scripts too.
- **`[traffic]` lines were logged for every proxy** (PR #118): only proxies serving clients print a per-proxy traffic line, so an idle box no longer fills its buffer with zeroes.

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

## [v3.23.0-fix.24.4] — 2026-06-22

### Fixed
- **`proxy add-source` held the proxy lock across its SOCKS5 probe** (direct to main): the lock is released before the probe, so a slow or dead candidate cannot block every other proxy operation while it times out.

---

## [v3.23.0-fix.24.3] — 2026-06-22

### Fixed
- **The `[c]ping` suppression counters were per connection, so the rate limit did not hold** (direct to main): the counters moved to package level and are shared across connections. This is the same fix `v3.23.0-fix.24.2` shipped; the tag re-cuts it.

---

## [v3.23.0-fix.24.2] — 2026-06-22

### Added
- **`urnet-tools auth`** (direct to main): mint or check provider auth without going through the installer.

### Fixed
- **`[c]ping` logged on every ping** (direct to main): the line is rate-limited to one per five minutes with a suppressed count, and the suppression counters live at package level so all connections share them.
- **Documentation for the 24 defaults** (direct to main): the performance tuning guide, the Docker examples with volumes and capabilities, and a quick-install one-liner.

---

## [v3.23.0-fix.24.1] — 2026-06-21

### Fixed
- **MultiRaceClientCount set to 16 unconditionally**: Replaces the CPU-based tier system with a simple constant — 16 racers on all platforms. The race is double-bounded at runtime by actual healthy provider count and per-flow packet budgets, so the value acts only as a ceiling. Higher values on single-core nodes were previously held back by a conservative CPU heuristic that had no basis in the actual resource cost (goroutines are I/O-bound, not CPU-bound).

### CI
- **Docker build depends on test success**: `build-and-push` now `needs: test-and-lint`, saving ~3 min of Docker build compute on every failing PR.
- **Go module cache restored**: Re-added `~/go/pkg/mod` cache alongside existing build cache. Saves ~30s per run.
- **Test order optimized**: Go tests (with `-race`) now run before shell installer tests for faster failure feedback.

---

## [v3.23.0-fix.24] — 2026-06-21

### Changed
- **Default throughput ceilings raised** (PR #105): the transport buffer went from 1 to 16 messages, which removes the serialization between the protocol framer and the WebSocket writer, and the TCP and UDP maximum window went from 1 MiB to 4 MiB, which removes roughly a 160 Mbps per-connection ceiling at 50 ms RTT.
- **Client-side auth rate limiter relaxed** (PR #107): the limiter's starting rate is higher, so a large pool no longer spends its first minutes queueing.
- **`MultiRaceClientCount` scales with CPU count** (PR #108): 4 on one or two cores, 6 on three or four, 8 on five to eight, and 12 above that, instead of a hardcoded 2 that left 14 of 16 candidate providers idle during connection setup.
- **`ContractFillFraction` follows observed RTT** (PR #109) instead of a fixed fraction.
- **Packet dispatch is sharded across goroutines** (PR #110), so one busy proxy's traffic does not serialize behind another's.

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

## [v3.23.0-fix.22.2] — 2026-06-20

### Fixed
- **A dead proxy was counted as API overload** (PR #94): reachability is now established before an auth attempt and before a URL-sourced proxy is merged, so a proxy that cannot be dialed is classified as dead instead of feeding the auth rate limiter's overload signal.

---

## [v3.23.0-fix.22.1] — 2026-06-20

### Added
- **Hub report URL hot-reloads** (PR #93): `~/.urnetwork/report_url` is re-read, so the report target changes without a restart.
- **The outage alert webhook hot-reloads too** (PR #93): the same file change reaches the alert path.

---

## [v3.23.0-fix.22] — 2026-06-20

### Added
- **Proxy URL sources** (PR #90): one or more URLs become a live, additive proxy source on top of `--proxy_file` and the internal config. Lists are fetched on an interval, parsed with or without a `socks5://` prefix and with optional credentials, and merged into the hot-reload pipeline without disturbing proxies that are already running. `proxy add-source` and `proxy remove-source` persist a source at runtime, and source-scoped dead-proxy cleanup (`none`/`url`/`all`) lets a noisy public list prune itself without touching curated entries.
- **Docker CLI parity and a larger RAMLOG** (PR #86).
- **Proactive seven-day JWT refresh** (PR #89): the token is renewed weekly with a 48-hour expiry fallback, startup jitter desynchronizes the fleet, and the last refresh is persisted so a restart does not re-refresh.

### Fixed
- **quic-go memory exhaustion** (PR #85, #86): the dependency moved to v0.59.1 for the vulnerability fix.
- **Proxy state was not written at startup** (PR #87): `started_at` is now persisted, and a zero value is healed on the heartbeat.
- **`urnet-tools` relied on bashisms and non-POSIX JSON parsing** (PR #88): it now parses without `jq`-only constructs and runs under dash.
- **Auth timeouts and connection failures were not treated as overload** (PR #92): they are now classified as an overload signal, so the rate limiter backs off instead of retrying into a struggling API.

### Changed
- **Changelog, fork-changes and READMEs caught up for the release** (PR #91).

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

## [v3.23.0-fix.19.1] — 2026-06-09

### Added
- **A node remove endpoint and its dashboard button** (direct to main): a node that has left the fleet can be removed from the hub instead of sitting in the list forever.

### Fixed
- **Hub UI corrections** (direct to main): the dashboard's layout and refresh behavior.

### Changed
- **Release notes and a dashboard preview image** (direct to main): the notes were rewritten for this release and the preview added to the wiki and README.

---

## [v3.23.0-fix.19] — 2026-06-09

### Added
- **Bandwidth hub reporting** (PR #47): an opt-in reporter posts per-proxy metrics and system information to `URNETWORK_REPORT_URL` every 60 seconds, and a standalone hub server with no external dependencies stores them and serves `/api/nodes` and the dashboard. Nothing changes for a provider that does not set the URL.
- **A hub dashboard** (direct to main): fleet view, per-node proxy lists and history.
- **Proactive JWT renewal** (PR #49): the expiry is checked hourly and the token is refreshed inside 48 hours of `exp`, so a provider no longer exits 78 to force re-authentication. Live sessions keep working throughout.
- **Fatal errors reach the RAMLOG and every failure path has an exit code** (PR #48): `shmLogFatal` writes directly to the file, bypassing the pipe goroutine that could lose the message when the process died.
- **Per-proxy failure reason tracking** (direct to main): atomic counters per reason, so a proxy's failures can be told apart.
- **Proxy benchmarking** (direct to main): staggered TCP connect and SOCKS5 RTT probes.
- **Graceful draining for proxy hot-reload** (direct to main): a removed proxy finishes its live clients instead of cutting them.
- **Hub unit tests** (direct to main): 18 tests plus CI integration.

### Fixed
- **User session counting lived in the SOCKS5 dialer** (direct to main): it moved to the NAT buffer layer, where the sessions actually are.
- **Upstream proxy fixes ported** (direct to main): the out-of-band provide acknowledgement and the ping companion contract from upstream `proxy-fixes2`.

---

## [v3.23.0-fix.18.4] — unreleased

### Added
- **Proactive JWT Renewal**: The provider now checks the JWT expiry once per hour. When the token is within 48 hours of its `exp` claim, it proactively calls the auth API for a replacement and writes it to disk — no restart, no exit-78 blip. The check runs immediately at startup and every hour after. If the API is temporarily unreachable, it retries on the next cycle.
- **shmLogFatal**: All fatal error paths now write a `FATAL [exit <code>]: ...` line directly to the ramlog file before terminating (bypasses the pipe goroutine, so the message is never lost to a race on exit). Also writes to stderr for Docker logs. Works regardless of whether ramlogs are enabled.
- **Unique Exit Codes**: Every failure path now has a documented exit code so operators can triage from the exit code alone. See `FORK_CHANGES.md#exit-code-reference` for the full table.

### Fixed
- **Ramlog Race on Fatal Exit**: When the provider exited with code 78 (expired JWT), the error message was written to the stdout/stderr pipe but the process often died before the ramlog goroutine could flush it to `/dev/shm/urnetwork.log`. `shmLogFatal` writes directly to the file, sidestepping the race entirely.

---

## [v3.23.0-fix.18.3] — 2026-06-08

### Added
- **A CLI configuration change triggers a proxy hot-reload** (PR #44): adding or removing a proxy through the tool no longer needs a separate `proxy refresh`.
- **A native `[direct]` proxy connection in proxy mode** (PR #45): proxy mode can run a direct connection natively instead of requiring it to come from a list.

### Fixed
- **Proxy health log tests still asserted the old table widths** (direct to main): they now match the widened ASCII tables.

---

## [v3.23.0-fix.18.2] — 2026-06-08

### Fixed
- **One proxy's auth failure killed the whole provider** (PR #43): the failure is contained to that proxy.
- **The installer gave up on a transient download failure** (direct to main): downloads retry with fallbacks, and the installer tests parse URLs the way the script does.
- **Idle timeout lines flooded the log** (direct to main): demoted to `V(2)`.

### Changed
- **Proxy health and traffic tables widened** (direct to main): the columns no longer run together at fleet scale.

---

## [v3.23.0-fix.18.1] — 2026-06-07

### Added
- **`proxy-traffic` and `logs` aliases inside the container** (direct to main): `docker exec -it <name> logs` tails the RAMLOG, which fixes the empty `docker logs` output, and `proxy-traffic` gives a quick load check.

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

## [v3.23.0-fix.17-rc.1] — 2026-06-02

### Added
- **Per-proxy bandwidth tracking** (direct to main): cumulative billable and total bytes per proxy, surviving a RAMLOG rotation.
- **Active NAT session tracking** (direct to main): `clients=N` on the `[net][s]select` lines and in the traffic report, so sessions multiplexed through a proxy are counted.

### Fixed
- **Proxy ids sorted lexicographically** (direct to main): they sort numerically now, and the table column was widened for the result.

### Changed
- **CI detects a pre-release from the tag name** (direct to main) and supports a short image tag, which is how this candidate was published.

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

## [v3.23.0-fix.15.1] — 2026-05-29

### Changed
- **Optimized sysctls added to the Docker examples** (direct to main): documentation only.

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

## [v3.23.0-fix.14.3] — 2026-05-28

### Fixed
- **`optimize` failed on a kernel without ZRAM support** (direct to main): for example Oracle Linux's UEK. ZRAM is skipped with a warning and the remaining sysctl and ulimit work still runs, so `optimize` completes instead of aborting.

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
