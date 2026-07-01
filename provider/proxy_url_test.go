package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestWriteReadProxyURLState_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy_url.json")

	s := &ProxyURLState{
		Sources: []string{"https://example.com/list.txt"},
		Cache: map[string]ProxyURLEntry{
			"1.2.3.4:1080": {},
			"5.6.7.8:1080": {User: "u", Password: "p"},
		},
	}

	if err := writeProxyURLStateTo(path, s); err != nil {
		t.Fatal(err)
	}

	got, err := readProxyURLStateFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sources) != 1 || got.Sources[0] != "https://example.com/list.txt" {
		t.Errorf("sources: got %v", got.Sources)
	}
	if len(got.Cache) != 2 {
		t.Errorf("cache: got %d entries, want 2", len(got.Cache))
	}
	if got.Cache["5.6.7.8:1080"].User != "u" {
		t.Errorf("cache entry user: got %q, want %q", got.Cache["5.6.7.8:1080"].User, "u")
	}
}

func TestReadProxyURLState_NotExist(t *testing.T) {
	s, err := readProxyURLStateFrom("/tmp/does-not-exist-proxy_url.json")
	if err != nil {
		t.Fatal(err)
	}
	if s.Cache == nil {
		t.Fatal("expected non-nil Cache map")
	}
}

func TestParseProxyURLLine(t *testing.T) {
	tests := []struct {
		name         string
		line         string
		wantAddr     string
		wantUser     string
		wantPassword string
		wantOK       bool
	}{
		{"plain host:port", "1.2.3.4:1080", "1.2.3.4:1080", "", "", true},
		{"host:port:user:pass", "1.2.3.4:1080:myuser:mypass", "1.2.3.4:1080", "myuser", "mypass", true},
		{"socks5 no creds", "socks5://1.2.3.4:1080", "1.2.3.4:1080", "", "", true},
		{"socks5 with creds", "socks5://myuser:mypass@1.2.3.4:1080", "1.2.3.4:1080", "myuser", "mypass", true},
		{"SOCKS5 case-insensitive scheme", "SOCKS5://1.2.3.4:1080", "1.2.3.4:1080", "", "", true},
		{"blank line", "", "", "", "", false},
		{"comment line", "# 1.2.3.4:1080", "", "", "", false},
		{"unsupported scheme", "http://1.2.3.4:1080", "", "", "", false},
		{"whitespace padded", "  1.2.3.4:1080  ", "1.2.3.4:1080", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, user, password, ok := parseProxyURLLine(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("ok: got %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if addr != tt.wantAddr {
				t.Errorf("address: got %q, want %q", addr, tt.wantAddr)
			}
			if user != tt.wantUser {
				t.Errorf("user: got %q, want %q", user, tt.wantUser)
			}
			if password != tt.wantPassword {
				t.Errorf("password: got %q, want %q", password, tt.wantPassword)
			}
		})
	}
}

func TestFetchProxyURLLines_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("1.2.3.4:1080\n5.6.7.8:1080\n"))
	}))
	defer srv.Close()

	lines, err := fetchProxyURLLines(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != "1.2.3.4:1080" || lines[1] != "5.6.7.8:1080" {
		t.Fatalf("got %v", lines)
	}
}

func TestFetchProxyURLLines_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := fetchProxyURLLines(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestFetchProxyURLLines_EmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	_, err := fetchProxyURLLines(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for empty body")
	}
}

func TestFetchProxyURLLines_BodyTruncatedAtLimit(t *testing.T) {
	huge := strings.Repeat("a", maxProxyURLFetchBytes+1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(huge))
	}))
	defer srv.Close()

	lines, err := fetchProxyURLLines(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, l := range lines {
		total += len(l)
	}
	if total > maxProxyURLFetchBytes {
		t.Fatalf("body not truncated: got %d bytes, want <= %d", total, maxProxyURLFetchBytes)
	}
}

func TestMergeProxyURLEntries_AddsNewSkipsExisting(t *testing.T) {
	state := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.2.3.4:1080": {},
	}}
	added := mergeProxyURLEntries(state, []string{
		"1.2.3.4:1080", // already present, not re-added
		"5.6.7.8:1080:user:pass",
		"# comment, skipped",
	}, 0)
	if added != 1 {
		t.Fatalf("added: got %d, want 1", added)
	}
	if len(state.Cache) != 2 {
		t.Fatalf("cache size: got %d, want 2", len(state.Cache))
	}
	if state.Cache["5.6.7.8:1080"].User != "user" {
		t.Errorf("expected creds preserved, got %+v", state.Cache["5.6.7.8:1080"])
	}
}

func TestMergeProxyURLEntries_RespectsMaxTotal(t *testing.T) {
	state := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.2.3.4:1080": {},
	}}
	added := mergeProxyURLEntries(state, []string{
		"5.6.7.8:1080",
		"9.9.9.9:1080",
	}, 2)
	if added != 1 {
		t.Fatalf("added: got %d, want 1 (cap of 2 total, 1 already present)", added)
	}
	if len(state.Cache) != 2 {
		t.Fatalf("cache size: got %d, want 2", len(state.Cache))
	}
}

func TestMergeProxyURLCache_PrimarySourceWins(t *testing.T) {
	desiredSet := map[string]*connect.ProxySettings{
		"1.2.3.4:1080": {Network: "tcp", Address: "1.2.3.4:1080"},
	}
	sourceOf := map[string]string{"1.2.3.4:1080": "file"}
	urlState := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.2.3.4:1080": {User: "should-be-ignored"},
		"5.6.7.8:1080": {User: "u", Password: "p"},
	}}

	mergeProxyURLCache(desiredSet, sourceOf, urlState)

	if sourceOf["1.2.3.4:1080"] != "file" {
		t.Errorf("existing entry's source was overwritten: got %q", sourceOf["1.2.3.4:1080"])
	}
	if sourceOf["5.6.7.8:1080"] != "url" {
		t.Errorf("new entry not tagged url: got %q", sourceOf["5.6.7.8:1080"])
	}
	settings, ok := desiredSet["5.6.7.8:1080"]
	if !ok {
		t.Fatal("expected new address merged into desiredSet")
	}
	if settings.Auth == nil || settings.Auth.User != "u" || settings.Auth.Password != "p" {
		t.Errorf("expected auth u/p, got %+v", settings.Auth)
	}
}

func TestWriteReadProxyURLState_RoundTrip_Blacklist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy_url.json")

	ts := time.Now().UTC().Truncate(time.Second)
	s := &ProxyURLState{
		Cache:     map[string]ProxyURLEntry{},
		Blacklist: map[string]time.Time{"1.2.3.4:1080": ts},
	}
	if err := writeProxyURLStateTo(path, s); err != nil {
		t.Fatal(err)
	}

	got, err := readProxyURLStateFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	gotTs, ok := got.Blacklist["1.2.3.4:1080"]
	if !ok {
		t.Fatal("expected blacklist entry to round-trip")
	}
	if !gotTs.Equal(ts) {
		t.Errorf("blacklist timestamp: got %v, want %v", gotTs, ts)
	}
}

func TestMergeProxyURLEntries_SkipsBlacklisted(t *testing.T) {
	state := &ProxyURLState{
		Cache:     map[string]ProxyURLEntry{},
		Blacklist: map[string]time.Time{"1.2.3.4:1080": time.Now()},
	}
	added := mergeProxyURLEntries(state, []string{
		"1.2.3.4:1080", // blacklisted, must be skipped even though not yet cached
		"5.6.7.8:1080",
	}, 0)
	if added != 1 {
		t.Fatalf("added: got %d, want 1", added)
	}
	if _, ok := state.Cache["1.2.3.4:1080"]; ok {
		t.Error("blacklisted address must not be added to cache")
	}
	if _, ok := state.Cache["5.6.7.8:1080"]; !ok {
		t.Error("non-blacklisted address should still be added")
	}
}

func TestMergeProxyURLCache_NilStateIsNoop(t *testing.T) {
	desiredSet := map[string]*connect.ProxySettings{}
	sourceOf := map[string]string{}
	mergeProxyURLCache(desiredSet, sourceOf, nil)
	if len(desiredSet) != 0 {
		t.Fatalf("expected no-op, got %v", desiredSet)
	}
}

func TestMergeProxyURLEntriesSkipsExcludePatterns(t *testing.T) {
	state := &ProxyURLState{
		Cache:           map[string]ProxyURLEntry{},
		ExcludePatterns: []string{"dc.decodo.com"},
	}
	lines := []string{
		"dc.decodo.com:8001",
		"DC.DECODO.COM:8002:user:pass",
		"gate.smartproxy.com:7000",
	}
	added := mergeProxyURLEntries(state, lines, 0)
	if added != 1 {
		t.Fatalf("added = %d, want 1 (only the non-excluded proxy)", added)
	}
	if _, ok := state.Cache["gate.smartproxy.com:7000"]; !ok {
		t.Errorf("expected gate.smartproxy.com:7000 in cache")
	}
	for addr := range state.Cache {
		if matchProxyHost("dc.decodo.com", addr) {
			t.Errorf("excluded address %q was cached", addr)
		}
	}
}

func TestAddExcludePattern(t *testing.T) {
	state := &ProxyURLState{}
	if !addExcludePattern(state, "dc.decodo.com") {
		t.Fatal("first add should return true")
	}
	if addExcludePattern(state, "DC.Decodo.Com") {
		t.Fatal("case-insensitive duplicate should return false")
	}
	if len(state.ExcludePatterns) != 1 {
		t.Fatalf("ExcludePatterns = %v, want 1 entry", state.ExcludePatterns)
	}
}
