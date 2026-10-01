package connect

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// RegisterProxyBandwidth deliberately creates an entry for an unknown index
// (the provider registers bandwidth before the proxy). The lookup used by
// logging and by the dial path must not: after UnregisterProxy it recreated
// an address-less entry that nothing would ever remove.
func TestRegisteredProxyBandwidthNeverCreatesAnEntry(t *testing.T) {
	ResetProxyHealthForTesting()
	if bw := RegisteredProxyBandwidth(42); bw != nil {
		t.Fatalf("unregistered index returned %v, want nil", bw)
	}
	if got := ProxyHealthCount(); got != 0 {
		t.Fatalf("ProxyHealthCount = %d, want 0 after a lookup of an unknown index", got)
	}
}

func TestRegisteredProxyBandwidthReturnsLiveEntry(t *testing.T) {
	ResetProxyHealthForTesting()
	RegisterProxy(7, "1.2.3.4:1080", "1.2.3.4:1080")
	want := RegisterProxyBandwidth(7)
	if got := RegisteredProxyBandwidth(7); got != want {
		t.Fatalf("lookup returned a different bandwidth than the registered one")
	}
	if got := ProxyHealthCount(); got != 1 {
		t.Fatalf("ProxyHealthCount = %d, want 1", got)
	}
}

func TestDialerStringAfterUnregisterDoesNotRecreateEntry(t *testing.T) {
	ResetProxyHealthForTesting()
	RegisterProxy(9, "5.6.7.8:1080", "5.6.7.8:1080")
	RegisterProxyBandwidth(9)
	UnregisterProxy(9)

	d := &clientDialer{settings: &ClientStrategySettings{ProxySettings: &ProxySettings{Index: 9, Address: "5.6.7.8:1080"}}}
	if s := d.String(); !strings.Contains(s, "proxy[9]") {
		t.Fatalf("String() = %q, want it to still describe proxy[9]", s)
	}
	if got := ProxyHealthCount(); got != 0 {
		t.Fatalf("ProxyHealthCount = %d, want 0: String() recreated an unregistered proxy", got)
	}
}

// A dial that completes after its proxy was unregistered (draining goroutine)
// must not recreate the entry either.
func TestDialAfterUnregisterDoesNotRecreateEntry(t *testing.T) {
	ResetProxyHealthForTesting()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 262)
		if _, err := io.ReadAtLeast(c, buf[:3], 3); err != nil { // greeting: ver, nmethods, method
			return
		}
		c.Write([]byte{5, 0})
		if _, err := c.Read(buf); err != nil { // CONNECT request
			return
		}
		c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
		io.Copy(io.Discard, c)
	}()

	ps := &ProxySettings{Index: 11, Network: "tcp", Address: ln.Addr().String()}
	RegisterProxy(11, ps.Address, ps.Address)
	RegisterProxyBandwidth(11)
	UnregisterProxy(11)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := ps.NewDialContext(ctx, &net.Dialer{})(ctx, "tcp", "127.0.0.1:80")
	if err != nil {
		t.Fatalf("dial through the fake SOCKS5 proxy: %v", err)
	}
	conn.Close()
	if got := ProxyHealthCount(); got != 0 {
		t.Fatalf("ProxyHealthCount = %d, want 0: the dial path recreated an unregistered proxy", got)
	}
}
