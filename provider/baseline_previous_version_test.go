package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The recorder writes its start mark synchronously during startup, and
// `baseline compare` finds the upgrade boundary from the previous_version on
// that row. detectStartup is what fills the value in and it is otherwise lazy,
// so previousVersionForBaseline has to run it: with nothing else having run
// first (the real startup order) the row must still carry the version the
// provider replaced. Earlier tests passed a hand-written version straight to
// baselineStart and never went through this path.
func TestPreviousVersionForBaselineReadsTheReplacedVersionAtStartup(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".provider_version"), []byte("v3.23.0-fix.32.7\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	savedDiag, savedVersion := startupDiag, Version
	startupDiag = &startupDiagnostics{}
	Version = "v3.23.0-fix.32.8"
	defer func() { startupDiag, Version = savedDiag, savedVersion }()

	if got := previousVersionForBaseline(); got != "v3.23.0-fix.32.7" {
		t.Fatalf("previous version = %q, want the version recorded before this start", got)
	}
	// Idempotent: a later metrics scrape or snapshot restart() must see the
	// same value, and the version file now names the running build.
	if got := previousVersionForBaseline(); got != "v3.23.0-fix.32.7" {
		t.Fatalf("second read = %q, want the same value", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".provider_version"))
	if err != nil || string(data) != "v3.23.0-fix.32.8" {
		t.Fatalf("version file = %q, %v; want the running version written", data, err)
	}
}
