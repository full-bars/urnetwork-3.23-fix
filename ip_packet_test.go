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
		ipVersion:        4,
		sourceIp:         net.IPv4(10, 0, 0, 1).To4(),
		sourcePort:       40000,
		destinationIp:    net.IPv4(192, 168, 1, 1).To4(),
		destinationPort:  443,
		sendSeq:          100,
		receiveSeq:       200,
		windowSize:       65535,
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

	packet, err := cs.SynAck()
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
	if dataOffset != 24 {
		t.Fatalf("SynAck data offset: want 24, got %d", dataOffset)
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
	cs := transportChecksum(IP_PROTOCOL_UDP, src, dst, udp)
	binary.BigEndian.PutUint16(udp[6:8], cs)
	copy(udp[UdpHeaderSize:], payload)
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

func TestParseTcpWindowScaleOpts_Nil(t *testing.T) {
	found, shift := ParseTcpWindowScaleOpts(nil)
	if found {
		t.Fatal("expected no window scale found for nil options")
	}
	if shift != 0 {
		t.Fatalf("shift: want 0, got %d", shift)
	}
}

func TestParseTcpWindowScaleOpts_Empty(t *testing.T) {
	found, shift := ParseTcpWindowScaleOpts([]byte{})
	if found {
		t.Fatal("expected no window scale for empty options")
	}
	if shift != 0 {
		t.Fatalf("shift: want 0, got %d", shift)
	}
}

func TestParseTcpWindowScaleOpts_WithWindowScale(t *testing.T) {
	opts := []byte{3, 3, 7}
	found, shift := ParseTcpWindowScaleOpts(opts)
	if !found {
		t.Fatal("expected to find window scale")
	}
	if shift != 7 {
		t.Fatalf("window scale: want 7, got %d", shift)
	}
}

func TestParseTcpWindowScaleOpts_WindowScaleCappedAt14(t *testing.T) {
	opts := []byte{3, 3, 20}
	found, shift := ParseTcpWindowScaleOpts(opts)
	if !found {
		t.Fatal("expected to find window scale")
	}
	if shift != 14 {
		t.Fatalf("window scale: want 14 (capped), got %d", shift)
	}
}

func TestParseTcpWindowScaleOpts_NopBeforeWindowScale(t *testing.T) {
	opts := []byte{1, 3, 3, 7}
	found, shift := ParseTcpWindowScaleOpts(opts)
	if !found {
		t.Fatal("expected to find window scale after NOP")
	}
	if shift != 7 {
		t.Fatalf("window scale: want 7, got %d", shift)
	}
}

func TestParseTcpWindowScaleOpts_Eol(t *testing.T) {
	opts := []byte{0, 3, 3, 7}
	found, shift := ParseTcpWindowScaleOpts(opts)
	if found {
		t.Fatal("expected no window scale (EOL stops parsing)")
	}
	if shift != 0 {
		t.Fatalf("shift: want 0, got %d", shift)
	}
}

func TestParseTcpWindowScaleOpts_EolAfterOptions(t *testing.T) {
	opts := []byte{3, 3, 5, 0}
	found, shift := ParseTcpWindowScaleOpts(opts)
	if !found {
		t.Fatal("expected to find window scale before EOL")
	}
	if shift != 5 {
		t.Fatalf("window scale: want 5, got %d", shift)
	}
}

func TestParseTcpWindowScaleOpts_UnknownOption(t *testing.T) {
	opts := []byte{5, 3, 1, 3, 3, 7}
	found, shift := ParseTcpWindowScaleOpts(opts)
	if !found {
		t.Fatal("expected to find window scale after unknown option")
	}
	if shift != 7 {
		t.Fatalf("window scale: want 7, got %d", shift)
	}
}

func TestParseTcpWindowScaleOpts_MultipleNops(t *testing.T) {
	opts := []byte{1, 1, 1, 3, 3, 3}
	found, shift := ParseTcpWindowScaleOpts(opts)
	if !found {
		t.Fatal("expected to find window scale after multiple NOPs")
	}
	if shift != 3 {
		t.Fatalf("window scale: want 3, got %d", shift)
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
	if checksumFinish(checksumAdd(0, packet)) != 0 {
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

	packet, err := cs.SynAck()
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
