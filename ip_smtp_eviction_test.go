package connect

import "testing"

// The provider guard shares one flow table across all authenticated sources.
// When the table is full, eviction must spare established secure flows:
// evicting one would reset a validated TLS session, because the recreated
// state would re-inspect opaque TLS data as a fresh negotiation and reject
// it. The eviction must prefer rejected or still-negotiating flows.
func TestSmtpGuardEvictionSparesSecureFlows(t *testing.T) {
	var guard smtpEgressGuard

	// Fill the table to the cap with established secure flows.
	for index := 0; index < smtpMaxFlowCount-1; index++ {
		requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
			smtpTestPath(20000+index, smtpImplicitTlsPort, uint32(index+1)),
			smtpTestClientHello,
		))
	}
	// One still-negotiating flow: SYN only, no ClientHello yet.
	negotiating := smtpTestSyn(30000, smtpImplicitTlsPort, 1)
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(negotiating, nil))

	// The table is full; a new flow triggers eviction. The deterministic
	// non-secure pass must remove the negotiating flow and spare the secure
	// ones.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestSyn(31000, smtpImplicitTlsPort, 1), nil,
	))

	guard.stateLock.Lock()
	defer guard.stateLock.Unlock()
	if len(guard.flows) != smtpMaxFlowCount {
		t.Fatalf("flow table size = %d, want %d", len(guard.flows), smtpMaxFlowCount)
	}
	negotiatingKey, ok := smtpFlowKeyForOwnerPath(Id{}, negotiating)
	if !ok {
		t.Fatal("could not build the negotiating flow key")
	}
	if _, exists := guard.flows[negotiatingKey]; exists {
		t.Fatal("eviction did not remove the negotiating flow")
	}
	firstSecure, ok := smtpFlowKeyForOwnerPath(Id{}, smtpTestPath(20000, smtpImplicitTlsPort, 1))
	if !ok {
		t.Fatal("could not build the first secure flow key")
	}
	if _, exists := guard.flows[firstSecure]; !exists {
		t.Fatal("eviction removed an established secure flow")
	}
}

// A secure flow that has been latched rejected is expendable: it is already
// dead, so evicting it cannot reset a valid session. Eviction must prefer it
// over a still-valid secure flow.
func TestSmtpGuardEvictionPrefersRejectedSecureFlow(t *testing.T) {
	var guard smtpEgressGuard

	// A secure flow that is still valid.
	valid := smtpTestPath(20000, smtpImplicitTlsPort, 1)
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(valid, smtpTestClientHello))

	// A secure flow latched rejected by a conflicting retransmission of the
	// validated ClientHello prefix.
	rejected := smtpTestPath(20001, smtpImplicitTlsPort, 1)
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(rejected, smtpTestClientHello))
	conflict := append([]byte(nil), smtpTestClientHello...)
	conflict[5] = 0x02
	requireSmtpVerdict(t, smtpEgressReject, guard.inspect(rejected, conflict))

	// Fill the rest of the table with valid secure flows.
	for index := 2; index < smtpMaxFlowCount; index++ {
		requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
			smtpTestPath(20000+index, smtpImplicitTlsPort, uint32(index+1)),
			smtpTestClientHello,
		))
	}

	// The table is full; a new flow triggers eviction. The rejected secure
	// flow must be evicted and the valid secure flow must survive.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestSyn(31000, smtpImplicitTlsPort, 1), nil,
	))

	guard.stateLock.Lock()
	defer guard.stateLock.Unlock()
	if len(guard.flows) != smtpMaxFlowCount {
		t.Fatalf("flow table size = %d, want %d", len(guard.flows), smtpMaxFlowCount)
	}
	rejectedKey, ok := smtpFlowKeyForOwnerPath(Id{}, rejected)
	if !ok {
		t.Fatal("could not build the rejected flow key")
	}
	if _, exists := guard.flows[rejectedKey]; exists {
		t.Fatal("eviction did not remove the rejected secure flow")
	}
	validKey, ok := smtpFlowKeyForOwnerPath(Id{}, valid)
	if !ok {
		t.Fatal("could not build the valid flow key")
	}
	if _, exists := guard.flows[validKey]; !exists {
		t.Fatal("eviction removed a valid secure flow")
	}
}
