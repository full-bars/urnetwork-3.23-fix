# 📜 URnetwork Provider — Log Message Reference

A plain-language guide to every log line you'll regularly see running urnetwork providers, whether binary or Docker. Examples are drawn from real production deployments.

---

## 📊 System Auditor & Smart Auto

```
[audit] Running system checks...
[audit] Conntrack Max: 65536 (Suboptimal! Target: 2097152)
[audit] Hint: System is not optimized for high volume. Run 'urnet-tools optimize' to fix.
[audit] Disk write speed: 22.4 MB/s (1024MB sync test)
[audit] Auto-enabling RAM logs due to slow disk I/O.
[tune] auto-profile: detected 1969 MiB RAM; applying 'Balanced' settings
```

Fires **once per process** at startup when `URNETWORK_PROFILE=auto` is set, regardless of how many proxy servers are loaded. (Prior to fix.15, this line fired once per proxy server, producing thousands of identical lines on large proxy lists.)

| Message | Meaning |
|---|---|
| `[audit] Suboptimal...` | The host OS has low limits (default ulimit or conntrack). This will throttle connections under heavy load. |
| `[audit] Disk write speed...` | Result of the 1GB cache-busting stress test. |
| `[audit] Auto-enabling RAM logs...` | The provider decided your disk is too slow and moved logs to `/dev/shm` to protect network performance. |
| `[tune] auto-profile...` | Confirms which performance tier (Low/Balanced/Perf/Extreme) was selected based on detected RAM. |

---

## 🚀 Provider Startup

```
❤️ [startup] provider version=v3.23.0-fix.24.34
```

Emitted exactly **once per provider process**, early in the startup sequence before any proxy work begins. This line confirms the exact binary version that is running.

| Field | Meaning |
|---|---|
| `version` | The provider binary version (from `-ldflags -X main.Version=...`). In Docker this matches the image tag; after `urnet-tools update` it reflects the updated binary. |

> [!NOTE]
> Docker deployments also log `[INFO] Running UrNetwork build v...` from the startup script; the `[startup]` line makes the same information available in bare-metal/binary installs and is written to RAMLOGS when enabled.

The existing `client_id` and `instance_id` lines are printed separately, once per proxy, and are unchanged by this log line.

### Launch ranking

```
💰 [startup] earnings ranking: 588 of 812 proxies have earnings history, 34 URL-sourced promoted to launch with the file list, top earner 203.0.113.9:1080 at 4.1 GB
💰 [startup] earnings ranking: no history yet, launching by warmth and source only
```

Emitted once per process, after the proxy list is resolved. The provider now
keeps a per-proxy record of billable traffic that survives restarts, and this
line reports what that record currently holds.

| Field | Meaning |
|---|---|
| `N of M proxies have earnings history` | How many of this launch set the node has ever observed earning. |
| `top earner` | The highest-scoring proxy and its score. |

The score is billable bytes with a one-week half-life, persisted to
`~/.urnetwork/proxy_earnings.json` and written at most every 15 minutes. A
proxy that stops earning decays out of the record on its own, so it reflects
what earns now rather than what earned once. The file is capped at 20,000
entries, with the lowest scorers evicted first.

Launches are ordered by three rules, in this order:

1. **Warmth.** A proxy holding a valid client JWT dials before one that must
   mint a fresh identity. Warmth comes first deliberately: minting is
   rate-limited, so a rich but cold proxy jumping the queue would spend a
   scarce mint slot and stall warm identities that could have dialled
   straight through. A warm URL-sourced proxy therefore launches before a
   cold file proxy.
2. **Trusted provenance.** Within a warmth tier, file-sourced and internal
   proxies launch first. A URL-sourced proxy joins them once its earnings
   score passes 64 MiB, at which point it is a known earner rather than an
   unproven address off a public list. That is the `promoted` count.
3. **Earnings.** Within one tier and group, the bigger earner dials first.

Among cold proxies, one unproven URL-sourced proxy is interleaved after
every five trusted cold proxies, so unproven addresses still get tried and
can build a history instead of waiting behind the whole cold list.

> [!NOTE]
> A node needs roughly a week of uptime before the record means much. A
> fresh node orders launches by warmth and source only and reports
> `no history yet`. That is expected on a first run and until the provider
> has observed billable traffic. It is not an error.

---

## ⚙️ Settings Changes (v31+)

Every change that reaches the provider is logged at the provider, so an
operator can confirm from the node's own log that a setting registered with
the daemon rather than trusting the CLI's exit code.

```
⚙️ [control] set profile=v8 (was unset)
⚙️ [control] cleared profile (was v4)
[metrics] started Prometheus /metrics on 192.200.0.5:9100
[metrics] stopped Prometheus /metrics
```

The failure paths log too, because a refused change is exactly when an
operator most needs the log to explain why their setting did not take:

```
❌ [control] set profile=nope rejected: unknown profile
❌ [control] set gogc=50 failed to persist, rolled back: no space left on device
⚠️ [control] set gomemlimit=2GiB (was 1GiB) persisted but live apply failed, takes effect on restart: ...
🔒 [control] rejected connection: peer uid does not own this provider
```

| Line | Meaning |
|---|---|
| `set <key>=<value> (was <old>)` | Applied and persisted. `unset` as the old value means the key had no prior setting. |
| `cleared <key> (was <old>)` | Key removed, default restored. |
| `rejected` | Validation refused the value. Nothing changed. |
| `failed to persist, rolled back` | The write failed and the in-memory value was reverted, so the log and the daemon agree. |
| `persisted but live apply failed` | Stored, but the running process could not adopt it. It takes effect on the next restart. |
| `🔒 rejected connection` | A peer that does not own this provider tried to use the control socket. |

> [!NOTE]
> Reads are deliberately silent. `urnet-tools status` polls `get` on every
> invocation, so logging reads would bury the writes that matter.

---

## 🧠 Adaptive GC Governor (pressure monitor)

```text
[proxy][pressure] gcGovernor armed (baseline GOGC=100)
[proxy][pressure] gcGovernor tighten_gogc50_heap0.72 (heap=0.72 go=50)
[proxy][pressure] gcGovernor hard_gogc25_heap0.82 (heap=0.82 go=25)
[proxy][pressure] gcGovernor critical_gogc10_heap0.94 (heap=0.94 go=10)
```

The consolidated adaptive GC governor lives in the pressure monitor and is the single writer to the Go GC percentage knob for the whole process. It is fed by both the process heap fraction and host available RAM, and it takes the tighter of the two. It applies to every profile and is on by default. Operators can disable it with `URNETWORK_ADAPTIVE_GC=0`.

| Message | Meaning |
|---|---|
| `gcGovernor armed` | Startup message that shows the captured baseline GOGC. |
| `gcGovernor tighten` | Governor lowered GOGC to `min(baseline, 50)` (heap >= 0.70). |
| `gcGovernor hard` | Governor lowered GOGC to `min(baseline, 25)` (heap >= 0.80 or host RAM <= 300 MiB). |
| `gcGovernor critical` | Governor lowered GOGC to `min(baseline, 10)` and called `FreeOSMemory` (heap >= 0.92 or host RAM <= 150 MiB). |

The former `[eco]` memory monitor lines are retired. Their host available RAM signal now flows through this governor, so small memory-fragile boxes keep the same protection.

---

## 🎚️ Capacity Control: Trim, OOM Cap, Memory Headroom

Every line here is mirrored to `/dev/shm/urnetwork-important.log` and to `~/.urnetwork/events.log` (1 MB, one rotation), so they survive a reboot and a full RAM buffer.

```text
[proxy][trim] received: cap=2000 (was none); 4127 running, 4127 desired, applying
[proxy][trim] received: cap cleared (was 2000); pool may regrow toward 4127 desired
[proxy][trim] applied: cap=2000: shed 2127 worst-graded running, held 0 additions (pool ~2000)
[proxy][trim] startup: cap=2000, launching 2000 of 4127 desired, holding 2127 worst-graded until the cap is raised
[proxy][trim] warn: proxy_trim holds "abc", which is not a proxy count, so no operator cap applies; set one with `urnet-tools proxy trim <count>`
[proxy][trim] warn: cannot read proxy_trim (read ...: permission denied); ignoring the operator cap, the automatic cap still applies
[oomcap] shadow: OOM kill since the last start (peak running 4127): would reduce the automatic start cap 0 -> 3301 (not enforced; set URNETWORK_OOM_CAP=on to enforce)
[oomcap] applied: OOM kill since the last start (peak running 4127): reduce the automatic start cap 0 -> 3301
[proxy][resources] effective RAM ceiling 1024 MiB (cgroup v2 memory.max at /system.slice/urnetwork.service)
[proxy][resources] warning: GOMEMLIMIT=400 MiB is below the ~945 MiB of heap this pool of 1300 proxies is expected to need, ...
[proxy][resources] warning: this pool of 2001 proxies is expected to need about 1894 MiB of memory but this box gives the provider about 1930 MiB (1630 MiB left after holding back 300 MiB for the OS and other tenants), so expect swapping and stalls. ...
[proxy][resources] low memory headroom: 132 MiB available, below 193 MiB (2001 proxies, 77780 goroutines); ...
[proxy][resources] memory headroom recovered: 420 MiB available (2001 proxies, 41000 goroutines)
[baseline] cannot write baseline.jsonl: open ...: no space left on device; recording is off until this clears
```

| Message | Meaning |
|---|---|
| `[proxy][trim] received` | An operator trim cap (or the automatic OOM cap) was seen for the first time or changed. Logged once per change with what it replaces and the running and desired counts. `received: cap cleared` is logged when the cap is removed. |
| `[proxy][trim] applied` | The result of applying the cap: how many worst-graded running proxies were shed and how many additions were held back. Logged even when nothing needed shedding. Shed proxies keep their state (ID, health, grade) so they relaunch as themselves when the cap is raised. |
| `[proxy][trim] startup` | The cap is applied before launching, so a restart never opens every desired proxy and then sheds down. Held proxies stay desired and are admitted by the next reload once the cap allows it. |
| `[proxy][trim] warn` | The operator's `proxy_trim` could not be used: either it holds something that is not a proxy count, or the file itself could not be read (permissions, a directory where the file belongs, a broken disk). The operator cap is then ignored, the automatic OOM cap still applies, and the reason is logged once rather than on every reload. |
| `[oomcap] shadow` / `applied` | The OOM-aware start cap. When a process in this provider's own cgroup subtree was OOM-killed since the previous start (same boot, higher `oom_kill` counter in the parent cgroup's `memory.events`, which is hierarchical and, unlike the service's own cgroup, survives a restart; a reboot is never blamed, and another workload's kill elsewhere on the host is not counted). The counter covers the PARENT cgroup's subtree, which for a system unit is `/system.slice` and therefore includes other services and, with the systemd Docker driver, other containers on the same box; a kill in a sibling container can be counted here. Where no cgroup v2 `memory.events` can be read (cgroup v1, no cgroup), the host-wide `/proc/vmstat` counter is used instead, as before, so any workload's kill can count there, the next start runs 80% of the peak running proxies, never below max(50, desired/4), at most 3 reductions per 24h. The cap relaxes 10% for each clean 24h window since the last change, evaluated at START, so a long-running process keeps its cap until the next restart; a marker not seen running in the last 72h is never blamed (a heartbeat refreshes hourly while the process is up, so a long-running provider stays blamable; the counter may belong to other workloads once the process has actually been gone that long). `shadow` (default) only logs what it would do. **A Docker restart resets the counter**: with a private cgroup namespace the container gets a new subtree, so the first start after a container recreation sees a lower counter and never claims a kill. |
| `[oomcap] cleared the automatic start cap` | The kill switch went off (or a start found it off), so the standing automatic cap is forgotten, not merely left unenforced. Without this, a cap set days earlier was still on disk and bound again the moment the switch went back on. The reduction history is kept. |
| `[oomcap] mode on: enforcing cap N, this sheds about M of R running proxies` | A live `urnet-tools set oom-cap on` says what it will cost: the cap now in force and how many running proxies the next reload would drop. A mode switch that silently sheds a third of the pool reads as a mystery a day later. |
| `[proxy][resources] effective RAM ceiling` | Once at startup: the RAM ceiling the provider tunes itself against (tier selection, soft memory limit, GC governor, headroom threshold, hot-swap gate) and which limit set it: the tightest cgroup `memory.max` or `memory.high` on the unit or an ancestor slice, a cgroup v1 limit, or the host's total RAM when nothing limits it. `memory.high` is a soft throttle below `memory.max`, so a unit that sets both reads the lower number. If a box tunes smaller than expected after an upgrade, this line says why. |
| `[proxy][resources] warning` | At startup the limits this process runs under are short for the pool. Two separate checks, because the two numbers answer different questions. **Heap:** the finite `GOMEMLIMIT` is compared against the Go heap the pool needs (100 MiB plus about 0.65 MiB per proxy, sampled from running nodes after days of uptime; the average client share is already in that figure). This is logged after the tier and profile limits are applied, so it reports the limit actually in force, not one the tier code was about to replace. **Box:** the whole-process footprint (100 MiB plus about 0.9 MiB per proxy, RSS plus swap) is compared against the RAM the box gives the provider, minus a reserve of the larger of 300 MiB and 10% for the OS and other tenants. The two checks are separate on purpose: a pool can sit comfortably under its heap limit while already thrashing physical RAM. The footprint estimate includes swap while a cgroup `memory.max` does not, so a cgroup-capped box is the least validated case. The provider never changes these limits; it says what to change. |
| `low memory headroom` | Host or cgroup available memory stayed below max(150 MiB, 10% of RAM), at most 400 MiB, for two consecutive 30s samples. Connected clients drive memory (about 0.8 MiB each), so this can happen with no change in the proxy count. `recovered` is logged after four consecutive samples 25% above the line. Observation only. |
| `[baseline] cannot write` | The baseline recorder (`~/.urnetwork/baseline.jsonl`, see [Configuration](docs/Configuration.md)) could not append a sample: a full or read-only disk, or no state directory. At most one line per hour, so a full disk does not log once per sample. Recording resumes by itself when the write succeeds. Nothing else changes: the recorder never affects the proxy pool. |

Every automatic or operator capacity decision is also appended to `~/.urnetwork/autopilot.jsonl` (one JSON object per line: time, actor, action, from, to, mode, reason; capped at 256 KiB, newest lines kept). Read it with `urnet-tools autopilot log [limit]` (shadow decisions show as `[shadow]`) or the control socket's `ledger` command. That file is the audit trail for a self-managing provider: if it is ever empty or zero bytes, the ledger was truncated rather than cleared, and that is a bug.

> [!NOTE]
> The kill switch is `urnet-tools set oom-cap on|off|shadow`. Any source saying `off` wins, including an `off` set here against `URNETWORK_OOM_CAP=on` in the unit.

---

## 🏊 Buffer Pool Health

```
pool[2048] tag=0 [] r=1616413/t=1617695/c=20087 = 99.92% return / 98.76% reuse
```

Fires every 60 seconds. This is the provider's internal memory health check.

| Field | Meaning |
|---|---|
| `pool[2048]` | Buffer size in bytes. The provider pools fixed-size byte slices to avoid constant GC pressure. |
| `tag=0 []` | Internal tag used to categorize allocations. Usually `0` with an empty caller name in production. |
| `r=` | **Returned** — total buffers handed back to the pool (cumulative lifetime count). |
| `t=` | **Taken** — total buffers checked out from the pool (cumulative lifetime count). |
| `c=` | **Created** — how many times `Get()` found the pool empty and had to allocate a fresh buffer instead of reusing one. |
| `return %` | `r / t` — what fraction of taken buffers came back. Should be ~100%. A leak shows here. |
| `reuse %` | `(t - c) / t` — what fraction of checkouts found an existing buffer ready in the pool. High is good. |

Every dump ends with one **summary line** that says whether any of the above is a problem:

```
pool summary: 28 tags checked, all clear, holding 31.2 MiB in 8412 buffers
pool summary: 28 tags checked, 1 possible leak: pool[4096] tag=9 [ip.go:100/ip.go:200] outstanding 1000 -> 1900 over 10 dumps and still rising, holding 31.2 MiB in 8412 buffers
```

It judges the **trend of outstanding buffers** (`t - r`) over the last 10 dumps, not the return percentage. Buffers in flight when the dump runs count as taken but not yet returned, so a healthy tag can read 34% right after a restart and climb toward 100% by itself; a bounded set held in a queue is a flat outstanding count, not a leak. A possible leak is outstanding growing steadily and not levelling off. A plateau, a ramp that flattens, and jitter are not flagged. It also flags `low reuse` (under 50% reuse with at least 10,000 takes). Until 10 dumps exist the line says `trend needs 10 dumps (have N)` instead of judging. Problems log as a warning naming the pool, tag and call site.

**What to watch for:**
- The `pool summary` line saying `possible leak` — outstanding buffers growing steadily for 10 dumps
- `return %` on its own is not a leak signal: read the outstanding count (`t - r`) over several dumps
- `reuse %` below 95% — the pool is undersized for the load; GC pressure is higher than ideal
- `c=` growing rapidly between checks — pool is being depleted under load

**Examples from the fleet:**
- Detroit test server (1000 proxies, early): `c=320`, `99.99% reuse` — pool nearly perfectly sized
- Production server (long-running): `c=20087`, `98.76% reuse` — higher allocation pressure, still healthy
- Another production server: `c=7195`, `99.28% reuse` — moderate, normal for busy deployments

---

## 🌐 URL-Sourced Proxies (`[proxy][url]`)

A URL fetch cycle prints detail lines, then **one line per source** saying what that source contributed, then **one headline** for the whole cycle. They are always printed, including when nothing changed, and are kept in the important buffer.

```
📥 [proxy][url] source lists.example.com/proxies/http.txt: +8 new of 42 listed (30 already known, 2 rejected, 2 dead)
📥 [proxy][url] source other.example.org/raw: nothing new of 60 listed (60 already known, 0 rejected, 0 dead)
📥 [proxy][url] source broken.example.net/list: fetch failed
➕ [proxy][url] cycle: +8 new to the pool from 2 of 3 sources, 1 failed (90 already known, 4 rejected, 2 held for re-probe); pool now 340 qualified of 371 cached
✔️ [proxy][url] cycle: nothing new from 2 sources (60 already known, 0 rejected); pool 340 qualified of 371 cached
⚠️ [proxy][url] cycle: every source failed (2 of 2); pool unchanged at 340 qualified of 371 cached
```

A source is labeled by host and path only. The query string and any userinfo (`user:password@`) are never logged in these lines, since source URLs often carry an API token there. A path segment that looks like a credential (the segment after `/token/` or `/key/`, a long opaque blob, or a JWT-like dotted token) is shown as `[redacted]`, including when the secret contains an encoded slash (`%2F`); a filename such as `http.txt` is kept. A short token in an ordinary path segment cannot be recognized and would be shown, so keep tokens in the query string where you can. A URL that cannot be parsed is shown by its host alone, and a URL whose credentials may contain an unescaped slash is shown as `[unparseable source]`. Two sources that would share a label are numbered (`#2`).

| Field | Meaning |
|---|---|
| `+N new` | New proxies from this source that qualified and were admitted to the pool. |
| `of N listed` | Parseable proxy lines the source returned. |
| `already known` | Addresses the cache already had, or that another source listed earlier in the same cycle (a proxy listed by two sources is credited to the first). |
| `rejected` | New addresses that were not admitted: probed below the bar, or socks5-only. |
| `dead` | New addresses that failed the probe outright and were dropped. |
| `held for re-probe` | Rejected entries cached anyway, so the reaper can retry them. |
| `pool ... qualified of ... cached` | The URL pool after the cycle: qualified proxies, and all cached entries. |

The headline is the sum of the sources (its `rejected` includes their dead).

When a reload actually starts URL-sourced proxies it says so on its own line, and the routine summary attributes each addition to its source:

```
🚀 [proxy][url] launching 7 of 12 new URL-sourced proxies (5 held until the file proxies finish warming up)
🔄 [proxy] reloaded: +14 added (url 12, file 2), -0 removed [3s]
```

Unproven URL proxies wait until the file proxies finish warming up, so `launching 7 of 12` means 5 are still queued, not lost.

---

## 🚫 Transport Auth Error

```
[t]auth error 019e2d83-3118-5186-995f-aabe3b2dcf0b = Timeout. (34 suppressed)
```

The provider failed to authenticate a transport connection to the URnetwork platform. Each transport ID (the UUID) represents one proxy or connection attempt.

- The error is usually `Timeout.` — the platform didn't respond in time
- `(N suppressed)` tells you how many additional transports also failed since the last log line was emitted. The rate limiter allows at most one log per minute globally across all transports.
- Without the suppressed count, the first failure of a new session logs cleanly: `[t]auth error <id> = Timeout.`
- This is normal during platform outages or high load. The provider retries automatically.
- Seeing this occasionally is expected. Seeing it continuously for many minutes indicates a platform-side issue.


---

## 🔌 H3 (QUIC) Beside H1 (beta, `URNETWORK_H3` or the `h3` key)

Only a box that runs H3 logs these, and only for the direct identity. H3 is off by default. It starts when `URNETWORK_H3` is set at startup, or when the `h3` control key is switched on live with `urnet-tools set h3 on`.

```
[t]h3 eligible for the direct identity, currently on (urnet-tools set h3 on|off or URNETWORK_H3): H3 runs beside H1 and falls back to H1 quietly
[c]h3 connect to 203.0.113.10:443 (api.example.net)
[t]h3 unavailable, staying on h1 (retrying quietly): <error>
⚙️ [control] applied h3=on (was off)
```

- `[t]h3 eligible for the direct identity, currently <on|off>` is logged once for a direct identity that is allowed to run H3. It says whether H3 is currently on, and names the two ways to change it.
- `[c]h3 connect to <address> (<server>)` is one H3 connection attempt to the platform. It is expected at start and on each retry.
- `[t]h3 unavailable, staying on h1 (retrying quietly)` appears once per transport when H3 cannot connect, for example because UDP is filtered. It is not an error: H1 carries the node, the failure is not counted as a backend or proxy auth failure, and H3 retries with a backoff that doubles up to 10 minutes. The same failure is logged again only at verbose level 2 as `[t]h3 unavailable: <error>`.
- `[c]h3 connect err = <error>` is the transport-level detail behind a failed attempt.
- `⚙️ [control] applied h3=<on|off>` is the control-socket confirmation when the `h3` key changes. The datagram keys log `applied h3_datagram=...` and `applied h3_datagram_send=...`. A change of any of the three is not counted as an H3 drop.
- What to watch while testing it: the `[t]auth error` rate and the backend-degraded state must not rise, and billable per hour must not fall against a box without it. Any of those is a reason to turn it off (`urnet-tools set h3 off` or unset `URNETWORK_H3` and restart).

### What H3 adds to the `[health]` line

Once H3 has been attempted, the health heartbeat line gains these fields. A box that never enables H3 sees none of them.

```
[health] ... h3_up=1 h3_tx_share=42% h3_drops=0 h3_conn_fail=1
```

- `h3_up` is 1 while an H3 connection is up, 0 otherwise.
- `h3_tx_share` is the share of the direct identity's outbound payload frames that H3 carried. `n/a` means no frames were sent yet.
- `h3_drops` counts H3 connections that ended while the transport was still wanted. Switching `h3` is not one.
- `h3_conn_fail` counts H3 connect attempts that failed.

With `h3_datagram` on and a server that accepted the offer, the line also carries `h3_dg=accepted/offered dg_rx=<n> dg_rx_drop=<n>`. With `h3_datagram_send` on, it adds `dg_tx`, `dg_tx_stream`, `dg_tx_err` and `dg_blackhole`.

---

## 📡 STUN Success Aggregate

The provider reports the result of its STUN probe as one low-noise line. The probe follows the active ICE settings and adds a probe-only Cloudflare endpoint, so a filtered or broken STUN path shows up without turning on verbose logging.

```
📡 [stun] ok=12 fail=0 | google: v4=ok v6=ok · meteredca: v4=ok v6=fail
```

- `ok` and `fail` count the probe results in the current window. The line is written on a low-volume interval and moves to a shorter interval when activity is high, so a busy host reports more often and a quiet one stays quiet.
- The part after `|` lists each STUN provider and the result for the IPv4 and IPv6 families. `ok` means the binding request was answered, `fail` means it was not, and a family the host cannot reach is reported as such.
- The first line of a run measures from the first pulse, not from process start, so a long idle period is not read as a burst of failures.

---

## 🧭 Smart Dialer and Give-up Lines

```text
[smart-dialer][proxy] measured connect cost of 4 transports
[proxy][auth] proxy[12] (203.0.113.7:1080) attempts=4 admit_wait=1.2s attempt=15.0s cut_short=true err=context deadline exceeded
[proxy][auth] direct attempts=4 admit_wait=0.0s attempt=1.8s cut_short=false err=tls: handshake failure
```

| Message | Meaning |
|---|---|
| `[smart-dialer][proxy]` / `[smart-dialer][direct]` `measured connect cost of N transports` | A background probe round measured N transports for a proxy's client strategy (`proxy`) or the provider's own connection (`direct`). The first round runs 20 to 80 seconds after start, then a check runs every 5 minutes. A round that attempted nothing logs nothing, so silence is normal: everything is already measured, the smart dialer is off (the default), or custom extenders are configured. Probes are connect-only, never carry a request, and a failed probe never demotes a transport that has real successes. |
| `[proxy][auth] ... attempts=` | One line per auth give-up. `attempts` counts the ladder's attempts, genuine failures and slow ones together. `admit_wait` is the time spent waiting for an admission slot and `attempt` is how long the last attempt ran, so latency can be told apart from a refusal. `err` is the raw error. `direct` is the provider's own non-proxy connection. |
| `cut_short=true` | The last attempt was cut off by the connect deadline: the proxy is slow, not refused. A slow give-up does not count toward URL eviction or the 14-day drop and does not move the shared auth rate limiter, so latency cannot spiral into a failure cascade. `cut_short=false` is a genuine failure. |

> [!NOTE]
> Turn the smart dialer on or off with `urnet-tools smart-dialer [status|on|off]`. It is live, persisted and off by default. With it off, transport scoring and ordering are exactly what they were before it existed.

---

## ⏳ OOB Contract Backoff

```
[contract]oob err = Timeout.; backing off create contract OOB requests for 1m0s
```

The provider tried to request a contract via the out-of-band (OOB) control channel and got a timeout. It will stop sending OOB contract requests for 60 seconds before retrying.

- Fires at most once per minute (rate-limited)
- Sustained appearances over many minutes = platform OOB service degraded
- Does not affect already-established sessions, only new contract negotiations
- The provider continues running and retrying throughout

---

## 🚪 Session Exit — Could Not Create Contract

```
[s]019e0f4d-b48e-45e3-33e6-d7228666f41e->[]...019e2f50-4c42-571c-6adb-5c9a990d99e9 s(00000000-0000-0000-0000-000000000000) exit could not create contract.
```

A session between two clients failed because no contract could be allocated. The format is:

```
[s]<source-client-id>->[]...<destination-client-id> s(<contract-id>) exit <reason>
```

- `s(00000000-...)` — the nil contract ID means no contract was ever assigned
- This fires when traffic is being attempted but the platform can't issue contracts (OOB down, rate limited, etc.)
- Seeing these during an OOB backoff period is expected — they're proof that clients are trying to use this provider
- The session will retry

---

## ⚠️ Debit Contract Near Capacity

```
[s]debit contract 019e2c16-80c4-ef1d-edc7-47d788752706 failed +1420->13750 (12330/13107 total 94.1% full)
```

A contract was allocated and is filling up. The provider tried to debit bytes from it but it's near its limit.

- `+1420->13750` — tried to debit 1420 bytes, bringing the total to 13750
- `12330/13107 total 94.1% full` — the contract has used 94.1% of its byte allowance
- When a contract fills up a new one is negotiated automatically
- This line being present means data is actually flowing through the provider — it's a sign of real traffic

---

## 🚨 Outage Watcher

```
[outage] watcher active node=my-server (docker) webhook=configured
[outage] backend degraded
[outage] backend recovered
```

Monitors backend connectivity. It is designed to be conservative to avoid false alarms.

| Message | Meaning |
|---|---|
| `watcher active` | Confirms the background monitor is running and identifies the node. |
| `backend degraded` | The provider has failed several consecutive connection attempts to the platform. New connections are likely to fail. |
| `backend recovered` | Connectivity has been restored. The provider will resume normal operations. |

> [!NOTE]
> An outage is only declared after **5 minutes** of continuous failure. Alerts via webhook (if configured) fire on these transitions.

---

## 🗑️ Packet Drop Rate-Limiting

```
[r]drop: write error: connection reset by peer (1,420 suppressed)
```

The `[r]drop` message indicates the provider dropped a packet because it couldn't be delivered to the final destination (e.g., target website or proxy).

- These are **rate-limited to 1 per minute** globally to prevent log flooding during network instability.
- The `(N suppressed)` suffix shows how many other drops occurred since the last log line.
- High drop counts are normal during global outages or if a specific proxy server goes down.

---

## 🔒 Proxy Quality Control: MiTM, Signal Quality, and Continuous Re-grading

Every candidate proxy earns its place in the pool. Nothing is admitted on the strength of "it responded once".

### 1. Man-in-the-middle detection is a real TLS handshake, not a heuristic

`probeProxy` (`provider/proxy_probe.go:226`) runs three stages over a single tunnel:

1. **SOCKS5 greeting.** Is this actually a SOCKS5 proxy?
2. **SOCKS5 `CONNECT` to the API host on 443.** Can the proxy reach the backend at all?
3. **A real TLS handshake through that tunnel**, with `ServerName` pinned to the API host and verified against the **system root pool** (`proxyProbeTLSClientConfig`, `proxy_probe.go:222`).

Stage 3 is the MiTM check, and it is the reason a proxy cannot quietly intercept traffic. An interceptor answers `CONNECT` with `0x00` exactly like an honest proxy, so it passes stage 2. It then presents **its own certificate** at the TLS layer. Verification against the system roots fails, and the proxy is classified `probeTLSFailed`, so it is never admitted.

`probeTLSFailed` is deliberately kept distinct from `probeDead` and `probeSocks5Only` (see the comment at `proxy_table_probe.go:427`): the tunnel works and the proxy is alive, it simply will not relay TLS transparently. A dead proxy and a lying proxy are different faults and are counted differently.

A hostile proxy that was previously good is caught on the same path. The stale re-probe demotes a once-good proxy that has turned hostile, and consecutive TLS failures retire it from the cache entirely. Both behaviours are pinned by tests, including a regression test written specifically for the case where a good proxy turns hostile (`TestReview_ReaperStaleReprobeDemotesTLSFailed`, `TestReview_ReaperBlacklistsTLSFailedAfterThree`).

What you see in the log when this happens:

- `ProbeOK=false` recorded against the cached entry, so the address is not launched
- a demotion line on a stale re-probe, naming a TLS-verify failure
- a blacklist line once the consecutive-failure threshold is reached

### 2. The signal quality gate: can this proxy actually reach the popular web?

A proxy that passes MiTM checking can still be useless, so a second gate measures whether it can reach real, in-demand destinations.

- **127 popular hostnames**, all on port 443 (`provider/ip_probe_targets.go:29`)
- each is reached **through the proxy** with a SOCKS5 `CONNECT`, and counts only when the reply is `REP == 0x00`
- the score is `OK / Total` over the targets **actually attempted**, so a local DNS or routing failure on our side can never convict a working proxy
- a `Decidable` gate separates "we could not measure" from "we measured and it was bad"
- **pass bar 0.6**: anything below it is never spawned. The **preferred bar of 0.9 is recorded and clamped but gates nothing** in this tree; the only bar that decides admission is the pass bar.
- **Sample width 12 by default**, growing to a maximum of 36, and the growth happens **only for borderline candidates** (a score within 0.15 of the pass bar). A clearly good or clearly dead proxy is settled at the small width, so probe bandwidth is spent in proportion to uncertainty rather than on every proxy. `min_sample_width` defaults to 0, so the start-small staging path is off unless an operator sets it.
- Successive sweeps use a **disjoint-block rotation**, so consecutive passes at the same width land on non-overlapping strides and genuinely new hosts rather than re-dialing the same ones

The two stages are separate jobs. **Stage 0** is liveness: the three-stage check above decides only whether the proxy is alive and honest, and says nothing about quality. **Stage 1** is the table probe above, and it is what produces the A to F grade that decides admission order.

### 3. Grades are continuously refreshed, so the pool converges on the best of the best

Nothing is graded once and trusted forever.

- **The fetch cycle probes new addresses only.** Re-probing everything every cycle would be both slow and a suspicious traffic pattern, and would be especially bad on a large box.
- **The URL reaper** ticks every 5 minutes, works to a stale window that scales from 3 hours down to 1 hour under memory pressure, and spends a budget of 32 grade refreshes per cycle, oldest first. Under pressure the window shortens, so refreshes happen more often.
- **The paid and file grader** runs on a wider window, 6 hours down to 3 under pressure, skips proxies that earned recently, and force-probes anything not checked in 24 hours.
- **Below-bar entries are never spawned**, so a proxy that decays is not merely ignored, it stops carrying traffic.

The net effect is that a proxy holds its place only by continuing to pass. The A to F funnel admits the best first on every fill, and the re-grading keeps re-ordering the pool against reality rather than against a snapshot from days ago.

## 💓 Health Heartbeat

```
[health][build] v3.23.0-fix.31.8 profile=auto host=node-a
[health] uptime=15m0s profile=auto heap=80MiB sys=255MiB goroutines=2156 connections=998 proxies=1150
```

The `[health][build]` line leads every heartbeat tick. It exists so a log tail
or a pasted excerpt identifies the build that produced it: the startup banner
scrolls out of a RAM log window, and every other line in the block is only
interpretable once you know the version. `host` is the node name
(`URNETWORK_NODE_NAME`, the `~/.urnetwork/node_name` override, `HOST_HOSTNAME`,
then the kernel hostname).

Fires every 5 minutes (default). Provides passive liveness confirmation and resource utilization trends.

| Field | Meaning |
|---|---|
| `uptime` | How long the provider process has been running. |
| `profile` | The active performance profile (e.g., `auto`, `turbo-v4`, `lowmem`). |
| `heap` | RAM currently used by live Go objects. |
| `sys` | Total RAM reserved from the OS (includes stack, heap, and unused reservations). |
| `goroutines` | Number of live goroutines. Useful for spotting leaks or runaway growth (e.g., the self-wake loop fixed in v3.23.0-fix.24.33). |
| `connections` | Total number of **active end-user NAT sessions** (TCP/UDP) currently routing through the provider. |
| `proxies` | Total number of **authenticated, working proxy links** to the platform (how many proxies from your list are online). |

**What to watch for:**
- `connections` staying at 0 — the provider is running but no traffic is being routed (normal if `proxies` is also 0, otherwise indicates lack of users).
- `proxies` much lower than your `proxy.txt` count — indicates many proxies are failing auth or networking (check `[net][s]select` logs).
- `heap` growing continuously over hours/days — potential memory leak.
- `heap` vs `connections` — if heap grows while connections stay flat, memory is being consumed by something other than traffic (e.g. large proxy list storage).
- `goroutines` climbing steadily while load is flat — likely a goroutine leak (watch for repeated logs that should fire once per process, such as `[tune] auto-profile`).

### 🧠 Message-Pool Health

One line per heartbeat tick, written to be actionable without knowing what a
message pool is. It leads with a verdict so nothing has to be compared by eye:

```
[health][pool] ok — 117 buffers in use, none stuck, 2048 allocated since start (99.65% returned)
[health][pool] warming — 117 buffers in use, 2048 allocated since start (99.65% returned). Stuck-buffer check needs 1h of uptime (41m to go).
[health][pool] watch — 96 buffers were taken and never given back, up from 38 an hour ago. If this keeps climbing, memory use grows until the provider restarts. 1204 in use now, 4096 allocated since start (99.71% returned).
[health][pool] leak — 512 buffers taken and never given back, climbing for 35+ minutes. Memory will keep growing until restart. This is a bug worth reporting with this line. 4291 in use now, 8192 allocated since start (99.71% returned).
```

| Verdict | Meaning |
|---|---|
| `warming` | Less than an hour of uptime, so there is no trustworthy baseline yet. No judgement is made. |
| `ok` | The pool drains to the same baseline every hour. Buffers are being recycled. |
| `watch` | The baseline has climbed for 15 minutes without interruption, or the pool is still allocating new buffers an hour after startup. |
| `leak` | The baseline has climbed for 30 minutes without interruption. Memory grows until restart. Report it with the line. |

**Why the percentage is context and not the signal.** `returned` is
self-normalizing: a leak proportional to throughput keeps it pinned near 100%
forever, because the denominator grows exactly as fast as the leak. Losing one
buffer per 10,000 taken reads 99.99% while shedding roughly 1,440 buffers a
day. The number the verdict actually watches is the *stuck* count, the lowest
in-use count over the trailing hour. Buffers legitimately in flight come and
go, so a healthy pool touches a low number at least once an hour; a buffer
taken and never given back raises that minimum permanently.

The cost is latency. Pre-leak samples must age out of the hour-long window
before the minimum can move, so a leak present from boot is called about 90
minutes in. That is deliberate, because a faster signal fires on ordinary load
steps.

> [!NOTE]
> This line is independent of `debugTags` in `message_pool.go`, which is a
> compile-time `false` and gates per-allocation tagging. The heartbeat reads
> `MessagePoolSummary()`, which works in production builds and does no
> hot-path work.

### 💀 Dead-Proxy Health Report

In addition to the main `[health]` line, when running with a proxy list the provider emits proxy health lines:

```
[health][proxies] up=1193 down=7 dead=4 degraded=3 recovered=5 lost=0 lifetime_recovered=51 lifetime_lost=39
[health][proxies] dead: proxy[112] (45.3.32.184:1081), proxy[266] (104.207.45.110:1081), ... (+2 more)
[health][proxies] degraded: proxy[49] (209.50.167.49:1081), proxy[1037] (209.50.169.110:1081), proxy[660] (98.76.54.32:1081)
```

| Field | Meaning |
|---|---|
| `up` / `down` | Current proxy state (`up` agrees with `proxies=N`). |
| `dead` | Proxies that have never successfully authenticated (trustworthy after ~1h). |
| `degraded` | Proxies that worked before but are currently down. |
| `recovered` / `lost` | Down->up and up->down transitions since the last heartbeat. |
| `lifetime_recovered` / `lifetime_lost` | Cumulative transition counts since process start. |

- The detail lines are capped at 50 entries in stdout (shows `... (+N more)` when truncated).
- A complete, uncapped history is mirrored to `proxy_health.state` and `proxy_health.log` (default `~/.urnetwork`).
- A real-time bandwidth and concurrent session load tracker is mirrored to `proxy_traffic.state` (default `~/.urnetwork`).

### `proxy_health.log` row format

`proxy_health.log` receives one append-line per proxy state transition (complete, uncapped; rotated to `proxy_health.log.1` at 20 MB, one generation kept):

```
| 2026-08-04T00:12:03Z | RECOVERED | proxy[47]  | 1.2.3.4:8080     | after=3m12s |
| 2026-08-04T00:12:03Z | DEGRADED  | proxy[49]  | 5.6.7.8:1081     |             |
| 2026-08-04T00:12:03Z | DEAD      | proxy[112] | 45.3.32.184:1081 |             |
```

| Field | Meaning |
|---|---|
| Timestamp | RFC3339 UTC. |
| `RECOVERED` | A proxy that was down came back up. `after=` shows how long it was down (only when a `downSince` was recorded). |
| `DEGRADED` | A proxy that was up went down (worked before, now not). |
| `DEAD` | A proxy that never connected within a full pulse cycle. Emitted **once per proxy** (the `deadLogged` latch) prevents repeat rows for the same proxy. |

> [!IMPORTANT]
> `DEAD` rows were unreachable before the connecting-state bound shipped (the `!connecting` gate could never pass for a never-up proxy, so the path was latently dead). A fleet that has never seen `DEAD` rows will start seeing them for proxies that genuinely never connected within 65 minutes. This is a fixed latent bug; the rows are diagnostics only and nothing alerts on them.

### ⏱️ Hourly Pulse Marker

```
[pulse] waking stalled transports: down=12 dead=3 degraded=9 connecting=4
```

An hourly retry sweep is performed to wake stalled transports. This marker logs the pre-pulse state, so you can track how many of the `down` proxies are `recovered` in the next heartbeat.

| Field | Meaning |
|---|---|
| `down` | Sum of `dead` + `degraded` proxies. |
| `dead` | Proxies that have never successfully authenticated (trustworthy after ~1h). |
| `degraded` | Proxies that worked before but are currently down. |
| `connecting` | Proxies registered and still establishing their first WebSocket. A never-connected proxy counts as `connecting` only until its `connectingStaleAfter` window expires (65 minutes, one hourly pulse interval plus margin); past that it falls to `dead`. |

---

## 🔀 Outbound Connection Health (3.23-fix variant)

```
[net][s]select: proxy[42] (1.2.3.4:1081) [fragment] success=6086 error=192 clients=0
[net][s]select: proxy[13] (5.6.7.8:1081) [direct] success=2221 error=223
[net][s]select: direct success=171 error=3
```

Logged at INFO level in the 3.23-fix fork (promoted from debug level 2). Each line fires when the **provider itself** makes an outbound API call or WebSocket dial to the URnetwork platform (e.g. `api.bringyour.com/connect/control`) and records which route was used. This is the provider's own control-plane traffic, **not** end-user relay traffic.

> [!IMPORTANT]
> `success` and `clients` measure completely different things and do not correlate. A proxy with `success=5000 clients=0` is healthy and talking to the platform — it just has no users assigned to it right now. The platform decides which providers serve which clients.

| Field | Meaning |
|---|---|
| `proxy[N] (ip:port)` | The SOCKS5 proxy used to reach the platform. Absent when using the direct path. |
| `[fragment]` / `[reorder]` / `[fragment+reorder]` / `[direct]` | DPI bypass strategy used for this outbound call (see below). |
| `success=N` | Cumulative provider API/WebSocket calls that succeeded through this route since last reset. |
| `error=N` | Cumulative failures. A healthy error rate is under ~10% of successes. |
| `clients=N` | **Independent metric.** Number of end-user relay sessions currently routing through this proxy via LocalUserNat. Zero is normal when no users are assigned. |
| `age=Xs` | How long the oldest current user session has been continuously present on this proxy. Only shown when `clients > 0`. |

### Connection strategies

The provider tries multiple strategies for its outbound connections to avoid DPI and firewall interference. These are techniques applied to the TLS handshake, not to user traffic:

| Mode | Meaning |
|---|---|
| `direct` | Standard TLS with no modifications — the default path when no proxy is configured. |
| `fragment` | Splits the TLS ClientHello across multiple TCP segments so stateful DPI cannot read the SNI hostname. Highest priority; no throughput cost. |
| `reorder` | Sends TLS fragments out of order to confuse stateless DPI inspectors. |
| `fragment+reorder` | Both techniques combined. |

The selector tracks per-strategy success rates and prefers whichever is most reliable. When errors accumulate on one strategy, it rotates to the next.

**What to watch for:**
- `error` growing faster than `success` on a specific proxy — that proxy's outbound path to the platform is degraded. Consider removing it from `proxy.txt`.
- Repeated strategy rotations on the same proxy (log shows `[fragment]` → `[fragment+reorder]` → `[direct]` in quick succession) — the proxy has inconsistent connectivity to the platform.
- `clients=N` staying at 0 across all proxies for extended periods is normal when the platform hasn't assigned users to this provider. It is not related to `success` counts.

---

## 📡 Relay Traffic Rates

```
[traffic] total rx=2.3 MB/s tx=0.8 MB/s clients=1 active_proxies=2 billable_today=1.5 GB earning=yes
[traffic] proxy[124] (216.26.228.3:1081) rx=2.3 MB/s tx=0.8 MB/s clients=1 age=5m12s billable_today=1.2 GB
[traffic] proxy[230] (45.3.48.195:1081) rx=0.4 MB/s tx=0.1 MB/s clients=0 billable_today=340 MB
```

Fires on every health heartbeat tick (same cadence as `[health]`). Measures **actual end-user relay traffic** — bytes flowing through the provider's IP relay stack (LocalUserNat) on behalf of connected clients. This is what earns you platform credit.

| Field | Meaning |
|---|---|
| `rx` / `tx` | Bytes per second relayed since the previous heartbeat tick. |
| `clients` | Number of end-user relay sessions active on this proxy right now. |
| `age` | How long the current client session has been continuously present (shown when `clients > 0`). |
| `billable_today` | Cumulative billable bytes relayed through this proxy since midnight (local time). Resets at midnight. On the `total` line it is the fleet-wide sum. |
| `active_proxies` | How many proxies moved any bytes since the last tick (summary line only). |
| `earning` | `yes` if any billable bytes moved this tick, else `no` (summary line only) — a quick grep for "is this node earning at all". |

The per-proxy lines only appear for proxies that moved bytes since the last tick — proxies with zero traffic are omitted. The `total` summary line always appears so you have one line to grep even when nothing is flowing (`rx=0 B/s tx=0 B/s clients=0`).

> [!NOTE]
> `[traffic]` and `[net][s]select` measure different things. `[net][s]select success=N` is the provider's own API calls to the platform. `[traffic] rx=X` is actual bytes your provider relayed for end-users. Both can be high or low independently.

---

## 🔐 Post-Quantum Session Visibility (`[pqe]`)

```
🔐 [pqe] direct-e2e tunnels terminated: live pqe=10 classical=0 | opens since-start: pqe=10 clas=0 | 1h: pqe=10 clas=0 | 24h: pqe=50 clas=0 | 7d: pqe=50 clas=0
🔐 [pqe] all-time opens (persists across restarts): pqe=50 classical=0
```

Fires on the earning-windows tick when any PQE/classical session activity exists. Counts **per-peer end-to-end TLS tunnels this node personally terminates**, classified by negotiated key exchange (ML-KEM hybrids = `pqe`, anything else = `classical`). Modern peers almost always negotiate PQE, so `clas=` lines staying 0 is normal.

**Scope warning:** these are NOT client or earnings counters. Client traffic merely *forwarded* through this node (transit hops) never opens an e2e session here — transit visibility lives on `🛰️ [relay] as-hop:` and byte totals on `📈 [traffic]`.

| Field | Meaning |
|---|---|
| `live` | Tunnels currently in flight. |
| `since-start` | Opens since process start (the old "lifetime" — resets every restart). |
| `1h` / `24h` / `7d` | Sliding windows of open events. |
| `all-time` | Cumulative opens from `~/.urnetwork/lifetime_metrics.json` — survives restarts. |

---

## ♾️ All-Time Lifetime Metrics (`[lifetime]`)

```
♾️ [lifetime] all-time: pqe_opens=50 clas_opens=0 contracts_acquired=312 denied=41 proxies_recovered=51 lost=39 billable_total=842.1 GB
```

Emitted on the earning tick once any total is non-zero. Backed by `~/.urnetwork/lifetime_metrics.json` (atomic writes, 5-minute flush throttle, final flush at shutdown). Counters are fed reset-guarded deltas from the existing live counters: a source counter restarting contributes zero for that tick rather than corrupting history; worst case (power loss) loses at most one throttle window of deltas. A corrupt state file resets stats rather than blocking the provider.

| Field | Meaning |
|---|---|
| `pqe_opens` / `clas_opens` | All-time e2e tunnel opens by key-exchange family. |
| `contracts_acquired` / `denied` | All-time bandwidth-contract wins/losses. |
| `proxies_recovered` / `lost` | All-time proxy health transitions. |
| `billable_total` | Cumulative billable bytes ever relayed. |

---

## 🛰️ Transit Hop Visibility (`[relay]`)

```
🛰️ [relay] as-hop: clients=27 on 12 proxy(ies) rx=32.5 KB/s tx=8.1 KB/s (bytes we forward for others; tunnels we terminate: 🔐 [pqe], totals: 📈 [traffic])
```

Emitted on the earning tick only while transit traffic is actively flowing (silent when idle to avoid duplicating `[traffic]`). Reports what this node carries **as an intermediate hop**: client sessions routed through its proxies and their current rates. Distinct identities behind transit hops are unknowable by design (end-to-end encryption) — volume + serving proxies is the honest transit metric.


---

## 💰 Profit Heartbeat (3.23-fix)

```
[profit] earning=yes reason=- clients=4 rate=2.1 MB/s proxies_up=12 serving=3 idle=9
[profit] earning=no reason=idle clients=0 rate=0 B/s proxies_up=12 serving=0 idle=12
```

A fast, focused answer to **"are we earning right now, and if not, why?"**, emitted by `runProfitHeartbeat` every **15 seconds** — independent of the 5-minute `[health]`/`[traffic]` heartbeat. It uses `ProxyHealthSnapshot`, so it never disturbs the health heartbeat's dead/recovered baseline. It folds the headline earning signal into one greppable line so it survives even a tiny in-RAM log window.

| Field | Meaning |
|---|---|
| `earning` | `yes` if billable bytes moved in the last interval, else `no`. |
| `reason` | Why not earning (`-` while earning): `warmup` (still ramping up), `no_proxies` (none up), `idle` (proxies up but no clients matched), `no_traffic` (clients present but no billable bytes moved). |
| `clients` | End-user relay sessions active across all proxies right now. |
| `rate` | Aggregate billable throughput since the previous tick. |
| `proxies_up` | Proxies whose platform transport is currently live. |
| `serving` | Of those, how many are carrying at least one client. |
| `idle` | Up proxies carrying no clients (`proxies_up - serving`). |

To keep quiet periods from flooding the log, `earning=no` lines throttle to **once every 5 minutes** — except an `earning=no` line always fires **immediately on the `yes -> no` transition**, so the exact moment traffic stopped is visible. `earning=yes` lines print every tick.

---

## 📈 Earning Windows & Utilization (3.23-fix)

```
[earn] billable_1m=4.2 MB billable_5m=31 MB billable_15m=88 MB billable_60m=402 MB active=yes
[earn] proxies_up=12 serving=3 idle=9 clients=4
```

Two distinct `[earn]` lines surface **how much** and **how well** the node is earning:

**Rolling windows** (`billable_1m`/`5m`/`15m`/`60m`) — emitted per minute by `runEarningWindows`. Cumulative billable bytes over the trailing 1/5/15/60-minute windows, so you can see the trend at a glance. `active=yes` when any billable bytes moved in the last minute.

**Proxy utilization** (`proxies_up`/`serving`/`idle`/`clients`) — emitted on the 5-minute health tick. Shows how many up proxies are actually carrying users (`serving`) versus sitting `idle`. Sustained high `idle` with `proxies_up > 0` means the platform is **not assigning users** to this node — an earning signal distinct from `[traffic]` (bytes) and `[contract]` (assignments).

---

## 💾 Billable Rate Writer

```
[billable_rate] writer started (interval=10s)
[billable_rate] warn: write failed: open /root/.urnetwork/billable_rate: permission denied
[billable_rate] writer stopped
```

Persists the current billable transfer rate to `~/.urnetwork/billable_rate` on an interval so one-shot tools can read it without polling the live counters. This file is what `urnet-tools idle-update`'s threshold check reads. If you are debugging why `idle-update` never fires, this is the file (and these lines) to look at.

| Message | Meaning |
|---|---|
| `writer started (interval=...)` | The writer loop began; interval is the persistence cadence. |
| `warn: write failed: ...` | The rate file could not be written (permissions, disk). The rate itself is unaffected; the file is a mirror. |
| `writer stopped` | The writer loop exited (provider shutdown or context cancel). |

---

## 📑 Contract Lifecycle (3.23-fix)

```
[contract] acquired size=256 KiB destination=0142...c3a9
[contract] denied = insufficient allowance destination=0142...c3a9
[contract] closed acked=198 KiB allotted=256 KiB util=77% destination=0142...c3a9
```

A contract is the platform's bandwidth grant for relaying a client's traffic. These lines bracket a contract's life:

| Field | Meaning |
|---|---|
| `size` (acquired) | Bytes granted by this contract. |
| `denied` | The platform rejected a contract request; the message after `=` is the reason (e.g. `insufficient allowance`). A denied contract carries no `size`/`acked`. Frequent denials alongside low `util` means contracts are being refused, not just unused. |
| `acked` (closed) | Bytes actually acknowledged/relayed before the contract closed. |
| `allotted` (closed) | Bytes the contract granted (same basis as `size`). |
| `util` (closed) | `acked / allotted` as a percentage — actual revenue-generating usage, not just the grant. |
| `destination` | The client/destination the contract served. |

Low `util` across many `[contract] closed` lines means contracts are being acquired but barely used (clients connecting then leaving, or short transfers) — distinct from not acquiring contracts at all.

---

## ⏱️ TCP Write Timeout (transport stream)

```
[ts]019e28a3-76dd-1fd5-08a3-342775fdfa7b-> error = write tcp 172.17.0.2:58902->216.26.233.197:1081: i/o timeout
```

A TCP write to a proxy server timed out at the transport stream layer. This appears when network conditions are degraded (high latency, packet loss).

- `172.17.0.2` — the container's internal IP
- `216.26.233.197:1081` — the proxy server that stopped responding
- Followed shortly by a `[t]auth error` for the same transport ID
- Common during netem stress testing or real network degradation

---

## 😱 Startup — Proxy Auth Panic (handled)

```
W0516 trace.go:47] Unexpected error: {"error":"*errors.errorString=Timeout.","stack":[...,"main.provideAuth",...]}
```

During startup with a large proxy pool, many proxies attempt to authenticate simultaneously. Some time out and `provideAuth` panics with the timeout error. The `HandleError` wrapper catches the panic and logs it as JSON instead of crashing.

- This is benign — the proxy goroutine restarts and retries
- Expected on startup with 200+ proxies
- Goes away once the initial auth rush settles (usually within 2-3 minutes)
- Only the provider binary startup path triggers this, not the ongoing connection phase

---

## ℹ️ Startup — Provider Info

```
Provider e442be5 started
client_id: 019e2d67-5a52-b4f0-a00f-0bb97281dfe0
instance_id: 019e2d67-5a73-4bb3-6661-df9b5c595003
```

- `Provider <version>` — the git commit hash or version tag the binary was built from
- `client_id` — the provider's permanent identity on the URnetwork platform
- `instance_id` — unique ID for this specific run, changes on restart

---

## 🔄 Startup — Proxy Loading

```
[INFO] proxy.txt found; adding proxy
added server 65.111.10.67:1081 (91***rn/cf***9m)
Using 1000 proxy servers:
  proxy[0] 216.26.225.158:1081 (91***rn/cf***9m)
  proxy[1] 45.3.34.215:1081 (91***rn/cf***9m)
  ...
```

- Each `added server` line confirms a proxy was registered successfully
- Credentials are partially redacted in logs (`***`)
- `Using N proxy servers:` summarizes the loaded pool with index assignments

### `proxy.state` reconciliation

```
[proxy] pruned 711 stale proxy.state entries (no longer desired)
[proxy] skipping state prune this cycle: proxy_url.json unavailable
```

Emitted by the reload reconciler when it reconciles `~/.urnetwork/proxy.state` against the desired proxy set (config/file + URL cache). Entries for proxies that are no longer desired are pruned so `remove-dead` doesn't re-report ghosts forever.

| Message | Meaning |
|---|---|
| `pruned N stale proxy.state entries (no longer desired)` | `N` state entries were deleted because their proxies are gone from every source. On the first run after upgrading, this can be a large number: the accumulated ghost backlog being cleaned, not an outage. |
| `skipping state prune this cycle: proxy_url.json unavailable` | The URL cache could not be read, so the prune pass was skipped rather than risking deletion of state for still-desired URL proxies over a transient error. Nothing was removed this cycle; the next reload retries. |

---

## 📈 Reading Pool Stats Across Time

The pool stat fires every minute, so you can derive buffer throughput by subtracting consecutive `r=` values:

```
r=5601295  (05:25)
r=5607261  (05:26)
```
→ 5,966 buffers returned in 1 minute = active traffic flowing

A flat `r=` counter that doesn't grow means no sessions are active. A rapidly growing counter means heavy throughput.

---

## 🔑 JWT Auto-Refresh

```
[jwt] refreshing token — 7-day periodic refresh due (last refresh 168h0m ago)
🔑 [jwt] refresh → step 1/3: requesting auth code...
🔑 [jwt] refresh → step 1/3 ok: auth code received (684 chars)
🔑 [jwt] refresh → step 2/3: exchanging auth code for network JWT...
🔑 [jwt] refresh → step 2/3 ok: network JWT received (512 chars)
🔑 [jwt] refresh → step 3/3: verifying new token against https://api.bringyour.com/transfer/stats...
🔑 [jwt] refresh → step 3/3 ok: verification passed (HTTP 200, unpaid: 500.0 MB, paid: 1.0 GB)
🔑 [jwt] refresh OK — network JWT written to /root/.urnetwork/jwt (512 bytes, next refresh in 168h0m)
```

Two triggers (OR logic — either fires a refresh):
1. **Periodic (7-day)**: Has it been ≥7 days since the last successful refresh? Primary mechanism. Guarantees the token is rotated on a fixed cadence.
2. **Expiry fallback (48h)**: Is the token within 48 hours of expiring? Safety net if the periodic refresh failed repeatedly.

The refresher uses `/auth/code-create → /auth/code-login` (same flow as initial login). Before overwriting the on-disk JWT, it verifies the new token against `GET /transfer/stats`. A regression guard rejects any response containing a `client_id` claim (catches future regressions).

| Message | Meaning |
|---|---|
| `step 1/3` | Requesting an auth code from the API. |
| `step 2/3` | Exchanging the auth code for a fresh network JWT. |
| `step 3/3` | Verifying the new token works via a read-only stats endpoint. |
| `refresh OK` | New network JWT written to disk successfully. |
| `refresh FAILED: ... — keeping existing JWT` | Refresh failed at any step. The existing JWT is preserved. Will retry in 1h. |

## 🔑 JWT Startup Health

```
🔑 [jwt] expires in 12 days
🔑 [jwt] EXPIRED 3 days ago — refresh needed
```

Emitted once at startup. Shows the current JWT's health status.

---

## 🌐 WebRTC Peer Lifecycle

```
🔗 [signal] peer connected client_id=abc... type=webrtc
🔗 [signal] peer disconnected client_id=abc... type=webrtc reason=timeout
```

Fires once per P2P session creation/destruction. Low frequency — one event per peer connection, not per packet.

| Message | Meaning |
|---|---|
| `peer connected` | A new WebRTC peer connection was established. |
| `peer disconnected` | A peer connection was closed or timed out. |

### SCTP progress watchdog

```
[peerconn]SCTP no progress for 10s with 524288 bytes buffered; reconnecting
```

Logged by the lazy SCTP progress watchdog (added in the WebRTC tuning pass). It starts only after the first successful write, and fires when the data plane has made no progress for the watchdog window (10s) while bytes are still buffered, i.e. ICE consent looks healthy but the association is blackholed. The connection is torn down and re-established.

> [!NOTE]
> This line looks alarming but is **correct, expected behaviour**: it is the teardown path for an association that stopped moving data, not a transport failure report. Seeing it occasionally on lossy links is normal; seeing it constantly for the same destination warrants investigation.

---

## 📈 Traffic Velocity & Peaks

```
📈 [traffic] velocity: 3.2x → rx=12.3 MB/s tx=8.7 MB/s (was rx=3.8 MB/s tx=2.1 MB/s)
📈 [traffic] velocity: 0.3x → rx=1.2 MB/s tx=0.8 MB/s — traffic dropping
📈 [traffic] total rx=6.3 MB/s tx=3.9 MB/s peak_rx=18.4 MB/s peak_tx=7.8 MB/s clients=16
```

Velocity detection fires when total rate changes 3x+ between 5-minute health heartbeat ticks. Peak tracking records the maximum observed rates since startup.

| Message | Meaning |
|---|---|
| `velocity: N.Mx →` | Aggregate rate changed by N.Mx since last tick. Greater than 1 = increase. |
| `traffic dropping` | Rate decreased below 0.5x of previous — notable decline. |
| `peak_rx` / `peak_tx` | Highest observed receive/transmit rates in this session. |

---

## ✈️ Client Flight Markers

```
✈️ [traffic] clients 0→4 (first connect in 5h)
🛬 [traffic] clients 4→0 (last disconnect in 3m)
```

Emitted when aggregate client count transitions between zero and non-zero (and vice versa).

---

## 🚨 DNS Health

```
[doh] ⚠ 5 failures in last 5m
🚨 [doh] 120 failures in last 5m — possible DNS outage
```

Rate-limited to 1 per 5 minutes globally. Escalates to 🚨 when failures exceed 100 in a window. Failures also tracked as `dns_failures=N` in the `[health]` heartbeat.

| Message | Meaning |
|---|---|
| `⚠ N failures in last 5m` | Moderate DoH resolution failures — investigate if persistent. |
| `🚨 N failures` | Over 100 failures in 5 minutes — likely DNS outage. |

---

## 🐌 Proxy Startup Pace Monitor

```
[pace] ⚠ warmup: 47/200 up (24%), 150 connecting, 3 done
[pace] warmup: 142/200 up (71%), 55 connecting
[pace] ✓ warmup: 196/200 up (98%), 4 connecting — done
```

Fires every **30 seconds** during provider startup when the proxy fleet is warming up. Shows real-time progress of proxy authentication and connection. The pace monitor is a passive observer — it does not influence the stagger rate.

Once the `✓ done` line is logged, the `paceMonitor` goroutine exits. No further `[pace]` output is produced — silence after that line is expected and correct.

| Message | Meaning |
|---|---|
| `⚠ warmup: X/Y up (Z%), N connecting` | Fewer than 50% of proxies are up and more than 10 are still connecting — slow warmup. |
| `warmup: X/Y up (Z%), N connecting` | Normal warmup progress. |
| `✓ warmup: X/Y up (Z%), N connecting — done` | More than 90% of proxies are up and fewer than 5 are still connecting. Logged once, then the goroutine exits. |
