# Dead-Proxy Health Report Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a pure-observability per-heartbeat report of which proxies are dead vs degraded, a record of how many the fix.11 retry pulse recovers, and durable on-disk files (plus host/Docker commands) so the picture survives RAMLOGS.

**Architecture:** A process-global registry in package `connect` (`proxy_health.go`) tracks each proxy's up/ever-up/down-since state, updated at the same two points in `transport.go` that PR #31 instrumented. The provider's heartbeat reads the registry once per tick, prints `[health][proxies]` lines (50-entry display cap), and mirrors the full state to `proxy_health.state` (snapshot) and `proxy_health.log` (append-only event log, 20 MB + 1 rotation) on the persistent config dir. The hourly pulse goroutine logs a `[pulse]` marker. Access via `urnet-tools proxy health` (host) and a `proxy-health` helper baked into the Docker image.

**Tech Stack:** Go 1.25 (stdlib only: `sync`, `sort`, `time`, `os`, `path/filepath`, `fmt`), POSIX shell (urnet-tools + Docker helper), `go test`.

**Spec:** `docs/design/dead-proxy-health-report.md`

---

## File Structure

- Create: `proxy_health.go` (package `connect`) — registry: state, register/mark, snapshot, heartbeat report.
- Create: `proxy_health_test.go` (package `connect`) — registry unit tests.
- Modify: `transport.go` — `proxyIndex()` helper + `markProxyUp/Down` calls in `runH1`/`runH3`.
- Create: `provider/proxy_health_log.go` (package `main`) — pure formatters (`capProxyList`, `formatStateFile`, `formatEventLines`) + file writers (`writeProxyHealthState`, `appendProxyHealthEvents`, `rotateIfNeeded`) + dir resolver.
- Create: `provider/proxy_health_log_test.go` (package `main`) — formatter/rotation tests.
- Modify: `provider/main.go` — register proxies at startup; heartbeat output + persistence; pulse-fire marker.
- Modify: `scripts/Provider_Install_Linux.sh` — `proxy health` case + usage text.
- Create: `docker/scripts/proxy-health.sh` — Docker access helper.
- Modify: `Dockerfile` — install `proxy-health` into `PATH`.
- Modify: `LOG_REFERENCE.md`, `CHANGELOG.md`, `FORK_CHANGES.md`, `docs/Configuration.md`, `docs/Docker-Deployment.md`, `README.md` — docs.

---

## Task 1: Registry state + register/mark

**Files:**
- Create: `proxy_health.go`
- Test: `proxy_health_test.go`

- [ ] **Step 1: Write the failing test**

Create `proxy_health_test.go`:

```go
package connect

import "testing"

// resetProxyHealthForTest clears global registry state between tests.
func resetProxyHealthForTest() {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	proxyHealthByIndex = map[int]*proxyHealth{}
	proxyLifetimeRecovered = 0
	proxyLifetimeLost = 0
	proxyBaselineSet = false
}

func TestProxyHealthRegisterAndMark(t *testing.T) {
	resetProxyHealthForTest()

	RegisterProxy(0, "1.1.1.1:1081")
	RegisterProxy(1, "2.2.2.2:1081")
	if got := ProxyHealthCount(); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}

	// idempotent: re-register keeps the entry
	RegisterProxy(0, "1.1.1.1:1081")
	if got := ProxyHealthCount(); got != 2 {
		t.Fatalf("count after re-register = %d, want 2", got)
	}

	markProxyUp(0)
	proxyHealthMu.Lock()
	up := proxyHealthByIndex[0].currentlyUp
	ever := proxyHealthByIndex[0].everUp
	proxyHealthMu.Unlock()
	if !up || !ever {
		t.Fatalf("after markProxyUp: currentlyUp=%v everUp=%v, want true,true", up, ever)
	}

	markProxyDown(0)
	proxyHealthMu.Lock()
	up = proxyHealthByIndex[0].currentlyUp
	ever = proxyHealthByIndex[0].everUp
	downStamped := !proxyHealthByIndex[0].downSince.IsZero()
	proxyHealthMu.Unlock()
	if up || !ever || !downStamped {
		t.Fatalf("after markProxyDown: up=%v ever=%v downStamped=%v, want false,true,true", up, ever, downStamped)
	}

	// mark on unknown index is a no-op (must not panic)
	markProxyUp(999)
	markProxyDown(999)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run TestProxyHealthRegisterAndMark -v`
Expected: FAIL — compile error (`proxyHealthByIndex`, `RegisterProxy`, etc. undefined).

- [ ] **Step 3: Write minimal implementation**

Create `proxy_health.go`:

```go
package connect

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// proxyHealth tracks one proxy's platform-transport liveness for the
// [health][proxies] report. See docs/design/dead-proxy-health-report.md.
type proxyHealth struct {
	address     string
	currentlyUp bool
	everUp      bool
	downSince   time.Time // when currentlyUp last went false (for recovery latency)
	lastSeenUp  bool      // currentlyUp as of the previous heartbeat (baseline)
	deadLogged  bool      // a confirmed-dead event has been emitted for this proxy
}

// ProxyEvent identifies a proxy in a transition list. After is set for
// recovered events (time the proxy was down before coming back).
type ProxyEvent struct {
	Index   int
	Address string
	After   time.Duration
}

// ProxyHealthReport is the full per-heartbeat result.
type ProxyHealthReport struct {
	Up       int
	Dead     []string // formatted "proxy[idx] (addr)", index-sorted, complete (uncapped)
	Degraded []string

	Recovered     []ProxyEvent // down->up since last heartbeat
	NewlyDegraded []ProxyEvent // up->down since last heartbeat
	NewlyDead     []ProxyEvent // never-up proxies newly confirmed dead (logged once)

	LifetimeRecovered int
	LifetimeLost      int
}

var (
	proxyHealthMu      sync.Mutex
	proxyHealthByIndex = map[int]*proxyHealth{}

	proxyLifetimeRecovered int
	proxyLifetimeLost      int
	proxyBaselineSet       bool
)

// RegisterProxy adds a proxy to the registry if absent. Idempotent so a list
// re-read preserves everUp. Called eagerly at startup for every proxy.
func RegisterProxy(index int, address string) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	if _, ok := proxyHealthByIndex[index]; !ok {
		proxyHealthByIndex[index] = &proxyHealth{address: address}
	}
}

// markProxyUp records that the proxy's platform transport is live.
func markProxyUp(index int) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	if h, ok := proxyHealthByIndex[index]; ok {
		h.currentlyUp = true
		h.everUp = true
	}
}

// markProxyDown records that the proxy's platform transport went down, stamping
// downSince when it was previously up (for recovery-latency reporting).
func markProxyDown(index int) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	if h, ok := proxyHealthByIndex[index]; ok {
		if h.currentlyUp {
			h.downSince = time.Now()
		}
		h.currentlyUp = false
	}
}

// ProxyHealthCount returns the number of registered proxies (0 = non-proxy mode).
func ProxyHealthCount() int {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	return len(proxyHealthByIndex)
}

func formatProxyEntry(index int, address string) string {
	return fmt.Sprintf("proxy[%d] (%s)", index, address)
}

// sortedIndicesLocked returns registry indices in ascending order. Caller holds the lock.
func sortedIndicesLocked() []int {
	indices := make([]int, 0, len(proxyHealthByIndex))
	for idx := range proxyHealthByIndex {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	return indices
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test . -run TestProxyHealthRegisterAndMark -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add proxy_health.go proxy_health_test.go
git commit -m "feat: proxy health registry state, register and mark"
```

---

## Task 2: Read-only snapshot

**Files:**
- Modify: `proxy_health.go`
- Test: `proxy_health_test.go`

- [ ] **Step 1: Write the failing test**

Append to `proxy_health_test.go`:

```go
func TestProxyHealthSnapshot(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(2, "c:1") // dead (never up)
	RegisterProxy(0, "a:1") // will be up
	RegisterProxy(1, "b:1") // will be degraded

	markProxyUp(0)
	markProxyUp(1)
	markProxyDown(1) // up then down -> degraded

	up, dead, degraded := ProxyHealthSnapshot()
	if up != 1 {
		t.Fatalf("up = %d, want 1", up)
	}
	if len(dead) != 1 || dead[0] != "proxy[2] (c:1)" {
		t.Fatalf("dead = %v, want [proxy[2] (c:1)]", dead)
	}
	if len(degraded) != 1 || degraded[0] != "proxy[1] (b:1)" {
		t.Fatalf("degraded = %v, want [proxy[1] (b:1)]", degraded)
	}

	// snapshot must NOT advance the baseline: lastSeenUp stays false everywhere
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	for idx, h := range proxyHealthByIndex {
		if h.lastSeenUp {
			t.Fatalf("snapshot advanced baseline for idx %d", idx)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run TestProxyHealthSnapshot -v`
Expected: FAIL — `ProxyHealthSnapshot` undefined.

- [ ] **Step 3: Write minimal implementation**

Append to `proxy_health.go`:

```go
// ProxyHealthSnapshot returns the current state without advancing the transition
// baseline, so it is safe to call from the pulse-fire marker. Lists are complete
// (no display cap) and index-sorted.
func ProxyHealthSnapshot() (up int, dead []string, degraded []string) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	for _, idx := range sortedIndicesLocked() {
		h := proxyHealthByIndex[idx]
		switch {
		case h.currentlyUp:
			up++
		case h.everUp:
			degraded = append(degraded, formatProxyEntry(idx, h.address))
		default:
			dead = append(dead, formatProxyEntry(idx, h.address))
		}
	}
	return up, dead, degraded
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test . -run TestProxyHealthSnapshot -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add proxy_health.go proxy_health_test.go
git commit -m "feat: proxy health read-only snapshot"
```

---

## Task 3: Heartbeat report (transitions + lifetime + confirmed-dead)

**Files:**
- Modify: `proxy_health.go`
- Test: `proxy_health_test.go`

- [ ] **Step 1: Write the failing test**

Append to `proxy_health_test.go`:

```go
func TestProxyHealthHeartbeatTransitions(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(0, "a:1")
	RegisterProxy(1, "b:1") // stays dead

	// First call establishes the baseline: no transitions, no dead (confirmDead=false).
	r := ProxyHealthHeartbeat(false)
	if len(r.Recovered) != 0 || len(r.NewlyDegraded) != 0 || len(r.NewlyDead) != 0 {
		t.Fatalf("first call should have no events, got %+v", r)
	}
	if r.LifetimeRecovered != 0 || r.LifetimeLost != 0 {
		t.Fatalf("first call lifetime counters should be 0, got %+v", r)
	}

	// Proxy 0 comes up -> recovered=1 (first-ever connect, after omitted).
	markProxyUp(0)
	r = ProxyHealthHeartbeat(false)
	if len(r.Recovered) != 1 || r.Recovered[0].Index != 0 {
		t.Fatalf("Recovered = %+v, want [idx 0]", r.Recovered)
	}
	if r.LifetimeRecovered != 1 {
		t.Fatalf("LifetimeRecovered = %d, want 1", r.LifetimeRecovered)
	}

	// Proxy 0 drops -> NewlyDegraded=1, lifetime_lost=1, lifetime_recovered unchanged.
	markProxyDown(0)
	r = ProxyHealthHeartbeat(false)
	if len(r.NewlyDegraded) != 1 || r.NewlyDegraded[0].Index != 0 {
		t.Fatalf("NewlyDegraded = %+v, want [idx 0]", r.NewlyDegraded)
	}
	if r.LifetimeLost != 1 || r.LifetimeRecovered != 1 {
		t.Fatalf("lifetime = (rec %d, lost %d), want (1,1)", r.LifetimeRecovered, r.LifetimeLost)
	}

	// confirmDead=true: proxy 1 (never up) is logged dead once.
	r = ProxyHealthHeartbeat(true)
	if len(r.NewlyDead) != 1 || r.NewlyDead[0].Index != 1 {
		t.Fatalf("NewlyDead = %+v, want [idx 1]", r.NewlyDead)
	}
	// ...and not again on the next confirmDead call.
	r = ProxyHealthHeartbeat(true)
	if len(r.NewlyDead) != 0 {
		t.Fatalf("NewlyDead repeated = %+v, want empty", r.NewlyDead)
	}
}

func TestProxyHealthHeartbeatFlappingCountsTwice(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(0, "a:1")
	ProxyHealthHeartbeat(false) // baseline

	markProxyUp(0)
	ProxyHealthHeartbeat(false) // recovered #1
	markProxyDown(0)
	ProxyHealthHeartbeat(false) // lost #1
	markProxyUp(0)
	r := ProxyHealthHeartbeat(false) // recovered #2

	if r.LifetimeRecovered != 2 {
		t.Fatalf("LifetimeRecovered = %d, want 2 (event semantics)", r.LifetimeRecovered)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run TestProxyHealthHeartbeat -v`
Expected: FAIL — `ProxyHealthHeartbeat` undefined.

- [ ] **Step 3: Write minimal implementation**

Append to `proxy_health.go`:

```go
// ProxyHealthHeartbeat builds the per-heartbeat report and advances the transition
// baseline. Call exactly once per heartbeat. On the first call it only establishes
// the baseline (no transition events). NewlyDead is populated only when confirmDead
// is true (caller passes uptime >= deadConfirmDelay), once per never-up proxy.
func ProxyHealthHeartbeat(confirmDead bool) ProxyHealthReport {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()

	now := time.Now()
	first := !proxyBaselineSet
	var r ProxyHealthReport

	for _, idx := range sortedIndicesLocked() {
		h := proxyHealthByIndex[idx]

		switch {
		case h.currentlyUp:
			r.Up++
		case h.everUp:
			r.Degraded = append(r.Degraded, formatProxyEntry(idx, h.address))
		default:
			r.Dead = append(r.Dead, formatProxyEntry(idx, h.address))
		}

		if !first {
			switch {
			case h.currentlyUp && !h.lastSeenUp:
				ev := ProxyEvent{Index: idx, Address: h.address}
				if !h.downSince.IsZero() {
					ev.After = now.Sub(h.downSince)
				}
				r.Recovered = append(r.Recovered, ev)
				proxyLifetimeRecovered++
			case !h.currentlyUp && h.lastSeenUp:
				r.NewlyDegraded = append(r.NewlyDegraded, ProxyEvent{Index: idx, Address: h.address})
				proxyLifetimeLost++
			}
		}

		if confirmDead && !h.currentlyUp && !h.everUp && !h.deadLogged {
			r.NewlyDead = append(r.NewlyDead, ProxyEvent{Index: idx, Address: h.address})
			h.deadLogged = true
		}

		h.lastSeenUp = h.currentlyUp
	}

	proxyBaselineSet = true
	r.LifetimeRecovered = proxyLifetimeRecovered
	r.LifetimeLost = proxyLifetimeLost
	return r
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test . -run TestProxyHealthHeartbeat -v`
Expected: PASS (both heartbeat tests).

- [ ] **Step 5: Run the full connect package tests to confirm no regressions**

Run: `go test . -run TestProxyHealth -v`
Expected: PASS (all four proxy-health tests).

- [ ] **Step 6: Commit**

```bash
git add proxy_health.go proxy_health_test.go
git commit -m "feat: proxy health heartbeat report with transitions and confirmed-dead"
```

---

## Task 4: Instrument transport.go (runH1 + runH3)

**Files:**
- Modify: `transport.go` (add helper near `PlatformTransport` methods; edit `runH1` ~664-673 and `runH3` ~1179-1188)

No unit test (the run loops are integration-level); verified by build + the live test in Task 11. The `markProxy*` funcs are already covered by Task 1-3 tests.

- [ ] **Step 1: Add the `proxyIndex` helper**

In `transport.go`, immediately before `func (self *PlatformTransport) runH1(` (around line 640), add:

```go
// proxyIndex returns this transport's proxy list index when running behind a
// proxy, or ok=false in non-proxy (direct) mode.
func (self *PlatformTransport) proxyIndex() (int, bool) {
	if self.clientStrategy == nil || self.clientStrategy.settings == nil {
		return 0, false
	}
	ps := self.clientStrategy.settings.ProxySettings
	if ps == nil {
		return 0, false
	}
	return ps.Index, true
}
```

- [ ] **Step 2: Instrument `runH1`**

In `transport.go`, replace the block at ~664-673:

```go
			self.routeManager.UpdateTransport(sendTransport, []Route{exportedSend})
			self.routeManager.UpdateTransport(receiveTransport, []Route{receive})

			atomic.AddInt64(&activeProxyConnections, 1)

			defer func() {
				atomic.AddInt64(&activeProxyConnections, -1)
				self.routeManager.RemoveTransport(sendTransport)
				self.routeManager.RemoveTransport(receiveTransport)
			}()
```

with:

```go
			self.routeManager.UpdateTransport(sendTransport, []Route{exportedSend})
			self.routeManager.UpdateTransport(receiveTransport, []Route{receive})

			atomic.AddInt64(&activeProxyConnections, 1)
			if idx, ok := self.proxyIndex(); ok {
				markProxyUp(idx)
			}

			defer func() {
				atomic.AddInt64(&activeProxyConnections, -1)
				if idx, ok := self.proxyIndex(); ok {
					markProxyDown(idx)
				}
				self.routeManager.RemoveTransport(sendTransport)
				self.routeManager.RemoveTransport(receiveTransport)
			}()
```

- [ ] **Step 3: Instrument `runH3`**

In `transport.go`, replace the block at ~1179-1188:

```go
			self.routeManager.UpdateTransport(sendTransport, []Route{send})
			self.routeManager.UpdateTransport(receiveTransport, []Route{receive})

			atomic.AddInt64(&activeProxyConnections, 1)

			defer func() {
				atomic.AddInt64(&activeProxyConnections, -1)
				self.routeManager.RemoveTransport(sendTransport)
				self.routeManager.RemoveTransport(receiveTransport)
```

with:

```go
			self.routeManager.UpdateTransport(sendTransport, []Route{send})
			self.routeManager.UpdateTransport(receiveTransport, []Route{receive})

			atomic.AddInt64(&activeProxyConnections, 1)
			if idx, ok := self.proxyIndex(); ok {
				markProxyUp(idx)
			}

			defer func() {
				atomic.AddInt64(&activeProxyConnections, -1)
				if idx, ok := self.proxyIndex(); ok {
					markProxyDown(idx)
				}
				self.routeManager.RemoveTransport(sendTransport)
				self.routeManager.RemoveTransport(receiveTransport)
```

(Leave the rest of the runH3 defer body — the `// note `send` is not closed` comment and following lines — unchanged.)

- [ ] **Step 4: Build to verify it compiles**

Run: `go build -o provider_bin ./provider`
Expected: exit 0, no output.

- [ ] **Step 5: Commit**

```bash
git add transport.go
git commit -m "feat: record proxy up/down in health registry from runH1/runH3"
```

---

## Task 5: Provider-side formatters (cap + state file + event lines)

**Files:**
- Create: `provider/proxy_health_log.go`
- Test: `provider/proxy_health_log_test.go`

- [ ] **Step 1: Write the failing test**

Create `provider/proxy_health_log_test.go`:

```go
package main

import (
	"strings"
	"testing"
	"time"

	"bringyour.com/connect"
)

func TestCapProxyList(t *testing.T) {
	if got := capProxyList(nil, 50); got != "" {
		t.Fatalf("empty = %q, want empty", got)
	}
	if got := capProxyList([]string{"a", "b"}, 50); got != "a, b" {
		t.Fatalf("under cap = %q, want \"a, b\"", got)
	}
	got := capProxyList([]string{"a", "b", "c"}, 2)
	if got != "a, b, ... (+1 more)" {
		t.Fatalf("over cap = %q, want \"a, b, ... (+1 more)\"", got)
	}
}

func TestFormatStateFile(t *testing.T) {
	r := connect.ProxyHealthReport{
		Up:       3,
		Dead:     []string{"proxy[2] (c:1)"},
		Degraded: []string{"proxy[1] (b:1)"},
		LifetimeRecovered: 5,
		LifetimeLost:      4,
	}
	now := time.Date(2026, 6, 2, 16, 5, 11, 0, time.UTC)
	out := formatStateFile(r, now)

	if !strings.Contains(out, "up=3 down=2 dead=1 degraded=1") {
		t.Fatalf("missing summary header in:\n%s", out)
	}
	if !strings.Contains(out, "lifetime_recovered=5 lifetime_lost=4") {
		t.Fatalf("missing lifetime header in:\n%s", out)
	}
	if !strings.Contains(out, "DEAD     proxy[2] (c:1)") {
		t.Fatalf("missing dead line in:\n%s", out)
	}
	if !strings.Contains(out, "DEGRADED proxy[1] (b:1)") {
		t.Fatalf("missing degraded line in:\n%s", out)
	}
}

func TestFormatEventLines(t *testing.T) {
	r := connect.ProxyHealthReport{
		Recovered:     []connect.ProxyEvent{{Index: 1, Address: "b:1", After: 55*time.Minute + 8*time.Second}},
		NewlyDegraded: []connect.ProxyEvent{{Index: 3, Address: "d:1"}},
		NewlyDead:     []connect.ProxyEvent{{Index: 2, Address: "c:1"}},
	}
	now := time.Date(2026, 6, 2, 16, 5, 11, 0, time.UTC)
	lines := formatEventLines(r, now)

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "2026-06-02T16:05:11Z RECOVERED proxy[1] (b:1) after=55m8s") {
		t.Fatalf("missing/!= recovered line in:\n%s", joined)
	}
	if !strings.Contains(joined, "2026-06-02T16:05:11Z DEGRADED  proxy[3] (d:1)") {
		t.Fatalf("missing degraded line in:\n%s", joined)
	}
	if !strings.Contains(joined, "2026-06-02T16:05:11Z DEAD      proxy[2] (c:1)") {
		t.Fatalf("missing dead line in:\n%s", joined)
	}
}

func TestFormatEventLinesRecoveredWithoutLatency(t *testing.T) {
	r := connect.ProxyHealthReport{
		Recovered: []connect.ProxyEvent{{Index: 0, Address: "a:1"}}, // After == 0 -> omit
	}
	now := time.Date(2026, 6, 2, 16, 0, 0, 0, time.UTC)
	lines := formatEventLines(r, now)
	if len(lines) != 1 || strings.Contains(lines[0], "after=") {
		t.Fatalf("recovered without latency should omit after=, got %v", lines)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./provider/ -run 'TestCapProxyList|TestFormatStateFile|TestFormatEventLines' -v`
Expected: FAIL — `capProxyList`/`formatStateFile`/`formatEventLines` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `provider/proxy_health_log.go`:

```go
package main

import (
	"fmt"
	"strings"
	"time"

	"bringyour.com/connect"
)

// proxyHealthListCap bounds the stdout/combined-log detail lines. It does NOT
// apply to the persistent files, which always carry the complete list.
const proxyHealthListCap = 50

// capProxyList joins items with ", ", truncating to cap with a "(+N more)" suffix.
func capProxyList(items []string, cap int) string {
	if len(items) == 0 {
		return ""
	}
	if len(items) <= cap {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:cap], ", ") + fmt.Sprintf(", ... (+%d more)", len(items)-cap)
}

// formatStateFile renders the complete current-state snapshot (uncapped).
func formatStateFile(r connect.ProxyHealthReport, now time.Time) string {
	var b strings.Builder
	down := len(r.Dead) + len(r.Degraded)
	fmt.Fprintf(&b, "# updated %s  up=%d down=%d dead=%d degraded=%d\n",
		now.UTC().Format(time.RFC3339), r.Up, down, len(r.Dead), len(r.Degraded))
	fmt.Fprintf(&b, "# lifetime_recovered=%d lifetime_lost=%d\n", r.LifetimeRecovered, r.LifetimeLost)
	for _, s := range r.Dead {
		fmt.Fprintf(&b, "DEAD     %s\n", s)
	}
	for _, s := range r.Degraded {
		fmt.Fprintf(&b, "DEGRADED %s\n", s)
	}
	return b.String()
}

// formatEventLines renders one append-line per transition (complete, uncapped).
func formatEventLines(r connect.ProxyHealthReport, now time.Time) []string {
	ts := now.UTC().Format(time.RFC3339)
	var lines []string
	for _, e := range r.Recovered {
		if e.After > 0 {
			lines = append(lines, fmt.Sprintf("%s RECOVERED %s after=%s", ts, connectProxyEntry(e), e.After.Round(time.Second)))
		} else {
			lines = append(lines, fmt.Sprintf("%s RECOVERED %s", ts, connectProxyEntry(e)))
		}
	}
	for _, e := range r.NewlyDegraded {
		lines = append(lines, fmt.Sprintf("%s DEGRADED  %s", ts, connectProxyEntry(e)))
	}
	for _, e := range r.NewlyDead {
		lines = append(lines, fmt.Sprintf("%s DEAD      %s", ts, connectProxyEntry(e)))
	}
	return lines
}

func connectProxyEntry(e connect.ProxyEvent) string {
	return fmt.Sprintf("proxy[%d] (%s)", e.Index, e.Address)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./provider/ -run 'TestCapProxyList|TestFormatStateFile|TestFormatEventLines' -v`
Expected: PASS (all four tests).

- [ ] **Step 5: Commit**

```bash
git add provider/proxy_health_log.go provider/proxy_health_log_test.go
git commit -m "feat: provider proxy-health formatters (cap, state file, event lines)"
```

---

## Task 6: Persistence writers (files + rotation + dir resolver)

**Files:**
- Modify: `provider/proxy_health_log.go`
- Test: `provider/proxy_health_log_test.go`

- [ ] **Step 1: Write the failing test**

Append to `provider/proxy_health_log_test.go`:

```go
import_extra_marker := true // (delete; placeholder so reviewer adds os/filepath imports)
```

Replace the import block at the top of `provider/proxy_health_log_test.go` with:

```go
import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bringyour.com/connect"
)
```

Then append these tests:

```go
func TestRotateIfNeeded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy_health.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 100)), 0644); err != nil {
		t.Fatal(err)
	}

	// Under the cap: no rotation.
	rotateIfNeeded(path, 1000)
	if _, err := os.Stat(filepath.Join(dir, "proxy_health.log.1")); !os.IsNotExist(err) {
		t.Fatalf("rotated under cap, want no .1 file")
	}

	// Over the cap: rotate to .1, original gone.
	rotateIfNeeded(path, 50)
	if _, err := os.Stat(filepath.Join(dir, "proxy_health.log.1")); err != nil {
		t.Fatalf(".1 file missing after rotation: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("original log still present after rotation")
	}
}

func TestWriteProxyHealthFiles(t *testing.T) {
	dir := t.TempDir()
	r := connect.ProxyHealthReport{
		Up:        1,
		Dead:      []string{"proxy[2] (c:1)"},
		NewlyDead: []connect.ProxyEvent{{Index: 2, Address: "c:1"}},
	}
	now := time.Date(2026, 6, 2, 16, 0, 0, 0, time.UTC)

	writeProxyHealthState(dir, r, now)
	writeProxyHealthEvents(dir, r, now)

	state, err := os.ReadFile(filepath.Join(dir, "proxy_health.state"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(state), "DEAD     proxy[2] (c:1)") {
		t.Fatalf("state file missing dead entry:\n%s", state)
	}

	events, err := os.ReadFile(filepath.Join(dir, "proxy_health.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), "DEAD      proxy[2] (c:1)") {
		t.Fatalf("event log missing dead entry:\n%s", events)
	}

	// No events -> event log unchanged (no empty append).
	before, _ := os.ReadFile(filepath.Join(dir, "proxy_health.log"))
	writeProxyHealthEvents(dir, connect.ProxyHealthReport{}, now)
	after, _ := os.ReadFile(filepath.Join(dir, "proxy_health.log"))
	if string(before) != string(after) {
		t.Fatalf("empty report should not append to event log")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./provider/ -run 'TestRotateIfNeeded|TestWriteProxyHealthFiles' -v`
Expected: FAIL — `rotateIfNeeded`/`writeProxyHealthState`/`writeProxyHealthEvents` undefined.

- [ ] **Step 3: Write minimal implementation**

Append to `provider/proxy_health_log.go` (and add `os`, `path/filepath` to its import block):

```go
const proxyHealthLogMaxBytes = 20 * 1024 * 1024 // 20 MB

// proxyHealthDir resolves the directory for the persistent files:
// URNETWORK_PROXY_HEALTH_DIR, else <home>/.urnetwork. Returns ok=false if neither
// can be resolved (persistence then disabled by the caller).
func proxyHealthDir() (string, bool) {
	if d := os.Getenv("URNETWORK_PROXY_HEALTH_DIR"); d != "" {
		return d, true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(home, ".urnetwork"), true
}

// writeProxyHealthState atomically rewrites the current-state snapshot file.
func writeProxyHealthState(dir string, r connect.ProxyHealthReport, now time.Time) {
	path := filepath.Join(dir, "proxy_health.state")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(formatStateFile(r, now)), 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// writeProxyHealthEvents appends transition lines (if any) to the event log,
// rotating first when it would exceed the size cap.
func writeProxyHealthEvents(dir string, r connect.ProxyHealthReport, now time.Time) {
	lines := formatEventLines(r, now)
	if len(lines) == 0 {
		return
	}
	path := filepath.Join(dir, "proxy_health.log")
	rotateIfNeeded(path, proxyHealthLogMaxBytes)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(strings.Join(lines, "\n") + "\n")
}

// rotateIfNeeded renames path to path.1 (replacing any prior .1) when it exceeds
// maxBytes, keeping one generation of history.
func rotateIfNeeded(path string, maxBytes int64) {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= maxBytes {
		return
	}
	_ = os.Rename(path, path+".1")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./provider/ -run 'TestRotateIfNeeded|TestWriteProxyHealthFiles' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add provider/proxy_health_log.go provider/proxy_health_log_test.go
git commit -m "feat: provider proxy-health persistent file writers with rotation"
```

---

## Task 7: Wire registration + heartbeat output + persistence into main.go

**Files:**
- Modify: `provider/main.go` (registration loop ~1008-1022; `runHealthHeartbeat` ~783-815 and its call site ~881)

- [ ] **Step 1: Register proxies at startup**

In `provider/main.go`, inside the `for i, proxySettings := range allProxySettings` loop that prints the proxy list (the block at ~1008-1022), add a registration call right after `proxySettings.Index = i`:

```go
		for i, proxySettings := range allProxySettings {
			proxySettings.Index = i
			connect.RegisterProxy(i, proxySettings.Address)
			var user string
```

(Leave the rest of the loop body unchanged.)

- [ ] **Step 2: Emit heartbeat proxy lines + persistence**

In `provider/main.go`, replace the `for { ... }` body of `runHealthHeartbeat` (the block from `for {` at ~797 through the closing `}` at ~815) with:

```go
	// deadConfirmDelay gates confirmed-dead event logging until one pulse cycle has
	// elapsed, so the startup ramp is not recorded as dead.
	const deadConfirmDelay = 65 * time.Minute

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		samples := []metrics.Sample{
			{Name: "/memory/classes/heap/objects:bytes"},
			{Name: "/memory/classes/total:bytes"},
		}
		metrics.Read(samples)
		heapMiB := metricBytesToMiB("/memory/classes/heap/objects:bytes", samples[0].Value)
		sysMiB := metricBytesToMiB("/memory/classes/total:bytes", samples[1].Value)
		uptime := time.Since(startTime).Truncate(time.Second)
		fmt.Printf("[health] uptime=%s profile=%s heap=%dMiB sys=%dMiB connections=%d proxies=%d\n",
			uptime, profile, heapMiB, sysMiB, connect.ActiveConnectionCount(), connect.ActiveProxyConnections())

		if connect.ProxyHealthCount() == 0 {
			continue // non-proxy mode: no [health][proxies] lines
		}

		now := time.Now()
		report := connect.ProxyHealthHeartbeat(uptime >= deadConfirmDelay)
		down := len(report.Dead) + len(report.Degraded)
		fmt.Printf("[health][proxies] up=%d down=%d dead=%d degraded=%d recovered=%d lost=%d lifetime_recovered=%d lifetime_lost=%d\n",
			report.Up, down, len(report.Dead), len(report.Degraded),
			len(report.Recovered), len(report.NewlyDegraded),
			report.LifetimeRecovered, report.LifetimeLost)
		if len(report.Dead) > 0 {
			fmt.Printf("[health][proxies] dead: %s\n", capProxyList(report.Dead, proxyHealthListCap))
		}
		if len(report.Degraded) > 0 {
			fmt.Printf("[health][proxies] degraded: %s\n", capProxyList(report.Degraded, proxyHealthListCap))
		}

		if dir, ok := proxyHealthDir(); ok {
			writeProxyHealthState(dir, report, now)
			writeProxyHealthEvents(dir, report, now)
		}
	}
```

- [ ] **Step 3: Build to verify it compiles**

Run: `go build -o provider_bin ./provider`
Expected: exit 0.

- [ ] **Step 4: Run provider tests**

Run: `go test ./provider/ -v`
Expected: PASS (formatters + any existing provider tests).

- [ ] **Step 5: Commit**

```bash
git add provider/main.go
git commit -m "feat: emit [health][proxies] lines and persist proxy health each heartbeat"
```

---

## Task 8: Pulse-fire marker

**Files:**
- Modify: `provider/main.go` (hourly pulse goroutine ~854-863)

- [ ] **Step 1: Add the marker before TriggerPulse**

In `provider/main.go`, replace the hourly pulse goroutine body:

```go
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Hour):
				connect.TriggerPulse()
			}
		}
	}()
```

with:

```go
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Hour):
				if connect.ProxyHealthCount() > 0 {
					up, dead, degraded := connect.ProxyHealthSnapshot()
					down := len(dead) + len(degraded)
					_ = up
					fmt.Printf("[pulse] waking stalled transports: down=%d dead=%d degraded=%d\n",
						down, len(dead), len(degraded))
				}
				connect.TriggerPulse()
			}
		}
	}()
```

- [ ] **Step 2: Build to verify it compiles**

Run: `go build -o provider_bin ./provider`
Expected: exit 0.

- [ ] **Step 3: Commit**

```bash
git add provider/main.go
git commit -m "feat: log [pulse] marker with pre-pulse down counts"
```

---

## Task 9: `urnet-tools proxy health` (host)

**Files:**
- Modify: `scripts/Provider_Install_Linux.sh` (`do_proxy()` ~1565-1587; `usage()` ~34-35)

- [ ] **Step 1: Add the `health` case to `do_proxy()`**

In `scripts/Provider_Install_Linux.sh`, inside `do_proxy()`'s `case "$cmd" in`, add a new case before the `*)` default:

```bash
        health)
            health_dir="${URNETWORK_PROXY_HEALTH_DIR:-$HOME/.urnetwork}"
            state_file="$health_dir/proxy_health.state"
            log_file="$health_dir/proxy_health.log"
            if [ -f "$state_file" ]; then
                pr_info "Current proxy health ($state_file):"
                cat "$state_file"
            else
                pr_warn "No snapshot yet at %s (waiting for first heartbeat?)." "$state_file"
            fi
            if [ -f "$log_file" ]; then
                echo
                pr_info "Streaming proxy health events ($log_file). Ctrl-C to stop."
                tail -n 20 -f "$log_file"
            else
                pr_warn "No event log yet at %s." "$log_file"
            fi
            ;;
```

- [ ] **Step 2: Add `proxy health` to the usage text**

In `scripts/Provider_Install_Linux.sh` `usage()`, after the `proxy clear` line (~35), add:

```bash
    echo "  proxy health            ❤️  HEALTH: show dead/degraded proxies + live event log"
```

- [ ] **Step 3: Syntax-check the script**

Run: `bash -n scripts/Provider_Install_Linux.sh`
Expected: exit 0, no output.

- [ ] **Step 4: Commit**

```bash
git add scripts/Provider_Install_Linux.sh
git commit -m "feat: urnet-tools proxy health command"
```

---

## Task 10: Docker `proxy-health` helper

**Files:**
- Create: `docker/scripts/proxy-health.sh`
- Modify: `Dockerfile` (after the `COPY docker/scripts/*.sh /app/` + chmod step ~44-51)

- [ ] **Step 1: Create the helper script**

Create `docker/scripts/proxy-health.sh`:

```sh
#!/bin/sh
# proxy-health -- show the persistent dead/degraded proxy record inside the container.
# RAMLOGS-independent: these files always live on the config volume.
set -eu

health_dir="${URNETWORK_PROXY_HEALTH_DIR:-/root/.urnetwork}"
state_file="$health_dir/proxy_health.state"
log_file="$health_dir/proxy_health.log"

if [ -f "$state_file" ]; then
    echo "== Current proxy health ($state_file) =="
    cat "$state_file"
else
    echo "No snapshot yet at $state_file (waiting for first heartbeat?)."
fi

if [ -f "$log_file" ]; then
    echo
    echo "== Proxy health events ($log_file) -- Ctrl-C to stop =="
    tail -n 20 -f "$log_file"
else
    echo "No event log yet at $log_file."
fi
```

- [ ] **Step 2: Install it into PATH in the Dockerfile**

In `Dockerfile`, immediately after the existing `RUN dos2unix /app/*.sh ... && chmod +x /app/*.sh ...` line (~51), add:

```dockerfile
# Expose the proxy-health helper on PATH for `docker exec <c> proxy-health`
RUN ln -sf /app/proxy-health.sh /usr/local/bin/proxy-health
```

- [ ] **Step 3: Syntax-check the helper**

Run: `sh -n docker/scripts/proxy-health.sh`
Expected: exit 0.

- [ ] **Step 4: Commit**

```bash
git add docker/scripts/proxy-health.sh Dockerfile
git commit -m "feat: docker proxy-health helper on PATH"
```

---

## Task 11: Documentation

**Files:**
- Modify: `LOG_REFERENCE.md`, `CHANGELOG.md`, `FORK_CHANGES.md`, `docs/Configuration.md`, `docs/Docker-Deployment.md`, `README.md`

- [ ] **Step 1: LOG_REFERENCE.md** — under the Health Heartbeat section, document the `[health][proxies]` summary fields (`up`/`down`/`dead`/`degraded`/`recovered`/`lost`/`lifetime_recovered`/`lifetime_lost`), the `dead` (never authenticated) vs `degraded` (was up, now down) labels, the 50-entry stdout cap, the `[pulse]` marker line, and the persistent files (`proxy_health.state`, `proxy_health.log`). Note the "trustworthy after ~1h" framing for `dead`. Use the exact sample lines from `docs/design/dead-proxy-health-report.md`.

- [ ] **Step 2: CHANGELOG.md** — add to `[Unreleased] > Added`:

```markdown
- **Dead-Proxy Health Report**: The `[health]` heartbeat now emits `[health][proxies]` lines listing `dead` (never authenticated) and `degraded` (worked before, down now) proxies, plus `recovered`/`lost` and `lifetime_recovered`/`lifetime_lost` counters that make the hourly retry pulse's effectiveness visible. A `[pulse]` marker logs each retry sweep. Full dead/degraded lists and a transition history are mirrored to `proxy_health.state` and `proxy_health.log` on the config volume (survives RAMLOGS), readable via `urnet-tools proxy health` (host) or `proxy-health` (Docker).
```

- [ ] **Step 3: FORK_CHANGES.md** — add a bullet noting the new observability surface and the access commands (host + Docker), referencing the design doc.

- [ ] **Step 4: docs/Configuration.md** — add a "Viewing proxy health" subsection: host `urnet-tools proxy health`; note `URNETWORK_PROXY_HEALTH_DIR` (default `<home>/.urnetwork`) and `URNETWORK_HEALTH_INTERVAL` interaction.

- [ ] **Step 5: docs/Docker-Deployment.md** — add the Docker viewing commands and the RAMLOGS distinction, copied from the spec's Documentation section:
  - Persistent (always): `docker exec -it <container> proxy-health`
  - Live-tail RAMLOGS on: `docker exec -it <c> sh -c "tail -f /dev/shm/urnetwork.log | grep -E '\[health\]\[proxies\]|\[pulse\]'"`
  - Live-tail RAMLOGS off: `docker logs -f <c> 2>&1 | grep -E '\[health\]\[proxies\]|\[pulse\]'`

- [ ] **Step 6: README.md** — add `proxy health` to the command overview next to the other `urnet-tools` / proxy commands, with a one-line description and a pointer to the docs.

- [ ] **Step 7: Commit**

```bash
git add LOG_REFERENCE.md CHANGELOG.md FORK_CHANGES.md docs/Configuration.md docs/Docker-Deployment.md README.md
git commit -m "docs: document dead-proxy health report, counters, and access commands"
```

> Wiki: the `docs/*.md` pages mirror GitHub wiki pages. After merge, update the corresponding wiki pages (Configuration, Docker Deployment, and any Logging/Commands page) to match Steps 4-6. (Wiki is a separate git repo, not part of this PR.)

---

## Task 12: Full build, test, and live verification

**Files:** none (verification only)

- [ ] **Step 1: Full test suite**

Run: `./test.sh`
Expected: PASS (includes the new `connect` and `provider` tests).

- [ ] **Step 2: Build for current platform**

Run: `go build -o provider_bin ./provider`
Expected: exit 0.

- [ ] **Step 3: Push branch and build the Docker image**

Push the branch, then trigger the workflow_dispatch build (the `pull_request` event does NOT push images; only non-PR events do):

```bash
git push
gh workflow run build.yml --ref feature/dead-proxy-health-report
gh run watch "$(gh run list --branch feature/dead-proxy-health-report --event workflow_dispatch --limit 1 --json databaseId -q '.[0].databaseId')" --exit-status
```

Expected: build succeeds and pushes `ghcr.io/full-bars/urnetwork-3.23-fix:feature-dead-proxy-health-report`.

- [ ] **Step 4: Deploy to the Detroit test node**

```bash
ssh -o StrictHostKeyChecking=no user@100.116.23.13 'docker pull ghcr.io/full-bars/urnetwork-3.23-fix:feature-dead-proxy-health-report && docker stop urtest && docker rm urtest && docker run -d --name urtest --restart unless-stopped -p 9001:8080 --cap-add CAP_NET_ADMIN --cap-add CAP_NET_RAW --sysctl net.ipv4.ip_forward=1 -e ENABLE_VNSTAT=true -e URNETWORK_RAMLOGS=1 -e URNETWORK_PROFILE=auto -e BUILD=jwt -v /home/user/ur-docker/config:/root/.urnetwork -v /home/user/ps1200_combined.txt:/app/proxy.txt ghcr.io/full-bars/urnetwork-3.23-fix:feature-dead-proxy-health-report'
```

- [ ] **Step 5: Verify the heartbeat output**

After the first heartbeat (~5 min), check the live log:

```bash
ssh user@100.116.23.13 'grep -a "\[health\]\[proxies\]" /dev/shm/urnetwork.log | tail -3'
```

Expected: a summary line where `up + down == 1200` (list size) and `dead + degraded == down`, plus `dead:`/`degraded:` detail lines.

- [ ] **Step 6: Verify the persistent files + Docker command**

```bash
ssh user@100.116.23.13 'docker exec urtest sh -c "ls -la /root/.urnetwork/proxy_health.* ; head -5 /root/.urnetwork/proxy_health.state"'
ssh user@100.116.23.13 'docker exec urtest proxy-health' # Ctrl-C after confirming output (interactive tail)
```

Expected: `proxy_health.state` lists the complete dead/degraded set with no `(+N more)`; `proxy-health` prints the snapshot.

- [ ] **Step 7: Verify recovery + pulse over time (optional, >1h)**

After an hourly pulse, confirm a `[pulse]` line appears and `recovered` climbs / `lifetime_recovered` accumulates in the next heartbeat, and that `proxy_health.log` has `RECOVERED`/`DEGRADED` (and `DEAD` after ~65 min) lines.

```bash
ssh user@100.116.23.13 'grep -aE "\[pulse\]" /dev/shm/urnetwork.log | tail -2'
ssh user@100.116.23.13 'docker exec urtest tail -20 /root/.urnetwork/proxy_health.log'
```

---

## Self-Review Notes (resolved)

- **Spec coverage:** registry (Task 1-3) ↔ spec Component 1; instrumentation (Task 4) ↔ Component 2; heartbeat output (Task 7) ↔ Component 3; persistence (Task 5-7) ↔ Component 5; pulse marker (Task 8) ↔ Component 4; access commands (Task 9-10) ↔ Component 6; interval handling (Task 7 uses `URNETWORK_HEALTH_INTERVAL` via existing ticker, deltas interval-agnostic) ↔ spec "Configurable interval"; docs (Task 11) ↔ spec Documentation.
- **Display cap is stdout-only** (Task 5 `capProxyList` used only in Task 7 stdout prints; `formatStateFile`/`formatEventLines` never cap) — matches the spec's emphasis.
- **Type consistency:** `ProxyHealthReport`, `ProxyEvent`, `ProxyHealthHeartbeat(bool)`, `ProxyHealthSnapshot()`, `ProxyHealthCount()`, `RegisterProxy`, `markProxyUp/Down` names match across `connect` and `provider` packages.
- **`NewlyDead` semantics corrected** from the original spec draft (never-up confirmed-once, not an unreachable up->down+everUp==false case); spec updated to match.
