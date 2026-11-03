package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/promtext"
)

func TestH1PlusMetricsExposition(t *testing.T) {
	snap := connect.H1PlusStatsSnapshot{
		Attempts:                         100,
		Accepted:                         90,
		HandshakeFailures:                10,
		HandshakeFailuresTimeout:         3,
		HandshakeFailuresTls:             2,
		HandshakeFailuresProxyConnect:    1,
		HandshakeFailuresAuth:            1,
		HandshakeFailuresUpgradeRejected: 1,
		HandshakeFailuresIO:              1,
		HandshakeFailuresOther:           1,
	}
	var b strings.Builder
	writeH1PlusMetricsStats(&b, snap)
	out := b.String()

	if !strings.Contains(out, "# HELP urnet_h1plus_handshake_failures_total H1+ upgrades that failed without falling back, all classes. The per-class breakdown is urnet_h1plus_handshake_failures_class_total; do not add the two together.\n") {
		t.Errorf("missing or incorrect HELP line for urnet_h1plus_handshake_failures_total")
	}
	if !strings.Contains(out, "# HELP urnet_h1plus_handshake_failures_class_total H1+ handshake failures by class; the classes sum to urnet_h1plus_handshake_failures_total, so use one family or the other, never both.\n") {
		t.Errorf("missing or incorrect HELP line for urnet_h1plus_handshake_failures_class_total")
	}
	if !strings.Contains(out, "# TYPE urnet_h1plus_handshake_failures_class_total counter\n") {
		t.Errorf("missing TYPE line for urnet_h1plus_handshake_failures_class_total")
	}
	if !strings.Contains(out, "# TYPE urnet_h1plus_handshake_failures_total counter\n") {
		t.Errorf("missing TYPE line for urnet_h1plus_handshake_failures_total")
	}
	if !strings.Contains(out, "\nurnet_h1plus_handshake_failures_total 10\n") {
		t.Errorf("missing unlabeled total line for urnet_h1plus_handshake_failures_total")
	}

	expectedClasses := []string{
		"timeout",
		"tls",
		"proxy_connect",
		"auth",
		"upgrade_rejected",
		"io",
		"other",
	}

	for _, class := range expectedClasses {
		expectedLine := fmt.Sprintf("urnet_h1plus_handshake_failures_class_total{class=%s}", connect.PrometheusLabelValue(class))
		if !strings.Contains(out, expectedLine) {
			t.Errorf("missing expected class series: %s", expectedLine)
		}
	}

	// Promtext linter check
	for _, err := range promtext.Lint(out) {
		t.Errorf("promtext lint error: %v", err)
	}
}

func TestH1PlusSummarySyntheticSnapshot(t *testing.T) {
	last := connect.H1PlusStatsSnapshot{
		Attempts:                         100,
		Accepted:                         50,
		HandshakeFailures:                10,
		HandshakeFailuresTimeout:         5,
		HandshakeFailuresTls:             2,
		HandshakeFailuresProxyConnect:    1,
		HandshakeFailuresAuth:            0,
		HandshakeFailuresUpgradeRejected: 1,
		HandshakeFailuresIO:              1,
		HandshakeFailuresOther:           0,
	}
	now := connect.H1PlusStatsSnapshot{
		Attempts:                         1029,
		Accepted:                         115,
		HandshakeFailures:                850,
		HandshakeFailuresTimeout:         815,
		HandshakeFailuresTls:             14,
		HandshakeFailuresProxyConnect:    10,
		HandshakeFailuresAuth:            0,
		HandshakeFailuresUpgradeRejected: 7,
		HandshakeFailuresIO:              4,
		HandshakeFailuresOther:           0,
	}

	diffTimeout := now.HandshakeFailuresTimeout - last.HandshakeFailuresTimeout
	diffTls := now.HandshakeFailuresTls - last.HandshakeFailuresTls
	diffProxy := now.HandshakeFailuresProxyConnect - last.HandshakeFailuresProxyConnect
	diffAuth := now.HandshakeFailuresAuth - last.HandshakeFailuresAuth
	diffUpgrade := now.HandshakeFailuresUpgradeRejected - last.HandshakeFailuresUpgradeRejected
	diffIO := now.HandshakeFailuresIO - last.HandshakeFailuresIO
	diffOther := now.HandshakeFailuresOther - last.HandshakeFailuresOther

	totalDiff := now.HandshakeFailures - last.HandshakeFailures
	classSum := diffTimeout + diffTls + diffProxy + diffAuth + diffUpgrade + diffIO + diffOther
	if classSum != totalDiff {
		t.Fatalf("class sum %d != total failure difference %d", classSum, totalDiff)
	}

	line := formatH1PlusSummaryLine(now, last, 15999, true)
	wantClasses := "handshake_classes(timeout=810 tls=12 proxy_connect=9 auth=0 upgrade_rejected=6 io=3 other=0)"
	if !strings.Contains(line, wantClasses) {
		t.Errorf("line does not contain expected classes string:\nwant: %s\ngot line: %s", wantClasses, line)
	}
	wantSegment := "handshake_failures=840 handshake_classes(timeout=810 tls=12 proxy_connect=9 auth=0 upgrade_rejected=6 io=3 other=0) auth_failures=0"
	if !strings.Contains(line, wantSegment) {
		t.Errorf("line does not preserve field ordering around handshake_classes:\nwant segment: %s\ngot line: %s", wantSegment, line)
	}
}

func TestH1PlusSummaryZeroFailures(t *testing.T) {
	last := connect.H1PlusStatsSnapshot{
		Attempts:                 100,
		Accepted:                 100,
		HandshakeFailures:        5,
		HandshakeFailuresTimeout: 5,
	}
	now := connect.H1PlusStatsSnapshot{
		Attempts:                 200,
		Accepted:                 200,
		HandshakeFailures:        5,
		HandshakeFailuresTimeout: 5,
	}

	line := formatH1PlusSummaryLine(now, last, 15999, true)
	if !strings.Contains(line, "handshake_failures=0 handshake_classes(none) auth_failures=0") {
		t.Errorf("expected zero failures to produce 'handshake_classes(none)', got line: %s", line)
	}

	classes := formatH1PlusHandshakeClasses(now, last)
	if classes != "handshake_classes(none)" {
		t.Errorf("expected 'handshake_classes(none)', got %q", classes)
	}
	for _, class := range []string{"timeout=", "tls=", "proxy_connect=", "auth=", "upgrade_rejected=", "io=", "other="} {
		if strings.Contains(classes, class) {
			t.Errorf("zero failures classes should not contain class field %q, got: %s", class, classes)
		}
	}
}

func TestH1PlusClassesCompleteness(t *testing.T) {
	// Assert all 7 classes are present in formatH1PlusHandshakeClasses and none can be omitted.
	now := connect.H1PlusStatsSnapshot{
		HandshakeFailures:                7,
		HandshakeFailuresTimeout:         1,
		HandshakeFailuresTls:             1,
		HandshakeFailuresProxyConnect:    1,
		HandshakeFailuresAuth:            1,
		HandshakeFailuresUpgradeRejected: 1,
		HandshakeFailuresIO:              1,
		HandshakeFailuresOther:           1,
	}
	last := connect.H1PlusStatsSnapshot{}

	rendered := formatH1PlusHandshakeClasses(now, last)
	expectedClasses := []string{
		"timeout=1",
		"tls=1",
		"proxy_connect=1",
		"auth=1",
		"upgrade_rejected=1",
		"io=1",
		"other=1",
	}

	if !strings.HasPrefix(rendered, "handshake_classes(") || !strings.HasSuffix(rendered, ")") {
		t.Fatalf("malformed rendered classes: %s", rendered)
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(rendered, "handshake_classes("), ")")
	fields := strings.Fields(inner)
	if len(fields) != len(expectedClasses) {
		t.Fatalf("expected exactly %d class fields, got %d in %q", len(expectedClasses), len(fields), rendered)
	}
	for i, want := range expectedClasses {
		if fields[i] != want {
			t.Errorf("field %d: expected %q, got %q", i, want, fields[i])
		}
	}

	var b strings.Builder
	writeH1PlusMetricsStats(&b, now)
	out := b.String()

	labeledCount := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "urnet_h1plus_handshake_failures_class_total{class=") {
			labeledCount++
		}
	}
	if labeledCount != 7 {
		t.Errorf("expected exactly 7 labeled handshake failure series, got %d", labeledCount)
	}
}

// sumSamples adds every sample whose metric name (the text before any label
// set or space) is exactly name, the way a bare sum(name) would in PromQL.
func sumSamples(t *testing.T, out string, name string) (sum uint64, samples int) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		end := strings.IndexAny(line, "{ ")
		if end < 0 || line[:end] != name {
			continue
		}
		var v uint64
		if _, err := fmt.Sscanf(line[strings.LastIndex(line, " ")+1:], "%d", &v); err != nil {
			t.Fatalf("unparseable sample %q: %v", line, err)
		}
		sum += v
		samples++
	}
	return
}

// A bare sum over the total's name must return the real count. When the
// per-class children shared that name, the sum returned twice the count.
func TestH1PlusFailureTotalDoesNotCollideWithClasses(t *testing.T) {
	snap := connect.H1PlusStatsSnapshot{
		HandshakeFailures:                10,
		HandshakeFailuresTimeout:         3,
		HandshakeFailuresTls:             2,
		HandshakeFailuresProxyConnect:    1,
		HandshakeFailuresAuth:            1,
		HandshakeFailuresUpgradeRejected: 1,
		HandshakeFailuresIO:              1,
		HandshakeFailuresOther:           1,
	}
	var b strings.Builder
	writeH1PlusMetricsStats(&b, snap)
	out := b.String()

	total, samples := sumSamples(t, out, "urnet_h1plus_handshake_failures_total")
	if samples != 1 || total != 10 {
		t.Errorf("sum over urnet_h1plus_handshake_failures_total = %d across %d samples; want 10 across 1", total, samples)
	}
	classTotal, classSamples := sumSamples(t, out, "urnet_h1plus_handshake_failures_class_total")
	if classSamples != 7 || classTotal != 10 {
		t.Errorf("sum over urnet_h1plus_handshake_failures_class_total = %d across %d samples; want 10 across 7", classTotal, classSamples)
	}
	if strings.Contains(out, "urnet_h1plus_handshake_failures_total{") {
		t.Errorf("labeled series under the total's name: the family would double under sum()")
	}
}
