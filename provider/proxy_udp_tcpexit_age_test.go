package main

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A UDP success must not keep a remembered TCP exit alive. The store hands the
// probe a remembered TCP exit only while a lookup observed it recently, so a
// proxy whose TCP exit changed is eventually looked up again and its mismatch
// is found.
func TestProxyUDPCheckRemembersTcpExitOnlyUntilItExpires(t *testing.T) {
	var seen []string
	store := newProxyUDPCheckStore(func(ctx context.Context, _ *connect.ProxySettings) proxyUDPResult {
		cached, _ := ctx.Value(cachedProxyTCPExitKey{}).(string)
		seen = append(seen, cached)
		// a lookup happens when nothing is reusable, as checkProxyUDP does
		return proxyUDPResult{Status: proxyUDPOK, TCPExit: "198.51.100.1", UDPExit: "198.51.100.1", TCPExitObserved: cached == ""}
	})
	settings := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
	t0 := time.Unix(1_000_000, 0)
	ctx := context.Background()

	for _, step := range []struct {
		after time.Duration
		want  string // what the probe may reuse
	}{
		{0, ""},                                            // first check: nothing remembered
		{time.Hour, "198.51.100.1"},                        // fresh: reused, not renewed
		{3 * time.Hour, "198.51.100.1"},                    // still fresh, 3h after the lookup
		{proxyTCPExitMaxAge + time.Minute, ""},             // expired although UDP succeeded in between
		{proxyTCPExitMaxAge + 2*time.Hour, "198.51.100.1"}, // the new lookup restarted the window
	} {
		seen = nil
		store.Check(ctx, settings, t0.Add(step.after))
		if len(seen) != 1 || seen[0] != step.want {
			t.Fatalf("after %v the probe was offered %q, want %q", step.after, seen, step.want)
		}
	}
}

// A check that could not look the TCP exit up leaves the old observation to
// expire, so the next check tries the lookup again.
func TestProxyUDPCheckRetriesTcpExitAfterFailedLookup(t *testing.T) {
	var seen []string
	fail := false
	store := newProxyUDPCheckStore(func(ctx context.Context, _ *connect.ProxySettings) proxyUDPResult {
		cached, _ := ctx.Value(cachedProxyTCPExitKey{}).(string)
		seen = append(seen, cached)
		if fail {
			return proxyUDPResult{Status: proxyUDPUnknown, UDPExit: "198.51.100.1"}
		}
		return proxyUDPResult{Status: proxyUDPOK, TCPExit: "198.51.100.1", UDPExit: "198.51.100.1", TCPExitObserved: true}
	})
	settings := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.2:1080"}
	t0 := time.Unix(2_000_000, 0)
	store.Check(context.Background(), settings, t0)
	fail = true
	seen = nil
	store.Check(context.Background(), settings, t0.Add(proxyTCPExitMaxAge+time.Minute))
	store.Check(context.Background(), settings, t0.Add(proxyTCPExitMaxAge+2*time.Hour))
	if len(seen) != 2 || seen[0] != "" || seen[1] != "" {
		t.Fatalf("offered %q, want no reuse after the observation expired", seen)
	}
}
