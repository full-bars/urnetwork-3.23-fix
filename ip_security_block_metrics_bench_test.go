package connect

import (
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// The Inspect hot path with the block metrics: the allow path (the common
// case, one flow, steady state) and the static-drop path. The addition must
// not allocate and must not add measurable cost to the decision.

func BenchmarkSecurityEgressInspectAllow(b *testing.B) {
	policy := newEgressPolicyForTest()
	path := metricFlowPath(IpProtocolTcp, 40100, 443, false)
	payload := tlsClientHello()
	// one warm-up call so the flow is classified and the steady state runs
	policy.Inspect(protocol.ProvideMode_Public, path, payload)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i += 1 {
		policy.Inspect(protocol.ProvideMode_Public, path, payload)
	}
}

func BenchmarkSecurityEgressInspectBlockedPort(b *testing.B) {
	policy := newEgressPolicyForTest()
	path := metricFlowPath(IpProtocolTcp, 40101, 6881, false)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i += 1 {
		policy.Inspect(protocol.ProvideMode_Public, path, nil)
	}
}
