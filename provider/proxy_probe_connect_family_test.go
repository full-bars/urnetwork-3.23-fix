package main

import (
	"bytes"
	"net"
	"testing"
)

// TestSocks5ConnectEncodesBothFamilies (CR finding on this PR): an AAAA-only
// target reached the v4 encoder, which wrote ATYP=0x01 plus a nil To4(), so the
// proxy was asked for 0.0.0.0 instead of the target.
func TestSocks5ConnectEncodesBothFamilies(t *testing.T) {
	v4 := net.ParseIP("93.184.216.34")
	frame := socks5Connect(v4, 443)
	if len(frame) != 10 || frame[3] != 0x01 {
		t.Fatalf("v4 frame = %v (len %d), want ATYP 0x01 and 10 bytes", frame, len(frame))
	}
	if !bytes.Equal(frame[4:8], v4.To4()) {
		t.Fatalf("v4 destination = %v, want %v", frame[4:8], v4.To4())
	}

	v6 := net.ParseIP("2606:2800:220:1:248:1893:25c8:1946")
	frame = socks5Connect(v6, 8443)
	if len(frame) != 22 || frame[3] != 0x04 {
		t.Fatalf("v6 frame = %v (len %d), want ATYP 0x04 and 22 bytes", frame, len(frame))
	}
	if !bytes.Equal(frame[4:20], v6.To16()) {
		t.Fatalf("v6 destination = %v, want %v", frame[4:20], v6.To16())
	}
	if frame[20] != 0x20 || frame[21] != 0xfb {
		t.Fatalf("port bytes = %#x %#x, want 0x20 0xfb for 8443", frame[20], frame[21])
	}
	// The defect this pins: the old path produced an all-zero v4 destination.
	if bytes.Equal(frame[4:8], []byte{0, 0, 0, 0}) {
		t.Fatal("v6 target was encoded as an all-zero v4 destination")
	}
}
