package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

func TestURLUDPProbeSettingsOwnership(t *testing.T) {
	paid := &connect.ProxySettings{Address: "shared:1", Auth: &proxy.Auth{User: "paid", Password: "secret"}}
	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"shared:1":  {User: "other", Password: "other-secret"},
		"url:2":     {User: "account", Password: "secret"},
		"offline:3": {},
	}}
	key := (&connect.ProxySettings{Address: "url:2", Auth: &proxy.Auth{User: "account"}}).Key()
	shared := (&connect.ProxySettings{Address: "shared:1", Auth: &proxy.Auth{User: "other"}}).Key()
	got := urlUDPProbeSettings([]string{key, shared, "offline:wrong"}, map[string]*connect.ProxySettings{paid.Key(): paid}, url)
	if len(got) != 1 || got[0].Key() != key || got[0].Auth == nil || got[0].Auth.Password != "secret" {
		t.Fatal("URL selection or credential ownership incorrect")
	}
}

func TestPaidUDPProbeTick(t *testing.T) {
	home := withTempHome(t)
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)
	t.Cleanup(resetProbeConfigCache)
	file := filepath.Join(home, "paid.txt")
	if err := os.WriteFile(file, []byte("127.0.0.1:1234:alice:fake\n127.0.0.1:1234:bob:fake2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	alice := (&connect.ProxySettings{Address: "127.0.0.1:1234", Auth: &proxy.Auth{User: "alice"}}).Key()
	bob := (&connect.ProxySettings{Address: "127.0.0.1:1234", Auth: &proxy.Auth{User: "bob"}}).Key()
	if err := writeProxyState(&ProxyState{Source: file, Proxies: map[string]ProxyEntry{alice: {Source: "file", Health: "up", Graded: true, Score: .8}, bob: {Source: "file", Health: proxyHealthParked}}}); err != nil {
		t.Fatal(err)
	}
	before, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	connect.NoteLiveProxyHealth(alice, "up", time.Time{})
	connect.NoteLiveProxyHealth("url:2", "up", time.Time{})
	calls := 0
	old := globalProxyUDPCheck
	globalProxyUDPCheck = newProxyUDPCheckStore(func(_ context.Context, ps *connect.ProxySettings) proxyUDPResult {
		calls++
		if ps.Key() != alice || ps.Auth == nil || ps.Auth.Password != "fake" {
			t.Error("wrong identity or credentials")
		}
		return proxyUDPResult{Status: proxyUDPOK}
	})
	t.Cleanup(func() { globalProxyUDPCheck = old })
	writeReviewProbeOverride(t, map[string]any{"enabled": false})
	runPaidUDPProbeTick(context.Background())
	if calls != 0 {
		t.Fatal("disabled hook probed")
	}
	writeReviewProbeOverride(t, map[string]any{"enabled": true})
	runPaidUDPProbeTick(context.Background())
	if calls != 1 {
		t.Fatalf("probe calls = %d, want 1", calls)
	}
	after, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("grade or park state changed")
	}
	if err := os.WriteFile(file, []byte("malformed:secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, trusted := udpPaidDesiredSet(before); trusted {
		t.Fatal("malformed source treated as trusted")
	}
}

func TestURLUDPProbeTickKillSwitchAndState(t *testing.T) {
	withTempHome(t)
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)
	t.Cleanup(resetProbeConfigCache)
	state := &ProxyState{Proxies: map[string]ProxyEntry{"url:2": {Source: "url", Health: "up", Graded: true, Score: .7}, "held:3": {Health: proxyHealthParked}}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{"url:2": {Graded: true, Score: .8}, "offline:4": {}}}); err != nil {
		t.Fatal(err)
	}
	before, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	connect.NoteLiveProxyHealth("url:2", "up", time.Time{})
	calls := 0
	old := globalProxyUDPCheck
	globalProxyUDPCheck = newProxyUDPCheckStore(func(_ context.Context, ps *connect.ProxySettings) proxyUDPResult {
		calls++
		if ps.Key() != "url:2" {
			t.Error("wrong identity")
		}
		return proxyUDPResult{Status: proxyUDPOK}
	})
	t.Cleanup(func() { globalProxyUDPCheck = old })
	writeReviewProbeOverride(t, map[string]any{"enabled": false})
	runURLUDPProbeTick(context.Background())
	if calls != 0 {
		t.Fatal("disabled hook probed")
	}
	writeReviewProbeOverride(t, map[string]any{"enabled": true})
	runURLUDPProbeTick(context.Background())
	if calls != 1 {
		t.Fatalf("probe calls = %d, want 1", calls)
	}
	after, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("grade or park state changed")
	}
}
