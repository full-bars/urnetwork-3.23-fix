package gossip

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/transport"
	"github.com/libp2p/go-libp2p/p2p/host/eventbus"
	"github.com/libp2p/go-libp2p/p2p/host/peerstore/pstoremem"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	ma "github.com/multiformats/go-multiaddr"
)

type inboundTestTransport struct {
	transport.Transport
	listener *inboundTestListener
}

func (t *inboundTestTransport) Protocols() []int { return []int{ma.P_TCP} }
func (t *inboundTestTransport) Proxy() bool      { return false }
func (t *inboundTestTransport) Listen(ma.Multiaddr) (transport.Listener, error) {
	return t.listener, nil
}

type inboundTestListener struct {
	addr     ma.Multiaddr
	incoming chan transport.CapableConn
	done     chan struct{}
	once     sync.Once
}

func (l *inboundTestListener) Accept() (transport.CapableConn, error) {
	select {
	case c := <-l.incoming:
		return c, nil
	case <-l.done:
		return nil, transport.ErrListenerClosed
	}
}
func (l *inboundTestListener) Close() error            { l.once.Do(func() { close(l.done) }); return nil }
func (l *inboundTestListener) Addr() net.Addr          { return &net.TCPAddr{} }
func (l *inboundTestListener) Multiaddr() ma.Multiaddr { return l.addr }

type inboundTestConn struct {
	transport.CapableConn
	id   peer.ID
	addr ma.Multiaddr
	done chan struct{}
	once sync.Once
}

func (c *inboundTestConn) RemotePeer() peer.ID                        { return c.id }
func (c *inboundTestConn) RemoteMultiaddr() ma.Multiaddr              { return c.addr }
func (c *inboundTestConn) LocalMultiaddr() ma.Multiaddr               { return c.addr }
func (c *inboundTestConn) RemotePublicKey() crypto.PubKey             { return nil }
func (c *inboundTestConn) Close() error                               { c.once.Do(func() { close(c.done) }); return nil }
func (c *inboundTestConn) CloseWithError(network.ConnErrorCode) error { return c.Close() }
func (c *inboundTestConn) IsClosed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}
func (c *inboundTestConn) AcceptStream() (network.MuxedStream, error) {
	<-c.done
	return nil, errors.New("closed")
}
func (c *inboundTestConn) OpenStream(context.Context) (network.MuxedStream, error) {
	return nil, errors.New("unused")
}

func TestInboundConnectionClearsDialBackoff(t *testing.T) {
	ps, err := pstoremem.NewPeerstore()
	if err != nil {
		t.Fatal(err)
	}
	defer ps.Close()
	s, err := swarm.NewSwarm(peer.ID("local"), ps, eventbus.NewBus())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	addr := ma.StringCast("/ip4/192.0.2.1/tcp/1234")
	l := &inboundTestListener{addr: addr, incoming: make(chan transport.CapableConn), done: make(chan struct{})}
	if err := s.AddTransport(&inboundTestTransport{listener: l}); err != nil {
		t.Fatal(err)
	}
	connected := make(chan network.Conn, 1)
	s.Notify(&network.NotifyBundle{ConnectedF: func(_ network.Network, c network.Conn) { connected <- c }})
	if err := s.Listen(addr); err != nil {
		t.Fatal(err)
	}
	a := peer.AddrInfo{ID: peer.ID("failed-peer"), Addrs: []ma.Multiaddr{addr}}
	b := peer.AddrInfo{ID: peer.ID("later-peer"), Addrs: []ma.Multiaddr{addr}}
	// Keep the real swarm backoff active throughout the channel-synchronized test.
	for range 100 {
		s.Backoff().AddBackoff(a.ID, addr)
	}
	selectRound := func() ([]peer.AddrInfo, bool) {
		return selectExtenderDials([]peer.AddrInfo{a, b}, 1, func(id peer.ID) bool { return s.Connectedness(id) == network.Connected }, func(info peer.AddrInfo) bool { return allAddrsBackedOff(info, s.Backoff().Backoff) })
	}
	got, skipped := selectRound()
	if !skipped || len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("backoff selection: %v %v", got, skipped)
	}
	l.incoming <- &inboundTestConn{id: a.ID, addr: addr, done: make(chan struct{})}
	c := <-connected
	if c.Stat().Direction != network.DirInbound {
		t.Fatal("connection was not inbound")
	}
	if s.Backoff().Backoff(a.ID, addr) {
		t.Fatal("inbound connection did not clear backoff")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	got, skipped = selectRound()
	if skipped || len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("selection after inbound disconnect: %v %v", got, skipped)
	}
}
