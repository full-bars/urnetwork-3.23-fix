package main

import (
	"io"
	"net"
	"net/http"
	"testing"
)

func TestProfileListenerFallbackAndStop(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	var p profileListener
	addr, err := p.start(occupied.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer p.stop()
	if addr == occupied.Addr().String() || addr != p.addr() {
		t.Fatalf("unexpected fallback address %q", addr)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		t.Fatalf("not loopback: %q", addr)
	}
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/debug/pprof/goroutine?debug=1", http.StatusOK},
		{"/metrics", http.StatusNotFound},
		{"/metrics/pool", http.StatusNotFound},
		{"/", http.StatusNotFound},
	} {
		resp, err := http.Get("http://" + addr + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: got %d, want %d", tc.path, resp.StatusCode, tc.want)
		}
	}
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
	if p.addr() != "" {
		t.Fatal("address remains after stop")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listener not released: %v", err)
	}
	_ = ln.Close()
	if _, err := p.start(addr); err != nil {
		t.Fatalf("restart: %v", err)
	}
}

func TestProfileListenerRejectsNonLoopback(t *testing.T) {
	for _, addr := range []string{":6060", "0.0.0.0:6060", "localhost:6060", "192.0.2.1:6060", "[::]:6060", "127.0.0.1:-1", "127.0.0.1:65536"} {
		var p profileListener
		if got, err := p.start(addr); err == nil {
			_ = p.stop()
			t.Errorf("accepted %s on %s", addr, got)
		}
	}
}
