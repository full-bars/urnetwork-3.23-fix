package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSetWriteReadClear exercises the runtime-override file lifecycle that
// applySetOverride drives: write a value, read it back via formatSets, clear
// it, and reject an unknown key.
func TestSetWriteReadClear(t *testing.T) {
	dir := t.TempDir()
	p := Provider{StateDir: dir}

	// Write a value (node-name override file = node_name).
	if err := applySetOverride(p, "node-name", "edge01", false); err != nil {
		t.Fatalf("set value: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "node_name"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(b) != "edge01" {
		t.Fatalf("node_name = %q, want edge01", string(b))
	}

	// Clear it.
	if err := applySetOverride(p, "node-name", "off", false); err != nil {
		t.Fatalf("clear value: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_name")); !os.IsNotExist(err) {
		t.Fatalf("expected node_name to be removed, err=%v", err)
	}

	// Unknown keys are rejected, not silently absorbed.
	if err := applySetOverride(p, "not-a-key", "x", false); err == nil {
		t.Fatalf("expected an error for unknown key, got nil")
	}
}

// TestFastAuthMarker round-trips the auth-rate-limiter bypass marker.
func TestFastAuthMarker(t *testing.T) {
	dir := t.TempDir()
	p := Provider{StateDir: dir}
	file := filepath.Join(dir, "fast_auth")

	if err := setFastAuthMarker(p, true, false); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("fast_auth marker should exist after enable: %v", err)
	}

	// Dry-run off must NOT remove the marker (dry-run is a no-op).
	if err := setFastAuthMarker(p, false, true); err != nil {
		t.Fatalf("dry-run disable: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("dry-run must not remove marker: %v", err)
	}

	if err := setFastAuthMarker(p, false, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("marker should be gone after disable, err=%v", err)
	}
}

// TestFormatSetsEmpty lists a provider with no overrides.
func TestFormatSetsEmpty(t *testing.T) {
	p := Provider{StateDir: t.TempDir()}
	if err := formatSets(p, ""); err != nil {
		t.Fatalf("formatSets(empty): %v", err)
	}
}

// TestSetKeyMapping ensures every low-level key maps to the provider filename
// the runtime reads (guards against drift in the setKeyFiles table).
func TestSetKeyMapping(t *testing.T) {
	want := map[string]string{
		"node-name":         "node_name",
		"report-interval":   "report_interval",
		"proxy-url-max":     "proxy_url_max",
		"proxy-url-refresh": "proxy_url_refresh",
		"cleanup-scope":     "proxy_dead_cleanup_scope",
		"cleanup-interval":  "proxy_dead_cleanup_interval",
		"fast-auth":         "fast_auth",
	}
	for k, f := range want {
		if setKeyFiles[k] != f {
			t.Errorf("setKeyFiles[%q] = %q, want %q", k, setKeyFiles[k], f)
		}
	}
}

// TestRestoredHelpRouting verifies the newly wired subcommands print help and
// return nil (help-never-executes invariant) without needing a live provider.
func TestRestoredHelpRouting(t *testing.T) {
	for _, args := range [][]string{
		{"set", "--help"},
		{"set", "help"},
		{"auth", "--help"},
		{"choose-network", "--help"},
		{"fast-auth", "--help"},
	} {
		if err := Run(args); err != nil {
			t.Errorf("Run(%v) = %v, want nil (help)", args, err)
		}
	}
}

// TestCmdFastAuthTargetFlagBeforeAction guards the review finding that
// `fast-auth --unit X on|off` must recognize the action even when a target
// flag precedes it, instead of erroring misleadingly.
func TestCmdFastAuthTargetFlagBeforeAction(t *testing.T) {
	// No such unit exists, so selectTarget must fail — but NOT with the
	// "takes on|off|status only" ordering error.
	err := cmdFastAuth([]string{"--unit", "definitely-no-such-unit", "off"}, false, false)
	if err == nil {
		t.Fatal("expected an error (unit does not exist)")
	}
	if strings.Contains(err.Error(), "takes on|off|status only") {
		t.Fatalf("misleading ordering error for fast-auth: %v", err)
	}
}

// TestSetDryRunNoWrite verifies --dry-run performs no writes for a plain set
// key (write and clear branches are both no-ops).
func TestSetDryRunNoWrite(t *testing.T) {
	dir := t.TempDir()
	p := Provider{StateDir: dir}

	if err := applySetOverride(p, "node-name", "edge01", true); err != nil {
		t.Fatalf("dry-run set: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_name")); !os.IsNotExist(err) {
		t.Fatal("dry-run set must not create node_name")
	}

	if err := os.WriteFile(filepath.Join(dir, "node_name"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := applySetOverride(p, "node-name", "off", true); err != nil {
		t.Fatalf("dry-run clear: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_name")); err != nil {
		t.Fatalf("dry-run clear must not remove node_name: %v", err)
	}
}

// TestFastAuthInvalidRejected pins the CodeRabbit finding: an invalid
// fast-auth value must error, not silently enable the bypass, for both the
// standalone command and `set fast-auth <value>`.
func TestFastAuthInvalidRejected(t *testing.T) {
	dir := t.TempDir()
	p := Provider{StateDir: dir}
	if err := applySetOverride(p, "fast-auth", "bogus", false); err == nil {
		t.Fatal("set fast-auth bogus must be rejected")
	}
	if err := applySetOverride(p, "fast-auth", "onn", false); err == nil {
		t.Fatal("set fast-auth onn (typo) must be rejected, not enable")
	}
	if _, err := os.Stat(filepath.Join(dir, "fast_auth")); !os.IsNotExist(err) {
		t.Fatal("invalid fast-auth value must not create the bypass marker")
	}
}
