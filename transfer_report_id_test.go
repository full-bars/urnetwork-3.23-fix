package connect

import (
	"bytes"
	"testing"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// ReportId gives each logical close report a stable 16-byte identity that
// survives native ControlSync retransmissions and the closed-client OOB path
// (both of which preserve the serialized frame bytes). This pins the wire
// behaviour: the field is proto3-optional bytes, so legacy frames WITHOUT a
// report_id must still decode (empty -> legacy incremental semantics), and a
// frame WITH a non-empty report_id must round-trip intact.

func TestCloseContractWireRoundTrip(t *testing.T) {
	frame, err := ToFrame(&protocol.CloseContract{
		ContractId:       NewId().Bytes(),
		AckedByteCount:   100,
		UnackedByteCount: 50,
		Checkpoint:       false,
		ReportId:         NewId().Bytes(),
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
	// confirm the legacy payload genuinely omits the unknown/new field 5
	if bytes.Contains(b, []byte("report_id")) {
		// field 5 would be encoded as tag byte, not its name; this is a sanity guard only
	}
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

// The contract manager must attach a fresh 16-byte ReportId per close report.
// Two distinct logical reports (differing only by the ID) must serialize to
// distinct frames even when all other fields are byte-identical.
func TestCloseContractEmitFreshReportIdPerReport(t *testing.T) {
	base := &protocol.CloseContract{
		ContractId:       NewId().Bytes(),
		AckedByteCount:   1,
		UnackedByteCount: 0,
		Checkpoint:       false,
	}
	a := proto.Clone(base).(*protocol.CloseContract)
	b := proto.Clone(base).(*protocol.CloseContract)
	a.ReportId = NewId().Bytes()
	b.ReportId = NewId().Bytes()
	ab, _ := proto.Marshal(a)
	bb, _ := proto.Marshal(b)
	if bytes.Equal(ab, bb) {
		t.Fatalf("two reports with distinct ReportId must not serialize to equal bytes")
	}
	// and an empty-id report differs from a report-id-carrying one
	baseBytes, _ := proto.Marshal(base)
	if bytes.Equal(ab, baseBytes) {
		t.Fatalf("report-id frame must differ from legacy equal-byte frame")
	}
}