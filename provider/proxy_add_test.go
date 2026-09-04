package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/docopt/docopt-go"
)

func TestProxyAddStraightPathAndAliases(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	proxyFile := filepath.Join(dir, "proxies.txt")
	if err := os.WriteFile(proxyFile, []byte("1.2.3.4:1080\n5.6.7.8:1080:user:pass\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// 1. Straight path as <key_address> positional
	opts := docopt.Opts{
		"<key_address>": []string{proxyFile},
		"-f":            true,
	}
	proxyAdd(opts)

	cfg := readProxyConfig()
	if _, ok := cfg.Servers["1.2.3.4:1080"]; !ok {
		t.Errorf("expected 1.2.3.4:1080 to be added from straight file path")
	}
	if _, ok := cfg.Servers["5.6.7.8:1080:user:pass"]; !ok {
		t.Errorf("expected 5.6.7.8:1080:user:pass to be added from straight file path")
	}

	// 2. Straight URL auto-routing
	optsURL := docopt.Opts{
		"<key_address>": []string{"https://example.com/fleet.txt"},
	}
	proxyAdd(optsURL)

	urlState, err := readProxyURLState()
	if err != nil {
		t.Fatalf("readProxyURLState: %v", err)
	}
	foundURL := false
	for _, src := range urlState.Sources {
		if src == "https://example.com/fleet.txt" {
			foundURL = true
			break
		}
	}
	if !foundURL {
		t.Errorf("expected https://example.com/fleet.txt to be auto-added to URL sources")
	}

	// 3. --file= alias flag
	file2 := filepath.Join(dir, "proxies2.txt")
	if err := os.WriteFile(file2, []byte("9.9.9.9:1080\n"), 0600); err != nil {
		t.Fatal(err)
	}
	optsFile := docopt.Opts{
		"--file": file2,
		"-f":     true,
	}
	proxyAdd(optsFile)

	cfg2 := readProxyConfig()
	if _, ok := cfg2.Servers["9.9.9.9:1080"]; !ok {
		t.Errorf("expected 9.9.9.9:1080 to be added via --file flag")
	}
}
