package main

import (
	"errors"
	"net"
	"testing"
	"time"
)

type feedbackPacket struct {
	from net.Addr
	err  error
}
type feedbackPacketConn struct{ packets []feedbackPacket }

func (f *feedbackPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	if len(f.packets) == 0 {
		return 0, nil, errors.New("empty")
	}
	v := f.packets[0]
	f.packets = f.packets[1:]
	if v.err != nil {
		return 0, nil, v.err
	}
	p[0] = 1
	return 1, v.from, nil
}
func (*feedbackPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) { return len(p), nil }
func (*feedbackPacketConn) Close() error                              { return nil }
func (*feedbackPacketConn) LocalAddr() net.Addr                       { return &net.UDPAddr{} }
func (*feedbackPacketConn) SetDeadline(time.Time) error               { return nil }
func (*feedbackPacketConn) SetReadDeadline(time.Time) error           { return nil }
func (*feedbackPacketConn) SetWriteDeadline(time.Time) error          { return nil }

func TestUDPFlowReplyFeedback(t *testing.T) {
	now := time.Unix(1000000, 0)
	store := newProxyUDPCheckStore(nil)
	dest := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 53}
	wrong := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 2), Port: 53}
	pc := &feedbackPacketConn{packets: []feedbackPacket{{from: wrong}, {err: errors.New("read failure")}, {from: dest}, {from: dest}}}
	calls := 0
	conn := &proxiedUdpConn{relayed: pc, destination: dest, onReply: func() { calls++; store.noteFlowReply("key", now) }}
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil || calls != 0 {
		t.Fatal("unrelated packet or read error counted as a reply")
	}
	if got := store.coverage([]string{"key"}, now); got.Unchecked != 1 {
		t.Fatalf("premature coverage: %+v", got)
	}
	for i := 0; i < 2; i++ {
		if _, err := conn.Read(buf); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("callbacks = %d", calls)
	}
	if got := store.coverage([]string{"key"}, now); got.Unknown != 1 || got.OK != 0 {
		t.Fatalf("coverage: %+v", got)
	}
}

func TestUDPFlowReplyPreservesCheck(t *testing.T) {
	now := time.Unix(1000000, 0)
	for _, status := range []proxyUDPStatus{proxyUDPOK, proxyUDPNoRelay, proxyUDPExitMismatch, proxyUDPUnknown} {
		store := newProxyUDPCheckStore(nil)
		store.entries["key"] = &proxyUDPCheckEntry{result: proxyUDPResult{Status: status}, at: now}
		store.noteFlowReply("key", now.Add(time.Second))
		if got := store.entries["key"].result.Status; got != status {
			t.Fatalf("status %v changed to %v", status, got)
		}
	}
	store := newProxyUDPCheckStore(nil)
	unknown := &proxyUDPCheckEntry{result: proxyUDPResult{Status: proxyUDPUnknown}, at: now}
	store.entries["unknown"] = unknown
	store.noteFlowReply("unknown", now.Add(time.Second))
	store.noteFlowReply("unknown", now.Add(2*time.Second))
	if !unknown.flowReply || !unknown.at.Equal(now.Add(time.Second)) {
		t.Fatal("reply did not refresh unknown once")
	}
	running := &proxyUDPCheckEntry{done: make(chan struct{})}
	store.entries["key"] = running
	store.noteFlowReply("key", now)
	if store.entries["key"] != running {
		t.Fatal("in-flight check replaced")
	}
	store.entries["key"] = &proxyUDPCheckEntry{result: proxyUDPResult{Status: proxyUDPOK}, at: now.Add(-proxyUDPOKTTL)}
	store.noteFlowReply("key", now)
	if got := store.coverage([]string{"key"}, now); got.Unknown != 1 {
		t.Fatalf("expired coverage: %+v", got)
	}
}
