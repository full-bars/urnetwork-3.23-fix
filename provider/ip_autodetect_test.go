package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tempHome sets HOME to a temp dir and returns a cleanup function.
func tempHome(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	return dir, func() {
		os.Setenv("HOME", oldHome)
	}
}

// disableFile creates or removes the disable_ip_autodetect marker.
func disableFile(t *testing.T, dir string, create bool) {
	t.Helper()
	path := filepath.Join(dir, ".urnetwork", "disable_ip_autodetect")
	if create {
		os.MkdirAll(filepath.Dir(path), 0o700)
		os.WriteFile(path, []byte("1\n"), 0o644)
	} else {
		os.Remove(path)
	}
}

func TestResolvePublicIP_EnvVarTakesPriority(t *testing.T) {
	_, cleanup := tempHome(t)
	defer cleanup()
	defer os.Unsetenv("URNETWORK_PUBLIC_IP")

	os.Setenv("URNETWORK_PUBLIC_IP", "203.0.113.5")
	got := resolvePublicIP()
	if got != "203.0.113.5" {
		t.Fatalf("expected 203.0.113.5, got %s", got)
	}
}

func TestResolvePublicIP_DisableFileSkipsAutodetect(t *testing.T) {
	dir, cleanup := tempHome(t)
	defer cleanup()
	defer os.Unsetenv("URNETWORK_PUBLIC_IP")

	os.Unsetenv("URNETWORK_PUBLIC_IP")
	disableFile(t, dir, true)

	// Should return empty even though network might work
	got := resolvePublicIP()
	if got != "" {
		t.Fatalf("expected empty string when autodetect disabled, got %s", got)
	}
}

func TestResolvePublicIP_DisableFileNotPresentAttemptsFetch(t *testing.T) {
	dir, cleanup := tempHome(t)
	defer cleanup()
	defer os.Unsetenv("URNETWORK_PUBLIC_IP")

	os.Unsetenv("URNETWORK_PUBLIC_IP")
	disableFile(t, dir, false) // file does not exist

	// Without network, this should return "" (the fetch fails)
	got := resolvePublicIP()
	if got != "" {
		// If network is available, we'd get a real IP — just verify it parses
		if ip := parseIPv4(got); ip == nil {
			t.Fatalf("expected valid IPv4 or empty, got %s", got)
		}
	}
}

func parseIPv4(s string) []byte {
	// Simple check: must be exactly 4 dot-separated octets, each 0-255
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 4 {
		return nil
	}
	var b [4]byte
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return nil
		}
		b[i] = byte(n)
	}
	return b[:]
}

func TestParseIPv4_Valid(t *testing.T) {
	cases := []string{
		"66.249.75.83",
		"192.0.2.1",
		"203.0.113.96",
		"10.0.0.1",
	}
	for _, c := range cases {
		if parseIPv4(c) == nil {
			t.Errorf("parseIPv4(%q) returned nil for valid IPv4", c)
		}
	}
}

func TestParseIPv4_Invalid(t *testing.T) {
	cases := []string{
		"not an ip",
		"",
		"256.1.1.1",
		"1.2.3",
		"::1",
		"2001:db8::1",
		"1.2.3.4.5",
		"999.999.999.999",
	}
	for _, c := range cases {
		if parseIPv4(c) != nil {
			t.Errorf("parseIPv4(%q) returned non-nil for invalid input", c)
		}
	}
}

func TestIPDetectionDisabled_DefaultFalse(t *testing.T) {
	dir, cleanup := tempHome(t)
	defer cleanup()
	disableFile(t, dir, false)
	if ipDetectionDisabled() {
		t.Fatal("expected false when disable file is missing")
	}
}

func TestIPDetectionDisabled_FilePresentTrue(t *testing.T) {
	dir, cleanup := tempHome(t)
	defer cleanup()
	disableFile(t, dir, true)
	if !ipDetectionDisabled() {
		t.Fatal("expected true when disable file exists")
	}
}

// TestGetCachedPublicIP_ConcurrentCallersShareOneFetch guards against a
// thundering herd: when the cache is cold or just expired, every concurrent
// caller of getCachedPublicIP (e.g. every proxy on this provider starting up
// at once) must NOT spawn its own fetchPublicIP goroutine. Only the first
// caller to observe a stale cache should trigger a fetch; the rest get the
// stale/empty value back immediately without adding another outbound
// request to ip.me.
func TestGetCachedPublicIP_ConcurrentCallersShareOneFetch(t *testing.T) {
	cachedIPMu.Lock()
	cachedIP = ""
	cachedIPTime = time.Time{}
	cachedIPRefresh = false
	cachedIPMu.Unlock()
	t.Cleanup(func() {
		// Reset the package-level cache so later tests (e.g.
		// TestProviderDescription_*) don't observe the fake IP this test
		// wrote into it.
		cachedIPMu.Lock()
		cachedIP = ""
		cachedIPTime = time.Time{}
		cachedIPRefresh = false
		cachedIPMu.Unlock()
	})

	var fetchCount int32
	release := make(chan struct{})
	origFetch := fetchPublicIPFunc
	fetchPublicIPFunc = func() string {
		atomic.AddInt32(&fetchCount, 1)
		<-release // hold the "in flight" window open so callers race for it
		return "203.0.113.7"
	}
	defer func() { fetchPublicIPFunc = origFetch }()

	const callers = 50
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			getCachedPublicIP()
		}()
	}

	// Give every caller a chance to reach getCachedPublicIP and observe the
	// stale cache before the in-flight fetch is allowed to complete.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&fetchCount); got != 1 {
		t.Fatalf("fetchPublicIPFunc called %d times for %d concurrent callers, want exactly 1", got, callers)
	}

	// getCachedPublicIP returns before the single in-flight fetch goroutine
	// finishes writing its result back to cachedIP, so poll briefly rather
	// than asserting immediately after wg.Wait().
	deadline := time.Now().Add(time.Second)
	var got string
	for time.Now().Before(deadline) {
		cachedIPMu.Lock()
		got = cachedIP
		cachedIPMu.Unlock()
		if got == "203.0.113.7" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got != "203.0.113.7" {
		t.Fatalf("cachedIP after fetch = %q, want the fetched value", got)
	}
}
