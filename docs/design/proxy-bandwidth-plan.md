# Proxy Bandwidth Tracking Implementation Plan

## Objective
Implement per-proxy bandwidth observability tracking both total platform traffic (transport overhead, retries, etc.) and billable payload traffic. The metrics will be emitted to a dedicated snapshot file (`proxy_traffic.state`) formatted as a clean, descendingly-sorted ASCII dashboard.

## 1. Health Registry (`connect/proxy_health.go`)
- **Data Structures**: Add a `ProxyBandwidth` struct using lock-free `atomic.Uint64` fields (`TotalRx`, `TotalTx`, `BillableRx`, `BillableTx`). Embed this inside the `proxyHealth` struct.
- **Registration Hook**: Create `func RegisterProxyBandwidth(index int) *ProxyBandwidth`. This allows the transport and NAT components to grab a direct pointer to the counters, enabling extremely fast lock-free atomic updates on the packet hot-paths without bottlenecking the registry.
- **Reporting**: Extend the `ProxyHealthReport` struct to return a mapped snapshot of bandwidth (`map[string]ProxyBandwidth`), where the key is the formatted proxy string (e.g. `proxy[50] (1.2.3.4)`).

## 2. Total Traffic Hooks (`connect/transport.go`)
- When a proxy connection is established in `runH1` and `runH3`, call `RegisterProxyBandwidth(idx)` to grab the counter pointer.
- Inside the respective `readLoop` and `writeLoop` functions, use `atomic.AddUint64` to increment `TotalRx` and `TotalTx` by `len(message)` for every single packet sent/received over the websocket/quic stream.

## 3. Billable Traffic Hooks (`connect/ip.go` & `provider/main.go`)
- Pass the `*ProxyBandwidth` pointer into `LocalUserNat` and `RemoteUserNatProvider` when they are instantiated inside `provider/main.go`'s `provideWithProxy` loop.
- Inside `RemoteUserNatProvider.Receive` and `ClientReceive` (and the Local equivalents), use `atomic.AddUint64` to increment `BillableTx` and `BillableRx` by `len(packet)`. This accurately tracks the raw user payload successfully relayed.

## 4. Formatting and Output (`provider/proxy_health_log.go` & `provider/main.go`)
- **Formatting**: Create `formatTrafficStateFile(report ProxyHealthReport, now time.Time) string`.
- **Sorting**: Write a custom sorting function to order the `ProxyBandwidth` map entries descendingly by `(BillableTx + BillableRx)`.
- **Output**: Format the output into the approved ASCII table style with human-readable byte sizes (KB, MB, GB).
- **Disk I/O**: Add `writeProxyTrafficState` that creates `proxy_traffic.state.tmp` and renames it over `proxy_traffic.state` atomically during the hourly/periodic heartbeat in `provider/main.go`.
