package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

func TestUDPPersistRestartAndExpiry(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	ps := &connect.ProxySettings{Network: "tcp", Address: "example.test:1234", Auth: &proxy.Auth{User: "private-user", Password: "private-password"}}
	count := 0
	store := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		count++
		return proxyUDPResult{Status: proxyUDPOK, TCPExit: "192.0.2.1", UDPExit: "192.0.2.1", Err: errors.New("private-error")}
	})
	store.enablePersistence(path, now)
	store.Check(context.Background(), ps, now)
	store.flushPersist()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-user", "private-password", "private-error", "example.test"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("state contains private identity or error")
		}
	}
	loaded := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		t.Fatal("unexpected probe")
		return proxyUDPResult{}
	})
	loaded.enablePersistence(path, now.Add(time.Minute))
	result := loaded.Check(context.Background(), ps, now.Add(time.Minute))
	if result.Status != proxyUDPOK || result.UDPExit != "192.0.2.1" || count != 1 {
		t.Fatalf("restored result: %+v, calls %d", result, count)
	}
	if got := loaded.coverage([]string{ps.Key()}, now.Add(proxyUDPOKTTL)); got.Unchecked != 1 {
		t.Fatalf("expired coverage: %+v", got)
	}
	expired := newProxyUDPCheckStore(nil)
	expired.enablePersistence(path, now.Add(proxyUDPOKTTL))
	if got := expired.coverage([]string{ps.Key()}, now.Add(proxyUDPOKTTL)); got.Unchecked != 1 {
		t.Fatalf("expired restart: %+v", got)
	}
}

func TestUDPPersistInvalidInput(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ps := &connect.ProxySettings{Address: "example.test:1234"}
	path := filepath.Join(t.TempDir(), "state.json")
	// the entry itself is well formed, so a rejection can only come from the version
	future := udpPersistFile{Version: 1, Entries: []udpPersistEntry{{
		Key:     udpPersistKey(ps.Key()),
		Status:  proxyUDPOK,
		At:      now.Add(time.Second),
		TCPExit: "192.0.2.7",
		UDPExit: "192.0.2.7",
	}}}
	futureJSON, _ := json.Marshal(future)
	for _, data := range [][]byte{[]byte("{"), []byte(`{"version":3,"entries":[]}`), futureJSON, []byte(strings.Repeat("x", udpStateMaxBytes+1))} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		store := newProxyUDPCheckStore(nil)
		store.enablePersistence(path, now)
		if got := store.coverage([]string{ps.Key()}, now); got.Unchecked != 1 {
			t.Fatalf("invalid input accepted: %+v", got)
		}
	}
}

func okProbe(context.Context, *connect.ProxySettings) proxyUDPResult {
	return proxyUDPResult{Status: proxyUDPOK, TCPExit: "192.0.2.1", UDPExit: "192.0.2.1"}
}

// A write that never returns must not stop the store: checks, flow replies and
// coverage reads all hold the store lock, so none of them may wait for the disk.
func TestUDPPersistWriteNeverBlocksTheStore(t *testing.T) {
	prevWrite, prevDelay := udpPersistWriteFile, udpPersistDelay
	t.Cleanup(func() { udpPersistWriteFile, udpPersistDelay = prevWrite, prevDelay })
	udpPersistDelay = 0
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	udpPersistWriteFile = func(path string, list []udpPersistEntry) ([]string, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil, nil
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	store := newProxyUDPCheckStore(okProbe)
	store.enablePersistence(filepath.Join(t.TempDir(), "state.json"), now)
	first := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
	store.Check(context.Background(), first, now)
	waitSignal(t, entered, "the writer to start its write")

	done := make(chan struct{})
	go func() {
		defer close(done)
		second := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.2:1080"}
		store.Check(context.Background(), second, now)
		store.noteFlowReply(second.Key(), now)
		store.coverage([]string{first.Key(), second.Key()}, now)
	}()
	waitSignal(t, done, "checks and reads while a write is stuck")
	close(release)
	store.flushPersist()
}

// A burst of completed checks costs a few writes, and the last write holds all
// of them.
func TestUDPPersistCoalescesABurstOfResults(t *testing.T) {
	prevWrite, prevDelay := udpPersistWriteFile, udpPersistDelay
	t.Cleanup(func() { udpPersistWriteFile, udpPersistDelay = prevWrite, prevDelay })
	udpPersistDelay = time.Hour // only flushPersist triggers a write
	var writes atomic.Int32
	udpPersistWriteFile = func(path string, list []udpPersistEntry) ([]string, error) {
		writes.Add(1)
		return writeUDPPersistFile(path, list)
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	store := newProxyUDPCheckStore(okProbe)
	store.enablePersistence(path, now)
	const n = 25
	for i := 0; i < n; i++ {
		ps := &connect.ProxySettings{Network: "tcp", Address: fmt.Sprintf("192.0.2.%d:1080", i+1)}
		store.Check(context.Background(), ps, now)
	}
	if got := writes.Load(); got != 0 {
		t.Fatalf("%d writes before the delay ended, want 0", got)
	}
	store.flushPersist()
	if got := writes.Load(); got != 1 {
		t.Fatalf("%d writes for one burst, want 1", got)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state udpPersistFile
	if err := json.Unmarshal(b, &state); err != nil || len(state.Entries) != n {
		t.Fatalf("file holds %d entries (err %v), want %d", len(state.Entries), err, n)
	}
	// nothing left to write, so a second flush writes nothing
	store.flushPersist()
	if got := writes.Load(); got != 1 {
		t.Fatalf("an idle flush wrote again: %d writes", got)
	}
}

// The writer exits when it has nothing to write, so a store that is no longer
// used leaves no goroutine behind.
func TestUDPPersistWriterExitsWhenIdle(t *testing.T) {
	prevDelay := udpPersistDelay
	t.Cleanup(func() { udpPersistDelay = prevDelay })
	udpPersistDelay = 0
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	store := newProxyUDPCheckStore(okProbe)
	store.enablePersistence(filepath.Join(t.TempDir(), "state.json"), now)
	store.Check(context.Background(), &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}, now)
	store.flushPersist()
	store.mu.Lock()
	writing, dirty := store.persistWriting, store.persistDirty
	store.mu.Unlock()
	if writing || dirty {
		t.Fatalf("writer still active after flush: writing=%v dirty=%v", writing, dirty)
	}
}

func TestUDPPersistIPv6AndLegacy(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	key := "test-proxy"
	v6 := proxyIPv6Result{Status: proxyIPv6Yes, Source: "measured", UDP: "yes", TCPExit: "2001:db8::1", UDPExit: "2001:db8::1", At: now, Interval: 7 * 24 * time.Hour}
	store := newProxyUDPCheckStore(nil)
	store.enablePersistence(path, now)
	store.mu.Lock()
	entry := &proxyUDPCheckEntry{at: now, result: proxyUDPResult{Status: proxyUDPOK}, ipv6: v6}
	if err := store.persistLocked(key, entry, now); err != nil {
		t.Fatal(err)
	}
	store.mu.Unlock()
	store.flushPersist()
	loaded := newProxyUDPCheckStore(nil)
	loaded.enablePersistence(path, now.Add(2*24*time.Hour))
	if got := loaded.cachedEntryLocked(key); got == nil || got.ipv6 != v6 {
		t.Fatalf("restored IPv6 = %+v", got)
	}
	legacy, err := json.Marshal(udpPersistFile{Version: 1, Entries: []udpPersistEntry{{Key: udpPersistKey(key), At: now, Status: proxyUDPOK}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	loaded = newProxyUDPCheckStore(nil)
	loaded.enablePersistence(path, now)
	if got := loaded.cachedEntryLocked(key); got == nil || got.ipv6 != (proxyIPv6Result{}) {
		t.Fatalf("legacy IPv6 = %+v", got)
	}
}

// A remembered TCP exit is reuse material, not part of a failed check: after a
// restart a NoRelay result must not report an exit the check never recorded.
func TestUDPPersistRememberedExitDoesNotJoinAFailedCheck(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ps := &connect.ProxySettings{Network: "tcp", Address: "example.test:1234"}
	path := filepath.Join(t.TempDir(), "state.json")
	file := udpPersistFile{Version: 2, Entries: []udpPersistEntry{{
		Key:     udpPersistKey(ps.Key()),
		Status:  proxyUDPNoRelay,
		At:      now,
		TCPExit: "192.0.2.7",
	}}}
	b, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	store := newProxyUDPCheckStore(nil)
	store.enablePersistence(path, now)
	if tcp, udp := store.exitsFor(ps.Key(), now); tcp != "" || udp != "" {
		t.Fatalf("a failed check reported exits after a restart: tcp %q udp %q", tcp, udp)
	}
	// the remembered exit survives for reuse
	store.mu.Lock()
	entry := store.cachedEntryLocked(ps.Key())
	remembered := ""
	if entry != nil {
		remembered = entry.tcpExit
	}
	store.mu.Unlock()
	if remembered != "192.0.2.7" {
		t.Fatalf("remembered exit lost on restore: %q", remembered)
	}
}

// A successful or mismatched check recorded its exits, so a restart keeps them
// visible; only a result that never recorded an exit stays empty.
func TestUDPPersistKeepsTheExitOfARestoredGoodCheck(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ps := &connect.ProxySettings{Network: "tcp", Address: "example.test:1234"}
	path := filepath.Join(t.TempDir(), "state.json")
	file := udpPersistFile{Version: 2, Entries: []udpPersistEntry{{
		Key:     udpPersistKey(ps.Key()),
		Status:  proxyUDPOK,
		At:      now,
		TCPExit: "192.0.2.7",
	}}}
	b, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	store := newProxyUDPCheckStore(nil)
	store.enablePersistence(path, now)
	if tcp, _ := store.exitsFor(ps.Key(), now); tcp != "192.0.2.7" {
		t.Fatalf("a restored successful check lost its tcp exit: %q", tcp)
	}
}
