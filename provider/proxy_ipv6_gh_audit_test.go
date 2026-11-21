package main

import (
	"context"
	"net"
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
	time.Sleep(50 * time.Millisecond)
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
