package main

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/promtext"
)

// TestWritePQESessionMetricsStagesAndCurves pins the handshake-stage and
// negotiated-curve families: the counters must name every stage, a curve row
// must carry the escaped curve label, and the whole exposition must survive the
// text-format linter (a gauge named _total or a Go-only escape fails a scrape).
func TestWritePQESessionMetricsStagesAndCurves(t *testing.T) {
	var b strings.Builder
	writePQESessionMetrics(&b, connect.PQECounts{
		ActivePQE: 3, ActiveClas: 0,
		PQELifetime: 10, ClasLifetime: 0,
		Attempts: 12, Established: 11, Verified: 10, IdentityFailed: 1,
		Curves: []connect.CurveCount{
			{Curve: "X25519MLKEM768", Count: 11},
			{Curve: "X25519", Count: 0},
		},
	})
	out := b.String()

	for _, want := range []string{
		`urnet_sessions_handshake_total{stage="attempts"} 12`,
		`urnet_sessions_handshake_total{stage="tls_ok"} 11`,
		`urnet_sessions_handshake_total{stage="verified"} 10`,
		`urnet_sessions_handshake_total{stage="identity_failed"} 1`,
		`urnet_sessions_curve_total{curve="X25519MLKEM768"} 11`,
		`urnet_sessions_curve_total{curve="X25519"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in exposition:\n%s", want, out)
		}
	}

	// The histogram is lifetime data, so it must be a counter family.
	if !strings.Contains(out, "# TYPE urnet_sessions_curve_total counter") {
		t.Fatal("urnet_sessions_curve_total must be declared as a counter")
	}

	for _, err := range promtext.Lint(out) {
		t.Error(err)
	}
}

// TestWritePQESessionMetricsOmitsEmptyCurves keeps a fresh provider from
// advertising an empty curve family, which would otherwise look like a scrape
// gap rather than "no handshakes yet".
func TestWritePQESessionMetricsOmitsEmptyCurves(t *testing.T) {
	var b strings.Builder
	writePQESessionMetrics(&b, connect.PQECounts{})
	if strings.Contains(b.String(), "urnet_sessions_curve_total") {
		t.Fatalf("empty tracker must not emit the curve family:\n%s", b.String())
	}
}
