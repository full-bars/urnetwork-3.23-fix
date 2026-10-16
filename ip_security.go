package connect

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"

	"golang.org/x/exp/maps"

	"github.com/urnetwork/connect/protocol"
)

type SecurityPolicyResult int

const (
	SecurityPolicyResultDrop     SecurityPolicyResult = 0
	SecurityPolicyResultAllow    SecurityPolicyResult = 1
	SecurityPolicyResultIncident SecurityPolicyResult = 2
)

func (self SecurityPolicyResult) String() string {
	switch self {
	case SecurityPolicyResultDrop:
		return "drop"
	case SecurityPolicyResultAllow:
		return "allow"
	case SecurityPolicyResultIncident:
		return "incident"
	default:
		return "unknown"
	}
}

type SecurityPolicy interface {
	Stats() *SecurityPolicyStatsCollector
	// Inspect decides the fate of a packet. payload is the L4 payload (may be nil
	// for header-only inspection); it is valid only for the duration of the call.
	Inspect(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error)
}

// dmcaIngressToucher is implemented by the egress security policy: the relay's
// ingress path reports received packets here so the payload detector keeps each
// flow's idle clock fresh in both directions. Policies that do not implement it
// simply never refresh anything.
type dmcaIngressToucher interface {
	touchIngress(ipPath *IpPath)
}

// dmcaFlowToucher is implemented by the egress security policy for callers that
// hold the flow's own (client to server) path, as the provider's return path does.
type dmcaFlowToucher interface {
	touchFlow(ipPath *IpPath)
}

type egressSecurityPolicy struct {
	stats *SecurityPolicyStatsCollector
	cfaa  *cfaaDetector
	dmca  *dmcaDetector
}

func DefaultEgressSecurityPolicy() SecurityPolicy {
	// no context: the idle-flow scan stays off
	return DefaultEgressSecurityPolicyWithStats(nil, DefaultSecurityPolicyStatsCollector())
}

// DefaultEgressSecurityPolicyWithStats matches the EgressSecurityPolicyGenerator
// shape: the context ties the detector's idle-flow scan to the policy's owner,
// and a nil context leaves the scan off.
func DefaultEgressSecurityPolicyWithStats(ctx context.Context, stats *SecurityPolicyStatsCollector) SecurityPolicy {
	return NewEgressSecurityPolicy(
		ctx,
		DefaultCfaaSecurityPolicySettings(),
		DefaultDmcaSecurityPolicySettings(),
		DefaultWebStandardSettings(),
		stats,
	)
}

func NewEgressSecurityPolicy(ctx context.Context, cfaaSettings *CfaaSecurityPolicySettings, dmcaSettings *DmcaSecurityPolicySettings, webSettings *WebStandardSettings, stats *SecurityPolicyStatsCollector) SecurityPolicy {
	return &egressSecurityPolicy{
		stats: stats,
		cfaa:  newCfaaDetector(cfaaSettings),
		dmca:  newDmcaDetector(ctx, dmcaSettings, newWebStandardDetector(webSettings)),
	}
}

func (self *egressSecurityPolicy) Stats() *SecurityPolicyStatsCollector {
	return self.stats
}

// touchIngress refreshes the dmca flow clock from a received packet, so a flow
// whose traffic is (currently) one-directional is not reclaimed as idle.
func (self *egressSecurityPolicy) touchIngress(ipPath *IpPath) {
	self.dmca.touchIngress(ipPath)
}

// touchFlow is touchIngress for a path in the flow's own orientation.
func (self *egressSecurityPolicy) touchFlow(ipPath *IpPath) {
	self.dmca.touchFlow(ipPath)
}

func (self *egressSecurityPolicy) Inspect(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	result, err := self.inspect(provideMode, ipPath, payload)
	if ipPath != nil {
		self.stats.AddDestination(ipPath, result, 1)
		securityDecisionAdd(secDirEgress, result)
	}
	return result, err
}

func (self *egressSecurityPolicy) inspect(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	if protocol.ProvideMode_Network == provideMode {
		return SecurityPolicyResultAllow, nil
	}

	if !isPublicUnicast(ipPath.DestinationIp) {
		securityBlockedAdd(secDirEgress, secReasonNonPublicDestination, ipPath.Protocol)
		return SecurityPolicyResultIncident, nil
	}

	// static endpoint reputation (blocked ips + port policy) on the destination
	switch verdict := self.cfaa.inspect(ipPath.DestinationIp, ipPath.DestinationPort, ipPath.Protocol, ipPath.Version); verdict {
	case cfaaDropIp:
		securityBlockedAdd(secDirEgress, secReasonBlockedIp, ipPath.Protocol)
		return SecurityPolicyResultDrop, nil
	case cfaaDropPort:
		securityBlockedAdd(secDirEgress, secReasonBlockedPort, ipPath.Protocol)
		securityBlockedPortAdd(secDirEgress, ipPath.Protocol, ipPath.DestinationPort)
		return SecurityPolicyResultDrop, nil
	case cfaaAllow:
		return SecurityPolicyResultAllow, nil
	default:
		// No static verdict: run stateful payload DPI. Switch on the verdict so
		// the policy reads explicitly: a positive BitTorrent signature is reported;
		// a flow that looks fully encrypted is dropped UNLESS it matched a
		// sanctioned web/communication standard (TLS/QUIC/DTLS/STUN/TURN or
		// RTP/RTCP), or an exact provider-scoped gaming exception; that positive
		// match is the fallback that rescues an otherwise-ambiguous encrypted flow.
		// Enforcement of each verdict honors the detector settings (log-only,
		// drop/report toggles), applied by result(); dmcaOutcome reports the
		// decided and enforced split to the block metrics.
		v, reason, decidedNow := self.dmca.classifyDetailed(ipPath, payload)
		recordDpiDecision(ipPath, reason, decidedNow)
		switch v {
		case dmcaBittorrent:
			return self.dmcaOutcome(v, ipPath), nil
		case dmcaDropEncrypted:
			return self.dmcaOutcome(v, ipPath), nil
		default:
			// still inspecting, a sanctioned web standard, or benign plaintext
			return SecurityPolicyResultAllow, nil
		}
	}
}

// dmcaOutcome maps a terminal dpi verdict through the detector settings and
// moves the decided/enforced/blocked counters: decided once per terminal
// classification, enforced and blocked only when the settings let it drop or
// report. A downgraded verdict (log-only, report toggles off) stays decided.
func (self *egressSecurityPolicy) dmcaOutcome(v dmcaVerdict, ipPath *IpPath) SecurityPolicyResult {
	direction := secDirEgress
	reason := secReasonDmcaBittorrent
	if v == dmcaDropEncrypted {
		reason = secReasonDmcaEncrypted
	}
	securityDecidedAdd(direction, reason, ipPath.Protocol)
	result := self.dmca.result(v)
	if result == SecurityPolicyResultDrop || result == SecurityPolicyResultIncident {
		securityEnforcedAdd(direction, reason, ipPath.Protocol)
		securityBlockedAdd(direction, reason, ipPath.Protocol)
	}
	return result
}

type ingressSecurityPolicy struct {
	stats *SecurityPolicyStatsCollector
	cfaa  *cfaaDetector
}

func DefaultIngressSecurityPolicy() SecurityPolicy {
	return DefaultIngressSecurityPolicyWithStats(DefaultSecurityPolicyStatsCollector())
}

func DefaultIngressSecurityPolicyWithStats(stats *SecurityPolicyStatsCollector) SecurityPolicy {
	return NewIngressSecurityPolicy(DefaultCfaaSecurityPolicySettings(), stats)
}

func NewIngressSecurityPolicy(cfaaSettings *CfaaSecurityPolicySettings, stats *SecurityPolicyStatsCollector) SecurityPolicy {
	return &ingressSecurityPolicy{
		stats: stats,
		cfaa:  newCfaaDetector(cfaaSettings),
	}
}

func (self *ingressSecurityPolicy) Stats() *SecurityPolicyStatsCollector {
	return self.stats
}

func (self *ingressSecurityPolicy) Inspect(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	result, err := self.inspect(provideMode, ipPath)
	self.stats.AddSource(ipPath, result, 1)
	securityDecisionAdd(secDirIngress, result)
	return result, err
}

func (self *ingressSecurityPolicy) inspect(provideMode protocol.ProvideMode, ipPath *IpPath) (SecurityPolicyResult, error) {
	if protocol.ProvideMode_Network == provideMode {
		return SecurityPolicyResultAllow, nil
	}

	// mirror the egress static drops (blocked ips + port policy), evaluated on the
	// source endpoint
	switch self.cfaa.inspect(ipPath.SourceIp, ipPath.SourcePort, ipPath.Protocol, ipPath.Version) {
	case cfaaDropIp:
		securityBlockedAdd(secDirIngress, secReasonBlockedIp, ipPath.Protocol)
		return SecurityPolicyResultDrop, nil
	case cfaaDropPort:
		securityBlockedAdd(secDirIngress, secReasonBlockedPort, ipPath.Protocol)
		securityBlockedPortAdd(secDirIngress, ipPath.Protocol, ipPath.SourcePort)
		return SecurityPolicyResultDrop, nil
	}
	return SecurityPolicyResultAllow, nil
}

type disableSecurityPolicy struct {
	stats *SecurityPolicyStatsCollector
}

func DisableSecurityPolicy() SecurityPolicy {
	return DisableSecurityPolicyWithStats(DefaultSecurityPolicyStatsCollector())
}

func DisableSecurityPolicyWithStats(stats *SecurityPolicyStatsCollector) SecurityPolicy {
	return &disableSecurityPolicy{
		stats: stats,
	}
}

func (self *disableSecurityPolicy) Stats() *SecurityPolicyStatsCollector {
	return self.stats
}

func (self *disableSecurityPolicy) Inspect(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	return SecurityPolicyResultAllow, nil
}

// cgnat4 is RFC 6598 carrier-grade NAT 100.64.0.0/10, used as internal
// address space on hosting providers and Tailscale's default range.
var cgnat4 = net.IPNet{
	IP:   net.IPv4(100, 64, 0, 0),
	Mask: net.CIDRMask(10, 32),
}

func isPublicUnicast(ip net.IP) bool {
	// an IPv4-mapped IPv6 address (::ffff:a.b.c.d) is dialed over IPv4, so it is
	// judged as the IPv4 address it wraps
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	} else if len(ip) == net.IPv6len && isSpecialUseIpv6(ip) {
		return false
	}
	switch {
	case ip.IsPrivate(),
		ip.IsLoopback(),
		ip.IsLinkLocalUnicast(),
		ip.IsMulticast(),
		ip.IsUnspecified(),
		cgnat4.Contains(ip):
		return false
	default:
		return true
	}
}

// specialUseIpv6 are IPv6 ranges that are not an ordinary public destination for
// relayed traffic. Several of them embed an IPv4 address (NAT64, 6to4, Teredo,
// IPv4-compatible): relaying to them on a host or network that translates them
// reaches IPv4 space the IPv4 checks (private ranges, the CFAA blocklist) never
// saw. They are refused outright.
var specialUseIpv6 = func() []*net.IPNet {
	cidrs := []string{
		"::/96",          // IPv4-compatible (deprecated), also the unspecified address
		"64:ff9b::/96",   // NAT64 well-known prefix (RFC 6052)
		"64:ff9b:1::/48", // NAT64 local-use prefix (RFC 8215)
		"100::/64",       // discard-only (RFC 6666)
		"2001::/32",      // Teredo
		"2001:2::/48",    // benchmarking (RFC 5180), not globally reachable
		"2001:db8::/32",  // documentation
		"2002::/16",      // 6to4
		"3fff::/20",      // documentation (RFC 9637)
		"3ffe::/16",      // 6bone (retired)
		"5f00::/16",      // SRv6 segment identifiers (RFC 9602), not globally reachable
		"fec0::/10",      // site-local (deprecated)
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(err)
		}
		nets = append(nets, n)
	}
	return nets
}()

func isSpecialUseIpv6(ip net.IP) bool {
	for _, n := range specialUseIpv6 {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

type SecurityPolicyStats = map[SecurityPolicyResult]map[SecurityDestination]uint64

const (
	// securityPolicyStatsMaxDestinationsPerResult includes one overflow
	// destination. Security-policy statistics are diagnostics, not flow state:
	// a long-lived provider must not retain every ephemeral port it has ever
	// relayed or clone that growing set whenever statistics are inspected.
	securityPolicyStatsMaxDestinationsPerResult = 1024
	securityPolicyStatsUnknownResult            = SecurityPolicyResult(-1)
)

var securityPolicyStatsOverflowDestination = SecurityDestination{}

type SecurityDestination struct {
	Version  int
	Protocol IpProtocol
	Ip       string
	Port     int
}

func newSecurityDestinationPort(ipPath *IpPath) SecurityDestination {
	return SecurityDestination{
		Version:  ipPath.Version,
		Protocol: ipPath.Protocol,
		Ip:       "",
		Port:     ipPath.DestinationPort,
	}
}

func newSecurityDestination(ipPath *IpPath) SecurityDestination {
	return SecurityDestination{
		Version:  ipPath.Version,
		Protocol: ipPath.Protocol,
		Ip:       ipPath.DestinationIp.String(),
		Port:     ipPath.DestinationPort,
	}
}

func newSecuritySourcePort(ipPath *IpPath) SecurityDestination {
	return SecurityDestination{
		Version:  ipPath.Version,
		Protocol: ipPath.Protocol,
		Ip:       "",
		Port:     ipPath.SourcePort,
	}
}

func newSecuritySource(ipPath *IpPath) SecurityDestination {
	return SecurityDestination{
		Version:  ipPath.Version,
		Protocol: ipPath.Protocol,
		Ip:       ipPath.SourceIp.String(),
		Port:     ipPath.SourcePort,
	}
}

func (self *SecurityDestination) Cmp(b SecurityDestination) int {
	if self.Version < b.Version {
		return -1
	} else if b.Version < self.Version {
		return 1
	}

	if self.Protocol < b.Protocol {
		return -1
	} else if b.Protocol < self.Protocol {
		return 1
	}

	if self.Ip < b.Ip {
		return -1
	} else if b.Ip < self.Ip {
		return 1
	}

	if self.Port < b.Port {
		return -1
	} else if b.Port < self.Port {
		return 1
	}

	return 0
}

func (self *SecurityDestination) String() string {
	if *self == securityPolicyStatsOverflowDestination {
		return "other destinations"
	}
	return fmt.Sprintf("ipv%d %s %s",
		self.Version,
		self.Protocol.String(),
		net.JoinHostPort(self.Ip, strconv.Itoa(self.Port)),
	)
}

// get current counts of outcomes per (protocol, destination port)
type SecurityPolicyStatsCollector struct {
	includeIp bool

	stateLock               sync.Mutex
	resultDestinationCounts SecurityPolicyStats
}

func DefaultSecurityPolicyStatsCollector() *SecurityPolicyStatsCollector {
	return &SecurityPolicyStatsCollector{
		includeIp:               false,
		resultDestinationCounts: SecurityPolicyStats{},
	}
}

// add records one diagnostic count while bounding every result's destination
// cardinality. Built-in policies produce the three declared results; callers
// passing another integer share one unknown-result bucket so arbitrary result
// values cannot defeat the memory bound.
func (self *SecurityPolicyStatsCollector) add(
	destination SecurityDestination,
	result SecurityPolicyResult,
	count uint64,
) {
	if count == 0 {
		return
	}
	switch result {
	case SecurityPolicyResultDrop,
		SecurityPolicyResultAllow,
		SecurityPolicyResultIncident:
	default:
		result = securityPolicyStatsUnknownResult
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	destinationCounts, ok := self.resultDestinationCounts[result]
	if !ok {
		destinationCounts = map[SecurityDestination]uint64{}
		self.resultDestinationCounts[result] = destinationCounts
	}
	if _, ok := destinationCounts[destination]; !ok &&
		securityPolicyStatsMaxDestinationsPerResult <= len(destinationCounts)+1 {
		// Reserve the final slot for all later destinations. A real IpPath has
		// version 4 or 6, so the zero destination cannot collide with one.
		destination = securityPolicyStatsOverflowDestination
	}
	destinationCounts[destination] += count
}

func (self *SecurityPolicyStatsCollector) AddDestination(ipPath *IpPath, result SecurityPolicyResult, count uint64) {
	var destination SecurityDestination
	if self.includeIp {
		destination = newSecurityDestination(ipPath)
	} else {
		// port only, no ip
		destination = newSecurityDestinationPort(ipPath)
	}
	self.add(destination, result, count)
}

func (self *SecurityPolicyStatsCollector) AddSource(ipPath *IpPath, result SecurityPolicyResult, count uint64) {
	var destination SecurityDestination
	if self.includeIp {
		destination = newSecuritySource(ipPath)
	} else {
		// port only, no ip
		destination = newSecuritySourcePort(ipPath)
	}
	self.add(destination, result, count)
}

func (self *SecurityPolicyStatsCollector) Stats(reset bool) SecurityPolicyStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	resultDestinationCounts := SecurityPolicyStats{}
	for result, destinationCounts := range self.resultDestinationCounts {
		resultDestinationCounts[result] = maps.Clone(destinationCounts)
	}
	if reset {
		clear(self.resultDestinationCounts)
	}
	return resultDestinationCounts
}
