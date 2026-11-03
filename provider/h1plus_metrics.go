package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// h1PlusProcessStats is the one H1+ collector every identity's platform transport
// reports to. H1+ is the authenticated custom upgrade each H1 dial tries before
// falling back to a plain websocket.
var h1PlusProcessStats = &connect.H1PlusStats{}

// writeH1PlusMetrics exports whether H1+ is switched on and what the dials did.
func writeH1PlusMetrics(b *strings.Builder) {
	writeH1PlusMetricsStats(b, h1PlusProcessStats.Snapshot())
}

func writeH1PlusMetricsStats(b *strings.Builder, stats connect.H1PlusStatsSnapshot) {
	enabled := 0
	if connect.H1PlusAvailable() {
		enabled = 1
	}
	counter := func(name string, help string, value uint64) {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, value)
	}
	fmt.Fprintf(b, "# HELP urnet_h1plus_enabled 1 while H1+ upgrades are allowed (control key h1_plus, urnet-tools set h1-plus).\n# TYPE urnet_h1plus_enabled gauge\nurnet_h1plus_enabled %d\n", enabled)
	fmt.Fprintf(b, "# HELP urnet_h1_max_message_bytes Largest complete message read on any H1 transport (websocket or H1+); the read limit is 65535.\n# TYPE urnet_h1_max_message_bytes gauge\nurnet_h1_max_message_bytes %d\n", connect.H1MaxMessageSeen())
	counter("urnet_h1plus_attempts_total", "H1+ upgrade attempts.", stats.Attempts)
	counter("urnet_h1plus_accepted_total", "H1+ upgrades the server accepted: connections that run on the custom carrier.", stats.Accepted)
	counter("urnet_h1plus_websocket_selected_total", "H1 dials that ended on a plain websocket (a fallback, or H1+ off).", stats.WebSocketSelected)
	fmt.Fprintf(b, "# HELP urnet_h1plus_fallbacks_total H1+ upgrades that fell back to a websocket, by reason.\n# TYPE urnet_h1plus_fallbacks_total counter\n")
	for reason, value := range map[string]uint64{"rejected": stats.FallbackRejected, "invalid": stats.FallbackInvalid, "io": stats.FallbackIO, "proxy": stats.FallbackProxy} {
		fmt.Fprintf(b, "urnet_h1plus_fallbacks_total{reason=%s} %d\n", connect.PrometheusLabelValue(reason), value)
	}
	fmt.Fprintf(b, "# HELP urnet_h1plus_handshake_failures_total H1+ upgrades that failed without falling back (TLS, authentication). Labeled series partition the total.\n# TYPE urnet_h1plus_handshake_failures_total counter\n")
	fmt.Fprintf(b, "urnet_h1plus_handshake_failures_total %d\n", stats.HandshakeFailures)
	for _, c := range []struct {
		class string
		value uint64
	}{
		{"timeout", stats.HandshakeFailuresTimeout},
		{"tls", stats.HandshakeFailuresTls},
		{"proxy_connect", stats.HandshakeFailuresProxyConnect},
		{"auth", stats.HandshakeFailuresAuth},
		{"upgrade_rejected", stats.HandshakeFailuresUpgradeRejected},
		{"io", stats.HandshakeFailuresIO},
		{"other", stats.HandshakeFailuresOther},
	} {
		fmt.Fprintf(b, "urnet_h1plus_handshake_failures_total{class=%s} %d\n", connect.PrometheusLabelValue(c.class), c.value)
	}
	counter("urnet_h1plus_auth_failures_total", "H1+ upgrades answered 401 or 403.", stats.AuthFailures)
	counter("urnet_h1plus_handshake_seconds_total", "Time spent in H1+ handshakes, seconds.", stats.HandshakeNanos/1_000_000_000)
	counter("urnet_h1plus_messages_total", "Messages carried on H1+ connections.", stats.Messages)
	counter("urnet_h1plus_bytes_total", "Message bytes carried on H1+ connections.", stats.Bytes)
	counter("urnet_h1plus_writes_total", "Physical writes on H1+ connections.", stats.Writes)
	counter("urnet_h1plus_flushes_total", "Batched flushes on H1+ connections.", stats.Flushes)
	counter("urnet_h1plus_read_errors_total", "Read errors on H1+ connections.", stats.ReadErrors)
	counter("urnet_h1plus_write_errors_total", "Write errors on H1+ connections.", stats.WriteErrors)
}

func formatH1PlusHandshakeClasses(now, last connect.H1PlusStatsSnapshot) string {
	failures := now.HandshakeFailures - last.HandshakeFailures
	if failures == 0 {
		return "handshake_classes(none)"
	}
	return fmt.Sprintf("handshake_classes(timeout=%d tls=%d proxy_connect=%d auth=%d upgrade_rejected=%d io=%d other=%d)",
		now.HandshakeFailuresTimeout-last.HandshakeFailuresTimeout,
		now.HandshakeFailuresTls-last.HandshakeFailuresTls,
		now.HandshakeFailuresProxyConnect-last.HandshakeFailuresProxyConnect,
		now.HandshakeFailuresAuth-last.HandshakeFailuresAuth,
		now.HandshakeFailuresUpgradeRejected-last.HandshakeFailuresUpgradeRejected,
		now.HandshakeFailuresIO-last.HandshakeFailuresIO,
		now.HandshakeFailuresOther-last.HandshakeFailuresOther,
	)
}

func formatH1PlusSummaryLine(now, last connect.H1PlusStatsSnapshot, maxMsgSeen int64, enabled bool) string {
	avg := time.Duration(0)
	if attempts := now.Attempts - last.Attempts; 0 < attempts {
		avg = time.Duration((now.HandshakeNanos-last.HandshakeNanos)/attempts) * time.Nanosecond
	}
	failures := now.HandshakeFailures - last.HandshakeFailures
	classes := formatH1PlusHandshakeClasses(now, last)
	return fmt.Sprintf("[h1plus] last 10m: attempts=%d accepted=%d websocket=%d fallbacks(rejected=%d invalid=%d io=%d proxy=%d) handshake_failures=%d %s auth_failures=%d avg_handshake=%s messages=%d bytes=%dMiB flushes=%d errors(read=%d write=%d) largest_message=%dB enabled=%t",
		now.Attempts-last.Attempts, now.Accepted-last.Accepted, now.WebSocketSelected-last.WebSocketSelected,
		now.FallbackRejected-last.FallbackRejected, now.FallbackInvalid-last.FallbackInvalid, now.FallbackIO-last.FallbackIO, now.FallbackProxy-last.FallbackProxy,
		failures, classes, now.AuthFailures-last.AuthFailures, avg.Round(time.Millisecond),
		now.Messages-last.Messages, (now.Bytes-last.Bytes)>>20, now.Flushes-last.Flushes,
		now.ReadErrors-last.ReadErrors, now.WriteErrors-last.WriteErrors, maxMsgSeen, enabled)
}

var h1PlusSummaryOnce sync.Once

// startH1PlusSummaryLog writes one line every ten minutes saying how the H1 dials
// of every identity ended, so the carrier in use shows in the log without a
// metrics scrape:
//
//	[h1plus] last 10m: attempts=330 accepted=328 websocket=2 fallbacks(rejected=2 invalid=0 io=0 proxy=0) handshake_failures=0 handshake_classes(none) auth_failures=0 avg_handshake=186ms messages=48210 bytes=61MiB flushes=9012 errors(read=3 write=1) largest_message=15999B enabled=true
func startH1PlusSummaryLog(ctx context.Context) {
	h1PlusSummaryOnce.Do(func() {
		go connect.HandleError(func() {
			last := h1PlusProcessStats.Snapshot()
			ticker := time.NewTicker(10 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				now := h1PlusProcessStats.Snapshot()
				tlog("%s\n", formatH1PlusSummaryLine(now, last, connect.H1MaxMessageSeen(), connect.H1PlusAvailable()))
				last = now
			}
		})
	})
}
