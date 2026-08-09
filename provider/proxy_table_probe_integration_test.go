package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// listenSocks5Smart starts a TCP listener that answers the SOCKS5 greeting
// and replies to CONNECT with REP 0x00 when the destination IP is in the
// ok map, REP 0x05 otherwise. Used to build a proxy that passes the API
// CONNECT (stage 0) but fails most table targets (stage 1) — the
// below-bar admission case.
func listenSocks5Smart(t *testing.T, okIPs map[string]bool) (addr string, cleanup func()) {
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
				buf := make([]byte, 3)
				if _, err := c.Read(buf); err != nil {
					return
				}
				c.Write([]byte{0x05, 0x00})
				frame := make([]byte, 10)
				if _, err := c.Read(frame); err != nil {
					return
				}
				// ATYP=1 (v4): bytes 3-6 are the destination IP
				rep := byte(0x05)
				if len(frame) == 10 && frame[3] == 0x01 && okIPs[net.IP(frame[4:8]).String()] {
					rep = 0x00
				}
				resp := []byte{0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
				c.Write(resp)
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// TestProbeAndGradeProxyURLLines_QualifiedGetsGrade: a proxy that answers
// every CONNECT with success is qualified and carries its score/failed.
func TestProbeAndGradeProxyURLLines_QualifiedGetsGrade(t *testing.T) {
	apiIP, _ := resolveAPIProbeAddr("api.bringyour.com", 443)
	if apiIP == nil {
		t.Skip("could not resolve api.bringyour.com; DNS required for this test")
	}
	addr, cleanup := listenSocks5ConnectOnce(t, 0x00)
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 4
	cfg.TargetTimeout = time.Second

	lines := []string{addr}
	grades := probeAndGradeProxyURLLines(context.Background(), lines, "api.bringyour.com", 443, cfg)

	g, ok := grades[addr]
	if !ok {
		t.Fatalf("expected a grade for %s, got %v", addr, grades)
	}
	if !g.Qualified {
		t.Fatalf("expected qualified, got %+v", g)
	}
	if g.Score != 1.0 {
		t.Fatalf("expected score 1.0, got %v", g.Score)
	}
	if len(g.Failed) != 0 {
		t.Fatalf("expected no failed targets, got %v", g.Failed)
	}
}

// TestProbeAndGradeProxyURLLines_DeadDropped: a dead address gets no grade
// entry at all (dropped before cache).
func TestProbeAndGradeProxyURLLines_DeadDropped(t *testing.T) {
	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 4
	cfg.TargetTimeout = 200 * time.Millisecond

	dead := closedPortAddr(t)
	grades := probeAndGradeProxyURLLines(context.Background(), []string{dead}, "api.bringyour.com", 443, cfg)
	if _, ok := grades[dead]; ok {
		t.Fatalf("expected dead address to be dropped, got grade %+v", grades[dead])
	}
}

// TestProbeAndGradeProxyURLLines_Socks5OnlyFlagged: an address that speaks
// SOCKS5 but refuses the API CONNECT is flagged Socks5Only, never admitted.
func TestProbeAndGradeProxyURLLines_Socks5OnlyFlagged(t *testing.T) {
	addr, cleanup := listenSocks5ConnectOnce(t, 0x05) // every CONNECT refused
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 4
	cfg.TargetTimeout = time.Second

	grades := probeAndGradeProxyURLLines(context.Background(), []string{addr}, "api.bringyour.com", 443, cfg)
	g, ok := grades[addr]
	if !ok {
		t.Fatalf("expected a grade for %s", addr)
	}
	if !g.Socks5Only {
		t.Fatalf("expected Socks5Only flag, got %+v", g)
	}
	if g.Qualified {
		t.Fatalf("socks5-only must never be qualified, got %+v", g)
	}
}

// TestProbeAndGradeProxyURLLines_BelowBarNotQualified: a proxy that passes
// the API CONNECT but fails nearly all table targets is not qualified —
// it carries a score below the bar and no auth admission.
func TestProbeAndGradeProxyURLLines_BelowBarNotQualified(t *testing.T) {
	// The API CONNECT destination resolves to an address the fake will
	// accept; every table target (the sampled hosts) resolves elsewhere and
	// is refused. To make stage 0 pass we must accept the actual resolved
	// API IP, so resolve it first (same cache the probe uses).
	apiIP, _ := resolveAPIProbeAddr("api.bringyour.com", 443)
	if apiIP == nil {
		t.Skip("could not resolve api.bringyour.com; DNS required for this test")
	}
	addr, cleanup := listenSocks5Smart(t, map[string]bool{apiIP.String(): true})
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 4
	cfg.TargetTimeout = time.Second

	grades := probeAndGradeProxyURLLines(context.Background(), []string{addr}, "api.bringyour.com", 443, cfg)
	g, ok := grades[addr]
	if !ok {
		t.Fatalf("expected a grade for %s", addr)
	}
	if g.Qualified {
		t.Fatalf("below-bar proxy must not be qualified, got %+v", g)
	}
	if g.Score >= cfg.PassBar {
		t.Fatalf("expected score below bar %v, got %v", cfg.PassBar, g.Score)
	}
	if g.Socks5Only {
		t.Fatalf("below-bar is not socks5-only (it passed the API CONNECT), got %+v", g)
	}
}

// TestResolveProxyTableProbeConfig_OverrideFile: an operator-written
// ~/.urnetwork/proxy_probe.json changes the effective config; a malformed
// file falls back to defaults.
func TestResolveProxyTableProbeConfig_OverrideFile(t *testing.T) {
	home := withTempHome(t)
	path := filepath.Join(home, ".urnetwork", "proxy_probe.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}

	good := map[string]any{"sample_width": 20, "timeout_ms": 2000, "pass_bar": 0.5, "preferred_bar": 0.85}
	b, _ := json.Marshal(good)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	resetProbeConfigCache()
	cfg := resolveProxyTableProbeConfig()
	if cfg.SampleWidth != 20 || cfg.TargetTimeout != 2*time.Second || cfg.PassBar != 0.5 || cfg.PreferredBar != 0.85 {
		t.Fatalf("override not applied: %+v", cfg)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	resetProbeConfigCache()
	cfg = resolveProxyTableProbeConfig()
	def := defaultProxyTableProbeConfig()
	if cfg.SampleWidth != def.SampleWidth || cfg.PassBar != def.PassBar {
		t.Fatalf("malformed override must fall back to defaults, got %+v", cfg)
	}
}

// TestFetchAndMergeProxyURLs_GradedPersistence: the fetch pipeline persists
// the stage-1 score/failed into proxy_url.json and only admits qualified
// proxies with ProbeOK=true.
func TestFetchAndMergeProxyURLs_GradedPersistence(t *testing.T) {
	withTempHome(t)

	apiIP, _ := resolveAPIProbeAddr("api.bringyour.com", 443)
	if apiIP == nil {
		t.Skip("could not resolve api.bringyour.com; DNS required for this test")
	}

	// One proxy that passes everything (qualified), one dead (dropped).
	goodAddr, cleanup := listenSocks5ConnectOnce(t, 0x00)
	defer cleanup()
	deadAddr := closedPortAddr(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(goodAddr + "\n" + deadAddr + "\n"))
	}))
	defer srv.Close()

	// Point the probe config at a small fast sample so the test stays quick.
	home := os.Getenv("HOME")
	probePath := filepath.Join(home, ".urnetwork", "proxy_probe.json")
	os.MkdirAll(filepath.Dir(probePath), 0700)
	b, _ := json.Marshal(map[string]any{"sample_width": 4, "timeout_ms": 500})
	os.WriteFile(probePath, b, 0600)
	defer os.Remove(probePath)
	resetProbeConfigCache()

	fetchAndMergeProxyURLs(context.Background(), []string{srv.URL}, 100, "api.bringyour.com", 443)

	state, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	good, ok := state.Cache[goodAddr]
	if !ok {
		t.Fatalf("expected qualified proxy in cache, got %v", state.Cache)
	}
	if !good.ProbeOK {
		t.Errorf("qualified proxy must have ProbeOK=true, got %+v", good)
	}
	if good.Score < 0.6 {
		t.Errorf("expected score >= pass bar, got %v", good.Score)
	}
	if _, ok := state.Cache[deadAddr]; ok {
		t.Errorf("dead proxy must not be cached, got %v", state.Cache[deadAddr])
	}
}

// TestFetchAndMergeProxyURLs_AllBelowBarNothingAdmitted: when every fetched
// proxy is below the stage-1 bar, nothing gets ProbeOK=true — the auth-time
// gate then blocks them all.
func TestFetchAndMergeProxyURLs_AllBelowBarNothingAdmitted(t *testing.T) {
	withTempHome(t)

	apiIP, _ := resolveAPIProbeAddr("api.bringyour.com", 443)
	if apiIP == nil {
		t.Skip("could not resolve api.bringyour.com; DNS required for this test")
	}
	smartAddr, cleanup := listenSocks5Smart(t, map[string]bool{apiIP.String(): true})
	defer cleanup()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(smartAddr + "\n"))
	}))
	defer srv.Close()

	home := os.Getenv("HOME")
	probePath := filepath.Join(home, ".urnetwork", "proxy_probe.json")
	os.MkdirAll(filepath.Dir(probePath), 0700)
	b, _ := json.Marshal(map[string]any{"sample_width": 4, "timeout_ms": 500})
	os.WriteFile(probePath, b, 0600)
	defer os.Remove(probePath)
	resetProbeConfigCache()

	fetchAndMergeProxyURLs(context.Background(), []string{srv.URL}, 100, "api.bringyour.com", 443)

	state, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := state.Cache[smartAddr]
	if !ok {
		t.Fatalf("below-bar entry should still be cached for the reaper, got %v", state.Cache)
	}
	if entry.ProbeOK {
		t.Errorf("below-bar proxy must not be ProbeOK, got %+v", entry)
	}
	if entry.Score >= 0.6 {
		t.Errorf("expected below-bar score, got %v", entry.Score)
	}
}
