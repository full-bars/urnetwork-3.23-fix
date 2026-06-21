# Upstream PR #185 Adoption Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cherry-pick the no-conflict, high-value changes from upstream PR #185 ("experimental: performance optimizations") into the fork without touching the gVisor rewrite.

**Architecture:** Three independent changes in priority order: (1) a fast-path protobuf parser in `connect.go` that reduces allocations on the per-frame hot path, (2) a contract queue expiry mechanism in `transfer_contract_manager.go` that prevents memory growth from orphaned contracts, (3) the Logger abstraction as strategic groundwork. Each change is a standalone PR.

**Tech Stack:** Go 1.25, existing fork toolchain (`go test ./...`, `./test.sh`).

---

## Plain-English Background

### What is a "hot path" and why does allocation matter?

Your fork routes thousands of network frames per second through each proxy. A "hot path" is code that runs on every single one of those frames — even a tiny slowdown multiplies across millions of calls. Go's garbage collector (GC) has to pause briefly to reclaim memory. If a function on the hot path creates temporary objects ("allocates"), the GC has more work to do and pauses happen more often. The goal of Task 1 is to make the most-called function in the routing path create *zero* temporary objects.

### What is protobuf and why does "reflection" matter?

Protobuf is the binary format used to encode all network messages. The normal way to decode a protobuf message in Go is with `proto.Unmarshal`, which uses *reflection* — Go's ability to inspect and manipulate data structures generically at runtime. Reflection is flexible but slow: it has to look up field types, create temporary objects, and walk the message schema on every call. For something called millions of times a second, that overhead adds up.

The fast-path parser in Task 1 bypasses reflection entirely. It reads the raw bytes manually, extracts only the three fields it needs (source ID, destination ID, stream ID), and copies them directly. No reflection, no temporary objects.

### What is a "contract queue" and why does it leak?

When a proxy wants to send data, it first negotiates a "contract" with the network that reserves bandwidth. Contracts land in a queue keyed by destination. When the conversation is done, the queue is cleaned up. But sometimes a contract arrives *after* the conversation already ended (race condition, slow API, provider rotation). That contract sits in the queue forever because nobody is waiting for it — the queue is now an orphan. Over time these orphans accumulate and waste memory. Task 2 adds a background cleanup goroutine that periodically expires contracts that have been waiting too long.

---

## File Map

| File | What changes |
|---|---|
| `connect.go` | Add `parseFilteredTransferPath` (155 lines) + `FilteredTransferPath` wrapper |
| `connect_filtered_path_test.go` | New file — upstream's tests for the parser |
| `transfer.go` | Two call sites updated to use `FilteredTransferPath()` |
| `transfer_contract_manager.go` | Add `queuedContract` struct, update `contractQueue`, add expiry goroutine, new settings fields |

---

## Task 1: Fast-path protobuf parser (`connect.go`)

**What we're doing and why:** Right now, every frame that gets routed calls `proto.Unmarshal` to decode the full message just to read three UUIDs (where it came from, where it's going, which stream). We're adding a 155-line function that reads those same bytes manually — no full decode, no temporary objects. It has a safety fallback: if anything looks unexpected it falls back to the existing `proto.Unmarshal` path. The existing code keeps working unchanged; we're just adding a faster lane in front of it.

**Files:**
- Modify: `connect.go` (insert after line 215)
- Modify: `transfer.go` (lines 609–619, 1000–1016)
- Create: `connect_filtered_path_test.go`

- [ ] **Step 1: Create a branch**

```bash
git checkout -b feat/upstream-pr185-adoption
```

- [ ] **Step 2: Run the tests to establish a baseline**

```bash
go test ./... 2>&1 | tail -20
```

Expected: all tests pass. If anything fails, note it — we didn't break it.

- [ ] **Step 3: Add `parseFilteredTransferPath` and `FilteredTransferPath` to `connect.go`**

Insert the following block into `connect.go` after line 215 (after the closing brace of `ToProtobuf()`, before `// comparable`):

```go
// parseFilteredTransferPath extracts the transfer path from an encoded
// `protocol.TransferFrame` without reflection or allocation. This runs for
// every frame a client routes (see `Client.run` and `Client.Forward`),
// where the reflection-based `FilteredTransferFrame` unmarshal dominates
// the allocation profile. Unknown fields are skipped the same as the
// protobuf decoder. `ok` is false on any unexpected encoding, in which
// case the caller must fall back to the full unmarshal.
func parseFilteredTransferPath(b []byte) (path TransferPath, exists bool, ok bool) {
	i := 0
	n := len(b)

	readVarint := func(limit int) (uint64, bool) {
		var v uint64
		var shift uint
		for {
			if limit <= i || 63 < shift {
				return 0, false
			}
			c := b[i]
			i += 1
			v |= uint64(c&0x7f) << shift
			if c < 0x80 {
				return v, true
			}
			shift += 7
		}
	}

	skipField := func(wireType uint64, limit int) bool {
		switch wireType {
		case 0:
			// varint
			_, varintOk := readVarint(limit)
			return varintOk
		case 1:
			// fixed 64
			if limit < i+8 {
				return false
			}
			i += 8
			return true
		case 2:
			// length delimited
			length, lengthOk := readVarint(limit)
			if !lengthOk || uint64(limit-i) < length {
				return false
			}
			i += int(length)
			return true
		case 5:
			// fixed 32
			if limit < i+4 {
				return false
			}
			i += 4
			return true
		default:
			// group types are not used by the protocol
			return false
		}
	}

	for i < n {
		tag, tagOk := readVarint(n)
		if !tagOk {
			return TransferPath{}, false, false
		}
		fieldNumber := tag >> 3
		wireType := tag & 0x7

		// TransferFrame { TransferPath transfer_path = 1; ... }
		if fieldNumber != 1 {
			if !skipField(wireType, n) {
				return TransferPath{}, false, false
			}
			continue
		}
		if wireType != 2 {
			return TransferPath{}, false, false
		}
		length, lengthOk := readVarint(n)
		if !lengthOk || uint64(n-i) < length {
			return TransferPath{}, false, false
		}
		end := i + int(length)
		exists = true

		// TransferPath {
		//     optional bytes destination_id = 1;
		//     optional bytes source_id = 2;
		//     optional bytes stream_id = 3;
		// }
		for i < end {
			pathTag, pathTagOk := readVarint(end)
			if !pathTagOk {
				return TransferPath{}, false, false
			}
			pathFieldNumber := pathTag >> 3
			pathWireType := pathTag & 0x7

			switch pathFieldNumber {
			case 1, 2, 3:
				if pathWireType != 2 {
					return TransferPath{}, false, false
				}
				idLength, idLengthOk := readVarint(end)
				if !idLengthOk || uint64(end-i) < idLength {
					return TransferPath{}, false, false
				}
				if idLength != 16 {
					// ids are always 16 bytes
					return TransferPath{}, false, false
				}
				var id Id
				copy(id[:], b[i:i+16])
				i += 16
				switch pathFieldNumber {
				case 1:
					path.DestinationId = id
				case 2:
					path.SourceId = id
				case 3:
					path.StreamId = id
				}
			default:
				if !skipField(pathWireType, end) {
					return TransferPath{}, false, false
				}
			}
		}
	}
	return path, exists, true
}

// FilteredTransferPath parses the transfer path of an encoded
// `protocol.TransferFrame`, using the allocation-free fast path with a
// full `FilteredTransferFrame` unmarshal fallback
func FilteredTransferPath(transferFrameBytes []byte) (TransferPath, error) {
	if path, exists, ok := parseFilteredTransferPath(transferFrameBytes); ok {
		if !exists {
			return TransferPath{}, errors.New("Missing transfer path")
		}
		return path, nil
	}
	// fall back to the full unmarshal
	filteredTransferFrame := &protocol.FilteredTransferFrame{}
	if err := ProtoUnmarshal(transferFrameBytes, filteredTransferFrame); err != nil {
		return TransferPath{}, err
	}
	if filteredTransferFrame.TransferPath == nil {
		return TransferPath{}, errors.New("Missing transfer path")
	}
	return TransferPathFromProtobuf(filteredTransferFrame.TransferPath)
}
```

- [ ] **Step 4: Update first call site in `transfer.go`**

The current code at lines 609–619 manually decodes the protobuf and then converts the path. Replace it with a single call to the new function.

Find this block:
```go
	var filteredTransferFrame protocol.FilteredTransferFrame
	if err := ProtoUnmarshal(transferFrameBytes, &filteredTransferFrame); err != nil {
		// bad protobuf
		return false, err
	}

	path, err := TransferPathFromProtobuf(filteredTransferFrame.TransferPath)
	if err != nil {
		// bad protobuf
		return false, err
	}
```

Replace with:
```go
	path, err := FilteredTransferPath(transferFrameBytes)
	if err != nil {
		// bad protobuf
		return false, err
	}
```

- [ ] **Step 5: Update second call site in `transfer.go`**

The current code at lines 999–1016 does the same thing inside `Client.run`. Find this block:

```go
		// decode a minimal subset of the full message needed to make a routing decision
		filteredTransferFrame := &protocol.FilteredTransferFrame{}
		if err := ProtoUnmarshal(transferFrameBytes, filteredTransferFrame); err != nil {
			// bad protobuf (unexpected, see route note above)
			MessagePoolReturn(transferFrameBytes)
			continue
		}
		if filteredTransferFrame.TransferPath == nil {
			// bad protobuf (unexpected, see route note above)
			MessagePoolReturn(transferFrameBytes)
			continue
		}
		path, err := TransferPathFromProtobuf(filteredTransferFrame.TransferPath)
		if err != nil {
			// bad protobuf (unexpected, see route note above)
			MessagePoolReturn(transferFrameBytes)
			continue
		}
```

Replace with:
```go
		// decode a minimal subset of the full message needed to make a routing decision
		path, err := FilteredTransferPath(transferFrameBytes)
		if err != nil {
			// bad protobuf (unexpected, see route note above)
			MessagePoolReturn(transferFrameBytes)
			continue
		}
```

- [ ] **Step 6: Create the test file**

Create `connect_filtered_path_test.go` with the following content (this is upstream's test, verbatim — it tests both the fast path and the fallback):

```go
package connect

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect/protocol"
)

func TestParseFilteredTransferPath(t *testing.T) {
	sourceId := NewId()
	destinationId := NewId()
	streamId := NewId()

	marshal := func(transferFrame *protocol.TransferFrame) []byte {
		b, err := proto.Marshal(transferFrame)
		assert.Equal(t, nil, err)
		return b
	}

	// a full transfer frame with a pack payload: the parser must skip the
	// unknown (unfiltered) fields the same as the protobuf decoder
	fullFrame := func(path TransferPath) []byte {
		return marshal(&protocol.TransferFrame{
			TransferPath: path.ToProtobuf(),
			Pack: &protocol.Pack{
				MessageId:      NewId().Bytes(),
				SequenceId:     NewId().Bytes(),
				SequenceNumber: 1234567,
				Frames: []*protocol.Frame{
					{
						MessageType:  protocol.MessageType_TestSimpleMessage,
						MessageBytes: make([]byte, 1024),
					},
				},
			},
		})
	}

	paths := []TransferPath{
		NewTransferPath(sourceId, destinationId, Id{}),
		NewTransferPath(sourceId, destinationId, streamId),
		SourceId(sourceId),
		DestinationId(destinationId),
		{},
	}
	for _, expectedPath := range paths {
		b := fullFrame(expectedPath)

		path, exists, ok := parseFilteredTransferPath(b)
		assert.Equal(t, true, ok)
		assert.Equal(t, true, exists)
		assert.Equal(t, expectedPath, path)

		// the wrapper must agree with the full unmarshal path
		path, err := FilteredTransferPath(b)
		assert.Equal(t, nil, err)
		assert.Equal(t, expectedPath, path)
	}

	// ack frames have a different payload field
	ackFrame := marshal(&protocol.TransferFrame{
		TransferPath: NewTransferPath(sourceId, destinationId, Id{}).ToProtobuf(),
		Ack: &protocol.Ack{
			MessageId:  NewId().Bytes(),
			SequenceId: NewId().Bytes(),
		},
	})
	path, exists, ok := parseFilteredTransferPath(ackFrame)
	assert.Equal(t, true, ok)
	assert.Equal(t, true, exists)
	assert.Equal(t, NewTransferPath(sourceId, destinationId, Id{}), path)

	// missing transfer path
	noPath := marshal(&protocol.TransferFrame{})
	_, exists, ok = parseFilteredTransferPath(noPath)
	assert.Equal(t, true, ok)
	assert.Equal(t, false, exists)
	_, err := FilteredTransferPath(noPath)
	if err == nil {
		t.Fatal("expected an error for a missing transfer path")
	}

	// malformed inputs must not panic, and the wrapper must return an error
	// (either from the fast-path id check or the full unmarshal fallback)
	malformed := [][]byte{
		{0x0a},             // truncated length-delimited field
		{0x0a, 0xff},       // length past the end
		{0x08, 0x01},       // transfer_path with the wrong wire type
		{0xff, 0xff, 0xff}, // bad tag varint
	}
	for _, b := range malformed {
		_, _, _ = parseFilteredTransferPath(b)
		if _, err := FilteredTransferPath(b); err == nil {
			t.Fatalf("expected an error for malformed input %v", b)
		}
	}

	// an id with the wrong length falls back to the full unmarshal, which
	// surfaces the id error
	badId := marshal(&protocol.TransferFrame{
		TransferPath: &protocol.TransferPath{
			SourceId: []byte{1, 2, 3},
		},
	})
	_, _, ok = parseFilteredTransferPath(badId)
	assert.Equal(t, false, ok)
	if _, err := FilteredTransferPath(badId); err == nil {
		t.Fatal("expected an error for a bad id length")
	}
}
```

- [ ] **Step 7: Run the tests**

```bash
go test ./... -run TestParseFilteredTransferPath -v
go test ./...
```

Expected: `TestParseFilteredTransferPath` passes. All other tests still pass.

- [ ] **Step 8: Commit**

```bash
git add connect.go connect_filtered_path_test.go transfer.go
git commit -m "perf: add allocation-free protobuf fast-path parser for frame routing

Ports parseFilteredTransferPath + FilteredTransferPath from upstream PR #185.
Replaces reflection-based proto.Unmarshal at the two hottest routing call
sites in transfer.go with a zero-allocation binary parser. Falls back to
full unmarshal on any unexpected encoding."
```

---

## Task 2: Contract queue expiry (`transfer_contract_manager.go`)

**What we're doing and why:** When a contract arrives from the server, it goes into a queue keyed by destination. The sequence that requested it will pick it up from the queue. But sometimes the sequence finishes (or the provider rotates) *before* the contract arrives — and the contract sits in the queue forever because nobody comes to claim it. These orphaned contracts accumulate silently over time, wasting memory.

We're adding:
1. An `enqueueTime` timestamp on each queued contract (so we know how old it is)
2. A `Poll(minEnqueueTime)` that skips contracts older than the cutoff
3. An `Expire(minEnqueueTime)` method that removes and returns all stale contracts
4. A background goroutine (`expireQueuedContracts`) that wakes up every 60 seconds, finds contracts older than 120 seconds, and closes them with the server (releasing their escrow)
5. Two new settings: `OriginContractLinger` (reserved, not yet wired) and `ContractQueueExpireTimeout: 120s`

This also has a nice shutdown benefit: on graceful shutdown, the goroutine immediately closes all still-pending contracts so their escrow is released rather than timing out.

**Files:**
- Modify: `transfer_contract_manager.go`

- [ ] **Step 1: Add `queuedContract` struct and update `contractQueue`**

The `contractQueue` struct currently stores `map[Id]*protocol.Contract`. We need to wrap each contract with its enqueue time. Find this block (around line 1178):

```go
type contractQueue struct {
	updateMonitor *Monitor

	mutex     sync.Mutex
	openCount int
	contracts map[Id]*protocol.Contract

	// remember all added contract ids
	trackUsedContracts bool
	usedContractIds    map[Id]bool
}

func newContractQueue(trackUsedContracts bool) *contractQueue {
	return &contractQueue{
		updateMonitor:      NewMonitor(),
		openCount:          0,
		contracts:          map[Id]*protocol.Contract{},
		trackUsedContracts: trackUsedContracts,
		usedContractIds:    map[Id]bool{},
	}
}
```

Replace with:

```go
// queuedContract wraps a contract with its arrival time so stale entries
// can be expired (see ContractQueueExpireTimeout).
type queuedContract struct {
	contract    *protocol.Contract
	enqueueTime time.Time
}

type contractQueue struct {
	updateMonitor *Monitor

	mutex     sync.Mutex
	openCount int
	contracts map[Id]*queuedContract

	// remember all added contract ids
	trackUsedContracts bool
	usedContractIds    map[Id]bool
}

func newContractQueue(trackUsedContracts bool) *contractQueue {
	return &contractQueue{
		updateMonitor:      NewMonitor(),
		openCount:          0,
		contracts:          map[Id]*queuedContract{},
		trackUsedContracts: trackUsedContracts,
		usedContractIds:    map[Id]bool{},
	}
}
```

- [ ] **Step 2: Update `Poll()` to accept a minimum enqueue time and skip stale contracts**

Find the current `Poll()` method:

```go
func (self *contractQueue) Poll() *protocol.Contract {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if len(self.contracts) == 0 {
		return nil
	}

	contractIds := maps.Keys(self.contracts)
	// choose arbitrarily
	contractId := contractIds[0]
	contract := self.contracts[contractId]
	delete(self.contracts, contractId)
	return contract
}
```

Replace with:

```go
// Poll returns one queued contract, never one enqueued before
// minEnqueueTime — stale entries are removed and returned as expired for
// the caller to close. A zero minEnqueueTime expires nothing.
func (self *contractQueue) Poll(minEnqueueTime time.Time) (*protocol.Contract, []*protocol.Contract) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	expired := self.expireWithLock(minEnqueueTime)

	// choose arbitrarily
	for contractId, qc := range self.contracts {
		delete(self.contracts, contractId)
		return qc.contract, expired
	}
	return nil, expired
}

// expireWithLock removes and returns all contracts enqueued before minEnqueueTime.
// Must be called with self.mutex held.
func (self *contractQueue) expireWithLock(minEnqueueTime time.Time) []*protocol.Contract {
	if minEnqueueTime.IsZero() {
		return nil
	}
	var expired []*protocol.Contract
	for contractId, qc := range self.contracts {
		if qc.enqueueTime.Before(minEnqueueTime) {
			expired = append(expired, qc.contract)
			delete(self.contracts, contractId)
		}
	}
	return expired
}

// Expire removes and returns all contracts enqueued before minEnqueueTime.
func (self *contractQueue) Expire(minEnqueueTime time.Time) []*protocol.Contract {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.expireWithLock(minEnqueueTime)
}
```

- [ ] **Step 3: Update `Add()` to store enqueue time**

Find the `Add()` method. It currently stores `self.contracts[contractId] = contract`. Change every store of `*protocol.Contract` to store a `*queuedContract` instead.

Find:
```go
	// update contract if present
	if _, ok := self.contracts[contractId]; ok {
		glog.V(2).Infof("[contract]add update existing %s\n", contractId)
		self.contracts[contractId] = contract
		self.updateMonitor.NotifyAll()
	} else if !self.trackUsedContracts || !self.usedContractIds[contractId] {
		glog.V(2).Infof("[contract]add %s\n", contractId)
		if self.trackUsedContracts {
			self.usedContractIds[contractId] = true
		}
		self.contracts[contractId] = contract
		self.updateMonitor.NotifyAll()
```

Replace with:
```go
	// update contract if present
	if _, ok := self.contracts[contractId]; ok {
		glog.V(2).Infof("[contract]add update existing %s\n", contractId)
		self.contracts[contractId] = &queuedContract{contract: contract, enqueueTime: time.Now()}
		self.updateMonitor.NotifyAll()
	} else if !self.trackUsedContracts || !self.usedContractIds[contractId] {
		glog.V(2).Infof("[contract]add %s\n", contractId)
		if self.trackUsedContracts {
			self.usedContractIds[contractId] = true
		}
		self.contracts[contractId] = &queuedContract{contract: contract, enqueueTime: time.Now()}
		self.updateMonitor.NotifyAll()
```

- [ ] **Step 4: Update `Flush()` to unwrap `queuedContract`**

`Flush()` returns `[]*protocol.Contract`. It uses `maps.Values(self.contracts)` which now returns `[]*queuedContract`. Unwrap them.

Find:
```go
func (self *contractQueue) Flush(removeUsedContractIds bool) []*protocol.Contract {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	contracts := maps.Values(self.contracts)
	self.contracts = map[Id]*protocol.Contract{}
```

Replace with:
```go
func (self *contractQueue) Flush(removeUsedContractIds bool) []*protocol.Contract {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	contracts := make([]*protocol.Contract, 0, len(self.contracts))
	for _, qc := range self.contracts {
		contracts = append(contracts, qc.contract)
	}
	self.contracts = map[Id]*queuedContract{}
```

- [ ] **Step 5: Update `TakeContract` to pass `minEnqueueTime` to `Poll`**

Find the call to `contractQueue.Poll()` inside `TakeContract` (around line 853):

```go
		contract := contractQueue.Poll()
```

Replace with:
```go
		var minEnqueueTime time.Time
		if 0 < self.settings.ContractQueueExpireTimeout {
			minEnqueueTime = time.Now().Add(-self.settings.ContractQueueExpireTimeout)
		}
		contract, expired := contractQueue.Poll(minEnqueueTime)
		if 0 < len(expired) {
			self.closeContracts(expired)
		}
```

- [ ] **Step 6: Add the two new settings fields**

In `ContractManagerSettings` (around line 199), add after `ProvidePingTimeout`:

```go
	// server-side companion policy: allow a return (companion) contract to be
	// created for up to this long after the origin contract in the opposite
	// direction was closed, so reply traffic can resume after the request side
	// goes idle. Reserved for future server-side enforcement.
	OriginContractLinger time.Duration

	// expire queued contracts that no sequence has taken within this window.
	// Bounds destinationContracts growth from orphans. <= 0 disables expiry.
	ContractQueueExpireTimeout time.Duration
```

In `DefaultContractManagerSettingsWithBufferSize`, add after `ProvidePingTimeout: 0,`:

```go
		OriginContractLinger: 300 * time.Second,

		ContractQueueExpireTimeout: 120 * time.Second,
```

- [ ] **Step 7: Add the `expireQueuedContracts` goroutine**

Add this method to `ContractManager` (add it near the other background goroutine methods like `providePing`):

```go
// expireQueuedContracts periodically closes queued contracts that no sequence
// took within ContractQueueExpireTimeout and removes the emptied queues.
// On shutdown it immediately closes all still-pending contracts so their
// escrow is released rather than timing out server-side.
func (self *ContractManager) expireQueuedContracts() {
	timeout := self.settings.ContractQueueExpireTimeout

	finalFlush := func() {
		pending := []*protocol.Contract{}
		func() {
			self.mutex.Lock()
			defer self.mutex.Unlock()

			for contractKey, contractQueue := range self.destinationContracts {
				pending = append(pending, contractQueue.Flush(false)...)
				if contractQueue.IsDone() {
					delete(self.destinationContracts, contractKey)
				}
			}
		}()
		if 0 < len(pending) {
			glog.V(1).Infof("[contract]closing %d pending contracts on close\n", len(pending))
			self.closeContracts(pending)
		}
	}

	for {
		// when expiry is disabled the nil tick channel blocks forever and
		// the loop only exits on shutdown
		var tick <-chan time.Time
		if 0 < timeout {
			tick = time.After(timeout / 2)
		}

		select {
		case <-self.ctx.Done():
			finalFlush()
			return
		case <-self.client.Done():
			finalFlush()
			return
		case <-tick:
		}

		minEnqueueTime := time.Now().Add(-timeout)
		expired := []*protocol.Contract{}
		func() {
			self.mutex.Lock()
			defer self.mutex.Unlock()

			for contractKey, contractQueue := range self.destinationContracts {
				expired = append(expired, contractQueue.Expire(minEnqueueTime)...)
				if contractQueue.IsDone() {
					delete(self.destinationContracts, contractKey)
				}
			}
		}()
		if 0 < len(expired) {
			glog.V(1).Infof("[contract]expired %d queued contracts\n", len(expired))
			self.closeContracts(expired)
		}
	}
}
```

- [ ] **Step 8: Wire the goroutine in `NewContractManager`**

Find where `providePing` is started in `NewContractManager`:

```go
	if client.ClientId() != ControlId {
		go HandleError(contractManager.providePing, client.Cancel)
	}
```

Add the expiry goroutine directly after:

```go
	go HandleError(contractManager.expireQueuedContracts, client.Cancel)
```

- [ ] **Step 9: Run the tests**

```bash
go test ./... -v 2>&1 | grep -E "FAIL|PASS|ok|---"
```

Expected: all tests pass. The contract manager tests exercise `TakeContract` and queue operations.

- [ ] **Step 10: Commit**

```bash
git add transfer_contract_manager.go
git commit -m "feat: add contract queue expiry to bound orphan contract memory growth

Ports ContractQueueExpireTimeout mechanism from upstream PR #185.
Queued contracts older than 120s are closed and their escrow released.
On shutdown, all pending contracts are closed immediately rather than
timing out server-side. Also adds OriginContractLinger field (reserved,
not yet wired to server-side logic)."
```

---

## Task 3: Open the PR

- [ ] **Step 1: Push the branch**

```bash
git push -u origin feat/upstream-pr185-adoption
```

- [ ] **Step 2: Open the PR**

```bash
gh pr create \
  --title "perf/feat: adopt upstream PR #185 changes (fast-path parser + contract expiry)" \
  --body "$(cat <<'EOF'
## What

Adopts two changes from upstream's experimental-perf PR (#185) that are clean cherry-picks with no conflict against fork-specific patches.

## Changes

### 1. Allocation-free protobuf fast-path parser (`connect.go`, `transfer.go`)

Every routed frame was calling `proto.Unmarshal` (reflection-based) just to read three UUIDs. Replaced with a hand-rolled binary parser that reads the same bytes with zero heap allocation. Falls back to full unmarshal on any unexpected encoding — existing behavior is preserved. Upstream's test suite included.

### 2. Contract queue expiry (`transfer_contract_manager.go`)

Orphaned contracts (arrived after their owning sequence exited) accumulated indefinitely. Added:
- `queuedContract` wrapper with `enqueueTime`
- `Poll(minEnqueueTime)` skips stale contracts
- `Expire(minEnqueueTime)` bulk-removes stale contracts
- `expireQueuedContracts` background goroutine (120s expiry window, wakes every 60s)
- Graceful shutdown: closes all pending contracts immediately so escrow is released

## Not included

- Logger abstraction (strategic, separate PR)
- `addSend()` lock consolidation (requires manual reconciliation with fork's affinity logic)
- gVisor / ip.go rewrite (experimental upstream, separate tracking issue)
EOF
)"
```

---

## Future Roadmap

### Task 4: `addSend()` lock consolidation (medium effort, ~3-5h)

**What:** Merges three separate lock acquisitions per sent packet (`addSendNack`, `addSendSyn`, `addSource`) into one. Can't be directly cherry-picked — requires manual reconciliation because the fork's `addSource` body (which feeds the affinity logic) differs from upstream's.

**How to approach:**
1. Read `ip_remote_multi_client.go` lines 3029–3139 (the three `add*` methods)
2. Read upstream's `addSend()` for the lock structure
3. Write a new `addSend()` that uses upstream's single-lock structure but preserves the fork's `addSourceToEventBucketWithLock` body
4. Update `SendDetailedWithAck` to call it

### Task 5: Logger abstraction (1-2 days, strategic)

**What:** New `Logger` interface in a new `log.go` file; every settings struct gets a `Log Logger` field; all `glog.*` calls replaced with `self.log.*`. The fork keeps its rate-limit hacks but wraps them in a `Logger` implementation.

**Why defer:** High line-count change across many files the fork has already patched. No user-visible behavior change. Its value is strategic — it's the prerequisite for a future gVisor migration.

**How to approach:**
1. Copy upstream's `log.go` verbatim
2. Add `Log Logger` field to `ContractManagerSettings`, `MultiClientSettings`, `LocalUserNatSettings`, `UdpBufferSettings`, `TcpBufferSettings`
3. In each constructor, call `loggerOrDefault(settings.Log)` and store result on the struct
4. Replace `glog.*` call sites one file at a time: `transfer_contract_manager.go` first (smallest surface), then `ip_remote_multi_client.go` (largest)
5. Wrap the fork's `shouldLogOobErr` rate-limit logic into a `rateLimitedLogger` wrapper type

### Task 6: gVisor / `ip.go` rewrite (weeks of work, do not start without planning session)

**What this actually is:** A complete replacement of the fork's hand-built TCP/IP relay (`ip.go`, ~3,300 lines) with Google's gVisor netstack. Not a merge — a rewrite.

**Prerequisites before starting:**
- Task 5 (Logger abstraction) must be complete
- Full understanding of gVisor's `tcpip.Endpoint` model
- A plan for re-implementing affinity logic and multi-race in the new model
- A dedicated branch with a 4-8 week runway

**The right time:** When upstream's gVisor integration stabilizes (it's still "experimental"), or when a fork feature requires running many client instances in-process. Neither is true today.

**Tracking:** Open a GitHub issue to track this decision point. Re-evaluate each upstream release cycle.
