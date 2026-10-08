package connect

import (
	"context"
	"net"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/urnetwork/connect/protocol"
)

type upstreamTestPolicy interface {
	SecurityPolicy
	InspectEgress(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error)
	InspectIngress(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error)
	RefreshEgress(ipPath *IpPath)
	RefreshIngress(ipPath *IpPath)
	Testing_FlowCount() int
}

type upstreamTestPolicyImpl struct {
	policy     SecurityPolicy
	cfaa       *cfaaDetector
	dmca       *dmcaDetector
	isProvider bool
}

func (p *upstreamTestPolicyImpl) Stats() *SecurityPolicyStatsCollector {
	return p.policy.Stats()
}

func (p *upstreamTestPolicyImpl) Inspect(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	return p.policy.Inspect(provideMode, ipPath, payload)
}

func (p *upstreamTestPolicyImpl) InspectEgress(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	if p.isProvider {
		return p.inspectIngressDirect(provideMode, ipPath)
	}
	return p.policy.Inspect(provideMode, ipPath, payload)
}

func (p *upstreamTestPolicyImpl) InspectIngress(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	if p.isProvider {
		return p.policy.Inspect(provideMode, ipPath, payload)
	}
	return p.inspectIngressDirect(provideMode, ipPath)
}

func (p *upstreamTestPolicyImpl) inspectIngressDirect(provideMode protocol.ProvideMode, ipPath *IpPath) (SecurityPolicyResult, error) {
	if protocol.ProvideMode_Network == provideMode {
		return SecurityPolicyResultAllow, nil
	}
	if p.cfaa != nil && ipPath != nil {
		if cfaaDrop == p.cfaa.inspect(ipPath.SourceIp, ipPath.SourcePort, ipPath.Protocol, ipPath.Version) {
			return SecurityPolicyResultDrop, nil
		}
	}
	return SecurityPolicyResultAllow, nil
}

func (p *upstreamTestPolicyImpl) RefreshEgress(_ *IpPath) {}
func (p *upstreamTestPolicyImpl) RefreshIngress(_ *IpPath) {}

func (p *upstreamTestPolicyImpl) Testing_FlowCount() int {
	if p.dmca != nil {
		return p.dmca.Testing_FlowCount()
	}
	return 0
}

func DefaultSecurityPolicy(_ context.Context) upstreamTestPolicy {
	cfaaSettings := DefaultCfaaSecurityPolicySettings()
	dmcaSettings := DefaultDmcaSecurityPolicySettings()
	webSettings := DefaultWebStandardSettings()
	stats := DefaultSecurityPolicyStatsCollector()
	cfaa := newCfaaDetector(cfaaSettings)
	dmca := newDmcaDetector(dmcaSettings, newWebStandardDetector(webSettings))
	return &upstreamTestPolicyImpl{
		policy: &egressSecurityPolicy{
			stats: stats,
			cfaa:  cfaa,
			dmca:  dmca,
		},
		cfaa: cfaa,
		dmca: dmca,
	}
}

func DefaultSecurityPolicyWithStats(_ context.Context, stats *SecurityPolicyStatsCollector) upstreamTestPolicy {
	cfaaSettings := DefaultCfaaSecurityPolicySettings()
	dmcaSettings := DefaultDmcaSecurityPolicySettings()
	webSettings := DefaultWebStandardSettings()
	cfaa := newCfaaDetector(cfaaSettings)
	dmca := newDmcaDetector(dmcaSettings, newWebStandardDetector(webSettings))
	return &upstreamTestPolicyImpl{
		policy: &egressSecurityPolicy{
			stats: stats,
			cfaa:  cfaa,
			dmca:  dmca,
		},
		cfaa: cfaa,
		dmca: dmca,
	}
}

func NewSecurityPolicy(
	_ context.Context,
	cfaaSettings *CfaaSecurityPolicySettings,
	dmcaSettings *DmcaSecurityPolicySettings,
	webSettings *WebStandardSettings,
	stats *SecurityPolicyStatsCollector,
) upstreamTestPolicy {
	cfaa := newCfaaDetector(cfaaSettings)
	dmca := newDmcaDetector(dmcaSettings, newWebStandardDetector(webSettings))
	return &upstreamTestPolicyImpl{
		policy: &egressSecurityPolicy{
			stats: stats,
			cfaa:  cfaa,
			dmca:  dmca,
		},
		cfaa: cfaa,
		dmca: dmca,
	}
}

func DefaultProviderSecurityPolicy(ctx context.Context) upstreamTestPolicy {
	client := DefaultSecurityPolicy(ctx).(*upstreamTestPolicyImpl)
	client.isProvider = true
	return client
}

func Reverse(policy upstreamTestPolicy) upstreamTestPolicy {
	if impl, ok := policy.(*upstreamTestPolicyImpl); ok {
		implCopy := *impl
		implCopy.isProvider = !implCopy.isProvider
		return &implCopy
	}
	return policy
}

func inspectAndRefreshIngressForSenderBorrowed(
	policy any,
	_ Id,
	provideMode protocol.ProvideMode,
	ipPath IpPath,
	payload []byte,
) (SecurityPolicyResult, error) {
	if u, ok := policy.(upstreamTestPolicy); ok {
		return u.InspectIngress(provideMode, &ipPath, payload)
	}
	if sp, ok := policy.(SecurityPolicy); ok {
		return sp.Inspect(provideMode, &ipPath, payload)
	}
	return SecurityPolicyResultAllow, nil
}

func dmcaFlowKeyForPath(_ Id, ipPath *IpPath) Ip6Path {
	return ipPath.ToIp6Path()
}

func (self *dmcaDetector) classifyForSenderDetailed(_ Id, ipPath *IpPath, payload []byte) (dmcaVerdict, SecurityPolicyReason, bool) {
	return self.classifyDetailed(ipPath, payload)
}

func (self *dmcaDetector) Testing_FlowCount() int {
	count := 0
	for _, shard := range self.shards {
		shard.mu.RLock()
		count += len(shard.flows)
		shard.mu.RUnlock()
	}
	return count
}

func craftSecurityPacket(proto IpProtocol, srcIP net.IP, srcPort int, dstIP net.IP, dstPort int, syn bool, payload []byte) []byte {
	ip := &layers.IPv4{
		Version: 4,
		TTL:     64,
		SrcIP:   srcIP.To4(),
		DstIP:   dstIP.To4(),
	}
	var transport gopacket.SerializableLayer
	switch proto {
	case IpProtocolTcp:
		ip.Protocol = layers.IPProtocolTCP
		tcp := &layers.TCP{
			SrcPort: layers.TCPPort(srcPort),
			DstPort: layers.TCPPort(dstPort),
			SYN:     syn,
			Seq:     1,
			Window:  65535,
		}
		tcp.SetNetworkLayerForChecksum(ip)
		transport = tcp
	case IpProtocolUdp:
		ip.Protocol = layers.IPProtocolUDP
		udp := &layers.UDP{
			SrcPort: layers.UDPPort(srcPort),
			DstPort: layers.UDPPort(dstPort),
		}
		udp.SetNetworkLayerForChecksum(ip)
		transport = udp
	default:
		return nil
	}
	buf := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(
		buf,
		gopacket.SerializeOptions{ComputeChecksums: true, FixLengths: true},
		ip, transport, gopacket.Payload(payload),
	); err != nil {
		return nil
	}
	return append([]byte(nil), buf.Bytes()...)
}

const IpProtocolIcmp IpProtocol = 3

func cfaaVerdictName(v cfaaVerdict) string {
	switch v {
	case cfaaPass:
		return "pass"
	case cfaaAllow:
		return "allow"
	case cfaaDrop:
		return "drop"
	default:
		return "unknown"
	}
}

