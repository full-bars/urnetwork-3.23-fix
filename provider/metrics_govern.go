package main

import (
	"fmt"
	"strings"

	"github.com/urnetwork/connect"
)

// governHealthLine is the per-heartbeat governor line next to
// [health][proxies]: whether the backend reads as degraded, what the shared
// connect budget is doing about it, and how many connects died young.
func governHealthLine(s connect.GovernorStatus) string {
	ss := connect.SpeedEchoStatsNow()
	return fmt.Sprintf(
		"❤️ [health][govern] mode=%s verdict=%s rate=%.1f/s waiting=%d attempts=%d connected=%d died_young=%d would_hold=%d flap_backoffs=%d speedtests=%d echoed_MB=%d",
		s.Mode, s.Verdict, s.BudgetRate, s.Waiting, s.Attempts, s.Connected, s.DiedYoung, s.WouldHold, s.FlapBackoffs,
		ss.Tests, ss.EchoedBytes>>20,
	)
}

// writeGovernMetrics exposes the reconnect governor. The window gauges cover the
// governor's last 60 seconds.
func writeGovernMetrics(b *strings.Builder) {
	s := connect.GovernorStatusNow()
	mode := map[string]int{"off": 0, "shadow": 1, "enforce": 2}[s.Mode]
	degraded := 0
	if s.Degraded {
		degraded = 1
	}
	gauge := func(name, help string, value any) {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n%s %v\n", name, help, name, name, value)
	}
	gauge("urnet_govern_mode", "Reconnect governor mode: 0 off, 1 shadow (observe only), 2 enforce.", mode)
	gauge("urnet_govern_degraded", "1 when the governor reads the platform backend as degraded.", degraded)
	gauge("urnet_govern_budget_rate", "Platform connect attempts per second the budget admits (200 is open).", fmt.Sprintf("%.1f", s.BudgetRate))
	gauge("urnet_govern_waiting", "Proxies waiting their turn under the connect budget.", s.Waiting)
	gauge("urnet_govern_attempts", "Platform connect attempts in the last 60s.", s.Attempts)
	gauge("urnet_govern_died_young", "Connects in the last 60s whose session died within a minute.", s.DiedYoung)
	xs := connect.TransferSendStatsNow()
	fmt.Fprintf(b, "# HELP urnet_transfer_send_frames_total Transfer-layer frames written, split into first sends and resends.\n# TYPE urnet_transfer_send_frames_total counter\nurnet_transfer_send_frames_total{kind=\"first\"} %d\nurnet_transfer_send_frames_total{kind=\"resend\"} %d\n", xs.FirstFrames, xs.ResendFrames)
	fmt.Fprintf(b, "# HELP urnet_transfer_send_bytes_total Transfer-layer frame bytes written, split into first sends and resends.\n# TYPE urnet_transfer_send_bytes_total counter\nurnet_transfer_send_bytes_total{kind=\"first\"} %d\nurnet_transfer_send_bytes_total{kind=\"resend\"} %d\n", xs.FirstBytes, xs.ResendBytes)
	fmt.Fprintf(b, "# HELP urnet_transfer_resend_deferred_total Resends the retry budget held back instead of sending.\n# TYPE urnet_transfer_resend_deferred_total counter\nurnet_transfer_resend_deferred_total{unit=\"frames\"} %d\nurnet_transfer_resend_deferred_total{unit=\"bytes\"} %d\n", xs.ResendDeferredFrames, xs.ResendDeferredBytes)
	fmt.Fprintf(b, "# HELP urnet_transfer_resend_would_defer_total Resends shadow mode sent anyway that enforce would have held back.\n# TYPE urnet_transfer_resend_would_defer_total counter\nurnet_transfer_resend_would_defer_total{unit=\"frames\"} %d\nurnet_transfer_resend_would_defer_total{unit=\"bytes\"} %d\n", xs.ResendWouldDeferFrames, xs.ResendWouldDeferBytes)
	fmt.Fprintf(b, "# HELP urnet_transfer_budget_earned_bytes_total Acknowledged bytes credited to the resend budget.\n# TYPE urnet_transfer_budget_earned_bytes_total counter\nurnet_transfer_budget_earned_bytes_total %d\n", xs.BudgetEarnedBytes)
	fmt.Fprintf(b, "# HELP urnet_transfer_budget_tokens Resend budget tokens (bytes) left right now.\n# TYPE urnet_transfer_budget_tokens gauge\nurnet_transfer_budget_tokens %.0f\n", connect.TransferRetryBudgetTokens())
	dc := connect.DeadClientStatsNow()
	fmt.Fprintf(b, "# HELP urnet_dead_client_released_total Clients released after acknowledging nothing for the silence window, with what was cancelled for them.\n# TYPE urnet_dead_client_released_total counter\nurnet_dead_client_released_total{unit=\"clients\"} %d\nurnet_dead_client_released_total{unit=\"nat_flows\"} %d\nurnet_dead_client_released_total{unit=\"send_sequences\"} %d\n", dc.Released, dc.ReleasedFlows, dc.ReleasedSequences)
	fmt.Fprintf(b, "# HELP urnet_dead_client_would_release_total Silences shadow mode would have released, by why release was allowed.\n# TYPE urnet_dead_client_would_release_total counter\nurnet_dead_client_would_release_total %d\n", dc.WouldRelease)
	fmt.Fprintf(b, "# HELP urnet_dead_client_reason_total Releases and would-be releases by reason.\n# TYPE urnet_dead_client_reason_total counter\nurnet_dead_client_reason_total{reason=\"healthy_fleet\"} %d\nurnet_dead_client_reason_total{reason=\"hard_silence\"} %d\nurnet_dead_client_reason_total{reason=\"small_fleet\"} %d\n", dc.ByReasonHealthy, dc.ByReasonHard, dc.ByReasonSmall)
	fmt.Fprintf(b, "# HELP urnet_dead_client_silent Clients still being written to that were past the silence window at the latest sweep.\n# TYPE urnet_dead_client_silent gauge\nurnet_dead_client_silent %d\n", dc.Silent)
	fmt.Fprintf(b, "# HELP urnet_dead_client_fleet_collapsed 1 when the latest sweep judged the platform, not the clients, at fault and held releases.\n# TYPE urnet_dead_client_fleet_collapsed gauge\nurnet_dead_client_fleet_collapsed %d\n", dc.FleetCollapsed)
	fmt.Fprintf(b, "# HELP urnet_dead_client_fleet Destinations judged by the latest sweep: active (written to or acknowledged recently) and acking.\n# TYPE urnet_dead_client_fleet gauge\nurnet_dead_client_fleet{state=\"active\"} %d\nurnet_dead_client_fleet{state=\"acking\"} %d\nurnet_dead_client_fleet{state=\"records\"} %d\n", dc.Active, dc.Acking, dc.Records)
	bounds, cumulative := connect.RecoveredSilenceHistogram()
	fmt.Fprintf(b, "# HELP urnet_dead_client_recovered_silence_seconds Silences that ended in an acknowledgement, the data the release window should be chosen from.\n# TYPE urnet_dead_client_recovered_silence_seconds histogram\n")
	for i, bound := range bounds {
		fmt.Fprintf(b, "urnet_dead_client_recovered_silence_seconds_bucket{le=\"%g\"} %d\n", bound, cumulative[i])
	}
	fmt.Fprintf(b, "urnet_dead_client_recovered_silence_seconds_bucket{le=\"+Inf\"} %d\nurnet_dead_client_recovered_silence_seconds_count %d\n", cumulative[len(cumulative)-1], cumulative[len(cumulative)-1])
	mx := connect.TransferMoreStatsNow()
	fmt.Fprintf(b, "# HELP urnet_transfer_acked_bytes_total Frame bytes the far end acknowledged. Divide its rate by the send rates for the ack yield.\n# TYPE urnet_transfer_acked_bytes_total counter\nurnet_transfer_acked_bytes_total %d\n", xs.BudgetEarnedBytes)
	fmt.Fprintf(b, "# HELP urnet_transfer_ack_sent_total Acknowledgement frames this node wrote to peers.\n# TYPE urnet_transfer_ack_sent_total counter\nurnet_transfer_ack_sent_total{unit=\"frames\"} %d\nurnet_transfer_ack_sent_total{unit=\"bytes\"} %d\n", mx.AckFrames, mx.AckBytes)
	fmt.Fprintf(b, "# HELP urnet_transfer_destination_sent_total Frames that reached a carrier, first sends and resends, by destination class.\n# TYPE urnet_transfer_destination_sent_total counter\nurnet_transfer_destination_sent_total{dest=\"client\",unit=\"frames\"} %d\nurnet_transfer_destination_sent_total{dest=\"client\",unit=\"bytes\"} %d\nurnet_transfer_destination_sent_total{dest=\"control\",unit=\"frames\"} %d\nurnet_transfer_destination_sent_total{dest=\"control\",unit=\"bytes\"} %d\n", mx.ClientSentFrames, mx.ClientSentBytes, mx.ControlSentFrames, mx.ControlSentBytes)
	fmt.Fprintf(b, "# HELP urnet_transfer_sequence_exits_total Send sequences that ended, by why.\n# TYPE urnet_transfer_sequence_exits_total counter\n")
	for _, reason := range connect.SendSequenceExitReasons() {
		fmt.Fprintf(b, "urnet_transfer_sequence_exits_total{reason=\"%s\"} %d\n", reason, mx.SequenceExits[reason])
	}
	fmt.Fprintf(b, "# HELP urnet_transfer_retained_bytes Undelivered provider return bytes held for retry right now across all send sequences.\n# TYPE urnet_transfer_retained_bytes gauge\nurnet_transfer_retained_bytes %d\n", mx.RetainedBytes)
	fmt.Fprintf(b, "# HELP urnet_dead_client_released_held_bytes_total Undelivered bytes held for clients at the moment they were released, or in shadow would have been.\n# TYPE urnet_dead_client_released_held_bytes_total counter\nurnet_dead_client_released_held_bytes_total %d\n", mx.ReleasedHeldBytes)
	for _, shadow := range []bool{false, true} {
		mode := "enforce"
		if shadow {
			mode = "shadow"
		}
		rb, rc := connect.ReturnAfterReleaseHistogram(shadow)
		if !shadow {
			fmt.Fprintf(b, "# HELP urnet_dead_client_return_after_release_seconds Released clients that acknowledged again and how long they took. Every count is a release that was wrong.\n# TYPE urnet_dead_client_return_after_release_seconds histogram\n")
		}
		for i, bound := range rb {
			fmt.Fprintf(b, "urnet_dead_client_return_after_release_seconds_bucket{mode=\"%s\",le=\"%g\"} %d\n", mode, bound, rc[i])
		}
		fmt.Fprintf(b, "urnet_dead_client_return_after_release_seconds_bucket{mode=\"%s\",le=\"+Inf\"} %d\nurnet_dead_client_return_after_release_seconds_count{mode=\"%s\"} %d\n", mode, rc[len(rc)-1], mode, rc[len(rc)-1])
	}
	sb := connect.SocketBytesNow()
	fmt.Fprintf(b, "# HELP urnet_socket_bytes_total Bytes on the proxy sockets by what the connection was doing: handshake (a connect attempt that had not finished, failed ones included), session (the same connection after it connected) and other (connections made outside a connect attempt).\n# TYPE urnet_socket_bytes_total counter\n")
	for _, phase := range connect.SocketPhaseNames() {
		fmt.Fprintf(b, "urnet_socket_bytes_total{phase=\"%s\",dir=\"rx\"} %d\nurnet_socket_bytes_total{phase=\"%s\",dir=\"tx\"} %d\n", phase, sb.Rx[phase], phase, sb.Tx[phase])
	}
	fmt.Fprintf(b, "# HELP urnet_connect_attempt_bytes_total Bytes spent by finished platform connect attempts, by outcome.\n# TYPE urnet_connect_attempt_bytes_total counter\n")
	outcomes := connect.ConnectAttemptOutcomesNow()
	for _, outcome := range outcomes {
		fmt.Fprintf(b, "urnet_connect_attempt_bytes_total{outcome=\"%s\",dir=\"rx\"} %d\nurnet_connect_attempt_bytes_total{outcome=\"%s\",dir=\"tx\"} %d\n", outcome.Outcome, outcome.RxBytes, outcome.Outcome, outcome.TxBytes)
	}
	fmt.Fprintf(b, "# HELP urnet_connect_attempts_total Finished platform connect attempts, by outcome.\n# TYPE urnet_connect_attempts_total counter\n")
	for _, outcome := range outcomes {
		fmt.Fprintf(b, "urnet_connect_attempts_total{outcome=\"%s\"} %d\n", outcome.Outcome, outcome.Attempts)
	}
	pingTx, pingRx := connect.TransportPingFramesNow()
	fmt.Fprintf(b, "# HELP urnet_transport_ping_frames_total Keepalive frames on the platform transports.\n# TYPE urnet_transport_ping_frames_total counter\nurnet_transport_ping_frames_total{dir=\"tx\"} %d\nurnet_transport_ping_frames_total{dir=\"rx\"} %d\n", pingTx, pingRx)
	se := connect.SessionEndStatsNow()
	fmt.Fprintf(b, "# HELP urnet_transport_session_lifetime_seconds Platform transport sessions that ended, by proxy source, how long they lived and why. Every session is a new connection and costs about 1.75 MB of platform speed tests.\n# TYPE urnet_transport_session_lifetime_seconds histogram\n")
	for _, source := range connect.ProxySourceNames() {
		for _, reason := range connect.SessionEndReasons() {
			cumulative := se.Cumulative[source][reason]
			if cumulative[len(cumulative)-1] == 0 {
				continue
			}
			for i, bound := range se.BoundsSeconds {
				fmt.Fprintf(b, "urnet_transport_session_lifetime_seconds_bucket{source=\"%s\",reason=\"%s\",le=\"%g\"} %d\n", source, reason, bound, cumulative[i])
			}
			fmt.Fprintf(b, "urnet_transport_session_lifetime_seconds_bucket{source=\"%s\",reason=\"%s\",le=\"+Inf\"} %d\nurnet_transport_session_lifetime_seconds_count{source=\"%s\",reason=\"%s\"} %d\n", source, reason, cumulative[len(cumulative)-1], source, reason, cumulative[len(cumulative)-1])
		}
	}
	bursts, largestBurst := connect.RemoteCloseBurstsNow()
	fmt.Fprintf(b, "# HELP urnet_transport_mass_disconnects_total Bursts of 50 or more transports closed by the platform or network within 10 seconds, the signature of a platform rollout cycling connections.\n# TYPE urnet_transport_mass_disconnects_total counter\nurnet_transport_mass_disconnects_total %d\n# HELP urnet_transport_mass_disconnect_largest The most transports closed within one 10 second window.\n# TYPE urnet_transport_mass_disconnect_largest gauge\nurnet_transport_mass_disconnect_largest %d\n", bursts, largestBurst)
	fmt.Fprintf(b, "# HELP urnet_transport_close_cause_total Platform transport read and write errors by cause: a websocket close code from the platform, a timeout, a reset, an EOF, or our own teardown.\n# TYPE urnet_transport_close_cause_total counter\n")
	for _, cause := range connect.TransportCloseCausesNow() {
		if cause.Count == 0 {
			continue
		}
		fmt.Fprintf(b, "urnet_transport_close_cause_total{source=\"%s\",mode=\"%s\",side=\"%s\",cause=\"%s\"} %d\n", cause.Source, cause.Mode, cause.Side, cause.Cause, cause.Count)
	}
	fmt.Fprintf(b, "# HELP urnet_transfer_resend_depth_total Resends by how many times the item had already been sent.\n# TYPE urnet_transfer_resend_depth_total counter\n")
	for i, label := range []string{"2", "3-4", "5-8", "9+"} {
		fmt.Fprintf(b, "urnet_transfer_resend_depth_total{send=\"%s\"} %d\n", label, xs.ResendDepth[i])
	}
	ss := connect.SpeedEchoStatsNow()
	fmt.Fprintf(b, "# HELP urnet_speedtest_total Platform speed tests started on a transport (every connect gets about two).\n# TYPE urnet_speedtest_total counter\nurnet_speedtest_total %d\n", ss.Tests)
	fmt.Fprintf(b, "# HELP urnet_speedtest_echo_bytes_total Speed test bytes echoed back to the platform (the same again arrives inbound).\n# TYPE urnet_speedtest_echo_bytes_total counter\nurnet_speedtest_echo_bytes_total %d\n", ss.EchoedBytes)
	fmt.Fprintf(b, "# HELP urnet_govern_flap_backoffs_total Reconnects held back because the previous session died young.\n# TYPE urnet_govern_flap_backoffs_total counter\nurnet_govern_flap_backoffs_total %d\n", s.FlapBackoffs)
}

// writeTlsCancellationMetrics exposes why TLS handshake epochs were cancelled
// before they established. A coalesced failure line cannot say which transition
// cancelled an epoch; these counters can, and a canary reads the rate per
// source. Every cell is exported, zeros included, so a rate query has a series
// to grow into from the first scrape.
func writeTlsCancellationMetrics(b *strings.Builder) {
	fmt.Fprintf(b, "# HELP urnet_tls_handshake_cancel_total Handshake epochs cancelled before they established, by the transition that cancelled them.\n# TYPE urnet_tls_handshake_cancel_total counter\n")
	for _, cell := range connect.GetTlsHandshakeCancellationCounts() {
		fmt.Fprintf(
			b,
			"urnet_tls_handshake_cancel_total{source=%s,role=%s,companion=\"%t\"} %d\n",
			connect.PrometheusLabelValue(cell.Source),
			connect.PrometheusLabelValue(cell.Role),
			cell.Companion,
			cell.Count,
		)
	}
}
