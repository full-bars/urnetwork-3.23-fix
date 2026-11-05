//go:build windows

package connect

import (
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
