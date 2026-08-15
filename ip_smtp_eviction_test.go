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
