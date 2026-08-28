# 🌐 Proxy URL Sources

This guide covers feeding the provider a **live proxy list URL** instead of (or alongside) a static `proxy.txt` file — useful if you pull proxies from a service that publishes a rotating list of fresh entries.

## 🛑 The Problem

Some proxy-list endpoints publish new `ip:port` entries on a rolling basis — some every few minutes. Without this feature, picking up fresh entries means manually re-downloading the list, re-importing it, and hoping you don't duplicate proxies you already added. There's also no way to automatically prune entries that go dead without touching proxies you added by hand.

## ⚡ What It Does

Point the provider at a URL and it will:
- 🌱 **Fetch on an interval** (default every 15 minutes) and add any genuinely new proxies — entries already running, by address, are skipped. This never disturbs already-warmed-up proxies.
- 🧹 **Optionally clean up dead entries** on a much slower, separate cadence (default once a day) — and only the ones that came from a URL, by default. Proxies you added yourself via a file or `proxy add` are left alone unless you explicitly widen the cleanup scope.
- 🤝 **Coexist with your existing proxy file.** A URL source is additive — you can run `--proxy_file` and `--proxy_url` at the same time. They share one hot-reload pipeline (see [Proxy Management & Hot-Reloading](Proxy-Management.md)).

---

## 🎓 Stage-1 Quality Gate

Proxies that pass stage-0 (the SOCKS5 greeting and API `CONNECT`) are not yet
trusted with client traffic. Each one is graded against a sampled block of
the backend's destination table — dialed through the proxy itself at `:443`
— before it reaches the auth queue.

**Scoring**: a pass samples `sample_width` hosts (default 12) from the
backend's ~127-host health table, disjoint from the previous pass, and
dials each at `:443` with a per-target timeout of `timeout_ms` (default
4000ms). The proxy's score is the fraction of successful dials in the
pass. Only a SynAck counts as success; a timeout or refusal is not treated
as proof the proxy is bad, since the target host itself could be
unreachable for unrelated reasons. Hostnames are resolved through the
box's own DNS, not through the proxy being probed, so a proxy with broken
DNS is not penalized for a resolution failure that isn't its fault.

A grade is only written when the pass is decidable — a quorum of the
sampled hosts answered and the probe context wasn't cancelled. An empty,
cancelled, or resolver-gutted pass leaves the proxy's previous grade in
place rather than overwriting it with a false failure.

**`pass_bar`**: a proxy must score at or above `pass_bar` (default 0.6) to
be admitted to the auth queue. A score at or above `preferred_bar`
(default 0.9) marks the proxy as preferred tier. Proxies that clear
stage-0 but fail stage-1 never spawn, and are reported separately from
dead proxies in the auth gate's summary.

---

## 🎓 A-F Quality Tiers (v3.23.0-fix.27.0+)

Every URL-source proxy that clears the stage-1 gate is assigned a letter
grade from its score:

| Grade | Score |
|---|---|
| A | `>= 0.9` |
| B | `>= 0.8` |
| C | `>= 0.7` |
| D | `>= 0.6` |
| F | `< 0.6` |

- **Admission funnel**: candidates from all sources are pooled and added
  best-first up to the cache cap, instead of per-source in whatever order
  sources happen to be processed.
- **Best-overall cache eviction**: when the cache is full, eviction
  compares candidates across all sources by tier — a full cache keeps the
  fleet's highest-tier proxies regardless of source.
- **Per-cycle grade breakdown** is logged (`admitted by tier`, `probe grade
  breakdown`, `cap eviction`, `reaper: refreshed grade`).
- **Fetch probing**: each cycle probes only newly-seen addresses; the
  reaper's stale sweep refreshes grades on already-cached proxies.
- **Cross-source duplicates** are table-probed once per cycle.

## 🎓 Paid / File-List Proxy Grading (v3.23.0-fix.27.0+)

Proxies from `--proxy_file` or the internal config bypass the URL
admission gate by construction. A background sweep grades every non-URL
proxy the box serves with the same stage-1 table probe on the same 1-3h
stale cadence, persisting Score/Graded/Failed/LastGraded into
`proxy.state`. **Read-only by construction**: grades never gate admission,
never evict, and never feed give-up/cleanup — a graded F keeps serving
exactly as it did before. `proxy_probe.json enabled=false` skips the sweep
entirely.

**Credentialed URL entries**: RFC 1929 auth (`host:port:user:pass`) is
carried through both the stage-0 and stage-1 probes, so paid/credentialed
URL-source entries are graded on the same footing as free ones.

**Kill switch**: create `~/.urnetwork/proxy_probe.json` with
`{"enabled": false}` to disable stage-1 entirely and fall back to
stage-0-only admission. See [Configuration](Configuration.md) for the
full set of `proxy_probe.json` knobs.

**Persistence**: `Score`, `Graded`, and `Failed` are stored per proxy in
`proxy_url.json`, alongside the existing cache fields.

---

## 📝 Setting It Up

### 🐧 Binary / Linux Service

Start the provider with a live source:

```sh
urnetwork provide --proxy_url=https://example.com/your-proxy-list.txt
```

Or manage sources at runtime without restarting:

```sh
urnet-tools proxy add-source https://example.com/your-proxy-list.txt
urnet-tools proxy remove-source https://example.com/your-proxy-list.txt
```

`add-source` triggers an immediate fetch and persists the URL so it survives restarts — you don't need to keep passing `--proxy_url` by hand.

### 🐋 Docker

```bash
docker run -d \
  --name=urnetwork \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -e BUILD=jwt \
  -e PROXY_URL='https://example.com/your-proxy-list.txt' \
  -v /path/to/your/proxy.txt:/app/proxy.txt \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest YOUR_AUTH_CODE_HERE
```

`PROXY_URL` and `-v .../proxy.txt` can be used together — the URL source adds on top of whatever's in the mounted file.

> [!TIP]
> **Multiple URLs in Docker:** there's no `PROXY_URL_2`/`PROXY_URL_3` — repeating `-e PROXY_URL=...` just overwrites itself, since Docker env vars aren't additive. Put all your sources in one comma-separated `PROXY_URL`. The repeatable `--proxy_url=<url> --proxy_url=<url>` form is only available on the binary/CLI side, where docopt flags can be passed more than once.

---

## 🎛️ Tuning Flags

| Flag | Env var | Default | What it controls |
| :--- | :--- | :--- | :--- |
| `--proxy_url=<url>` | `PROXY_URL` | — | The live source. Pass multiple times (or comma-separate the env var) for more than one source. |
| `--proxy_url_refresh=<duration>` | `PROXY_URL_REFRESH` | `15m` | How often to fetch and add new entries. |
| `--proxy_url_max=<n>` | `PROXY_URL_MAX` | `500` | Caps total URL-sourced proxies. `0` = unlimited. Once hit, new entries are skipped until cleanup or restart frees room — existing proxies are never evicted to make space. |
| `--proxy_dead_cleanup_scope=url\|all\|none` | `PROXY_DEAD_CLEANUP_SCOPE` | `none` | Which proxies the **automatic** daily cleanup is allowed to remove. `none` disables it entirely (manual `proxy remove-dead` still works regardless). |
| `--proxy_dead_cleanup_interval=<duration>` | `PROXY_DEAD_CLEANUP_INTERVAL` | `24h` | How often the automatic cleanup runs, when scope isn't `none`. |

> [!TIP]
> **If you're pulling from a free/public list:** set `--proxy_dead_cleanup_scope=url`. That way the provider keeps adding fresh entries every 15 minutes and quietly retires ones that never panned out once a day — but it will never touch proxies from your own hand-curated file.

---

## 📄 Supported List Format

v1 supports plain-text lists only, one proxy per line — the same format `--proxy_file` already accepts, plus an optional `socks5://` prefix:

```
1.2.3.4:1080
1.2.3.4:1080:myuser:mypass
socks5://1.2.3.4:1080
socks5://myuser:mypass@1.2.3.4:1080
```

Blank lines and `#` comments are ignored. Lines with a non-`socks5://` protocol prefix are skipped with a warning (this fork is SOCKS5-only) — one bad line doesn't fail the whole fetch. CSV and JSON list formats are not supported yet.

---

## 🧹 How Cleanup Scope Works

Every proxy is tagged internally with where it came from: `url`, `file`, or `internal` (i.e. added via `proxy add`). The `--proxy_dead_cleanup_scope` flag controls which of those tags the **automatic** daily job is allowed to act on:

| Scope | Behavior |
| :--- | :--- |
| `none` (default) | Automatic cleanup never runs. You prune dead proxies yourself with `urnet-tools proxy remove-dead`. |
| `url` | Automatic cleanup only ever removes dead proxies that came from a `--proxy_url` source. Your file/`proxy add` entries are never touched automatically. |
| `all` | Automatic cleanup treats every source the same — equivalent to running `proxy remove-dead --all` once a day. |

Manual `urnet-tools proxy remove-dead` is unaffected by this setting in every case — it always lets you choose interactively, regardless of source.

---

## ⏳ Auth Give-Up Backoff & Permanent Eviction

URL-sourced lists are often noisy — an entry can be reachable on the wire but fail auth forever (e.g. a live SOCKS5 endpoint behind a stale password). Older builds retried these on a flat 15-minute cycle indefinitely, keeping the auth rate limiter busy and filling logs with give-up messages for hopeless addresses.

Each URL-sourced address now gets an **escalating per-address backoff** after it gives up on auth:

| Give-up cycle | Retry delay (±20% jitter) |
| :--- | :--- |
| 1st | 15m |
| 2nd | 30m |
| 3rd | 1h |
| 4th | 2h |
| 5th | 4h |
| 6th | 8h |
| 7th | 16h |
| 8th+ | 24h (capped) |

After **10 give-up cycles**, the address is **permanently evicted**: removed from the URL cache and written to a blacklist persisted in `proxy_url.json`. Blacklisted addresses are skipped at the only add path (`mergeProxyURLEntries`), so a hopeless proxy can never re-enter the auth lottery — even across provider restarts or if the source list keeps republishing it.

> [!NOTE]
> Eviction is permanent and survives restarts. If a previously-blacklisted address genuinely comes back to life (e.g. the upstream fixes the password), remove it from the `Blacklist` map in `proxy_url.json` and restart, or it will keep being skipped.

This is independent of the daily dead-proxy cleanup (`--proxy_dead_cleanup_scope`): backoff/eviction is driven by repeated **auth give-ups**, while cleanup acts on proxies marked **dead** by the health tracker.

---

## 🛡️ Overlapping Fetch Prevention

Concurrent fetch cycles for the same URL are now prevented. If a fetch is already in progress when the refresh interval fires, the new cycle is skipped with a log line (`[proxy-url] fetch already in progress for <url>, skipping`). This prevents accidental thundering-herd when multiple triggers fire near-simultaneously (e.g., a `--proxy_url` interval coinciding with a `add-source` command).

## 🧹 Memory Pruning

The provider periodically prunes internal data structures to control memory growth over long runtimes with large proxy lists:

- **Failure history**: Per-proxy failure counters and last-error timestamps for proxies that have been removed or replaced are freed after the cleanup cycle.
- **Proven set**: The internal set of addresses that have been validated (proven working) is periodically pruned of entries that are no longer in the active proxy list, preventing unbounded growth.

This ensures that a provider running for weeks with high proxy churn doesn't accumulate stale metadata that bloats heap usage.

## 🌐 Custom HTTP Client for URL Fetches

The URL fetch subsystem now uses a dedicated HTTP client with sensible timeouts, rather than relying on the provider's default transport:

- **Connection timeout**: 30 seconds
- **Response timeout**: 60 seconds
- **User-Agent**: `urnetwork-proxy-url-fetcher/1.0`

This prevents a slow or hanging proxy list URL from blocking the provider's control-plane transport. The dedicated client is used exclusively for `--proxy_url` / `PROXY_URL` fetches and is independent of the provider's WebSocket/QUIC transports.

---

## ❓ FAQ

**Will this duplicate proxies already in my file?**
No — new entries are deduplicated by address against everything currently running (URL, file, and internal sources share one address space). If the same `ip:port` shows up in both your file and a URL source, the most recently applied one wins, same as today's hot-reload behavior.

**What happens if the URL is unreachable?**
The fetch cycle is skipped with a logged warning. Already-added proxies from that source keep running — a stale list is better than wiping working proxies because of a transient network blip. After several consecutive failures, the provider logs a louder warning suggesting the source may be dead, but it won't remove the source for you.

**What happens when I run `proxy clear`?**
`proxy clear` (which calls `proxy remove --all`) now wipes the entire `proxy_url.json` — cache, blacklist, and source URLs — alongside the internal config and `proxy.state`. After a clear, no URL-sourced proxies will be fetched unless a source is re-added via `urnet-tools proxy add-source <url>`. This ensures a clean slate when you want only the proxies you explicitly add.

**Does this validate proxies before adding them?**
No — newly added proxies go through the same warmup and health-tracking lifecycle as any other proxy (see [Proxy Management & Hot-Reloading](Proxy-Management.md#-removing-dead-proxies-interactively)). If a fetched proxy never connects, it'll show as `dead` in `proxy health` and get swept up by cleanup (if scope allows) or by a manual `remove-dead`. An address that connects but repeatedly fails auth is handled separately — it backs off on an escalating schedule and is permanently evicted after 10 give-up cycles (see [Auth Give-Up Backoff & Permanent Eviction](#-auth-give-up-backoff--permanent-eviction)).
