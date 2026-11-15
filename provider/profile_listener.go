package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// profileListener owns an isolated, loopback-only pprof HTTP server.
type profileListener struct {
	mu       sync.Mutex
	server   *http.Server
	listener net.Listener
	// wanted is the address the last start asked for, and fellBack says the
	// port was taken so a free one was used instead.
	wanted   string
	fellBack bool
}

// start binds the requested literal loopback address, falling back to a free
// port only when that address is already occupied. It returns the bound address.
func (p *profileListener) start(addr string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.server != nil {
		return "", fmt.Errorf("pprof listener already running")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("pprof address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("pprof address must use a literal loopback IP")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return "", fmt.Errorf("invalid pprof port %q", port)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), port))
	fellBack := false
	if errors.Is(err, syscall.EADDRINUSE) {
		ln, err = net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
		fellBack = true
	}
	if err != nil {
		return "", fmt.Errorf("pprof listen: %w", err)
	}
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok || tcp.IP == nil || !tcp.IP.IsLoopback() {
		_ = ln.Close()
		return "", fmt.Errorf("pprof listener is not loopback")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	p.server, p.listener = srv, ln
	p.wanted, p.fellBack = net.JoinHostPort(ip.String(), port), fellBack
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), nil
}

// stop closes active requests and the listener before allowing a new start.
func (p *profileListener) stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.server == nil {
		return nil
	}
	err := p.server.Close()
	p.server, p.listener = nil, nil
	return err
}

func (p *profileListener) addr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listener == nil {
		return ""
	}
	return p.listener.Addr().String()
}

// usingFallback reports that the last start found its port taken.
func (p *profileListener) usingFallback() (wanted string, fellBack bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.wanted, p.fellBack
}
