# Proxy Hot-Reload Design

**Date:** 2026-06-03
**Status:** Approved — ready for implementation planning

## Problem

Every proxy list change (add, remove) currently requires a full provider restart (`urnet-tools stop && urnet-tools start`). Each restart wipes the warm-up state across the entire proxy fleet. Proxies take 8–12 hours to warm up — establishing contracts, building routing state, and being deemed trustworthy enough for bulk traffic assignment. Removing one dead proxy today costs a full cold-start on every live proxy.

**Goal:** Add and remove proxies without touching running ones. Only the proxies that actually changed are affected. All others keep running with their full warm state intact.

---

## Scope

This design covers proxy list changes only. Settings changes (turbo, eco, lowmem, etc.) that require restarts are out of scope.

---

## Architecture

### External Proxy File as Live Source

The provider gains a `--proxy_file=<path>` startup flag designating an external file as the live proxy source. If omitted, falls back to `~/.urnetwork/proxy` (existing behavior fully preserved — no breaking change).

**Docker workflow:**
```
-v /home/ubuntu/pshalf.txt:/app/proxy.txt
```
Startup script passes `--proxy_file=/app/proxy.txt`. User edits their host file, runs `urnet-tools proxy refresh`. No internal file management needed.

**Binary workflow:**
```
urnetwork provide --proxy_file=/home/ubuntu/pshalf.txt
```
User edits their file, runs `urnetwork proxy refresh`. The existing `proxy add/remove` commands continue to work for users who prefer them.

### New Files (all in `~/.urnetwork/`)

| File | Purpose |
|---|---|
| `proxy.state` | JSON written atomically at startup and after each reload. Records the live source path, loaded proxy set (address → monotonic ID + health snapshot). Written via temp file + `os.Rename` within the same directory. |
| `proxy.reload` | Trigger file. Refresh subcommand writes a content hash or sequence number here after confirmations pass. Provider detects the change by hashing contents (not mtime — unreliable on Docker bind mounts). |
| `proxy.lock` | Lockfile. Prevents concurrent `proxy refresh` calls from racing on the proxy file. |

### Provider Internal Changes

- **Monotonic proxy ID counter** — proxies are assigned a never-reused integer ID at goroutine spawn. Health tracking, transport logs, and select logs all key off this ID, not list position. Decouples identity from list position so hot-reload never corrupts health history.
- **Per-proxy cancel map** — `map[address]cancelFunc` tracks running goroutines. Reload cancels entries for removed proxies, adds new entries for added ones.
- **`UnregisterProxy(id)`** — called after a proxy goroutine fully drains (not at cancel time). Prevents health registry from growing unbounded.
- **Reload watcher goroutine** — polls `proxy.reload` content hash every 2 seconds. On change: acquires an internal reload mutex (rejects concurrent reloads), re-reads the source file, diffs vs the in-memory cancel map (authoritative), starts new goroutines, cancels removed ones, updates `proxy.state`.
- **Provider owns the diff** — the refresh subcommand only writes the trigger. The watcher does the authoritative diff against the live in-memory state and re-validates proxy health at reload time.

---

## Proxy Format

The only supported format is:

```
ip:port:user:pass
```

Non-empty user and password are **required**. This is a security requirement, not just a formatting rule.

### Why unauthenticated proxies are prohibited

The urnetwork encryption tunnel ends at the provider. After that, traffic flows through the SOCKS5 proxy to the internet — the SOCKS5 proxy is the last hop and sees all plaintext traffic (for non-HTTPS destinations) and destination metadata. Routing through an unauthenticated proxy breaks the trust chain urnetwork users rely on:

- **No access control** — open/free proxies are overwhelmingly used by botnets, scrapers, and spam operations
- **Terrible IP reputation** — these IPs are flagged and blocked by virtually every major service, CDN, and fraud detection system
- **Monitoring risk** — the proxy operator (or anyone who has compromised the proxy) can observe plaintext traffic and connection metadata
- **Privacy violation** — urnetwork users expect a private, high-quality connection; routing their traffic through garbage IPs defeats that entirely

The provider technically accepts `ip:port` (falls through to a no-auth SOCKS5 connection) but this is a silent footgun. Validation enforces credentials at entry points so this path is never reached.

For IP-whitelisted proxies that don't check credentials, use dummy values: `ip:port:dummy:dummy`.

**Validation** fires at `proxy add` time and when scanning new entries during `proxy refresh`. Invalid entries are rejected before any confirmation prompt:

```
ERROR: proxy "1.2.3.4:1080" — missing credentials.
Required format: ip:port:user:pass

ERROR: proxy "user:pass@1.2.3.4:1080" — unsupported format.
Required format: ip:port:user:pass
```

---

## Proxy Health States

### Dead
Never came up. No `everUp` flag set. Confirmed dead only after provider uptime ≥ 65 minutes (same threshold as the existing `deadConfirmDelay`).

### Degraded (tiered by time since last seen online)

| Label | Time since last seen online | Displayed as |
|---|---|---|
| `recently offline` | < 24h | `proxy[4] 1.2.3.4:1080 — recently offline (3h 12m)` |
| `offline` | 24–72h | `proxy[4] 1.2.3.4:1080 — offline (2d 4h)` |
| `long offline` | 72h–7d | `proxy[4] 1.2.3.4:1080 — long offline (5d 2h)` |
| `inactive` | 7d+ | `proxy[4] 1.2.3.4:1080 — inactive (9d 14h)` |

`recently offline` is treated identically to UP for confirmation purposes — a proxy that was online 3 hours ago likely still has warm state worth protecting.

`inactive` (7d+) is treated identically to dead for confirmation purposes — `remove-dead` may optionally include these.

**Known limitation:** there is no automatic degraded → dead escalation today. Time-based escalation is a future feature. The `LifetimeRecovered` and `LifetimeLost` counters in `proxy_health.go` are a foundation for future flapping detection.

### UP
Currently connected and moving traffic.

---

## Commands

### `provider proxy refresh`

Diffs the proxy source file (desired) against currently running proxies (live in-memory state via `proxy.state`), shows what will change, handles confirmation, writes the trigger.

Also available as `urnet-tools proxy refresh` or `urnet-tools proxy reload` (alias) — cross-platform urnet-tools subcommands that run `docker exec <container> provider proxy refresh` against the managed container.

**Warmup block:** If provider uptime < 8 hours, refuses to remove any proxy:
```
Provider has been running for 2h 14m. Proxies need 8-12h to warm up —
a proxy that looks dead now may still be establishing contracts.
Run again after 8h uptime, or use --force to override.
```

**Concurrent reload rejection:** If a reload is already in progress:
```
Reload already in progress — try again in a moment.
```

No queue. One reload at a time.

### `provider proxy remove-dead`

Reads confirmed-dead proxies from live health state (respecting the 65-minute `deadConfirmDelay`), removes them from the source file, triggers refresh. Optionally includes `inactive` (7d+) proxies.

Explicitly does NOT touch degraded or UP proxies.

---

## Confirmation UX

### Case 1 — Dead/inactive proxies only (low risk)

```
proxy refresh: 3 proxies will be removed, 2 will be added.

  Removing:
    proxy[2]  1.2.3.4:1080   — dead
    proxy[7]  5.6.7.8:1080   — dead
    proxy[11] 9.0.1.2:1080   — inactive (8d 3h)

  Adding:
    10.0.0.1:1080
    10.0.0.2:1080

Proceed? [y/N]
```

Single confirmation.

### Case 2 — Degraded (offline/long offline/recently offline) or UP proxies being removed (high risk)

```
proxy refresh: 5 proxies will be removed, 1 will be added.

  Removing:
    proxy[2]  1.2.3.4:1080   — dead
    proxy[4]  2.3.4.5:1080   — UP, moving traffic
    proxy[9]  5.6.7.8:1080   — recently offline (3h 12m)
    proxy[11] 9.0.1.2:1080   — offline (2d 4h)
    proxy[14] 3.4.5.6:1080   — dead

  Adding:
    10.0.0.1:1080

WARNING: 3 of the proxies being removed are online or recently online.
Remove them anyway? [y/N]
```

If `y`:
```
Are you sure? This may interrupt live traffic. [y/N]
```

Both confirmations required. Abort entirely on any `N`.

### Case 3 — `--force` override during warmup block

Even with `--force`, removing an UP or recently-offline proxy always requires both confirmations. `--force` only bypasses the 8-hour uptime gate — it never reduces confirmation count.

```
WARNING: Provider has been running for only 2h 14m.
Are you sure you want to remove proxies before warmup is complete? [y/N]
```

Then the normal high-risk confirmation flow if applicable.

### Edge Cases

| Situation | Behavior |
|---|---|
| No diff detected | `proxy list is already up to date. Nothing to do.` |
| Provider not running / no `proxy.state` | Error: suggest editing file for next startup; `proxy add/remove` still work |
| All proxies would be removed | Extra warning: `This will leave the provider running with no proxies — unique IPs drive earning scale; removing all proxies significantly reduces capacity.` |
| Proxy file parse/read error | Refuse to write anything. Never overwrite a corrupt file with an empty one. |
| New proxy references missing auth key | Error before confirmation prompt: validate all entries resolve before proceeding |
| Concurrent `proxy refresh` calls | Reject second call immediately: `Reload already in progress — try again in a moment.` |
| Proxy warming up < 65min classified as dead | Gated by `deadConfirmDelay` — not shown as dead until confirmed |

---

## urnet-tools Additions (in scope for this release)

### Warmup Protection — Cold Restart Warning

Any command that triggers a cold restart (stops the provider, resets all proxy warm-up state) applies this warning flow when provider uptime ≥ 8 hours. The warning always shows actual uptime and live proxy count so the user sees the concrete cost, not an abstract prompt.

**Commands that trigger this warning:**
- `urnet-tools stop`
- `urnet-tools restart`
- Any settings change that forces a provider restart (turbo, eco, lowmem, ramlogs, etc.) — applies now even though settings hot-reload is a future feature; the protection ships with the command, not after

**Warning flow (uptime ≥ 8 hours):**

```
╔══════════════════════════════════════════════════════════════╗
║  WARNING: COLD RESTART — ALL WARMUP PROGRESS WILL BE LOST   ║
╚══════════════════════════════════════════════════════════════╝

Provider uptime:  14h 32m
Warmed proxies:   37 online, 4 degraded
Active traffic:   yes

Restarting will immediately disconnect all proxies and reset the
8-12h warmup period from scratch. Earning capacity will be
significantly reduced during recovery.

Are you sure you want to discard 14h 32m of warmup progress? [y/N]
```

If `y`:
```
This will interrupt live traffic on 37 proxies. Proceed? [y/N]
```

**If uptime < 8 hours:** single lightweight confirmation only — warmup progress is minimal and the cost is low.

**Rule:** If it resets warmup state and uptime ≥ 8h, it shows this warning. No exceptions. New commands added to urnet-tools in the future that stop or restart the provider must inherit this check.

---

### `urnet-tools stop`
Stops the provider. Applies the cold-restart warning above if uptime ≥ 8 hours.

### `urnet-tools restart`
Convenience wrapper for `urnet-tools stop && urnet-tools start`. Applies the cold-restart warning above if uptime ≥ 8 hours. Saves the user from running two commands for a deliberate restart.

---

## Future Directions

- **Approach B: Unix domain socket control channel** (`~/.urnetwork/provider.sock`) — bidirectional, live health state, foundation for richer automation. Implement after Approach A is stable.
- **Approach C: HTTP control endpoint** — extend existing `--port` status server with `/proxy/reload`. Remote-accessible, requires `--port` to be enabled.
- **Settings hot-reload** — apply turbo, eco, lowmem, and other settings changes without restarting the provider or losing proxy warm-up state. High value; shares much of the infrastructure built for proxy hot-reload (trigger file, watcher goroutine, reload mutex). Natural next feature after this release.
- **Flapping detection** — use existing `LifetimeRecovered` / `LifetimeLost` counters to surface proxies with high up/down churn.
- **Time-based degraded → dead escalation** — automatically promote long-offline proxies to dead after a configurable threshold.
- **Top proxy tracking** — maintain a ranked list of top earners (top 10/25/100 by contracts/bandwidth) to inform which proxies are most valuable to protect from accidental removal.
- **Live binary updates** — feasibility partially established via `provideSecretKeys` persistence; platform matchmaking is the unrecoverable part. Substantial separate project.
