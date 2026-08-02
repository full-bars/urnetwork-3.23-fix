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
	packet[0] = 0x45 // version 4, IHL 5 (20 bytes)
	packet[9] = 17   // protocol = UDP
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
