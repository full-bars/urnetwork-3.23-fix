package main

import (
	"fmt"
	"strings"

	"github.com/urnetwork/connect"
)

// writeDirectFamilyMetrics emits Prometheus metrics for direct path dual-stack counters.
// Metrics report counts and byte sums per address family only, with no addresses or credentials.
func writeDirectFamilyMetrics(b *strings.Builder) {
	stats := connect.DirectDualStackStats()
	v4 := stats.IPv4.Snapshot()
	v6 := stats.IPv6.Snapshot()

	fmt.Fprintf(b, "# HELP urnet_direct_connects_total Direct path connections that succeeded, by address family.\n")
	fmt.Fprintf(b, "# TYPE urnet_direct_connects_total counter\n")
	fmt.Fprintf(b, "urnet_direct_connects_total{family=\"ipv4\"} %d\n", v4.ConnectsOK)
	fmt.Fprintf(b, "urnet_direct_connects_total{family=\"ipv6\"} %d\n", v6.ConnectsOK)

	fmt.Fprintf(b, "# HELP urnet_direct_connect_failures_total Direct path connection attempts that failed, by address family.\n")
	fmt.Fprintf(b, "# TYPE urnet_direct_connect_failures_total counter\n")
	fmt.Fprintf(b, "urnet_direct_connect_failures_total{family=\"ipv4\"} %d\n", v4.ConnectsFailed)
	fmt.Fprintf(b, "urnet_direct_connect_failures_total{family=\"ipv6\"} %d\n", v6.ConnectsFailed)

	fmt.Fprintf(b, "# HELP urnet_direct_bytes_total Direct path network bytes transferred, by address family and direction.\n")
	fmt.Fprintf(b, "# TYPE urnet_direct_bytes_total counter\n")
	fmt.Fprintf(b, "urnet_direct_bytes_total{dir=\"rx\",family=\"ipv4\"} %d\n", v4.RxBytes)
	fmt.Fprintf(b, "urnet_direct_bytes_total{dir=\"tx\",family=\"ipv4\"} %d\n", v4.TxBytes)
	fmt.Fprintf(b, "urnet_direct_bytes_total{dir=\"rx\",family=\"ipv6\"} %d\n", v6.RxBytes)
	fmt.Fprintf(b, "urnet_direct_bytes_total{dir=\"tx\",family=\"ipv6\"} %d\n", v6.TxBytes)

	fmt.Fprintf(b, "# HELP urnet_direct_last_success_timestamp_seconds Unix timestamp of the last successful direct connect by address family.\n")
	fmt.Fprintf(b, "# TYPE urnet_direct_last_success_timestamp_seconds gauge\n")
	fmt.Fprintf(b, "urnet_direct_last_success_timestamp_seconds{family=\"ipv4\"} %d\n", v4.LastSuccessUnix)
	fmt.Fprintf(b, "urnet_direct_last_success_timestamp_seconds{family=\"ipv6\"} %d\n", v6.LastSuccessUnix)
}
