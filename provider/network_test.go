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
