package connect

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// Security-policy block metrics. The counters sit on the Inspect path but are
// fixed-size atomic arrays indexed by small ints computed without allocation:
// no maps, no strings, no locks. Label sets are closed by construction and
// never derive from policy input (no ips, hosts, client ids or paths).

const (
	secDirEgress  = 0
	secDirIngress = 1
	secDirCount   = 2
)

// The closed reason enum: one entry per drop/incident source in the policy.
const (
	// secReasonNonPublicDestination: egress SecurityPolicyResultIncident for a
	// destination that is not public unicast (ip_security.go inspect).
	secReasonNonPublicDestination = iota
	// secReasonBlockedIp: cfaaDropIp, a destination (egress) or source
	// (ingress) inside the static blocked-ip table.
	secReasonBlockedIp
	// secReasonBlockedPort: cfaaDropPort, the cfaa port policy (bittorrent and
	// unofficial ports, plain dns over tcp, http over udp, every other
	// privileged port).
	secReasonBlockedPort
	// secReasonDmcaBittorrent: a terminal BitTorrent classification.
	secReasonDmcaBittorrent
	// secReasonDmcaEncrypted: a terminal unsanctioned-encrypted classification.
	secReasonDmcaEncrypted
	secReasonCount
)

var (
	secDirectionNames = [secDirCount]string{"egress", "ingress"}
	secReasonNames    = [secReasonCount]string{
		"non_public_destination",
		"blocked_ip",
		"blocked_port",
		"dmca_bittorrent",
		"dmca_encrypted",
	}
	// secResultNames is indexed by SecurityPolicyResult (drop, allow, incident).
	secResultNames = [3]string{"drop", "allow", "incident"}
	secProtoNames  = dmcaProtocolBuckets
)

// securityBlockedPortLabels are exactly the port numbers the cfaa port policy
// names as drops in ip_security_cfaa.go: the bittorrent and unofficial set
// (6881-6889, 6969, 1337, 9337, 2710), plain dns over tcp (53; udp is
// allowed), http over udp (80; tcp passes to DPI), and the two smtp ports
// (25=local, 587=starttls) where the rule does not reach the pass branch.
// Every other port - including the remaining privileged range - folds into
// the final "other" label, so the label set is fixed at len+1 and cannot grow.
var securityBlockedPortLabels = [...]int{
	25, 53, 80, 587,
	1337, 2710, 6969, 9337,
	6881, 6882, 6883, 6884, 6885, 6886, 6887, 6888, 6889,
}

var (
	// securityDecisionsTotal[direction][result]: every Inspect call.
	securityDecisionsTotal [secDirCount][3]atomic.Int64
	// securityBlockedTotal[direction][reason][protocol]: enforced drops and
	// incidents only.
	securityBlockedTotal [secDirCount][secReasonCount][3]atomic.Int64
	// securityBlockedPortTotal[direction][protocol][port label]: enforced
	// blocked_port drops only; the port label set is bounded by the rule list.
	securityBlockedPortTotal [secDirCount][3][len(securityBlockedPortLabels) + 1]atomic.Int64
	// securityDecidedTotal / securityEnforcedTotal[direction][reason][protocol]:
	// terminal dmca classifications before settings, and how many of them were
	// enforced as drop or incident (the remainder were downgraded by log-only
	// or report settings).
	securityDecidedTotal  [secDirCount][secReasonCount][3]atomic.Int64
	securityEnforcedTotal [secDirCount][secReasonCount][3]atomic.Int64
)

var securityBlockedPortNames [len(securityBlockedPortLabels) + 1]string

func init() {
	for i, port := range securityBlockedPortLabels {
		securityBlockedPortNames[i] = fmt.Sprintf("%d", port)
	}
	securityBlockedPortNames[len(securityBlockedPortLabels)] = "other"
}

// securityBlockedPortIndex maps any port to its label index; ports the rule
// list does not name share the final "other" index.
func securityBlockedPortIndex(port int) int {
	for i, rulePort := range securityBlockedPortLabels {
		if port == rulePort {
			return i
		}
	}
	return len(securityBlockedPortLabels)
}

func securityDecisionAdd(direction int, result SecurityPolicyResult) {
	r := int(result)
	if direction < 0 || secDirCount <= direction || r < 0 || len(securityDecisionsTotal[direction]) <= r {
		return
	}
	securityDecisionsTotal[direction][r].Add(1)
}

func securityBlockedAdd(direction int, reason int, protocol IpProtocol) {
	if direction < 0 || secDirCount <= direction || reason < 0 || secReasonCount <= reason {
		return
	}
	securityBlockedTotal[direction][reason][dmcaProtocolBucket(protocol)].Add(1)
}

func securityBlockedPortAdd(direction int, protocol IpProtocol, port int) {
	if direction < 0 || secDirCount <= direction {
		return
	}
	securityBlockedPortTotal[direction][dmcaProtocolBucket(protocol)][securityBlockedPortIndex(port)].Add(1)
}

func securityDecidedAdd(direction int, reason int, protocol IpProtocol) {
	if direction < 0 || secDirCount <= direction || reason < 0 || secReasonCount <= reason {
		return
	}
	securityDecidedTotal[direction][reason][dmcaProtocolBucket(protocol)].Add(1)
}

func securityEnforcedAdd(direction int, reason int, protocol IpProtocol) {
	if direction < 0 || secDirCount <= direction || reason < 0 || secReasonCount <= reason {
		return
	}
	securityEnforcedTotal[direction][reason][dmcaProtocolBucket(protocol)].Add(1)
}

// secDmcaReasonIndexes are the reason rows the decided/enforced families
// render: only terminal dmca classifications have a decided-versus-enforced
// split.
var secDmcaReasonIndexes = [2]int{secReasonDmcaBittorrent, secReasonDmcaEncrypted}

// writeSecurityBlockMetrics renders the block metrics (called by the stream
// metrics render). Every label combination is emitted from process start
// (zero values) so scrapes are stable.
func writeSecurityBlockMetrics(b *strings.Builder) {
	b.WriteString("# HELP urnet_security_decisions_total Security-policy Inspect decisions by direction and result; every call, so the result dimension sums to the inspection rate.\n")
	b.WriteString("# TYPE urnet_security_decisions_total counter\n")
	for direction := range securityDecisionsTotal {
		for r := range securityDecisionsTotal[direction] {
			fmt.Fprintf(b, "urnet_security_decisions_total{direction=%q,result=%q} %d\n",
				secDirectionNames[direction], secResultNames[r], securityDecisionsTotal[direction][r].Load())
		}
	}
	b.WriteString("# HELP urnet_security_blocked_total Enforced security-policy drops and incidents by direction, reason and protocol.\n")
	b.WriteString("# TYPE urnet_security_blocked_total counter\n")
	for direction := range securityBlockedTotal {
		for reason := range securityBlockedTotal[direction] {
			for protocol := range securityBlockedTotal[direction][reason] {
				fmt.Fprintf(b, "urnet_security_blocked_total{direction=%q,reason=%q,protocol=%q} %d\n",
					secDirectionNames[direction], secReasonNames[reason], secProtoNames[protocol],
					securityBlockedTotal[direction][reason][protocol].Load())
			}
		}
	}
	b.WriteString("# HELP urnet_security_blocked_port_total Enforced blocked_port drops by direction, protocol and port; every port the cfaa policy does not name folds into port=\"other\".\n")
	b.WriteString("# TYPE urnet_security_blocked_port_total counter\n")
	for direction := range securityBlockedPortTotal {
		for protocol := range securityBlockedPortTotal[direction] {
			for port := range securityBlockedPortTotal[direction][protocol] {
				fmt.Fprintf(b, "urnet_security_blocked_port_total{direction=%q,protocol=%q,port=%q} %d\n",
					secDirectionNames[direction], secProtoNames[protocol], securityBlockedPortNames[port],
					securityBlockedPortTotal[direction][protocol][port].Load())
			}
		}
	}
	b.WriteString("# HELP urnet_security_decided_total Terminal dmca classifications, before the detector settings decide enforcement.\n")
	b.WriteString("# TYPE urnet_security_decided_total counter\n")
	for direction := range securityDecidedTotal {
		for _, reason := range secDmcaReasonIndexes {
			for protocol := range securityDecidedTotal[direction][reason] {
				fmt.Fprintf(b, "urnet_security_decided_total{direction=%q,reason=%q,protocol=%q} %d\n",
					secDirectionNames[direction], secReasonNames[reason], secProtoNames[protocol],
					securityDecidedTotal[direction][reason][protocol].Load())
			}
		}
	}
	b.WriteString("# HELP urnet_security_enforced_total Of the terminal dmca classifications, how many were enforced as drop or incident; the remainder were downgraded by log-only or report settings.\n")
	b.WriteString("# TYPE urnet_security_enforced_total counter\n")
	for direction := range securityEnforcedTotal {
		for _, reason := range secDmcaReasonIndexes {
			for protocol := range securityEnforcedTotal[direction][reason] {
				fmt.Fprintf(b, "urnet_security_enforced_total{direction=%q,reason=%q,protocol=%q} %d\n",
					secDirectionNames[direction], secReasonNames[reason], secProtoNames[protocol],
					securityEnforcedTotal[direction][reason][protocol].Load())
			}
		}
	}
}
