package connect

import (
	"bytes"
	"testing"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// ReportId gives each logical close report a stable 16-byte identity that
// survives native ControlSync retransmissions, which preserve the serialized
// frame bytes. This pins the wire behaviour: the field is proto3-optional bytes,
// so legacy frames WITHOUT a report_id must still decode (empty -> legacy
// incremental semantics), and a frame WITH a non-empty report_id must round-trip
// intact.

func TestCloseContractWireRoundTrip(t *testing.T) {
	origReportId := NewId().Bytes()
	frame, err := ToFrame(&protocol.CloseContract{
		ContractId:       NewId().Bytes(),
		AckedByteCount:   100,
		UnackedByteCount: 50,
		Checkpoint:       false,
		ReportId:         origReportId,
	}, DefaultProtocolVersion)
	if err != nil {
		t.Fatalf("ToFrame: %v", err)
	}
	if frame == nil || frame.GetMessageType() != protocol.MessageType_TransferCloseContract {
		t.Fatalf("unexpected frame: %v", frame.GetMessageType())
	}

	msg, err := FromFrame(frame)
	if err != nil {
		t.Fatalf("FromFrame: %v", err)
	}
	cc, ok := msg.(*protocol.CloseContract)
	if !ok {
		t.Fatalf("decoded to %T, want *protocol.CloseContract", msg)
	}
	if len(cc.ContractId) != 16 || cc.AckedByteCount != 100 || cc.UnackedByteCount != 50 || cc.Checkpoint {
		t.Fatalf("fields mangled: %+v", cc)
	}
	if len(cc.ReportId) != 16 {
		t.Fatalf("ReportId lost in round trip: len=%d", len(cc.ReportId))
	}
	if !bytes.Equal(cc.ReportId, origReportId) {
		t.Fatalf("ReportId changed in transit: %x != %x", cc.ReportId, origReportId)
	}
}

// Legacy path: a marshaled CloseContract with no report_id field set must
// decode cleanly (report_id empty), matching pre-ReportId incremental semantics.
func TestCloseContractLegacyFrameWithoutReportIdDecodes(t *testing.T) {
	legacy := &protocol.CloseContract{
		ContractId:       NewId().Bytes(),
		AckedByteCount:   0,
		UnackedByteCount: 0,
		Checkpoint:       true,
	}
	b, err := proto.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The decode assertions below are the real check: a legacy payload carries no
	// field 5, so report_id must come back empty.
	decoded := &protocol.CloseContract{}
	if err := proto.Unmarshal(b, decoded); err != nil {
		t.Fatalf("unmarshal legacy frame: %v", err)
	}
	if len(decoded.GetReportId()) != 0 {
		t.Fatalf("legacy frame should decode to empty report_id, got %x", decoded.GetReportId())
	}
	if !decoded.GetCheckpoint() {
		t.Fatalf("checkpoint flag lost on legacy decode")
	}
}

// The manager builds every close report through newCloseContract, the production
// constructor, so this test fails if the ReportId is ever dropped there. Two
// reports of one contract must not share an identity — that is what lets the
// backend tell a ControlSync retransmission from a new report — and a checkpoint
// is a distinct operation with its own id.
func TestCloseReportCarriesAFreshIdentity(t *testing.T) {
	contractId := NewId()
	acked, unacked := ByteCount(100), ByteCount(50)

	a := newCloseContract(contractId, acked, unacked, false)
	b := newCloseContract(contractId, acked, unacked, false)
	if len(a.ReportId) != 16 {
		t.Fatalf("report id len = %d, want 16", len(a.ReportId))
	}
	if bytes.Equal(a.ReportId, b.ReportId) {
		t.Fatal("two reports of one contract must carry distinct report ids")
	}
	if !bytes.Equal(a.ContractId, contractId.Bytes()) ||
		a.AckedByteCount != uint64(acked) || a.UnackedByteCount != uint64(unacked) || a.Checkpoint {
		t.Fatalf("close contract fields mangled: %+v", a)
	}

	checkpointed := newCloseContract(contractId, acked, unacked, true)
	if !checkpointed.Checkpoint {
		t.Fatal("checkpoint flag lost")
	}
	if bytes.Equal(a.ReportId, checkpointed.ReportId) {
		t.Fatal("a checkpoint report must not reuse the plain close's identity")
	}

	// The identity the retransfer path relies on must survive serialization.
	frame, err := ToFrame(a, DefaultProtocolVersion)
	if err != nil {
		t.Fatalf("ToFrame: %v", err)
	}
	msg, err := FromFrame(frame)
	if err != nil {
		t.Fatalf("FromFrame: %v", err)
	}
	cc, ok := msg.(*protocol.CloseContract)
	if !ok {
		t.Fatalf("decoded to %T, want *protocol.CloseContract", msg)
	}
	if !bytes.Equal(cc.ReportId, a.ReportId) {
		t.Fatalf("report id changed in transit: %x != %x", cc.ReportId, a.ReportId)
	}
}
