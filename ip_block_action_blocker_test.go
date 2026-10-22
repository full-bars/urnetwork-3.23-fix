package connect

import (
	"encoding/binary"
	"net"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// the pin consults the ad/tracker blocker in the egress decision and surfaces
// the match as a Blocker-attributed block action. this fork's egress path
// carries no block action stream or block stats, so these tests pin the fork's
// hook points: blockActionApply's blocker fusion, and the multi client
// consulting `SetBlocker` before any route decision (including the port 25
// local route and the local security bypass), so a blocked destination is
// dropped without reaching the security policy and toggling the blocker takes
// effect immediately.

// TestBlockActionApplyBlocker: the blocker fuses into the egress decision as
// a default block, overridable like the security decision — except that an
// un-blocked blocker match (allow-classified) egresses remotely as normal.
func TestBlockActionApplyBlocker(t *testing.T) {
	blockTrue := &blockActionMatch{blockOverride: &BlockOverride{Block: true}}
	blockFalse := &blockActionMatch{blockOverride: &BlockOverride{Block: false}}
	localTrue := &blockActionMatch{routeOverride: &RouteOverride{Local: true}}

	type applyCase struct {
		r            SecurityPolicyResult
		bypass       bool
		blockerBlock bool
		match        *blockActionMatch
		block        bool
		local        bool
	}
	cases := []applyCase{
		// a blocker match blocks by default
		{SecurityPolicyResultAllow, false, true, nil, true, false},
		{SecurityPolicyResultAllow, true, true, nil, true, false},
		// the blocker block wins over the bypass-local default route
		{SecurityPolicyResultDrop, true, true, nil, true, false},
		// an un-blocked blocker match egresses remotely (allow-classified)
		{SecurityPolicyResultAllow, false, true, blockFalse, false, false},
		// an un-blocked drop-classified match still never egresses remotely
		{SecurityPolicyResultDrop, false, true, blockFalse, false, true},
		// a block override agrees with the blocker
		{SecurityPolicyResultAllow, false, true, blockTrue, true, false},
		// a route override on a blocker match: block wins over local
		{SecurityPolicyResultAllow, false, true, localTrue, true, false},
		// incident is terminal regardless of the blocker
		{SecurityPolicyResultIncident, false, true, blockFalse, true, false},
		// no blocker match leaves the existing behavior (spot check)
		{SecurityPolicyResultAllow, false, false, nil, false, false},
		{SecurityPolicyResultDrop, true, false, nil, false, true},
	}
	for i, c := range cases {
		block, local := blockActionApply(c.r, c.bypass, c.blockerBlock, c.match)
		AssertEqual(t, c.block, block)
		AssertEqual(t, c.local, local)
		if c.block != block || c.local != local {
			t.Logf("case %d failed: %+v", i, c)
		}
	}
}

func newBlockerMultiClient(t *testing.T, policy *smtpSeamPolicy) *RemoteUserNatMultiClient {
	t.Helper()
	return newSmtpSeamMulti(t, policy, func(
		_ TransferPath,
		_ protocol.ProvideMode,
		_ *IpPath,
		_ []byte,
	) {
	})
}

// blockerTestTcp4Packet is smtpSeamTcp4Packet with a caller-chosen destination,
// so one test can drive several destinations with one blocker.
func blockerTestTcp4Packet(destinationIp net.IP, sourcePort int, destinationPort int, flags byte) []byte {
	sourceIp := net.IPv4(10, 0, 0, 2).To4()
	destinationIp = destinationIp.To4()
	packet := make([]byte, Ipv4HeaderSizeWithoutExtensions+TcpHeaderSizeWithoutExtensions)
	writeIpv4Header(packet, ipProtocolNumberTcp, sourceIp, destinationIp)
	tcp := packet[Ipv4HeaderSizeWithoutExtensions:]
	binary.BigEndian.PutUint16(tcp[0:2], uint16(sourcePort))
	binary.BigEndian.PutUint16(tcp[2:4], uint16(destinationPort))
	binary.BigEndian.PutUint32(tcp[4:8], 0)
	binary.BigEndian.PutUint32(tcp[8:12], 0)
	tcp[12] = byte(TcpHeaderSizeWithoutExtensions/4) << 4
	tcp[13] = flags
	binary.BigEndian.PutUint16(tcp[14:16], 65535)
	binary.BigEndian.PutUint16(tcp[16:18], transportChecksum(
		ipProtocolNumberTcp,
		sourceIp,
		destinationIp,
		tcp,
	))
	return packet
}

// TestMultiClientBlockerIp: a destination inside the blocker's ranges is
// dropped before the security policy, and toggling the blocker takes effect
// immediately. the pinned packet classifies as an incident, which the policy
// path drops after consulting the policy — so an unblocked packet proves the
// policy is reached while a blocked one must never be.
func TestMultiClientBlockerIp(t *testing.T) {
	policy := &smtpSeamPolicy{
		stats:  DefaultSecurityPolicyStatsCollector(),
		result: SecurityPolicyResultIncident,
	}
	multi := newBlockerMultiClient(t, policy)
	defer multi.Close()

	blocker := blockerTestNew(
		nil,
		[][2]uint32{{blockerTestIp4("203.0.113.0"), blockerTestIp4("203.0.113.255")}},
		nil,
	)
	multi.SetBlocker(blocker)

	blockedPacket := blockerTestTcp4Packet(net.IPv4(203, 0, 113, 10), 48010, 443, byte(tcpFlagSyn))
	if multi.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, blockedPacket, 0) {
		t.Fatal("blocked egress accepted")
	}
	if calls := policy.calls.Load(); calls != 0 {
		t.Fatalf("blocked egress reached the security policy %d times, want 0", calls)
	}

	// disabling the blocker takes effect immediately
	blocker.SetEnabled(false)
	cleanPacket := blockerTestTcp4Packet(net.IPv4(203, 0, 113, 10), 48011, 443, byte(tcpFlagSyn))
	multi.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, cleanPacket, 0)
	if calls := policy.calls.Load(); calls != 1 {
		t.Fatalf("unblocked egress reached the security policy %d times, want 1", calls)
	}

	// re-enabling blocks again
	blocker.SetEnabled(true)
	blockedAgain := blockerTestTcp4Packet(net.IPv4(203, 0, 113, 10), 48012, 443, byte(tcpFlagSyn))
	if multi.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, blockedAgain, 0) {
		t.Fatal("re-blocked egress accepted")
	}
	if calls := policy.calls.Load(); calls != 1 {
		t.Fatalf("re-blocked egress reached the security policy %d times, want 1", calls)
	}

	// clearing the blocker returns the destination to the policy
	multi.SetBlocker(nil)
	clearedPacket := blockerTestTcp4Packet(net.IPv4(203, 0, 113, 10), 48013, 443, byte(tcpFlagSyn))
	multi.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, clearedPacket, 0)
	if calls := policy.calls.Load(); calls != 2 {
		t.Fatalf("cleared blocker egress reached the security policy %d times, want 2", calls)
	}
}

type blockerTestServerNameLookup struct {
	names map[string][]string
}

func (self *blockerTestServerNameLookup) ServerNames(ip string) []string {
	return self.names[ip]
}

// TestMultiClientBlockerServerName: a destination outside the blocked ranges
// whose own observed server name is a blocked host is dropped, while a
// destination with only clean names is not; installing the lookup after the
// blocker keeps the blocker (the config setters must compose).
func TestMultiClientBlockerServerName(t *testing.T) {
	policy := &smtpSeamPolicy{
		stats:  DefaultSecurityPolicyStatsCollector(),
		result: SecurityPolicyResultIncident,
	}
	multi := newBlockerMultiClient(t, policy)
	defer multi.Close()

	multi.SetBlocker(blockerTestNew([]string{"ads.example.com"}, nil, nil))
	multi.SetServerNameLookup(&blockerTestServerNameLookup{
		names: map[string][]string{
			"203.0.114.10": {"cdn.example.com", "sub.ads.example.com"},
			"203.0.115.10": {"cdn.example.com"},
		},
	})

	// blocked via its own server name (the blocked base suffix-matches the
	// subdomain), without any blocked ip range in play
	blockedPacket := blockerTestTcp4Packet(net.IPv4(203, 0, 114, 10), 48020, 443, byte(tcpFlagSyn))
	if multi.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, blockedPacket, 0) {
		t.Fatal("blocked server name accepted")
	}
	if calls := policy.calls.Load(); calls != 0 {
		t.Fatalf("blocked server name reached the security policy %d times, want 0", calls)
	}

	// a clean destination with clean names reaches the policy
	cleanPacket := blockerTestTcp4Packet(net.IPv4(203, 0, 115, 10), 48021, 443, byte(tcpFlagSyn))
	multi.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, cleanPacket, 0)
	if calls := policy.calls.Load(); calls != 1 {
		t.Fatalf("clean destination reached the security policy %d times, want 1", calls)
	}
}

// TestMultiClientBlockerBeforeLocalRoutes: the blocker gate runs before both
// local routes — the deliberate TCP/25 route and the local security bypass —
// and before the security policy, so a blocked destination never takes
// either. each case first proves the route accepts the destination without
// the blocker, so the rejection cannot be an unrelated early return.
func TestMultiClientBlockerBeforeLocalRoutes(t *testing.T) {
	blockerRanges := [][2]uint32{{
		blockerTestIp4("203.0.113.0"),
		blockerTestIp4("203.0.113.255"),
	}}

	t.Run("port25", func(t *testing.T) {
		policy := &smtpSeamPolicy{
			stats:  DefaultSecurityPolicyStatsCollector(),
			result: SecurityPolicyResultDrop,
		}
		multi := newBlockerMultiClient(t, policy)
		defer multi.Close()

		// without a blocker, tcp/25 takes the deliberate local route
		openPacket := blockerTestTcp4Packet(net.IPv4(203, 0, 113, 10), 48030, smtpLocalPort, byte(tcpFlagSyn))
		if !multi.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, openPacket, 0) {
			t.Fatal("unblocked tcp/25 did not take the local route")
		}
		if calls := policy.calls.Load(); calls != 0 {
			t.Fatalf("unblocked tcp/25 reached the security policy %d times, want 0", calls)
		}

		multi.SetBlocker(blockerTestNew(nil, blockerRanges, nil))
		blockedPacket := blockerTestTcp4Packet(net.IPv4(203, 0, 113, 10), 48031, smtpLocalPort, byte(tcpFlagSyn))
		if multi.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, blockedPacket, 0) {
			t.Fatal("blocked tcp/25 was accepted on the local route")
		}
		if calls := policy.calls.Load(); calls != 0 {
			t.Fatalf("blocked tcp/25 reached the security policy %d times, want 0", calls)
		}
	})

	t.Run("localSecurityBypass", func(t *testing.T) {
		policy := &smtpSeamPolicy{
			stats:  DefaultSecurityPolicyStatsCollector(),
			result: SecurityPolicyResultDrop,
		}
		multi := newBlockerMultiClient(t, policy)
		defer multi.Close()
		multi.SetLocalSecurityBypass(true)

		// without a blocker, drop-classified traffic takes the bypass route
		openPacket := blockerTestTcp4Packet(net.IPv4(203, 0, 113, 10), 48032, 443, byte(tcpFlagSyn))
		if !multi.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, openPacket, 0) {
			t.Fatal("unblocked drop-classified traffic did not take the bypass route")
		}
		if calls := policy.calls.Load(); calls != 1 {
			t.Fatalf("unblocked drop-classified traffic reached the security policy %d times, want 1", calls)
		}

		multi.SetBlocker(blockerTestNew(nil, blockerRanges, nil))
		blockedPacket := blockerTestTcp4Packet(net.IPv4(203, 0, 113, 10), 48033, 443, byte(tcpFlagSyn))
		if multi.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, blockedPacket, 0) {
			t.Fatal("blocked egress was accepted over the local security bypass")
		}
		if calls := policy.calls.Load(); calls != 1 {
			t.Fatalf("blocked egress reached the security policy %d times, want 1", calls)
		}
	})

	t.Run("allowClassified", func(t *testing.T) {
		policy := &smtpSeamPolicy{
			stats:  DefaultSecurityPolicyStatsCollector(),
			result: SecurityPolicyResultAllow,
		}
		multi := newBlockerMultiClient(t, policy)
		defer multi.Close()
		multi.SetBlocker(blockerTestNew(nil, blockerRanges, nil))

		// the blocker blocks by default whatever the security classification
		blockedPacket := blockerTestTcp4Packet(net.IPv4(203, 0, 113, 10), 48034, 443, byte(tcpFlagSyn))
		if multi.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, blockedPacket, 0) {
			t.Fatal("blocked egress was accepted before the security policy")
		}
		if calls := policy.calls.Load(); calls != 0 {
			t.Fatalf("blocked egress reached the security policy %d times, want 0", calls)
		}

		// and the same destination is allowed once the blocker is off
		multi.SetBlocker(nil)
		openPacket := blockerTestTcp4Packet(net.IPv4(203, 0, 113, 10), 48035, 443, byte(tcpFlagSyn))
		multi.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, openPacket, 0)
		if calls := policy.calls.Load(); calls != 1 {
			t.Fatalf("unblocked egress reached the security policy %d times, want 1", calls)
		}
	})
}
