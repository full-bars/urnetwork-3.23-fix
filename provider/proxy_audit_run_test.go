package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/proxy"

	"github.com/urnetwork/connect"
)

func auditCancelMap(addrs ...string) (map[string]context.CancelFunc, map[string]*bool) {
	m := map[string]context.CancelFunc{}
	flags := map[string]*bool{}
	for _, a := range addrs {
		called := new(bool)
		flags[a] = called
		m[a] = func() { *called = true }
	}
	return m, flags
}

var govCancelMap = auditCancelMap

func TestParkProxies_CancelsAndRemovesFromCancelMap(t *testing.T) {
	m, called := auditCancelMap("a:1", "b:1")
	var mu sync.Mutex

	got := parkProxies([]string{"a:1"}, m, &mu, func(string) bool { return true }, nil)

	if len(got) != 1 || got[0] != "a:1" {
		t.Fatalf("expected [a:1], got %v", got)
	}
	if !*called["a:1"] {
		t.Fatalf("a:1 cancel should have been called")
	}
	if m["a:1"] != nil {
		t.Fatalf("a:1 must be removed from the map")
	}
	if *called["b:1"] || m["b:1"] == nil {
		t.Fatalf("b:1 must be untouched")
	}
}

// The candidate list was built from a snapshot that is stale by the time we
// act; the proxy may have picked up sessions since. Re-verify at the trigger.
func TestParkProxies_SkipsWhenReverifyFails(t *testing.T) {
	m, called := auditCancelMap("a:1")
	var mu sync.Mutex

	got := parkProxies([]string{"a:1"}, m, &mu, func(string) bool { return false }, nil)

	if len(got) != 0 || *called["a:1"] || m["a:1"] == nil {
		t.Fatalf("failing reverify must cancel nothing and leave map intact")
	}
}

func TestParkProxies_SkipsAddressNotInCancelMap(t *testing.T) {
	m, _ := auditCancelMap("a:1")
	var mu sync.Mutex

	got := parkProxies([]string{"b:1"}, m, &mu, func(string) bool { return true }, nil)

	if len(got) != 0 {
		t.Fatalf("missing address must not be reported parked")
	}
}

// Re-verify must run under the same lock as the cancel+delete so nothing can
// change between the check and the act (same rule as reapProxies).
func TestParkProxies_ReverifiesUnderTheCancelLock(t *testing.T) {
	m, _ := auditCancelMap("a:1")
	var mu sync.Mutex
	reverifySawLockHeld := false

	parkProxies([]string{"a:1"}, m, &mu, func(string) bool {
		// TryLock fails iff mu is currently held.
		reverifySawLockHeld = !mu.TryLock()
		if !reverifySawLockHeld {
			mu.Unlock()
		}
		return true
	}, nil)

	if !reverifySawLockHeld {
		t.Fatalf("reverify must run with cancelMu held")
	}
}

// A7: a panic in a callback must not leave the cancel lock held. reload and the
// reaper take the same lock, and HandleError only recovers the goroutine.
func TestParkProxies_ReleasesTheLockWhenACallbackPanics(t *testing.T) {
	for _, which := range []string{"eligible", "onParked"} {
		t.Run(which, func(t *testing.T) {
			m, _ := auditCancelMap("a:1")
			var mu sync.Mutex

			func() {
				defer func() { _ = recover() }()
				eligible := func(string) bool {
					if which == "eligible" {
						panic("boom")
					}
					return true
				}
				onParked := func(string) {
					if which == "onParked" {
						panic("boom")
					}
				}
				parkProxies([]string{"a:1"}, m, &mu, eligible, onParked)
			}()

			// If the defer in parkProxies did not run, TryLock deadlocks or fails.
			if !mu.TryLock() {
				t.Fatalf("cancelMu remained locked after a panic in %s", which)
			}
			mu.Unlock()
		})
	}
}

// B7: an address that left the paid list is released, not "restored".
func TestProxyAuditRunOnce_ReleaseOfARemovedAddressIsNotCalledRestored(t *testing.T) {
	h := newAuditHarness("a:1")
	h.act = true
	h.twoBadTicks("a:1")

	// Drop a:1 from the desired paid set so readPaid no longer returns it, then
	// advance the clock past its backoff.
	delete(h.entries, "a:1")
	h.now = h.now.Add(24 * time.Hour)

	h.g.runOnce()

	for _, l := range h.logs {
		if strings.Contains(l, "restored a:1") {
			t.Fatalf("expected a 'released' line, got: %v", h.logs)
		}
	}
	if h.logged("released a:1 (no longer in the paid proxy list)") != 1 {
		t.Fatalf("expected 'released a:1 (no longer in the paid proxy list)', got: %v", h.logs)
	}
}

// A4/B6: the backoff must be in place before the cancel lock is released.
// Otherwise a reload that runs between the cancel and the backoff sees the
// address not running and eligible, and relaunches what was just parked.
func TestParkProxies_OnParkedRunsUnderTheCancelLockForParkedAddressesOnly(t *testing.T) {
	m, _ := auditCancelMap("a:1", "b:1")
	var mu sync.Mutex

	var parked []string
	sawLockHeld := false
	got := parkProxies([]string{"a:1", "c:1"}, m, &mu, func(string) bool { return true }, func(addr string) {
		sawLockHeld = !mu.TryLock()
		if !sawLockHeld {
			mu.Unlock()
		}
		parked = append(parked, addr)
	})

	if !slices.Equal(got, []string{"a:1"}) || !slices.Equal(parked, []string{"a:1"}) {
		t.Fatalf("onParked should fire only for actually parked addresses, got %v / %v", got, parked)
	}
	if !sawLockHeld {
		t.Fatalf("onParked must run with cancelMu held")
	}
}

// --- runOnce: fakes for every live dependency --------------------------------

type auditHarness struct {
	g           *proxyAuditor
	now         time.Time
	entries     map[string]ProxyEntry
	creds       map[string]string
	fileOK      bool
	act         bool
	healthUp    map[string]bool
	clients     func(addr string) (int64, bool)
	cancels     map[string]context.CancelFunc
	called      map[string]*bool
	mu          sync.Mutex
	parkedMarks []string
	logs        []string
	hist        *proxyFailureHistory
}

type govHarness = auditHarness

func newAuditHarness(addrs ...string) *auditHarness {
	h := &auditHarness{
		now:      auditEpoch,
		entries:  map[string]ProxyEntry{},
		creds:    map[string]string{},
		fileOK:   true,
		healthUp: map[string]bool{},
		hist:     &proxyFailureHistory{failures: map[string]int{}},
	}
	h.clients = func(string) (int64, bool) { return 0, true }
	h.cancels, h.called = auditCancelMap(addrs...)
	for _, a := range addrs {
		h.healthUp[a] = true
	}
	env := proxyAuditEnv{
		now: func() time.Time { return h.now },
		readPaid: func() (map[string]ProxyEntry, map[string]string, bool) {
			out := map[string]ProxyEntry{}
			for k, v := range h.entries {
				out[k] = v
			}
			creds := map[string]string{}
			for k, v := range h.creds {
				creds[k] = v
			}
			return out, creds, h.fileOK
		},
		health: func() map[string]connect.ProxyHealthStatus {
			out := map[string]connect.ProxyHealthStatus{}
			for a, up := range h.healthUp {
				st := "dead"
				if up {
					st = "up"
				}
				out[a] = connect.ProxyHealthStatus{Health: st}
			}
			return out
		},
		clients:        func(a string) (int64, bool) { return h.clients(a) },
		earningsScore:  func(string, time.Time) float64 { return 0 },
		earnedRecently: func(string) bool { return false },
		trackerWarm:    func() bool { return true },
		act:            func() bool { return h.act },
		markParked: func(addrs []string) {
			h.parkedMarks = append(h.parkedMarks, addrs...)
		},
		rand:           func() float64 { return 0.5 },
		failureHistory: h.hist,
		cancelMap:      h.cancels,
		cancelMu:       &h.mu,
		log:            func(f string, a ...any) { h.logs = append(h.logs, fmt.Sprintf(f, a...)) },
	}
	h.g = newProxyAuditor(auditCfg(), env)
	return h
}

var newGovHarness = newAuditHarness

// grade sets addr's grade as a decidable pass stamped at gradedAt, the way the
// grader writes one: both the attempt clock and the verdict clock advance.
func (h *govHarness) grade(addr string, score float64, gradedAt time.Time) {
	h.entries[addr] = ProxyEntry{Health: "up", Score: score, Graded: true, LastGraded: gradedAt, LastDecided: gradedAt}
}

// twoBadTicks feeds the two qualifying bad grades and returns after the second
// tick, so the caller sees what the second tick did.
func (h *govHarness) twoBadTicks(addr string) {
	h.grade(addr, 0.1, h.now.Add(-7*time.Hour))
	h.g.runOnce()
	h.now = h.now.Add(6 * time.Hour)
	h.grade(addr, 0.1, h.now.Add(-time.Hour))
	h.g.runOnce()
}

func (h *govHarness) logged(sub string) int {
	n := 0
	for _, l := range h.logs {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// Self-heal off is observe mode: proxy audit says what it WOULD do, once per
// proxy, and touches nothing.
func TestProxyAuditRunOnce_ObserveModeLogsWouldParkOnceAndCancelsNothing(t *testing.T) {
	h := newGovHarness("a:1")
	h.act = false
	h.twoBadTicks("a:1")
	h.now = h.now.Add(5 * time.Minute)
	h.g.runOnce() // same grades, still eligible

	if *h.called["a:1"] || h.cancels["a:1"] == nil {
		t.Fatalf("observe mode must not cancel anything")
	}
	if got := h.logged("would-park a:1"); got != 1 {
		t.Fatalf("would-park should be logged once per newly eligible proxy, got %d in %v", got, h.logs)
	}
	if !h.hist.Eligible("a:1", h.now) {
		t.Fatalf("observe mode must not set a backoff")
	}
}

func TestProxyAuditRunOnce_ActModeParksAndBacksOff(t *testing.T) {
	h := newGovHarness("a:1")
	h.act = true
	h.twoBadTicks("a:1")

	if !*h.called["a:1"] || h.cancels["a:1"] != nil {
		t.Fatalf("act mode should cancel the proxy and drop it from the cancel map")
	}
	if h.hist.Eligible("a:1", h.now.Add(5*time.Hour)) || !h.hist.Eligible("a:1", h.now.Add(6*time.Hour+time.Second)) {
		t.Fatalf("expected a 6h backoff (first ladder rung, no jitter at rnd 0.5)")
	}
	if len(h.parkedMarks) != 1 || h.parkedMarks[0] != "a:1" {
		t.Fatalf("a parked proxy must be marked parked in proxy.state, got %v", h.parkedMarks)
	}
	if h.logged("parked a:1") != 1 {
		t.Fatalf("expected a parked log line, got %v", h.logs)
	}
}

// B3: the mark can fail (proxy lock busy) or be overwritten by the heartbeat
// before the old goroutine unregisters. Either leaves a parked proxy reading
// "up" in proxy.state. Proxy audit re-asserts it on the next tick.
func TestProxyAuditRunOnce_ReassertsAMarkThatWasLostOrOverwritten(t *testing.T) {
	h := newGovHarness("a:1")
	h.act = true
	h.twoBadTicks("a:1")
	if len(h.parkedMarks) != 1 {
		t.Fatalf("setup: expected the initial mark, got %v", h.parkedMarks)
	}

	// The next tick still sees "up" in proxy.state (the heartbeat wrote it back).
	h.now = h.now.Add(5 * time.Minute)
	h.healthUp["a:1"] = false
	e := h.entries["a:1"]
	e.Health = "up"
	h.entries["a:1"] = e
	h.g.runOnce()
	if len(h.parkedMarks) != 2 || h.parkedMarks[1] != "a:1" {
		t.Fatalf("a parked proxy reading up must be re-marked, got %v", h.parkedMarks)
	}

	// Once proxy.state says parked, proxy audit leaves it alone: every mark
	// takes the cross-process proxy lock.
	h.now = h.now.Add(5 * time.Minute)
	e.Health = proxyHealthParked
	h.entries["a:1"] = e
	h.g.runOnce()
	if len(h.parkedMarks) != 2 {
		t.Fatalf("a correctly marked proxy must not be re-marked, got %v", h.parkedMarks)
	}
}

// The snapshot said idle; by the time we pull the trigger it has sessions.
// Nothing may be cancelled, and no backoff or ladder rung may be spent.
func TestProxyAuditRunOnce_ReverifyAtTriggerProtectsBusyProxy(t *testing.T) {
	h := newGovHarness("a:1")
	h.act = true
	calls := 0
	h.clients = func(string) (int64, bool) {
		calls++
		if calls <= 2 { // the two snapshot reads (one per tick) see idle
			return 0, true
		}
		return 3, true // the re-verify under the lock sees sessions
	}
	h.twoBadTicks("a:1")

	if *h.called["a:1"] || h.cancels["a:1"] == nil {
		t.Fatalf("a proxy that turned busy at the trigger must not be cancelled")
	}
	if !h.hist.Eligible("a:1", h.now) || len(h.parkedMarks) != 0 {
		t.Fatalf("no backoff or parked mark for a park that did not happen")
	}
}

// Flipping self-heal off must give back what proxy audit took.
func TestProxyAuditRunOnce_TurningActOffReleasesParks(t *testing.T) {
	h := newGovHarness("a:1")
	h.act = true
	h.twoBadTicks("a:1")
	if h.hist.Eligible("a:1", h.now) {
		t.Fatalf("setup: expected a:1 parked")
	}

	h.act = false
	h.now = h.now.Add(5 * time.Minute)
	h.g.runOnce()

	if !h.hist.Eligible("a:1", h.now) {
		t.Fatalf("turning self-heal off must clear proxy audit's backoff so reload relaunches it")
	}
	if h.logged("released") == 0 {
		t.Fatalf("expected a release log line, got %v", h.logs)
	}
}

func TestProxyAuditRunOnce_GoodRegradeAfterDwellRestores(t *testing.T) {
	h := newGovHarness("a:1")
	h.act = true
	h.twoBadTicks("a:1")

	h.now = h.now.Add(2 * time.Hour)
	h.grade("a:1", 0.9, h.now.Add(-30*time.Minute)) // regraded well after the park
	h.healthUp["a:1"] = false                       // it is parked, not running
	h.g.runOnce()

	if !h.hist.Eligible("a:1", h.now) {
		t.Fatalf("a healthy regrade should clear the backoff so the hourly reload restores it")
	}
}

// LA7 (2026-09-18): re-pasting the same host:port with new credentials rotates
// the proxy. A park (and its address-keyed backoff) made under the old
// credentials must not survive that, or the fixed credentials sit out the ladder.
func TestProxyAuditRunOnce_RotatedCredentialsClearThePark(t *testing.T) {
	h := newGovHarness("a:1")
	h.act = true
	h.creds["a:1"] = "old"
	h.twoBadTicks("a:1")
	if h.hist.Eligible("a:1", h.now) {
		t.Fatalf("setup: expected a:1 parked")
	}

	h.now = h.now.Add(5 * time.Minute)
	h.creds["a:1"] = "new" // operator re-pasted the address with new credentials
	h.g.runOnce()

	if !h.hist.Eligible("a:1", h.now) {
		t.Fatalf("rotating a parked address's credentials must clear its backoff so reload relaunches it")
	}
}

// The same rotation on a proxy that was NOT parked: two bad grades taken under
// the old credentials must not let proxy audit park the new ones.
func TestProxyAuditRunOnce_BadGradesUnderOldCredentialsDoNotParkTheRotatedProxy(t *testing.T) {
	h := newGovHarness("a:1")
	h.act = true
	h.creds["a:1"] = "old"
	h.grade("a:1", 0.1, h.now.Add(-7*time.Hour))
	h.g.runOnce()
	h.now = h.now.Add(6 * time.Hour)
	h.grade("a:1", 0.1, h.now.Add(-time.Hour))
	h.creds["a:1"] = "new" // rotated before the second tick
	h.g.runOnce()

	if *h.called["a:1"] || !h.hist.Eligible("a:1", h.now) {
		t.Fatalf("grades measured under the old credentials are not evidence about the new ones")
	}
}

// The fingerprint is what tells rotated credentials from an unchanged proxy.
// It must separate near-identical endpoints (same host, different port or
// credentials), be stable for identical input, and never expose the secret.
func TestProxyAuditCredFingerprint(t *testing.T) {
	a := proxyAuditCredFingerprint(&proxy.Auth{User: "alice", Password: "pw1"})
	if a != proxyAuditCredFingerprint(&proxy.Auth{User: "alice", Password: "pw1"}) {
		t.Fatalf("identical credentials must fingerprint identically")
	}
	for name, other := range map[string]*proxy.Auth{
		"password": {User: "alice", Password: "pw2"},
		"user":     {User: "alicf", Password: "pw1"},
		"split":    {User: "alicepw", Password: "1"}, // must not collide with a naive user+password concat
	} {
		if proxyAuditCredFingerprint(other) == a {
			t.Fatalf("credentials differing in %s must not share a fingerprint", name)
		}
	}
	if proxyAuditCredFingerprint(nil) != "" {
		t.Fatalf("a proxy with no credentials has the empty fingerprint")
	}
	if proxyAuditCredFingerprint(&proxy.Auth{}) == a || proxyAuditCredFingerprint(&proxy.Auth{}) != "" {
		t.Fatalf("empty credentials are the same as no credentials")
	}
	if strings.Contains(a, "pw1") || strings.Contains(a, "alice") {
		t.Fatalf("the fingerprint must not contain the credentials, got %q", a)
	}
}

// Through the live reader: near-identical endpoints keep distinct identities,
// and re-pasting the SAME address with new credentials changes only its own
// fingerprint.
func TestLiveReadPaid_FingerprintsFollowTheAddressNotTheHost(t *testing.T) {
	home := withTempHome(t)
	src := filepath.Join(home, "paid.txt")
	write := func(lines string) {
		os.WriteFile(src, []byte(lines), 0600)
	}
	if err := writeProxyState(&ProxyState{Source: src, Proxies: map[string]ProxyEntry{
		identityKey("9.9.9.9:1080", "alice"): {ID: 1, Health: "up", Source: "file"},
		identityKey("9.9.9.9:1081", "alice"): {ID: 2, Health: "up", Source: "file"}, // same host, next port
		identityKey("9.9.9.8:1080", "alice"): {ID: 3, Health: "up", Source: "file"}, // next host, same port
	}}); err != nil {
		t.Fatal(err)
	}
	write("9.9.9.9:1080:alice:pw1\n9.9.9.9:1081:alice:pw1\n9.9.9.8:1080:alice:pw1\n")
	_, before, ok := liveReadPaid()
	if !ok || len(before) != 3 {
		t.Fatalf("expected 3 fingerprints, ok=%v got %v", ok, before)
	}

	write("9.9.9.9:1080:alice:pw2\n9.9.9.9:1081:alice:pw1\n9.9.9.8:1080:alice:pw1\n")
	_, after, _ := liveReadPaid()
	k1080, k1081, k8 := identityKey("9.9.9.9:1080", "alice"), identityKey("9.9.9.9:1081", "alice"), identityKey("9.9.9.8:1080", "alice")
	if after[k1080] == before[k1080] {
		t.Fatalf("re-pasting an address with new credentials must change its fingerprint")
	}
	if after[k1081] != before[k1081] || after[k8] != before[k8] {
		t.Fatalf("near-identical endpoints must not be disturbed by another address's rotation")
	}
}

// If the source file cannot be trusted the paid set is unknown: act on nothing.
func TestProxyAuditRunOnce_UntrustedPaidSetDoesNothing(t *testing.T) {
	h := newGovHarness("a:1")
	h.act = true
	h.fileOK = false
	h.twoBadTicks("a:1")

	if *h.called["a:1"] || h.cancels["a:1"] == nil {
		t.Fatalf("nothing may be parked when the paid set is not trustworthy")
	}
}

// B4/A8: pausing must not be silent. It is logged once when it starts and once
// when it ends, published in the status (with the tick still advancing, so a
// stuck audit is distinguishable from a quiet one), and it must not stop
// parks from being released when self-heal is turned off.
func TestProxyAuditRunOnce_PausedIsLoggedOnceAndPublished(t *testing.T) {
	currentProxyAuditStatus.Store(nil)
	t.Cleanup(func() { currentProxyAuditStatus.Store(nil) })
	h := newGovHarness("a:1")
	h.act = true
	h.fileOK = false

	h.g.runOnce()
	first := proxyAuditStatusSnapshot()
	h.now = h.now.Add(5 * time.Minute)
	h.g.runOnce()
	second := proxyAuditStatusSnapshot()

	if h.logged("paused") != 1 {
		t.Fatalf("expected the pause to be logged once across two ticks, got %v", h.logs)
	}
	if first == nil || !first.Paused || second == nil || !second.Paused {
		t.Fatalf("status must report paused, got %+v then %+v", first, second)
	}
	if !second.UpdatedAt.After(first.UpdatedAt) {
		t.Fatalf("a paused audit still ticks; UpdatedAt must advance")
	}
	if !first.PausedSince.Equal(second.PausedSince) || first.PausedSince.IsZero() {
		t.Fatalf("PausedSince must stay at when the pause began, got %v then %v", first.PausedSince, second.PausedSince)
	}

	h.fileOK = true
	h.entries["a:1"] = ProxyEntry{Health: "up"}
	h.now = h.now.Add(5 * time.Minute)
	h.g.runOnce()
	if h.logged("resumed") != 1 {
		t.Fatalf("expected one resume line, got %v", h.logs)
	}
	if st := proxyAuditStatusSnapshot(); st == nil || st.Paused {
		t.Fatalf("status must clear paused on resume, got %+v", st)
	}
}

func TestProxyAuditRunOnce_TurningActOffWhilePausedStillReleasesParks(t *testing.T) {
	h := newGovHarness("a:1")
	h.act = true
	h.twoBadTicks("a:1")
	if h.hist.Eligible("a:1", h.now) {
		t.Fatalf("setup: expected a:1 parked")
	}

	h.fileOK = false // paid set becomes untrusted
	h.act = false    // and the operator turns self-heal off
	h.now = h.now.Add(5 * time.Minute)
	h.g.runOnce()

	if !h.hist.Eligible("a:1", h.now) {
		t.Fatalf("turning self-heal off must release parks even while the paid set is untrusted")
	}
}

// --- live wiring ---------------------------------------------------------------

func writeProxyAuditMarker(t *testing.T, home, value string) {
	t.Helper()
	path := filepath.Join(home, ".urnetwork", "proxy_audit")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

var writeSelfHealMarker = writeProxyAuditMarker

// Proxy audit acts only when proxy audit is on AND hot restart is on. With hot
// restart off every relaunch mints a fresh client identity, so parking would
// churn identities instead of resting a proxy.
func TestProxyAuditActEnabled_RequiresAuditAndHotRestart(t *testing.T) {
	home := withTempHome(t)

	if proxyAuditActEnabled(false) {
		t.Fatalf("no marker and startup off: must observe")
	}
	if !proxyAuditActEnabled(true) {
		t.Fatalf("startup on (URNETWORK_PROXY_AUDIT=1) with hot restart default-on should act")
	}

	writeProxyAuditMarker(t, home, "on")
	if !proxyAuditActEnabled(false) {
		t.Fatalf("`urnet-tools proxy audit on` must enable acting live")
	}

	t.Setenv("URNETWORK_HOT_RESTART", "0")
	if proxyAuditActEnabled(false) {
		t.Fatalf("hot restart off must force observe even with proxy audit on")
	}
	t.Setenv("URNETWORK_HOT_RESTART", "")

	writeProxyAuditMarker(t, home, "off")
	if proxyAuditActEnabled(true) {
		t.Fatalf("`urnet-tools proxy audit off` must win over the startup value")
	}
}

func TestMarkProxiesInactive_UpdatesOnlyNamedTrackedEntries(t *testing.T) {
	withTempHome(t)
	err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{
		"a:1": {ID: 1, Health: "up", Score: 0.1, Graded: true},
		"b:1": {ID: 2, Health: "up"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	markProxiesParked([]string{"a:1", "ghost:1"})

	state, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Proxies["a:1"].Health != proxyHealthParked {
		t.Fatalf("a:1 should be %q, got %q", proxyHealthParked, state.Proxies["a:1"].Health)
	}
	if !state.Proxies["a:1"].Graded || state.Proxies["a:1"].Score != 0.1 {
		t.Fatalf("grade fields must be left untouched: %+v", state.Proxies["a:1"])
	}
	if state.Proxies["b:1"].Health != "up" {
		t.Fatalf("other entries must be untouched, got %q", state.Proxies["b:1"].Health)
	}
	if _, ok := state.Proxies["ghost:1"]; ok {
		t.Fatalf("an address that is not tracked must not be created")
	}
}

func TestLiveReadPaid_ReturnsTrackedPaidProxiesOnly(t *testing.T) {
	home := withTempHome(t)
	src := filepath.Join(home, "paid.txt")
	os.WriteFile(src, []byte("1.1.1.1:1080:u:p\n3.3.3.3:1080:u:p\n"), 0600)
	err := writeProxyState(&ProxyState{Source: src, Proxies: map[string]ProxyEntry{
		identityKey("1.1.1.1:1080", "u"): {ID: 1, Health: "up", Source: "file"},
		"2.2.2.2:1080":                   {ID: 2, Health: "up", Source: "url"}, // URL-sourced: not paid
	}})
	if err != nil {
		t.Fatal(err)
	}

	got, _, ok := liveReadPaid()
	if !ok {
		t.Fatalf("a readable source file is a trustworthy paid set")
	}
	if len(got) != 1 || got[identityKey("1.1.1.1:1080", "u")].ID != 1 {
		t.Fatalf("expected only the tracked file proxy, got %v", got)
	}
}

// A credentialed file proxy plus an unauthenticated internal-config proxy,
// tracked under the keys reload writes (identity for the credentialed one, bare
// address for the other), must both surface. Before the paid set was
// identity-keyed the credentialed proxy was invisible to the audit.
func TestLiveReadPaid_MixedCredentialedAndBareTracked(t *testing.T) {
	home := withTempHome(t)
	src := filepath.Join(home, "paid.txt")
	os.WriteFile(src, []byte("10.0.0.1:1080:u:p\n"), 0600)
	writeProxyConfig(&ProxyConfig{Servers: map[string]string{"10.0.0.2:1080": ""}})
	credKey := identityKey("10.0.0.1:1080", "u")
	if err := writeProxyState(&ProxyState{Source: src, Proxies: map[string]ProxyEntry{
		credKey:         {ID: 1, Health: "up", Source: "file"},
		"10.0.0.2:1080": {ID: 2, Health: "up", Source: "file"},
	}}); err != nil {
		t.Fatal(err)
	}
	got, creds, ok := liveReadPaid()
	if !ok || len(got) != 2 {
		t.Fatalf("expected both tracked file proxies, ok=%v got %v", ok, got)
	}
	if creds[credKey] == "" || creds["10.0.0.2:1080"] != "" {
		t.Fatalf("only the credentialed proxy has a fingerprint, got %v", creds)
	}
}

func TestLiveReadPaid_UnreadableSourceIsUntrusted(t *testing.T) {
	home := withTempHome(t)
	err := writeProxyState(&ProxyState{Source: filepath.Join(home, "missing.txt"), Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {ID: 1, Health: "up", Source: "file"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := liveReadPaid(); ok {
		t.Fatalf("an unreadable source file must make the paid set untrusted")
	}
}

// "No earnings" only means something once the in-memory tracker has watched a
// couple of windows; right after a restart it knows nothing.
func TestProxyAuditTrackerWarm(t *testing.T) {
	start := auditEpoch
	if proxyAuditTrackerWarm(start, start.Add(2*paidEarnWindow-time.Second)) {
		t.Fatalf("inside two earn windows of start the tracker is cold")
	}
	if !proxyAuditTrackerWarm(start, start.Add(2*paidEarnWindow)) {
		t.Fatalf("after two earn windows the tracker is warm")
	}
}

func TestRunProxyAudit_ExitsOnCancelledContext(t *testing.T) {
	withTempHome(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		var mu sync.Mutex
		runProxyAudit(ctx, map[string]context.CancelFunc{}, &mu, false)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runProxyAudit did not exit promptly on a cancelled context")
	}
}

// The systemd STATUS line (what `urnet-tools status` shows on Linux) follows
// proxy audit: the parked count while it holds proxies, the paused note while
// it cannot act, and both clear when it releases or resumes.
func TestProxyAuditRunOnce_FeedsTheSystemdStatusLine(t *testing.T) {
	t.Cleanup(func() { setProxyAuditSystemdState(0, false) })
	h := newGovHarness("a:1")
	h.act = true
	h.twoBadTicks("a:1")
	if proxiesParked.Load() != 1 || proxyAuditPaused.Load() {
		t.Fatalf("one parked proxy expected in the status line state, parked=%d paused=%v", proxiesParked.Load(), proxyAuditPaused.Load())
	}

	h.fileOK = false
	h.now = h.now.Add(5 * time.Minute)
	h.g.runOnce()
	if !proxyAuditPaused.Load() {
		t.Fatalf("a paused audit must show in the status line state")
	}

	h.fileOK = true
	h.act = false // proxy audit off releases everything
	h.now = h.now.Add(5 * time.Minute)
	h.g.runOnce()
	if proxiesParked.Load() != 0 || proxyAuditPaused.Load() {
		t.Fatalf("releasing and resuming must clear the status line state, parked=%d paused=%v", proxiesParked.Load(), proxyAuditPaused.Load())
	}
}

// B5: "not acting" has two different causes with two different fixes, and the
// status must say which.
func TestProxyAuditNotActingReason(t *testing.T) {
	home := withTempHome(t)

	writeProxyAuditMarker(t, home, "off")
	t.Setenv("URNETWORK_HOT_RESTART", "1")
	if got := proxyAuditNotActingReason(false); got != auditReasonAuditOff {
		t.Fatalf("proxy audit off: got %q", got)
	}

	writeProxyAuditMarker(t, home, "on")
	t.Setenv("URNETWORK_HOT_RESTART", "0")
	if got := proxyAuditNotActingReason(false); got != auditReasonHotRestartOff {
		t.Fatalf("proxy audit on, hot restart off: got %q", got)
	}

	t.Setenv("URNETWORK_HOT_RESTART", "1")
	if got := proxyAuditNotActingReason(false); got != "" {
		t.Fatalf("acting has no reason, got %q", got)
	}
}

func TestProxyAuditRunOnce_PublishesWhyItIsNotActing(t *testing.T) {
	currentProxyAuditStatus.Store(nil)
	t.Cleanup(func() { currentProxyAuditStatus.Store(nil) })
	t.Cleanup(func() { setProxyAuditSystemdState(0, false) })
	h := newGovHarness("a:1")
	reason := auditReasonHotRestartOff
	h.g.env.notActingReason = func() string { return reason }

	h.act = false
	h.g.runOnce()
	if st := proxyAuditStatusSnapshot(); st == nil || st.NotActingReason != auditReasonHotRestartOff {
		t.Fatalf("an observing audit must publish why, got %+v", st)
	}

	h.act = true
	reason = ""
	h.g.runOnce()
	if st := proxyAuditStatusSnapshot(); st == nil || st.NotActingReason != "" {
		t.Fatalf("an acting audit has no not-acting reason, got %+v", st)
	}
}

// Re-review #2: while the paid list is unreadable proxy audit cannot judge
// proxies, but time still passes. A park whose backoff has lapsed must stop
// showing as parked (reload relaunches it regardless), and the rolling 24h
// budget must keep aging. What it must NOT do is release parks by presence:
// with no list every address looks "gone", which would release everything.
func TestProxyAuditRunOnce_PausedStillExpiresLapsedParksButNeverReleasesTheRest(t *testing.T) {
	t.Cleanup(func() { setProxyAuditSystemdState(0, false) })
	currentProxyAuditStatus.Store(nil)
	t.Cleanup(func() { currentProxyAuditStatus.Store(nil) })
	h := newGovHarness("a:1", "b:1")
	h.act = true
	// Both proxies get the same two bad grades on the same two ticks.
	for _, a := range []string{"a:1", "b:1"} {
		h.grade(a, 0.1, h.now.Add(-7*time.Hour))
	}
	h.g.runOnce()
	h.now = h.now.Add(6 * time.Hour)
	for _, a := range []string{"a:1", "b:1"} {
		h.grade(a, 0.1, h.now.Add(-time.Hour))
	}
	h.g.runOnce()
	st := proxyAuditStatusSnapshot()
	if st == nil || len(st.Parked) != 2 {
		t.Fatalf("setup: expected both parked, got %+v", st)
	}

	// A day of the list being unreadable: every 6h-ish park has lapsed.
	h.fileOK = false
	h.now = h.now.Add(9 * time.Hour) // past the longest jittered first rung (7.5h)
	h.g.runOnce()
	st = proxyAuditStatusSnapshot()
	if st == nil || !st.Paused {
		t.Fatalf("setup: expected paused, got %+v", st)
	}
	if len(st.Parked) != 0 {
		t.Fatalf("parks whose backoff has lapsed must stop showing as parked while paused, got %+v", st.Parked)
	}
	if proxiesParked.Load() != 0 {
		t.Fatalf("the systemd line must stop counting them too, got %d", proxiesParked.Load())
	}
}

func TestProxyAuditRunOnce_PausedDoesNotReleaseParksThatAreStillInBackoff(t *testing.T) {
	t.Cleanup(func() { setProxyAuditSystemdState(0, false) })
	h := newGovHarness("a:1")
	h.act = true
	h.twoBadTicks("a:1")

	h.fileOK = false
	h.now = h.now.Add(5 * time.Minute)
	h.g.runOnce()

	if h.hist.Eligible("a:1", h.now) {
		t.Fatalf("a park still inside its backoff must survive a paused tick (an unreadable list is not a reason to release)")
	}
	if !h.g.st.isParked("a:1") {
		t.Fatalf("the park record must survive a paused tick")
	}
}

// Re-review #7: releasing a park clears the backoff proxy audit set, not
// whatever else is on the address.
func TestProxyAuditRunOnce_RestoreLeavesALongerBackoffFromSomeoneElseInPlace(t *testing.T) {
	h := newGovHarness("a:1")
	h.act = true
	h.twoBadTicks("a:1") // parked for 6h

	longer := h.now.Add(48 * time.Hour) // e.g. a URL give-up backoff set on top
	h.hist.SetBackoffUntil("a:1", longer)

	h.now = h.now.Add(2 * time.Hour)
	h.grade("a:1", 0.9, h.now.Add(-30*time.Minute)) // healthy regrade after the dwell
	h.healthUp["a:1"] = false
	h.g.runOnce()

	if h.hist.Eligible("a:1", h.now.Add(time.Hour)) {
		t.Fatalf("a restore must not clear a longer backoff that is not proxy audit's")
	}
}

// Re-review #6: the mark is only for proxies that are actually down. A park
// record can outlive its cancel (something relaunched the proxy), and stamping
// "parked" over a live proxy makes proxy.state undercount what is running.
func TestProxyAuditRunOnce_DoesNotMarkAParkedProxyThatIsRunning(t *testing.T) {
	h := newGovHarness("a:1")
	h.act = true
	h.twoBadTicks("a:1")
	marks := len(h.parkedMarks)

	h.now = h.now.Add(5 * time.Minute)
	h.healthUp["a:1"] = true // relaunched behind our back
	e := h.entries["a:1"]
	e.Health = "up"
	h.entries["a:1"] = e
	h.g.runOnce()

	if len(h.parkedMarks) != marks {
		t.Fatalf("a running proxy must not be re-marked parked, marks went %d -> %d", marks, len(h.parkedMarks))
	}
}

// B4: before the first 5 minute tick /metrics and `self-heal status` must
// already say whether proxy audit will act, or a "not acting" alert fires on
// every restart of a box that will.
func TestRunProxyAudit_PublishesAStatusBeforeTheFirstTick(t *testing.T) {
	home := withTempHome(t)
	writeSelfHealMarker(t, home, "on")
	t.Setenv("URNETWORK_HOT_RESTART", "1")
	currentProxyAuditStatus.Store(nil)
	t.Cleanup(func() { currentProxyAuditStatus.Store(nil) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var mu sync.Mutex
	runProxyAudit(ctx, map[string]context.CancelFunc{}, &mu, false)

	st := proxyAuditStatusSnapshot()
	if st == nil {
		t.Fatalf("a status must be published at startup, before the first tick")
	}
	if !st.Acting {
		t.Fatalf("self-heal and hot restart are on, so the startup status must say acting, got %+v", st)
	}
}

// A parked proxy must NOT be marked with a health value that means "safe to
// delete". "inactive" is exactly that (unseen 7+ days): dead-proxy cleanup and
// `proxy remove-dead` both collect it and edit the operator's own proxy file,
// which would permanently remove a proxy proxy audit only meant to rest.
func TestParkedProxyIsNeverRemovedFromTheProxyFileByCleanup(t *testing.T) {
	home := withTempHome(t)
	src := filepath.Join(home, "paid.txt")
	if err := os.WriteFile(src, []byte("1.1.1.1:1080:u:p\n2.2.2.2:1080:u:p\n3.3.3.3:1080:u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// State is keyed by proxy identity, the way reload writes it for a
	// credentialed proxy. 3.3.3.3 is the control: genuinely inactive, so cleanup
	// CAN remove it, which proves the parked proxy survives because it is
	// parked and not because the removal never matches an identity-keyed line.
	parkedKey := identityKey("2.2.2.2:1080", "u")
	err := writeProxyState(&ProxyState{Source: src, StartedAt: auditEpoch.Add(-48 * time.Hour), Proxies: map[string]ProxyEntry{
		identityKey("1.1.1.1:1080", "u"): {ID: 1, Health: "up", Source: "file"},
		parkedKey:                        {ID: 2, Health: "up", Source: "file"},
		identityKey("3.3.3.3:1080", "u"): {ID: 3, Health: "inactive", Source: "file"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	markProxiesParked([]string{parkedKey})

	if removed := runProxyURLCleanupOnce("all"); removed != 1 {
		t.Fatalf("scope=all cleanup removed %d proxies, want exactly the inactive control", removed)
	}
	b, _ := os.ReadFile(src)
	if got, want := string(b), "1.1.1.1:1080:u:p\n2.2.2.2:1080:u:p\n"; got != want {
		t.Fatalf("proxy file = %q, want %q: the inactive control goes, the parked proxy stays", got, want)
	}
}

func TestRemoveDeadCandidatesIgnoreParkedProxies(t *testing.T) {
	state := &ProxyState{StartedAt: auditEpoch.Add(-48 * time.Hour), Proxies: map[string]ProxyEntry{
		"2.2.2.2:1080": {ID: 2, Health: proxyHealthParked, Source: "file"},
	}}
	dead, inactive, degraded, authFailing := collectRemoveDeadCandidates(state, removeDeadOptions{}, 100*time.Hour)
	if len(dead)+len(inactive)+len(degraded)+len(authFailing) != 0 {
		t.Fatalf("`proxy remove-dead` must not collect a parked proxy: dead=%v inactive=%v degraded=%v auth=%v", dead, inactive, degraded, authFailing)
	}
}

// `proxy remove-dead --auth-failures=N` collects any proxy that is not "up" and
// has piled up auth failures, and `--degraded` switches that check on by
// default (250 a day). A parked proxy is not up, and its cumulative failure
// counter can be high from before it was parked, so without an exclusion a
// plain `proxy remove-dead --degraded` in cron deletes it from the operator's
// proxy file.
func TestRemoveDeadCandidatesNeverCollectAParkedProxyForAuthFailures(t *testing.T) {
	state := &ProxyState{StartedAt: auditEpoch.Add(-100 * time.Hour), Proxies: map[string]ProxyEntry{
		"parked:1": {ID: 1, Health: proxyHealthParked, Source: "file", AuthFailures: 100000},
		"offline:1": {ID: 2, Health: "offline", Source: "file", AuthFailures: 100000,
			DownSince: auditEpoch.Add(-72 * time.Hour).Format(time.RFC3339)},
	}}

	for name, o := range map[string]removeDeadOptions{
		"explicit --auth-failures": {authFailMin: 10},
		"implicit via --degraded":  {authFailMin: 250, degradedDur: 24 * time.Hour},
	} {
		_, _, _, authFailing := collectRemoveDeadCandidates(state, o, 100*time.Hour)
		var got []string
		for _, r := range authFailing {
			got = append(got, r.addr)
		}
		for _, addr := range got {
			if addr == "parked:1" {
				t.Fatalf("%s: a parked proxy must never be collected as auth-failing, got %v", name, got)
			}
		}
		// Guard against the fix over-reaching: a real auth-failing proxy that
		// is merely not up is still collected.
		if len(got) != 1 || got[0] != "offline:1" {
			t.Fatalf("%s: the offline auth-failing proxy must still be collected, got %v", name, got)
		}
	}
}

// Every other runOnce test uses govCfg(), which switches the aggregate
// protections off so each test isolates one behavior. This one runs the SHIPPED
// policy end to end on a realistic fleet, so a protection that quietly makes
// proxy audit inert (or unbounded) shows up here.
func TestProxyAuditRunOnce_ShippedPolicyOnARealisticFleet(t *testing.T) {
	addrs := make([]string, 30)
	for i := range addrs {
		addrs[i] = fmt.Sprintf("10.0.0.%d:1080", i+1)
	}
	h := newGovHarness(addrs...)
	h.g.cfg = defaultProxyAuditConfig()
	h.act = true

	// 4 junk proxies (worst first: 0.05, 0.10, 0.15, 0.20), 26 healthy ones.
	scoreOf := func(i int) float64 {
		if i < 4 {
			return 0.05 * float64(i+1)
		}
		return 0.95
	}
	gradeAll := func(at time.Time) {
		for i, a := range addrs {
			h.grade(a, scoreOf(i), at)
		}
	}

	gradeAll(h.now.Add(-7 * time.Hour))
	h.g.runOnce()
	h.now = h.now.Add(6 * time.Hour)
	gradeAll(h.now.Add(-time.Hour))
	h.g.runOnce()

	var cancelled []string
	for _, a := range addrs {
		if *h.called[a] {
			cancelled = append(cancelled, a)
		}
	}
	// 4 are proven junk, but the per-tick cap is 3: the worst three go first.
	if len(cancelled) != 3 {
		t.Fatalf("expected exactly 3 parks (per-tick cap) under the shipped policy, got %v", cancelled)
	}
	for _, want := range addrs[:3] {
		if !*h.called[want] {
			t.Fatalf("the worst three (%v) should be parked, but %s was not; parked %v", addrs[:3], want, cancelled)
		}
	}
	if *h.called[addrs[3]] {
		t.Fatalf("the fourth junk proxy waits for a later tick, it was parked too")
	}
	if h.hist.Eligible(addrs[0], h.now.Add(time.Hour)) {
		t.Fatalf("a parked proxy should be backed off")
	}
}

// Control-socket audit actions (on/off/release) mutate the same park state
// and published status as the ticker's runOnce, so they must serialize with
// it. -race is the detector: this test stays green with the auditor's
// serialization mutex and reports a data race without it.
func TestProxyAuditRunOnceConcurrentWithControlActions(t *testing.T) {
	h := newAuditHarness("audit-concurrent-a:1", "audit-concurrent-b:1")
	h.act = true
	h.fileOK = true

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				h.g.runOnce()
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				// Mirror the control-socket release path: mutate park
				// state and publish under the auditor lock.
				h.g.mu.Lock()
				h.g.st.releaseAll()
				if h.g.st.isParked("audit-concurrent-a:1") {
					h.g.st.release("audit-concurrent-a:1")
				}
				h.g.publish(h.g.env.now(), h.g.env.act(), proxyAuditResult{})
				h.g.mu.Unlock()
			}
		}()
	}
	wg.Wait()
	// A tick after the storm must still complete without panic and publish.
	h.g.runOnce()
	if proxyAuditStatusSnapshot() == nil {
		t.Fatal("no status published after concurrent tick/control storm")
	}
}

// TestEarningCredentialedProxyNotParkEligible pins the identity-keyed earn
// tracker: proxy audit's park veto (stillEligible) asks whether THIS
// IDENTITY earned recently, and an earning credentialed proxy (identity =
// address+user) must not be park-eligible. With the tracker keyed by bare
// address, the identity lookup missed and a live earning proxy read as
// never-earning — parked out of the paid pool. Deterministic: fixed keys,
// no timing dependence (hearing window is checked at the moment of the
// test).
func TestEarningCredentialedProxyNotParkEligible(t *testing.T) {
	t.Cleanup(func() { globalPerProxyEarnTracker.Update(nil) })
	globalPerProxyEarnTracker.Update(nil)

	const addr = "gw.example.com:1080"
	const identityKey = addr + "\x1falice" // ProxySettings.Key() shape

	env := proxyAuditEnv{
		health: func() map[string]connect.ProxyHealthStatus {
			return map[string]connect.ProxyHealthStatus{identityKey: {Health: "up"}}
		},
		clients:        func(string) (int64, bool) { return 0, true },
		earnedRecently: func(a string) bool { return globalPerProxyEarnTracker.EarnedSince(a, paidEarnWindow) },
	}
	g := &proxyAuditor{env: env}

	// Quiet proxy: park-eligible (up, idle, never earned).
	if !g.stillEligible(identityKey) {
		t.Fatal("quiet credentialed proxy should be park-eligible")
	}

	// Now it earns: the identity-keyed tracker entry must veto the park.
	idx := int(earnTrackerTestSeq.Add(1))
	bw := connect.RegisterProxyBandwidth(idx)
	t.Cleanup(func() { connect.UnregisterProxy(idx) })
	globalPerProxyEarnTracker.Update(map[string]*connect.ProxyBandwidth{identityKey: bw})
	bw.BillableRx.Store(1 << 20)
	globalPerProxyEarnTracker.Update(map[string]*connect.ProxyBandwidth{identityKey: bw})

	if g.stillEligible(identityKey) {
		t.Fatal("earning credentialed proxy must not be park-eligible — a bare-address tracker reads it as never-earning")
	}
	// The bare-address lookup must NOT see the earnings either: the
	// identity split is exactly what this guards.
	if globalPerProxyEarnTracker.EarnedSince(addr, paidEarnWindow) {
		t.Fatal("bare-address lookup must not match an identity-keyed earning record")
	}
}
