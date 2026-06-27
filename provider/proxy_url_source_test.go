package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestFetchAndMergeProxyURLs_PersistsAndTriggersReload(t *testing.T) {
	withTempHome(t)

	// The merge step now probes every fetched address with a SOCKS5
	// handshake before adding it, so the fixture addresses need to actually
	// speak SOCKS5 — unlike the old fake 1.2.3.4:1080-style addresses, which
	// the probe correctly filters out as dead.
	addr1, cleanup1 := listenSocks5Once(t)
	defer cleanup1()
	addr2, cleanup2 := listenSocks5Once(t)
	defer cleanup2()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(addr1 + "\n" + addr2 + "\n"))
	}))
	defer srv.Close()

	fetchAndMergeProxyURLs(context.Background(), []string{srv.URL}, 0, "", 0)

	got, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cache) != 2 {
		t.Fatalf("cache: got %d entries, want 2", len(got.Cache))
	}

	reloadPath, _ := proxyReloadPath()
	seq, _ := readReloadSeq(reloadPath)
	if seq != 1 {
		t.Errorf("reload trigger: got seq %d, want 1", seq)
	}
}

func TestFetchAndMergeProxyURLs_NoOpOnFetchFailure(t *testing.T) {
	withTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	fetchAndMergeProxyURLs(context.Background(), []string{srv.URL}, 0, "", 0)

	got, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cache) != 0 {
		t.Fatalf("expected no entries added on fetch failure, got %d", len(got.Cache))
	}
}

func TestRunProxyURLCleanupOnce_ScopeURL_OnlyTouchesURLSourced(t *testing.T) {
	withTempHome(t)

	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"4.4.4.4:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}

	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"4.4.4.4:1080": {Health: "dead", Source: "url"},
		"3.3.3.3:1080": {Health: "dead", Source: "internal"},
	}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}
	writeProxyConfig(&ProxyConfig{Servers: map[string]string{"3.3.3.3:1080": ""}})

	removed := runProxyURLCleanupOnce("url")
	if removed != 1 {
		t.Fatalf("removed: got %d, want 1", removed)
	}

	gotURLState, _ := readProxyURLState()
	if _, ok := gotURLState.Cache["4.4.4.4:1080"]; ok {
		t.Error("expected url-sourced dead proxy to be removed from cache")
	}

	cfg := readProxyConfig()
	if _, ok := cfg.Servers["3.3.3.3:1080"]; !ok {
		t.Error("internal-sourced dead proxy must NOT be removed when scope=url")
	}
}

func TestRunProxyURLCleanupOnce_ScopeNone_RemovesNothing(t *testing.T) {
	withTempHome(t)

	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"4.4.4.4:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"4.4.4.4:1080": {Health: "dead", Source: "url"},
	}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	removed := runProxyURLCleanupOnce("none")
	if removed != 0 {
		t.Fatalf("removed: got %d, want 0 when scope=none", removed)
	}
}

func TestEvictProxyURLAddress_RemovesFromCacheAddsToBlacklist(t *testing.T) {
	withTempHome(t)

	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"4.4.4.4:1080": {},
		"5.5.5.5:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}

	if err := evictProxyURLAddress("4.4.4.4:1080"); err != nil {
		t.Fatal(err)
	}

	got, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Cache["4.4.4.4:1080"]; ok {
		t.Error("evicted address should be removed from cache")
	}
	if _, ok := got.Cache["5.5.5.5:1080"]; !ok {
		t.Error("other cached address should be untouched")
	}
	if _, ok := got.Blacklist["4.4.4.4:1080"]; !ok {
		t.Error("evicted address should be recorded in blacklist")
	}

	reloadPath, _ := proxyReloadPath()
	seq, _ := readReloadSeq(reloadPath)
	if seq != 1 {
		t.Errorf("reload trigger: got seq %d, want 1", seq)
	}
}

func TestEvictProxyURLAddress_ThenFetchNeverReadsItBack(t *testing.T) {
	withTempHome(t)

	addr, cleanup := listenSocks5Once(t)
	defer cleanup()

	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		addr: {},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := evictProxyURLAddress(addr); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(addr + "\n"))
	}))
	defer srv.Close()

	fetchAndMergeProxyURLs(context.Background(), []string{srv.URL}, 0, "", 0)

	got, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Cache[addr]; ok {
		t.Error("evicted/blacklisted address must not be re-added by a later fetch, even though reachable")
	}
}

func TestCurrentDesiredProxyAddresses_MergesFileAndURLCache(t *testing.T) {
	home := withTempHome(t)

	fileSourcePath := filepath.Join(home, "proxy.txt")
	if err := os.WriteFile(fileSourcePath, []byte("1.1.1.1:1080:u:p\n2.2.2.2:1080:u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{Source: fileSourcePath, Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"3.3.3.3:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}

	got, err := currentDesiredProxyAddresses()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1.1.1.1:1080", "2.2.2.2:1080", "3.3.3.3:1080"} {
		if !got[want] {
			t.Errorf("expected %s in desired set, got %v", want, got)
		}
	}
}

func TestCurrentDesiredProxyAddresses_InternalConfigWhenNoFileSource(t *testing.T) {
	withTempHome(t)

	if err := writeProxyState(&ProxyState{Source: "", Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	writeProxyConfig(&ProxyConfig{Servers: map[string]string{
		"9.9.9.9:1080": "",
	}})

	got, err := currentDesiredProxyAddresses()
	if err != nil {
		t.Fatal(err)
	}
	if !got["9.9.9.9:1080"] {
		t.Errorf("expected internal-config address in desired set, got %v", got)
	}
}

func TestCurrentDesiredProxyAddresses_SurvivesGiveUpWaitWindow(t *testing.T) {
	withTempHome(t)

	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"4.4.4.4:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}

	got, err := currentDesiredProxyAddresses()
	if err != nil {
		t.Fatal(err)
	}
	if !got["4.4.4.4:1080"] {
		t.Error("expected url-cached address to count as desired even though it has no live health registration")
	}
}

func TestRunProxyURLFetcher_StopsOnContextCancel(t *testing.T) {
	withTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("1.2.3.4:1080\n"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runProxyURLFetcher(ctx, []string{srv.URL}, time.Hour, 0, "", 0)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runProxyURLFetcher did not stop after context cancellation")
	}
}
