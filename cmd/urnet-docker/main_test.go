package main

import "testing"

// TestVersionDefault: Version must default to "dev" when the binary is not
// built via release.yml's ldflags (-X main.Version=...). The self-update
// path and release tooling depend on this var existing and being a plain
// string, so a build without the ldflag override must still produce a
// sane, non-empty value rather than a zero value.
func TestVersionDefault(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must not be empty")
	}
	if Version != "dev" {
		t.Errorf("Version = %q, want %q (unless overridden by -ldflags, which this test run did not do)", Version, "dev")
	}
}
