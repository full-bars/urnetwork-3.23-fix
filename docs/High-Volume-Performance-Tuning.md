# High-Volume Performance Tuning

This guide explains the architectural tuning parameters available in the `urnetwork-3.23-fix` fork. These settings are designed for high-volume networking environments where standard protocol defaults may lead to signaling congestion or connection instability.

> **Note:** These optimizations are experimental. High-volume environments vary significantly based on network latency, CPU overhead, and provider capacity.

---

## 1. Signaling & Contract Management

In the URnetwork protocol, a **Contract** represents the authorized bandwidth quota for a connection. Managing these contracts efficiently is critical for stability in high-volume environments.

### Initial Contract Size (`InitialContractTransferByteCount`)
*   **Default:** 16 KiB (Upstream) / 2 MiB (Fix-4)
*   **Rationale:** Standard defaults are optimized for anti-spam and low-volume users. In high-volume environments, a small initial quota causes an immediate "signaling storm" as thousands of connections simultaneously request refills.
*   **Tuning:** Increasing this value reduces the initial signaling frequency, giving the provider more "breathing room" during connection spikes.

### Signaling Timeout (`CreateContractTimeout`)
*   **Default:** 30s (Upstream) / 60s (Fix-4)
*   **Rationale:** Under heavy load, the Out-of-Band (OOB) signaling layer can become congested. If a response isn't received within the timeout window, the sequence will exit with a `could not create contract` error.
*   **Tuning:** Extending this timeout allows the node to survive temporary signaling backlogs without dropping active connections.

### Contract Fill Fraction (`ContractFillFraction`)
*   **Default:** 0.8 (Upstream) / 0.7 (Fix-4)
*   **Rationale:** This determines when a client requests a "refill" (a new contract). At 0.8, the client waits until 80% of the current quota is used. 
*   **Tuning:** Lowering this to 0.7 or 0.6 starts the refill process earlier. This provides a larger "safety buffer" of remaining data to burn while waiting for the signaling layer to respond.

---

## 2. Memory & Throughput Optimizations

### Accordion TCP Scaling
*   **Mechanism:** Connections start with a minimal **4KB** TCP window to conserve RAM. As throughput increases, the window dynamically expands (up to **1MB**).
*   **Efficiency:** If a connection becomes idle, the window automatically shrinks back to 4KB after 30 seconds. This allows the provider to manage thousands of concurrent connections with a significantly lower memory footprint than fixed-window implementations.

### Message Pool Expansion
*   **Mechanism:** We utilize pre-allocated pools for 16KB, 32KB, and 64KB message frames.
*   **Benefit:** In high-throughput scenarios, this minimizes Garbage Collector (GC) pressure by reusing memory buffers instead of constantly allocating/deallocating frames.

---

## 3. Troubleshooting Signaling Issues

If you continue to see `exit could not create contract` or `oob err = Timeout` in your logs:

1.  **Monitor Connection Concurrency:** Check if your system's `netstat` shows an unexpected spike in `ESTABLISHED` connections.
2.  **Evaluate Latency:** High latency to the signaling provider will require a lower `ContractFillFraction` to compensate for the RTT (Round Trip Time).
3.  **Check CPU Pressure:** If the CPU is pegged at 100%, the signaling thread may be starved, leading to artificial timeouts.
