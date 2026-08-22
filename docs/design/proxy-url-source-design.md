# Proxy URL Source Design

**Date:** 2026-06-19
**Status:** Approved — ready for implementation planning

## Problem

Operators running large numbers of proxies currently maintain proxy lists manually (`--proxy_file` or `proxy add`/`remove` against the internal config). Several public proxy-list endpoints publish fresh `ip:port` entries on a rolling basis (some rotating every few minutes). Picking these up today requires manually re-downloading and re-importing the list, with no deduplication against what's already running and no automatic cleanup of entries that go dead.

**Goal:** Let the provider treat a URL as a live proxy source: periodically fetch it, add new (deduplicated) entries without disturbing already-running proxies, and optionally clean up dead entries on a separate, slower cadence — without requiring a restart, and without forcing this behavior on operators who don't want it.

---

## Scope

This design covers proxy ingestion from a remote URL only: fetching, parsing, deduplication, merging into the existing hot-reload pipeline, and source-scoped dead-proxy cleanup. It builds on top of the existing hot-reload system (`docs/design/proxy-hot-reload-design.md`) rather than replacing it.

**v1 format support:** plain-text (`.txt`) lists only, line-based. CSV and JSON list formats are explicitly out of scope for v1 (noted as future work) — field-naming conventions for JSON list APIs vary too much to commit to a schema without a concrete second example to validate against.

---

## Relationship to the existing hot-reload design

The existing hot-reload design treats external file (Workflow A) and internal config (Workflow B) as **mutually exclusive, never-interacting** sources. This design intentionally breaks that exclusivity: a URL source is **additive on top of either workflow**. An operator can run `--proxy_file=<path>` (Workflow A) *and* `--proxy_url=<url>` together, or run internal-config mode (Workflow B) *and* `--proxy_url=<url>` together. The `ProxyReloader`'s existing diff/cancel/start/drain logic is unmodified — it still just reconciles "desired set" vs "running set" by address; the only change is that the desired set can now be assembled from up to three contributing sources instead of one.

---

## Architecture

### New component: `ProxySourceManager` (`provider/proxy_url_source.go`)

A new, self-contained module started alongside the existing JWT refresher and `ProxyReloader` in `main.go`'s startup sequence. It does not modify `ProxyReloader`'s reload/diff/cancel/drain logic — it only ever calls the existing `writeReloadTrigger()` to hand off work.

**Add ticker** (default `15m`, configurable):
1. GETs each configured URL.
2. Parses the response body with the line-based parser (extended — see Parsing below).
3. Dedups new entries against the full current address set (URL + file + internal share one address namespace — same "last write wins" semantics the reloader already uses).
4. Merges genuinely new entries into the internal `~/.urnetwork/proxy` config, tagging each with `source: "url"` in `proxy.state` (file/internal-sourced entries are tagged `"file"` / `"internal"` for symmetry).
5. Enforces `--proxy_url_max` if set: once the cap is reached, further new entries from that cycle are skipped (existing entries are never evicted to make room).
6. Calls `writeReloadTrigger()`.

**Cleanup ticker** (default `24h`, only runs if `--proxy_dead_cleanup_scope != none`):
- Runs the same health-check logic as the existing `provider proxy remove-dead`, pre-filtered by the `source` tag in `proxy.state` according to `--proxy_dead_cleanup_scope`.
- Manual `provider proxy remove-dead --all` is unaffected by this flag and continues to work as today — the scope flag only governs the automatic daily job.

### Parsing

The existing line parser (`ip:port`, `ip:port:user:pass`, blank lines / `#` comments ignored) gains support for a protocol-prefixed form:

```
socks5://1.2.3.4:1080
socks5://user:pass@1.2.3.4:1080
```

Only `socks5://` is accepted (this fork is SOCKS5-only); any other scheme prefix is rejected with a warning log and the line is skipped, not the whole fetch. This parser is shared by `--proxy_file`, `proxy add --proxy_file`, and the new URL source — one parsing path, three callers.

---

## Configuration surface

### CLI flags (binary)

| Flag | Default | Purpose |
|---|---|---|
| `--proxy_url=<url>` | — | Live proxy source. Repeatable for multiple sources. Additive with `--proxy_file` / internal config. |
| `--proxy_url_refresh=<duration>` | `15m` | Add-only fetch interval. |
| `--proxy_url_max=<n>` | unset (unlimited) | Cap on total URL-sourced proxies. |
| `--proxy_dead_cleanup_scope=url\|all\|none` | `none` | Which source(s) the *automatic* daily cleanup targets. |
| `--proxy_dead_cleanup_interval=<duration>` | `24h` | Cleanup cadence, only relevant when scope != `none`. |

### Runtime subcommand

- `provider proxy add-source <url>` / `provider proxy remove-source <url>` — persists URL sources into `~/.urnetwork/proxy` so they survive restarts without re-passing `--proxy_url` every time. Triggers an immediate fetch on add.

### Docker environment variables

| Env var | Maps to |
|---|---|
| `PROXY_URL` | `--proxy_url=<url>` (comma-separated for multiple sources) |
| `PROXY_URL_REFRESH` | `--proxy_url_refresh=<duration>` |
| `PROXY_URL_MAX` | `--proxy_url_max=<n>` |
| `PROXY_DEAD_CLEANUP_SCOPE` | `--proxy_dead_cleanup_scope=url\|all\|none` |
| `PROXY_DEAD_CLEANUP_INTERVAL` | `--proxy_dead_cleanup_interval=<duration>` |

Read directly via `os.Getenv` inside `provide()` as a fallback whenever the
corresponding `--proxy_*` flag isn't passed — the same pattern already used
by `URNETWORK_PROFILE` and `URNETWORK_REPORT_URL`. No startup-script changes
are needed; setting the env var in `docker run` is sufficient.

---

## Error handling

- Fetch failures (network error, non-200, empty body, unparseable content) log a single rate-limited warning and skip that cycle. They never crash the provider and never clear already-merged URL-sourced proxies — a stale-but-working list is better than an empty one.
- A source failing N consecutive cycles (suggested N=5) logs an elevated warning that the URL may be dead, but is not auto-removed — the operator decides via `remove-source`.
- Lines that fail to parse are skipped individually with a warning; one bad line does not fail the whole fetch.

---

## Testing

- Unit tests for the parser extension: `protocol://[user:pass@]ip:port` variants, rejected non-socks5 schemes, malformed lines, mixed-format files.
- Unit test for cross-source dedup by address (URL entry colliding with an existing file/internal entry — confirm "last write wins" matches today's documented reloader behavior).
- Unit test for `--proxy_dead_cleanup_scope` filtering (`url` / `all` / `none` each touch only the proxies they should).
- Unit test for `--proxy_url_max` enforcement (cap reached mid-cycle, no eviction of existing entries).
- Integration test against a local HTTP server serving a sample `.txt` list: verify a full add cycle end-to-end and a forced short-interval cleanup cycle, following the existing `nsenter` + `tc netem` warmup methodology used for other provider features.

---

## Out of scope for v1

- CSV and JSON list formats.
- Per-source refresh intervals (one global `--proxy_url_refresh` applies to all configured URL sources).
- Proactive connectivity validation of fetched proxies before merge (they go through the same warmup/health lifecycle as any other newly added proxy today).
