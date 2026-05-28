# High-Volume Performance Tuning

This guide covers the architectural tuning applied in the `urnetwork-3.23-fix` fork, how to choose the right profile for your server, and how to use the operational features added for fleet management.

---

## Profile Quick Reference

| Profile | `URNETWORK_PROFILE` | RAM | Throughput | Notes |
| :--- | :--- | :--- | :--- | :--- |
| **Auto** | `auto` | Any | Optimized | **Recommended.** Dynamically selects Tier based on RAM. |
| **Turbo V8** | `turbo-v8` | 16 GiB+ | Maximum | Dedicated servers, max earnings |
| **Turbo V4** | `turbo-v4` | 4–16 GiB | High | Well-provisioned VPS |
| *(default)* | *(unset)* | 2–4 GiB | Standard | General use |
| **Eco** | `eco` | 1–2 GiB | Standard | Full throughput, GC-tuned for RAM constraints |
| **Lowmem** | `lowmem` | < 1 GiB | Reduced | Minimum footprint, RAM logs on |

Turbo mode can also be set via the `TURBO=v4` / `TURBO=v8` Docker environment variable or `urnet-tools turbo <v4|v8|off>` on systemd installs.

---

## System Optimizer (`urnet-tools optimize`)

A high-volume provider can easily saturate default OS limits. The `optimize` command (run as root) applies "Golden Fleet" network tuning to the host:

1.  **File Descriptors**: Bumps `ulimit -n` to 1,048,576.
2.  **Conntrack Table**: Raises `nf_conntrack_max` to 2,097,152 (standard across all RAM sizes based on fleet observations).
3.  **Timeouts**: Reduces TCP established timeout from 5 days to 1 hour, clearing stale connections faster.
4.  **Port Range**: Expands local port range and enables TCP port reuse.

**Usage:**
```bash
sudo urnet-tools optimize
```

The provider binary also includes a **System Auditor** that checks these limits on every startup. It also performs a **Disk I/O Latency Test** (cache-busting sync write with a dynamic file size up to 1 GB). 

**Auto-Optimization:**
*   If `URNETWORK_PROFILE=auto` is set and disk speed is below **50 MB/s**, the provider will **automatically enable RAM logging** (`/dev/shm`) to prevent I/O wait from bottlenecking the network stack.
*   If free disk space is below **1 GiB**, it logs a critical warning.

---

## Master Parameter Reference

All values compared across upstream defaults and fork profiles. "Fork default" is what runs when no profile is set.

### Contract & Transfer

| Parameter | Upstream | Fork default | Lowmem | Eco | Turbo V4 | Turbo V8 |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| `InitialContractTransferByteCount` | 16 KiB | 2 MiB | 256 KiB | 2 MiB | 2 MiB | 2 MiB |
| `ContractTransferByteSeqScale` | 4 | 4 | 4 | 4 | 2 | 2 |
| `ContractFillFraction` | 0.8 | 0.7 | 0.7 | 0.7 | 0.7 | 0.7 |
| `CreateContractTimeout` | 30 s | 60 s | 60 s | 60 s | 60 s | 60 s |
| `ResendQueueMaxByteCount` | 2 MiB | 2 MiB | reduced | 2 MiB | 8 MiB | 16 MiB |
| `ReceiveQueueMaxByteCount` | ~2.5 MiB | ~2.5 MiB | reduced | ~2.5 MiB | 8 MiB | 16 MiB |
| `MaxResendCount` | unlimited | 16 | 16 | 16 | 16 | 16 |
| Transfer `SequenceBufferSize` | 16 | 16 | reduced | 16 | 64 | 64 |

### TCP / IP Layer

| Parameter | Upstream | Fork default | Lowmem | Eco | Turbo V4 | Turbo V8 |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| TCP `MaxWindowSize` | 1 MiB | 1 MiB (Accordion) | reduced | 1 MiB | 4 MiB | 8 MiB |
| UDP `MaxWindowSize` | 1 MiB | 1 MiB | reduced | 1 MiB | 4 MiB | 8 MiB |
| IP `SequenceBufferSize` | 64 | 256 | reduced | 256 | 512 | 512 |
| Accordion window start | fixed | 4 KiB | 4 KiB | 4 KiB | 4 KiB | 4 KiB |
| Accordion idle shrink | none | 30 s | 30 s | 30 s | 30 s | 30 s |

### WebRTC

| Parameter | Upstream | Fork default | Turbo V4 | Turbo V8 |
| :--- | :--- | :--- | :--- | :--- |
| `ReceiveBufferSize` per peer | 4 MiB | 4 MiB | 8 MiB | 16 MiB |

### Memory & GC

| Parameter | Upstream | Fork default | Lowmem | Eco | Turbo V4 | Turbo V8 |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| `InitialMessagePoolByteCount` | 1 MiB | auto (RAM/32) | auto | auto | auto | auto |
| Message pool floor | — | 8 MiB | 8 MiB | 8 MiB | 8 MiB | 8 MiB |
| Message pool cap | — | 256 MiB | 256 MiB | 256 MiB | 256 MiB | 256 MiB |
| GOGC | 100 | 100 | 100 | 50 (dynamic) | 200 | 200 |
| GOMEMLIMIT | unset | unset | 85% RAM | 75% RAM | unset | unset |
| RAM logging | off | off | on | off | off | off |

---

## Profiles In Depth

### Auto Profile (`auto`)

The `auto` profile is the recommended setting for most users. It detects available system RAM at startup and selects the best internal balance of contract sizes and buffer depths. For RAM-constrained systems (Low and Balanced tiers), it **automatically enables the dynamic Eco Memory Monitor** to prevent OOMs.

| Tier | RAM Range | Contract Floor | IP Buffer | TCP Window | WebRTC Recv | Eco Monitor |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **Low** | < 1.2 GiB | 128 KiB | 32 | 128 KiB | 512 KiB | **Enabled** |
| **Balanced**| 1.2 - 3 GiB | 256 KiB | 128 | 512 KiB | 1 MiB | **Enabled** |
| **Perf** | > 3 GiB | 2 MiB | 256 | 1 MiB | 4 MiB | Disabled |

**Enabling:**
```bash
# Docker
-e URNETWORK_PROFILE=auto
```

### Turbo Mode (V4 / V8)

The default 1 MiB TCP Accordion window creates a hard per-connection throughput ceiling: `throughput = window / RTT`. At a typical 10ms RTT that caps each connection at ~100 Mbps regardless of available bandwidth. Turbo mode raises this ceiling by scaling the window and all dependent buffers.

**Theoretical ceilings per connection:**

| RTT | Default | V4 | V8 |
| :--- | :--- | :--- | :--- |
| 5 ms | ~210 Mbps | ~838 Mbps | ~1.6 Gbps |
| 10 ms | ~105 Mbps | ~419 Mbps | ~838 Mbps |
| 20 ms | ~52 Mbps | ~210 Mbps | ~419 Mbps |
| 50 ms | ~21 Mbps | ~84 Mbps | ~168 Mbps |

**What turbo changes:**
- TCP/UDP Accordion window ceiling: 1 MiB → 4 MiB (V4) / 8 MiB (V8)
- Transfer resend and receive queues: 2 MiB → 8/16 MiB (so they don't become the new bottleneck)
- IP sequence buffer depth: 256 → 512
- Transfer goroutine buffer depth: 16 → 64
- WebRTC DataChannel buffer: 4 MiB → 8/16 MiB per peer
- Contract ramp (`ContractTransferByteSeqScale`): 4 → 2 (full speed in 2 contracts instead of 4)
- GOGC raised to 200, GOMEMLIMIT unset (heap breathes freely on RAM-rich boxes)

**Enabling:**
```bash
# Binary (systemd)
urnet-tools turbo v4      # or v8
urnet-tools turbo off     # return to default
urnet-tools turbo         # show current state

# Docker
-e TURBO=v4               # or TURBO=v8
```

**When to use V4 vs V8:** V4 is a good starting point for 4–16 GiB boxes. V8 is for dedicated servers with 16 GiB+ RAM where the extra 4 MiB per connection window is affordable. Check RSS under load before rolling V8 broadly.

---

### Eco Mode

Eco mode is for providers on RAM-constrained systems who still want full throughput and earnings. It leaves all buffer and contract sizes untouched and only tunes the GC.

**What eco changes:**
- GOGC: 100 → 50 (GC runs twice as often, keeping heap smaller at the cost of slightly more CPU)
- GOMEMLIMIT: unset → 75% of detected RAM (cgroup-aware; Docker `--memory` containers get the correct ceiling)
- Dynamic GC pressure monitor: if available memory drops below 300 MiB, GOGC drops to 25; below 150 MiB, drops to 10; recovers with hysteresis when pressure clears
- All buffer and contract settings unchanged — throughput is unaffected

**Enabling:**
```bash
# Binary (systemd)
urnet-tools eco on
urnet-tools eco off

# Docker
-e URNETWORK_PROFILE=eco
```

---

### Lowmem Mode

Lowmem mode is for the most RAM-constrained environments where the provider must share a host with other processes. It reduces buffer sizes, enables RAM logging, and applies a hard memory cap.

**What lowmem changes:**
- Buffer sizes reduced (IP, transfer, TCP/UDP windows)
- GOMEMLIMIT: 85% of detected RAM (cgroup-aware)
- RAM logging enabled unconditionally (logs go to `/dev/shm/urnetwork.log`, 1 MB cap)
- `InitialContractTransferByteCount`: kept at 256 KiB (not reduced to stock 16 KiB) — throughput ramp-up is preserved even in low-memory mode

**Enabling:**
```bash
# Binary (systemd)
urnet-tools lowmode on
urnet-tools lowmode off

# Docker
-e URNETWORK_PROFILE=lowmem
```

> **Note:** When lowmem is active, logs go to RAM (`/dev/shm`) rather than stdout. Remove `--log-driver` / `--log-opt` from your Docker run command. View logs with: `docker exec -it <container> tail -f /dev/shm/urnetwork.log`

---

## Accordion TCP Scaling

Connections start with a minimal **4 KiB** TCP window to conserve RAM on idle connections. As throughput increases, the window doubles on each successful delivery up to the profile ceiling (1 MiB default, 4/8 MiB turbo). If a connection goes idle, the window shrinks back to 4 KiB after 30 seconds.

This means the provider can hold many concurrent idle connections cheaply while still serving active ones at full speed. The overhead of an idle connection is the 4 KiB window allocation, not the profile ceiling.

---

## Contract Management

**`InitialContractTransferByteCount`** controls how many bytes are authorized per contract on the first request. The upstream default of 16 KiB means a new connection hits its quota almost immediately and must renegotiate — under heavy load this creates a signaling storm. The fork raises this to 2 MiB, so a new connection can transfer a meaningful amount before the first renegotiation.

**`ContractTransferByteSeqScale`** controls how many contracts it takes to reach full (`StandardContractTransferByteCount`) speed. At the default of 4, the ramp takes 4 contracts. Turbo mode sets this to 2, halving the ramp time.

**`ContractFillFraction`** (0.7 vs upstream 0.8) starts the contract refill request earlier — when 70% of the current quota is used instead of 80%. This gives more headroom before exhaustion, which matters when signaling latency is high.

**`CreateContractTimeout`** (60s vs upstream 30s) gives the OOB signaling layer more time to respond during load spikes before dropping the connection.

---

## Message Pool Auto-Sizing

The message pool is a free-list of pre-allocated byte buffers (size classes: 2 KB, 4 KB, 16 KB, 32 KB, 64 KB). Packets are served from this pool on the hot path; a pool miss falls back to a heap allocation and GC pressure increases.

The upstream default caps the pool at 1 MiB total regardless of available RAM. On a server running many proxies, this pool is exhausted quickly under real load — most packets fall back to heap allocation, creating unnecessary GC churn.

**Auto-sizing** (added in fix.14) scales the pool to available RAM at startup:

```
pool_size = RAM / 32
floor:  8 MiB
cap:   256 MiB
```

This is skipped when `URNETWORK_PROFILE=lowmem` (lowmem manages its own footprint) and when `--max-memory` is set explicitly (that path already calls `ResizeMessagePools(maxMemory/8)`).

**Startup log:**
```
[pool] message pool 61MiB (RAM=1964MiB)
```

No configuration is required — auto-sizing runs on every startup.

---

## Outage Detection and Alerting

The provider monitors backend connectivity in the background and logs state transitions. This runs on every startup regardless of configuration.

**Log output:**
```
[outage] watcher active node=my-server (docker) webhook=configured
[outage] backend degraded
[outage] backend recovered
```

**How it works:**

The watcher is built to distinguish a real, sustained outage from the isolated connection timeouts that are normal churn on a busy provider. A single failed connect or a one-off contract signaling timeout must **not** raise an alarm — only the backend being broadly unreachable should.

Two mechanisms work together:

1. **Consecutive-failure counter (the signal).** The provider keeps a process-wide counter of backend failures (transport auth/connect failures and contract OOB errors). *Any* successful connect or OOB result resets it to zero. `IsBackendDegraded()` reports degraded only when the counter has crossed a threshold (currently 3 consecutive failures with no intervening success) **and** the most recent failure was within the last 2 minutes. Because a single success anywhere resets the count, isolated timeouts never accumulate — the counter only climbs when essentially *every* attempt is failing, which is what a genuine platform outage looks like. The 2-minute recency guard ensures a stale count left by an old blip on an idle provider is not mistaken for a live outage.

2. **Start-side debounce (the confirmation).** The watcher polls `IsBackendDegraded()` every 30 seconds and requires **10 consecutive degraded polls — a full 5 minutes of continuous degradation — before firing `outage_start`.** If the backend recovers at any point during that window (any poll reads healthy), the count resets and no alarm is sent. This trades detection latency (up to ~5 minutes) for a near-zero false-alarm rate: for an alert to fire, the backend must fail continuously with zero successful connects or OOB calls for the entire 5-minute window.

- Recovery (`outage_clear`) requires 2 consecutive healthy polls — prevents premature all-clears during brief mid-outage lulls.
- State transitions are always logged to stdout, whether or not a webhook is configured.

> **Tuning the tradeoff:** the 5-minute confirmation window is deliberately conservative to avoid false positives. Detection of a true outage is therefore not instantaneous — expect the `outage_start` event roughly 5 minutes after connectivity is actually lost. Recovery is reported promptly (within ~1 minute).

**Webhook alerts (optional):**

Set `URNETWORK_ALERT_WEBHOOK` to receive a push notification on each outage event. The provider POSTs JSON:

```json
{
  "event": "outage_start",
  "node": "my-server (docker)",
  "timestamp": "2026-05-27T23:48:34Z",
  "message": "Backend unreachable — provider holding existing connections but not accepting new ones."
}
```

`event` is `outage_start` or `outage_clear`.

**Per-event cooldown:** webhook calls have a 5-minute per-event cooldown to prevent spam at the recovery boundary (e.g., if the backend briefly flickers back and forth). Webhook delivery is non-blocking — a slow or unreachable endpoint never delays the poll loop.

**Supported services:** any HTTP endpoint accepting POST with a JSON body. Tested with Discord, Slack, PagerDuty, and ntfy.

```bash
# Examples
URNETWORK_ALERT_WEBHOOK=https://discord.com/api/webhooks/YOUR_ID/YOUR_TOKEN
URNETWORK_ALERT_WEBHOOK=https://hooks.slack.com/services/T.../B.../...
URNETWORK_ALERT_WEBHOOK=https://ntfy.sh/your-topic
```

**Node identity:**

`URNETWORK_NODE_NAME` sets the label included in webhook payloads and log lines. If not set, the provider auto-generates a name from `os.Hostname()` suffixed with `(docker)` or `(binary)` depending on whether `/.dockerenv` is present.

```bash
# Docker
-e URNETWORK_NODE_NAME=atl-provider-1

# Result in payloads
"node": "atl-provider-1 (docker)"
```

On bare-metal installs the suffix is `(binary)`, so alerts from containers and binaries on the same host are distinguishable without any configuration.

> When running multiple containers on one host, set a distinct `URNETWORK_NODE_NAME` per container so webhook alerts identify which one fired.

---

## Health Heartbeat

The provider logs a periodic heartbeat line with uptime, active profile, and memory statistics. This provides passive liveness confirmation and heap trend visibility without external tooling.

**Log output:**
```
[health] uptime=2h34m profile=turbo-v8 heap=142MiB sys=198MiB
```

- `heap`: live Go heap allocations (bytes in use by Go objects)
- `sys`: total memory obtained from the OS by the Go runtime
- Uses `runtime/metrics` (lock-free, no stop-the-world pause) rather than `runtime.ReadMemStats`

**Configuration:**

| Variable | Default | Description |
| :--- | :--- | :--- |
| `URNETWORK_HEALTH_INTERVAL` | `5m` | Heartbeat interval. Accepts Go duration strings (`10m`, `1h`). Minimum `1m`. |

---

## Log Spam Reduction

During backend outages, certain log lines would historically fire thousands of times per minute. The fork applies global rate limiting:

| Log pattern | Rate limit | Suppression summary |
| :--- | :--- | :--- |
| `[t]auth error` | 1 per minute | suppressed count appended when outage clears |
| `[contract]oob err` | 1 per minute | suppressed count appended when outage clears |

**Example:**
```
[t]auth error: connection refused (3,952 suppressed)
```

The `(N suppressed)` suffix ensures no errors are silently dropped — you see the count even if the individual lines were suppressed.

---

## Troubleshooting

**`[net][s]select` not appearing in logs:**
This line is promoted to INFO level in the fork (upstream logs it at Debug level 2, hidden unless `-v` is passed). If it's missing, the provider is not successfully connecting any proxies — check auth and network connectivity.

**High memory usage under load:**
If RSS grows without bound, consider switching from turbo to the default profile or enabling eco mode. The Accordion window means idle connections are cheap, but connections that have ramped up to the ceiling will hold their window allocation until they go idle. Check `[health]` log lines to track heap trend over time.

**Contract renegotiation noise (`could not create contract`, `oob err = Timeout`):**
These appear when the OOB signaling layer is overloaded. Tuning `CreateContractTimeout` higher (already 60s in fork) and `ContractFillFraction` lower gives more headroom. If they persist, it's usually a backend API outage — the `[outage]` watcher will detect and log this.

**RAM logging + Docker log drivers conflict:**
`URNETWORK_RAMLOGS=1` and `--log-driver` / `--log-opt` are mutually exclusive. When RAM logging is active, nothing is written to stdout. Remove the Docker log driver flags and use `docker exec -it <container> tail -f /dev/shm/urnetwork.log` to view logs.
