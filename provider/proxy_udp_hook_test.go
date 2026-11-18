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

func TestUDPProbeMalformedSourceKeepsValidProxies(t *testing.T) {
	home := withTempHome(t)
	file := filepath.Join(home, "paid.txt")
	if err := os.WriteFile(file, []byte("shared:1234::fake\nvalid:1234:alice:fake\n"), 0600); err != nil {
		t.Fatal(err)
	}
	paid, trusted := udpPaidDesiredSet(&ProxyState{Source: file})
	if !trusted {
		t.Fatal("malformed source line blocked valid proxies")
	}
	key := (&connect.ProxySettings{Address: "valid:1234", Auth: &proxy.Auth{User: "alice"}}).Key()
	if paid[key] == nil {
		t.Fatal("valid proxy missing")
	}
	if paid["shared:1234"] != nil {
		t.Fatal("malformed source line added as unauthenticated proxy")
	}
	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{"shared:1234": {User: "url", Password: "fake"}}}
	urlKey := (&connect.ProxySettings{Address: "shared:1234", Auth: &proxy.Auth{User: "url"}}).Key()
	if got := urlUDPProbeSettings([]string{urlKey}, paid, url); len(got) != 1 {
		t.Fatal("rejected primary source must not claim URL address")
	}
}

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
	if err := writeProxyState(&ProxyState{Source: file, Proxies: map[string]ProxyEntry{alice: {ID: 27, Source: "file", Health: "up", Graded: true, Score: .8}, bob: {Source: "file", Health: proxyHealthParked}}}); err != nil {
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
	var runs int
	runPaidUDPProbeTickWith(context.Background(), func(_ context.Context, store *proxyUDPCheckStore, selected []*connect.ProxySettings, _ time.Time, window, tick time.Duration, _ float64, enabled bool) {
		runs++
		if len(selected) == 1 && selected[0].Index != 27 {
			t.Errorf("paid index = %d, want 27", selected[0].Index)
		}
		if store != globalProxyUDPCheck || !enabled || window != 24*time.Hour || tick != proxyReaperInterval || len(selected) != 1 || selected[0].Key() != alice {
			t.Error("paid tick passed incorrect arguments to runner")
		}
	})
	if runs != 1 {
		t.Fatalf("paid runner calls = %d", runs)
	}
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
	if paid, trusted := udpPaidDesiredSet(before); !trusted || len(paid) != 0 {
		t.Fatal("rejected source must be trusted and claim no proxies")
	}
}

func TestURLUDPProbeTickKillSwitchAndState(t *testing.T) {
	withTempHome(t)
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)
	resetTrimHeld()
	t.Cleanup(resetTrimHeld)
	t.Cleanup(resetProbeConfigCache)
	state := &ProxyState{Proxies: map[string]ProxyEntry{"url:2": {ID: 31, Source: "url", Health: "up", Graded: true, Score: .7}, "held:3": {Health: proxyHealthParked}}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{"url:2": {Graded: true, Score: .8}, "held:3": {}, "offline:4": {}}}); err != nil {
		t.Fatal(err)
	}
	before, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	connect.NoteLiveProxyHealth("url:2", "recently_offline", time.Now())
	connect.NoteLiveProxyHealth("held:3", "up", time.Time{})
	setTrimHeld("held:3", true)
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
	var runs int
	runURLUDPProbeTickWith(context.Background(), func(_ context.Context, store *proxyUDPCheckStore, selected []*connect.ProxySettings, _ time.Time, window, tick time.Duration, _ float64, enabled bool) {
		runs++
		if len(selected) == 1 && selected[0].Index != 31 {
			t.Errorf("URL index = %d, want 31", selected[0].Index)
		}
		if store != globalProxyUDPCheck || !enabled || window != 24*time.Hour || tick != proxyReaperInterval || len(selected) != 1 || selected[0].Key() != "url:2" {
			t.Error("URL tick passed incorrect arguments to runner")
		}
	})
	if runs != 1 {
		t.Fatalf("URL runner calls = %d", runs)
	}
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
