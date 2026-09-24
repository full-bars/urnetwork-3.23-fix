package main

import (
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Adopting saved logins must cost ONE rewrite of the store file however many
// move. Every flush reads, re-encodes and fsyncs the whole file, so a node with
// thousands of saved logins spent minutes at startup, with no proxy launching,
// when adoption flushed once per login.
func TestAdoptLegacy_OneFlushForManyLogins(t *testing.T) {
	s := newJWTStoreForTest(t)
	const n = 500

	var desired []*connect.ProxySettings
	seed := map[string]clientJWTEntry{}
	for i := 0; i < n; i++ {
		addr := "10.1." + strconv.Itoa(i/250) + "." + strconv.Itoa(i%250) + ":1080"
		seed[addr] = entryWithClient("c" + strconv.Itoa(i))
		desired = append(desired, credSettings(addr, "user"))
	}
	s.mu.Lock()
	s.loadLocked()
	if err := s.flushBatchLocked(seed, nil); err != nil {
		t.Fatal(err)
	}
	s.flushes = 0
	s.mu.Unlock()

	start := time.Now()
	adopted, split := s.AdoptLegacy(desired)
	t.Logf("adopted %d logins in %v", adopted, time.Since(start))

	if adopted != n || split != 0 {
		t.Fatalf("adopted=%d split=%d, want %d and 0", adopted, split, n)
	}
	if s.flushes != 1 {
		t.Fatalf("adoption flushed the store %d times, want exactly 1", s.flushes)
	}

	// The single flush must have persisted everything: a fresh store on the same
	// file sees identity keys only, with the original logins.
	reloaded := newClientJWTStore(s.path)
	for i, d := range desired {
		got, ok := reloaded.Get(d.Key())
		if !ok || got.ClientID != "c"+strconv.Itoa(i) {
			t.Fatalf("identity key %q not persisted with its login: ok=%v got=%q", d.Key(), ok, got.ClientID)
		}
		if _, stale := reloaded.Get(d.Address); stale {
			t.Fatalf("legacy bare-address slot %q survived adoption", d.Address)
		}
	}

	// Idempotent: a second run has nothing to move and must not rewrite the file.
	if a, _ := s.AdoptLegacy(desired); a != 0 || s.flushes != 1 {
		t.Fatalf("second adoption moved %d logins and flushed %d times total, want 0 and still 1", a, s.flushes)
	}
}

// Startup at fleet scale: a node with ~12,800 saved logins (a ~13 MB store)
// must finish adoption in well under a second of work, not minutes. This is the
// size that stalled a production provider with zero proxies up. The bound is
// generous so a slow CI disk cannot flake it, and far below the per-login-flush
// cost (about 2s each), which would take hours here.
func TestAdoptLegacy_FleetScaleStoreStartsFast(t *testing.T) {
	s := newJWTStoreForTest(t)
	const n = 12800
	jwt := make([]byte, 900) // a realistic JWT is ~1 KB, so the file is ~13 MB
	for i := range jwt {
		jwt[i] = 'a' + byte(i%26)
	}

	var desired []*connect.ProxySettings
	seed := map[string]clientJWTEntry{}
	for i := 0; i < n; i++ {
		addr := "10." + strconv.Itoa(i/65025) + "." + strconv.Itoa((i/255)%255) + "." + strconv.Itoa(i%255) + ":1080"
		e := entryWithClient("c" + strconv.Itoa(i))
		e.ByClientJWT = string(jwt)
		seed[addr] = e
		desired = append(desired, credSettings(addr, "user"))
	}
	s.mu.Lock()
	s.loadLocked()
	if err := s.flushBatchLocked(seed, nil); err != nil {
		t.Fatal(err)
	}
	s.flushes = 0
	s.mu.Unlock()

	start := time.Now()
	adopted, _ := s.AdoptLegacy(desired)
	elapsed := time.Since(start)
	t.Logf("adopted %d logins from a %d-entry store in %v", adopted, n, elapsed)

	if adopted != n {
		t.Fatalf("adopted %d of %d logins", adopted, n)
	}
	if s.flushes != 1 {
		t.Fatalf("adoption flushed the store %d times, want 1", s.flushes)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("adopting %d logins took %v; startup must not stall proxy launch", n, elapsed)
	}
}
