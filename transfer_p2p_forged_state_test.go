package connect

import (
	"runtime"
	"testing"
	"time"
)

const forgedStateSourceCount = 256

// forgedState is the peer state one client holds for a set of forged sources.
type forgedState struct {
	receiveSequences int
	sessions         int
	goroutines       int
	stats            ClientReceiveStatsSnapshot
}

// runForgedPlaintextPacks sends one plaintext pack from each of `count`
// distinct forged source ids on a P2P lane bound to a real peer, then a pack
// from that peer as a barrier. The receive loop handles one lane in order, so
// once the barrier is delivered every forged pack has been decided. It returns
// the receive sequences and encryption sessions held for the forged ids, and
// the goroutines started since the client was idle.
//
// With `contracted` set, each forged id has a contract with this client, which
// is the case of a peer naming real clients of this provider. A sequence for an
// id with no contract closes at its first pack, so only a contracted id shows
// the state a forged pack leaves behind.
func runForgedPlaintextPacks(t *testing.T, mode string, count int, contracted bool) forgedState {
	t.Helper()
	setSourcePolicy(t, mode, true)
	peer := NewId()
	client, route := newSourcePolicyClient(t, TransportTypeP2p, peer, count+8)
	client.ContractManager().AddNoContractPeer(peer)
	delivered := deliverContent(client)

	// Let the new client's own goroutines start before taking the baseline.
	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	forged := make(map[Id]bool, count)
	for range count {
		forgedId := NewId()
		forged[forgedId] = true
		if contracted {
			client.ContractManager().AddNoContractPeer(forgedId)
		}
		route <- forgeBarrierFrame(t, forgedId, client.ClientId(), "forged")
	}
	route <- forgeBarrierFrame(t, peer, client.ClientId(), "barrier")
	wantForged := 0
	if mode == "count" && contracted {
		wantForged = count
	}
	barrier, forgedDelivered := false, 0
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for !barrier || forgedDelivered < wantForged {
		select {
		case got := <-delivered:
			switch got {
			case "barrier":
				barrier = true
			case "forged":
				forgedDelivered++
			default:
				t.Fatalf("unexpected delivery %q", got)
			}
		case <-timer.C:
			t.Fatalf("timed out: barrier=%v, forged deliveries=%d/%d", barrier, forgedDelivered, wantForged)
		}
	}

	state := forgedState{
		goroutines: runtime.NumGoroutine() - baseline,
		stats:      client.ReceiveStats(),
	}
	client.receiveBuffer.mutex.Lock()
	for sequenceId := range client.receiveBuffer.receiveSequences {
		if forged[sequenceId.Source.SourceId] {
			state.receiveSequences += 1
		}
	}
	client.receiveBuffer.mutex.Unlock()
	client.encryptionSessionManager.stateLock.Lock()
	for key := range client.encryptionSessionManager.sessions {
		if forged[key.peerId] {
			state.sessions += 1
		}
	}
	client.encryptionSessionManager.stateLock.Unlock()
	return state
}

// A peer on a bound P2P lane names 256 other sources in plaintext packs. Drop
// discards every pack before decode, so no receive sequence, session or
// sequence goroutine exists for those ids. Count admits them: each contracted
// id holds a live receive sequence and its goroutines until the sequence idles
// out. The receive sequence count is the decisive check. The goroutine count is
// process wide, so it is checked with a margin.
func TestP2PForgedPlaintextPacksCreateNoStateInDropMode(t *testing.T) {
	const count = forgedStateSourceCount
	const goroutineMargin = 32

	for _, contracted := range []bool{true, false} {
		state := runForgedPlaintextPacks(t, "drop", count, contracted)
		if state.receiveSequences != 0 || state.sessions != 0 {
			t.Fatalf("drop mode (contracted=%v) holds state for forged sources: %+v", contracted, state)
		}
		if goroutineMargin <= state.goroutines {
			t.Fatalf("drop mode (contracted=%v) started %d goroutines for %d forged packs", contracted, state.goroutines, count)
		}
		if state.stats.SourceMismatchDroppedCount != count || state.stats.SourceMismatchDecodedCount != 0 || state.stats.SourceMismatchRejectedCount != 0 {
			t.Fatalf("drop mode (contracted=%v) stats = %+v, want %d dropped", contracted, state.stats, count)
		}
	}

	// The contrast: the same packs under count leave one receive sequence per
	// forged id, and at least one goroutine each.
	state := runForgedPlaintextPacks(t, "count", count, true)
	if state.receiveSequences != count {
		t.Fatalf("count mode holds %d receive sequences for %d forged sources, want %d: %+v", state.receiveSequences, count, count, state)
	}
	if state.goroutines < count {
		t.Fatalf("count mode started %d goroutines for %d forged sources, want at least %d", state.goroutines, count, count)
	}
	if state.sessions != 0 {
		t.Fatalf("count mode created %d encryption sessions from plaintext packs, want 0", state.sessions)
	}
	t.Logf("count mode, %d contracted forged sources: %d receive sequences, %d sessions, %d goroutines", count, state.receiveSequences, state.sessions, state.goroutines)
}
