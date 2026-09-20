package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/promtext"
)

func TestControlSocketSnapshotEndToEnd(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	resp, err := dialControlSocket(controlRequest{Cmd: "snapshot"})
	if err != nil {
		t.Fatalf("dial snapshot: %v", err)
	}
	if !resp.OK || resp.Snapshot == nil {
		t.Fatalf("snapshot response: %+v", resp)
	}
	s := resp.Snapshot
	if s.V != 1 || s.Version == "" || s.Rate.HistoryBps == nil || s.Resources.Goroutines < 1 {
		t.Fatalf("snapshot incomplete: %+v", s)
	}
	switch s.State {
	case "starting", "degraded", "idle", "flowing":
	default:
		t.Fatalf("state = %q, not one of the contract states", s.State)
	}
	switch s.Restart.Reason {
	case "update", "hotswap", "manual", "clean", "unclean", "first-start":
	default:
		t.Fatalf("restart reason = %q, not one of the contract reasons", s.Restart.Reason)
	}
}

// Existing commands must not start carrying a snapshot.
func TestControlResponseSnapshotOnlyOnSnapshotCommand(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()
	for _, cmd := range []string{"version", "status"} {
		resp := handleControlRequest(globalControlState, controlRequest{Cmd: cmd})
		b, err := json.Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `"snapshot"`) {
			t.Errorf("%s response carries a snapshot: %s", cmd, b)
		}
	}
}

func TestWriteNodeGauges(t *testing.T) {
	var b strings.Builder
	writeNodeGauges(&b, "update", SnapshotResources{
		MemLimitBytes: 4 << 30, RSSBytes: 123, OpenFDs: 45, FDLimit: 1024,
	})
	out := b.String()
	for _, want := range []string{
		"# TYPE urnet_restart_reason gauge\n",
		`urnet_restart_reason{reason="update"} 1` + "\n",
		"urnet_mem_limit_bytes 4294967296\n",
		"urnet_rss_bytes 123\n",
		"urnet_open_fds 45\n",
		"urnet_fd_limit 1024\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	if errs := promtext.Lint(out); len(errs) != 0 {
		t.Errorf("lint: %v", errs)
	}
}

func TestWriteNodeGaugesOmitsUnknown(t *testing.T) {
	var b strings.Builder
	writeNodeGauges(&b, "", SnapshotResources{HeapInuseBytes: 1, Goroutines: 1})
	if out := b.String(); out != "" {
		t.Errorf("expected no gauges for unknown values, got:\n%s", out)
	}
}

// The gauges join an existing family list: none of the old names may move and
// urnet_uptime_seconds (already exported by connect) must stay a single family.
func TestProviderMetricsIncludeNodeGaugesAndKeepExisting(t *testing.T) {
	withTempHome(t)
	connect.SetExtraMetricsProvider(providerExtraMetrics)
	t.Cleanup(func() { connect.SetExtraMetricsProvider(nil) })

	w := httptest.NewRecorder()
	connect.PrometheusHandler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()

	for _, name := range []string{
		"urnet_info", "urnet_startup_clean_shutdown", "urnet_startup_restarted",
		"urnet_startup_upgraded", "urnet_control_commands_total", "urnet_sessions_pqe",
		"urnet_sessions_classical", "urnet_pressure_score", "urnet_doh_failures_total",
		"urnet_uptime_seconds", "urnet_restart_reason",
	} {
		if !strings.Contains(body, "# TYPE "+name+" ") {
			t.Errorf("family %s missing from scrape", name)
		}
	}
	if n := strings.Count(body, "# TYPE urnet_uptime_seconds "); n != 1 {
		t.Errorf("urnet_uptime_seconds declared %d times, want 1", n)
	}
	for _, e := range promtext.Lint(body) {
		t.Error(e)
	}
}
