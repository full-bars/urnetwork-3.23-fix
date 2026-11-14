package connect

// ip_remote_multi_client_window_client_stats.go - the counters of flows that
// no window could place.
//
// Upstream's WindowClientStats also counts candidates, removals, warnings and
// window states. This fork keeps only the no-compatible counters, which the
// family-aware flow selector writes.

import (
	"sync/atomic"
)

const (
	windowStatsIpVersion4 = iota
	windowStatsIpVersion6
	windowStatsIpVersionCount
)

const (
	windowStatsFlowProtocolTcp = iota
	windowStatsFlowProtocolUdp
	windowStatsFlowProtocolIcmp
	windowStatsFlowProtocolCount
)

var windowStatsIpVersionLabels = [windowStatsIpVersionCount]string{"4", "6"}
var windowStatsFlowProtocolLabels = [windowStatsFlowProtocolCount]string{"tcp", "udp", "icmp"}

// The counters' index of a flow's ip version and protocol. False for a path
// they do not count.
func windowStatsFlowOf(ipPath *IpPath) (ipVersion int, protocol int, ok bool) {
	if ipPath == nil {
		return 0, 0, false
	}
	switch ipPath.Version {
	case 4:
		ipVersion = windowStatsIpVersion4
	case 6:
		ipVersion = windowStatsIpVersion6
	default:
		return 0, 0, false
	}
	switch ipPath.Protocol {
	case IpProtocolTcp:
		protocol = windowStatsFlowProtocolTcp
	case IpProtocolUdp:
		protocol = windowStatsFlowProtocolUdp
	case IpProtocolIcmp:
		protocol = windowStatsFlowProtocolIcmp
	default:
		return 0, 0, false
	}
	return ipVersion, protocol, true
}

// The zero value is ready.
type WindowClientStats struct {
	noCompatible [windowStatsIpVersionCount][windowStatsFlowProtocolCount]atomic.Uint64
}

// One flow answered with an unreachable because no window held an exit
// labelled to carry it. Nil-safe.
func (self *WindowClientStats) addNoCompatible(ipPath *IpPath) {
	if self == nil {
		return
	}
	if ipVersion, protocol, ok := windowStatsFlowOf(ipPath); ok {
		self.noCompatible[ipVersion][protocol].Add(1)
	}
}

// Calls visit for every no-compatible counter, in a fixed order.
func (self *WindowClientStats) VisitNoCompatible(visit func(ipVersion string, protocol string, count uint64)) {
	if self == nil {
		return
	}
	for v := range self.noCompatible {
		for p := range self.noCompatible[v] {
			visit(windowStatsIpVersionLabels[v], windowStatsFlowProtocolLabels[p], self.noCompatible[v][p].Load())
		}
	}
}
