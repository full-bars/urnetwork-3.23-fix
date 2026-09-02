package connect

// Exported accessors for the embedded probe target table.
//
// The table itself (probeHostNames, probeResolverIps, probePassFraction,
// sampleProbeTargets) is upstream-parity and deliberately stays lowercase:
// the provider package needs read access to drive its own Stage-1 quality
// probe (see provider/proxy_table_probe.go), and these wrappers are the
// only surface it is allowed to see. The reputation-class exclusion and the
// "positive evidence only" design notes live in ip_probe_targets.go and
// apply unchanged.

// ProbeHostNames returns the health-class hostname table, dialed at :443.
// The returned slice is a COPY: callers must not be able to mutate the
// package-owned table, which concurrent samplers read.
func ProbeHostNames() []string { return append([]string(nil), probeHostNames...) }

// ProbeHostCount returns the size of the health-class hostname table
// without copying it — callers that only need the length should not pay for
// a full table copy.
func ProbeHostCount() int { return len(probeHostNames) }

// ProbeResolverIps returns the dns-class resolver table, queried at :53.
// The returned slice is a COPY.
func ProbeResolverIps() []string { return append([]string(nil), probeResolverIps...) }

// ProbePassFraction is the share of a pass's targets that must answer for a
// provider to qualify (0.6). Deliberately below 1 (anti-bot egress drops)
// and above 1/2 (a minority of answers is not a demonstration of dialing
// the internet).
func ProbePassFraction() float64 { return probePassFraction }

// SampleProbeTargets returns one pass's worth of targets for a provider:
// n health hostnames and one resolver ip, chosen deterministically from
// seed. See sampleProbeTargets in ip_probe_targets.go for the rotation
// semantics (disjoint blocks, deterministic reproduction).
func SampleProbeTargets(seed uint64, n int) (hosts []string, resolver string) {
	return sampleProbeTargets(seed, n)
}
