package main

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func isolatedFailureHistory(t *testing.T) *proxyFailureHistory {
	t.Helper()
	hist := &proxyFailureHistory{failures: map[string]int{}}
	saved := globalProxyFailureHistory
	globalProxyFailureHistory = hist
	t.Cleanup(func() { globalProxyFailureHistory = saved })
	return hist
}

// A shed credentialed URL proxy must actually leave the URL cache. state.Proxies
// is keyed by identity while the cache is keyed by bare address; shedding used
// to hand the identity key straight to a cache delete, so the entry stayed,
// the pool logged "shed" and nothing was removed.
func TestShedPoolToTarget_RemovesCredentialedURLProxyFromCache(t *testing.T) {
	withTempHome(t)
	hist := isolatedFailureHistory(t)
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)

	deadKey := identityKey("2.2.2.2:1080", "u")
	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{
		deadKey:        {ID: 2, Health: "dead", Source: "url"}, // shed first
		"1.1.1.1:1080": {ID: 1, Health: "up", Source: "url"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"2.2.2.2:1080": {},
		"1.1.1.1:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}

	shedPoolToTarget(1)

	got, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Cache["2.2.2.2:1080"]; ok {
		t.Fatalf("shed credentialed URL proxy is still in the URL cache: %v", got.Cache)
	}
	if _, ok := got.Cache["1.1.1.1:1080"]; !ok {
		t.Fatalf("the surviving proxy must stay cached: %v", got.Cache)
	}
	// Half the backoff from now: far from both edges, so no timing race.
	if hist.Eligible(deadKey, time.Now().Add(shedBackoff/2)) {
		t.Fatalf("the shed backoff must be recorded under the identity key")
	}
}

// Among equally healthy URL proxies the shed ranking protects traffic. A
// credentialed earner's traffic is keyed by identity, so it must outrank an
// idle proxy instead of reading as zero.
func TestShedPoolToTarget_KeepsCredentialedEarner(t *testing.T) {
	withTempHome(t)
	isolatedFailureHistory(t)
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)

	earnerKey := identityKey("1.1.1.1:1080", "u")
	registerBandwidthProxy(1, "1.1.1.1:1080", earnerKey, 10<<30)
	registerBandwidthProxy(2, "2.2.2.2:1080", "2.2.2.2:1080", 0)
	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{
		earnerKey:      {ID: 1, Health: "up", Source: "url"},
		"2.2.2.2:1080": {ID: 2, Health: "up", Source: "url"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.1.1.1:1080": {},
		"2.2.2.2:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}

	shedPoolToTarget(1)

	got, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Cache["1.1.1.1:1080"]; !ok {
		t.Fatalf("the credentialed earner was shed: cache=%v", got.Cache)
	}
	if _, ok := got.Cache["2.2.2.2:1080"]; ok {
		t.Fatalf("the idle proxy should have been shed: cache=%v", got.Cache)
	}
}
