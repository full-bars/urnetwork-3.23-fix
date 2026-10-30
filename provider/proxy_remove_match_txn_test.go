package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveMatchAbortsOnUnreadableURLState(t *testing.T) {
	withTempHome(t)
	addr := "203.0.113.7:1080"
	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{addr: {}}}); err != nil {
		t.Fatal(err)
	}
	path, err := proxyURLStatePath()
	if err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"exclude_patterns":["old.example"],`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := removeMatchingProxies("203.0.113.7"); err == nil {
		t.Error("expected unreadable URL state to abort removal")
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Errorf("URL state changed: %q, err=%v", got, err)
	}
	state, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Proxies[addr]; !ok {
		t.Error("matching proxy removed despite unreadable URL state")
	}
}

// If the removal fails part way, the exclusion is already on disk so no fetch
// can re-add the matches before the operator reruns the command.
func TestRemoveMatchPersistsTheExclusionBeforeRemoving(t *testing.T) {
	withTempHome(t)

	missing := filepath.Join(t.TempDir(), "gone", "proxies.txt")
	if err := writeProxyState(&ProxyState{
		Source:  missing,
		Proxies: map[string]ProxyEntry{"203.0.113.7:1080": {Source: "file"}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := removeMatchingProxies("203.0.113.7"); err == nil {
		t.Fatal("expected the unreadable file source to fail the removal")
	}
	got, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ExcludePatterns) != 1 || got.ExcludePatterns[0] != "203.0.113.7" {
		t.Fatalf("exclude patterns = %v, want the pattern persisted before the failed removal", got.ExcludePatterns)
	}
}

// A merge that lands while afterMatchRemovalHook runs sees the exclusion already
// persisted and cannot merge a matching proxy back in.
func TestRemoveMatchHoldsOutAURLMergeBetweenRemovalAndExclusion(t *testing.T) {
	withTempHome(t)

	addr := "198.51.100.1:1080"
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{addr: {}}}); err != nil {
		t.Fatal(err)
	}

	afterMatchRemovalHook = func() {
		// Simulate a concurrent merge attempt while the transaction is mid-flight
		urlState, err := readProxyURLState()
		if err != nil {
			return
		}
		// If the exclusion is already written, matching addresses are rejected
		if !hostMatchesAny(urlState.ExcludePatterns, addr) {
			urlState.Cache[addr] = ProxyURLEntry{}
			writeProxyURLState(urlState)
		}
	}
	defer func() { afterMatchRemovalHook = nil }()

	if _, err := removeMatchingProxies("198.51.100.1"); err != nil {
		t.Fatal(err)
	}
	got, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if _, back := got.Cache[addr]; back {
		t.Fatalf("a URL merge during remove --match restored %s", addr)
	}
	if len(got.ExcludePatterns) != 1 {
		t.Fatalf("exclude patterns = %v, want the pattern persisted", got.ExcludePatterns)
	}
}
