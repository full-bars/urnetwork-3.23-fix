package main

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect/gossip"
)

func TestGossipDialMetrics(t *testing.T) {
	var b strings.Builder
	writeGossipDialMetrics(&b, nil)
	if b.Len() != 0 {
		t.Fatalf("absent role emitted metrics: %s", b.String())
	}
	writeGossipDialMetrics(&b, &gossip.DialStats{Attempts: 28, Timeout: 1, Refused: 2, Canceled: 3, Other: 4, Backoff: 5, NoAddrs: 6, Gated: 7})
	for _, want := range []string{
		"# TYPE urnet_gossip_dial_attempts_total counter\n",
		"urnet_gossip_dial_attempts_total 28\n",
		"# TYPE urnet_gossip_dial_failures_total counter\n",
		"urnet_gossip_dial_failures_total{class=\"timeout\"} 1\n",
		"urnet_gossip_dial_failures_total{class=\"refused\"} 2\n",
		"urnet_gossip_dial_failures_total{class=\"canceled\"} 3\n",
		"urnet_gossip_dial_failures_total{class=\"backoff\"} 5\n",
		"urnet_gossip_dial_failures_total{class=\"no_addrs\"} 6\n",
		"urnet_gossip_dial_failures_total{class=\"gated\"} 7\n",
		"urnet_gossip_dial_failures_total{class=\"other\"} 4\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in %s", want, b.String())
		}
	}
}
