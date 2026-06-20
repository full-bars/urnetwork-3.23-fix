package main

import (
	"os"
	"path/filepath"
	"testing"
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
