package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestResolveReportURL_FallsBackToEnvWhenNoOverrideFile is a regression test
// for existing deployments: with no ~/.urnetwork/report_url file present,
// resolveReportURL must keep honoring URNETWORK_REPORT_URL exactly like
// before, since that's how the reporter is configured today.
func TestResolveReportURL_FallsBackToEnvWhenNoOverrideFile(t *testing.T) {
	withTempHome(t)

	if got := resolveReportURL("http://example.com"); got != "http://example.com" {
		t.Fatalf("expected fallback to env value, got %q", got)
	}
}

// TestResolveReportURL_OverrideFileTakesPrecedence is the core of the
// feature this exists for: an operator dropping a URL into
// ~/.urnetwork/report_url must be able to turn on (or repoint) hub reporting
// for an already-running process, since systemd Environment= changes only
// take effect on the next process start.
func TestResolveReportURL_OverrideFileTakesPrecedence(t *testing.T) {
	home := withTempHome(t)

	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report_url"), []byte("http://hub.example.com\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if got := resolveReportURL("http://fallback.example.com"); got != "http://hub.example.com" {
		t.Fatalf("expected override file to take precedence, got %q", got)
	}
}

// TestResolveReportURL_EmptyOverrideFileFallsBackToEnv ensures an operator
// can't accidentally disable reporting by leaving a blank/whitespace-only
// override file lying around — only a non-empty value in the file counts as
// an override.
func TestResolveReportURL_EmptyOverrideFileFallsBackToEnv(t *testing.T) {
	home := withTempHome(t)

	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report_url"), []byte("  \n"), 0600); err != nil {
		t.Fatal(err)
	}

	if got := resolveReportURL("http://fallback.example.com"); got != "http://fallback.example.com" {
		t.Fatalf("expected blank override file to fall back to env, got %q", got)
	}
}

// TestResolveReportURL_NoOverrideAndNoEnvIsEmpty confirms reporting stays off
// by default when neither the override file nor the env var is set, matching
// pre-existing behavior where a missing URNETWORK_REPORT_URL disabled the
// reporter entirely.
func TestResolveReportURL_NoOverrideAndNoEnvIsEmpty(t *testing.T) {
	withTempHome(t)

	if got := resolveReportURL(""); got != "" {
		t.Fatalf("expected empty result with no override and no env fallback, got %q", got)
	}
}

// TestResolveAlertWebhook_OverrideFileTakesPrecedence mirrors
// TestResolveReportURL_OverrideFileTakesPrecedence for the outage watcher's
// webhook: an operator must be able to set/change/clear the alert webhook
// for an already-running provider without a restart.
func TestResolveAlertWebhook_OverrideFileTakesPrecedence(t *testing.T) {
	home := withTempHome(t)

	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "alert_webhook"), []byte("https://discord.com/api/webhooks/x\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if got := resolveAlertWebhook("https://fallback.example.com"); got != "https://discord.com/api/webhooks/x" {
		t.Fatalf("expected override file to take precedence, got %q", got)
	}
}

// TestResolveAlertWebhook_FallsBackToEnvWhenNoOverrideFile ensures existing
// deployments configured purely via URNETWORK_ALERT_WEBHOOK keep working
// unchanged.
func TestResolveAlertWebhook_FallsBackToEnvWhenNoOverrideFile(t *testing.T) {
	withTempHome(t)

	if got := resolveAlertWebhook("https://fallback.example.com"); got != "https://fallback.example.com" {
		t.Fatalf("expected fallback to env value, got %q", got)
	}
}

// TestBuildHeartbeat_NoProxiesConfigured is the deterministic case for
// buildHeartbeat: with no proxies registered in the global bandwidth map,
// it must still return a well-formed report (node/host set, zeroed
// counters) rather than panicking or leaving fields uninitialized. This is
// the lightweight companion to buildReport's per-proxy detail — it must
// carry the same NodeID/Uptime so the hub can match it to an existing node.
func TestBuildHeartbeat_NoProxiesConfigured(t *testing.T) {
	start := time.Now().Add(-5 * time.Minute)

	hb := buildHeartbeat("test-node", "test-host", start)

	if hb.NodeID != "test-node" {
		t.Errorf("NodeID = %q, want %q", hb.NodeID, "test-node")
	}
	if hb.TotalRX != 0 || hb.TotalTX != 0 {
		t.Errorf("TotalRX/TX = %d/%d, want 0/0 with no proxies configured", hb.TotalRX, hb.TotalTX)
	}
	if hb.Clients != 0 {
		t.Errorf("Clients = %d, want 0 with no proxies configured", hb.Clients)
	}
	if hb.Uptime <= 0 {
		t.Errorf("Uptime = %f, want > 0 for a start time 5m in the past", hb.Uptime)
	}
}

// TestBuildHeartbeat_ProjectsProxyStatus is the deterministic case for the
// new Proxies field: with no proxies registered in the global bandwidth
// map (same setup as TestBuildHeartbeat_NoProxiesConfigured — buildReport
// itself isn't independently testable since it reads global state), the
// projection must produce an empty slice rather than nil-panicking or
// carrying stale data.
func TestBuildHeartbeat_ProjectsProxyStatus(t *testing.T) {
	start := time.Now().Add(-1 * time.Minute)

	hb := buildHeartbeat("test-node", "test-host", start)

	if len(hb.Proxies) != 0 {
		t.Errorf("Proxies = %+v, want empty with no proxies configured", hb.Proxies)
	}
}

// TestNextHeartbeatInterval_NoFailuresUsesBase covers the steady-state case:
// with no consecutive failures, the heartbeat loop must tick at exactly the
// configured base interval.
func TestNextHeartbeatInterval_NoFailuresUsesBase(t *testing.T) {
	if got := nextHeartbeatInterval(15*time.Second, 0); got != 15*time.Second {
		t.Errorf("interval with 0 failures = %s, want 15s", got)
	}
}

// TestNextHeartbeatInterval_BacksOffOnConsecutiveFailures is the fix for a
// flaky-internet hub (e.g. Detroit): a fleet of 30 providers hammering a
// down hub every 15s with no backoff turns a transient outage into
// sustained connection churn. Each consecutive failure should double the
// wait, capped, so the fleet backs off during an extended outage instead of
// retry-storming it.
func TestNextHeartbeatInterval_BacksOffOnConsecutiveFailures(t *testing.T) {
	base := 15 * time.Second
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{1, 30 * time.Second},
		{2, 60 * time.Second},
		{3, 120 * time.Second},
		{4, 240 * time.Second},
		{5, 300 * time.Second}, // capped at 5m
		{50, 300 * time.Second},
	}
	for _, c := range cases {
		if got := nextHeartbeatInterval(base, c.failures); got != c.want {
			t.Errorf("interval with %d failures = %s, want %s", c.failures, got, c.want)
		}
	}
}

func TestFilterChangedProxies_UnchangedEntryExcluded(t *testing.T) {
	prev := map[string]proxyStatus{
		"p1": {ID: "p1", Status: "up", ContractsAcquired: 5, ContractsDenied: 1},
	}
	current := []proxyStatus{
		{ID: "p1", Status: "up", ContractsAcquired: 5, ContractsDenied: 1},
	}

	changed, next := filterChangedProxies(prev, current)

	if len(changed) != 0 {
		t.Errorf("changed = %+v, want empty for an unchanged proxy", changed)
	}
	if next["p1"] != current[0] {
		t.Errorf("next[p1] = %+v, want %+v", next["p1"], current[0])
	}
}

func TestFilterChangedProxies_StatusChangeIncluded(t *testing.T) {
	prev := map[string]proxyStatus{
		"p1": {ID: "p1", Status: "up"},
	}
	current := []proxyStatus{
		{ID: "p1", Status: "dead"},
	}

	changed, _ := filterChangedProxies(prev, current)

	if len(changed) != 1 || changed[0].Status != "dead" {
		t.Errorf("changed = %+v, want a single dead-status entry", changed)
	}
}

func TestFilterChangedProxies_ContractCounterChangeIncluded(t *testing.T) {
	prev := map[string]proxyStatus{
		"p1": {ID: "p1", Status: "up", ContractsAcquired: 5},
	}
	current := []proxyStatus{
		{ID: "p1", Status: "up", ContractsAcquired: 6},
	}

	changed, _ := filterChangedProxies(prev, current)

	if len(changed) != 1 || changed[0].ContractsAcquired != 6 {
		t.Errorf("changed = %+v, want a single entry with ContractsAcquired=6", changed)
	}
}

func TestFilterChangedProxies_UnknownEntryAlwaysIncluded(t *testing.T) {
	prev := map[string]proxyStatus{}
	current := []proxyStatus{
		{ID: "p1", Status: "up"},
		{ID: "p2", Status: "up"},
	}

	changed, next := filterChangedProxies(prev, current)

	if len(changed) != 2 {
		t.Errorf("changed = %+v, want both entries included on first sighting", changed)
	}
	if len(next) != 2 {
		t.Errorf("next = %+v, want both entries recorded for the following diff", next)
	}
}
