//go:build !js

package connect

import (
	"net"
	"syscall"

	"github.com/pion/transport/v4"
	"github.com/pion/transport/v4/stdnet"
)

// egressNet is a pion transport.Net that binds the UDP sockets pion creates for
// ICE to the egress interface, so p2p (webrtc) traffic does not loop back into
// the tunnel this process provides (R1). It embeds the standard net and only
// overrides socket creation; a no-op unless an egress index is set / off
// Windows.
type egressNet struct {
	transport.Net
}

func newEgressNet() (transport.Net, error) {
	base, err := stdnet.NewNet()
	if err != nil {
		return nil, err
	}
	return &egressNet{Net: base}, nil
}

// ListenUDP creates a UDP socket through the embedded transport.Net and,
// when the connection exposes a syscall.Conn, binds it to the egress
// interface via applyEgress (binding errors are ignored) so p2p (webrtc)
// traffic does not loop back into the tunnel this process provides.
func (self *egressNet) ListenUDP(network string, locAddr *net.UDPAddr) (transport.UDPConn, error) {
	conn, err := self.Net.ListenUDP(network, locAddr)
	if err != nil {
		return nil, err
	}
	if sc, ok := conn.(syscall.Conn); ok {
		_ = applyEgress(sc)
	}
	return conn, nil
}

// ListenPacket is the packet-connection variant of ListenUDP: it creates the
// socket through the embedded transport.Net and, when the connection exposes
// a syscall.Conn, applies the egress interface binding best-effort (binding
// errors are ignored, so the egress binding is not guaranteed).
func (self *egressNet) ListenPacket(network string, address string) (net.PacketConn, error) {
	conn, err := self.Net.ListenPacket(network, address)
	if err != nil {
		return nil, err
	}
	if sc, ok := conn.(syscall.Conn); ok {
		_ = applyEgress(sc)
	}
	return conn, nil
}
