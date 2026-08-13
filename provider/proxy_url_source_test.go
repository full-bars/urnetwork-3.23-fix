package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
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
	// The probe-config and admission-state TTL caches are process-global and
	// hold snapshots keyed to the previous HOME; a HOME change invalidates
	// them, otherwise a config/state read in one test leaks into the next.
	// Reset immediately (isolate this test's initial state) AND on cleanup
	// (belt-and-suspenders so nothing outlives the test, review round 2).
	resetProbeConfigCache()
	resetAdmissionStateCache()
	t.Cleanup(resetProbeConfigCache)
	t.Cleanup(resetAdmissionStateCache)
	// The per-address earn tracker is also process-global and its state is
	// load-bearing for the paid-grader earn-skip tests: a test that seeds
	// earning state must not influence the next test's expected probe/skip
	// behavior. Fresh tracker per test (independent review finding).
	globalPerProxyEarnTracker = newPerProxyEarnTracker()
	t.Cleanup(func() { globalPerProxyEarnTracker = newPerProxyEarnTracker() })
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

// TestDesiredAddressesForHistoryPruning_KeepsShedAddress reproduces the bug
// this function fixes: a self-heal shed deletes the address from the URL
// cache, so currentDesiredProxyAddresses alone would drop it, and the next
// prune would wipe its history hours before its 1h backoff elapses.
func TestDesiredAddressesForHistoryPruning_KeepsShedAddress(t *testing.T) {
	withTempHome(t)
	t.Cleanup(func() { globalProxyFailureHistory.Reset("5.5.5.5:1080") })

	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	// Empty cache: the address was already shed (removeDeadProxies deletes
	// it), so it won't show up via currentDesiredProxyAddresses.
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{}}); err != nil {
		t.Fatal(err)
	}
	globalProxyFailureHistory.SetBackoffUntil("5.5.5.5:1080", time.Now().Add(time.Hour))

	bare, err := currentDesiredProxyAddresses()
	if err != nil {
		t.Fatal(err)
	}
	if bare["5.5.5.5:1080"] {
		t.Fatal("sanity check failed: shed address shouldn't appear via currentDesiredProxyAddresses alone")
	}

	got, err := desiredAddressesForHistoryPruning()
	if err != nil {
		t.Fatal(err)
	}
	if !got["5.5.5.5:1080"] {
		t.Fatal("shed address mid-backoff must still be kept, or its history gets pruned before it can return")
	}
}

// TestDesiredAddressesForHistoryPruning_DropsAfterBackoffElapses confirms the
// carve-out doesn't pin an address forever — once its backoff elapses (and
// it's still absent from the cache/file/internal sources), it's fair game
// for pruning again, same as any other address that's genuinely gone.
func TestDesiredAddressesForHistoryPruning_DropsAfterBackoffElapses(t *testing.T) {
	withTempHome(t)
	t.Cleanup(func() { globalProxyFailureHistory.Reset("6.6.6.6:1080") })

	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{}}); err != nil {
		t.Fatal(err)
	}
	globalProxyFailureHistory.SetBackoffUntil("6.6.6.6:1080", time.Now().Add(-time.Hour))

	got, err := desiredAddressesForHistoryPruning()
	if err != nil {
		t.Fatal(err)
	}
	if got["6.6.6.6:1080"] {
		t.Fatal("an address whose backoff already elapsed shouldn't be force-kept")
	}
}

// TestDesiredAddressesForHistoryPruning_PropagatesUnderlyingError ensures
// callers still see a currentDesiredProxyAddresses failure (e.g. an unreadable
// configured proxy file) instead of it being silently swallowed by the merge.
func TestDesiredAddressesForHistoryPruning_PropagatesUnderlyingError(t *testing.T) {
	home := withTempHome(t)

	missingSource := filepath.Join(home, "does-not-exist.txt")
	if err := writeProxyState(&ProxyState{Source: missingSource, Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}

	if _, err := desiredAddressesForHistoryPruning(); err == nil {
		t.Fatal("expected an error to propagate when the underlying proxy file source is unreadable")
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

// TestRunProxyURLFetcher_StartupCooldownDefersFirstFetch pins the
// probe-amplification fix: even once file-proxy warmup is done, the first
// fetch must NOT happen until probeStartupCooldown has elapsed. A
// crash-looping process that never lives that long must never reach the
// fetch. This test cancels the context well within the cooldown window and
// asserts the URL server never received a single request.
func TestRunProxyURLFetcher_StartupCooldownDefersFirstFetch(t *testing.T) {
	withTempHome(t)
	proxyWarmupDone.Store(true)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write([]byte("1.2.3.4:1080\n"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runProxyURLFetcher(ctx, []string{srv.URL}, time.Hour, 0, "", 0, true)
		close(done)
	}()

	// Cancel well within the 20s cooldown; the fetcher must still be
	// parked in the cooldown select, not mid-fetch.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runProxyURLFetcher did not stop after context cancellation during the startup cooldown")
	}

	if n := requests.Load(); n != 0 {
		t.Fatalf("fetcher made %d request(s) before the startup cooldown elapsed — the cooldown must defer the first fetch", n)
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
func TestRunProxyURLCleanup_UnconditionalImmediateCleanup(t *testing.T) {
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
		runProxyURLCleanup(ctx, "url", time.Hour, false)
		close(done)
	}()
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
	if _, ok := gotURLState.Cache["4.4.4.4:1080"]; ok {
		t.Error("expected dead proxy to be removed by the unconditional immediate cleanup pass")
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

// TestFetchStretch covers the pressure-to-multiplier ramp that replaces the
// old binary load gate: 1x while calm (below fetchStretchStart), linear
// growth to fetchStretchMax at fetchStretchFull, clamped above that.
func TestFetchStretch(t *testing.T) {
	if v := fetchStretch(0.0); !almostEq(v, 1.0) {
		t.Fatalf("calm: %v", v)
	}
	if v := fetchStretch(0.3); !almostEq(v, 1.0) {
		t.Fatalf("threshold edge: %v", v)
	}
	if v := fetchStretch(0.6); !almostEq(v, 4.5) { // midpoint of 0.3..0.9 → 1+ (7 * 0.5)
		t.Fatalf("mid: %v", v)
	}
	if v := fetchStretch(1.0); !almostEq(v, 8.0) {
		t.Fatalf("pinned: %v", v)
	}
}

// TestShouldFetchNow covers the pacing decision that replaced the binary
// skip gate: under pressure the effective interval stretches up to 8x, but
// a near-empty cache is never stretched (starvation floor).
func TestShouldFetchNow(t *testing.T) {
	base := time.Hour
	// calm: due exactly at base
	if !shouldFetchNow(time.Hour, base, 0, 500) {
		t.Fatal("calm+due must fetch")
	}
	// pressured: base elapsed but stretched interval not yet reached
	if shouldFetchNow(time.Hour, base, 1.0, 500) {
		t.Fatal("pressure 1.0 stretches to 8h; 1h elapsed must not fetch")
	}
	if !shouldFetchNow(8*time.Hour, base, 1.0, 500) {
		t.Fatal("stretched interval elapsed must fetch")
	}
	// starvation floor: near-empty cache always fetches when base elapsed
	if !shouldFetchNow(time.Hour, base, 1.0, 49) {
		t.Fatal("cache <50 must never be gated")
	}
	// scheduling-jitter slack: landing sub-second short of due (ticker fires
	// one interval after the previous tick, but lastFetch is stamped after
	// handler latency) must still count as due; clearly-early must not.
	if !shouldFetchNow(time.Hour-500*time.Millisecond, base, 0, 500) {
		t.Fatal("within 1s slack of due must fetch")
	}
	if shouldFetchNow(time.Hour-2*time.Second, base, 0, 500) {
		t.Fatal("2s early is beyond slack; must not fetch")
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

// TestResolveEffectiveProxyURLMax verifies the AIMD-learned TargetPoolSize
// only caps fetch admission while self-heal is enabled: toggling self-heal
// off restores the configured ceiling immediately, even with a persisted
// target on disk.
func TestResolveEffectiveProxyURLMax(t *testing.T) {
	home := withTempHome(t)
	selfHealMarker := filepath.Join(home, ".urnetwork", "proxy_self_heal")
	writeMarker := func(v string) {
		if err := os.MkdirAll(filepath.Dir(selfHealMarker), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(selfHealMarker, []byte(v), 0600); err != nil {
			t.Fatal(err)
		}
	}

	// (a) no persisted state at all, self-heal on → plain ceiling
	writeMarker("on")
	if v := resolveEffectiveProxyURLMax(500, false); v != 500 {
		t.Fatalf("no state: got %d, want 500", v)
	}

	// (b) target 50 < ceiling 500, self-heal marker "on" → target wins
	if err := writeProxyURLState(&ProxyURLState{
		Cache:          map[string]ProxyURLEntry{},
		TargetPoolSize: 50,
	}); err != nil {
		t.Fatal(err)
	}
	if v := resolveEffectiveProxyURLMax(500, false); v != 50 {
		t.Fatalf("self-heal on: got %d, want 50", v)
	}

	// (c) same persisted target but marker "off" → ceiling restored
	writeMarker("off")
	if v := resolveEffectiveProxyURLMax(500, false); v != 500 {
		t.Fatalf("self-heal off must ignore learned target: got %d, want 500", v)
	}

	// (d) target above the ceiling → ceiling wins
	writeMarker("on")
	if err := writeProxyURLState(&ProxyURLState{
		Cache:          map[string]ProxyURLEntry{},
		TargetPoolSize: 900,
	}); err != nil {
		t.Fatal(err)
	}
	if v := resolveEffectiveProxyURLMax(500, false); v != 500 {
		t.Fatalf("target over ceiling: got %d, want 500", v)
	}

	// (e) ceiling 0 (unlimited) with target 50, self-heal on → target caps
	if err := writeProxyURLState(&ProxyURLState{
		Cache:          map[string]ProxyURLEntry{},
		TargetPoolSize: 50,
	}); err != nil {
		t.Fatal(err)
	}
	if v := resolveEffectiveProxyURLMax(0, false); v != 50 {
		t.Fatalf("unlimited ceiling: got %d, want 50", v)
	}
}
