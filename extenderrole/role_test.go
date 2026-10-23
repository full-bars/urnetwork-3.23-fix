// The composition the provider extender role installs, exercised through its
// exported entry points only.
//
// The feed cases drive a real client exchange through `FeedConnHandler`: the
// request and the frames are the root package's own codec, so what is under
// test is the seam and not a copy of the protocol. The mesh case stands up an
// operator node on loopback and the role beside it, and asserts both directions
// -- a record the operator publishes reaches the role's directory, and a record
// the role publishes reaches the operator's -- and then that the feed the role
// serves carries what the mesh taught it.
//
// What is not testable here is the extender hop itself: the carrier dial behind
// `connect.DialExtender` is a stub in this fork, so no second host can reach
// the role's in-process listener over a carrier. The listener's own contract is
// covered in connect/gossip; here `GossipConnHandler` is covered for the two
// things this package adds: it refuses at once with no node, and it holds the
// stream while there is one.
package extenderrole

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"net"
	"net/netip"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	ma "github.com/multiformats/go-multiaddr"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/gossip"
	"github.com/urnetwork/connect/protocol"
)

// The network space of every record in these tests.
const testNetworkHost = "space.example"

// How long a barrier waits for the mesh or the directory. Generous, because a
// race build of two libp2p hosts is slow; a healthy mesh never waits this long.
const testRoleTimeout = 60 * time.Second

// One ed25519 identity, kept as the seed the role is handed.
type testKey struct {
	seed       []byte
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
}

func newTestKey(t *testing.T) *testKey {
	t.Helper()
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := connect.ExtenderPrivateKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	return &testKey{
		seed:       seed,
		privateKey: privateKey,
		publicKey:  privateKey.Public().(ed25519.PublicKey),
	}
}

// One record signed by the root key, for one address and tcp port. The ip
// version is derived from the address rather than fixed.
func signTestRecord(
	t *testing.T,
	rootKey *testKey,
	extenderKey *testKey,
	ip string,
	tcpPort int,
	issueTime time.Time,
) *protocol.ExtenderRecord {
	t.Helper()
	parsedIp, err := netip.ParseAddr(ip)
	if err != nil {
		t.Fatal(err)
	}
	ipVersion := 4
	if parsedIp.Unmap().Is6() {
		ipVersion = 6
	}
	record, err := connect.SignExtenderRecord(rootKey.privateKey, &protocol.ExtenderRecordBody{
		PublicKey: extenderKey.publicKey,
		Addresses: []*protocol.ExtenderAddress{
			{
				Ip:        parsedIp.Unmap().String(),
				IpVersion: uint32(ipVersion),
				Carriers:  []string{connect.ExtenderCarrierTcp},
			},
		},
		TcpPort:      uint32(tcpPort),
		CountryCode:  "us",
		IssueTimeMs:  uint64(issueTime.UnixMilli()),
		ExpireTimeMs: uint64(issueTime.Add(14 * 24 * time.Hour).UnixMilli()),
		NetworkHost:  testNetworkHost,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// One directory anchored on the test root key. The role adds no trust of its
// own, so every case here hands it a directory that already trusts the key the
// records are signed with.
func newTestDirectory(t *testing.T, rootKey *testKey) *connect.ExtenderDirectory {
	t.Helper()
	settings := connect.DefaultExtenderDirectorySettings()
	settings.NetworkHosts = []string{testNetworkHost}
	ctx, cancel := context.WithCancel(context.Background())
	directory := connect.NewExtenderDirectory(ctx, settings)
	directory.SetRootKeys(connect.NewExtenderRootKeySet(rootKey.publicKey))
	t.Cleanup(func() {
		directory.Close()
		cancel()
	})
	return directory
}

// The dial and status periods the tests tune. The peering period is long so
// only a directory change or a wake drives a round; the status period is short
// so a barrier sees a peer that arrived.
func fastTestNodeSettings() *gossip.NodeSettings {
	settings := gossip.DefaultNodeSettings(gossip.NodeRoleExtender)
	settings.PeerTimeout = 5 * time.Minute
	settings.PeerCoalesceTimeout = 10 * time.Millisecond
	settings.DialTimeout = 20 * time.Second
	settings.StatusTimeout = 50 * time.Millisecond
	return settings
}

// A record frame's identity key, decoded from the body the frame carries.
func recordFramePublicKey(t *testing.T, frame *protocol.ExtenderFeedFrame) []byte {
	t.Helper()
	if frame.GetRecord() == nil {
		t.Fatalf("the feed carried %v, expected a record", frame)
	}
	body := &protocol.ExtenderRecordBody{}
	if err := proto.Unmarshal(frame.GetRecord().Body, body); err != nil {
		t.Fatal(err)
	}
	return body.PublicKey
}

// Waits for a directory to hold an active record, watching the change monitor
// rather than polling.
func waitForDirectoryKey(
	t *testing.T,
	directory *connect.ExtenderDirectory,
	publicKey []byte,
	what string,
) {
	t.Helper()
	deadline := time.Now().Add(testRoleTimeout)
	for {
		_, change := directory.ChangeMonitor().Get()
		if directory.IsActiveKey(publicKey) {
			return
		}
		select {
		case <-change:
		case <-time.After(time.Until(deadline)):
			t.Fatalf("the directory did not hold %s (%s)", what, hex.EncodeToString(publicKey))
		}
	}
}

// The same for one node's mesh status, watching the monitor rather than
// polling.
func waitForMeshStatus(
	t *testing.T,
	node *gossip.Node,
	what string,
	condition func(status gossip.NodeStatus) bool,
) {
	t.Helper()
	deadline := time.Now().Add(testRoleTimeout)
	for {
		_, change := node.StatusMonitor().Get()
		if condition(node.Status()) {
			return
		}
		select {
		case <-change:
		case <-time.After(time.Until(deadline)):
			t.Fatalf("the node did not reach %s (status %+v)", what, node.Status())
		}
	}
}

// A feed-only role still bootstraps a client: the sample it serves leads with
// the extender's own record, whatever else the directory holds.
func TestExtenderRoleServesItsOwnRecordOnTheFeed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	rootKey := newTestKey(t)
	extenderKey := newTestKey(t)
	directory := newTestDirectory(t, rootKey)
	record := signTestRecord(t, rootKey, extenderKey, "203.0.113.9", 443, time.Now())
	if _, err := directory.ApplyRecord(record, connect.ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	// a second record, so serving the own record first is a choice and not the
	// only record there is
	otherKey := newTestKey(t)
	if _, err := directory.ApplyRecord(
		signTestRecord(t, rootKey, otherKey, "203.0.113.10", 443, time.Now()),
		connect.ExtenderSourceFeed,
	); err != nil {
		t.Fatal(err)
	}

	role, err := New(ctx, &Settings{
		NetworkHost:     testNetworkHost,
		Directory:       directory,
		IdentityKeySeed: extenderKey.seed,
		DisableGossip:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(role.Close)

	status := role.Status()
	if status.GossipRunning {
		t.Fatalf("a feed-only role reports gossip running")
	}
	if status.PeerId == "" || status.PeerId != role.PeerId() {
		t.Fatalf("peer id = %q, status %q", role.PeerId(), status.PeerId)
	}
	if role.Node() != nil || role.Listener() != nil {
		t.Fatalf("a feed-only role built a mesh node or a listener")
	}
	if status.FeedSubscriberCount != 0 {
		t.Fatalf("subscribers = %d, expected 0", status.FeedSubscriberCount)
	}

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close() })
	served := make(chan struct{})
	go func() {
		defer close(served)
		defer serverConn.Close()
		role.FeedConnHandler(serverConn)
	}()

	if err := connect.WriteExtenderFeedRequest(clientConn, &protocol.ExtenderFeedRequest{
		SampleCount: 8,
	}); err != nil {
		t.Fatal(err)
	}
	clientConn.SetReadDeadline(time.Now().Add(testRoleTimeout))
	first, err := connect.ReadExtenderFeedFrame(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	if publicKey := recordFramePublicKey(t, first); !bytes.Equal(publicKey, role.PublicKey()) {
		t.Fatalf("the sample led with %s, expected the extender's own key %s",
			hex.EncodeToString(publicKey), hex.EncodeToString(role.PublicKey()))
	}
	recordCount := 1
	for {
		frame, err := connect.ReadExtenderFeedFrame(clientConn)
		if err != nil {
			t.Fatal(err)
		}
		if frame.GetEndOfSample() {
			break
		}
		if frame.GetRecord() == nil {
			t.Fatalf("the sample carried %v, expected a record or end_of_sample", frame)
		}
		recordCount += 1
	}
	// the own record leads, and the rest of the open tier follows it
	if recordCount != 2 {
		t.Fatalf("the sample carried %d records, expected 2", recordCount)
	}

	// a client that did not subscribe is not held: the serve returns and the
	// extender closes the stream
	select {
	case <-served:
	case <-time.After(testRoleTimeout):
		t.Fatal("the feed kept a non-subscriber stream")
	}
	if _, err := connect.ReadExtenderFeedFrame(clientConn); err == nil {
		t.Fatal("a non-subscriber stream carried more than the sample")
	}
}

// A role with no node refuses the gossip service rather than parking the
// extender's connection goroutine on a mesh that is not there.
func TestExtenderRoleRefusesTheGossipServiceWithNoNode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	rootKey := newTestKey(t)
	extenderKey := newTestKey(t)
	directory := newTestDirectory(t, rootKey)
	role, err := New(ctx, &Settings{
		NetworkHost:     testNetworkHost,
		Directory:       directory,
		IdentityKeySeed: extenderKey.seed,
		DisableGossip:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(role.Close)

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close() })
	refused := make(chan struct{})
	go func() {
		defer close(refused)
		role.GossipConnHandler(serverConn)
	}()
	select {
	case <-refused:
	case <-time.After(10 * time.Second):
		t.Fatal("a role with no node parked the gossip stream instead of refusing it")
	}
}

// A caller that builds the listener keeps it, and the addresses it carries are
// what the mesh advertises: `ListenAddrs` in the settings would be ignored
// here, which is the contract the provider role relies on when it installs the
// listener before the node exists.
func TestExtenderRoleUsesTheCallersListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	rootKey := newTestKey(t)
	extenderKey := newTestKey(t)
	directory := newTestDirectory(t, rootKey)

	// an address no socket is bound for: the in-process listener takes the
	// stream from the extender, not from the kernel
	listenAddr := ma.StringCast("/ip4/203.0.113.9/tcp/443")
	listenerSettings := gossip.DefaultInProcessListenerSettings()
	listenerSettings.ListenAddrs = []ma.Multiaddr{listenAddr}
	listener := gossip.NewInProcessListener(ctx, listenerSettings)
	t.Cleanup(listener.Close)

	role, err := New(ctx, &Settings{
		NetworkHost:     testNetworkHost,
		Directory:       directory,
		IdentityKeySeed: extenderKey.seed,
		// a decoy: the caller's listener carries its own addresses
		ListenAddrs: []ma.Multiaddr{ma.StringCast("/ip4/203.0.113.10/tcp/443")},
		Listener:    listener,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(role.Close)

	if role.Listener() != listener {
		t.Fatal("the role did not use the listener it was handed")
	}
	listenAddrs := role.Node().ListenAddrs()
	if len(listenAddrs) != 1 || !listenAddrs[0].Equal(listenAddr) {
		t.Fatalf("the node listens on %v, expected the listener's %s", listenAddrs, listenAddr)
	}
}

// The mesh, both directions, through the role's own entry points: the operator
// fills the role's directory, the role publishes to the operator, and the feed
// serves what arrived.
func TestExtenderRolePublishesAndAppliesRecordsOverTheMesh(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	rootKey := newTestKey(t)

	// the operator: an ordinary member node, listening over the websocket
	// transport on loopback, and the only node that originates (D6)
	operatorDirectory := newTestDirectory(t, rootKey)
	operatorSettings := gossip.DefaultNodeSettings(gossip.NodeRoleMember)
	operatorSettings.NetworkHost = testNetworkHost
	operatorSettings.Directory = operatorDirectory
	operatorSettings.IdentityKeySeed = newTestKey(t).seed
	operatorSettings.ConnectSettings = connect.DefaultConnectSettings()
	operatorSettings.PeerTimeout = 5 * time.Minute
	operatorSettings.PeerCoalesceTimeout = 10 * time.Millisecond
	operatorSettings.DialTimeout = 20 * time.Second
	operatorSettings.StatusTimeout = 50 * time.Millisecond
	operatorSettings.RandSeed = 1
	operatorListenAddrs, err := gossip.WebsocketListenAddrs("127.0.0.1", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	operatorSettings.ListenAddrs = operatorListenAddrs
	operator, err := gossip.NewNode(ctx, operatorSettings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(operator.Close)

	operatorListenAddrs = operator.ListenAddrs()
	if len(operatorListenAddrs) == 0 {
		t.Fatal("the operator bound no address")
	}
	p2pComponent, err := ma.NewComponent("p2p", operator.PeerId().String())
	if err != nil {
		t.Fatal(err)
	}
	operatorAddrs := []ma.Multiaddr{operatorListenAddrs[0].Encapsulate(p2pComponent)}

	// the served extender. It advertises nothing: the carrier dial behind an
	// extender hop is a stub in this fork, so the operator's link is the one
	// that can be made here
	roleDirectory := newTestDirectory(t, rootKey)
	extenderKey := newTestKey(t)
	role, err := New(ctx, &Settings{
		NetworkHost:     testNetworkHost,
		Directory:       roleDirectory,
		IdentityKeySeed: extenderKey.seed,
		OperatorAddrs:   operatorAddrs,
		ConnectSettings: connect.DefaultConnectSettings(),
		NodeSettings:    fastTestNodeSettings(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(role.Close)

	if !role.GossipRunning() {
		t.Fatal("the role is not running a gossip node")
	}
	waitForMeshStatus(t, role.Node(), "the operator in the topic", func(status gossip.NodeStatus) bool {
		return 1 <= status.MeshPeerCount && status.OperatorConnected
	})
	// the operator has to have heard the role's subscription before its own
	// flood publish can reach it: a publish to a topic with no peer is a
	// silent success, which is the one race a mesh test must not ride
	waitForMeshStatus(t, operator, "the extender in the topic", func(status gossip.NodeStatus) bool {
		return 1 <= status.MeshPeerCount
	})

	// (a) the operator publishes this extender's own record; the role's node
	// applies it to the directory the feed serves
	if err := operator.Publish(ctx, &protocol.ExtenderGossipMessage{
		Message: &protocol.ExtenderGossipMessage_Record{
			Record: signTestRecord(t, rootKey, extenderKey, "203.0.113.9", 443, time.Now()),
		},
	}); err != nil {
		t.Fatal(err)
	}
	waitForDirectoryKey(t, roleDirectory, extenderKey.publicKey, "the extender's own record")

	// (b) the role publishes another extender's record: the operator applies
	// it, and so does the role's own directory
	otherKey := newTestKey(t)
	if err := role.Publish(ctx, &protocol.ExtenderGossipMessage{
		Message: &protocol.ExtenderGossipMessage_Record{
			Record: signTestRecord(t, rootKey, otherKey, "203.0.113.10", 443, time.Now()),
		},
	}); err != nil {
		t.Fatal(err)
	}
	waitForDirectoryKey(t, operatorDirectory, otherKey.publicKey, "the record the role published")
	waitForDirectoryKey(t, roleDirectory, otherKey.publicKey, "the record the role published to itself")

	// (c) a record no root key signed is refused before it is relayed
	if err := role.Publish(ctx, &protocol.ExtenderGossipMessage{
		Message: &protocol.ExtenderGossipMessage_Record{
			Record: signTestRecord(t, newTestKey(t), newTestKey(t), "203.0.113.11", 443, time.Now()),
		},
	}); err == nil {
		t.Fatal("the role published a record no root key signed")
	}

	// (d) the feed now leads with that record, which is the whole point of the
	// composition: what the mesh taught the extender is what a client is served
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close() })
	go func() {
		defer serverConn.Close()
		role.FeedConnHandler(serverConn)
	}()
	if err := connect.WriteExtenderFeedRequest(clientConn, &protocol.ExtenderFeedRequest{
		SampleCount: 8,
	}); err != nil {
		t.Fatal(err)
	}
	clientConn.SetReadDeadline(time.Now().Add(testRoleTimeout))
	first, err := connect.ReadExtenderFeedFrame(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	if publicKey := recordFramePublicKey(t, first); !bytes.Equal(publicKey, extenderKey.publicKey) {
		t.Fatalf("the feed led with %s, expected the extender's own key %s",
			hex.EncodeToString(publicKey), hex.EncodeToString(extenderKey.publicKey))
	}
}
