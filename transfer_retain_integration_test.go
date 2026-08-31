package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestBackstopDropsRetainedItem verifies the backstop ceiling end-to-end:
// a retained item past its backstopDeadline is force-dropped through the
// actual send loop.
func TestBackstopDropsRetainedItem(t *testing.T) {
	var forceAck atomic.Bool
	clientSettings := DefaultClientSettings()
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	clientSettings.SendBufferSettings.MinResendInterval = 5 * time.Millisecond
	clientSettings.SendBufferSettings.MaxResendInterval = 10 * time.Millisecond
	clientSettings.SendBufferSettings.AckTimeout = 10 * time.Millisecond
	clientSettings.SendBufferSettings.IdleTimeout = time.Hour
	clientSettings.SendBufferSettings.forceAckTimeoutForTest = func(sendSequenceId) bool {
		return forceAck.Load()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)

	sendBuffer := NewSendBuffer(ctx, client, clientSettings.SendBufferSettings)
	destination := DestinationId(NewId())
	seq := NewSendSequence(
		ctx,
		client,
		sendBuffer,
		destination,
		MultiHopId{},
		false,
		true,
		sequenceTlsRoleClient,
		false,
		clientSettings.SendBufferSettings,
	)

	go HandleError(func() { seq.Run() })

	dropped := make(chan error, 1)
	item := &sendItem{
		transferItem: transferItem{
			messageId:        NewId(),
			sequenceNumber:   1,
			messageByteCount: ByteCount(len("test")),
		},
		sendTime:              time.Now().Add(-time.Minute),
		resendTime:            time.Now().Add(-time.Second),
		sendCount:             1,
		head:                  true,
		transferFrameBytes:    []byte("test"),
		ackCallback:           func(err error) { dropped <- err },
		retainAfterAckTimeout: true,
		backstopDeadline:      time.Now().Add(-time.Second),
	}

	seq.resendQueue.Add(item)

	forceAck.Store(true)

	select {
	case err := <-dropped:
		if err == nil {
			t.Fatal("backstop dropped item with nil error")
		}
		t.Logf("backstop dropped item: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("backstop did not drop item within timeout")
	}
}
