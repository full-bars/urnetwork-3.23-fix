package connect

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect/protocol"
)

// Regression tests for the three contract-stats bugs found in the 2026-07-13
// audit of PR #264 (transfer_contract_manager.go / transfer.go /
// transfer_contract_stats.go), verifying the fixes landed in commit 7440e8b:
//   - checkpoint-path leak of `contractStatsEntries` (closeContractStats was
//     skipped whenever a contract was checkpointed rather than closed)
//   - receive-side stats updated one item early, from `updateContract`'s
//     reservation step instead of `receiveHead`'s ack step, permanently
//     losing the final processed item's bytes on close
//   - `runContractStats` hot-spinning at 100% CPU when `ContractStatsEpoch`
//     is left at its zero value

// TestContractStatsEpochDefaultsWhenInvalid pins finding 3.3: a
// ContractManagerSettings with an unset/invalid ContractStatsEpoch must not
// leave `runContractStats` ticking on `time.After(0)`.
func TestContractStatsEpochDefaultsWhenInvalid(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := NewClient(ctx, NewId(), NewNoContractClientOob(), DefaultClientSettings())
	defer client.Cancel()

	settings := DefaultContractManagerSettings()
	settings.ContractStatsEpoch = 0
	contractManager := NewContractManager(ctx, client, settings)

	assert.Equal(t, true, 0 < contractManager.settings.ContractStatsEpoch)

	// Confirm the worker actually respects a sane interval rather than
	// spinning: with the epoch normalized, a callback should not fire faster
	// than the (small but nonzero) floor the constructor applies.
	var fireCount int
	var mu sync.Mutex
	remove := contractManager.AddContractStatsCallback(func(events []*ContractStatsEvent) {
		mu.Lock()
		fireCount += 1
		mu.Unlock()
	})
	defer remove()

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	// A hot-spin (time.After(0)) would fire thousands of times in 200ms;
	// a normalized multi-second-scale epoch fires at most once or twice.
	assert.Equal(t, true, fireCount < 50)
}

// TestContractStatsClosedOnCheckpoint pins finding 3.1: checkpointing a
// contract (the path taken when a sequence terminates but the wire contract
// stays open server-side) must still release the client-side stats entry,
// not just a hard CloseContract.
func TestContractStatsClosedOnCheckpoint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientSettings := DefaultClientSettings()
	clientSettings.ContractManagerSettings.ContractStatsEpoch = 20 * time.Millisecond
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	defer client.Cancel()
	contractManager := client.ContractManager()

	contractId := NewId()
	path := TransferPath{DestinationId: NewId()}

	t.Run("worker never started: checkpoint deletes the entry immediately", func(t *testing.T) {
		entry := contractManager.registerContractStats(contractId, true, false, path, Mib(2))
		assert.Equal(t, false, entry.closed.Load())

		contractManager.CheckpointContract(contractId, Kib(10), 0)

		contractManager.contractStatsLock.Lock()
		_, stillPresent := contractManager.contractStatsEntries[contractStatsKey{contractId: contractId, receive: true}]
		contractManager.contractStatsLock.Unlock()

		// Before the fix, `CloseContractWithCheckpoint` skipped
		// `closeContractStats` entirely on the checkpoint path, so this entry
		// would remain in the map forever (the leak).
		assert.Equal(t, false, stillPresent)
	})

	t.Run("worker started: checkpoint marks the entry closed for the epoch worker to reap", func(t *testing.T) {
		contractId2 := NewId()
		entry := contractManager.registerContractStats(contractId2, true, false, path, Mib(2))

		var events []*ContractStatsEvent
		var mu sync.Mutex
		remove := contractManager.AddContractStatsCallback(func(e []*ContractStatsEvent) {
			mu.Lock()
			events = append(events, e...)
			mu.Unlock()
		})
		defer remove()

		entry.updateUsedByteCount(Kib(5))
		contractManager.CheckpointContract(contractId2, Kib(5), 0)

		deadline := time.After(5 * time.Second)
		for {
			mu.Lock()
			found := false
			for _, e := range events {
				if e.ContractId == contractId2 && !e.Open {
					found = true
				}
			}
			n := len(events)
			mu.Unlock()
			if found {
				break
			}
			select {
			case <-deadline:
				t.Fatalf("no closed event observed for checkpointed contract after 5s (saw %d events)", n)
			case <-time.After(10 * time.Millisecond):
			}
		}

		contractManager.contractStatsLock.Lock()
		_, stillPresent := contractManager.contractStatsEntries[contractStatsKey{contractId: contractId2, receive: true}]
		contractManager.contractStatsLock.Unlock()
		assert.Equal(t, false, stillPresent)
	})
}

// TestContractStatsReceiveSideNotLagging pins finding 3.2 end-to-end: the
// final contract-stats event for a receive-side contract must account for
// every acked byte, including the last message processed before the sequence
// closes. Before the fix, `updateContractStats` was called from
// `updateContract` (the reservation step, before `ackedByteCount` is
// incremented) instead of `receiveHead` (the ack step), so the reported
// UsedByteCount always lagged by one message and the last message's bytes
// were lost entirely once the sequence closed.
func TestContractStatsReceiveSideNotLagging(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	aClientId := NewId()
	bClientId := NewId()

	aSend := make(chan []byte)
	bSend := make(chan []byte)

	_, bReceive := newConditioner(ctx, aSend)
	_, aReceive := newConditioner(ctx, bSend)

	aSendTransport := NewSendGatewayTransport()
	aReceiveTransport := NewReceiveGatewayTransport()
	bSendTransport := NewSendGatewayTransport()
	bReceiveTransport := NewReceiveGatewayTransport()

	provideModes := map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true}

	clientSettingsA := DefaultClientSettings()
	clientSettingsA.SendBufferSettings.SequenceBufferSize = 0
	clientSettingsA.SendBufferSettings.AckBufferSize = 0
	clientSettingsA.SendBufferSettings.AckTimeout = 60 * time.Second
	clientSettingsA.SendBufferSettings.IdleTimeout = 60 * time.Second
	clientSettingsA.ReceiveBufferSettings.SequenceBufferSize = 0
	clientSettingsA.ReceiveBufferSettings.GapTimeout = 60 * time.Second
	clientSettingsA.ReceiveBufferSettings.IdleTimeout = 60 * time.Second
	clientSettingsA.ForwardBufferSettings.SequenceBufferSize = 0
	clientSettingsA.ForwardBufferSettings.IdleTimeout = 60 * time.Second
	a := NewClient(ctx, aClientId, NewNoContractClientOob(), clientSettingsA)
	defer a.Cancel()
	a.RouteManager().UpdateTransport(aSendTransport, []Route{aSend})
	a.RouteManager().UpdateTransport(aReceiveTransport, []Route{aReceive})
	a.ContractManager().SetProvideModes(provideModes)

	clientSettingsB := DefaultClientSettings()
	clientSettingsB.SendBufferSettings.SequenceBufferSize = 0
	clientSettingsB.SendBufferSettings.AckBufferSize = 0
	clientSettingsB.SendBufferSettings.AckTimeout = 60 * time.Second
	clientSettingsB.SendBufferSettings.IdleTimeout = 60 * time.Second
	clientSettingsB.ReceiveBufferSettings.SequenceBufferSize = 0
	clientSettingsB.ReceiveBufferSettings.GapTimeout = 60 * time.Second
	clientSettingsB.ReceiveBufferSettings.IdleTimeout = 60 * time.Second
	clientSettingsB.ForwardBufferSettings.SequenceBufferSize = 0
	clientSettingsB.ForwardBufferSettings.IdleTimeout = 60 * time.Second
	// Short epoch so the closing event flushes quickly once the sequence
	// checkpoints.
	clientSettingsB.ContractManagerSettings.ContractStatsEpoch = 20 * time.Millisecond
	b := NewClient(ctx, bClientId, NewNoContractClientOob(), clientSettingsB)
	defer b.Cancel()
	b.RouteManager().UpdateTransport(bSendTransport, []Route{bSend})
	b.RouteManager().UpdateTransport(bReceiveTransport, []Route{bReceive})
	b.ContractManager().SetProvideModes(provideModes)

	var eventsMu sync.Mutex
	var lastClosedUsedByteCount ByteCount
	var sawClosed bool
	removeCallback := b.ContractManager().AddContractStatsCallback(func(events []*ContractStatsEvent) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		for _, e := range events {
			if e.Receive && !e.Open {
				sawClosed = true
				lastClosedUsedByteCount = e.UsedByteCount
			}
		}
	})
	defer removeCallback()

	// Grant a single receive-side contract, generously sized so all messages
	// fit in it (no rollover — this test isolates the lagging-stats bug, not
	// the cross-contract rollover case covered by 0823eca).
	err := a.ContractManager().HandleControlFrame(
		ContractKey{Destination: DestinationId(bClientId)},
		requireContractResult(
			protocol.ProvideMode_Network,
			b.ContractManager().RequireProvideSecretKey(protocol.ProvideMode_Network),
			aClientId,
			bClientId,
		),
	)
	assert.Equal(t, err, nil)

	n := 20
	acks := make(chan error, n)
	receives := make(chan *protocol.SimpleMessage, n)
	b.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, peer Peer) {
		for _, frame := range frames {
			m, err := FromFrame(frame)
			if err != nil {
				continue
			}
			if sm, ok := m.(*protocol.SimpleMessage); ok {
				receives <- sm
			}
		}
	})

	for i := 0; i < n; i += 1 {
		message := &protocol.SimpleMessage{Content: fmt.Sprintf("msg-%d", i)}
		frame, err := ToFrame(message, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		success := a.Send(frame, DestinationId(bClientId), func(err error) {
			acks <- err
		})
		assert.Equal(t, success, true)
	}

	ackCount, receiveCount := 0, 0
	deadline := time.After(30 * time.Second)
	for ackCount < n || receiveCount < n {
		select {
		case err := <-acks:
			assert.Equal(t, err, nil)
			ackCount += 1
		case <-receives:
			receiveCount += 1
		case <-deadline:
			t.Fatalf("timed out waiting for all messages (acked=%d received=%d of %d)", ackCount, receiveCount, n)
		}
	}

	// All n messages are acked and received; ground truth for "bytes actually
	// processed by the receive sequence" is `b`'s own contract accounting,
	// captured by closing the receive sequence (checkpoint) and reading the
	// accumulated ReceiveContractCloseByteCount off LocalStats. This is
	// exactly the `receiveContract.ackedByteCount` value at close time,
	// independent of the stats-event machinery under test.
	b.Cancel()

	deadline2 := time.After(5 * time.Second)
	for {
		groundTruth := b.ContractManager().LocalStats().ReceiveContractCloseByteCount
		eventsMu.Lock()
		closed, used := sawClosed, lastClosedUsedByteCount
		eventsMu.Unlock()
		if closed && 0 < groundTruth {
			// Before the fix this would fail: the closing stats event's
			// UsedByteCount was captured one ack behind, so it never
			// included the final message's bytes and would be strictly
			// less than groundTruth.
			assert.Equal(t, groundTruth, used)
			return
		}
		select {
		case <-deadline2:
			t.Fatalf("no closed contract-stats event observed after close (sawClosed=%t)", closed)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
