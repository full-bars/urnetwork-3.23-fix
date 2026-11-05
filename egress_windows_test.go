//go:build windows

package connect

import (
	"errors"
	"net"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// Dual-stack sockets accept the IPv4 option even if IPv6 was never pinned.
func TestEgressIPv6Pin(t *testing.T) {
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	var index uint32
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 && iface.Flags&net.FlagUp != 0 {
			index = uint32(iface.Index)
			break
		}
	}
	if index == 0 {
		t.Fatal("no active loopback interface")
	}
	saved4, saved6 := EgressInterfaceIndex()
	defer SetEgressInterfaceIndex(saved4, saved6)
	for _, indexes := range [][2]uint32{{index, 0}, {0, index}, {index, index}} {
		SetEgressInterfaceIndex(indexes[0], indexes[1])
		var readErr error
		var pinned int
		var called bool
		dialer := net.Dialer{Control: func(network, address string, raw syscall.RawConn) error {
			called = true
			if err := egressControl(network, address, raw); err != nil {
				return err
			}
			return raw.Control(func(fd uintptr) {
				pinned, readErr = windows.GetsockoptInt(windows.Handle(fd), windows.IPPROTO_IPV6, ipv6UnicastIf)
			})
		}}
		conn, err := dialer.Dial("udp", "[::1]:9")
		if err != nil {
			t.Fatalf("indexes %v: dial: %v", indexes, err)
		}
		conn.Close()
		if !called || readErr != nil || pinned != int(index) {
			t.Fatalf("indexes %v: called=%t read=%v pinned=%d want=%d", indexes, called, readErr, pinned, index)
		}
	}
}

func loopbackIndexForTest(t *testing.T) uint32 {
	t.Helper()
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 && iface.Flags&net.FlagUp != 0 {
			return uint32(iface.Index)
		}
	}
	t.Fatal("no active loopback interface")
	return 0
}

// The H3 shape: a wildcard AF_INET6 socket pinned after creation through
// applyEgress, which has no destination address. When the IPv6 pin fails the
// failure must reach the caller so it can mark the socket unpinned and log it.
// A dual-stack socket still accepts the IPv4 option, which is the case where
// dropping the error would leave the socket following the route table.
func TestEgressIPv6PinFailureSurfacesOnWildcardSocket(t *testing.T) {
	good := loopbackIndexForTest(t)
	// no interface has this index, so the IPv6 option is rejected
	const bad = 0x7ffffff0
	saved4, saved6 := EgressInterfaceIndex()
	defer SetEgressInterfaceIndex(saved4, saved6)
	SetEgressInterfaceIndex(good, bad)

	for _, network := range []string{"udp", "udp6"} {
		conn, err := net.ListenUDP(network, &net.UDPAddr{IP: net.IPv6zero})
		if err != nil {
			t.Fatalf("%s: %v", network, err)
		}
		err = applyEgress(conn)
		conn.Close()
		if err == nil {
			t.Fatalf("%s: applyEgress returned nil for an AF_INET6 socket whose IPv6 pin failed", network)
		}
		// only the dual-stack socket accepts the IPv4 option; the v6-only one
		// fails both options and reports the generic pin error
		if network == "udp" && !errors.Is(err, errEgressIpv6Unpinned) {
			t.Fatalf("%s: error does not wrap errEgressIpv6Unpinned: %v", network, err)
		}
	}
}
