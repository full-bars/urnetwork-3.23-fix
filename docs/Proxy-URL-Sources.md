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
| `--proxy_url_max=<n>` | `PROXY_URL_MAX` | unlimited | Caps total URL-sourced proxies. Once hit, new entries are skipped until cleanup or restart frees room — existing proxies are never evicted to make space. |
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

**Does this validate proxies before adding them?**
No — newly added proxies go through the same warmup and health-tracking lifecycle as any other proxy (see [Proxy Management & Hot-Reloading](Proxy-Management.md#-removing-dead-proxies-interactively)). If a fetched proxy never connects, it'll show as `dead` in `proxy health` and get swept up by cleanup (if scope allows) or by a manual `remove-dead`.
