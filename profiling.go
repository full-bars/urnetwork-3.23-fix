package connect

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
)

// EnableProfiling starts a loopback-only diagnostics HTTP server. It serves
// pprof (/debug/pprof/*) plus the pool and error metrics endpoints
// (/metrics/pool, /metrics/errors), all on one loopback address. It returns
// an error when addr is not a loopback address, so these diagnostics can
// never be exposed on a network-facing interface by accident.
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
	// Pool and error metrics are diagnostics for operators, so they are served
	// on the same loopback-only listener rather than the public status port.
	mux.HandleFunc("/metrics/pool", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, EnhancedMetrics())
	})
	mux.HandleFunc("/metrics/errors", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, ErrorMetrics())
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("profiling: listen %s: %w", addr, err)
	}
	go func() {
		_ = http.Serve(ln, mux)
	}()
	return nil
}

// writeJSON serializes v as JSON to w with a 200 status, falling back to a
// 500 on serialization failure.
func writeJSON(w http.ResponseWriter, v map[string]any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "metrics serialization failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}
