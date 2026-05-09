# UrNetwork v3.23 Fix (Custom Build)

This is a high-performance, high-visibility fork of the **UrNetwork Connect** provider, based on the stable **v3.23** engine. It combines the latest protocol optimizations with surgical improvements for professional providers managing large proxy lists.

## 🚀 Key Improvements

### 1. High-Signal Monitoring (Promoted Logs)
In standard builds, connection handshake logs are hidden behind debug flags, leading to "silent" nodes. In this version:
*   **[net][s]select (Serial Select)**: Promoted from Debug Level 2 to **Standard INFO level**. You will see exactly one clean line every time a proxy connection is successfully established.
*   **Noise Reduction**: Parallel selection logs ([net][p]) remain silenced, ensuring that even with high-scale proxy lists, your logs stay readable and useful.

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

### 4. Advanced Optimization (New)
For servers with limited resources (e.g., 1GB RAM) or high-volume nodes sensitive to disk I/O latency:
*   **Lowmode**: A specialized profile that reduces buffer sizes, tunes the Go Garbage Collector (`GOGC=50`), and sets a dynamic `GOMEMLIMIT` (85% of system RAM).
*   **RAM Logging**: Redirects all provider logs to `/dev/shm` (Linux RAM disk) with a 1MB rotation cap. This eliminates disk I/O overhead and is ideal for weak cloud instances.

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
| `urnet-tools lowmode on/off` | Toggle Low-Memory mode with dynamic RAM scaling. |
| `urnet-tools ramlogs on/off` | Toggle RAM-disk logging independently. |
| `urnet-tools update` | Upgrade to the latest version. |

---

## 🛠 Usage

### Standard Docker Run (JWT)
Replace `AUTH_CODE_HERE` with your token from [ur.io](https://ur.io).

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
  -v vnstat_data:/var/lib/vnstat \
  -v /path/to/your/proxy.txt:/app/proxy.txt \
  -p 9001:8080 \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest AUTH_CODE_HERE
```

### Environment Variables
| Variable | Default | Description |
| :--- | :--- | :--- |
| `BUILD` | `stable` | Set to `jwt` for auth code login, or `stable` for email/pass. |
| `USER_AUTH` | - | Your email (required if BUILD=stable). |
| `PASSWORD` | - | Your password (required if BUILD=stable). |
| `ENABLE_VNSTAT` | `true` | Enables the traffic monitor. |
| `ENABLE_IP_CHECKER` | `false` | Prints your public IP to the logs on startup. |

---

## 📦 Architecture & Build

This repository is designed to be **standalone**.
*   **Base Engine**: UrNetwork v3.23.
*   **Builder**: Go 1.25 (Alpine).
*   **CI/CD**: GitHub Actions automatically builds and pushes multi-arch images to GHCR.
*   **Bridge-Friendly**: Optimized to work within standard Docker bridge networks without requiring `--network host` (though NET_ADMIN capabilities are still recommended).

## ⚠️ Disclaimer
This is a private, custom modification intended for testing and professional provider use. It is not affiliated with the official UrNetwork project.
