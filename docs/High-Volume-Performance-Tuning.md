# 🚀 High-Volume Performance Tuning

This guide covers the architectural tuning applied in the `urnetwork-3.23-fix` fork, how to choose the right profile for your server, and how to use the operational features added for fleet management.

---

## 📊 Profile Quick Reference

| Profile | `URNETWORK_PROFILE` | RAM | Throughput | Notes |
| :--- | :--- | :--- | :--- | :--- |
| **Auto** | `auto` | Any | Optimized | **Recommended.** Dynamically selects Tier based on RAM (Low, Balanced, Perf, or Extreme). |
| **Turbo V8** | `turbo-v8` | 16 GiB+ | Maximum | Dedicated servers, maximum throughput and concurrency |
| **Turbo V4** | `turbo-v4` | 4–16 GiB | High | Well-provisioned VPS |
| *(default)* | *(unset)* | 2–4 GiB | Standard | General use |
| **Eco** | `eco` | 1–2 GiB | Standard | Full throughput, GC-tuned for RAM constraints |
| **Lowmem** | `lowmem` | < 1 GiB | Reduced | Minimum footprint, RAM logs on |

Turbo mode can also be set via the `TURBO=v4` / `TURBO=v8` Docker environment variable or `urnet-tools turbo <v4|v8|off>` on systemd installs.

---

## 🔧 System Optimizer (`urnet-tools optimize`)

A provider deploying many proxies can easily saturate default OS limits, even before the deployment feels "high volume" in day-to-day language. The `optimize` command (run as root) applies full system-level tuning to the host for large proxy lists and high-volume network traffic:

1.  **File Descriptors**: Bumps `ulimit -n` to 1,048,576.
2.  **Conntrack Table**: Raises `nf_conntrack_max` to 2,097,152 (standard across all RAM sizes based on fleet observations).
3.  **Timeouts**: Reduces TCP established timeout from 5 days to 1 hour, clearing stale connections faster.
4.  **Port Range**: Expands local port range and enables TCP port reuse.
5.  **TCP Congestion**: Enables **BBR** (Bottleneck Bandwidth and Round-trip propagation time) and **Fair Queuing (fq)** for superior throughput and reduced bufferbloat.

**Usage:**
```bash
urnet-tools optimize
```

The provider binary also includes a **System Auditor** that checks these limits on every startup. It also performs a **Disk I/O Latency Test** (cache-busting sync write with a dynamic file size up to 1 GB). 

**Auto-Optimization:**
*   If `URNETWORK_PROFILE=auto` is set and disk speed is below **50 MB/s**, the provider will **automatically enable RAM logging** (`/dev/shm`) to prevent I/O wait from bottlenecking the network stack.
*   If free disk space is below **1 GiB**, it logs a critical warning.

---

## 🎛️ Master Parameter Reference

All values compared across upstream defaults and fork profiles. "Fork default" is what runs when no profile is set.

### 📜 Contract & Transfer

| Parameter | Upstream | Fork default | Lowmem | Eco | Auto Extreme | Turbo V4 | Turbo V8 |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| `InitialContractTransferByteCount` | 16 KiB | 2 MiB | 128 KiB | 2 MiB | 2 MiB | 2 MiB | 2 MiB |
| `ContractTransferByteSeqScale` | 4 | 3 | 4 | 4 | 3 | 2 | 3 |
| `ContractFillFraction` | 0.8 | dynamic* | 0.7 | 0.7 | dynamic* | dynamic* | dynamic* |
| `CreateContractTimeout` | 30 s | 60 s | 60 s | 60 s | 60 s | 60 s | 60 s |
| `ResendQueueMaxByteCount` | 2 MiB | 2 MiB | reduced | 2 MiB | 16 MiB | 8 MiB | 16 MiB |
| `ReceiveQueueMaxByteCount` | ~2.5 MiB | ~2.5 MiB | reduced | ~2.5 MiB | 16 MiB | 8 MiB | 16 MiB |
| `MaxResendCount` | unlimited | 16 | 16 | 16 | 16 | 16 | 16 |
| Transfer `SequenceBufferSize` | 16 | 16 | reduced | 16 | 64 | 64 | 64 |

### 🌐 TCP / IP Layer

| Parameter | Upstream | Fork default | Lowmem | Eco | Auto Extreme | Turbo V4 | Turbo V8 |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| TCP `MaxWindowSize` | 1 MiB | 4 MiB (Accordion) | reduced | 4 MiB | 8 MiB | 4 MiB | 8 MiB |
| UDP `MaxWindowSize` | 1 MiB | 4 MiB | reduced | 4 MiB | 8 MiB | 4 MiB | 8 MiB |
| IP `SequenceBufferSize` | 64 | 256 | reduced | 256 | 512 | 512 | 512 |
| Accordion window start | fixed | 4 KiB | 4 KiB | 4 KiB | 4 KiB | 4 KiB | 4 KiB |
| Accordion idle shrink | none | 30 s | 30 s | 30 s | 30 s | 30 s | 30 s |

### 📡 WebRTC

| Parameter | Upstream | Fork default | Auto Extreme | Turbo V4 | Turbo V8 |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `ReceiveBufferSize` per peer | 4 MiB | 4 MiB | 16 MiB | 8 MiB | 16 MiB |

### 🧠 Memory & GC

| Parameter | Upstream | Fork default | Lowmem | Eco | Auto Extreme | Turbo V4 | Turbo V8 |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| `InitialMessagePoolByteCount` | 1 MiB | auto (RAM/32) | auto | auto | auto | auto | auto |
| Message pool floor | — | 8 MiB | 8 MiB | 8 MiB | 8 MiB | 8 MiB | 8 MiB |
| Message pool cap | — | 256 MiB | 256 MiB | 256 MiB | 256 MiB | 256 MiB | 256 MiB |
| GOGC | 100 | 100 | 100 | 50 (static baseline) | 200 | 200 | 200 |
| GOMEMLIMIT | unset | unset | 85% RAM | 75% RAM | unset | 80% RAM | 80% RAM |
| RAM logging | off | off | on | off | off | off | off |

---

## 🔬 Profiles In Depth

### 🤖 Auto Profile (`auto`)

The `auto` profile is the recommended setting for most users. It detects available system RAM at startup and selects the best internal balance of contract sizes and buffer depths. All tiers are covered by the consolidated adaptive GC governor in the pressure monitor, which tightens GOGC under memory pressure to prevent OOMs. On machines with **8 GiB+ RAM**, auto selects the **Extreme** tier, which applies the same turbo-v8 settings (8 MiB windows, 16 MiB queues, seq buf 512, GOGC 200) with a contract ramp scale of 3.

| Tier | RAM Range | Contract Floor | IP Buffer | TCP Window | WebRTC Recv | Adaptive GC |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **Low** | < 1.2 GiB | 128 KiB | 32 | 128 KiB | 512 KiB | Enabled |
| **Balanced**| 1.2 - 3 GiB | 256 KiB | 128 | 512 KiB | 1 MiB | Enabled |
| **Perf** | 3 - 8 GiB | 2 MiB | 256 | 4 MiB | 4 MiB | Enabled |
| **Extreme** | >= 8 GiB | 2 MiB | 512 | 8 MiB | 16 MiB | Enabled |

**Enabling:**
```bash
# Docker
-e URNETWORK_PROFILE=auto
```

### 🏎️ Turbo Mode (V4 / V8)

The default 1 MiB TCP Accordion window creates a theoretical per-connection throughput ceiling based on latency: `ceiling = window / RTT`. At a typical 10ms RTT, the protocol is mathematically capped at ~100 Mbps regardless of available bandwidth. Turbo mode raises this mathematical ceiling by scaling the window and all dependent buffers.

**Theoretical Window Ceilings (Mathematical Maximums):**

| RTT (Latency) | Fork Default (4 MiB) | Turbo V4 (4 MiB) | Turbo V8 (8 MiB) |
| :--- | :--- | :--- | :--- |
| 5 ms | ~6.4 Gbps | ~6.4 Gbps | ~12.8 Gbps |
| 10 ms | ~400 Mbps | ~400 Mbps | ~800 Mbps |
| 20 ms | ~200 Mbps | ~200 Mbps | ~400 Mbps |
| 50 ms | ~80 Mbps | ~80 Mbps | ~160 Mbps |

*Actual throughput will always be lower due to packet loss, processing overhead, and network congestion.*


**What turbo changes:**
- TCP/UDP Accordion window ceiling: 1 MiB → 4 MiB (V4) / 8 MiB (V8)
- Transfer resend and receive queues: 2 MiB → 8/16 MiB (so they don't become the new bottleneck)
- IP sequence buffer depth: 256 → 512
- Transfer goroutine buffer depth: 16 → 64
- WebRTC DataChannel buffer: 4 MiB → 8/16 MiB per peer
- Contract ramp (`ContractTransferByteSeqScale`): 4 → 3 (full speed in 3 contracts instead of 4)
- GOGC raised to 200, GOMEMLIMIT set to 80% of available RAM as a safety ceiling for RAM-rich boxes (prevents unbounded heap growth during outages without impacting normal throughput)

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

### 🍃 Eco Mode

Eco mode is for providers on RAM-constrained systems who still want full throughput and standard buffer allocations. It leaves all buffer and contract sizes untouched and only tunes the GC.

**What eco changes:**
- GOGC: 100 → 50 (GC runs twice as often, keeping heap smaller at the cost of slightly more CPU)
- GOMEMLIMIT: unset → 75% of detected RAM (cgroup-aware; Docker `--memory` containers get the correct ceiling)
- Consolidated adaptive GC governor (part of the pressure monitor): on by default for every profile. It tightens GOGC below the eco baseline under memory pressure, to `min(baseline, 50)` at heap >= 0.70, `min(baseline, 25)` at heap >= 0.80 or host RAM <= 300 MiB, and `min(baseline, 10)` plus `FreeOSMemory` at heap >= 0.92 or host RAM <= 150 MiB. The static 50 below is only the startup baseline; the governor only ever tightens further, never raises it. Disable with `URNETWORK_ADAPTIVE_GC=0`.
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

### 🪫 Lowmem Mode

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

> [!NOTE]
> When lowmem is active, logs go to RAM (`/dev/shm`) rather than stdout. Remove `--log-driver` / `--log-opt` from your Docker run command. View logs with: `docker exec -it <container> tail -f /dev/shm/urnetwork.log`

---

## 🪗 Accordion TCP Scaling

Connections start with a minimal **4 KiB** TCP window to conserve RAM on idle connections. As throughput increases, the window doubles on each successful delivery up to the profile ceiling (4 MiB default, 8 MiB turbo). If a connection goes idle, the window shrinks back to 4 KiB after 30 seconds.

This means the provider can hold many concurrent idle connections cheaply while still serving active ones at full speed. The overhead of an idle connection is the 4 KiB window allocation, not the profile ceiling.

---

## 📝 Contract Management

**`InitialContractTransferByteCount`** controls how many bytes are authorized per contract on the first request. The upstream default of 16 KiB means a new connection hits its quota almost immediately and must renegotiate — under heavy load this creates a signaling storm. The fork raises this to 2 MiB, so a new connection can transfer a meaningful amount before the first renegotiation.

**`ContractTransferByteSeqScale`** controls how many contracts it takes to reach full (`StandardContractTransferByteCount`) speed. The fork default is 3 (full speed in 3 contracts, 4 steps: 2 MiB → ~44 MiB → ~86 MiB → 128 MiB). Turbo profiles override to 2 for faster ramp-up. Lowmem and eco use 4 to reduce signaling frequency on constrained nodes.

**`ContractFillFraction`** (`dynamic` starting v3.23.0-fix.24) adapts to the observed transfer RTT. At low RTT (≤100ms) it fills to 0.85 — refills arrive quickly so we can pack close to capacity. At high RTT (≥1000ms) it drops to 0.50 — bytes drain faster relative to API round-trip so more headroom is needed. Falls back to the static 0.7 when no RTT data is available (e.g. cold start). Lowmem and eco profiles still use the static 0.7 to avoid the per-sequence RTT window allocation.

**`CreateContractTimeout`** (60s vs upstream 30s) gives the OOB signaling layer more time to respond during load spikes before dropping the connection.

---

## 🏇 Multi-Race Client Count

The `MultiRaceClientCount` setting controls how many remote providers are raced simultaneously to deliver the first packet of a new flow. Only one needs to ack — the first responder wins and becomes the active provider for the flow's lifetime. Racing more providers improves the chance of a fast pick.

**Fork default:** 16 on all platforms. The race count is double-bounded at runtime by the number of healthy providers in the window and by per-flow packet budgets, so a node with 3 healthy providers races 3 regardless of this ceiling. Each race goroutine is I/O-bound (blocks on network response), so single-core nodes benefit just as much as multicore ones.

---

## 📦 Message Pool Auto-Sizing

The message pool is a free-list of pre-allocated byte buffers (size classes: 2 KB, 4 KB, 16 KB, 32 KB, 64 KB). Packets are served from this pool on the hot path; a pool miss falls back to a heap allocation and GC pressure increases.

The upstream default caps the pool at 1 MiB total regardless of available RAM. On a server running many proxies, this pool is exhausted quickly under real load — most packets fall back to heap allocation, creating unnecessary GC churn.

**Auto-sizing** (added in fix.14) scales the pool to available RAM at startup:

```
pool_size = RAM / 32
floor:  8 MiB
cap:   256 MiB
```

This is skipped when `URNETWORK_PROFILE=lowmem` (lowmem manages its own footprint) and when `--max-memory` is set explicitly (that path already calls `ResizeMessagePools(maxMemory/8)`).

**Mutex sharding** (added in v24.35) splits each size class into N internal shards with independent mutexes, eliminating cross-proxy lock contention on the hottest allocation path. Default is 16 shards. Override with `URNETWORK_MESSAGE_POOL_SHARD_COUNT` (power of two, 1–256). Set to `1` to disable (pre-v24.35 behavior).

**Startup log:**
```
[pool] message pool 61MiB (RAM=1964MiB)
```

No configuration is required — auto-sizing runs on every startup.

---



## 💓 Health Heartbeat

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

## 🔇 Log Spam Reduction

During backend outages, certain log lines would historically fire thousands of times per minute. The fork applies rate limiting or once-per-process guards depending on the source:

| Log pattern | Mechanism | Notes |
| :--- | :--- | :--- |
| `[t]auth error` | 1 per minute rate limit | Suppressed count appended when outage clears |
| `[contract]oob err` | 1 per minute rate limit | Suppressed count appended when outage clears |
| `[tune] auto-profile` | Once per process | Was once per proxy on startup; fixed in fix.15 |

**Example (outage rate-limiting):**
```
[t]auth error: connection refused (3,952 suppressed)
```

The `(N suppressed)` suffix ensures no errors are silently dropped — you see the count even if the individual lines were suppressed.

**Startup spam (large proxy lists):** With `URNETWORK_PROFILE=auto` and a large proxy list (e.g. 3,000 proxies), the `[tune] auto-profile` line was previously emitted once per proxy rather than once per process. It is now guarded by an atomic once-flag so it fires exactly once regardless of proxy count. (The older eco memory monitor duplication is moot: that runtime loop has been retired and folded into the unified adaptive GC governor.)

---

## 🛠️ Troubleshooting

**`[net][s]select` not appearing in logs:**
This line is promoted to INFO level in the fork (upstream logs it at Debug level 2, hidden unless `-v` is passed). If it's missing, the provider is not successfully connecting any proxies — check auth and network connectivity.

**High memory usage under load:**
If RSS grows without bound, consider switching from turbo to the default profile or enabling eco mode. The Accordion window means idle connections are cheap, but connections that have ramped up to the ceiling will hold their window allocation until they go idle. Check `[health]` log lines to track heap trend over time and verify the `connections=N` count — if heap grows while connections stay flat, it may indicate a leak or oversized proxy list metadata.

**Contract renegotiation noise (`could not create contract`, `oob err = Timeout`):**
These appear when the OOB signaling layer is overloaded. Tuning `CreateContractTimeout` higher (already 60s in fork) and `ContractFillFraction` lower gives more headroom. If they persist, it's usually a backend API outage — the `[outage]` watcher will detect and log this.

**RAM logging + Docker log drivers conflict:**
`URNETWORK_RAMLOGS=1` and `--log-driver` / `--log-opt` are mutually exclusive. When RAM logging is active, nothing is written to stdout. Remove the Docker log driver flags and use `docker exec -it <container> tail -f /dev/shm/urnetwork.log` to view logs.
