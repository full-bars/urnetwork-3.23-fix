# Live heartbeat push (SSE) + per-proxy status in heartbeat

## Context

`/api/heartbeat` (shipped in PR #187) gives the hub a 15s-cadence liveness
signal per node, but only for aggregate fields (Mbps, uptime, last-seen,
system metrics). Two gaps remain:

1. Per-proxy status (up/degraded/dead) and contract win/deny counts still
   only update on the full `/api/report` (5-15min cadence), so the
   dashboard's proxy counts, earning %, and contract win-rate lag far
   behind the Mbps number.
2. Even for fields the heartbeat *does* refresh server-side every 15s, the
   browser dashboard only picks them up on its own independent 30s poll
   timer (`hub/main.go`, `secondsLeft = 30`). There's no push — the hub has
   fresher data sitting ready between polls with no way to tell the browser.

Goal: make the dashboard feel live — proxy status, earning%, contract
win-rate, and Mbps all update as soon as the underlying data arrives at the
hub — without increasing DB write volume (SQLite snapshots/rollups stay on
the existing slow cadence; they're the historical-trend source, not the
live-state source).

## Design

### 1. Heartbeat payload gains compact per-proxy status

`heartbeatReport` (both `hub/main.go` and `provider/bandwidth_reporter.go`)
gets one new field:

```go
type proxyStatus struct {
    ID                string `json:"id"`
    Status            string `json:"status"`
    ContractsAcquired int64  `json:"contracts_acquired"`
    ContractsDenied   int64  `json:"contracts_denied"`
}

// on heartbeatReport:
Proxies []proxyStatus `json:"proxies,omitempty"`
```

`buildHeartbeat` (provider) projects this straight off the same
`buildReport()` call it already makes internally — no new global-state
reads, no new lock contention on the bandwidth map.

On the hub, `store.heartbeat()` merges these into the *existing*
`n.Proxies` slice by matching `ID`: updates `Status`, `ContractsAcquired`,
`ContractsDenied` in place, leaves `TotalRX`/`TotalTX`/`BillRX`/`BillTX`/
`Clients`/`MaxAge` untouched (heartbeats don't carry byte-level detail).
A proxy `ID` present in the heartbeat but absent from `n.Proxies` is
skipped — new proxies are still only created by a full `/api/report`,
same as today. This keeps `store.heartbeat()`'s existing contract: no DB
write, no-op for a node with no prior full report.

### 1a. Sparse: only send proxies that actually changed

Most proxies in a fleet are idle at any given 15s window — sending all of
them every tick makes the heartbeat payload scale with proxy count instead
of with actual activity, which defeats the point of a lightweight signal
at fleet scale (nodes can carry dozens to hundreds of proxies). Instead,
the provider only includes a proxy in `Proxies` if its `Status` or either
contract counter differs from what it sent last tick.

To keep `buildHeartbeat` pure and independently testable (no hidden
cross-tick state), the diffing lives in a separate pure helper that the
already-stateful `runHeartbeatReporter` loop threads across ticks (it
already carries loop-local state like `consecutiveFailures` and
`activeReportURL`, so this fits the existing shape):

```go
// filterChangedProxies returns only the entries in current whose Status or
// contract counters differ from prev (or that have no entry in prev), plus
// the updated snapshot to pass as prev on the next call. proxyStatus is a
// plain comparable struct, so equality is a simple !=.
func filterChangedProxies(prev map[string]proxyStatus, current []proxyStatus) ([]proxyStatus, map[string]proxyStatus) {
    next := make(map[string]proxyStatus, len(current))
    var changed []proxyStatus
    for _, p := range current {
        next[p.ID] = p
        if old, ok := prev[p.ID]; !ok || old != p {
            changed = append(changed, p)
        }
    }
    return changed, next
}
```

`runHeartbeatReporter` holds `prevProxyStatus := map[string]proxyStatus{}`
as a loop-local var (reset each process start, same lifetime as
`consecutiveFailures`), and each tick does
`hb.Proxies, prevProxyStatus = filterChangedProxies(prevProxyStatus, hb.Proxies)`
before marshaling.

Two accepted consequences, worth being explicit about:

- **First tick after a (re)start sends the full proxy list** (`prev` is
  empty, so every entry counts as "changed") — this establishes the hub's
  baseline and is the same shape as a full report, just once. Steady
  state then shrinks to whatever's actually moving.
- **A proxy that disappears from the provider's list (e.g. removed via
  `proxy remove`) is never explicitly retracted via heartbeat** — it just
  stops appearing in `current`, so the hub keeps showing its last-known
  heartbeat state until the next full `/api/report` reconciles the whole
  `n.Proxies` slice. This matches today's behavior (heartbeat never
  deletes), so it's not a new gap, just worth naming.

### 2. Hub broadcasts a "something changed" pulse over SSE

New `GET /api/events` endpoint, unauthenticated (matches `/api/nodes`,
`/api/proxies/top`, etc. — dashboard reads are trusted-network-only today,
no token required).

A small in-process broadcaster owned by `*store`:

```go
type broadcaster struct {
    mu   sync.Mutex
    subs map[chan struct{}]bool
}

func (b *broadcaster) subscribe() chan struct{}      // buffered, size 1
func (b *broadcaster) unsubscribe(ch chan struct{})
func (b *broadcaster) publish()                        // non-blocking send to all subs
```

`handleHeartbeat` and `handleReport` call `s.broadcast.publish()` after a
successful `s.heartbeat(...)` / `s.upsert(...)` — not on 400/401/unknown-node
responses. Each connected browser tab holds one size-1 buffered channel;
`publish()` does a non-blocking send (`select { case ch <- struct{}{}:
default: }`), so a channel that already has a pending notification is left
alone — multiple heartbeats landing in a burst coalesce into one pending
wake for a slow client, and a stuck/dead client can never block the hub.

The SSE handler (`handleEvents`) blocks on its subscribed channel in a
loop, writing a bare `data: refresh\n\n` line (flushed via
`http.Flusher`) on each wake, until the request context is cancelled
(browser navigates away / connection drops), at which point it
unsubscribes and returns. No payload duplication — the event carries no
data, it's purely a "go re-fetch" signal.

### 3. Browser

```js
var es = new EventSource('/api/events');
es.onmessage = function() { refreshDashboard(); };
```

`refreshDashboard()` already guards against overlapping fetches via its
existing `refreshing` flag, so a burst of SSE pulses just coalesces into
one `/api/nodes` fetch. `EventSource` retries on drop natively (browser
built-in exponential backoff) — no custom reconnect logic needed, which
matters given the flaky-Detroit-link concern from the heartbeat work.

The existing 30s poll timer **stays as-is**, unchanged, as a backstop —
e.g. an intermediate proxy that buffers or strips SSE would otherwise
leave the dashboard silently stale forever. This is cheap insurance: worst
case it's a redundant fetch that `refreshDashboard()` already dedupes
against.

### 4. Not in scope (future work, per user's stated direction)

- WebSocket transport — user wants to start with SSE, may expand later.
  The `publish()`-signals-a-refetch design doesn't block that: a future
  WS handler could subscribe to the same broadcaster and push real payload
  instead of a bare signal, without touching `/api/nodes` or the SSE path.
- Any change to DB write cadence or full-report interval.
- Any change to what data full `/api/report` carries.

## Testing

- `filterChangedProxies`: unchanged entry (same status/counters as prev) is
  excluded; changed status, changed acquired, or changed denied each
  trigger inclusion independently; an entry absent from `prev` is always
  included (first-tick/new-proxy case); the returned `next` map reflects
  `current` exactly regardless of what was filtered, so the following
  call's diff is correct.
- `store.heartbeat()`: extend existing tests — per-proxy status merge for
  a known ID, a heartbeat proxy ID with no match in `n.Proxies` is
  skipped (no panic, no partial entry created), existing byte counters
  (`TotalRX` etc.) survive a heartbeat untouched.
- `broadcaster`: unit test with 2+ subscriber channels — `publish()`
  delivers to all subscribed channels; a channel already holding a
  pending notification doesn't block or panic on a second `publish()`
  (non-blocking send verified via a channel with no reader draining it);
  `unsubscribe` stops further delivery.
- `handleHeartbeat` / `handleReport`: assert the broadcaster fires (via a
  subscribed test channel receiving) on a 204/202-success response, and
  does *not* fire on 400/401.
- `handleEvents`: `httptest.NewServer` + manual read of the
  `text/event-stream` response body, publish from the test, assert a
  `data: refresh` line arrives; assert the handler returns (unsubscribes)
  when the request context is cancelled.
