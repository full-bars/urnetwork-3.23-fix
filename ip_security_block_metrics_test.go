package connect

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// The block metrics are package-level counters. Every test snapshots all five
// families and asserts the exact delta, so "incremented exactly once, others
// unchanged" is checked wholesale instead of per cell.

type secMetrics struct {
	decisions [secDirCount][3]int64
	blocked   [secDirCount][secReasonCount][3]int64
	ports     [secDirCount][3][len(securityBlockedPortLabels) + 1]int64
	decided   [secDirCount][secReasonCount][3]int64
	enforced  [secDirCount][secReasonCount][3]int64
}

func (m *secMetrics) addDecision(direction, result int) { m.decisions[direction][result] += 1 }
func (m *secMetrics) addBlocked(direction, reason, proto int) {
	m.blocked[direction][reason][proto] += 1
}
func (m *secMetrics) addPort(direction, proto, label int) { m.ports[direction][proto][label] += 1 }
func (m *secMetrics) addDecided(direction, reason, proto int) {
	m.decided[direction][reason][proto] += 1
}
func (m *secMetrics) addEnforced(direction, reason, proto int) {
	m.enforced[direction][reason][proto] += 1
}

func readSecMetrics() secMetrics {
	var m secMetrics
	for d := range securityDecisionsTotal {
		for r := range securityDecisionsTotal[d] {
			m.decisions[d][r] = securityDecisionsTotal[d][r].Load()
		}
	}
	for d := range securityBlockedTotal {
		for reason := range securityBlockedTotal[d] {
			for p := range securityBlockedTotal[d][reason] {
				m.blocked[d][reason][p] = securityBlockedTotal[d][reason][p].Load()
			}
		}
	}
	for d := range securityBlockedPortTotal {
		for p := range securityBlockedPortTotal[d] {
			for port := range securityBlockedPortTotal[d][p] {
				m.ports[d][p][port] = securityBlockedPortTotal[d][p][port].Load()
			}
		}
	}
	for d := range securityDecidedTotal {
		for reason := range securityDecidedTotal[d] {
			for p := range securityDecidedTotal[d][reason] {
				m.decided[d][reason][p] = securityDecidedTotal[d][reason][p].Load()
			}
		}
	}
	for d := range securityEnforcedTotal {
		for reason := range securityEnforcedTotal[d] {
			for p := range securityEnforcedTotal[d][reason] {
				m.enforced[d][reason][p] = securityEnforcedTotal[d][reason][p].Load()
			}
		}
	}
	return m
}

func diffSecMetrics(before, after secMetrics) secMetrics {
	var m secMetrics
	for d := range before.decisions {
		for r := range before.decisions[d] {
			m.decisions[d][r] = after.decisions[d][r] - before.decisions[d][r]
		}
	}
	for d := range before.blocked {
		for reason := range before.blocked[d] {
			for p := range before.blocked[d][reason] {
				m.blocked[d][reason][p] = after.blocked[d][reason][p] - before.blocked[d][reason][p]
			}
		}
	}
	for d := range before.ports {
		for p := range before.ports[d] {
			for port := range before.ports[d][p] {
				m.ports[d][p][port] = after.ports[d][p][port] - before.ports[d][p][port]
			}
		}
	}
	for d := range before.decided {
		for reason := range before.decided[d] {
			for p := range before.decided[d][reason] {
				m.decided[d][reason][p] = after.decided[d][reason][p] - before.decided[d][reason][p]
			}
		}
	}
	for d := range before.enforced {
		for reason := range before.enforced[d] {
			for p := range before.enforced[d][reason] {
				m.enforced[d][reason][p] = after.enforced[d][reason][p] - before.enforced[d][reason][p]
			}
		}
	}
	return m
}

func expectSecDelta(t *testing.T, before secMetrics, want secMetrics) {
	t.Helper()
	if got := diffSecMetrics(before, readSecMetrics()); got != want {
		t.Fatalf("metric delta mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func newEgressPolicyForTest() SecurityPolicy {
	return NewEgressSecurityPolicy(
		nil,
		DefaultCfaaSecurityPolicySettings(),
		DefaultDmcaSecurityPolicySettings(),
		DefaultWebStandardSettings(),
		DefaultSecurityPolicyStatsCollector(),
	)
}

func newIngressPolicyForTest() SecurityPolicy {
	return NewIngressSecurityPolicy(DefaultCfaaSecurityPolicySettings(), DefaultSecurityPolicyStatsCollector())
}

// metricFlowPath is dmcaPath: a private source, a public destination, so the
// egress policy inspects the destination and the ingress policy the source.
func metricFlowPath(proto IpProtocol, sport, dport int, syn bool) *IpPath {
	return dmcaPath(proto, sport, dport, syn)
}

func TestSecurityBlockMetricsNonPublicDestination(t *testing.T) {
	policy := newEgressPolicyForTest()
	path := metricFlowPath(IpProtocolTcp, 40001, 50000, false)
	path.DestinationIp = net.ParseIP("10.0.0.1")
	before := readSecMetrics()
	result, err := policy.Inspect(protocol.ProvideMode_Public, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result != SecurityPolicyResultIncident {
		t.Fatalf("private destination = %v, want incident", result)
	}
	var want secMetrics
	want.addDecision(secDirEgress, int(SecurityPolicyResultIncident))
	want.addBlocked(secDirEgress, secReasonNonPublicDestination, dmcaProtocolBucket(IpProtocolTcp))
	expectSecDelta(t, before, want)
}

func TestSecurityBlockMetricsBlockedIpEgress(t *testing.T) {
	policy := newEgressPolicyForTest()
	path := metricFlowPath(IpProtocolTcp, 40002, 50000, false)
	path.DestinationIp = firstCfaaBlockedIp4(t)
	before := readSecMetrics()
	result, err := policy.Inspect(protocol.ProvideMode_Public, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result != SecurityPolicyResultDrop {
		t.Fatalf("blocked destination ip = %v, want drop", result)
	}
	var want secMetrics
	want.addDecision(secDirEgress, int(SecurityPolicyResultDrop))
	want.addBlocked(secDirEgress, secReasonBlockedIp, dmcaProtocolBucket(IpProtocolTcp))
	expectSecDelta(t, before, want)
}

func TestSecurityBlockMetricsBlockedPortEgress(t *testing.T) {
	policy := newEgressPolicyForTest()
	path := metricFlowPath(IpProtocolTcp, 40003, 6881, false)
	label := securityBlockedPortIndex(6881)
	if label == len(securityBlockedPortLabels) {
		t.Fatal("setup: 6881 must have a named port label")
	}
	before := readSecMetrics()
	result, err := policy.Inspect(protocol.ProvideMode_Public, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result != SecurityPolicyResultDrop {
		t.Fatalf("bittorrent port = %v, want drop", result)
	}
	var want secMetrics
	want.addDecision(secDirEgress, int(SecurityPolicyResultDrop))
	want.addBlocked(secDirEgress, secReasonBlockedPort, dmcaProtocolBucket(IpProtocolTcp))
	want.addPort(secDirEgress, dmcaProtocolBucket(IpProtocolTcp), label)
	expectSecDelta(t, before, want)
}

func TestSecurityBlockMetricsBittorrentDecidedAndEnforced(t *testing.T) {
	policy := newEgressPolicyForTest()
	path := metricFlowPath(IpProtocolTcp, 40004, 51413, false)
	before := readSecMetrics()
	result, err := policy.Inspect(protocol.ProvideMode_Public, path, btHandshake())
	if err != nil {
		t.Fatal(err)
	}
	if result != SecurityPolicyResultIncident {
		t.Fatalf("bittorrent signature under default settings = %v, want incident", result)
	}
	var want secMetrics
	want.addDecision(secDirEgress, int(SecurityPolicyResultIncident))
	want.addBlocked(secDirEgress, secReasonDmcaBittorrent, dmcaProtocolBucket(IpProtocolTcp))
	want.addDecided(secDirEgress, secReasonDmcaBittorrent, dmcaProtocolBucket(IpProtocolTcp))
	want.addEnforced(secDirEgress, secReasonDmcaBittorrent, dmcaProtocolBucket(IpProtocolTcp))
	expectSecDelta(t, before, want)
}

// A log-only node still classifies terminal verdicts; they are decided but
// never enforced, and the delta shows exactly that.
func TestSecurityBlockMetricsBittorrentDowngradedStaysDecided(t *testing.T) {
	settings := DefaultDmcaSecurityPolicySettings()
	settings.LogOnly = true
	policy := NewEgressSecurityPolicy(
		nil,
		DefaultCfaaSecurityPolicySettings(),
		settings,
		DefaultWebStandardSettings(),
		DefaultSecurityPolicyStatsCollector(),
	)
	path := metricFlowPath(IpProtocolTcp, 40005, 51413, false)
	before := readSecMetrics()
	result, err := policy.Inspect(protocol.ProvideMode_Public, path, btHandshake())
	if err != nil {
		t.Fatal(err)
	}
	if result != SecurityPolicyResultAllow {
		t.Fatalf("log-only bittorrent signature = %v, want allow", result)
	}
	var want secMetrics
	want.addDecision(secDirEgress, int(SecurityPolicyResultAllow))
	want.addDecided(secDirEgress, secReasonDmcaBittorrent, dmcaProtocolBucket(IpProtocolTcp))
	expectSecDelta(t, before, want)
}

func TestSecurityBlockMetricsEncryptedDecidedAndEnforced(t *testing.T) {
	settings := DefaultDmcaSecurityPolicySettings()
	policy := NewEgressSecurityPolicy(
		nil,
		DefaultCfaaSecurityPolicySettings(),
		settings,
		DefaultWebStandardSettings(),
		DefaultSecurityPolicyStatsCollector(),
	)
	synPath := metricFlowPath(IpProtocolTcp, 40006, 50000, true)
	dataPath := metricFlowPath(IpProtocolTcp, 40006, 50000, false)
	before := readSecMetrics()
	if result, _ := policy.Inspect(protocol.ProvideMode_Public, synPath, nil); result != SecurityPolicyResultAllow {
		t.Fatalf("syn = %v, want allow (inspecting)", result)
	}
	var last SecurityPolicyResult
	for i := 0; i < settings.EncryptedDecisionPackets; i += 1 {
		last, _ = policy.Inspect(protocol.ProvideMode_Public, dataPath, encryptedPayload(512))
	}
	if last != SecurityPolicyResultDrop {
		t.Fatalf("encrypted flow = %v, want drop", last)
	}
	var want secMetrics
	want.decisions[secDirEgress][int(SecurityPolicyResultAllow)] = int64(settings.EncryptedDecisionPackets)
	want.addDecision(secDirEgress, int(SecurityPolicyResultDrop))
	want.addBlocked(secDirEgress, secReasonDmcaEncrypted, dmcaProtocolBucket(IpProtocolTcp))
	want.addDecided(secDirEgress, secReasonDmcaEncrypted, dmcaProtocolBucket(IpProtocolTcp))
	want.addEnforced(secDirEgress, secReasonDmcaEncrypted, dmcaProtocolBucket(IpProtocolTcp))
	expectSecDelta(t, before, want)
}

func TestSecurityBlockMetricsAllowMovesOnlyDecisions(t *testing.T) {
	policy := newEgressPolicyForTest()
	path := metricFlowPath(IpProtocolTcp, 40007, 443, false)
	before := readSecMetrics()
	result, err := policy.Inspect(protocol.ProvideMode_Public, path, tlsClientHello())
	if err != nil {
		t.Fatal(err)
	}
	if result != SecurityPolicyResultAllow {
		t.Fatalf("sanctioned tls flow = %v, want allow", result)
	}
	var want secMetrics
	want.addDecision(secDirEgress, int(SecurityPolicyResultAllow))
	expectSecDelta(t, before, want)
}

func TestSecurityBlockMetricsIngressMirror(t *testing.T) {
	policy := newIngressPolicyForTest()

	blocked := metricFlowPath(IpProtocolTcp, 40008, 50000, false)
	blocked.SourceIp = firstCfaaBlockedIp4(t)
	before := readSecMetrics()
	result, err := policy.Inspect(protocol.ProvideMode_Public, blocked, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result != SecurityPolicyResultDrop {
		t.Fatalf("blocked source ip = %v, want drop", result)
	}
	var want secMetrics
	want.addDecision(secDirIngress, int(SecurityPolicyResultDrop))
	want.addBlocked(secDirIngress, secReasonBlockedIp, dmcaProtocolBucket(IpProtocolTcp))
	expectSecDelta(t, before, want)

	port := metricFlowPath(IpProtocolUdp, 6889, 50000, false)
	label := securityBlockedPortIndex(6889)
	if label == len(securityBlockedPortLabels) {
		t.Fatal("setup: 6889 must have a named port label")
	}
	before = readSecMetrics()
	result, err = policy.Inspect(protocol.ProvideMode_Public, port, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result != SecurityPolicyResultDrop {
		t.Fatalf("blocked source port = %v, want drop", result)
	}
	want = secMetrics{}
	want.addDecision(secDirIngress, int(SecurityPolicyResultDrop))
	want.addBlocked(secDirIngress, secReasonBlockedPort, dmcaProtocolBucket(IpProtocolUdp))
	want.addPort(secDirIngress, dmcaProtocolBucket(IpProtocolUdp), label)
	expectSecDelta(t, before, want)
}

// The port label set is closed at the rule list plus "other": no input can
// grow it.
func TestSecurityBlockedPortLabelSetIsBounded(t *testing.T) {
	distinct := map[int]bool{}
	for port := -1; port <= 65536; port += 1 {
		index := securityBlockedPortIndex(port)
		if index < 0 || len(securityBlockedPortLabels) < index {
			t.Fatalf("port %d mapped to out-of-range label %d", port, index)
		}
		distinct[index] = true
	}
	if len(distinct) != len(securityBlockedPortLabels)+1 {
		t.Fatalf("distinct port labels = %d, want %d", len(distinct), len(securityBlockedPortLabels)+1)
	}
	for i, rulePort := range securityBlockedPortLabels {
		if securityBlockedPortIndex(rulePort) != i {
			t.Fatalf("named rule port %d does not map to its own label", rulePort)
		}
	}
	other := len(securityBlockedPortLabels)
	if securityBlockedPortIndex(31337) != other {
		t.Fatal("an unnamed user port must fold to other")
	}
	if securityBlockedPortIndex(0) != other {
		t.Fatal("an unnamed privileged port must fold to other")
	}
	if securityBlockedPortIndex(-1) != other {
		t.Fatal("a negative port must fold to other")
	}
}

// The exposition text is well formed: one HELP/TYPE per family, no duplicate
// series, and every family's series count matches its closed label set.
func TestSecurityBlockMetricsRenderIsWellFormed(t *testing.T) {
	var b strings.Builder
	writeSecurityBlockMetrics(&b)
	text := b.String()

	families := []struct {
		name   string
		series int
	}{
		{"urnet_security_decisions_total", secDirCount * 3},
		{"urnet_security_blocked_total", secDirCount * secReasonCount * 3},
		{"urnet_security_blocked_port_total", secDirCount * 3 * (len(securityBlockedPortLabels) + 1)},
		{"urnet_security_decided_total", secDirCount * len(secDmcaReasonIndexes) * 3},
		{"urnet_security_enforced_total", secDirCount * len(secDmcaReasonIndexes) * 3},
	}
	for _, family := range families {
		if got := strings.Count(text, "# HELP "+family.name+" "); got != 1 {
			t.Fatalf("HELP lines for %s = %d, want 1", family.name, got)
		}
		if got := strings.Count(text, "# TYPE "+family.name+" "); got != 1 {
			t.Fatalf("TYPE lines for %s = %d, want 1", family.name, got)
		}
		if got := strings.Count(text, "\n"+family.name+"{"); got != family.series {
			t.Fatalf("series for %s = %d, want %d", family.name, got, family.series)
		}
	}

	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sep := strings.LastIndex(line, "}")
		if sep < 0 {
			t.Fatalf("series line lacks labels: %q", line)
		}
		key := line[:sep+1]
		if seen[key] {
			t.Fatalf("duplicate series: %s", key)
		}
		seen[key] = true
		if _, err := fmt.Sscanf(line[sep+1:], " %d", new(int)); err != nil {
			t.Fatalf("series value is not an integer: %q", line)
		}
	}
}
