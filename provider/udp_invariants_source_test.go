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
		t.Fatal("valid source lines must remain usable")
	}
	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{"shared:1234": {}}}
	if got := urlUDPProbeSettings([]string{"shared:1234"}, paid, url); len(got) != 1 {
		t.Fatal("rejected primary entry must not claim the URL address")
	}
}

func TestUDPInvariantMalformedPrimaryDoesNotPauseSweep(t *testing.T) {
	home := withTempHome(t)
	path := filepath.Join(home, "source.txt")
	if err := os.WriteFile(path, []byte("first:1234:u:p\nbroken:abc:u:p\nlast:5678:u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{Source: path}); err != nil {
		t.Fatal(err)
	}
	writeReviewProbeOverride(t, map[string]any{"enabled": true})
	t.Cleanup(resetProbeConfigCache)
	runs := 0
	runPaidUDPProbeTickWith(context.Background(), func(_ context.Context, _ *proxyUDPCheckStore, _ []*connect.ProxySettings, _ time.Time, _, _ time.Duration, _ float64, _ bool) {
		runs++
	})
	if runs != 1 {
		t.Fatalf("paid sweep ran %d times", runs)
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
