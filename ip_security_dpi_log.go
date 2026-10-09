package connect

import (
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"
)

// Operator-visible record of the stateful payload inspection. A flow is counted
// once, when it reaches its terminal verdict, under the reason that decided it,
// so "allow-app-standard:wireguard" against "drop-encrypted" shows directly what
// the application-standard admits are rescuing. Ports and protocols only: no
// address is ever logged, matching the reason statistics in
// ip_security_reason.go.
//
// Three outputs, all local log lines:
//   - the first admit under each reason, once per process
//   - a sample of dropped flows (at most one a minute, with the suppressed count)
//   - a cumulative summary of decided flows per reason (at most one every five
//     minutes, emitted by whichever flow decides next, so a quiet node is quiet)
var (
	dpiDecidedFlows [securityPolicyReasonEnd]atomic.Uint64
	// every inspected packet under the reason it was handled with, including the
	// undecided ones, so a quiet flow log can be told from no traffic at all
	dpiPackets       [securityPolicyReasonEnd]atomic.Uint64
	dpiTotalPackets  atomic.Uint64
	dpiFirstLogged   [securityPolicyReasonEnd]atomic.Bool
	dpiDropSample    = newLogThrottle(time.Minute)
	dpiSummary       = newLogThrottle(5 * time.Minute)
	dpiSummaryOffset = time.Now()
)

// Called by the egress policy for every inspected packet. Counts a flow when it
// reaches its terminal verdict. Application-standard admits are counted earlier,
// by recordDpiAppAdmit, so they are skipped here.
func recordDpiDecision(ipPath *IpPath, reason SecurityPolicyReason, decidedNow bool) {
	if SecurityPolicyReasonUnknown < reason && reason < securityPolicyReasonEnd {
		dpiPackets[reason].Add(1)
	}
	// the throttle clock is read only every 4096th packet to keep the hot path to atomics
	if dpiTotalPackets.Add(1)&0xfff == 0 {
		logDpiSummary(time.Now())
	}
	if !decidedNow || isDpiAppAdmitReason(reason) {
		return
	}
	countDpiFlow(ipPath, reason)
}

func logDpiSummary(now time.Time) {
	if ok, _ := dpiSummary.Allow(now); ok {
		glog.Infof(
			"[security][dpi] since start (%s) flows decided: %s | packets: %s\n",
			now.Sub(dpiSummaryOffset).Round(time.Second), dpiSummaryString(), dpiPacketString(),
		)
	}
}

// Called when an application standard first admits a flow.
func recordDpiAppAdmit(ipPath *IpPath, reason SecurityPolicyReason) {
	countDpiFlow(ipPath, reason)
}

func countDpiFlow(ipPath *IpPath, reason SecurityPolicyReason) {
	if reason <= SecurityPolicyReasonUnknown || securityPolicyReasonEnd <= reason {
		return
	}
	dpiDecidedFlows[reason].Add(1)

	now := time.Now()
	switch reason {
	case SecurityPolicyReasonDropEncrypted, SecurityPolicyReasonBittorrent:
		if ok, suppressed := dpiDropSample.Allow(now); ok {
			glog.Infof(
				"[security][dpi] dropped flow reason=%s proto=%s dst_port=%d (%d more since the last sample)\n",
				reason, dpiProtocolName(ipPath), ipPath.DestinationPort, suppressed,
			)
		}
	default:
		if isDpiAdmitReason(reason) && dpiFirstLogged[reason].CompareAndSwap(false, true) {
			glog.Infof(
				"[security][dpi] first admit reason=%s proto=%s dst_port=%d\n",
				reason, dpiProtocolName(ipPath), ipPath.DestinationPort,
			)
		}
	}

	logDpiSummary(now)
}

// reasons that go through dmcaFlowState.allowAppStandard
func isDpiAppAdmitReason(reason SecurityPolicyReason) bool {
	switch reason {
	case SecurityPolicyReasonAllowMessaging,
		SecurityPolicyReasonAllowGaming,
		SecurityPolicyReasonAllowWireGuard,
		SecurityPolicyReasonAllowOpenVpn,
		SecurityPolicyReasonAllowRtmp,
		SecurityPolicyReasonAllowLevin,
		SecurityPolicyReasonAllowRakNet,
		SecurityPolicyReasonAllowEthereumDiscv4,
		SecurityPolicyReasonAllowEthereumRlpx,
		SecurityPolicyReasonAllowWhatsApp:
		return true
	}
	return false
}

// the reasons the application-standard work added: these are the admits
func isDpiAdmitReason(reason SecurityPolicyReason) bool {
	switch reason {
	case SecurityPolicyReasonAllowGaming,
		SecurityPolicyReasonAllowMessaging,
		SecurityPolicyReasonAllowTurn,
		SecurityPolicyReasonAllowRtcp,
		SecurityPolicyReasonAllowRtp,
		SecurityPolicyReasonAllowWireGuard,
		SecurityPolicyReasonAllowOpenVpn,
		SecurityPolicyReasonAllowRtmp,
		SecurityPolicyReasonAllowLevin,
		SecurityPolicyReasonAllowRakNet,
		SecurityPolicyReasonAllowEthereumDiscv4,
		SecurityPolicyReasonAllowEthereumRlpx,
		SecurityPolicyReasonAllowWhatsApp:
		return true
	}
	return false
}

// "reason=count" for every reason that decided at least one flow, in reason order.
func dpiSummaryString() string {
	var parts []string
	for reason := SecurityPolicyReason(1); reason < securityPolicyReasonEnd; reason++ {
		if n := dpiDecidedFlows[reason].Load(); 0 < n {
			parts = append(parts, reason.String()+"="+strconv.FormatUint(n, 10))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

// "reason=count" for every reason that handled at least one packet.
func dpiPacketString() string {
	var parts []string
	for reason := SecurityPolicyReason(1); reason < securityPolicyReasonEnd; reason++ {
		if n := dpiPackets[reason].Load(); 0 < n {
			parts = append(parts, reason.String()+"="+strconv.FormatUint(n, 10))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

func dpiProtocolName(ipPath *IpPath) string {
	switch ipPath.Protocol {
	case IpProtocolTcp:
		return "tcp"
	case IpProtocolUdp:
		return "udp"
	}
	return "other"
}

func resetDpiLogForTest() {
	for i := range dpiDecidedFlows {
		dpiDecidedFlows[i].Store(0)
		dpiPackets[i].Store(0)
		dpiFirstLogged[i].Store(false)
	}
	dpiTotalPackets.Store(0)
	dpiDropSample = newLogThrottle(time.Minute)
	dpiSummary = newLogThrottle(5 * time.Minute)
	dpiSummaryOffset = time.Now()
}
