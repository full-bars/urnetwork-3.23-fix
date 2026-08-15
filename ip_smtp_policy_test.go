package connect

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

var smtpTestClientHello = []byte{
	0x16, 0x03, 0x01, 0x00, 0x40, // TLS handshake record, 64 bytes.
	0x01, 0x00, 0x00, 0x3c, // ClientHello, 60-byte body.
}

func smtpTestPath(sourcePort int, destinationPort int, sequence uint32) *IpPath {
	return &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.ParseIP("10.0.0.2"),
		SourcePort:      sourcePort,
		DestinationIp:   net.ParseIP("203.0.113.10"),
		DestinationPort: destinationPort,
		SequenceNumber:  sequence,
		Ack:             true,
	}
}

func smtpTestSyn(sourcePort int, destinationPort int, sequence uint32) *IpPath {
	path := smtpTestPath(sourcePort, destinationPort, sequence)
	path.Ack = false
	path.Syn = true
	return path
}

func requireSmtpVerdict(t *testing.T, want smtpEgressVerdict, got smtpEgressVerdict) {
	t.Helper()
	if got != want {
		t.Fatalf("SMTP verdict = %d, want %d", got, want)
	}
}

func TestSmtpPortClassification(t *testing.T) {
	path := smtpTestPath(41000, smtpLocalPort, 1)
	if !smtpRoutesLocally(path) {
		t.Fatal("TCP/25 was not classified as the explicit local route")
	}
	if smtpNeedsEncryptionInspection(path) {
		t.Fatal("TCP/25 entered the encrypted SMTP inspector")
	}

	for _, port := range []int{smtpImplicitTlsPort, smtpStartTlsPort} {
		path.DestinationPort = port
		if smtpRoutesLocally(path) || !smtpNeedsEncryptionInspection(path) {
			t.Fatalf("TCP/%d SMTP classification is wrong", port)
		}
	}

	path.DestinationPort = 443
	if smtpRoutesLocally(path) || smtpNeedsEncryptionInspection(path) {
		t.Fatal("HTTPS was classified as SMTP")
	}
}

func TestSmtp465RequiresFragmentedTlsClientHello(t *testing.T) {
	var guard smtpEgressGuard
	const sourcePort = 41001
	const synSequence = uint32(1000)

	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestSyn(sourcePort, smtpImplicitTlsPort, synSequence), nil,
	))
	first := smtpTestClientHello[:3]
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, synSequence+1), first,
	))
	// An exact retransmission is accepted without advancing the stream.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, synSequence+1), first,
	))
	// An overlapping retransmission supplies the rest of the nine-byte prefix.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, synSequence+2), smtpTestClientHello[1:],
	))
	// Once the prefix is verified, opaque TLS records are no longer inspected.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, synSequence+10), []byte{0xff, 0x00, 0x7f},
	))
}

func TestSmtp465RejectsPlaintextAndLatchesFlow(t *testing.T) {
	var guard smtpEgressGuard
	const sourcePort = 41002
	const synSequence = uint32(2000)

	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestSyn(sourcePort, smtpImplicitTlsPort, synSequence), nil,
	))
	requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, synSequence+1), []byte("EHLO plaintext.example\r\n"),
	))
	// A rejected connection cannot disguise a later segment as a new stream.
	requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, synSequence+1), smtpTestClientHello,
	))
}

func TestSmtp465RejectsMalformedClientHelloPrefixes(t *testing.T) {
	tests := map[string][]byte{
		"application data record": {0x17, 0x03, 0x03, 0x00, 0x40, 0x01, 0x00, 0x00, 0x3c},
		"tls alert not handshake": {0x15, 0x03, 0x03, 0x00, 0x40, 0x01, 0x00, 0x00, 0x3c},
		"non TLS version":         {0x16, 0x02, 0x00, 0x00, 0x40, 0x01, 0x00, 0x00, 0x3c},
		"SSLv3 version":           {0x16, 0x03, 0x00, 0x00, 0x40, 0x01, 0x00, 0x00, 0x3c},
		"oversized record":        {0x16, 0x03, 0x03, 0x40, 0x01, 0x01, 0x00, 0x00, 0x3c},
		"server hello":            {0x16, 0x03, 0x03, 0x00, 0x40, 0x02, 0x00, 0x00, 0x3c},
		"short client hello":      {0x16, 0x03, 0x03, 0x00, 0x40, 0x01, 0x00, 0x00, 0x28},
	}
	for name, prefix := range tests {
		t.Run(name, func(t *testing.T) {
			var guard smtpEgressGuard
			requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
				smtpTestPath(41005, smtpImplicitTlsPort, 3600), prefix,
			))
		})
	}
}

func TestSmtp465AcceptsLargeFirstTlsSegmentWithBoundedPrefix(t *testing.T) {
	var guard smtpEgressGuard
	payload := append(append([]byte{}, smtpTestClientHello...), make([]byte, 4096)...)
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(41003, smtpImplicitTlsPort, 3000), payload,
	))

	guard.stateLock.Lock()
	defer guard.stateLock.Unlock()
	for _, flow := range guard.flows {
		if !flow.secure || len(flow.stream) != smtpTlsClientHelloPrefixBytes {
			t.Fatalf("verified 465 flow retained prefix: secure=%t bytes=%d", flow.secure, len(flow.stream))
		}
	}
}

func TestSmtpSecureFlowValidatesNegotiationRetransmissions(t *testing.T) {
	var guard smtpEgressGuard
	const sequence = uint32(3500)
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(41004, smtpImplicitTlsPort, sequence), smtpTestClientHello,
	))

	// Exact retransmissions and opaque data after the validated prefix remain
	// valid, but the prefix cannot be replaced once the flow is marked secure.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(41004, smtpImplicitTlsPort, sequence), smtpTestClientHello,
	))
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(41004, smtpImplicitTlsPort, sequence+uint32(len(smtpTestClientHello))), []byte{0x17, 0x03, 0x03},
	))
	conflict := append([]byte(nil), smtpTestClientHello...)
	conflict[5] = 0x02
	requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
		smtpTestPath(41004, smtpImplicitTlsPort, sequence), conflict,
	))
}

func TestSmtp587AllowsNegotiationThenRequiresClientHello(t *testing.T) {
	var guard smtpEgressGuard
	const sourcePort = 42001
	const synSequence = uint32(4000)
	sequence := synSequence + 1

	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestSyn(sourcePort, smtpStartTlsPort, synSequence), nil,
	))
	firstEhlo := []byte("eh")
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpStartTlsPort, sequence), firstEhlo,
	))
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpStartTlsPort, sequence), firstEhlo,
	))
	sequence += uint32(len(firstEhlo))
	restEhlo := []byte("lo client.example\r\n")
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpStartTlsPort, sequence), restEhlo,
	))
	sequence += uint32(len(restEhlo))

	startTls := []byte("STARTTLS\r\n")
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpStartTlsPort, sequence), startTls,
	))
	sequence += uint32(len(startTls))

	firstTls := smtpTestClientHello[:4]
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpStartTlsPort, sequence), firstTls,
	))
	// Overlap two verified bytes while completing the ClientHello prefix.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpStartTlsPort, sequence+2), smtpTestClientHello[2:],
	))
	sequence += uint32(len(smtpTestClientHello))

	// AUTH is opaque TLS application data after the ClientHello, not plaintext
	// SMTP, and therefore passes the now-secure connection marker.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpStartTlsPort, sequence), []byte("AUTH PLAIN encrypted"),
	))
}

func TestSmtp587AcceptsStartTlsSplitAcrossTcpSegments(t *testing.T) {
	var guard smtpEgressGuard
	const sourcePort = 42002
	sequence := uint32(4500)
	for _, fragment := range [][]byte{
		[]byte("EHLO client.example\r\nSTA"),
		[]byte("RT"),
		[]byte("TLS\r"),
		[]byte("\n"),
		smtpTestClientHello[:2],
		smtpTestClientHello[2:],
	} {
		requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
			smtpTestPath(sourcePort, smtpStartTlsPort, sequence), fragment,
		))
		sequence += uint32(len(fragment))
	}
	// The split command and ClientHello must leave the flow in the secure,
	// opaque phase rather than continuing to parse TLS records as SMTP text.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpStartTlsPort, sequence), []byte{0x17, 0x03, 0x03},
	))
}

func TestSmtp587RejectsTransactionCommandsBeforeStartTls(t *testing.T) {
	commands := []string{
		"AUTH PLAIN secret\r\n",
		"MAIL FROM:<sender@example.com>\r\n",
		"RCPT TO:<recipient@example.com>\r\n",
		"DATA\r\n",
		"NOOP secret\r\n",
		"VRFY user\r\n",
	}
	for index, command := range commands {
		t.Run(command[:4], func(t *testing.T) {
			var guard smtpEgressGuard
			port := 43000 + index
			requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
				smtpTestPath(port, smtpStartTlsPort, 5000), []byte(command),
			))
		})
	}
}

func TestSmtp587RejectsFragmentedTransactionCommandAtFirstDisallowedPrefix(t *testing.T) {
	commands := []string{"AUTH", "MAIL", "RCPT", "DATA"}
	for index, command := range commands {
		t.Run(command, func(t *testing.T) {
			var guard smtpEgressGuard
			// None of the permitted pre-TLS negotiation commands starts with these
			// bytes, so a segmented transaction command must fail closed before a
			// later segment can carry credentials or message data.
			requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
				smtpTestPath(43500+index, smtpStartTlsPort, 5500),
				[]byte(command[:1]),
			))
		})
	}
}

func TestSmtp587RejectsPlaintextAfterStartTls(t *testing.T) {
	var guard smtpEgressGuard
	negotiation := []byte("EHLO client.example\r\nSTARTTLS\r\n")
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(44001, smtpStartTlsPort, 6000), negotiation,
	))
	requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
		smtpTestPath(44001, smtpStartTlsPort, 6000+uint32(len(negotiation))), []byte("AUTH PLAIN secret\r\n"),
	))
}

func TestSmtp587BoundsNegotiationBuffer(t *testing.T) {
	var guard smtpEgressGuard
	sequence := uint32(6500)
	line := []byte("EHLO client.example\r\n")
	buffered := 0
	for buffered+len(line) <= smtpMaxNegotiationBytes {
		requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
			smtpTestPath(44002, smtpStartTlsPort, sequence), line,
		))
		sequence += uint32(len(line))
		buffered += len(line)
	}
	requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
		smtpTestPath(44002, smtpStartTlsPort, sequence), line,
	))

	guard.stateLock.Lock()
	defer guard.stateLock.Unlock()
	for _, flow := range guard.flows {
		if smtpMaxNegotiationBytes < len(flow.stream) {
			t.Fatalf("587 negotiation buffer retained %d bytes, max %d", len(flow.stream), smtpMaxNegotiationBytes)
		}
	}
}

func TestSmtpGuardRejectsGapsAndConflictingRetransmissions(t *testing.T) {
	t.Run("gap", func(t *testing.T) {
		var guard smtpEgressGuard
		requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
			smtpTestPath(45001, smtpStartTlsPort, 7000), []byte("EH"),
		))
		requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
			smtpTestPath(45001, smtpStartTlsPort, 7003), []byte("LO client\r\n"),
		))
	})

	t.Run("conflicting overlap", func(t *testing.T) {
		var guard smtpEgressGuard
		requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
			smtpTestPath(45002, smtpStartTlsPort, 8000), []byte("EH"),
		))
		requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
			smtpTestPath(45002, smtpStartTlsPort, 8000), []byte("EX"),
		))
	})
}

func TestSmtpGuardFreshSynReplacesTupleState(t *testing.T) {
	var guard smtpEgressGuard
	const sourcePort = 46001
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, 9000), smtpTestClientHello,
	))
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestSyn(sourcePort, smtpImplicitTlsPort, 10000), nil,
	))
	requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, 10001), []byte("plaintext"),
	))
}

func TestSmtpGuardRstClearsTupleState(t *testing.T) {
	var guard smtpEgressGuard
	const sourcePort = 46003
	const sequence = uint32(10500)

	requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, sequence), []byte("plaintext"),
	))
	rstPath := smtpTestPath(sourcePort, smtpImplicitTlsPort, sequence)
	rstPath.Rst = true
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(rstPath, nil))
	// Reusing the tuple after teardown must start from empty state rather than
	// inherit the prior connection's latched rejection.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, sequence), smtpTestClientHello,
	))
}

func TestSmtpGuardNamespacesProviderFlowsBySource(t *testing.T) {
	var guard smtpEgressGuard
	firstSource := NewId()
	secondSource := NewId()
	path := smtpTestPath(46002, smtpImplicitTlsPort, 11000)

	// The first remote client poisons only its own exact tuple.
	requireSmtpVerdict(t, smtpEgressReject, guard.inspectForOwner(
		firstSource,
		path,
		[]byte("plaintext"),
	))
	// A different authenticated client may legitimately reuse the same tunnel
	// address, ports, and sequence number without inheriting that rejection.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspectForOwner(
		secondSource,
		path,
		smtpTestClientHello,
	))
}

func TestSmtpGuardBoundsFlowTable(t *testing.T) {
	var guard smtpEgressGuard
	for index := 0; index < smtpMaxFlowCount+1; index++ {
		requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
			smtpTestSyn(10000+index, smtpImplicitTlsPort, uint32(index+1)), nil,
		))
	}
	guard.stateLock.Lock()
	defer guard.stateLock.Unlock()
	if len(guard.flows) != smtpMaxFlowCount {
		t.Fatalf("SMTP flow table size = %d, want %d", len(guard.flows), smtpMaxFlowCount)
	}
}

func smtpTestTcp4Packet(flags byte, sequence uint32, ack uint32, payload []byte) []byte {
	return smtpTestTcp4PacketToPort(smtpImplicitTlsPort, flags, sequence, ack, payload)
}

func smtpTestTcp4PacketToPort(destinationPort int, flags byte, sequence uint32, ack uint32, payload []byte) []byte {
	sourceIp := net.IPv4(10, 0, 0, 2).To4()
	destinationIp := net.IPv4(203, 0, 113, 10).To4()
	packet := make([]byte, Ipv4HeaderSizeWithoutExtensions+TcpHeaderSizeWithoutExtensions+len(payload))
	writeIpv4Header(packet, IP_PROTOCOL_TCP, sourceIp, destinationIp)
	tcp := packet[Ipv4HeaderSizeWithoutExtensions:]
	binary.BigEndian.PutUint16(tcp[0:2], 47001)
	binary.BigEndian.PutUint16(tcp[2:4], uint16(destinationPort))
	binary.BigEndian.PutUint32(tcp[4:8], sequence)
	binary.BigEndian.PutUint32(tcp[8:12], ack)
	tcp[12] = byte(TcpHeaderSizeWithoutExtensions/4) << 4
	tcp[13] = flags
	binary.BigEndian.PutUint16(tcp[14:16], 65535)
	copy(tcp[TcpHeaderSizeWithoutExtensions:], payload)
	binary.BigEndian.PutUint16(tcp[16:18], transportChecksum(
		IP_PROTOCOL_TCP,
		sourceIp,
		destinationIp,
		tcp,
	))
	return packet
}

func TestTcpRstForSmtpPolicyReject(t *testing.T) {
	packet := smtpTestTcp4Packet(byte(tcpFlagAck|tcpFlagPsh), 12000, 34000, []byte("plaintext"))
	reset := tcpRstForPolicyReject(packet)
	if reset == nil {
		t.Fatal("SMTP policy rejection did not build a TCP reset")
	}
	defer MessagePoolReturn(reset)

	ipProtocol, sourceIp, destinationIp, transport, ok := parseIpv4(reset)
	if !ok || ipProtocol != IP_PROTOCOL_TCP {
		t.Fatal("SMTP policy reset is not valid IPv4/TCP")
	}
	var tcp parsedTcp
	if !parseTcpPacket(sourceIp, destinationIp, transport, &tcp) {
		t.Fatal("could not parse SMTP policy reset")
	}
	if !tcp.rst || tcp.ack || tcp.seq != 34000 {
		t.Fatalf("reset flags/sequence = rst:%t ack:%t seq:%d", tcp.rst, tcp.ack, tcp.seq)
	}
	if !sourceIp.Equal(net.IPv4(203, 0, 113, 10)) || !destinationIp.Equal(net.IPv4(10, 0, 0, 2)) {
		t.Fatalf("reset addresses were not reversed: %s -> %s", sourceIp, destinationIp)
	}
	if tcp.sourcePort != smtpImplicitTlsPort || tcp.destinationPort != 47001 {
		t.Fatalf("reset ports = %d -> %d", tcp.sourcePort, tcp.destinationPort)
	}
	if checksum := transportChecksum(IP_PROTOCOL_TCP, sourceIp, destinationIp, transport); checksum != 0 {
		t.Fatalf("reset TCP checksum verification = %#x, want 0", checksum)
	}

	rstPacket := smtpTestTcp4Packet(byte(tcpFlagRst), 1, 0, nil)
	if secondReset := tcpRstForPolicyReject(rstPacket); secondReset != nil {
		MessagePoolReturn(secondReset)
		t.Fatal("policy reset builder answered a reset with another reset")
	}
}

func smtpTestTcp6PacketToPort(destinationPort int, flags byte, sequence uint32, ack uint32, payload []byte) []byte {
	sourceIp := net.ParseIP("2001:db8::2")
	destinationIp := net.ParseIP("2001:db8::10")
	packet := make([]byte, Ipv6HeaderSize+TcpHeaderSizeWithoutExtensions+len(payload))
	writeIpv6Header(packet, IP_PROTOCOL_TCP, sourceIp, destinationIp)
	tcp := packet[Ipv6HeaderSize:]
	binary.BigEndian.PutUint16(tcp[0:2], 47001)
	binary.BigEndian.PutUint16(tcp[2:4], uint16(destinationPort))
	binary.BigEndian.PutUint32(tcp[4:8], sequence)
	binary.BigEndian.PutUint32(tcp[8:12], ack)
	tcp[12] = byte(TcpHeaderSizeWithoutExtensions/4) << 4
	tcp[13] = flags
	binary.BigEndian.PutUint16(tcp[14:16], 65535)
	copy(tcp[TcpHeaderSizeWithoutExtensions:], payload)
	binary.BigEndian.PutUint16(tcp[16:18], transportChecksum(
		IP_PROTOCOL_TCP,
		sourceIp,
		destinationIp,
		tcp,
	))
	return packet
}

// The reset builder shares the LocalUserNat orphan-reset logic across IP
// versions; pin that the IPv6 header path also reverses addresses and ports
// correctly, since the SMTP guard applies equally to IPv6 flows.
func TestTcpRstForSmtpPolicyRejectIpv6(t *testing.T) {
	packet := smtpTestTcp6PacketToPort(smtpImplicitTlsPort, byte(tcpFlagAck|tcpFlagPsh), 5000, 9000, []byte("plaintext"))
	reset := tcpRstForPolicyReject(packet)
	if reset == nil {
		t.Fatal("SMTP policy rejection did not build an IPv6 TCP reset")
	}
	defer MessagePoolReturn(reset)

	ipProtocol, sourceIp, destinationIp, transport, ok := parseIpv6(reset)
	if !ok || ipProtocol != IP_PROTOCOL_TCP {
		t.Fatal("SMTP policy reset is not valid IPv6/TCP")
	}
	var tcp parsedTcp
	if !parseTcpPacket(sourceIp, destinationIp, transport, &tcp) {
		t.Fatal("could not parse IPv6 SMTP policy reset")
	}
	if !tcp.rst || tcp.ack || tcp.seq != 9000 {
		t.Fatalf("ipv6 reset flags/sequence = rst:%t ack:%t seq:%d", tcp.rst, tcp.ack, tcp.seq)
	}
	if !sourceIp.Equal(net.ParseIP("2001:db8::10")) || !destinationIp.Equal(net.ParseIP("2001:db8::2")) {
		t.Fatalf("ipv6 reset addresses were not reversed: %s -> %s", sourceIp, destinationIp)
	}
	if tcp.sourcePort != smtpImplicitTlsPort || tcp.destinationPort != 47001 {
		t.Fatalf("ipv6 reset ports = %d -> %d", tcp.sourcePort, tcp.destinationPort)
	}
}

// deliverTcpPolicyReset must be a safe no-op when the packet cannot be
// parsed back into a TCP segment (tcpRstForPolicyReject returns nil), and
// must tolerate a nil receive callback (the RemoteUserNatClient contract
// always supplies one, but the helper itself should not assume it).
func TestDeliverTcpPolicyResetNoOpOnUnparseablePacket(t *testing.T) {
	called := false
	deliverTcpPolicyReset(
		func(TransferPath, protocol.ProvideMode, *IpPath, []byte) { called = true },
		SourceId(NewId()),
		protocol.ProvideMode_Network,
		nil,
		nil,
	)
	if called {
		t.Fatal("deliverTcpPolicyReset invoked receive for a nil packet")
	}

	called = false
	deliverTcpPolicyReset(
		func(TransferPath, protocol.ProvideMode, *IpPath, []byte) { called = true },
		SourceId(NewId()),
		protocol.ProvideMode_Network,
		nil,
		[]byte{0x00},
	)
	if called {
		t.Fatal("deliverTcpPolicyReset invoked receive for a truncated, unparseable packet")
	}
}

func TestDeliverTcpPolicyResetToleratesNilReceive(t *testing.T) {
	packet := smtpTestTcp4Packet(byte(tcpFlagAck|tcpFlagPsh), 100, 200, []byte("data"))
	// Must not panic when no receive callback is supplied.
	deliverTcpPolicyReset(nil, SourceId(NewId()), protocol.ProvideMode_Network, nil, packet)
}

// smtpFlowKeyForOwnerPath is the exact-tuple key builder underlying every
// guard decision. Pin its validation and version-namespacing directly,
// since a bug here silently merges or drops unrelated flows.
func TestSmtpFlowKeyForOwnerPath(t *testing.T) {
	t.Run("nil path rejected", func(t *testing.T) {
		if _, ok := smtpFlowKeyForOwnerPath(Id{}, nil); ok {
			t.Fatal("nil IpPath produced a flow key")
		}
	})

	t.Run("non-tcp protocol rejected", func(t *testing.T) {
		path := smtpTestPath(41000, smtpImplicitTlsPort, 1)
		path.Protocol = IpProtocolUdp
		if _, ok := smtpFlowKeyForOwnerPath(Id{}, path); ok {
			t.Fatal("UDP path produced a flow key")
		}
	})

	t.Run("out of range source port rejected", func(t *testing.T) {
		path := smtpTestPath(-1, smtpImplicitTlsPort, 1)
		if _, ok := smtpFlowKeyForOwnerPath(Id{}, path); ok {
			t.Fatal("negative source port produced a flow key")
		}
		path = smtpTestPath(65536, smtpImplicitTlsPort, 1)
		if _, ok := smtpFlowKeyForOwnerPath(Id{}, path); ok {
			t.Fatal("out-of-range source port produced a flow key")
		}
	})

	t.Run("out of range destination port rejected", func(t *testing.T) {
		path := smtpTestPath(41000, -1, 1)
		if _, ok := smtpFlowKeyForOwnerPath(Id{}, path); ok {
			t.Fatal("negative destination port produced a flow key")
		}
		path = smtpTestPath(41000, 65536, 1)
		if _, ok := smtpFlowKeyForOwnerPath(Id{}, path); ok {
			t.Fatal("out-of-range destination port produced a flow key")
		}
	})

	t.Run("unsupported ip version rejected", func(t *testing.T) {
		path := smtpTestPath(41000, smtpImplicitTlsPort, 1)
		path.Version = 5
		if _, ok := smtpFlowKeyForOwnerPath(Id{}, path); ok {
			t.Fatal("unsupported IP version produced a flow key")
		}
	})

	t.Run("valid ipv4 path produces a key", func(t *testing.T) {
		path := smtpTestPath(41000, smtpImplicitTlsPort, 1)
		key, ok := smtpFlowKeyForOwnerPath(Id{}, path)
		if !ok {
			t.Fatal("valid IPv4 path did not produce a flow key")
		}
		if key.ipVersion != 4 || key.sourcePort != 41000 || key.destinationPort != smtpImplicitTlsPort {
			t.Fatalf("unexpected IPv4 key fields: %+v", key)
		}
	})

	t.Run("valid ipv6 path produces a key", func(t *testing.T) {
		path := smtpTestPathIPv6(41000, smtpImplicitTlsPort, 1)
		key, ok := smtpFlowKeyForOwnerPath(Id{}, path)
		if !ok {
			t.Fatal("valid IPv6 path did not produce a flow key")
		}
		if key.ipVersion != 6 || key.sourcePort != 41000 || key.destinationPort != smtpImplicitTlsPort {
			t.Fatalf("unexpected IPv6 key fields: %+v", key)
		}
	})

	t.Run("owner id namespaces the key", func(t *testing.T) {
		path := smtpTestPath(41000, smtpImplicitTlsPort, 1)
		firstOwner := NewId()
		secondOwner := NewId()
		firstKey, ok := smtpFlowKeyForOwnerPath(firstOwner, path)
		if !ok {
			t.Fatal("expected a flow key for the first owner")
		}
		secondKey, ok := smtpFlowKeyForOwnerPath(secondOwner, path)
		if !ok {
			t.Fatal("expected a flow key for the second owner")
		}
		if firstKey == secondKey {
			t.Fatal("distinct owners produced identical flow keys")
		}
	})
}

func smtpTestPathIPv6(sourcePort int, destinationPort int, sequence uint32) *IpPath {
	return &IpPath{
		Version:         6,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.ParseIP("2001:db8::2"),
		SourcePort:      sourcePort,
		DestinationIp:   net.ParseIP("2001:db8::10"),
		DestinationPort: destinationPort,
		SequenceNumber:  sequence,
		Ack:             true,
	}
}

// The guard must enforce the same encryption boundary over IPv6, and an
// IPv6 flow must not collide with an IPv4 flow that happens to reuse the
// same ports and sequence numbers.
func TestSmtpGuardEnforcesEncryptionOverIpv6(t *testing.T) {
	var guard smtpEgressGuard

	requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
		smtpTestPathIPv6(41100, smtpImplicitTlsPort, 100), []byte("EHLO plaintext.example\r\n"),
	))
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPathIPv6(41101, smtpImplicitTlsPort, 200), smtpTestClientHello,
	))

	// An IPv4 flow with the same ports and sequence must not inherit the
	// rejected IPv6 flow's latched state.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(41100, smtpImplicitTlsPort, 100), smtpTestClientHello,
	))
}

// tlsClientHelloStreamPrefix is exercised indirectly through the guard
// elsewhere; these are direct boundary checks on the record-length and
// handshake-length fields it validates.
func TestTlsClientHelloStreamPrefixBoundaries(t *testing.T) {
	buildPrefix := func(versionMinor byte, recordBytes uint16, handshakeBytes uint32) []byte {
		prefix := make([]byte, smtpTlsClientHelloPrefixBytes)
		prefix[0] = 0x16
		prefix[1] = 0x03
		prefix[2] = versionMinor
		binary.BigEndian.PutUint16(prefix[3:5], recordBytes)
		prefix[5] = 0x01
		prefix[6] = byte(handshakeBytes >> 16)
		prefix[7] = byte(handshakeBytes >> 8)
		prefix[8] = byte(handshakeBytes)
		return prefix
	}

	t.Run("minimum handshake body length is valid", func(t *testing.T) {
		valid, complete := tlsClientHelloStreamPrefix(buildPrefix(0x01, 0x40, 41))
		if !valid || !complete {
			t.Fatalf("handshakeBytes=41 valid=%t complete=%t, want true/true", valid, complete)
		}
	})

	t.Run("just below minimum handshake body length is invalid", func(t *testing.T) {
		valid, _ := tlsClientHelloStreamPrefix(buildPrefix(0x01, 0x40, 40))
		if valid {
			t.Fatal("handshakeBytes=40 was accepted, want rejected")
		}
	})

	t.Run("maximum handshake body length is valid", func(t *testing.T) {
		valid, complete := tlsClientHelloStreamPrefix(buildPrefix(0x01, 0x40, 1<<20))
		if !valid || !complete {
			t.Fatalf("handshakeBytes=2^20 valid=%t complete=%t, want true/true", valid, complete)
		}
	})

	t.Run("just above maximum handshake body length is invalid", func(t *testing.T) {
		valid, _ := tlsClientHelloStreamPrefix(buildPrefix(0x01, 0x40, (1<<20)+1))
		if valid {
			t.Fatal("handshakeBytes=2^20+1 was accepted, want rejected")
		}
	})

	t.Run("minimum record length is valid while incomplete", func(t *testing.T) {
		valid, complete := tlsClientHelloStreamPrefix(buildPrefix(0x01, 4, 0)[:5])
		if !valid || complete {
			t.Fatalf("recordBytes=4 (5-byte prefix) valid=%t complete=%t, want true/false", valid, complete)
		}
	})

	t.Run("just below minimum record length is invalid", func(t *testing.T) {
		valid, _ := tlsClientHelloStreamPrefix(buildPrefix(0x01, 3, 0)[:5])
		if valid {
			t.Fatal("recordBytes=3 was accepted, want rejected")
		}
	})

	t.Run("maximum record length is valid while incomplete", func(t *testing.T) {
		valid, complete := tlsClientHelloStreamPrefix(buildPrefix(0x01, 1<<14, 0)[:5])
		if !valid || complete {
			t.Fatalf("recordBytes=2^14 (5-byte prefix) valid=%t complete=%t, want true/false", valid, complete)
		}
	})

	t.Run("just above maximum record length is invalid", func(t *testing.T) {
		valid, _ := tlsClientHelloStreamPrefix(buildPrefix(0x01, (1<<14)+1, 0)[:5])
		if valid {
			t.Fatal("recordBytes=2^14+1 was accepted, want rejected")
		}
	})

	t.Run("legacy record version 0x04 is valid", func(t *testing.T) {
		valid, complete := tlsClientHelloStreamPrefix(buildPrefix(0x04, 0x40, 41))
		if !valid || !complete {
			t.Fatalf("versionMinor=0x04 valid=%t complete=%t, want true/true", valid, complete)
		}
	})

	t.Run("legacy record version 0x05 is invalid", func(t *testing.T) {
		valid, _ := tlsClientHelloStreamPrefix(buildPrefix(0x05, 0x40, 41))
		if valid {
			t.Fatal("versionMinor=0x05 was accepted, want rejected")
		}
	})

	t.Run("empty stream is incomplete but not rejected", func(t *testing.T) {
		valid, complete := tlsClientHelloStreamPrefix(nil)
		if !valid || complete {
			t.Fatalf("empty stream valid=%t complete=%t, want true/false", valid, complete)
		}
	})
}

// Direct pins for the pre-TLS SMTP command vocabulary used by the 587
// negotiation parser: which commands are permitted, argument requirements,
// and case-insensitivity.
func TestCompleteSmtpNegotiationCommand(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		wantOk      bool
		wantCommand smtpCommand
	}{
		{"ehlo requires argument", "EHLO", false, 0},
		{"ehlo with argument", "EHLO client.example", true, smtpCommandEhlo},
		{"helo with argument", "HELO client.example", true, smtpCommandHelo},
		{"lowercase ehlo is accepted", "ehlo client.example", true, smtpCommandEhlo},
		{"mixed case ehlo is accepted", "EhLo client.example", true, smtpCommandEhlo},
		{"tab separates command and argument", "EHLO\tclient.example", true, smtpCommandEhlo},
		{"quit takes no argument", "QUIT", true, smtpCommandQuit},
		{"quit with argument is rejected", "QUIT now", false, 0},
		{"quit with whitespace only argument is accepted", "QUIT   ", true, smtpCommandQuit},
		{"starttls takes no argument", "STARTTLS", true, smtpCommandStartTls},
		{"starttls with argument is rejected", "STARTTLS now", false, 0},
		{"unknown command is rejected", "NOOP", false, 0},
		{"empty line is rejected", "", false, 0},
		{"control character is rejected", "EHLO client\x01example", false, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command, ok := completeSmtpNegotiationCommand([]byte(test.line))
			if ok != test.wantOk {
				t.Fatalf("completeSmtpNegotiationCommand(%q) ok = %t, want %t", test.line, ok, test.wantOk)
			}
			if ok && command != test.wantCommand {
				t.Fatalf("completeSmtpNegotiationCommand(%q) command = %d, want %d", test.line, command, test.wantCommand)
			}
		})
	}
}

// validPartialSmtpNegotiationLine governs whether an as-yet-incomplete line
// (no CRLF observed yet) may still resolve into an allowed command; a wrong
// answer here would let disallowed commands hide in a segment boundary or
// reject a legitimate command that simply arrived split across packets.
func TestValidPartialSmtpNegotiationLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"empty partial line is valid", "", true},
		{"partial prefix of ehlo is valid", "EH", true},
		{"partial prefix is case insensitive", "eh", true},
		{"prefix not matching any command is invalid", "XY", false},
		{"full ehlo without argument yet is valid", "EHLO", true},
		{"ehlo with argument in progress is valid", "EHLO client.exam", true},
		{"quit with no argument yet is valid", "QUIT", true},
		{"quit followed by a non-whitespace argument is invalid", "QUIT now", false},
		{"starttls with trailing space only is valid", "STARTTLS ", true},
		{"starttls followed by an argument is invalid", "STARTTLS x", false},
		{"trailing bare cr is stripped before length check", "EHLO client\r", true},
		{"embedded cr is invalid", "EHLO cli\rent", false},
		{"embedded lf is invalid", "EHLO cli\nent", false},
		{"control character is invalid", "EHLO cli\x01ent", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validPartialSmtpNegotiationLine([]byte(test.line)); got != test.want {
				t.Fatalf("validPartialSmtpNegotiationLine(%q) = %t, want %t", test.line, got, test.want)
			}
		})
	}
}

// validSmtpAscii is the shared character-class guard for both the complete
// and partial line parsers: only tab and the printable ASCII range 0x20-0x7e
// are allowed, closing off control characters and 8-bit bytes as a smuggling
// channel for extended protocol semantics.
func TestValidSmtpAscii(t *testing.T) {
	tests := []struct {
		name  string
		value []byte
		want  bool
	}{
		{"empty is valid", nil, true},
		{"tab is valid", []byte{'\t'}, true},
		{"space boundary is valid", []byte{0x20}, true},
		{"tilde boundary is valid", []byte{0x7e}, true},
		{"just below space is invalid", []byte{0x1f}, false},
		{"del is invalid", []byte{0x7f}, false},
		{"high bit byte is invalid", []byte{0x80}, false},
		{"newline is invalid", []byte{'\n'}, false},
		{"carriage return is invalid", []byte{'\r'}, false},
		{"printable ascii sentence is valid", []byte("EHLO client.example"), true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validSmtpAscii(test.value); got != test.want {
				t.Fatalf("validSmtpAscii(%v) = %t, want %t", test.value, got, test.want)
			}
		})
	}
}

// The 587 line parser rejects any single negotiation line beyond RFC 5321's
// 512-byte limit (510 bytes plus CRLF); pin the exact boundary.
func TestSmtp587RejectsCommandLineExactlyOverLimit(t *testing.T) {
	buildLine := func(totalLineBytes int) []byte {
		// "EHLO " (5 bytes) + filler + CRLF.
		filler := totalLineBytes - len("EHLO ")
		line := append([]byte("EHLO "), bytes.Repeat([]byte("a"), filler)...)
		return append(line, '\r', '\n')
	}

	t.Run("exactly at the limit is accepted", func(t *testing.T) {
		var guard smtpEgressGuard
		requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
			smtpTestPath(47001, smtpStartTlsPort, 12000), buildLine(smtpMaxCommandLineBytes),
		))
	})

	t.Run("one byte over the limit is rejected", func(t *testing.T) {
		var guard smtpEgressGuard
		requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
			smtpTestPath(47002, smtpStartTlsPort, 13000), buildLine(smtpMaxCommandLineBytes+1),
		))
	})
}
