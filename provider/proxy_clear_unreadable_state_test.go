package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"
)

// An unreadable proxy.state must not swallow the error or prevent URL cache clearing:
// the internal servers and URL cache must be cleared, reload triggered, and the unreadable
// state reported to the operator.
func TestProxyRemoveAllReportsUnreadableStateAndClearsCache(t *testing.T) {
	withTempHome(t)

	// Seed internal config
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ProxyConfig{Servers: map[string]string{"1.2.3.4:1080": ""}}
	writeProxyConfig(cfg)

	// Seed URL cache
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{"5.6.7.8:1080": {}}}); err != nil {
		t.Fatal(err)
	}

	// Corrupt proxy.state
	statePath := filepath.Join(home, ".urnetwork", "proxy.state")
	if err := os.WriteFile(statePath, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}

	// Capture stderr
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w

	proxyRemove(docopt.Opts{"--all": true})

	w.Close()
	os.Stderr = oldStderr
	var buf bytes.Buffer
	io.Copy(&buf, r)
	stderrOutput := buf.String()

	if !strings.Contains(stderrOutput, "proxy.state not cleared") {
		t.Fatalf("expected stderr to report 'proxy.state not cleared', got:\n%s", stderrOutput)
	}

	// Internal config cleared
	afterCfg := readProxyConfig()
	if len(afterCfg.Servers) != 0 {
		t.Fatalf("internal config not cleared: %v", afterCfg.Servers)
	}

	// URL cache cleared past the unreadable state
	afterURL, err := readProxyURLState()
	if err != nil || len(afterURL.Cache) != 0 {
		t.Fatalf("URL cache not cleared: %v, err=%v", afterURL, err)
	}

	// Reload trigger written
	reloadPath, err := proxyReloadPath()
	if err != nil {
		t.Fatal(err)
	}
	seq, err := readReloadSeq(reloadPath)
	if err != nil || seq < 1 {
		t.Fatalf("reload trigger seq = %d (%v), want >= 1", seq, err)
	}
}
