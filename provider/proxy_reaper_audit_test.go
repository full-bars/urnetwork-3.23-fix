package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Regression tests ported from the disconnected `pr-259` branch (which is
// not attached to any PR) to the real, open PR #259
// (feat/resource-aware-proxy-self-heal), which shares the same
// proxy_url_source.go/proxy_reload.go lineage and had the same bugs. See
// the 2026-07-13 audit report (~/teamwork_projects/pr_code_review) for the
// original findings 1.1-1.6.

// listenSocks5ApiOK simulates a proxy that is both a real SOCKS5 endpoint
// and successfully CONNECTs to the probed API destination — the
// probeAPIReachable case, which listenSocks5Once (greeting-only) cannot
// produce. It replies to the greeting and then unconditionally acks any
// SOCKS5 CONNECT frame with a success response.
func listenSocks5ApiOK(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				greet := make([]byte, 3)
				if _, err := c.Read(greet); err != nil {
					return
				}
				if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
					return
				}
				connectFrame := make([]byte, 10)
				if _, err := c.Read(connectFrame); err != nil {
					return
				}
				c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// listenSlowThenClose accepts a connection, waits `delay`, then closes
// without responding — probeProxy's stage 1 read fails, so this always
// resolves to probeDead, but only after `delay` has elapsed. Used to widen
// the window between a reaper's candidate snapshot and its result-apply
// step so a concurrent write can land in between.
func listenSlowThenClose(t *testing.T, delay time.Duration) (addr string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				time.Sleep(delay)
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// TestFetchAndMergeProxyURLs_MarksApiOKAndSocks5OnlyCorrectly pins findings
// 1.1 (cache lookup used the raw fetched line, including credentials/
// newlines, instead of the parsed address) and 1.2 (successfully-probed
// apiOK proxies never had ProbeOK set to true on ingest).
func TestFetchAndMergeProxyURLs_MarksApiOKAndSocks5OnlyCorrectly(t *testing.T) {
	withTempHome(t)

	// The api-ok fake must now be a faithful transparent proxy: CONNECT
	// succeeds AND the tunnel terminates in a TLS server whose cert the
	// probe trusts (stage 3 of probeProxy verifies TLS through the proxy).
	ca := newTestCA(t)
	leaf := issueLeafForHost(t, ca, "127.0.0.1")
	withProbeTLSRoot(t, ca)

	apiOKAddr, apiOKCleanup := listenSocks5ApiOKTLS(t, &leaf)
	defer apiOKCleanup()
	socks5OnlyAddr, socks5OnlyCleanup := listenSocks5Once(t)
	defer socks5OnlyCleanup()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// api-ok line carries credentials, exercising the "raw line has
		// extra fields" half of finding 1.1's failure mechanism.
		w.Write([]byte(apiOKAddr + ":user:pass\n" + socks5OnlyAddr + "\n"))
	}))
	defer srv.Close()

	before := time.Now()
	fetchAndMergeProxyURLs(context.Background(), []string{srv.URL}, 0, "127.0.0.1", 1)

	got, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cache) != 2 {
		t.Fatalf("cache: got %d entries, want 2: %+v", len(got.Cache), got.Cache)
	}

	apiOKEntry, ok := got.Cache[apiOKAddr]
	if !ok {
		t.Fatalf("api-ok address %s missing from cache", apiOKAddr)
	}
	if !apiOKEntry.ProbeOK {
		t.Errorf("api-ok address %s: ProbeOK = false, want true (finding 1.2)", apiOKAddr)
	}
	if apiOKEntry.LastProbe.Before(before) {
		t.Errorf("api-ok address %s: LastProbe not updated (finding 1.1: raw-line lookup miss)", apiOKAddr)
	}

	socks5Entry, ok := got.Cache[socks5OnlyAddr]
	if !ok {
		t.Fatalf("socks5-only address %s missing from cache", socks5OnlyAddr)
	}
	if socks5Entry.ProbeOK {
		t.Errorf("socks5-only address %s: ProbeOK = true, want false", socks5OnlyAddr)
	}
	if socks5Entry.LastProbe.Before(before) {
		t.Errorf("socks5-only address %s: LastProbe not updated (finding 1.1: raw-line lookup miss)", socks5OnlyAddr)
	}
}

// TestDoWriteReloadTrigger_ConcurrentCallsDoNotCorrupt pins finding 1.4:
// concurrent doWriteReloadTrigger calls raced on the filesystem with no
// lock, corrupting the trigger file or losing increments.
func TestDoWriteReloadTrigger_ConcurrentCallsDoNotCorrupt(t *testing.T) {
	withTempHome(t)
	path, err := proxyReloadPath()
	if err != nil {
		t.Fatal(err)
	}

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i += 1 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- doWriteReloadTrigger(path)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("doWriteReloadTrigger returned error under concurrency: %v", err)
		}
	}

	seq, err := readReloadSeq(path)
	if err != nil {
		t.Fatal(err)
	}
	if seq != n {
		t.Errorf("reload seq after %d concurrent writers: got %d, want %d (lost updates indicate the race is back)", n, seq, n)
	}
}

// TestAcquireProxyLockWithRetry_SucceedsOnceContentionClears and
// TestAcquireProxyLockWithRetry_GivesUpAfterWindow pin finding 1.5:
// evictProxyURLAddress (and the reaper's apply step) used the non-retrying
// acquireProxyLock, so any contention silently aborted the operation.
func TestAcquireProxyLockWithRetry_SucceedsOnceContentionClears(t *testing.T) {
	withTempHome(t)
	path, err := proxyLockPath()
	if err != nil {
		t.Fatal(err)
	}

	release, err := acquireProxyLockAt(path)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		release()
	}()

	start := time.Now()
	releaseRetry, err := acquireProxyLockWithRetry()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("acquireProxyLockWithRetry gave up despite contention clearing: %v", err)
	}
	defer releaseRetry()

	if elapsed < 150*time.Millisecond {
		t.Errorf("lock acquired before contention should have cleared (elapsed=%v) — test setup invalid", elapsed)
	}
}

func TestAcquireProxyLockWithRetry_GivesUpAfterWindow(t *testing.T) {
	withTempHome(t)
	path, err := proxyLockPath()
	if err != nil {
		t.Fatal(err)
	}
	release, err := acquireProxyLockAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	start := time.Now()
	_, err = acquireProxyLockWithRetry()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected acquireProxyLockWithRetry to fail when the lock is held for its entire retry window")
	}
	if elapsed > 2*time.Second {
		t.Errorf("acquireProxyLockWithRetry took %v, want bounded to ~5x100ms retry window", elapsed)
	}
}

// TestRunURLProxyReaperOnce_DoesNotOverwriteFresherEntry pins findings 1.3
// (the reaper's apply step overwrote a fresher concurrent success with a
// stale probe result) and 1.6 (the apply step used the non-retrying lock,
// so contention silently dropped a whole batch of completed probe
// results). Exercises runURLProxyReaperOnce directly — this branch already
// split it out of the ticker loop specifically for testability.
func TestRunURLProxyReaperOnce_DoesNotOverwriteFresherEntry(t *testing.T) {
	withTempHome(t)

	// A slow-closing listener widens the window between the reaper's
	// candidate snapshot and its result-apply step (probeProxy always
	// returns probeDead for it, but only after `delay`).
	addr, cleanup := listenSlowThenClose(t, 300*time.Millisecond)
	defer cleanup()

	staleTime := time.Now().Add(-4 * time.Hour) // older than the 3h calm-pressure threshold
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		addr: {ProbeOK: true, LastProbe: staleTime},
	}}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runURLProxyReaperOnce(context.Background(), "", 0)
	}()

	// Mid-probe (before the slow listener closes, and before the reaper
	// re-acquires the lock to apply results), simulate a concurrent fetch
	// that found this proxy alive and refreshed it.
	time.Sleep(100 * time.Millisecond)
	freshTime := time.Now()
	func() {
		release, err := acquireProxyLockWithRetry()
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		state, err := readProxyURLState()
		if err != nil {
			t.Fatal(err)
		}
		state.Cache[addr] = ProxyURLEntry{ProbeOK: true, LastProbe: freshTime}
		if err := writeProxyURLState(state); err != nil {
			t.Fatal(err)
		}
	}()

	wg.Wait()

	got, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := got.Cache[addr]
	if !ok {
		t.Fatalf("%s was removed from cache — the stale probeDead result should have been skipped, not applied", addr)
	}
	if !entry.ProbeOK {
		t.Errorf("%s: ProbeOK = false, want true — the reaper's stale probeDead result overwrote the concurrent fresh success (finding 1.3)", addr)
	}
	if !entry.LastProbe.Equal(freshTime) {
		t.Errorf("%s: LastProbe = %v, want %v (the concurrently-written fresh timestamp) — got overwritten by the reaper", addr, entry.LastProbe, freshTime)
	}
}
