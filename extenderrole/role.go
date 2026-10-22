// Package extenderrole composes the discovery plane a served extender runs:
// the feed server a client bootstraps from, the in-process listener other nodes
// reach this one through, and the mesh node that fills the directory the feed
// serves.
//
// The pieces live apart. The directory, the records and the feed codec are
// connect root; the mesh and the feed server are connect/gossip; and neither
// knows about the other -- the root never imports gossip, and gossip reads only
// the directory it is handed. This package is the seam between them: one
// constructor takes the directory the role shares with the client it also runs,
// the extender's identity seed, and the operator's mesh addresses, and returns
// the two callbacks an extender server installs
//
//	settings.IdentityKeySeed = identityKeySeed
//	settings.GossipConnHandler = role.GossipConnHandler
//	settings.FeedConnHandler = role.FeedConnHandler
//
// plus the seams the role's lifecycle needs while it runs: the operator
// addresses learned from hello (`SetOperatorAddrs`), the mesh addresses of a
// family that activates later (`Listen`), and the mesh status
// (`Status`, `Node().StatusMonitor()`).
//
// Nothing here opens a socket, dials a carrier, reads a setting store or writes
// a record. The caller owns the extender server, the carriers, the identity
// file and the directory's lifecycle; this package owns only what it builds --
// the feed server always, and the in-process listener and the node unless the
// caller passes its own. A caller that passes a listener keeps it: it is closed
// by the caller, not by `Close`.
//
// Two limits are deliberate and worth stating where the role is wired. The feed
// is served whether or not the mesh is up (`DisableGossip`), because a client
// that reaches an extender must get a sample even when this host could not join
// the mesh; the gossip service is then refused at once rather than queueing a
// stream that nothing will accept. And the directory the role is handed must
// already carry the operator's root key set and its network hosts: this package
// adds no trust of its own, so a directory without them accepts no record and
// the role serves an empty feed.
package extenderrole

import (
	"context"
	"fmt"
	"net"
	"slices"
	"sync"

	ma "github.com/multiformats/go-multiaddr"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/gossip"
	"github.com/urnetwork/connect/protocol"
)

// Settings is everything the role is built from. The four required fields are
// validated by `New`; every other field has a working default.
type Settings struct {
	// Log is the logger of everything the role builds. Nil is silent.
	Log connect.Logger

	// NetworkHost is the network space this extender belongs to (required): the
	// topic of the mesh, and the host every applied record must name.
	NetworkHost string

	// Directory is the extender directory the node fills and the feed serves
	// (required). The caller owns it -- it is the same directory the client it
	// runs is filled from -- and must have set its root keys and network hosts.
	Directory *connect.ExtenderDirectory

	// IdentityKeySeed is the ed25519 seed of the extender identity (required).
	// It is the key in the extender's own signed record, the mesh peer id, and
	// the identity the feed serves first.
	IdentityKeySeed []byte

	// ListenAddrs are the mesh addresses this extender is reachable at, one per
	// activated family (`gossip.ExtenderListenAddrs`). They are what the mesh
	// advertises for this node. Empty is a node that still joins the mesh but
	// advertises nothing, which is what a host with no activated family has.
	//
	// Used only when the role builds the listener: a listener passed in
	// `Listener` carries its own addresses.
	ListenAddrs []ma.Multiaddr

	// OperatorAddrs are the operator's mesh addresses, each carrying
	// `/p2p/<id>`. They are empty until hello resolves the operator's identity;
	// `SetOperatorAddrs` replaces them then.
	OperatorAddrs []ma.Multiaddr

	// ConnectSettings is the dial configuration of the extender carrier the
	// mesh is reached through. Nil takes the connect defaults.
	ConnectSettings *connect.ConnectSettings

	// FeedSettings are the bounds of the feed server. Nil takes the gossip
	// defaults.
	FeedSettings *gossip.FeedServerSettings

	// ListenerSettings are the bounds of the in-process listener, used only
	// when the role builds the listener. Nil takes the gossip defaults.
	ListenerSettings *gossip.InProcessListenerSettings

	// NodeSettings are the optional tuning of the mesh node: the peer target,
	// the connection manager watermarks, the peering, dial and status periods,
	// the clock and the peer choice seed. A zero field keeps the extender
	// defaults (`gossip.DefaultNodeSettings(gossip.NodeRoleExtender)`); the
	// role owns the identity, space, directory, role, listener and addresses
	// and overwrites them whatever this holds.
	NodeSettings *gossip.NodeSettings

	// Listener is an in-process listener the caller already built, for a caller
	// that needs to install it before the role exists. Nil builds one from
	// `ListenAddrs` and `ListenerSettings`, which the role then owns.
	Listener *gossip.InProcessListener

	// DisableGossip runs the feed alone: no listener, no node, and every
	// gossip stream refused at once. The extender still bootstraps clients from
	// whatever its directory holds. A listener passed in `Listener` is not used
	// and not closed.
	DisableGossip bool
}

// Status is the role as one comparable value, so a consumer can publish it on a
// monitor and be woken only by a change.
type Status struct {
	// PeerId is the mesh identity, derived from the identity seed. It is the
	// same in a feed-only role, which is what lets a record carry it before the
	// mesh is up.
	PeerId string
	// GossipRunning is false on a feed-only role.
	GossipRunning bool
	// Node is the mesh status, zero while gossip is not running.
	Node gossip.NodeStatus
	// FeedSubscriberCount is the subscribers the feed is serving now.
	FeedSubscriberCount int
}

// Role is one served extender's discovery plane, running when `New` returns.
//
// The methods are safe for concurrent use, and `Close` is idempotent.
type Role struct {
	cancel    context.CancelFunc
	closeOnce sync.Once

	publicKey []byte
	peerId    string

	directory *connect.ExtenderDirectory
	feed      *gossip.FeedServer

	listener     *gossip.InProcessListener
	ownsListener bool

	node *gossip.Node
}

// New builds and starts the role: the feed server always, and the in-process
// listener and the mesh node unless gossip is disabled. The error of a node
// that cannot start is returned rather than swallowed, because a caller that
// wanted the mesh must not silently run without it; a caller that can live with
// the feed alone sets `DisableGossip` and retries the role later.
func New(ctx context.Context, settings *Settings) (*Role, error) {
	if settings == nil {
		return nil, fmt.Errorf("the extender role needs settings")
	}
	if settings.NetworkHost == "" {
		return nil, fmt.Errorf("the extender role needs a network host")
	}
	if settings.Directory == nil {
		return nil, fmt.Errorf("the extender role needs a directory")
	}
	if len(settings.IdentityKeySeed) == 0 {
		return nil, fmt.Errorf("the extender role needs an identity key seed")
	}

	publicKey, err := connect.ExtenderPublicKeyFromSeed(settings.IdentityKeySeed)
	if err != nil {
		return nil, err
	}
	peerId, err := gossip.PeerIdForExtenderPublicKey(publicKey)
	if err != nil {
		return nil, err
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	self := &Role{
		cancel:    cancel,
		publicKey: append([]byte(nil), publicKey...),
		peerId:    peerId.String(),
		directory: settings.Directory,
	}
	success := false
	defer func() {
		if !success {
			// nothing escapes a constructor that failed half way: the feed
			// server, the listener and the node are joined in reverse order
			self.Close()
		}
	}()

	self.feed = gossip.NewFeedServer(
		cancelCtx,
		settings.Directory,
		self.publicKey,
		settings.FeedSettings,
	)
	if settings.DisableGossip {
		success = true
		return self, nil
	}

	self.listener = settings.Listener
	if self.listener == nil {
		listenerSettings := gossip.DefaultInProcessListenerSettings()
		if settings.ListenerSettings != nil {
			copiedListenerSettings := *settings.ListenerSettings
			listenerSettings = &copiedListenerSettings
		}
		listenerSettings.Log = settings.Log
		listenerSettings.ListenAddrs = slices.Clone(settings.ListenAddrs)
		self.listener = gossip.NewInProcessListener(cancelCtx, listenerSettings)
		self.ownsListener = true
	}

	// the extender defaults are the base, so a caller that supplies only the
	// tuning it cares about keeps the watermarks and peer target of the role
	nodeSettings := gossip.DefaultNodeSettings(gossip.NodeRoleExtender)
	if base := settings.NodeSettings; base != nil {
		if 0 < base.PeerTarget {
			nodeSettings.PeerTarget = base.PeerTarget
		}
		if 0 < base.ConnManagerLowWater {
			nodeSettings.ConnManagerLowWater = base.ConnManagerLowWater
		}
		if 0 < base.ConnManagerHighWater {
			nodeSettings.ConnManagerHighWater = base.ConnManagerHighWater
		}
		if 0 < base.PeerTimeout {
			nodeSettings.PeerTimeout = base.PeerTimeout
		}
		if 0 < base.PeerCoalesceTimeout {
			nodeSettings.PeerCoalesceTimeout = base.PeerCoalesceTimeout
		}
		if 0 < base.DialTimeout {
			nodeSettings.DialTimeout = base.DialTimeout
		}
		if 0 < base.StatusTimeout {
			nodeSettings.StatusTimeout = base.StatusTimeout
		}
		if base.Now != nil {
			nodeSettings.Now = base.Now
		}
		if base.RandSeed != 0 {
			nodeSettings.RandSeed = base.RandSeed
		}
		nodeSettings.ConnectSettings = base.ConnectSettings
	}
	nodeSettings.Log = settings.Log
	nodeSettings.NetworkHost = settings.NetworkHost
	nodeSettings.Directory = settings.Directory
	nodeSettings.IdentityKeySeed = settings.IdentityKeySeed
	nodeSettings.Role = gossip.NodeRoleExtender
	nodeSettings.ExtenderListener = self.listener
	// the addresses are the listener's, which the transport advertises per
	// family; passing them twice would ask the swarm to listen on each twice
	nodeSettings.ListenAddrs = nil
	nodeSettings.OperatorAddrs = slices.Clone(settings.OperatorAddrs)
	if settings.ConnectSettings != nil {
		nodeSettings.ConnectSettings = settings.ConnectSettings
	}

	self.node, err = gossip.NewNode(cancelCtx, nodeSettings)
	if err != nil {
		return nil, err
	}

	success = true
	return self, nil
}

// GossipConnHandler is what an extender server installs as
// `ExtenderSettings.GossipConnHandler` (A8): the taken-over stream of a gossip
// service. It owns the stream until it returns, which is why the mesh releasing
// it is what ends the call.
//
// A role with no node -- feed only, or a node that never started -- refuses at
// once. The extender closes the stream and the client retries elsewhere, which
// is the bounded answer; queueing it would park the extender's connection
// goroutine behind a mesh that is not there.
func (self *Role) GossipConnHandler(conn net.Conn) {
	if self.node == nil {
		return
	}
	self.listener.Handle(conn)
}

// FeedConnHandler is what an extender server installs as
// `ExtenderSettings.FeedConnHandler` (A8): it serves one feed stream to
// completion -- the sample, and then the subscription if the client asked for
// one.
func (self *Role) FeedConnHandler(conn net.Conn) {
	self.feed.Serve(conn)
}

// Publish sends one root-signed record or revocation to the mesh. Only the
// operator originates (D6), and an extender does not in the normal course; it
// is here because a host that runs both roles -- and every test of the mesh --
// needs the one publish path, and because a record no root key signed is
// refused here rather than relayed.
func (self *Role) Publish(ctx context.Context, message *protocol.ExtenderGossipMessage) error {
	if self.node == nil {
		return fmt.Errorf("the extender role is not running a gossip node")
	}
	return self.node.Publish(ctx, message)
}

// SetOperatorAddrs replaces the operator addresses and runs a peering round at
// once. The caller calls it when hello first carries the operator's mesh
// identity, which is after the role is already up. It is a no-op on a feed-only
// role.
func (self *Role) SetOperatorAddrs(operatorAddrs []ma.Multiaddr) {
	if self.node == nil {
		return
	}
	self.node.SetOperatorAddrs(operatorAddrs)
}

// Listen adds mesh addresses this extender is reachable at, on top of the ones
// the listener was built with. The caller calls it when a family it did not
// have at construction activates. It reports an error on a feed-only role,
// which has no mesh to listen on.
func (self *Role) Listen(listenAddrs ...ma.Multiaddr) error {
	if self.node == nil {
		return fmt.Errorf("the extender role is not running a gossip node")
	}
	return self.node.Listen(listenAddrs...)
}

// PublicKey is the extender identity in its own signed record, as a copy.
func (self *Role) PublicKey() []byte {
	return append([]byte(nil), self.publicKey...)
}

// PeerId is the mesh identity as text, which is what a hello carries. It is set
// on a feed-only role too, because it follows from the identity seed alone.
func (self *Role) PeerId() string {
	return self.peerId
}

// Directory is the directory the node fills and the feed serves, as handed to
// the role.
func (self *Role) Directory() *connect.ExtenderDirectory {
	return self.directory
}

// Feed is the feed server, for its subscriber count.
func (self *Role) Feed() *gossip.FeedServer {
	return self.feed
}

// Listener is the in-process listener, nil on a feed-only role.
func (self *Role) Listener() *gossip.InProcessListener {
	return self.listener
}

// Node is the mesh node, nil on a feed-only role.
func (self *Role) Node() *gossip.Node {
	return self.node
}

// GossipRunning reports whether the mesh is running, which a caller checks
// before installing `GossipConnHandler` on a server it builds later.
func (self *Role) GossipRunning() bool {
	return self.node != nil
}

// Status is the role as one comparable value.
func (self *Role) Status() Status {
	status := Status{
		PeerId:              self.peerId,
		GossipRunning:       self.node != nil,
		FeedSubscriberCount: self.feed.SubscriberCount(),
	}
	if self.node != nil {
		status.Node = self.node.Status()
	}
	return status
}

// Close ends the role: the feed streams, the streams the mesh holds through the
// listener, and the node's loops and host, in that order. A listener the caller
// passed in is left to the caller, because it may outlive this role.
func (self *Role) Close() {
	self.closeOnce.Do(func() {
		self.cancel()
		if self.feed != nil {
			self.feed.Close()
		}
		if self.ownsListener {
			self.listener.Close()
		}
		if self.node != nil {
			self.node.Close()
		}
	})
}
