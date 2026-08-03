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

	// Capacity is capped at securityPolicyStatsMaxDestinationsPerResult,
	// which includes the overflow bucket, regardless of how many distinct
	// destinations were fed in.
	if len(destinationCounts) > securityPolicyStatsMaxDestinationsPerResult {
		t.Fatalf(
			"expected at most %d distinct destinations, got %d",
			securityPolicyStatsMaxDestinationsPerResult,
			len(destinationCounts),
		)
	}

	overflowCount, ok := destinationCounts[securityPolicyStatsOverflowDestination]
	if !ok {
		t.Fatalf("expected an overflow bucket once the cap was exceeded")
	}
	if overflowCount == 0 {
		t.Fatalf("expected the overflow bucket to have accumulated counts")
	}
}

// An unrecognized result value must not bypass the bound by opening a new,
// uncapped bucket keyed by an arbitrary integer.
func TestSecurityPolicyStatsCollectorUnknownResultSharesOneBucket(t *testing.T) {
	collector := DefaultSecurityPolicyStatsCollector()

	for i := 0; i < 10; i++ {
		ipPath := &IpPath{
			Version:         4,
			Protocol:        IpProtocolTcp,
			SourceIp:        net.IPv4(10, 0, 0, 1),
			SourcePort:      1234,
			DestinationIp:   net.IPv4(1, 1, 1, byte(i)),
			DestinationPort: 443 + i,
		}
		// SecurityPolicyResult(99) is not one of the three declared results.
		collector.AddDestination(ipPath, SecurityPolicyResult(99), 1)
	}

	stats := collector.Stats(false)
	if _, ok := stats[SecurityPolicyResult(99)]; ok {
		t.Fatalf("expected result(99) to be remapped, not stored under its own key")
	}
	if _, ok := stats[securityPolicyStatsUnknownResult]; !ok {
		t.Fatalf("expected all unrecognized results to share securityPolicyStatsUnknownResult")
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
