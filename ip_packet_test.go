package connect

import (
	"encoding/binary"
	"net"
	"testing"
)

func TestChecksumAddAndFinish(t *testing.T) {
	sum := checksumAdd(0, []byte{0x00, 0x01})
	result := checksumFinish(sum)
	if result != 0xfffe {
		t.Fatalf("checksum of {0x00, 0x01}: want 0xfffe, got 0x%x", result)
	}

	sum = checksumAdd(0, []byte{0xff, 0xff})
	result = checksumFinish(sum)
	if result != 0 {
		t.Fatalf("checksum of {0xff, 0xff}: want 0x0000, got 0x%x", result)
	}

	sum = checksumAdd(0, []byte{0x12, 0x34, 0x56, 0x78})
	result = checksumFinish(sum)
	if result != 0x9753 {
		t.Fatalf("checksum of multi-word: want 0x9753, got 0x%x", result)
	}
}

func TestTransportChecksumIndependent(t *testing.T) {
	src := net.IPv4(192, 168, 1, 1)
	dst := net.IPv4(10, 0, 0, 1)
	transport := []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}

	cs := transportChecksum(IP_PROTOCOL_TCP, src, dst, transport)
	sum := checksumAdd(0, src)
	sum = checksumAdd(sum, dst)
	sum = checksumAdd(sum, []byte{0, byte(IP_PROTOCOL_TCP)})
	sum = checksumAdd(sum, []byte{0, byte(len(transport))})
	sum = checksumAdd(sum, transport)
	direct := checksumFinish(sum)
	if cs != direct {
		t.Fatalf("transportChecksum mismatch: want 0x%x, got 0x%x", direct, cs)
	}
}

func TestTcpPacketChecksumCoversPayload(t *testing.T) {
	cs := &ConnectionState{
		ipVersion:         4,
		sourceIp:          net.IPv4(10, 0, 0, 1).To4(),
		sourcePort:        40000,
		destinationIp:     net.IPv4(192, 168, 1, 1).To4(),
		destinationPort:   443,
		sendSeq:           100,
		receiveSeq:        200,
		windowSize:        65535,
		enableWindowScale: false,
	}
	packet := cs.tcpPacket([]byte("hello"), 100)
	defer MessagePoolReturn(packet)

	ipHeaderLen := int(packet[0]&0x0f) * 4
	tcpBytes := packet[ipHeaderLen:]

	if transportChecksum(IP_PROTOCOL_TCP, cs.destinationIp, cs.sourceIp, tcpBytes) != 0 {
		t.Fatal("TCP checksum verification failed")
	}
}

func TestSynAckChecksumCoversOptions(t *testing.T) {
	cs := &ConnectionState{
		ipVersion:         4,
		sourceIp:          net.IPv4(10, 0, 0, 1).To4(),
		sourcePort:        40000,
		destinationIp:     net.IPv4(192, 168, 1, 1).To4(),
		destinationPort:   443,
		sendSeq:           100,
		receiveSeq:        200,
		windowSize:        65535,
		enableWindowScale: true,
		windowScale:       7,
	}

	packet, err := cs.SynAck(DefaultMtu)
	if err != nil {
		t.Fatalf("SynAck failed: %v", err)
	}
	defer MessagePoolReturn(packet)

	ipHeaderLen := int(packet[0]&0x0f) * 4
	tcpBytes := packet[ipHeaderLen:]

	if transportChecksum(IP_PROTOCOL_TCP, cs.destinationIp, cs.sourceIp, tcpBytes) != 0 {
		t.Fatal("SynAck TCP checksum verification failed")
	}

	dataOffset := int(tcpBytes[12]>>4) * 4
	// MSS (4) + window scale (3) + NOP pad = 8 option bytes; header 28.
	if dataOffset != 28 {
		t.Fatalf("SynAck data offset: want 28 (MSS+WS options), got %d", dataOffset)
	}
}

func TestConnectionStatePureAck(t *testing.T) {
	cs := &ConnectionState{
		ipVersion:       4,
		sourceIp:        net.IPv4(10, 0, 0, 1),
		sourcePort:      40000,
		destinationIp:   net.IPv4(192, 168, 1, 1),
		destinationPort: 443,
		sendSeq:         2000,
		receiveSeq:      1500,
		windowSize:      65535,
	}

	packet, err := cs.PureAck()
	if err != nil {
		t.Fatalf("PureAck failed: %v", err)
	}
	defer MessagePoolReturn(packet)

	ipHeaderLen := int(packet[0]&0x0f) * 4
	tcpBytes := packet[ipHeaderLen:]

	if tcpBytes[13]&tcpFlagAck == 0 {
		t.Fatal("ACK flag not set")
	}
	if tcpBytes[13]&tcpFlagSyn != 0 {
		t.Fatal("SYN flag should not be set on PureAck")
	}

	if transportChecksum(IP_PROTOCOL_TCP, cs.destinationIp, cs.sourceIp, tcpBytes) != 0 {
		t.Fatalf("PureAck TCP checksum verification failed: stored=0x%x",
			binary.BigEndian.Uint16(tcpBytes[16:18]))
	}
}

func TestConnectionStateRstAck(t *testing.T) {
	cs := &ConnectionState{
		ipVersion:       4,
		sourceIp:        net.IPv4(10, 0, 0, 1),
		sourcePort:      40000,
		destinationIp:   net.IPv4(192, 168, 1, 1),
		destinationPort: 443,
		sendSeq:         2000,
		receiveSeq:      1500,
		windowSize:      65535,
	}

	packet, err := cs.RstAck()
	if err != nil {
		t.Fatalf("RstAck failed: %v", err)
	}
	defer MessagePoolReturn(packet)

	ipHeaderLen := int(packet[0]&0x0f) * 4
	tcpBytes := packet[ipHeaderLen:]

	if tcpBytes[13]&tcpFlagRst == 0 {
		t.Fatal("RST flag not set")
	}
	if tcpBytes[13]&tcpFlagAck == 0 {
		t.Fatal("ACK flag not set")
	}

	if transportChecksum(IP_PROTOCOL_TCP, cs.destinationIp, cs.sourceIp, tcpBytes) != 0 {
		t.Fatalf("RstAck TCP checksum verification failed: stored=0x%x",
			binary.BigEndian.Uint16(tcpBytes[16:18]))
	}
}

func TestParseIpv4RoundTrip(t *testing.T) {
	payload := []byte{0x01, 0x02, 0x03, 0x04}
	packet := MessagePoolGet(Ipv4HeaderSizeWithoutExtensions + UdpHeaderSize + len(payload))
	writeIpv4Header(packet, IP_PROTOCOL_UDP, net.IPv4(10, 0, 0, 1).To4(), net.IPv4(192, 168, 1, 1).To4())
	udp := packet[Ipv4HeaderSizeWithoutExtensions:]
	binary.BigEndian.PutUint16(udp[0:2], 12345)
	binary.BigEndian.PutUint16(udp[2:4], 80)
	binary.BigEndian.PutUint16(udp[4:6], uint16(UdpHeaderSize+len(payload)))
	binary.BigEndian.PutUint16(udp[6:8], 0)
	copy(udp[UdpHeaderSize:], payload)
	cs := checksumFinish(checksumAdd(0, udp))
	binary.BigEndian.PutUint16(udp[6:8], cs)
	defer MessagePoolReturn(packet)

	ipProtocol, srcIP, dstIP, transport, ok := parseIpv4(packet)
	if !ok {
		t.Fatal("parseIpv4 failed")
	}
	if ipProtocol != IP_PROTOCOL_UDP {
		t.Fatalf("protocol: want UDP(17), got %d", ipProtocol)
	}
	if !srcIP.Equal(net.IPv4(10, 0, 0, 1)) {
		t.Fatalf("source IP mismatch: %v", srcIP)
	}
	if !dstIP.Equal(net.IPv4(192, 168, 1, 1)) {
		t.Fatalf("dest IP mismatch: %v", dstIP)
	}
	var udpPkt parsedUdp
	if !parseUdpPacket(srcIP, dstIP, transport, &udpPkt) {
		t.Fatal("parseUdpPacket failed")
	}
	if udpPkt.sourcePort != 12345 {
		t.Fatalf("UDP src port: want 12345, got %d", udpPkt.sourcePort)
	}
	if udpPkt.destinationPort != 80 {
		t.Fatalf("UDP dst port: want 80, got %d", udpPkt.destinationPort)
	}
}

func TestParseIpv6RoundTrip(t *testing.T) {
	payload := []byte{0x05, 0x06}
	src := net.ParseIP("2001:db8::1")
	dst := net.ParseIP("2001:db8::2")
	packet := MessagePoolGet(Ipv6HeaderSize + UdpHeaderSize + len(payload))
	writeIpv6Header(packet, IP_PROTOCOL_UDP, src, dst)
	udp := packet[Ipv6HeaderSize:]
	binary.BigEndian.PutUint16(udp[0:2], 33445)
	binary.BigEndian.PutUint16(udp[2:4], 53)
	binary.BigEndian.PutUint16(udp[4:6], uint16(UdpHeaderSize+len(payload)))
	binary.BigEndian.PutUint16(udp[6:8], 0)
	copy(udp[UdpHeaderSize:], payload)
	cs := checksumFinish(checksumAdd(0, udp))
	binary.BigEndian.PutUint16(udp[6:8], cs)
	defer MessagePoolReturn(packet)

	ipProtocol, srcIP, dstIP, transport, ok := parseIpv6(packet)
	if !ok {
		t.Fatal("parseIpv6 failed")
	}
	if ipProtocol != IP_PROTOCOL_UDP {
		t.Fatalf("protocol: want UDP(17), got %d", ipProtocol)
	}
	if !srcIP.Equal(src) {
		t.Fatalf("source IP mismatch: %v", srcIP)
	}
	if !dstIP.Equal(dst) {
		t.Fatalf("dest IP mismatch: %v", dstIP)
	}
	var udpPkt parsedUdp
	if !parseUdpPacket(srcIP, dstIP, transport, &udpPkt) {
		t.Fatal("parseUdpPacket failed")
	}
	if udpPkt.sourcePort != 33445 {
		t.Fatalf("UDP src port: want 33445, got %d", udpPkt.sourcePort)
	}
	if udpPkt.destinationPort != 53 {
		t.Fatalf("UDP dst port: want 53, got %d", udpPkt.destinationPort)
	}
}

func TestParseUdpRoundTrip(t *testing.T) {
	payload := []byte("hello udp")
	src := net.IPv4(10, 0, 0, 1)
	dst := net.IPv4(192, 168, 1, 1)
	packet := MessagePoolGet(Ipv4HeaderSizeWithoutExtensions + UdpHeaderSize + len(payload))
	writeIpv4Header(packet, IP_PROTOCOL_UDP, src.To4(), dst.To4())
	udp := packet[Ipv4HeaderSizeWithoutExtensions:]
	binary.BigEndian.PutUint16(udp[0:2], 40000)
	binary.BigEndian.PutUint16(udp[2:4], 53)
	binary.BigEndian.PutUint16(udp[4:6], uint16(UdpHeaderSize+len(payload)))
	copy(udp[UdpHeaderSize:], payload)
	cs := transportChecksum(IP_PROTOCOL_UDP, src, dst, udp)
	binary.BigEndian.PutUint16(udp[6:8], cs)
	defer MessagePoolReturn(packet)

	var udpPkt parsedUdp
	ipProtocol, _, _, transport, ok := parseIpv4(packet)
	if !ok {
		t.Fatal("parseIpv4 failed")
	}
	if ipProtocol != IP_PROTOCOL_UDP {
		t.Fatalf("protocol: want UDP, got %d", ipProtocol)
	}
	if !parseUdpPacket(nil, nil, transport, &udpPkt) {
		t.Fatal("parseUdpPacket failed")
	}
	if udpPkt.sourcePort != 40000 {
		t.Fatalf("UDP src port: want 40000, got %d", udpPkt.sourcePort)
	}
	if udpPkt.destinationPort != 53 {
		t.Fatalf("UDP dst port: want 53, got %d", udpPkt.destinationPort)
	}
}

func TestParseTcpRoundTrip(t *testing.T) {
	payload := []byte("hello tcp")
	src := net.IPv4(10, 0, 0, 1)
	dst := net.IPv4(192, 168, 1, 1)
	packet := MessagePoolGet(Ipv4HeaderSizeWithoutExtensions + TcpHeaderSizeWithoutExtensions + len(payload))
	writeIpv4Header(packet, IP_PROTOCOL_TCP, src.To4(), dst.To4())
	tcp := packet[Ipv4HeaderSizeWithoutExtensions:]
	writeTcpHeader(tcp, 40000, 80, 100, 200, tcpFlagSyn|tcpFlagAck, 65535, nil)
	copy(tcp[TcpHeaderSizeWithoutExtensions:], payload)
	cs := transportChecksum(IP_PROTOCOL_TCP, src, dst, tcp)
	binary.BigEndian.PutUint16(tcp[16:18], cs)
	defer MessagePoolReturn(packet)

	var tcpPkt parsedTcp
	ipProtocol, _, _, transport, ok := parseIpv4(packet)
	if !ok {
		t.Fatal("parseIpv4 failed")
	}
	if ipProtocol != IP_PROTOCOL_TCP {
		t.Fatalf("protocol: want TCP(6), got %d", ipProtocol)
	}
	if !parseTcpPacket(nil, nil, transport, &tcpPkt) {
		t.Fatal("parseTcpPacket failed")
	}
	if tcpPkt.sourcePort != 40000 {
		t.Fatalf("TCP src port: want 40000, got %d", tcpPkt.sourcePort)
	}
	if tcpPkt.destinationPort != 80 {
		t.Fatalf("TCP dst port: want 80, got %d", tcpPkt.destinationPort)
	}
	if tcpPkt.seq != 100 {
		t.Fatalf("TCP seq: want 100, got %d", tcpPkt.seq)
	}
	if !tcpPkt.ack {
		t.Fatal("ACK flag not set")
	}
	if tcpPkt.ackNumber != 200 {
		t.Fatalf("TCP ack: want 200, got %d", tcpPkt.ackNumber)
	}
	if !tcpPkt.syn || !tcpPkt.ack {
		t.Fatal("SYN+ACK flags not set")
	}
}

func TestParseTcpSynWithWindowScale(t *testing.T) {
	src := net.IPv4(10, 0, 0, 1)
	dst := net.IPv4(192, 168, 1, 1)
	optsBytes := make([]byte, 8)
	optsBytes[0] = 3
	optsBytes[1] = 3
	optsBytes[2] = 7
	tcpOptionLen := len(optsBytes)
	packet := MessagePoolGet(Ipv4HeaderSizeWithoutExtensions + TcpHeaderSizeWithoutExtensions + tcpOptionLen)
	writeIpv4Header(packet, IP_PROTOCOL_TCP, src.To4(), dst.To4())
	tcp := packet[Ipv4HeaderSizeWithoutExtensions:]
	writeTcpHeader(tcp, 40000, 80, 100, 200, tcpFlagSyn, 65535, optsBytes)
	fullTcp := packet[Ipv4HeaderSizeWithoutExtensions:]
	cs := transportChecksum(IP_PROTOCOL_TCP, src, dst, fullTcp)
	binary.BigEndian.PutUint16(tcp[16:18], cs)
	defer MessagePoolReturn(packet)

	var tcpPkt parsedTcp
	_, _, _, transport, ok := parseIpv4(packet)
	if !ok {
		t.Fatal("parseIpv4 failed")
	}
	if !parseTcpPacket(nil, nil, transport, &tcpPkt) {
		t.Fatal("parseTcpPacket failed")
	}
	if !tcpPkt.syn {
		t.Fatal("SYN flag not set")
	}
}

func TestParseTcpOptionsWindowScale(t *testing.T) {
	// Table-driven coverage of the unified option parser's window-scale
	// extraction. parseTcpOptions replaced ParseTcpWindowScaleOpts; these
	// cases preserve the legacy parser's behavioral coverage against the
	// new API (nil, empty, NOPs, EOL, unknown options, cap at 14).
	cases := []struct {
		name string
		opts []byte
		want bool
		ws   uint32
	}{
		{"nil", nil, false, 0},
		{"empty", []byte{}, false, 0},
		{"with-window-scale", []byte{3, 3, 7}, true, 7},
		{"capped-at-14", []byte{3, 3, 20}, true, 14},
		{"nop-before", []byte{1, 3, 3, 7}, true, 7},
		{"eol-stops", []byte{0, 3, 3, 7}, false, 0},
		{"eol-after", []byte{3, 3, 5, 0}, true, 5},
		{"unknown-option", []byte{5, 3, 1, 3, 3, 7}, true, 7},
		{"multiple-nops", []byte{1, 1, 1, 3, 3, 3}, true, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tcp := &parsedTcp{options: c.opts}
			parseTcpOptions(tcp)
			if tcp.enableWindowScale != c.want {
				t.Fatalf("enableWindowScale: want %v, got %v", c.want, tcp.enableWindowScale)
			}
			if tcp.windowScale != c.ws {
				t.Fatalf("windowScale: want %d, got %d", c.ws, tcp.windowScale)
			}
		})
	}
}

func TestParseTcpOptionsMssAndTimestamp(t *testing.T) {
	// MSS (kind 2, len 4) and timestamp (kind 8, len 10) extraction.
	tcp := &parsedTcp{options: []byte{
		2, 4, 0x05, 0xb4, // MSS 1460
		1,                                                     // NOP
		8, 10, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x02, // TSval=1 TSecr=2
	}}
	parseTcpOptions(tcp)
	if !tcp.enableMss || tcp.mss != 1460 {
		t.Fatalf("MSS: want enable=1460, got enable=%v mss=%d", tcp.enableMss, tcp.mss)
	}
	if !tcp.enableTimestamp || tcp.timestampValue != 1 || tcp.timestampEcho != 2 {
		t.Fatalf("timestamp: want val=1 echo=2, got enable=%v val=%d echo=%d", tcp.enableTimestamp, tcp.timestampValue, tcp.timestampEcho)
	}
}

func TestParseTcpOptionsMalformedTail(t *testing.T) {
	// A malformed option tail must stop parsing without corrupting the
	// options already extracted (the parse must not panic).
	tcp := &parsedTcp{options: []byte{2, 4, 0x05, 0xb4, 3, 4, 0x07}}
	parseTcpOptions(tcp)
	if !tcp.enableMss || tcp.mss != 1460 {
		t.Fatalf("MSS before malformed tail: want 1460, got %d", tcp.mss)
	}
}

func TestWriteIpv4RoundTrip(t *testing.T) {
	packet := MessagePoolGet(Ipv4HeaderSizeWithoutExtensions + UdpHeaderSize)
	writeIpv4Header(packet, IP_PROTOCOL_UDP, net.IPv4(10, 0, 0, 1).To4(), net.IPv4(192, 168, 1, 1).To4())
	defer MessagePoolReturn(packet)

	_, srcIP, dstIP, _, ok := parseIpv4(packet)
	if !ok {
		t.Fatal("parseIpv4 failed on self-constructed header")
	}
	if !srcIP.Equal(net.IPv4(10, 0, 0, 1)) {
		t.Fatalf("source IP: want 10.0.0.1, got %v", srcIP)
	}
	if !dstIP.Equal(net.IPv4(192, 168, 1, 1)) {
		t.Fatalf("dest IP: want 192.168.1.1, got %v", dstIP)
	}
	// the IPv4 header checksum covers only the 20-byte header, not whatever
	// follows in the allocated buffer (pool buffers aren't guaranteed zeroed,
	// so verifying over the full buffer is flaky depending on pool reuse)
	if checksumFinish(checksumAdd(0, packet[:Ipv4HeaderSizeWithoutExtensions])) != 0 {
		t.Fatalf("IPv4 checksum verification failed")
	}
}

func TestWriteIpv6RoundTrip(t *testing.T) {
	src := net.ParseIP("2001:db8::1")
	dst := net.ParseIP("2001:db8::2")
	packet := MessagePoolGet(Ipv6HeaderSize)
	writeIpv6Header(packet, IP_PROTOCOL_UDP, src, dst)
	defer MessagePoolReturn(packet)

	_, srcIP, dstIP, _, ok := parseIpv6(packet)
	if !ok {
		t.Fatal("parseIpv6 failed on self-constructed header")
	}
	if !srcIP.Equal(src) {
		t.Fatalf("source IP: want 2001:db8::1, got %v", srcIP)
	}
	if !dstIP.Equal(dst) {
		t.Fatalf("dest IP: want 2001:db8::2, got %v", dstIP)
	}
}

func TestWriteTcpRoundTrip(t *testing.T) {
	src := net.IPv4(10, 0, 0, 1)
	dst := net.IPv4(192, 168, 1, 1)
	opts := []byte{3, 3, 7, 0}
	tcpOptionLen := len(opts)
	packet := MessagePoolGet(Ipv4HeaderSizeWithoutExtensions + TcpHeaderSizeWithoutExtensions + tcpOptionLen)
	writeIpv4Header(packet, IP_PROTOCOL_TCP, src.To4(), dst.To4())
	tcp := packet[Ipv4HeaderSizeWithoutExtensions:]
	writeTcpHeader(tcp, 40000, 80, 100, 200, tcpFlagSyn|tcpFlagAck, 65535, opts)
	fullTcp := packet[Ipv4HeaderSizeWithoutExtensions:]
	cs := transportChecksum(IP_PROTOCOL_TCP, src, dst, fullTcp)
	binary.BigEndian.PutUint16(tcp[16:18], cs)
	defer MessagePoolReturn(packet)

	var tcpPkt parsedTcp
	_, _, _, transport, ok := parseIpv4(packet)
	if !ok {
		t.Fatal("parseIpv4 failed")
	}
	if !parseTcpPacket(nil, nil, transport, &tcpPkt) {
		t.Fatal("parseTcpPacket failed")
	}
	if tcpPkt.sourcePort != 40000 {
		t.Fatalf("TCP src port: want 40000, got %d", tcpPkt.sourcePort)
	}
	if tcpPkt.destinationPort != 80 {
		t.Fatalf("TCP dst port: want 80, got %d", tcpPkt.destinationPort)
	}
	if !tcpPkt.syn || !tcpPkt.ack {
		t.Fatal("SYN+ACK flags not set")
	}
}

func TestParseUdpTooShort(t *testing.T) {
	var udp parsedUdp
	if parseUdpPacket(nil, nil, []byte{0x00}, &udp) {
		t.Fatal("expected parseUdpPacket to fail on short buffer")
	}
}

func TestParseTcpTooShort(t *testing.T) {
	var tcp parsedTcp
	if parseTcpPacket(nil, nil, []byte{0x00}, &tcp) {
		t.Fatal("expected parseTcpPacket to fail on short buffer")
	}
}

func TestParseTcpMalformedOptions(t *testing.T) {
	src := net.IPv4(10, 0, 0, 1)
	dst := net.IPv4(192, 168, 1, 1)
	packet := MessagePoolGet(Ipv4HeaderSizeWithoutExtensions + TcpHeaderSizeWithoutExtensions)
	writeIpv4Header(packet, IP_PROTOCOL_TCP, src.To4(), dst.To4())
	tcp := packet[Ipv4HeaderSizeWithoutExtensions:]
	tcp[12] = 0xFF
	tcp[13] = tcpFlagAck
	cs := transportChecksum(IP_PROTOCOL_TCP, src, dst, tcp)
	binary.BigEndian.PutUint16(tcp[16:18], cs)
	defer MessagePoolReturn(packet)

	var tcpPkt parsedTcp
	_, _, _, transport, ok := parseIpv4(packet)
	if !ok {
		t.Fatal("parseIpv4 failed")
	}
	if parseTcpPacket(nil, nil, transport, &tcpPkt) {
		t.Fatal("expected parseTcpPacket to fail with malformed data offset")
	}
}

func TestEmptyPacketGuard(t *testing.T) {
	_, _, _, _, ok := parseIpv4(nil)
	if ok {
		t.Fatal("expected parseIpv4 to fail on nil")
	}
	_, _, _, _, ok = parseIpv4([]byte{})
	if ok {
		t.Fatal("expected parseIpv4 to fail on empty")
	}
	_, _, _, _, ok = parseIpv6(nil)
	if ok {
		t.Fatal("expected parseIpv6 to fail on nil")
	}
	_, _, _, _, ok = parseIpv6([]byte{})
	if ok {
		t.Fatal("expected parseIpv6 to fail on empty")
	}
}

func TestFlowHashIPv4Consistent(t *testing.T) {
	src := net.IPv4(10, 0, 0, 1)
	dst := net.IPv4(192, 168, 1, 1)
	packet := MessagePoolGet(Ipv4HeaderSizeWithoutExtensions + TcpHeaderSizeWithoutExtensions)
	writeIpv4Header(packet, IP_PROTOCOL_TCP, src.To4(), dst.To4())
	tcp := packet[Ipv4HeaderSizeWithoutExtensions:]
	writeTcpHeader(tcp, 40000, 80, 100, 200, tcpFlagSyn, 65535, nil)
	cs := transportChecksum(IP_PROTOCOL_TCP, src, dst, tcp)
	binary.BigEndian.PutUint16(tcp[16:18], cs)
	defer MessagePoolReturn(packet)

	h1 := flowHash(packet)
	h2 := flowHash(packet)
	if h1 != h2 {
		t.Fatalf("flowHash not consistent: %d vs %d", h1, h2)
	}
}

func TestFlowHashIPv4DifferentPort(t *testing.T) {
	src := net.IPv4(10, 0, 0, 1)
	dst := net.IPv4(192, 168, 1, 1)
	packet1 := MessagePoolGet(Ipv4HeaderSizeWithoutExtensions + TcpHeaderSizeWithoutExtensions)
	writeIpv4Header(packet1, IP_PROTOCOL_TCP, src.To4(), dst.To4())
	tcp1 := packet1[Ipv4HeaderSizeWithoutExtensions:]
	writeTcpHeader(tcp1, 40000, 80, 100, 200, tcpFlagSyn, 65535, nil)
	cs1 := transportChecksum(IP_PROTOCOL_TCP, src, dst, tcp1)
	binary.BigEndian.PutUint16(tcp1[16:18], cs1)
	defer MessagePoolReturn(packet1)

	packet2 := MessagePoolGet(Ipv4HeaderSizeWithoutExtensions + TcpHeaderSizeWithoutExtensions)
	writeIpv4Header(packet2, IP_PROTOCOL_TCP, src.To4(), dst.To4())
	tcp2 := packet2[Ipv4HeaderSizeWithoutExtensions:]
	writeTcpHeader(tcp2, 40001, 80, 100, 200, tcpFlagSyn, 65535, nil)
	cs2 := transportChecksum(IP_PROTOCOL_TCP, src, dst, tcp2)
	binary.BigEndian.PutUint16(tcp2[16:18], cs2)
	defer MessagePoolReturn(packet2)

	flowHash(packet1)
	flowHash(packet2)
}

func TestFlowHashIPv6Consistent(t *testing.T) {
	src := net.ParseIP("2001:db8::1")
	dst := net.ParseIP("2001:db8::2")
	packet := MessagePoolGet(Ipv6HeaderSize + TcpHeaderSizeWithoutExtensions)
	writeIpv6Header(packet, IP_PROTOCOL_TCP, src, dst)
	tcp := packet[Ipv6HeaderSize:]
	writeTcpHeader(tcp, 40000, 80, 100, 200, tcpFlagSyn, 65535, nil)
	defer MessagePoolReturn(packet)

	h1 := flowHash(packet)
	h2 := flowHash(packet)
	if h1 != h2 {
		t.Fatalf("flowHash not consistent for IPv6: %d vs %d", h1, h2)
	}
}

func TestFlowHashShortPacket(t *testing.T) {
	if h := flowHash([]byte{0x45}); h != 0 {
		t.Fatalf("expected 0 for short packet, got %d", h)
	}
	if h := flowHash([]byte{}); h != 0 {
		t.Fatalf("expected 0 for empty packet, got %d", h)
	}
}

func TestSynAckDataOffsetMatchesOptions(t *testing.T) {
	cs := &ConnectionState{
		ipVersion:         4,
		sourceIp:          net.IPv4(10, 0, 0, 1).To4(),
		sourcePort:        40000,
		destinationIp:     net.IPv4(192, 168, 1, 1).To4(),
		destinationPort:   443,
		sendSeq:           100,
		receiveSeq:        200,
		windowSize:        65535,
		enableWindowScale: true,
		windowScale:       7,
	}

	packet, err := cs.SynAck(DefaultMtu)
	if err != nil {
		t.Fatalf("SynAck failed: %v", err)
	}
	defer MessagePoolReturn(packet)

	ipHeaderLen := int(packet[0]&0x0f) * 4
	tcpBytes := packet[ipHeaderLen:]
	dataOffsetBytes := int(tcpBytes[12]>>4) * 4
	actualLen := len(tcpBytes)
	if dataOffsetBytes != actualLen {
		t.Fatalf("SynAck: header claims %d bytes but segment is %d bytes", dataOffsetBytes, actualLen)
	}
}

func TestUdpChecksumNonZero(t *testing.T) {
	src := net.IPv4(10, 0, 0, 1)
	dst := net.IPv4(192, 168, 1, 1)
	packet := MessagePoolGet(Ipv4HeaderSizeWithoutExtensions + UdpHeaderSize)
	writeIpv4Header(packet, IP_PROTOCOL_UDP, src.To4(), dst.To4())
	udp := packet[Ipv4HeaderSizeWithoutExtensions:]
	binary.BigEndian.PutUint16(udp[0:2], 40000)
	binary.BigEndian.PutUint16(udp[2:4], 53)
	binary.BigEndian.PutUint16(udp[4:6], UdpHeaderSize)
	binary.BigEndian.PutUint16(udp[6:8], 0)
	cs := transportChecksum(IP_PROTOCOL_UDP, src, dst, udp)
	binary.BigEndian.PutUint16(udp[6:8], cs)
	defer MessagePoolReturn(packet)

	if cs == 0 {
		t.Fatal("UDP checksum should be non-zero for this packet")
	}

	if transportChecksum(IP_PROTOCOL_UDP, src, dst, udp) != 0 {
		t.Fatal("UDP checksum verification failed")
	}
}

func TestUdpChecksumZero(t *testing.T) {
	src := net.IPv4(10, 0, 0, 1)
	dst := net.IPv4(192, 168, 1, 1)
	packet := MessagePoolGet(Ipv4HeaderSizeWithoutExtensions + UdpHeaderSize)
	writeIpv4Header(packet, IP_PROTOCOL_UDP, src.To4(), dst.To4())
	udp := packet[Ipv4HeaderSizeWithoutExtensions:]
	binary.BigEndian.PutUint16(udp[0:2], 40000)
	binary.BigEndian.PutUint16(udp[2:4], 53)
	binary.BigEndian.PutUint16(udp[4:6], UdpHeaderSize)
	binary.BigEndian.PutUint16(udp[6:8], 0)
	defer MessagePoolReturn(packet)

	var udpPkt parsedUdp
	_, _, _, transport, ok := parseIpv4(packet)
	if !ok {
		t.Fatal("parseIpv4 failed")
	}
	if !parseUdpPacket(nil, nil, transport, &udpPkt) {
		t.Fatal("parseUdpPacket failed with zero checksum (RFC 768 allows)")
	}
}

func TestConnectionStateFinAck(t *testing.T) {
	cs := &ConnectionState{
		ipVersion:       4,
		sourceIp:        net.IPv4(10, 0, 0, 1),
		sourcePort:      40000,
		destinationIp:   net.IPv4(192, 168, 1, 1),
		destinationPort: 443,
		sendSeq:         2000,
		receiveSeq:      1500,
		windowSize:      65535,
	}

	packet, err := cs.FinAck()
	if err != nil {
		t.Fatalf("FinAck failed: %v", err)
	}
	defer MessagePoolReturn(packet)

	ipHeaderLen := int(packet[0]&0x0f) * 4
	tcpBytes := packet[ipHeaderLen:]

	if tcpBytes[13]&tcpFlagFin == 0 {
		t.Fatal("FIN flag not set")
	}
	if tcpBytes[13]&tcpFlagAck == 0 {
		t.Fatal("ACK flag not set")
	}

	if transportChecksum(IP_PROTOCOL_TCP, cs.destinationIp, cs.sourceIp, tcpBytes) != 0 {
		t.Fatal("FinAck TCP checksum verification failed")
	}
}

func TestSynAckIPv6(t *testing.T) {
	cs := &ConnectionState{
		ipVersion:         6,
		sourceIp:          net.ParseIP("2001:db8::1"),
		sourcePort:        40000,
		destinationIp:     net.ParseIP("2001:db8::2"),
		destinationPort:   443,
		sendSeq:           100,
		receiveSeq:        200,
		windowSize:        65535,
		enableWindowScale: true,
		windowScale:       7,
	}

	packet, err := cs.SynAck(DefaultMtu)
	if err != nil {
		t.Fatalf("SynAck IPv6 failed: %v", err)
	}
	defer MessagePoolReturn(packet)

	_, srcIP, dstIP, _, ok := parseIpv6(packet)
	if !ok {
		t.Fatal("parseIpv6 failed")
	}
	if !srcIP.Equal(cs.destinationIp) {
		t.Fatalf("IPv6 source IP mismatch: %v", srcIP)
	}
	if !dstIP.Equal(cs.sourceIp) {
		t.Fatalf("IPv6 dest IP mismatch: %v", dstIP)
	}

	if packet[0]>>4 != 6 {
		t.Fatal("not an IPv6 packet")
	}

	tcpBytes := packet[Ipv6HeaderSize:]
	if transportChecksum(IP_PROTOCOL_TCP, cs.destinationIp, cs.sourceIp, tcpBytes) != 0 {
		t.Fatal("IPv6 SynAck TCP checksum verification failed")
	}

	dataOffset := int(tcpBytes[12]>>4) * 4
	actualLen := len(tcpBytes)
	if dataOffset != actualLen {
		t.Fatalf("IPv6 SynAck: header claims %d bytes but segment is %d bytes", dataOffset, actualLen)
	}
}

func TestConnectionStatePureAckIPv6(t *testing.T) {
	cs := &ConnectionState{
		ipVersion:       6,
		sourceIp:        net.ParseIP("2001:db8::1"),
		sourcePort:      40000,
		destinationIp:   net.ParseIP("2001:db8::2"),
		destinationPort: 443,
		sendSeq:         2000,
		receiveSeq:      1500,
		windowSize:      65535,
	}

	packet, err := cs.PureAck()
	if err != nil {
		t.Fatalf("PureAck IPv6 failed: %v", err)
	}
	defer MessagePoolReturn(packet)

	if packet[0]>>4 != 6 {
		t.Fatal("not an IPv6 packet")
	}

	tcpBytes := packet[Ipv6HeaderSize:]
	if tcpBytes[13]&tcpFlagAck == 0 {
		t.Fatal("ACK flag not set")
	}
	if tcpBytes[13]&tcpFlagSyn != 0 {
		t.Fatal("SYN flag should not be set on PureAck")
	}

	if transportChecksum(IP_PROTOCOL_TCP, cs.destinationIp, cs.sourceIp, tcpBytes) != 0 {
		t.Fatal("IPv6 PureAck TCP checksum verification failed")
	}
}

func TestConnectionStateRstAckIPv6(t *testing.T) {
	cs := &ConnectionState{
		ipVersion:       6,
		sourceIp:        net.ParseIP("2001:db8::1"),
		sourcePort:      40000,
		destinationIp:   net.ParseIP("2001:db8::2"),
		destinationPort: 443,
		sendSeq:         2000,
		receiveSeq:      1500,
		windowSize:      65535,
	}

	packet, err := cs.RstAck()
	if err != nil {
		t.Fatalf("RstAck IPv6 failed: %v", err)
	}
	defer MessagePoolReturn(packet)

	if packet[0]>>4 != 6 {
		t.Fatal("not an IPv6 packet")
	}

	tcpBytes := packet[Ipv6HeaderSize:]
	if tcpBytes[13]&tcpFlagRst == 0 {
		t.Fatal("RST flag not set")
	}
	if tcpBytes[13]&tcpFlagAck == 0 {
		t.Fatal("ACK flag not set")
	}

	if transportChecksum(IP_PROTOCOL_TCP, cs.destinationIp, cs.sourceIp, tcpBytes) != 0 {
		t.Fatal("IPv6 RstAck TCP checksum verification failed")
	}
}

func TestConnectionStateFinAckIPv6(t *testing.T) {
	cs := &ConnectionState{
		ipVersion:       6,
		sourceIp:        net.ParseIP("2001:db8::1"),
		sourcePort:      40000,
		destinationIp:   net.ParseIP("2001:db8::2"),
		destinationPort: 443,
		sendSeq:         2000,
		receiveSeq:      1500,
		windowSize:      65535,
	}

	packet, err := cs.FinAck()
	if err != nil {
		t.Fatalf("FinAck IPv6 failed: %v", err)
	}
	defer MessagePoolReturn(packet)

	if packet[0]>>4 != 6 {
		t.Fatal("not an IPv6 packet")
	}

	tcpBytes := packet[Ipv6HeaderSize:]
	if tcpBytes[13]&tcpFlagFin == 0 {
		t.Fatal("FIN flag not set")
	}
	if tcpBytes[13]&tcpFlagAck == 0 {
		t.Fatal("ACK flag not set")
	}

	if transportChecksum(IP_PROTOCOL_TCP, cs.destinationIp, cs.sourceIp, tcpBytes) != 0 {
		t.Fatal("IPv6 FinAck TCP checksum verification failed")
	}
}

func TestTcpPacketIPv6WithPayload(t *testing.T) {
	cs := &ConnectionState{
		ipVersion:         6,
		sourceIp:          net.ParseIP("2001:db8::1"),
		sourcePort:        40000,
		destinationIp:     net.ParseIP("2001:db8::2"),
		destinationPort:   443,
		sendSeq:           100,
		receiveSeq:        200,
		windowSize:        65535,
		enableWindowScale: false,
	}

	payload := []byte("hello ipv6")
	packet := cs.tcpPacket(payload, 100)
	defer MessagePoolReturn(packet)

	if packet[0]>>4 != 6 {
		t.Fatal("not an IPv6 packet")
	}

	_, srcIP, dstIP, _, ok := parseIpv6(packet)
	if !ok {
		t.Fatal("parseIpv6 failed")
	}
	if !srcIP.Equal(cs.destinationIp) {
		t.Fatalf("IPv6 source IP mismatch: %v", srcIP)
	}
	if !dstIP.Equal(cs.sourceIp) {
		t.Fatalf("IPv6 dest IP mismatch: %v", dstIP)
	}

	tcpBytes := packet[Ipv6HeaderSize:]
	if transportChecksum(IP_PROTOCOL_TCP, cs.destinationIp, cs.sourceIp, tcpBytes) != 0 {
		t.Fatal("IPv6 tcpPacket checksum verification failed")
	}
}

func TestSynAckNoWindowScale(t *testing.T) {
	cs := &ConnectionState{
		ipVersion:         4,
		sourceIp:          net.IPv4(10, 0, 0, 1).To4(),
		sourcePort:        40000,
		destinationIp:     net.IPv4(192, 168, 1, 1).To4(),
		destinationPort:   443,
		sendSeq:           100,
		receiveSeq:        200,
		windowSize:        65535,
		enableWindowScale: false,
	}

	packet, err := cs.SynAck(DefaultMtu)
	if err != nil {
		t.Fatalf("SynAck (no WS) failed: %v", err)
	}
	defer MessagePoolReturn(packet)

	ipHeaderLen := int(packet[0]&0x0f) * 4
	tcpBytes := packet[ipHeaderLen:]

	if tcpBytes[13]&(tcpFlagSyn|tcpFlagAck) != (tcpFlagSyn | tcpFlagAck) {
		t.Fatal("SYN+ACK flags not set")
	}

	if transportChecksum(IP_PROTOCOL_TCP, cs.destinationIp, cs.sourceIp, tcpBytes) != 0 {
		t.Fatal("SynAck (no WS) TCP checksum verification failed")
	}

	dataOffset := int(tcpBytes[12]>>4) * 4
	// MSS (4) always advertised; no WS, no TS -> header 24.
	if dataOffset != TcpHeaderSizeWithoutExtensions+4 {
		t.Fatalf("SynAck (no WS) data offset: want %d (MSS only), got %d", TcpHeaderSizeWithoutExtensions+4, dataOffset)
	}
}

func TestDataPacketsSinglePacket(t *testing.T) {
	cs := &ConnectionState{
		ipVersion:         4,
		sourceIp:          net.IPv4(10, 0, 0, 1).To4(),
		sourcePort:        40000,
		destinationIp:     net.IPv4(192, 168, 1, 1).To4(),
		destinationPort:   443,
		sendSeq:           100,
		receiveSeq:        200,
		windowSize:        65535,
		enableWindowScale: false,
	}

	payload := []byte("small")
	packets, err := cs.DataPackets(payload, len(payload), 1500)
	if err != nil {
		t.Fatalf("DataPackets failed: %v", err)
	}
	if len(packets) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(packets))
	}
	defer MessagePoolReturn(packets[0])

	ipHeaderLen := int(packets[0][0]&0x0f) * 4
	tcpBytes := packets[0][ipHeaderLen:]
	if transportChecksum(IP_PROTOCOL_TCP, cs.destinationIp, cs.sourceIp, tcpBytes) != 0 {
		t.Fatal("DataPackets single packet checksum failed")
	}
}

func TestDataPacketsMultiMtu(t *testing.T) {
	cs := &ConnectionState{
		ipVersion:         4,
		sourceIp:          net.IPv4(10, 0, 0, 1).To4(),
		sourcePort:        40000,
		destinationIp:     net.IPv4(192, 168, 1, 1).To4(),
		destinationPort:   443,
		sendSeq:           100,
		receiveSeq:        200,
		windowSize:        65535,
		enableWindowScale: false,
	}

	mtu := 256
	maxPayloadPerPacket := mtu - Ipv4HeaderSizeWithoutExtensions - TcpHeaderSizeWithoutExtensions
	payload := make([]byte, maxPayloadPerPacket*3+1)
	for i := range payload {
		payload[i] = byte(i)
	}

	packets, err := cs.DataPackets(payload, len(payload), mtu)
	if err != nil {
		t.Fatalf("DataPackets failed: %v", err)
	}

	expectedCount := (len(payload) + maxPayloadPerPacket - 1) / maxPayloadPerPacket
	if len(packets) != expectedCount {
		t.Fatalf("expected %d packets for %d bytes at mtu %d, got %d",
			expectedCount, len(payload), mtu, len(packets))
	}
	for i, pkt := range packets {
		defer MessagePoolReturn(pkt)
		ipHeaderLen := int(pkt[0]&0x0f) * 4
		tcpBytes := pkt[ipHeaderLen:]
		if transportChecksum(IP_PROTOCOL_TCP, cs.destinationIp, cs.sourceIp, tcpBytes) != 0 {
			t.Fatalf("DataPackets packet %d checksum failed", i)
		}
	}
}

func TestDataPacketsExactFit(t *testing.T) {
	cs := &ConnectionState{
		ipVersion:         4,
		sourceIp:          net.IPv4(10, 0, 0, 1).To4(),
		sourcePort:        40000,
		destinationIp:     net.IPv4(192, 168, 1, 1).To4(),
		destinationPort:   443,
		sendSeq:           100,
		receiveSeq:        200,
		windowSize:        65535,
		enableWindowScale: false,
	}

	mtu := 256
	maxPayloadPerPacket := mtu - Ipv4HeaderSizeWithoutExtensions - TcpHeaderSizeWithoutExtensions
	payload := make([]byte, maxPayloadPerPacket)

	packets, err := cs.DataPackets(payload, len(payload), mtu)
	if err != nil {
		t.Fatalf("DataPackets failed: %v", err)
	}
	if len(packets) != 1 {
		t.Fatalf("expected 1 packet for exact MTU fit, got %d", len(packets))
	}
	defer MessagePoolReturn(packets[0])

	ipHeaderLen := int(packets[0][0]&0x0f) * 4
	tcpBytes := packets[0][ipHeaderLen:]
	if transportChecksum(IP_PROTOCOL_TCP, cs.destinationIp, cs.sourceIp, tcpBytes) != 0 {
		t.Fatal("DataPackets exact-fit checksum failed")
	}
}

func TestDataPacketsIPv6(t *testing.T) {
	cs := &ConnectionState{
		ipVersion:         6,
		sourceIp:          net.ParseIP("2001:db8::1"),
		sourcePort:        40000,
		destinationIp:     net.ParseIP("2001:db8::2"),
		destinationPort:   443,
		sendSeq:           100,
		receiveSeq:        200,
		windowSize:        65535,
		enableWindowScale: false,
	}

	payload := make([]byte, 100)
	packets, err := cs.DataPackets(payload, len(payload), 1500)
	if err != nil {
		t.Fatalf("DataPackets IPv6 failed: %v", err)
	}
	if len(packets) != 1 {
		t.Fatalf("expected 1 packet for IPv6, got %d", len(packets))
	}
	defer MessagePoolReturn(packets[0])

	if packets[0][0]>>4 != 6 {
		t.Fatal("not an IPv6 packet")
	}

	tcpBytes := packets[0][Ipv6HeaderSize:]
	if transportChecksum(IP_PROTOCOL_TCP, cs.destinationIp, cs.sourceIp, tcpBytes) != 0 {
		t.Fatal("DataPackets IPv6 checksum failed")
	}
}

func TestTcpPacketEmptyPayload(t *testing.T) {
	cs := &ConnectionState{
		ipVersion:         4,
		sourceIp:          net.IPv4(10, 0, 0, 1).To4(),
		sourcePort:        40000,
		destinationIp:     net.IPv4(192, 168, 1, 1).To4(),
		destinationPort:   443,
		sendSeq:           100,
		receiveSeq:        200,
		windowSize:        65535,
		enableWindowScale: false,
	}

	packet := cs.tcpPacket(nil, 100)
	defer MessagePoolReturn(packet)

	ipHeaderLen := int(packet[0]&0x0f) * 4
	tcpBytes := packet[ipHeaderLen:]
	if len(tcpBytes) != TcpHeaderSizeWithoutExtensions {
		t.Fatalf("empty payload tcpPacket: want %d bytes, got %d",
			TcpHeaderSizeWithoutExtensions, len(tcpBytes))
	}

	if transportChecksum(IP_PROTOCOL_TCP, cs.destinationIp, cs.sourceIp, tcpBytes) != 0 {
		t.Fatal("empty payload tcpPacket checksum failed")
	}
}

func TestFlowHashUdp(t *testing.T) {
	src := net.IPv4(10, 0, 0, 1)
	dst := net.IPv4(192, 168, 1, 1)
	packet := MessagePoolGet(Ipv4HeaderSizeWithoutExtensions + UdpHeaderSize)
	writeIpv4Header(packet, IP_PROTOCOL_UDP, src.To4(), dst.To4())
	udp := packet[Ipv4HeaderSizeWithoutExtensions:]
	binary.BigEndian.PutUint16(udp[0:2], 40000)
	binary.BigEndian.PutUint16(udp[2:4], 53)
	binary.BigEndian.PutUint16(udp[4:6], UdpHeaderSize)
	binary.BigEndian.PutUint16(udp[6:8], 0)
	defer MessagePoolReturn(packet)

	h := flowHash(packet)
	if h == 0 {
		t.Fatal("flowHash returned 0 for valid UDP packet")
	}
	h2 := flowHash(packet)
	if h != h2 {
		t.Fatal("flowHash not consistent for UDP")
	}
}

func TestTransportChecksumIPv6(t *testing.T) {
	src := net.ParseIP("2001:db8::1")
	dst := net.ParseIP("2001:db8::2")
	transport := make([]byte, TcpHeaderSizeWithoutExtensions)

	cs := transportChecksum(IP_PROTOCOL_TCP, src, dst, transport)
	if cs == 0 {
		t.Fatal("expected non-zero transport checksum for IPv6 with zeroed transport")
	}

	cs2 := transportChecksum(IP_PROTOCOL_TCP, src, dst, transport)
	if cs != cs2 {
		t.Fatal("transportChecksum IPv6 not consistent")
	}
}

func TestParseIpv4TooShort(t *testing.T) {
	_, _, _, _, ok := parseIpv4(make([]byte, 19))
	if ok {
		t.Fatal("expected parseIpv4 to fail on 19-byte buffer")
	}

	_, _, _, _, ok = parseIpv4(make([]byte, 18))
	if ok {
		t.Fatal("expected parseIpv4 to fail on 18-byte buffer")
	}

	_, _, _, _, ok = parseIpv4(make([]byte, 21))
	if ok {
		t.Fatal("expected parseIpv4 to fail on 21-byte with header=24 (bogus ihl)")
	}
}

func TestParseIpv4InvalidLength(t *testing.T) {
	buf := make([]byte, Ipv4HeaderSizeWithoutExtensions+UdpHeaderSize)
	buf[0] = 0x45
	binary.BigEndian.PutUint16(buf[2:4], uint16(Ipv4HeaderSizeWithoutExtensions-1))
	_, _, _, _, ok := parseIpv4(buf)
	if ok {
		t.Fatal("expected parseIpv4 to fail when total length < header length")
	}

	binary.BigEndian.PutUint16(buf[2:4], uint16(Ipv4HeaderSizeWithoutExtensions+UdpHeaderSize+1))
	_, _, _, _, ok = parseIpv4(buf)
	if ok {
		t.Fatal("expected parseIpv4 to fail when total length > buffer length")
	}
}

func TestParseIpv6TooShort(t *testing.T) {
	_, _, _, _, ok := parseIpv6(make([]byte, 39))
	if ok {
		t.Fatal("expected parseIpv6 to fail on 39-byte buffer")
	}

	_, _, _, _, ok = parseIpv6(make([]byte, 0))
	if ok {
		t.Fatal("expected parseIpv6 to fail on empty buffer")
	}
}

func TestWriteIpv4HeaderTo4Required(t *testing.T) {
	src := net.IPv4(10, 0, 0, 1)
	dst := net.IPv4(192, 168, 1, 1)

	packet := make([]byte, Ipv4HeaderSizeWithoutExtensions+UdpHeaderSize)
	writeIpv4Header(packet, IP_PROTOCOL_UDP, src.To4(), dst.To4())

	if checksumFinish(checksumAdd(0, packet)) != 0 {
		t.Fatal("IPv4 checksum verification failed with To4() IPs")
	}

	_, srcIP, dstIP, _, ok := parseIpv4(packet)
	if !ok {
		t.Fatal("parseIpv4 failed on header built with To4() IPs")
	}
	if !srcIP.Equal(src) {
		t.Fatalf("source IP mismatch: %v", srcIP)
	}
	if !dstIP.Equal(dst) {
		t.Fatalf("dest IP mismatch: %v", dstIP)
	}
}

func TestParseTcpWindowScaleCombined(t *testing.T) {
	tests := []struct {
		name      string
		opts      []byte
		wantFound bool
		wantShift uint32
	}{
		{"MSS then WS", []byte{2, 4, 0x05, 0xdc, 3, 3, 7}, true, 7},
		{"SACK permitted then WS", []byte{4, 2, 3, 3, 5}, true, 5},
		{"NOP NOP MSS WS", []byte{1, 1, 2, 4, 0x05, 0xdc, 3, 3, 7}, true, 7},
		{"WS at shift=0", []byte{3, 3, 0}, true, 0},
		{"WS at max=14", []byte{3, 3, 14}, true, 14},
		{"only unknown opts", []byte{5, 3, 1, 7, 3, 1}, false, 0},
		{"short WS (len=2)", []byte{3, 2, 7}, false, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tcp := &parsedTcp{options: tc.opts}
			parseTcpOptions(tcp)
			if tcp.enableWindowScale != tc.wantFound {
				t.Fatalf("found: want %v, got %v", tc.wantFound, tcp.enableWindowScale)
			}
			if tcp.windowScale != tc.wantShift {
				t.Fatalf("shift: want %d, got %d", tc.wantShift, tcp.windowScale)
			}
		})
	}
}

func TestFinAckIPv6ChecksumCoverage(t *testing.T) {
	cs := &ConnectionState{
		ipVersion:       6,
		sourceIp:        net.ParseIP("2001:db8::1"),
		sourcePort:      40000,
		destinationIp:   net.ParseIP("2001:db8::2"),
		destinationPort: 443,
		sendSeq:         2000,
		receiveSeq:      1500,
		windowSize:      65535,
	}

	packet, err := cs.FinAck()
	if err != nil {
		t.Fatalf("FinAck IPv6 failed: %v", err)
	}
	defer MessagePoolReturn(packet)

	tcpBytes := packet[Ipv6HeaderSize:]
	if transportChecksum(IP_PROTOCOL_TCP, cs.destinationIp, cs.sourceIp, tcpBytes) != 0 {
		t.Fatal("FinAck IPv6 checksum verification failed")
	}
}

func TestSeqNumIncrementAcrossDataPackets(t *testing.T) {
	cs := &ConnectionState{
		ipVersion:         4,
		sourceIp:          net.IPv4(10, 0, 0, 1).To4(),
		sourcePort:        40000,
		destinationIp:     net.IPv4(192, 168, 1, 1).To4(),
		destinationPort:   443,
		sendSeq:           100,
		receiveSeq:        200,
		windowSize:        65535,
		enableWindowScale: false,
	}

	mtu := 256
	maxPayloadPerPacket := mtu - Ipv4HeaderSizeWithoutExtensions - TcpHeaderSizeWithoutExtensions
	payload := make([]byte, maxPayloadPerPacket*2)
	for i := range payload {
		payload[i] = byte(i)
	}

	packets, err := cs.DataPackets(payload, len(payload), mtu)
	if err != nil {
		t.Fatalf("DataPackets failed: %v", err)
	}
	if len(packets) < 2 {
		t.Fatalf("expected at least 2 packets, got %d", len(packets))
	}
	for _, pkt := range packets {
		defer MessagePoolReturn(pkt)
	}

	p0tcp := packets[0][Ipv4HeaderSizeWithoutExtensions:]
	p1tcp := packets[1][Ipv4HeaderSizeWithoutExtensions:]

	seq0 := binary.BigEndian.Uint32(p0tcp[4:8])
	seq1 := binary.BigEndian.Uint32(p1tcp[4:8])

	payloadLen0 := len(p0tcp) - TcpHeaderSizeWithoutExtensions
	expectedSeq1 := seq0 + uint32(payloadLen0)
	if seq1 != expectedSeq1 {
		t.Fatalf("seq not incremented by payload length: seq0=%d + payload=%d, got seq1=%d",
			seq0, payloadLen0, seq1)
	}
}

func TestSynAckWithTimestamp(t *testing.T) {
	// Timestamp option (kind 8, len 10) layout: MSS(4) + TS(10) padded to 16
	// option bytes -> header 36 bytes, data offset 9 words; this test
	// sets enableTimestamp=true and verifies the timestamp option.
	cs := &ConnectionState{
		ipVersion:             4,
		sourceIp:              net.IPv4(10, 0, 0, 1).To4(),
		sourcePort:            40000,
		destinationIp:         net.IPv4(192, 168, 1, 1).To4(),
		destinationPort:       443,
		sendSeq:               100,
		receiveSeq:            200,
		windowSize:            65535,
		enableTimestamp:       true,
		timestampRecent:       42,
		timestampValueForTest: func() uint32 { return 7 },
		enableWindowScale:     false,
	}

	packet, err := cs.SynAck(DefaultMtu)
	if err != nil {
		t.Fatalf("SynAck (TS) failed: %v", err)
	}
	defer MessagePoolReturn(packet)

	ipHeaderLen := int(packet[0]&0x0f) * 4
	tcpBytes := packet[ipHeaderLen:]

	dataOffset := int(tcpBytes[12]>>4) * 4
	if dataOffset != 36 {
		t.Fatalf("SynAck (TS) data offset: want 36 (MSS+TS), got %d", dataOffset)
	}

	// Verify the timestamp option bytes: NOP? no — TS starts at offset 4
	// (after MSS), kind 8 len 10, TSval=7, TSecr=42.
	opts := tcpBytes[TcpHeaderSizeWithoutExtensions:dataOffset]
	if opts[0] != 2 || opts[1] != 4 {
		t.Fatalf("MSS option missing: %v", opts[:4])
	}
	if opts[4] != 8 || opts[5] != 10 {
		t.Fatalf("TS option header missing: %v", opts[4:6])
	}
	tsval := binary.BigEndian.Uint32(opts[6:10])
	tsecr := binary.BigEndian.Uint32(opts[10:14])
	if tsval != 7 || tsecr != 42 {
		t.Fatalf("TS val/echo: want 7/42, got %d/%d", tsval, tsecr)
	}

	if transportChecksum(IP_PROTOCOL_TCP, cs.destinationIp, cs.sourceIp, tcpBytes) != 0 {
		t.Fatal("SynAck (TS) TCP checksum verification failed")
	}
}

func TestTcpPacketWithTimestamp(t *testing.T) {
	// tcpPacket must emit NOP-NOP-TSval-TSecr once timestamps are
	// negotiated (RFC 7323 §3.2) — the data-path option layout
	cs := &ConnectionState{
		ipVersion:             4,
		sourceIp:              net.IPv4(10, 0, 0, 1).To4(),
		sourcePort:            40000,
		destinationIp:         net.IPv4(192, 168, 1, 1).To4(),
		destinationPort:       443,
		sendSeq:               2000,
		receiveSeq:            1500,
		windowSize:            65535,
		enableTimestamp:       true,
		timestampRecent:       99,
		timestampValueForTest: func() uint32 { return 11 },
	}
	payload := []byte("hello")
	packet := cs.tcpPacket(payload, cs.receiveSeq)
	defer MessagePoolReturn(packet)

	ipHeaderLen := int(packet[0]&0x0f) * 4
	tcpBytes := packet[ipHeaderLen:]
	dataOffset := int(tcpBytes[12]>>4) * 4
	// 20 header + 12 TS options = 32 bytes.
	if dataOffset != 32 {
		t.Fatalf("tcpPacket data offset: want 32 (TS), got %d", dataOffset)
	}
	opts := tcpBytes[TcpHeaderSizeWithoutExtensions:dataOffset]
	// NOP NOP TS(8,10) TSval=11 TSecr=99
	if opts[0] != 1 || opts[1] != 1 || opts[2] != 8 || opts[3] != 10 {
		t.Fatalf("TS option header wrong: %v", opts[:4])
	}
	tsval := binary.BigEndian.Uint32(opts[4:8])
	tsecr := binary.BigEndian.Uint32(opts[8:12])
	if tsval != 11 || tsecr != 99 {
		t.Fatalf("TS val/echo: want 11/99, got %d/%d", tsval, tsecr)
	}
	if string(packet[ipHeaderLen+dataOffset:]) != "hello" {
		t.Fatal("payload not at expected offset")
	}
	if transportChecksum(IP_PROTOCOL_TCP, cs.destinationIp, cs.sourceIp, tcpBytes) != 0 {
		t.Fatal("tcpPacket checksum verification failed")
	}
}

func TestPureAckWithTimestamp(t *testing.T) {
	// PureAck must include TSopt once negotiated; without it
	// strict stacks can discard and Linux loses RTT samples.
	cs := &ConnectionState{
		ipVersion:             4,
		sourceIp:              net.IPv4(10, 0, 0, 1).To4(),
		sourcePort:            40000,
		destinationIp:         net.IPv4(192, 168, 1, 1).To4(),
		destinationPort:       443,
		sendSeq:               2000,
		receiveSeq:            1500,
		windowSize:            65535,
		enableTimestamp:       true,
		timestampRecent:       5,
		timestampValueForTest: func() uint32 { return 6 },
	}
	packet, err := cs.PureAck()
	if err != nil {
		t.Fatalf("PureAck failed: %v", err)
	}
	defer MessagePoolReturn(packet)

	ipHeaderLen := int(packet[0]&0x0f) * 4
	tcpBytes := packet[ipHeaderLen:]
	dataOffset := int(tcpBytes[12]>>4) * 4
	if dataOffset != 32 {
		t.Fatalf("PureAck data offset: want 32 (TS), got %d", dataOffset)
	}
	if transportChecksum(IP_PROTOCOL_TCP, cs.destinationIp, cs.sourceIp, tcpBytes) != 0 {
		t.Fatal("PureAck checksum verification failed")
	}
}

func TestDataPacketsTimestampSegmentation(t *testing.T) {
	// With timestamps on an IPv6 flow, a full read (ReadBufferByteCount
	// accounts for the TS option) must NOT split into a runt tail segment
	// 1440 MTU - (40 IPv6 + 20 TCP + 12 TS) = 1368 payload.
	cs := &ConnectionState{
		ipVersion:       6,
		sourceIp:        net.ParseIP("2001:db8::1"),
		destinationIp:   net.ParseIP("2001:db8::2"),
		sourcePort:      40000,
		destinationPort: 443,
		enableTimestamp: true,
		peerMss:         1400,
	}
	payload := make([]byte, 1368)
	packets, err := cs.DataPackets(payload, len(payload), DefaultMtu)
	if err != nil {
		t.Fatalf("DataPackets failed: %v", err)
	}
	// One segment: 1368 <= min(1368, peerMss - 12 opts) = 1368.
	if len(packets) != 1 {
		t.Fatalf("want 1 segment (no runt), got %d", len(packets))
	}
}

func TestDataPacketsPeerMssWireBudget(t *testing.T) {
	// With timestamps on, a segment at the peer's MSS
	// must leave room for OUR 12-byte TS option so the wire size stays
	// within the peer's path-MTU budget (no fragmentation/drop).
	// peerMss 1395, IPv4 (20+20 headers) + 12 TS -> data budget = min(
	// 1440-52=1388, 1395-12=1383) = 1383 — the peer term binds, and the
	// option-aware value (1383) differs from data-only (1395) and from
	// unclamped (1388), so this test fails under EITHER a removed clamp or
	// a re-revert to the data-only form (the
	// prior 1460 case was vacuous because the MTU term dominated).
	cs := &ConnectionState{
		ipVersion:       4,
		sourceIp:        net.IPv4(10, 0, 0, 1).To4(),
		destinationIp:   net.IPv4(192, 168, 1, 1).To4(),
		sourcePort:      40000,
		destinationPort: 443,
		enableTimestamp: true,
		peerMss:         1395,
	}
	payload := make([]byte, 1400) // exceeds both budgets; forces splitting
	packets, err := cs.DataPackets(payload, len(payload), DefaultMtu)
	if err != nil {
		t.Fatalf("DataPackets failed: %v", err)
	}
	// Every segment's data payload must be <= 1383 (option-aware peer budget).
	for i, pkt := range packets {
		ipHeaderLen := int(pkt[0]&0x0f) * 4
		dataOffset := int(pkt[ipHeaderLen+12]>>4) * 4
		dataLen := len(pkt) - ipHeaderLen - dataOffset
		if dataLen > 1383 {
			t.Fatalf("segment %d: data %d exceeds option-aware peer budget 1383 (clamp removed or reverted?)", i, dataLen)
		}
	}
}

func TestDataPacketsPeerMssMtuWins(t *testing.T) {
	// Sanity companion: with peerMss 1460 the MTU-derived budget (1388)
	// binds instead (the direction the old vacuous test intended to cover).
	cs := &ConnectionState{
		ipVersion:       4,
		sourceIp:        net.IPv4(10, 0, 0, 1).To4(),
		destinationIp:   net.IPv4(192, 168, 1, 1).To4(),
		sourcePort:      40000,
		destinationPort: 443,
		enableTimestamp: true,
		peerMss:         1460,
	}
	payload := make([]byte, 1460)
	packets, err := cs.DataPackets(payload, len(payload), DefaultMtu)
	if err != nil {
		t.Fatalf("DataPackets failed: %v", err)
	}
	for i, pkt := range packets {
		ipHeaderLen := int(pkt[0]&0x0f) * 4
		dataOffset := int(pkt[ipHeaderLen+12]>>4) * 4
		dataLen := len(pkt) - ipHeaderLen - dataOffset
		if dataLen > 1388 {
			t.Fatalf("segment %d: data %d exceeds MTU-derived budget 1388", i, dataLen)
		}
	}
}
