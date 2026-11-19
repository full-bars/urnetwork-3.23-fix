package main

import (
	"context"
	"fmt"

	"github.com/urnetwork/connect"
	"testing"
	"time"
)

func TestUDPInterruptedRecheckPreservesCoverage(t *testing.T) {
	now := time.Unix(200000, 0)
	settings := &connect.ProxySettings{Network: "tcp", Address: "example:1080"}
	store := newProxyUDPCheckStore(func(ctx context.Context, _ *connect.ProxySettings) proxyUDPResult {
		return proxyUDPResult{Status: proxyUDPOK}
	})
	store.Check(context.Background(), settings, now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store.Check(ctx, settings, now.Add(35*time.Minute))
	coverage := store.coverage([]string{settings.Key()}, now.Add(35*time.Minute))
	if coverage.OK != 1 {
		t.Fatalf("coverage after cancellation = %+v, want OK=1", coverage)
	}
}

func TestUDPSweepRefreshesBeforeCoverageExpiry(t *testing.T) {
	now := time.Unix(200000, 0)
	for _, status := range []proxyUDPStatus{proxyUDPOK, proxyUDPUnknown} {
		t.Run(status.String(), func(t *testing.T) {
			candidates := make([]udpProbeCandidate, 100)
			for i := range candidates {
				candidates[i] = udpProbeCandidate{key: fmt.Sprint(i), online: true, status: status, lastChecked: now.Add(-proxyUDPTTL(status) / 2)}
			}
			got := selectUDPProbeBatch(now, candidates, 24*time.Hour, 5*time.Minute, nil)
			if len(got) == 0 {
				t.Fatal("no refresh candidates before expiry")
			}
			if status == proxyUDPUnknown && len(got) < 17 {
				t.Fatalf("unknown batch = %d, want at least 17 to cover pool before expiry", len(got))
			}
		})
	}
}
