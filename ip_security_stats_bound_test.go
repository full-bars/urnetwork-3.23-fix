package connect

import (
	"net"
	"testing"
)

// Regression guard for the destination-cardinality bound: without it,
// SecurityPolicyStatsCollector.resultDestinationCounts grows without limit
// as a long-running provider relays traffic to arbitrarily many distinct
// destinations, since every unique (protocol, ip, port) tuple gets its own
// map entry forever.
func TestSecurityPolicyStatsCollectorBoundsDestinationCount(t *testing.T) {
	collector := DefaultSecurityPolicyStatsCollector()
	collector.includeIp = true

	// Feed more distinct destinations than the cap.
	for i := 0; i < securityPolicyStatsMaxDestinationsPerResult+50; i++ {
		ipPath := &IpPath{
			Version:         4,
			Protocol:        IpProtocolTcp,
			SourceIp:        net.IPv4(10, 0, 0, 1),
			SourcePort:      1234,
			DestinationIp:   net.IPv4(byte(i>>24), byte(i>>16), byte(i>>8), byte(i)),
			DestinationPort: 443,
		}
		collector.AddDestination(ipPath, SecurityPolicyResultAllow, 1)
	}

	stats := collector.Stats(false)
	destinationCounts, ok := stats[SecurityPolicyResultAllow]
	if !ok {
		t.Fatalf("expected SecurityPolicyResultAllow to have recorded destinations")
	}

	// The bound is exact, not just an upper limit: the (securityPolicyStats-
	// MaxDestinationsPerResult - 1)th unique destination fills the last real
	// slot, and every unique destination after that — 50 extra plus the one
	// that would have been the 1024th real slot, 51 in total — collapses
	// into the single overflow bucket. Map size is therefore exactly the cap.
	if len(destinationCounts) != securityPolicyStatsMaxDestinationsPerResult {
		t.Fatalf(
			"expected exactly %d distinct destinations, got %d",
			securityPolicyStatsMaxDestinationsPerResult,
			len(destinationCounts),
		)
	}

	overflowCount, ok := destinationCounts[securityPolicyStatsOverflowDestination]
	if !ok {
		t.Fatalf("expected an overflow bucket once the cap was exceeded")
	}
	const expectedOverflowCount = 51
	if overflowCount != expectedOverflowCount {
		t.Fatalf("expected overflow bucket count %d, got %d", expectedOverflowCount, overflowCount)
	}
}

// Two distinct unrecognized result values must not bypass the bound by each
// opening their own uncapped bucket keyed by an arbitrary integer — both
// have to remap into, and accumulate together under, the single unknown
// bucket.
func TestSecurityPolicyStatsCollectorUnknownResultSharesOneBucket(t *testing.T) {
	collector := DefaultSecurityPolicyStatsCollector()

	// Neither 99 nor 100 is one of the three declared results.
	unsupportedResults := []SecurityPolicyResult{99, 100}
	for i := 0; i < 10; i++ {
		ipPath := &IpPath{
			Version:         4,
			Protocol:        IpProtocolTcp,
			SourceIp:        net.IPv4(10, 0, 0, 1),
			SourcePort:      1234,
			DestinationIp:   net.IPv4(1, 1, 1, byte(i)),
			DestinationPort: 443 + i,
		}
		collector.AddDestination(ipPath, unsupportedResults[i%len(unsupportedResults)], 1)
	}

	stats := collector.Stats(false)
	for _, result := range unsupportedResults {
		if _, ok := stats[result]; ok {
			t.Fatalf("expected result(%d) to be remapped, not stored under its own key", result)
		}
	}

	unknownCounts, ok := stats[securityPolicyStatsUnknownResult]
	if !ok {
		t.Fatalf("expected all unrecognized results to share securityPolicyStatsUnknownResult")
	}
	var total uint64
	for _, count := range unknownCounts {
		total += count
	}
	if total != 10 {
		t.Fatalf("expected both unsupported results' counts to accumulate together, got total %d", total)
	}
}

// A zero count must be a no-op: it should not create a destination entry at
// all, matching the original (pre-bound) behavior.
func TestSecurityPolicyStatsCollectorZeroCountIsNoop(t *testing.T) {
	collector := DefaultSecurityPolicyStatsCollector()

	ipPath := &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.IPv4(10, 0, 0, 1),
		SourcePort:      1234,
		DestinationIp:   net.IPv4(1, 1, 1, 1),
		DestinationPort: 443,
	}
	collector.AddDestination(ipPath, SecurityPolicyResultAllow, 0)

	stats := collector.Stats(false)
	if _, ok := stats[SecurityPolicyResultAllow]; ok {
		t.Fatalf("expected zero count to record nothing")
	}
}

func TestSecurityDestinationOverflowString(t *testing.T) {
	var overflow SecurityDestination
	if got := overflow.String(); got != "other destinations" {
		t.Fatalf("expected overflow destination to stringify as 'other destinations', got %q", got)
	}
}
