package main

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

func TestGovernHealthLine_CarriesTheOperatorFacts(t *testing.T) {
	line := governHealthLine(connect.GovernorStatus{
		Mode: "enforce", Verdict: "degraded", BudgetRate: 8, Waiting: 3100,
		Attempts: 138, Connected: 45, DiedYoung: 42, WouldHold: 18, FlapBackoffs: 77,
	})
	for _, want := range []string{
		"[health][govern]", "mode=enforce", "verdict=degraded", "rate=8.0/s",
		"waiting=3100", "attempts=138", "connected=45", "died_young=42", "flap_backoffs=77",
		"speedtests=", "echoed_MB=",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
}

func TestProviderMetrics_ExposeTheGovernor(t *testing.T) {
	body := providerExtraMetrics()
	for _, want := range []string{
		"urnet_govern_degraded", "urnet_govern_budget_rate", "urnet_govern_waiting",
		"urnet_govern_attempts", "urnet_govern_died_young", "urnet_govern_flap_backoffs_total",
		"urnet_govern_mode",
		"urnet_transfer_send_frames_total", "urnet_transfer_resend_deferred_total",
		"urnet_speedtest_total", "urnet_speedtest_echo_bytes_total",
		// ack yield and the new transfer measurements
		"urnet_transfer_acked_bytes_total", "urnet_transfer_ack_sent_total",
		"urnet_transfer_destination_sent_total", "urnet_transfer_sequence_exits_total",
		"urnet_transfer_retained_bytes", "urnet_transfer_resend_would_defer_total",
		"urnet_transfer_budget_tokens",
		// dead-client release
		"urnet_dead_client_released_total", "urnet_dead_client_would_release_total",
		"urnet_dead_client_reason_total", "urnet_dead_client_silent",
		"urnet_dead_client_fleet_collapsed", "urnet_dead_client_fleet",
		"urnet_dead_client_recovered_silence_seconds", "urnet_dead_client_return_after_release_seconds",
		"urnet_dead_client_released_held_bytes_total",
		// where the proxy socket bytes go
		"urnet_socket_bytes_total", "urnet_connect_attempt_bytes_total",
		"urnet_connect_attempts_total", "urnet_transport_ping_frames_total",
		"urnet_transport_session_lifetime_seconds", "urnet_transport_mass_disconnects_total", "urnet_transport_close_cause_total",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics are missing %s", want)
		}
	}
}

func TestProviderMetricsBillablePackets(t *testing.T) {
	bw := connect.RegisterProxyBandwidth(712345)
	defer connect.UnregisterProxy(712345)
	bw.BillableRxPackets.Store(31)
	bw.BillableTxPackets.Store(47)
	body := providerExtraMetrics()
	for _, want := range []string{"# HELP urnet_billable_packets_total ", "# TYPE urnet_billable_packets_total counter", `urnet_billable_packets_total{dir="rx"} 31`, `urnet_billable_packets_total{dir="tx"} 47`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
}
