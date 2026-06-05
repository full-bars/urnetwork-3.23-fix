# 📈 Why Proxy Usage Exceeds Dashboard Credits: Understanding Traffic Amplification

A common observation for professional providers is that the bandwidth consumed by their proxy list is significantly higher than the "Billable Traffic" credited in the URNetwork dashboard. This discrepancy can be substantial, often representing a significant multiple of the billable total.

While some of this is an intentional trade-off for performance, a significant portion is driven by a known "bandwidth leak" in the upstream protocol handling that remains unoptimized.

## 🏁 1. The Multi-Race Strategy (The "Race Tax")
To ensure the lowest possible latency, URN does not just pick one proxy and wait. Instead, it uses a **Multi-Race** mechanism for every new connection.

*   **Parallel Transmission:** When a connection starts, the node sends the initial packets to up to **10-12 providers simultaneously**.
*   **The Winner:** The first provider to respond successfully "wins" the contract and handles the rest of the connection.
*   **The Losers:** The other 9-11 providers still processed those initial packets. From their perspective, bandwidth was spent. From the URN perspective, those bytes are **non-billable** because they didn't win the contract.
*   **Amplification Factor:** If a race involves 10 proxies, you are effectively paying a 10x "tax" on the first few packets of every single connection.

## 💧 2. Upstream Failure Leaks (The Unfixed Issue)
In the standard upstream version of the protocol, there is a known inefficiency in how failures are handled. 
*   **Outage Spirals:** When a provider fails, the upstream code often triggers aggressive re-racing and redundant packet sends that aren't properly throttled.
*   **The "Leak":** During these periods, the protocol can move massive amounts of data across your proxies while attempting to stabilize, but because the connections are failing before a contract can be fully finalized, almost none of that data is credited to the provider. 

## ⏱️ 3. Sequence Idle Resets
URN maintains a `SequenceIdleTimeout` (default: 120 seconds). If a specific connection (like a background app sync) goes silent for more than two minutes, URN clears the provider assignment to save resources.

The next time that app wakes up to send data—even if it's just a few kilobytes—the node **triggers a brand new 10-way race**. For mobile devices or background services that "chatter" every few minutes, this "Race Tax" is paid repeatedly throughout the day.

## 🎭 4. Provider Evaluation (The "Audition")
Nodes are constantly looking for the best possible providers to keep in their active "Quality" and "Speed" windows. 
*   **Evaluation Pings:** Every 15 seconds, nodes check their window sizes and may fire off **IpPing** packets to proxies that aren't even currently moving data.
*   **Health Checks:** These pings are necessary to ensure a proxy is actually alive and fast before it's allowed to enter a race. This "audition" traffic is strictly control overhead and is never billable.

## 🎛️ 5. Control Plane Overhead
The protocol requires significant "paperwork" to keep connections secure and accounted for. 
*   **Negotiation:** Opening contracts, exchanging HMAC signatures, and performing handshakes all consume bandwidth.
*   **Non-Billable ID:** Messages sent to the "Control ID" (ID 0) are explicitly excluded from contract byte counts. 
*   **Protocol Weight:** For small web requests, the ratio of control "paperwork" to actual data is very high.

## 🔁 6. Retransmission & Reliability
URN runs over UDP for performance, but it provides a reliable stream to the user.
*   **Duplicate Sends:** If a packet is lost or delayed, the node will retransmit it.
*   **The Gap:** Your proxy bills you for every byte sent (including the retry). URN only credits you for the "effective" bytes that were successfully acknowledged once.

## 🎯 Conclusion
The gap between proxy consumption and dashboard credits is caused by a mix of intentional "racing" for performance and unoptimized failure handling in the upstream code. While custom builds can mitigate some of these leaks, the nature of the protocol ensures that proxy usage will always be significantly higher than billable credits, especially during periods of network instability.
