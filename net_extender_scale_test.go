//go:build !js

package connect

import (
	"context"
	"encoding/binary"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The scale fixtures the client directory's tests share (GEOMAP §5.8 item 5).
//
// The pin's scale suite is one file: the million-record feed into a capped
// directory and the pinger over a hundred thousand known peers. Only the
// directory half is carried in this phase -- the peer pinger is the gossip
// phase's (net_extender_peer_pinger.go) -- so the fixtures both halves use are
// here and the two scale tests themselves land with the pinger.

// The carriers every generated record lists, shared, since a body is only
// read.
var testScaleCarriers = []string{ExtenderCarrierTcp}

// One generated record: its body verified in all but the signature, and the
// key it carries. The key and the address come from the index, so every
// record is distinct and nothing about one is kept to make the next.
func testScaleRecord(i int, continent string, now time.Time) (*protocol.ExtenderRecordBody, []byte) {
	publicKey := make([]byte, 32)
	copy(publicKey, "scale-test-extender-identity-key")
	binary.BigEndian.PutUint64(publicKey[24:], uint64(i))
	// the address in the v6 documentation prefix
	ipBytes := [16]byte{0x20, 0x01, 0x0d, 0xb8}
	binary.BigEndian.PutUint64(ipBytes[8:], uint64(i))
	body := &protocol.ExtenderRecordBody{
		PublicKey: publicKey,
		Addresses: []*protocol.ExtenderAddress{
			{
				Ip:        netip.AddrFrom16(ipBytes).String(),
				IpVersion: 6,
				Carriers:  testScaleCarriers,
			},
		},
		TcpPort:       443,
		ContinentCode: continent,
		IssueTimeMs:   uint64(now.UnixMilli()),
		ExpireTimeMs:  uint64(now.Add(24 * time.Hour).UnixMilli()),
		NetworkHost:   testExtenderNetworkHost,
	}
	return body, publicKey
}

// A directory for a scale test, on a clock that never moves.
func newTestScaleDirectory(t *testing.T, configure func(settings *ExtenderDirectorySettings)) (*ExtenderDirectory, time.Time) {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	settings := DefaultExtenderDirectorySettings()
	settings.Now = func() time.Time { return now }
	settings.NetworkHosts = []string{testExtenderNetworkHost}
	if configure != nil {
		configure(settings)
	}
	ctx, cancel := context.WithCancel(context.Background())
	directory := NewExtenderDirectory(ctx, settings)
	t.Cleanup(func() {
		directory.Close()
		cancel()
	})
	return directory, now
}
