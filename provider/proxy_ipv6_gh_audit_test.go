package main

import (
	"context"
	"fmt"
	"github.com/urnetwork/connect"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// Findings of the GitHub review of the IPv6 stack, each pinned by a test that
// fails without its fix.

// A refused CONNECT counts as a yes, so on the TCP only path a result with no
// measured TCP exit is not evidence that IPv6 TCP works.
func TestProxyIPv6TcpOnlyPathNeedsAMeasuredTcpExit(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Hour)
	noRelay := proxyUDPResult{Status: proxyUDPNoRelay}
	withExit := v6Result(proxyIPv6Yes, "unknown", recent, 12*time.Hour)
	if !proxyIPv6TransportWanted(noRelay, withExit, now) {
		t.Fatal("a TCP only proxy with a measured IPv6 TCP exit must qualify")
	}
	noExit := withExit
	noExit.TCPExit, noExit.UDPExit = "", ""
	if proxyIPv6TransportWanted(noRelay, noExit, now) {
		t.Fatal("a refused CONNECT with no TCP exit qualified a TCP only proxy")
	}
}

// A first qualifying result that expired before a second one came is not
// pending evidence: one fresh result after the gap does not start the transport.
func TestProxyIPv6GateDropsAPendingStartWhenItsEvidenceExpires(t *testing.T) {
	var gate proxyIPv6Gate
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	okUDP := proxyUDPResult{Status: proxyUDPOK}
	first := v6Result(proxyIPv6Yes, "yes", t0, 12*time.Hour)
	if start, _ := gate.observe(t0, okUDP, first); start {
		t.Fatal("started on the first result")
	}
	// the first result expires (twice its interval) and is polled again
	if start, _ := gate.observe(t0.Add(25*time.Hour), okUDP, first); start {
		t.Fatal("started on an expired result")
	}
	t1 := t0.Add(26 * time.Hour)
	if start, _ := gate.observe(t1, okUDP, v6Result(proxyIPv6Yes, "yes", t1, 12*time.Hour)); start {
		t.Fatal("one fresh result after the gap started the transport on a stale streak")
	}
	t2 := t1.Add(13 * time.Hour)
	if start, _ := gate.observe(t2, okUDP, v6Result(proxyIPv6Yes, "yes", t2, 12*time.Hour)); !start {
		t.Fatal("two fresh results did not start it")
	}
}

// ipv6UDPAllowed admits IPv6 UDP on a yes and requires the UDP exit to be the
// TCP exit, so a persisted record that says yes with two exits is not valid.
func TestProxyIPv6ValidRequiresMatchedExitsForAUdpYes(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour)
	matched := v6Result(proxyIPv6Yes, "yes", at, 12*time.Hour)
	if !matched.valid(now) {
		t.Fatal("a matched yes record was rejected")
	}
	spelled := matched
	spelled.UDPExit = "2001:0db8:0:0:0:0:0:1"
	if !spelled.valid(now) {
		t.Fatal("the same address spelled another way was rejected, the parsed addresses must be compared")
	}
	differ := matched
	differ.UDPExit = "2001:db8::2"
	if differ.valid(now) {
		t.Fatal("a record that says yes with two different exits was accepted")
	}
	missing := matched
	missing.UDPExit = ""
	if missing.valid(now) {
		t.Fatal("a record that says yes with no UDP exit was accepted")
	}
	mismatch := differ
	mismatch.UDP = "mismatch"
	if !mismatch.valid(now) {
		t.Fatal("a mismatch record keeps its two exits and is valid")
	}
}

// A SOCKS5 reply has a reserved byte that must be zero.
func TestReadSocks5ConnectCodeRejectsANonzeroReservedByte(t *testing.T) {
	reply := func(reserved byte) net.Conn {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			server.Write([]byte{5, 0, reserved, 1, 192, 0, 2, 1, 0, 80})
		}()
		return client
	}
	conn := reply(0)
	code, err := readSocks5ConnectCode(conn)
	conn.Close()
	if err != nil || code != 0 {
		t.Fatalf("a valid reply: code %d, %v", code, err)
	}
	conn = reply(1)
	if _, err := readSocks5ConnectCode(conn); err == nil {
		t.Fatal("a reply with a nonzero reserved byte was accepted")
	}
	conn.Close()
}

type writeDeadlineSTUNConn struct {
	*ipv6STUNPacketConn
	deadline       time.Time
	setBeforeWrite bool
}

func (c *writeDeadlineSTUNConn) SetWriteDeadline(t time.Time) error {
	c.deadline = t
	return nil
}

func (c *writeDeadlineSTUNConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.setBeforeWrite = !c.deadline.IsZero()
	return c.ipv6STUNPacketConn.WriteTo(p, addr)
}

// The STUN send is bounded by the same deadline as the read, so a full local
// send buffer cannot hold the probe past its context.
func TestProxyIPv6UDPExitBoundsTheSend(t *testing.T) {
	pc := &writeDeadlineSTUNConn{ipv6STUNPacketConn: &ipv6STUNPacketConn{mapped: net.ParseIP("2001:db8::42")}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	limit, _ := ctx.Deadline()
	dst := &net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 3478}
	if _, err := proxyIPv6UDPExit(ctx, pc, dst); err != nil {
		t.Fatal(err)
	}
	if !pc.setBeforeWrite {
		t.Fatal("the send had no write deadline")
	}
	if pc.deadline.After(limit) {
		t.Fatalf("the write deadline %s is past the context deadline %s", pc.deadline, limit)
	}
}

// Switching the probe off while a refresh waits for a slot starts no
// measurement and records no failed one.
func TestIPv6RefreshWaitingForASlotHonorsAProbeSwitchedOff(t *testing.T) {
	h := newIPv6CheckHarness(t)
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ctx := withIPv6Sweep(context.Background())
	// the UDP result is stored without the IPv6 stage (no sweep context)
	h.store.Check(context.Background(), settings, now)
	if h.ipv6Runs.Load() != 0 {
		t.Fatal("the stage ran in a check outside the sweep")
	}
	// every slot is taken, so the refresh waits
	for i := 0; i < cap(h.store.slots); i++ {
		h.store.slots <- struct{}{}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.store.refreshIPv6(ctx, settings, settings.Key(), now)
	}()
	// wait for the refresh to take its reservation while every slot is held,
	// so the switch-off below lands after the initial enable check and the test
	// cannot pass by taking the early return
	reserved := func() bool {
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		entry := h.store.cachedEntryLocked(settings.Key())
		return entry != nil && entry.ipv6Running
	}
	deadline := time.Now().Add(5 * time.Second)
	for !reserved() {
		if time.Now().After(deadline) {
			t.Fatal("the refresh never took its reservation")
		}
		time.Sleep(time.Millisecond)
	}
	if err := globalControlState.set("proxy_ipv6_probe", "off"); err != nil {
		t.Fatal(err)
	}
	<-h.store.slots
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh did not finish after a slot opened")
	}
	if got := h.ipv6Runs.Load(); got != 0 {
		t.Fatalf("the probe ran %d times after it was switched off", got)
	}
	h.store.mu.Lock()
	entry := h.store.entries[settings.Key()]
	retry, running := entry.ipv6RetryAt, entry.ipv6Running
	h.store.mu.Unlock()
	if !retry.IsZero() || running {
		t.Fatalf("a skipped refresh left a wait (%v) or the running flag (%v)", retry, running)
	}
}

type deadlineRecordingConn struct {
	net.Conn
	deadlines []time.Time
}

func (c *deadlineRecordingConn) SetDeadline(t time.Time) error {
	c.deadlines = append(c.deadlines, t)
	return c.Conn.SetDeadline(t)
}

// After CONNECT succeeds the HTTP exchange gets a deadline of its own. The
// deadline set before the greeting would otherwise expire the GET when the
// greeting and CONNECT were slow.
func TestProxyIPv6HTTPExitGetsItsOwnDeadlineAfterConnect(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go func() {
		defer server.Close()
		buffer := make([]byte, 64)
		server.Read(buffer[:3])
		time.Sleep(300 * time.Millisecond) // a slow greeting
		server.Write([]byte{5, 0})
		server.Read(buffer[:22])
		time.Sleep(300 * time.Millisecond) // a slow CONNECT
		server.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
		request := make([]byte, 512)
		server.Read(request)
		server.Write([]byte("HTTP/1.0 200 OK\r\nContent-Length: 12\r\n\r\n2001:db8::42"))
	}()
	conn := &deadlineRecordingConn{Conn: client}
	settings := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
	exit, err := proxyIPv6HTTPExitOnConn(context.Background(), conn, settings, net.ParseIP("2001:db8::1"))
	if err != nil || exit != "2001:db8::42" {
		t.Fatalf("exit %q: %v", exit, err)
	}
	if len(conn.deadlines) < 2 {
		t.Fatalf("the exchange set %d deadlines, want a new one after CONNECT", len(conn.deadlines))
	}
	first, last := conn.deadlines[0], conn.deadlines[len(conn.deadlines)-1]
	if !last.After(first.Add(500 * time.Millisecond)) {
		t.Fatalf("the exchange deadline %s was not renewed past the pre greeting one %s", last, first)
	}
}

// The stage budget can run out in the UDP exit check after IPv6 TCP and its
// exit were established. That evidence is kept as a yes with UDP unknown. A run
// the caller cancelled still keeps the previous result.
func TestRunProxyIPv6StageKeepsTcpEvidenceWhenTheBudgetRunsOut(t *testing.T) {
	settings := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	previous := proxyIPv6Result{}
	blockedUdp := func(ctx context.Context, _ *connect.ProxySettings) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	dials := newIPv6StageDials(0, nil, nil, "2001:db8::1", nil, "", nil).dials
	dials.udpExit = blockedUdp

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result, measured := runProxyIPv6Stage(ctx, settings, previous, now, dials)
	if !measured {
		t.Fatal("a budget that ran out after the TCP evidence discarded it")
	}
	if result.Status != proxyIPv6Yes || result.UDP != "unknown" || result.TCPExit != "2001:db8::1" || result.UDPExit != "" {
		t.Fatalf("result = %+v, want yes with the TCP exit and UDP unknown", result)
	}

	cancelled, cancelNow := context.WithCancel(context.Background())
	dials.udpExit = func(ctx context.Context, _ *connect.ProxySettings) (string, error) {
		cancelNow()
		<-ctx.Done()
		return "", ctx.Err()
	}
	if _, measured := runProxyIPv6Stage(cancelled, settings, previous, now, dials); measured {
		t.Fatal("a cancelled run recorded a measurement")
	}
}

// A refresh whose entry was replaced while it ran (capacity pruning evicted it
// and a later check recreated it with a refresh of its own) must not clear the
// flag of the new reservation or publish its result into the new entry.
func TestIPv6RefreshDoesNotTouchAReplacedEntry(t *testing.T) {
	h := newIPv6CheckHarness(t)
	settings := ipv6TestSettings()
	key := settings.Key()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	started := make(chan struct{})
	release := make(chan struct{})
	h.store.ipv6Probe = func(_ context.Context, _ *connect.ProxySettings, previous proxyIPv6Result, at time.Time, _ proxyIPv6Dials) (proxyIPv6Result, bool) {
		close(started)
		<-release
		return previous.measured(at, proxyIPv6Yes, "yes", "2001:db8::1", "2001:db8::1"), true
	}
	h.store.Check(context.Background(), settings, now)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.store.refreshIPv6(withIPv6Sweep(context.Background()), settings, key, now)
	}()
	<-started
	// the entry is replaced by one whose own refresh holds another reservation
	h.store.mu.Lock()
	replacement := &proxyUDPCheckEntry{result: proxyUDPResult{Status: proxyUDPOK}, at: now, ipv6Running: true, ipv6Token: 1 << 40}
	h.store.entries[key] = replacement
	h.store.mu.Unlock()
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh did not finish")
	}
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	if !replacement.ipv6Running {
		t.Fatal("the first refresh cleared the flag of the refresh that holds the new reservation")
	}
	if !replacement.ipv6.At.IsZero() {
		t.Fatalf("the first refresh published its result into the replacement: %+v", replacement.ipv6)
	}
}

// An entry that holds an IPv6 reservation is not evicted, by age or by the
// capacity bound, because its refresh would publish into a replacement.
func TestPruneKeepsAnEntryWithAnIPv6Reservation(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	old := now.Add(-1000 * time.Hour)
	store := newProxyUDPCheckStore(okProbe)
	store.entries["reserved"] = &proxyUDPCheckEntry{at: old, result: proxyUDPResult{Status: proxyUDPNoRelay}, ipv6Running: true, ipv6Token: 1}
	store.entries["idle"] = &proxyUDPCheckEntry{at: old, result: proxyUDPResult{Status: proxyUDPNoRelay}}
	store.prune(now)
	if store.entries["reserved"] == nil {
		t.Fatal("age pruning evicted an entry that holds a reservation")
	}
	if store.entries["idle"] != nil {
		t.Fatal("age pruning kept an idle old entry")
	}

	capped := newProxyUDPCheckStore(okProbe)
	capped.entries["reserved"] = &proxyUDPCheckEntry{at: old.Add(-time.Hour), result: proxyUDPResult{Status: proxyUDPNoRelay}, ipv6Running: true, ipv6Token: 1}
	for i := 0; i < 4*udpStateMaxEntries+2; i++ {
		capped.entries[fmt.Sprintf("%05d", i)] = &proxyUDPCheckEntry{at: now.Add(-time.Duration(4*udpStateMaxEntries+2-i) * time.Second), result: proxyUDPResult{Status: proxyUDPNoRelay}}
	}
	capped.lastPrune = now
	capped.prune(now)
	if capped.entries["reserved"] == nil {
		t.Fatal("capacity pruning evicted the oldest entry although it holds a reservation")
	}
}

// An IPv6 refresh that finishes during a UDP recheck sees a placeholder entry
// with no UDP result. Persisting it would replace the last completed UDP
// result on disk with that placeholder.
func TestIPv6RefreshDuringAUdpRecheckKeepsThePersistedUdpResult(t *testing.T) {
	h := newIPv6CheckHarness(t)
	h.store.persistPath = filepath.Join(t.TempDir(), "udp.json")
	settings := ipv6TestSettings()
	key := settings.Key()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	h.ipv6OK = true
	h.store.Check(context.Background(), settings, now)
	h.store.mu.Lock()
	persisted := h.store.persisted[udpPersistKey(key)]
	completed := h.store.entries[key]
	placeholder := &proxyUDPCheckEntry{done: make(chan struct{}), ipv6: completed.ipv6, tcpExit: completed.tcpExit}
	h.store.entries[key] = placeholder
	h.store.mu.Unlock()
	if persisted.Status != proxyUDPOK {
		t.Fatalf("the completed UDP result was not persisted: %+v", persisted)
	}

	h.store.refreshIPv6(withIPv6Sweep(context.Background()), settings, key, now)
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	if got := h.store.persisted[udpPersistKey(key)]; got.Status != proxyUDPOK || got.At.IsZero() {
		t.Fatalf("the refresh replaced the persisted UDP result with a placeholder: %+v", got)
	}
	if placeholder.ipv6.At.IsZero() {
		t.Fatal("the refresh did not record its result on the entry")
	}
}

// The first qualifying result expires at twice its interval. A replacement that
// arrives after that, with no poll in between, does not complete the start. One
// that arrives before it does.
func TestProxyIPv6GateExpiresPendingEvidenceBeforeCountingAReplacement(t *testing.T) {
	okUDP := proxyUDPResult{Status: proxyUDPOK}
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	yes := func(at time.Time) proxyIPv6Result { return v6Result(proxyIPv6Yes, "yes", at, 12*time.Hour) }

	var late proxyIPv6Gate
	late.observe(t0, okUDP, yes(t0))
	t1 := t0.Add(26 * time.Hour)
	if start, _ := late.observe(t1, okUDP, yes(t1)); start {
		t.Fatal("a result that replaced an expired one started the transport")
	}
	t2 := t1.Add(13 * time.Hour)
	if start, _ := late.observe(t2, okUDP, yes(t2)); !start {
		t.Fatal("two fresh results did not start it")
	}

	var inTime proxyIPv6Gate
	inTime.observe(t0, okUDP, yes(t0))
	t3 := t0.Add(13 * time.Hour)
	if start, _ := inTime.observe(t3, okUDP, yes(t3)); !start {
		t.Fatal("a second result before the first expired did not start it")
	}
}
