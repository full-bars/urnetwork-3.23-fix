package connect

import (
	"net/http"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// The platform compares these values literally. The tests that use the
// constants cannot notice a wrong value, so this pins the server's contract:
// header X-UR-Provide-Intent == "1", close code and WebSocket/QUIC error code
// 4001, the 5-byte close control [3,0,0,0,1], Auth.provide_intent == field 7.
func TestProvideIntentWireLiterals(t *testing.T) {
	if HeaderProvideIntent != "X-UR-Provide-Intent" {
		t.Fatalf("header name = %q", HeaderProvideIntent)
	}
	if ProvideIntentDeclared != "1" {
		t.Fatalf("declared value = %q", ProvideIntentDeclared)
	}
	if ClientLimitCloseCode != 4001 {
		t.Fatalf("close code = %d", ClientLimitCloseCode)
	}
	if TransportControlClose != 3 || TransportCloseReasonClientLimitExceeded != 1 {
		t.Fatalf("close control = %d reason = %d", TransportControlClose, TransportCloseReasonClientLimitExceeded)
	}
	reason, ok := transportCloseReason([]byte{3, 0, 0, 0, 1})
	if !ok || reason != TransportCloseReasonClientLimitExceeded {
		t.Fatalf("[3,0,0,0,1] read as reason=%d ok=%v", reason, ok)
	}
	if ClientLimitBackoffTimeout.Minutes() != 15 {
		t.Fatalf("hold = %v", ClientLimitBackoffTimeout)
	}
}

func TestProvideIntentHeaderOnlyWhenDeclared(t *testing.T) {
	declared := http.Header{}
	applyProvideIntentHeader(declared, ClientAuth{ProvideIntent: true})
	if got := declared.Get("X-UR-Provide-Intent"); got != "1" {
		t.Fatalf("declared header = %q", got)
	}
	undeclared := http.Header{}
	applyProvideIntentHeader(undeclared, ClientAuth{})
	if _, present := undeclared["X-Ur-Provide-Intent"]; present {
		t.Fatal("an undeclared client must not send the header")
	}
}

func TestAuthProvideIntentIsProtoField7(t *testing.T) {
	fields := (&protocol.Auth{}).ProtoReflect().Descriptor().Fields()
	field := fields.ByName("provide_intent")
	if field == nil || field.Number() != 7 || field.Kind().String() != "bool" {
		t.Fatalf("Auth.provide_intent = %v", field)
	}
}
