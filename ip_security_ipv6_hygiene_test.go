package connect

import (
	"net"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// the first IPv4 address on the CFAA blocklist, read from the packed table
// (8 bytes per range: first and last address, big endian)
func firstCfaaBlockedIp4(t *testing.T) net.IP {
	t.Helper()
	if cfaaBlockedPrefixCount == 0 || len(cfaaBlockedPrefixData) < 8 {
		t.Fatal("the IPv4 blocklist is empty")
	}
	d := cfaaBlockedPrefixData
	ip := net.IPv4(d[0], d[1], d[2], d[3])
	if !cfaaBlockedIp4(uint32(d[0])<<24 | uint32(d[1])<<16 | uint32(d[2])<<8 | uint32(d[3])) {
		t.Fatal("setup: the first table entry is not reported blocked")
	}
	return ip
}

func mappedIpv6(ip4 net.IP) net.IP {
	b := ip4.To4()
	return net.IP{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, b[0], b[1], b[2], b[3]}
}

// A packet is IPv6 on the wire, but the destination ::ffff:a.b.c.d is dialed over
// IPv4. The blocklist must apply to the address it wraps.
func TestCfaaMappedIpv6DestinationIsJudgedByTheIpv4Blocklist(t *testing.T) {
	blocked := firstCfaaBlockedIp4(t)
	d := newCfaaDetector(DefaultCfaaSecurityPolicySettings())
	// a user port, so only the reputation check can decide it
	const port = 40000

	if got := d.inspect(blocked, port, IpProtocolTcp, 4); got != cfaaDropIp {
		t.Fatalf("setup: the plain IPv4 form gave %v, want drop", got)
	}
	if got := d.inspect(mappedIpv6(blocked), port, IpProtocolTcp, 6); got != cfaaDropIp {
		t.Fatalf("the mapped IPv6 form of a blocked IPv4 address gave %v, want drop", got)
	}
}

func TestEgressPolicyDropsAMappedFormOfABlockedAddress(t *testing.T) {
	blocked := firstCfaaBlockedIp4(t)
	policy := DefaultEgressSecurityPolicy()
	path := &IpPath{
		Version: 6, Protocol: IpProtocolTcp,
		SourceIp: net.ParseIP("2606:4700:4700::1111"), SourcePort: 50000,
		DestinationIp: mappedIpv6(blocked), DestinationPort: 40000,
	}
	res, err := policy.Inspect(protocol.ProvideMode_Public, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res != SecurityPolicyResultDrop {
		t.Fatalf("mapped form of a blocked address gave %v, want drop", res)
	}
}

func TestIsPublicUnicastJudgesMappedAddressesByTheirIpv4Form(t *testing.T) {
	for _, tc := range []struct {
		ip     string
		public bool
	}{
		{"::ffff:10.0.0.1", false},    // private
		{"::ffff:192.168.1.1", false}, // private
		{"::ffff:127.0.0.1", false},   // loopback
		{"::ffff:100.64.0.1", false},  // carrier-grade NAT
		{"::ffff:169.254.1.1", false}, // link local
		{"::ffff:203.0.113.40", true}, // a public IPv4 stays reachable in mapped form
		{"::ffff:8.8.8.8", true},
	} {
		if got := isPublicUnicast(net.ParseIP(tc.ip)); got != tc.public {
			t.Errorf("isPublicUnicast(%s) = %v, want %v", tc.ip, got, tc.public)
		}
	}
}

// Ranges that embed an IPv4 address, or are not an ordinary destination, are
// refused. Ordinary global unicast stays reachable.
func TestIsPublicUnicastRefusesSpecialUseIpv6(t *testing.T) {
	for _, tc := range []struct {
		ip     string
		public bool
	}{
		{"64:ff9b::808:808", false},    // NAT64 well-known
		{"64:ff9b:1::1", false},        // NAT64 local-use
		{"2002:c000:204::1", false},    // 6to4
		{"2001:0:4136:e378::1", false}, // Teredo
		{"::8.8.8.8", false},           // IPv4-compatible
		{"::", false},                  // unspecified
		{"100::1", false},              // discard
		{"2001:db8::1", false},         // documentation
		{"fec0::1", false},             // site-local
		{"3ffe::1", false},             // 6bone
		{"fd00::1", false},             // ULA (already private)
		{"fe80::1", false},             // link local
		// not globally reachable per the IANA special-purpose registry; the
		// addresses just outside each range keep their existing behavior
		{"2001:2::1", false},                               // benchmarking /48, first
		{"2001:2:0:ffff:ffff:ffff:ffff:ffff", false},       // benchmarking /48, last
		{"2001:2:1::1", true},                              // just past the benchmarking /48
		{"2001:3::1", true},                                // AMT, globally reachable
		{"3fff::1", false},                                 // documentation /20, first
		{"3fff:fff:ffff:ffff:ffff:ffff:ffff:ffff", false},  // documentation /20, last
		{"3fff:1000::1", true},                             // just past the /20
		{"3ffd::1", true},                                  // just below 6bone 3ffe::/16
		{"5f00::1", false},                                 // SRv6 SIDs /16, first
		{"5f00:ffff:ffff:ffff:ffff:ffff:ffff:ffff", false}, // SRv6 SIDs /16, last
		{"5f01::1", true},                                  // just past the /16
		{"2606:4700:4700::1111", true},                     // ordinary global unicast
		{"2a00:1450:4001:80b::200e", true},
		{"2001:4860:4860::8888", true}, // next to 2001::/32 but outside it
		{"2003::1", true},              // next to 2002::/16 but outside it
	} {
		if got := isPublicUnicast(net.ParseIP(tc.ip)); got != tc.public {
			t.Errorf("isPublicUnicast(%s) = %v, want %v", tc.ip, got, tc.public)
		}
	}
}
