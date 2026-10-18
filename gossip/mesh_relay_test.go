// The live mesh end to end (EXTENDER.md D1, D3, D6), over the transports this
// fork has.
//
// The pin's `mesh_test.go` stands up an operator listening with the websocket
// transport, three extenders each running a real extender server with its
// in-process gossip listener and feed server, and one member app. The extender
// hop needs two things that have not landed here: the carrier dial behind
// `connect.DialExtender` (a stub in net_extender_carrier.go) and the v2026
// extender server seams the fixture installs (`GossipConnHandler`,
// `FeedConnHandler`, `IdentityKeySeed`, `Listen`, `CloseAndWait`). What is
// ported instead is the half those do not carry: the operator link over the
// websocket transport, gossipsub flooding a record to a member, the directory
// apply, and the validator refusing a forgery at the origin. The extender-hop
// tests (`TestGossipMeshRelaysSignedRecords`, `TestGossipMeshPeersWithTheOperator`
// and every case of `mesh_ipv6_test.go`) are recorded as gaps until the
// carrier and the server port land.
//
// The fixture is loopback only: the operator binds a random port on 127.0.0.1
// and the member dials it there, so no test here touches the public network.
// Nothing sleeps for a result; progress is observed through the directory's
// change monitor and the node's status monitor, both of which publish exactly
// when the thing under test lands.

package gossip

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	ma "github.com/multiformats/go-multiaddr"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// How long a barrier waits for the mesh to reach a state. Generous, because
// the race build of two libp2p hosts is slow; nothing waits this long when the
// mesh is healthy.
const testMeshTimeout = 60 * time.Second

// A relay fixture: the operator, one member, and the connect settings they
// share. The member holds nothing but the operator address, so every message
// it applies arrived over the mesh.
type testRelayMesh struct {
	t       *testing.T
	ctx     context.Context
	rootKey *testKey

	operator          *Node
	operatorDirectory *connect.ExtenderDirectory
	member            *Node
	memberDirectory   *connect.ExtenderDirectory
}

// Builds the fixture on loopback. The operator listens with the websocket
// transport, which is the operator's shape (C6, D1).
func newTestRelayMesh(t *testing.T) *testRelayMesh {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	mesh := &testRelayMesh{
		t:       t,
		ctx:     ctx,
		rootKey: newTestKey(t),
	}

	mesh.operatorDirectory = newTestDirectory(t, mesh.rootKey)
	operatorListenAddrs, err := WebsocketListenAddrs("127.0.0.1", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	mesh.operator = mesh.newNode(t, "operator", newTestKey(t), mesh.operatorDirectory, func(settings *NodeSettings) {
		settings.ListenAddrs = operatorListenAddrs
	})
	operatorAddrs := mesh.operatorAddrs()

	mesh.memberDirectory = newTestDirectory(t, mesh.rootKey)
	mesh.member = mesh.newNode(t, "member", newTestKey(t), mesh.memberDirectory, func(settings *NodeSettings) {
		settings.OperatorAddrs = operatorAddrs
	})
	return mesh
}

// The operator's dialable address, taken from what its swarm actually bound.
func (self *testRelayMesh) operatorAddrs() []ma.Multiaddr {
	self.t.Helper()
	listenAddrs := self.operator.ListenAddrs()
	if len(listenAddrs) == 0 {
		self.t.Fatal("the operator bound no address")
	}
	p2pComponent, err := ma.NewComponent("p2p", self.operator.PeerId().String())
	if err != nil {
		self.t.Fatal(err)
	}
	return []ma.Multiaddr{listenAddrs[0].Encapsulate(p2pComponent)}
}

// One node with a peering tick long enough that only a directory change or a
// wake can drive a round.
func (self *testRelayMesh) newNode(
	t *testing.T,
	name string,
	key *testKey,
	directory *connect.ExtenderDirectory,
	configure func(settings *NodeSettings),
) *Node {
	t.Helper()
	settings := DefaultNodeSettings(NodeRoleMember)
	settings.NetworkHost = testNetworkHost
	settings.Directory = directory
	settings.IdentityKeySeed = key.seed
	settings.ConnectSettings = connect.DefaultConnectSettings()
	settings.PeerTimeout = 5 * time.Minute
	settings.PeerCoalesceTimeout = 10 * time.Millisecond
	settings.DialTimeout = 20 * time.Second
	settings.StatusTimeout = 50 * time.Millisecond
	settings.RandSeed = int64(len(name))
	if configure != nil {
		configure(settings)
	}
	node, err := NewNode(self.ctx, settings)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	t.Cleanup(node.Close)
	return node
}

// Every directory the fixture fills.
func (self *testRelayMesh) directories() []*connect.ExtenderDirectory {
	return []*connect.ExtenderDirectory{
		self.operatorDirectory,
		self.memberDirectory,
	}
}

// Publishes one message from the operator, the only node that originates (D6).
func (self *testRelayMesh) publish(message *protocol.ExtenderGossipMessage) {
	self.t.Helper()
	ctx, cancel := context.WithTimeout(self.ctx, testMeshTimeout)
	defer cancel()
	if err := self.operator.Publish(ctx, message); err != nil {
		self.t.Fatalf("publish: %v", err)
	}
}

// Waits until the member sees the operator in the topic, which is the barrier
// a publish needs: gossipsub floods a locally published message to every topic
// peer it knows.
func (self *testRelayMesh) waitForTopic(t *testing.T) {
	t.Helper()
	waitForNodeStatus(t, self.member, "the operator in the topic", func(status NodeStatus) bool {
		return 1 <= status.MeshPeerCount
	})
}

// Waits for a directory to reach a state, watching the change monitor rather
// than polling.
func waitForDirectory(
	t *testing.T,
	directory *connect.ExtenderDirectory,
	what string,
	condition func(snapshot *connect.ExtenderDirectorySnapshot) bool,
) {
	t.Helper()
	deadline := time.Now().Add(testMeshTimeout)
	for {
		_, change := directory.ChangeMonitor().Get()
		if condition(directory.Snapshot()) {
			return
		}
		select {
		case <-change:
		case <-time.After(time.Until(deadline)):
			t.Fatalf("the directory did not reach %s", what)
		}
	}
}

// The same for a node's status.
func waitForNodeStatus(
	t *testing.T,
	node *Node,
	what string,
	condition func(status NodeStatus) bool,
) {
	t.Helper()
	deadline := time.Now().Add(testMeshTimeout)
	for {
		status, change := node.StatusMonitor().Get()
		if condition(status) {
			return
		}
		select {
		case <-change:
		case <-time.After(time.Until(deadline)):
			t.Fatalf("the node did not reach %s, status = %+v", what, node.Status())
		}
	}
}

// The state of one address in a snapshot, empty when it is not known.
func snapshotState(snapshot *connect.ExtenderDirectorySnapshot, ip netip.Addr) string {
	for _, entry := range snapshot.Entries {
		if entry.Ip == ip {
			return entry.State
		}
	}
	return ""
}

// A record the operator publishes reaches the member's directory, a revocation
// does the same, and a message signed by another key is refused at the origin
// (D1, D6).
func TestGossipMeshRelaysSignedRecords(t *testing.T) {
	mesh := newTestRelayMesh(t)
	mesh.waitForTopic(t)

	// the operator link is the only address the member knows, and it is up
	waitForNodeStatus(t, mesh.member, "the operator link", func(status NodeStatus) bool {
		return status.OperatorConnected
	})

	issueTime := time.Now()
	extenderKey := newTestKey(t)
	extenderIp := netip.MustParseAddr("192.0.2.71")
	record := signTestRecord(t, mesh.rootKey, extenderKey, extenderIp.String(), 8443, issueTime)
	mesh.publish(&protocol.ExtenderGossipMessage{
		Message: &protocol.ExtenderGossipMessage_Record{Record: record},
	})
	for _, directory := range mesh.directories() {
		waitForDirectory(t, directory, "the published record", func(snapshot *connect.ExtenderDirectorySnapshot) bool {
			return snapshotState(snapshot, extenderIp) == connect.ExtenderStateActive
		})
	}

	// a message signed by a key the directories do not accept is rejected by
	// the validator, which refuses the publish at the origin
	forgedRootKey := newTestKey(t)
	forgedIp := netip.MustParseAddr("192.0.2.72")
	forgedRecord := signTestRecord(t, forgedRootKey, newTestKey(t), forgedIp.String(), 8443, issueTime)
	publishCtx, publishCancel := context.WithTimeout(mesh.ctx, testMeshTimeout)
	defer publishCancel()
	if err := mesh.operator.Publish(publishCtx, &protocol.ExtenderGossipMessage{
		Message: &protocol.ExtenderGossipMessage_Record{Record: forgedRecord},
	}); err == nil {
		t.Fatal("a forged record was published")
	}

	// the revocation is the flush barrier: once it has reached a directory,
	// anything published before it has been delivered there too
	revocation, err := connect.SignExtenderRevocation(
		mesh.rootKey.privateKey,
		&protocol.ExtenderRevocationBody{
			PublicKey:   extenderKey.publicKey,
			IssueTimeMs: uint64(issueTime.Add(time.Second).UnixMilli()),
			NetworkHost: testNetworkHost,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	mesh.publish(&protocol.ExtenderGossipMessage{
		Message: &protocol.ExtenderGossipMessage_Revocation{Revocation: revocation},
	})
	for _, directory := range mesh.directories() {
		waitForDirectory(t, directory, "the published revocation", func(snapshot *connect.ExtenderDirectorySnapshot) bool {
			return snapshotState(snapshot, extenderIp) == connect.ExtenderStateRevoked
		})
		if state := snapshotState(directory.Snapshot(), forgedIp); state != "" {
			t.Fatalf("the forged record was applied as %s", state)
		}
	}
}

// A peering round with dials in flight and no mesh peer is the connecting
// state the app's status dot shows (K4). The operator address here is a socket
// that accepts and never speaks, so the dial stays in flight rather than
// failing, and the member holds no mesh peer at all.
func TestGossipNodeReportsConnectingWhileItDials(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		listener.Close()
	})
	acceptedLock := sync.Mutex{}
	accepted := []net.Conn{}
	t.Cleanup(func() {
		acceptedLock.Lock()
		defer acceptedLock.Unlock()
		for _, conn := range accepted {
			conn.Close()
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			acceptedLock.Lock()
			accepted = append(accepted, conn)
			acceptedLock.Unlock()
		}
	}()

	operatorKey := newTestKey(t)
	operatorPeerId, err := PeerIdForExtenderPublicKey(operatorKey.publicKey)
	if err != nil {
		t.Fatal(err)
	}
	listenAddrs, err := WebsocketListenAddrs("127.0.0.1", listener.Addr().(*net.TCPAddr).Port, false)
	if err != nil {
		t.Fatal(err)
	}
	p2pComponent, err := ma.NewComponent("p2p", operatorPeerId.String())
	if err != nil {
		t.Fatal(err)
	}

	settings := DefaultNodeSettings(NodeRoleMember)
	settings.NetworkHost = testNetworkHost
	settings.Directory = newTestDirectory(t, newTestKey(t))
	settings.IdentityKeySeed = newTestKey(t).seed
	settings.OperatorAddrs = []ma.Multiaddr{listenAddrs[0].Encapsulate(p2pComponent)}
	// one round, held open by the silent socket for the whole dial budget
	settings.PeerTimeout = 5 * time.Minute
	settings.DialTimeout = testMeshTimeout
	settings.StatusTimeout = 50 * time.Millisecond
	node, err := NewNode(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(node.Close)

	waitForNodeStatus(t, node, "the connecting state", func(status NodeStatus) bool {
		return status.Connecting
	})
	status := node.Status()
	if status.MeshPeerCount != 0 {
		t.Fatalf("mesh peers = %d, want none while connecting", status.MeshPeerCount)
	}
	if state := connect.ExtenderGossipStateForMember(
		status.MeshPeerCount,
		status.Connecting,
	); state != connect.ExtenderGossipStateConnecting {
		t.Fatalf("gossip state = %q, want %q", state, connect.ExtenderGossipStateConnecting)
	}
}
