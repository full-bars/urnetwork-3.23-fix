package connect

// Stateful, all-ports BitTorrent / unsanctioned-encrypted egress detector.
//
// This is the deep-packet-inspection backstop the port rules in ip_security.go
// were always waiting for (see the "better deep packet inspection" FIXMEs). It
// advances a small state machine over the first payload-bearing packets of each
// egress flow until it reaches a terminal verdict:
//
//   - a positive, plaintext BitTorrent signature  -> Incident (report) + Drop
//   - an initial payload that looks fully encrypted/random AND is NOT a
//     whitelisted web standard (TLS, DTLS, QUIC, STUN/TURN) -> Drop
//   - anything else (plaintext unknown protocol, a web standard, or budget
//     exhausted without a hit) -> Allow
//
// Packets are allowed through while the flow is still INSPECTING; enforcement
// only begins once a terminal verdict is reached. Detection is keyed off the
// outbound (client->destination) direction.
//
// Everything here is a clean-room implementation from the public protocol
// definitions: BitTorrent BEP 3 (peer wire / HTTP tracker), BEP 5 (DHT KRPC),
// BEP 15 (UDP tracker), BEP 29 (uTP); TLS RFC 8446/5246; DTLS RFC 6347/9147;
// QUIC RFC 9000/9369; STUN RFC 5389. The byte signatures are protocol facts,
// not derived from any third-party implementation.

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/bits"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/exp/maps"
)

// number of flow-table shards. Sharding keeps the per-packet table lookup off a
// single global lock on the hot path.
const dmcaFlowShards = 16

const (
	// Two packets sharing the 32-bit SSRC and payload type, with coherent
	// sequence/timestamp movement, provide far stronger evidence than RFC
	// 7983's broad first-byte RTP range by itself.
	rtpValidationPackets = 2
	rtpMaxSequenceGap    = 64
)

// DmcaSecurityPolicySettings holds every threshold and decision input for the
// egress BitTorrent detector, so behavior can be tuned (and later driven by a
// control message) without code changes. Use DefaultDmcaSecurityPolicySettings
// for reasonable defaults.
type DmcaSecurityPolicySettings struct {
	// Enabled turns the payload detector on. When false the egress policy keeps
	// only its port/ip rules and never inspects payloads.
	Enabled bool

	// LogOnly evaluates the state machine and records stats but never converts a
	// detection into Drop/Incident. Use to measure false positives before
	// enforcing.
	LogOnly bool

	// DropBittorrentSignature enforces on flows matching a positive, plaintext
	// BitTorrent signature.
	DropBittorrentSignature bool
	// ReportBittorrentIncident returns SecurityPolicyResultIncident (ReportAbuse)
	// rather than a silent Drop for signature matches.
	ReportBittorrentIncident bool

	// DropUnsanctionedEncrypted enforces on flows whose initial payload looks
	// fully encrypted/random and are not positively identified as a whitelisted
	// web or communication standard. This is the heuristic backstop for
	// obfuscated BitTorrent (MSE/PE over TCP, or encrypted uTP over UDP).
	DropUnsanctionedEncrypted bool

	// Gaming configures provider-scoped gaming exceptions. These are evaluated
	// after positive BitTorrent signatures but before the encrypted heuristic.
	// Nil disables all gaming exceptions.
	Gaming *GamingSecurityPolicySettings

	// Provider-scoped messaging exceptions (WhatsApp on Meta's own address
	// space). They are evaluated after positive BitTorrent signatures and the
	// application standards, as the backstop for the WhatsApp Noise detector,
	// but before the encrypted heuristic, and an allowed flow keeps checking
	// the signatures for its whole inspection budget. Nil disables all
	// messaging exceptions.
	Messaging *MessagingSecurityPolicySettings

	// App configures the positive application-standard detectors (WireGuard,
	// OpenVPN, RTMP, Levin, RakNet, Ethereum discovery v4 and RLPx, WhatsApp
	// Noise). They are evaluated after the BitTorrent signatures, the gaming
	// exceptions and the web standards. Nil disables them.
	App *AppStandardSettings

	// InspectPrivilegedSignatures checks the stateless BitTorrent signatures on
	// privileged destination ports (<1024), which are otherwise allowed without
	// inspection. No flow state and no entropy heuristic run there.
	InspectPrivilegedSignatures bool

	// InspectionPacketBudget is the max number of payload-bearing packets to
	// inspect for a flow before giving up and treating it as not-BitTorrent.
	InspectionPacketBudget int
	// EncryptedDecisionPackets is how many consecutive encrypted-looking,
	// otherwise-unidentified payload packets are required before the encrypted
	// heuristic fires (capped by InspectionPacketBudget).
	EncryptedDecisionPackets int
	// MaxInspectionPayload caps how many leading payload bytes are examined.
	MaxInspectionPayload int

	// MinEncryptedPayload is the minimum payload length before the entropy
	// heuristic will classify a payload as encrypted (short payloads are
	// statistically unreliable and treated as inconclusive).
	MinEncryptedPayload int
	// EncryptedPopcountBand is the max distance from 0.5 of the fraction of set
	// bits for a payload to be considered random (0.10 => [0.40, 0.60]).
	EncryptedPopcountBand float64
	// EncryptedMaxPrintableFraction is the max fraction of printable-ASCII bytes
	// allowed for a payload to be considered encrypted (text protocols are mostly
	// printable; ciphertext is mostly not).
	EncryptedMaxPrintableFraction float64
	// EncryptedMinNormalizedEntropy is the min Shannon entropy (normalized to the
	// sample size, 0..1) for a payload to be considered encrypted.
	EncryptedMinNormalizedEntropy float64

	// MaxFlows bounds the total tracked flows for memory; the oldest are evicted
	// first. 0 disables the bound.
	MaxFlows int
}

func DefaultDmcaSecurityPolicySettings() *DmcaSecurityPolicySettings {
	settings := &DmcaSecurityPolicySettings{
		Enabled:                       true,
		LogOnly:                       false,
		DropBittorrentSignature:       true,
		ReportBittorrentIncident:      true,
		DropUnsanctionedEncrypted:     true,
		Gaming:                        DefaultGamingSecurityPolicySettings(),
		Messaging:                     DefaultMessagingSecurityPolicySettings(),
		App:                           DefaultAppStandardSettings(),
		InspectPrivilegedSignatures:   true,
		InspectionPacketBudget:        8,
		EncryptedDecisionPackets:      3,
		MaxInspectionPayload:          512,
		MinEncryptedPayload:           32,
		EncryptedPopcountBand:         0.10,
		EncryptedMaxPrintableFraction: 0.50,
		EncryptedMinNormalizedEntropy: 0.85,
		MaxFlows:                      65536,
	}
	applyDpiAdmitsEnvToDmca(settings)
	return settings
}

type dmcaVerdict int32

const (
	dmcaInspecting    dmcaVerdict = 0
	dmcaAllow         dmcaVerdict = 1
	dmcaDropEncrypted dmcaVerdict = 2
	dmcaBittorrent    dmcaVerdict = 3
)

// dmcaFlowState is the per-flow state machine. It implements UserLimited so the
// shared applyLruUserLimit eviction can bound memory.
type dmcaFlowState struct {
	// atomic; first field for 64-bit alignment on 32-bit architectures
	lastActivityUnixNanos int64
	// atomic dmcaVerdict; the terminal-verdict fast path reads this without
	// taking mu, so steady-state packets on a decided flow are lock-free
	terminal int32
	// atomic SecurityPolicyReason for the terminal verdict. Stored before
	// terminal, so a reader that observes a terminal verdict observes its reason.
	terminalReason int32

	key Ip6Path

	// mu guards the inspection bookkeeping below, touched only while INSPECTING
	mu               sync.Mutex
	inspectedPackets int
	encryptedPackets int
	sawObservation   bool
	sawFlowStart     bool
	sawPlaintext     bool
	rtpCandidates    [2]rtpCandidate
	// a pending two-packet application standard
	appCandidate appCandidate
	// the application standard that allowed the flow while it still checks the
	// BitTorrent signatures for the rest of its budget; unknown when none
	appReason SecurityPolicyReason
}

type rtpCandidate struct {
	ssrc        uint32
	timestamp   uint32
	sequence    uint16
	payloadType uint8
	packets     uint8
	used        bool
}

func (self *dmcaFlowState) LastActivityTime() time.Time {
	return time.Unix(0, atomic.LoadInt64(&self.lastActivityUnixNanos))
}

func (self *dmcaFlowState) Cancel() {
	// no async resources; eviction just drops the map entry
}

// setTerminal publishes the terminal verdict and its reason. It is called with
// mu held and returns the verdict as decided by this packet.
func (self *dmcaFlowState) setTerminal(v dmcaVerdict, reason SecurityPolicyReason) (dmcaVerdict, SecurityPolicyReason, bool) {
	atomic.StoreInt32(&self.terminalReason, int32(reason))
	atomic.StoreInt32(&self.terminal, int32(v))
	return v, reason, true
}

// terminalVerdict is the lock-free read of a decided flow.
func (self *dmcaFlowState) terminalVerdict() (dmcaVerdict, SecurityPolicyReason) {
	v := dmcaVerdict(atomic.LoadInt32(&self.terminal))
	if v == dmcaInspecting {
		return v, SecurityPolicyReasonInspecting
	}
	return v, SecurityPolicyReason(atomic.LoadInt32(&self.terminalReason))
}

// observeRtp validates continuity for up to two interleaved media sources (for
// example, audio and video). Out-of-order/duplicate packets do not destroy a
// promising candidate; an implausibly large forward jump starts probation over.
func (self *dmcaFlowState) observeRtp(header rtpHeader) bool {
	replacement := 0
	hasEmpty := false
	for i := range self.rtpCandidates {
		candidate := &self.rtpCandidates[i]
		if candidate.used && candidate.ssrc == header.ssrc && candidate.payloadType == header.payloadType {
			delta := uint16(header.sequence - candidate.sequence)
			switch {
			case delta == 0:
				// Duplicate.
				return false
			case 0x8000 <= delta:
				// Older/out-of-order under serial-number arithmetic.
				return false
			case rtpMaxSequenceGap < delta || 0x80000000 <= uint32(header.timestamp-candidate.timestamp):
				// Forward, but not a credible continuation.
				*candidate = newRtpCandidate(header)
				return false
			default:
				candidate.sequence = header.sequence
				candidate.timestamp = header.timestamp
				if candidate.packets < 0xff {
					candidate.packets++
				}
				return rtpValidationPackets <= candidate.packets
			}
		}

		if !candidate.used {
			replacement = i
			hasEmpty = true
		} else if !hasEmpty && candidate.packets < self.rtpCandidates[replacement].packets {
			replacement = i
		}
	}

	self.rtpCandidates[replacement] = newRtpCandidate(header)
	return false
}

func newRtpCandidate(header rtpHeader) rtpCandidate {
	return rtpCandidate{
		ssrc:        header.ssrc,
		timestamp:   header.timestamp,
		sequence:    header.sequence,
		payloadType: header.payloadType,
		packets:     1,
		used:        true,
	}
}

// advance moves the state machine forward by one packet and returns the current
// verdict, the reason for it, and whether this packet reached the terminal
// verdict. The payload is read synchronously and never retained, so the shared
// packet buffer is not aliased.
func (self *dmcaFlowState) advance(
	ipPath *IpPath,
	payload []byte,
	settings *DmcaSecurityPolicySettings,
	web *webStandardDetector,
	app *appStandardDetector,
) (dmcaVerdict, SecurityPolicyReason, bool) {
	self.mu.Lock()
	defer self.mu.Unlock()

	// recheck: another goroutine may have decided between the fast-path load and here
	if v, reason := self.terminalVerdict(); v != dmcaInspecting {
		return v, reason, false
	}

	if !self.sawObservation {
		self.sawObservation = true
		// we can only trust the encrypted heuristic if we observed the flow from
		// its start. For TCP that means we saw the SYN; for UDP the first datagram
		// of a 5-tuple is effectively the start.
		if IpProtocolTcp == ipPath.Protocol {
			self.sawFlowStart = ipPath.Syn
		} else {
			self.sawFlowStart = true
		}
	}

	// empty payloads (TCP SYN / pure ACK) carry no signal but keep the flow open
	if 0 == len(payload) {
		return dmcaInspecting, SecurityPolicyReasonInspecting, false
	}

	self.inspectedPackets += 1

	b := payload
	if settings.MaxInspectionPayload < len(b) {
		b = b[:settings.MaxInspectionPayload]
	}

	if detectBittorrentSignature(ipPath, b) {
		return self.setTerminal(dmcaBittorrent, SecurityPolicyReasonBittorrent)
	}
	if self.appReason != SecurityPolicyReasonUnknown {
		// allowed by an application standard: the BitTorrent signatures above keep
		// precedence for the rest of the budget, at any offset, then the allow
		// becomes terminal
		if anyOffsetBittorrentMarker(b) {
			return self.setTerminal(dmcaBittorrent, SecurityPolicyReasonBittorrent)
		}
		if settings.InspectionPacketBudget <= self.inspectedPackets {
			return self.setTerminal(dmcaAllow, self.appReason)
		}
		return dmcaAllow, self.appReason, false
	}
	if isSanctionedGamingEndpoint(settings.Gaming, ipPath) {
		// Provider prefix + transport + documented remote port is sufficient
		// evidence for the Steam exception. The BitTorrent checks keep precedence:
		// like an application standard the allow is not terminal until the
		// inspection budget is spent, and every packet is scanned at any offset.
		return self.allowAppStandard(ipPath, b, SecurityPolicyReasonAllowGaming, settings)
	}
	if reason, ok := web.matchReason(ipPath, payload); ok {
		// A sanctioned web/communication standard. Full framing is evaluated over
		// the complete payload; MaxInspectionPayload only caps signature and entropy
		// work below.
		return self.setTerminal(dmcaAllow, reason)
	}
	if reason, headerEnd, ok := app.match(ipPath, payload, 1 == self.inspectedPackets); ok {
		// a single-packet application standard, matched over the complete payload
		// (the Ethereum invariants cover every byte). The bytes it carries after
		// the recognized header must not hide a BitTorrent payload.
		rest := payload[headerEnd:]
		if settings.MaxInspectionPayload < len(rest) {
			rest = rest[:settings.MaxInspectionPayload]
		}
		if containsBittorrentSignature(rest) {
			return self.setTerminal(dmcaBittorrent, SecurityPolicyReasonBittorrent)
		}
		return self.allowAppStandard(ipPath, b, reason, settings)
	}
	if self.appCandidate.kind != appCandidateNone {
		candidate := self.appCandidate
		self.appCandidate = appCandidate{}
		reason, ok := app.confirm(&candidate, ipPath, payload)
		if ok {
			return self.allowAppStandard(ipPath, b, reason, settings)
		}
		if reason == SecurityPolicyReasonInspecting {
			// a WhatsApp stream prefix that continues in the next segment
			return self.holdAppCandidate(candidate, settings)
		}
		// a failed candidate is judged normally below (or reopens one)
	}
	if candidate, ok := app.open(ipPath, payload, 1 == self.inspectedPackets); ok {
		if anyOffsetBittorrentMarker(b) {
			return self.setTerminal(dmcaBittorrent, SecurityPolicyReasonBittorrent)
		}
		// The opening packet of a two-packet standard consumes budget but is not
		// counted as encrypted: the next packet confirms it or is judged normally.
		// A random flow whose blob happens to match an opener leaks one packet.
		return self.holdAppCandidate(candidate, settings)
	}
	if isSanctionedMessagingEndpoint(settings.Messaging, ipPath) {
		// Vendor prefix + transport + chat port admits the WhatsApp exception.
		// It runs after the application standards as the backstop for the
		// WhatsApp flows the Noise detector did not recognize. Unlike the Steam
		// exception the allow is not terminal at once: like an application
		// standard it keeps the BitTorrent signatures above in force for the
		// rest of the inspection budget.
		return self.allowAppStandard(ipPath, b, SecurityPolicyReasonAllowMessaging, settings)
	}
	if header, ok := web.rtpHeader(ipPath, payload); ok && self.observeRtp(header) {
		// RTP/SRTP needs coherent headers from multiple packets before it is trusted;
		// a single first byte in RFC 7983's 128-191 range is intentionally insufficient.
		return self.setTerminal(dmcaAllow, SecurityPolicyReasonAllowRtp)
	}
	if isHttpRequest(b) {
		// raw plaintext HTTP (a request line) - media/radio streaming relies on it,
		// including on non-standard ports. Allow definitively (and early, before any
		// budget/entropy bookkeeping) so a later high-entropy body never trips the
		// encrypted-traffic heuristic. Checked after the BitTorrent signatures so an
		// HTTP-tracker GET is still classified as BitTorrent.
		return self.setTerminal(dmcaAllow, SecurityPolicyReasonAllowHttp)
	}
	if payloadLooksEncrypted(b, settings) {
		self.encryptedPackets += 1
	} else if settings.MinEncryptedPayload <= len(b) {
		// long enough to judge and not random: an unidentified plaintext protocol
		self.sawPlaintext = true
	}
	// payloads too short to judge are inconclusive and only consume budget

	decisionPackets := settings.EncryptedDecisionPackets
	if settings.InspectionPacketBudget < decisionPackets {
		decisionPackets = settings.InspectionPacketBudget
	}

	switch {
	case self.sawPlaintext:
		// per policy only sketchy (encrypted) non-web-standard traffic is dropped
		return self.setTerminal(dmcaAllow, SecurityPolicyReasonAllowPlaintext)
	case settings.DropUnsanctionedEncrypted && self.sawFlowStart && decisionPackets <= self.encryptedPackets:
		return self.setTerminal(dmcaDropEncrypted, SecurityPolicyReasonDropEncrypted)
	case settings.InspectionPacketBudget <= self.inspectedPackets:
		return self.setTerminal(dmcaAllow, SecurityPolicyReasonAllowBudget)
	default:
		return dmcaInspecting, SecurityPolicyReasonInspecting, false
	}
}

// allowAppStandard allows the flow for an application standard. The allow is
// terminal once the inspection budget is spent; until then the flow keeps
// checking the BitTorrent signatures.
func (self *dmcaFlowState) allowAppStandard(ipPath *IpPath, b []byte, reason SecurityPolicyReason, settings *DmcaSecurityPolicySettings) (dmcaVerdict, SecurityPolicyReason, bool) {
	// no admit path may let a BitTorrent payload through behind a forged header
	if anyOffsetBittorrentMarker(b) {
		return self.setTerminal(dmcaBittorrent, SecurityPolicyReasonBittorrent)
	}
	if self.appReason == SecurityPolicyReasonUnknown {
		// counted here, not at the terminal verdict: the allow only becomes
		// terminal once the inspection budget is spent, which a short flow never
		// reaches
		recordDpiAppAdmit(ipPath, reason)
	}
	self.appReason = reason
	if settings.InspectionPacketBudget <= self.inspectedPackets {
		return self.setTerminal(dmcaAllow, reason)
	}
	return dmcaAllow, reason, false
}

// Keeps a pending application candidate. Its packet consumes budget but is not
// counted as encrypted, so the budget still ends the flow's inspection.
func (self *dmcaFlowState) holdAppCandidate(candidate appCandidate, settings *DmcaSecurityPolicySettings) (dmcaVerdict, SecurityPolicyReason, bool) {
	self.appCandidate = candidate
	if settings.InspectionPacketBudget <= self.inspectedPackets {
		return self.setTerminal(dmcaAllow, SecurityPolicyReasonAllowBudget)
	}
	return dmcaInspecting, SecurityPolicyReasonInspecting, false
}

type dmcaFlowShard struct {
	mu    sync.RWMutex
	flows map[Ip6Path]*dmcaFlowState
}

type dmcaDetector struct {
	settings    *DmcaSecurityPolicySettings
	web         *webStandardDetector
	app         *appStandardDetector
	perShardCap int
	shards      [dmcaFlowShards]*dmcaFlowShard
}

func newDmcaDetector(settings *DmcaSecurityPolicySettings, web *webStandardDetector) *dmcaDetector {
	perShardCap := 0
	if 0 < settings.MaxFlows {
		perShardCap = settings.MaxFlows / dmcaFlowShards
		if perShardCap < 1 {
			perShardCap = 1
		}
	}
	self := &dmcaDetector{
		settings:    settings,
		web:         web,
		app:         newAppStandardDetector(settings.App),
		perShardCap: perShardCap,
	}
	for i := range self.shards {
		self.shards[i] = &dmcaFlowShard{
			flows: map[Ip6Path]*dmcaFlowState{},
		}
	}
	return self
}

func dmcaShardIndex(key Ip6Path) int {
	// FNV-1a over the destination ip + ports; distribution, not security
	h := uint32(2166136261)
	for _, b := range key.DestinationIp {
		h = (h ^ uint32(b)) * 16777619
	}
	h = (h ^ uint32(key.SourcePort)) * 16777619
	h = (h ^ uint32(key.DestinationPort)) * 16777619
	return int(h % dmcaFlowShards)
}

// classify advances the per-flow state machine and returns the raw verdict
// (consulting the injected web-standards detector during inspection).
func (self *dmcaDetector) classify(ipPath *IpPath, payload []byte) dmcaVerdict {
	v, _, _ := self.classifyDetailed(ipPath, payload)
	return v
}

// classifyDetailed is classify plus the verdict reason and whether this packet
// moved the flow from inspecting to its terminal verdict.
func (self *dmcaDetector) classifyDetailed(ipPath *IpPath, payload []byte) (dmcaVerdict, SecurityPolicyReason, bool) {
	if !self.settings.Enabled {
		return dmcaAllow, SecurityPolicyReasonAllowUninspected, false
	}
	switch ipPath.Protocol {
	case IpProtocolTcp, IpProtocolUdp:
	default:
		return dmcaAllow, SecurityPolicyReasonAllowUninspected, false
	}

	// A privileged destination port (<1024) is trusted as a service port: it is allowed without
	// flow tracking or the encrypted heuristic, which lets legitimate non-web-standard encrypted
	// services there (e.g. Telegram MTProto or OpenVPN on 443) through. A peer can still listen on
	// a privileged port, so the stateless positive BitTorrent signatures run on every payload; TLS,
	// QUIC and HTTP fail their first comparison.
	if ipPath.DestinationPort < 1024 {
		if self.settings.InspectPrivilegedSignatures && 0 < len(payload) {
			b := payload
			if self.settings.MaxInspectionPayload < len(b) {
				b = b[:self.settings.MaxInspectionPayload]
			}
			if detectBittorrentSignature(ipPath, b) {
				return dmcaBittorrent, SecurityPolicyReasonBittorrent, true
			}
		}
		return dmcaAllow, SecurityPolicyReasonAllowPrivileged, false
	}

	key := ipPath.ToIp6Path()
	shard := self.shards[dmcaShardIndex(key)]

	shard.mu.RLock()
	st := shard.flows[key]
	shard.mu.RUnlock()
	if st == nil {
		shard.mu.Lock()
		st = shard.flows[key]
		if st == nil {
			st = &dmcaFlowState{
				key:                   key,
				lastActivityUnixNanos: time.Now().UnixNano(),
			}
			self.evictWithLock(shard)
			shard.flows[key] = st
		}
		shard.mu.Unlock()
	}

	atomic.StoreInt64(&st.lastActivityUnixNanos, time.Now().UnixNano())

	if v, reason := st.terminalVerdict(); v != dmcaInspecting {
		return v, reason, false
	}
	return st.advance(ipPath, payload, self.settings, self.web, self.app)
}

// inspect classifies the flow and maps the verdict to a SecurityPolicyResult via
// the policy settings. The egress policy switches on classify directly (to keep
// the bittorrent / web-standard / encrypted decision explicit); this is the
// convenience form for callers that only want the enforced result.
func (self *dmcaDetector) inspect(ipPath *IpPath, payload []byte) SecurityPolicyResult {
	return self.result(self.classify(ipPath, payload))
}

// evictWithLock drops the oldest flows so that inserting one more stays within
// the per-shard cap. Caller holds shard.mu for writing.
func (self *dmcaDetector) evictWithLock(shard *dmcaFlowShard) {
	if self.perShardCap <= 0 {
		return
	}
	if len(shard.flows) < self.perShardCap {
		return
	}
	applyLruUserLimit(maps.Values(shard.flows), self.perShardCap-1, func(st *dmcaFlowState) bool {
		delete(shard.flows, st.key)
		return true
	})
}

func (self *dmcaDetector) result(v dmcaVerdict) SecurityPolicyResult {
	switch v {
	case dmcaBittorrent:
		if self.settings.LogOnly || !self.settings.DropBittorrentSignature {
			return SecurityPolicyResultAllow
		}
		if self.settings.ReportBittorrentIncident {
			return SecurityPolicyResultIncident
		}
		return SecurityPolicyResultDrop
	case dmcaDropEncrypted:
		if self.settings.LogOnly || !self.settings.DropUnsanctionedEncrypted {
			return SecurityPolicyResultAllow
		}
		return SecurityPolicyResultDrop
	default:
		return SecurityPolicyResultAllow
	}
}

// --- positive BitTorrent signatures (clean-room from the BEPs) ---

// BEP 3 peer wire handshake: <0x13><"BitTorrent protocol"> (20 leading bytes of
// a 68-byte handshake). The pstr length byte 0x13 == 19 == len("BitTorrent protocol").
var bittorrentHandshakePrefix = []byte("\x13BitTorrent protocol")

func hasBittorrentHandshake(b []byte) bool {
	return bytes.HasPrefix(b, bittorrentHandshakePrefix)
}

// BEP 3 HTTP tracker: a GET to an /announce or /scrape endpoint carrying an
// info_hash query parameter.
func hasHttpTrackerRequest(b []byte) bool {
	if !bytes.HasPrefix(b, []byte("GET ")) {
		return false
	}
	line := b
	if i := bytes.IndexByte(b, '\n'); 0 <= i {
		line = b[:i]
	}
	if !bytes.Contains(line, []byte("info_hash=")) {
		return false
	}
	return bytes.Contains(line, []byte("/announce")) || bytes.Contains(line, []byte("/scrape"))
}

// HTTP/1.x request methods (RFC 9110) -- the leading token of a request line. CONNECT
// is deliberately excluded: it opens an opaque tunnel that could carry anything.
var httpRequestMethods = [][]byte{
	[]byte("GET "), []byte("HEAD "), []byte("POST "), []byte("PUT "),
	[]byte("DELETE "), []byte("OPTIONS "), []byte("PATCH "), []byte("TRACE "),
}

// isHttpRequest reports whether b begins with a plaintext HTTP/1.x request line -- a
// method token followed by an "HTTP/1." version on the first line. Used to positively
// allow raw HTTP (e.g. media/radio streaming) on any port, including non-standard ones.
func isHttpRequest(b []byte) bool {
	method := false
	for _, m := range httpRequestMethods {
		if bytes.HasPrefix(b, m) {
			method = true
			break
		}
	}
	if !method {
		return false
	}
	line := b
	if i := bytes.IndexByte(b, '\n'); 0 <= i {
		line = b[:i]
	}
	return bytes.Contains(line, []byte(" HTTP/1."))
}

// BEP 5 DHT (Kademlia KRPC over UDP): bencoded dictionaries. Bencode keys are
// lexically sorted, which fixes the query/response prefixes; the generic case
// keys off the single-char 'y' (message type) and 't' (transaction) keys.
func isDhtKrpc(b []byte) bool {
	if len(b) < 5 || 'd' != b[0] {
		return false
	}
	if bytes.HasPrefix(b, []byte("d1:ad2:id20:")) || bytes.HasPrefix(b, []byte("d1:rd2:id20:")) {
		return true
	}
	hasType := bytes.Contains(b, []byte("1:y1:q")) ||
		bytes.Contains(b, []byte("1:y1:r")) ||
		bytes.Contains(b, []byte("1:y1:e"))
	return hasType && bytes.Contains(b, []byte("1:t"))
}

// BEP 15 UDP tracker: the connect request opens with the 64-bit magic
// 0x41727101980 followed by a 32-bit action == 0.
var udpTrackerConnectMagic = []byte{0x00, 0x00, 0x04, 0x17, 0x27, 0x10, 0x19, 0x80}

func isUdpTrackerConnect(b []byte) bool {
	if len(b) < 16 {
		return false
	}
	if !bytes.HasPrefix(b, udpTrackerConnectMagic) {
		return false
	}
	return 0 == binary.BigEndian.Uint32(b[8:12])
}

// BEP 29 uTP: a version-1 header (20 bytes) whose payload begins with a plaintext
// peer-wire handshake. A bare uTP header is only a weak structural hint (~2% of
// random datagrams), so it is not by itself a drop trigger; encrypted uTP is left
// to the entropy heuristic.
func utpV1CarriesHandshake(b []byte) bool {
	if len(b) < 20 {
		return false
	}
	packetType := b[0] >> 4
	version := b[0] & 0x0f
	if 1 != version || 4 < packetType {
		return false
	}
	if 2 < b[1] {
		return false
	}
	return hasBittorrentHandshake(b[20:])
}

func detectBittorrentSignature(ipPath *IpPath, b []byte) bool {
	switch ipPath.Protocol {
	case IpProtocolTcp:
		return hasBittorrentHandshake(b) || hasHttpTrackerRequest(b)
	case IpProtocolUdp:
		return isDhtKrpc(b) || isUdpTrackerConnect(b) || utpV1CarriesHandshake(b)
	}
	return false
}

// --- "looks fully encrypted" heuristic ---

// payloadLooksEncrypted reports whether a payload is statistically
// indistinguishable from random bytes: a near-even fraction of set bits, few
// printable-ASCII bytes, and near-maximal entropy for the sample size. All three
// gates must pass, which keeps false positives off structured/text protocols.
func payloadLooksEncrypted(b []byte, settings *DmcaSecurityPolicySettings) bool {
	if len(b) < settings.MinEncryptedPayload {
		return false
	}
	printableFraction, popcountRatio, normalizedEntropy := dmcaByteStats(b)
	if settings.EncryptedMaxPrintableFraction < printableFraction {
		return false
	}
	if settings.EncryptedPopcountBand < math.Abs(popcountRatio-0.5) {
		return false
	}
	return settings.EncryptedMinNormalizedEntropy <= normalizedEntropy
}

func dmcaByteStats(b []byte) (printableFraction float64, popcountRatio float64, normalizedEntropy float64) {
	var counts [256]int
	printable := 0
	setBits := 0
	for _, c := range b {
		counts[c] += 1
		if 0x20 <= c && c <= 0x7e {
			printable += 1
		}
		setBits += bits.OnesCount8(c)
	}
	n := float64(len(b))
	printableFraction = float64(printable) / n
	popcountRatio = float64(setBits) / (n * 8)

	entropy := 0.0
	for _, count := range counts {
		if 0 < count {
			p := float64(count) / n
			entropy -= p * math.Log2(p)
		}
	}
	maxDistinct := n
	if 256 < maxDistinct {
		maxDistinct = 256
	}
	if maxBits := math.Log2(maxDistinct); 0 < maxBits {
		normalizedEntropy = entropy / maxBits
	}
	return printableFraction, popcountRatio, normalizedEntropy
}
