package protocol

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"
)

// TestEncryptedControlGetEpochIdNilSafety verifies the generated getter for
// the new epoch_id field is nil-safe, both for a nil receiver and a
// zero-value message.
func TestEncryptedControlGetEpochIdNilSafety(t *testing.T) {
	var nilControl *EncryptedControl
	if got := nilControl.GetEpochId(); got != nil {
		t.Fatalf("expected GetEpochId on a nil receiver to return nil, got %v", got)
	}

	empty := &EncryptedControl{}
	if got := empty.GetEpochId(); got != nil {
		t.Fatalf("expected GetEpochId on a zero-value message to return nil, got %v", got)
	}
}

// TestEncryptedControlGetEpochIdReturnsValue verifies the getter surfaces a
// set epoch_id unchanged.
func TestEncryptedControlGetEpochIdReturnsValue(t *testing.T) {
	id := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	ec := &EncryptedControl{EpochId: id}
	if got := ec.GetEpochId(); !bytes.Equal(got, id) {
		t.Fatalf("GetEpochId returned %v, want %v", got, id)
	}
}

// TestEncryptedControlEpochIdRoundTrip verifies a set epoch_id survives a
// marshal/unmarshal cycle byte for byte, alongside the pre-existing fields on
// the message.
func TestEncryptedControlEpochIdRoundTrip(t *testing.T) {
	id := make([]byte, 16)
	for i := range id {
		id[i] = byte(i + 1)
	}
	want := &EncryptedControl{
		ControlType: EncryptedControlType_EncryptedControlHandshake,
		Payload:     []byte("handshake payload"),
		SessionRole: SequenceRole_SequenceRoleClient,
		Companion:   true,
		EpochId:     id,
	}

	b, err := proto.Marshal(want)
	if err != nil {
		t.Fatalf("proto.Marshal: %s", err)
	}

	got := &EncryptedControl{}
	if err := proto.Unmarshal(b, got); err != nil {
		t.Fatalf("proto.Unmarshal: %s", err)
	}

	if !proto.Equal(want, got) {
		t.Fatalf("round-tripped message differs: got %+v, want %+v", got, want)
	}
	if !bytes.Equal(got.GetEpochId(), id) {
		t.Fatalf("epoch_id round-tripped incorrectly: got %v, want %v", got.GetEpochId(), id)
	}
}

// TestEncryptedControlEpochIdUnsetRoundTripsToNil verifies that a control
// with no epoch_id set at all (the legacy-peer wire shape) round-trips with a
// nil epoch_id, so callers like DeliverEncryptedControl can tell "no
// generation on the wire" apart from "explicit empty generation".
func TestEncryptedControlEpochIdUnsetRoundTripsToNil(t *testing.T) {
	want := &EncryptedControl{
		ControlType: EncryptedControlType_EncryptedControlHandshake,
		Payload:     []byte("legacy payload"),
	}

	b, err := proto.Marshal(want)
	if err != nil {
		t.Fatalf("proto.Marshal: %s", err)
	}

	got := &EncryptedControl{}
	if err := proto.Unmarshal(b, got); err != nil {
		t.Fatalf("proto.Unmarshal: %s", err)
	}

	if got.GetEpochId() != nil {
		t.Fatalf("expected an omitted epoch_id to round-trip to nil, got %v", got.GetEpochId())
	}
}

// TestEncryptedControlEpochIdPresentEmptyDiffersFromUnset verifies the
// optional field's presence tracking: an explicitly-set, non-nil empty
// epoch_id is wire-distinguishable from an entirely unset one, and round
// trips as present (non-nil).
func TestEncryptedControlEpochIdPresentEmptyDiffersFromUnset(t *testing.T) {
	present := &EncryptedControl{EpochId: []byte{}}
	presentBytes, err := proto.Marshal(present)
	if err != nil {
		t.Fatalf("proto.Marshal(present): %s", err)
	}

	unset := &EncryptedControl{}
	unsetBytes, err := proto.Marshal(unset)
	if err != nil {
		t.Fatalf("proto.Marshal(unset): %s", err)
	}

	if bytes.Equal(presentBytes, unsetBytes) {
		t.Fatal("expected an explicitly-present empty epoch_id to serialize differently from an unset one")
	}

	got := &EncryptedControl{}
	if err := proto.Unmarshal(presentBytes, got); err != nil {
		t.Fatalf("proto.Unmarshal: %s", err)
	}
	if got.GetEpochId() == nil {
		t.Fatal("expected the explicitly-present empty epoch_id to round-trip as non-nil")
	}
	if len(got.GetEpochId()) != 0 {
		t.Fatalf("expected an empty epoch_id, got %d bytes", len(got.GetEpochId()))
	}
}

// TestEncryptedControlEpochIdIndependentOfCompanion is a small regression
// check that the new field is additive: toggling companion/control-type does
// not disturb an independently-set epoch_id, and vice versa.
func TestEncryptedControlEpochIdIndependentOfCompanion(t *testing.T) {
	id := bytes.Repeat([]byte{0xAB}, 16)
	ec := &EncryptedControl{
		ControlType: EncryptedControlType_EncryptedControlIdentityProof,
		Companion:   false,
		EpochId:     id,
	}

	b, err := proto.Marshal(ec)
	if err != nil {
		t.Fatalf("proto.Marshal: %s", err)
	}
	got := &EncryptedControl{}
	if err := proto.Unmarshal(b, got); err != nil {
		t.Fatalf("proto.Unmarshal: %s", err)
	}
	if got.GetCompanion() != false {
		t.Fatal("expected companion to remain false")
	}
	if got.GetControlType() != EncryptedControlType_EncryptedControlIdentityProof {
		t.Fatalf("expected control type to round-trip, got %v", got.GetControlType())
	}
	if !bytes.Equal(got.GetEpochId(), id) {
		t.Fatalf("expected epoch_id to round-trip alongside other fields, got %v", got.GetEpochId())
	}
}