package main

import (
	"os"
	"path/filepath"
	"strings"
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

// TestProxyAddFileBackedAppendsToSourceFile: when the provider is file-backed
// (Workflow A — proxy.state declares a live source file), `proxy add` must
// append to THAT file, not the internal config. Regression for a 2026-09-20
// fleet report: the file-backed paste refusal error suggested "proxy add
// --proxy_file=..." as a workaround, but proxyAdd wrote to the internal
// config which a file-backed reload discards — additions were silently lost.
func TestProxyAddFileBackedAppendsToSourceFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Simulate a running file-backed provider: proxy.state declares the
	// source file, which pre-exists with a couple of proxies.
	stateDir := filepath.Join(dir, ".urnetwork")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	sourceFile := filepath.Join(dir, "proxies.txt")
	if err := os.WriteFile(sourceFile, []byte("1.2.3.4:1080:u1:p1\n5.6.7.8:1080:u2:p2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "proxy.state"), []byte(
		"{\"source\":\""+sourceFile+"\",\"proxies\":{}}\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// Add a new proxy via a file arg — the exact shape the old error message
	// suggested as a workaround.
	toAdd := filepath.Join(dir, "to-add.txt")
	if err := os.WriteFile(toAdd, []byte("9.9.9.9:1080:u9:p9\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := docopt.Opts{
		"<key_address>": []string{toAdd},
		"-f":            true,
	}
	proxyAdd(opts)

	// The source file (not the internal config) must contain the addition,
	// with the original entries preserved.
	b, err := os.ReadFile(sourceFile)
	if err != nil {
		t.Fatal(err)
	}
	content := string(b)
	if !strings.Contains(content, "9.9.9.9:1080:u9:p9") {
		t.Errorf("source file must contain the added proxy, got:\n%q", content)
	}
	if !strings.Contains(content, "1.2.3.4:1080:u1:p1") || !strings.Contains(content, "5.6.7.8:1080:u2:p2") {
		t.Errorf("source file lost pre-existing entries, got:\n%q", content)
	}

	// Internal config must NOT have been written (file-backed reload ignores
	// it): adding 9.9.9.9:1080 to the internal config is exactly the lost
	// write we are fixing.
	cfg := readProxyConfig()
	if _, ok := cfg.Servers["9.9.9.9:1080"]; ok {
		t.Errorf("file-backed add must not write the internal config (reload discards it), got server in config")
	}
}

// TestProxyAddFileBackedDedupsExistingLines: re-adding a line already in the
// source file is a no-op — no duplicate lines, no rewrite.
func TestProxyAddFileBackedDedupsExistingLines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	stateDir := filepath.Join(dir, ".urnetwork")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	sourceFile := filepath.Join(dir, "proxies.txt")
	if err := os.WriteFile(sourceFile, []byte("1.2.3.4:1080:u1:p1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "proxy.state"), []byte(
		"{\"source\":\""+sourceFile+"\",\"proxies\":{}}\n"), 0600); err != nil {
		t.Fatal(err)
	}

	toAdd := filepath.Join(dir, "to-add.txt")
	if err := os.WriteFile(toAdd, []byte("1.2.3.4:1080:u1:p1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := docopt.Opts{
		"<key_address>": []string{toAdd},
		"-f":            true,
	}
	proxyAdd(opts)

	b, err := os.ReadFile(sourceFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("re-add must not duplicate the line, got %d lines: %+v", len(lines), lines)
	}
}
