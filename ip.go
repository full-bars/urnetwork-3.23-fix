package connect

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	mathrand "math/rand"
	"net"
	"runtime"
	// "syscall"
	// "runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/exp/maps"

	// "google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

var activeConnectionCount int64

func ActiveConnectionCount() int64 {
	return atomic.LoadInt64(&activeConnectionCount)
}

// implements user-space NAT (UNAT) and packet inspection
// The UNAT emulates a raw socket using user-space sockets.

// use 0 for deadlock testing
const DefaultIpBufferSize = 256

const DefaultMtu = 1440
const Ipv4HeaderSizeWithoutExtensions = 20
const Ipv6HeaderSize = 40
const UdpHeaderSize = 8
const TcpHeaderSizeWithoutExtensions = 20

const debugVerifyHeaders = false

type IPProtocol uint8

const (
	IP_PROTOCOL_UDP IPProtocol = 17
	IP_PROTOCOL_TCP IPProtocol = 6
)

type UDPPort uint16
type TCPPort uint16

type parsedUdp struct {
	sourceIp        net.IP
	destinationIp   net.IP
	sourcePort      UDPPort
	destinationPort UDPPort
	payload         []byte
}

type parsedTcp struct {
	sourceIp        net.IP
	destinationIp   net.IP
	sourcePort      TCPPort
	destinationPort TCPPort
	fin             bool
	syn             bool
	rst             bool
	psh             bool
	ack             bool
	seq             uint32
	ackNumber       uint32
	windowSize      uint16
	options         []byte
	enableMss         bool
	mss               uint32
	enableWindowScale bool
	windowScale       uint32
	enableTimestamp   bool
	timestampValue    uint32
	timestampEcho     uint32
	payload           []byte
}

func (self *parsedTcp) flagsString() string {
	flags := []string{}
	if self.fin {
		flags = append(flags, "FIN")
	}
	if self.syn {
		flags = append(flags, "SYN")
	}
	if self.rst {
		flags = append(flags, "RST")
	}
	if self.psh {
		flags = append(flags, "PSH")
	}
	if self.ack {
		flags = append(flags, "ACK")
	}
	return strings.Join(flags, ", ")
}

func parseIpv4(ipPacket []byte) (ipProtocol IPProtocol, sourceIp net.IP, destinationIp net.IP, transport []byte, ok bool) {
	if len(ipPacket) < Ipv4HeaderSizeWithoutExtensions {
		return
	}
	headerByteCount := int(ipPacket[0]&0xf) * 4
	totalByteCount := int(binary.BigEndian.Uint16(ipPacket[2:4]))
	if headerByteCount < Ipv4HeaderSizeWithoutExtensions || totalByteCount < headerByteCount || len(ipPacket) < totalByteCount {
		return
	}
	// fragments are not reassembled: a non-first fragment has no transport
	// header and a first fragment has a truncated payload, so either would
	// misparse payload bytes as transport fields. one 16 bit load covers mf
	// plus the whole offset field (0x3fff); df and the reserved bit pass.
	if binary.BigEndian.Uint16(ipPacket[6:8])&0x3fff != 0 {
		return
	}
	ipProtocol = IPProtocol(ipPacket[9])
	sourceIp = net.IP(ipPacket[12:16])
	destinationIp = net.IP(ipPacket[16:20])
	transport = ipPacket[headerByteCount:totalByteCount]
	ok = true
	return
}

func parseIpv6(ipPacket []byte) (ipProtocol IPProtocol, sourceIp net.IP, destinationIp net.IP, transport []byte, ok bool) {
	if len(ipPacket) < Ipv6HeaderSize {
		return
	}
	payloadByteCount := int(binary.BigEndian.Uint16(ipPacket[4:6]))
	if len(ipPacket) < Ipv6HeaderSize+payloadByteCount {
		return
	}
	ipProtocol = IPProtocol(ipPacket[6])
	sourceIp = net.IP(ipPacket[8:24])
	destinationIp = net.IP(ipPacket[24:40])
	transport = ipPacket[Ipv6HeaderSize : Ipv6HeaderSize+payloadByteCount]
	ok = true
	return
}

func parseUdpPacket(sourceIp net.IP, destinationIp net.IP, transport []byte, udp *parsedUdp) bool {
	if len(transport) < UdpHeaderSize {
		return false
	}
	udpByteCount := int(binary.BigEndian.Uint16(transport[4:6]))
	if udpByteCount < UdpHeaderSize || len(transport) < udpByteCount {
		return false
	}
	udp.sourceIp = sourceIp
	udp.destinationIp = destinationIp
	udp.sourcePort = UDPPort(binary.BigEndian.Uint16(transport[0:2]))
	udp.destinationPort = UDPPort(binary.BigEndian.Uint16(transport[2:4]))
	udp.payload = transport[UdpHeaderSize:udpByteCount]
	return true
}

func parseTcpPacket(sourceIp net.IP, destinationIp net.IP, transport []byte, tcp *parsedTcp) bool {
	if len(transport) < TcpHeaderSizeWithoutExtensions {
		return false
	}
	headerByteCount := int(transport[12]>>4) * 4
	if headerByteCount < TcpHeaderSizeWithoutExtensions || len(transport) < headerByteCount {
		return false
	}
	flags := transport[13]
	tcp.sourceIp = sourceIp
	tcp.destinationIp = destinationIp
	tcp.sourcePort = TCPPort(binary.BigEndian.Uint16(transport[0:2]))
	tcp.destinationPort = TCPPort(binary.BigEndian.Uint16(transport[2:4]))
	tcp.seq = binary.BigEndian.Uint32(transport[4:8])
	tcp.ackNumber = binary.BigEndian.Uint32(transport[8:12])
	tcp.fin = (flags & 0x01) != 0
	tcp.syn = (flags & 0x02) != 0
	tcp.rst = (flags & 0x04) != 0
	tcp.psh = (flags & 0x08) != 0
	tcp.ack = (flags & 0x10) != 0
	tcp.windowSize = binary.BigEndian.Uint16(transport[14:16])
	tcp.options = transport[TcpHeaderSizeWithoutExtensions:headerByteCount]
	parseTcpOptions(tcp)
	tcp.payload = transport[headerByteCount:]
	return true
}

// Extracts the options needed by the synthetic provider endpoint. Unknown
// options remain opaque, and malformed tails stop parsing without turning an
// otherwise valid TCP segment into a malformed IP packet.
func parseTcpOptions(tcp *parsedTcp) {
	tcp.enableMss = false
	tcp.mss = 0
	tcp.enableWindowScale = false
	tcp.windowScale = 0
	tcp.enableTimestamp = false
	tcp.timestampValue = 0
	tcp.timestampEcho = 0
	for optionIndex := 0; optionIndex < len(tcp.options); {
		switch tcp.options[optionIndex] {
		case 0:
			return
		case 1:
			optionIndex += 1
		default:
			if len(tcp.options) < optionIndex+2 {
				return
			}
			optionByteCount := int(tcp.options[optionIndex+1])
			if optionByteCount < 2 || len(tcp.options) < optionIndex+optionByteCount {
				return
			}
			switch tcp.options[optionIndex] {
			case 2:
				if optionByteCount == 4 {
					mss := binary.BigEndian.Uint16(tcp.options[optionIndex+2 : optionIndex+4])
					if mss != 0 {
						tcp.enableMss = true
						tcp.mss = uint32(mss)
					}
				}
			case 3:
				if optionByteCount == 3 {
					tcp.enableWindowScale = true
					tcp.windowScale = min(uint32(tcp.options[optionIndex+2]), 14)
				}
			case 8:
				if optionByteCount == 10 {
					tcp.enableTimestamp = true
					tcp.timestampValue = binary.BigEndian.Uint32(tcp.options[optionIndex+2 : optionIndex+6])
					tcp.timestampEcho = binary.BigEndian.Uint32(tcp.options[optionIndex+6 : optionIndex+10])
				}
			}
			optionIndex += optionByteCount
		}
	}
}

// ParseTcpWindowScaleOpts returns the window scale from a SYN's options.
// Retained for backward compatibility; new code should use parseTcpOptions.
func ParseTcpWindowScaleOpts(opts []byte) (bool, uint32) {
	for i := 0; i < len(opts); {
		kind := opts[i]
		if kind == 0 {
			break
		}
		if kind == 1 {
			i += 1
			continue
		}
		if i+1 >= len(opts) {
			break
		}
		length := opts[i+1]
		if length < 2 || i+int(length) > len(opts) {
			break
		}
		if kind == 3 && length >= 3 {
			shift := uint32(opts[i+2])
			if 14 < shift {
				shift = 14
			}
			return true, shift
		}
		i += int(length)
	}
	return false, 0
}

const (
	tcpFlagFin = byte(0x01)
	tcpFlagSyn = byte(0x02)
	tcpFlagRst = byte(0x04)
	tcpFlagPsh = byte(0x08)
	tcpFlagAck = byte(0x10)
)

func checksumAdd(sum uint32, b []byte) uint32 {
	i := 0
	for ; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i : i+2]))
	}
	if i < len(b) {
		sum += uint32(b[i]) << 8
	}
	return sum
}

func checksumFinish(sum uint32) uint16 {
	for 0xffff < sum {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

func transportChecksum(protocol IPProtocol, packetSourceIp net.IP, packetDestinationIp net.IP, transport []byte) uint16 {
	sum := checksumAdd(0, packetSourceIp)
	sum = checksumAdd(sum, packetDestinationIp)
	sum += uint32(protocol)
	sum += uint32(len(transport))
	return checksumFinish(checksumAdd(sum, transport))
}

func writeIpv4Header(packet []byte, ipProtocol IPProtocol, packetSourceIp net.IP, packetDestinationIp net.IP) {
	packet[0] = 0x45
	packet[1] = 0
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	packet[4] = 0
	packet[5] = 0
	packet[6] = 0
	packet[7] = 0
	packet[8] = 64
	packet[9] = byte(ipProtocol)
	packet[10] = 0
	packet[11] = 0
	copy(packet[12:16], packetSourceIp)
	copy(packet[16:20], packetDestinationIp)
	binary.BigEndian.PutUint16(packet[10:12], checksumFinish(checksumAdd(0, packet[0:Ipv4HeaderSizeWithoutExtensions])))
}

func writeIpv6Header(packet []byte, ipProtocol IPProtocol, packetSourceIp net.IP, packetDestinationIp net.IP) {
	packet[0] = 0x60
	packet[1] = 0
	packet[2] = 0
	packet[3] = 0
	binary.BigEndian.PutUint16(packet[4:6], uint16(len(packet)-Ipv6HeaderSize))
	packet[6] = byte(ipProtocol)
	packet[7] = 64
	copy(packet[8:24], packetSourceIp)
	copy(packet[24:40], packetDestinationIp)
}

// send from a raw socket
// note `ipProtocol` is not supplied. The implementation must do a packet inspection to determine protocol
// `provideMode` is the relationship between the source and this device
type SendPacketFunction func(provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool

// receive into a raw socket
type ReceivePacketFunction func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte)

type UserNatClient interface {
	// `SendPacketFunction`
	SendPacket(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool
	Close()
	Shuffle()

	SecurityPolicyStats(reset bool) SecurityPolicyStats

	// allow traffic that fails the security policy of the peers to stay local
	SetLocalSecurityBypass(localSecurityBypass bool)
}

func DefaultUdpBufferSettings() *UdpBufferSettings {
	return &UdpBufferSettings{
		ReadTimeout:         300 * time.Second,
		WriteTimeout:        15 * time.Second,
		IdleTimeout:         300 * time.Second,
		Mtu:                 DefaultMtu,
		ReadBufferByteCount: DefaultMtu,
		SequenceBufferSize:  DefaultIpBufferSize,
		UserLimit:           0,
		MaxWindowSize:       uint32(mib(4)),
		ConnectSettings:     *DefaultConnectSettings(),
	}
}

func DefaultTcpBufferSettings() *TcpBufferSettings {
	tcpBufferSettings := &TcpBufferSettings{
		// ConnectTimeout:     60 * time.Second,
		ReadTimeout:        300 * time.Second,
		WriteTimeout:       15 * time.Second,
		AckCompressTimeout: time.Duration(0),
		IdleTimeout:        300 * time.Second,
		ScaleDownTimeout:   30 * time.Second,
		SequenceBufferSize: DefaultIpBufferSize,
		Mtu:                DefaultMtu,
		// avoid fragmentation
		ReadBufferByteCount: DefaultMtu - max(Ipv4HeaderSizeWithoutExtensions, Ipv6HeaderSize) - max(UdpHeaderSize, TcpHeaderSizeWithoutExtensions),
		MinWindowSize:       uint32(kib(4)),
		MaxWindowSize:       uint32(mib(4)),
		UserLimit:           0,
		ConnectSettings:     *DefaultConnectSettings(),
	}
	return tcpBufferSettings
}

func DefaultLocalUserNatSettings() *LocalUserNatSettings {
	return &LocalUserNatSettings{
		SequenceBufferSize: DefaultIpBufferSize,
		// BufferTimeout:      15 * time.Second,
		UdpBufferSettings: DefaultUdpBufferSettings(),
		TcpBufferSettings: DefaultTcpBufferSettings(),
	}
}

type LocalUserNatSettings struct {
	SequenceBufferSize int
	// BufferTimeout      time.Duration
	UdpBufferSettings *UdpBufferSettings
	TcpBufferSettings *TcpBufferSettings

	// Log, when set, is used by the local user nat and its udp/tcp buffers
	// and sequences (propagated to the buffer settings `Log` fields that are
	// nil). nil resolves to `DefaultLogger()`.
	Log Logger
}

// forwards packets using user space sockets
// this assumes transfer between the packet source and this is lossless and in order,
// so the protocol stack implementations do not implement any retransmit logic
type LocalUserNat struct {
	ctx       context.Context
	cancel    context.CancelFunc
	clientTag string
	log       Logger

	sendShards []chan *SendPacket
	numShards  int

	settings *LocalUserNatSettings

	bw *ProxyBandwidth

	// receive callback
	receiveCallbacks *CallbackList[ReceivePacketFunction]
}

func NewLocalUserNatWithDefaults(ctx context.Context, clientTag string, bw *ProxyBandwidth) *LocalUserNat {
	return NewLocalUserNat(ctx, clientTag, bw, DefaultLocalUserNatSettings())
}

func NewLocalUserNat(ctx context.Context, clientTag string, bw *ProxyBandwidth, settings *LocalUserNatSettings) *LocalUserNat {
	cancelCtx, cancel := context.WithCancel(ctx)

	log := loggerOrDefault(settings.Log)
	// propagate so a nat-level logger covers the udp/tcp buffers and sequences
	if settings.UdpBufferSettings != nil && settings.UdpBufferSettings.Log == nil {
		settings.UdpBufferSettings.Log = log
	}
	if settings.TcpBufferSettings != nil && settings.TcpBufferSettings.Log == nil {
		settings.TcpBufferSettings.Log = log
	}

	numShards := runtime.NumCPU()
	if numShards < 1 {
		numShards = 1
	}
	if numShards > 16 {
		numShards = 16
	}
	shardBufSize := settings.SequenceBufferSize / numShards
	if shardBufSize < 1 {
		shardBufSize = 1
	}
	sendShards := make([]chan *SendPacket, numShards)
	for i := 0; i < numShards; i++ {
		sendShards[i] = make(chan *SendPacket, shardBufSize)
	}

	localUserNat := &LocalUserNat{
		ctx:              cancelCtx,
		cancel:           cancel,
		clientTag:        clientTag,
		log:              log,
		sendShards:       sendShards,
		numShards:        numShards,
		settings:         settings,
		bw:               bw,
		receiveCallbacks: NewCallbackList[ReceivePacketFunction](),
	}

	for i := 0; i < numShards; i++ {
		go func(shard int) {
			HandleError(func() {
				localUserNat.runShard(shard)
			})
		}(i)
	}

	return localUserNat
}

func (self *LocalUserNat) SecurityPolicyStats(reset bool) SecurityPolicyStats {
	return SecurityPolicyStats{}
}

func (self *LocalUserNat) pickShard(packet []byte) int {
	if self.numShards <= 1 {
		return 0
	}
	hash := flowHash(packet)
	return int(hash % uint32(self.numShards))
}

func flowHash(packet []byte) uint32 {
	const offset32 = 2166136261
	const prime32 = 16777619

	if len(packet) < 20 {
		return 0
	}

	hash := func(data []byte) uint32 {
		h := uint32(offset32)
		for _, b := range data {
			h ^= uint32(b)
			h *= prime32
		}
		return h
	}

	ipVersion := packet[0] >> 4
	switch ipVersion {
	case 4:
		if len(packet) >= 20 {
			h := hash(packet[12:20])
			protocol := packet[9]
			if protocol == 6 || protocol == 17 {
				headerLen := int(packet[0]&0x0F) * 4
				if len(packet) >= headerLen+4 {
					h ^= hash(packet[headerLen : headerLen+4])
				}
			}
			return h
		}
	case 6:
		if len(packet) >= 40 {
			h := hash(packet[8:40])
			protocol := packet[6]
			if protocol == 6 || protocol == 17 {
				if len(packet) >= 44 {
					h ^= hash(packet[40:44])
				}
			}
			return h
		}
	}
	return 0
}

// TODO provide mode of the destination determines filtering rules - e.g. local networks
// TODO currently filter all local networks and non-encrypted traffic
func (self *LocalUserNat) SendPacketWithTimeout(source TransferPath, provideMode protocol.ProvideMode,
	packet []byte, timeout time.Duration) bool {
	sendPacket := &SendPacket{
		source:      source,
		provideMode: provideMode,
		packet:      packet,
	}
	shard := self.pickShard(packet)
	if timeout < 0 {
		select {
		case <-self.ctx.Done():
			return false
		case self.sendShards[shard] <- sendPacket:
			return true
		}
	} else if 0 == timeout {
		select {
		case <-self.ctx.Done():
			return false
		case self.sendShards[shard] <- sendPacket:
			return true
		default:
			// full
			return false
		}
	} else {
		select {
		case <-self.ctx.Done():
			return false
		case self.sendShards[shard] <- sendPacket:
			return true
		case <-time.After(timeout):
			// full
			return false
		}
	}
}

// `SendPacketFunction`
func (self *LocalUserNat) SendPacket(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
	return self.SendPacketWithTimeout(source, provideMode, packet, timeout)
}

// func (self *LocalUserNat) ReceiveN(source TransferPath, provideMode protocol.ProvideMode, packet []byte, n int) {
//     self.Receive(source, provideMode, packet[0:n])
// }

func (self *LocalUserNat) AddReceivePacketCallback(receiveCallback ReceivePacketFunction) func() {
	callbackId := self.receiveCallbacks.Add(receiveCallback)
	return func() {
		self.receiveCallbacks.Remove(callbackId)
	}
}

// func (self *LocalUserNat) RemoveReceivePacketCallback(receiveCallback ReceivePacketFunction) {
//     self.receiveCallbacks.Remove(receiveCallback)
// }

// `ReceivePacketFunction`
func (self *LocalUserNat) receive(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
	for _, receiveCallback := range self.receiveCallbacks.Get() {
		HandleError(func() {
			receiveCallback(source, provideMode, ipPath, packet)
		})
	}
}

func (self *LocalUserNat) runShard(shard int) {
	defer self.cancel()

	udp4Buffer := NewUdp4Buffer(self.ctx, self.receive, self.settings.UdpBufferSettings, self.bw)
	udp6Buffer := NewUdp6Buffer(self.ctx, self.receive, self.settings.UdpBufferSettings, self.bw)
	tcp4Buffer := NewTcp4Buffer(self.ctx, self.receive, self.settings.TcpBufferSettings, self.bw)
	tcp6Buffer := NewTcp6Buffer(self.ctx, self.receive, self.settings.TcpBufferSettings, self.bw)
	shardCh := self.sendShards[shard]

	for {
		select {
		case <-self.ctx.Done():
			return
		case sendPacket := <-shardCh:
			ipPacket := sendPacket.packet
			if len(ipPacket) == 0 {
				MessagePoolReturn(ipPacket)
				continue
			}
			var udpPacket parsedUdp
			var tcpPacket parsedTcp
			ipVersion := uint8(ipPacket[0]) >> 4
			switch ipVersion {
			case 4:
				ipProtocol, sourceIp, destinationIp, transport, ok := parseIpv4(ipPacket)
				if !ok {
					MessagePoolReturn(ipPacket)
					continue
				}
				switch ipProtocol {
				case IP_PROTOCOL_UDP:
					if !parseUdpPacket(sourceIp, destinationIp, transport, &udpPacket) {
						MessagePoolReturn(ipPacket)
						continue
					}

					c := func() bool {
						success, err := udp4Buffer.send(
							sendPacket.source,
							sendPacket.provideMode,
							&udpPacket,
							self.settings.UdpBufferSettings.WriteTimeout,
							ipPacket,
						)
						return success && err == nil
					}
					if self.log.V(2).Enabled() {
						TraceWithReturn(
							fmt.Sprintf("[lnr]send udp4 %s<-%s s(%s)", self.clientTag, sendPacket.source.SourceId, sendPacket.source.StreamId),
							c,
						)
					} else {
						c()
					}
				case IP_PROTOCOL_TCP:
					if !parseTcpPacket(sourceIp, destinationIp, transport, &tcpPacket) {
						MessagePoolReturn(ipPacket)
						continue
					}

					c := func() bool {
						success, err := tcp4Buffer.send(
							sendPacket.source,
							sendPacket.provideMode,
							&tcpPacket,
							self.settings.TcpBufferSettings.WriteTimeout,
							ipPacket,
						)
						return success && err == nil
					}
					if self.log.V(2).Enabled() {
						TraceWithReturn(
							fmt.Sprintf("[lnr]send tcp4 %s<-%s s(%s)", self.clientTag, sendPacket.source.SourceId, sendPacket.source.StreamId),
							c,
						)
					} else {
						c()
					}
				default:
					// no support for this protocol, drop
					MessagePoolReturn(ipPacket)
				}
			case 6:
				ipProtocol, sourceIp, destinationIp, transport, ok := parseIpv6(ipPacket)
				if !ok {
					MessagePoolReturn(ipPacket)
					continue
				}
				switch ipProtocol {
				case IP_PROTOCOL_UDP:
					if !parseUdpPacket(sourceIp, destinationIp, transport, &udpPacket) {
						MessagePoolReturn(ipPacket)
						continue
					}

					c := func() bool {
						success, err := udp6Buffer.send(
							sendPacket.source,
							sendPacket.provideMode,
							&udpPacket,
							self.settings.UdpBufferSettings.WriteTimeout,
							ipPacket,
						)
						return success && err == nil
					}
					if self.log.V(2).Enabled() {
						TraceWithReturn(
							fmt.Sprintf("[lnr]send udp6 %s<-%s s(%s)", self.clientTag, sendPacket.source.SourceId, sendPacket.source.StreamId),
							c,
						)
					} else {
						c()
					}
				case IP_PROTOCOL_TCP:
					if !parseTcpPacket(sourceIp, destinationIp, transport, &tcpPacket) {
						MessagePoolReturn(ipPacket)
						continue
					}

					c := func() bool {
						success, err := tcp6Buffer.send(
							sendPacket.source,
							sendPacket.provideMode,
							&tcpPacket,
							self.settings.TcpBufferSettings.WriteTimeout,
							ipPacket,
						)
						return success && err == nil
					}
					if self.log.V(2).Enabled() {
						TraceWithReturn(
							fmt.Sprintf("[lnr]send tcp6 %s<-%s s(%s)", self.clientTag, sendPacket.source.SourceId, sendPacket.source.StreamId),
							c,
						)
					} else {
						c()
					}
				default:
					// no support for this protocol, drop
					MessagePoolReturn(ipPacket)
				}
			default:
				// no support for this ip version, drop
				MessagePoolReturn(ipPacket)
			}
		}
	}
}

func (self *LocalUserNat) Close() {
	self.cancel()
}

type SendPacket struct {
	source      TransferPath
	provideMode protocol.ProvideMode
	packet      []byte
}

// comparable
type BufferId4 struct {
	source          TransferPath
	sourceIp        [4]byte
	sourcePort      int
	destinationIp   [4]byte
	destinationPort int
}

func NewBufferId4(source TransferPath, sourceIp net.IP, sourcePort int, destinationIp net.IP, destinationPort int) BufferId4 {
	return BufferId4{
		source:          source,
		sourceIp:        [4]byte(sourceIp),
		sourcePort:      sourcePort,
		destinationIp:   [4]byte(destinationIp),
		destinationPort: destinationPort,
	}
}

// comparable
type BufferId6 struct {
	source          TransferPath
	sourceIp        [16]byte
	sourcePort      int
	destinationIp   [16]byte
	destinationPort int
}

func NewBufferId6(source TransferPath, sourceIp net.IP, sourcePort int, destinationIp net.IP, destinationPort int) BufferId6 {
	return BufferId6{
		source:          source,
		sourceIp:        [16]byte(sourceIp),
		sourcePort:      sourcePort,
		destinationIp:   [16]byte(destinationIp),
		destinationPort: destinationPort,
	}
}

type UdpBufferSettings struct {
	// nil resolves to the local user nat `Log`
	Log                 Logger
	ReadTimeout         time.Duration
	WriteTimeout        time.Duration
	IdleTimeout         time.Duration
	Mtu                 int
	ReadBufferByteCount int
	SequenceBufferSize  int
	// the number of open sockets per user
	// uses an lru cleanup where new sockets over the limit close old sockets
	UserLimit     int
	MaxWindowSize uint32

	ConnectSettings
}

type Udp4Buffer struct {
	UdpBuffer[BufferId4]
}

func NewUdp4Buffer(ctx context.Context, receiveCallback ReceivePacketFunction,
	udpBufferSettings *UdpBufferSettings, bw *ProxyBandwidth) *Udp4Buffer {
	return &Udp4Buffer{
		UdpBuffer: *newUdpBuffer[BufferId4](ctx, receiveCallback, udpBufferSettings, bw),
	}
}

func (self *Udp4Buffer) send(source TransferPath, provideMode protocol.ProvideMode,
	udp *parsedUdp, timeout time.Duration, ipPacket []byte) (bool, error) {
	bufferId := NewBufferId4(
		source,
		udp.sourceIp, int(udp.sourcePort),
		udp.destinationIp, int(udp.destinationPort),
	)

	return self.udpSend(
		bufferId,
		udp.sourceIp,
		udp.destinationIp,
		source,
		provideMode,
		4,
		udp,
		timeout,
		ipPacket,
	)
}

type Udp6Buffer struct {
	UdpBuffer[BufferId6]
}

func NewUdp6Buffer(ctx context.Context, receiveCallback ReceivePacketFunction,
	udpBufferSettings *UdpBufferSettings, bw *ProxyBandwidth) *Udp6Buffer {
	return &Udp6Buffer{
		UdpBuffer: *newUdpBuffer[BufferId6](ctx, receiveCallback, udpBufferSettings, bw),
	}
}

func (self *Udp6Buffer) send(source TransferPath, provideMode protocol.ProvideMode,
	udp *parsedUdp, timeout time.Duration, ipPacket []byte) (bool, error) {
	bufferId := NewBufferId6(
		source,
		udp.sourceIp, int(udp.sourcePort),
		udp.destinationIp, int(udp.destinationPort),
	)

	return self.udpSend(
		bufferId,
		udp.sourceIp,
		udp.destinationIp,
		source,
		provideMode,
		6,
		udp,
		timeout,
		ipPacket,
	)
}

type UdpBuffer[BufferId comparable] struct {
	ctx               context.Context
	log               Logger
	receiveCallback   ReceivePacketFunction
	udpBufferSettings *UdpBufferSettings
	bw                *ProxyBandwidth

	mutex sync.Mutex

	sequences       map[BufferId]*UdpSequence
	sourceSequences map[TransferPath]map[BufferId]*UdpSequence
}

func newUdpBuffer[BufferId comparable](
	ctx context.Context,
	receiveCallback ReceivePacketFunction,
	udpBufferSettings *UdpBufferSettings,
	bw *ProxyBandwidth,
) *UdpBuffer[BufferId] {
	return &UdpBuffer[BufferId]{
		ctx:               ctx,
		log:               loggerOrDefault(udpBufferSettings.Log),
		receiveCallback:   receiveCallback,
		udpBufferSettings: udpBufferSettings,
		bw:                bw,
		sequences:         map[BufferId]*UdpSequence{},
		sourceSequences:   map[TransferPath]map[BufferId]*UdpSequence{},
	}
}

func (self *UdpBuffer[BufferId]) udpSend(
	bufferId BufferId,
	sourceIp net.IP,
	destinationIp net.IP,
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipVersion int,
	udp *parsedUdp,
	timeout time.Duration,
	ipPacket []byte,
) (bool, error) {
	initSequence := func(skip *UdpSequence) *UdpSequence {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		sequence, ok := self.sequences[bufferId]
		if ok {
			if skip == nil || skip != sequence {
				return sequence
			} else {
				sequence.Cancel()
				delete(self.sequences, bufferId)
				sourceSequences := self.sourceSequences[sequence.source]
				delete(sourceSequences, bufferId)
				if 0 == len(sourceSequences) {
					delete(self.sourceSequences, sequence.source)
					if self.bw != nil {
						self.bw.Clients.Add(-1)
						self.bw.RemoveSession(sequence.source)
					}
				}
			}
		}

		if 0 < self.udpBufferSettings.UserLimit {
			if sourceSequences := self.sourceSequences[source]; self.udpBufferSettings.UserLimit < len(sourceSequences) {
				applyLruUserLimit(maps.Values(sourceSequences), self.udpBufferSettings.UserLimit, func(sequence *UdpSequence) bool {
					if v := self.log.V(1); v.Enabled() {
						v.Infof(
							"[lnr]udp limit source %s->%s\n",
							source,
							net.JoinHostPort(
								sequence.destinationIp.String(),
								strconv.Itoa(int(sequence.destinationPort)),
							),
						)
					}
					return true
				})
			}
		}

		sourceIpCopy := make(net.IP, len(sourceIp))
		copy(sourceIpCopy, sourceIp)

		destinationIpCopy := make(net.IP, len(destinationIp))
		copy(destinationIpCopy, destinationIp)

		sequence = NewUdpSequence(
			self.ctx,
			self.receiveCallback,
			source,
			provideMode,
			ipVersion,
			sourceIpCopy,
			udp.sourcePort,
			destinationIpCopy,
			udp.destinationPort,
			self.udpBufferSettings,
		)
		self.sequences[bufferId] = sequence
		sourceEntries, ok := self.sourceSequences[source]
		if !ok {
			sourceEntries = map[BufferId]*UdpSequence{}
			self.sourceSequences[source] = sourceEntries
			if self.bw != nil {
				self.bw.Clients.Add(1)
				self.bw.AddSession(source, time.Now())
			}
		}
		sourceEntries[bufferId] = sequence
		go HandleError(func() {
			defer func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()
				sequence.Close()
				if sequence == self.sequences[bufferId] {
					delete(self.sequences, bufferId)
					sourceSequences := self.sourceSequences[sequence.source]
					delete(sourceSequences, bufferId)
					if 0 == len(sourceSequences) {
						delete(self.sourceSequences, sequence.source)
						if self.bw != nil {
							self.bw.Clients.Add(-1)
							self.bw.RemoveSession(sequence.source)
						}
					}
				}
			}()
			sequence.Run()
		})
		return sequence
	}

	sendItem := &UdpSendItem{
		provideMode: provideMode,
		udp:         udp,
		ipPacket:    ipPacket,
	}
	sequence := initSequence(nil)
	if success, err := sequence.send(sendItem, timeout); err == nil {
		// send() only enqueues ipPacket into sendItems on success — on every
		// drop path (idle-close, full channel, timeout) ownership never
		// transferred, so the caller (runShard) never sees a reason to
		// return it to the pool. Free it here or it leaks on backpressure.
		if !success {
			MessagePoolReturn(ipPacket)
		}
		return success, nil
	} else {
		// sequence closed, retry against a fresh one
		success, err := initSequence(sequence).send(sendItem, timeout)
		if !success {
			MessagePoolReturn(ipPacket)
		}
		return success, err
	}
}

type UdpSequence struct {
	ctx               context.Context
	cancel            context.CancelFunc
	log               Logger
	receiveCallback   ReceivePacketFunction
	udpBufferSettings *UdpBufferSettings

	sendMutex sync.Mutex
	sendItems chan *UdpSendItem

	idleCondition *IdleCondition

	StreamState
}

func NewUdpSequence(ctx context.Context, receiveCallback ReceivePacketFunction,
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipVersion int,
	sourceIp net.IP, sourcePort UDPPort,
	destinationIp net.IP, destinationPort UDPPort,
	udpBufferSettings *UdpBufferSettings) *UdpSequence {
	cancelCtx, cancel := context.WithCancel(ctx)
	// e2e-pqe merge: upstream now inlines StreamState in the return below; keep
	// the fork's active-connection accounting.
	atomic.AddInt64(&activeConnectionCount, 1)
	return &UdpSequence{
		ctx:               cancelCtx,
		cancel:            cancel,
		log:               loggerOrDefault(udpBufferSettings.Log),
		receiveCallback:   receiveCallback,
		sendItems:         make(chan *UdpSendItem, udpBufferSettings.SequenceBufferSize),
		udpBufferSettings: udpBufferSettings,
		idleCondition:     NewIdleCondition(),
		StreamState: StreamState{
			source:          source,
			provideMode:     provideMode,
			ipVersion:       ipVersion,
			sourceIp:        sourceIp,
			sourcePort:      sourcePort,
			destinationIp:   destinationIp,
			destinationPort: destinationPort,

			userLimited: userLimited{
				lastActivityTime: time.Now(),
			},
		},
	}
}

func (self *UdpSequence) send(sendItem *UdpSendItem, timeout time.Duration) (bool, error) {
	self.sendMutex.Lock()
	defer self.sendMutex.Unlock()

	select {
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	if !self.idleCondition.UpdateOpen() {
		return false, nil
	}
	defer self.idleCondition.UpdateClose()

	select {
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	if timeout < 0 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.sendItems <- sendItem:
			return true, nil
		}
	} else if timeout == 0 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.sendItems <- sendItem:
			return true, nil
		default:
			return false, nil
		}
	} else {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.sendItems <- sendItem:
			return true, nil
		case <-time.After(timeout):
			return false, nil
		}
	}
}

func (self *UdpSequence) Run() {
	type writePayload struct {
		sendIter uint64
		payload  []byte
		ipPacket []byte
	}
	var writePayloads chan writePayload

	defer func() {
		atomic.AddInt64(&activeConnectionCount, -1)
		self.cancel()

		func() {
			self.sendMutex.Lock()
			defer self.sendMutex.Unlock()
			close(self.sendItems)
		}()

		// drain the channel
		func() {
			for {
				select {
				case sendItem, ok := <-self.sendItems:
					if !ok {
						return
					}
					MessagePoolReturn(sendItem.ipPacket)
				default:
					return
				}
			}
		}()

		// drain write payloads after the main send loop exits, catching any
		// items the loop enqueued after the write goroutine's own drain ran
		if writePayloads != nil {
			for {
				select {
				case p, ok := <-writePayloads:
					if !ok {
						return
					}
					MessagePoolReturn(p.ipPacket)
				default:
					return
				}
			}
		}
	}()

	receive := func(packet []byte) {
		self.receiveCallback(self.source, self.provideMode, self.IpPath(), packet)
		MessagePoolReturn(packet)
	}

	self.log.V(2).Infof("[init]udp connect\n")
	socket, err := self.udpBufferSettings.DialContext(
		self.ctx,
		"udp",
		self.IpPath().DestinationHostPort(),
	)
	if err != nil {
		self.log.V(1).Infof("[init]udp connect error = %s\n", err)
		return
	}
	defer socket.Close()
	self.UpdateLastActivityTime()
	self.log.V(2).Infof("[init]connect success\n")

	// if udpConn, ok := socket.(*net.UDPConn); ok {
	// 	udpConn.SetReadBuffer(int(self.udpBufferSettings.MaxWindowSize))
	// 	udpConn.SetWriteBuffer(int(self.udpBufferSettings.MaxWindowSize))
	// }
	// f, _ := udpConn.File()
	// fd := SocketHandle(f.Fd())
	// syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_MTU, self.udpBufferSettings.Mtu)

	// pipelines

	writePayloads = make(chan writePayload, self.udpBufferSettings.SequenceBufferSize)
	go HandleError(func() {
		defer self.cancel()

		// on exit, return any buffers still queued so they aren't leaked from
		// the message pool (mirrors the readPackets drain below)
		defer func() {
			for {
				select {
				case writePayload, ok := <-writePayloads:
					if !ok {
						return
					}
					MessagePoolReturn(writePayload.ipPacket)
				default:
					return
				}
			}
		}()

		for {
			select {
			case <-self.ctx.Done():
				return
			case writePayload, ok := <-writePayloads:
				if !ok {
					return
				}
				payload := writePayload.payload
				sendIter := writePayload.sendIter

				writeEndTime := time.Now().Add(self.udpBufferSettings.WriteTimeout)

				for i := 0; i < len(payload); {
					select {
					case <-self.ctx.Done():
						MessagePoolReturn(writePayload.ipPacket)
						return
					default:
					}

					socket.SetWriteDeadline(writeEndTime)
					n, err := socket.Write(payload[i:])

					if err == nil {
						if v := self.log.V(2); v.Enabled() {
							v.Infof("[f%d]udp forward %d\n", sendIter, n)
						}
					} else {
						if v := self.log.V(1); v.Enabled() {
							v.Infof("[f%d]udp forward %d error = %s", sendIter, n, err)
						}
					}

					if 0 < n {
						self.UpdateLastActivityTime()

						j := i
						i += n
						if v := self.log.V(2); v.Enabled() {
							v.Infof("[f%d]udp forward %d/%d -> %d/%d +%d\n", sendIter, j, len(payload), i, len(payload), n)
						}
					}

					if err != nil {
						if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
							MessagePoolReturn(writePayload.ipPacket)
							return
						} else {
							// some other error
							MessagePoolReturn(writePayload.ipPacket)
							return
						}
					}
				}
				MessagePoolReturn(writePayload.ipPacket)
			}
		}
	}, self.cancel)

	readPackets := make(chan []byte, self.udpBufferSettings.SequenceBufferSize)
	go HandleError(func() {
		defer self.cancel()

		defer func() {
			for {
				select {
				case packet, ok := <-readPackets:
					if !ok {
						return
					}
					receive(packet)
				}
			}
		}()

		for {
			select {
			case <-self.ctx.Done():
				return
			case packet, ok := <-readPackets:
				if !ok {
					return
				}
				receive(packet)
			}
		}
	}, self.cancel)

	go HandleError(func() {
		defer func() {
			self.cancel()
			close(readPackets)
		}()

		buffer := make([]byte, self.udpBufferSettings.ReadBufferByteCount)

		for forwardIter := uint64(0); ; forwardIter += 1 {
			select {
			case <-self.ctx.Done():
				return
			default:
			}

			readTimeout := time.Now().Add(self.udpBufferSettings.ReadTimeout)
			socket.SetReadDeadline(readTimeout)
			n, err := socket.Read(buffer)

			if err != nil {
				if v := self.log.V(1); v.Enabled() {
					v.Infof("[f%d]udp receive err = %s\n", forwardIter, err)
				}
			}

			if 0 < n {
				self.UpdateLastActivityTime()

				packets, packetsErr := self.DataPackets(buffer, n, self.udpBufferSettings.Mtu)
				if packetsErr != nil {
					self.log.Infof("[f%d]udp receive packets error = %s\n", forwardIter, packetsErr)
					return
				}
				if 1 < len(packets) {
					if v := self.log.V(2); v.Enabled() {
						v.Infof("[f%d]udp receive segemented packets = %d\n", forwardIter, len(packets))
					}
				}
				for _, packet := range packets {
					if v := self.log.V(1); v.Enabled() {
						v.Infof("[f%d]udp receive %d\n", forwardIter, len(packet))
					}
					select {
					case <-self.ctx.Done():
						MessagePoolReturn(packet)
					case readPackets <- packet:
					}
				}
			}

			if err != nil {
				if err == io.EOF {
					return
				} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					if v := self.log.V(1); v.Enabled() {
						v.Infof("[f%d]timeout\n", forwardIter)
					}
					return
				} else {
					// some other error
					return
				}
			}
		}
	}, self.cancel)

	// reusable idle timer: this send loop wakes per datagram, so a per-iteration
	// time.After allocated a timer per packet (a dominant alloc in the udp
	// egress profile). reuse one timer across iterations instead.
	idleTimer := time.NewTimer(0)
	defer idleTimer.Stop()

	for sendIter := uint64(0); ; sendIter += 1 {
		checkpointId := self.idleCondition.Checkpoint()
		idleTimer.Reset(self.udpBufferSettings.IdleTimeout)
		select {
		case <-self.ctx.Done():
			return
		case sendItem, ok := <-self.sendItems:
			if !ok {
				return
			}
			payload := sendItem.udp.payload

			if 0 < len(payload) {
				writePayload := writePayload{
					payload:  payload,
					sendIter: sendIter,
					ipPacket: sendItem.ipPacket,
				}
				select {
				case writePayloads <- writePayload:
				case <-self.ctx.Done():
					MessagePoolReturn(sendItem.ipPacket)
					return
				}
			} else {
				MessagePoolReturn(sendItem.ipPacket)
			}
		case <-idleTimer.C:
			done := false
			func() {
				self.sendMutex.Lock()
				defer self.sendMutex.Unlock()
				if self.idleCondition.Close(checkpointId) {
					// close the sequence
					done = true
				}
			}()
			if done {
				// close the sequence
				return
			}
			// else there pending updates
		}
	}
}

func (self *UdpSequence) Cancel() {
	self.cancel()
}

func (self *UdpSequence) Close() {
	self.cancel()
}

type UdpSendItem struct {
	source      TransferPath
	provideMode protocol.ProvideMode
	udp         *parsedUdp
	ipPacket    []byte
}

type StreamState struct {
	source          TransferPath
	provideMode     protocol.ProvideMode
	ipVersion       int
	sourceIp        net.IP
	sourcePort      UDPPort
	destinationIp   net.IP
	destinationPort UDPPort

	userLimited

	// cached immutable ip path for this stream (see IpPath). The stream
	// identity (version, ips, ports) never changes, so it is built once and
	// then read-only.
	ipPath *IpPath

	// reusable backing for the common single-datagram DataPackets result.
	// DataPackets is called from one goroutine and its result is consumed
	// before the next call, so the backing can be reused; fragmented payloads
	// allocate a fresh slice.
	singleDataPacket [1][]byte
}

// IpPath returns the immutable ip path for this stream. The path is built once
// and cached; the stream identity (version, ips, ports) never changes.
func (self *StreamState) IpPath() *IpPath {
	if self.ipPath == nil {
		self.ipPath = &IpPath{
			Version:         self.ipVersion,
			Protocol:        IpProtocolUdp,
			SourceIp:        self.sourceIp,
			SourcePort:      int(self.sourcePort),
			DestinationIp:   self.destinationIp,
			DestinationPort: int(self.destinationPort),
		}
	}
	return self.ipPath
}

// this must only be called from one goroutine
// this is called from the writer only and does not need to syncrhronize with the reader state
func (self *StreamState) DataPackets(payload []byte, n int, mtu int) ([][]byte, error) {
	var headerByteCount int
	switch self.ipVersion {
	case 4:
		headerByteCount = Ipv4HeaderSizeWithoutExtensions + UdpHeaderSize
	case 6:
		headerByteCount = Ipv6HeaderSize + UdpHeaderSize
	}

	if mtu <= headerByteCount {
		return nil, fmt.Errorf("mtu %d is too small for IP+UDP headers (%d bytes)", mtu, headerByteCount)
	}
	packetByteCount := mtu - headerByteCount
	if n <= packetByteCount {
		self.singleDataPacket[0] = self.udpPacket(payload[0:n])
		return self.singleDataPacket[:], nil
	}
	packets := make([][]byte, 0, (n+packetByteCount-1)/packetByteCount)
	for i := 0; i < n; {
		j := min(i+packetByteCount, n)
		packets = append(packets, self.udpPacket(payload[i:j]))
		i = j
	}
	return packets, nil
}

// builds a udp packet from the stream destination to the stream source
// into a single pool buffer
func (self *StreamState) udpPacket(payload []byte) []byte {
	var ipHeaderByteCount int
	switch self.ipVersion {
	case 4:
		ipHeaderByteCount = Ipv4HeaderSizeWithoutExtensions
	case 6:
		ipHeaderByteCount = Ipv6HeaderSize
	}

	packet := MessagePoolGet(ipHeaderByteCount + UdpHeaderSize + len(payload))
	switch self.ipVersion {
	case 4:
		writeIpv4Header(packet, IP_PROTOCOL_UDP, self.destinationIp, self.sourceIp)
	case 6:
		writeIpv6Header(packet, IP_PROTOCOL_UDP, self.destinationIp, self.sourceIp)
	}

	udp := packet[ipHeaderByteCount:]
	binary.BigEndian.PutUint16(udp[0:2], uint16(self.destinationPort))
	binary.BigEndian.PutUint16(udp[2:4], uint16(self.sourcePort))
	binary.BigEndian.PutUint16(udp[4:6], uint16(UdpHeaderSize+len(payload)))
	udp[6] = 0
	udp[7] = 0
	copy(udp[UdpHeaderSize:], payload)
	checksum := transportChecksum(IP_PROTOCOL_UDP, self.destinationIp, self.sourceIp, udp)
	if checksum == 0 {
		checksum = 0xffff
	}
	binary.BigEndian.PutUint16(udp[6:8], checksum)
	return packet
}

func writeTcpHeader(tcp []byte, sourcePort, destinationPort uint16, seq, ackNumber uint32, flags uint8, windowSize uint16, options []byte) {
	binary.BigEndian.PutUint16(tcp[0:2], sourcePort)
	binary.BigEndian.PutUint16(tcp[2:4], destinationPort)
	binary.BigEndian.PutUint32(tcp[4:8], seq)
	binary.BigEndian.PutUint32(tcp[8:12], ackNumber)
	headerWordCount := 5 + (len(options)+3)/4
	tcp[12] = uint8(headerWordCount << 4)
	tcp[13] = flags
	binary.BigEndian.PutUint16(tcp[14:16], windowSize)
	tcp[16] = 0
	tcp[17] = 0
	tcp[18] = 0
	tcp[19] = 0
	copy(tcp[20:], options)
}

type TcpBufferSettings struct {
	// nil resolves to the local user nat `Log`
	Log Logger
	// ConnectTimeout     time.Duration
	ReadTimeout        time.Duration
	WriteTimeout       time.Duration
	AckCompressTimeout time.Duration
	// ReadPollTimeout time.Duration
	// WritePollTimeout time.Duration
	IdleTimeout         time.Duration
	ScaleDownTimeout    time.Duration
	ReadBufferByteCount int
	SequenceBufferSize  int
	Mtu                 int
	// the window size is the max amount of packet data in memory for each sequence
	// `WindowSize / 2^WindowScale` must fit in uint16
	// see https://datatracker.ietf.org/doc/html/rfc1323#page-8
	WindowScale uint32
	// the initial window size
	MinWindowSize uint32
	// `MaxWindowSize` should be a power of 2 multiple of `MinWindowSize`
	MaxWindowSize uint32
	// the number of open sockets per user
	// uses an lru cleanup where new sockets over the limit close old sockets
	UserLimit int

	ConnectSettings
}

type Tcp4Buffer struct {
	TcpBuffer[BufferId4]
}

func NewTcp4Buffer(ctx context.Context, receiveCallback ReceivePacketFunction,
	tcpBufferSettings *TcpBufferSettings, bw *ProxyBandwidth) *Tcp4Buffer {
	return &Tcp4Buffer{
		TcpBuffer: *newTcpBuffer[BufferId4](ctx, receiveCallback, tcpBufferSettings, bw),
	}
}

func (self *Tcp4Buffer) send(source TransferPath, provideMode protocol.ProvideMode,
	tcp *parsedTcp, timeout time.Duration, ipPacket []byte) (bool, error) {
	bufferId := NewBufferId4(
		source,
		tcp.sourceIp, int(tcp.sourcePort),
		tcp.destinationIp, int(tcp.destinationPort),
	)

	return self.tcpSend(
		bufferId,
		tcp.sourceIp,
		tcp.destinationIp,
		source,
		provideMode,
		4,
		tcp,
		timeout,
		ipPacket,
	)
}

type Tcp6Buffer struct {
	TcpBuffer[BufferId6]
}

func NewTcp6Buffer(ctx context.Context, receiveCallback ReceivePacketFunction,
	tcpBufferSettings *TcpBufferSettings, bw *ProxyBandwidth) *Tcp6Buffer {
	return &Tcp6Buffer{
		TcpBuffer: *newTcpBuffer[BufferId6](ctx, receiveCallback, tcpBufferSettings, bw),
	}
}

func (self *Tcp6Buffer) send(source TransferPath, provideMode protocol.ProvideMode,
	tcp *parsedTcp, timeout time.Duration, ipPacket []byte) (bool, error) {
	bufferId := NewBufferId6(
		source,
		tcp.sourceIp, int(tcp.sourcePort),
		tcp.destinationIp, int(tcp.destinationPort),
	)

	return self.tcpSend(
		bufferId,
		tcp.sourceIp,
		tcp.destinationIp,
		source,
		provideMode,
		6,
		tcp,
		timeout,
		ipPacket,
	)
}

type TcpBuffer[BufferId comparable] struct {
	ctx               context.Context
	log               Logger
	receiveCallback   ReceivePacketFunction
	tcpBufferSettings *TcpBufferSettings
	bw                *ProxyBandwidth

	mutex sync.Mutex

	sequences       map[BufferId]*TcpSequence
	sourceSequences map[TransferPath]map[BufferId]*TcpSequence
}

func newTcpBuffer[BufferId comparable](
	ctx context.Context,
	receiveCallback ReceivePacketFunction,
	tcpBufferSettings *TcpBufferSettings,
	bw *ProxyBandwidth,
) *TcpBuffer[BufferId] {
	return &TcpBuffer[BufferId]{
		ctx:               ctx,
		log:               loggerOrDefault(tcpBufferSettings.Log),
		receiveCallback:   receiveCallback,
		tcpBufferSettings: tcpBufferSettings,
		bw:                bw,
		sequences:         map[BufferId]*TcpSequence{},
		sourceSequences:   map[TransferPath]map[BufferId]*TcpSequence{},
	}
}

func (self *TcpBuffer[BufferId]) tcpSend(
	bufferId BufferId,
	sourceIp net.IP,
	destinationIp net.IP,
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipVersion int,
	tcp *parsedTcp,
	timeout time.Duration,
	ipPacket []byte,
) (bool, error) {
	initSequence := func() *TcpSequence {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		if sequence, ok := self.sequences[bufferId]; ok {
			if tcp.rst {
				sequence.Cancel()
				delete(self.sequences, bufferId)
				sourceSequences := self.sourceSequences[sequence.source]
				delete(sourceSequences, bufferId)
				if 0 == len(sourceSequences) {
					delete(self.sourceSequences, sequence.source)
					if self.bw != nil {
						self.bw.Clients.Add(-1)
						self.bw.RemoveSession(sequence.source)
					}
				}
				MessagePoolReturn(ipPacket)
				return nil
			}
			return sequence
		}

		if !tcp.syn {
			MessagePoolReturn(ipPacket)
			if v := self.log.V(2); v.Enabled() {
				v.Infof("[lnr]tcp drop no syn (%s)\n", tcp.flagsString())
			}
			return nil
		}

		if 0 < self.tcpBufferSettings.UserLimit {
			if sourceSequences := self.sourceSequences[source]; self.tcpBufferSettings.UserLimit < len(sourceSequences) {
				applyLruUserLimit(maps.Values(sourceSequences), self.tcpBufferSettings.UserLimit, func(sequence *TcpSequence) bool {
					if v := self.log.V(1); v.Enabled() {
						v.Infof(
							"[lnr]tcp limit source %s->%s\n",
							source,
							net.JoinHostPort(
								sequence.destinationIp.String(),
								strconv.Itoa(int(sequence.destinationPort)),
							),
						)
					}
					return true
				})
			}
		}

		sourceIpCopy := make(net.IP, len(sourceIp))
		copy(sourceIpCopy, sourceIp)

		destinationIpCopy := make(net.IP, len(destinationIp))
		copy(destinationIpCopy, destinationIp)

		sequence := NewTcpSequence(
			self.ctx,
			self.receiveCallback,
			source,
			provideMode,
			ipVersion,
			sourceIpCopy,
			tcp.sourcePort,
			destinationIpCopy,
			tcp.destinationPort,
			self.tcpBufferSettings,
		)
		self.sequences[bufferId] = sequence
		sourceEntries, ok := self.sourceSequences[source]
		if !ok {
			sourceEntries = map[BufferId]*TcpSequence{}
			self.sourceSequences[source] = sourceEntries
			if self.bw != nil {
				self.bw.Clients.Add(1)
				self.bw.AddSession(source, time.Now())
			}
		}
		sourceEntries[bufferId] = sequence
		go HandleError(func() {
			defer func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()
				sequence.Close()
				if sequence == self.sequences[bufferId] {
					delete(self.sequences, bufferId)
					sourceSequences := self.sourceSequences[sequence.source]
					delete(sourceSequences, bufferId)
					if 0 == len(sourceSequences) {
						delete(self.sourceSequences, sequence.source)
						if self.bw != nil {
							self.bw.Clients.Add(-1)
							self.bw.RemoveSession(sequence.source)
						}
					}
				}
			}()
			sequence.Run()
		})
		return sequence
	}
	sendItem := &TcpSendItem{
		provideMode: provideMode,
		tcp:         tcp,
		ipPacket:    ipPacket,
	}
	if sequence := initSequence(); sequence == nil {
		// initSequence already returns ipPacket to the pool itself on both
		// of its nil-returning paths (RST-cancel, non-SYN drop) — freeing it
		// again here would double-return a possibly still-shared buffer.
		return false, nil
	} else {
		// send() only enqueues ipPacket into sendItems on success — free it
		// here on every drop path or it leaks on backpressure.
		success, err := sequence.send(sendItem, timeout)
		if !success {
			MessagePoolReturn(ipPacket)
		}
		return success, err
	}
}

/*
** Important implementation note **
In this implementation, packet flow from the UNAT to the source
is assumed to never require retransmits. The retrasmit logic
is not implemented.
This is a safe assumption when moving packets from local raw socket
to the UNAT via `transfer`, which is lossless and in-order.
*/
type TcpSequence struct {
	ctx    context.Context
	cancel context.CancelFunc
	log    Logger

	receiveCallback ReceivePacketFunction

	tcpBufferSettings *TcpBufferSettings

	sendMutex sync.Mutex
	sendItems chan *TcpSendItem

	idleCondition *IdleCondition

	ConnectionState
}

func NewTcpSequence(ctx context.Context, receiveCallback ReceivePacketFunction,
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipVersion int,
	sourceIp net.IP, sourcePort TCPPort,
	destinationIp net.IP, destinationPort TCPPort,
	tcpBufferSettings *TcpBufferSettings) *TcpSequence {
	cancelCtx, cancel := context.WithCancel(ctx)

	// e2e-pqe merge: upstream now inlines ConnectionState in the struct below
	// (window settings preserved there); keep the fork's `sequence :=` form so
	// the active-connection accounting tail still works.
	sequence := &TcpSequence{
		ctx:               cancelCtx,
		cancel:            cancel,
		log:               loggerOrDefault(tcpBufferSettings.Log),
		receiveCallback:   receiveCallback,
		tcpBufferSettings: tcpBufferSettings,
		sendItems:         make(chan *TcpSendItem, tcpBufferSettings.SequenceBufferSize),
		idleCondition:     NewIdleCondition(),
		ConnectionState: ConnectionState{
			source:          source,
			provideMode:     provideMode,
			ipVersion:       ipVersion,
			sourceIp:        sourceIp,
			sourcePort:      sourcePort,
			destinationIp:   destinationIp,
			destinationPort: destinationPort,
			// the window size starts at the fixed value
			enableWindowScale: false,
			// FIXME start this at initial window size, and it grows up to max window size
			// FIXME initial window size should be ~4k, set max window size as a 2^amount multiplier of initial size
			windowSize:  tcpBufferSettings.MinWindowSize,
			windowScale: 0,

			userLimited: userLimited{
				lastActivityTime: time.Now(),
			},
		},
	}
	atomic.AddInt64(&activeConnectionCount, 1)
	return sequence
}

func (self *TcpSequence) send(sendItem *TcpSendItem, timeout time.Duration) (bool, error) {
	self.sendMutex.Lock()
	defer self.sendMutex.Unlock()

	select {
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	if !self.idleCondition.UpdateOpen() {
		return false, nil
	}
	defer self.idleCondition.UpdateClose()

	select {
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	if timeout < 0 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.sendItems <- sendItem:
			return true, nil
		}
	} else if timeout == 0 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.sendItems <- sendItem:
			return true, nil
		default:
			return false, nil
		}
	} else {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.sendItems <- sendItem:
			return true, nil
		case <-time.After(timeout):
			return false, nil
		}
	}
}

func (self *TcpSequence) Run() {
	type writePayload struct {
		sendIter uint64
		payload  []byte
		ipPacket []byte
	}
	var writePayloads chan writePayload

	defer func() {
		atomic.AddInt64(&activeConnectionCount, -1)
		self.cancel()

		func() {
			self.sendMutex.Lock()
			defer self.sendMutex.Unlock()
			close(self.sendItems)
		}()

		// drain the channel
		func() {
			for {
				select {
				case sendItem, ok := <-self.sendItems:
					if !ok {
						return
					}
					MessagePoolReturn(sendItem.ipPacket)
				default:
					return
				}
			}
		}()

		// drain write payloads after the main send loop exits
		if writePayloads != nil {
			for {
				select {
				case p, ok := <-writePayloads:
					if !ok {
						return
					}
					MessagePoolReturn(p.ipPacket)
				default:
					return
				}
			}
		}
	}()

	// note receive is called from multiple goroutines
	// tcp packets with ack may be reordered due to being written in parallel
	receive := func(packet []byte) {
		self.receiveCallback(self.source, self.provideMode, self.IpPath(), packet)
		MessagePoolReturn(packet)
	}

	// f, _ := tcpConn.File()
	// fd := SocketHandle(f.Fd())
	// syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_MTU, self.tcpBufferSettings.Mtu)

	var packet []byte
	var packetErr error
	for syn := false; !syn; {
		checkpointId := self.idleCondition.Checkpoint()
		select {
		case <-self.ctx.Done():
			return
		case sendItem := <-self.sendItems:
			self.log.V(2).Infof("[init]send(%d)\n", len(sendItem.tcp.payload))
			// the first packet must be a syn
			if sendItem.tcp.syn {
				self.log.V(2).Infof("[init]SYN\n")

				func() {
					self.mutex.Lock()
					defer self.mutex.Unlock()

					// sendSeq is the next expected sequence number
					// SYN and FIN consume one
					self.sendSeq = sendItem.tcp.seq + 1
					// start the send seq at send seq
					// this is arbitrary, and since there is no transport security risk back to sender is fine
					self.receiveSeq = sendItem.tcp.seq
					self.receiveSeqAck = sendItem.tcp.seq

					self.enableWindowScale, self.receiveWindowScale = ParseTcpWindowScaleOpts(sendItem.tcp.options)
					self.receiveWindowSize = uint32(sendItem.tcp.windowSize) << self.receiveWindowScale
					self.enableTimestamp = sendItem.tcp.enableTimestamp
					self.timestampRecent = sendItem.tcp.timestampValue
					if sendItem.tcp.enableMss {
						self.peerMss = sendItem.tcp.mss
					}
					if self.enableWindowScale {
						// compute the window scale to fit the window size in uint16
						bits := math.Log2(float64(self.tcpBufferSettings.MaxWindowSize) / float64(math.MaxUint16))
						if 0 <= bits {
							self.windowScale = uint32(math.Ceil(bits))
						} else {
							self.windowScale = 0
						}
					} else {
						// turn off window scale for send
						self.windowScale = 0
					}
					self.log.V(2).Infof("[init]window=%d/%d, receive=%d/%d\n", self.windowSize, self.windowScale, self.receiveWindowSize, self.receiveWindowScale)

					packet, packetErr = self.SynAck()
					self.receiveSeq += 1
				}()

				syn = true
			} else {
				// an ACK here could be for a previous FIN
				self.log.V(2).Infof("[init]waiting for SYN (%s)\n", tcpFlagsString(sendItem.tcp))
			}
			MessagePoolReturn(sendItem.ipPacket)
		case <-time.After(self.tcpBufferSettings.ConnectTimeout):
			if self.idleCondition.Close(checkpointId) {
				// close the sequence
				self.log.V(2).Infof("[init]connect timeout\n")
				return
			}
			// else there pending updates
		}
	}

	if packetErr != nil {
		return
	}

	// connect to upstream before sending the syn+ack
	self.log.V(2).Infof("[init]tcp connect\n")
	socket, err := self.tcpBufferSettings.DialContext(
		self.ctx,
		"tcp",
		self.IpPath().DestinationHostPort(),
	)
	if err != nil {
		self.log.V(1).Infof("[init]tcp connect error = %s\n", err)
		return
	}
	self.UpdateLastActivityTime()
	self.log.V(2).Infof("[init]connect success\n")

	defer socket.Close()
	if tcpConn, ok := socket.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetNoDelay(true)
		// tcpConn.SetReadBuffer(int(self.tcpBufferSettings.MaxWindowSize))
		// tcpConn.SetWriteBuffer(int(self.tcpBufferSettings.MaxWindowSize))
	}

	self.log.V(2).Infof("[init]receive SYN+ACK\n")
	receive(packet)

	/*
		if v, ok := socket.(*net.TCPConn); ok {
			if err := v.SetWriteBuffer(int(self.windowSize)); err != nil {
				self.log.Infof("[init]could not set write buffer = %d\n", self.windowSize)
			}
			// if err := v.SetReadBuffer(int(self.receiveWindowSize)); err != nil {
			// 	self.log.Infof("[init]could not set read buffer = %d\n", self.receiveWindowSize)
			// }
		}
	*/

	receiveAckCond := sync.NewCond(&self.mutex)
	ackCond := sync.NewCond(&self.mutex)
	defer func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		receiveAckCond.Broadcast()
		ackCond.Broadcast()
	}()

	var ackedSendSeq uint32
	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		ackedSendSeq = self.sendSeq
	}()

	// pipelines

	writePayloads = make(chan writePayload, self.tcpBufferSettings.SequenceBufferSize)
	go HandleError(func() {
		defer self.cancel()

		// on exit, return any buffers still queued so they aren't leaked from
		// the message pool (mirrors the readPackets drain below)
		defer func() {
			for {
				select {
				case writePayload, ok := <-writePayloads:
					if !ok {
						return
					}
					MessagePoolReturn(writePayload.ipPacket)
				default:
					return
				}
			}
		}()

		for {
			select {
			case <-self.ctx.Done():
				return
			case writePayload, ok := <-writePayloads:
				if !ok {
					return
				}
				payload := writePayload.payload
				sendIter := writePayload.sendIter
				writeEndTime := time.Now().Add(self.tcpBufferSettings.WriteTimeout)
				for i := 0; i < len(payload); {
					select {
					case <-self.ctx.Done():
						MessagePoolReturn(writePayload.ipPacket)
						return
					default:
					}

					socket.SetWriteDeadline(writeEndTime)
					n, err := socket.Write(payload[i:])

					if err == nil {
						if v := self.log.V(2); v.Enabled() {
							v.Infof("[f%d]tcp forward %d\n", sendIter, n)
						}
					} else {
						if v := self.log.V(1); v.Enabled() {
							v.Infof("[f%d]tcp forward %d error = %s\n", sendIter, n, err)
						}
					}

					if 0 < n {
						// func() {
						// 	self.mutex.Lock()
						// 	defer self.mutex.Unlock()

						// 	self.sendSeq += uint32(n)
						// 	ackCond.Broadcast()
						// }()

						self.UpdateLastActivityTime()

						j := i
						i += n
						if v := self.log.V(2); v.Enabled() {
							v.Infof("[f%d]tcp forward %d/%d -> %d/%d +%d\n", sendIter, j, len(payload), i, len(payload), n)
						}
					}

					if err != nil {
						if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
							MessagePoolReturn(writePayload.ipPacket)
							return
						} else {
							// some other error
							MessagePoolReturn(writePayload.ipPacket)
							return
						}
					}
				}
				MessagePoolReturn(writePayload.ipPacket)
			}
		}
	}, self.cancel)

	readPackets := make(chan []byte, self.tcpBufferSettings.SequenceBufferSize)
	go HandleError(func() {
		defer self.cancel()

		defer func() {
			for {
				select {
				case packet, ok := <-readPackets:
					if !ok {
						return
					}
					receive(packet)
				}
			}
		}()

		for {
			select {
			case <-self.ctx.Done():
				return
			case packet, ok := <-readPackets:
				if !ok {
					return
				}
				receive(packet)
			}
		}
	}, self.cancel)

	go HandleError(func() {
		fin := false
		defer func() {
			self.cancel()

			if !fin {
				var packet []byte
				var err error
				func() {
					self.mutex.Lock()
					defer self.mutex.Unlock()

					packet, err = self.RstAck()
				}()
				if err == nil {
					select {
					case readPackets <- packet:
						fin = true
					}
				}
			}

			close(readPackets)
		}()

		buffer := make([]byte, self.tcpBufferSettings.ReadBufferByteCount)

		for forwardIter := uint64(0); ; forwardIter += 1 {
			select {
			case <-self.ctx.Done():
				return
			default:
			}

			readTimeout := time.Now().Add(self.tcpBufferSettings.ReadTimeout)
			socket.SetReadDeadline(readTimeout)

			n, err := socket.Read(buffer)

			if err != nil {
				if v := self.log.V(1); v.Enabled() {
					v.Infof("[f%d]tcp receive error = %s\n", forwardIter, err)
				}
			}

			if 0 < n {
				self.UpdateLastActivityTime()

				// since the transfer from local to remove is lossless and preserves order,
				// do not worry about retransmits
				var packets [][]byte
				var packetsErr error
				func() {
					self.mutex.Lock()
					defer self.mutex.Unlock()

					select {
					case <-self.ctx.Done():
						return
					default:
					}

					for uint32(self.receiveWindowSize) < self.receiveSeq-self.receiveSeqAck+uint32(n) {
						if v := self.log.V(2); v.Enabled() {
							v.Infof("[f%d]tcp receive window wait\n", forwardIter)
						}
						receiveAckCond.Wait()
						select {
						case <-self.ctx.Done():
							return
						default:
						}
					}

					packets, packetsErr = self.DataPackets(buffer, n, self.tcpBufferSettings.Mtu)
					if packetsErr != nil {
						self.log.Infof("[f%d]tcp receive packets error = %s\n", forwardIter, packetsErr)
						return
					}

					if 1 < len(packets) {
						if v := self.log.V(2); v.Enabled() {
							v.Infof("[f%d]tcp receive segmented packets %d\n", forwardIter, len(packets))
						}
					}
					if v := self.log.V(2); v.Enabled() {
						v.Infof("[f%d]tcp receive %d %d %d\n", forwardIter, n, len(packets), self.receiveSeq)
					}

					self.receiveSeq += uint32(n)

					ackedSendSeq = self.sendSeq
				}()
				if packets == nil {
					return
				}

				select {
				case <-self.ctx.Done():
					return
				default:
				}

				for _, packet := range packets {
					select {
					case <-self.ctx.Done():
						MessagePoolReturn(packet)
					case readPackets <- packet:
					}
				}
			}

			if err != nil {
				if err == io.EOF {
					// closed (FIN)
					// propagate the FIN and close the sequence
					self.log.V(2).Infof("[final]FIN\n")
					var finPacket []byte
					var finErr error
					func() {
						self.mutex.Lock()
						defer self.mutex.Unlock()

						finPacket, finErr = self.FinAck()
						self.receiveSeq += 1
					}()
					if finErr == nil {
						select {
						case <-self.ctx.Done():
							MessagePoolReturn(finPacket)
						case readPackets <- finPacket:
							fin = true
						}
					}
					return
				} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					if v := self.log.V(2); v.Enabled() {
						v.Infof("[f%d]timeout\n", forwardIter)
					}
					return
				} else {
					// some other error
					return
				}
			}
		}
	}, self.cancel)

	go HandleError(func() {
		defer self.cancel()

		for {
			select {
			case <-self.ctx.Done():
				return
			default:
			}

			var packet []byte
			func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()

				select {
				case <-self.ctx.Done():
					return
				default:
				}

				for self.sendSeq == ackedSendSeq {
					ackCond.Wait()
					select {
					case <-self.ctx.Done():
						return
					default:
					}
				}

				var err error
				packet, err = self.PureAck()
				if err != nil {
					self.log.Infof("[r]ack err = %s\n", err)
				}
				ackedSendSeq = self.sendSeq
			}()
			if packet == nil {
				return
			}

			select {
			case <-self.ctx.Done():
				return
			default:
			}

			receive(packet)

			if 0 < self.tcpBufferSettings.AckCompressTimeout {
				select {
				case <-time.After(self.tcpBufferSettings.AckCompressTimeout):
				case <-self.ctx.Done():
					return
				}
			}
		}
	}, self.cancel)

	// window scaling depends on `nonBlockingByteCount` and `blockingByteCount` per `self.windowSize`
	nonBlockingByteCount := uint32(0)
	blockingByteCount := uint32(0)
	lastActivityTime := time.Now()
	fin := false
	for sendIter := uint64(0); !fin; sendIter += 1 {
		checkpointId := self.idleCondition.Checkpoint()
		select {
		case <-self.ctx.Done():
			return
		case sendItem := <-self.sendItems:
			lastActivityTime = time.Now()
			if self.log.V(2).Enabled() {
				if "ACK" != tcpFlagsString(sendItem.tcp) {
					self.log.Infof("[r%d]receive(%d %s)\n", sendIter, len(sendItem.tcp.payload), tcpFlagsString(sendItem.tcp))
				}
			}

			if sendItem.tcp.rst {
				// a RST typically appears for a bad TCP segment
				self.log.V(2).Infof("[r%d]RST\n", sendIter)
				MessagePoolReturn(sendItem.ipPacket)
				return
			}

			drop := false

			func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()

				if self.sendSeq != sendItem.tcp.seq || sendItem.tcp.syn {
					// a retransmit
					// since the transfer from local to remote is lossless and preserves order,
					// the packet is already pending. Ignore.
					drop = true
				} else if sendItem.tcp.ack {
					// acks are reliably delivered (see above)
					// we do not need to resend receive packets on missing acks
					// note the window size can be be adjusted at any time for the same receive seq number,
					// e.g. ->0 then ->full on receiver full
					if self.receiveSeqAck <= sendItem.tcp.ackNumber {
						self.receiveWindowSize = uint32(sendItem.tcp.windowSize) << self.receiveWindowScale
						self.receiveSeqAck = sendItem.tcp.ackNumber
						self.updateTimestampRecentWithLock(sendItem.tcp)
						receiveAckCond.Broadcast()
					}
				}
			}()

			if drop {
				MessagePoolReturn(sendItem.ipPacket)
				continue
			}

			if sendItem.tcp.fin {
				self.log.V(2).Infof("[r%d]FIN\n", sendIter)
				func() {
					self.mutex.Lock()
					defer self.mutex.Unlock()

					self.sendSeq += 1
					ackCond.Broadcast()
				}()
			}

			payload := sendItem.tcp.payload
			if 0 < len(payload) {
				writePayload := writePayload{
					payload:  payload,
					sendIter: sendIter,
					ipPacket: sendItem.ipPacket,
				}
				select {
				case writePayloads <- writePayload:
					nonBlockingByteCount += uint32(len(payload))
				default:
					select {
					case writePayloads <- writePayload:
						blockingByteCount += uint32(len(payload))
					case <-self.ctx.Done():
						MessagePoolReturn(sendItem.ipPacket)
						return
					}
				}
				func() {
					self.mutex.Lock()
					defer self.mutex.Unlock()
					if self.windowSize <= blockingByteCount+nonBlockingByteCount {
						if self.windowSize <= nonBlockingByteCount {
							nextWindowSize := min(self.windowSize*2, self.tcpBufferSettings.MaxWindowSize)
							if self.windowSize != nextWindowSize {
								self.log.V(1).Infof("[r%d]increase window size %d -> %d\n", sendIter, self.windowSize, nextWindowSize)
								self.windowSize = nextWindowSize
							}
						} else if self.windowSize/2 <= blockingByteCount {
							nextWindowSize := max(self.windowSize/2, self.tcpBufferSettings.MinWindowSize)
							if self.windowSize != nextWindowSize {
								self.log.V(1).Infof("[r%d]decrease window size %d -> %d\n", sendIter, self.windowSize, nextWindowSize)
								self.windowSize = nextWindowSize
							}
						}
						// reset the stats
						nonBlockingByteCount = uint32(0)
						blockingByteCount = uint32(0)
					}

					self.sendSeq += uint32(len(payload))
					ackCond.Broadcast()
				}()
			} else {
				MessagePoolReturn(sendItem.ipPacket)
			}

			if sendItem.tcp.fin {
				// flush the write channel to propage the FIN and close the sequence
				close(writePayloads)
				fin = true
			}

		case <-time.After(self.tcpBufferSettings.ScaleDownTimeout):
			func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()
				if self.windowSize != self.tcpBufferSettings.MinWindowSize {
					self.log.V(1).Infof("[r%d]idle scale down window size %d -> %d\n", sendIter, self.windowSize, self.tcpBufferSettings.MinWindowSize)
					self.windowSize = self.tcpBufferSettings.MinWindowSize
					nonBlockingByteCount = uint32(0)
					blockingByteCount = uint32(0)
					ackCond.Broadcast()
				}
			}()

			if self.tcpBufferSettings.IdleTimeout <= time.Since(lastActivityTime) {
				done := false
				func() {
					self.sendMutex.Lock()
					defer self.sendMutex.Unlock()
					if self.idleCondition.Close(checkpointId) {
						// close the sequence
						done = true
					}
				}()
				if done {
					// close the sequence
					self.log.V(2).Infof("[r%d]timeout\n", sendIter)
					return
				}
			}
		}
	}

	// wait for `writePayloads` to finish
	select {
	case <-self.ctx.Done():
	}
}

func (self *TcpSequence) Cancel() {
	self.cancel()
}

func (self *TcpSequence) Close() {
	self.cancel()
}

type TcpSendItem struct {
	provideMode protocol.ProvideMode
	tcp         *parsedTcp
	ipPacket    []byte
}

type ConnectionState struct {
	source          TransferPath
	provideMode     protocol.ProvideMode
	ipVersion       int
	sourceIp        net.IP
	sourcePort      TCPPort
	destinationIp   net.IP
	destinationPort TCPPort

	mutex sync.Mutex

	sendSeq            uint32
	receiveSeq         uint32
	receiveSeqAck      uint32
	receiveWindowSize  uint32
	receiveWindowScale uint32
	enableWindowScale  bool
	windowSize         uint32
	windowScale        uint32
	enableTimestamp    bool
	timestampRecent    uint32
	timestampOffset    uint32
	peerMss            uint32
	// Tests provide an exact timestamp without sleeping. Nil uses elapsed
	// monotonic process time and is a production no-op.
	timestampValueForTest func() uint32
	// encodedWindowSize  uint16

	userLimited
}

func (self *ConnectionState) IpPath() *IpPath {
	return &IpPath{
		Version:         self.ipVersion,
		Protocol:        IpProtocolTcp,
		SourceIp:        self.sourceIp,
		SourcePort:      int(self.sourcePort),
		DestinationIp:   self.destinationIp,
		DestinationPort: int(self.destinationPort),
	}
}

func (self *ConnectionState) encodedWindowSize() uint16 {
	return uint16(min(
		uint32(self.windowSize>>self.windowScale),
		uint32(math.MaxUint16),
	))
}

const tcpTimestampOptionByteCount = 12

var tcpTimestampEpoch = time.Now()

// Returns a nonzero millisecond timestamp for RFC 7323 packets. TCP compares
// these values modulo 32 bits, so process-relative monotonic time is enough.
func (self *ConnectionState) timestampValue() uint32 {
	if self.timestampValueForTest != nil {
		return self.timestampValueForTest()
	}
	return self.timestampOffset + uint32(time.Since(tcpTimestampEpoch)/time.Millisecond) + 1
}

// Tracks the newest timestamp observed from the source. The sequence mutex
// must be held. Reordered older packets do not move the echoed value backward.
func (self *ConnectionState) updateTimestampRecentWithLock(tcp *parsedTcp) {
	if !self.enableTimestamp || !tcp.enableTimestamp {
		return
	}
	// RFC 7323 updates the recent timestamp only when the segment begins at or
	// before the greatest cumulative acknowledgement already sent. A future
	// out-of-order segment is reconsidered when its gap closes; accepting its
	// timestamp here would move the echo clock ahead of the receive frontier.
	if 0 < int32(tcp.seq-self.sendSeq) {
		return
	}
	if self.timestampRecent == 0 || 0 <= int32(tcp.timestampValue-self.timestampRecent) {
		self.timestampRecent = tcp.timestampValue
	}
}

func (self *ConnectionState) SynAck() ([]byte, error) {
	var ipHeaderByteCount int
	switch self.ipVersion {
	case 4:
		ipHeaderByteCount = Ipv4HeaderSizeWithoutExtensions
	case 6:
		ipHeaderByteCount = Ipv6HeaderSize
	}

	var optsBytes []byte
	if self.enableWindowScale {
		windowScaleBytes := make([]byte, 4)
		binary.BigEndian.PutUint32(windowScaleBytes[0:4], self.windowScale)
		// options must be padded to a 4-byte boundary to match writeTcpHeader's
		// data-offset (headerWordCount) computation; make() zero-fills the pad,
		// which reads as an EOL terminator (kind 0)
		const optionsByteCount = 3
		paddedOptionsByteCount := (optionsByteCount + 3) &^ 3
		optsBytes = make([]byte, paddedOptionsByteCount)
		optsBytes[0] = 3
		optsBytes[1] = 3
		optsBytes[2] = windowScaleBytes[3]
	}

	tcpHeaderByteCount := TcpHeaderSizeWithoutExtensions + len(optsBytes)
	packet := MessagePoolGet(ipHeaderByteCount + tcpHeaderByteCount)
	switch self.ipVersion {
	case 4:
		writeIpv4Header(packet, IP_PROTOCOL_TCP, self.destinationIp, self.sourceIp)
	case 6:
		writeIpv6Header(packet, IP_PROTOCOL_TCP, self.destinationIp, self.sourceIp)
	}

	flags := tcpFlagSyn | tcpFlagAck
	writeTcpHeader(packet[ipHeaderByteCount:], uint16(self.destinationPort), uint16(self.sourcePort), self.receiveSeq, self.sendSeq, flags, self.encodedWindowSize(), optsBytes)

	// checksum covers the full segment (header + options), not just the fixed header
	tcpBytes := packet[ipHeaderByteCount:]
	checksum := transportChecksum(IP_PROTOCOL_TCP, self.destinationIp, self.sourceIp, tcpBytes)
	binary.BigEndian.PutUint16(tcpBytes[16:18], checksum)

	return packet, nil
}
func (self *ConnectionState) PureAck() ([]byte, error) {
	var ipHeaderByteCount int
	switch self.ipVersion {
	case 4:
		ipHeaderByteCount = Ipv4HeaderSizeWithoutExtensions
	case 6:
		ipHeaderByteCount = Ipv6HeaderSize
	}

	packet := MessagePoolGet(ipHeaderByteCount + TcpHeaderSizeWithoutExtensions)
	switch self.ipVersion {
	case 4:
		writeIpv4Header(packet, IP_PROTOCOL_TCP, self.destinationIp, self.sourceIp)
	case 6:
		writeIpv6Header(packet, IP_PROTOCOL_TCP, self.destinationIp, self.sourceIp)
	}

	writeTcpHeader(packet[ipHeaderByteCount:], uint16(self.destinationPort), uint16(self.sourcePort), self.receiveSeq, self.sendSeq, tcpFlagAck, self.encodedWindowSize(), nil)

	tcpBytes := packet[ipHeaderByteCount : ipHeaderByteCount+TcpHeaderSizeWithoutExtensions]
	checksum := transportChecksum(IP_PROTOCOL_TCP, self.destinationIp, self.sourceIp, tcpBytes)
	binary.BigEndian.PutUint16(tcpBytes[16:18], checksum)

	return packet, nil
}

func (self *ConnectionState) FinAck() ([]byte, error) {
	var ipHeaderByteCount int
	switch self.ipVersion {
	case 4:
		ipHeaderByteCount = Ipv4HeaderSizeWithoutExtensions
	case 6:
		ipHeaderByteCount = Ipv6HeaderSize
	}

	packet := MessagePoolGet(ipHeaderByteCount + TcpHeaderSizeWithoutExtensions)
	switch self.ipVersion {
	case 4:
		writeIpv4Header(packet, IP_PROTOCOL_TCP, self.destinationIp, self.sourceIp)
	case 6:
		writeIpv6Header(packet, IP_PROTOCOL_TCP, self.destinationIp, self.sourceIp)
	}

	flags := tcpFlagFin | tcpFlagAck
	writeTcpHeader(packet[ipHeaderByteCount:], uint16(self.destinationPort), uint16(self.sourcePort), self.receiveSeq, self.sendSeq, flags, self.encodedWindowSize(), nil)

	tcpBytes := packet[ipHeaderByteCount : ipHeaderByteCount+TcpHeaderSizeWithoutExtensions]
	checksum := transportChecksum(IP_PROTOCOL_TCP, self.destinationIp, self.sourceIp, tcpBytes)
	binary.BigEndian.PutUint16(tcpBytes[16:18], checksum)

	return packet, nil
}
func (self *ConnectionState) RstAck() ([]byte, error) {
	var ipHeaderByteCount int
	switch self.ipVersion {
	case 4:
		ipHeaderByteCount = Ipv4HeaderSizeWithoutExtensions
	case 6:
		ipHeaderByteCount = Ipv6HeaderSize
	}

	packet := MessagePoolGet(ipHeaderByteCount + TcpHeaderSizeWithoutExtensions)
	switch self.ipVersion {
	case 4:
		writeIpv4Header(packet, IP_PROTOCOL_TCP, self.destinationIp, self.sourceIp)
	case 6:
		writeIpv6Header(packet, IP_PROTOCOL_TCP, self.destinationIp, self.sourceIp)
	}

	flags := tcpFlagRst | tcpFlagAck
	writeTcpHeader(packet[ipHeaderByteCount:], uint16(self.destinationPort), uint16(self.sourcePort), self.receiveSeq, self.sendSeq, flags, self.encodedWindowSize(), nil)

	tcpBytes := packet[ipHeaderByteCount : ipHeaderByteCount+TcpHeaderSizeWithoutExtensions]
	checksum := transportChecksum(IP_PROTOCOL_TCP, self.destinationIp, self.sourceIp, tcpBytes)
	binary.BigEndian.PutUint16(tcpBytes[16:18], checksum)

	return packet, nil
}
func (self *ConnectionState) DataPackets(payload []byte, n int, mtu int) ([][]byte, error) {
	var ipHeaderByteCount int
	switch self.ipVersion {
	case 4:
		ipHeaderByteCount = Ipv4HeaderSizeWithoutExtensions
	case 6:
		ipHeaderByteCount = Ipv6HeaderSize
	}

	headerByteCount := ipHeaderByteCount + TcpHeaderSizeWithoutExtensions
	if mtu <= headerByteCount {
		return nil, fmt.Errorf("mtu %d is too small for IP+TCP headers (%d bytes)", mtu, headerByteCount)
	}
	packetByteCount := mtu - headerByteCount
	if n <= packetByteCount {
		pkt := self.tcpPacket(payload[0:n], self.receiveSeq)
		return [][]byte{pkt}, nil
	}
	packets := make([][]byte, 0, (n+packetByteCount-1)/packetByteCount)
	for i := 0; i < n; {
		j := min(i+packetByteCount, n)
		packets = append(packets, self.tcpPacket(payload[i:j], self.receiveSeq+uint32(i)))
		i = j
	}
	return packets, nil
}

func (self *ConnectionState) tcpPacket(payload []byte, seq uint32) []byte {
	var ipHeaderByteCount int
	switch self.ipVersion {
	case 4:
		ipHeaderByteCount = Ipv4HeaderSizeWithoutExtensions
	case 6:
		ipHeaderByteCount = Ipv6HeaderSize
	}

	tcpHeaderByteCount := TcpHeaderSizeWithoutExtensions
	totalLen := ipHeaderByteCount + tcpHeaderByteCount + len(payload)
	packet := MessagePoolGet(totalLen)
	switch self.ipVersion {
	case 4:
		writeIpv4Header(packet, IP_PROTOCOL_TCP, self.destinationIp, self.sourceIp)
	case 6:
		writeIpv6Header(packet, IP_PROTOCOL_TCP, self.destinationIp, self.sourceIp)
	}

	writeTcpHeader(packet[ipHeaderByteCount:], uint16(self.destinationPort), uint16(self.sourcePort), seq, self.sendSeq, tcpFlagAck, self.encodedWindowSize(), nil)
	copy(packet[ipHeaderByteCount+tcpHeaderByteCount:], payload)

	// checksum covers the full segment (header + payload), not just the header
	tcpBytes := packet[ipHeaderByteCount:]
	checksum := transportChecksum(IP_PROTOCOL_TCP, self.destinationIp, self.sourceIp, tcpBytes)
	binary.BigEndian.PutUint16(tcpBytes[16:18], checksum)

	return packet
}
func tcpFlagsString(tcp *parsedTcp) string {
	return tcp.flagsString()
}

func DefaultRemoteUserNatProviderSettings() *RemoteUserNatProviderSettings {
	return &RemoteUserNatProviderSettings{
		WriteTimeout:                  30 * time.Second,
		ProtocolVersion:               DefaultProtocolVersion,
		EgressSecurityPolicyGenerator: DefaultEgressSecurityPolicyWithStats,
	}
}

type RemoteUserNatProviderSettings struct {
	WriteTimeout time.Duration

	ProtocolVersion int

	EgressSecurityPolicyGenerator func(*SecurityPolicyStatsCollector) SecurityPolicy
}

type RemoteUserNatProvider struct {
	client            *Client
	localUserNat      *LocalUserNat
	securityPolicy    SecurityPolicy
	settings          *RemoteUserNatProviderSettings
	bw                *ProxyBandwidth
	localUserNatUnsub func()
	clientUnsub       func()
}

func NewRemoteUserNatProviderWithDefaults(
	client *Client,
	localUserNat *LocalUserNat,
	bw *ProxyBandwidth,
) *RemoteUserNatProvider {
	return NewRemoteUserNatProvider(client, localUserNat, bw, DefaultRemoteUserNatProviderSettings())
}

func NewRemoteUserNatProvider(
	client *Client,
	localUserNat *LocalUserNat,
	bw *ProxyBandwidth,
	settings *RemoteUserNatProviderSettings,
) *RemoteUserNatProvider {
	userNatProvider := &RemoteUserNatProvider{
		client:         client,
		localUserNat:   localUserNat,
		securityPolicy: settings.EgressSecurityPolicyGenerator(DefaultSecurityPolicyStatsCollector()),
		settings:       settings,
		bw:             bw,
	}

	localUserNatUnsub := localUserNat.AddReceivePacketCallback(userNatProvider.Receive)
	userNatProvider.localUserNatUnsub = localUserNatUnsub
	clientUnsub := client.AddReceiveCallback(userNatProvider.ClientReceive)
	userNatProvider.clientUnsub = clientUnsub

	return userNatProvider
}

func (self *RemoteUserNatProvider) SecurityPolicyStats(reset bool) SecurityPolicyStats {
	return self.securityPolicy.Stats().Stats(reset)
}

// `ReceivePacketFunction`
func (self *RemoteUserNatProvider) Receive(
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipPath *IpPath,
	packet []byte,
) {
	if self.bw != nil {
		self.bw.BillableRx.Add(uint64(len(packet)))
	}
	// self.client.log.Infof("[trace]provider return packet for %s\n", source.SourceId)

	if self.client.ClientId() == source.SourceId {
		// locally generated traffic should use a separate local user nat
		self.client.log.V(2).Infof("drop remote user nat provider s packet ->%s\n", source.SourceId)
		return
	}

	ipPacketFromProvider := &protocol.IpPacketFromProvider{
		IpPacket: &protocol.IpPacket{
			PacketBytes: MessagePoolShareReadOnly(packet),
		},
	}
	frame, err := ToFrame(ipPacketFromProvider, self.settings.ProtocolVersion)
	if err != nil {
		self.client.log.V(2).Infof("drop remote user nat provider s packet ->%s = %s\n", source.SourceId, err)
		panic(err)
	}
	if !frame.Raw {
		defer MessagePoolReturn(ipPacketFromProvider.IpPacket.PacketBytes)
	}

	opts := []any{
		CompanionContract(),
	}
	// note udp is sent with ack because because otherwise the delivery reliability will mulitply with the egress
	c := func() bool {
		// ack := make(chan error)
		sent := self.client.SendWithTimeout(
			frame,
			source.Reverse(),
			func(err error) {},
			self.settings.WriteTimeout,
			opts...,
		)
		// if sent {
		// 	self.client.log.Infof("[trace]provider return packet sent for %s\n", source.SourceId)
		// }
		return sent
	}
	if self.client.log.V(2).Enabled() {
		TraceWithReturn(
			fmt.Sprintf("[unps]%s %s->%s s(%s)", ipPath.Protocol, self.client.ClientTag(), source.SourceId, source.StreamId),
			c,
		)
	} else {
		c()
	}

}

// `connect.ReceiveFunction`
func (self *RemoteUserNatProvider) ClientReceive(source TransferPath, frames []*protocol.Frame, peer Peer) {
	for _, frame := range frames {
		switch frame.MessageType {
		case protocol.MessageType_IpIpPing:
			self.client.log.V(1).Infof("[ip]provider ping <- %s(%d)\n", source, peer.ProvideMode)
			// echo back over a companion contract, like the provider's other
			// return traffic; the source only provides ProvideMode_Stream, so a
			// forward contract here would be rejected (no permission).
			self.client.SendWithTimeout(
				frame,
				source.Reverse(),
				func(err error) {},
				self.settings.WriteTimeout,
				CompanionContract(),
			)
		case protocol.MessageType_IpIpPacketToProvider:
			ipPacketToProvider_, err := FromFrame(frame)
			if err != nil {
				panic(err)
			}
			ipPacketToProvider := ipPacketToProvider_.(*protocol.IpPacketToProvider)
			if self.bw != nil {
				self.bw.BillableTx.Add(uint64(len(ipPacketToProvider.IpPacket.PacketBytes)))
			}

			ipPath, payload, err := ParseIpPathWithPayload(ipPacketToProvider.IpPacket.PacketBytes)
			if err == nil {
				r, err := self.securityPolicy.Inspect(peer.ProvideMode, ipPath, payload)
				if err == nil {
					switch r {
					case SecurityPolicyResultAllow:
						c := func() bool {
							var packet []byte
							if frame.Raw {
								packet = MessagePoolShareReadOnly(ipPacketToProvider.IpPacket.PacketBytes)
							} else {
								packet = MessagePoolCopy(ipPacketToProvider.IpPacket.PacketBytes)
							}
							// self.client.log.Infof("[trace]provider send packet from %s\n", source.SourceId)
							success := self.localUserNat.SendPacketWithTimeout(
								source,
								peer.ProvideMode,
								packet,
								self.settings.WriteTimeout,
							)
							if !success {
								MessagePoolReturn(packet)
							}
							return success
						}
						if self.client.log.V(2).Enabled() {
							TraceWithReturn(
								fmt.Sprintf("[unpr] %s<-%s s(%s)", self.client.ClientTag(), source.SourceId, source.StreamId),
								c,
							)
						} else {
							c()
						}
					case SecurityPolicyResultIncident:
						self.client.ReportAbuse(source)
					}
				}
			}
		}
	}
}

func (self *RemoteUserNatProvider) Close() {
	// self.client.RemoveReceiveCallback(self.clientCallbackId)
	// self.localUserNat.RemoveReceivePacketCallback(self.localUserNatCallbackId)
	self.clientUnsub()
	self.localUserNatUnsub()
}

// this is a basic implementation. See `RemoteUserNatWindowedClient` for a more robust implementation
type RemoteUserNatClient struct {
	client                *Client
	receivePacketCallback ReceivePacketFunction
	securityPolicy        SecurityPolicy
	pathTable             *pathTable
	// the provide mode of the source packets
	// for locally generated packets this is `ProvideMode_Network`
	provideMode       protocol.ProvideMode
	localUserNat      *LocalUserNat
	closeCallback     func()
	clientUnsub       func()
	localUserNatUnsub func()

	stateLock           sync.Mutex
	allowDirect         bool
	localSecurityBypass bool
}

func NewRemoteUserNatClient(
	client *Client,
	receivePacketCallback ReceivePacketFunction,
	destinations []MultiHopId,
	provideMode protocol.ProvideMode,
) *RemoteUserNatClient {
	return NewRemoteUserNatClientWithClose(client, receivePacketCallback, destinations, provideMode, nil)
}

func NewRemoteUserNatClientWithClose(
	client *Client,
	receivePacketCallback ReceivePacketFunction,
	destinations []MultiHopId,
	provideMode protocol.ProvideMode,
	closeCallback func(),
) *RemoteUserNatClient {
	pathTable := newPathTable(destinations)

	localUserNatSettings := DefaultLocalUserNatSettings()
	// no ulimit for local traffic
	localUserNatSettings.UdpBufferSettings.UserLimit = 0
	localUserNatSettings.TcpBufferSettings.UserLimit = 0
	localUserNat := NewLocalUserNat(client.Ctx(), "remote local", nil, localUserNatSettings)

	userNatClient := &RemoteUserNatClient{
		client:                client,
		receivePacketCallback: receivePacketCallback,
		securityPolicy:        DefaultEgressSecurityPolicy(),
		pathTable:             pathTable,
		provideMode:           provideMode,
		localUserNat:          localUserNat,
		closeCallback:         closeCallback,
	}

	clientUnsub := client.AddReceiveCallback(userNatClient.ClientReceive)
	userNatClient.clientUnsub = clientUnsub

	userNatClient.localUserNatUnsub = localUserNat.AddReceivePacketCallback(receivePacketCallback)

	return userNatClient
}

func (self *RemoteUserNatClient) Destinations() []MultiHopId {
	return self.pathTable.Destinations()
}

func (self *RemoteUserNatClient) DestinationIds() []Id {
	return self.pathTable.DestinationIds()
}

func (self *RemoteUserNatClient) SecurityPolicyStats(reset bool) SecurityPolicyStats {
	return self.securityPolicy.Stats().Stats(reset)
}

// `SendPacketFunction`
func (self *RemoteUserNatClient) SendPacket(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
	minRelationship := max(provideMode, self.provideMode)

	ipPath, payload, err := ParseIpPathWithPayload(packet)
	if err != nil {
		return false
	}
	r, err := self.securityPolicy.Inspect(minRelationship, ipPath, payload)
	if err != nil {
		return false
	}

	switch r {
	case SecurityPolicyResultAllow:
		destination, err := self.pathTable.SelectDestination(packet)
		if err != nil {
			// drop
			return false
		}

		ipPacketToProvider := &protocol.IpPacketToProvider{
			IpPacket: &protocol.IpPacket{
				PacketBytes: MessagePoolShareReadOnly(packet),
			},
		}
		frame, err := ToFrame(ipPacketToProvider, DefaultProtocolVersion)
		if err != nil {
			panic(err)
		}
		if !frame.Raw {
			defer MessagePoolReturn(packet)
		}

		// the sender will control transfer
		opts := []any{}
		// note udp is sent with ack because because otherwise the delivery reliability will mulitply with the egress
		success := self.client.SendMultiHopWithTimeout(frame, destination, func(err error) {}, timeout, opts...)
		return success
	case SecurityPolicyResultDrop:
		if self.LocalSecurityBypass() {
			return self.localUserNat.SendPacket(source, provideMode, packet, timeout)
		} else {
			return false
		}
	default:
		return false
	}
}

// `connect.ReceiveFunction`
func (self *RemoteUserNatClient) ClientReceive(source TransferPath, frames []*protocol.Frame, peer Peer) {
	// only process frames from the destinations
	// if allow := self.sourceFilter[source]; !allow {
	//     return
	// }

	for _, frame := range frames {
		// self.log.Infof("[trace]receive frame %s\n", frame.MessageType)
		switch frame.MessageType {
		case protocol.MessageType_IpIpPacketFromProvider:
			ipPacketFromProvider_, err := FromFrame(frame)
			if err != nil {
				panic(err)
			}
			ipPacketFromProvider := ipPacketFromProvider_.(*protocol.IpPacketFromProvider)

			packet := ipPacketFromProvider.IpPacket.PacketBytes

			ipPath, err := ParseIpPath(packet)
			if err == nil {
				HandleError(func() {
					self.receivePacketCallback(
						source,
						peer.ProvideMode,
						ipPath,
						packet,
					)
				})
			}
			// else not an ip packet, drop
		}
	}
}

func (self *RemoteUserNatClient) Shuffle() {
}

func (self *RemoteUserNatClient) Close() {
	// self.client.RemoveReceiveCallback(self.clientCallbackId)
	self.localUserNat.Close()
	self.localUserNatUnsub()
	self.clientUnsub()
	if self.closeCallback != nil {
		self.closeCallback()
	}
}

func (self *RemoteUserNatClient) SetAllowDirect(allowDirect bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.allowDirect = allowDirect
}

func (self *RemoteUserNatClient) AllowDirect() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.allowDirect
}

func (self *RemoteUserNatClient) SetLocalSecurityBypass(localSecurityBypass bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.localSecurityBypass = localSecurityBypass
}

func (self *RemoteUserNatClient) LocalSecurityBypass() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.localSecurityBypass
}

type pathTable struct {
	destinations []MultiHopId

	// TODO clean up entries that haven't been used in some time
	paths4 map[Ip4Path]MultiHopId
	paths6 map[Ip6Path]MultiHopId
}

func newPathTable(destinations []MultiHopId) *pathTable {
	return &pathTable{
		destinations: destinations,
		paths4:       map[Ip4Path]MultiHopId{},
		paths6:       map[Ip6Path]MultiHopId{},
	}
}

func (self *pathTable) Destinations() []MultiHopId {
	return slices.Clone(self.destinations)
}

func (self *pathTable) DestinationIds() []Id {
	var clientIds []Id
	for _, destination := range self.destinations {
		clientIds = append(clientIds, destination.Tail())
	}
	return clientIds
}

func (self *pathTable) SelectDestination(packet []byte) (MultiHopId, error) {
	if len(self.destinations) == 0 {
		return MultiHopId{}, fmt.Errorf("No destinations")
	}
	if len(self.destinations) == 1 {
		return self.destinations[0], nil
	}

	ipPath, err := ParseIpPath(packet)
	if err != nil {
		return MultiHopId{}, err
	}
	switch ipPath.Version {
	case 4:
		ip4Path := ipPath.ToIp4Path()
		if destination, ok := self.paths4[ip4Path]; ok {
			return destination, nil
		}
		i := mathrand.Intn(len(self.destinations))
		destination := self.destinations[i]
		self.paths4[ip4Path] = destination
		return destination, nil
	case 6:
		ip6Path := ipPath.ToIp6Path()
		if destination, ok := self.paths6[ip6Path]; ok {
			return destination, nil
		}
		i := mathrand.Intn(len(self.destinations))
		destination := self.destinations[i]
		self.paths6[ip6Path] = destination
		return destination, nil
	default:
		// no support for this version
		return MultiHopId{}, fmt.Errorf("No support for ip version %d", ipPath.Version)
	}
}

type IpProtocol int

const (
	IpProtocolUnknown IpProtocol = 0
	IpProtocolTcp     IpProtocol = 1
	IpProtocolUdp     IpProtocol = 2
)

func (self IpProtocol) String() string {
	switch self {
	case IpProtocolTcp:
		return "tcp"
	case IpProtocolUdp:
		return "udp"
	default:
		return "unknown"
	}
}

type IpPath struct {
	Version         int
	Protocol        IpProtocol
	SourceIp        net.IP
	SourcePort      int
	DestinationIp   net.IP
	DestinationPort int

	SequenceNumber uint32
	Syn            bool
	Rst            bool
	Ack            bool
}

func ParseIpPath(ipPacket []byte) (*IpPath, error) {
	ipPath, _, err := ParseIpPathWithPayload(ipPacket)
	return ipPath, err
}

func ParseIpPathWithPayload(ipPacket []byte) (*IpPath, []byte, error) {
	if len(ipPacket) == 0 {
		return nil, nil, fmt.Errorf("Empty packet.")
	}
	ipVersion := uint8(ipPacket[0]) >> 4
	switch ipVersion {
	case 4:
		ipProtocol4, sourceIp4, destinationIp4, transport, ok := parseIpv4(ipPacket)
		if !ok {
			return nil, nil, fmt.Errorf("No support for protocol")
		}

		// copy the ips so the ip path can be retained independently of the
		// shared packet buffer (which is recycled after the handoff call). both
		// copies share one backing allocation instead of one per address.
		ipBacking := make(net.IP, len(sourceIp4)+len(destinationIp4))
		sn := copy(ipBacking, sourceIp4)
		copy(ipBacking[sn:], destinationIp4)
		sourceIpCopy := ipBacking[:sn:sn]
		destinationIpCopy := ipBacking[sn:]

		switch ipProtocol4 {
		case IP_PROTOCOL_UDP:
			var udpPacket parsedUdp
			if !parseUdpPacket(sourceIp4, destinationIp4, transport, &udpPacket) {
				return nil, nil, fmt.Errorf("No support for protocol")
			}

			return &IpPath{
				Version:         int(ipVersion),
				Protocol:        IpProtocolUdp,
				SourceIp:        sourceIpCopy,
				SourcePort:      int(udpPacket.sourcePort),
				DestinationIp:   destinationIpCopy,
				DestinationPort: int(udpPacket.destinationPort),
			}, udpPacket.payload, nil
		case IP_PROTOCOL_TCP:
			var tcpPacket parsedTcp
			if !parseTcpPacket(sourceIp4, destinationIp4, transport, &tcpPacket) {
				return nil, nil, fmt.Errorf("No support for protocol")
			}

			return &IpPath{
				Version:         int(ipVersion),
				Protocol:        IpProtocolTcp,
				SourceIp:        sourceIpCopy,
				SourcePort:      int(tcpPacket.sourcePort),
				DestinationIp:   destinationIpCopy,
				DestinationPort: int(tcpPacket.destinationPort),
				SequenceNumber:  tcpPacket.seq,
				Syn:             tcpPacket.syn,
				Rst:             tcpPacket.rst,
				Ack:             tcpPacket.ack,
			}, tcpPacket.payload, nil
		default:
			// no support for this protocol
			return nil, nil, fmt.Errorf("No support for protocol %d", ipProtocol4)
		}
	case 6:
		ipProtocol6, sourceIp6, destinationIp6, transport, ok := parseIpv6(ipPacket)
		if !ok {
			return nil, nil, fmt.Errorf("No support for protocol")
		}

		// copy the ips so the ip path can be retained independently of the
		// shared packet buffer (which is recycled after the handoff call). both
		// copies share one backing allocation instead of one per address.
		ipBacking := make(net.IP, len(sourceIp6)+len(destinationIp6))
		sn := copy(ipBacking, sourceIp6)
		copy(ipBacking[sn:], destinationIp6)
		sourceIpCopy := ipBacking[:sn:sn]
		destinationIpCopy := ipBacking[sn:]

		switch ipProtocol6 {
		case IP_PROTOCOL_UDP:
			var udpPacket parsedUdp
			if !parseUdpPacket(sourceIp6, destinationIp6, transport, &udpPacket) {
				return nil, nil, fmt.Errorf("No support for protocol")
			}

			return &IpPath{
				Version:         int(ipVersion),
				Protocol:        IpProtocolUdp,
				SourceIp:        sourceIpCopy,
				SourcePort:      int(udpPacket.sourcePort),
				DestinationIp:   destinationIpCopy,
				DestinationPort: int(udpPacket.destinationPort),
			}, udpPacket.payload, nil
		case IP_PROTOCOL_TCP:
			var tcpPacket parsedTcp
			if !parseTcpPacket(sourceIp6, destinationIp6, transport, &tcpPacket) {
				return nil, nil, fmt.Errorf("No support for protocol")
			}

			return &IpPath{
				Version:         int(ipVersion),
				Protocol:        IpProtocolTcp,
				SourceIp:        sourceIpCopy,
				SourcePort:      int(tcpPacket.sourcePort),
				DestinationIp:   destinationIpCopy,
				DestinationPort: int(tcpPacket.destinationPort),
				SequenceNumber:  tcpPacket.seq,
				Syn:             tcpPacket.syn,
				Rst:             tcpPacket.rst,
				Ack:             tcpPacket.ack,
			}, tcpPacket.payload, nil
		default:
			// no support for this protocol
			return nil, nil, fmt.Errorf("No support for protocol %d", ipProtocol6)
		}
	default:
		// no support for this version
		return nil, nil, fmt.Errorf("No support for ip version %d", ipVersion)
	}
}

// func (self *IpPath) Copy() *IpPath {
// 	sourceIpCopy := make(net.IP, len(self.SourceIp))
// 	copy(sourceIpCopy, self.SourceIp)

// 	destinationIpCopy := make(net.IP, len(self.DestinationIp))
// 	copy(destinationIpCopy, self.DestinationIp)

// 	return &IpPath{
// 		Version:         self.Version,
// 		Protocol:        self.Protocol,
// 		SourceIp:        sourceIpCopy,
// 		SourcePort:      self.SourcePort,
// 		DestinationIp:   destinationIpCopy,
// 		DestinationPort: self.DestinationPort,
// 	}
// }

func (self *IpPath) SourceHostPort() string {
	return net.JoinHostPort(
		self.SourceIp.String(),
		strconv.Itoa(self.SourcePort),
	)
}

func (self *IpPath) DestinationHostPort() string {
	return net.JoinHostPort(
		self.DestinationIp.String(),
		strconv.Itoa(self.DestinationPort),
	)
}

func (self *IpPath) ToIp4Path() Ip4Path {
	var sourceIp [4]byte
	if self.SourceIp != nil {
		if sourceIp4 := self.SourceIp.To4(); sourceIp4 != nil {
			sourceIp = [4]byte(sourceIp4)
		}
	}
	var destinationIp [4]byte
	if self.DestinationIp != nil {
		if destinationIp4 := self.DestinationIp.To4(); destinationIp4 != nil {
			destinationIp = [4]byte(destinationIp4)
		}
	}
	return Ip4Path{
		Protocol:        self.Protocol,
		SourceIp:        sourceIp,
		SourcePort:      self.SourcePort,
		DestinationIp:   destinationIp,
		DestinationPort: self.DestinationPort,
	}
}

func (self *IpPath) ToIp6Path() Ip6Path {
	var sourceIp [16]byte
	if self.SourceIp != nil {
		if sourceIp6 := self.SourceIp.To16(); sourceIp6 != nil {
			sourceIp = [16]byte(sourceIp6)
		}
	}
	var destinationIp [16]byte
	if self.DestinationIp != nil {
		if destinationIp6 := self.DestinationIp.To16(); destinationIp6 != nil {
			destinationIp = [16]byte(destinationIp6)
		}
	}
	return Ip6Path{
		Protocol:        self.Protocol,
		SourceIp:        sourceIp,
		SourcePort:      self.SourcePort,
		DestinationIp:   destinationIp,
		DestinationPort: self.DestinationPort,
	}
}

func (self *IpPath) Source() *IpPath {
	return &IpPath{
		Protocol:   self.Protocol,
		Version:    self.Version,
		SourceIp:   self.SourceIp,
		SourcePort: self.SourcePort,
	}
}

func (self *IpPath) Destination() *IpPath {
	return &IpPath{
		Protocol:        self.Protocol,
		Version:         self.Version,
		DestinationIp:   self.DestinationIp,
		DestinationPort: self.DestinationPort,
	}
}

func (self *IpPath) Reverse() *IpPath {
	return &IpPath{
		Protocol:        self.Protocol,
		Version:         self.Version,
		SourceIp:        self.DestinationIp,
		SourcePort:      self.DestinationPort,
		DestinationIp:   self.SourceIp,
		DestinationPort: self.SourcePort,
	}
}

// comparable
type Ip4Path struct {
	Protocol        IpProtocol
	SourceIp        [4]byte
	SourcePort      int
	DestinationIp   [4]byte
	DestinationPort int
}

func (self *Ip4Path) Source() Ip4Path {
	return Ip4Path{
		Protocol:   self.Protocol,
		SourceIp:   self.SourceIp,
		SourcePort: self.SourcePort,
	}
}

func (self *Ip4Path) Destination() Ip4Path {
	return Ip4Path{
		Protocol:        self.Protocol,
		DestinationIp:   self.DestinationIp,
		DestinationPort: self.DestinationPort,
	}
}

// comparable
type Ip6Path struct {
	Protocol        IpProtocol
	SourceIp        [16]byte
	SourcePort      int
	DestinationIp   [16]byte
	DestinationPort int
}

func (self *Ip6Path) Source() Ip6Path {
	return Ip6Path{
		Protocol:   self.Protocol,
		SourceIp:   self.SourceIp,
		SourcePort: self.SourcePort,
	}
}

func (self *Ip6Path) Destination() Ip6Path {
	return Ip6Path{
		Protocol:        self.Protocol,
		DestinationIp:   self.DestinationIp,
		DestinationPort: self.DestinationPort,
	}
}

type UserLimited interface {
	LastActivityTime() time.Time
	Cancel()
}

type userLimited struct {
	mutex            sync.Mutex
	lastActivityTime time.Time
}

func newUserLimited() *userLimited {
	return &userLimited{
		lastActivityTime: time.Now(),
	}
}

func (self *userLimited) LastActivityTime() time.Time {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.lastActivityTime
}

func (self *userLimited) UpdateLastActivityTime() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.lastActivityTime = time.Now()
}

func applyLruUserLimit[R UserLimited](resources []R, ulimit int, limitCallback func(R) bool) {
	// limit the total connections per source to avoid blowing up the ulimit
	if n := len(resources) - ulimit; 0 < n {
		resourceLastActivityTimes := map[UserLimited]time.Time{}
		for _, resource := range resources {
			resourceLastActivityTimes[resource] = resource.LastActivityTime()
		}
		// order by last activity time
		slices.SortFunc(resources, func(a R, b R) int {
			lastActivityTimeA := resourceLastActivityTimes[a]
			lastActivityTimeB := resourceLastActivityTimes[b]
			if lastActivityTimeA.Before(lastActivityTimeB) {
				return -1
			} else if lastActivityTimeB.Before(lastActivityTimeA) {
				return 1
			} else {
				return 0
			}
		})
		i := 0
		for _, resource := range resources {
			if limitCallback(resource) {
				i += 1
				resource.Cancel()
			}
			if n <= i {
				break
			}
		}
	}
}
