package main

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// the discovery counters are the answer to "why is this provider not getting a
// data path", so both the success side and the refusal reason must be counted
// and rendered
func TestContractDiscoveryCountsAndRenders(t *testing.T) {
	var discovery contractDiscovery
	discovery.record(&connect.ContractStatus{})
	trust := protocol.ContractError_Trust
	discovery.record(&connect.ContractStatus{Error: &trust})
	discovery.record(&connect.ContractStatus{Error: &trust})
	balance := protocol.ContractError_InsufficientBalance
	discovery.record(&connect.ContractStatus{Error: &balance})

	text := discovery.metricsText()
	for _, want := range []string{
		`urnet_contract_attempts_total{result="acquired"} 1`,
		`urnet_contract_attempts_total{result="denied"} 3`,
		`urnet_contract_denied_total{reason="Trust"} 2`,
		`urnet_contract_denied_total{reason="InsufficientBalance"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics text missing %q:\n%s", want, text)
		}
	}
}

func TestContractDiscoveryEmptyRendersZeroes(t *testing.T) {
	var discovery contractDiscovery
	text := discovery.metricsText()
	if !strings.Contains(text, `urnet_contract_attempts_total{result="acquired"} 0`) {
		t.Fatalf("empty discovery did not render a zero:\n%s", text)
	}
	if !strings.Contains(text, `urnet_contract_attempts_total{result="denied"} 0`) {
		t.Fatalf("empty discovery did not render a zero:\n%s", text)
	}
}
