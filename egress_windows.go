//go:build windows

package connect

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// IP_UNICAST_IF (IPPROTO_IP, option 31) forces the outgoing interface for
// unicast, overriding the route table. The interface index must be passed in
// NETWORK byte order. IPV6_UNICAST_IF (IPPROTO_IPV6, option 31) does the same
// for IPv6 but takes the index in HOST byte order. This asymmetry matches
// wireguard-windows' bindSocketRoute.
const (
	ipUnicastIf   = 31
	ipv6UnicastIf = 31
)

// SO_PROTOCOL_INFOW identifies the actual socket family.
const soProtocolInfoW = 0x2005

func socketIsIpv6(h windows.Handle) (bool, error) {
	var info windows.WSAProtocolInfo
	size := int32(unsafe.Sizeof(info))
	if err := syscall.Getsockopt(syscall.Handle(h), windows.SOL_SOCKET, soProtocolInfoW, (*byte)(unsafe.Pointer(&info)), &size); err != nil {
		return false, err
	}
	return info.AddressFamily == windows.AF_INET6, nil
}

func htonl(x uint32) uint32 {
	return (x&0x000000ff)<<24 | (x&0x0000ff00)<<8 | (x&0x00ff0000)>>8 | (x&0xff000000)>>24
}

// applyEgressInterface pins both families and checks the socket's actual family.
// An IPv6 socket can accept the IPv4 option while its IPv6 traffic is unpinned.
func applyEgressInterface(fd uintptr, index4 uint32, index6 uint32) error {
	if index4 == 0 && index6 == 0 {
		return nil
	}
	if index4 == 0 {
		index4 = index6
	}
	if index6 == 0 {
		index6 = index4
	}
	h := windows.Handle(fd)
	err6 := windows.SetsockoptInt(h, windows.IPPROTO_IPV6, ipv6UnicastIf, int(index6))
	err4 := windows.SetsockoptInt(h, windows.IPPROTO_IP, ipUnicastIf, int(htonl(index4)))
	is6, err := socketIsIpv6(h)
	if err != nil {
		return fmt.Errorf("connect: could not read the socket family: %w", err)
	}
	switch {
	case is6 && err6 == nil, !is6 && err4 == nil:
		return nil
	case is6 && err4 == nil:
		return fmt.Errorf("%w (v4=%d v6=%d): %w", errEgressIpv6Unpinned, index4, index6, err6)
	}
	err = err4
	if is6 {
		err = err6
	}
	return fmt.Errorf(
		"connect: could not pin the socket to the egress interface (v4=%d v6=%d): %w; "+
			"the socket would follow the route table and loop into the tunnel",
		index4, index6, err,
	)
}
