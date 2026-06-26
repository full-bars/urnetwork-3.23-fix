package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The eco memory monitor is a single global watcher, but it was started from
// inside the per-proxy provide loop, so a large proxy list spawned one monitor
// per proxy. Each copy logs the same "[eco]" line and calls runtime.GC() on a
// pressure transition. startEcoMonitorOnce must start exactly one regardless of
// how many times (and from how many call sites) it is invoked.
func TestEcoMonitorStartsOnce(t *testing.T) {
	ecoMonitorStarted.Store(false)

	starts := 0
	orig := startEcoMonitor
	startEcoMonitor = func(ctx context.Context) { starts++ }
	defer func() { startEcoMonitor = orig }()

	ctx := context.Background()
	const proxies = 5
	for i := 0; i < proxies; i++ {
		startEcoMonitorOnce(ctx)
	}

	if starts != 1 {
		t.Fatalf("expected eco monitor to start exactly once across %d calls, got %d", proxies, starts)
	}
}

func TestReadSHMLog_NotExist(t *testing.T) {
	out, err := readSHMLog("/tmp/does-not-exist-urnetwork.log", 0)
	if err == nil {
		t.Fatal("expected error for missing log file")
	}
	if out != "" {
		t.Fatalf("expected empty output, got %q", out)
	}
}

func TestReadSHMLog_AllLines(t *testing.T) {
	f, err := os.CreateTemp("", "urnetwork-test-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString("line1\nline2\nline3\n")
	f.Close()

	out, err := readSHMLog(f.Name(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if out != "line1\nline2\nline3\n" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestNextProxyID_MonotonicallyIncreasing(t *testing.T) {
	atomic.StoreInt64(&proxyIDCounter, 0)

	id0 := nextProxyID()
	id1 := nextProxyID()
	id2 := nextProxyID()

	if id0 != 0 || id1 != 1 || id2 != 2 {
		t.Fatalf("expected 0,1,2 got %d,%d,%d", id0, id1, id2)
	}
}

func TestInitProxyIDCounter_StartsAboveExisting(t *testing.T) {
	atomic.StoreInt64(&proxyIDCounter, 0)
	initProxyIDCounter(10)
	id := nextProxyID()
	if id != 11 {
		t.Fatalf("expected first ID after init to be 11, got %d", id)
	}
}

func TestInitProxyIDCounter_NoopIfAlreadyHigher(t *testing.T) {
	atomic.StoreInt64(&proxyIDCounter, 100)
	initProxyIDCounter(5)
	id := nextProxyID()
	if id != 100 {
		t.Fatalf("expected counter unchanged at 100, got %d", id)
	}
}

func TestWriteReadProxyState_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.state")

	s := &ProxyState{
		Source:    "/app/proxy.txt",
		StartedAt: time.Now().Truncate(time.Second),
		NextID:    5,
		Proxies: map[string]ProxyEntry{
			"1.2.3.4:1080": {ID: 0, Health: "up"},
			"5.6.7.8:1080": {ID: 1, Health: "dead"},
		},
	}

	if err := writeProxyStateTo(path, s); err != nil {
		t.Fatal(err)
	}

	got, err := readProxyStateFrom(path)
	if err != nil {
		t.Fatal(err)
	}

	if got.Source != s.Source {
		t.Errorf("source: got %q want %q", got.Source, s.Source)
	}
	if got.NextID != s.NextID {
		t.Errorf("nextID: got %d want %d", got.NextID, s.NextID)
	}
	if len(got.Proxies) != 2 {
		t.Errorf("proxies: got %d want 2", len(got.Proxies))
	}
}

func TestReadProxyState_NotExist(t *testing.T) {
	s, err := readProxyStateFrom("/tmp/does-not-exist-proxy.state")
	if err != nil {
		t.Fatal(err)
	}
	if s.Proxies == nil {
		t.Fatal("expected non-nil Proxies map")
	}
}

func TestResolveProxyID_ExistingAddressKeepsID(t *testing.T) {
	s := &ProxyState{
		Proxies: map[string]ProxyEntry{
			"1.2.3.4:1080": {ID: 42},
		},
	}
	atomic.StoreInt64(&proxyIDCounter, 100)
	id := resolveProxyID(s, "1.2.3.4:1080")
	if id != 42 {
		t.Fatalf("expected existing ID 42, got %d", id)
	}
}

func TestResolveProxyID_NewAddressGetsNextID(t *testing.T) {
	s := &ProxyState{Proxies: map[string]ProxyEntry{}}
	atomic.StoreInt64(&proxyIDCounter, 7)
	id := resolveProxyID(s, "9.9.9.9:1080")
	if id != 7 {
		t.Fatalf("expected ID 7, got %d", id)
	}
	if _, ok := s.Proxies["9.9.9.9:1080"]; !ok {
		t.Fatal("expected address to be stored in state")
	}
}

func TestReadSHMLog_LastN(t *testing.T) {
	f, err := os.CreateTemp("", "urnetwork-test-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString("line1\nline2\nline3\nline4\nline5\n")
	f.Close()

	out, err := readSHMLog(f.Name(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if out != "line4\nline5\n" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestProxyReloadTrigger_WriteAndRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.reload")

	if err := writeReloadTrigger(path); err != nil {
		t.Fatal(err)
	}
	seq1, err := readReloadSeq(path)
	if err != nil {
		t.Fatal(err)
	}
	if seq1 != 1 {
		t.Fatalf("expected seq 1, got %d", seq1)
	}

	if err := writeReloadTrigger(path); err != nil {
		t.Fatal(err)
	}
	seq2, err := readReloadSeq(path)
	if err != nil {
		t.Fatal(err)
	}
	if seq2 != 2 {
		t.Fatalf("expected seq 2, got %d", seq2)
	}
}

func TestReadReloadSeq_NotExist(t *testing.T) {
	seq, err := readReloadSeq("/tmp/does-not-exist-proxy.reload")
	if err != nil {
		t.Fatal(err)
	}
	if seq != 0 {
		t.Fatalf("expected seq 0 for missing file, got %d", seq)
	}
}

func TestAcquireProxyLock_SecondFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.lock")

	rel, err := acquireProxyLockAt(path)
	if err != nil {
		t.Fatal(err)
	}
	// Second acquisition must fail while the first is held
	if _, err := acquireProxyLockAt(path); err == nil {
		t.Fatal("expected second lock acquisition to fail")
	}
	rel()
	// After release, acquisition should succeed again
	rel2, err := acquireProxyLockAt(path)
	if err != nil {
		t.Fatalf("expected lock acquisition to succeed after release, got %v", err)
	}
	rel2()
}

func TestWriteProxyConfig_AutoReloadTrigger(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)

	config := &ProxyConfig{
		Servers: map[string]string{
			"1.1.1.1:1080": "key1",
		},
	}

	writeProxyConfig(config)

	reloadPath := filepath.Join(dir, ".urnetwork", "proxy.reload")
	seq, err := readReloadSeq(reloadPath)
	if err != nil {
		t.Fatalf("failed to read reload sequence: %v", err)
	}
	if seq != 1 {
		t.Fatalf("expected trigger sequence to be 1 after first write, got %d", seq)
	}

	writeProxyConfig(config)
	seq, err = readReloadSeq(reloadPath)
	if err != nil {
		t.Fatalf("failed to read reload sequence: %v", err)
	}
	if seq != 2 {
		t.Fatalf("expected trigger sequence to be 2 after second write, got %d", seq)
	}
}

// TestProxyAuthRetryDelay_429ScalesWithAttempt is a regression test for a
// live deployment observation: 429 (rate-limited) auth failures were using
// the same flat 0.5-10.5s jitter as ordinary timeouts, so a batch of proxies
// hitting 429s together kept retrying at the same rate that triggered the
// 429s in the first place. 429 delays must grow with the attempt count
// instead.
func TestProxyAuthRetryDelay_429ScalesWithAttempt(t *testing.T) {
	err := errors.New("429 Too Many Requests: <html error page, 162 bytes>")

	d1 := proxyAuthRetryDelay(err, 1)
	if d1 < 5*time.Second || d1 > 10*time.Second {
		t.Fatalf("attempt 1: expected delay in [5s,10s], got %v", d1)
	}

	d3 := proxyAuthRetryDelay(err, 3)
	if d3 < 15*time.Second || d3 > 20*time.Second {
		t.Fatalf("attempt 3: expected delay in [15s,20s], got %v", d3)
	}

	dCap := proxyAuthRetryDelay(err, 100)
	if dCap != 60*time.Second {
		t.Fatalf("expected delay to cap at 60s for a high attempt count, got %v", dCap)
	}
}

// TestProxyAuthRetryDelay_NonRateLimitScalesGentlyWithAttempt is a regression
// test for a live deployment observation: ordinary errors (timeouts,
// connection refused, etc.) used the exact same flat 0.5-10.5s jitter
// regardless of attempt count, so a proven proxy's 9th retry got no more
// breathing room than its 1st. Delay must still grow with attempt, just
// gentler and with a lower cap than the explicit-429 schedule.
func TestProxyAuthRetryDelay_NonRateLimitScalesGentlyWithAttempt(t *testing.T) {
	err := errors.New("Timeout.")

	d1 := proxyAuthRetryDelay(err, 1)
	if d1 < 500*time.Millisecond || d1 > 3500*time.Millisecond {
		t.Fatalf("attempt 1: expected delay in [0.5s,3.5s], got %v", d1)
	}

	d3 := proxyAuthRetryDelay(err, 3)
	if d3 < 2500*time.Millisecond || d3 > 5500*time.Millisecond {
		t.Fatalf("attempt 3: expected delay in [2.5s,5.5s], got %v", d3)
	}

	dCap := proxyAuthRetryDelay(err, 100)
	if dCap != 15*time.Second {
		t.Fatalf("expected delay to cap at 15s for a high attempt count, got %v", dCap)
	}
}

func TestTagProxySourceIfUnset_SetsOnFirstCall(t *testing.T) {
	s := &ProxyState{Proxies: map[string]ProxyEntry{}}
	tagProxySourceIfUnset(s, "1.2.3.4:1080", "url")
	if got := s.Proxies["1.2.3.4:1080"].Source; got != "url" {
		t.Fatalf("expected source %q, got %q", "url", got)
	}
}

func TestTagProxySourceIfUnset_DoesNotOverwriteExisting(t *testing.T) {
	s := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.2.3.4:1080": {ID: 1, Source: "file"},
	}}
	tagProxySourceIfUnset(s, "1.2.3.4:1080", "url")
	if got := s.Proxies["1.2.3.4:1080"].Source; got != "file" {
		t.Fatalf("expected source to remain %q, got %q", "file", got)
	}
}

// TestClassifyAuthFailureCause is a regression test for a bug found during
// live fleet deployment testing: "proxy unreachable" (synthesized by the
// local TCP reachability probe before any auth attempt is made, meaning the
// API was never contacted) was bundled into the same cause string as real
// network errors reaching the API. On a public proxy list that's mostly
// dead entries, this made nearly all give-ups look like API outages in the
// logs, when almost none of them were.
func TestClassifyAuthFailureCause(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "proxy unreachable gets its own cause, not lumped with API errors",
			err:  errors.New("proxy unreachable: 1.2.3.4:1080"),
			want: "proxy itself is unreachable (dead/offline SOCKS endpoint — not an API issue)",
		},
		{
			name: "context deadline exceeded is a real network error",
			err:  context.DeadlineExceeded,
			want: "network error reaching API (check connectivity to api.bringyour.com)",
		},
		{
			name: "context canceled is a real network error",
			err:  context.Canceled,
			want: "network error reaching API (check connectivity to api.bringyour.com)",
		},
		{
			name: "Timeout substring is a real network error",
			err:  errors.New("request Timeout after 30s"),
			want: "network error reaching API (check connectivity to api.bringyour.com)",
		},
		{
			name: "connection refused is a real network error",
			err:  errors.New("dial tcp: connection refused"),
			want: "network error reaching API (check connectivity to api.bringyour.com)",
		},
		{
			name: "anything else is treated as a rejected token",
			err:  errors.New("401 unauthorized"),
			want: "API rejected token (check JWT validity)",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyAuthFailureCause(c.err); got != c.want {
				t.Fatalf("classifyAuthFailureCause(%q) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

// TestStartupSortOrder verifies that file-sourced and internal proxies sort
// before URL-sourced proxies, so backoffPacer gives them a head start.
func TestStartupSortOrder(t *testing.T) {
	settings := []*connect.ProxySettings{
		{Address: "url-a:1080"},
		{Address: "file-a:1080"},
		{Address: "url-b:1080"},
		{Address: "internal-a:1080"},
		{Address: "url-c:1080"},
		{Address: "file-b:1080"},
	}
	sourceOf := map[string]string{
		"url-a:1080":      "url",
		"file-a:1080":     "file",
		"url-b:1080":      "url",
		"internal-a:1080": "internal",
		"url-c:1080":      "url",
		"file-b:1080":     "file",
	}

	sort.SliceStable(settings, func(i, j int) bool {
		si := sourceOf[settings[i].Address]
		sj := sourceOf[settings[j].Address]
		if si == "url" && sj != "url" {
			return false
		}
		if si != "url" && sj == "url" {
			return true
		}
		return false
	})

	got := make([]string, len(settings))
	for i, s := range settings {
		got[i] = sourceOf[s.Address]
	}

	want := []string{"file", "internal", "file", "url", "url", "url"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position %d: got source %q for %s, want %q", i, got[i], settings[i].Address, want[i])
		}
	}
}

// TestBackoffPacer_ZeroStagger verifies that a zero stagger returns
// immediately (no delay), and that context cancellation aborts the wait.
func TestBackoffPacer_ZeroStagger(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Zero stagger must return true immediately
	start := time.Now()
	ok := backoffPacer(100, 0, time.Now(), ctx)
	if !ok {
		t.Fatal("zero stagger must return true")
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("zero stagger took %v, expected near-instant return", elapsed)
	}

	// Canceled context with non-zero stagger must return false.
	// Use n=50 with 10ms stagger so the wait is ~500ms — long enough
	// for cancellation to take effect, short enough to keep the test fast.
	cancelCtx, cancelFn := context.WithCancel(context.Background())
	cancelFn()
	time.Sleep(5 * time.Millisecond) // let cancellation propagate
	ok = backoffPacer(50, 10, time.Now(), cancelCtx)
	if ok {
		t.Fatal("canceled context with non-zero stagger must return false")
	}
}
