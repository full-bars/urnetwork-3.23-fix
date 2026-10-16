package connect

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// tests for the provider-side icmp echo wire-up (feat/icmp-wire): the parse
// gate, the security posture, the fixed flow caps, the shard identity and the
// flow lifecycle. The egress socket is environment dependent, so the lifecycle
// test skips when the platform cannot open one (the path fails soft, see
// ICMP.md).

func icmpTransport(typ byte, identifier uint16, sequence uint16) []byte {
	b := make([]byte, IcmpHeaderSize+16)
	b[0] = typ
	b[1] = 0
	binary.BigEndian.PutUint16(b[4:6], identifier)
	binary.BigEndian.PutUint16(b[6:8], sequence)
	for i := IcmpHeaderSize; i < len(b); i += 1 {
		b[i] = 0xa5
	}
	return b
}

func ipv4IcmpPacket(protocolByte ipProtocolNumber, src net.IP, dst net.IP, transport []byte) []byte {
	packet := make([]byte, 20+len(transport))
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	packet[8] = 64
	packet[9] = byte(protocolByte)
	copy(packet[12:16], src.To4())
	copy(packet[16:20], dst.To4())
	copy(packet[20:], transport)
	return packet
}

func ipv6IcmpPacket(nextHeader ipProtocolNumber, src net.IP, dst net.IP, transport []byte) []byte {
	packet := make([]byte, 40+len(transport))
	packet[0] = 0x60
	binary.BigEndian.PutUint16(packet[4:6], uint16(len(transport)))
	packet[6] = byte(nextHeader)
	packet[7] = 64
	copy(packet[8:24], src.To16())
	copy(packet[24:40], dst.To16())
	copy(packet[40:], transport)
	return packet
}

func TestIcmpWireParseEchoShapes(t *testing.T) {
	resetIcmpGate(t)
	src4 := net.IPv4(192, 0, 2, 1)
	dst4 := net.IPv4(192, 0, 2, 2)

	for _, c := range []struct {
		name        string
		version     int
		transport   []byte
		ok          bool
		echoRequest bool
	}{
		{"v4 request", 4, icmpTransport(icmp4TypeEchoRequest, 7, 1), true, true},
		{"v4 reply", 4, icmpTransport(icmp4TypeEchoReply, 7, 1), true, false},
		{"v6 request", 6, icmpTransport(icmp6TypeEchoRequest, 7, 1), true, true},
		{"v6 reply", 6, icmpTransport(icmp6TypeEchoReply, 7, 1), true, false},
		{"v4 destination unreachable", 4, icmpTransport(3, 0, 0), false, false},
		{"short", 4, make([]byte, IcmpHeaderSize-1), false, false},
	} {
		var parsed parsedIcmp
		ok := parseIcmpPacket(c.version, src4, dst4, c.transport, &parsed)
		if ok != c.ok {
			t.Fatalf("%s: parse = %v, want %v", c.name, ok, c.ok)
		}
		if !ok {
			continue
		}
		if parsed.echoRequest != c.echoRequest {
			t.Errorf("%s: echoRequest = %v, want %v", c.name, parsed.echoRequest, c.echoRequest)
		}
		if parsed.identifier != 7 {
			t.Errorf("%s: identifier = %d, want 7", c.name, parsed.identifier)
		}
		if len(parsed.payload) != 16 {
			t.Errorf("%s: payload = %d bytes, want 16", c.name, len(parsed.payload))
		}
	}

	// a non-zero echo code is not an echo (icmpv6 echo replies never set one)
	badCode := icmpTransport(icmp4TypeEchoRequest, 7, 1)
	badCode[1] = 1
	var parsed parsedIcmp
	if parseIcmpPacket(4, src4, dst4, badCode, &parsed) {
		t.Fatal("an echo with code != 0 parsed as an echo")
	}
	// the families do not cross: a v6 request type is not a v4 request
	if parseIcmpPacket(4, src4, dst4, icmpTransport(icmp6TypeEchoRequest, 7, 1), &parsed) {
		t.Fatal("a v6 echo request type parsed as a v4 echo")
	}
}

// the dispatch drops an outbound echo reply before any flow is touched: the
// gate it reads is echoRequest above, so a reply must never read as a request.
func TestIcmpWireReplyIsNotARequest(t *testing.T) {
	resetIcmpGate(t)
	src := net.IPv4(192, 0, 2, 1)
	dst := net.IPv4(192, 0, 2, 2)
	var parsed parsedIcmp
	if !parseIcmpPacket(4, src, dst, icmpTransport(icmp4TypeEchoReply, 7, 1), &parsed) {
		t.Fatal("the reply did not parse")
	}
	if parsed.echoRequest {
		t.Fatal("an echo reply read as a request, so the dispatch would create a flow for it")
	}
}

// no ping may leave toward a destination the egress hygiene refuses, under a
// public relationship. This is the guard against a provider becoming a
// scanner; it applies to icmp exactly as it does to tcp/udp.
func TestIcmpWireEgressDestinationsRefused(t *testing.T) {
	resetIcmpGate(t)
	for _, refused := range []string{
		"::ffff:10.0.0.5",   // v4-mapped private
		"::ffff:127.0.0.1",  // v4-mapped loopback
		"::1",               // loopback
		"fc00::1",           // ula
		"fe80::1",           // link local
		"fec0::1",           // site local (deprecated)
		"ff02::1",           // multicast
		"::",                // unspecified
		"2001:db8::1",       // documentation
		"2002:0a00:0005::1", // 6to4 carrying 10.0.0.5
		"64:ff9b::10.0.0.5", // nat64
		"100::1",            // discard
		"10.0.0.5",          // private
		"127.0.0.1",         // loopback
		"169.254.169.254",   // link local (cloud metadata)
		"100.64.0.1",        // carrier grade nat
	} {
		if isPublicUnicast(net.ParseIP(refused)) {
			t.Errorf("isPublicUnicast(%s) = true; an icmp echo would leave toward it", refused)
		}
	}
	for _, allowed := range []string{"2606:4700:4700::1111", "2001:4860:4860::8888", "::ffff:8.8.8.8", "8.8.8.8"} {
		if !isPublicUnicast(net.ParseIP(allowed)) {
			t.Errorf("isPublicUnicast(%s) = false; a public echo destination is refused", allowed)
		}
	}
}

// an icmp "port" is the echo identifier, so the privileged-port policy must not
// see it: an identifier of 0 or below 1024 would otherwise drop every ping. The
// blocked-ip reputation check above it still applies.
func TestIcmpWireCfaaPortPolicy(t *testing.T) {
	resetIcmpGate(t)
	detector := newCfaaDetector(DefaultCfaaSecurityPolicySettings())
	public := net.ParseIP("2606:4700:4700::1111")
	for _, identifier := range []int{0, 1, 1023, 65535} {
		if verdict := detector.inspect(public, identifier, IpProtocolIcmp, 6); verdict != cfaaAllow {
			t.Errorf("icmp identifier %d got %v, want cfaaAllow (the port policy cannot apply)", identifier, verdict)
		}
	}
	// the same identifiers on a privileged-adjacent tcp port still drop, so the
	// branch really is the icmp one and not a weakened port policy
	if verdict := detector.inspect(public, 1, IpProtocolTcp, 6); verdict != cfaaDropPort {
		t.Errorf("tcp port 1 got %v, want cfaaDropPort", verdict)
	}
}

func TestIcmpWireOosPacketReturnsNilInsteadOfPanicking(t *testing.T) {
	resetIcmpGate(t)
	for _, version := range []int{4, 6} {
		ipPath := &IpPath{
			Version:  version,
			Protocol: IpProtocolIcmp,
		}
		if packet := ipOosPacket(ipPath, []byte{1, 2, 3}); packet != nil {
			t.Errorf("v%d: ipOosPacket on an icmp path returned %d bytes, want nil", version, len(packet))
		}
	}
}

// the caps are ours and fixed: this pins them so a later sync of the icmp files
// cannot quietly swap in the memory-budget-derived ones.
func TestIcmpWireFixedCapsPinned(t *testing.T) {
	resetIcmpGate(t)
	settings := DefaultLocalUserNatSettings()
	if settings.IcmpBufferSettings == nil {
		t.Fatal("the default settings carry no icmp buffer settings")
	}
	if settings.IcmpBufferSettings.UserLimit != 64 {
		t.Errorf("icmp UserLimit = %d, want the fixed 64", settings.IcmpBufferSettings.UserLimit)
	}
	if settings.IcmpBufferSettings.GlobalLimit != 128 {
		t.Errorf("icmp GlobalLimit = %d, want the fixed 128", settings.IcmpBufferSettings.GlobalLimit)
	}
	if settings.IcmpBufferSettings.IdleTimeout != 60*time.Second {
		t.Errorf("icmp IdleTimeout = %s, want 60s (an echo flow is chatty or dead)", settings.IcmpBufferSettings.IdleTimeout)
	}
}

// the eviction helper the flow caps ride on.

type wireTestLimited struct {
	lastActivityTime time.Time
}

func (self *wireTestLimited) LastActivityTime() time.Time {
	return self.lastActivityTime
}

func (self *wireTestLimited) Cancel() {}

func TestIcmpWireLruEviction(t *testing.T) {
	resetIcmpGate(t)
	resources := map[uint16]*wireTestLimited{}
	for i := uint16(0); i < 4; i += 1 {
		resources[i] = &wireTestLimited{
			lastActivityTime: time.Unix(1_000_000, 0).Add(time.Duration(i) * time.Second),
		}
	}
	evicted := []uint16{}
	applyLruMapLimit(resources, 2, func(key uint16, resource *wireTestLimited) bool {
		evicted = append(evicted, key)
		delete(resources, key)
		return true
	})
	if len(resources) != 2 {
		t.Fatalf("kept %d flows, want 2", len(resources))
	}
	if len(evicted) != 2 || evicted[0] != 0 || evicted[1] != 1 {
		t.Fatalf("evicted %v, want the two oldest first ([0 1])", evicted)
	}
}

// the shard identity for icmp is the echo identifier, not a port pair: two
// packets of one flow (same identifier, different sequence) must pin together.
func TestIcmpWireFlowHashIdentity(t *testing.T) {
	resetIcmpGate(t)
	src := net.IPv4(192, 0, 2, 1)
	dst := net.IPv4(192, 0, 2, 2)
	one := ipv4IcmpPacket(ipProtocolNumberIcmp4, src, dst, icmpTransport(icmp4TypeEchoRequest, 7, 1))
	two := ipv4IcmpPacket(ipProtocolNumberIcmp4, src, dst, icmpTransport(icmp4TypeEchoRequest, 7, 2))
	other := ipv4IcmpPacket(ipProtocolNumberIcmp4, src, dst, icmpTransport(icmp4TypeEchoRequest, 8, 1))
	if flowHash(one) != flowHash(two) {
		t.Error("one flow's echo requests hashed to different shards")
	}
	if flowHash(one) == flowHash(other) {
		t.Error("a different echo identifier hashed to the same shard")
	}

	src6 := net.ParseIP("2606:4700:4700::1111")
	dst6 := net.ParseIP("2606:4700:4700::2222")
	v6one := ipv6IcmpPacket(ipProtocolNumberIcmp6, src6, dst6, icmpTransport(icmp6TypeEchoRequest, 7, 1))
	v6two := ipv6IcmpPacket(ipProtocolNumberIcmp6, src6, dst6, icmpTransport(icmp6TypeEchoRequest, 7, 2))
	v6other := ipv6IcmpPacket(ipProtocolNumberIcmp6, src6, dst6, icmpTransport(icmp6TypeEchoRequest, 8, 1))
	if flowHash(v6one) != flowHash(v6two) {
		t.Error("one icmpv6 flow's echo requests hashed to different shards")
	}
	if flowHash(v6one) == flowHash(v6other) {
		t.Error("a different icmpv6 echo identifier hashed to the same shard")
	}
}

// the flow lifecycle through the buffer, when this platform can open an echo
// socket at all. The path fails soft when it cannot (ICMP.md), so that case
// skips rather than fails.
func TestIcmpWireFlowCreateAndReap(t *testing.T) {
	resetIcmpGate(t)
	settings := DefaultIcmpBufferSettings()
	settings.IdleTimeout = 100 * time.Millisecond
	settings.WriteTimeout = 200 * time.Millisecond
	buffer := NewIcmp4Buffer(context.Background(), func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {}, settings)

	icmp := &parsedIcmp{
		sourceIp:      net.IPv4(127, 0, 0, 1),
		destinationIp: net.IPv4(127, 0, 0, 1),
		echoRequest:   true,
		identifier:    1234,
		payload:       []byte{0xa5, 0xa5, 0xa5, 0xa5},
	}
	ipPacket := ipv4IcmpPacket(ipProtocolNumberIcmp4, icmp.sourceIp, icmp.destinationIp, icmpTransport(icmp4TypeEchoRequest, 1234, 1))
	source := TransferPath{SourceId: NewId(), StreamId: NewId()}
	success, err := buffer.send(source, protocol.ProvideMode_Public, icmp, settings.WriteTimeout, ipPacket)
	if !success || err != nil {
		t.Skipf("this platform cannot open the icmp echo backend (%v); the path fails soft by design", err)
	}
	buffer.mutex.Lock()
	flows := len(buffer.sequences)
	buffer.mutex.Unlock()
	if flows != 1 {
		t.Fatalf("after send: %d flows, want 1", flows)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		buffer.mutex.Lock()
		flows := len(buffer.sequences)
		buffer.mutex.Unlock()
		if flows == 0 || !time.Now().Before(deadline) {
			if flows != 0 {
				t.Fatalf("the idle flow was not reaped (IdleTimeout %s)", settings.IdleTimeout)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}
