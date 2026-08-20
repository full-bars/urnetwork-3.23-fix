package connect

import (
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
)

// EnableProfiling starts a pprof HTTP server on a loopback address.
// It returns an error when addr is not a loopback address, so profiling
// can never be exposed on a network-facing interface by accident.
func EnableProfiling(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("profiling: bad addr %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("profiling: refusing non-loopback bind %q", addr)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("profiling: listen %s: %w", addr, err)
	}
	go func() {
		_ = http.Serve(ln, mux)
	}()
	return nil
}
