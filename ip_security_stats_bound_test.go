package connect

import (
	"net"
	"sync"
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

// String() gained an overflow branch, but ordinary, non-zero destinations
// must still render exactly as before.
func TestSecurityDestinationString(t *testing.T) {
	cases := []struct {
		name        string
		destination SecurityDestination
		want        string
	}{
		{
			name:        "ipv4 with ip",
			destination: SecurityDestination{Version: 4, Protocol: IpProtocolTcp, Ip: "1.2.3.4", Port: 443},
			want:        "ipv4 tcp 1.2.3.4:443",
		},
		{
			name:        "ipv6 port only",
			destination: SecurityDestination{Version: 6, Protocol: IpProtocolUdp, Ip: "", Port: 53},
			want:        "ipv6 udp :53",
		},
		{
			name:        "overflow bucket",
			destination: securityPolicyStatsOverflowDestination,
			want:        "other destinations",
		},
	}
	for _, c := range cases {
		if got := c.destination.String(); got != c.want {
			t.Errorf("%s: String() = %q, want %q", c.name, got, c.want)
		}
	}
}

// Built-in policies only ever produce Drop, Allow, or Incident. Each must
// keep its own independent bucket rather than being folded together or into
// the unknown-result bucket.
func TestSecurityPolicyStatsCollectorKnownResultsKeepOwnBuckets(t *testing.T) {
	collector := DefaultSecurityPolicyStatsCollector()

	results := []SecurityPolicyResult{SecurityPolicyResultDrop, SecurityPolicyResultAllow, SecurityPolicyResultIncident}
	for i, result := range results {
		ipPath := &IpPath{
			Version:         4,
			Protocol:        IpProtocolTcp,
			SourceIp:        net.IPv4(10, 0, 0, 1),
			SourcePort:      1234,
			DestinationIp:   net.IPv4(1, 1, 1, byte(i)),
			DestinationPort: 443,
		}
		collector.AddDestination(ipPath, result, uint64(i+1))
	}

	stats := collector.Stats(false)
	for i, result := range results {
		destinationCounts, ok := stats[result]
		if !ok {
			t.Fatalf("expected known result %v to have its own bucket", result)
		}
		var total uint64
		for _, count := range destinationCounts {
			total += count
		}
		want := uint64(i + 1)
		if total != want {
			t.Fatalf("expected result %v total count %d, got %d", result, want, total)
		}
	}
	if _, ok := stats[securityPolicyStatsUnknownResult]; ok {
		t.Fatalf("expected no unknown-result bucket when only known results are used")
	}
}

// The bound must also apply to AddSource, which shares the same underlying
// add() path as AddDestination.
func TestSecurityPolicyStatsCollectorAddSourceBoundsDestinationCount(t *testing.T) {
	collector := DefaultSecurityPolicyStatsCollector()
	collector.includeIp = true

	total := securityPolicyStatsMaxDestinationsPerResult + 30
	for i := 0; i < total; i++ {
		ipPath := &IpPath{
			Version:         4,
			Protocol:        IpProtocolUdp,
			SourceIp:        net.IPv4(byte(i>>24), byte(i>>16), byte(i>>8), byte(i)),
			SourcePort:      1024,
			DestinationIp:   net.IPv4(1, 1, 1, 1),
			DestinationPort: 53,
		}
		collector.AddSource(ipPath, SecurityPolicyResultDrop, 1)
	}

	stats := collector.Stats(false)
	sourceCounts, ok := stats[SecurityPolicyResultDrop]
	if !ok {
		t.Fatalf("expected SecurityPolicyResultDrop to have recorded sources")
	}
	if len(sourceCounts) != securityPolicyStatsMaxDestinationsPerResult {
		t.Fatalf(
			"expected exactly %d distinct sources, got %d",
			securityPolicyStatsMaxDestinationsPerResult,
			len(sourceCounts),
		)
	}
	overflowCount, ok := sourceCounts[securityPolicyStatsOverflowDestination]
	if !ok {
		t.Fatalf("expected an overflow bucket for AddSource once the cap was exceeded")
	}
	wantOverflow := uint64(total - (securityPolicyStatsMaxDestinationsPerResult - 1))
	if overflowCount != wantOverflow {
		t.Fatalf("expected overflow bucket count %d, got %d", wantOverflow, overflowCount)
	}
}

// The cap reserves exactly one slot for overflow, so only
// (securityPolicyStatsMaxDestinationsPerResult - 1) real destinations ever
// get their own entry. This pins the exact boundary rather than just an
// upper bound.
func TestSecurityPolicyStatsCollectorRealSlotsCapAtMaxMinusOne(t *testing.T) {
	collector := DefaultSecurityPolicyStatsCollector()
	collector.includeIp = true

	capMinusOne := securityPolicyStatsMaxDestinationsPerResult - 1
	for i := 0; i < capMinusOne; i++ {
		ipPath := &IpPath{
			Version:         4,
			Protocol:        IpProtocolTcp,
			SourceIp:        net.IPv4(10, 0, 0, 1),
			SourcePort:      1234,
			DestinationIp:   net.IPv4(byte(i>>24), byte(i>>16), byte(i>>8), byte(i)),
			DestinationPort: 443,
		}
		collector.AddDestination(ipPath, SecurityPolicyResultIncident, 1)
	}

	stats := collector.Stats(false)
	destinationCounts := stats[SecurityPolicyResultIncident]
	if len(destinationCounts) != capMinusOne {
		t.Fatalf("expected exactly %d real destination slots before overflow, got %d", capMinusOne, len(destinationCounts))
	}
	if _, ok := destinationCounts[securityPolicyStatsOverflowDestination]; ok {
		t.Fatalf("did not expect an overflow bucket before the cap is exceeded")
	}

	// The next distinct destination is the first one redirected to overflow.
	ipPath := &IpPath{
		Version:    4,
		Protocol:   IpProtocolTcp,
		SourceIp:   net.IPv4(10, 0, 0, 1),
		SourcePort: 1234,
		DestinationIp: net.IPv4(
			byte(capMinusOne>>24), byte(capMinusOne>>16), byte(capMinusOne>>8), byte(capMinusOne),
		),
		DestinationPort: 443,
	}
	collector.AddDestination(ipPath, SecurityPolicyResultIncident, 1)

	stats = collector.Stats(false)
	destinationCounts = stats[SecurityPolicyResultIncident]
	if len(destinationCounts) != securityPolicyStatsMaxDestinationsPerResult {
		t.Fatalf(
			"expected total slots (real + overflow) to reach the cap %d, got %d",
			securityPolicyStatsMaxDestinationsPerResult, len(destinationCounts),
		)
	}
	overflowCount, ok := destinationCounts[securityPolicyStatsOverflowDestination]
	if !ok || overflowCount != 1 {
		t.Fatalf("expected the first destination past the cap to seed the overflow bucket with count 1, got count=%d ok=%v", overflowCount, ok)
	}
}

// Once a destination already has its own slot, later traffic to that same
// destination must keep accumulating there even after the map has started
// overflowing — the overflow redirect only applies to destinations seen for
// the first time.
func TestSecurityPolicyStatsCollectorExistingDestinationIncrementsAfterOverflow(t *testing.T) {
	collector := DefaultSecurityPolicyStatsCollector()
	collector.includeIp = true

	firstIpPath := &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.IPv4(10, 0, 0, 1),
		SourcePort:      1234,
		DestinationIp:   net.IPv4(9, 9, 9, 9),
		DestinationPort: 443,
	}
	// Establish this destination's own slot first.
	collector.AddDestination(firstIpPath, SecurityPolicyResultAllow, 1)

	// Push well past the cap with other distinct destinations so the map is
	// definitely overflowing.
	for i := 0; i < securityPolicyStatsMaxDestinationsPerResult+100; i++ {
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

	// The pre-existing destination receives more traffic after overflow began.
	collector.AddDestination(firstIpPath, SecurityPolicyResultAllow, 5)

	stats := collector.Stats(false)
	destinationCounts := stats[SecurityPolicyResultAllow]

	firstDestination := newSecurityDestination(firstIpPath)
	count, ok := destinationCounts[firstDestination]
	if !ok {
		t.Fatalf("expected the pre-existing destination to keep its own slot despite overflow")
	}
	if count != 6 {
		t.Fatalf("expected pre-existing destination count to accumulate to 6, got %d", count)
	}
}

// The cap is enforced per result, not globally: each result's destination
// map fills and overflows independently of any other result's map.
func TestSecurityPolicyStatsCollectorCapAppliesPerResultIndependently(t *testing.T) {
	collector := DefaultSecurityPolicyStatsCollector()
	collector.includeIp = true

	dropTotal := securityPolicyStatsMaxDestinationsPerResult + 10
	allowTotal := securityPolicyStatsMaxDestinationsPerResult + 20

	for i := 0; i < dropTotal; i++ {
		ipPath := &IpPath{
			Version:         4,
			Protocol:        IpProtocolTcp,
			SourceIp:        net.IPv4(10, 0, 0, 1),
			SourcePort:      1234,
			DestinationIp:   net.IPv4(byte(i>>24), byte(i>>16), byte(i>>8), byte(i)),
			DestinationPort: 443,
		}
		collector.AddDestination(ipPath, SecurityPolicyResultDrop, 1)
	}
	for i := 0; i < allowTotal; i++ {
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

	dropCounts := stats[SecurityPolicyResultDrop]
	if len(dropCounts) != securityPolicyStatsMaxDestinationsPerResult {
		t.Fatalf("expected Drop to be capped at %d, got %d", securityPolicyStatsMaxDestinationsPerResult, len(dropCounts))
	}
	wantDropOverflow := uint64(dropTotal - (securityPolicyStatsMaxDestinationsPerResult - 1))
	if got := dropCounts[securityPolicyStatsOverflowDestination]; got != wantDropOverflow {
		t.Fatalf("expected Drop overflow count %d, got %d", wantDropOverflow, got)
	}

	allowCounts := stats[SecurityPolicyResultAllow]
	if len(allowCounts) != securityPolicyStatsMaxDestinationsPerResult {
		t.Fatalf("expected Allow to be capped at %d, got %d", securityPolicyStatsMaxDestinationsPerResult, len(allowCounts))
	}
	wantAllowOverflow := uint64(allowTotal - (securityPolicyStatsMaxDestinationsPerResult - 1))
	if got := allowCounts[securityPolicyStatsOverflowDestination]; got != wantAllowOverflow {
		t.Fatalf("expected Allow overflow count %d, got %d", wantAllowOverflow, got)
	}
}

// The refactor consolidated AddDestination and AddSource onto a shared,
// mutex-protected add() path. Concurrent callers must not lose updates or
// trigger a data race (run this test with -race).
func TestSecurityPolicyStatsCollectorConcurrentAddIsSafe(t *testing.T) {
	collector := DefaultSecurityPolicyStatsCollector()

	const goroutines = 50
	const perGoroutine = 20

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				ipPath := &IpPath{
					Version:         4,
					Protocol:        IpProtocolTcp,
					SourceIp:        net.IPv4(10, 0, 0, 1),
					SourcePort:      1234,
					DestinationIp:   net.IPv4(8, 8, 8, 8),
					DestinationPort: 443,
				}
				collector.AddDestination(ipPath, SecurityPolicyResultAllow, 1)
			}
		}()
	}
	wg.Wait()

	stats := collector.Stats(false)
	destinationCounts := stats[SecurityPolicyResultAllow]
	var total uint64
	for _, count := range destinationCounts {
		total += count
	}
	const want = uint64(goroutines * perGoroutine)
	if total != want {
		t.Fatalf("expected total count %d after concurrent adds, got %d", want, total)
	}
}
