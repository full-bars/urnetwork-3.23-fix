package main

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The paid store is keyed by identity, so a credentialed proxy's grade is found
// by its identity key, and the address-keyed URL cache by the key's address.
func TestProxyGradeFor_IdentityKeyResolvesBothStores(t *testing.T) {
	key := identityKey("10.0.0.1:1080", "u")
	paid := &ProxyState{Proxies: map[string]ProxyEntry{key: {Graded: true, Score: 0.9}}}
	if g, ok := proxyGradeFor(key, paid, nil); !ok || g.Score != 0.9 {
		t.Fatalf("paid grade by identity key: ok=%v %+v", ok, g)
	}

	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{"10.0.0.2:1080": {Graded: true, Score: 0.7}}}
	urlKey := identityKey("10.0.0.2:1080", "u")
	if g, ok := proxyGradeFor(urlKey, &ProxyState{}, url); !ok || g.Score != 0.7 {
		t.Fatalf("URL cache is address-keyed and must resolve via the key's address: ok=%v %+v", ok, g)
	}
}

// The trim resolver is handed identity keys and must grade credentialed URL
// proxies too, otherwise they read as ungraded and shed before graded ones.
func TestBuildTrimGradeResolver_CredentialedURLProxy(t *testing.T) {
	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{"10.0.0.2:1080": {Graded: true, Score: 0.95}}}
	resolve := buildTrimGradeResolver(&ProxyState{}, url)
	if score, ok := resolve(identityKey("10.0.0.2:1080", "u")); !ok || score != 0.95 {
		t.Fatalf("credentialed URL proxy must resolve its grade: ok=%v score=%v", ok, score)
	}
}

// The hub report is built from display strings; a credentialed proxy's grade
// lives under its identity key in proxy.state and must still be attached.
func TestBuildReport_GradesCredentialedProxyByIdentity(t *testing.T) {
	withTempHome(t)
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)

	addr := "10.0.0.1:1080"
	key := identityKey(addr, "u")
	registerBandwidthProxy(1, addr, key, 1<<20)
	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{
		key: {Graded: true, Score: 0.9, LastGraded: time.Now()},
	}}); err != nil {
		t.Fatal(err)
	}

	report := buildReport("node", "host", time.Now())
	var found *proxyReport
	for i := range report.Proxies {
		if report.Proxies[i].Address == addr {
			found = &report.Proxies[i]
		}
	}
	if found == nil {
		t.Fatalf("proxy missing from report: %+v", report.Proxies)
	}
	if !found.Graded || found.Score != 0.9 {
		t.Fatalf("credentialed proxy must carry its identity-keyed grade, got graded=%v score=%v", found.Graded, found.Score)
	}
}
