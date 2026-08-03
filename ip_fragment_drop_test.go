package connect

import (
	"encoding/binary"
	"net"
	"testing"
)

func TestParseIpv4DropsFragments(t *testing.T) {
	// Minimal valid IPv4 header (20 bytes) + 8 bytes payload, with the
	// more-fragments (MF) bit set in the flags/fragment-offset field.
	packet := make([]byte, 28)
	packet[0] = 0x45                                // version 4, IHL 5 (20 bytes)
	packet[9] = 17                                  // protocol = UDP
	binary.BigEndian.PutUint16(packet[2:4], 28)     // total length
	binary.BigEndian.PutUint16(packet[6:8], 0x2000) // MF bit set, offset 0
	copy(packet[12:16], net.IPv4(10, 0, 0, 1).To4())
	copy(packet[16:20], net.IPv4(10, 0, 0, 2).To4())

	_, _, _, _, ok := parseIpv4(packet)
	if ok {
		t.Fatalf("expected fragmented packet (MF set) to be dropped, got ok=true")
	}
}

func TestParseIpv4DropsNonZeroOffset(t *testing.T) {
	packet := make([]byte, 28)
	packet[0] = 0x45
	packet[9] = 17
	binary.BigEndian.PutUint16(packet[2:4], 28)
	binary.BigEndian.PutUint16(packet[6:8], 0x0001) // MF clear, offset=1 (non-first fragment)
	copy(packet[12:16], net.IPv4(10, 0, 0, 1).To4())
	copy(packet[16:20], net.IPv4(10, 0, 0, 2).To4())

	_, _, _, _, ok := parseIpv4(packet)
	if ok {
		t.Fatalf("expected non-first fragment (offset != 0) to be dropped, got ok=true")
	}
}

func TestParseIpv4AllowsUnfragmented(t *testing.T) {
	packet := make([]byte, 28)
	packet[0] = 0x45
	packet[9] = 17
	binary.BigEndian.PutUint16(packet[2:4], 28)
	binary.BigEndian.PutUint16(packet[6:8], 0x0000) // no MF, no offset
	copy(packet[12:16], net.IPv4(10, 0, 0, 1).To4())
	copy(packet[16:20], net.IPv4(10, 0, 0, 2).To4())

	ipProtocol, _, _, _, ok := parseIpv4(packet)
	if !ok {
		t.Fatalf("expected unfragmented packet to parse, got ok=false")
	}
	if ipProtocol != IPProtocol(17) {
		t.Fatalf("expected protocol=17 (UDP), got %v", ipProtocol)
	}
}

func TestParseIpv4AllowsDontFragmentBit(t *testing.T) {
	// DF (don't-fragment) is bit 14, outside the 0x3fff mask (MF + 13-bit
	// offset). Must not be misidentified as a fragment.
	packet := make([]byte, 28)
	packet[0] = 0x45
	packet[9] = 17
	binary.BigEndian.PutUint16(packet[2:4], 28)
	binary.BigEndian.PutUint16(packet[6:8], 0x4000) // DF bit set only
	copy(packet[12:16], net.IPv4(10, 0, 0, 1).To4())
	copy(packet[16:20], net.IPv4(10, 0, 0, 2).To4())

	_, _, _, _, ok := parseIpv4(packet)
	if !ok {
		t.Fatalf("expected DF-only packet (not fragmented) to parse, got ok=false")
	}
}

func TestParseIpv4AllowsReservedBitOnly(t *testing.T) {
	// The reserved bit is bit 15 (0x8000), also outside the 0x3fff mask.
	// It must not cause an unfragmented packet to be dropped.
	packet := make([]byte, 28)
	packet[0] = 0x45
	packet[9] = 17
	binary.BigEndian.PutUint16(packet[2:4], 28)
	binary.BigEndian.PutUint16(packet[6:8], 0x8000) // reserved bit set only
	copy(packet[12:16], net.IPv4(10, 0, 0, 1).To4())
	copy(packet[16:20], net.IPv4(10, 0, 0, 2).To4())

	_, _, _, _, ok := parseIpv4(packet)
	if !ok {
		t.Fatalf("expected reserved-bit-only packet (not fragmented) to parse, got ok=false")
	}
}

func TestParseIpv4DropsMaxFragmentOffset(t *testing.T) {
	// Largest possible 13-bit fragment offset (0x1fff) with MF clear: this
	// is the tail fragment of a fragmented datagram and must be dropped.
	packet := make([]byte, 28)
	packet[0] = 0x45
	packet[9] = 17
	binary.BigEndian.PutUint16(packet[2:4], 28)
	binary.BigEndian.PutUint16(packet[6:8], 0x1fff) // MF clear, max offset
	copy(packet[12:16], net.IPv4(10, 0, 0, 1).To4())
	copy(packet[16:20], net.IPv4(10, 0, 0, 2).To4())

	_, _, _, _, ok := parseIpv4(packet)
	if ok {
		t.Fatalf("expected max-offset tail fragment to be dropped, got ok=true")
	}
}

func TestParseIpv4DropsFragmentWithDfAndReservedBitsSet(t *testing.T) {
	// MF combined with DF and the reserved bit set: the fragment bits (mask
	// 0x3fff) must still trigger the drop regardless of the other two bits.
	packet := make([]byte, 28)
	packet[0] = 0x45
	packet[9] = 17
	binary.BigEndian.PutUint16(packet[2:4], 28)
	binary.BigEndian.PutUint16(packet[6:8], 0xe000) // reserved+DF+MF, offset 0
	copy(packet[12:16], net.IPv4(10, 0, 0, 1).To4())
	copy(packet[16:20], net.IPv4(10, 0, 0, 2).To4())

	_, _, _, _, ok := parseIpv4(packet)
	if ok {
		t.Fatalf("expected fragment with DF/reserved bits also set to be dropped, got ok=true")
	}
}

func TestParseIpv4DropsFragmentReturnsZeroValues(t *testing.T) {
	// When a fragment is dropped, all named return values should be left at
	// their zero values rather than partially populated from the packet.
	packet := make([]byte, 28)
	packet[0] = 0x45
	packet[9] = 17
	binary.BigEndian.PutUint16(packet[2:4], 28)
	binary.BigEndian.PutUint16(packet[6:8], 0x2000) // MF bit set
	copy(packet[12:16], net.IPv4(10, 0, 0, 1).To4())
	copy(packet[16:20], net.IPv4(10, 0, 0, 2).To4())

	ipProtocol, sourceIp, destinationIp, transport, ok := parseIpv4(packet)
	if ok {
		t.Fatalf("expected fragment to be dropped, got ok=true")
	}
	if ipProtocol != IPProtocol(0) {
		t.Fatalf("expected zero-value ipProtocol on drop, got %v", ipProtocol)
	}
	if sourceIp != nil {
		t.Fatalf("expected nil sourceIp on drop, got %v", sourceIp)
	}
	if destinationIp != nil {
		t.Fatalf("expected nil destinationIp on drop, got %v", destinationIp)
	}
	if transport != nil {
		t.Fatalf("expected nil transport on drop, got %v", transport)
	}
}

func TestParseIpv4DropsFragmentBuiltWithWriteIpv4Header(t *testing.T) {
	// Build an otherwise well-formed header using the production
	// writeIpv4Header helper (as used elsewhere in the test suite), then
	// mark it as a non-first fragment to confirm the drop applies even to
	// headers produced through the normal write path.
	packet := MessagePoolGet(Ipv4HeaderSizeWithoutExtensions + UdpHeaderSize)
	defer MessagePoolReturn(packet)
	writeIpv4Header(packet, IP_PROTOCOL_UDP, net.IPv4(10, 0, 0, 1).To4(), net.IPv4(10, 0, 0, 2).To4())
	binary.BigEndian.PutUint16(packet[6:8], 0x0005) // MF clear, offset=5

	_, _, _, _, ok := parseIpv4(packet)
	if ok {
		t.Fatalf("expected fragment built via writeIpv4Header to be dropped, got ok=true")
	}
}
