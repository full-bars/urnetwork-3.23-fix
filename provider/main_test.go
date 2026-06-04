package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
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
