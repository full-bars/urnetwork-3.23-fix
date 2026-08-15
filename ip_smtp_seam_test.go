package connect

import (
	"context"
	"encoding/binary"
	"net"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// Fork-adapted integration seams for the SMTP policy layer. Upstream's
// ip_smtp_integration_test.go pins upstream-only internals (InspectEgress
// interface, multi-client block-action observability, provider return test
// sequences); these tests pin the fork's actual hook points: the basic
// client SendPacket gate, the provider ClientReceive ingress guard, and the
// multi-client SendPacket gate.

type smtpSeamPolicy struct {
	stats  *SecurityPolicyStatsCollector
	result SecurityPolicyResult
	calls  atomic.Int64
}

func (self *smtpSeamPolicy) Stats() *SecurityPolicyStatsCollector {
	return self.stats
}

func (self *smtpSeamPolicy) Inspect(
	_ protocol.ProvideMode,
	_ *IpPath,
	_ []byte,
) (SecurityPolicyResult, error) {
	self.calls.Add(1)
	return self.result, nil
}

func newSmtpSeamBasicClient(
	t *testing.T,
	policy *smtpSeamPolicy,
	receive ReceivePacketFunction,
) *RemoteUserNatClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	clientSettings := DefaultClientSettings()
	clientSettings.SendBufferSettings.SequenceBufferSize = 0
	clientSettings.SendBufferSettings.AckBufferSize = 0
	clientSettings.ReceiveBufferSettings.SequenceBufferSize = 0
	clientSettings.ForwardBufferSettings.SequenceBufferSize = 0
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	remote := NewRemoteUserNatClient(client, receive, nil, protocol.ProvideMode_Network)
	remote.securityPolicy = policy
	t.Cleanup(func() {
		remote.Close()
		client.Cancel()
		cancel()
	})
	return remote
}

func newSmtpSeamProvider(
	t *testing.T,
	policy *smtpSeamPolicy,
) *RemoteUserNatProvider {
	provider, _ := newSmtpSeamProviderWithClient(t, policy)
	return provider
}

func newSmtpSeamProviderWithClient(
	t *testing.T,
	policy *smtpSeamPolicy,
) (*RemoteUserNatProvider, *Client) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	clientSettings := DefaultClientSettings()
	clientSettings.SendBufferSettings.SequenceBufferSize = 0
	clientSettings.SendBufferSettings.AckBufferSize = 0
	clientSettings.ReceiveBufferSettings.SequenceBufferSize = 0
	clientSettings.ForwardBufferSettings.SequenceBufferSize = 0
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	localUserNat := NewLocalUserNatWithDefaults(ctx, "smtp-provider-test", nil)
	settings := DefaultRemoteUserNatProviderSettings()
	settings.EgressSecurityPolicyGenerator = func(*SecurityPolicyStatsCollector) SecurityPolicy {
		return policy
	}
	provider := NewRemoteUserNatProvider(client, localUserNat, nil, settings)
	t.Cleanup(func() {
		provider.Close()
		localUserNat.Close()
		client.Cancel()
		cancel()
	})
	return provider, client
}

func sendSmtpSeamProviderPacket(
	t *testing.T,
	provider *RemoteUserNatProvider,
	source TransferPath,
	packet []byte,
) {
	t.Helper()
	pooled := MessagePoolCopy(packet)
	frame, err := ToFrame(&protocol.IpPacketToProvider{
		IpPacket: &protocol.IpPacket{PacketBytes: pooled},
	}, DefaultProtocolVersion)
	if err != nil {
		MessagePoolReturn(pooled)
		t.Fatal(err)
	}
	provider.ClientReceive(
		source,
		[]*protocol.Frame{frame},
		Peer{ProvideMode: protocol.ProvideMode_Public},
	)
	MessagePoolReturn(pooled)
}

func newSmtpSeamMulti(
	t *testing.T,
	policy *smtpSeamPolicy,
	receive ReceivePacketFunction,
) *RemoteUserNatMultiClient {
	t.Helper()
	generator := &TestMultiClientGenerator{
		nextDestinations: func(
			count int,
			excludeDestinations []MultiHopId,
			rankMode string,
		) (map[MultiHopId]DestinationStats, error) {
			return nil, nil
		},
		newClientArgs: func() (*MultiClientGeneratorClientArgs, error) {
			return &MultiClientGeneratorClientArgs{ClientId: NewId(), ClientAuth: nil}, nil
		},
		removeClientArgs: func(args *MultiClientGeneratorClientArgs) {},
		removeClientWithArgs: func(client *Client, args *MultiClientGeneratorClientArgs) {
		},
		newClientSettings: func() *ClientSettings {
			return DefaultClientSettings()
		},
		newClient: func(
			ctx context.Context,
			args *MultiClientGeneratorClientArgs,
			clientSettings *ClientSettings,
		) (*Client, error) {
			return NewClient(ctx, args.ClientId, NewNoContractClientOob(), clientSettings), nil
		},
	}
	settings := DefaultMultiClientSettings()
	settings.EgressSecurityPolicyGenerator = func(*SecurityPolicyStatsCollector) SecurityPolicy {
		return policy
	}
	multi := NewRemoteUserNatMultiClient(
		context.Background(),
		generator,
		receive,
		protocol.ProvideMode_Network,
		settings,
	)
	t.Cleanup(func() {
		multi.Close()
	})
	return multi
}

func TestBasicClientPort25RoutesLocallyBeforePolicy(t *testing.T) {
	policy := &smtpSeamPolicy{
		stats:  DefaultSecurityPolicyStatsCollector(),
		result: SecurityPolicyResultDrop,
	}
	remote := newSmtpSeamBasicClient(t, policy, func(
		_ TransferPath,
		_ protocol.ProvideMode,
		_ *IpPath,
		_ []byte,
	) {
	})

	packet := smtpTestTcp4PacketToPort(smtpLocalPort, byte(tcpFlagSyn), 48006, 0, nil)
	if !remote.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, packet, 0) {
		t.Fatal("basic client did not accept TCP/25 on the explicit local route")
	}
	MessagePoolReturn(packet)
	if calls := policy.calls.Load(); calls != 0 {
		t.Fatalf("basic client TCP/25 reached policy %d times, want 0", calls)
	}
}

func TestBasicClientRejectsPlaintext465WithResetBeforePolicy(t *testing.T) {
	policy := &smtpSeamPolicy{
		stats:  DefaultSecurityPolicyStatsCollector(),
		result: SecurityPolicyResultAllow,
	}
	var resetCount atomic.Int64
	remote := newSmtpSeamBasicClient(t, policy, func(
		_ TransferPath,
		_ protocol.ProvideMode,
		_ *IpPath,
		_ []byte,
	) {
		resetCount.Add(1)
	})

	packet := smtpTestTcp4Packet(
		byte(tcpFlagAck|tcpFlagPsh),
		12600,
		34600,
		[]byte("EHLO plaintext.example\r\n"),
	)
	if remote.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, packet, 0) {
		t.Fatal("basic client accepted plaintext TCP/465")
	}
	MessagePoolReturn(packet)
	if calls := policy.calls.Load(); calls != 0 {
		t.Fatalf("basic client plaintext TCP/465 reached policy %d times, want 0", calls)
	}
	if resets := resetCount.Load(); resets != 1 {
		t.Fatalf("basic client plaintext TCP/465 resets = %d, want 1", resets)
	}
}

func TestBasicClientTls465ContinuesToGeneralPolicy(t *testing.T) {
	policy := &smtpSeamPolicy{
		stats:  DefaultSecurityPolicyStatsCollector(),
		result: SecurityPolicyResultDrop,
	}
	var resetCount atomic.Int64
	remote := newSmtpSeamBasicClient(t, policy, func(
		_ TransferPath,
		_ protocol.ProvideMode,
		_ *IpPath,
		_ []byte,
	) {
		resetCount.Add(1)
	})

	packet := smtpTestTcp4Packet(
		byte(tcpFlagAck|tcpFlagPsh),
		14000,
		36000,
		smtpTestClientHello,
	)
	// No destination is configured, so the send fails after policy; the gate
	// must not reset a valid TLS flow before the general policy runs.
	if remote.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, packet, 0) {
		t.Fatal("providerless basic client unexpectedly accepted TCP/465 TLS")
	}
	MessagePoolReturn(packet)
	if calls := policy.calls.Load(); calls != 1 {
		t.Fatalf("basic client TLS TCP/465 reached policy %d times, want 1", calls)
	}
	if resets := resetCount.Load(); resets != 0 {
		t.Fatalf("basic client TLS TCP/465 received %d SMTP resets", resets)
	}
}

func TestBasicClient587StartTlsNegotiationContinuesToPolicy(t *testing.T) {
	policy := &smtpSeamPolicy{
		stats:  DefaultSecurityPolicyStatsCollector(),
		result: SecurityPolicyResultDrop,
	}
	var resetCount atomic.Int64
	remote := newSmtpSeamBasicClient(t, policy, func(
		_ TransferPath,
		_ protocol.ProvideMode,
		_ *IpPath,
		_ []byte,
	) {
		resetCount.Add(1)
	})

	source := SourceId(NewId())
	sequence := uint32(16000)
	packets := [][]byte{
		smtpTestTcp4PacketToPort(smtpStartTlsPort, byte(tcpFlagSyn), sequence, 0, nil),
	}
	sequence += 1
	for _, payload := range [][]byte{
		[]byte("EHLO ios.example\r\n"),
		[]byte("STARTTLS\r\n"),
		smtpTestClientHello,
	} {
		packets = append(packets, smtpTestTcp4PacketToPort(
			smtpStartTlsPort,
			byte(tcpFlagAck|tcpFlagPsh),
			sequence,
			40000,
			payload,
		))
		sequence += uint32(len(payload))
	}
	for _, packet := range packets {
		remote.SendPacket(source, protocol.ProvideMode_Network, packet, 0)
		MessagePoolReturn(packet)
	}
	if calls := policy.calls.Load(); calls != int64(len(packets)) {
		t.Fatalf("basic client valid TCP/587 reached policy %d times, want %d", calls, len(packets))
	}
	if resets := resetCount.Load(); resets != 0 {
		t.Fatalf("basic client valid TCP/587 received %d SMTP resets", resets)
	}
}

func TestProviderRejectsPlaintext465BeforePolicy(t *testing.T) {
	policy := &smtpSeamPolicy{
		stats:  DefaultSecurityPolicyStatsCollector(),
		result: SecurityPolicyResultAllow,
	}
	provider := newSmtpSeamProvider(t, policy)

	sendSmtpSeamProviderPacket(t, provider, SourceId(NewId()), smtpTestTcp4Packet(
		byte(tcpFlagAck|tcpFlagPsh),
		18000,
		38000,
		[]byte("EHLO plaintext.example\r\n"),
	))
	if calls := policy.calls.Load(); calls != 0 {
		t.Fatalf("provider plaintext TCP/465 reached policy %d times, want 0", calls)
	}
}

func TestProviderAllowsTls465ToReachPolicy(t *testing.T) {
	policy := &smtpSeamPolicy{
		stats:  DefaultSecurityPolicyStatsCollector(),
		result: SecurityPolicyResultDrop,
	}
	provider := newSmtpSeamProvider(t, policy)

	sendSmtpSeamProviderPacket(t, provider, SourceId(NewId()), smtpTestTcp4Packet(
		byte(tcpFlagAck|tcpFlagPsh),
		18000,
		38000,
		smtpTestClientHello,
	))
	if calls := policy.calls.Load(); calls != 1 {
		t.Fatalf("provider TLS TCP/465 reached policy %d times, want 1", calls)
	}
}

func TestProviderTunneledPort25DecidedByGeneralPolicy(t *testing.T) {
	policy := &smtpSeamPolicy{
		stats:  DefaultSecurityPolicyStatsCollector(),
		result: SecurityPolicyResultDrop,
	}
	provider := newSmtpSeamProvider(t, policy)

	// The provider SMTP guard is not applicable to TCP/25 (it enforces
	// encryption on 465/587 only); the general reversed policy is what
	// refuses tunneled port 25, exactly once.
	sendSmtpSeamProviderPacket(t, provider, SourceId(NewId()), smtpTestTcp4PacketToPort(
		smtpLocalPort,
		byte(tcpFlagSyn),
		19000,
		0,
		nil,
	))
	if calls := policy.calls.Load(); calls != 1 {
		t.Fatalf("provider tunneled TCP/25 reached policy %d times, want 1", calls)
	}
}

func TestProviderAllows587NegotiationThroughStartTls(t *testing.T) {
	policy := &smtpSeamPolicy{
		stats:  DefaultSecurityPolicyStatsCollector(),
		result: SecurityPolicyResultDrop,
	}
	provider := newSmtpSeamProvider(t, policy)
	source := SourceId(NewId())
	sequence := uint32(19500)
	segments := [][]byte{
		smtpTestTcp4PacketToPort(smtpStartTlsPort, byte(tcpFlagSyn), sequence, 0, nil),
	}
	sequence += 1
	for _, payload := range [][]byte{
		[]byte("EHLO ios.example\r\n"),
		[]byte("STARTTLS\r\n"),
		smtpTestClientHello,
	} {
		segments = append(segments, smtpTestTcp4PacketToPort(
			smtpStartTlsPort,
			byte(tcpFlagAck|tcpFlagPsh),
			sequence,
			39500,
			payload,
		))
		sequence += uint32(len(payload))
	}
	for _, segment := range segments {
		sendSmtpSeamProviderPacket(t, provider, source, segment)
	}
	if calls := policy.calls.Load(); calls != int64(len(segments)) {
		t.Fatalf("provider valid TCP/587 reached policy %d times, want %d", calls, len(segments))
	}

	// A separate flow cannot authenticate before STARTTLS; the rejected
	// segment must never reach the general policy. The AUTH packet uses a
	// fresh source port so the guard starts a new flow instead of treating
	// it as opaque TLS data after the validated ClientHello.
	sendSmtpSeamProviderPacket(t, provider, source, smtpSeamTcp4Packet(
		48007,
		smtpStartTlsPort,
		byte(tcpFlagAck|tcpFlagPsh),
		48008,
		39500,
		[]byte("AUTH PLAIN secret\r\n"),
	))
	if calls := policy.calls.Load(); calls != int64(len(segments)) {
		t.Fatalf("provider plaintext TCP/587 reached policy %d times, want %d", calls, len(segments))
	}
}

// smtpSeamTcp4Packet builds an IPv4/TCP packet with a caller-chosen source
// port, unlike smtpTestTcp4PacketToPort's fixed 47001. A distinct source
// port distinguishes a new flow for the provider guard.
func smtpSeamTcp4Packet(
	sourcePort int,
	destinationPort int,
	flags byte,
	sequence uint32,
	ack uint32,
	payload []byte,
) []byte {
	sourceIp := net.IPv4(10, 0, 0, 2).To4()
	destinationIp := net.IPv4(203, 0, 113, 10).To4()
	packet := make([]byte, Ipv4HeaderSizeWithoutExtensions+TcpHeaderSizeWithoutExtensions+len(payload))
	writeIpv4Header(packet, IP_PROTOCOL_TCP, sourceIp, destinationIp)
	tcp := packet[Ipv4HeaderSizeWithoutExtensions:]
	binary.BigEndian.PutUint16(tcp[0:2], uint16(sourcePort))
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

func TestMultiClientPort25RoutesLocallyBeforePolicy(t *testing.T) {
	policy := &smtpSeamPolicy{
		stats:  DefaultSecurityPolicyStatsCollector(),
		result: SecurityPolicyResultDrop,
	}
	multi := newSmtpSeamMulti(t, policy, func(
		_ TransferPath,
		_ protocol.ProvideMode,
		_ *IpPath,
		_ []byte,
	) {
	})
	defer multi.Close()

	packet := smtpTestTcp4PacketToPort(smtpLocalPort, byte(tcpFlagSyn), 48003, 0, nil)
	if !multi.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, packet, 0) {
		t.Fatal("multi client did not accept TCP/25 on the explicit local route")
	}
	MessagePoolReturn(packet)
	if calls := policy.calls.Load(); calls != 0 {
		t.Fatalf("multi client TCP/25 reached policy %d times, want 0", calls)
	}
}

func TestMultiClientRejectsPlaintext465BeforePolicy(t *testing.T) {
	policy := &smtpSeamPolicy{
		stats:  DefaultSecurityPolicyStatsCollector(),
		result: SecurityPolicyResultAllow,
	}
	var resetCount atomic.Int64
	multi := newSmtpSeamMulti(t, policy, func(
		_ TransferPath,
		_ protocol.ProvideMode,
		_ *IpPath,
		_ []byte,
	) {
		resetCount.Add(1)
	})
	defer multi.Close()

	packet := smtpTestTcp4Packet(
		byte(tcpFlagAck|tcpFlagPsh),
		12000,
		34000,
		[]byte("EHLO plaintext.example\r\n"),
	)
	if multi.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, packet, 0) {
		t.Fatal("multi client accepted plaintext TCP/465")
	}
	MessagePoolReturn(packet)
	if calls := policy.calls.Load(); calls != 0 {
		t.Fatalf("multi client plaintext TCP/465 reached policy %d times, want 0", calls)
	}
	if resets := resetCount.Load(); resets != 1 {
		t.Fatalf("multi client plaintext TCP/465 resets = %d, want 1", resets)
	}
}

func TestMultiClientRejectsAuthBeforeStartTls587(t *testing.T) {
	policy := &smtpSeamPolicy{
		stats:  DefaultSecurityPolicyStatsCollector(),
		result: SecurityPolicyResultAllow,
	}
	var resetCount atomic.Int64
	multi := newSmtpSeamMulti(t, policy, func(
		_ TransferPath,
		_ protocol.ProvideMode,
		_ *IpPath,
		_ []byte,
	) {
		resetCount.Add(1)
	})
	defer multi.Close()

	// AUTH on a fresh flow (distinct source port) must be rejected before the
	// general policy: transaction commands are forbidden before STARTTLS.
	packet := smtpSeamTcp4Packet(
		48009,
		smtpStartTlsPort,
		byte(tcpFlagAck|tcpFlagPsh),
		49000,
		40000,
		[]byte("AUTH PLAIN secret\r\n"),
	)
	if multi.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, packet, 0) {
		t.Fatal("multi client accepted plaintext TCP/587 AUTH")
	}
	MessagePoolReturn(packet)
	if calls := policy.calls.Load(); calls != 0 {
		t.Fatalf("multi client plaintext TCP/587 reached policy %d times, want 0", calls)
	}
	if resets := resetCount.Load(); resets != 1 {
		t.Fatalf("multi client plaintext TCP/587 resets = %d, want 1", resets)
	}
}

// Provider reset delivery is deliberately NOT round-trip pinned here. The
// reset builder is unit-tested (TestTcpRstForSmtpPolicyReject), the guard
// rejection is pinned above, and deliverSmtpPolicyReset mirrors the
// production Receive return path (IpPacketFromProvider + CompanionContract).
// A full delivery assertion needs upstream's installProviderReturnTestSequence
// fixture, which the fork lacks; the fork's bare test client has no return
// transport for frames sent via client.SendWithTimeout.
