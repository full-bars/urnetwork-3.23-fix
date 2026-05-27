# UrNetwork v3.23 Fix (Custom Build)

This is a high-performance, high-visibility fork of the **UrNetwork Connect** provider, based on the stable **v3.23** engine. It combines the latest protocol optimizations with surgical improvements for professional providers managing large proxy lists.

## 🚀 Key Improvements

### 1. High-Signal Monitoring (Promoted Logs)
In standard builds, connection handshake logs are hidden behind debug flags, leading to "silent" nodes. In this version:
*   **[net][s]select (Serial Select)**: Promoted from Debug Level 2 to **Standard INFO level**. You will see exactly one clean line every time a proxy connection is successfully established.
*   **Noise Reduction**: Parallel selection logs ([net][p]) remain silenced, ensuring that even with high-scale proxy lists, your logs stay readable and useful.
*   **Log Spam Reduction**: During backend outages, `[t]auth error` and `[contract]oob err` are rate-limited to one line per minute globally across all proxy instances. A suppressed-count suffix (e.g. `(3,952 suppressed)`) is appended when the outage clears so no errors are silently dropped.

### 2. Throughput & Scalability (Unlocked Engine)
The default UrNetwork engine is often bottlenecked for high-bandwidth providers, leading to capacity caps and micro-stutters.
*   **Contract Cap**: Boosted `InitialContractTransferByteCount` from 16 KiB to **2 MiB** for faster connection ramp-up.
*   **High-Scale Stability**: Increased `CreateContractTimeout` to **60s** and tuned `ContractFillFraction` to **0.7** to prevent connection drops during massive signaling spikes.
*   **Accordion Scaling**: Implemented dynamic TCP window scaling. Windows start small (**4KB**) to save RAM on idle connections and grow on demand (up to **1MB**) for active throughput. Windows automatically shrink back to 4KB after 30s of inactivity.
*   **Zero-Allocation Path**: Expanded internal Message Pools (16KB, 32KB, 64KB) to eliminate Garbage Collector CPU spikes during high-throughput transfers.
*   **Burst Protection**: Quadrupled IP Buffer Depth to **256** to absorb network volatility without dropping packets.

### 3. Professional Docker Integration
This image integrates the excellent wrapper scripts from the community-maintained `techroy23/Docker-UrNetwork` project.
*   **JWT & User/Pass Support**: Full support for `BUILD=jwt` or standard email/password authentication.
*   **vnStat Integration**: Real-time traffic monitoring built-in (accessible via port 8080).
*   **Multi-Arch**: Native builds for both **AMD64** (Intel/AMD) and **ARM64** (Oracle Cloud, Raspberry Pi, Graviton).

### 4. Performance Profiles

Choose the profile that matches your server's available RAM:

| Profile | Best For | RAM |
| :--- | :--- | :--- |
| **Turbo V8** | Maximum throughput, dedicated servers | 16 GiB+ |
| **Turbo V4** | High throughput, well-provisioned VPS | 4–16 GiB |
| *(default)* | General use | 2–4 GiB |
| **Eco** | RAM-constrained, full throughput | 1–2 GiB |
| **Lowmode** | Minimum RAM, reduced throughput | < 1 GiB |

*   **Turbo V4 / V8**: Raises the TCP Accordion window from 1 MiB to 4 or 8 MiB, removing the ~100–150 Mbps per-connection ceiling that exists at typical internet RTTs. Scales resend/receive queues, buffer depths, and WebRTC buffers to match. GOGC is raised to 200 and GOMEMLIMIT is unset so the heap can use available RAM freely. For RAM-rich boxes where throughput and earnings are the priority.
*   **Eco**: GC-tuned for RAM-constrained systems. Sets GOMEMLIMIT to 75% of detected RAM (cgroup-aware), enables dynamic GC pressure monitoring, and leaves all buffers untouched so throughput is unaffected.
*   **Lowmode**: Reduces buffer sizes and sets a hard GOMEMLIMIT (85% of RAM) for the most constrained environments. Initial contract size is kept at 256 KiB (not reduced) so throughput ramp-up is preserved even in low-memory mode.
*   **RAM Logging**: Redirects all provider logs to `/dev/shm` (Linux RAM disk) with a 1MB rotation cap. Eliminates disk I/O overhead on weak cloud instances.

---

## ⚡ Quick Start (Linux)

Install the optimized provider directly as a background service:

**Install:**
```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/main/scripts/Provider_Install_Linux.sh | sh
```

**Uninstall:**
```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/main/scripts/Provider_Uninstall_Linux.sh | sh
```

### 🛠 Post-Install Commands
The installation includes the `urnet-tools` suite for easy management:

| Command | Description |
| :--- | :--- |
| `urnet-tools status` | Check service health and uptime. |
| `urnet-tools logs` | Stream logs (automatically detects RAM vs Disk). |
| `urnet-tools turbo v4` | Enable Turbo V4 mode (~400 Mbps ceiling at 10ms RTT). |
| `urnet-tools turbo v8` | Enable Turbo V8 mode (~800 Mbps ceiling at 10ms RTT). |
| `urnet-tools turbo off` | Disable turbo mode. |
| `urnet-tools turbo` | Show current turbo state. |
| `urnet-tools eco on/off` | Toggle Eco mode (GC-tuned, full throughput, RAM-constrained boxes). |
| `urnet-tools lowmode on/off` | Toggle Low-Memory mode with dynamic RAM scaling. |
| `urnet-tools ramlogs on/off` | Toggle RAM-disk logging independently. |
| `urnet-tools update` | Upgrade to the latest version. |

---

## 🛠 Usage

### Standard Docker Run (JWT)
Replace `AUTH_CODE_HERE` with your token from [ur.io](https://ur.io).

**Using GHCR (GitHub Registry):**
```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  --log-driver=json-file \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  -e URNETWORK_RAMLOGS=0 \
  -e BUILD='jwt' \
  -e ENABLE_VNSTAT=true \
  -v vnstat_data:/var/lib/vnstat \
  -v /path/to/your/proxy.txt:/app/proxy.txt \
  -p 9001:8080 \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest AUTH_CODE_HERE
```

**Using Docker Hub (Alternative):**
If you experience `denied` errors or rate-limiting on GitHub, use our official Docker Hub mirror:
```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -e BUILD='jwt' \
  -e ENABLE_VNSTAT=true \
  -v /path/to/your/proxy.txt:/app/proxy.txt \
  -p 9001:8080 \
  3cape/urnetwork-3.23-fix:latest AUTH_CODE_HERE
```

### Environment Variables
| Variable | Default | Description |
| :--- | :--- | :--- |
| `BUILD` | `stable` | Set to `jwt` for auth code login, or `stable` for email/pass. |
| `USER_AUTH` | - | Your email (required if BUILD=stable). |
| `PASSWORD` | - | Your password (required if BUILD=stable). |
| `ENABLE_VNSTAT` | `true` | Enables the traffic monitor. |
| `ENABLE_IP_CHECKER` | `false` | Prints your public IP to the logs on startup. |
| `TURBO` | - | Set to `v4` or `v8` to enable turbo mode. Raises the TCP window ceiling from 1 MiB to 4 or 8 MiB, removing the ~100–150 Mbps per-connection limit. Use `v4` on 4–16 GiB boxes, `v8` on 16 GiB+. |
| `URNETWORK_RAMLOGS` | `0` | Set to `1` to redirect provider logs to RAM instead of stdout. Cannot be used with `--log-opt`. See below. |
| `URNETWORK_PROFILE` | - | Advanced: directly sets the provider profile (`lowmem`, `eco`, `turbo-v4`, `turbo-v8`). For turbo, prefer the `TURBO` variable above. `lowmem` reduces IP/transfer buffer sizes and sets GOMEMLIMIT=85% RAM while preserving the 256 KiB contract floor. Cannot be used with `--log-opt` when set to `lowmem`. |

### RAM Logging (Optional)
Setting `URNETWORK_RAMLOGS=1` redirects provider logs to `/dev/shm/urnetwork.log` inside the container — a RAM-backed filesystem — instead of stdout. This keeps log I/O entirely off disk, which can help on weak cloud instances with slow storage.

> **Note:** `URNETWORK_RAMLOGS=1` and `--log-opt` are mutually exclusive. When RAM logging is active, nothing is written to stdout so Docker's log driver has nothing to capture. Remove the `--log-driver` and `--log-opt` flags if you enable this.
>
> **Note:** `URNETWORK_PROFILE=lowmem` also enables RAM logging unconditionally. If you use lowmem mode, remove `--log-driver` and `--log-opt` from your docker run command and use `docker exec` to view logs as shown below.

To view logs live (replace `urfix` with your container name if different):
```bash
docker exec -it urfix tail -f /dev/shm/urnetwork.log
```

RAM logs are capped at 1MB with automatic rotation and are lost when the container restarts.

---

## 📦 Architecture & Build

This repository is designed to be **standalone**.
*   **Base Engine**: UrNetwork v3.23.
*   **Builder**: Go 1.25 (Alpine).
*   **CI/CD**: GitHub Actions automatically builds and pushes multi-arch images to GHCR.
*   **Bridge-Friendly**: Optimized to work within standard Docker bridge networks without requiring `--network host` (though NET_ADMIN capabilities are still recommended).

## ⚠️ Disclaimer
This is a private, custom modification intended for testing and professional provider use. It is not affiliated with the official UrNetwork project.
