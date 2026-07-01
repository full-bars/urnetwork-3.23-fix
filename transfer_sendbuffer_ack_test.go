package connect

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect/protocol"
)

// TestSendBufferAckTriesAllCandidateSequences guards against a regression
// where `SendBuffer.Ack` broke out of its candidate loop unconditionally
// after the first `sendSequencesByDestination` entry, instead of only after
// a successful ack. Two independent SendSequences to the same destination
// can coexist briefly (e.g. a stale sequence alongside its replacement), and
// `sendSequencesByDestination` iteration order (via `maps.Keys`) is
// randomized, so the bug only manifested on the iterations where the
// non-matching sequence happened to be visited first.
func TestSendBufferAckTriesAllCandidateSequences(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := NewClient(ctx, NewId(), NewNoContractClientOob(), DefaultClientSettingsNoNetworkEvents())
	defer client.Cancel()

	destination := DestinationId(NewId())
	sendBuffer := client.sendBuffer

	seqStale := NewSendSequence(ctx, client, sendBuffer, destination, MultiHopId{}, false, false, sequenceTlsRoleClient, false, sendBuffer.sendBufferSettings)
	seqLive := NewSendSequence(ctx, client, sendBuffer, destination, MultiHopId{}, false, false, sequenceTlsRoleClient, false, sendBuffer.sendBufferSettings)
	defer seqStale.Cancel()
	defer seqLive.Cancel()

	sendBuffer.mutex.Lock()
	sendBuffer.sendSequencesByDestination[destination] = map[*SendSequence]bool{
		seqStale: true,
		seqLive:  true,
	}
	sendBuffer.mutex.Unlock()

	ack := &protocol.Ack{
		SequenceId: seqLive.sequenceId.Bytes(),
	}

	// Map iteration order is randomized in Go, so run enough attempts that a
	// regression (break on the first, non-matching candidate) would almost
	// certainly surface as a failure. Nothing drains seqLive.acks (Run() was
	// never started), so drain it manually each iteration to avoid a false
	// failure from the buffered channel filling up.
	for i := 0; i < 50; i += 1 {
		success := sendBuffer.Ack(destination, ack, 0)
		assert.Equal(t, success, true)
		<-seqLive.acks
	}
}
