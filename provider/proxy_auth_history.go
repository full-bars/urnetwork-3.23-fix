package main

import "sync"

// provenProxySet tracks, for the lifetime of this process, which proxy
// addresses have completed at least one successful auth. A proxy that has
// never succeeded and then times out is at least as likely to be a broken
// local hop — an open port with a non-functional SOCKS5 service behind it,
// which the TCP-only reachability probe in proxy_probe.go can't detect — as
// it is a sign the API itself is overloaded. Gating which timeouts are
// allowed to feed the shared auth rate limiter on this history keeps a list
// full of "port open, proxy broken" entries from throttling every other
// proxy the same way a list full of dead ports used to.
type provenProxySet struct {
	mu     sync.Mutex
	proven map[string]bool
}

var globalProvenProxies = &provenProxySet{proven: map[string]bool{}}

// MarkSucceeded records that address has completed at least one successful
// auth this run.
func (s *provenProxySet) MarkSucceeded(address string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proven[address] = true
}

// HasSucceeded reports whether address has ever completed a successful auth
// this run.
func (s *provenProxySet) HasSucceeded(address string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proven[address]
}

// Prune removes entries for addresses not in keepAddrs, called periodically
// to prevent unbounded growth from proxies that cycled out of the fleet.
func (s *provenProxySet) Prune(keepAddrs map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for addr := range s.proven {
		if !keepAddrs[addr] {
			delete(s.proven, addr)
		}
	}
}
