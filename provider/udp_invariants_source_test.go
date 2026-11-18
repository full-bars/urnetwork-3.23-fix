package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestUDPInvariantMalformedPrimaryCannotClaimURLAddress(t *testing.T) {
	home := withTempHome(t)
	path := filepath.Join(home, "source.txt")
	if err := os.WriteFile(path, []byte("shared:1234:alice:pw:extra\nvalid:5678:bob:pw\n"), 0600); err != nil {
		t.Fatal(err)
	}
	paid, trusted := udpPaidDesiredSet(&ProxyState{Source: path})
	if !trusted {
		return
	}
	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{"shared:1234": {}}}
	if got := urlUDPProbeSettings([]string{"shared:1234"}, paid, url); len(got) != 0 {
		t.Fatal("malformed primary entry allowed its address to be claimed by URL")
	}
}

func TestUDPInvariantURLSweepProtectsUnknownPaidOwnership(t *testing.T) {
	for _, tc := range []struct {
		name  string
		empty bool
	}{{"missing", false}, {"empty", true}} {
		t.Run(tc.name, func(t *testing.T) {
			home := withTempHome(t)
			connect.ResetProxyHealthForTesting()
			t.Cleanup(connect.ResetProxyHealthForTesting)
			t.Cleanup(resetProbeConfigCache)
			path := filepath.Join(home, "source.txt")
			if tc.empty {
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := writeProxyState(&ProxyState{Source: path, Proxies: map[string]ProxyEntry{"127.0.0.1:24001": {ID: 12}}}); err != nil {
				t.Fatal(err)
			}
			if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{"127.0.0.1:24001": {}}}); err != nil {
				t.Fatal(err)
			}
			connect.NoteLiveProxyHealth("127.0.0.1:24001", "up", time.Time{})
			writeReviewProbeOverride(t, map[string]any{"enabled": true})
			runs := 0
			runURLUDPProbeTickWith(context.Background(), func(_ context.Context, _ *proxyUDPCheckStore, _ []*connect.ProxySettings, _ time.Time, _, _ time.Duration, _ float64, _ bool) {
				runs++
			})
			if runs != 0 {
				t.Fatalf("URL sweep ran %d times without trusted primary ownership", runs)
			}
		})
	}
}
