### Unreleased

**Status:** draft. Nothing is tagged or pushed. The open PRs 981, 986, 989, 990, 991, 992, 993 and 994 are merging before the cut and are covered here. The full list of merged changes since the last tag is the Unreleased section of [CHANGELOG.md](../CHANGELOG.md); fill the rest of this draft from it at the cut.

Legend: **[redeploy]** needs the built artifact redeployed. **[installer]** needs the installer or unit refresh. **[no redeploy]** docs, CI or tests only.

## Transport

- **A silent platform edge no longer drains egress through proxied H3** **[redeploy]** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/994>). With `h3` set to `all`, a count or `auto`, every proxy identity runs the H3, H3-over-dns and dns-pump carriers beside its H1. When the edge answers nothing, each carrier redials on its own 30 to 60 second timer and every dial runs the whole QUIC retransmit ladder, about 33 KB. One node on `h3 all` spent a flat 15.5 to 16.4 Mbit/s for about 15 hours of the 2026-10-23 outage, roughly 30 times more tx than rx, while the governor read `verdict=ok rate=200.0/s`. Three things hid it, and each is fixed: QUIC bytes were never charged to the connect attempt, so the failed-byte breaker saw zero; a canceled dial (two of every three, because the per-transport H3 gate cancels a lower-priority carrier when a better one waits) was dropped with its bytes; and H3 dials never waited on the attempt budget. The handshake's wire bytes are now charged once, a canceled attempt's bytes count toward the breaker without counting as an attempt, and a proxied identity's H3 waits on the shared budget like its H1. The direct identity, proxy index 0 and family-pinned transports never wait.

## Provider

- **A failed OOM-cap state write is reported, not discarded** **[redeploy]** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/993>). The startup OOM-cap decision wrote its state with the result discarded. That state carries the cap and the clean-window clock, so a lost write could let the next start read a stale window and relax the cap straight after a fresh OOM kill, and the applied path could ledger a cap that nothing enforces. Both branches now report the failure in the warnings, and the applied path leaves the timeline to the last cap that really was persisted.
- **The memory-headroom warning fires at the cap on a host whose RAM is unreadable** **[redeploy]** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/990>). `EffectiveRAMLimit()` returns 850 MiB with the source `fallback default, host memory unreadable` when it cannot read host memory, which the headroom threshold turned into its 150 MiB floor, so the warning fired far too late on such a host. The limit is now classified by its source, never its value: an unreadable host gets the 400 MiB cap, a real 850 MiB host keeps the floor, and the note explaining the threshold goes to the persisted log once.
- **The degraded reaper holds off when the scores cannot rank the pool** **[redeploy]** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/986>). `scoreDegradedProxies` ranks by lifetime bytes and contracts, and when the platform is unreachable neither moves, so every candidate tied and the reaper still dropped half the pool (249 of 498 proxies on one node in the 2026-10-23 outage). A round where every candidate scored the same, or there are too few to rank, now holds off instead of culling.
- **The smart dialer costs nothing while it is off** **[redeploy]** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/989>). The idle preference's off state still cost a goroutine, a context and a timer per proxy, so a node with thousands of proxies carried thousands of idle goroutines. The start function is gated, so while the preference is off there is none. Because the loop now only exists when the preference was on at start, `smart_dialer` is reclassified restart-required: `set smart_dialer on` is persisted and takes effect at the next start, the CLI says so, and `set` now prints the same "takes effect at the next start" wording for `profile` and `ramlogs`, which never applied live. The default is unchanged: off.

## Observability

- **The disk benchmark samples by time, not a fixed 16 MiB** **[redeploy]** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/992>). The benchmark stopped at a fixed 16 MiB, which an NVMe writes in about 20 ms, so the rate reported at startup and in `urnet_disk_*` was a burst figure that moved between runs. The 3 s budget now governs, up to 256 MiB: microSD is unchanged at about 20 MiB, eMMC samples about 240 MiB, NVMe the 256 MiB ceiling in about 85 ms. The result carries its duration, so the log reads `900.0 MB/s (256 MB written in 85 ms)`. The container guard, IO-pressure skip, space cap, per-device cache and hung-write watchdog are unchanged.

## Install and CI

- **FreeBSD is served from the universal autoinstaller** **[no redeploy]** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/981>). The universal autoinstaller sent any `uname` it did not recognise to Unsupported OS, which included FreeBSD. The `dl` worker now routes FreeBSD to `install-freebsd.sh`, with the matching wrangler route and alias, and its landing text names Linux, macOS and FreeBSD and says the Linux path covers OpenRC. Depends on the FreeBSD installer script being on `main`. This is the `dl` worker, so it deploys separately from the provider.
- **The installer PATH-hint test runs in CI** **[no redeploy]** (<https://github.com/full-bars/urnetwork-3.23-fix/pull/991>). `scripts/test_provider_install_shellrc_symlink.sh` proves the installer's PATH hint never writes through a name the service user controls, and it never ran in CI. It now runs with full history, and the script fails, not skips, when its falsification pin is missing under `REQUIRE_FALSIFICATION`, so a shallow checkout cannot turn it into a silent pass.

## Deploy notes

- **The provider needs a redeploy.** There is no installer or unit change and no new required setting.
- **`smart_dialer` is now restart-required.** A node that runs `urnet-tools set smart_dialer on` sees the change at its next start, not live, and the CLI says so. The default is still off.
- **Only nodes with `h3` set to `all`, a count or `auto` change for the H3 fix.** Nodes on the default `h3 direct` dial H3 from the direct identity alone and spent 0.1 to 1.1 Mbit/s in the same outage. `urnet-tools set h3` shows the setting. In an outage those nodes log `[govern] budget cut ... reason=bytes`, which was silent before.
- **The disk figure changes on fast media.** The first benchmark after the update samples up to 256 MiB, so an NVMe rate is a representative figure and no longer a 20 ms burst that moved between runs. Slow media report what they did before.
- **The `dl` worker deploys separately** for the FreeBSD route (`workers/dl`), and the FreeBSD installer script it points at must already be on `main`.

## Caveats

- The H3 fix has not been run against a live outage. The attempt budget floor is 2 per second, so a throttled node still spends about 0.5 Mbit/s, not zero. A silent timeout still carries no weight in the governor's verdict, on purpose: a proxy that cannot relay UDP fails the same way whatever the backend does.

> [!NOTE]
> Everything that was staged here has shipped. The v33.1 batch is in
> [v3.23.0-fix.33.1](v3.23.0-fix.33.1.md), the engine release that follows it is
> [v3.23.0-fix.34.0](v3.23.0-fix.34.0.md), and the patch on top of the engine
> release is [v3.23.0-fix.34.1](v3.23.0-fix.34.1.md). The `v3.23.0-fix.31.1`
> through `v3.23.0-fix.31.9-rc1` tags were release candidates cut to exercise
> that work on a test node; they were never deployed, and their contents are
> folded into the v31.0 notes rather than tracked separately.
