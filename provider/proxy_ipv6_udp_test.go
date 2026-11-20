package main

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"
)

type ipv6STUNPacketConn struct {
	destination *net.UDPAddr
	mapped      net.IP
	request     []byte
	read        bool
}

func (c *ipv6STUNPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	if c.read {
		return 0, nil, errors.New("no more packets")
	}
	c.read = true
	response := make([]byte, 44)
	binary.BigEndian.PutUint16(response[0:2], stunBindingSuccess)
	binary.BigEndian.PutUint16(response[2:4], 24)
	binary.BigEndian.PutUint32(response[4:8], stunMagicCookie)
	copy(response[8:20], c.request[8:20])
	binary.BigEndian.PutUint16(response[20:22], stunAttrMapped)
	binary.BigEndian.PutUint16(response[22:24], 20)
	response[25] = stunFamilyIpv6
	copy(response[28:44], c.mapped.To16())
	return copy(p, response), c.destination, nil
}
func (c *ipv6STUNPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.request = append([]byte(nil), p...)
	c.destination = addr.(*net.UDPAddr)
	return len(p), nil
}
func (*ipv6STUNPacketConn) Close() error                     { return nil }
func (*ipv6STUNPacketConn) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (*ipv6STUNPacketConn) SetDeadline(time.Time) error      { return nil }
func (*ipv6STUNPacketConn) SetReadDeadline(time.Time) error  { return nil }
func (*ipv6STUNPacketConn) SetWriteDeadline(time.Time) error { return nil }

func TestProxyIPv6UDPExit(t *testing.T) {
	pc := &ipv6STUNPacketConn{mapped: net.ParseIP("2001:db8::42")}
	dst := &net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 3478}
	exit, err := proxyIPv6UDPExit(context.Background(), pc, dst)
	if err != nil || exit != "2001:db8::42" {
		t.Fatalf("exit %q: %v", exit, err)
	}
	if _, err := proxyIPv6UDPExit(context.Background(), pc, &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 3478}); err == nil {
		t.Fatal("accepted IPv4 destination")
	}
}

func TestProxyIPv6UDPStatus(t *testing.T) {
	for _, tc := range []struct{ tcp, udp, want string }{
		{"2001:db8::1", "2001:db8:0:0::1", "yes"},
		{"2001:db8::1", "2001:db8::2", "mismatch"},
		{"", "2001:db8::1", "unknown"},
		{"192.0.2.1", "192.0.2.1", "unknown"},
	} {
		if got := proxyIPv6UDPStatus(tc.tcp, tc.udp); got != tc.want {
			t.Errorf("%q, %q: got %q want %q", tc.tcp, tc.udp, got, tc.want)
		}
	}
}
