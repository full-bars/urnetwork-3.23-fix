package main

import (
	"os"
	"path/filepath"
	"testing"
)

// withTempHome redirects os.UserHomeDir() (and therefore every
// proxy*Path() helper) to a temp directory for the duration of the test.
func withTempHome(t *testing.T) string {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir) // os.UserHomeDir() reads this on Windows
	return dir
}

func TestRemoveDeadProxies_RoutesBySource(t *testing.T) {
	home := withTempHome(t)

	fileSourcePath := filepath.Join(home, "proxy.txt")
	if err := os.WriteFile(fileSourcePath, []byte("1.1.1.1:1080:u:p\n2.2.2.2:1080:u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}

	writeProxyConfig(&ProxyConfig{Servers: map[string]string{
		"3.3.3.3:1080": "",
	}})

	urlState := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"4.4.4.4:1080": {},
	}}
	if err := writeProxyURLState(urlState); err != nil {
		t.Fatal(err)
	}

	state := &ProxyState{Source: fileSourcePath, Proxies: map[string]ProxyEntry{}}

	err := removeDeadProxies(state, map[string][]string{
		"file":     {"1.1.1.1:1080"},
		"internal": {"3.3.3.3:1080"},
		"url":      {"4.4.4.4:1080"},
	})
	if err != nil {
		t.Fatal(err)
	}

	b, _ := os.ReadFile(fileSourcePath)
	if got := string(b); got != "2.2.2.2:1080:u:p\n" {
		t.Errorf("file source: got %q", got)
	}

	cfg := readProxyConfig()
	if _, ok := cfg.Servers["3.3.3.3:1080"]; ok {
		t.Errorf("internal source: 3.3.3.3:1080 should have been removed")
	}

	gotURLState, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := gotURLState.Cache["4.4.4.4:1080"]; ok {
		t.Errorf("url source: 4.4.4.4:1080 should have been removed from cache")
	}
}
