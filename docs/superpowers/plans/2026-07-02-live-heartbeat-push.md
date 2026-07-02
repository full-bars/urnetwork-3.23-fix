# Live Heartbeat Push (SSE) + Per-Proxy Status Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the hub dashboard feel live — proxy status, contract win/deny counts, and Mbps all update in the browser as soon as the underlying data lands at the hub, via a sparse per-proxy heartbeat payload and a Server-Sent Events push instead of a fixed 30s poll.

**Architecture:** Extend the existing `/api/heartbeat` payload with a compact, diff-filtered per-proxy status array (id/status/contract counters only, no byte detail); merge it into the hub's in-memory node state (no DB write, same as today's heartbeat). Add an in-process pub/sub `broadcaster` the hub pings after every successful heartbeat/report; a new `GET /api/events` SSE endpoint blocks on that broadcaster and tells connected browser tabs to re-fetch `/api/nodes`. The existing 30s poll stays as a backstop.

**Tech Stack:** Go (`net/http`, `encoding/json`, `sync`), no new dependencies. Browser: native `EventSource` (SSE), no new JS dependencies.

## Global Constraints

- json tags on `proxyStatus` (both hub and provider copies) must match exactly — this is the same cross-package contract `heartbeatReport` already has (see existing comment in both files: "json tags must stay in sync").
- No new DB writes anywhere in this plan. `store.heartbeat()` must remain a pure in-memory update, same contract as today (no-op for unknown node, never calls `persist()`).
- `broadcaster.publish()` must be safe to call on a nil `*broadcaster` receiver, since dozens of existing hub tests construct `&store{...}` literals directly without going through `openStore` (which is the only place that will set the `broadcast` field going forward).
- Follow TDD: write the failing test, watch it fail (often a compile failure for a missing type/field — that's an acceptable RED, consistent with how `heartbeatReport`/`handleHeartbeat` were introduced in PR #187), then implement.

---

### Task 1: Hub — `proxyStatus` type + `store.heartbeat()` merge

**Files:**
- Modify: `hub/main.go:98-110` (add `proxyStatus` type, add `Proxies` field to `heartbeatReport`)
- Modify: `hub/main.go:221-247` (`store.heartbeat()` — merge per-proxy status)
- Test: `hub/main_test.go` (append new tests near the existing `TestStoreHeartbeat*` tests, after line 663 or wherever they currently end)

**Interfaces:**
- Produces: `type proxyStatus struct { ID string; Status string; ContractsAcquired int64; ContractsDenied int64 }` (all with matching json tags `id`/`status`/`contracts_acquired`/`contracts_denied`)
- Produces: `heartbeatReport.Proxies []proxyStatus` (json tag `proxies,omitempty`)
- Consumes: existing `proxyReport` struct (`hub/main.go:55-67`) — has matching `ID`, `Status`, `ContractsAcquired`, `ContractsDenied` fields to merge into.

- [ ] **Step 1: Write the failing tests**

Append to `hub/main_test.go`:

```go
func TestStoreHeartbeatMergesKnownProxyStatus(t *testing.T) {
	s := &store{
		Nodes: make(map[string]*nodeState),
		rates: make(map[string]*nodeRate),
	}
	now := time.Now().UTC()
	s.upsert("n1", &nodeState{
		NodeID: "n1", Timestamp: now,
		Proxies: []proxyReport{
			{ID: "p1", Status: "up", TotalRX: 1000, ContractsAcquired: 2, ContractsDenied: 1},
			{ID: "p2", Status: "up", TotalRX: 2000},
		},
	})

	ok := s.heartbeat("n1", &heartbeatReport{
		NodeID: "n1", Timestamp: now.Add(10 * time.Second),
		Proxies: []proxyStatus{
			{ID: "p1", Status: "degraded", ContractsAcquired: 3, ContractsDenied: 2},
		},
	})
	if !ok {
		t.Fatalf("heartbeat for known node returned false")
	}

	p1 := s.Nodes["n1"].Proxies[0]
	if p1.Status != "degraded" {
		t.Errorf("p1 status = %q, want %q", p1.Status, "degraded")
	}
	if p1.ContractsAcquired != 3 || p1.ContractsDenied != 2 {
		t.Errorf("p1 contracts = %d/%d, want 3/2", p1.ContractsAcquired, p1.ContractsDenied)
	}
	if p1.TotalRX != 1000 {
		t.Errorf("p1 TotalRX = %d, want 1000 (heartbeat must not touch byte counters)", p1.TotalRX)
	}

	p2 := s.Nodes["n1"].Proxies[1]
	if p2.Status != "up" || p2.ContractsAcquired != 0 {
		t.Errorf("p2 was modified by a heartbeat that didn't mention it: %+v", p2)
	}
}

func TestStoreHeartbeatSkipsUnknownProxyID(t *testing.T) {
	s := &store{
		Nodes: make(map[string]*nodeState),
		rates: make(map[string]*nodeRate),
	}
	now := time.Now().UTC()
	s.upsert("n1", &nodeState{
		NodeID: "n1", Timestamp: now,
		Proxies: []proxyReport{{ID: "p1", Status: "up"}},
	})

	ok := s.heartbeat("n1", &heartbeatReport{
		NodeID: "n1", Timestamp: now.Add(10 * time.Second),
		Proxies: []proxyStatus{{ID: "ghost-proxy", Status: "up"}},
	})
	if !ok {
		t.Fatalf("heartbeat for known node returned false")
	}
	if len(s.Nodes["n1"].Proxies) != 1 {
		t.Errorf("unknown proxy ID in heartbeat created an entry: %+v", s.Nodes["n1"].Proxies)
	}
	if s.Nodes["n1"].Proxies[0].ID != "p1" {
		t.Errorf("existing proxy was replaced: %+v", s.Nodes["n1"].Proxies[0])
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd hub && go test . -run TestStoreHeartbeatMergesKnownProxyStatus -v`
Expected: FAIL — compile error, `undefined: proxyStatus` (the type doesn't exist yet) and/or `unknown field Proxies in struct literal of type heartbeatReport`.

- [ ] **Step 3: Add the `proxyStatus` type and `Proxies` field**

In `hub/main.go`, replace the `heartbeatReport` block (currently lines 98-110):

```go
// proxyStatus is the compact per-proxy fields a heartbeat carries — status
// and contract counters only, no byte-level detail. json tags must match
// provider/bandwidth_reporter.go's proxyStatus.
type proxyStatus struct {
	ID                string `json:"id"`
	Status            string `json:"status"`
	ContractsAcquired int64  `json:"contracts_acquired"`
	ContractsDenied   int64  `json:"contracts_denied"`
}

// heartbeatReport is the lightweight, high-frequency (10-30s) counterpart to
// bandwidthReport (provider/bandwidth_reporter.go): no byte-level detail,
// just enough to keep the dashboard's "last seen", Mbps rate, and per-proxy
// status/contracts live between the much less frequent full /api/report
// ticks. Never persisted to DB. Proxies is sparse — the provider only
// includes entries that changed since its last heartbeat tick (see
// filterChangedProxies in provider/bandwidth_reporter.go), so most ticks
// carry an empty or near-empty slice.
type heartbeatReport struct {
	NodeID    string        `json:"node_id"`
	Timestamp time.Time     `json:"ts"`
	Uptime    float64       `json:"uptime"`
	TotalRX   uint64        `json:"rx"`
	TotalTX   uint64        `json:"tx"`
	Clients   int64         `json:"clients"`
	System    systemMetrics `json:"sys"`
	Proxies   []proxyStatus `json:"proxies,omitempty"`
}
```

- [ ] **Step 4: Merge per-proxy status in `store.heartbeat()`**

In `hub/main.go`, in `store.heartbeat()` (currently lines 221-247), insert the merge loop after the rate-tracking block and before the final `n.Timestamp = hb.Timestamp` assignment:

```go
func (s *store) heartbeat(nodeID string, hb *heartbeatReport) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, ok := s.Nodes[nodeID]
	if !ok {
		return false
	}

	if prev, ok := s.rates[nodeID]; ok {
		dt := hb.Timestamp.Sub(prev.ts).Seconds()
		if dt > 1 && hb.TotalRX >= prev.rx && hb.TotalTX >= prev.tx {
			prev.mbpsRx = float64(hb.TotalRX-prev.rx) / dt * 8 / 1_000_000
			prev.mbpsTx = float64(hb.TotalTX-prev.tx) / dt * 8 / 1_000_000
		}
		prev.ts = hb.Timestamp
		prev.rx = hb.TotalRX
		prev.tx = hb.TotalTX
	} else {
		s.rates[nodeID] = &nodeRate{ts: hb.Timestamp, rx: hb.TotalRX, tx: hb.TotalTX}
	}

	if len(hb.Proxies) > 0 {
		byID := make(map[string]int, len(n.Proxies))
		for i, p := range n.Proxies {
			byID[p.ID] = i
		}
		for _, ps := range hb.Proxies {
			if i, ok := byID[ps.ID]; ok {
				n.Proxies[i].Status = ps.Status
				n.Proxies[i].ContractsAcquired = ps.ContractsAcquired
				n.Proxies[i].ContractsDenied = ps.ContractsDenied
			}
		}
	}

	n.Timestamp = hb.Timestamp
	n.Uptime = hb.Uptime
	n.System = hb.System
	return true
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd hub && go test . -run TestStoreHeartbeat -v`
Expected: PASS for all `TestStoreHeartbeat*` tests, including the two new ones.

- [ ] **Step 6: Full package check and commit**

Run: `cd hub && go build ./... && go vet ./... && go test ./... -timeout 60s`
Expected: all green.

```bash
git add hub/main.go hub/main_test.go
git commit -m "feat(hub): merge per-proxy status/contracts from heartbeat"
```

---

### Task 2: Provider — matching `proxyStatus` + `buildHeartbeat` projection

**Files:**
- Modify: `provider/bandwidth_reporter.go:54-67` (add `proxyStatus` type, add `Proxies` field to `heartbeatReport`)
- Modify: `provider/bandwidth_reporter.go:397-421` (`buildHeartbeat` — populate `Proxies`)
- Test: `provider/bandwidth_reporter_test.go` (append near the existing `TestBuildHeartbeat_NoProxiesConfigured`)

**Interfaces:**
- Consumes: `proxyReport` (provider's own copy, `provider/bandwidth_reporter.go:34-46`) — has `ID`, `Status`, `ContractsAcquired`, `ContractsDenied`.
- Produces: `type proxyStatus struct { ID string; Status string; ContractsAcquired int64; ContractsDenied int64 }` — **json tags must exactly match Task 1's hub-side `proxyStatus`**.
- Produces: `heartbeatReport.Proxies []proxyStatus`.

- [ ] **Step 1: Write the failing test**

Append to `provider/bandwidth_reporter_test.go`:

```go
// TestBuildHeartbeat_ProjectsProxyStatus is the deterministic case for the
// new Proxies field: with no proxies registered in the global bandwidth
// map (same setup as TestBuildHeartbeat_NoProxiesConfigured — buildReport
// itself isn't independently testable since it reads global state), the
// projection must produce an empty slice rather than nil-panicking or
// carrying stale data.
func TestBuildHeartbeat_ProjectsProxyStatus(t *testing.T) {
	start := time.Now().Add(-1 * time.Minute)

	hb := buildHeartbeat("test-node", "test-host", start)

	if len(hb.Proxies) != 0 {
		t.Errorf("Proxies = %+v, want empty with no proxies configured", hb.Proxies)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd provider && go test . -run TestBuildHeartbeat_ProjectsProxyStatus -v`
Expected: FAIL — compile error, `hb.Proxies undefined (type heartbeatReport has no field or method Proxies)`.

- [ ] **Step 3: Add the `proxyStatus` type and `Proxies` field**

In `provider/bandwidth_reporter.go`, replace the `heartbeatReport` block (currently lines 54-67):

```go
// proxyStatus is the compact per-proxy fields a heartbeat carries — status
// and contract counters only, no byte-level detail. json tags must match
// hub/main.go's proxyStatus.
type proxyStatus struct {
	ID                string `json:"id"`
	Status            string `json:"status"`
	ContractsAcquired int64  `json:"contracts_acquired"`
	ContractsDenied   int64  `json:"contracts_denied"`
}

// heartbeatReport is the lightweight, high-frequency (10-30s) counterpart to
// bandwidthReport: no byte-level detail, just enough for the hub to keep
// "last seen", the Mbps rate, and per-proxy status/contracts live between
// the much less frequent full /api/report ticks (5-15m default). Its json
// tags must stay in sync with hub/main.go's heartbeatReport. Proxies is
// sparse by the time it's marshaled — see filterChangedProxies, applied by
// runHeartbeatReporter before sending.
type heartbeatReport struct {
	NodeID    string        `json:"node_id"`
	Timestamp time.Time     `json:"ts"`
	Uptime    float64       `json:"uptime"`
	TotalRX   uint64        `json:"rx"`
	TotalTX   uint64        `json:"tx"`
	Clients   int64         `json:"clients"`
	System    systemMetrics `json:"sys"`
	Proxies   []proxyStatus `json:"proxies,omitempty"`
}
```

- [ ] **Step 4: Populate `Proxies` in `buildHeartbeat`**

In `provider/bandwidth_reporter.go`, replace `buildHeartbeat` (currently lines 397-421):

```go
// buildHeartbeat is buildReport's lightweight counterpart: it reuses the
// same proxy-bandwidth aggregation (buildReport already talks to the global
// bandwidth map, proxy health snapshot, and contract metrics) but projects
// down to just the top-level counters plus a compact per-proxy status/
// contracts list, since a heartbeat carries no byte-level per-proxy detail.
func buildHeartbeat(nodeID, host string, startTime time.Time) heartbeatReport {
	report := buildReport(nodeID, host, startTime)

	var totalRX, totalTX uint64
	var clients int64
	proxies := make([]proxyStatus, 0, len(report.Proxies))
	for _, p := range report.Proxies {
		totalRX += p.TotalRX
		totalTX += p.TotalTX
		clients += p.Clients
		proxies = append(proxies, proxyStatus{
			ID:                p.ID,
			Status:            p.Status,
			ContractsAcquired: p.ContractsAcquired,
			ContractsDenied:   p.ContractsDenied,
		})
	}

	return heartbeatReport{
		NodeID:  nodeID,
		Uptime:  report.Uptime,
		TotalRX: totalRX,
		TotalTX: totalTX,
		Clients: clients,
		System:  report.System,
		Proxies: proxies,
	}
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd provider && go test . -run TestBuildHeartbeat -v`
Expected: PASS for both `TestBuildHeartbeat_NoProxiesConfigured` and `TestBuildHeartbeat_ProjectsProxyStatus`.

- [ ] **Step 6: Full package check and commit**

Run: `cd provider && go build ./... && go vet ./... && go test . -timeout 60s`
Expected: all green.

```bash
git add provider/bandwidth_reporter.go provider/bandwidth_reporter_test.go
git commit -m "feat(provider): project per-proxy status/contracts into heartbeat"
```

---

### Task 3: Provider — `filterChangedProxies` sparse-diff helper

**Files:**
- Modify: `provider/bandwidth_reporter.go` (add `filterChangedProxies`, placed right after `buildHeartbeat`)
- Test: `provider/bandwidth_reporter_test.go` (append after the Task 2 test)

**Interfaces:**
- Consumes: `proxyStatus` (Task 2).
- Produces: `func filterChangedProxies(prev map[string]proxyStatus, current []proxyStatus) (changed []proxyStatus, next map[string]proxyStatus)` — consumed by Task 4's loop wiring.

- [ ] **Step 1: Write the failing tests**

Append to `provider/bandwidth_reporter_test.go`:

```go
func TestFilterChangedProxies_UnchangedEntryExcluded(t *testing.T) {
	prev := map[string]proxyStatus{
		"p1": {ID: "p1", Status: "up", ContractsAcquired: 5, ContractsDenied: 1},
	}
	current := []proxyStatus{
		{ID: "p1", Status: "up", ContractsAcquired: 5, ContractsDenied: 1},
	}

	changed, next := filterChangedProxies(prev, current)

	if len(changed) != 0 {
		t.Errorf("changed = %+v, want empty for an unchanged proxy", changed)
	}
	if next["p1"] != current[0] {
		t.Errorf("next[p1] = %+v, want %+v", next["p1"], current[0])
	}
}

func TestFilterChangedProxies_StatusChangeIncluded(t *testing.T) {
	prev := map[string]proxyStatus{
		"p1": {ID: "p1", Status: "up"},
	}
	current := []proxyStatus{
		{ID: "p1", Status: "dead"},
	}

	changed, _ := filterChangedProxies(prev, current)

	if len(changed) != 1 || changed[0].Status != "dead" {
		t.Errorf("changed = %+v, want a single dead-status entry", changed)
	}
}

func TestFilterChangedProxies_ContractCounterChangeIncluded(t *testing.T) {
	prev := map[string]proxyStatus{
		"p1": {ID: "p1", Status: "up", ContractsAcquired: 5},
	}
	current := []proxyStatus{
		{ID: "p1", Status: "up", ContractsAcquired: 6},
	}

	changed, _ := filterChangedProxies(prev, current)

	if len(changed) != 1 || changed[0].ContractsAcquired != 6 {
		t.Errorf("changed = %+v, want a single entry with ContractsAcquired=6", changed)
	}
}

func TestFilterChangedProxies_UnknownEntryAlwaysIncluded(t *testing.T) {
	prev := map[string]proxyStatus{}
	current := []proxyStatus{
		{ID: "p1", Status: "up"},
		{ID: "p2", Status: "up"},
	}

	changed, next := filterChangedProxies(prev, current)

	if len(changed) != 2 {
		t.Errorf("changed = %+v, want both entries included on first sighting", changed)
	}
	if len(next) != 2 {
		t.Errorf("next = %+v, want both entries recorded for the following diff", next)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd provider && go test . -run TestFilterChangedProxies -v`
Expected: FAIL — compile error, `undefined: filterChangedProxies`.

- [ ] **Step 3: Implement `filterChangedProxies`**

In `provider/bandwidth_reporter.go`, add immediately after `buildHeartbeat`:

```go
// filterChangedProxies returns only the entries in current whose Status or
// contract counters differ from prev (or that have no entry in prev), plus
// the updated snapshot to pass as prev on the next call. proxyStatus is a
// plain comparable struct (string/string/int64/int64 fields), so equality
// is a simple !=. Most proxies in a fleet are idle at any given tick, so
// this keeps the heartbeat's per-proxy payload proportional to what's
// actually changing rather than to total proxy count.
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

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd provider && go test . -run TestFilterChangedProxies -v`
Expected: PASS for all four tests.

- [ ] **Step 5: Full package check and commit**

Run: `cd provider && go build ./... && go vet ./... && go test . -timeout 60s`
Expected: all green.

```bash
git add provider/bandwidth_reporter.go provider/bandwidth_reporter_test.go
git commit -m "feat(provider): add filterChangedProxies sparse-diff helper"
```

---

### Task 4: Provider — wire sparse-diff into `runHeartbeatReporter`

**Files:**
- Modify: `provider/bandwidth_reporter.go:431-onward` (`runHeartbeatReporter` loop)

**Interfaces:**
- Consumes: `filterChangedProxies` (Task 3), `buildHeartbeat` (Task 2).
- Produces: no new exported surface — this task only changes what `runHeartbeatReporter` sends over the wire. Not independently unit-testable (the loop itself has never had direct tests — same as `runBandwidthReporter`); verified via build/vet/test plus the existing `nextHeartbeatInterval`/`buildHeartbeat`/`filterChangedProxies` unit tests still passing.

- [ ] **Step 1: Add loop-local state and call the filter**

In `provider/bandwidth_reporter.go`, in `runHeartbeatReporter`, find this block:

```go
	var client *http.Client
	var activeReportURL string
	consecutiveFailures := 0
	activeInterval := baseInterval
```

Replace with:

```go
	var client *http.Client
	var activeReportURL string
	consecutiveFailures := 0
	activeInterval := baseInterval
	prevProxyStatus := map[string]proxyStatus{}
```

Then find:

```go
		activeHost := resolveNodeName(host)
		hb := buildHeartbeat(nodeID, activeHost, startTime)

		body, err := json.Marshal(hb)
```

Replace with:

```go
		activeHost := resolveNodeName(host)
		hb := buildHeartbeat(nodeID, activeHost, startTime)
		hb.Proxies, prevProxyStatus = filterChangedProxies(prevProxyStatus, hb.Proxies)

		body, err := json.Marshal(hb)
```

- [ ] **Step 2: Build, vet, and run the full provider test suite**

Run: `cd provider && go build ./... && go vet ./... && go test . -timeout 60s`
Expected: all green — no test directly exercises the loop, so this step is confirming nothing else broke.

- [ ] **Step 3: Commit**

```bash
git add provider/bandwidth_reporter.go
git commit -m "feat(provider): only send changed proxy status in each heartbeat tick"
```

---

### Task 5: Hub — `broadcaster` fan-out type

**Files:**
- Create: `hub/broadcaster.go`
- Test: `hub/broadcaster_test.go`

**Interfaces:**
- Produces: `type broadcaster struct{...}`, `func newBroadcaster() *broadcaster`, `func (b *broadcaster) subscribe() chan struct{}`, `func (b *broadcaster) unsubscribe(ch chan struct{})`, `func (b *broadcaster) publish()` — `publish` must be safe on a nil `*broadcaster` receiver (see Global Constraints).
- Consumed by: Task 6 (`store.broadcast` field + handler wiring), Task 7 (`handleEvents`).

- [ ] **Step 1: Write the failing tests**

Create `hub/broadcaster_test.go`:

```go
package main

import (
	"testing"
	"time"
)

func TestBroadcasterPublishDeliversToAllSubscribers(t *testing.T) {
	b := newBroadcaster()
	ch1 := b.subscribe()
	ch2 := b.subscribe()

	b.publish()

	select {
	case <-ch1:
	default:
		t.Errorf("ch1 did not receive a notification")
	}
	select {
	case <-ch2:
	default:
		t.Errorf("ch2 did not receive a notification")
	}
}

func TestBroadcasterPublishNonBlockingWithPendingNotification(t *testing.T) {
	b := newBroadcaster()
	ch := b.subscribe()

	done := make(chan struct{})
	go func() {
		b.publish()
		b.publish() // ch's buffer is already full after the first publish; must not block
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publish blocked with a full subscriber buffer")
	}

	select {
	case <-ch:
	default:
		t.Errorf("ch did not receive the coalesced notification")
	}
}

func TestBroadcasterUnsubscribeStopsDelivery(t *testing.T) {
	b := newBroadcaster()
	ch := b.subscribe()
	b.unsubscribe(ch)

	b.publish()

	select {
	case <-ch:
		t.Errorf("unsubscribed channel received a notification")
	default:
	}
}

func TestBroadcasterPublishOnNilReceiverIsSafe(t *testing.T) {
	var b *broadcaster
	b.publish() // must not panic — many existing store tests build &store{} without a broadcaster
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd hub && go test . -run TestBroadcaster -v`
Expected: FAIL — compile error, `undefined: newBroadcaster`.

- [ ] **Step 3: Implement `broadcaster`**

Create `hub/broadcaster.go`:

```go
package main

import "sync"

// broadcaster is a minimal fan-out signal for the dashboard's live-update
// SSE stream: publish() wakes every subscribed channel with a bare
// notification (no payload) so a browser tab knows to re-fetch /api/nodes.
// Subscribers are size-1 buffered channels; publish never blocks on a slow
// or dead subscriber, and a channel with an already-pending notification
// just absorbs a burst of publishes into one pending wake.
type broadcaster struct {
	mu   sync.Mutex
	subs map[chan struct{}]bool
}

func newBroadcaster() *broadcaster {
	return &broadcaster{subs: make(map[chan struct{}]bool)}
}

// subscribe registers a new size-1 buffered channel that receives a value
// on every publish() call until unsubscribe is called with it.
func (b *broadcaster) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	b.subs[ch] = true
	b.mu.Unlock()
	return ch
}

// unsubscribe removes ch from the subscriber set. Safe to call more than
// once or with a channel that was never subscribed.
func (b *broadcaster) unsubscribe(ch chan struct{}) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}

// publish wakes every current subscriber. Non-blocking: a subscriber whose
// buffered channel already holds a pending notification is left alone
// rather than blocking the publisher. Safe to call on a nil receiver
// (no-op) since most existing store tests construct &store{} directly
// without a broadcaster.
func (b *broadcaster) publish() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd hub && go test . -run TestBroadcaster -v`
Expected: PASS for all four tests.

- [ ] **Step 5: Full package check and commit**

Run: `cd hub && go build ./... && go vet ./... && go test ./... -timeout 60s`
Expected: all green.

```bash
git add hub/broadcaster.go hub/broadcaster_test.go
git commit -m "feat(hub): add broadcaster fan-out for live-update SSE"
```

---

### Task 6: Hub — wire `broadcaster` into `store` and the report/heartbeat handlers

**Files:**
- Modify: `hub/main.go:87-96` (`store` struct — add `broadcast` field)
- Modify: `hub/main.go:124-157` (`openStore` — initialize `broadcast`)
- Modify: `hub/main.go:440-503` (`handleReport`, `handleHeartbeat` — call `s.broadcast.publish()`)
- Test: `hub/main_test.go` (append near the existing report/heartbeat endpoint tests)

**Interfaces:**
- Consumes: `broadcaster`, `newBroadcaster()`, `(*broadcaster).publish()`, `(*broadcaster).subscribe()` (Task 5).
- Produces: `store.broadcast *broadcaster` field — consumed by Task 7's `handleEvents`.

- [ ] **Step 1: Write the failing tests**

Append to `hub/main_test.go`:

```go
func TestHandleReportPublishesOnSuccess(t *testing.T) {
	s := &store{
		Nodes:     make(map[string]*nodeState),
		rates:     make(map[string]*nodeRate),
		broadcast: newBroadcaster(),
	}
	ch := s.broadcast.subscribe()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/report", handleReport(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	report := nodeState{NodeID: "n1", Proxies: []proxyReport{{ID: "p1"}}}
	body, _ := json.Marshal(report)
	resp, err := http.Post(ts.URL+"/api/report", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	select {
	case <-ch:
	default:
		t.Errorf("broadcaster did not fire on a successful report")
	}
}

func TestHandleReportDoesNotPublishOnBadRequest(t *testing.T) {
	s := &store{
		Nodes:     make(map[string]*nodeState),
		rates:     make(map[string]*nodeRate),
		broadcast: newBroadcaster(),
	}
	ch := s.broadcast.subscribe()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/report", handleReport(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body, _ := json.Marshal(nodeState{Host: "no-id"}) // missing node_id -> 400
	resp, err := http.Post(ts.URL+"/api/report", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	select {
	case <-ch:
		t.Errorf("broadcaster fired on a 400 response")
	default:
	}
}

func TestHandleHeartbeatPublishesOnKnownNode(t *testing.T) {
	s := &store{
		Nodes:     make(map[string]*nodeState),
		rates:     make(map[string]*nodeRate),
		broadcast: newBroadcaster(),
	}
	s.upsert("n1", &nodeState{NodeID: "n1", Timestamp: time.Now().UTC(), Proxies: []proxyReport{{TotalRX: 0}}})
	ch := s.broadcast.subscribe()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/heartbeat", handleHeartbeat(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body, _ := json.Marshal(heartbeatReport{NodeID: "n1"})
	resp, err := http.Post(ts.URL+"/api/heartbeat", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	select {
	case <-ch:
	default:
		t.Errorf("broadcaster did not fire on a known-node heartbeat")
	}
}

func TestHandleHeartbeatDoesNotPublishOnUnknownNode(t *testing.T) {
	s := &store{
		Nodes:     make(map[string]*nodeState),
		rates:     make(map[string]*nodeRate),
		broadcast: newBroadcaster(),
	}
	ch := s.broadcast.subscribe()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/heartbeat", handleHeartbeat(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body, _ := json.Marshal(heartbeatReport{NodeID: "ghost"})
	resp, err := http.Post(ts.URL+"/api/heartbeat", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	select {
	case <-ch:
		t.Errorf("broadcaster fired on an unknown-node (202) heartbeat")
	default:
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd hub && go test . -run "TestHandleReportPublishes|TestHandleReportDoesNotPublish|TestHandleHeartbeatPublishes|TestHandleHeartbeatDoesNotPublish" -v`
Expected: FAIL — compile error, `unknown field broadcast in struct literal of type store`.

- [ ] **Step 3: Add the `broadcast` field to `store` and initialize it in `openStore`**

In `hub/main.go`, in the `store` struct (currently lines 87-96), add a field:

```go
type store struct {
	mu           sync.RWMutex
	db           *sql.DB
	Nodes        map[string]*nodeState        `json:"nodes"`
	rates        map[string]*nodeRate         `json:"-"`
	prevBillable map[string]map[string]uint64 `json:"-"` // nodeID -> proxyID -> last seen BillRX+BillTX
	earning      map[string]map[string]bool   `json:"-"` // nodeID -> proxyID -> earning=yes/no
	proxyIDs     map[string]int64             `json:"-"` // proxy addr -> interned proxies.id
	deltas       *deltaTracker                `json:"-"` // cumulative -> per-interval counters
	broadcast    *broadcaster                 `json:"-"` // live-update SSE fan-out; nil-safe, see broadcaster.publish
}
```

In `openStore` (currently lines 124-157), add `broadcast: newBroadcaster(),` to the `s := &store{...}` literal:

```go
	s := &store{
		db:           db,
		Nodes:        make(map[string]*nodeState),
		rates:        make(map[string]*nodeRate),
		prevBillable: make(map[string]map[string]uint64),
		earning:      make(map[string]map[string]bool),
		proxyIDs:     make(map[string]int64),
		deltas:       newDeltaTracker(),
		broadcast:    newBroadcaster(),
	}
```

- [ ] **Step 4: Call `s.broadcast.publish()` on success in `handleReport` and `handleHeartbeat`**

In `hub/main.go`, in `handleReport` (currently lines 440-469), change:

```go
		s.upsert(ns.NodeID, &ns)
		w.WriteHeader(204)
```

to:

```go
		s.upsert(ns.NodeID, &ns)
		s.broadcast.publish()
		w.WriteHeader(204)
```

In `handleHeartbeat` (currently lines 476-503), change:

```go
		hb.Timestamp = time.Now().UTC()
		if !s.heartbeat(hb.NodeID, &hb) {
			w.WriteHeader(202)
			return
		}
		w.WriteHeader(204)
```

to:

```go
		hb.Timestamp = time.Now().UTC()
		if !s.heartbeat(hb.NodeID, &hb) {
			w.WriteHeader(202)
			return
		}
		s.broadcast.publish()
		w.WriteHeader(204)
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd hub && go test . -run "TestHandleReportPublishes|TestHandleReportDoesNotPublish|TestHandleHeartbeatPublishes|TestHandleHeartbeatDoesNotPublish" -v`
Expected: PASS for all four.

- [ ] **Step 6: Full package check and commit**

Run: `cd hub && go build ./... && go vet ./... && go test ./... -timeout 60s`
Expected: all green — this also confirms every pre-existing test that builds `&store{...}` without a `broadcast` field still passes (relying on `publish()`'s nil-receiver safety from Task 5).

```bash
git add hub/main.go hub/main_test.go
git commit -m "feat(hub): publish broadcaster event on successful report/heartbeat"
```

---

### Task 7: Hub — `GET /api/events` SSE handler

**Files:**
- Modify: `hub/broadcaster.go` (add `handleEvents`)
- Modify: `hub/main.go:804-811` (register the route)
- Test: `hub/broadcaster_test.go` (append)

**Interfaces:**
- Consumes: `broadcaster.subscribe()`/`unsubscribe()` (Task 5), `store.broadcast` (Task 6).
- Produces: `func handleEvents(s *store) http.HandlerFunc` — registered at `/api/events`.

- [ ] **Step 1: Write the failing tests**

Append to `hub/broadcaster_test.go`:

```go
func TestHandleEventsDeliversRefreshOnPublish(t *testing.T) {
	s := &store{
		Nodes:     make(map[string]*nodeState),
		rates:     make(map[string]*nodeRate),
		broadcast: newBroadcaster(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/events", handleEvents(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	result := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := resp.Body.Read(buf)
		result <- string(buf[:n])
	}()

	time.Sleep(50 * time.Millisecond) // let the handler subscribe before publishing
	s.broadcast.publish()

	select {
	case got := <-result:
		if got != "data: refresh\n\n" {
			t.Errorf("body = %q, want %q", got, "data: refresh\n\n")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE event")
	}
}

func TestHandleEventsUnsubscribesOnClientDisconnect(t *testing.T) {
	s := &store{
		Nodes:     make(map[string]*nodeState),
		rates:     make(map[string]*nodeRate),
		broadcast: newBroadcaster(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/events", handleEvents(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close() // simulate the browser navigating away

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.broadcast.mu.Lock()
		n := len(s.broadcast.subs)
		s.broadcast.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("handler did not unsubscribe after client disconnect")
}
```

Add `"net/http"`, `"net/http/httptest"` imports to `hub/broadcaster_test.go` (the file will now need them alongside `"testing"` and `"time"`).

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd hub && go test . -run TestHandleEvents -v`
Expected: FAIL — compile error, `undefined: handleEvents`.

- [ ] **Step 3: Implement `handleEvents`**

Append to `hub/broadcaster.go`:

```go
// handleEvents serves the dashboard's live-update stream over
// Server-Sent Events: GET /api/events. It carries no payload — each event
// is a bare "go re-fetch" signal the browser turns into one /api/nodes
// call (see refreshDashboard() in the dashboard template). Blocks for the
// lifetime of the connection; returns when the browser disconnects.
func handleEvents(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(200)
		flusher.Flush()

		ch := s.broadcast.subscribe()
		defer s.broadcast.unsubscribe(ch)

		for {
			select {
			case <-r.Context().Done():
				return
			case <-ch:
				if _, err := w.Write([]byte("data: refresh\n\n")); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}
}
```

Add `"net/http"` import to `hub/broadcaster.go` (alongside the existing `"sync"`).

- [ ] **Step 4: Register the route**

In `hub/main.go`, in the mux registration block (currently lines 804-811), add after the `/api/history` line:

```go
	mux.HandleFunc("/api/report", requireAuth(hubToken, handleReport(s)))
	mux.HandleFunc("/api/heartbeat", requireAuth(hubToken, handleHeartbeat(s)))
	mux.HandleFunc("/api/nodes/remove", requireAuth(hubToken, handleNodeRemove(s)))
	mux.HandleFunc("/api/nodes/contracts", handleNodeContracts(s))
	mux.HandleFunc("/api/nodes/", handleNodes(s))
	mux.HandleFunc("/api/proxies/top", handleProxiesTop(s))
	mux.HandleFunc("/api/proxies/history", handleProxiesHistory(s))
	mux.HandleFunc("/api/history", handleHistory(s))
	mux.HandleFunc("/api/events", handleEvents(s))
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd hub && go test . -run TestHandleEvents -v`
Expected: PASS for both tests.

- [ ] **Step 6: Full package check and commit**

Run: `cd hub && go build ./... && go vet ./... && go test ./... -timeout 60s`
Expected: all green.

```bash
git add hub/broadcaster.go hub/broadcaster_test.go hub/main.go
git commit -m "feat(hub): add GET /api/events SSE endpoint"
```

---

### Task 8: Dashboard — subscribe to `/api/events` and trigger `refreshDashboard()`

**Files:**
- Modify: `hub/main.go:1367-1374` (dashboard `<script>` template — add SSE wiring)
- Test: `hub/main_test.go` (extend `TestDashboardEndpoint`)

**Interfaces:**
- Consumes: `/api/events` (Task 7), existing `refreshDashboard()` JS function (`hub/main.go:1375` onward — unchanged).

- [ ] **Step 1: Extend the failing test**

In `hub/main_test.go`, in `TestDashboardEndpoint` (currently lines 520-562), add an assertion after the existing `bytes.Contains` checks:

```go
	if !bytes.Contains(body, []byte("/api/events")) {
		t.Errorf("dashboard body does not wire up the live-update SSE endpoint")
	}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd hub && go test . -run TestDashboardEndpoint -v`
Expected: FAIL — `dashboard body does not wire up the live-update SSE endpoint`.

- [ ] **Step 3: Add the SSE wiring to the dashboard template**

In `hub/main.go`, the auto-refresh block currently reads (lines 1366-1375):

```
// === Auto-refresh ===
var secondsLeft = 30, refreshing = false;
setInterval(function tick(){
  if (!document.getElementById('auto-refresh').checked) { document.getElementById('countdown').textContent = 'paused'; return; }
  secondsLeft--;
  if (secondsLeft <= 0) { secondsLeft = 30; refreshDashboard(); return; }
  document.getElementById('countdown').textContent = secondsLeft + 's';
}, 1000);
function toggleRefresh() { if (document.getElementById('auto-refresh').checked) secondsLeft = 30; }
function refreshDashboard() {
```

Insert a new block between `function toggleRefresh() { ... }` and `function refreshDashboard() {`:

```
// === Auto-refresh ===
var secondsLeft = 30, refreshing = false;
setInterval(function tick(){
  if (!document.getElementById('auto-refresh').checked) { document.getElementById('countdown').textContent = 'paused'; return; }
  secondsLeft--;
  if (secondsLeft <= 0) { secondsLeft = 30; refreshDashboard(); return; }
  document.getElementById('countdown').textContent = secondsLeft + 's';
}, 1000);
function toggleRefresh() { if (document.getElementById('auto-refresh').checked) secondsLeft = 30; }

// === Live updates (SSE) ===
// Pushes a bare "something changed" signal from the hub the moment a
// heartbeat or report lands, so the dashboard doesn't wait out the full
// 30s poll above. The poll stays as a backstop for links where SSE gets
// buffered/stripped (e.g. some reverse proxies) — EventSource retries on
// its own with native backoff, no custom reconnect logic needed here.
if (window.EventSource) {
  var liveEvents = new EventSource('/api/events');
  liveEvents.onmessage = function() { refreshDashboard(); };
}

function refreshDashboard() {
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd hub && go test . -run TestDashboardEndpoint -v`
Expected: PASS.

- [ ] **Step 5: Full package check and commit**

Run: `cd hub && go build ./... && go vet ./... && go test ./... -timeout 60s`
Expected: all green.

```bash
git add hub/main.go hub/main_test.go
git commit -m "feat(hub): subscribe dashboard to live-update SSE stream"
```

- [ ] **Step 6: Manual verification**

Automated coverage stops at "the HTML references `/api/events`" — there's no browser-JS test harness in this repo (consistent with the rest of the dashboard template). Before considering this task done, start the hub locally, open the dashboard in a browser, open DevTools → Network, and confirm:
1. An `/api/events` request appears with type `eventsource` and stays open (status stays "pending"/streaming, doesn't complete).
2. POSTing a heartbeat or report (e.g. via `curl` to `/api/heartbeat` with a valid `node_id` already in `s.Nodes`) causes a new `/api/nodes` fetch to appear in the Network tab within roughly a second, without waiting for the 30s countdown to reach zero.

---

## Post-plan

After all 8 tasks are merged, update `progress.md` per this repo's workflow convention (checkpoint after significant milestones) and bump the parent repo's submodule gitlink if this was implemented in the `urnetwork-3.23-fix` submodule context.
