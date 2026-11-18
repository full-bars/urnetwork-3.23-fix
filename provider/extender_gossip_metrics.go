package main

import (
	"fmt"
	"strings"

	"github.com/urnetwork/connect/gossip"
)

// These counters belong to the current gossip role and restart with that role.
// Do not publish zeroes when gossip is absent: no role means no measurement.
func writeGossipDialMetrics(b *strings.Builder, stats *gossip.DialStats) {
	if stats == nil {
		return
	}
	fmt.Fprintln(b, "# HELP urnet_gossip_dial_attempts_total Outgoing gossip peering calls since the role started.")
	fmt.Fprintln(b, "# TYPE urnet_gossip_dial_attempts_total counter")
	fmt.Fprintf(b, "urnet_gossip_dial_attempts_total %d\n", stats.Attempts)
	fmt.Fprintln(b, "# HELP urnet_gossip_dial_failures_total Failed outgoing gossip peering calls by error class since the role started.")
	fmt.Fprintln(b, "# TYPE urnet_gossip_dial_failures_total counter")
	fmt.Fprintf(b, "urnet_gossip_dial_failures_total{class=\"timeout\"} %d\n", stats.Timeout)
	fmt.Fprintf(b, "urnet_gossip_dial_failures_total{class=\"refused\"} %d\n", stats.Refused)
	fmt.Fprintf(b, "urnet_gossip_dial_failures_total{class=\"canceled\"} %d\n", stats.Canceled)
	fmt.Fprintf(b, "urnet_gossip_dial_failures_total{class=\"other\"} %d\n", stats.Other)
}
