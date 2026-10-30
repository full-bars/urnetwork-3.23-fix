// The feed as an open channel of the tiered directory (EXTENDER.md Q1, Q2):
// the sample and the stream are bound to the client's vantage, and the gated
// tier is never served. The clock is pinned on every directory here, so the
// epoch never turns under a test.

package gossip

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"net"
	"slices"
	"testing"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// A fixed partition secret, so which partition a vantage is dealt is the same
// on every run and a test can name vantages that are dealt apart.
var testPartitionSecret = []byte("gossip-feed-test-partition-secret")

// A directory whose clock never moves.
func newTestPinnedDirectory(t *testing.T, rootKey *testKey) *connect.ExtenderDirectory {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	settings := connect.DefaultExtenderDirectorySettings()
	settings.NetworkHosts = []string{testNetworkHost}
	settings.PartitionSecret = testPartitionSecret
	settings.Now = func() time.Time {
		return now
	}
	ctx, cancel := context.WithCancel(context.Background())
	directory := connect.NewExtenderDirectory(ctx, settings)
	directory.SetRootKeys(connect.NewExtenderRootKeySet(rootKey.publicKey))
	t.Cleanup(func() {
		directory.Close()
		cancel()
	})
	return directory
}

// A pipe end that reports a remote address, which is what the feed server
// takes the client's vantage from.
type testAddrConn struct {
	net.Conn
	remoteAddr net.Addr
}

func (self *testAddrConn) RemoteAddr() net.Addr {
	return self.remoteAddr
}

// One served stream from a client at `remote`, with the request sent.
func newTestFeedClientAt(
	t *testing.T,
	feed *FeedServer,
	remote string,
	request *protocol.ExtenderFeedRequest,
) *testFeedClient {
	t.Helper()
	remoteAddr, err := net.ResolveTCPAddr("tcp", remote)
	if err != nil {
		t.Fatal(err)
	}
	clientConn, serverConn := net.Pipe()
	client := &testFeedClient{
		conn: clientConn,
		done: make(chan struct{}),
	}
	go func() {
		defer close(client.done)
		defer serverConn.Close()
		feed.Serve(&testAddrConn{Conn: serverConn, remoteAddr: remoteAddr})
	}()
	t.Cleanup(func() {
		clientConn.Close()
		<-client.done
	})
	if err := connect.WriteExtenderFeedRequest(client.conn, request); err != nil {
		t.Fatal(err)
	}
	return client
}

// The key of each record frame, hex, in order.
func frameKeyHexes(t *testing.T, directory *connect.ExtenderDirectory, frames []*protocol.ExtenderFeedFrame) []string {
	t.Helper()
	keyHexes := []string{}
	for _, frame := range frames {
		if frame.GetRecord() == nil {
			continue
		}
		body, err := directory.RootKeys().VerifyRecord(frame.GetRecord())
		if err != nil {
			t.Fatal(err)
		}
		keyHexes = append(keyHexes, hex.EncodeToString(body.PublicKey))
	}
	return keyHexes
}

// Applies `count` open records and returns their keys, hex and sorted.
func applyTestOpenFeedRecords(t *testing.T, directory *connect.ExtenderDirectory, rootKey *testKey, count int, octet int) []string {
	t.Helper()
	keyHexes := []string{}
	for i := range count {
		extenderKey := newTestKey(t)
		applyTestFeedRecord(t, directory, rootKey, extenderKey, fmt.Sprintf("198.51.%d.%d", octet, 1+i))
		keyHexes = append(keyHexes, hex.EncodeToString(extenderKey.publicKey))
	}
	slices.Sort(keyHexes)
	return keyHexes
}

// A record whose signed body carries the gated tier as a reader that
// predates the field would receive it: the legacy body bytes with field 13
// appended, signed over exactly those bytes.
func signTestGatedRecord(t *testing.T, rootKey *testKey, extenderKey *testKey, ip string) *protocol.ExtenderRecord {
	t.Helper()
	bodyBytes, err := proto.Marshal(&protocol.ExtenderRecordBody{
		PublicKey: extenderKey.publicKey,
		Addresses: []*protocol.ExtenderAddress{
			{Ip: ip, IpVersion: 4, Carriers: []string{connect.ExtenderCarrierTcp}},
		},
		TcpPort:      8443,
		CountryCode:  "us",
		IssueTimeMs:  uint64(time.Now().UnixMilli()),
		ExpireTimeMs: uint64(time.Now().Add(14 * 24 * time.Hour).UnixMilli()),
		NetworkHost:  testNetworkHost,
	})
	if err != nil {
		t.Fatal(err)
	}
	bodyBytes = protowire.AppendTag(bodyBytes, 13, protowire.VarintType)
	bodyBytes = protowire.AppendVarint(bodyBytes, 1)
	signingBytes := append([]byte(connect.ExtenderRecordSignatureDomain), bodyBytes...)
	return &protocol.ExtenderRecord{
		Body:          bodyBytes,
		RootSignature: ed25519.Sign(rootKey.privateKey, signingBytes),
		RootKeyId:     connect.ExtenderKeyId(rootKey.publicKey),
	}
}

// Root cause: one observer enumerates the fleet because the feed serves the
// whole set at random. The sample is bound to the client's vantage: polling
// forever from one prefix sees one partition of the open tier, and two
// addresses of the prefix are one vantage (Q2).
func TestGossipFeedSampleIsBoundToTheClientVantage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	rootKey := newTestKey(t)
	directory := newTestPinnedDirectory(t, rootKey)
	keyHexes := applyTestOpenFeedRecords(t, directory, rootKey, 64, 100)

	settings := DefaultFeedServerSettings()
	settings.KeepaliveTimeout = time.Minute
	feed := NewFeedServer(ctx, directory, nil, settings)
	t.Cleanup(feed.Close)

	poll := func(remote string) []string {
		client := newTestFeedClientAt(t, feed, remote, &protocol.ExtenderFeedRequest{
			SampleCount: 8,
			Subscribe:   false,
		})
		return frameKeyHexes(t, directory, client.readSample(t))
	}
	seen := map[string]bool{}
	first := poll("203.0.113.7:40000")
	if len(first) == 0 {
		t.Fatal("the sample is empty")
	}
	for i := range 48 {
		sample := poll(fmt.Sprintf("203.0.113.%d:%d", 7+i, 40000+i))
		for _, keyHex := range sample {
			seen[keyHex] = true
		}
	}
	// sixty-four records make eight partitions of about eight; a vantage
	// that saw more than half the fleet was not bound to one
	if 32 < len(seen) {
		t.Fatalf("forty-eight polls from one /24 saw %d of %d records", len(seen), len(keyHexes))
	}
	// two polls from one prefix in one epoch are one sample
	if again := poll("203.0.113.200:40001"); !slices.Equal(again, first) {
		t.Fatalf("a second address of the prefix was sampled %v, expected %v", again, first)
	}
}

// The stream is bound the same way: a subscriber is sent the open records of
// its partition as they are applied, and no other (Q2). A revocation is
// streamed whatever the partition.
func TestGossipFeedStreamIsBoundToTheClientVantage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	rootKey := newTestKey(t)
	directory := newTestPinnedDirectory(t, rootKey)
	// a non-trivial open pool before any subscriber exists, so the partitions
	// have something to partition; the streamed records grow it from 33 to 96
	applyTestOpenFeedRecords(t, directory, rootKey, 32, 100)

	settings := DefaultFeedServerSettings()
	settings.KeepaliveTimeout = time.Minute
	feed := NewFeedServer(ctx, directory, nil, settings)
	t.Cleanup(feed.Close)

	// two subscribers of one prefix, and two of other prefixes
	type streamResult struct {
		keys       []string
		revocation *protocol.ExtenderRevocation
		err        error
	}
	type subscriber struct {
		name    string
		vantage []byte
		client  *testFeedClient
		results chan streamResult
	}
	readStreamed := func(client *testFeedClient) streamResult {
		streamed := []string{}
		for {
			frame, err := connect.ReadExtenderFeedFrame(client.conn)
			if err != nil {
				return streamResult{err: err}
			}
			if frame.GetRevocation() != nil {
				return streamResult{keys: streamed, revocation: frame.GetRevocation()}
			}
			if frame.GetRecord() == nil {
				continue
			}
			body, err := directory.RootKeys().VerifyRecord(frame.GetRecord())
			if err != nil {
				return streamResult{err: err}
			}
			streamed = append(streamed, hex.EncodeToString(body.PublicKey))
		}
	}
	// The open pool runs from 33 to 96 records, which is 8 and then 16
	// partitions. The foreign prefixes are picked to be dealt different
	// partitions modulo 8, so they differ from the first, and from each other,
	// at both sizes: whatever a foreign client is sent that the first is not
	// owed is a leak, and the test cannot pass for want of a distinct vantage.
	vantageOf := func(remote string) []byte {
		addr, err := net.ResolveTCPAddr("tcp", remote)
		if err != nil {
			t.Fatal(err)
		}
		return connect.ExtenderVantageKeyOfAddr(addr)
	}
	partitionOf := func(remote string) int {
		return connect.ExtenderVantagePartition(testPartitionSecret, connect.ExtenderChannelFeed, vantageOf(remote), 8)
	}
	taken := map[int]bool{partitionOf("203.0.113.7:40000"): true}
	foreign := []string{}
	for octet := range 256 {
		remote := fmt.Sprintf("198.18.%d.9:40002", octet)
		if partition := partitionOf(remote); !taken[partition] {
			taken[partition] = true
			foreign = append(foreign, remote)
		}
		if len(foreign) == 2 {
			break
		}
	}
	if len(foreign) != 2 {
		t.Fatal("no two foreign prefixes dealt different partitions")
	}
	subscribers := []*subscriber{}
	for _, s := range []struct{ name, remote string }{
		{"first", "203.0.113.7:40000"},
		{"sibling", "203.0.113.200:40001"},
		{"other-a", foreign[0]},
		{"other-b", foreign[1]},
	} {
		client := newTestFeedClientAt(t, feed, s.remote, &protocol.ExtenderFeedRequest{
			SampleCount: 0,
			Subscribe:   true,
		})
		client.readEndOfSample(t)
		sub := &subscriber{
			name:    s.name,
			vantage: vantageOf(s.remote),
			client:  client,
			results: make(chan streamResult, 1),
		}
		// each reader hands over one batch per revocation marker and stops at
		// the first error, so the reader never blocks the server on a full pipe
		go func() {
			for {
				result := readStreamed(sub.client)
				select {
				case sub.results <- result:
				case <-ctx.Done():
					return
				}
				if result.err != nil {
					return
				}
			}
		}()
		subscribers = append(subscribers, sub)
	}

	markerKey := newTestKey(t)
	markerTime := uint64(time.Now().UnixMilli())
	streamedCount := 0
	// batches in which a foreign vantage was owed a different answer than the
	// first, which is what gives the leak check something to catch
	distinguishedCount := 0
	for i := range 64 {
		extenderKey := newTestKey(t)
		applyTestFeedRecord(t, directory, rootKey, extenderKey, fmt.Sprintf("198.51.101.%d", 1+i))
		// What each vantage is owed for this record, from the directory's sample
		// of the pool as it now stands rather than the stream's filter. Nothing
		// changes the pool until every handler has passed this batch (the marker
		// revokes a key that was never applied), so the answer cannot move
		// between here and the handlers' own decisions.
		owed := map[string][]string{}
		for _, sub := range subscribers {
			owed[sub.name] = []string{}
			for _, message := range directory.SampleRecords(96, nil, sub.vantage) {
				body, err := directory.RootKeys().VerifyRecord(message.GetRecord())
				if err != nil {
					t.Fatal(err)
				}
				if slices.Equal(body.PublicKey, extenderKey.publicKey) {
					owed[sub.name] = append(owed[sub.name], hex.EncodeToString(extenderKey.publicKey))
				}
			}
		}
		revocation, err := connect.SignExtenderRevocation(rootKey.privateKey, &protocol.ExtenderRevocationBody{
			PublicKey:   markerKey.publicKey,
			IssueTimeMs: markerTime + uint64(i),
			NetworkHost: testNetworkHost,
		})
		if err != nil {
			t.Fatal(err)
		}
		if changed, err := directory.ApplyRevocationSource(revocation, connect.ExtenderSourceGossip); err != nil || !changed {
			t.Fatalf("apply marker %d: changed=%t err=%v", i, changed, err)
		}
		for _, sub := range subscribers {
			result := <-sub.results
			if result.err != nil {
				t.Fatalf("%s batch %d: %v", sub.name, i, result.err)
			}
			// fails if the feed streamed a record outside this client's
			// partition (a leak of another vantage's record), or withheld one
			// inside it
			if !slices.Equal(result.keys, owed[sub.name]) {
				t.Fatalf("%s batch %d: streamed records %v, owed %v", sub.name, i, result.keys, owed[sub.name])
			}
			if !proto.Equal(result.revocation, revocation) {
				t.Fatalf("%s batch %d: marker %v, want %v", sub.name, i, result.revocation, revocation)
			}
		}
		streamedCount += len(owed["first"])
		if !slices.Equal(owed["first"], owed["other-a"]) || !slices.Equal(owed["first"], owed["other-b"]) {
			distinguishedCount += 1
		}
		// one prefix is one vantage: the sibling is owed exactly the same
		if !slices.Equal(owed["first"], owed["sibling"]) {
			t.Fatalf("batch %d: a sibling of the prefix is owed %v, the first %v", i, owed["sibling"], owed["first"])
		}
	}
	if streamedCount == 0 {
		t.Fatal("the stream carried none of the applied records")
	}
	if distinguishedCount == 0 {
		t.Fatal("every vantage was owed the same records, so the partitions were not tested")
	}
}

// Root cause: operator hosts are in the open channels. A gated record the
// directory holds -- here one that leaked in over the mesh -- is served by
// neither the sample nor the stream (Q1).
func TestGossipFeedNeverServesAGatedRecord(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	rootKey := newTestKey(t)
	directory := newTestPinnedDirectory(t, rootKey)
	gatedKey := newTestKey(t)
	gatedKeyHex := hex.EncodeToString(gatedKey.publicKey)
	if _, err := directory.ApplyRecord(signTestGatedRecord(t, rootKey, gatedKey, "192.0.2.77"), connect.ExtenderSourceGossip); err != nil {
		t.Fatal(err)
	}
	// one open record beside it, so an empty sample is not a passing sample
	openKey := newTestKey(t)
	applyTestFeedRecord(t, directory, rootKey, openKey, "198.51.100.1")

	settings := DefaultFeedServerSettings()
	settings.KeepaliveTimeout = time.Minute
	feed := NewFeedServer(ctx, directory, gatedKey.publicKey, settings)
	t.Cleanup(feed.Close)

	// the sample, from many vantages, with the gated key as the server's own
	for i := range 16 {
		client := newTestFeedClientAt(t, feed, fmt.Sprintf("203.0.%d.7:40000", i), &protocol.ExtenderFeedRequest{
			SampleCount: 8,
			Subscribe:   false,
		})
		keyHexes := frameKeyHexes(t, directory, client.readSample(t))
		if slices.Contains(keyHexes, gatedKeyHex) {
			t.Fatalf("vantage %d was sampled the gated record", i)
		}
		if !slices.Contains(keyHexes, hex.EncodeToString(openKey.publicKey)) {
			t.Fatalf("vantage %d was not sampled the open record", i)
		}
	}

	// the stream: a newer gated record of the same key, then a revocation of
	// the open one, which every subscriber is sent; the first frame must be
	// the revocation, never the gated record
	client := newTestFeedClientAt(t, feed, "203.0.113.7:40000", &protocol.ExtenderFeedRequest{
		SampleCount: 0,
		Subscribe:   true,
	})
	client.readEndOfSample(t)
	if _, err := directory.ApplyRecord(signTestGatedRecord(t, rootKey, gatedKey, "192.0.2.78"), connect.ExtenderSourceGossip); err != nil {
		t.Fatal(err)
	}
	revocation, err := connect.SignExtenderRevocation(rootKey.privateKey, &protocol.ExtenderRevocationBody{
		PublicKey:   openKey.publicKey,
		IssueTimeMs: uint64(time.Now().Add(time.Second).UnixMilli()),
		NetworkHost: testNetworkHost,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := directory.ApplyRevocationSource(revocation, connect.ExtenderSourceGossip); err != nil {
		t.Fatal(err)
	}
	frame, err := connect.ReadExtenderFeedFrame(client.conn)
	if err != nil {
		t.Fatal(err)
	}
	if frame.GetRevocation() == nil {
		t.Fatalf("the stream carried %v before the revocation, expected the gated record to be withheld", frame)
	}
}

// Root cause: a gated record that reaches the mesh is relayed to every
// member. The validator does not accept one, so the node relays nothing of
// the gated tier, and accepts the same record signed open (Q1).
func TestGossipValidatorIgnoresAGatedRecord(t *testing.T) {
	rootKey := newTestKey(t)
	extenderKey := newTestKey(t)
	directory := newTestDirectory(t, rootKey)
	node := &Node{
		settings: &NodeSettings{
			NetworkHost: testNetworkHost,
			Directory:   directory,
		},
	}
	validate := func(record *protocol.ExtenderRecord) pubsub.ValidationResult {
		return testValidate(t, node, &protocol.ExtenderGossipMessage{
			Message: &protocol.ExtenderGossipMessage_Record{Record: record},
		})
	}
	if result := validate(signTestRecord(t, rootKey, extenderKey, "198.51.100.1", 8443, time.Now())); result != pubsub.ValidationAccept {
		t.Fatalf("an open record was %v, expected accept", result)
	}
	if result := validate(signTestGatedRecord(t, rootKey, extenderKey, "198.51.100.1")); result == pubsub.ValidationAccept {
		t.Fatal("a gated record was accepted for relay")
	}
}
