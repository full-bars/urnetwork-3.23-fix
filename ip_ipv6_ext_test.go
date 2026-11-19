package connect

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
)

// ip_ipv6_ext_test.go — the extension-header walker and every parser and
// peek that shares it (upstream connect IPV6.md C3): a flow whose packets carry hop-by-hop,
// routing, destination-options, authentication or atomic fragment headers must
// parse, classify, shard and sniff exactly like its plain packets, and a
// non-atomic fragment must be recognized as one everywhere.

// testIpv6Ext is one extension header for the test builder: `kind` is the
// next-header value that names it and `extra8` is how many 8-octet units it
// carries beyond the first 8.
type testIpv6Ext struct {
	kind   byte
	extra8 int
}

// testIpv6ExtChain encodes a chain of options-style extension headers so that
// the last one points at `transport`. The base header is not included.
func testIpv6ExtChain(exts []testIpv6Ext, transport byte) []byte {
	out := []byte{}
	for i, ext := range exts {
		next := transport
		if i+1 < len(exts) {
			next = exts[i+1].kind
		}
		var header []byte
		switch ext.kind {
		case ipv6NextHeaderAuthentication:
			// rfc 4302: length in 4-octet units minus 2; keep it at the
			// minimum 12 bytes plus extra8*8
			byteCount := 12 + ext.extra8*8
			header = make([]byte, byteCount)
			header[0] = next
			header[1] = byte(byteCount/4 - 2)
		default:
			byteCount := 8 * (1 + ext.extra8)
			header = make([]byte, byteCount)
			header[0] = next
			header[1] = byte(ext.extra8)
			// pad-n options fill the rest; a walker never reads them
		}
		out = append(out, header...)
	}
	return out
}

// testIpv6Packet builds a v6 packet: base header, the extension chain, then
// `transport` bytes for protocol `transportProtocol`.
func testIpv6Packet(src string, dst string, exts []testIpv6Ext, transportProtocol byte, transport []byte) []byte {
	chain := testIpv6ExtChain(exts, transportProtocol)
	packet := make([]byte, Ipv6HeaderSize+len(chain)+len(transport))
	packet[0] = 0x60
	binary.BigEndian.PutUint16(packet[4:6], uint16(len(chain)+len(transport)))
	if 0 < len(exts) {
		packet[6] = exts[0].kind
	} else {
		packet[6] = transportProtocol
	}
	packet[7] = 64
	copy(packet[8:24], net.ParseIP(src).To16())
	copy(packet[24:40], net.ParseIP(dst).To16())
	copy(packet[Ipv6HeaderSize:], chain)
	copy(packet[Ipv6HeaderSize+len(chain):], transport)
	return packet
}

// testIpv6Udp builds a checksummed udp packet with the given extension chain.
func testIpv6Udp(src string, dst string, exts []testIpv6Ext, srcPort int, dstPort int, payload []byte) []byte {
	udp := make([]byte, UdpHeaderSize+len(payload))
	binary.BigEndian.PutUint16(udp[0:2], uint16(srcPort))
	binary.BigEndian.PutUint16(udp[2:4], uint16(dstPort))
	binary.BigEndian.PutUint16(udp[4:6], uint16(len(udp)))
	copy(udp[UdpHeaderSize:], payload)
	checksum := transportChecksum(ipProtocolNumberUdp, net.ParseIP(src).To16(), net.ParseIP(dst).To16(), udp)
	if checksum == 0 {
		checksum = 0xffff
	}
	binary.BigEndian.PutUint16(udp[6:8], checksum)
	return testIpv6Packet(src, dst, exts, byte(ipProtocolNumberUdp), udp)
}

// testIpv6Tcp builds a checksummed tcp packet (no options) with the given
// extension chain.
func testIpv6Tcp(src string, dst string, exts []testIpv6Ext, srcPort int, dstPort int, flags byte, seq uint32, payload []byte) []byte {
	tcp := make([]byte, TcpHeaderSizeWithoutExtensions+len(payload))
	binary.BigEndian.PutUint16(tcp[0:2], uint16(srcPort))
	binary.BigEndian.PutUint16(tcp[2:4], uint16(dstPort))
	binary.BigEndian.PutUint32(tcp[4:8], seq)
	tcp[12] = byte(TcpHeaderSizeWithoutExtensions/4) << 4
	tcp[13] = flags
	binary.BigEndian.PutUint16(tcp[14:16], 4096)
	copy(tcp[TcpHeaderSizeWithoutExtensions:], payload)
	binary.BigEndian.PutUint16(tcp[16:18], transportChecksum(ipProtocolNumberTcp, net.ParseIP(src).To16(), net.ParseIP(dst).To16(), tcp))
	return testIpv6Packet(src, dst, exts, byte(ipProtocolNumberTcp), tcp)
}

// testIpv6FragmentHeader encodes a fragment header pointing at `next`.
func testIpv6FragmentHeader(next byte, offset int, more bool, identification uint32) []byte {
	header := make([]byte, ipv6FragmentHeaderSize)
	header[0] = next
	offsetAndFlags := uint16(offset)
	if more {
		offsetAndFlags |= 1
	}
	binary.BigEndian.PutUint16(header[2:4], offsetAndFlags)
	binary.BigEndian.PutUint32(header[4:8], identification)
	return header
}

// testIpv6FragmentPacket builds a raw fragment: base header, optional
// hop-by-hop header, then the fragment header, then `data`.
func testIpv6FragmentPacket(src string, dst string, hopByHop bool, next byte, offset int, more bool, identification uint32, data []byte) []byte {
	body := []byte{}
	if hopByHop {
		body = append(body, testIpv6ExtChain([]testIpv6Ext{{kind: ipv6NextHeaderHopByHop}}, ipv6NextHeaderFragment)...)
	}
	body = append(body, testIpv6FragmentHeader(next, offset, more, identification)...)
	body = append(body, data...)
	packet := make([]byte, Ipv6HeaderSize+len(body))
	packet[0] = 0x60
	binary.BigEndian.PutUint16(packet[4:6], uint16(len(body)))
	if hopByHop {
		packet[6] = ipv6NextHeaderHopByHop
	} else {
		packet[6] = ipv6NextHeaderFragment
	}
	packet[7] = 64
	copy(packet[8:24], net.ParseIP(src).To16())
	copy(packet[24:40], net.ParseIP(dst).To16())
	copy(packet[Ipv6HeaderSize:], body)
	return packet
}

const (
	testIpv6Src = "fd00::1"
	testIpv6Dst = "2606:2800:220:1::1"
)

var testIpv6ChainAll = []testIpv6Ext{
	{kind: ipv6NextHeaderHopByHop},
	{kind: ipv6NextHeaderDestinationOptions, extra8: 1},
	{kind: ipv6NextHeaderRouting, extra8: 2},
	{kind: ipv6NextHeaderAuthentication, extra8: 1},
	{kind: ipv6NextHeaderDestinationOptions},
}

func testIpv6ChainByteCount(exts []testIpv6Ext) int {
	return len(testIpv6ExtChain(exts, byte(ipProtocolNumberUdp)))
}

func TestWalkIpv6ExtensionHeaders(t *testing.T) {
	resetIcmpGate(t)
	udp := make([]byte, UdpHeaderSize+4)
	binary.BigEndian.PutUint16(udp[4:6], uint16(len(udp)))

	// no extension chain
	plain := testIpv6Packet(testIpv6Src, testIpv6Dst, nil, byte(ipProtocolNumberUdp), udp)
	walk, ok := walkIpv6ExtensionHeaders(plain)
	if !ok || walk.nextHeader != ipProtocolNumberUdp || walk.transportOffset != Ipv6HeaderSize ||
		walk.payloadEnd != len(plain) || walk.fragmentPresent || walk.fragmented {
		t.Fatalf("plain walk = %+v, %t", walk, ok)
	}

	// every options-style header plus authentication, chained
	chained := testIpv6Packet(testIpv6Src, testIpv6Dst, testIpv6ChainAll, byte(ipProtocolNumberUdp), udp)
	walk, ok = walkIpv6ExtensionHeaders(chained)
	wantOffset := Ipv6HeaderSize + testIpv6ChainByteCount(testIpv6ChainAll)
	if !ok || walk.nextHeader != ipProtocolNumberUdp || walk.transportOffset != wantOffset ||
		walk.payloadEnd != len(chained) || walk.fragmentPresent {
		t.Fatalf("chained walk = %+v, %t, want transport at %d", walk, ok, wantOffset)
	}

	// an atomic fragment (offset 0, m clear) is walked through
	atomic := testIpv6FragmentPacket(testIpv6Src, testIpv6Dst, true, byte(ipProtocolNumberUdp), 0, false, 0x11223344, udp)
	walk, ok = walkIpv6ExtensionHeaders(atomic)
	if !ok || walk.fragmented || !walk.fragmentPresent || walk.nextHeader != ipProtocolNumberUdp ||
		walk.transportOffset != Ipv6HeaderSize+8+ipv6FragmentHeaderSize ||
		walk.fragment.identification != 0x11223344 ||
		walk.fragment.headerOffset != Ipv6HeaderSize+8 ||
		walk.fragment.precedingNextHeaderFieldOffset != Ipv6HeaderSize {
		t.Fatalf("atomic fragment walk = %+v, %t", walk, ok)
	}

	// a non-atomic fragment ends the walk at the fragment header
	first := testIpv6FragmentPacket(testIpv6Src, testIpv6Dst, false, byte(ipProtocolNumberUdp), 0, true, 7, make([]byte, 16))
	walk, ok = walkIpv6ExtensionHeaders(first)
	if !ok || !walk.fragmented || walk.fragment.offset != 0 || !walk.fragment.moreFragments ||
		walk.fragment.identification != 7 || walk.transportOffset != Ipv6HeaderSize+ipv6FragmentHeaderSize ||
		walk.fragment.precedingNextHeaderFieldOffset != 6 {
		t.Fatalf("first fragment walk = %+v, %t", walk, ok)
	}
	last := testIpv6FragmentPacket(testIpv6Src, testIpv6Dst, false, byte(ipProtocolNumberUdp), 16, false, 7, make([]byte, 5))
	walk, ok = walkIpv6ExtensionHeaders(last)
	if !ok || !walk.fragmented || walk.fragment.offset != 16 || walk.fragment.moreFragments {
		t.Fatalf("last fragment walk = %+v, %t", walk, ok)
	}

	// esp and no-next-header stop the walk with their own value
	for _, terminal := range []byte{ipv6NextHeaderEsp, ipv6NextHeaderNoNextHeader} {
		packet := testIpv6Packet(testIpv6Src, testIpv6Dst, []testIpv6Ext{{kind: ipv6NextHeaderHopByHop}}, terminal, make([]byte, 8))
		walk, ok = walkIpv6ExtensionHeaders(packet)
		if !ok || walk.nextHeader != ipProtocolNumber(terminal) || walk.transportOffset != Ipv6HeaderSize+8 {
			t.Fatalf("terminal %d walk = %+v, %t", terminal, walk, ok)
		}
	}

	// malformed: a truncated extension header
	truncated := append([]byte(nil), chained...)
	truncated = truncated[:Ipv6HeaderSize+12]
	binary.BigEndian.PutUint16(truncated[4:6], 12)
	if _, ok := walkIpv6ExtensionHeaders(truncated); ok {
		t.Fatal("truncated extension header walked")
	}
	// malformed: a declared payload longer than the packet
	short := append([]byte(nil), plain...)
	binary.BigEndian.PutUint16(short[4:6], uint16(len(short)))
	if _, ok := walkIpv6ExtensionHeaders(short); ok {
		t.Fatal("over-declared payload walked")
	}
	// malformed: two fragment headers
	double := testIpv6FragmentPacket(testIpv6Src, testIpv6Dst, false, ipv6NextHeaderFragment, 0, false, 1,
		append(testIpv6FragmentHeader(byte(ipProtocolNumberUdp), 0, false, 2), udp...))
	if _, ok := walkIpv6ExtensionHeaders(double); ok {
		t.Fatal("double fragment header walked")
	}
	// malformed: a chain longer than the bound
	long := []testIpv6Ext{}
	for i := 0; i <= ipv6MaxExtensionHeaderCount; i += 1 {
		long = append(long, testIpv6Ext{kind: ipv6NextHeaderDestinationOptions})
	}
	if _, ok := walkIpv6ExtensionHeaders(testIpv6Packet(testIpv6Src, testIpv6Dst, long, byte(ipProtocolNumberUdp), udp)); ok {
		t.Fatal("over-long chain walked")
	}
	// not ipv6
	if _, ok := walkIpv6ExtensionHeaders(testingUdp4Packet("10.0.0.1", "203.0.113.7", 53, nil)); ok {
		t.Fatal("ipv4 walked as ipv6")
	}
	if _, ok := walkIpv6ExtensionHeaders(nil); ok {
		t.Fatal("empty walked")
	}
}

// TestParseIpPathIpv6ExtensionHeaders and TestParseIpPathIpv6FragmentDrop
// below are restored verbatim from upstream connect b31ec82df ip_ipv6_ext_test.go
// now that parseIpv6 consults the extension walker (T30), except that the icmp
// sub-case asserts the current contract: icmp stays unsupported at ParseIpPath
// on this line until the icmp echo parse wire-up lands.

func TestParseIpPathIpv6ExtensionHeaders(t *testing.T) {
	resetIcmpGate(t)
	payload := []byte("hello")
	udp := testIpv6Udp(testIpv6Src, testIpv6Dst, testIpv6ChainAll, 40001, 53, payload)
	ipPath, got, err := ParseIpPathWithPayload(udp)
	if err != nil {
		t.Fatalf("udp with extension chain: %v", err)
	}
	if ipPath.Version != 6 || ipPath.Protocol != IpProtocolUdp || ipPath.SourcePort != 40001 ||
		ipPath.DestinationPort != 53 || !bytes.Equal(got, payload) ||
		ipPath.SourceIp.String() != testIpv6Src || ipPath.DestinationIp.String() != testIpv6Dst {
		t.Fatalf("udp path = %+v payload=%q", ipPath, got)
	}

	tcp := testIpv6Tcp(testIpv6Src, testIpv6Dst, []testIpv6Ext{{kind: ipv6NextHeaderDestinationOptions, extra8: 3}}, 40002, 443, tcpFlagSyn, 0x1234, nil)
	ipPath, err = ParseIpPath(tcp)
	if err != nil {
		t.Fatalf("tcp with destination options: %v", err)
	}
	if ipPath.Protocol != IpProtocolTcp || !ipPath.Syn || ipPath.SequenceNumber != 0x1234 || ipPath.DestinationPort != 443 {
		t.Fatalf("tcp path = %+v", ipPath)
	}

	// an atomic fragment carries a whole packet
	udpTransport := udp[Ipv6HeaderSize+testIpv6ChainByteCount(testIpv6ChainAll):]
	atomic := testIpv6FragmentPacket(testIpv6Src, testIpv6Dst, false, byte(ipProtocolNumberUdp), 0, false, 9, udpTransport)
	if ipPath, err = ParseIpPath(atomic); err != nil || ipPath.DestinationPort != 53 {
		t.Fatalf("atomic fragment = %+v, %v", ipPath, err)
	}

	// icmp echo is wired in: the chain is walked and the echo is parsed behind
	// the extension header exactly as behind none, and the kill switch still
	// rejects it at the parse
	echo := testingIcmp6EchoPacket(testIpv6Src, testIpv6Dst, 0x77, 1, []byte("ping"))
	icmp := echo[Ipv6HeaderSize:]
	wrapped := testIpv6Packet(testIpv6Src, testIpv6Dst, []testIpv6Ext{{kind: ipv6NextHeaderHopByHop}}, byte(ipProtocolNumberIcmp6), icmp)
	if ipPath, err = ParseIpPath(wrapped); err != nil || ipPath.Protocol != IpProtocolIcmp || ipPath.Version != 6 {
		t.Fatalf("icmp echo over hop-by-hop = %+v, %v", ipPath, err)
	}
}

// the v6 analog of TestParseIpPathIpv4FragmentDrop: only a non-atomic
// fragment is rejected, since it has no transport header to read
func TestParseIpPathIpv6FragmentDrop(t *testing.T) {
	resetIcmpGate(t)
	udp := testIpv6Udp(testIpv6Src, testIpv6Dst, nil, 40001, 53, []byte("payload"))
	transport := udp[Ipv6HeaderSize:]
	cases := []struct {
		name       string
		offset     int
		more       bool
		expectDrop bool
	}{
		{name: "atomic", offset: 0, more: false, expectDrop: false},
		{name: "first", offset: 0, more: true, expectDrop: true},
		{name: "middle", offset: 8, more: true, expectDrop: true},
		{name: "last", offset: 8, more: false, expectDrop: true},
	}
	for _, c := range cases {
		packet := testIpv6FragmentPacket(testIpv6Src, testIpv6Dst, false, byte(ipProtocolNumberUdp), c.offset, c.more, 5, transport)
		_, err := ParseIpPath(packet)
		if c.expectDrop != (err != nil) {
			t.Errorf("%s: expectDrop=%v err=%v", c.name, c.expectDrop, err)
		}
	}
}

// PORTED SUBSET (T16, S1). Upstream's ip_ipv6_ext_test.go is a cross-cutting
// suite: it exercises the walker through every parser and peek that shares it.
// S1 added the walker as an INERT file; T30 (fix/v6-ext-parse) wired it into
// parseIpv6 and restored the two S3-owned parse tests above, with the icmp
// sub-case adapted to this line's contract (icmp parse lands with its own
// wire-up, not before).
//
// The six remaining upstream tests and the slice that owns each:
//
//   TestSendShardIpv6ExtensionHeaders        -> S3 (ip.go: sendShard)
//   TestPeekClaimIpv6ExtensionHeaders        -> the mux port (ip_mux_upgrade.go)
//   TestPeekTlsSegmentIpv6ExtensionHeaders   -> the mux port (needs tlsSegment)
//   TestFirstLoadTcpPeekIpv6                 -> the mux port (FirstLoad)
//   TestIcmpUnreachableIpv6ExtensionHeaders  -> T1/S3 (ipOosUnreachable)
//   TestParseIpPathIcmpv6LinkControlError    -> S2 (icmp parse)
//
// Restore each verbatim from upstream when its slice lands.
