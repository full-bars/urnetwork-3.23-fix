# Plan: Replace direct `glog.*` calls with the `Logger` interface (`self.log`)

**Date:** 2026-06-15
**Repo:** `/home/full-bars/ur/urnetwork-3.23-fix`
**Branch:** `refactor/glog-to-logger` (new branch off `master`)
**Upstream reference (already refactored):** `/home/full-bars/ur/connect`

## Why

The fork has **357 direct `glog.*` calls across 21 `.go` files**. Upstream replaced
all of them with a `Logger` interface so embedders can silence the library. Every
upstream rebase that touches a core file collides with the fork's surviving `glog.*`
lines. This refactor brings the fork to the same end-state, eliminating that recurring
conflict surface. The pattern is mechanical and already proven in `/home/full-bars/ur/connect`.

## Mandatory workflow constraints (read first)

> [!IMPORTANT]
> **All work goes through a new branch and a PR.** Do not commit to `master`.
> 1. `cd /home/full-bars/ur/urnetwork-3.23-fix && git checkout master && git checkout -b refactor/glog-to-logger`
> 2. Each task below is one commit on that branch.
> 3. After the final task, push the branch and open a PR (`gh pr create`). Do **not** merge — the user signs off.
> 4. No AI attribution anywhere: no `Co-Authored-By`, no AI-crediting text in commits/PR body/files.
> 5. No `CLAUDE.md` / AI-indicating files committed.

## Invariants for every task

- After **every** task: `go build ./...` must pass.
- Before **every** commit: `go vet ./...` then `go test ./...`.
- The translation is **behavior-preserving**. Do not change log strings, levels, or
  the `\n` suffixes. Only change the call target (`glog.X` → `self.log.X` /
  `self.client.log.X` / `DefaultLogger().X`).
- Do **not** touch fork-specific log lines' content (traffic-rate logging, client-age
  fix, `[direct]` path label). Only retarget the call.
- Remove the `glog` import from a file **only in the final task**, after confirming the
  file has zero remaining `glog.` references.

## The four call-site translations (apply everywhere)

| Before (`glog`) | After |
|---|---|
| `glog.Infof(fmt, args...)` | `self.log.Infof(fmt, args...)` |
| `glog.Info(args...)` | `self.log.Info(args...)` |
| `glog.Errorf(fmt, args...)` | `self.log.Errorf(fmt, args...)` |
| `glog.Warningf(fmt, args...)` | `self.log.Warningf(fmt, args...)` |
| `glog.V(n).Infof(fmt, args...)` | `self.log.V(n).Infof(fmt, args...)` |
| `if glog.V(n) { ... glog.Infof(...) ... }` | `if self.log.V(n).Enabled() { ... self.log.V(n).Infof(...) ... }` |

The logger expression on the left of `.Infof` depends on what the struct holds:

- Struct has its **own** `log Logger` field → `self.log`
- Struct has a `client *Client` field but **no** own `log` field → `self.client.log`
- **Process-global / free function** with no receiver and no `*Client` → `DefaultLogger()`
- **Free function that takes a `log Logger` parameter** → `log`

> [!NOTE]
> The `if glog.V(n) { glog.Infof(...) }` guarded form is common in `transfer.go` and
> `ip.go`. The guard becomes `if self.log.V(n).Enabled() {` and **each** `glog.Infof`
> inside becomes `self.log.V(n).Infof`. Verify by grepping `grep -n "glog.V(.*) {" <file>`
> in each file before translating it.

---

## Task 1 — Install `log.go` (the `Logger` shim)

The fork's `log.go` is currently a 27-line doc-comment stub: the `Logger` interface,
`glogLogger`, `noopLogger`, `DefaultLogger`, `SetDefaultLogger`, and `loggerOrDefault`
are **not present**. Copy the upstream file verbatim.

```bash
cp /home/full-bars/ur/connect/log.go /home/full-bars/ur/urnetwork-3.23-fix/log.go
```

The resulting file must contain (this is the verbatim upstream content — confirm it matches):

```go
package connect

import (
	"sync/atomic"

	"github.com/urnetwork/glog"
)

// ... (preserved doc comment) ...

type Logger interface {
	Info(args ...any)
	Infof(format string, args ...any)
	Warningf(format string, args ...any)
	Errorf(format string, args ...any)
	V(level int32) Verbose
}

type Verbose interface {
	Enabled() bool
	Info(args ...any)
	Infof(format string, args ...any)
}

var defaultLogger atomic.Pointer[Logger]

func DefaultLogger() Logger {
	if log := defaultLogger.Load(); log != nil {
		return *log
	}
	return glogLogger{}
}

func SetDefaultLogger(log Logger) {
	if log == nil {
		defaultLogger.Store(nil)
	} else {
		defaultLogger.Store(&log)
	}
}

func loggerOrDefault(log Logger) Logger {
	if log == nil {
		return DefaultLogger()
	}
	return log
}

func NewGlogLogger() Logger { return glogLogger{} }

type glogLogger struct{}

func (self glogLogger) Info(args ...any)                  { glog.InfoDepth(1, args...) }
func (self glogLogger) Infof(format string, args ...any)  { glog.InfoDepthf(1, format, args...) }
func (self glogLogger) Warningf(format string, args ...any){ glog.WarningDepthf(1, format, args...) }
func (self glogLogger) Errorf(format string, args ...any) { glog.ErrorDepthf(1, format, args...) }
func (self glogLogger) V(level int32) Verbose {
	return glogVerbose(glog.VDepth(1, glog.Level(level)))
}

type glogVerbose glog.Verbose

func (self glogVerbose) Enabled() bool                    { return bool(self) }
func (self glogVerbose) Info(args ...any)                 { glog.Verbose(self).InfoDepth(1, args...) }
func (self glogVerbose) Infof(format string, args ...any) { glog.Verbose(self).InfoDepthf(1, format, args...) }

func NewNoopLogger() Logger { return noopLogger{} }

type noopLogger struct{}

func (self noopLogger) Info(args ...any)                   {}
func (self noopLogger) Infof(format string, args ...any)   {}
func (self noopLogger) Warningf(format string, args ...any){}
func (self noopLogger) Errorf(format string, args ...any)  {}
func (self noopLogger) V(level int32) Verbose              { return noopVerbose{} }

type noopVerbose struct{}

func (self noopVerbose) Enabled() bool                     { return false }
func (self noopVerbose) Info(args ...any)                  {}
func (self noopVerbose) Infof(format string, args ...any)  {}
```

> The literal source above is condensed for readability; use `cp` to get the exact
> upstream formatting. After `cp`, run `diff log.go /home/full-bars/ur/connect/log.go` —
> it must report no differences.

**Verify & commit:**
```bash
go build ./...   # log.go compiles; glogLogger satisfies Logger
go vet ./...
go test ./...
git add log.go && git commit -m "feat: add Logger interface shim (log.go)"
```

---

## Task 2 — `transfer.go` + `transfer_contract_manager.go` + `transfer_route_manager.go` + `transfer_control.go` + `transfer_control_oob.go` + `transfer_key.go` + `transfer_stream_manager.go` + `transfer_rtt.go`

This is the `Client` hierarchy: one logger resolved at `NewClientWithTag`, then threaded
to every nested struct. **126 glog calls total** (93 + 22 + 6 + 3 + 1 + 1 + 1 — `transfer_rtt`
is wired here because `SendSequence` constructs `RttWindow`).

### 2a. `transfer.go` — struct fields to add

Add a `Log Logger` field to **`ClientSettings`** (after `ControlPingTimeout`, before the
nested `*Settings` block), with the upstream doc comment:

```go
	// Log, when set, is used by the client and all nested components
	// (propagated to nested settings `Log` fields that are nil).
	// nil resolves to `DefaultLogger()`. See log.go.
	Log Logger
```

Add a private `log Logger` field to these implementation structs:

| Struct | Place the field |
|---|---|
| `Client` | after `clientOob OutOfBandControl` |
| `SendBuffer` | after `client *Client` |
| `SendSequence` | after `sendBuffer *SendBuffer` |
| `ReceiveBuffer` | after `client *Client` |
| `ReceiveSequence` | after its `client *Client` |
| `sequenceContract` | first field (it has no `*Client`) |
| `ForwardSequence` | after `clientTag string` (upstream places it right after `clientTag`) |
| `SequencePeerAudit` | after its `client *Client` |

> `ForwardBuffer`, `SendBuffer`, `ReceiveBuffer` all hold `client *Client`. Upstream gives
> `SendBuffer`/`ReceiveBuffer` their own `log` field but **not** `ForwardBuffer` (it only
> creates `ForwardSequence`, which carries its own). Follow that exactly: add `log` to
> `SendBuffer`, `ReceiveBuffer`, `SendSequence`, `ReceiveSequence`, `ForwardSequence`,
> `sequenceContract`, `SequencePeerAudit`, `Client`. Do **not** add to `ForwardBuffer`.

### 2b. `transfer.go` — constructor wiring

**`NewClientWithTag`** — resolve and propagate. Right after `cancelCtx, cancel := context.WithCancel(ctx)`:

```go
	log := loggerOrDefault(settings.Log)
	// nested components without a client reference resolve their own settings
	// `Log`. Propagate so a client-level logger covers the entire client tree.
	if settings.WebRtcSettings != nil && settings.WebRtcSettings.Log == nil {
		settings.WebRtcSettings.Log = log
	}
```

Add `log: log,` to the `client := &Client{...}` literal (after `clientOob: clientOob,`).

Change the route-manager construction from the fork's current
`NewRouteManager(ctx, clientTag)` (or equivalent) to:

```go
	routeManager := NewRouteManagerWithLogger(ctx, clientTag, log)
```

Add the accessor (used by tests/embedders; upstream has it):

```go
// Log is the logger used by this client and its nested components.
func (self *Client) Log() Logger {
	return self.log
}
```

**Buffer/sequence constructors** — each receives `client *Client`, so wire `log: client.log,`
into the struct literal:

- `NewSendBuffer` → add `log: client.log,` to the `&SendBuffer{...}` literal.
- `NewReceiveBuffer` → add `log: client.log,` to the `&ReceiveBuffer{...}` literal.
- `NewSendSequence` → add `log: client.log,` to the `&SendSequence{...}` literal.
- `NewReceiveSequence` → add `log: client.log,` to the `&ReceiveSequence{...}` literal.
- `NewForwardSequence` → add `log: client.log,` to the `&ForwardSequence{...}` literal.
- `NewSequencePeerAudit` → add `log: client.log,` to the `&SequencePeerAudit{...}` literal.

**`NewSendSequence` RttWindow call** — `NewRttWindow` gains a leading `log Logger` param
(see 2h). Update the call in `NewSendSequence`:

```go
	rttWindow := NewRttWindow(
		client.log,
		sendBufferSettings.RttWindowSize,
		sendBufferSettings.RttWindowTimeout,
		sendBufferSettings.RttScale,
		sendBufferSettings.MinResendInterval,
		sendBufferSettings.MaxResendInterval,
	)
```

**`newSequenceContract`** — change signature to take `log Logger` as the first param and
wire `log: log,` into the `&sequenceContract{...}` literal:

```go
func newSequenceContract(log Logger, tag string, contract *protocol.Contract, minUpdateByteCount ByteCount, contractFillFraction float32) (*sequenceContract, error) {
	...
	return &sequenceContract{
		log:    log,
		...
	}, nil
}
```

Every caller of `newSequenceContract` must now pass the logger. In `SendSequence` /
`ReceiveSequence` methods, the caller is `self`, so pass `self.log`:

```go
	nextSendContract, err := newSequenceContract(
		self.log,
		"[s]",
		...
	)
```

Grep all call sites: `grep -n "newSequenceContract(" transfer.go` and prepend the right
logger expression (`self.log` inside `SendSequence`/`ReceiveSequence` methods).

### 2c. `transfer.go` — call-site translation (real examples from this file)

```
line 1001:  glog.V(1).Infof("[cr] %s cannot route message with stream\n", self.clientTag)
        ->  self.log.V(1).Infof("[cr] %s cannot route message with stream\n", self.clientTag)
```
(in a `Client` method → `self.log`)

```
line 3935:  glog.V(1).Infof("[r]head %d (%s) %s<-%s s(%s)\n", item.sequenceNumber, frameMessageTypesStr, self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
        ->  self.log.V(1).Infof("[r]head %d (%s) %s<-%s s(%s)\n", item.sequenceNumber, frameMessageTypesStr, self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
```
(in a `ReceiveSequence` method, which now has its own `log` → `self.log`)

Guarded blocks: run `grep -n "glog.V(.*) {" transfer.go`. For each, change
`if glog.V(2) {` → `if self.log.V(2).Enabled() {` and the inner `glog.Infof` →
`self.log.V(2).Infof`.

### 2d. `transfer_contract_manager.go` (22 calls)

`ContractManager` holds `client *Client` and has **no** own log field. Translate every
`glog.X` in `ContractManager` methods to `self.client.log.X`. Real example:

```
glog.Infof("[contract]provide ping wait\n")
   ->  self.client.log.Infof("[contract]provide ping wait\n")
```

`contractQueue` is the one struct here that gets its own `log Logger` field (upstream).
Add `log Logger` to `contractQueue` and wire it in its constructor:
```go
		log: loggerOrDefault(log),
```
Find the `contractQueue` constructor (`grep -n "contractQueue{" transfer_contract_manager.go`)
and ensure the function that builds it receives/passes a logger; upstream passes the
`ContractManager`'s `self.client.log` at the call site. Inside `contractQueue` methods,
`glog.X` → `self.log.X`.

### 2e. `transfer_route_manager.go` (6 calls)

Structs `RouteManager`, `MatchState`, `MultiRouteSelector` each get a `log Logger` field.
The entry point changes:

```go
func NewRouteManager(ctx context.Context, clientTag string) *RouteManager {
	return NewRouteManagerWithLogger(ctx, clientTag, nil)
}

func NewRouteManagerWithLogger(ctx context.Context, clientTag string, log Logger) *RouteManager {
	log = loggerOrDefault(log)
	return &RouteManager{
		...
		log: log,
		...
	}
}
```

`MatchState` and `MultiRouteSelector` constructors take/propagate `log` and wire
`log: loggerOrDefault(log),`. Inside their methods, `glog.X` → `self.log.X`. Find the
constructors with `grep -n "func New" transfer_route_manager.go` and thread the
`RouteManager`'s `self.log` through to them.

### 2f. `transfer_control.go` (3 calls) and `transfer_control_oob.go` (1 call)

Both have receivers holding `client *Client`. Translate `glog.X` → `self.client.log.X`.
Real example:
```
glog.V(2).Infof("[control][%d]start sync for scope = %s\n", syncIndex, self.scopeTag)
   ->  self.client.log.V(2).Infof("[control][%d]start sync for scope = %s\n", syncIndex, self.scopeTag)
```

### 2g. `transfer_key.go` (1 call) and `transfer_stream_manager.go` (1 call)

Both have receivers holding `client *Client`. `glog.X` → `self.client.log.X`. Example:
```
glog.Errorf("[key]%s could not build ClientKey frame: %s\n", self.client.ClientTag(), err)
   ->  self.client.log.Errorf("[key]%s could not build ClientKey frame: %s\n", self.client.ClientTag(), err)
```

### 2h. `transfer_rtt.go` (1 call)

`RttWindow` gets a `log Logger` field. Change the constructor to take a leading
`log Logger` param and wire it:

```go
func NewRttWindow(
	log Logger,
	windowSize int,
	windowTimeout time.Duration,
	rttScale float32,
	minScaledRtt time.Duration,
	maxScaledRtt time.Duration,
) *RttWindow {
	return &RttWindow{
		log: loggerOrDefault(log),
		...
	}
}
```

Inside `RttWindow` methods, `glog.X` → `self.log.X`. The only caller of `NewRttWindow`
is `NewSendSequence` (already updated in 2b to pass `client.log`). Confirm no other
callers: `grep -rn "NewRttWindow(" .`.

**Verify & commit:**
```bash
go build ./...
go vet ./...
go test ./...
git add transfer.go transfer_contract_manager.go transfer_route_manager.go \
        transfer_control.go transfer_control_oob.go transfer_key.go \
        transfer_stream_manager.go transfer_rtt.go
git commit -m "refactor: route transfer/contract/route logging through Logger"
```

---

## Task 3 — `transfer_encrypt.go` (44 calls)

The encryption session structs hold `client *Client`. Most calls become
`self.client.log.X`. Free helper functions take a `log Logger` param.

### Receiver methods
Every `glog.X` inside a method whose receiver has a `client *Client` →
`self.client.log.X`. Real examples:
```
self.client.log.V(1).Infof("[tls]%s session idle — reaped\n", self.logTag)        // was glog.V(1).Infof
self.client.log.Errorf("[tls]%s completeHandshake failed: %s\n", self.logTag, err) // was glog.Errorf
```

### Free helper functions (take `log Logger`)
Upstream adds a `log Logger` parameter to these package-level helpers and passes the
caller's logger:

- `peerCertificatesOfEpoch(log Logger, e *tlsHandshakeEpoch)`
- `logTlsHandshake(log Logger, ...)`
- `logTlsHandshakePeerCert(log Logger, ...)`

Inside these helpers, `glog.X` → `log.X`. At each call site (in receiver methods),
pass `self.client.log`:
```go
	logTlsHandshake(self.client.log, self.logTag, err)
	logTlsHandshakePeerCert(self.client.log, self.logTag, peerCertificatesOfEpoch(self.client.log, e))
```

Find them: `grep -n "func peerCertificatesOfEpoch\|func logTlsHandshake\|glog\." transfer_encrypt.go`.
Cross-check the exact set of helper functions and signatures against
`/home/full-bars/ur/connect/transfer_encrypt.go` (they match the fork structurally).

**Verify & commit:**
```bash
go build ./... && go vet ./... && go test ./...
git add transfer_encrypt.go && git commit -m "refactor: route transfer_encrypt logging through Logger"
```

---

## Task 4 — `ip.go` (57 calls) + `ip_remote_multi_client.go` (34) + `ip_remote_multi_client_api.go` (1)

### `ip.go`
Structs that get their own `log Logger` field (upstream): `LocalUserNat`, `UdpBuffer`,
`UdpSequence`, `TcpBuffer`, `TcpSequence`. Add `log Logger` to each, and wire it in
their constructors:

- Top-level constructor (e.g. `NewLocalUserNat*`) resolves from its settings:
  `log := loggerOrDefault(settings.Log)` and sets `log: log,`. If `LocalUserNatSettings`
  (or the relevant settings struct) lacks a `Log Logger` field, add one (with the
  standard doc comment). Cross-check field name against `/home/full-bars/ur/connect/ip.go`.
- Nested buffer/sequence constructors receive the parent's logger and set
  `log: <parent>.log,` (parent is the `LocalUserNat` or buffer that creates them). Where
  a constructor takes no parent reference, thread a `log Logger` param through, matching
  upstream's `ip.go` signatures.

There are 5 `loggerOrDefault` resolution points and 8 `self.client.log`-style accesses
in upstream `ip.go`; replicate them. The reliable procedure:

1. `grep -n "glog\." ip.go` — list all 57.
2. For each, identify the receiver. If the receiver struct now has `log` → `self.log`.
   If it holds a `client *Client` but no `log` → `self.client.log`. If it's a free
   function → ensure it has a `log Logger` param.
3. Diff your constructor signatures and struct field placements against
   `/home/full-bars/ur/connect/ip.go` to confirm parity.

### `ip_remote_multi_client.go`
Structs `RemoteUserNatMultiClient`, `multiClientWindow`, `clientWindowStats`,
`multiClientChannel` each get a `log Logger` field. Top constructor resolves from
settings (`loggerOrDefault(settings.Log)` — add `Log Logger` to the multi-client
settings struct if missing, matching upstream). One free function takes a `log Logger`
param. Translate receiver-method `glog.X` → `self.log.X`. There are 4 `loggerOrDefault`
points upstream; replicate.

> [!NOTE]
> `ip_remote_multi_client.go` structs do **not** hold a `*Client` (multi-client manages
> many clients), so they must carry their own `log` field — do not reach for
> `self.client.log` here.

### `ip_remote_multi_client_api.go` (1 call)
The single occurrence is a commented-out line in upstream
(`// self.log.Infof("[multi]eval fixed ...")`). Check the fork's line:
`grep -n "glog\." ip_remote_multi_client_api.go`. If the receiver has a `log` field
(added above) → `self.log.X`; if commented out, just retarget the comment text to match.

**Verify & commit:**
```bash
go build ./... && go vet ./... && go test ./...
git add ip.go ip_remote_multi_client.go ip_remote_multi_client_api.go
git commit -m "refactor: route ip/multi-client logging through Logger"
```

---

## Task 5 — Transports: `transport.go` (29) + `transport_pt.go` (21) + `transport_p2p_webrtc.go` (8)

### `transport.go`
`PlatformTransportSettings` gets a `Log Logger` field (upstream has it at
`PlatformTransportSettings`). `PlatformTransport` gets a `log Logger` field. The
constructor that does the real work is `NewPlatformTransportWithTargetMode` — resolve
and wire there:

```go
	log := loggerOrDefault(settings.Log)
	...
	transport := &PlatformTransport{
		...
		log: log,
		...
	}
```

(`NewPlatformTransport` and `NewPlatformTransportWithDefaults` delegate down to
`...WithTargetMode`, so they need no change beyond passing settings through.)
Receiver methods: `glog.X` → `self.log.X`.

### `transport_pt.go`
`PacketTranslationSettings` gets a `Log Logger` field; `packetTranslation` gets a
`log Logger` field. The work constructor is `NewPacketTranslationWithPrefix`:
```go
		log: loggerOrDefault(settings.Log),
```
Receiver methods: `glog.X` → `self.log.X`.

### `transport_p2p_webrtc.go`
`WebRtcSettings` already gets `Log` propagated from `NewClientWithTag` (Task 2b).
Structs `WebRtcManager` and `peerConn` use the logger; the pion logging adapters
`pionLoggerFactory` / `pionLeveledLogger` get a `log Logger` field. Upstream has 2
`loggerOrDefault` resolution points and 4 `client.log` accesses and 1 `log Logger`
param here. Procedure:

1. Add `Log Logger` to `WebRtcSettings` if not present (cross-check
   `/home/full-bars/ur/connect/transport_p2p_webrtc.go`).
2. `NewWebRtcManager` resolves `loggerOrDefault(settings.Log)` and stores it on the
   manager; propagate to `peerConn` and to `pionLoggerFactory{log: ...}`.
3. Translate the 8 `glog.X` calls: receiver methods → `self.log.X` (or `self.client.log.X`
   where a `*Client` is held — match upstream exactly).

**Verify & commit:**
```bash
go build ./... && go vet ./... && go test ./...
git add transport.go transport_pt.go transport_p2p_webrtc.go
git commit -m "refactor: route transport logging through Logger"
```

---

## Task 6 — `net.go` (3) + `net_http.go` (21) + `message_framer.go` (3) + `message_pool.go` (4) + `trace.go` (3) + `tuning.go` (1)

### `net.go` + `net_http.go` (these share `ClientStrategy`)
`ClientStrategySettings` (defined in `net.go`) gets a `Log Logger` field. `ClientStrategy`
(struct + constructor in `net_http.go`) gets a `log Logger` field, wired in
`NewClientStrategy`:
```go
		log: loggerOrDefault(settings.Log),
```
- In `net.go`, the standalone resolution `if log := loggerOrDefault(self.Log).V(2); log.Enabled() {`
  pattern is used where there's no `ClientStrategy` receiver — replicate upstream's
  `net.go` exactly (it resolves `loggerOrDefault(self.Log)` on a settings-bearing
  receiver). Cross-check `/home/full-bars/ur/connect/net.go` lines around the 3 calls.
- In `net_http.go`, receiver methods on `ClientStrategy` → `self.log.X`. One free
  function takes a `log Logger` param upstream — match it.

### `message_framer.go` (3 calls)
`Framer` gets a `log Logger` field, resolved in its constructor from its settings
(`FramerSettings` gets a `Log Logger` field; wire `log: loggerOrDefault(settings.Log)`).
Receiver methods → `self.log.X`. Cross-check `/home/full-bars/ur/connect/message_framer.go`.

### `message_pool.go` (4 calls) — process-global
The message pool is process-global (no per-client logger). Upstream uses
`DefaultLogger()` directly. Translate:
```
glog.Infof(...)    -> DefaultLogger().Infof(...)
glog.Errorf(...)   -> DefaultLogger().Errorf(...)
glog.Warningf(...) -> DefaultLogger().Warningf(...)
```
Real example:
```
DefaultLogger().Warningf("[mp]share message[%d] not taken", id)   // was glog.Warningf
```

### `trace.go` (3 calls) — process-global
Same as message_pool: `glog.X` → `DefaultLogger().X`. Examples:
```
DefaultLogger().Warningf("Unexpected error: %s\n", ErrorJson(r, debug.Stack()))
DefaultLogger().Infof("[%-8s]%s (%d)\n", "start", tag, start.UnixMilli())
```

### `tuning.go` (1 call)
Determine the context with `grep -n "glog\." tuning.go`. If it's in a receiver with a
`log`/`client` → use that; if process-global → `DefaultLogger().X`. Match upstream
`/home/full-bars/ur/connect/tuning.go`.

**Verify & commit:**
```bash
go build ./... && go vet ./... && go test ./...
git add net.go net_http.go message_framer.go message_pool.go trace.go tuning.go
git commit -m "refactor: route net/framer/pool/trace logging through Logger"
```

---

## Task 7 — Remove now-unused `glog` imports

At this point **only `log.go`** should reference `glog` directly. Confirm:

```bash
cd /home/full-bars/ur/urnetwork-3.23-fix
# Should print ONLY log.go:
grep -rl "glog\." *.go
# Files still importing glog but no longer calling it:
comm -23 <(grep -rl '"github.com/urnetwork/glog"' *.go | sort) <(grep -rl 'glog\.' *.go | sort)
```

The cleanup set (files importing `glog` with zero `glog.` calls remaining) will be the
21 refactored files **plus** these 4 that already import `glog` without using it (and
upstream keeps importing — verify whether upstream actually still imports them; if
upstream dropped the import, drop it here too):
`api.go`, `ip_security.go`, `net_http_doh.go`, `net_resilient.go`.

> [!IMPORTANT]
> `goimports` / `gofmt` will **not** auto-remove an unused import — Go treats an unused
> import as a **compile error**, so `go build` already fails if any import is dangling.
> Remove the `"github.com/urnetwork/glog"` line from every file that no longer references
> `glog`, fixing up the import grouping (keep the blank line between stdlib and
> third-party groups). The reliable approach:

```bash
# For each file in the grep list above (excluding log.go), open it and delete the
# glog import line. Then:
gofmt -w *.go
go build ./...    # any remaining unused/needed import surfaces here
```

> [!NOTE]
> For `api.go`, `ip_security.go`, `net_http_doh.go`, `net_resilient.go`: these already
> import `glog` without a `glog.` call in the **current** fork (pre-existing). Check
> whether upstream still imports glog in each (`grep -c 'urnetwork/glog'
> /home/full-bars/ur/connect/<file>`). If upstream keeps it, leave it. If upstream removed
> it, remove it here. Do not introduce a divergence from upstream in either direction.

**Verify & commit:**
```bash
go build ./... && go vet ./... && go test ./...
git add -A && git commit -m "refactor: drop unused glog imports after Logger migration"
```

---

## Final verification (before PR)

```bash
cd /home/full-bars/ur/urnetwork-3.23-fix
# No direct glog calls remain outside the shim:
test "$(grep -rl 'glog\.' *.go)" = "log.go" && echo "OK: only log.go uses glog"
go build ./...
go vet ./...
go test ./...
# Spot-check call counts went to zero in the 21 target files:
grep -rc 'glog\.' transfer.go ip.go transfer_encrypt.go transport.go   # all 0
```

Optional behavioral check (proves the shim default preserves glog output, and that a
`NoopLogger` silences): write a tiny throwaway test that constructs a `Client` with
`ClientSettings{Log: NewNoopLogger()}` and asserts it builds/runs — but this is not
required if `go test ./...` is green.

## Open PR (do not merge)

```bash
git push -u origin refactor/glog-to-logger
gh pr create --base master --title "refactor: route all logging through the Logger interface" --body "$(cat <<'EOF'
Replaces all 357 direct `glog.*` calls across 21 files with the `Logger` interface
(`self.log` / `self.client.log` / `DefaultLogger()`), matching the upstream
`urnetwork/connect` refactor. Embedders can now silence the library via
`NewNoopLogger()` on settings or `SetDefaultLogger`, and core files stop colliding
with upstream on every rebase.

Behavior-preserving: log strings, levels, and formatting are unchanged; only the call
target changed. `glog` now lives solely behind `glogLogger` in `log.go`, which remains
the default logger.
EOF
)"
```

> The user signs off before merge. Do not merge the PR yourself.

## Reference files (consult when a struct/signature is ambiguous)

- `/home/full-bars/ur/connect/log.go` — the shim (verbatim source for Task 1)
- `/home/full-bars/ur/connect/transfer.go` — Client hierarchy end-state
- `/home/full-bars/ur/connect/transfer_contract_manager.go` — `contractQueue` log field + `client.log` pattern
- `/home/full-bars/ur/connect/transfer_route_manager.go` — `NewRouteManagerWithLogger` entry point
- `/home/full-bars/ur/connect/transfer_encrypt.go` — `log Logger` helper params
- `/home/full-bars/ur/connect/transport.go`, `transport_pt.go`, `transport_p2p_webrtc.go`
- `/home/full-bars/ur/connect/ip.go`, `ip_remote_multi_client.go`
- `/home/full-bars/ur/connect/net.go`, `net_http.go`, `message_framer.go`, `message_pool.go`, `trace.go`

All fork struct names, settings field names, and constructor signatures were verified to
match these upstream files at plan-writing time.
