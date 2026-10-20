package main

import "testing"

func TestAuthAppVersion(t *testing.T) {
	savedAuth, savedVersion := AuthVersion, Version
	defer func() { AuthVersion, Version = savedAuth, savedVersion }()

	Version = "v3.23.0-fix.34.1"

	AuthVersion = ""
	if got := AuthAppVersion(); got != "v3.23.0-fix.34.1" {
		t.Errorf("without AuthVersion the auth reports the plain version, got %q", got)
	}

	AuthVersion = "2026.10.20-1076956260-fix.34.1"
	if got := AuthAppVersion(); got != AuthVersion {
		t.Errorf("got %q, want %q", got, AuthVersion)
	}
	// RequireVersion is untouched: logs, metrics and hotswap keep the tag
	if got := RequireVersion(); got != "v3.23.0-fix.34.1" {
		t.Errorf("RequireVersion = %q, must stay the build version", got)
	}
	if got := newProviderClientAuth("jwt", [16]byte{}).AppVersion; got != AuthVersion {
		t.Errorf("provider auth app_version = %q, want %q", got, AuthVersion)
	}
}
