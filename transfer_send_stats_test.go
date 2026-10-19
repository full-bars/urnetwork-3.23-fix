package connect

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestTransferSendStats_SplitsFirstSendsFromResends(t *testing.T) {
	before := TransferSendStatsNow()

	recordTransferFirstSend(1000)
	recordTransferFirstSend(500)
	recordTransferResend(1000, 2)
	recordTransferResend(1000, 5)
	recordTransferResend(1000, 16)

	got := TransferSendStatsNow()
	if d := got.FirstFrames - before.FirstFrames; d != 2 {
		t.Fatalf("first frames +%d, want 2", d)
	}
	if d := got.FirstBytes - before.FirstBytes; d != 1500 {
		t.Fatalf("first bytes +%d, want 1500", d)
	}
	if d := got.ResendFrames - before.ResendFrames; d != 3 {
		t.Fatalf("resend frames +%d, want 3", d)
	}
	if d := got.ResendBytes - before.ResendBytes; d != 3000 {
		t.Fatalf("resend bytes +%d, want 3000", d)
	}
	// the depth buckets say how many times an item had already been sent
	if got.ResendDepth[0]-before.ResendDepth[0] != 1 { // 2nd send
		t.Fatalf("depth bucket 2 = %+v", got.ResendDepth)
	}
	if got.ResendDepth[2]-before.ResendDepth[2] != 1 { // 5th send (5-8)
		t.Fatalf("depth bucket 5-8 = %+v", got.ResendDepth)
	}
	if got.ResendDepth[3]-before.ResendDepth[3] != 1 { // 16th send (9+)
		t.Fatalf("depth bucket 9+ = %+v", got.ResendDepth)
	}
}

// scriptedWriteWriter stands in for the contract multi-route writer with a
// fixed outcome. MultiRouteSelector's documented contract is that a failed
// write has already returned the frame to the pool, so the fake does the same.
type scriptedWriteWriter struct {
	writeErr error
	calls    int
}

func (self *scriptedWriteWriter) Write(
	ctx context.Context,
	transferFrameBytes []byte,
	timeout time.Duration,
) error {
	self.calls += 1
	MessagePoolReturn(transferFrameBytes)
	return self.writeErr
}

func (self *scriptedWriteWriter) WriteDetailed(
	ctx context.Context,
	transferFrameBytes []byte,
	timeout time.Duration,
) (bool, error) {
	err := self.Write(ctx, transferFrameBytes, timeout)
	return err == nil, err
}

func (self *scriptedWriteWriter) GetActiveRoutes() []Route   { return nil }
func (self *scriptedWriteWriter) GetInactiveRoutes() []Route { return nil }

// newFirstSendAccountingSequence builds a live sequence whose only writer is
// the given fake, so one send exercises sendWithSetContract's write path
// without a route or a transport.
func newFirstSendAccountingSequence(t *testing.T, writer MultiRouteWriter) *SendSequence {
	t.Helper()
	clientSettings := DefaultClientSettings()
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	sendBuffer := NewSendBuffer(ctx, client, clientSettings.SendBufferSettings)
	sequence := NewSendSequence(
		ctx,
		client,
		sendBuffer,
		DestinationId(NewId()),
		MultiHopId{},
		false,
		false,
		sequenceTlsRoleClient,
		false,
		clientSettings.SendBufferSettings,
	)
	t.Cleanup(sequence.cancel)
	sequence.contractMultiRouteWriter = writer
	sequence.contractMultiRouteWriterDestination = sequence.destination
	return sequence
}

func sendOneTestFrame(sequence *SendSequence) {
	sequence.send(
		&protocol.Frame{
			MessageType:  protocol.MessageType_TransferPack,
			MessageBytes: MessagePoolGet(64),
		},
		nil,
		true,
		false,
		false,
	)
}

// A first write that fails never reaches the wire, so it is not a first send.
// Resend accounting already follows the same rule.
func TestTransferSendStats_FailedFirstWriteIsNotAFirstSend(t *testing.T) {
	writer := &scriptedWriteWriter{writeErr: errors.New("write failed")}
	sequence := newFirstSendAccountingSequence(t, writer)

	before := TransferSendStatsNow()
	sendOneTestFrame(sequence)
	after := TransferSendStatsNow()

	if writer.calls != 1 {
		t.Fatalf("write calls = %d, want 1", writer.calls)
	}
	if got := after.FirstFrames - before.FirstFrames; got != 0 {
		t.Fatalf("a failed first write counted %d first frames, want 0", got)
	}
	if got := after.FirstBytes - before.FirstBytes; got != 0 {
		t.Fatalf("a failed first write counted %d first-send bytes, want 0", got)
	}
	// the failed frame stays queued for resend, which is why its accounting
	// belongs to the resend path, not to the first send
	if len(sequence.sendItems) != 1 {
		t.Fatalf("send items after a failed first write = %d, want 1", len(sequence.sendItems))
	}
}

// The control: a first write that reaches the writer is counted, so the test
// above is failing on the write outcome and not on a dead accounting path.
func TestTransferSendStats_SuccessfulFirstWriteIsAFirstSend(t *testing.T) {
	writer := &scriptedWriteWriter{}
	sequence := newFirstSendAccountingSequence(t, writer)

	before := TransferSendStatsNow()
	sendOneTestFrame(sequence)
	after := TransferSendStatsNow()

	if writer.calls != 1 {
		t.Fatalf("write calls = %d, want 1", writer.calls)
	}
	if got := after.FirstFrames - before.FirstFrames; got != 1 {
		t.Fatalf("a successful first write counted %d first frames, want 1", got)
	}
	if got := after.FirstBytes - before.FirstBytes; got <= 0 {
		t.Fatalf("a successful first write counted %d first-send bytes, want > 0", got)
	}
}
