package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestUDPInvariantURLSweepSurvivesUnavailablePaidSource(t *testing.T) {
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
			runURLUDPProbeTickWith(context.Background(), func(_ context.Context, _ *proxyUDPCheckStore, selected []*connect.ProxySettings, _ time.Time, _, _ time.Duration, _ float64, _ bool) {
				runs++
				if len(selected) != 1 || selected[0].Key() != "127.0.0.1:24001" {
					t.Errorf("selected=%v, want URL identity", selected)
				}
			})
			if runs != 1 {
				t.Fatalf("URL sweep runs=%d, want 1 despite unavailable paid source", runs)
			}
		})
	}
}
