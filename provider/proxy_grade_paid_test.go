package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// Tests for the paid/file-list proxy grading sweep (design note 2026-08-09):
// non-URL proxies are graded with the same stage-1 table probe, read-only
// with respect to the proxy lifecycle — grades persist into proxy.state
// (Score/Graded/Failed/LastGraded) and never gate, evict, or give up on
// anything.

// writePaidGradeProbeOverride sets the probe config for paid-grading tests.
func writePaidGradeProbeOverride(t *testing.T, enabled bool) {
	t.Helper()
	writeReviewProbeOverride(t, map[string]any{"enabled": enabled, "sample_width": 4, "timeout_ms": 500})
}

// TestPaidProxyGrader_GradesFileProxy is the positive path: a credentialed
// file proxy gets table-probed once (sample_width table CONNECTs — the
// sweep calls probeTableThroughProxy directly, no separate stage-0 API
// CONNECT), the decidable grade is persisted, and the proxy lifecycle
// fields are untouched.
func TestPaidProxyGrader_GradesFileProxy(t *testing.T) {
	home := withTempHome(t)
	writePaidGradeProbeOverride(t, true)

	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()
	seedProbeDNSForAddress(t, addr, tableProbePassCounter.Load())

	src := filepath.Join(home, "paid.txt")
	if err := os.WriteFile(src, []byte(addr+":u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Pre-existing tracker entry with lifecycle fields the sweep must not touch.
	if err := writeProxyState(&ProxyState{
		Source: src,
		Proxies: map[string]ProxyEntry{
			addr: {ID: 7, Health: "up", Source: "file", AuthFailures: 3},
		},
	}); err != nil {
		t.Fatal(err)
	}

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	// The paid sweep runs a stage-0 backend-reachability pass (probeProxy:
	// SOCKS5 + API CONNECT through the proxy) THEN the table probe. Both dial
	// the fake proxy, so the count is sample_width table CONNECTs + 1 stage-0
	// dial = 5.
	if n := connects.Load(); n != 5 {
		t.Fatalf("expected 5 CONNECTs (4 table + 1 stage-0), got %d", n)
	}
	state, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := state.Proxies[addr]
	if !ok {
		t.Fatal("entry must remain in proxy.state")
	}
	if !e.Graded || e.Score != 1.0 {
		t.Errorf("expected grade 1.0 persisted, got graded=%v score=%v", e.Graded, e.Score)
	}
	if !e.LastGraded.After(time.Now().Add(-time.Minute)) {
		t.Errorf("LastGraded %v not advanced", e.LastGraded)
	}
	// Lifecycle fields untouched: the sweep is read-only w.r.t. the proxy.
	if e.ID != 7 || e.Health != "up" || e.Source != "file" || e.AuthFailures != 3 {
		t.Errorf("lifecycle fields clobbered: %+v", e)
	}
}

// TestPaidProxyGrader_InternalConfig covers the no-source-file path: the
// internal config (Servers map) is the desired set, and its proxies get
// graded the same way.
func TestPaidProxyGrader_InternalConfig(t *testing.T) {
	withTempHome(t)
	writePaidGradeProbeOverride(t, true)

	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()
	seedProbeDNSForAddress(t, addr, tableProbePassCounter.Load())

	writeProxyConfig(&ProxyConfig{Servers: map[string]string{addr: ""}})
	if err := writeProxyState(&ProxyState{
		Source: "",
		Proxies: map[string]ProxyEntry{
			addr: {ID: 2, Health: "up", Source: "internal"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	if n := connects.Load(); n != 5 {
		t.Fatalf("internal-config proxy must be graded: %d CONNECTs, want 5 (4 table + 1 stage-0)", n)
	}
	state, _ := readProxyState()
	e := state.Proxies[addr]
	if !e.Graded || e.Score != 1.0 {
		t.Errorf("internal proxy grade not persisted: %+v", e)
	}
}

// TestPaidProxyGrader_KillSwitchSkips: enabled=false must skip the sweep
// entirely (full skip of the table probe, mirroring the fetch/reaper
// invariant) — no CONNECTs, no grade fields written.
func TestPaidProxyGrader_KillSwitchSkips(t *testing.T) {
	home := withTempHome(t)
	writePaidGradeProbeOverride(t, false)

	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()

	src := filepath.Join(home, "paid.txt")
	if err := os.WriteFile(src, []byte(addr+":u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{
		Source: src,
		Proxies: map[string]ProxyEntry{
			addr: {ID: 1, Health: "up", Source: "file"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	if n := connects.Load(); n != 0 {
		t.Fatalf("kill switch off must skip the probe entirely: %d CONNECTs", n)
	}
	state, _ := readProxyState()
	if e := state.Proxies[addr]; e.Graded || !e.LastGraded.IsZero() {
		t.Errorf("kill switch off must not write grades: %+v", e)
	}
}

// TestPaidProxyGrader_SkipsFreshGrade: an entry graded within the 1-3h
// stale window is not re-probed (rides the reaper stale cadence).
func TestPaidProxyGrader_SkipsFreshGrade(t *testing.T) {
	home := withTempHome(t)
	writePaidGradeProbeOverride(t, true)

	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()

	src := filepath.Join(home, "paid.txt")
	if err := os.WriteFile(src, []byte(addr+":u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{
		Source: src,
		Proxies: map[string]ProxyEntry{
			addr: {ID: 1, Health: "up", Source: "file", Graded: true, Score: 0.9, LastGraded: time.Now().Add(-time.Minute)},
		},
	}); err != nil {
		t.Fatal(err)
	}

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	if n := connects.Load(); n != 0 {
		t.Fatalf("fresh grade must not be re-probed: %d CONNECTs", n)
	}
}

// TestPaidProxyGrader_GradesFileProxyWithStaleURLTag pins the review HIGH
// finding: an address in the file/internal desired set is served as a file
// proxy (file wins in mergeProxyURLCache) and MUST be graded here even if
// its state entry carries a stale first-seen "url" tag. The desired set is
// the ownership definition, not the tag.
func TestPaidProxyGrader_GradesFileProxyWithStaleURLTag(t *testing.T) {
	home := withTempHome(t)
	writePaidGradeProbeOverride(t, true)

	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()
	seedProbeDNSForAddress(t, addr, tableProbePassCounter.Load())

	src := filepath.Join(home, "paid.txt")
	if err := os.WriteFile(src, []byte(addr+":u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Stale first-seen tag: the address was first added from a URL source,
	// but the operator ALSO put it in the paid file. The box serves it as a
	// file proxy; the sweep must grade it regardless of the tag.
	if err := writeProxyState(&ProxyState{
		Source: src,
		Proxies: map[string]ProxyEntry{
			addr: {ID: 1, Health: "up", Source: "url", LastGraded: time.Time{}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	if n := connects.Load(); n != 5 {
		t.Fatalf("file-desired proxy with stale url tag must be graded: %d CONNECTs, want 5 (4 table + 1 stage-0)", n)
	}
	state, _ := readProxyState()
	e := state.Proxies[addr]
	if !e.Graded || e.Score != 1.0 {
		t.Errorf("expected grade persisted despite stale url tag: %+v", e)
	}
	if e.Source != "url" {
		t.Errorf("lifecycle Source tag must be untouched: %q", e.Source)
	}
}

// TestPaidProxyGrader_SkipsMissingEntry pins the review MEDIUM finding: a
// proxy with NO ProxyEntry (never launched, or removed between collect and
// apply) must not be graded and must not get a ghost entry created.
func TestPaidProxyGrader_SkipsMissingEntry(t *testing.T) {
	home := withTempHome(t)
	writePaidGradeProbeOverride(t, true)

	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()

	src := filepath.Join(home, "paid.txt")
	if err := os.WriteFile(src, []byte(addr+":u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// No ProxyEntry at all for the address.
	if err := writeProxyState(&ProxyState{Source: src, Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	if n := connects.Load(); n != 0 {
		t.Fatalf("missing entry must not be probed: %d CONNECTs", n)
	}
	state, _ := readProxyState()
	if _, ok := state.Proxies[addr]; ok {
		t.Error("must not create a ghost ProxyEntry for an untracked address")
	}
}

// TestPaidProxyGrader_ReadErrorStillProbesTracked: an unreadable source
// file cannot prove non-ownership, so tracked non-URL entries are still
// probed AND graded (the read error only degrades the file leg of the union).
func TestPaidProxyGrader_ReadErrorStillProbesTracked(t *testing.T) {
	home := withTempHome(t)
	writePaidGradeProbeOverride(t, true)

	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()

	// Source points at a file that does not exist.
	missing := filepath.Join(home, "does-not-exist.txt")
	before := &ProxyState{
		Source: missing,
		Proxies: map[string]ProxyEntry{
			addr: {ID: 1, Health: "up", Source: "file"},
		},
	}
	if err := writeProxyState(before); err != nil {
		t.Fatal(err)
	}

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	// The tracked proxy must STILL be probed/graded even though the source
	// file is unreadable: the collector uses tracked proxy.state entries as
	// the source of truth (fix for "paid proxies never graded when
	// state.Source is empty or the file is missing").
	if n := connects.Load(); n == 0 {
		t.Fatalf("tracked proxy must be probed despite source-file read error (0 CONNECTs)")
	}
	state, _ := readProxyState()
	e := state.Proxies[addr]
	if e.LastGraded.IsZero() {
		t.Errorf("expected a grade write (LastGraded advanced) for the tracked proxy: %+v", e)
	}
}

// TestPaidProxyGrader_EmptySourceFileStillProbesTracked: a readable-but-empty
// source file proves nothing about ownership (mid-edit), so tracked entries
// stay probed instead of a clean no-op.
func TestPaidProxyGrader_EmptySourceFileStillProbesTracked(t *testing.T) {
	home := withTempHome(t)
	writePaidGradeProbeOverride(t, true)

	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()

	src := filepath.Join(home, "empty.txt")
	if err := os.WriteFile(src, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{
		Source: src,
		Proxies: map[string]ProxyEntry{
			addr: {ID: 1, Health: "up", Source: "file", LastGraded: time.Time{}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	// Tracked proxies must be probed even when the source file is empty: the
	// tracked proxy.state entries are the source of truth (fix for "paid
	// proxies stayed ungraded when the config had no proxies").
	if n := connects.Load(); n == 0 {
		t.Fatalf("tracked proxy must be probed despite empty source file (0 CONNECTs)")
	}
}

// TestPaidProxyGrader_UndecidableKeepsPriorGrade: a pass whose sample was
// gutted by the box's own DNS carries no verdict — the prior grade is kept
// intact (C1/C2 semantics), but LastGraded still advances so the entry is
// not re-probed every 5-minute tick (no herd).
func TestPaidProxyGrader_UndecidableKeepsPriorGrade(t *testing.T) {
	home := withTempHome(t)
	writePaidGradeProbeOverride(t, true)

	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()
	// Force every sampled host unresolvable -> Total=0 -> undecidable.
	//
	// The probe samples a host block derived from
	// (fnv(address) + tableProbePassCounter.Load()) with SampleWidth
	// rotation. Two independent hazards make a window-of-passes seed
	// (previously 8) flaky:
	//
	// 1. The host table contains literal IPs (1.1.1.1, 8.8.8.8, 9.9.9.9)
	//    at fixed indices, and resolveProbeTarget short-circuits literal IPs
	//    BEFORE consulting the DNS cache (net.ParseIP fast path) — a seeded
	//    fail can never make them unresolvable. A pass whose block includes
	//    one always dials it, and the 0-CONNECT assertion below fails
	//    regardless of any seeding. The rotation puts a literal IP in the
	//    block for ~4.7% of passes (observed 5/40 failures in isolation).
	// 2. URL fetch cycles from other tests' leaked background fetchers
	//    advance the counter between seeding and the probe's Load(), so the
	//    probe can read a pass outside the seeded window.
	//
	// Deterministic fix: PIN the counter to a pass whose block is
	// hostname-only (no literal IP — always exists within a few passes of
	// any address since consecutive passes walk the table one block apart),
	// seed exactly that pass, and restore the old value afterwards. The
	// probe reads exactly what was seeded, and the residual race window
	// (a background Add() between the Store and the probe's Load) is
	// covered by seeding the next two passes as well — all hostname-only
	// by the same rotation argument.
	origPass := tableProbePassCounter.Load()
	pinnedPass := tableProbePassPinnedUndecidable(addr)
	tableProbePassCounter.Store(pinnedPass)
	t.Cleanup(func() { tableProbePassCounter.Store(origPass) })
	seedProbeDNSFailForAddress(t, addr, pinnedPass, pinnedPass+1, pinnedPass+2)

	src := filepath.Join(home, "paid.txt")
	if err := os.WriteFile(src, []byte(addr+":u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{
		Source: src,
		Proxies: map[string]ProxyEntry{
			addr: {ID: 1, Health: "up", Source: "file", Graded: true, Score: 0.9, LastGraded: time.Now().Add(-24 * time.Hour)},
		},
	}); err != nil {
		t.Fatal(err)
	}

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	state, _ := readProxyState()
	e := state.Proxies[addr]
	if !e.Graded || e.Score != 0.9 {
		t.Errorf("undecidable pass must keep prior grade, got graded=%v score=%v", e.Graded, e.Score)
	}
	if !e.LastGraded.After(time.Now().Add(-time.Minute)) {
		t.Error("LastGraded must advance on any completed pass (no re-probe herd)")
	}
	// No resolvable table targets -> the TABLE probe dials nothing; only the
	// single stage-0 backend-reachability pass hits the proxy (probeProxy:
	// SOCKS5 + API CONNECT, which the fake answers, then TLS fails ->
	// probeTLSFailed -> passes the gate). So exactly 1 dial, not a hammered
	// block. Pins that an undecidable pass does NOT burn a sample on it.
	if n := connects.Load(); n != 1 {
		t.Fatalf("expected 1 CONNECT (stage-0 only; table unresolvable), got %d", n)
	}
}

// tableProbePassPinnedUndecidable returns the first pass >= 0 whose sampled
// block for address contains no literal-IP target (which resolveProbeTarget
// resolves without consulting the DNS cache, so a seeded fail can never
// block it). Consecutive passes walk the table one block (SampleWidth) apart,
// and the literal IPs occupy a fixed small set of indices, so a hostname-only
// pass always exists within a few passes of any address.
func tableProbePassPinnedUndecidable(address string) uint64 {
	cfg := resolveProxyTableProbeConfig()
	for pass := uint64(0); pass < 64; pass++ {
		hosts, _ := connect.SampleProbeTargets(tableProbeSeed(address, pass), cfg.SampleWidth)
		hasLiteral := false
		for _, h := range hosts {
			if net.ParseIP(h) != nil {
				hasLiteral = true
				break
			}
		}
		if !hasLiteral {
			return pass
		}
	}
	return 0 // unreachable: hostname-only passes are dense; fall back safely
}

// TestPaidGradeSettingsMatch pins the stale-settings guard used at apply
// time: a probe result whose credentials no longer match the address's
// current settings must be rejected (coderabbit review) — otherwise a
// concurrent reload that rotated credentials would persist a stale-creds
// grade and defer the next probe by the whole 1-3h window.
func TestPaidGradeSettingsMatch(t *testing.T) {
	withCreds := connect.ProxySettings{Address: "1.2.3.4:1080", Auth: &proxy.Auth{User: "u", Password: "p"}}
	noCreds := connect.ProxySettings{Address: "1.2.3.4:1080"}

	cases := []struct {
		name           string
		s              connect.ProxySettings
		user, password string
		want           bool
	}{
		{"same creds", withCreds, "u", "p", true},
		{"user changed", withCreds, "u2", "p", false},
		{"password changed", withCreds, "u", "p2", false},
		{"both changed", withCreds, "u2", "p2", false},
		{"auth dropped", withCreds, "", "", false},
		{"no creds matches empty", noCreds, "", "", true},
		{"no creds vs provided", noCreds, "u", "p", false},
	}
	for _, c := range cases {
		if got := paidGradeSettingsMatch(c.s, c.user, c.password); got != c.want {
			t.Errorf("%s: paidGradeSettingsMatch(%+v, %q, %q) = %v, want %v",
				c.name, c.s, c.user, c.password, got, c.want)
		}
	}
}

// seedProbeDNSFailForAddress injects FAILED resolutions for the stage-1
// sampled targets of address at the given probe pass values, forcing an
// undecidable (DNS-gutted) pass offline. Removed on test cleanup.
func seedProbeDNSFailForAddress(t *testing.T, address string, passes ...uint64) {
	t.Helper()
	cfg := resolveProxyTableProbeConfig()
	added := map[string]bool{}
	probeDNSCache.Lock()
	for _, pass := range passes {
		hosts, _ := connect.SampleProbeTargets(tableProbeSeed(address, pass), cfg.SampleWidth)
		for _, h := range hosts {
			if _, exists := probeDNSCache.fail[h]; !exists {
				added[h] = true
			}
			probeDNSCache.fail[h] = time.Now()
			delete(probeDNSCache.m, h)
		}
	}
	probeDNSCache.Unlock()
	t.Cleanup(func() {
		probeDNSCache.Lock()
		defer probeDNSCache.Unlock()
		for h := range added {
			delete(probeDNSCache.fail, h)
		}
	})
}
