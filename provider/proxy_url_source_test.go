package main

import (
	"context"
	"errors"
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
	// Disable reload trigger debounce for tests that write triggers back-to-back.
	lastReloadTriggerTime.ts = time.Time{}
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
		runProxyURLFetcher(ctx, []string{srv.URL}, time.Hour, 0, "", 0, true)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runProxyURLFetcher did not stop after context cancellation")
	}
}

// TestResolveSelfHealEnabled_Override covers the `urnet-tools self-heal
// on|off` runtime toggle's marker file: absent/empty means the startup
// value (default off unless URNETWORK_SELF_HEAL=1) passes through, "on"
// (case-insensitive) means enabled, and any other non-empty value means
// disabled — the override always wins over the startup value when present.
func TestResolveSelfHealEnabled_Override(t *testing.T) {
	home := withTempHome(t)

	if got := resolveSelfHealEnabled(false); got {
		t.Error("no override file: expected startup value false to pass through")
	}
	if got := resolveSelfHealEnabled(true); !got {
		t.Error("no override file: expected startup value true to pass through")
	}

	path := filepath.Join(home, ".urnetwork", "proxy_self_heal")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("on"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := resolveSelfHealEnabled(false); !got {
		t.Error("override file = \"on\": expected enabled regardless of startup value")
	}

	if err := os.WriteFile(path, []byte("ON\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := resolveSelfHealEnabled(false); !got {
		t.Error("override file = \"ON\": expected case-insensitive match to enable")
	}

	if err := os.WriteFile(path, []byte("off"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := resolveSelfHealEnabled(true); got {
		t.Error("override file = \"off\": expected disabled regardless of startup value")
	}

	if err := os.WriteFile(path, []byte("garbage"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := resolveSelfHealEnabled(true); got {
		t.Error("override file = \"garbage\": expected disabled (only \"on\" enables)")
	}

	if err := os.WriteFile(path, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	if got := resolveSelfHealEnabled(true); !got {
		t.Error("override file empty: expected startup value true to pass through")
	}
}

// TestRunProxyURLCleanup_SelfHealOff_SkipsImmediateCleanup guards the fix
// to runProxyURLCleanup's startup pass: an operator who wrote the self-heal
// override file to "off" *before* the provider started must not have that
// first immediate cleanup call run anyway (it previously only checked
// activeScope, bypassing the toggle for the one call outside the ticker
// loop).
func TestRunProxyURLCleanup_SelfHealOff_SkipsImmediateCleanup(t *testing.T) {
	withTempHome(t)

	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"4.4.4.4:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{
		"4.4.4.4:1080": {Health: "dead", Source: "url"},
	}}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// selfHealEnabled=false with no override file present: the startup
		// value alone must suppress the immediate cleanup pass.
		runProxyURLCleanup(ctx, "url", time.Hour, false)
		close(done)
	}()
	// Give the immediate pass a moment to run (it happens before the
	// goroutine blocks on the ticker/ctx select), then cancel and wait.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runProxyURLCleanup did not stop after context cancellation")
	}

	gotURLState, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := gotURLState.Cache["4.4.4.4:1080"]; !ok {
		t.Error("expected dead proxy to survive the immediate cleanup pass when self-heal is off")
	}
}

// TestRunURLProxyReaperOnce_DemotesStaleProbeOKEntryOnFailure covers change
// 5 from the LA1 self-healing plan: without this, a proxy that passed its
// initial probe then died later was invisible to the reaper forever (only
// exit was the slow give-up eviction pipeline). A stale ProbeOK=true entry
// that now fails its re-probe must be demoted so the normal 3-strikes
// blacklist path picks it up.
func TestRunURLProxyReaperOnce_DemotesStaleProbeOKEntryOnFailure(t *testing.T) {
	withTempHome(t)

	deadAddr := "127.0.0.1:1" // nothing listens here: connection refused, fast
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		deadAddr: {
			ProbeOK:   true,
			LastProbe: time.Now().Add(-4 * time.Hour), // older than proxyReaperStaleThreshold (3h)
		},
	}}); err != nil {
		t.Fatal(err)
	}

	runURLProxyReaperOnce(context.Background(), "", 0)

	got, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := got.Cache[deadAddr]
	if !ok {
		t.Fatalf("expected %s to remain in cache after a single failed re-probe (demoted, not yet blacklisted)", deadAddr)
	}
	if entry.ProbeOK {
		t.Error("expected ProbeOK=false after stale re-probe failure")
	}
	if entry.ProbeFails != 1 {
		t.Errorf("expected ProbeFails=1 after first failed re-probe, got %d", entry.ProbeFails)
	}
}

// TestRunURLProxyReaperOnce_SkipsFreshProbeOKEntry ensures the reaper only
// re-probes once-good entries after proxyReaperStaleThreshold, not on every
// cycle — otherwise every proxy would be re-probed every 5 minutes forever.
func TestRunURLProxyReaperOnce_SkipsFreshProbeOKEntry(t *testing.T) {
	withTempHome(t)

	addr := "127.0.0.1:1"
	fresh := time.Now().Add(-1 * time.Minute)
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		addr: {ProbeOK: true, LastProbe: fresh},
	}}); err != nil {
		t.Fatal(err)
	}

	runURLProxyReaperOnce(context.Background(), "", 0)

	got, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	entry := got.Cache[addr]
	if !entry.ProbeOK {
		t.Error("fresh ProbeOK=true entry should not have been re-probed/demoted")
	}
	if !entry.LastProbe.Equal(fresh) {
		t.Error("fresh entry's LastProbe should be untouched (reaper should have skipped it, not re-probed)")
	}
}

// TestShouldSkipProxyURLFetch covers the load-gating decision (change 4).
// The key regression this guards against: gating must not depend on
// whether a cap is configured — it was previously wired to only apply when
// proxy_url_max > 0, silently disabling load protection on exactly the
// unbounded config where it matters most (LA1's original setup).
func TestShouldSkipProxyURLFetch(t *testing.T) {
	cases := []struct {
		name             string
		cacheSize        int
		consecutiveSkips int
		load1, load5     float64
		threshold        float64
		loadErr          error
		want             bool
	}{
		{"sustained overload gates", 200, 0, 3.0, 2.5, 2.0, nil, true},
		{"under threshold does not gate", 200, 0, 1.0, 0.8, 2.0, nil, false},
		{"transient spike (5m cool) does not gate", 200, 0, 5.0, 0.8, 2.0, nil, false},
		{"starvation escape: near-empty cache never gates", 10, 0, 9.0, 9.0, 2.0, nil, false},
		{"starvation escape: 6 consecutive skips forces a fetch", 200, 6, 9.0, 9.0, 2.0, nil, false},
		{"fail-open on load read error", 200, 0, 0, 0, 2.0, errors.New("no /proc/loadavg"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shouldSkipProxyURLFetch(c.cacheSize, c.consecutiveSkips, c.load1, c.load5, c.threshold, c.loadErr)
			if got != c.want {
				t.Errorf("shouldSkipProxyURLFetch(cache=%d, skips=%d, load1=%.1f, load5=%.1f, threshold=%.1f, err=%v) = %v, want %v",
					c.cacheSize, c.consecutiveSkips, c.load1, c.load5, c.threshold, c.loadErr, got, c.want)
			}
		})
	}
}

// TestResolveProxyLoadThreshold_Override covers the runtime override file
// used by `urnet-tools set proxy-load-threshold`.
func TestResolveProxyLoadThreshold_Override(t *testing.T) {
	home := withTempHome(t)

	if got := resolveProxyLoadThreshold(2.0); got != 2.0 {
		t.Errorf("no override file: got %v, want startup default 2.0", got)
	}

	path := filepath.Join(home, ".urnetwork", "proxy_load_threshold")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("3.5\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := resolveProxyLoadThreshold(2.0); got != 3.5 {
		t.Errorf("with override file: got %v, want 3.5", got)
	}

	// Invalid/non-positive values fall back to the startup default.
	if err := os.WriteFile(path, []byte("not-a-number"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := resolveProxyLoadThreshold(2.0); got != 2.0 {
		t.Errorf("with unparseable override: got %v, want fallback 2.0", got)
	}
}

// TestGetSystemLoad_ParsesProcLoadavg smoke-tests the /proc/loadavg reader.
// Skipped on non-Linux, where the function is expected to fail-open by
// returning an error.
func TestGetSystemLoad_ParsesProcLoadavg(t *testing.T) {
	load1, load5, err := getSystemLoad()
	if err != nil {
		t.Skipf("no /proc/loadavg on this platform (expected fail-open behavior): %v", err)
	}
	if load1 < 0 || load5 < 0 {
		t.Errorf("expected non-negative load averages, got load1=%v load5=%v", load1, load5)
	}
}
