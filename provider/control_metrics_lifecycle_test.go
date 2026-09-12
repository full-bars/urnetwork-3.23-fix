package main

import (
	"os"
	"strings"
	"testing"
)

// `clear metrics` did nothing. metrics is in liveEffectKeys, so the handler
// calls applyLiveDefault, but liveDefaults has no metrics entry, so that
// returns nil without touching the listener. The Prometheus endpoint kept
// serving while the CLI reported success and no restart needed.
//
// That is the control an operator reaches for when scraping goes wrong, so
// it silently not working is worse than it not existing.
func TestClearMetricsStopsTheListener(t *testing.T) {
	if _, ok := liveDefaults["metrics"]; !ok {
		t.Fatal("liveDefaults has no metrics entry, so clearing it cannot stop the listener")
	}
	if got := liveDefaults["metrics"]; !strings.EqualFold(got, "off") {
		t.Errorf("liveDefaults[metrics] = %q, want \"off\"", got)
	}
}

// A clear whose live apply failed reported OK:true with the error tucked
// into a field most callers never read, so the CLI printed success.
func TestClearReportsFailureAsFailure(t *testing.T) {
	b, err := readFileString("control_socket.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b, "OK: true, Error:") {
		t.Error("a clear path returns OK:true alongside an Error; callers checking OK see success")
	}
}

func readFileString(name string) (string, error) {
	b, err := os.ReadFile(name)
	return string(b), err
}
