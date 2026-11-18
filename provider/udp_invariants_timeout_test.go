package main

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestUDPInvariantCheckTimeoutNeverFallsBack(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	store := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		close(started)
		<-release
		return proxyUDPResult{Status: proxyUDPNoRelay}
	})
	defer close(release)
	var direct atomic.Int32
	cs := connect.DefaultConnectSettings()
	cs.DialContextSettings = &connect.DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) {
		direct.Add(1)
		return nil, errors.New("unexpected host dial")
	}}
	installProxiedUdpEgress(cs, &connect.ProxySettings{Network: "tcp", Address: "127.0.0.1:1"}, store)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	conn, err := cs.DialContext(ctx, "udp", "192.0.2.1:53")
	if conn != nil {
		conn.Close()
		t.Fatal("timed-out check returned a connection")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline", err)
	}
	select {
	case <-started:
	default:
		t.Fatal("check was not started")
	}
	if direct.Load() != 0 {
		t.Fatalf("host dials = %d", direct.Load())
	}
}

func TestUDPInvariantSilentHandshakeTimeoutNeverFallsBack(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		close(accepted)
		_, _ = io.Copy(io.Discard, conn)
		close(closed)
	}()
	var direct atomic.Int32
	cs := connect.DefaultConnectSettings()
	cs.DialContextSettings = &connect.DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) {
		direct.Add(1)
		return nil, errors.New("unexpected host dial")
	}}
	settings := &connect.ProxySettings{Network: "tcp", Address: listener.Addr().String()}
	installProxiedUdpEgress(cs, settings, stubUdpCheckStore(proxyUDPOK))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	conn, err := cs.DialContext(ctx, "udp", "192.0.2.1:53")
	if conn != nil {
		conn.Close()
		t.Fatal("silent proxy returned a connection")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline", err)
	}
	select {
	case <-accepted:
	default:
		t.Fatal("proxy was not contacted")
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("control connection was not closed")
	}
	if direct.Load() != 0 {
		t.Fatalf("host dials = %d", direct.Load())
	}
}
