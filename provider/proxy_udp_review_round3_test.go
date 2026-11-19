package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A check that started earlier can finish after one that started later. The
// later result must stay in the state file.
func TestUDPPersistNowNeverMovesBackwards(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	store := newProxyUDPCheckStore(nil)
	store.enablePersistence(path, base)
	late := &proxyUDPCheckEntry{at: base.Add(10 * time.Second), result: proxyUDPResult{Status: proxyUDPOK, UDPExit: "192.0.2.1"}}
	early := &proxyUDPCheckEntry{at: base.Add(5 * time.Second), result: proxyUDPResult{Status: proxyUDPOK, UDPExit: "192.0.2.2"}}
	store.mu.Lock()
	store.persistLocked("late", late, base.Add(10*time.Second))
	// the check that started first finishes second
	store.persistLocked("early", early, base.Add(5*time.Second))
	if !store.persistNow.Equal(base.Add(10 * time.Second)) {
		t.Fatalf("persistNow moved back to %v", store.persistNow)
	}
	store.mu.Unlock()
	store.flushPersist()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f udpPersistFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Entries) != 2 {
		t.Fatalf("expected both results in the file, got %d", len(f.Entries))
	}
}

// The byte limit cuts at the last entry that fits and the file stays valid.
func TestUDPPersistByteLimitCutsAtLastFittingEntry(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	list := make([]udpPersistEntry, 0, udpStateMaxEntries)
	for i := 0; i < udpStateMaxEntries; i++ {
		list = append(list, udpPersistEntry{
			Key: fmt.Sprintf("%064x", i), Status: proxyUDPOK, At: base.Add(-time.Duration(i) * time.Second),
			TCPExit: "2001:db8:ffff:ffff:ffff:ffff:ffff:ffff", UDPExit: "2001:db8:ffff:ffff:ffff:ffff:ffff:fffe",
		})
	}
	path := filepath.Join(t.TempDir(), "state.json")
	dropped, err := writeUDPPersistFile(path, list)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > udpStateMaxBytes {
		t.Fatalf("file is %d bytes, over the limit", len(b))
	}
	var f udpPersistFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Entries)+len(dropped) != udpStateMaxEntries || len(dropped) == 0 {
		t.Fatalf("kept %d dropped %d", len(f.Entries), len(dropped))
	}
	// one more entry must not have fitted
	extra, _ := json.Marshal(udpPersistFile{Version: 1, Entries: append(append([]udpPersistEntry{}, f.Entries...), list[len(f.Entries)])})
	if len(extra) <= udpStateMaxBytes {
		t.Fatalf("an entry that fits was dropped")
	}
	// newest first: the kept entries are the newest ones
	if f.Entries[0].Key != list[0].Key {
		t.Fatalf("newest entry missing")
	}
}

func TestUDPPersistByteLimitStaysLinear(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	list := make([]udpPersistEntry, 0, udpStateMaxEntries)
	for i := 0; i < udpStateMaxEntries; i++ {
		list = append(list, udpPersistEntry{Key: fmt.Sprintf("%064x", i), Status: proxyUDPOK, At: base.Add(-time.Duration(i) * time.Second),
			TCPExit: "2001:db8:ffff:ffff:ffff:ffff:ffff:ffff", UDPExit: "2001:db8:ffff:ffff:ffff:ffff:ffff:fffe"})
	}
	start := time.Now()
	if _, err := writeUDPPersistFile(filepath.Join(t.TempDir(), "state.json"), list); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("a write with a full list took %v", took)
	}
}

func TestClassifyProxySourceLineKeepsIPv6Zone(t *testing.T) {
	got, reason := classifyProxySourceLine("[fe80::1%eth0]:1080:alice:secret")
	if reason != "" || got == nil || got.Address != "[fe80::1%eth0]:1080" {
		t.Fatalf("zone entry: %+v %q", got, reason)
	}
	if got, reason := classifyProxySourceLine("[2001:db8::1]:1080:alice:secret"); reason != "" || got.Address != "[2001:db8::1]:1080" {
		t.Fatalf("plain bracketed IPv6: %+v %q", got, reason)
	}
	for _, bad := range []string{"[1.2.3.4]:1080:a:b", "[fe80::1%]:1080:a:b", "[not-an-ip]:1080:a:b"} {
		if _, reason := classifyProxySourceLine(bad); reason != "invalid_address" {
			t.Fatalf("%q: reason %q", bad, reason)
		}
	}
}

// The store decides to reuse a TCP exit before the probe waits for a slot. If
// the observation is too old by the time the probe relies on it, the probe must
// look the exit up again.
func TestReusableCachedTCPExitHonoursTheBudget(t *testing.T) {
	base := context.WithValue(context.Background(), cachedProxyTCPExitKey{}, "203.0.113.77")
	if got := reusableCachedTCPExit(base); got != "203.0.113.77" {
		t.Fatalf("without a budget the cached exit is kept, got %q", got)
	}
	fresh := context.WithValue(base, cachedProxyTCPExitReuseKey{}, tcpExitReuse{start: time.Now(), budget: time.Hour})
	if got := reusableCachedTCPExit(fresh); got != "203.0.113.77" {
		t.Fatalf("fresh budget: %q", got)
	}
	expired := context.WithValue(base, cachedProxyTCPExitReuseKey{}, tcpExitReuse{start: time.Now().Add(-time.Hour), budget: time.Minute})
	if got := reusableCachedTCPExit(expired); got != "" {
		t.Fatalf("expired budget must force a new lookup, got %q", got)
	}
	if got := reusableCachedTCPExit(context.Background()); got != "" {
		t.Fatalf("nothing cached: %q", got)
	}
}
