package connect

import "testing"

// Once a flow is secure, later TLS records are opaque and must pass at any
// forward sequence offset. A long-lived 465/587 connection advances past
// 2^31 bytes of sequence space; the signed relative-offset check in the
// secure phase must not misread that as a backward (negative) segment.
func TestSmtpSecureFlowAdvancesBeyondHalfSequenceSpace(t *testing.T) {
	var guard smtpEgressGuard
	const sourcePort = 41004
	const sequence = uint32(3500)

	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, sequence), smtpTestClientHello,
	))
	// Opaque TLS data just past the validated prefix still passes.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, sequence+uint32(len(smtpTestClientHello))),
		[]byte{0x17, 0x03, 0x03},
	))
	// The same flow, advanced by more than half of the TCP sequence space
	// (2^31 bytes), must still be accepted as opaque TLS data. The signed
	// arithmetic would read this offset as negative and reject the flow.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, sequence+(uint32(1)<<31)),
		[]byte{0x17, 0x03, 0x03},
	))
}
