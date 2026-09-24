package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Removal receives proxy.state KEYS (identity: address, or address+user for a
// credentialed proxy). It must remove exactly the named identity, never every
// account that shares its gateway address, and must never silently do nothing.

func TestRemoveDeadProxies_FileRemovesOnlyTheNamedAccountAtSharedGateway(t *testing.T) {
	home := withTempHome(t)
	src := filepath.Join(home, "proxy.txt")
	if err := os.WriteFile(src, []byte("gw.example:1080:u1:p1\ngw.example:1080:u2:p2\nother.example:1080:u1:p1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	state := &ProxyState{Source: src}

	err := removeDeadProxies(state, map[string][]string{"file": {identityKey("gw.example:1080", "u1")}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(src)
	want := "gw.example:1080:u2:p2\nother.example:1080:u1:p1\n"
	if string(b) != want {
		t.Fatalf("file after removal = %q, want %q", b, want)
	}
}

func TestRemoveDeadProxies_InternalRemovesOnlyTheNamedAccount(t *testing.T) {
	withTempHome(t)
	writeProxyConfig(&ProxyConfig{
		Servers: map[string]string{
			"gw.example:1080:u1:p1": "",
			"gw.example:1080:u2:p2": "",
			"gw.example:1081":       "ref3", // credentials live in Auths
		},
		Auths: map[string]*ProxyAuth{"ref3": {User: "u3", Password: "p3"}},
	})
	state := &ProxyState{}

	err := removeDeadProxies(state, map[string][]string{"internal": {
		identityKey("gw.example:1080", "u1"),
		identityKey("gw.example:1081", "u3"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := readProxyConfig()
	if len(cfg.Servers) != 1 {
		t.Fatalf("want only the u2 account left, got %v", cfg.Servers)
	}
	if _, ok := cfg.Servers["gw.example:1080:u2:p2"]; !ok {
		t.Fatalf("the other account at the shared gateway must survive, got %v", cfg.Servers)
	}
}

// The URL cache is keyed by bare address, so an identity key removes the cache
// entry through its address part.
func TestRemoveDeadProxies_URLIdentityKeyRemovesCacheEntry(t *testing.T) {
	withTempHome(t)
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"4.4.4.4:1080": {},
		"5.5.5.5:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}
	err := removeDeadProxies(&ProxyState{}, map[string][]string{"url": {identityKey("4.4.4.4:1080", "u")}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Cache["4.4.4.4:1080"]; ok {
		t.Fatalf("credentialed URL proxy still cached after removal: %v", got.Cache)
	}
	if _, ok := got.Cache["5.5.5.5:1080"]; !ok {
		t.Fatalf("unrelated URL proxy must survive: %v", got.Cache)
	}
}

// Operator-facing output must never carry the raw \x1f separator.
func TestFormatRemovedProxyLine_NoRawKeySeparator(t *testing.T) {
	rp := removedProxy{addr: identityKey("gw.example:1080", "alice-long-user"), entry: ProxyEntry{ID: 7, Health: "dead", AuthFailures: 3}}
	line := formatRemovedProxyLine(rp)
	if strings.Contains(line, "\x1f") {
		t.Fatalf("leaked the raw key separator: %q", line)
	}
	if !strings.Contains(line, "gw.example:1080") || !strings.Contains(line, "proxy[7]") || !strings.Contains(line, "auth_errors=3") {
		t.Fatalf("line lost its address, id or auth error count: %q", line)
	}
}
