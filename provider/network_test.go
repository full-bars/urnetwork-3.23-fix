package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/docopt/docopt-go"
)

func TestValidateApiUrl(t *testing.T) {
	cases := []struct {
		url     string
		wantErr bool
	}{
		{"https://example.com", false},
		{"http://example.com", false},
		{"ws://example.com", true},
		{"wss://example.com", true},
		{"ftp://example.com", true},
		{"not a url", true},
		{"", true},
	}
	for _, c := range cases {
		err := validateApiUrl(c.url)
		if c.wantErr && err == nil {
			t.Errorf("validateApiUrl(%q): expected error, got nil", c.url)
		}
		if !c.wantErr && err != nil {
			t.Errorf("validateApiUrl(%q): unexpected error: %s", c.url, err)
		}
	}
}

func TestValidateConnectUrl(t *testing.T) {
	cases := []struct {
		url     string
		wantErr bool
	}{
		{"ws://example.com", false},
		{"wss://example.com", false},
		{"http://example.com", true},
		{"https://example.com", true},
		{"ftp://example.com", true},
		{"not a url", true},
		{"", true},
	}
	for _, c := range cases {
		err := validateConnectUrl(c.url)
		if c.wantErr && err == nil {
			t.Errorf("validateConnectUrl(%q): expected error, got nil", c.url)
		}
		if !c.wantErr && err != nil {
			t.Errorf("validateConnectUrl(%q): unexpected error: %s", c.url, err)
		}
	}
}

func TestReadNetworkConfigMissing(t *testing.T) {
	withTempHome(t)
	_, ok, err := readNetworkConfig()
	if err != nil {
		t.Fatalf("readNetworkConfig: unexpected error: %s", err)
	}
	if ok {
		t.Fatalf("readNetworkConfig: expected ok=false for missing file")
	}
}

func TestWriteThenReadNetworkConfig(t *testing.T) {
	withTempHome(t)
	if err := writeNetworkConfig("https://example.com", "wss://example.com"); err != nil {
		t.Fatalf("writeNetworkConfig: unexpected error: %s", err)
	}
	cfg, ok, err := readNetworkConfig()
	if err != nil {
		t.Fatalf("readNetworkConfig: unexpected error: %s", err)
	}
	if !ok {
		t.Fatalf("readNetworkConfig: expected ok=true after write")
	}
	if cfg.ApiUrl != "https://example.com" {
		t.Errorf("ApiUrl = %q, want %q", cfg.ApiUrl, "https://example.com")
	}
	if cfg.ConnectUrl != "wss://example.com" {
		t.Errorf("ConnectUrl = %q, want %q", cfg.ConnectUrl, "wss://example.com")
	}
}

func TestWriteNetworkConfigRejectsBadUrls(t *testing.T) {
	withTempHome(t)
	if err := writeNetworkConfig("ws://example.com", "wss://example.com"); err == nil {
		t.Fatalf("writeNetworkConfig: expected error for bad api_url scheme")
	}
	if err := writeNetworkConfig("https://example.com", "https://example.com"); err == nil {
		t.Fatalf("writeNetworkConfig: expected error for bad connect_url scheme")
	}
	// Nothing should have been written.
	_, ok, err := readNetworkConfig()
	if err != nil {
		t.Fatalf("readNetworkConfig: unexpected error: %s", err)
	}
	if ok {
		t.Fatalf("readNetworkConfig: expected ok=false after rejected write")
	}
}

// TestWriteNetworkConfigRejectsBadUrlsDoesNotCreateDir guards against
// partial state: URL validation must happen before ~/.urnetwork is
// created, so a rejected write leaves no directory behind on a fresh
// install.
func TestWriteNetworkConfigRejectsBadUrlsDoesNotCreateDir(t *testing.T) {
	home := withTempHome(t)
	if err := writeNetworkConfig("ftp://example.com", "wss://example.com"); err == nil {
		t.Fatalf("writeNetworkConfig: expected error for bad api_url scheme")
	}
	if _, err := os.Stat(filepath.Join(home, ".urnetwork")); !os.IsNotExist(err) {
		t.Fatalf("expected ~/.urnetwork to not be created on rejected write, stat err = %v", err)
	}
}

// TestWriteNetworkConfigPermissions matches this repo's convention for
// other ~/.urnetwork state files (jwt, .provider.key, hub_ca.pem): the
// directory is 0700 and the file is 0600, since network.json can carry
// an internal/test-backend hostname a user wouldn't want world-readable.
func TestWriteNetworkConfigPermissions(t *testing.T) {
	home := withTempHome(t)
	if err := writeNetworkConfig("https://example.com", "wss://example.com"); err != nil {
		t.Fatalf("writeNetworkConfig: unexpected error: %s", err)
	}

	dirInfo, err := os.Stat(filepath.Join(home, ".urnetwork"))
	if err != nil {
		t.Fatalf("stat ~/.urnetwork: unexpected error: %s", err)
	}
	if dirInfo.Mode().Perm() != 0700 {
		t.Errorf("~/.urnetwork perms = %v, want 0700", dirInfo.Mode().Perm())
	}

	p, err := networkConfigPath()
	if err != nil {
		t.Fatalf("networkConfigPath: unexpected error: %s", err)
	}
	fileInfo, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat network.json: unexpected error: %s", err)
	}
	if fileInfo.Mode().Perm() != 0600 {
		t.Errorf("network.json perms = %v, want 0600", fileInfo.Mode().Perm())
	}
}

func TestResetNetworkConfig(t *testing.T) {
	withTempHome(t)

	// Reset on a missing file is a no-op, not an error.
	if err := resetNetworkConfig(); err != nil {
		t.Fatalf("resetNetworkConfig on missing file: unexpected error: %s", err)
	}

	if err := writeNetworkConfig("https://example.com", "wss://example.com"); err != nil {
		t.Fatalf("writeNetworkConfig: unexpected error: %s", err)
	}
	if err := resetNetworkConfig(); err != nil {
		t.Fatalf("resetNetworkConfig: unexpected error: %s", err)
	}
	_, ok, err := readNetworkConfig()
	if err != nil {
		t.Fatalf("readNetworkConfig: unexpected error: %s", err)
	}
	if ok {
		t.Fatalf("readNetworkConfig: expected ok=false after reset")
	}
}

func TestNetworkConfigPath(t *testing.T) {
	home := withTempHome(t)
	p, err := networkConfigPath()
	if err != nil {
		t.Fatalf("networkConfigPath: unexpected error: %s", err)
	}
	want := filepath.Join(home, ".urnetwork", "network.json")
	if p != want {
		t.Errorf("networkConfigPath = %q, want %q", p, want)
	}
	// Path resolution must not require the file or directory to exist.
	if _, err := os.Stat(filepath.Dir(p)); err == nil {
		t.Fatalf("expected ~/.urnetwork to not exist yet before any write")
	}
}

func TestResolveApiUrlPrecedence(t *testing.T) {
	withTempHome(t)

	// Neither flag nor saved config: falls back to DefaultApiUrl.
	got, err := resolveApiUrl(docopt.Opts{})
	if err != nil {
		t.Fatalf("resolveApiUrl: unexpected error: %s", err)
	}
	if got != DefaultApiUrl {
		t.Errorf("resolveApiUrl (no flag, no saved) = %q, want %q", got, DefaultApiUrl)
	}

	// Saved config present, no flag: saved config wins.
	if err := writeNetworkConfig("https://saved.example.com", "wss://saved.example.com"); err != nil {
		t.Fatalf("writeNetworkConfig: unexpected error: %s", err)
	}
	got, err = resolveApiUrl(docopt.Opts{})
	if err != nil {
		t.Fatalf("resolveApiUrl: unexpected error: %s", err)
	}
	if got != "https://saved.example.com" {
		t.Errorf("resolveApiUrl (saved, no flag) = %q, want %q", got, "https://saved.example.com")
	}

	// Flag present: flag wins over saved config.
	got, err = resolveApiUrl(docopt.Opts{"--api_url": "https://flag.example.com"})
	if err != nil {
		t.Fatalf("resolveApiUrl: unexpected error: %s", err)
	}
	if got != "https://flag.example.com" {
		t.Errorf("resolveApiUrl (flag) = %q, want %q", got, "https://flag.example.com")
	}
}

// TestResolveApiUrlEmptyFlagOverridesSaved is a characterization test:
// resolveApiUrl only checks whether the --api_url key parsed as a
// string without error, so an explicit but empty flag ("--api_url=")
// is indistinguishable from a real value and silently wins over a
// saved config, producing an empty apiUrl. This mirrors the upstream
// sn PR's resolveApiUrl and is not a regression introduced by this
// port, but it's worth pinning down so a future change to the
// precedence logic doesn't alter this behavior by accident.
func TestResolveApiUrlEmptyFlagOverridesSaved(t *testing.T) {
	withTempHome(t)
	if err := writeNetworkConfig("https://saved.example.com", "wss://saved.example.com"); err != nil {
		t.Fatalf("writeNetworkConfig: unexpected error: %s", err)
	}
	got, err := resolveApiUrl(docopt.Opts{"--api_url": ""})
	if err != nil {
		t.Fatalf("resolveApiUrl: unexpected error: %s", err)
	}
	if got != "" {
		t.Errorf("resolveApiUrl (empty flag) = %q, want empty string (current behavior: empty flag still wins over saved config)", got)
	}
}

func TestResolveConnectUrlPrecedence(t *testing.T) {
	withTempHome(t)

	got, err := resolveConnectUrl(docopt.Opts{})
	if err != nil {
		t.Fatalf("resolveConnectUrl: unexpected error: %s", err)
	}
	if got != DefaultConnectUrl {
		t.Errorf("resolveConnectUrl (no flag, no saved) = %q, want %q", got, DefaultConnectUrl)
	}

	if err := writeNetworkConfig("https://saved.example.com", "wss://saved.example.com"); err != nil {
		t.Fatalf("writeNetworkConfig: unexpected error: %s", err)
	}
	got, err = resolveConnectUrl(docopt.Opts{})
	if err != nil {
		t.Fatalf("resolveConnectUrl: unexpected error: %s", err)
	}
	if got != "wss://saved.example.com" {
		t.Errorf("resolveConnectUrl (saved, no flag) = %q, want %q", got, "wss://saved.example.com")
	}

	got, err = resolveConnectUrl(docopt.Opts{"--connect_url": "wss://flag.example.com"})
	if err != nil {
		t.Fatalf("resolveConnectUrl: unexpected error: %s", err)
	}
	if got != "wss://flag.example.com" {
		t.Errorf("resolveConnectUrl (flag) = %q, want %q", got, "wss://flag.example.com")
	}
}

// TestApiProbeHostPort covers apiProbeHostPort's URL parsing, including the
// M6 fix's port extraction and the case a manual scheme-trim +
// net.SplitHostPort implementation gets wrong: a path or query string after
// an explicit port (validateApiUrl permits paths; only scheme+host are
// required) must not prevent the port from being recognized.
func TestApiProbeHostPort(t *testing.T) {
	cases := []struct {
		name     string
		apiUrl   string
		wantHost string
		wantPort uint16
	}{
		{"empty falls back to default", "", defaultAPIHost, uint16(defaultAPIPort)},
		{"host with explicit port", "https://api.example.com:8443", "api.example.com", 8443},
		{"host without port uses default port", "https://api.example.com", "api.example.com", uint16(defaultAPIPort)},
		{"http scheme", "http://api.example.com:9000", "api.example.com", 9000},
		{"host and port survive a path suffix", "https://api.example.com:8443/v1", "api.example.com", 8443},
		{"unparseable falls back to default", "https://", defaultAPIHost, uint16(defaultAPIPort)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, port := apiProbeHostPort(c.apiUrl)
			if host != c.wantHost {
				t.Errorf("apiProbeHostPort(%q) host = %q, want %q", c.apiUrl, host, c.wantHost)
			}
			if port != c.wantPort {
				t.Errorf("apiProbeHostPort(%q) port = %d, want %d", c.apiUrl, port, c.wantPort)
			}
		})
	}
}

func TestChooseNetworkCmdSaves(t *testing.T) {
	withTempHome(t)
	opts := docopt.Opts{
		"choose_network": true,
		"<api_url>":      "https://example.com",
		"<connect_url>":  "wss://example.com",
	}
	chooseNetworkCmd(opts)

	cfg, ok, err := readNetworkConfig()
	if err != nil {
		t.Fatalf("readNetworkConfig: unexpected error: %s", err)
	}
	if !ok {
		t.Fatalf("expected network config to be saved")
	}
	if cfg.ApiUrl != "https://example.com" || cfg.ConnectUrl != "wss://example.com" {
		t.Fatalf("saved config = %+v, want api_url=https://example.com connect_url=wss://example.com", cfg)
	}
}

func TestResolveApiUrlCorruptConfig(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("MkdirAll: unexpected error: %s", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "network.json"), []byte("{not valid json"), 0600); err != nil {
		t.Fatalf("WriteFile: unexpected error: %s", err)
	}

	if _, err := resolveApiUrl(docopt.Opts{}); err == nil {
		t.Fatalf("resolveApiUrl: expected error for corrupt config, got nil")
	}
	if _, err := resolveConnectUrl(docopt.Opts{}); err == nil {
		t.Fatalf("resolveConnectUrl: expected error for corrupt config, got nil")
	}
}

func TestChooseNetworkCmdReset(t *testing.T) {
	withTempHome(t)
	if err := writeNetworkConfig("https://example.com", "wss://example.com"); err != nil {
		t.Fatalf("writeNetworkConfig: unexpected error: %s", err)
	}

	opts := docopt.Opts{
		"choose_network": true,
		"--reset":        true,
	}
	chooseNetworkCmd(opts)

	_, ok, err := readNetworkConfig()
	if err != nil {
		t.Fatalf("readNetworkConfig: unexpected error: %s", err)
	}
	if ok {
		t.Fatalf("expected network config to be cleared after reset")
	}
}
