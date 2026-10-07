package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// withFastDrainPoll shrinks the drain-completion goroutine's poll interval so
// tests that wait for a drain to finish take milliseconds instead of blocking
// on the real 5s interval, and restores it on cleanup.
func withFastDrainPoll(t *testing.T) {
	t.Helper()
	prev := drainPollInterval
	drainPollInterval = time.Millisecond
	t.Cleanup(func() { drainPollInterval = prev })
}

// A trim shed cancels the proxy but must NOT forget it: the trim code and the
// prune pass both say "do not erase grade/health history", so the removal loop
// must keep the state entry of a trim-shed proxy. Dropping it made the proxy
// relaunch (once the cap was raised) under a brand new ID, ungraded, having
// lost its health and downtime history.
func trimFixture(t *testing.T) (*ProxyReloader, []string, *atomic.Int32) {
	return trimFixtureRunning(t, 3)
}

// trimFixtureRunning builds the same three-proxy source with only the first
// `running` proxies launched (in cancelMap), as after a capped startup.
func trimFixtureRunning(t *testing.T, running int) (*ProxyReloader, []string, *atomic.Int32) {
	t.Helper()
	withTempHome(t)
	proxyWarmupDone.Store(true)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	// Credentialed proxies, so the state key is the identity key (addr+user).
	addrs := make([]string, 3)
	baseline := map[string]*connect.ProxySettings{}
	lines := ""
	for i, host := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		a := host + ":1080"
		s := &connect.ProxySettings{Address: a, Auth: &proxy.Auth{User: "u", Password: "p"}}
		addrs[i] = s.Key()
		baseline[s.Key()] = s // recorded auth, as the startup loop seeds it
		lines += a + ":u:p\n"
	}
	file := t.TempDir() + "/proxy.txt"
	if err := os.WriteFile(file, []byte(lines), 0600); err != nil {
		t.Fatal(err)
	}

	var cancelled atomic.Int32
	cancel := context.CancelFunc(func() { cancelled.Add(1) })
	parent, cancelParent := context.WithCancel(context.Background())
	r := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{},
		cancelMapMu: &sync.Mutex{},
		runningAuth: baseline,
		state: &ProxyState{Proxies: map[string]ProxyEntry{
			addrs[0]: {ID: 1, Health: "up", Graded: true, Score: 0.9},
			addrs[1]: {ID: 2, Health: "dead", Graded: true, Score: 0.2},
			addrs[2]: {ID: 3, Health: "offline", Graded: true, Score: 0.4},
		}},
		sourcePath:      file,
		parentCtx:       parent,
		wg:              &sync.WaitGroup{},
		spawnProxy:      func(context.Context, *connect.ProxySettings, bool, bool) { <-parent.Done() },
		drainingProxies: map[string]context.CancelFunc{},
	}
	for _, a := range addrs[:running] {
		r.cancelMap[a] = cancel
	}
	t.Cleanup(func() { cancelParent(); r.wg.Wait() })

	// reload() re-reads the persisted state at the start of every cycle, so the
	// fixture must be on disk, not just in r.state.
	if err := writeProxyState(r.state); err != nil {
		t.Fatal(err)
	}
	return r, addrs, &cancelled
}

// The reload summary's running count excludes the direct transport, like the
// trim receipt's: a direct-running node must not read as one proxy above the
// pool it actually serves. The count is computed once (reloadRunning) and
// reused by the trim section, so this also pins the shared-source refactor.
func TestReloadSummaryRunningCountExcludesDirect(t *testing.T) {
	resetTrimCapSeen()
	t.Cleanup(resetTrimCapSeen)
	r, _, _ := trimFixture(t)
	t.Setenv("DISABLE_DIRECT_IP", "")

	// A direct transport is running (in the cancel map), as after a previous
	// reload with direct enabled. It is not part of the desired set.
	r.cancelMapMu.Lock()
	r.cancelMap[directProxyKey] = func() {}
	r.cancelMapMu.Unlock()

	out := captureTlog(t, func() { r.reload() })
	if !strings.Contains(out, "running=3 desired=3") {
		t.Fatalf("the reload summary must count 3 non-direct running proxies, got:\n%s", out)
	}
	if strings.Contains(out, "running=4") {
		t.Fatalf("the direct transport must not be counted as a running proxy:\n%s", out)
	}
}

// A capped startup already logged and applied the cap via startupTrimSelection
// (main.go). If the reload loop's change-detector is not primed with that same
// cap, the first reload sees it as new and duplicates both the "received" log
// line and the ledger "applied" entry, whose From is a partial mid-ramp
// running count. Priming as startup does must make the first reload silent.
func TestReload_PrimedTrimCapDoesNotDuplicateOnFirstReload(t *testing.T) {
	resetTrimCapSeen()
	t.Cleanup(resetTrimCapSeen)
	r, _, _ := trimFixture(t)

	if err := writeTrimTarget(1); err != nil {
		t.Fatal(err)
	}
	// Mirror what provide() does right after the startup trim block: prime the
	// detector with the cap and source this start already saw and applied.
	trimCap, source, err := effectiveTrimCapSource()
	if err != nil || trimCap != 1 {
		t.Fatalf("effectiveTrimCapSource() = %d, %q, %v; want 1", trimCap, source, err)
	}
	primeTrimCapSeen(trimCap, source)

	out := captureTlog(t, func() { r.reload() })
	if strings.Contains(out, "[proxy][trim] received") {
		t.Fatalf("a primed cap must not be re-acknowledged on the first reload, got:\n%s", out)
	}
	dir, _ := oomCapDir()
	got, _ := ledgerTail(filepath.Join(dir, ledgerFileName), 10)
	if len(got) != 0 {
		t.Fatalf("a primed cap must not write a duplicate ledger entry, got %+v", got)
	}
}

// An OOM cap that relaxes to 0 (see oom_cap.go oomCapOnCleanStart) is an
// automatic decision, not an operator command. The reload's "cleared" ledger
// entry must attribute it to the source that actually bound, not a hard-coded
// "operator".
func TestReload_ClearedOOMCapIsAttributedToOOM(t *testing.T) {
	resetTrimCapSeen()
	t.Cleanup(resetTrimCapSeen)
	r, _, _ := trimFixture(t)
	t.Setenv("URNETWORK_OOM_CAP", "on")

	dir, _ := oomCapDir()
	_ = os.MkdirAll(dir, 0o700)
	if err := oomWriteJSON(filepath.Join(dir, "oom_cap.json"), oomCapState{Cap: 1}); err != nil {
		t.Fatal(err)
	}
	// First reload sees the auto cap bind at 1 and acknowledges it.
	r.reload()

	// The auto cap clears (relaxed to the desired size, or reset directly as
	// here); the reload must log the clear as "oomcap", not "operator".
	if err := oomWriteJSON(filepath.Join(dir, "oom_cap.json"), oomCapState{Cap: 0}); err != nil {
		t.Fatal(err)
	}
	out := captureTlog(t, func() { r.reload() })
	if !strings.Contains(out, "[proxy][trim] received: cap cleared") {
		t.Fatalf("missing cleared receipt, got:\n%s", out)
	}
	got, _ := ledgerTail(filepath.Join(dir, ledgerFileName), 10)
	if len(got) == 0 || got[len(got)-1].Action != "cleared" || got[len(got)-1].Mode != trimCapOOM {
		t.Fatalf("cleared ledger entry mode = %+v, want mode %q", got, trimCapOOM)
	}
}

func TestReload_TrimShedKeepsStateEntries(t *testing.T) {
	r, addrs, cancelled := trimFixture(t)
	if err := writeTrimTarget(1); err != nil {
		t.Fatal(err)
	}
	r.reload()

	if got := cancelled.Load(); got != 2 {
		t.Fatalf("trim to 1 of 3 must cancel exactly 2 proxies, cancelled %d", got)
	}
	for _, shed := range []string{addrs[1], addrs[2]} {
		e, ok := r.state.Proxies[shed]
		if !ok {
			t.Fatalf("trim-shed proxy %s lost its state entry (ID, health, grade history)", shed)
		}
		if !e.Graded || e.Health == "" {
			t.Fatalf("trim-shed proxy %s state entry was mutated: %+v", shed, e)
		}
	}
	if _, ok := r.state.Proxies[addrs[0]]; !ok {
		t.Fatalf("surviving proxy %s lost its state entry", addrs[0])
	}
}

// The operator must see the command was received and what it did: one
// "received" line when the cap first appears and an "applied" line with the
// result, then silence on the reloads that follow with an unchanged cap, and a
// "received ... cleared" line when the cap is removed.
func TestReload_TrimLogsReceiptAndResult(t *testing.T) {
	resetTrimCapSeen()
	t.Cleanup(resetTrimCapSeen)
	r, _, _ := trimFixture(t)

	if err := writeTrimTarget(1); err != nil {
		t.Fatal(err)
	}
	first := captureTlog(t, func() { r.reload() })
	if !strings.Contains(first, "[proxy][trim] received: limiting this provider to 1 running proxies (was none); 3 running now, 3 desired, applying") {
		t.Fatalf("missing receipt line, got:\n%s", first)
	}
	if !strings.Contains(first, "[proxy][trim] applied: the running cap is now 1 — removed 2 lowest-graded running proxies") {
		t.Fatalf("missing result line, got:\n%s", first)
	}
	if !strings.Contains(first, "(cap=1 shed=2 held=") {
		t.Fatalf("applied line must keep the machine tail, got:\n%s", first)
	}

	second := captureTlog(t, func() { r.reload() })
	if strings.Contains(second, "[proxy][trim] received") {
		t.Fatalf("unchanged cap must not be re-acknowledged, got:\n%s", second)
	}

	if err := writeTrimTarget(0); err != nil {
		t.Fatal(err)
	}
	cleared := captureTlog(t, func() { r.reload() })
	if !strings.Contains(cleared, "[proxy][trim] received: cap cleared (was 1)") {
		t.Fatalf("missing cleared line, got:\n%s", cleared)
	}
}

// After a capped startup only `cap` proxies are running and the rest are held.
// The startup reload must keep holding them (not launch them "as additions")
// while the cap stands, and must launch one once the cap is raised.
func TestReload_HeldProxiesStayHeldUntilTheCapRises(t *testing.T) {
	r, addrs, cancelled := trimFixtureRunning(t, 2)
	if err := writeTrimTarget(2); err != nil {
		t.Fatal(err)
	}

	r.reload()
	if _, launched := r.cancelMap[addrs[2]]; launched {
		t.Fatalf("held proxy %s was launched while the cap of 2 is already met", addrs[2])
	}
	if got := cancelled.Load(); got != 0 {
		t.Fatalf("cap already met: nothing may be shed, cancelled %d", got)
	}
	if _, ok := r.state.Proxies[addrs[2]]; !ok {
		t.Fatalf("held proxy lost its state entry")
	}

	if err := writeTrimTarget(3); err != nil {
		t.Fatal(err)
	}
	r.reload()
	if _, launched := r.cancelMap[addrs[2]]; !launched {
		t.Fatalf("with the cap raised to 3 the held proxy %s must be admitted", addrs[2])
	}
}

// A trim-shed proxy with active clients drains instead of being cancelled
// outright. When its last client leaves, the drain completes and the proxy is
// STILL in the desired set (the shed keeps its state on purpose) — but the
// shed must not re-trigger a reload: the cap would only hold it again, and
// each drained shed used to burn a reload cycle plus a false
// "re-added while draining" log line.
func TestReload_DrainedTrimShedProxyDoesNotRetriggerReload(t *testing.T) {
	withFastDrainPoll(t)
	r, addrs, _ := trimFixture(t)
	if err := writeTrimTarget(2); err != nil {
		t.Fatal(err)
	}
	// The drain goroutine re-reads the desired set through proxy.state's Source
	// (currentDesiredProxyIdentities), not r.sourcePath. Without this the shed
	// proxy never reads as still desired, the re-trigger branch is never
	// reached, and the test passes whether or not the trim-shed guard exists.
	r.state.Source = r.sourcePath
	if err := writeProxyState(r.state); err != nil {
		t.Fatal(err)
	}

	// The worst-graded proxy (dead) is the one shed at cap 2; give it an
	// active client so the shed enters the graceful drain path.
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)
	const idx = 500
	connect.RegisterProxy(idx, addrs[1], addrs[1])
	bw := connect.RegisterProxyBandwidth(idx)
	bw.Clients.Store(1)

	path, err := proxyReloadPath()
	if err != nil {
		t.Fatal(err)
	}
	seqBefore, _ := readReloadSeq(path)
	r.reload()
	if _, draining := r.drainingProxies[addrs[1]]; !draining {
		t.Fatalf("shed proxy with active clients must drain, not be cancelled outright")
	}

	// The client leaves; the drain goroutine notices and completes.
	bw.Clients.Store(0)
	waitForDrainToFinish(t, r, addrs[1])

	seqAfter, _ := readReloadSeq(path)
	if seqAfter != seqBefore {
		t.Fatalf("a trim-shed drain re-triggered a reload: seq %d -> %d", seqBefore, seqAfter)
	}
}

// waitForDrainToFinish polls until addr is no longer in r.drainingProxies,
// failing the test if it does not clear in time. Callers should shrink
// drainPollInterval with withFastDrainPoll first so this is fast, not
// wall-clock bound.
func waitForDrainToFinish(t *testing.T, r *ProxyReloader, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.drainMu.Lock()
		_, stillDraining := r.drainingProxies[addr]
		r.drainMu.Unlock()
		if !stillDraining {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("drain of %s did not complete in time", proxyKeyDisplay(addr))
		}
		time.Sleep(time.Millisecond)
	}
}

// The drain-completion re-trigger guard must suppress ONLY trim-shed
// addresses, not every drain while any cap happens to bind. A proxy removed
// for an unrelated reason (a URL-source flap) and then re-added while it
// drains is exactly the case the re-trigger exists for, and it must still fire
// promptly even though the trim cap binds throughout.
func TestReload_ReAddedNonShedProxyStillRetriggersUnderABindingCap(t *testing.T) {
	withFastDrainPoll(t)
	r, addrs, cancelled := trimFixture(t)
	// currentDesiredProxyIdentities() (used by the drain-completion goroutine)
	// reads proxy.state's Source field, not r.sourcePath directly; the fixture
	// leaves Source empty (internal config), so point it at the same file
	// reload() itself reads, and persist it, so "desired" agrees with reload().
	r.state.Source = r.sourcePath
	if err := writeProxyState(r.state); err != nil {
		t.Fatal(err)
	}
	if err := writeTrimTarget(2); err != nil {
		t.Fatal(err)
	}
	r.reload()
	if got := cancelled.Load(); got != 1 {
		t.Fatalf("trim to 2 of 3 must shed exactly 1 (the dead one), cancelled %d", got)
	}

	// A source flap drops addrs[2] from the file (not the trim cap's doing).
	// Give it an active client so it drains instead of being cancelled
	// outright.
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)
	const idx = 501
	connect.RegisterProxy(idx, addrs[2], addrs[2])
	bw := connect.RegisterProxyBandwidth(idx)
	bw.Clients.Store(1)

	if err := os.WriteFile(r.sourcePath, []byte(rewriteTrimFixtureFile(addrs[:2])), 0600); err != nil {
		t.Fatal(err)
	}
	r.reload()
	if _, draining := r.drainingProxies[addrs[2]]; !draining {
		t.Fatalf("addrs[2] with an active client must drain when dropped from the source")
	}
	// The trim cap of 2 is still binding: addrs[0] and addrs[1] (held) fill it.

	// The source flap reverses: addrs[2] is back in the file before its drain
	// completes, so it is desired again -- but it was never trim-shed.
	if err := os.WriteFile(r.sourcePath, []byte(rewriteTrimFixtureFile(addrs)), 0600); err != nil {
		t.Fatal(err)
	}

	path, err := proxyReloadPath()
	if err != nil {
		t.Fatal(err)
	}
	seqBefore, _ := readReloadSeq(path)
	bw.Clients.Store(0)
	waitForDrainToFinish(t, r, addrs[2])

	seqAfter, _ := readReloadSeq(path)
	if seqAfter == seqBefore {
		t.Fatalf("a re-added NON-shed proxy's drain must still re-trigger a reload even under a binding cap: seq stayed at %d", seqBefore)
	}
}

// rewriteTrimFixtureFile renders the given ProxySettings.Key() identities back
// into the plain "host:port:user:password" lines trimFixture's source file
// uses. trimFixture always uses user "u" password "p".
func rewriteTrimFixtureFile(addrs []string) string {
	out := ""
	for _, a := range addrs {
		host, _ := connect.SplitProxyKey(a)
		out += host + ":u:p\n"
	}
	return out
}

// events.log writes are an open+write+fsync. reload() must not do them while
// holding r.mu, or a slow disk stalls every other reload-path caller. Asserted
// from inside critLog's critical section (a test seam), so it is exact, and the
// lines must still reach events.log once the reload returns.
func TestReload_WritesEventsLogAfterReleasingTheReloaderLock(t *testing.T) {
	resetTrimCapSeen()
	t.Cleanup(resetTrimCapSeen)
	r, _, _ := trimFixture(t)
	if err := writeTrimTarget(1); err != nil {
		t.Fatal(err)
	}

	var calls, heldDuring int
	critLogInCriticalSection = func() {
		calls++
		// TryLock fails when reload still holds r.mu.
		if r.mu.TryLock() {
			r.mu.Unlock()
			return
		}
		heldDuring++
	}
	t.Cleanup(func() { critLogInCriticalSection = nil })

	r.reload()

	if calls == 0 {
		t.Fatal("the trim cap change wrote nothing to events.log")
	}
	if heldDuring != 0 {
		t.Fatalf("%d of %d events.log writes ran while reload held r.mu", heldDuring, calls)
	}
	p, err := critLogPath()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || !strings.Contains(string(b), "[proxy][trim] received") {
		t.Fatalf("events.log must still get the trim receipt: err=%v content=%q", err, string(b))
	}
}

// The action ledger is a blocking flock plus a rewrite of a file that can reach
// 256 KiB. reload() recorded its two trim entries while still holding r.mu, so a
// contended or slow ledger write stalled every other reload-path caller, not
// just this one. Asserted from inside ledgerRecord (a test seam), so it is
// exact, and the entries must still land once the reload returns.
func TestReload_WritesTheLedgerAfterReleasingTheReloaderLock(t *testing.T) {
	resetTrimCapSeen()
	t.Cleanup(resetTrimCapSeen)
	r, _, _ := trimFixture(t)
	if err := writeTrimTarget(1); err != nil {
		t.Fatal(err)
	}

	var calls, heldDuring int
	ledgerRecordHook = func() {
		calls++
		// TryLock fails when reload still holds r.mu.
		if r.mu.TryLock() {
			r.mu.Unlock()
			return
		}
		heldDuring++
	}
	t.Cleanup(func() { ledgerRecordHook = nil })

	r.reload()

	if calls == 0 {
		t.Fatal("the trim cap change wrote nothing to the ledger")
	}
	if heldDuring != 0 {
		t.Fatalf("%d of %d ledger writes ran while reload held r.mu", heldDuring, calls)
	}
	dir, _ := oomCapDir()
	got, _ := ledgerTail(filepath.Join(dir, ledgerFileName), 10)
	if len(got) == 0 || got[len(got)-1].Action != "applied" {
		t.Fatalf("the ledger must still get the trim entry, got %+v", got)
	}
}

// While desired exceeds the cap the held proxies re-enter the budget on every
// reload, so "held N additions" is true every cycle. That is not news: the
// durable events.log and the important buffer must get the applied line when
// the cap changes or running proxies are shed, not once per reload (hourly
// reconcile, every slow-auth reload, every drain re-trigger) for the life of
// the cap.
func TestReload_HeldAdditionsDoNotRepeatTheDurableAppliedLine(t *testing.T) {
	resetTrimCapSeen()
	t.Cleanup(resetTrimCapSeen)
	r, _, _ := trimFixtureRunning(t, 2) // capped startup: 2 running, 1 held
	if err := writeTrimTarget(2); err != nil {
		t.Fatal(err)
	}
	countApplied := func() int {
		p, err := critLogPath()
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return 0
		}
		return strings.Count(string(b), "[proxy][trim] applied")
	}

	r.reload() // the cap first appears: a change, logged durably once
	afterFirst := countApplied()
	if afterFirst != 1 {
		t.Fatalf("the cap change must write exactly one durable applied line, got %d", afterFirst)
	}

	var ram string
	for i := 0; i < 3; i++ {
		ram += captureTlog(t, func() { r.reload() })
	}
	if got := countApplied(); got != afterFirst {
		t.Fatalf("held additions with an unchanged cap wrote %d more durable applied lines, want 0", got-afterFirst)
	}
	if !strings.Contains(ram, "[proxy][trim] applied: the running cap is now 2 — nothing to remove; holding 1 additions") {
		t.Fatalf("the held-additions state must stay visible on the RAM log, got:\n%s", ram)
	}
}
