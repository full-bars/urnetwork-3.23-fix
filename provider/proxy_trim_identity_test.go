package main

import (
	"testing"

	"github.com/urnetwork/connect"
)

// registerBandwidthProxy registers a live proxy at index under key with the
// given cumulative traffic, the way the launch path does.
func registerBandwidthProxy(index int, address, key string, rx uint64) {
	connect.RegisterProxy(index, address, key)
	connect.RegisterProxyBandwidth(index).TotalRx.Store(rx)
}

// TestRunningProxyTraffic_KeyedByIdentity pins that the traffic map the trim and
// pool-shed rankings read is keyed by the same identity key as the running list
// and proxy.state. It used to be keyed by the parsed bare address, so a
// credentialed proxy's traffic was never found under its identity key and the
// earning protection silently did nothing.
func TestRunningProxyTraffic_KeyedByIdentity(t *testing.T) {
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)

	earnerKey := identityKey("10.0.0.1:1080", "u")
	registerBandwidthProxy(1, "10.0.0.1:1080", earnerKey, 10<<30)

	traffic := runningProxyTraffic()
	if traffic[earnerKey] != 10<<30 {
		t.Fatalf("traffic must be keyed by identity: traffic[%q]=%d, full map %v", earnerKey, traffic[earnerKey], traffic)
	}
}

// TestSelectWorstRunningProxies_KeepsCredentialedEarner is the end-to-end
// consequence: trimming to one proxy must shed the idle unauthenticated proxy,
// never the 10 GiB credentialed earner.
func TestSelectWorstRunningProxies_KeepsCredentialedEarner(t *testing.T) {
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)

	earnerKey := identityKey("10.0.0.1:1080", "u")
	idleKey := "10.0.0.2:1080"
	registerBandwidthProxy(1, "10.0.0.1:1080", earnerKey, 10<<30)
	registerBandwidthProxy(2, "10.0.0.2:1080", idleKey, 0)

	state := map[string]ProxyEntry{
		earnerKey: {Health: "up"},
		idleKey:   {Health: "up"},
	}
	// Tie the tiebreak the wrong way for the earner: its key sorts before the
	// idle one, so only the traffic rule can save it.
	shed := selectWorstRunningProxies(state, nil, runningProxyTraffic(), []string{earnerKey, idleKey}, 1)
	if len(shed) != 1 || shed[0] != idleKey {
		t.Fatalf("shed %q, want the idle proxy %q; the earner must be kept", shed, idleKey)
	}
}

// The trim preview ranks the running pool with the same identity-keyed state and
// traffic the live trim uses, so the running list must be identity keys too. It
// used to be parsed display addresses: a credentialed proxy then missed both
// lookups and read as unknown and idle.
func TestRunningProxyAddresses_ReturnsIdentityKeys(t *testing.T) {
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)

	credKey := identityKey("10.0.0.1:1080", "u")
	registerBandwidthProxy(1, "10.0.0.1:1080", credKey, 10<<30)
	registerBandwidthProxy(2, "10.0.0.2:1080", "10.0.0.2:1080", 0)

	got := map[string]bool{}
	for _, k := range runningProxyAddresses() {
		got[k] = true
	}
	if !got[credKey] || !got["10.0.0.2:1080"] || len(got) != 2 {
		t.Fatalf("running list must be identity keys, got %v", got)
	}
}

// End to end for the preview: with running, state and traffic all identity
// keyed, trimming to one proxy keeps the credentialed earner.
func TestTrimPreviewSelection_KeepsCredentialedEarner(t *testing.T) {
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)

	earnerKey := identityKey("10.0.0.1:1080", "u")
	idleKey := "10.0.0.2:1080"
	registerBandwidthProxy(1, "10.0.0.1:1080", earnerKey, 10<<30)
	registerBandwidthProxy(2, "10.0.0.2:1080", idleKey, 0)
	state := map[string]ProxyEntry{earnerKey: {Health: "up"}, idleKey: {Health: "up"}}

	shed := selectWorstRunningProxies(state, nil, runningProxyTraffic(), runningProxyAddresses(), 1)
	if len(shed) != 1 || shed[0] != idleKey {
		t.Fatalf("shed %q, want the idle proxy %q", shed, idleKey)
	}
}

// Two accounts at one shared gateway address are two running proxies. The trim
// preview must count and rank them separately: collapsing them to one bare
// address halves the count and shed the wrong account.
func TestTrimPreview_AccountsSharingAnAddressAreSeparateProxies(t *testing.T) {
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)

	const gw = "gw.example:1080"
	earnerKey, idleKey := identityKey(gw, "u1"), identityKey(gw, "u2")
	registerBandwidthProxy(1, gw, earnerKey, 10<<30)
	registerBandwidthProxy(2, gw, idleKey, 0)

	running := runningProxyAddresses()
	if len(running) != 2 {
		t.Fatalf("two accounts at one address must be two running proxies, got %q", running)
	}
	state := map[string]ProxyEntry{earnerKey: {Health: "up"}, idleKey: {Health: "up"}}
	shed := selectWorstRunningProxies(state, nil, runningProxyTraffic(), running, 1)
	if len(shed) != 1 || shed[0] != idleKey {
		t.Fatalf("shed %q, want the idle account %q and never the earner", shed, idleKey)
	}
}
